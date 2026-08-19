package main

// A weighted admission budget for work whose cost is known before it starts.
//
// The modification worker pool is the caller. Its cost per file is not the file
// size but the decompressed pixel size, which for a multi-frame acquisition can
// be a hundred times larger — decompressPixelData holds every frame at once, and
// recompressPixelData then holds the re-encoded output alongside them. Four such
// files in flight is several gigabytes, and running out of memory kills the
// export outright rather than failing one file.
//
// So the pool admits by weight rather than by count: each worker declares what
// it is about to allocate and waits until that much is free. Many small files
// still run at full width; large ones queue behind each other.

import "sync"

// memBudget admits work weighted by the bytes it is about to allocate. Safe for
// concurrent use; the zero value is not usable — call newMemBudget.
type memBudget struct {
	mu    sync.Mutex
	cond  *sync.Cond
	limit int64
	used  int64
}

func newMemBudget(limit int64) *memBudget {
	if limit < 1 {
		limit = 1
	}
	b := &memBudget{limit: limit}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// acquire blocks until n bytes are free, then reserves them and returns the
// function that gives them back. The caller must defer that function
// immediately: a release that never runs blocks every later file for the rest
// of the run.
//
// A request larger than the whole budget is clamped to it and runs alone rather
// than deadlocking against a limit it could never satisfy — the one file is
// going to allocate what it allocates either way, and holding everything else
// back while it does is the best available outcome.
//
// There is deliberately no context here. Cancelling a run stops the feed loop,
// not the files already in flight, and budget is only ever held by a worker
// that is making progress — whose release is deferred and therefore runs even
// if it panics. A waiter always drains, so ctx-awareness would be machinery
// guarding a case that cannot arise.
func (b *memBudget) acquire(n int64) (release func()) {
	if b == nil {
		return func() {}
	}
	if n < 0 {
		n = 0
	}
	b.mu.Lock()
	if n > b.limit {
		n = b.limit
	}
	for b.used+n > b.limit && b.used > 0 {
		b.cond.Wait()
	}
	b.used += n
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.used -= n
			if b.used < 0 {
				b.used = 0
			}
			b.mu.Unlock()
			// Broadcast rather than Signal: waiters want different amounts, so
			// the one woken might not be the one this release unblocks.
			b.cond.Broadcast()
		})
	}
}

// inFlight reports the bytes currently reserved. Tests use it to assert the
// limit is respected; nothing in the application reads it.
func (b *memBudget) inFlight() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
