package main

// maskCanvas — a DICOM frame with mask rectangles drawn over it, in the two
// places a mask has to be seen rather than described: the picker, where the
// rectangle is dragged out on a real image, and the preview, where the regions
// a run will apply are shown on a representative of each image geometry.
//
// Geometry is fractional throughout, so the same rectangle drawn on a 640-pixel
// preview of an 800×600 frame means the same thing on the 1024×768 series in
// the same study. Converting between the two is this file's whole job, and the
// letterboxing that ImageFillContain introduces is where it would go wrong: the
// image occupies a centred sub-rectangle of the widget, not the widget.

import (
	"image"
	"image/color"
	"math"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// maskCanvasMode selects between drawing rectangles and showing them.
type maskCanvasMode int

const (
	// maskCanvasEdit lets the user drag out rectangles; they are drawn
	// translucent so the image beneath stays visible while judging the fit.
	maskCanvasEdit maskCanvasMode = iota
	// maskCanvasPreview shows what an export will look like: opaque fill, no
	// editing. Anything the user can see through is not what a mask does.
	maskCanvasPreview
)

// maskCanvas draws src with rects over it. Rectangles are fractions of the
// image in [0,1].
type maskCanvas struct {
	widget.BaseWidget

	img  *canvas.Image
	mode maskCanvasMode

	imgW, imgH int
	rects      []MaskRegion

	// Drag state. The anchor is captured on the first Dragged event rather than
	// on MouseDown, so both ends of the drag are measured in the same
	// coordinate space — the viewport learned the same lesson with window/level
	// drags, where a press position from another space skewed the whole drag.
	dragging     bool
	dragFrom     fyne.Position
	dragTo       fyne.Position
	overlayCache []fyne.CanvasObject

	// onChange fires when a drag adds a rectangle.
	onChange func()
	// onDraw, when set, receives each drawn rectangle *instead* of the canvas
	// keeping it. The preview owns the run's regions and resolves them itself
	// (an ultrasound rule expands into bands that were never drawn), so it
	// cannot let the canvas accumulate a second, divergent list.
	onDraw func(MaskRegion)
}

// setMode switches between showing a mask and drawing one.
func (c *maskCanvas) setMode(mode maskCanvasMode) {
	if c.mode == mode {
		return
	}
	c.mode = mode
	// The cached rectangles carry the old mode's fill, so drop them.
	c.overlayCache = nil
	c.Refresh()
}

func newMaskCanvas(src image.Image, mode maskCanvasMode) *maskCanvas {
	if src == nil {
		src = image.NewGray(image.Rect(0, 0, 1, 1))
	}
	b := src.Bounds()
	c := &maskCanvas{
		img:  canvas.NewImageFromImage(src),
		mode: mode,
		imgW: b.Dx(),
		imgH: b.Dy(),
	}
	c.img.FillMode = canvas.ImageFillContain
	c.img.ScaleMode = canvas.ImageScaleFastest
	c.ExtendBaseWidget(c)
	return c
}

// setImage swaps the frame on display, keeping the rectangles.
func (c *maskCanvas) setImage(src image.Image) {
	if src == nil {
		return
	}
	b := src.Bounds()
	c.imgW, c.imgH = b.Dx(), b.Dy()
	c.img.Image = src
	c.img.Refresh()
	c.Refresh()
}

// setRects replaces the rectangles on display.
func (c *maskCanvas) setRects(rects []MaskRegion) {
	c.rects = rects
	c.Refresh()
}

// displayArea is the sub-rectangle of size the image actually occupies under
// ImageFillContain: scaled to fit, centred, letterboxed on one axis.
func (c *maskCanvas) displayArea(size fyne.Size) (offX, offY, w, h float32) {
	if c.imgW <= 0 || c.imgH <= 0 || size.Width <= 0 || size.Height <= 0 {
		return 0, 0, size.Width, size.Height
	}
	scale := min(size.Width/float32(c.imgW), size.Height/float32(c.imgH))
	w, h = float32(c.imgW)*scale, float32(c.imgH)*scale
	return (size.Width - w) / 2, (size.Height - h) / 2, w, h
}

// fractionAt maps a position in the widget to a fraction of the image, clamped
// so a drag that leaves the image still produces a rectangle on it.
func (c *maskCanvas) fractionAt(p fyne.Position) (float64, float64) {
	offX, offY, w, h := c.displayArea(c.Size())
	if w <= 0 || h <= 0 {
		return 0, 0
	}
	return clampFloat(float64((p.X-offX)/w), 0, 1), clampFloat(float64((p.Y-offY)/h), 0, 1)
}

// maskDragMinimum is the smallest drag that counts as a rectangle, as a
// fraction of the image. Below it the gesture was a click — selecting nothing,
// but not worth turning into a mask the user cannot see.
const maskDragMinimum = 0.005

func (c *maskCanvas) Dragged(e *fyne.DragEvent) {
	if c.mode != maskCanvasEdit {
		return
	}
	if !c.dragging {
		c.dragging = true
		c.dragFrom = e.Position
	}
	c.dragTo = e.Position
	c.Refresh()
}

func (c *maskCanvas) DragEnd() {
	if c.mode != maskCanvasEdit || !c.dragging {
		return
	}
	c.dragging = false
	x0, y0 := c.fractionAt(c.dragFrom)
	x1, y1 := c.fractionAt(c.dragTo)
	if x1 < x0 {
		x0, x1 = x1, x0
	}
	if y1 < y0 {
		y0, y1 = y1, y0
	}
	if x1-x0 < maskDragMinimum || y1-y0 < maskDragMinimum {
		c.Refresh() // a click, not a rectangle — drop the draft
		return
	}
	// Snap to this image's pixel grid before storing. Screen coordinates are
	// float32, so a drag across the top fifth of a 100-pixel image yields
	// 0.20000000298023224 rather than 0.2 — which would be written into
	// profiles.json and shown in the editor. Snapping also makes the stored
	// fraction mean something exact: the rectangle drawn, to the nearest pixel
	// of the image it was drawn on.
	snap := func(f0, f1 float64, dim int) (float64, float64) {
		if dim <= 0 {
			return f0, f1
		}
		p0 := clampInt(int(math.Round(f0*float64(dim))), 0, dim)
		p1 := clampInt(int(math.Round(f1*float64(dim))), 0, dim)
		if p1 <= p0 {
			p1 = min(p0+1, dim) // never store an empty rectangle
		}
		return float64(p0) / float64(dim), float64(p1) / float64(dim)
	}
	x0, x1 = snap(x0, x1, c.imgW)
	y0, y1 = snap(y0, y1, c.imgH)

	drawn := MaskRegion{Mode: maskModeRect, X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
	if c.onDraw != nil {
		c.Refresh() // clear the draft; the owner decides what is displayed next
		c.onDraw(drawn)
		return
	}
	c.rects = append(c.rects, drawn)
	c.Refresh()
	if c.onChange != nil {
		c.onChange()
	}
}

// undo removes the most recently drawn rectangle.
func (c *maskCanvas) undo() {
	if len(c.rects) == 0 {
		return
	}
	c.rects = c.rects[:len(c.rects)-1]
	c.Refresh()
	if c.onChange != nil {
		c.onChange()
	}
}

// clear removes every rectangle.
func (c *maskCanvas) clear() {
	c.rects = nil
	c.Refresh()
	if c.onChange != nil {
		c.onChange()
	}
}

func (c *maskCanvas) CreateRenderer() fyne.WidgetRenderer {
	c.ExtendBaseWidget(c)
	return &maskCanvasRenderer{c: c}
}

type maskCanvasRenderer struct {
	c *maskCanvas
}

// overlayRect builds the rectangle drawn for one region.
func (r *maskCanvasRenderer) overlayRect(preview bool) *canvas.Rectangle {
	rect := canvas.NewRectangle(color.NRGBA{R: 0, G: 0, B: 0, A: 255})
	if !preview {
		// Translucent while editing, with a visible edge: the point of drawing
		// on the image is to judge the fit against what is underneath.
		rect.FillColor = color.NRGBA{R: 0, G: 0, B: 0, A: 140}
		rect.StrokeColor = theme.Color(theme.ColorNamePrimary)
		rect.StrokeWidth = 2
	}
	return rect
}

func (r *maskCanvasRenderer) objects() []fyne.CanvasObject {
	c := r.c
	preview := c.mode == maskCanvasPreview
	want := len(c.rects)
	if c.dragging {
		want++
	}
	// Reuse the rectangle objects across refreshes; only their geometry and
	// count change, and rebuilding them every drag tick would allocate at
	// pointer-motion rate.
	for len(c.overlayCache) < want {
		c.overlayCache = append(c.overlayCache, r.overlayRect(preview))
	}
	objs := make([]fyne.CanvasObject, 0, want+1)
	objs = append(objs, c.img)
	objs = append(objs, c.overlayCache[:want]...)
	return objs
}

func (r *maskCanvasRenderer) Layout(size fyne.Size) {
	c := r.c
	c.img.Resize(size)
	c.img.Move(fyne.NewPos(0, 0))

	offX, offY, w, h := c.displayArea(size)
	place := func(o fyne.CanvasObject, x0, y0, x1, y1 float64) {
		px := offX + float32(x0)*w
		py := offY + float32(y0)*h
		o.Move(fyne.NewPos(px, py))
		o.Resize(fyne.NewSize(float32(x1-x0)*w, float32(y1-y0)*h))
	}
	objs := r.objects()[1:] // skip the image
	for i, region := range c.rects {
		if i >= len(objs) {
			break
		}
		place(objs[i], region.X, region.Y, region.X+region.W, region.Y+region.H)
	}
	if c.dragging && len(objs) > len(c.rects) {
		x0, y0 := c.fractionAt(c.dragFrom)
		x1, y1 := c.fractionAt(c.dragTo)
		if x1 < x0 {
			x0, x1 = x1, x0
		}
		if y1 < y0 {
			y0, y1 = y1, y0
		}
		place(objs[len(c.rects)], x0, y0, x1, y1)
	}
}

func (r *maskCanvasRenderer) MinSize() fyne.Size { return fyne.NewSize(320, 240) }

func (r *maskCanvasRenderer) Refresh() {
	r.Layout(r.c.Size())
	for _, o := range r.objects() {
		o.Refresh()
	}
	canvas.Refresh(r.c)
}

func (r *maskCanvasRenderer) Objects() []fyne.CanvasObject { return r.objects() }
func (r *maskCanvasRenderer) Destroy()                     {}

// pixelRectsToRegions converts resolved pixel rectangles back to fractions of a
// frame, which is what the canvas draws. The preview runs the engine's own
// resolution and displays the result, so what is shown is what will be masked
// rather than a second implementation of the same rules.
func pixelRectsToRegions(rects []pixelRect, cols, rows int) []MaskRegion {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	out := make([]MaskRegion, 0, len(rects))
	for _, r := range rects {
		out = append(out, MaskRegion{
			Mode: maskModeRect,
			X:    float64(r.x0) / float64(cols),
			Y:    float64(r.y0) / float64(rows),
			W:    float64(r.x1-r.x0) / float64(cols),
			H:    float64(r.y1-r.y0) / float64(rows),
		})
	}
	return out
}
