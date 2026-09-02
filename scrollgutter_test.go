package main

// The gutter's whole job is width on the right; if the wrapper stops adding
// it, every nested scrollbar in the app silently lands back in the outer
// bar's grab zone.

import (
	"testing"

	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

func TestPadForScrollbarAddsRightGutter(t *testing.T) {
	test.NewApp()
	bare := widget.NewLabel("content")
	wrapped := padForScrollbar(widget.NewLabel("content"))

	gutter := scrollGutterWidth()
	if gutter <= 0 {
		t.Fatalf("scrollGutterWidth() = %v, want > 0", gutter)
	}
	got := wrapped.MinSize().Width - bare.MinSize().Width
	if got != gutter {
		t.Errorf("wrapped width exceeds bare by %v, want the gutter %v", got, gutter)
	}
	if wrapped.MinSize().Height != bare.MinSize().Height {
		t.Errorf("gutter changed the height (%v → %v) — it must pad the right only",
			bare.MinSize().Height, wrapped.MinSize().Height)
	}
}
