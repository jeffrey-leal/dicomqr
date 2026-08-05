package main

// Tests for the main window's opening geometry. The arithmetic is separated
// from the Windows calls precisely so it can be checked here; the one test
// that does call Windows only asserts the result is plausible, since the real
// value depends on the machine running the test.

import "testing"

// sized is the current-window rectangle for a placement that is also setting
// the size, where the window's own size is irrelevant.
var sized = winRect{}

func TestMainWindowGeometry(t *testing.T) {
	// A 1920x1080 screen with a 40px taskbar at the bottom.
	area := winRect{Left: 0, Top: 0, Right: 1920, Bottom: 1040}
	x, y, w, h := mainWindowPlacement(area, sized, nil, 0.60, 0.80, true)

	if w != 1152 { // 60% of 1920
		t.Errorf("width = %d, want 1152 (60%% of the work area)", w)
	}
	if h != 832 { // 80% of 1040
		t.Errorf("height = %d, want 832 (80%% of the work area)", h)
	}

	// Vertically centred: equal margins above and below.
	above, below := y-area.Top, area.Bottom-(y+h)
	if above != below {
		t.Errorf("vertical margins %d above, %d below — want them equal", above, below)
	}

	// Horizontally left of centre, but not flush against the edge.
	left, right := x-area.Left, area.Right-(x+w)
	if left >= right {
		t.Errorf("left margin %d, right margin %d — want the window left of centre", left, right)
	}
	if left == 0 {
		t.Errorf("window is flush against the left edge; some margin was intended")
	}

	// Entirely inside the work area.
	if x < area.Left || y < area.Top || x+w > area.Right || y+h > area.Bottom {
		t.Errorf("geometry %dx%d at %d,%d falls outside the work area %+v", w, h, x, y, area)
	}
}

// TestMainWindowGeometryRespectsWorkAreaOrigin covers a taskbar docked left or
// top, where the work area does not start at 0,0 — the window must be placed
// relative to that origin rather than to the screen corner.
func TestMainWindowGeometryRespectsWorkAreaOrigin(t *testing.T) {
	area := winRect{Left: 80, Top: 30, Right: 1920, Bottom: 1080}
	x, y, w, h := mainWindowPlacement(area, sized, nil, 0.60, 0.80, true)

	if x < area.Left {
		t.Errorf("x = %d, left of the work area origin %d", x, area.Left)
	}
	if y < area.Top {
		t.Errorf("y = %d, above the work area origin %d", y, area.Top)
	}
	if x+w > area.Right || y+h > area.Bottom {
		t.Errorf("geometry %dx%d at %d,%d overflows the work area %+v", w, h, x, y, area)
	}
	above, below := y-area.Top, area.Bottom-(y+h)
	if above != below {
		t.Errorf("vertical margins %d/%d — want them equal within the work area", above, below)
	}
}

// TestMainWindowGeometryOddSizes guards the integer arithmetic against a work
// area whose leftover pixels do not divide evenly.
func TestMainWindowGeometryOddSizes(t *testing.T) {
	for _, area := range []winRect{
		{Right: 1, Bottom: 1},
		{Right: 1367, Bottom: 769},
		{Left: 7, Top: 3, Right: 2561, Bottom: 1441},
	} {
		x, y, w, h := mainWindowPlacement(area, sized, nil, 0.60, 0.80, true)
		if w < 0 || h < 0 {
			t.Errorf("area %+v gave a negative size %dx%d", area, w, h)
		}
		if x < area.Left || y < area.Top || x+w > area.Right || y+h > area.Bottom {
			t.Errorf("area %+v gave %dx%d at %d,%d, outside it", area, w, h, x, y)
		}
	}
}

// TestMainWindowPlacementKeepsRestoredSizeOnScreen is the regression test for
// the status bar disappearing under the taskbar. With a size restored from
// settings the placement must centre the window on the height it actually has,
// not on the height it would have had if it were being sized — the original
// fault, which left the bottom of a tall window overhanging the work area.
//
// The numbers are the ones that produced the report: a 2560x1440 screen with a
// 48px taskbar, and a window restored to roughly 1480x1264 plus its frame.
func TestMainWindowPlacementKeepsRestoredSizeOnScreen(t *testing.T) {
	area := winRect{Left: 0, Top: 0, Right: 2560, Bottom: 1392}
	cur := winRect{Left: 256, Top: 139, Right: 256 + 1496, Bottom: 139 + 1300}

	_, y, w, h := mainWindowPlacement(area, cur, nil, 0.60, 0.80, false)

	if w != 1496 || h != 1300 {
		t.Errorf("size = %dx%d, want the window's own 1496x1300 left alone", w, h)
	}
	if y+h > area.Bottom {
		t.Errorf("window spans y=%d..%d, past the work area bottom %d — the status bar "+
			"would sit under the taskbar", y, y+h, area.Bottom)
	}
	above, below := y-area.Top, area.Bottom-(y+h)
	if above != below {
		t.Errorf("vertical margins %d above, %d below — the restored size should still centre", above, below)
	}
}

// TestMainWindowPlacementRestoresSavedPosition covers the second half of the
// report: a window the user moved and closed properly must reopen where they
// left it, not back at the default placement.
func TestMainWindowPlacementRestoresSavedPosition(t *testing.T) {
	area := winRect{Left: 0, Top: 0, Right: 2560, Bottom: 1392}
	cur := winRect{Left: 0, Top: 0, Right: 1200, Bottom: 900}
	saved := &winPoint{X: 900, Y: 40}

	x, y, w, h := mainWindowPlacement(area, cur, saved, 0.60, 0.80, false)

	if x != saved.X || y != saved.Y {
		t.Errorf("placed at %d,%d, want the saved %d,%d", x, y, saved.X, saved.Y)
	}
	if w != 1200 || h != 900 {
		t.Errorf("size = %dx%d, want the window's own 1200x900", w, h)
	}
}

// TestMainWindowPlacementClampsStalePosition covers a saved position that no
// longer fits — the monitor it was on has gone, or the taskbar has moved. The
// window must be pulled back on screen rather than opening off it.
func TestMainWindowPlacementClampsStalePosition(t *testing.T) {
	area := winRect{Left: 0, Top: 0, Right: 1920, Bottom: 1040}
	cur := winRect{Left: 0, Top: 0, Right: 1200, Bottom: 900}

	for _, saved := range []*winPoint{
		{X: 3000, Y: 100}, // was on a second monitor to the right
		{X: -400, Y: 100}, // was on one to the left
		{X: 100, Y: 1000}, // below what is now the work area
		{X: 100, Y: -300}, // above it
	} {
		x, y, w, h := mainWindowPlacement(area, cur, saved, 0.60, 0.80, false)
		if x < area.Left || y < area.Top || x+w > area.Right || y+h > area.Bottom {
			t.Errorf("saved %+v placed at %d,%d (%dx%d), outside the work area %+v",
				*saved, x, y, w, h, area)
		}
	}
}

// TestMainWindowPlacementShrinksOversizeWindow covers a restored size larger
// than the current screen — a window last used on a bigger monitor. It has to
// be shrunk, since no position could fit it whole.
func TestMainWindowPlacementShrinksOversizeWindow(t *testing.T) {
	area := winRect{Left: 0, Top: 0, Right: 1366, Bottom: 728}
	cur := winRect{Left: 0, Top: 0, Right: 2400, Bottom: 1300}

	x, y, w, h := mainWindowPlacement(area, cur, nil, 0.60, 0.80, false)

	if w > area.Right-area.Left || h > area.Bottom-area.Top {
		t.Errorf("size %dx%d still exceeds the work area %dx%d",
			w, h, area.Right-area.Left, area.Bottom-area.Top)
	}
	if x < area.Left || y < area.Top || x+w > area.Right || y+h > area.Bottom {
		t.Errorf("placed at %d,%d (%dx%d), outside the work area", x, y, w, h)
	}
}

// TestScreenWorkArea calls Windows for real. It cannot assert exact numbers —
// they are whatever this machine reports — only that the call works and the
// rectangle makes sense, which is what the placement depends on.
func TestScreenWorkArea(t *testing.T) {
	area, ok := screenWorkArea()
	if !ok {
		t.Skip("no work area reported (headless session?)")
	}
	if area.Right <= area.Left || area.Bottom <= area.Top {
		t.Fatalf("work area %+v is empty or inverted", area)
	}
	w, h := area.Right-area.Left, area.Bottom-area.Top
	if w < 320 || h < 240 {
		t.Errorf("work area %dx%d is implausibly small", w, h)
	}
	t.Logf("work area %dx%d at %d,%d", w, h, area.Left, area.Top)
}

// TestFindProcessWindowAbsent checks the lookup fails cleanly rather than
// blocking or returning a stray handle when no window matches — the test
// binary has no windows at all.
func TestFindProcessWindowAbsent(t *testing.T) {
	if hwnd, ok := findProcessWindow("dicomqr-no-such-window-title"); ok || hwnd != 0 {
		t.Errorf("found a window (%v) for a title that cannot exist", hwnd)
	}
}
