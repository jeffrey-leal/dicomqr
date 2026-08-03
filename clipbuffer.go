package main

// clipBuffer holds the decoded frames of the chapter currently on screen, for
// the whole time that chapter is selected, filled in the background from one
// parse of the file.
//
// Playback cannot decode on demand: a 1016×758 echo frame costs ~18 ms to
// decode, so a clip stating 30 fps has under two frames of headroom on one core
// and judders. Decoding the whole clip up front across every core takes ~0.3 s
// for a 53-frame loop, after which playback is a memory read.
//
// Deliberately not the viewer's dicomFileCache: that cache serves one frame at
// a time for navigation, whereas this is a pinned array whose residency is
// bounded by dropping it on chapter change. Frames land in index order, so
// playback can begin before buffering finishes — callers gate on isReady and
// fall back to the on-demand path for a frame that has not arrived.

import (
	"image"
	"image/draw"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// clipBufferBudget caps one buffered clip. At 2.9 MB per decoded 1016×758 RGBA
// frame that is ~130 frames, so every loop in a real echo study (30-60 frames)
// buffers whole; a longer clip loops over the buffered prefix and the viewer
// says so rather than stalling at the boundary. A variable so tests can shrink
// it — a genuinely oversized clip is far too large to synthesise.
var clipBufferBudget int64 = 384 << 20

// clipBufferWorkers caps decode concurrency. More workers fill faster but all
// of their output is live at once, and the budget above is the real limit.
func clipBufferWorkers() int {
	n := runtime.NumCPU()
	if n < 1 {
		return 1
	}
	if n > 8 {
		return 8
	}
	return n
}

type clipBuffer struct {
	chapter chapter

	frames []atomic.Pointer[decodedFrame]
	// ann is the chapter's annotation set, identical for every frame of one
	// instance; nil until the parse succeeds.
	ann   atomic.Pointer[imageAnnotations]
	count atomic.Int32
	cap   atomic.Int32

	cancelled atomic.Bool
	failed    atomic.Bool
	complete  atomic.Bool
}

// startClipBuffer begins decoding a chapter in the background and returns
// immediately. onProgress is posted to the Fyne UI goroutine as frames land,
// with the count decoded so far, and once more when the fill stops.
func startClipBuffer(c chapter, onProgress func(decoded int)) *clipBuffer {
	b := &clipBuffer{chapter: c, frames: make([]atomic.Pointer[decodedFrame], maxInt(1, c.frames))}
	b.cap.Store(int32(maxInt(1, c.frames)))
	go b.fill(onProgress)
	return b
}

// cancel stops the background fill. Frames already decoded stay readable.
func (b *clipBuffer) cancel() { b.cancelled.Store(true) }

// isReady reports whether a frame has been decoded and can be shown without
// touching the file.
func (b *clipBuffer) isReady(index int) bool {
	return b != nil && index >= 0 && index < len(b.frames) && b.frames[index].Load() != nil
}

// frame returns the decoded frame, or nil when it has not been buffered (yet,
// or at all).
func (b *clipBuffer) frame(index int) *decodedFrame {
	if b == nil || index < 0 || index >= len(b.frames) {
		return nil
	}
	return b.frames[index].Load()
}

// annotations returns the chapter's annotation set once the file has been
// parsed, or nil before that.
func (b *clipBuffer) annotations() *imageAnnotations {
	if b == nil {
		return nil
	}
	return b.ann.Load()
}

func (b *clipBuffer) decodedCount() int { return int(b.count.Load()) }

// capacity is how many frames this buffer will ultimately hold — below the
// chapter's frame count only when the whole clip would not fit the budget, in
// which case playback is confined to the buffered prefix.
func (b *clipBuffer) capacity() int { return int(b.cap.Load()) }

// truncated reports a clip too large to buffer whole. A clip that could not be
// decoded at all is failed, not truncated: it has no playable prefix, and the
// on-demand path surfaces the real error instead.
func (b *clipBuffer) truncated() bool {
	return b != nil && !b.failed.Load() && b.capacity() < b.chapter.frames
}

func (b *clipBuffer) isFailed() bool { return b != nil && b.failed.Load() }

func (b *clipBuffer) isComplete() bool { return b != nil && b.complete.Load() }

// clipProgressInterval bounds how often buffering progress reaches the UI. The
// decode workers only count frames; one reporter goroutine publishes, so a
// 200-frame clip costs a handful of label updates instead of one per frame per
// worker.
const clipProgressInterval = 100 * time.Millisecond

func (b *clipBuffer) fill(onProgress func(decoded int)) {
	b.fillFrames(onProgress)
	b.complete.Store(true)
	b.post(onProgress)
}

func (b *clipBuffer) fillFrames(onProgress func(decoded int)) {
	parsed, err := parseDicomFile(b.chapter.path)
	if err != nil {
		// An unreadable clip leaves the buffer empty; every read falls back to the
		// on-demand path, which surfaces the same failure through the viewer's
		// existing error reporting.
		b.failed.Store(true)
		return
	}
	ann := parsed.ann
	b.ann.Store(&ann)

	total := minInt(len(b.frames), parsed.frameCount())
	if total <= 0 {
		b.failed.Store(true)
		return
	}

	// Decode frame 0 alone first: only once a real frame's size is known can the
	// budget decide how much of this clip fits.
	first, err := b.decode(parsed, 0)
	if err != nil || b.cancelled.Load() {
		b.failed.Store(err != nil)
		return
	}
	b.frames[0].Store(first)
	b.count.Add(1)
	b.post(onProgress)

	perFrame := int64(maxInt(1, frameBytes(first)))
	affordable := int(minInt64(int64(total), maxInt64(1, clipBufferBudget/perFrame)))
	b.cap.Store(int32(affordable))
	if affordable <= 1 {
		return
	}

	// One reporter goroutine owns progress publishing while the workers below
	// only count. Posting from every worker would put several UI updates in
	// flight at once for no benefit — the count is all the viewer shows.
	stopReporting := make(chan struct{})
	reporterDone := make(chan struct{})
	go func() {
		defer close(reporterDone)
		ticker := time.NewTicker(clipProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopReporting:
				return
			case <-ticker.C:
				b.post(onProgress)
			}
		}
	}()

	// The rest in parallel. Indices are handed out in order so frames land
	// roughly in playback order and the readiness gate rarely holds.
	next := int64(1)
	var wg sync.WaitGroup
	for w := 0; w < clipBufferWorkers(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1)) - 1
				if i >= affordable || b.cancelled.Load() {
					return
				}
				df, decErr := b.decode(parsed, i)
				if decErr != nil {
					// One bad frame mid-clip must not cost the rest of the loop: leave
					// the slot empty (isReady stays false, so playback holds rather
					// than showing rubbish) and carry on.
					continue
				}
				b.frames[i].Store(df)
				b.count.Add(1)
			}
		}()
	}
	wg.Wait()
	// Stop the reporter and wait for it, so the final post in fill is the last
	// one and cannot overlap with a tick.
	close(stopReporting)
	<-reporterDone
}

// decode produces one frame ready for display. Colour frames are converted to
// RGBA here, on a background goroutine: Fyne would otherwise convert on every
// paint, on the UI goroutine, at playback rate.
func (b *clipBuffer) decode(parsed *parsedDicom, index int) (*decodedFrame, error) {
	st, err := parsed.frameState(index)
	if err != nil {
		return nil, err
	}
	df := st.frame
	if df.colorImg != nil {
		df.colorImg = toRGBA(df.colorImg)
	}
	return df, nil
}

func (b *clipBuffer) post(onProgress func(decoded int)) {
	if onProgress == nil || b.cancelled.Load() {
		return
	}
	decoded := b.decodedCount()
	postUI(func() { onProgress(decoded) })
}

// toRGBA returns img as an *image.RGBA, without copying when it already is one.
func toRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba
	}
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
	return dst
}

// frameBytes estimates a decoded frame's memory footprint, for the budget.
func frameBytes(df *decodedFrame) int {
	if df == nil {
		return 0
	}
	if df.colorImg != nil {
		if rgba, ok := df.colorImg.(*image.RGBA); ok {
			return len(rgba.Pix)
		}
		b := df.colorImg.Bounds()
		return b.Dx() * b.Dy() * 4
	}
	// Grayscale: the float buffer the viewer re-windows from, plus the RGBA the
	// viewport renders it into.
	return len(df.gray)*4 + df.rows*df.cols*4
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
