package main

// Structural smoke tests for the viewer window. They build the real window
// through Fyne's headless test driver, which is the only way to exercise the
// chapter-mode wiring — widget construction, chapter selection, buffer start
// and player configuration — without a display.

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

// uiPostMu stands in for the UI thread that Fyne's test driver does not have.
// The driver runs fyne.Do inline on whichever goroutine calls it, so without a
// lock the viewer's background posts (clip buffering progress, filmstrip
// thumbnails, cine frames) touch widgets in parallel with window construction
// and with the test's own inspection — which corrupts Fyne's internal state and
// panics. The real app gets that serialisation from the UI thread itself.
var uiPostMu sync.Mutex

// TestMain installs the serialising postUI once for the whole test binary.
// Once, rather than per test, because the viewer's goroutines outlive the test
// that started them: restoring the original mid-run would be a write racing
// their reads.
func TestMain(m *testing.M) {
	original := postUI
	postUI = func(fn func()) {
		original(func() {
			uiPostMu.Lock()
			defer uiPostMu.Unlock()
			fn()
		})
	}
	os.Exit(m.Run())
}

// onUI runs fn under the same lock the viewer's posts take. Tests must use it
// for every widget access of their own — inspecting the tree, closing a window.
func onUI(fn func()) {
	uiPostMu.Lock()
	defer uiPostMu.Unlock()
	fn()
}

// openViewerForTest builds a viewer window off the test goroutine (its
// documented contract) and waits for construction to finish.
func openViewerForTest(t *testing.T, app fyne.App, chapters []chapter) fyne.Window {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		openViewerWindow(app, "smoke test", chapters, nil)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("openViewerWindow did not return")
	}
	windows := app.Driver().AllWindows()
	if len(windows) == 0 {
		t.Fatal("no window was created")
	}
	return windows[len(windows)-1]
}

// walkObjects visits every canvas object in a tree, descending into widgets
// through their renderers.
func walkObjects(t *testing.T, root fyne.CanvasObject, visit func(fyne.CanvasObject)) {
	if root == nil {
		return
	}
	visit(root)
	switch obj := root.(type) {
	case *fyne.Container:
		for _, child := range obj.Objects {
			walkObjects(t, child, visit)
		}
	case fyne.Widget:
		for _, child := range test.TempWidgetRenderer(t, obj).Objects() {
			walkObjects(t, child, visit)
		}
	}
}

type viewerParts struct {
	cells   int
	sliders int
	buttons int
	selects int
}

func inspectWindow(t *testing.T, win fyne.Window) viewerParts {
	t.Helper()
	var parts viewerParts
	walkObjects(t, win.Content(), func(o fyne.CanvasObject) {
		switch o.(type) {
		case *chapterCell:
			parts.cells++
		case *widget.Slider:
			parts.sliders++
		case *widget.Button:
			parts.buttons++
		case *widget.Select:
			parts.selects++
		}
	})
	return parts
}

// stableMin must stop text changes from shrinking the reported min size — a
// min-size change makes Fyne re-apply the window's size limits, which on
// Windows is a MoveWindow(repaint) of the whole frame; at cine-playback rate
// that storm is visible as flickering lines around the window.
func TestStableMinLayoutHighWaterMark(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()

	lbl := widget.NewLabel("short")
	wrapped := stableMin(lbl)
	small := wrapped.MinSize()

	lbl.SetText("Frame 130 / 218   (buffering 130 / 130)")
	grown := wrapped.MinSize()
	if grown.Width <= small.Width {
		t.Fatalf("min did not grow for longer text: %v -> %v", small, grown)
	}

	// Shorter text must not shrink the min — that change is exactly the
	// MoveWindow trigger this layout exists to suppress.
	lbl.SetText("Frame 1 / 218")
	if got := wrapped.MinSize(); got != grown {
		t.Errorf("min changed on shorter text: %v, want the high-water %v", got, grown)
	}
	// And repeated re-labels at the same length must be stable.
	lbl.SetText("Frame 2 / 218")
	if got := wrapped.MinSize(); got != grown {
		t.Errorf("min changed on same-length text: %v, want %v", got, grown)
	}
}

// A series of single-frame instances keeps the plain slider and grows no
// transport or filmstrip.
func TestViewerWindowSliceMode(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()

	dir := t.TempDir()
	chapters := scanChapters([]string{
		writeMultiframeTestFile(t, dir, 1, 1),
		writeMultiframeTestFile(t, dir, 1, 2),
	}, nil)
	if anyMultiFrame(chapters) {
		t.Fatal("single-frame files must not enter chapter mode")
	}

	win := openViewerForTest(t, app, chapters)
	defer onUI(func() { win.Close() })

	var parts viewerParts
	onUI(func() { parts = inspectWindow(t, win) })
	if parts.cells != 0 {
		t.Errorf("slice mode built %d filmstrip cells, want none", parts.cells)
	}
	if parts.sliders != 1 {
		t.Errorf("found %d sliders, want the single navigation slider", parts.sliders)
	}
}

// A series holding a clip enters chapter mode: one filmstrip cell per instance,
// plus the transport.
func TestViewerWindowChapterMode(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()

	dir := t.TempDir()
	chapters := scanChapters([]string{
		writeMultiframeTestFile(t, dir, 8, 1),
		writeMultiframeTestFile(t, dir, 1, 2),
		writeMultiframeTestFile(t, dir, 6, 3),
	}, nil)
	if !anyMultiFrame(chapters) {
		t.Fatal("a series holding a clip must enter chapter mode")
	}

	win := openViewerForTest(t, app, chapters)
	defer onUI(func() { win.Close() })

	var parts viewerParts
	onUI(func() { parts = inspectWindow(t, win) })
	if parts.cells != len(chapters) {
		t.Errorf("filmstrip has %d cells, want one per chapter (%d)", parts.cells, len(chapters))
	}
	// Play, previous chapter, next chapter and Reset.
	if parts.buttons < 4 {
		t.Errorf("found %d buttons, want at least the transport's four", parts.buttons)
	}
}

// One multi-frame instance gets the transport — cine through its frames — but
// no filmstrip, which would be a single cell of clutter.
func TestViewerWindowSingleChapterHasNoFilmstrip(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()

	chapters := scanChapters([]string{
		writeMultiframeTestFile(t, t.TempDir(), 12, 1),
	}, nil)

	win := openViewerForTest(t, app, chapters)
	defer onUI(func() { win.Close() })

	var parts viewerParts
	onUI(func() { parts = inspectWindow(t, win) })
	if parts.cells != 0 {
		t.Errorf("a one-chapter series built %d filmstrip cells, want none", parts.cells)
	}
}

// A phased MR series (same slice stack covered twice) gains the Phase dropdown
// beside Window and Colour; an ordinary series has only those two selects.
func TestViewerWindowPhaseMode(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()

	dir := t.TempDir()
	inst := 1
	for p := 0; p < 2; p++ {
		for s := 0; s < 3; s++ {
			writeMRTestFile(t, dir, inst, p+1, []string{"2.38", "4.62"}[p], float64(60-3*s))
			inst++
		}
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.dcm"))
	if err != nil || len(paths) != 6 {
		t.Fatalf("glob: %v (%d files)", err, len(paths))
	}
	chapters := scanChapters(paths, nil)
	if detectMRPhases(chapters) == nil {
		t.Fatal("test series must detect as phased")
	}

	win := openViewerForTest(t, app, chapters)
	defer onUI(func() { win.Close() })

	var parts viewerParts
	onUI(func() { parts = inspectWindow(t, win) })
	if parts.selects != 3 {
		t.Errorf("phased window has %d selects, want 3 (Phase, Window, Colour)", parts.selects)
	}
	if parts.sliders != 1 {
		t.Errorf("found %d sliders, want 1", parts.sliders)
	}
	if parts.cells != 0 {
		t.Errorf("phase mode built %d filmstrip cells, want none", parts.cells)
	}
}

// Closing the window must stop playback and buffering: a closed viewer that
// keeps a ticker and a decode pool running holds the process busy for the rest
// of the session.
func TestViewerWindowCloseStopsWork(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()

	dir := t.TempDir()
	chapters := scanChapters([]string{
		writeMultiframeTestFile(t, dir, 30, 1),
		writeMultiframeTestFile(t, dir, 30, 2),
	}, nil)

	win := openViewerForTest(t, app, chapters)
	onUI(func() { win.Close() })
	// Nothing to assert beyond surviving the close without a panic or a hang:
	// the goroutines are stopped through SetOnClosed, and the race detector run
	// of this test is what proves they are not still touching viewer state.
	time.Sleep(50 * time.Millisecond)
}
