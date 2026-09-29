package main

// Tests for cine playback arithmetic and the clip buffer. cineStep is pure, so
// wrapping and sweeping are checked exhaustively without a timer or a toolkit;
// the player's own tests cover only what its state machine owns.

import (
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"
)

func TestCineStepForwardWraps(t *testing.T) {
	pos := cinePosition{frame: 0, direction: 1}
	want := []int{1, 2, 3, 0, 1, 2, 3, 0}
	for i, w := range want {
		pos = cineStep(pos, 0, 3, false, 1)
		if pos.frame != w {
			t.Fatalf("step %d: frame = %d, want %d", i+1, pos.frame, w)
		}
		if pos.direction != 1 {
			t.Fatalf("step %d: looping playback must always run forward", i+1)
		}
	}
}

// A stall must cost the same as a single frame: the position is computed in
// closed form, not by iterating.
func TestCineStepMultipleStepsMatchOneAtATime(t *testing.T) {
	for _, bounce := range []bool{false, true} {
		for steps := 1; steps <= 25; steps++ {
			oneAtATime := cinePosition{frame: 2, direction: 1}
			for i := 0; i < steps; i++ {
				oneAtATime = cineStep(oneAtATime, 0, 7, bounce, 1)
			}
			atOnce := cineStep(cinePosition{frame: 2, direction: 1}, 0, 7, bounce, steps)
			if atOnce != oneAtATime {
				t.Errorf("bounce=%v steps=%d: %+v, want %+v", bounce, steps, atOnce, oneAtATime)
			}
		}
	}
}

// Sweeping runs to the end, turns, runs back to the start, and turns again —
// never repeating an endpoint twice in a row.
func TestCineStepSweeps(t *testing.T) {
	pos := cinePosition{frame: 0, direction: 1}
	want := []int{1, 2, 3, 2, 1, 0, 1, 2, 3, 2}
	for i, w := range want {
		pos = cineStep(pos, 0, 3, true, 1)
		if pos.frame != w {
			t.Fatalf("step %d: frame = %d, want %d (sequence so far diverged)", i+1, pos.frame, w)
		}
	}
}

// Playback is confined to the trim range, which may be a window inside the clip.
func TestCineStepHonoursTrimRange(t *testing.T) {
	pos := cinePosition{frame: 10, direction: 1}
	for i := 0; i < 20; i++ {
		pos = cineStep(pos, 10, 14, false, 1)
		if pos.frame < 10 || pos.frame > 14 {
			t.Fatalf("frame %d escaped the trim range 10..14", pos.frame)
		}
	}
	// Sweeping stays inside it too.
	pos = cinePosition{frame: 10, direction: 1}
	for i := 0; i < 20; i++ {
		pos = cineStep(pos, 10, 14, true, 1)
		if pos.frame < 10 || pos.frame > 14 {
			t.Fatalf("sweeping frame %d escaped the trim range 10..14", pos.frame)
		}
	}
}

// A position parked outside the range (the user scrubbed there) is pulled back
// in rather than running away.
func TestCineStepClampsPositionIntoRange(t *testing.T) {
	pos := cineStep(cinePosition{frame: 99, direction: 1}, 10, 14, false, 1)
	if pos.frame < 10 || pos.frame > 14 {
		t.Errorf("frame = %d, want a position inside 10..14", pos.frame)
	}
	pos = cineStep(cinePosition{frame: -5, direction: 1}, 10, 14, false, 1)
	if pos.frame < 10 || pos.frame > 14 {
		t.Errorf("frame = %d, want a position inside 10..14", pos.frame)
	}
}

// A still has nothing to play; stepping it must not move or divide by anything.
func TestCineStepSingleFrame(t *testing.T) {
	pos := cineStep(cinePosition{frame: 0, direction: 1}, 0, 0, false, 5)
	if pos.frame != 0 {
		t.Errorf("frame = %d, want 0", pos.frame)
	}
	pos = cineStep(cinePosition{frame: 3, direction: 1}, 3, 3, true, 5)
	if pos.frame != 3 {
		t.Errorf("frame = %d, want 3", pos.frame)
	}
}

func TestFrameIntervalNeverZero(t *testing.T) {
	for _, fps := range []float64{0, -1, 1e9} {
		if d := frameInterval(fps); d < time.Millisecond {
			t.Errorf("frameInterval(%v) = %v, want at least 1ms (a zero-period ticker panics)", fps, d)
		}
	}
	if got, want := frameInterval(30), time.Second/30; got != want {
		t.Errorf("frameInterval(30) = %v, want %v", got, want)
	}
}

func TestUsableFPSClamps(t *testing.T) {
	if got := usableFPS(0); got != defaultCineFPS {
		t.Errorf("usableFPS(0) = %v, want the default %v", got, defaultCineFPS)
	}
	if got := usableFPS(-3); got != defaultCineFPS {
		t.Errorf("usableFPS(-3) = %v, want the default %v", got, defaultCineFPS)
	}
	if got := usableFPS(9999); got != maxCineFPS {
		t.Errorf("usableFPS(9999) = %v, want %v", got, maxCineFPS)
	}
	if got := usableFPS(0.1); got != minCineFPS {
		t.Errorf("usableFPS(0.1) = %v, want %v", got, minCineFPS)
	}
}

func TestPlayerConfigureStopsAndPositions(t *testing.T) {
	p := newCinePlayer()
	p.configure(2, 8, 30, true, 5)
	if p.isRunning() {
		t.Error("configure must leave playback stopped")
	}
	if p.currentFrame() != 5 {
		t.Errorf("currentFrame = %d, want 5", p.currentFrame())
	}
	if p.currentFPS() != 30 {
		t.Errorf("fps = %v, want 30", p.currentFPS())
	}
	if !p.isBounce() {
		t.Error("bounce was not applied")
	}
	// A start frame outside the loop range is clamped into it.
	p.configure(2, 8, 30, false, 99)
	if p.currentFrame() != 8 {
		t.Errorf("currentFrame = %d, want the range end 8", p.currentFrame())
	}
}

func TestPlayerSetRangeKeepsPosition(t *testing.T) {
	p := newCinePlayer()
	p.configure(0, 99, 30, false, 40)
	// A clip too large to buffer whole: the loop is confined to what fits.
	p.setRange(0, 20)
	if p.currentFrame() != 20 {
		t.Errorf("currentFrame = %d, want to be pulled back to 20", p.currentFrame())
	}
	p.setCurrentFrame(5)
	p.setRange(0, 30)
	if p.currentFrame() != 5 {
		t.Errorf("currentFrame = %d, want 5 (widening must not move the position)", p.currentFrame())
	}
}

func TestPlayerSetCurrentFrameClampsToRange(t *testing.T) {
	p := newCinePlayer()
	p.configure(10, 20, 30, false, 10)
	p.setCurrentFrame(50)
	if p.currentFrame() != 20 {
		t.Errorf("currentFrame = %d, want 20", p.currentFrame())
	}
	p.setCurrentFrame(0)
	if p.currentFrame() != 10 {
		t.Errorf("currentFrame = %d, want 10", p.currentFrame())
	}
}

// Stopping an already-stopped player, and starting an already-running one, must
// both be no-ops — the transport calls them from several paths.
func TestPlayerStartStopIdempotent(t *testing.T) {
	p := newCinePlayer()
	p.configure(0, 9, 60, false, 0)
	p.stopPlayback() // not running
	p.start()
	if !p.isRunning() {
		t.Fatal("player did not start")
	}
	p.start() // already running
	p.stopPlayback()
	if p.isRunning() {
		t.Fatal("player did not stop")
	}
	p.stopPlayback()
}

// The player and the clip buffer together: frames must arrive in playback
// order, only ever ones the buffer holds, and stop arriving when playback does.
func TestPlayerDeliversBufferedFramesInOrder(t *testing.T) {
	test.NewApp() // fyne.Do runs the frame callback inline under the test driver

	// 20 fps rather than faster, so the most one late tick may advance
	// (cineMaxCatchup's worth, 5 frames) is well short of the 12-frame loop: a
	// forward skip then stays distinguishable from playing out of order.
	const frames = 12
	path := writeMultiframeTestFile(t, t.TempDir(), frames, 1)
	c := chapter{path: path, frames: frames, fps: 20, loopTo: frames - 1}
	buf := startClipBuffer(c, nil)
	waitForBuffer(t, buf)

	var mu sync.Mutex
	var seen []int
	p := newCinePlayer()
	p.setReadiness(func(frame int) bool { return buf.isReady(frame) })
	p.setOnFrame(func(frame int) {
		mu.Lock()
		seen = append(seen, frame)
		mu.Unlock()
	})
	p.configure(c.loopFrom, c.loopTo, c.fps, false, 0)
	p.start()
	time.Sleep(300 * time.Millisecond)
	p.stopPlayback()
	time.Sleep(50 * time.Millisecond) // let any in-flight tick finish

	mu.Lock()
	got := append([]int(nil), seen...)
	mu.Unlock()

	if len(got) < 3 {
		t.Fatalf("only %d frames delivered in 300ms at 20 fps: %v", len(got), got)
	}
	maxStep := int((cineMaxCatchup + frameInterval(c.fps) - 1) / frameInterval(c.fps))
	for i, frame := range got {
		if frame < 0 || frame >= frames {
			t.Fatalf("delivered frame %d is outside the clip", frame)
		}
		if !buf.isReady(frame) {
			t.Errorf("delivered frame %d was never buffered", frame)
		}
		if i > 0 {
			// Forward playback advances, wrapping at the end. Usually by one
			// frame, but the player turns elapsed time into frames, so a tick
			// that arrives late (a loaded test machine) legitimately advances
			// by several — up to maxStep. What must never happen is a step
			// backwards, which modulo the loop reads as more than maxStep.
			prev := got[i-1]
			if step := (frame - prev + frames) % frames; step < 1 || step > maxStep {
				t.Errorf("frame %d followed %d: a step of %d, want forward by 1..%d", frame, prev, step, maxStep)
			}
		}
	}

	// Nothing may arrive after the stop.
	mu.Lock()
	countAtStop := len(seen)
	mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	countLater := len(seen)
	mu.Unlock()
	if countLater != countAtStop {
		t.Errorf("%d frames arrived after playback stopped", countLater-countAtStop)
	}
}

// The readiness gate holds playback at a frame that has not been decoded rather
// than skipping ahead to whatever happens to be buffered.
func TestPlayerHoldsOnUnreadyFrame(t *testing.T) {
	p := newCinePlayer()
	p.configure(0, 9, 60, false, 0)
	p.setReadiness(func(frame int) bool { return false })
	p.setOnFrame(func(frame int) { t.Errorf("frame %d delivered while nothing was ready", frame) })
	p.start()
	time.Sleep(120 * time.Millisecond)
	p.stopPlayback()
	if p.currentFrame() != 0 {
		t.Errorf("currentFrame = %d, want 0 — playback must hold, not advance", p.currentFrame())
	}
}
