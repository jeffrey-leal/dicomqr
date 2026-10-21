package main

// Remembered placement of every window other than the main one (user decision
// 2026-10-21): position and size, by window type, restored when a window of
// that type next opens and recorded when it closes. The main window keeps its
// own route (windowplacement.go, Settings.WindowX/Y/Width/Height).
//
// Keyed by type ("preferences", "viewer", …), never by title: a viewer's title
// can carry a patient's name, which has no place in settings.json. Several
// windows of one type share one entry — the next viewer opens where the last
// one closed — and a window opening onto the spot an open sibling occupies is
// stepped down and right, so it does not hide exactly behind it.
//
// Fyne has no position API, but since 2.5 a window implements
// driver.NativeWindow, whose RunNative hands over the HWND; everything else is
// the Win32 calls windowplacement.go already makes, in the same physical
// pixels. Fyne creates the native window only inside Show, so a placement is
// applied straight after Show returns — on the UI goroutine, before Fyne paints
// the first frame. It is recorded from the window's OnClosed, which Fyne runs
// while the native window still exists. A saved placement is clamped into the
// work area of the monitor it was on, or the nearest one if that monitor is
// gone, exactly as the main window's is.
//
// Fyne's message boxes and confirmations are overlays drawn inside their
// window rather than windows, so they follow their window instead of
// remembering a place of their own.

import (
	"maps"
	"sync"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver"
)

var (
	procIsIconic = user32.NewProc("IsIconic")
	procIsZoomed = user32.NewProc("IsZoomed")
)

// windowCascadeStep is how far, in physical pixels, a window is stepped down
// and right off a sibling of the same type it would otherwise cover exactly.
const windowCascadeStep = 32

// windowPlacement is a window's outer rectangle in physical pixels, as
// GetWindowRect reports it and SetWindowPos takes it.
type windowPlacement struct {
	X int32 `json:"x"`
	Y int32 `json:"y"`
	W int32 `json:"width"`
	H int32 `json:"height"`
}

// placedWindow is an open window whose placement is recorded when it closes.
type placedWindow struct {
	kind string
	hwnd uintptr
}

var (
	placementMu   sync.Mutex
	placements    = map[string]windowPlacement{}
	placedWindows = map[fyne.Window]placedWindow{}
)

// initWindowPlacements loads the placements saved by an earlier session.
func initWindowPlacements(saved map[string]windowPlacement) {
	placementMu.Lock()
	defer placementMu.Unlock()
	placements = maps.Clone(saved)
	if placements == nil {
		placements = map[string]windowPlacement{}
	}
}

// snapshotWindowPlacements records every window still open and returns all
// placements, for the settings saved as the application closes — the windows
// still open then close after that save.
func snapshotWindowPlacements() map[string]windowPlacement {
	placementMu.Lock()
	open := make([]placedWindow, 0, len(placedWindows))
	for _, pw := range placedWindows {
		open = append(open, pw)
	}
	placementMu.Unlock()
	for _, pw := range open {
		if p, ok := currentWindowPlacement(pw.hwnd); ok {
			placementMu.Lock()
			placements[pw.kind] = p
			placementMu.Unlock()
		}
	}
	placementMu.Lock()
	defer placementMu.Unlock()
	if len(placements) == 0 {
		return nil
	}
	return maps.Clone(placements)
}

// nativeWindowHandle returns win's HWND, or 0 before it has been shown or
// under a driver with no native window (the test driver).
func nativeWindowHandle(win fyne.Window) uintptr {
	nw, ok := win.(driver.NativeWindow)
	if !ok {
		return 0
	}
	var hwnd uintptr
	nw.RunNative(func(ctx any) {
		if wc, ok := ctx.(driver.WindowsWindowContext); ok {
			hwnd = wc.HWND
		}
	})
	return hwnd
}

func nativeWindowRect(hwnd uintptr) (winRect, bool) {
	var r winRect
	ret, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r, ret != 0
}

// currentWindowPlacement reads hwnd's rectangle. A minimised or maximised
// window reports none: a minimised one sits off screen at -32000, and a
// maximised one's rectangle is the monitor's, not a size to reopen at — the
// placement it had before is kept instead.
func currentWindowPlacement(hwnd uintptr) (windowPlacement, bool) {
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		return windowPlacement{}, false
	}
	if zoomed, _, _ := procIsZoomed.Call(hwnd); zoomed != 0 {
		return windowPlacement{}, false
	}
	r, ok := nativeWindowRect(hwnd)
	if !ok || r.Right <= r.Left || r.Bottom <= r.Top {
		return windowPlacement{}, false
	}
	return windowPlacement{X: r.Left, Y: r.Top, W: r.Right - r.Left, H: r.Bottom - r.Top}, true
}

// placeWindow puts win, just shown, where the last window of kind was, and
// registers it so its placement is recorded when it closes
// (recordWindowPlacement). A kind never placed before leaves the window where
// Fyne and Windows put it. Must run on the UI goroutine, straight after Show.
func placeWindow(win fyne.Window, kind string) {
	if kind == "" {
		return
	}
	hwnd := nativeWindowHandle(win)
	if hwnd == 0 {
		return
	}
	placementMu.Lock()
	saved, ok := placements[kind]
	var siblings []uintptr
	for _, pw := range placedWindows {
		if pw.kind == kind {
			siblings = append(siblings, pw.hwnd)
		}
	}
	placedWindows[win] = placedWindow{kind: kind, hwnd: hwnd}
	placementMu.Unlock()
	if !ok || saved.W <= 0 || saved.H <= 0 {
		return
	}
	area, ok := monitorWorkAreaAt(winPoint{X: saved.X, Y: saved.Y})
	if !ok {
		return
	}
	var occupied []winRect
	for _, h := range siblings {
		if r, ok := nativeWindowRect(h); ok {
			occupied = append(occupied, r)
		}
	}
	x, y, w, h := cascadedPlacement(saved, occupied, area)
	if ret, _, err := procSetWindowPos.Call(hwnd, 0,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		swpNoZOrder|swpNoActivate); ret == 0 {
		logWarn("window placement: could not restore the %s window: %v", kind, err)
	}
}

// showPlacedWindow shows win and remembers its placement under kind, for a
// window with no OnClosed of its own: it installs one that records the
// placement. A window that has one calls placeWindow after Show and
// recordWindowPlacement from it instead — Fyne's SetOnClosed replaces, it
// does not chain.
func showPlacedWindow(win fyne.Window, kind string) {
	win.SetOnClosed(func() { recordWindowPlacement(win) })
	win.Show()
	placeWindow(win, kind)
}

// recordWindowPlacement stores win's placement under its kind and forgets the
// window. Call it from the window's OnClosed; a window placeWindow never
// registered, or one already recorded, is ignored.
func recordWindowPlacement(win fyne.Window) {
	placementMu.Lock()
	pw, ok := placedWindows[win]
	delete(placedWindows, win)
	placementMu.Unlock()
	if !ok {
		return
	}
	if p, ok := currentWindowPlacement(pw.hwnd); ok {
		placementMu.Lock()
		placements[pw.kind] = p
		placementMu.Unlock()
	}
}

// cascadedPlacement clamps saved into area, then steps it down and right past
// any open sibling whose origin it would land on, re-clamping each step. It
// gives up after a few steps, so a screen already full of viewers stacks the
// next on the last rather than looping. Free of Windows calls, for testing.
func cascadedPlacement(saved windowPlacement, siblings []winRect, area winRect) (x, y, w, h int32) {
	clamp := func(x, y int32) (int32, int32, int32, int32) {
		cur := winRect{Left: x, Top: y, Right: x + saved.W, Bottom: y + saved.H}
		return mainWindowPlacement(area, cur, &winPoint{X: x, Y: y}, 0, 0, false)
	}
	covers := func(x, y int32) bool {
		for _, s := range siblings {
			if abs32(s.Left-x) < windowCascadeStep/2 && abs32(s.Top-y) < windowCascadeStep/2 {
				return true
			}
		}
		return false
	}
	x, y, w, h = clamp(saved.X, saved.Y)
	for i := 0; i < 8 && covers(x, y); i++ {
		x, y, w, h = clamp(x+windowCascadeStep, y+windowCascadeStep)
	}
	return x, y, w, h
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}
