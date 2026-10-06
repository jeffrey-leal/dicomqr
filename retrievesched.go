package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	netdicom "github.com/algm/go-netdicom"
)

// retrieveRun schedules a retrieve's targets onto up to `limit` concurrent
// associations (the server profile's Parallel transfers) and falls back to one
// association the first time the server shows it does not want more.
//
// DICOM gives no way to ask a server how many associations it allows: a
// refusal (A-ASSOCIATE-RJ "local limit exceeded" / "temporary congestion") is
// the only explicit signal, and plenty of servers instead accept and then
// abort, stall, or fail the extra association some other way. So a target
// failure of any kind, in an attempt that began while more than one
// association was allowed, is treated as possibly the server's doing: the run
// degrades to one association and that target is re-queued to run again,
// once. Associations already in flight are left to finish — they were
// working. A failure in an attempt begun at one association is an ordinary
// target failure, exactly as the sequential loop had.
//
// The run is network-free: do is whatever retrieves one target, so the
// scheduling is testable on its own (retrievesched_test.go).
type retrieveRun struct {
	mu   sync.Mutex
	cond *sync.Cond

	queue      []int // target indices waiting to start, in order
	limit      int   // associations allowed at once; 1 after a degrade
	active     int
	degraded   bool
	degradeErr error                           // what caused the degrade
	retried    map[int]bool                    // targets already given their one re-run
	inFlight   map[int]context.CancelCauseFunc // cancels a running target
	// startedParallel records, per running target, whether its attempt began
	// while more than one association was allowed. Such an attempt's failure
	// earns the re-run even after another failure has already degraded the
	// run: a server allowing one association refuses the second and third
	// together, and only one of them can be first.
	startedParallel map[int]bool
	started         int // distinct targets started so far
	results         []targetResult

	// onDegrade, if set before run, is told when the run drops to one
	// association. Called with the run's lock held: it must only log.
	onDegrade func(err error)
}

type targetState int

const (
	targetNotStarted targetState = iota
	targetDone
	targetFailed
	targetCancelled
)

type targetResult struct {
	state targetState
	err   error
}

// errRequeue is the cancel cause for in-flight targets taken off the air by a
// stall under a parallel limit; each is re-queued to run at one association.
var errRequeue = errors.New("re-queued to run at one association")

// errParallelStall is the degrade reason recorded for a stall.
var errParallelStall = errors.New("the server stopped responding to parallel associations")

func newRetrieveRun(n, limit int) *retrieveRun {
	r := &retrieveRun{
		queue:           make([]int, n),
		limit:           max(limit, 1),
		retried:         make(map[int]bool),
		inFlight:        make(map[int]context.CancelCauseFunc),
		startedParallel: make(map[int]bool),
		results:         make([]targetResult, n),
	}
	for i := range r.queue {
		r.queue[i] = i
	}
	r.cond = sync.NewCond(&r.mu)
	return r
}

// run retrieves every target and returns once none is running. Cancelling ctx
// stops the run: in-flight targets end as cancelled, queued ones never start.
func (r *retrieveRun) run(ctx context.Context, do func(ctx context.Context, idx int) error) {
	// cond.Wait does not watch ctx; wake every waiter when it is cancelled.
	stop := context.AfterFunc(ctx, func() {
		r.mu.Lock()
		r.cond.Broadcast()
		r.mu.Unlock()
	})
	defer stop()

	r.mu.Lock()
	workers := min(r.limit, len(r.queue))
	r.mu.Unlock()
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				idx, tctx, ok := r.next(ctx)
				if !ok {
					return
				}
				r.finish(ctx, idx, tctx, do(tctx, idx))
			}
		}()
	}
	wg.Wait()
}

// next blocks until a target may start under the current limit, and returns
// it with its own context. ok is false when the queue is empty or the run is
// cancelled — a worker then exits. A target re-queued later is picked up by
// the worker that re-queued it, which is still looping.
func (r *retrieveRun) next(ctx context.Context) (idx int, tctx context.Context, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.queue) > 0 && r.active >= r.limit && ctx.Err() == nil {
		r.cond.Wait()
	}
	if len(r.queue) == 0 || ctx.Err() != nil {
		return 0, nil, false
	}
	idx = r.queue[0]
	r.queue = r.queue[1:]
	r.active++
	if !r.retried[idx] {
		r.started++
	}
	tctx, cancel := context.WithCancelCause(ctx)
	r.inFlight[idx] = cancel
	r.startedParallel[idx] = r.limit > 1
	return idx, tctx, true
}

func (r *retrieveRun) finish(ctx context.Context, idx int, tctx context.Context, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cancel := r.inFlight[idx]; cancel != nil {
		cancel(nil)
		delete(r.inFlight, idx)
	}
	parallel := r.startedParallel[idx]
	delete(r.startedParallel, idx)
	r.active--
	defer r.cond.Broadcast()

	switch {
	case err == nil:
		r.results[idx] = targetResult{state: targetDone}
	case ctx.Err() != nil:
		r.results[idx] = targetResult{state: targetCancelled, err: err}
	case context.Cause(tctx) == errRequeue:
		r.requeue(idx)
	case parallel && !r.retried[idx]:
		if !r.degraded {
			r.degrade(err)
		}
		r.requeue(idx)
	default:
		r.results[idx] = targetResult{state: targetFailed, err: err}
	}
}

// requeue puts idx back at the front of the queue for its one re-run.
func (r *retrieveRun) requeue(idx int) {
	r.retried[idx] = true
	r.queue = append([]int{idx}, r.queue...)
}

func (r *retrieveRun) degrade(err error) {
	r.degraded = true
	r.degradeErr = err
	r.limit = 1
	if r.onDegrade != nil {
		r.onDegrade(err)
	}
}

// onStall is the stall watchdog's first resort. Under a parallel limit, a run
// in which every open association has gone silent may be a server that
// accepted associations it then never serves: degrade, cancel the in-flight
// targets and re-queue them to run one at a time, and report true so the
// watchdog keeps watching instead of aborting. Once degraded (or with a limit
// of one) it reports false and the watchdog aborts the run, as it always has.
func (r *retrieveRun) onStall() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.degraded || r.limit <= 1 {
		return false
	}
	r.degrade(errParallelStall)
	for _, cancel := range r.inFlight {
		cancel(errRequeue)
	}
	return true
}

// status reports progress for the status line: distinct targets started, the
// association limit now in force, and whether the run has degraded.
func (r *retrieveRun) status() (started, limit int, degraded bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started, r.limit, r.degraded
}

// degradedBy reports the failure that made the run drop to one association,
// or nil if it never did.
func (r *retrieveRun) degradedBy() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.degradeErr
}

// outcome returns every target's result, in target order. Call after run.
func (r *retrieveRun) outcome() []targetResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]targetResult(nil), r.results...)
}

// retrieveStatusText is the status line shown as a target starts. A run
// configured for one association reads exactly as the sequential loop did.
func retrieveStatusText(noun string, started, count, configured, limit int, degraded bool) string {
	switch {
	case configured <= 1:
		return fmt.Sprintf("Retrieving %s %d/%d…", noun, started, count)
	case degraded:
		return fmt.Sprintf("Retrieving %s %d/%d (one at a time — the server would not take more)…", noun, started, count)
	default:
		return fmt.Sprintf("Retrieving %s %d/%d (up to %d at once)…", noun, started, count, limit)
	}
}

// describeParallelDegrade is the completion-summary clause for a run that
// dropped to one association, naming why, or "" if it never did.
func describeParallelDegrade(err error) string {
	if err == nil {
		return ""
	}
	why := "an extra association failed"
	var rj *netdicom.AssociationRejectedError
	switch {
	case errors.As(err, &rj):
		why = "it refused another association: " + rj.ReasonText()
	case errors.Is(err, errParallelStall):
		why = "it stopped responding with several associations open"
	}
	return fmt.Sprintf(" — the server would not take parallel transfers (%s), so the rest ran one at a time; "+
		"if this recurs, set Parallel transfers to 1 for this server", why)
}
