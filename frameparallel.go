package main

import (
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// Frame-level parallelism inside a modification run.
//
// A multi-frame file used to go through decompression, and then re-encoding
// with decode-back verification, one frame after another on the one worker
// holding it. That is the slow shape of exactly the files that dominate a
// masking run — a 240-frame SPECT acquisition, a long echo clip — and because
// the memory budget admits an oversized file only once nothing else is in
// flight, such a file tends to run last, alone, on one core, while every
// other worker has finished and sits idle.
//
// cpuTokens lends those idle cores to it without ever exceeding the worker cap.
// A run holds one token per worker (modifyWorkerCount — already sized to be a
// good neighbour). A worker takes a token for as long as it processes a file;
// when it reaches frame-by-frame work it also takes whichever tokens happen to
// be free — never waiting for one — and runs a helper on each. While every
// worker is busy nothing is free and the work runs exactly as before; as the
// run drains, finished workers' tokens pass to whatever large file remains.
//
// Two properties follow from every unit of CPU work holding a token:
//   - CPU: never more than the worker count at once, so the export's use of
//     the machine is still what modifyWorkerCount allowed; helpers also run at
//     below-normal priority, like the workers (workerpriority.go).
//   - Memory: never more frames in flight run-wide than workers — the bound the
//     pool already had when each worker held one frame at a time — so the
//     memory budget's accounting (fileMemoryWeight) needs no change.
type cpuTokens chan struct{}

func newCPUTokens(n int) cpuTokens { return make(cpuTokens, max(1, n)) }

// hold takes a token for the caller's own work, waiting if need be, and returns
// its release. A nil set (the receive path, tests) imposes nothing.
func (t cpuTokens) hold() (release func()) {
	if t == nil {
		return func() {}
	}
	t <- struct{}{}
	return func() { <-t }
}

// forEachFrame runs fn for every frame index in [0, n): on the caller, plus a
// helper for each token free at the moment of the call. Frames are claimed in
// ascending order and each writes only its own result, so the output is the
// same whoever ran which frame. Returns the error of the lowest-numbered frame
// that failed — what the serial loop returned — and stops claiming new frames
// once any has failed, since the file fails as a whole either way.
//
// A panic in fn is recovered into that frame's error: on a helper goroutine it
// would otherwise end the process, since the worker's own recover (the per-file
// backstop in runModificationImpl) cannot reach another goroutine.
func forEachFrame(tokens cpuTokens, n int, fn func(i int) error) error {
	errs := make([]error, n)
	var next atomic.Int64
	var failed atomic.Bool
	run := func() {
		for !failed.Load() {
			i := int(next.Add(1)) - 1
			if i >= n {
				return
			}
			if errs[i] = runFrame(i, fn); errs[i] != nil {
				failed.Store(true)
			}
		}
	}

	var wg sync.WaitGroup
	if tokens != nil {
	spawn:
		for h := 1; h < n; h++ {
			select {
			case tokens <- struct{}{}:
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-tokens }()
					defer lowerWorkerPriority()()
					run()
				}()
			default:
				break spawn // nothing free: the caller carries on alone
			}
		}
	}
	run()
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func runFrame(i int, fn func(int) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("frame %d: panic: %v\n%s", i+1, r, debug.Stack())
		}
	}()
	return fn(i)
}
