package main

// Tests for the owned-window helper. Fyne has no modal windows, so these cover
// the two properties the helper exists to provide: a parent never closes
// leaving a child stranded, and a blocking child renders its parent inert.

import (
	"runtime"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

// resetOwnedWindows clears the registry so each test starts clean; the maps
// are package state shared by every window the app opens.
func resetOwnedWindows(t *testing.T) {
	t.Helper()
	reset := func() {
		ownedMu.Lock()
		defer ownedMu.Unlock()
		ownedWindows = map[fyne.Window]*ownedWindowState{}
		ownedKeys = map[string]fyne.Window{}
		blockCount = map[fyne.Window]int{}
		blockOverlay = map[fyne.Window]fyne.CanvasObject{}
	}
	reset()
	t.Cleanup(reset)
}

func stubContent(fyne.Window) fyne.CanvasObject { return widget.NewLabel("content") }

func ownedChildCount(win fyne.Window) int {
	ownedMu.Lock()
	defer ownedMu.Unlock()
	if s, ok := ownedWindows[win]; ok {
		return len(s.children)
	}
	return 0
}

func ownedChildren(win fyne.Window) []fyne.Window {
	ownedMu.Lock()
	defer ownedMu.Unlock()
	if s, ok := ownedWindows[win]; ok {
		return append([]fyne.Window(nil), s.children...)
	}
	return nil
}

func ownedRegistered(win fyne.Window) bool {
	ownedMu.Lock()
	defer ownedMu.Unlock()
	_, ok := ownedWindows[win]
	return ok
}

// TestOwnedWindowCascadeClose is the orphan guard: closing a parent must take
// its children, and their children, with it.
func TestOwnedWindowCascadeClose(t *testing.T) {
	a := test.NewApp()
	resetOwnedWindows(t)

	root := a.NewWindow("root")
	parent := openOwnedWindow(a, windowSpec{Title: "parent", Parent: root}, stubContent)
	child := openOwnedWindow(a, windowSpec{Title: "child", Parent: parent}, stubContent)
	grandchild := openOwnedWindow(a, windowSpec{Title: "grandchild", Parent: child}, stubContent)

	if got := ownedChildCount(root); got != 1 {
		t.Errorf("root children = %d, want 1", got)
	}
	if got := ownedChildCount(parent); got != 1 {
		t.Errorf("parent children = %d, want 1", got)
	}

	parent.Close()

	for name, win := range map[string]fyne.Window{
		"parent": parent, "child": child, "grandchild": grandchild,
	} {
		if ownedRegistered(win) {
			t.Errorf("%s still registered after the parent closed", name)
		}
	}
	if got := ownedChildCount(root); got != 0 {
		t.Errorf("root children = %d after its child closed, want 0", got)
	}
}

// TestOwnedWindowClosingChildLeavesParent checks the cascade only runs downward.
func TestOwnedWindowClosingChildLeavesParent(t *testing.T) {
	a := test.NewApp()
	resetOwnedWindows(t)

	root := a.NewWindow("root")
	parent := openOwnedWindow(a, windowSpec{Title: "parent", Parent: root}, stubContent)
	child := openOwnedWindow(a, windowSpec{Title: "child", Parent: parent}, stubContent)

	child.Close()
	if !ownedRegistered(parent) {
		t.Errorf("parent was closed along with its child")
	}
	if got := ownedChildCount(parent); got != 0 {
		t.Errorf("parent children = %d after the child closed, want 0", got)
	}
}

// TestOwnedWindowKeyRaises covers the anti-duplicate rule: a keyed window opens
// once, and a second attempt raises it without rebuilding the content.
func TestOwnedWindowKeyRaises(t *testing.T) {
	a := test.NewApp()
	resetOwnedWindows(t)

	builds := 0
	build := func(fyne.Window) fyne.CanvasObject {
		builds++
		return widget.NewLabel("content")
	}
	spec := windowSpec{Key: "editor:x", Title: "x"}

	first := openOwnedWindow(a, spec, build)
	second := openOwnedWindow(a, spec, build)

	if first != second {
		t.Errorf("second open produced a different window")
	}
	if builds != 1 {
		t.Errorf("content built %d times, want 1", builds)
	}
	if !raiseOwnedWindow("editor:x") {
		t.Errorf("raiseOwnedWindow reported no window under an open key")
	}

	first.Close()
	if raiseOwnedWindow("editor:x") {
		t.Errorf("key still resolves after the window closed")
	}
	// The key is free again, so a later open builds afresh.
	openOwnedWindow(a, spec, build)
	if builds != 2 {
		t.Errorf("content built %d times after reopening, want 2", builds)
	}
}

// TestOwnedWindowBlocking covers the simulated modality: a blocking child puts
// an overlay on its parent, and only the last one to close removes it. Fyne
// routes pointer input to the top overlay alone, so the overlay's presence is
// what makes the parent inert.
func TestOwnedWindowBlocking(t *testing.T) {
	a := test.NewApp()
	resetOwnedWindows(t)

	parent := a.NewWindow("parent")
	parent.SetContent(widget.NewLabel("parent"))

	if got := len(parent.Canvas().Overlays().List()); got != 0 {
		t.Fatalf("parent starts with %d overlays, want 0", got)
	}

	first := openOwnedWindow(a, windowSpec{Title: "a", Parent: parent, Blocking: true}, stubContent)
	if got := len(parent.Canvas().Overlays().List()); got != 1 {
		t.Fatalf("blocking child added %d overlays, want 1", got)
	}

	// A second blocking child must not stack a second overlay, and closing it
	// must not unblock while the first is still open.
	second := openOwnedWindow(a, windowSpec{Title: "b", Parent: parent, Blocking: true}, stubContent)
	if got := len(parent.Canvas().Overlays().List()); got != 1 {
		t.Errorf("overlays = %d with two blocking children, want 1", got)
	}
	second.Close()
	if got := len(parent.Canvas().Overlays().List()); got != 1 {
		t.Errorf("overlay lifted while a blocking child was still open (overlays = %d)", got)
	}

	first.Close()
	if got := len(parent.Canvas().Overlays().List()); got != 0 {
		t.Errorf("overlays = %d after the last blocking child closed, want 0", got)
	}
}

// TestOwnedWindowNonBlocking is the other half: a window meant to be consulted
// while working must leave its parent usable.
func TestOwnedWindowNonBlocking(t *testing.T) {
	a := test.NewApp()
	resetOwnedWindows(t)

	parent := a.NewWindow("parent")
	parent.SetContent(widget.NewLabel("parent"))

	win := openOwnedWindow(a, windowSpec{Title: "log", Parent: parent}, stubContent)
	if got := len(parent.Canvas().Overlays().List()); got != 0 {
		t.Errorf("non-blocking child added %d overlays, want 0", got)
	}
	win.Close()
}

// TestActivityLogWindow covers the non-blocking conversion: the log opens as
// its own window that leaves the main window usable, opens only once, and
// stops its refresh ticker however it is closed — previously only the Close
// button did that, so any other route leaked the goroutine for the session.
func TestActivityLogWindow(t *testing.T) {
	a := test.NewApp()
	resetOwnedWindows(t)

	root := a.NewWindow("main")
	root.SetContent(widget.NewLabel("main"))

	showLogDialog(a, root)
	if got := ownedChildCount(root); got != 1 {
		t.Fatalf("log windows open = %d, want 1", got)
	}
	if got := len(root.Canvas().Overlays().List()); got != 0 {
		t.Errorf("the log blocked the main window (overlays = %d); it is meant to be "+
			"consulted while working", got)
	}

	// A second invocation raises the window rather than opening another.
	showLogDialog(a, root)
	if got := ownedChildCount(root); got != 1 {
		t.Errorf("log windows open = %d after a second invocation, want 1", got)
	}

	// Closing by any route must stop the ticker; goroutines outliving the
	// window would keep refreshing a dead widget tree every second.
	before := runtime.NumGoroutine()
	logWin := ownedKeys["activity-log"]
	if logWin == nil {
		t.Fatal("log window is not registered under its key")
	}
	logWin.Close()
	if raiseOwnedWindow("activity-log") {
		t.Errorf("log window still registered after closing")
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines %d → %d after closing: the refresh ticker outlived the window",
			before, after)
	}
}

// TestOwnedWindowOnClosed checks the caller's hook runs, and runs after the
// window's descendants are gone — cleanup often depends on that ordering.
func TestOwnedWindowOnClosed(t *testing.T) {
	a := test.NewApp()
	resetOwnedWindows(t)

	closed := false
	childOpenAtClose := true
	root := a.NewWindow("root")

	var child fyne.Window
	parent := openOwnedWindow(a, windowSpec{
		Title:  "parent",
		Parent: root,
		OnClosed: func() {
			closed = true
			childOpenAtClose = ownedRegistered(child)
		},
	}, stubContent)
	child = openOwnedWindow(a, windowSpec{Title: "child", Parent: parent}, stubContent)

	parent.Close()
	if !closed {
		t.Errorf("OnClosed did not run")
	}
	if childOpenAtClose {
		t.Errorf("OnClosed ran while a descendant was still open")
	}
}
