package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	netdicom "github.com/algm/go-netdicom"
)

// concurrencyMeter records the highest number of do calls running at once.
type concurrencyMeter struct {
	cur, peak atomic.Int32
}

func (m *concurrencyMeter) enter() {
	n := m.cur.Add(1)
	for {
		p := m.peak.Load()
		if n <= p || m.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

func (m *concurrencyMeter) leave() { m.cur.Add(-1) }

func states(res []targetResult) []targetState {
	out := make([]targetState, len(res))
	for i, r := range res {
		out[i] = r.state
	}
	return out
}

func TestRetrieveRunHonoursLimit(t *testing.T) {
	for _, limit := range []int{1, 2, 4} {
		var m concurrencyMeter
		r := newRetrieveRun(12, limit)
		r.run(context.Background(), func(ctx context.Context, idx int) error {
			m.enter()
			defer m.leave()
			time.Sleep(5 * time.Millisecond)
			return nil
		})
		if got := int(m.peak.Load()); got != limit {
			t.Errorf("limit %d: peak concurrency %d", limit, got)
		}
		for i, s := range states(r.outcome()) {
			if s != targetDone {
				t.Errorf("limit %d: target %d state %v, want done", limit, i, s)
			}
		}
		if started, _, degraded := r.status(); started != 12 || degraded {
			t.Errorf("limit %d: started=%d degraded=%v", limit, started, degraded)
		}
	}
}

func TestRetrieveRunLimitOneIsSequential(t *testing.T) {
	var mu sync.Mutex
	var order []int
	r := newRetrieveRun(6, 1)
	r.run(context.Background(), func(ctx context.Context, idx int) error {
		mu.Lock()
		order = append(order, idx)
		mu.Unlock()
		if idx == 2 {
			return errors.New("boom")
		}
		return nil
	})
	for i, idx := range order {
		if idx != i {
			t.Fatalf("order %v, want 0..5 in sequence (a failure must not be re-run at limit 1)", order)
		}
	}
	res := r.outcome()
	if res[2].state != targetFailed || r.degradedBy() != nil {
		t.Errorf("target 2 = %v, degradedBy = %v; want failed, never degraded", res[2], r.degradedBy())
	}
}

func TestRetrieveRunDegradesOnFirstFailure(t *testing.T) {
	refused := errors.New("association refused")
	var attempts [8]atomic.Int32
	var afterDegrade concurrencyMeter
	var degradedAt atomic.Bool
	r := newRetrieveRun(8, 3)
	r.run(context.Background(), func(ctx context.Context, idx int) error {
		n := attempts[idx].Add(1)
		if degradedAt.Load() {
			afterDegrade.enter()
			defer afterDegrade.leave()
		}
		// Target 1 is refused the first time, at once (a server allowing
		// fewer associations); target 5, which can only start after that,
		// fails every time (a genuinely bad target).
		if idx == 1 && n == 1 {
			degradedAt.Store(true)
			return refused
		}
		time.Sleep(20 * time.Millisecond)
		if idx == 5 {
			return errors.New("bad target")
		}
		return nil
	})
	res := r.outcome()
	if res[1].state != targetDone || attempts[1].Load() != 2 {
		t.Errorf("refused target: state %v after %d attempts; want done after 2", res[1].state, attempts[1].Load())
	}
	if res[5].state != targetFailed || attempts[5].Load() != 1 {
		t.Errorf("bad target: state %v after %d attempts; want failed after 1 (the degrade was already spent)",
			res[5].state, attempts[5].Load())
	}
	if !errors.Is(r.degradedBy(), refused) {
		t.Errorf("degradedBy = %v, want the refusal", r.degradedBy())
	}
	if _, limit, degraded := r.status(); limit != 1 || !degraded {
		t.Errorf("limit %d degraded %v after a failure; want 1, true", limit, degraded)
	}
	// Associations already open when the degrade happened may finish, but
	// nothing new may start beside them: allow those, never more.
	if p := afterDegrade.peak.Load(); p > 3 {
		t.Errorf("peak %d after degrade", p)
	}
}

// A server allowing one association refuses the second and third together;
// both refusals must be re-run, not just whichever reached the scheduler first.
func TestRetrieveRunRequeuesEverySimultaneousRefusal(t *testing.T) {
	var attempts [6]atomic.Int32
	r := newRetrieveRun(6, 3)
	r.run(context.Background(), func(ctx context.Context, idx int) error {
		n := attempts[idx].Add(1)
		if (idx == 1 || idx == 2) && n == 1 {
			time.Sleep(time.Duration(idx) * time.Millisecond) // distinct finish order
			return errors.New("local limit exceeded")
		}
		time.Sleep(5 * time.Millisecond)
		return nil
	})
	for i, s := range states(r.outcome()) {
		if s != targetDone {
			t.Errorf("target %d state %v after %d attempts, want done", i, s, attempts[i].Load())
		}
	}
	for _, i := range []int{1, 2} {
		if attempts[i].Load() != 2 {
			t.Errorf("refused target %d ran %d times, want 2", i, attempts[i].Load())
		}
	}
}

func TestRetrieveRunNewTargetsWaitForDegradeToDrain(t *testing.T) {
	// An attempt the scheduler started at one association (after the
	// degrade) must find no other running: it may only start once every
	// association open at the degrade has finished.
	var running atomic.Int32
	var overlapAfterDegrade atomic.Bool
	var failedOnce atomic.Bool
	r := newRetrieveRun(10, 4)
	startedParallel := func(idx int) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.startedParallel[idx]
	}
	r.run(context.Background(), func(ctx context.Context, idx int) error {
		n := running.Add(1)
		defer running.Add(-1)
		if !startedParallel(idx) && n > 1 {
			overlapAfterDegrade.Store(true)
		}
		time.Sleep(3 * time.Millisecond)
		if idx == 0 && failedOnce.CompareAndSwap(false, true) {
			return errors.New("refused")
		}
		return nil
	})
	if overlapAfterDegrade.Load() {
		t.Error("a target started after the degrade while another association was still open")
	}
	for i, s := range states(r.outcome()) {
		if s != targetDone {
			t.Errorf("target %d state %v, want done", i, s)
		}
	}
}

func TestRetrieveRunCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := newRetrieveRun(10, 2)
	var calls atomic.Int32
	r.run(ctx, func(tctx context.Context, idx int) error {
		if calls.Add(1) == 2 { // both associations open
			cancel()
		}
		<-tctx.Done() // a retrieve blocks until its association ends
		return tctx.Err()
	})
	var cancelled, notStarted int
	for _, s := range states(r.outcome()) {
		switch s {
		case targetCancelled:
			cancelled++
		case targetNotStarted:
			notStarted++
		default:
			t.Errorf("unexpected state %v after cancel", s)
		}
	}
	if cancelled == 0 || notStarted == 0 || cancelled+notStarted != 10 {
		t.Errorf("cancelled %d, not started %d", cancelled, notStarted)
	}
	if r.degradedBy() != nil {
		t.Error("a user cancel must not count as the server refusing")
	}
}

func TestRetrieveRunStallRequeuesOnce(t *testing.T) {
	r := newRetrieveRun(5, 3)
	var attempts [5]atomic.Int32
	inFlight := make(chan struct{}, 8)
	watchdogDone := make(chan struct{})
	defer func() { <-watchdogDone }()
	go func() {
		defer close(watchdogDone)
		// Once three associations are open and silent, the watchdog fires.
		for range 3 {
			<-inFlight
		}
		if !r.onStall() {
			t.Error("first stall under a parallel limit was not handled")
		}
		if r.onStall() {
			t.Error("second stall was handled; once degraded the watchdog must abort")
		}
	}()
	r.run(context.Background(), func(tctx context.Context, idx int) error {
		if attempts[idx].Add(1) == 1 && idx < 3 {
			inFlight <- struct{}{}
			<-tctx.Done() // silent until the watchdog takes it off the air
			return tctx.Err()
		}
		return nil
	})
	for i, s := range states(r.outcome()) {
		if s != targetDone {
			t.Errorf("target %d state %v, want done after its re-run", i, s)
		}
	}
	for i := range 3 {
		if attempts[i].Load() != 2 {
			t.Errorf("stalled target %d ran %d times, want 2", i, attempts[i].Load())
		}
	}
	if !errors.Is(r.degradedBy(), errParallelStall) {
		t.Errorf("degradedBy = %v", r.degradedBy())
	}
	if started, _, _ := r.status(); started != 5 {
		t.Errorf("started = %d; a re-run must not count as a new target", started)
	}
}

func TestRetrieveRunStallAtLimitOneIsNotHandled(t *testing.T) {
	if newRetrieveRun(3, 1).onStall() {
		t.Error("onStall handled a stall at limit 1; the watchdog must abort as it always has")
	}
}

// NewServiceUser canonicalises the syntaxes it is given in place, so the
// unrestricted proposal must never be the package-level default itself.
func TestProposedTransferSyntaxesReturnsCopy(t *testing.T) {
	a := proposedTransferSyntaxes(ServerProfile{})
	b := proposedTransferSyntaxes(ServerProfile{})
	if len(a) == 0 || &a[0] == &b[0] {
		t.Fatal("unrestricted proposals share one backing array")
	}
}

func TestParallelTransfersClamp(t *testing.T) {
	for in, want := range map[int]int{-3: 1, 0: 1, 1: 1, 3: 3, 4: 4, 9: maxParallelTransfers} {
		if got := (ServerProfile{ParallelTransfers: in}).parallelTransfers(); got != want {
			t.Errorf("ParallelTransfers %d -> %d, want %d", in, got, want)
		}
	}
}

func TestRetrieveStatusTextUnchangedAtOne(t *testing.T) {
	if got := retrieveStatusText("study", 3, 12, 1, 1, false); got != "Retrieving study 3/12\u2026" {
		t.Errorf("one association: %q", got)
	}
	if got := retrieveStatusText("series", 3, 12, 3, 3, false); got == retrieveStatusText("series", 3, 12, 1, 1, false) {
		t.Error("parallel status reads the same as sequential")
	}
}

func TestDescribeParallelDegrade(t *testing.T) {
	if describeParallelDegrade(nil) != "" {
		t.Error("clause for a run that never degraded")
	}
	rj := &netdicom.AssociationRejectedError{Result: 2, Source: 3, Reason: 2}
	if got := describeParallelDegrade(fmt.Errorf("c-move: %w", rj)); !strings.Contains(got, "local limit exceeded") {
		t.Errorf("refusal clause %q does not name the reason", got)
	}
	if got := describeParallelDegrade(errParallelStall); !strings.Contains(got, "stopped responding") {
		t.Errorf("stall clause %q", got)
	}
}

func TestDescribeConcurrentEcho(t *testing.T) {
	rj := &netdicom.AssociationRejectedError{Result: 2, Source: 3, Reason: 2}
	got := describeConcurrentEcho([]error{nil, nil, rj, errors.New("dial tcp: refused")})
	for _, want := range []string{"accepted 2 of 4", "#3: refused", "local limit exceeded", "#4: failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
	if got := describeConcurrentEcho([]error{nil, nil}); !strings.Contains(got, "all 2") {
		t.Errorf("all-accepted report:\n%s", got)
	}
}
