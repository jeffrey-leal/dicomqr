package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMemBudgetRespectsLimit is the property the whole thing exists for: however
// many goroutines pile in, the bytes reserved at any instant never exceed the
// limit. Each holder records a high-water mark, so a breach is caught even if it
// lasts microseconds.
func TestMemBudgetRespectsLimit(t *testing.T) {
	const (
		limit  = 100
		weight = 30 // three fit, the fourth must wait
		N      = 40
	)
	b := newMemBudget(limit)

	var live, peak int64
	var wg sync.WaitGroup
	for range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := b.acquire(weight)
			defer release()

			now := atomic.AddInt64(&live, weight)
			for {
				old := atomic.LoadInt64(&peak)
				if now <= old || atomic.CompareAndSwapInt64(&peak, old, now) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt64(&live, -weight)
		}()
	}
	wg.Wait()

	if peak > limit {
		t.Errorf("peak in flight = %d, over the %d limit", peak, limit)
	}
	if peak < weight {
		t.Errorf("peak in flight = %d — nothing ever ran", peak)
	}
	if got := b.inFlight(); got != 0 {
		t.Errorf("in flight = %d after every release, want 0", got)
	}
}

// TestMemBudgetOversizedRunsAlone: a file bigger than the whole budget must not
// deadlock against a limit it can never satisfy. It is clamped and runs on its
// own — the allocation happens either way, and holding everything else back
// while it does is the best available outcome.
func TestMemBudgetOversizedRunsAlone(t *testing.T) {
	b := newMemBudget(100)

	done := make(chan struct{})
	go func() {
		defer close(done)
		release := b.acquire(10_000) // far over the limit
		defer release()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an oversized acquire deadlocked instead of running alone")
	}
	if got := b.inFlight(); got != 0 {
		t.Errorf("in flight = %d after release, want 0", got)
	}
}

// TestMemBudgetReleaseUnblocks proves a waiter is actually woken by a release
// rather than by luck of scheduling: the second acquire cannot complete until
// the first has given its bytes back.
func TestMemBudgetReleaseUnblocks(t *testing.T) {
	b := newMemBudget(100)
	release := b.acquire(100) // the whole budget

	acquired := make(chan struct{})
	go func() {
		second := b.acquire(100)
		defer second()
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("the second acquire went through while the budget was fully held")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("releasing the budget did not wake the waiter")
	}
}

// TestMemBudgetReleasedOnPanic: the engine's worker relies on its deferred
// release running while a panic unwinds. If it did not, one panicking file
// would starve every file after it for the rest of the run.
func TestMemBudgetReleasedOnPanic(t *testing.T) {
	b := newMemBudget(100)

	func() {
		defer func() { _ = recover() }()
		release := b.acquire(100)
		defer release()
		panic("synthetic")
	}()

	if got := b.inFlight(); got != 0 {
		t.Fatalf("in flight = %d after a panicking holder, want 0", got)
	}
	// And the budget is usable again.
	release := b.acquire(100)
	release()
}

// TestMemBudgetDoubleReleaseIsHarmless — release is idempotent, so a caller that
// both defers it and calls it cannot drive the accounting negative and hand out
// budget that does not exist.
func TestMemBudgetDoubleReleaseIsHarmless(t *testing.T) {
	b := newMemBudget(100)
	release := b.acquire(60)
	release()
	release()
	if got := b.inFlight(); got != 0 {
		t.Fatalf("in flight = %d after a double release, want 0", got)
	}
}

// A nil budget is the no-limiter case the engine uses for runs that never
// decompress; acquiring from one must be a no-op rather than a crash.
func TestMemBudgetNilIsNoOp(t *testing.T) {
	var b *memBudget
	release := b.acquire(1 << 30)
	release()
}
