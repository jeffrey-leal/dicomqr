package main

// A list's scroll area with a grab bar beneath it that drags the area taller
// or shorter — Preferences' Server, Tag and Modification profile lists, so a
// tall enough window can show every entry without scrolling the list.
//
// Fyne has no resizable panel that works inside a scrolling tab: a Split
// divides a fixed space between two objects, and inside a VScroll every object
// is given only its minimum. So the bar changes the list's minimum height, and
// the tab's own scroll absorbs the difference. That also keeps a drag away from
// the window's minimum size, whose every change repaints the whole OS frame on
// Windows (see stableMin): the tab's VScroll reports a minimum of its own, so a
// change inside it never reaches the canvas root.

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

const (
	resizeGripHeight    = 10 // the bar's grab zone
	resizeGripLineWidth = 28 // the two short lines drawn across its middle
)

// resizeGrip is the bar: a vertical-resize cursor, a highlight while the
// pointer is over it, and drags reported as vertical deltas.
type resizeGrip struct {
	widget.BaseWidget
	onDrag  func(dy float32)
	onEnd   func()
	hovered bool
}

func newResizeGrip(onDrag func(dy float32), onEnd func()) *resizeGrip {
	g := &resizeGrip{onDrag: onDrag, onEnd: onEnd}
	g.ExtendBaseWidget(g)
	return g
}

func (g *resizeGrip) Dragged(e *fyne.DragEvent) {
	if g.onDrag != nil {
		g.onDrag(e.Dragged.DY)
	}
}

func (g *resizeGrip) DragEnd() {
	if g.onEnd != nil {
		g.onEnd()
	}
}

func (g *resizeGrip) Cursor() desktop.Cursor { return desktop.VResizeCursor }

func (g *resizeGrip) MouseIn(*desktop.MouseEvent) {
	g.hovered = true
	g.Refresh()
}

func (g *resizeGrip) MouseMoved(*desktop.MouseEvent) {}

func (g *resizeGrip) MouseOut() {
	g.hovered = false
	g.Refresh()
}

func (g *resizeGrip) CreateRenderer() fyne.WidgetRenderer {
	r := &resizeGripRenderer{
		grip:  g,
		bar:   canvas.NewRectangle(color.Transparent),
		lines: [2]*canvas.Rectangle{canvas.NewRectangle(color.Transparent), canvas.NewRectangle(color.Transparent)},
	}
	r.Refresh()
	return r
}

type resizeGripRenderer struct {
	grip  *resizeGrip
	bar   *canvas.Rectangle
	lines [2]*canvas.Rectangle
}

func (r *resizeGripRenderer) Layout(size fyne.Size) {
	r.bar.Resize(size)
	r.bar.Move(fyne.NewPos(0, 0))
	x := (size.Width - resizeGripLineWidth) / 2
	mid := size.Height / 2
	for i, l := range r.lines {
		l.Resize(fyne.NewSize(resizeGripLineWidth, 1))
		l.Move(fyne.NewPos(x, mid-2+float32(i)*3))
	}
}

func (r *resizeGripRenderer) MinSize() fyne.Size {
	return fyne.NewSize(resizeGripLineWidth, resizeGripHeight)
}

func (r *resizeGripRenderer) Refresh() {
	lineColor := theme.Color(theme.ColorNameDisabled)
	r.bar.FillColor = color.Transparent
	if r.grip.hovered {
		lineColor = theme.Color(theme.ColorNamePrimary)
		r.bar.FillColor = theme.Color(theme.ColorNameHover)
	}
	r.bar.Refresh()
	for _, l := range r.lines {
		l.FillColor = lineColor
		l.Refresh()
	}
}

func (r *resizeGripRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.bar, r.lines[0], r.lines[1]}
}

func (r *resizeGripRenderer) Destroy() {}

// resizableHeight sets scroll — a list's vertical scroll area — to height and
// puts a grip beneath it. A drag never takes the area below floor, nor past the
// height at which every row of the list already shows (or the height the drag
// started from, if that is taller — so a drag never jumps). onResized receives
// the height once a drag ends, for the caller to remember.
func resizableHeight(scroll *container.Scroll, height, floor float32, onResized func(float32)) fyne.CanvasObject {
	h := max(height, floor)
	scroll.SetMinSize(fyne.NewSize(0, h))

	// want accumulates the pointer's travel through a gesture, so a pointer
	// that overshoots a limit has to come back to it before the bar moves.
	var want, ceiling float32
	dragging := false
	grip := newResizeGrip(func(dy float32) {
		if !dragging {
			dragging = true
			want = h
			ceiling = max(scroll.Content.MinSize().Height, h)
		}
		want += dy
		next := min(max(want, floor), ceiling)
		if next == h {
			return
		}
		h = next
		scroll.SetMinSize(fyne.NewSize(0, h))
		scroll.Refresh()
	}, func() {
		dragging = false
		if onResized != nil {
			onResized(h)
		}
	})
	return container.NewVBox(scroll, grip)
}
