package main

// chapterStrip is the chapter picker: a horizontal strip of thumbnails, one per
// instance in the series, with the active chapter outlined. Clicking a cell
// selects that chapter.
//
// Each thumbnail is the chapter's middle frame — for a cine loop that is a
// frame with the anatomy actually in view, where frame 0 is often the start of
// a sweep or a part-formed image. They are decoded off the UI goroutine in
// parallel and dropped into their cells as they arrive, so the strip is usable
// immediately rather than after the slowest decode.
//
// For a real echo study the thumbnail is the only thing distinguishing one clip
// from another: the vendors that write no ImageComments, ProtocolName or
// ViewCodeSequence leave every 2D loop with the same label.

import (
	"image"
	"image/color"
	"strconv"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

const (
	chapterThumbSide  = 84
	chapterCellWidth  = chapterThumbSide + 12
	chapterCellHeight = chapterThumbSide + 38
)

// chapterCell is one clickable filmstrip entry: thumbnail, label, and a badge
// saying what there is to play.
type chapterCell struct {
	widget.BaseWidget
	thumb    *canvas.Image
	border   *canvas.Rectangle
	status   *canvas.Rectangle
	caption  *widget.Label
	badge    *widget.Label
	onTapped func()
}

func newChapterCell(c chapter, onTapped func()) *chapterCell {
	cell := &chapterCell{onTapped: onTapped}

	// A transparent 1×1 placeholder keeps the layout stable until the real
	// thumbnail lands (or forever, for an instance that cannot be decoded).
	cell.thumb = canvas.NewImageFromImage(image.NewRGBA(image.Rect(0, 0, 1, 1)))
	cell.thumb.FillMode = canvas.ImageFillContain
	cell.thumb.SetMinSize(fyne.NewSize(chapterThumbSide, chapterThumbSide))

	cell.border = canvas.NewRectangle(color.Transparent)
	cell.border.StrokeWidth = 2
	cell.border.StrokeColor = color.Transparent

	// The status stripe overlays the thumbnail's bottom edge, transparent
	// until a caller paints it (the viewer never does; the mask review window
	// uses it to say what masking will do to each image).
	cell.status = canvas.NewRectangle(color.Transparent)
	cell.status.SetMinSize(fyne.NewSize(0, 4))

	cell.caption = widget.NewLabel(c.label)
	cell.caption.Truncation = fyne.TextTruncateEllipsis
	cell.caption.Alignment = fyne.TextAlignCenter

	badge := "still"
	if c.playable() {
		badge = strconv.Itoa(c.frames) + " fr"
	}
	cell.badge = widget.NewLabelWithStyle(badge, fyne.TextAlignCenter,
		fyne.TextStyle{Italic: true})

	cell.ExtendBaseWidget(cell)
	return cell
}

// MinSize fixes the cell size so the strip is a tidy row of equal tiles and the
// caption truncates instead of stretching the cell to its own text width.
func (c *chapterCell) MinSize() fyne.Size {
	return fyne.NewSize(chapterCellWidth, chapterCellHeight)
}

func (c *chapterCell) Tapped(_ *fyne.PointEvent) {
	if c.onTapped != nil {
		c.onTapped()
	}
}

func (c *chapterCell) CreateRenderer() fyne.WidgetRenderer {
	c.ExtendBaseWidget(c)
	backdrop := canvas.NewRectangle(color.NRGBA{R: 0x20, G: 0x20, B: 0x20, A: 0xFF})
	return widget.NewSimpleRenderer(container.NewStack(
		c.border,
		container.NewBorder(nil, container.NewVBox(c.caption, c.badge), nil, nil,
			container.NewStack(backdrop, c.thumb,
				container.NewBorder(nil, c.status, nil, nil))),
	))
}

// setStatus paints the stripe along the thumbnail's bottom edge.
func (c *chapterCell) setStatus(col color.Color) {
	c.status.FillColor = col
	c.status.Refresh()
}

func (c *chapterCell) setSelected(selected bool) {
	if selected {
		c.border.StrokeColor = theme.Color(theme.ColorNamePrimary)
		c.border.FillColor = theme.Color(theme.ColorNameHover)
	} else {
		c.border.StrokeColor = color.Transparent
		c.border.FillColor = color.Transparent
	}
	c.border.Refresh()
}

func (c *chapterCell) setThumbnail(img image.Image) {
	c.thumb.Image = img
	c.thumb.Refresh()
}

// chapterStrip is the scrollable row of cells.
type chapterStrip struct {
	scroll   *container.Scroll
	cells    []*chapterCell
	selected int

	mu      sync.Mutex
	stopped bool
}

// newChapterStrip builds a cell per chapter and starts decoding thumbnails.
// Must be called on the UI goroutine; onSelect is invoked there too.
func newChapterStrip(chapters []chapter, onSelect func(index int)) *chapterStrip {
	s := &chapterStrip{selected: -1}
	cells := make([]fyne.CanvasObject, len(chapters))
	s.cells = make([]*chapterCell, len(chapters))
	for i, c := range chapters {
		index := i
		cell := newChapterCell(c, func() {
			if onSelect != nil {
				onSelect(index)
			}
		})
		s.cells[i] = cell
		cells[i] = cell
	}
	s.scroll = container.NewHScroll(container.NewHBox(cells...))
	go s.loadThumbnails(chapters)
	return s
}

func (s *chapterStrip) object() fyne.CanvasObject { return s.scroll }

// setStatuses paints every cell's status stripe in one pass — index i colours
// cell i, and a short slice leaves the rest untouched. Must be called on the
// UI goroutine, like everything else that touches the cells.
func (s *chapterStrip) setStatuses(colors []color.Color) {
	for i, col := range colors {
		if i < len(s.cells) {
			s.cells[i].setStatus(col)
		}
	}
}

// stop abandons thumbnail decoding — the window is closing, and there is
// nothing left to fill.
func (s *chapterStrip) stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
}

func (s *chapterStrip) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// selectIndex outlines a chapter as active and scrolls it into view.
func (s *chapterStrip) selectIndex(index int) {
	if index < 0 || index >= len(s.cells) || index == s.selected {
		return
	}
	if s.selected >= 0 && s.selected < len(s.cells) {
		s.cells[s.selected].setSelected(false)
	}
	s.selected = index
	s.cells[index].setSelected(true)
	s.scrollIntoView(index)
}

// scrollIntoView keeps the active cell visible as chapters are stepped through
// from the keyboard or the transport buttons.
func (s *chapterStrip) scrollIntoView(index int) {
	if len(s.cells) < 2 {
		return
	}
	cellX := float32(index) * (chapterCellWidth + theme.Padding())
	viewW := s.scroll.Size().Width
	offset := s.scroll.Offset.X
	switch {
	case cellX < offset:
		offset = cellX
	case cellX+chapterCellWidth > offset+viewW:
		offset = cellX + chapterCellWidth - viewW
	default:
		return // already visible
	}
	if offset < 0 {
		offset = 0
	}
	s.scroll.Offset.X = offset
	s.scroll.Refresh()
}

// loadThumbnails decodes each chapter's middle frame in parallel and posts it
// into its cell as it lands. A chapter that cannot be decoded keeps its blank
// backdrop rather than reporting a failure the user can do nothing about from a
// filmstrip.
func (s *chapterStrip) loadThumbnails(chapters []chapter) {
	type thumbnail struct {
		index int
		img   image.Image
	}
	jobs := make(chan int)
	// Buffered by the chapter count so a worker never blocks handing a finished
	// thumbnail over, even if this goroutine has stopped collecting.
	results := make(chan thumbnail, len(chapters))

	var wg sync.WaitGroup
	for w := 0; w < clipBufferWorkers(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if s.isStopped() {
					return
				}
				c := chapters[i]
				st, err := loadDicomFrame(c.path, c.frames/2)
				if err != nil || st.img == nil || s.isStopped() {
					continue
				}
				results <- thumbnail{index: i, img: scaleToThumb(st.img, chapterThumbSide)}
			}
		}()
	}
	go func() {
		for i := range chapters {
			if s.isStopped() {
				break
			}
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	// This goroutine alone posts to the UI: the decoders never touch a widget,
	// so however many of them run, one cell is filled at a time.
	for r := range results {
		if s.isStopped() {
			continue // drain, so the workers finish and the collector closes
		}
		index, img := r.index, r.img
		postUI(func() {
			if index < len(s.cells) && !s.isStopped() {
				s.cells[index].setThumbnail(img)
			}
		})
	}
}

// scaleToThumb box-filters img down to fit a side×side square, preserving
// aspect. Downscaling here rather than letting the canvas scale a full frame is
// what keeps a 173-chapter strip to a few MB instead of several hundred.
func scaleToThumb(img image.Image, side int) *image.RGBA {
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	scale := float64(side) / float64(b.Dx())
	if sy := float64(side) / float64(b.Dy()); sy < scale {
		scale = sy
	}
	if scale > 1 {
		scale = 1 // never upscale a small frame into a blurry tile
	}
	w := maxInt(1, int(float64(b.Dx())*scale))
	h := maxInt(1, int(float64(b.Dy())*scale))
	dst := image.NewRGBA(image.Rect(0, 0, w, h))

	for y := 0; y < h; y++ {
		sy0 := b.Min.Y + y*b.Dy()/h
		sy1 := maxInt(sy0+1, b.Min.Y+(y+1)*b.Dy()/h)
		for x := 0; x < w; x++ {
			sx0 := b.Min.X + x*b.Dx()/w
			sx1 := maxInt(sx0+1, b.Min.X+(x+1)*b.Dx()/w)
			var r, g, bl, a, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					cr, cg, cb, ca := img.At(sx, sy).RGBA()
					r += uint64(cr)
					g += uint64(cg)
					bl += uint64(cb)
					a += uint64(ca)
					n++
				}
			}
			if n == 0 {
				continue
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8(r / n >> 8)
			dst.Pix[i+1] = uint8(g / n >> 8)
			dst.Pix[i+2] = uint8(bl / n >> 8)
			dst.Pix[i+3] = uint8(a / n >> 8)
		}
	}
	return dst
}
