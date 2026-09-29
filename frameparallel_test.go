package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// concurrencyProbe records how many frame functions run at once.
type concurrencyProbe struct {
	now, peak atomic.Int32
	mu        sync.Mutex
}

func (p *concurrencyProbe) enter() {
	n := p.now.Add(1)
	p.mu.Lock()
	if n > p.peak.Load() {
		p.peak.Store(n)
	}
	p.mu.Unlock()
}

func (p *concurrencyProbe) leave() { p.now.Add(-1) }

// Every frame runs exactly once, whoever runs it, and the helpers give back
// every token they borrowed.
func TestForEachFrameRunsEveryFrameOnce(t *testing.T) {
	tokens := newCPUTokens(4)
	release := tokens.hold() // the calling worker's own token
	defer release()

	const n = 200
	counts := make([]atomic.Int32, n)
	if err := forEachFrame(tokens, n, func(i int) error {
		counts[i].Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := range counts {
		if c := counts[i].Load(); c != 1 {
			t.Fatalf("frame %d ran %d times", i, c)
		}
	}
	if held := len(tokens); held != 1 {
		t.Errorf("%d tokens held after the call, want only the caller's own", held)
	}
}

// Frame work never exceeds the run's token count: the caller plus a helper per
// free token. With every token taken by other workers, the caller runs alone.
func TestForEachFrameStaysWithinTheTokens(t *testing.T) {
	for _, tc := range []struct {
		name     string
		capacity int
		held     int // tokens held by other workers, besides the caller's
		want     int32
	}{
		{"idle run: all four cores", 4, 0, 4},
		{"two other workers busy", 4, 2, 2},
		{"every worker busy", 4, 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokens := newCPUTokens(tc.capacity)
			release := tokens.hold()
			defer release()
			for range tc.held {
				defer tokens.hold()()
			}
			var probe concurrencyProbe
			forEachFrame(tokens, 32, func(int) error {
				probe.enter()
				defer probe.leave()
				time.Sleep(2 * time.Millisecond)
				return nil
			})
			if p := probe.peak.Load(); p > tc.want {
				t.Errorf("%d frames ran at once, want at most %d", p, tc.want)
			}
			if tc.want > 1 && probe.peak.Load() < 2 {
				t.Errorf("free tokens were not used: peak %d", probe.peak.Load())
			}
		})
	}
}

// The file fails with the lowest-numbered failing frame, exactly the error the
// serial loop reported, however the frames were spread.
func TestForEachFrameReportsTheLowestFailingFrame(t *testing.T) {
	tokens := newCPUTokens(8)
	for range 20 { // repeat: which helper reaches which frame varies
		err := forEachFrame(tokens, 64, func(i int) error {
			if i == 9 || i == 40 {
				return fmt.Errorf("frame %d: broken", i+1)
			}
			return nil
		})
		if err == nil || err.Error() != "frame 10: broken" {
			t.Fatalf("error = %v, want frame 10's", err)
		}
	}
}

// A panic on a helper must become that frame's error, not end the process —
// the worker's own recover cannot reach another goroutine.
func TestForEachFramePanicBecomesError(t *testing.T) {
	tokens := newCPUTokens(4)
	err := forEachFrame(tokens, 16, func(i int) error {
		if i == 5 {
			panic("codec blew up")
		}
		time.Sleep(time.Millisecond)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "frame 6: panic: codec blew up") {
		t.Fatalf("error = %v, want frame 6's recovered panic", err)
	}
	if held := len(tokens); held != 0 {
		t.Errorf("%d tokens still held after a panic", held)
	}
}

// Without a token set (the receive path) frames run one at a time, in order.
func TestForEachFrameWithoutTokensIsSerial(t *testing.T) {
	var order []int
	err := forEachFrame(nil, 5, func(i int) error {
		order = append(order, i) // unsynchronised on purpose: -race proves no helpers
		if i == 3 {
			return errors.New("stop")
		}
		return nil
	})
	if err == nil || fmt.Sprint(order) != "[0 1 2 3]" {
		t.Errorf("order %v, err %v; want 0..3 then the error", order, err)
	}
}
