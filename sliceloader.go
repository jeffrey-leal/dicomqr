package main

import (
	"container/list"
	"sync"
)

// sliceLoader feeds the viewer's slice mode (a series of single-frame images):
// it decodes the slice the user asked for first, keeps recent slices decoded,
// and reads ahead in the direction they are scrolling.
//
// It replaced a goroutine per slider or wheel step. Those all queued on one
// file cache's mutex — which does not hand itself over in arrival order — and
// each posted its result without checking it was still wanted, so a fast
// scroll decoded and displayed every slice it passed and could come to rest on
// a slice other than the one the slider showed. And since every file in slice
// mode holds one frame, that one-file cache never hit: scrolling back re-read
// and re-decoded what had just been on screen.
//
// Three rules replace that:
//
//   - Latest wins. Only the most recent request is ever delivered; one
//     superseded while decoding is kept in the cache but not shown, and one
//     superseded before a worker reached it is never decoded at all.
//   - Recent slices stay decoded, least recently used evicted first, within
//     sliceCacheBudget — so scrolling back, or flipping MR phases at one slice,
//     costs nothing.
//   - Workers with nothing requested decode the read-ahead plan the viewer
//     supplies (the next slices in the scroll direction, the same slice in the
//     other phases), so a steady scroll is usually served from the cache.
//
// The requested slice is decoded with interactive options (every core on a
// JPEG 2000 image, since the user is waiting on it); read-ahead uses one thread
// per worker, so it never competes for cores with the slice on screen.
type sliceLoader struct {
	decode  func(key viewerSlice, interactive bool) (viewerState, error)
	deliver func(key viewerSlice, st viewerState, err error)

	mu       sync.Mutex
	wake     *sync.Cond
	entries  map[viewerSlice]*list.Element // value: *sliceEntry
	lru      *list.List                    // front = most recently used
	used     int64
	budget   int64
	want     viewerSlice // the slice to deliver, when hasWant
	hasWant  bool
	ahead    []viewerSlice // read-ahead plan, in priority order
	inflight map[viewerSlice]bool
	closed   bool
	workers  sync.WaitGroup
}

type sliceEntry struct {
	key   viewerSlice
	st    viewerState
	bytes int64
}

// sliceCacheBudget bounds the decoded slices a viewer window keeps: about 500
// indexed 512×512 slices, or a dozen 3000×3000 radiographs. A var so tests can
// exercise eviction.
var sliceCacheBudget int64 = 256 << 20

// sliceLoaderWorkers decode concurrently: one normally on the requested slice
// and one reading ahead. More would compete for the disk and cores the
// requested slice needs, for read-ahead the user may scroll away from.
const sliceLoaderWorkers = 2

// newSliceLoader starts a loader. decode produces one slice (interactive when
// the user is waiting on it); deliver is called, on a worker goroutine, with
// the result of the most recent request only.
func newSliceLoader(budget int64, decode func(viewerSlice, bool) (viewerState, error),
	deliver func(viewerSlice, viewerState, error)) *sliceLoader {
	l := &sliceLoader{
		decode:   decode,
		deliver:  deliver,
		entries:  map[viewerSlice]*list.Element{},
		lru:      list.New(),
		budget:   budget,
		inflight: map[viewerSlice]bool{},
	}
	l.wake = sync.NewCond(&l.mu)
	for range sliceLoaderWorkers {
		l.workers.Add(1)
		go l.work()
	}
	return l
}

// get returns key's decoded slice when it is cached, marking it recently used.
func (l *sliceLoader) get(key viewerSlice) (viewerState, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.entries[key]; ok {
		l.lru.MoveToFront(el)
		return el.Value.(*sliceEntry).st, true
	}
	return viewerState{}, false
}

// request makes key the slice to deliver — replacing any earlier request, which
// is then never delivered — unless it is already cached, in which case the
// caller has it from get and there is nothing to deliver. ahead replaces the
// read-ahead plan either way.
func (l *sliceLoader) request(key viewerSlice, ahead []viewerSlice) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, cached := l.entries[key]
	l.want, l.hasWant = key, !cached
	l.ahead = ahead
	l.wake.Broadcast()
}

// stop ends the workers once their current decode finishes, and waits for them.
// No delivery happens after it returns.
func (l *sliceLoader) stop() {
	l.mu.Lock()
	l.closed = true
	l.hasWant = false
	l.wake.Broadcast()
	l.mu.Unlock()
	l.workers.Wait()
}

// next picks a worker's job: the requested slice unless it is cached or another
// worker already has it, else the first read-ahead slice nobody has.
func (l *sliceLoader) next() (key viewerSlice, interactive, ok bool) {
	if l.hasWant && !l.inflight[l.want] {
		if _, cached := l.entries[l.want]; !cached {
			return l.want, true, true
		}
	}
	for _, k := range l.ahead {
		if _, cached := l.entries[k]; cached || l.inflight[k] {
			continue
		}
		return k, false, true
	}
	return viewerSlice{}, false, false
}

func (l *sliceLoader) work() {
	defer l.workers.Done()
	for {
		l.mu.Lock()
		key, interactive, ok := l.next()
		for !ok && !l.closed {
			l.wake.Wait()
			key, interactive, ok = l.next()
		}
		if l.closed {
			l.mu.Unlock()
			return
		}
		l.inflight[key] = true
		l.mu.Unlock()

		st, err := l.decode(key, interactive)

		l.mu.Lock()
		delete(l.inflight, key)
		if err == nil {
			l.insert(key, st)
		}
		// Checked after the decode, not before: a slice requested while it was
		// already being read ahead is delivered by that same decode.
		deliver := l.hasWant && l.want == key && !l.closed
		if deliver {
			l.hasWant = false
		}
		// Whatever this worker finished may unblock the others' choice.
		l.wake.Broadcast()
		l.mu.Unlock()

		if deliver {
			l.deliver(key, st, err)
		}
	}
}

// insert caches a decoded slice as the most recently used, evicting the least
// recently used until the cache fits its budget — never the slice just added.
func (l *sliceLoader) insert(key viewerSlice, st viewerState) {
	if el, ok := l.entries[key]; ok {
		l.lru.MoveToFront(el)
		return
	}
	e := &sliceEntry{key: key, st: st, bytes: int64(cachedFrameBytes(st.frame))}
	l.entries[key] = l.lru.PushFront(e)
	l.used += e.bytes
	for l.used > l.budget && l.lru.Len() > 1 {
		old := l.lru.Back()
		oe := old.Value.(*sliceEntry)
		l.lru.Remove(old)
		delete(l.entries, oe.key)
		l.used -= oe.bytes
	}
}

// cachedFrameBytes is what a decoded frame held in the slice cache occupies: its
// samples, or its colour image. Unlike frameBytes it leaves out the RGBA the
// viewport renders into, which exists once per window, not once per slice.
func cachedFrameBytes(df *decodedFrame) int {
	if df == nil {
		return 0
	}
	if df.colorImg != nil {
		return frameBytes(df)
	}
	return df.sampleBytes()
}

// sliceReadAhead is the read-ahead plan for the slice at idx, most useful first:
// the next two slices in the scroll direction, then the same slice in every
// other phase (so the P key flips instantly), then further ahead, then a couple
// behind in case the user reverses. dir is +1 or -1.
func sliceReadAhead(slices []viewerSlice, idx, dir int, phaseSlices [][]viewerSlice) []viewerSlice {
	const near, far, behind = 2, 8, 2
	var plan []viewerSlice
	add := func(s []viewerSlice, i int) {
		if i >= 0 && i < len(s) {
			plan = append(plan, s[i])
		}
	}
	for d := 1; d <= near; d++ {
		add(slices, idx+d*dir)
	}
	for _, ps := range phaseSlices {
		add(ps, idx)
	}
	for d := near + 1; d <= far; d++ {
		add(slices, idx+d*dir)
	}
	for d := 1; d <= behind; d++ {
		add(slices, idx-d*dir)
	}
	return plan
}
