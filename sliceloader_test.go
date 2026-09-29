package main

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeDecoder stands in for decodeViewerSlice: records every decode, and holds
// any key given a gate until the test releases it.
type fakeDecoder struct {
	mu      sync.Mutex
	calls   []viewerSlice
	demand  map[viewerSlice]bool // decoded with interactive=true
	gates   map[viewerSlice]chan struct{}
	started chan viewerSlice
	fail    map[viewerSlice]bool
	bytes   int // sample bytes of each decoded frame
}

func newFakeDecoder() *fakeDecoder {
	return &fakeDecoder{
		demand:  map[viewerSlice]bool{},
		gates:   map[viewerSlice]chan struct{}{},
		started: make(chan viewerSlice, 64),
		fail:    map[viewerSlice]bool{},
		bytes:   100,
	}
}

func (f *fakeDecoder) gate(k viewerSlice) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := make(chan struct{})
	f.gates[k] = g
	return g
}

func (f *fakeDecoder) decode(k viewerSlice, interactive bool) (viewerState, error) {
	f.mu.Lock()
	f.calls = append(f.calls, k)
	if interactive {
		f.demand[k] = true
	}
	g := f.gates[k]
	fail := f.fail[k]
	f.mu.Unlock()
	f.started <- k
	if g != nil {
		<-g
	}
	if fail {
		return viewerState{}, errors.New("unreadable")
	}
	return viewerState{frame: &decodedFrame{rows: 1, cols: f.bytes / 2, rawIdx: make([]uint16, f.bytes/2)}}, nil
}

func (f *fakeDecoder) count(k viewerSlice) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == k {
			n++
		}
	}
	return n
}

type delivery struct {
	key viewerSlice
	err error
}

func startLoader(t *testing.T, budget int64, f *fakeDecoder) (*sliceLoader, chan delivery) {
	t.Helper()
	got := make(chan delivery, 16)
	l := newSliceLoader(budget, f.decode, func(k viewerSlice, _ viewerState, err error) {
		got <- delivery{k, err}
	})
	t.Cleanup(l.stop)
	return l, got
}

func expectDelivery(t *testing.T, got chan delivery, want viewerSlice) delivery {
	t.Helper()
	select {
	case d := <-got:
		if d.key != want {
			t.Fatalf("delivered %v, want %v", d.key, want)
		}
		return d
	case <-time.After(5 * time.Second):
		t.Fatalf("no delivery of %v", want)
	}
	return delivery{}
}

func expectNoDelivery(t *testing.T, got chan delivery) {
	t.Helper()
	select {
	case d := <-got:
		t.Fatalf("unexpected delivery of %v", d.key)
	case <-time.After(100 * time.Millisecond):
	}
}

func waitStarted(t *testing.T, f *fakeDecoder, want viewerSlice) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case k := <-f.started:
			if k == want {
				return
			}
		case <-deadline:
			t.Fatalf("decode of %v never started", want)
		}
	}
}

func key(i int) viewerSlice { return viewerSlice{path: "s" + string(rune('a'+i)), frame: 0} }

// Latest wins: a request superseded while it decodes is cached but never
// shown, and only the slice now on the slider is delivered.
func TestSliceLoaderDeliversOnlyTheLatestRequest(t *testing.T) {
	f := newFakeDecoder()
	gateA := f.gate(key(0))
	l, got := startLoader(t, 1<<20, f)

	l.request(key(0), nil)
	waitStarted(t, f, key(0))
	l.request(key(1), nil) // the user scrolled on while A decodes
	expectDelivery(t, got, key(1))

	close(gateA)
	expectNoDelivery(t, got)
	if _, ok := l.get(key(0)); !ok {
		t.Error("the superseded slice was decoded but not kept")
	}
	if !f.demand[key(0)] || !f.demand[key(1)] {
		t.Error("requested slices must decode with interactive options")
	}
}

// A slice requested while it is already being read ahead is delivered by that
// decode — never decoded twice.
func TestSliceLoaderRequestJoinsReadAheadInFlight(t *testing.T) {
	f := newFakeDecoder()
	gate := f.gate(key(3))
	l, got := startLoader(t, 1<<20, f)

	l.request(key(0), []viewerSlice{key(3)})
	expectDelivery(t, got, key(0))
	waitStarted(t, f, key(3))
	l.request(key(3), nil)
	close(gate)
	expectDelivery(t, got, key(3))
	if n := f.count(key(3)); n != 1 {
		t.Errorf("slice decoded %d times, want once", n)
	}
	if f.demand[key(3)] {
		t.Error("read-ahead decoded with interactive options")
	}
}

// Idle workers fill the read-ahead plan; a cached slice is served by get and a
// request for it delivers nothing.
func TestSliceLoaderReadsAheadAndServesFromCache(t *testing.T) {
	f := newFakeDecoder()
	l, got := startLoader(t, 1<<20, f)

	l.request(key(0), []viewerSlice{key(1), key(2), key(3)})
	expectDelivery(t, got, key(0))
	// Two workers share the plan, so its slices can finish in any order: wait
	// for all of them.
	cached := func() bool {
		for i := 1; i <= 3; i++ {
			if _, ok := l.get(key(i)); !ok {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(5 * time.Second)
	for !cached() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !cached() {
		t.Fatal("read-ahead plan not cached")
	}
	l.request(key(2), nil)
	expectNoDelivery(t, got)
	if n := f.count(key(2)); n != 1 {
		t.Errorf("cached slice decoded %d times, want once", n)
	}
}

// The cache stays within budget by evicting the least recently used slice.
func TestSliceLoaderEvictsLeastRecentlyUsed(t *testing.T) {
	f := newFakeDecoder() // 100 bytes a slice
	l, got := startLoader(t, 250, f)
	for i := 0; i < 3; i++ {
		l.request(key(i), nil)
		expectDelivery(t, got, key(i))
		if i == 1 {
			l.get(key(0)) // touch 0, so 1 is now the oldest
		}
	}
	if _, ok := l.get(key(1)); ok {
		t.Error("least recently used slice survived eviction")
	}
	for _, i := range []int{0, 2} {
		if _, ok := l.get(key(i)); !ok {
			t.Errorf("slice %d evicted, want it kept", i)
		}
	}
}

// A failed decode is delivered as an error, and not cached.
func TestSliceLoaderDeliversErrors(t *testing.T) {
	f := newFakeDecoder()
	f.fail[key(0)] = true
	l, got := startLoader(t, 1<<20, f)
	l.request(key(0), nil)
	if d := expectDelivery(t, got, key(0)); d.err == nil {
		t.Fatal("decode error not delivered")
	}
	if _, ok := l.get(key(0)); ok {
		t.Error("a failed slice was cached")
	}
}

// stop waits for the workers and nothing is delivered afterwards.
func TestSliceLoaderStop(t *testing.T) {
	f := newFakeDecoder()
	gate := f.gate(key(0))
	got := make(chan delivery, 4)
	l := newSliceLoader(1<<20, f.decode, func(k viewerSlice, _ viewerState, err error) { got <- delivery{k, err} })
	l.request(key(0), nil)
	waitStarted(t, f, key(0))
	done := make(chan struct{})
	go func() { l.stop(); close(done) }()
	// Let the decode finish only once stop has marked the loader closed — the
	// case under test is a decode that completes after the window went away.
	for closed := false; !closed; {
		l.mu.Lock()
		closed = l.closed
		l.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return")
	}
	expectNoDelivery(t, got)
}

// The plan: next two in the scroll direction, the same slice in the other
// phases, further ahead, then two behind — clipped to the series.
func TestSliceReadAheadPlan(t *testing.T) {
	s := make([]viewerSlice, 20)
	p := make([]viewerSlice, 20)
	for i := range s {
		s[i] = viewerSlice{path: "a", frame: i}
		p[i] = viewerSlice{path: "b", frame: i}
	}
	names := func(v []viewerSlice) []string {
		var out []string
		for _, x := range v {
			out = append(out, fmt.Sprintf("%s%d", x.path, x.frame))
		}
		return out
	}
	// At slice 10 scrolling backwards, with one other phase.
	got := names(sliceReadAhead(s, 10, -1, [][]viewerSlice{p}))
	want := []string{
		"a9", "a8", // the next two in the scroll direction
		"b10",                              // the same slice in the other phase
		"a7", "a6", "a5", "a4", "a3", "a2", // further ahead
		"a11", "a12", // two behind
	}
	if !slices.Equal(got, want) {
		t.Errorf("plan = %v, want %v", got, want)
	}
	if plan := sliceReadAhead(s, 19, 1, nil); len(plan) != 2 || plan[0].frame != 18 || plan[1].frame != 17 {
		t.Errorf("plan at the end of the series = %v, want only the two behind", plan)
	}
}
