package main

// cinePlayer drives cine playback of one chapter: a ticker goroutine that
// converts elapsed wall time into frame advances at the chapter's rate and
// reports each new frame to a listener on the Fyne UI goroutine.
//
// Advancing by however much time really passed — rather than one frame per tick
// — matters at 30-60 fps, where scheduler jitter is otherwise visible as
// judder. All the position arithmetic lives in cineStep, which is pure and free
// of any timer or UI dependency, so wrapping, sweeping and rate changes are
// unit-testable without a toolkit.

import (
	"sync"
	"time"
)

// cineMaxCatchup caps the elapsed time a single tick may act on. Without it, a
// window left minimised (or a long GC pause) banks seconds of playback and then
// fast-forwards through them the moment ticking resumes.
const cineMaxCatchup = 250 * time.Millisecond

// cinePosition is where playback is and which way it is going (+1 forward, -1
// backward — backward only ever arises while sweeping).
type cinePosition struct {
	frame     int
	direction int
}

type cinePlayer struct {
	mu       sync.Mutex
	loopFrom int
	loopTo   int
	fps      float64
	bounce   bool
	current  int
	dir      int
	running  bool

	// stop closes to end the current ticker goroutine; generation guards against
	// a goroutine that has already been told to stop delivering a late frame.
	stop       chan struct{}
	generation int

	// onFrame receives each new frame index on the UI goroutine.
	onFrame func(frame int)
	// ready gates progressive buffering: playback holds rather than advancing
	// past a frame that has not been decoded yet. nil means everything is ready.
	ready func(frame int) bool
}

func newCinePlayer() *cinePlayer {
	return &cinePlayer{fps: defaultCineFPS, dir: 1}
}

// setOnFrame sets the listener. Called from the UI goroutine before playback.
func (p *cinePlayer) setOnFrame(fn func(frame int)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onFrame = fn
}

func (p *cinePlayer) setReadiness(fn func(frame int) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ready = fn
}

// configure points the player at a chapter's playback range and timing. It
// stops playback: the caller decides whether the new chapter should start.
func (p *cinePlayer) configure(loopFrom, loopTo int, fps float64, bounce bool, startFrame int) {
	p.stopPlayback()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loopFrom = maxInt(0, loopFrom)
	p.loopTo = maxInt(p.loopFrom, loopTo)
	p.fps = usableFPS(fps)
	p.bounce = bounce
	p.current = clampInt(startFrame, p.loopFrom, p.loopTo)
	p.dir = 1
}

// setRange narrows or widens the loop without stopping playback or moving the
// current frame — unlike configure, which is a full chapter change. Used when a
// clip turns out to be too large to buffer whole and the loop has to be
// confined to the frames that fit.
func (p *cinePlayer) setRange(loopFrom, loopTo int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loopFrom = maxInt(0, loopFrom)
	p.loopTo = maxInt(p.loopFrom, loopTo)
	p.current = clampInt(p.current, p.loopFrom, p.loopTo)
}

// setFPS changes the rate mid-playback without disturbing the current position.
func (p *cinePlayer) setFPS(fps float64) {
	p.mu.Lock()
	p.fps = usableFPS(fps)
	running := p.running
	p.mu.Unlock()
	if running {
		// Restart the ticker so the new interval takes effect immediately rather
		// than after one tick of the old one.
		p.stopPlayback()
		p.start()
	}
}

func (p *cinePlayer) currentFPS() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fps
}

func (p *cinePlayer) setBounce(bounce bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bounce = bounce
	p.dir = 1
}

func (p *cinePlayer) isBounce() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bounce
}

func (p *cinePlayer) isRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// setCurrentFrame syncs the player to a frame the user moved to by hand (slider
// drag, arrow key, filmstrip click) so resuming carries on from where they left
// off rather than from where playback stopped.
func (p *cinePlayer) setCurrentFrame(frame int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current = clampInt(frame, p.loopFrom, p.loopTo)
}

func (p *cinePlayer) currentFrame() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current
}

// start begins playback. Safe to call when already running (no-op).
func (p *cinePlayer) start() {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	// A position parked outside the preferred trim range (the user scrubbed
	// there) has to come back inside it before playback can loop sensibly.
	p.current = clampInt(p.current, p.loopFrom, p.loopTo)
	p.running = true
	p.generation++
	gen := p.generation
	stop := make(chan struct{})
	p.stop = stop
	interval := frameInterval(p.fps)
	p.mu.Unlock()

	go p.run(gen, stop, interval)
}

// stopPlayback ends playback. Safe to call when not running (no-op).
func (p *cinePlayer) stopPlayback() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	p.running = false
	close(p.stop)
	p.stop = nil
	p.generation++
	p.mu.Unlock()
}

// run is the ticker goroutine. It owns no state of its own: every read and
// write goes through the mutex, and a stale generation makes it exit silently
// rather than deliver a frame for a chapter that has been switched away from.
func (p *cinePlayer) run(gen int, stop chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	last := time.Now()
	var owed time.Duration

	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			elapsed := now.Sub(last)
			last = now
			if elapsed > cineMaxCatchup {
				elapsed = cineMaxCatchup
			}
			if elapsed <= 0 {
				continue
			}
			owed += elapsed

			p.mu.Lock()
			if !p.running || p.generation != gen {
				p.mu.Unlock()
				return
			}
			perFrame := frameInterval(p.fps)
			steps := int(owed / perFrame)
			if steps <= 0 {
				p.mu.Unlock()
				continue
			}
			owed -= time.Duration(steps) * perFrame

			next := cineStep(cinePosition{frame: p.current, direction: p.dir},
				p.loopFrom, p.loopTo, p.bounce, steps)
			if next.frame == p.current {
				p.mu.Unlock()
				continue
			}
			if p.ready != nil && !p.ready(next.frame) {
				// The decoder has not reached this frame yet — hold here and let it
				// catch up rather than skipping ahead to whatever is buffered.
				owed = 0
				p.mu.Unlock()
				continue
			}
			p.current = next.frame
			p.dir = next.direction
			frame := next.frame
			notify := p.onFrame
			p.mu.Unlock()

			if notify != nil {
				postUI(func() { notify(frame) })
			}
		}
	}
}

// cineStep advances pos by steps frames within the inclusive range [from, to],
// wrapping at the end (or reversing at both ends, when bounce).
//
// Computed in closed form rather than by iterating steps times, so a long stall
// costs the same as a single frame. Sweeping is modelled as a triangular wave:
// a phase runs over a period of 2*(span-1), the first half ascending and the
// second descending, which makes both turning points fall out of the arithmetic
// instead of needing special cases.
func cineStep(pos cinePosition, from, to int, bounce bool, steps int) cinePosition {
	span := to - from + 1
	if span <= 1 || steps <= 0 {
		return cinePosition{frame: from, direction: 1} // a still, or nothing to do
	}
	offset := clampInt(pos.frame-from, 0, span-1)

	if !bounce {
		return cinePosition{frame: from + (offset+steps)%span, direction: 1}
	}

	period := 2 * (span - 1)
	// Descending positions live in the second half of the period; frame 0 going
	// backward is the wrap point, which is phase 0 rather than the (out-of-range)
	// full period.
	phase := offset
	if pos.direction < 0 {
		phase = (period - offset) % period
	}
	phase = (phase + steps) % period
	frame := phase
	if phase >= span {
		frame = period - phase
	}
	direction := 1
	if phase >= span-1 {
		direction = -1
	}
	return cinePosition{frame: from + frame, direction: direction}
}

// frameInterval is the wall-clock gap between frames at fps, floored at 1 ms so
// a corrupt rate cannot produce a zero-period ticker (which panics).
func frameInterval(fps float64) time.Duration {
	d := time.Duration(float64(time.Second) / usableFPS(fps))
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d
}

func usableFPS(fps float64) float64 {
	if fps <= 0 {
		return defaultCineFPS
	}
	return clampFPS(fps)
}
