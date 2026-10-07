package main

// Smoke tests for the panels promoted from dialogs to owned windows: each opens
// as a child of the window that asked for it, blocks that window while open,
// and releases it again on close.

import (
	"image/color"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
)

func TestPromotedPanelsOpenAsBlockingChildren(t *testing.T) {
	a := test.NewApp()
	for _, tc := range []struct {
		name string
		open func(parent fyne.Window)
	}{
		{"tag profile editor", func(parent fyne.Window) {
			showTagProfileEditor(a, parent, TagProfile{Name: "Dates", Color: color.RGBA{A: 0xFF}}, func(TagProfile) {})
		}},
		{"per-modality override editor", func(parent fyne.Window) {
			showPerModalityEditor(a, parent, "CT", ModProfile{Removes: []string{"0008,0080"}}, nil, "",
				func(string, ModProfile) {})
		}},
		{"server profile editor", func(parent fyne.Window) {
			showServerProfileEditor(a, parent, ServerProfile{Name: "PACS", Port: 104},
				func() string { return "TEST" }, func(ServerProfile) {})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetOwnedWindows(t)
			parent := a.NewWindow("parent")
			defer parent.Close()

			tc.open(parent)

			if n := ownedChildCount(parent); n != 1 {
				t.Fatalf("%d child windows of the parent, want 1", n)
			}
			ownedMu.Lock()
			child := ownedWindows[parent].children[0]
			blocked := blockCount[parent]
			ownedMu.Unlock()
			if blocked != 1 {
				t.Errorf("parent block count %d while the panel is open, want 1", blocked)
			}

			child.Close()
			ownedMu.Lock()
			blocked = blockCount[parent]
			ownedMu.Unlock()
			if blocked != 0 {
				t.Errorf("parent still blocked (%d) after the panel closed", blocked)
			}
		})
	}
}
