package main

// Owned application windows.
//
// Fyne has no modal windows — its Window interface offers no parent, no modal
// flag and no transient-for — so a panel promoted from a dialog to a real
// window can be stranded when whatever opened it closes, and cannot stop the
// user working behind it. This file supplies both, in two separable parts.
//
// Ownership. Each window records the window that opened it, and closing a
// window closes its descendants first. The cascade hangs off SetOnClosed
// rather than SetCloseIntercept: an intercept fires only for the title-bar
// button, never for a programmatic Close, so it would miss most of the ways a
// window actually goes away. A window opened under a key is raised rather than
// built a second time.
//
// Blocking, opt-in per window. While a blocking child is open, a dimming
// overlay is pushed onto its parent's canvas. Fyne resizes a non-popup overlay
// to the whole canvas, and while any overlay exists it routes pointer input to
// that overlay alone — the same mechanism its own dialogs use — so the parent
// stays visible but inert. Window decorations sit outside the canvas, so a
// blocked parent can still be closed from its title bar; the cascade means
// that takes its children with it instead of orphaning them.

import (
	"image/color"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
)

// windowSpec describes a window to open.
type windowSpec struct {
	// Key identifies the window: opening the same key again raises what is
	// already on screen instead of building a rival copy. Empty leaves the
	// window anonymous, and it may then be opened any number of times.
	Key   string
	Title string
	Size  fyne.Size
	// Parent owns this window. Nil makes it top level, owned by nothing.
	Parent fyne.Window
	// Blocking dims the parent and swallows its input while this window is
	// open. Use it where the child edits state the parent also owns; leave it
	// off for a window meant to be consulted while working, such as a log.
	Blocking bool
	// OnClosed runs once the window and its descendants have gone.
	OnClosed func()
}

type ownedWindowState struct {
	spec     windowSpec
	children []fyne.Window
}

var (
	ownedMu      sync.Mutex
	ownedWindows = map[fyne.Window]*ownedWindowState{}
	ownedKeys    = map[string]fyne.Window{}
	blockCount   = map[fyne.Window]int{}
	blockOverlay = map[fyne.Window]fyne.CanvasObject{}
)

// raiseOwnedWindow focuses the window already open under key and reports
// whether there was one. Callers whose content is expensive to build check
// this before doing the work; openOwnedWindow makes the same check itself.
func raiseOwnedWindow(key string) bool {
	if key == "" {
		return false
	}
	ownedMu.Lock()
	win, ok := ownedKeys[key]
	ownedMu.Unlock()
	if !ok {
		return false
	}
	win.RequestFocus()
	return true
}

// openOwnedWindow shows content in a window owned per spec, or raises the
// window already open under spec.Key. build receives the new window so the
// content can parent its own dialogs and children to it; it is not called when
// an existing window is raised.
func openOwnedWindow(a fyne.App, spec windowSpec, build func(win fyne.Window) fyne.CanvasObject) fyne.Window {
	if spec.Key != "" {
		ownedMu.Lock()
		existing, ok := ownedKeys[spec.Key]
		ownedMu.Unlock()
		if ok {
			existing.RequestFocus()
			return existing
		}
	}

	win := a.NewWindow(spec.Title)

	ownedMu.Lock()
	ownedWindows[win] = &ownedWindowState{spec: spec}
	if spec.Key != "" {
		ownedKeys[spec.Key] = win
	}
	if spec.Parent != nil {
		// The parent may be a window this helper never opened — the main
		// window is the usual case — so give it a state to hang children off.
		//
		// Such a parent gets no cascade: closing it will not close these
		// children, because installing the close hook that drives the cascade
		// would silently replace whatever hook the app already set on it
		// (Fyne's SetOnClosed replaces rather than chains). That is why the
		// main window is the only unowned parent used here — its closing ends
		// the process anyway. Anything that must cascade has to be opened
		// through this helper itself.
		ps, ok := ownedWindows[spec.Parent]
		if !ok {
			ps = &ownedWindowState{}
			ownedWindows[spec.Parent] = ps
		}
		ps.children = append(ps.children, win)
	}
	ownedMu.Unlock()

	if spec.Blocking && spec.Parent != nil {
		blockWindow(spec.Parent)
	}

	win.SetOnClosed(func() { releaseOwnedWindow(win) })
	win.SetContent(build(win))
	if spec.Size.Width > 0 || spec.Size.Height > 0 {
		win.Resize(spec.Size)
	}
	win.Show()
	return win
}

// releaseOwnedWindow runs when win has closed: it drops win from the registry,
// closes any descendants, releases the parent it was blocking, and finally
// runs the caller's OnClosed. Deregistering before closing children keeps a
// child's own cleanup from finding a half-removed parent.
func releaseOwnedWindow(win fyne.Window) {
	ownedMu.Lock()
	state, ok := ownedWindows[win]
	if !ok {
		ownedMu.Unlock()
		return // already released; Close is not guaranteed to fire once
	}
	delete(ownedWindows, win)
	if state.spec.Key != "" && ownedKeys[state.spec.Key] == win {
		delete(ownedKeys, state.spec.Key)
	}
	if parent := state.spec.Parent; parent != nil {
		if ps, ok := ownedWindows[parent]; ok {
			ps.children = removeWindowFrom(ps.children, win)
		}
	}
	children := state.children
	ownedMu.Unlock()

	for _, child := range children {
		child.Close()
	}
	if state.spec.Blocking && state.spec.Parent != nil {
		unblockWindow(state.spec.Parent)
	}
	if state.spec.OnClosed != nil {
		state.spec.OnClosed()
	}
}

func removeWindowFrom(list []fyne.Window, win fyne.Window) []fyne.Window {
	out := list[:0]
	for _, w := range list {
		if w != win {
			out = append(out, w)
		}
	}
	return out
}

// blockWindow makes win inert: a full-canvas overlay dims it and takes its
// pointer input, and its focus is dropped so keystrokes stop reaching whatever
// was typed into last. Blocking children are counted, so the overlay lifts
// only with the last of them.
func blockWindow(win fyne.Window) {
	ownedMu.Lock()
	blockCount[win]++
	first := blockCount[win] == 1
	var dim fyne.CanvasObject
	if first {
		dim = canvas.NewRectangle(color.NRGBA{A: 96})
		blockOverlay[win] = dim
	}
	ownedMu.Unlock()

	if first {
		win.Canvas().Overlays().Add(dim)
		win.Canvas().Unfocus()
	}
}

// unblockWindow releases one block on win, lifting the overlay with the last.
func unblockWindow(win fyne.Window) {
	ownedMu.Lock()
	blockCount[win]--
	last := blockCount[win] <= 0
	var dim fyne.CanvasObject
	if last {
		delete(blockCount, win)
		dim = blockOverlay[win]
		delete(blockOverlay, win)
	}
	ownedMu.Unlock()

	if dim != nil {
		win.Canvas().Overlays().Remove(dim)
	}
}
