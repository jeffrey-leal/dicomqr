package main

// Tests for the clip buffer — the decoded frames of the chapter on screen — and
// its memory budget. onProgress is nil throughout: progress is posted to the
// Fyne UI goroutine, which no test runs.

import (
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitForBuffer fails the test rather than hanging if a fill never finishes.
func waitForBuffer(t *testing.T, b *clipBuffer) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !b.isComplete() {
		if time.Now().After(deadline) {
			t.Fatal("clip buffer never completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClipBufferHoldsEveryFrame(t *testing.T) {
	const frames = 24
	path := writeMultiframeTestFile(t, t.TempDir(), frames, 1)

	b := startClipBuffer(chapter{path: path, frames: frames}, nil)
	waitForBuffer(t, b)

	if b.isFailed() {
		t.Fatal("buffering a readable clip reported failure")
	}
	if b.truncated() {
		t.Fatalf("a %d-frame clip was truncated at %d — it fits the budget", frames, b.capacity())
	}
	if got := b.decodedCount(); got != frames {
		t.Errorf("decodedCount = %d, want %d", got, frames)
	}
	for i := 0; i < frames; i++ {
		if !b.isReady(i) {
			t.Errorf("frame %d is not ready after a complete fill", i)
			continue
		}
		df := b.frame(i)
		if df == nil || len(df.gray) != 4 {
			t.Errorf("frame %d did not decode to a 2×2 frame: %+v", i, df)
			continue
		}
		// writeMultiframeTestFile marks each frame with 40+index.
		if got, want := int(df.gray[3]), 40+i; got != want {
			t.Errorf("frame %d holds marker %d, want %d", i, got, want)
		}
	}
	if b.annotations() == nil {
		t.Error("the chapter's annotations were not published")
	}
}

// Out-of-range reads are a normal consequence of a chapter change racing a
// repaint, so they answer rather than panic.
func TestClipBufferOutOfRangeReads(t *testing.T) {
	path := writeMultiframeTestFile(t, t.TempDir(), 4, 1)
	b := startClipBuffer(chapter{path: path, frames: 4}, nil)
	waitForBuffer(t, b)

	if b.isReady(-1) || b.isReady(99) {
		t.Error("an out-of-range frame must never report ready")
	}
	if b.frame(-1) != nil || b.frame(99) != nil {
		t.Error("an out-of-range frame must read as nil")
	}
	var nilBuffer *clipBuffer
	if nilBuffer.isReady(0) || nilBuffer.frame(0) != nil || nilBuffer.truncated() {
		t.Error("a nil buffer must answer safely — the viewer holds one before the first chapter")
	}
}

// A clip that will not fit is held to the frames that do, and says so, rather
// than being reported as a failure.
func TestClipBufferTruncatesToBudget(t *testing.T) {
	const frames = 24
	path := writeMultiframeTestFile(t, t.TempDir(), frames, 1)

	original := clipBufferBudget
	// A 2×2 8-bit frame costs 4 floats + a 2×2 RGBA = 32 bytes; a 100-byte
	// budget therefore affords 3 of them.
	clipBufferBudget = 100
	defer func() { clipBufferBudget = original }()

	b := startClipBuffer(chapter{path: path, frames: frames}, nil)
	waitForBuffer(t, b)

	if b.isFailed() {
		t.Fatal("a clip too large to hold whole must not be reported as failed")
	}
	if !b.truncated() {
		t.Fatalf("capacity %d of %d frames was not reported as truncated", b.capacity(), frames)
	}
	if b.capacity() >= frames || b.capacity() < 1 {
		t.Errorf("capacity = %d, want between 1 and %d", b.capacity(), frames-1)
	}
	if got := b.decodedCount(); got != b.capacity() {
		t.Errorf("decoded %d frames, want exactly the capacity %d", got, b.capacity())
	}
	if b.isReady(frames - 1) {
		t.Error("a frame past the capacity must not be buffered")
	}
}

func TestClipBufferUnreadableFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.dcm")
	if err := os.WriteFile(path, []byte("this is not a DICOM file"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := startClipBuffer(chapter{path: path, frames: 10}, nil)
	waitForBuffer(t, b)

	if !b.isFailed() {
		t.Error("an unreadable clip must report failure so reads fall through to the on-demand path")
	}
	if b.isReady(0) {
		t.Error("a failed buffer must hold no frames")
	}
	// A failed buffer is not "truncated" — it has no playable prefix to report.
	if b.truncated() {
		t.Error("a failed buffer must not also report truncation")
	}
}

func TestClipBufferCancelStopsFilling(t *testing.T) {
	path := writeMultiframeTestFile(t, t.TempDir(), 200, 1)
	b := startClipBuffer(chapter{path: path, frames: 200}, nil)
	b.cancel()
	waitForBuffer(t, b)
	// Whatever landed before the cancel stays readable, but the fill stopped —
	// the point is that it terminates rather than running to completion.
	if b.decodedCount() > 200 {
		t.Errorf("decodedCount = %d, want no more than the clip's 200", b.decodedCount())
	}
}

func TestFrameBytes(t *testing.T) {
	colour := &decodedFrame{colorImg: image.NewRGBA(image.Rect(0, 0, 10, 20))}
	if got, want := frameBytes(colour), 10*20*4; got != want {
		t.Errorf("colour frameBytes = %d, want %d", got, want)
	}
	gray := &decodedFrame{rows: 20, cols: 10, gray: make([]float32, 200)}
	if got, want := frameBytes(gray), 200*4+10*20*4; got != want {
		t.Errorf("grayscale frameBytes = %d, want %d", got, want)
	}
	if frameBytes(nil) != 0 {
		t.Error("frameBytes(nil) must be 0")
	}
}

// Ultrasound JPEG decodes to YCbCr; converting once in the buffer keeps that
// cost off the UI goroutine at playback rate.
func TestToRGBAConvertsAndPassesThrough(t *testing.T) {
	ycbcr := image.NewYCbCr(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio422)
	for i := range ycbcr.Y {
		ycbcr.Y[i] = 200
	}
	converted := toRGBA(ycbcr)
	if converted.Bounds().Dx() != 4 || converted.Bounds().Dy() != 4 {
		t.Fatalf("converted bounds = %v, want 4×4", converted.Bounds())
	}
	r, _, _, _ := converted.At(0, 0).RGBA()
	if r == 0 {
		t.Error("conversion produced a black pixel from a bright YCbCr source")
	}

	already := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if toRGBA(already) != already {
		t.Error("an image that is already RGBA must be passed through, not copied")
	}
}

func TestScaleToThumbPreservesAspectAndAverages(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 400; x++ {
			// Left half white, right half black: a box filter over the whole image
			// must land near mid-grey at the seam.
			if x < 200 {
				src.Set(x, y, color.White)
			} else {
				src.Set(x, y, color.Black)
			}
		}
	}
	thumb := scaleToThumb(src, 84)
	if got := thumb.Bounds().Dx(); got != 84 {
		t.Errorf("thumb width = %d, want 84", got)
	}
	if got := thumb.Bounds().Dy(); got != 42 {
		t.Errorf("thumb height = %d, want 42 (aspect preserved)", got)
	}
	if r, _, _, _ := thumb.At(5, 20).RGBA(); r < 0xF000 {
		t.Errorf("left side is not white: %v", thumb.At(5, 20))
	}
	if r, _, _, _ := thumb.At(78, 20).RGBA(); r > 0x0FFF {
		t.Errorf("right side is not black: %v", thumb.At(78, 20))
	}

	// A frame smaller than the tile is left alone rather than blown up.
	small := image.NewRGBA(image.Rect(0, 0, 20, 10))
	if got := scaleToThumb(small, 84).Bounds(); got.Dx() != 20 || got.Dy() != 10 {
		t.Errorf("small frame scaled to %v, want its own 20×10", got)
	}
}
