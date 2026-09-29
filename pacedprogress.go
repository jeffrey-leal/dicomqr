package main

import "time"

// startPacedProgress calls publish every interval, from one goroutine, until the
// returned stop is called. stop waits for that goroutine to finish, so no
// publish is still running — or starts — once it returns.
//
// This is the one shape every progress report handed to fyne.Do should take:
// workers only count (an atomic), and this single reporter turns the count into
// UI updates at a fixed rate. Fyne's queue is unbounded and never blocks, so a
// report triggered per item — or per N items — emits at whatever rate the work
// runs at, and the UI goroutine has to drain every one of them; when each
// re-texts a label whose width changes, every one also repaints the whole
// window frame on Windows (see stableMin). See scanProgressInterval for the
// field report that made this a rule.
//
// The ticker can miss the end, so a caller that wants its last report to say
// "done" publishes that once itself after stop returns.
//
// A nil publish or a non-positive interval starts nothing; stop is then a no-op,
// so callers need no special case for "no progress wanted".
func startPacedProgress(interval time.Duration, publish func()) (stop func()) {
	if publish == nil || interval <= 0 {
		return func() {}
	}
	quit := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ticker.C:
				publish()
			}
		}
	}()
	stopped := false
	return func() {
		if stopped {
			return
		}
		stopped = true
		close(quit)
		<-exited
	}
}
