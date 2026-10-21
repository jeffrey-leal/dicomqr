package main

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

func testRows(n int) *fyne.Container {
	rows := container.NewVBox()
	for i := 0; i < n; i++ {
		rows.Add(widget.NewLabel("row"))
	}
	return rows
}

// The grip drags the list's height within its limits — never below the floor,
// never past the height at which every row shows — and reports where a drag
// ended.
func TestResizableHeightDrag(t *testing.T) {
	test.NewApp()
	rows := testRows(12)
	content := rows.MinSize().Height
	if content < 300 {
		t.Fatalf("test content only %v tall", content)
	}
	scroll := container.NewVScroll(rows)
	var saved []float32
	box := resizableHeight(scroll, 120, 80, func(h float32) { saved = append(saved, h) }).(*fyne.Container)
	grip, ok := box.Objects[1].(*resizeGrip)
	if !ok || box.Objects[0] != scroll {
		t.Fatalf("expected the list above its grip, got %T, %T", box.Objects[0], box.Objects[1])
	}
	var _ desktop.Cursorable = grip
	var _ desktop.Hoverable = grip
	height := func() float32 { return scroll.MinSize().Height }
	drag := func(dys ...float32) float32 {
		for _, dy := range dys {
			grip.Dragged(&fyne.DragEvent{Dragged: fyne.NewDelta(0, dy)})
		}
		grip.DragEnd()
		return height()
	}

	if height() != 120 {
		t.Fatalf("starting height %v, want 120", height())
	}
	if got := drag(30); got != 150 || saved[len(saved)-1] != 150 {
		t.Errorf("drag down 30: height %v, saved %v", got, saved)
	}
	if got := drag(10000); got != content {
		t.Errorf("drag far down: height %v, want the content's %v", got, content)
	}
	if got := drag(-10000); got != 80 {
		t.Errorf("drag far up: height %v, want the floor 80", got)
	}
	// An overshoot has to come back before the bar moves again.
	if got := drag(-50, 60); got != 90 {
		t.Errorf("overshoot then back: height %v, want 90", got)
	}
}

// A list already taller than its rows (a saved height, rows since deleted)
// does not jump when a drag starts: it can shrink, but not grow further.
func TestResizableHeightNeverJumps(t *testing.T) {
	test.NewApp()
	scroll := container.NewVScroll(testRows(2))
	box := resizableHeight(scroll, 300, 80, nil).(*fyne.Container)
	grip := box.Objects[1].(*resizeGrip)
	grip.Dragged(&fyne.DragEvent{Dragged: fyne.NewDelta(0, 5)})
	if h := scroll.MinSize().Height; h != 300 {
		t.Errorf("growing past the rows moved the list to %v", h)
	}
	grip.Dragged(&fyne.DragEvent{Dragged: fyne.NewDelta(0, -25)})
	grip.DragEnd()
	if h := scroll.MinSize().Height; h != 280 {
		t.Errorf("shrinking by 20 from 300 gave %v", h)
	}

	// A saved height below the floor starts at the floor.
	low := container.NewVScroll(testRows(2))
	resizableHeight(low, 10, 80, nil)
	if h := low.MinSize().Height; h != 80 {
		t.Errorf("height below the floor started at %v", h)
	}
}
