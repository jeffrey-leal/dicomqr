package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// postUI hands a change to the UI goroutine. Every background path in the
// viewer — clip buffering, filmstrip thumbnails, cine playback, on-demand frame
// loads — goes through it rather than calling fyne.Do directly, so there is a
// single definition of what "on the UI goroutine" means for the viewer.
//
// It is a variable because Fyne's test driver has no UI thread: it runs fyne.Do
// inline on whichever goroutine calls it, so a windowed test would have
// background work touching widgets in parallel with construction — which
// corrupts Fyne's own state and panics. Tests substitute a version that
// serialises the calls. In the shipped app this is exactly fyne.Do.
var postUI = fyne.Do

// viewerSlice identifies one navigable image in the viewer: a file plus the
// zero-based index of a frame within that file. CT and MR store one frame per
// file and so contribute a single slice each; NM/SPECT stores a whole
// acquisition (40-240 frames) as one multi-frame file, which contributes one
// slice per frame — without this the viewer would show only its first frame.
//
// Slices are the navigation model for series with no multi-frame instance. A
// series that holds one navigates by chapter instead (see chapters.go), because
// flattening every frame of every clip onto one slider is unusable for
// ultrasound; slices are still what picks a series' middle frame for a
// thumbnail.
type viewerSlice struct {
	path  string
	frame int
}

// expandFrames flattens ordered chapters into a navigable slice list, one entry
// per frame of each file.
func expandFrames(chapters []chapter) []viewerSlice {
	slices := make([]viewerSlice, 0, len(chapters))
	for _, c := range chapters {
		for f := 0; f < maxInt(1, c.frames); f++ {
			slices = append(slices, viewerSlice{path: c.path, frame: f})
		}
	}
	return slices
}

// chapterPaths returns the chapters' file paths, in order.
func chapterPaths(chapters []chapter) []string {
	paths := make([]string, len(chapters))
	for i, c := range chapters {
		paths[i] = c.path
	}
	return paths
}

// collectDicomFiles walks dir and returns one chapter per .dcm file it holds,
// ordered by InstanceNumber.
func collectDicomFiles(dir string) ([]chapter, error) {
	var paths []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".dcm") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, errors.New("no DICOM files found in: " + dir)
	}
	return scanChapters(paths, nil), nil
}

// imageAnnotations holds the overlay text for a single DICOM image, organised
// into the four standard corner zones and four edge orientation markers.
type imageAnnotations struct {
	// top-left: patient identity
	patientName   string
	patientID     string
	patientDOB    string
	patientSexAge string

	// top-right: study/acquisition context
	institution string
	studyDate   string
	accession   string
	studyDesc   string
	referringMD string

	// bottom-left: series identity
	modality   string
	seriesInfo string
	sliceThick string
	protocol   string

	// bottom-right: image geometry (instanceInfo filled by caller)
	sliceLoc     string
	pixelSpacing string
	windowStr    string

	// edge orientation markers (derived from ImageOrientationPatient)
	orientLeft   string
	orientRight  string
	orientTop    string
	orientBottom string
}

// ── Annotation text colour and size ───────────────────────────────────────────

var annColor = color.NRGBA{R: 0xFF, G: 0xFF, B: 0x00, A: 0xCC} // yellow

const annTextSize = float32(11)

// ── imageAnnLayout positions corner blocks and edge labels within the actual
// rendered image rect (FillContain), never into the letterbox bars. ──────────

const annPad = float32(6)

// imageAnnLayout computes the FillContain image rect and pins annotations
// inside it. img must be the same *canvas.Image used in the stack so that
// Image.Bounds() always reflects the current slice dimensions.
type imageAnnLayout struct{ img *canvas.Image }

// imageRect returns the position and size of the rendered image inside size,
// honouring FillContain scaling (i.e. the letterbox-free area).
func (l imageAnnLayout) imageRect(size fyne.Size) (fyne.Position, fyne.Size) {
	if l.img == nil || l.img.Image == nil {
		return fyne.NewPos(0, 0), size
	}
	b := l.img.Image.Bounds()
	iW, iH := float32(b.Dx()), float32(b.Dy())
	if iW <= 0 || iH <= 0 {
		return fyne.NewPos(0, 0), size
	}
	scaleX, scaleY := size.Width/iW, size.Height/iH
	scale := scaleX
	if scaleY < scaleX {
		scale = scaleY
	}
	rW, rH := iW*scale, iH*scale
	return fyne.NewPos((size.Width-rW)/2, (size.Height-rH)/2), fyne.NewSize(rW, rH)
}

func (imageAnnLayout) MinSize(_ []fyne.CanvasObject) fyne.Size { return fyne.Size{} }

func (l imageAnnLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	if len(objs) < 4 {
		return
	}
	orig, imgSz := l.imageRect(size)

	pin := func(o fyne.CanvasObject) fyne.Size {
		ms := o.MinSize()
		o.Resize(ms)
		return ms
	}

	// TL — left-aligned, top-left of image
	ms := pin(objs[0])
	objs[0].Move(fyne.NewPos(orig.X+annPad, orig.Y+annPad))

	// TR — right-aligned block, top-right of image
	ms = pin(objs[1])
	objs[1].Move(fyne.NewPos(orig.X+imgSz.Width-ms.Width-annPad, orig.Y+annPad))

	// BL — left-aligned, bottom-left of image
	ms = pin(objs[2])
	objs[2].Move(fyne.NewPos(orig.X+annPad, orig.Y+imgSz.Height-ms.Height-annPad))

	// BR — right-aligned block, bottom-right of image
	ms = pin(objs[3])
	objs[3].Move(fyne.NewPos(orig.X+imgSz.Width-ms.Width-annPad, orig.Y+imgSz.Height-ms.Height-annPad))

	if len(objs) < 8 {
		return
	}
	// Edge orientation markers: top, bottom, left, right — centred on each edge
	ms = pin(objs[4])
	objs[4].Move(fyne.NewPos(orig.X+(imgSz.Width-ms.Width)/2, orig.Y+annPad))

	ms = pin(objs[5])
	objs[5].Move(fyne.NewPos(orig.X+(imgSz.Width-ms.Width)/2, orig.Y+imgSz.Height-ms.Height-annPad))

	ms = pin(objs[6])
	objs[6].Move(fyne.NewPos(orig.X+annPad, orig.Y+(imgSz.Height-ms.Height)/2))

	ms = pin(objs[7])
	objs[7].Move(fyne.NewPos(orig.X+imgSz.Width-ms.Width-annPad, orig.Y+(imgSz.Height-ms.Height)/2))
}

// rightVBoxLayout stacks objects vertically and sizes each to the full
// container width so that canvas.Text with TextAlignTrailing renders
// right-aligned regardless of individual line length.
type rightVBoxLayout struct{}

func (rightVBoxLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	var maxW, totalH float32
	for _, o := range objs {
		ms := o.MinSize()
		if ms.Width > maxW {
			maxW = ms.Width
		}
		totalH += ms.Height
	}
	return fyne.NewSize(maxW, totalH)
}

func (rightVBoxLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	y := float32(0)
	for _, o := range objs {
		h := o.MinSize().Height
		o.Resize(fyne.NewSize(size.Width, h))
		o.Move(fyne.NewPos(0, y))
		y += h
	}
}

// ── Annotation object builders ─────────────────────────────────────────────────

func newAnnText(s string) *canvas.Text {
	t := canvas.NewText(s, annColor)
	t.TextSize = annTextSize
	return t
}

// annBlock returns a left-aligned VBox of annotation text lines, skipping empty strings.
func annBlock(lines ...string) fyne.CanvasObject {
	var objs []fyne.CanvasObject
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			objs = append(objs, newAnnText(l))
		}
	}
	if len(objs) == 0 {
		t := canvas.NewText("", annColor)
		t.TextSize = annTextSize
		return t
	}
	return container.NewVBox(objs...)
}

// annBlockRight returns a right-aligned block using rightVBoxLayout so that
// each line's right edge is flush with the container's right edge.
func annBlockRight(lines ...string) fyne.CanvasObject {
	var objs []fyne.CanvasObject
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			t := canvas.NewText(l, annColor)
			t.TextSize = annTextSize
			t.Alignment = fyne.TextAlignTrailing
			objs = append(objs, t)
		}
	}
	if len(objs) == 0 {
		t := canvas.NewText("", annColor)
		t.TextSize = annTextSize
		return t
	}
	return container.New(rightVBoxLayout{}, objs...)
}

// buildAnnObjects constructs the 8 canvas objects expected by imageAnnLayout:
// [0] TL, [1] TR (right-aligned), [2] BL, [3] BR (right-aligned),
// [4] top edge, [5] bottom edge, [6] left edge, [7] right edge.
func buildAnnObjects(ann imageAnnotations, idx, total int) []fyne.CanvasObject {
	instanceStr := fmt.Sprintf("Im: %d / %d", idx+1, total)
	return []fyne.CanvasObject{
		annBlock(ann.patientName, ann.patientID, ann.patientDOB, ann.patientSexAge),
		annBlockRight(ann.institution, ann.studyDate, ann.accession, ann.studyDesc, ann.referringMD),
		annBlock(ann.modality, ann.seriesInfo, ann.sliceThick, ann.protocol),
		annBlockRight(instanceStr, ann.sliceLoc, ann.pixelSpacing, ann.windowStr),
		newAnnText(ann.orientTop),
		newAnnText(ann.orientBottom),
		newAnnText(ann.orientLeft),
		newAnnText(ann.orientRight),
	}
}

// ── DICOM metadata extraction ──────────────────────────────────────────────────

func extractAnnotationsFromDataset(ds sdicom.Dataset) imageAnnotations {
	var ann imageAnnotations

	str := func(t tag.Tag) string {
		e, err := ds.FindElementByTag(t)
		if err != nil {
			return ""
		}
		strs := sdicom.MustGetStrings(e.Value)
		if len(strs) == 0 {
			return ""
		}
		return strings.TrimSpace(strs[0])
	}

	// Patient identity
	ann.patientName = formatDicomPersonName(str(tag.PatientName))
	ann.patientID = str(tag.PatientID)
	ann.patientDOB = formatDicomDate(str(tag.PatientBirthDate))
	sex, age := str(tag.PatientSex), str(tag.PatientAge)
	switch {
	case sex != "" && age != "":
		ann.patientSexAge = sex + "  " + age
	case sex != "":
		ann.patientSexAge = sex
	case age != "":
		ann.patientSexAge = age
	}

	// Study/acquisition context
	ann.institution = str(tag.InstitutionName)
	if d := formatDicomDate(str(tag.StudyDate)); d != "" {
		ann.studyDate = d
		if t2 := formatDicomTime(str(tag.StudyTime)); t2 != "" {
			ann.studyDate += "  " + t2
		}
	}
	ann.accession = str(tag.AccessionNumber)
	ann.studyDesc = str(tag.StudyDescription)
	ann.referringMD = formatDicomPersonName(str(tag.ReferringPhysicianName))

	// Series identity
	ann.modality = str(tag.Modality)
	sn, sd := str(tag.SeriesNumber), str(tag.SeriesDescription)
	switch {
	case sn != "" && sd != "":
		ann.seriesInfo = "Ser " + sn + " — " + sd
	case sn != "":
		ann.seriesInfo = "Ser " + sn
	case sd != "":
		ann.seriesInfo = sd
	}
	if t := str(tag.SliceThickness); t != "" {
		ann.sliceThick = "T: " + t + " mm"
	}
	ann.protocol = str(tag.ProtocolName)

	// Image geometry
	if loc := str(tag.SliceLocation); loc != "" {
		if f, err := strconv.ParseFloat(loc, 64); err == nil {
			ann.sliceLoc = fmt.Sprintf("Loc: %.1f mm", f)
		}
	}
	if e, err := ds.FindElementByTag(tag.PixelSpacing); err == nil {
		strs := sdicom.MustGetStrings(e.Value)
		if len(strs) >= 2 {
			r, e1 := strconv.ParseFloat(strings.TrimSpace(strs[0]), 64)
			c, e2 := strconv.ParseFloat(strings.TrimSpace(strs[1]), 64)
			if e1 == nil && e2 == nil {
				ann.pixelSpacing = fmt.Sprintf("%.4f × %.4f mm", r, c)
			}
		}
	}

	// Orientation markers from ImageOrientationPatient (6 direction cosines)
	if e, err := ds.FindElementByTag(tag.ImageOrientationPatient); err == nil {
		strs := sdicom.MustGetStrings(e.Value)
		if len(strs) == 6 {
			cos := make([]float64, 6)
			ok := true
			for i, s := range strs {
				v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
				if err != nil {
					ok = false
					break
				}
				cos[i] = v
			}
			if ok {
				// Row cosines (cos[0..2]): direction from left→right edge of image.
				// Col cosines (cos[3..5]): direction from top→bottom edge of image.
				ann.orientRight = dominantOrientLabel(cos[0], cos[1], cos[2])
				ann.orientLeft = flipOrientLabel(ann.orientRight)
				ann.orientBottom = dominantOrientLabel(cos[3], cos[4], cos[5])
				ann.orientTop = flipOrientLabel(ann.orientBottom)
			}
		}
	}

	return ann
}

// dominantOrientLabel returns the anatomical direction label for a direction
// cosine vector in DICOM LPS patient coordinates:
//
//	+X = patient Left,  +Y = patient Posterior,  +Z = patient Head (Superior)
func dominantOrientLabel(x, y, z float64) string {
	ax, ay, az := math.Abs(x), math.Abs(y), math.Abs(z)
	switch {
	case ax >= ay && ax >= az:
		if x > 0 {
			return "L"
		}
		return "R"
	case ay >= ax && ay >= az:
		if y > 0 {
			return "P"
		}
		return "A"
	default:
		if z > 0 {
			return "H"
		}
		return "F"
	}
}

func flipOrientLabel(l string) string {
	return map[string]string{"L": "R", "R": "L", "A": "P", "P": "A", "H": "F", "F": "H"}[l]
}

func formatDicomPersonName(s string) string {
	if s == "" {
		return ""
	}
	// DICOM: "LAST^FIRST^MIDDLE" — render as "First Last"
	parts := strings.SplitN(s, "^", 3)
	last := strings.TrimSpace(parts[0])
	first := ""
	if len(parts) > 1 {
		first = strings.TrimSpace(parts[1])
	}
	if first != "" && last != "" {
		return first + " " + last
	}
	return strings.TrimSpace(first + last)
}

func formatDicomDate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 8 {
		return s[0:4] + "-" + s[4:6] + "-" + s[6:8]
	}
	return s
}

func formatDicomTime(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "."); i >= 0 {
		s = s[:i]
	}
	if len(s) >= 6 {
		return s[0:2] + ":" + s[2:4] + ":" + s[4:6]
	}
	if len(s) >= 4 {
		return s[0:2] + ":" + s[2:4]
	}
	return s
}

// unsupportedTransferSyntaxNames maps encapsulated DICOM Transfer Syntax UIDs
// to readable format names. The suyashkumar/dicom library passes all
// encapsulated frames to jpeg.Decode regardless of transfer syntax, so any
// format other than JPEG Baseline (1.2.840.10008.1.2.4.50) and JPEG Extended
// (1.2.840.10008.1.2.4.51) fails with a misleading JPEG decode error.
// unsupportedTransferSyntaxNames lists encapsulated transfer syntaxes the
// built-in viewer cannot decode. JPEG 2000 (…4.90/…4.91) is intentionally absent
// — it is handled by decodeJPEG2000Frame when built with the openjpeg tag, and
// otherwise reports its own "not built in" message. JPEG Lossless (…4.57/…4.70)
// stays listed — transcode.go uses these names in its own error text — but the
// viewer veto lets it pass when built with the jpeglossless tag, where it is
// handled by decodeJPEGLosslessFrame.
var unsupportedTransferSyntaxNames = map[string]string{
	"1.2.840.10008.1.2.4.57": "JPEG Lossless Non-Hierarchical",
	"1.2.840.10008.1.2.4.70": "JPEG Lossless (Process 14, SV1)",
	"1.2.840.10008.1.2.4.80": "JPEG-LS Lossless",
	"1.2.840.10008.1.2.4.81": "JPEG-LS Near-Lossless",
	"1.2.840.10008.1.2.5":    "RLE Lossless",
}

// viewerState holds one decoded DICOM instance ready for display. img is the
// frame rendered at its default window (used by thumbnails and as the initial
// view); frame retains the decoded pixel data so the interactive viewer can
// re-window without re-reading the file.
type viewerState struct {
	img   image.Image
	frame *decodedFrame
	label string
	ann   imageAnnotations
}

// decodedFrame holds a single frame's pixel data decoded once, so that
// window/level changes re-render from memory (a tight loop over gray) instead
// of re-parsing the file. Colour frames are not windowable: colorImg is set and
// render returns it unchanged.
type decodedFrame struct {
	rows, cols int
	colorImg   image.Image // non-nil for RGB / JPEG-decoded frames (not windowable)
	invert     bool        // MONOCHROME1 — invert the display ramp

	// Greyscale samples, in one of two forms (see grayframe.go). Usually
	// indexed: pixel i's stored value is rawBase+rawIdx[i], displayed as
	// rescaled(rawIdx[i]), and rawSpan is the stored range's size. When the
	// stored values span more than 65,536, gray holds the rescaled values
	// directly instead. Both nil for colour.
	rawIdx           []uint16
	rawBase          int64
	rawSpan          int
	slope, intercept float64
	gray             []float32

	wc, ww         float64 // default window centre/width (from tags or auto)
	lo, hi         float64 // full rescaled data range, for the "Full range" preset
	windowFromTags bool    // true if wc/ww came from DICOM Window tags
	modality       string  // DICOM Modality (CT, PT, MR, …); selects the preset set

	overlays []dicomOverlay // decoded overlay planes, composited during render
}

// dicomOverlay holds one decoded DICOM overlay plane (groups 6000–60FE).
// pixels is a flat [rows*cols] byte array: 1 where the overlay bit is set, 0 elsewhere.
// originRow/Col are 1-based DICOM coordinates of the overlay's top-left corner.
type dicomOverlay struct {
	rows, cols           int
	originRow, originCol int
	pixels               []byte
}

// windowable reports whether window/level adjustment affects this frame.
func (d *decodedFrame) windowable() bool { return d != nil && d.colorImg == nil }

// render produces a displayable image at the given window centre/width, mapped
// through the supplied colour map (nil = grayscale).
func (d *decodedFrame) render(cm *colorMap, wc, ww float64) image.Image {
	if d.colorImg != nil {
		if len(d.overlays) == 0 {
			return d.colorImg
		}
		// Composite overlays onto a copy so the cached colorImg is not modified.
		b := d.colorImg.Bounds()
		dst := image.NewRGBA(b)
		if src, ok := d.colorImg.(*image.RGBA); ok {
			copy(dst.Pix, src.Pix)
		} else {
			for y := b.Min.Y; y < b.Max.Y; y++ {
				for x := b.Min.X; x < b.Max.X; x++ {
					dst.Set(x, y, d.colorImg.At(x, y))
				}
			}
		}
		d.paintOverlays(dst)
		return dst
	}
	img := image.NewRGBA(image.Rect(0, 0, d.cols, d.rows))
	d.renderInto(img, cm, wc, ww)
	d.paintOverlays(img)
	return img
}

// renderInto windows the frame into an existing *image.RGBA (dst must be
// cols×rows) and applies the colour map. Reusing a buffer across window/map
// changes avoids per-drag allocation and keeps the canvas.Image backing pointer
// stable, which is important for flicker-free interactive window/level dragging.
// *image.RGBA also uploads to the GPU without conversion.
func (d *decodedFrame) renderInto(dst *image.RGBA, cm *colorMap, wc, ww float64) {
	if !d.hasSamples() {
		return
	}
	if cm == nil {
		cm = &grayscaleMap
	}
	if ww < 1 {
		ww = 1
	}
	if d.rawIdx != nil {
		d.renderIndexed(dst, cm, wc, ww)
		return
	}
	lower := wc - ww/2
	inv := d.invert
	// Divide by ww before scaling to 255 (rather than premultiplying 255/ww) so
	// that a pixel exactly at the top of the window maps to 255, not 254. The
	// windowed intensity indexes the colour map. dst.Stride == 4*cols for a
	// cols×rows RGBA image, so pixel i starts at Pix[i*4].
	for i, v := range d.gray {
		idx := clampToUint8((float64(v) - lower) / ww * 255)
		if inv {
			idx = 255 - idx
		}
		c := cm.lut[idx]
		j := i * 4
		dst.Pix[j] = c[0]
		dst.Pix[j+1] = c[1]
		dst.Pix[j+2] = c[2]
		dst.Pix[j+3] = 255
	}
}

// paintOverlays composites all DICOM overlay planes onto dst in opaque yellow.
// Called at the end of renderInto (grayscale hot path) and once for colour frames.
func (d *decodedFrame) paintOverlays(dst *image.RGBA) {
	for _, ov := range d.overlays {
		baseRow := ov.originRow - 1
		baseCol := ov.originCol - 1
		for r := 0; r < ov.rows; r++ {
			imgRow := baseRow + r
			if imgRow < 0 || imgRow >= d.rows {
				continue
			}
			rowBase := r * ov.cols
			for c := 0; c < ov.cols; c++ {
				if ov.pixels[rowBase+c] == 0 {
					continue
				}
				imgCol := baseCol + c
				if imgCol < 0 || imgCol >= d.cols {
					continue
				}
				j := (imgRow*d.cols + imgCol) * 4
				dst.Pix[j] = 0xFF
				dst.Pix[j+1] = 0xFF
				dst.Pix[j+2] = 0x00
				dst.Pix[j+3] = 0xFF
			}
		}
	}
}

// extractOverlays scans all dataset elements for DICOM overlay planes in groups
// 6000–60FE and returns the decoded planes ready for compositing. Overlays that
// use the deprecated bit-position-in-pixel-data encoding are skipped.
func extractOverlays(ds sdicom.Dataset) []dicomOverlay {
	type attrs struct {
		rows, cols           int
		originRow, originCol int
		bitPos               int
		data                 []byte
	}
	groups := make(map[uint16]*attrs)

	for _, elem := range ds.Elements {
		g := elem.Tag.Group
		if g < 0x6000 || g > 0x60FE || g%2 != 0 {
			continue
		}
		a := groups[g]
		if a == nil {
			a = &attrs{originRow: 1, originCol: 1}
			groups[g] = a
		}
		switch elem.Tag.Element {
		case 0x0010: // Overlay Rows (US)
			if ints, ok := elem.Value.GetValue().([]int); ok && len(ints) > 0 {
				a.rows = ints[0]
			}
		case 0x0011: // Overlay Columns (US)
			if ints, ok := elem.Value.GetValue().([]int); ok && len(ints) > 0 {
				a.cols = ints[0]
			}
		case 0x0050: // Overlay Origin (SS[2]: row, col)
			if ints, ok := elem.Value.GetValue().([]int); ok && len(ints) >= 2 {
				a.originRow = ints[0]
				a.originCol = ints[1]
			}
		case 0x0102: // Overlay Bit Position (US)
			if ints, ok := elem.Value.GetValue().([]int); ok && len(ints) > 0 {
				a.bitPos = ints[0]
			}
		case 0x3000: // Overlay Data (OB or OW)
			if b, ok := elem.Value.GetValue().([]byte); ok {
				a.data = b
			}
		}
	}

	var result []dicomOverlay
	for _, a := range groups {
		if len(a.data) == 0 || a.rows <= 0 || a.cols <= 0 {
			continue
		}
		if a.bitPos != 0 {
			// Overlay stored inside pixel-data bit planes (retired 2004); skip.
			continue
		}
		total := a.rows * a.cols
		pixels := make([]byte, total)
		for i := 0; i < total; i++ {
			byteIdx := i / 8
			if byteIdx < len(a.data) && (a.data[byteIdx]>>uint(i%8))&1 != 0 {
				pixels[i] = 1
			}
		}
		result = append(result, dicomOverlay{
			rows:      a.rows,
			cols:      a.cols,
			originRow: a.originRow,
			originCol: a.originCol,
			pixels:    pixels,
		})
	}
	return result
}

// computeDefaultWindow fills wc/ww/lo/hi for a freshly decoded grayscale frame.
// If the file carried explicit Window tags they win; otherwise the window is
// derived from the 1st–99th percentile of the rescaled values (good contrast
// for PET/NM and other modalities lacking window tags); failing that, the full
// data range is used.
func (d *decodedFrame) computeDefaultWindow(hasWindow bool, wc, ww float64) {
	lo, hi, ok := d.grayRange()
	if !ok {
		lo, hi = 0, 0
	}
	d.lo, d.hi = lo, hi

	if hasWindow && ww > 0 {
		d.wc, d.ww, d.windowFromTags = wc, ww, true
		return
	}
	if ok && hi > lo {
		plo, phi := d.percentiles()
		if phi > plo {
			d.wc, d.ww = (plo+phi)/2, phi-plo
			return
		}
	}
	if hi > lo {
		d.wc, d.ww = (lo+hi)/2, hi-lo
	} else {
		d.wc, d.ww = lo, 1
	}
}

// wlDragRangeFraction sets window/level drag sensitivity: dragging the full
// viewport width (~512 px) shifts the window by this fraction of the frame's
// data range. Lower = less sensitive / finer control.
const wlDragRangeFraction = 0.25

// wlPresetKind selects how a wlPreset's parameters are interpreted, so that the
// same preset list can mix absolute windows (CT Hounsfield), windows expressed
// as a fraction of peak intensity (PET), and windows scaled relative to the
// frame's own auto window (MR and other modalities without absolute units).
type wlPresetKind int

const (
	wlDefault    wlPresetKind = iota // the frame's own/auto window (params ignored)
	wlFullRange                      // the full data range lo..hi (params ignored)
	wlAbsolute                       // a = centre, b = width (absolute units, e.g. HU)
	wlZeroToFrac                     // window 0 .. b×hi  (PET: fraction of peak)
	wlWidthScale                     // centre = frame's, width = frame's × b (relative)
)

// wlPreset is a named window/level preset whose meaning depends on kind.
type wlPreset struct {
	name string
	kind wlPresetKind
	a, b float64
}

// resolve turns a preset into a concrete (centre, width) for a given frame.
func (p wlPreset) resolve(df *decodedFrame) (wc, ww float64) {
	switch p.kind {
	case wlFullRange:
		ww = df.hi - df.lo
		if ww < 1 {
			ww = 1
		}
		return (df.lo + df.hi) / 2, ww
	case wlAbsolute:
		return p.a, p.b
	case wlZeroToFrac:
		upper := df.hi * p.b
		if upper < 1 {
			upper = 1
		}
		return upper / 2, upper // window spans 0 .. upper
	case wlWidthScale:
		ww = df.ww * p.b
		if ww < 1 {
			ww = 1
		}
		return df.wc, ww
	default: // wlDefault
		return df.wc, df.ww
	}
}

// CT: standard Hounsfield windows. Valid because CT pixel values are HU after
// RescaleSlope/Intercept.
var ctPresets = []wlPreset{
	{"Default", wlDefault, 0, 0},
	{"Full range", wlFullRange, 0, 0},
	{"Brain", wlAbsolute, 40, 80},
	{"Subdural", wlAbsolute, 75, 280},
	{"Soft tissue", wlAbsolute, 50, 400},
	{"Liver", wlAbsolute, 60, 160},
	{"Mediastinum", wlAbsolute, 50, 350},
	{"Bone", wlAbsolute, 480, 2500},
	{"Lung", wlAbsolute, -600, 1500},
}

// PET: PET brightness is conventionally set as a fraction of the peak value
// (e.g. SUVmax), so each preset windows from 0 up to a percentage of the peak.
// A lower percentage brightens low-uptake regions and raises contrast.
var petPresets = []wlPreset{
	{"Default", wlDefault, 0, 0},
	{"Full range", wlFullRange, 0, 0},
	{"0 → 75%", wlZeroToFrac, 0, 0.75},
	{"0 → 50%", wlZeroToFrac, 0, 0.50},
	{"0 → 40%", wlZeroToFrac, 0, 0.40},
	{"0 → 30%", wlZeroToFrac, 0, 0.30},
	{"0 → 20%", wlZeroToFrac, 0, 0.20},
}

// MR: MR intensities have no absolute scale, so presets adjust contrast relative
// to the frame's own auto window rather than using fixed numbers.
var mrPresets = []wlPreset{
	{"Default", wlDefault, 0, 0},
	{"Full range", wlFullRange, 0, 0},
	{"Lower contrast", wlWidthScale, 0, 2.0},
	{"Higher contrast", wlWidthScale, 0, 0.5},
	{"Highest contrast", wlWidthScale, 0, 0.25},
}

// genericPresets apply to modalities without a dedicated set; the relative
// contrast entries are meaningful for any grayscale image.
var genericPresets = []wlPreset{
	{"Default", wlDefault, 0, 0},
	{"Full range", wlFullRange, 0, 0},
	{"Lower contrast", wlWidthScale, 0, 2.0},
	{"Higher contrast", wlWidthScale, 0, 0.5},
}

// presetsForModality returns the W/L preset list appropriate for a DICOM
// Modality code (CT, PT = PET, NM = SPECT, MR), falling back to a generic set.
func presetsForModality(mod string) []wlPreset {
	switch strings.ToUpper(strings.TrimSpace(mod)) {
	case "CT":
		return ctPresets
	case "PT", "NM":
		// PET and SPECT/NM share the same fraction-of-peak brightness windows.
		return petPresets
	case "MR":
		return mrPresets
	default:
		return genericPresets
	}
}

// dicomIntParam reads an integer attribute from a suyashkumar Dataset, returning 0 if absent.
func dicomIntParam(ds sdicom.Dataset, t tag.Tag) int {
	e, err := ds.FindElementByTag(t)
	if err != nil {
		return 0
	}
	vals := sdicom.MustGetInts(e.Value)
	if len(vals) == 0 {
		return 0
	}
	return vals[0]
}

// decodeRawPixelFallback decodes raw uncompressed pixel bytes into a decodedFrame
// using the image dimensions and bit depth supplied by the caller.
//
// This is invoked when the suyashkumar/dicom library wraps native pixels in an
// EncapsulatedFrame — which happens when the PixelData element uses an
// undefined-length VL (technically non-conformant but common in older DICOM
// implementations) with an uncompressed transfer syntax.
func decodeRawPixelFallback(data []byte, rows, cols, samplesPerPixel, bitsAlloc int,
	hasWindow bool, wc, ww, slope, intercept float64, isSigned bool, photometric string) (*decodedFrame, error) {

	pixelsPerFrame := rows * cols
	if pixelsPerFrame <= 0 {
		return nil, errors.New("raw pixel fallback: invalid image dimensions")
	}

	// ── RGB ───────────────────────────────────────────────────────────────────
	if samplesPerPixel == 3 {
		bytesNeeded := pixelsPerFrame * 3
		if bitsAlloc > 8 {
			bytesNeeded *= 2
		}
		if len(data) < bytesNeeded {
			return nil, fmt.Errorf("raw pixel fallback: data too short for RGB (%d < %d)", len(data), bytesNeeded)
		}
		maxVal := float64(int(1)<<uint(bitsAlloc)) - 1
		if maxVal <= 0 {
			maxVal = 255
		}
		img := image.NewNRGBA(image.Rect(0, 0, cols, rows))
		if bitsAlloc <= 8 {
			for i := 0; i < pixelsPerFrame; i++ {
				img.Pix[i*4] = uint8(float64(data[i*3]) / maxVal * 255)
				img.Pix[i*4+1] = uint8(float64(data[i*3+1]) / maxVal * 255)
				img.Pix[i*4+2] = uint8(float64(data[i*3+2]) / maxVal * 255)
				img.Pix[i*4+3] = 255
			}
		} else {
			for i := 0; i < pixelsPerFrame; i++ {
				r := float64(binary.LittleEndian.Uint16(data[i*6:])) / maxVal * 255
				g := float64(binary.LittleEndian.Uint16(data[i*6+2:])) / maxVal * 255
				b := float64(binary.LittleEndian.Uint16(data[i*6+4:])) / maxVal * 255
				img.Pix[i*4] = clampToUint8(r)
				img.Pix[i*4+1] = clampToUint8(g)
				img.Pix[i*4+2] = clampToUint8(b)
				img.Pix[i*4+3] = 255
			}
		}
		return &decodedFrame{rows: rows, cols: cols, colorImg: img}, nil
	}

	// ── Grayscale ─────────────────────────────────────────────────────────────
	bytesNeeded := pixelsPerFrame
	if bitsAlloc > 8 {
		bytesNeeded *= 2
	}
	if len(data) < bytesNeeded {
		return nil, fmt.Errorf("raw pixel fallback: data too short for grayscale (%d < %d)", len(data), bytesNeeded)
	}

	invert := photometric == "MONOCHROME1"
	var df *decodedFrame
	switch {
	case bitsAlloc <= 8:
		df = newGrayFrame(rows, cols, data[:pixelsPerFrame], slope, intercept, invert)
	case isSigned:
		s := make([]int16, pixelsPerFrame)
		for i := range s {
			s[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
		}
		df = newGrayFrame(rows, cols, s, slope, intercept, invert)
	default:
		s := make([]uint16, pixelsPerFrame)
		for i := range s {
			s[i] = binary.LittleEndian.Uint16(data[i*2:])
		}
		df = newGrayFrame(rows, cols, s, slope, intercept, invert)
	}
	df.computeDefaultWindow(hasWindow, wc, ww)
	return df, nil
}

// parsedDicom is one DICOM file parsed once: its frames, plus every
// dataset-derived parameter needed to decode and present any one of them.
// Parsing is separated from decoding so that a multi-frame acquisition — NM and
// SPECT store 40-240 frames in a single file — is read from disk once and then
// decoded frame by frame as the user scrolls.
type parsedDicom struct {
	frames         []*frame.Frame
	transferSyntax string

	hasWindow bool
	wc, ww    float64
	slope     float64
	intercept float64
	isSigned  bool
	bitsAlloc int
	// Dimensions for the raw-pixel fallback path.
	rows, cols      int
	samplesPerPixel int
	photometric     string

	ann      imageAnnotations
	overlays []dicomOverlay
}

// frameCount is the number of decodable frames in the file (at least 1 for any
// successfully parsed file).
func (p *parsedDicom) frameCount() int { return len(p.frames) }

// frameState decodes one frame and renders it at its default window, ready for
// display. idx is clamped, so a file whose declared NumberOfFrames overstates
// what the parser could deliver still shows an image instead of failing.
func (p *parsedDicom) frameState(idx int) (viewerState, error) {
	return p.frameStateOpts(idx, frameDecodeOpts{})
}

// frameDecodeOpts tunes one frame's decode for where the image is going. The
// zero value — one thread, full resolution — is right for every pool that
// decodes many frames at once (clip buffer, filmstrip, overview, conversions),
// which already keep every core busy.
type frameDecodeOpts struct {
	// threads lets a codec that can split one image across threads do so —
	// JPEG 2000, whose code-blocks decode independently. Only for a decode the
	// user is waiting on, one at a time; see interactiveDecodeOpts.
	threads int
	// maxSide > 0 says only an image about this large is needed (a thumbnail),
	// so a codec that stores several resolutions (JPEG 2000) may decode a
	// smaller one — never smaller than maxSide on the longer side. The frame
	// then has the reduced dimensions, and frameStateOpts drops its overlays,
	// which are in full-resolution coordinates.
	maxSide int
	// skipRender leaves viewerState.img nil, for the viewer, whose viewport
	// windows the frame itself: the still image is never looked at there, and
	// would cost a render plus 4 bytes a pixel in every cached slice.
	skipRender bool
}

// interactiveDecodeOpts is for the frame on screen that the user is waiting on:
// every core on that one image.
func interactiveDecodeOpts() frameDecodeOpts {
	return frameDecodeOpts{threads: runtime.NumCPU()}
}

// viewerDecodeOpts is interactiveDecodeOpts for the viewer window, which renders
// through its viewport and never uses the still image.
func viewerDecodeOpts(interactive bool) frameDecodeOpts {
	opts := frameDecodeOpts{skipRender: true}
	if interactive {
		opts.threads = runtime.NumCPU()
	}
	return opts
}

// frameStateOpts is frameState with decode options (see frameDecodeOpts).
func (p *parsedDicom) frameStateOpts(idx int, opts frameDecodeOpts) (viewerState, error) {
	if len(p.frames) == 0 {
		return viewerState{}, errors.New("no pixel data in file")
	}
	f := p.frames[clampInt(idx, 0, len(p.frames)-1)]

	df, err := decodeFrame(f, opts, p.transferSyntax, p.hasWindow, p.wc, p.ww,
		p.slope, p.intercept, p.isSigned, p.bitsAlloc, p.photometric)

	// Fallback: some DICOM implementations store uncompressed pixel data with
	// an undefined-length VL, which the library mistakes for encapsulated (JPEG)
	// data. When jpeg.Decode fails, re-interpret the raw bytes natively. This
	// never applies to JPEG 2000 or JPEG Lossless, whose bytes are a genuine
	// codestream.
	if err != nil && f.IsEncapsulated() && p.rows > 0 && p.cols > 0 &&
		!isJPEG2000TransferSyntax(p.transferSyntax) && !isJPEGLosslessTransferSyntax(p.transferSyntax) {
		df, err = decodeRawPixelFallback(
			f.EncapsulatedData.Data,
			p.rows, p.cols, p.samplesPerPixel, p.bitsAlloc,
			p.hasWindow, p.wc, p.ww, p.slope, p.intercept, p.isSigned, p.photometric,
		)
	}
	if err != nil {
		return viewerState{}, err
	}

	df.modality = p.ann.modality
	// Overlay planes are positioned in the file's own pixel grid; a frame
	// decoded at reduced resolution (a thumbnail) would place them wrongly.
	if p.rows <= 0 || df.rows == p.rows {
		df.overlays = p.overlays
	}

	// Render the still image (thumbnails, initial view) through the modality's
	// default colour map so NM/PET overviews appear in colour like the viewer.
	var img image.Image
	if !opts.skipRender {
		img = df.render(colorMapByName(defaultColorMapForModality(df.modality)), df.wc, df.ww)
	}
	label := fmt.Sprintf("%d × %d", df.cols, df.rows)
	ann := p.ann // copy: windowStr is per-frame
	if df.windowable() {
		label += fmt.Sprintf("   W:%.0f  L:%.0f", df.ww, df.wc)
		ann.windowStr = fmt.Sprintf("W: %.0f  L: %.0f", df.ww, df.wc)
	}
	return viewerState{img: img, frame: df, label: label, ann: ann}, nil
}

// dicomFileCache holds the most recently parsed file so that scrolling through
// a multi-frame acquisition decodes only the requested frame instead of
// re-reading and re-parsing the whole file for every frame. One cache belongs
// to one viewer window and is released with it.
type dicomFileCache struct {
	mu     sync.Mutex
	path   string
	parsed *parsedDicom
}

// load returns the given frame of the given file, parsing the file only when it
// is not the one already cached. The lock is held across the decode so that two
// navigation events cannot parse the same file concurrently.
func (c *dicomFileCache) load(path string, frameIdx int) (viewerState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.parsed == nil || c.path != path {
		p, err := parseDicomFile(path)
		if err != nil {
			// Drop the stale entry: the next attempt should re-parse rather than
			// serve frames of a file the viewer has navigated away from.
			c.path, c.parsed = "", nil
			return viewerState{}, err
		}
		c.path, c.parsed = path, p
	}
	// The viewer's on-demand path: one frame, and the user is waiting on it.
	return c.parsed.frameStateOpts(frameIdx, viewerDecodeOpts(true))
}

// decodeViewerSlice decodes one slice-mode slice for the viewer: a single-frame
// file, so parsed afresh — there is nothing to reuse between slices.
func decodeViewerSlice(key viewerSlice, interactive bool) (viewerState, error) {
	p, err := parseDicomFile(key.path)
	if err != nil {
		return viewerState{}, err
	}
	return p.frameStateOpts(key.frame, viewerDecodeOpts(interactive))
}

// loadDicomImage parses a DICOM file and returns its first frame, windowed and
// rendered. Callers that navigate frames use parseDicomFile/frameState (via
// dicomFileCache) instead so the file is parsed only once.
func loadDicomImage(path string) (viewerState, error) {
	p, err := parseDicomFile(path)
	if err != nil {
		return viewerState{}, err
	}
	return p.frameState(0)
}

// loadDicomFrame parses a DICOM file and returns the requested frame.
func loadDicomFrame(path string, frameIdx int) (viewerState, error) {
	p, err := parseDicomFile(path)
	if err != nil {
		return viewerState{}, err
	}
	return p.frameState(frameIdx)
}

// loadDicomThumbnail is loadDicomFrame for an image that will only be shown
// about maxSide pixels across: a JPEG 2000 frame decodes at the smallest stored
// resolution still that large, rather than in full to be scaled down. A large
// CR or mammogram thumbnail decodes a small fraction of the codestream.
func loadDicomThumbnail(path string, frameIdx, maxSide int) (viewerState, error) {
	p, err := parseDicomFile(path)
	if err != nil {
		return viewerState{}, err
	}
	return p.frameStateOpts(frameIdx, frameDecodeOpts{maxSide: maxSide})
}

// parseDicomFile reads a DICOM file's frames and the parameters needed to
// decode them. It does not decode any pixels.
func parseDicomFile(path string) (*parsedDicom, error) {
	// The parser pushes each frame into frameCh with a blocking send DURING
	// ParseFile, so the channel must be drained concurrently: draining only
	// after ParseFile returns deadlocks on any file with more frames than the
	// channel buffer — NM/SPECT stores the whole acquisition as one file of
	// 40-240 frames (CT/MR are one frame per file, which is why only NM
	// studies froze the preview).
	frameCh := make(chan *frame.Frame, 8)
	collected := make(chan []*frame.Frame, 1)
	go func() {
		var fs []*frame.Frame
		for f := range frameCh {
			fs = append(fs, f)
		}
		collected <- fs
	}()
	ds, err := safeParseFile(path, frameCh)
	if err != nil {
		// The library closes frameCh on success, and leaves it open when it
		// returns an error or panics. After it returns no sender remains, so
		// closing here lets the collector goroutine finish instead of blocking
		// forever on the open channel.
		//
		// The recover is for the one path where the library has already closed
		// it and still reports failure: Parser.Next closes the channel before
		// returning ErrorEndOfDICOM, which parseInternal propagates as an error.
		// Closing again would panic — on a goroutine that may not be the main
		// one — so the double close is absorbed rather than risked.
		func() {
			defer func() { _ = recover() }()
			close(frameCh)
		}()
		<-collected
		return nil, err
	}
	frames := <-collected
	if len(frames) == 0 {
		return nil, errors.New("no pixel data in file")
	}

	transferSyntax := ""
	if e, err2 := ds.FindElementByTag(tag.TransferSyntaxUID); err2 == nil {
		if strs := sdicom.MustGetStrings(e.Value); len(strs) > 0 {
			transferSyntax = strings.TrimSpace(strs[0])
		}
	}
	// Reject encapsulated transfer syntaxes the built-in viewer cannot decode
	// (JPEG-LS, RLE, lossless JPEG) up front with a clear message instead of a
	// raw decode error. JPEG 2000 is handled by decodeFrame and is not listed;
	// JPEG Lossless is listed but passes through when its decoder is built in.
	if name, unsup := unsupportedTransferSyntaxNames[transferSyntax]; unsup {
		if !(jpegLosslessAvailable && isJPEGLosslessTransferSyntax(transferSyntax)) {
			return nil, fmt.Errorf(
				"%s compressed images cannot be decoded by the built-in viewer\n\nUse Open in Viewer to open this file in an external DICOM viewer.", name)
		}
	}

	wc, ww, hasWindow := dicomWindowParams(ds)
	slope, intercept := dicomRescaleParams(ds)
	isSigned := dicomPixelRepresentation(ds) == 1
	bitsAlloc := dicomBitsAllocated(ds)
	photometric := dicomPhotometricInterp(ds)

	// Dimensions are needed for the raw-pixel fallback path below.
	rows := dicomIntParam(ds, tag.Rows)
	cols := dicomIntParam(ds, tag.Columns)
	samplesPerPixel := dicomIntParam(ds, tag.SamplesPerPixel)
	if samplesPerPixel <= 0 {
		samplesPerPixel = 1
	}

	// A single-frame encapsulated image may legally arrive split across several
	// fragments (PS3.5 §A.4); the parser emits one frame per fragment, so
	// frames[0] alone would be a truncated codestream. Reassemble the full
	// stream before decoding (Philips echo JPEG Lossless files ship ~5
	// fragments per image). NumberOfFrames is VR IS, so datasetInt — not
	// dicomIntParam, which panics on string values — must read it.
	nFrames := datasetInt(&ds, tag.NumberOfFrames, 1)
	if nFrames <= 1 && len(frames) > 1 && frames[0].IsEncapsulated() {
		if merged, mErr := mergeEncapsulatedFragments(frames); mErr == nil {
			frames = []*frame.Frame{merged}
		}
	}
	// A multi-frame file split into several fragments per frame cannot be mapped
	// back to frames without the Basic Offset Table, so the navigable count is
	// whatever the parser delivered; say so rather than silently showing a
	// different number of images than the header declares.
	if nFrames > 1 && nFrames != len(frames) {
		logWarn("%s: declares %d frames but the parser delivered %d — showing %d",
			filepath.Base(path), nFrames, len(frames), len(frames))
	}

	return &parsedDicom{
		frames:          frames,
		transferSyntax:  transferSyntax,
		hasWindow:       hasWindow,
		wc:              wc,
		ww:              ww,
		slope:           slope,
		intercept:       intercept,
		isSigned:        isSigned,
		bitsAlloc:       bitsAlloc,
		rows:            rows,
		cols:            cols,
		samplesPerPixel: samplesPerPixel,
		photometric:     photometric,
		ann:             extractAnnotationsFromDataset(ds),
		overlays:        extractOverlays(ds),
	}, nil
}

func dicomWindowParams(ds sdicom.Dataset) (center, width float64, ok bool) {
	wcElem, e1 := ds.FindElementByTag(tag.WindowCenter)
	wwElem, e2 := ds.FindElementByTag(tag.WindowWidth)
	if e1 != nil || e2 != nil {
		return 0, 0, false
	}
	wcs := sdicom.MustGetStrings(wcElem.Value)
	wws := sdicom.MustGetStrings(wwElem.Value)
	if len(wcs) == 0 || len(wws) == 0 {
		return 0, 0, false
	}
	c, e1 := strconv.ParseFloat(strings.TrimSpace(wcs[0]), 64)
	w2, e2 := strconv.ParseFloat(strings.TrimSpace(wws[0]), 64)
	if e1 != nil || e2 != nil || w2 <= 0 {
		return 0, 0, false
	}
	return c, w2, true
}

func dicomRescaleParams(ds sdicom.Dataset) (slope, intercept float64) {
	slope = 1.0
	if e, err := ds.FindElementByTag(tag.RescaleSlope); err == nil {
		if strs := sdicom.MustGetStrings(e.Value); len(strs) > 0 {
			if v, err := strconv.ParseFloat(strings.TrimSpace(strs[0]), 64); err == nil {
				slope = v
			}
		}
	}
	if e, err := ds.FindElementByTag(tag.RescaleIntercept); err == nil {
		if strs := sdicom.MustGetStrings(e.Value); len(strs) > 0 {
			if v, err := strconv.ParseFloat(strings.TrimSpace(strs[0]), 64); err == nil {
				intercept = v
			}
		}
	}
	return
}

func dicomPixelRepresentation(ds sdicom.Dataset) int {
	e, err := ds.FindElementByTag(tag.PixelRepresentation)
	if err != nil {
		return 0
	}
	vals := sdicom.MustGetInts(e.Value)
	if len(vals) == 0 {
		return 0
	}
	return vals[0]
}

func dicomBitsAllocated(ds sdicom.Dataset) int {
	e, err := ds.FindElementByTag(tag.BitsAllocated)
	if err != nil {
		return 16
	}
	vals := sdicom.MustGetInts(e.Value)
	if len(vals) == 0 {
		return 16
	}
	return vals[0]
}

func dicomPhotometricInterp(ds sdicom.Dataset) string {
	e, err := ds.FindElementByTag(tag.PhotometricInterpretation)
	if err != nil {
		return ""
	}
	strs := sdicom.MustGetStrings(e.Value)
	if len(strs) == 0 {
		return ""
	}
	return strings.TrimSpace(strs[0])
}

// jpeg2000TransferSyntaxes are the DICOM JPEG 2000 transfer syntax UIDs
// (lossless-only and the general JPEG 2000 compression).
var jpeg2000TransferSyntaxes = map[string]bool{
	"1.2.840.10008.1.2.4.90": true, // JPEG 2000 Image Compression (Lossless Only)
	"1.2.840.10008.1.2.4.91": true, // JPEG 2000 Image Compression
}

func isJPEG2000TransferSyntax(ts string) bool {
	return jpeg2000TransferSyntaxes[strings.TrimSpace(ts)]
}

// jpegLosslessTransferSyntaxes are the DICOM JPEG Lossless (ITU-T T.81
// process 14, SOF3) transfer syntax UIDs.
var jpegLosslessTransferSyntaxes = map[string]bool{
	"1.2.840.10008.1.2.4.57": true, // JPEG Lossless, Non-Hierarchical (Process 14)
	"1.2.840.10008.1.2.4.70": true, // JPEG Lossless, Non-Hierarchical, First-Order Prediction (SV1)
}

func isJPEGLosslessTransferSyntax(ts string) bool {
	return jpegLosslessTransferSyntaxes[strings.TrimSpace(ts)]
}

// decodeFrame converts a parsed DICOM frame into a decodedFrame. Grayscale
// pixels are rescaled (slope/intercept) into a float buffer once so the viewer
// can re-window them cheaply; colour frames are rendered directly and are not
// windowable. The default window comes from DICOM Window tags when present,
// otherwise from the 1st–99th percentile of the rescaled values.
//
// transferSyntax selects the decode path for encapsulated frames: JPEG 2000 is
// handled by the OpenJPEG-backed decoder (decodeJPEG2000Frame), JPEG Lossless
// by the libjpeg-turbo-backed decoder (decodeJPEGLosslessFrame); other
// encapsulated syntaxes fall through to the library's JPEG Baseline decoder.
func decodeFrame(f *frame.Frame, opts frameDecodeOpts, transferSyntax string, hasWindow bool, wc, ww, slope, intercept float64, isSigned bool, bitsAlloc int, photometric string) (*decodedFrame, error) {
	if f.IsEncapsulated() {
		if isJPEG2000TransferSyntax(transferSyntax) {
			return decodeJPEG2000Frame(f.EncapsulatedData.Data, opts, slope, intercept, hasWindow, wc, ww, photometric)
		}
		if isJPEGLosslessTransferSyntax(transferSyntax) {
			return decodeJPEGLosslessFrame(f.EncapsulatedData.Data, slope, intercept, hasWindow, wc, ww, photometric, isSigned)
		}
		img, err := f.GetImage()
		if err != nil {
			return nil, fmt.Errorf("cannot decode compressed pixel data (%w)\n\nUse Open in Viewer to open this file in an external DICOM viewer.", err)
		}
		b := img.Bounds()
		return &decodedFrame{rows: b.Dy(), cols: b.Dx(), colorImg: img}, nil
	}

	nf, err := f.GetNativeFrame()
	if err != nil {
		return nil, err
	}
	rows, cols, spp, bps := nf.Rows(), nf.Cols(), nf.SamplesPerPixel(), nf.BitsPerSample()
	rawData := nf.RawDataSlice()

	// --- RGB / colour (3 samples per pixel) ---
	if spp == 3 {
		img := image.NewNRGBA(image.Rect(0, 0, cols, rows))
		maxVal := float64(int(1)<<uint(bps)) - 1
		if maxVal <= 0 {
			maxVal = 255
		}
		switch data := rawData.(type) {
		case []uint8:
			for i := 0; i < rows*cols; i++ {
				img.Pix[i*4] = uint8(float64(data[i*3]) / maxVal * 255)
				img.Pix[i*4+1] = uint8(float64(data[i*3+1]) / maxVal * 255)
				img.Pix[i*4+2] = uint8(float64(data[i*3+2]) / maxVal * 255)
				img.Pix[i*4+3] = 255
			}
		case []uint16:
			for i := 0; i < rows*cols; i++ {
				img.Pix[i*4] = uint8(float64(data[i*3]) / maxVal * 255)
				img.Pix[i*4+1] = uint8(float64(data[i*3+1]) / maxVal * 255)
				img.Pix[i*4+2] = uint8(float64(data[i*3+2]) / maxVal * 255)
				img.Pix[i*4+3] = 255
			}
		default:
			img2, err := nf.GetImage()
			if err != nil {
				return nil, err
			}
			return &decodedFrame{rows: rows, cols: cols, colorImg: img2}, nil
		}
		return &decodedFrame{rows: rows, cols: cols, colorImg: img}, nil
	}

	// --- Grayscale (1 sample per pixel): the stored values, see grayframe.go ---
	invert := photometric == "MONOCHROME1"
	var df *decodedFrame
	switch data := rawData.(type) {
	case []uint8:
		df = newGrayFrame(rows, cols, data, slope, intercept, invert)
	case []uint16:
		if isSigned {
			// Same bits read as two's complement — a reinterpretation of the
			// slice rather than a copy of it.
			df = newGrayFrame(rows, cols, unsafe.Slice((*int16)(unsafe.Pointer(unsafe.SliceData(data))), len(data)),
				slope, intercept, invert)
		} else {
			df = newGrayFrame(rows, cols, data, slope, intercept, invert)
		}
	default:
		img2, err := nf.GetImage()
		if err != nil {
			return nil, err
		}
		return &decodedFrame{rows: rows, cols: cols, colorImg: img2}, nil
	}
	df.computeDefaultWindow(hasWindow, wc, ww)
	return df, nil
}

func clampToUint8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v)
}

// seriesThumb bundles a display label, the series modality, and the file paths
// for one series, used by the study overview window.
type seriesThumb struct {
	label    string
	modality string // shown as the thumbnail stand-in when nothing is renderable
	paths    []string
}

// thumbSide is the square edge of a study-overview thumbnail.
const thumbSide = 180

// modalityPlaceholder builds the stand-in tile for a series with nothing to
// display — SR, KO, PR and other non-image objects, or pixel data none of the
// built-in decoders can render: a white square with the modality in black
// bold text scaled to fill about 80% of the tile. Matches the study overview
// in dicomhdr-java.
func modalityPlaceholder(modality string) fyne.CanvasObject {
	txt := strings.ToUpper(strings.TrimSpace(modality))
	if txt == "" {
		txt = "?"
	}
	t := canvas.NewText(txt, color.Black)
	t.TextStyle = fyne.TextStyle{Bold: true}
	// Measure at an arbitrary base size, then scale so the larger dimension
	// fills 80% of the tile.
	const base = 100
	m := fyne.MeasureText(txt, base, t.TextStyle)
	t.TextSize = base * thumbSide * 0.8 / fyne.Max(m.Width, m.Height)
	bg := canvas.NewRectangle(color.White)
	bg.SetMinSize(fyne.NewSize(thumbSide, thumbSide))
	return container.NewStack(bg, container.NewCenter(t))
}

// thumbnailCell is a widget displaying a single DICOM thumbnail — the series'
// middle-slice image, or the modality placeholder when it has none — with a
// label below it. Double-tapping opens the full series viewer.
type thumbnailCell struct {
	widget.BaseWidget
	preview  fyne.CanvasObject
	lbl      *widget.Label
	chapters []chapter
	title    string
	app      fyne.App
}

func newThumbnailCell(img image.Image, modality, label, title string, chapters []chapter, app fyne.App) *thumbnailCell {
	c := &thumbnailCell{
		lbl:      widget.NewLabelWithStyle(label, fyne.TextAlignCenter, fyne.TextStyle{}),
		chapters: chapters,
		title:    title,
		app:      app,
	}
	if img != nil {
		imgObj := canvas.NewImageFromImage(img)
		imgObj.FillMode = canvas.ImageFillContain
		imgObj.SetMinSize(fyne.NewSize(thumbSide, thumbSide))
		c.preview = imgObj
	} else {
		c.preview = modalityPlaceholder(modality)
	}
	c.lbl.Truncation = fyne.TextTruncateEllipsis
	c.ExtendBaseWidget(c)
	return c
}

// DoubleTapped opens the full series viewer for this thumbnail's files.
func (c *thumbnailCell) DoubleTapped(_ *fyne.PointEvent) {
	chapters, title, app := c.chapters, c.title, c.app
	go openViewerWindow(app, title, chapters, nil)
}

func (c *thumbnailCell) CreateRenderer() fyne.WidgetRenderer {
	c.ExtendBaseWidget(c)
	return widget.NewSimpleRenderer(
		container.NewBorder(nil, c.lbl, nil, nil, c.preview),
	)
}

// busyDialog is a modal "working…" indicator: a status line above an infinite
// progress bar, shown over a parent window while a preview is generated.
type busyDialog struct {
	status *widget.Label
	dlg    *dialog.CustomDialog
}

// showBusyDialog creates and shows a busyDialog. Must be called from a non-UI
// goroutine: creation is queued to the UI thread, and because the queue is
// FIFO it is guaranteed to run before a later hide() from the same goroutine.
func showBusyDialog(parent fyne.Window, title, initialStatus string) *busyDialog {
	b := &busyDialog{status: widget.NewLabel(initialStatus)}
	b.status.Alignment = fyne.TextAlignCenter
	fyne.Do(func() {
		b.dlg = dialog.NewCustomWithoutButtons(title,
			container.NewVBox(b.status, widget.NewProgressBarInfinite()), parent)
		b.dlg.Show()
	})
	return b
}

// setStatus updates the status line. Safe to call from any goroutine.
func (b *busyDialog) setStatus(msg string) {
	fyne.Do(func() { b.status.SetText(msg) })
}

// hide dismisses the dialog. Safe to call from any non-UI goroutine.
func (b *busyDialog) hide() {
	fyne.Do(func() { b.dlg.Hide() })
}

// overviewThumbWorkers is how many series thumbnails the study overview decodes
// at once. Four, matching the modification engine's tag-only cap: each is a full
// parse and decode of one file, so the limit that matters is cores and memory —
// a multi-frame SPECT or echo file parses every frame to show one — not disk
// queue depth, which is what the eight header-scan workers are sized for.
func overviewThumbWorkers(n int) int {
	return max(1, min(4, runtime.NumCPU(), n))
}

// showStudyOverviewWindow opens a grid window showing the middle slice of each
// series for a study. Each series' paths are sorted by InstanceNumber here —
// sorting reads every file's header in the study, so the caller must NOT
// pre-sort on the UI goroutine — and thumbnails are loaded on bounded pools
// while a modal busy dialog over parent reports progress. Double-clicking any thumbnail opens the
// full series viewer for that series.
// Must be called from a non-UI goroutine.
func showStudyOverviewWindow(a fyne.App, parent fyne.Window, title string, series []seriesThumb) {
	if len(series) == 0 {
		fyne.Do(func() {
			win := a.NewWindow(title)
			win.SetContent(container.NewCenter(widget.NewLabel("No series found.")))
			win.Resize(fyne.NewSize(420, 160))
			win.Show()
		})
		return
	}

	// Busy dialog: large studies take seconds to sort and thumbnail, and
	// without feedback the app looks hung.
	busy := showBusyDialog(parent, "Generating study preview", "Reading image headers…")

	// Two phases, each on a bounded pool — they used to run as one goroutine
	// per series, each with its own header-scan pool and its own thumbnail
	// decode, so a study's whole series count set the number of disk reads and
	// full decodes in flight at once.
	//
	// First every series' headers, on the one pool scanChapterGroups shares
	// across all of them (see there for why a pool per series was wrong).
	groups := make([][]string, len(series))
	for i, s := range series {
		groups[i] = s.paths
	}
	sorted := scanChapterGroups(groups, func(done, total int) {
		busy.setStatus(fmt.Sprintf("Reading image headers (%d/%d)…", done, total))
	})

	// Then the middle slice of each series. "Middle" is the middle of the
	// frame list, not of the file list, so a single-file multi-frame
	// acquisition (NM/SPECT) shows its central slice rather than its first
	// frame. Each is a full parse and decode of one file — a multi-frame
	// SPECT file holds its whole acquisition — so these are bounded by CPU and
	// memory rather than seek latency, and get their own, smaller pool.
	thumbs := make([]viewerState, len(series))
	var loaded, next atomic.Int64
	stopReporting := startPacedProgress(scanProgressInterval, func() {
		busy.setStatus(fmt.Sprintf("Loading series previews (%d/%d)…", loaded.Load(), len(series)))
	})
	var wg sync.WaitGroup
	for range overviewThumbWorkers(len(series)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(series) {
					return
				}
				if slices := expandFrames(sorted[i]); len(slices) > 0 {
					mid := slices[len(slices)/2]
					if vs, err := loadDicomThumbnail(mid.path, mid.frame, 2*thumbSide); err == nil {
						thumbs[i] = vs
					}
				}
				loaded.Add(1)
			}
		}()
	}
	wg.Wait()
	stopReporting()
	busy.hide()

	fyne.Do(func() {
		win := a.NewWindow(title)

		cells := make([]fyne.CanvasObject, len(series))
		for i, s := range series {
			// A nil image (no pixel data, or nothing the built-in decoders can
			// render) selects the modality placeholder tile.
			cells[i] = newThumbnailCell(thumbs[i].img, s.modality, s.label, "DICOM Preview — "+s.label, sorted[i], a)
		}

		// GridWrap reflows cells top-left to bottom-right as the window is
		// resized. It lays every cell out at one fixed size, so use the
		// largest minimum among the cells to fit them all.
		cellSize := fyne.NewSize(200, 230)
		for _, c := range cells {
			cellSize = cellSize.Max(c.MinSize())
		}
		grid := container.NewGridWrap(cellSize, cells...)

		hint := widget.NewLabelWithStyle(
			"Double-click a thumbnail to open the full series viewer.",
			fyne.TextAlignCenter, fyne.TextStyle{Italic: true},
		)

		cols := 3
		if len(cells) < cols {
			cols = len(cells)
		}
		win.SetContent(container.NewBorder(hint, nil, nil, nil, container.NewVScroll(grid)))
		win.Resize(fyne.NewSize(float32(cols)*(cellSize.Width+4)+40, 560))
		win.Show()
	})
}

// showDicomViewer opens the DICOM preview window for all images in folder.
// Collection magic-byte-checks every file in the tree, which can take a while
// for a large download folder, so a modal busy dialog over parent covers it.
// Must be called from a non-UI goroutine.
func showDicomViewer(a fyne.App, parent fyne.Window, folder string) {
	busy := showBusyDialog(parent, "Generating preview", "Scanning folder for DICOM files…")
	chapters, collectErr := collectDicomFiles(folder)
	busy.hide()
	openViewerWindow(a, "DICOM Preview — "+filepath.Base(folder), chapters, collectErr)
}

// showDicomViewerPaths opens the DICOM preview window for a specific set of files.
// Paths are sorted by InstanceNumber first — a header parse of every file, which
// takes seconds for large series — behind a modal busy dialog over parent.
// Must be called from a non-UI goroutine.
func showDicomViewerPaths(a fyne.App, parent fyne.Window, title string, rawPaths []string) {
	if len(rawPaths) == 0 {
		fyne.Do(func() {
			win := a.NewWindow(title)
			win.SetContent(container.NewCenter(widget.NewLabel("No images in this selection.")))
			win.Resize(fyne.NewSize(420, 160))
			win.Show()
		})
		return
	}
	total := len(rawPaths)
	busy := showBusyDialog(parent, "Generating series preview",
		fmt.Sprintf("Sorting images (0/%d)…", total))
	// scanChapters paces its own progress (see startPacedProgress), so every
	// report can go straight to the dialog.
	chapters := scanChapters(rawPaths, func(done int) {
		busy.setStatus(fmt.Sprintf("Sorting images (%d/%d)…", done, total))
	})
	busy.hide()
	openViewerWindow(a, title, chapters, nil)
}

// presetNames returns the ordered preset names of a list for the dropdown.
func presetNames(list []wlPreset) []string {
	out := make([]string, len(list))
	for i, p := range list {
		out[i] = p.name
	}
	return out
}

// resolvePreset finds a named preset in list and resolves it for the frame,
// falling back to the frame's own window when the name is not found.
func resolvePreset(list []wlPreset, name string, df *decodedFrame) (wc, ww float64) {
	for _, p := range list {
		if p.name == name {
			return p.resolve(df)
		}
	}
	return df.wc, df.ww
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// imageViewport is the interactive image surface of the DICOM viewer. It owns a
// canvas.Image and the annotation overlay, and translates mouse and scroll input
// into window/level, zoom, pan, and slice navigation:
//
//	Left-drag    window/level (horizontal = width, vertical = centre)
//	Right-drag   zoom (drag up to magnify)
//	Middle-drag  pan (when zoomed in)
//	Wheel        previous / next slice
//	Double-click reset zoom and pan to fit
//
// Window/level changes re-render from the cached decodedFrame, so they are cheap
// and never touch disk. Zoom/pan are implemented by cropping the rendered image
// (a packed copy of the crop — see cropPacked) so the displayed image always
// fills the viewport without overflow.
type imageViewport struct {
	widget.BaseWidget

	img     *canvas.Image
	overlay *fyne.Container

	frame *decodedFrame
	base  image.Image // frame rendered at current wc/ww/map (full frame, pre-crop)
	buf   *image.RGBA // reused windowing buffer for grayscale frames (flicker-free drag)
	// zoomBuf holds the displayed crop while zoomed in — a packed copy, never a
	// SubImage of base (see cropPacked). Reused across refreshes of one size.
	zoomBuf *image.RGBA
	curMap  *colorMap // active colour map applied to grayscale frames
	wc      float64
	ww      float64

	zoom         float64 // 1 = fit; >1 = magnified
	panCX, panCY float64 // crop centre in source-pixel coordinates
	wlSens       float64 // window/level units per dragged pixel

	ann   imageAnnotations
	idx   int
	total int

	showOverlays bool // composite overlay planes onto the rendered image

	btn       desktop.MouseButton
	wlDragged bool // a window/level drag is in progress (defer overlay rebuild)

	// Window/level drag anchor, captured on the FIRST Dragged event of a drag
	// (not on MouseDown — the press event's AbsolutePosition can be in a
	// different coordinate space than the drag events', which would offset the
	// whole drag). The window is then computed from the absolute displacement
	// since that anchor, immune to per-event delta accumulation.
	dragArmed    bool // set on MouseDown; cleared once the anchor is captured
	dragStartPos fyne.Position
	dragStartWC  float64
	dragStartWW  float64

	onScroll     func(delta int)      // wheel → slice navigation
	onWLChanged  func(wc, ww float64) // any window change → update info label
	onUserWindow func()               // user dragged W/L → clear preset, mark adjusted
}

func newImageViewport() *imageViewport {
	v := &imageViewport{
		img:          canvas.NewImageFromImage(image.NewGray(image.Rect(0, 0, 1, 1))),
		zoom:         1,
		wlSens:       1,
		curMap:       &grayscaleMap,
		showOverlays: true,
	}
	v.img.FillMode = canvas.ImageFillContain
	// Scale on the GPU (linear). The default ImageScaleSmooth re-runs a CPU
	// CatmullRom resample on every Refresh; during a window/level drag that cost
	// starves the paint loop and makes the image appear to flicker between the
	// old and new windows. ImageScaleFastest uploads the source as-is and lets
	// the GPU scale, so each drag tick is cheap and the image updates smoothly.
	v.img.ScaleMode = canvas.ImageScaleFastest
	v.img.SetMinSize(fyne.NewSize(512, 512))
	v.overlay = container.New(imageAnnLayout{img: v.img})
	v.ExtendBaseWidget(v)
	return v
}

// setContent installs a freshly decoded frame at the given window. When keepView
// is true the current zoom/pan are preserved (scrolling through a series);
// otherwise the view is reset to fit (opening a new series).
func (v *imageViewport) setContent(df *decodedFrame, wc, ww float64, ann imageAnnotations, idx, total int, keepView bool) {
	v.frame = df
	v.ann = ann
	v.idx, v.total = idx, total
	v.wc, v.ww = wc, ww

	if df.windowable() && df.hi > df.lo {
		// Dragging the full viewport width (~512 px) changes the window by
		// wlDragRangeFraction of the data range. A small fraction keeps fine
		// control; see wlDragRangeFraction.
		v.wlSens = (df.hi - df.lo) / 512 * wlDragRangeFraction
	} else {
		v.wlSens = 1
	}
	if v.wlSens < 1e-4 {
		v.wlSens = 1e-4
	}

	if !keepView {
		v.zoom = 1
		v.panCX = float64(df.cols) / 2
		v.panCY = float64(df.rows) / 2
	} else {
		v.panCX = clampFloat(v.panCX, 0, float64(df.cols))
		v.panCY = clampFloat(v.panCY, 0, float64(df.rows))
	}

	// A new frame may differ in size, so drop any stale windowing buffer.
	v.buf = nil
	v.renderBase(wc, ww)
	v.applyDisplay()
	v.refreshOverlay()
}

// setPlaybackFrame swaps in another frame of the same clip — same dimensions,
// same window, same annotations — so it skips the annotation rebuild, the
// buffer reset and the zoom/pan recalculation that setContent performs. This is
// the per-frame cine path, which runs at up to 60 fps: at that rate rebuilding
// the eight annotation objects costs more than the image swap itself and makes
// the overlay flicker. The counter and on-image annotation are brought back
// into agreement when playback stops.
func (v *imageViewport) setPlaybackFrame(df *decodedFrame, idx int) {
	if df == nil {
		return
	}
	v.frame = df
	v.idx = idx
	v.renderBase(v.wc, v.ww)
	v.applyDisplay()
}

// renderBase produces v.base for the current frame at (wc, ww). For grayscale
// frames it windows into the reused v.buf buffer (allocated on demand) so that
// rapid window changes neither allocate nor swap the backing image pointer.
func (v *imageViewport) renderBase(wc, ww float64) {
	df := v.frame
	if df == nil {
		return
	}
	if !df.windowable() {
		// Colour frame: compositing overlays creates a new RGBA every call,
		// so only do it when overlays are present and enabled.
		if v.showOverlays && len(df.overlays) > 0 {
			v.base = df.render(v.curMap, wc, ww)
		} else {
			v.base = df.colorImg
		}
		return
	}
	if v.buf == nil || v.buf.Rect.Dx() != df.cols || v.buf.Rect.Dy() != df.rows {
		v.buf = image.NewRGBA(image.Rect(0, 0, df.cols, df.rows))
	}
	df.renderInto(v.buf, v.curMap, wc, ww)
	if v.showOverlays {
		df.paintOverlays(v.buf)
	}
	v.base = v.buf
}

// setColorMap changes the active colour map and re-renders the current frame
// (no file re-read). For already-colour frames the map has no effect.
func (v *imageViewport) setColorMap(cm *colorMap) {
	v.curMap = cm
	if v.frame == nil {
		return
	}
	v.renderBase(v.wc, v.ww)
	v.applyDisplay()
}

// reWindow re-renders at a new window and rebuilds the annotation overlay. Used
// for discrete changes (presets, reset); interactive drags use applyWindow with
// updateOverlay=false to avoid per-tick overlay churn.
func (v *imageViewport) reWindow(wc, ww float64) {
	v.applyWindow(wc, ww, true)
}

// applyWindow re-renders the current frame at a new window without re-reading
// it. When updateOverlay is false the on-image annotation overlay is left
// untouched (its window text is refreshed once at drag end) — rebuilding the
// overlay every drag tick causes the image to flicker.
func (v *imageViewport) applyWindow(wc, ww float64, updateOverlay bool) {
	if v.frame == nil || !v.frame.windowable() {
		return
	}
	if ww < 1 {
		ww = 1
	}
	v.wc, v.ww = wc, ww
	v.renderBase(wc, ww)
	v.applyDisplay()
	v.ann.windowStr = fmt.Sprintf("W: %.0f  L: %.0f", ww, wc)
	if updateOverlay {
		v.refreshOverlay()
	}
	if v.onWLChanged != nil {
		v.onWLChanged(wc, ww)
	}
}

// applyDisplay sets the canvas image to the current base, cropped per zoom/pan.
func (v *imageViewport) applyDisplay() {
	if v.base == nil {
		return
	}
	b := v.base.Bounds()
	if v.zoom <= 1.000001 {
		v.img.Image = v.base
		v.img.Refresh()
		return
	}
	cw := int(float64(b.Dx())/v.zoom + 0.5)
	ch := int(float64(b.Dy())/v.zoom + 0.5)
	if cw < 1 {
		cw = 1
	}
	if ch < 1 {
		ch = 1
	}
	cx := clampInt(int(v.panCX+0.5)-cw/2, b.Min.X, b.Max.X-cw)
	cy := clampInt(int(v.panCY+0.5)-ch/2, b.Min.Y, b.Max.Y-ch)
	crop := image.Rect(cx, cy, cx+cw, cy+ch)
	v.zoomBuf = cropPacked(v.zoomBuf, v.base, crop)
	v.img.Image = v.zoomBuf
	v.img.Refresh()
}

// cropPacked copies the crop rectangle of src into dst — reused when it is
// already the crop's size, reallocated otherwise — as a tightly packed RGBA
// whose bounds start at (0,0), and returns it.
//
// The crop must be a copy, never src.SubImage(crop). A SubImage shares its
// parent's pixel buffer: its Pix starts at the crop's first pixel but its rows
// are still Stride (the full frame's width) apart. Fyne 2.7.3's texture upload
// (painter/gl imgToTexture) passes an *image.RGBA's Pix straight to
// glTexImage2D at Rect.Size() with no row-length setting, so it read each row
// as if it followed the last one directly and the zoomed image sheared; any
// other image type goes through draw.Draw from image.Point{}, which ignores a
// SubImage's non-zero origin. Both are satisfied only by an image that starts
// at (0,0) with Stride == 4 × width.
//
// Copying costs one crop's worth of memcpy, at most the displayed pixel count,
// and saves Fyne converting non-RGBA frames on every refresh. dst is reused for
// the same reason v.buf is: pan and window/level drags refresh at pointer rate,
// and a fresh allocation per tick is garbage the display gains nothing from.
func cropPacked(dst *image.RGBA, src image.Image, crop image.Rectangle) *image.RGBA {
	crop = crop.Intersect(src.Bounds())
	w, h := crop.Dx(), crop.Dy()
	if dst == nil || dst.Rect.Dx() != w || dst.Rect.Dy() != h {
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	if s, ok := src.(*image.RGBA); ok {
		rowBytes := w * 4
		for y := 0; y < h; y++ {
			off := s.PixOffset(crop.Min.X, crop.Min.Y+y)
			copy(dst.Pix[y*dst.Stride:y*dst.Stride+rowBytes], s.Pix[off:off+rowBytes])
		}
		return dst
	}
	draw.Draw(dst, dst.Rect, src, crop.Min, draw.Src)
	return dst
}

func (v *imageViewport) refreshOverlay() {
	v.overlay.Objects = buildAnnObjects(v.ann, v.idx, v.total)
	v.overlay.Refresh()
}

func (v *imageViewport) setShowAnn(show bool) {
	if show {
		v.overlay.Show()
	} else {
		v.overlay.Hide()
	}
}

func (v *imageViewport) setShowOverlays(show bool) {
	if v.showOverlays == show {
		return
	}
	v.showOverlays = show
	if v.frame == nil {
		return
	}
	v.renderBase(v.wc, v.ww)
	v.applyDisplay()
}

func (v *imageViewport) resetView() {
	v.zoom = 1
	if v.frame != nil {
		v.panCX = float64(v.frame.cols) / 2
		v.panCY = float64(v.frame.rows) / 2
	}
	v.applyDisplay()
}

// --- input handling ---------------------------------------------------------

func (v *imageViewport) MouseDown(e *desktop.MouseEvent) {
	v.btn = e.Button
	// Defer capturing the window/level anchor to the first Dragged event so the
	// anchor shares the drag events' coordinate space (see dragArmed).
	v.dragArmed = true
}
func (v *imageViewport) MouseUp(_ *desktop.MouseEvent) { v.btn = 0 }

func (v *imageViewport) Dragged(e *fyne.DragEvent) {
	switch v.btn {
	case desktop.MouseButtonSecondary: // zoom
		v.zoom = clampFloat(v.zoom*math.Exp(-float64(e.Dragged.DY)*0.01), 1, 16)
		v.applyDisplay()
	case desktop.MouseButtonTertiary: // pan
		if v.frame != nil {
			w := float64(v.Size().Width)
			h := float64(v.Size().Height)
			if w > 0 && h > 0 {
				v.panCX -= float64(e.Dragged.DX) * (float64(v.frame.cols) / v.zoom) / w
				v.panCY -= float64(e.Dragged.DY) * (float64(v.frame.rows) / v.zoom) / h
			}
			v.applyDisplay()
		}
	default: // window/level (primary button)
		if v.frame == nil || !v.frame.windowable() {
			return
		}
		// Capture the anchor on the first drag event so it shares this event's
		// coordinate space; the first event then has zero displacement (no jump).
		if v.dragArmed {
			v.dragStartPos = e.AbsolutePosition
			v.dragStartWC = v.wc
			v.dragStartWW = v.ww
			v.dragArmed = false
		}
		// Compute the window from the total displacement since the anchor, not by
		// accumulating this event's delta. Horizontal = width, vertical = centre.
		// applyWindow takes (centre, width) — keep that order to avoid swapping
		// Window and Level. Update only the image during the drag; the overlay is
		// rebuilt once in DragEnd to avoid per-tick flicker.
		dx := float64(e.AbsolutePosition.X - v.dragStartPos.X)
		dy := float64(e.AbsolutePosition.Y - v.dragStartPos.Y)
		v.applyWindow(v.dragStartWC+dy*v.wlSens, v.dragStartWW+dx*v.wlSens, false)
		v.wlDragged = true
		if v.onUserWindow != nil {
			v.onUserWindow()
		}
	}
}

func (v *imageViewport) DragEnd() {
	v.btn = 0
	if v.wlDragged {
		v.wlDragged = false
		v.refreshOverlay() // sync the on-image W/L annotation to the final window
	}
}

func (v *imageViewport) Scrolled(e *fyne.ScrollEvent) {
	if v.onScroll == nil {
		return
	}
	if e.Scrolled.DY < 0 {
		v.onScroll(1)
	} else if e.Scrolled.DY > 0 {
		v.onScroll(-1)
	}
}

func (v *imageViewport) DoubleTapped(_ *fyne.PointEvent) { v.resetView() }

func (v *imageViewport) CreateRenderer() fyne.WidgetRenderer {
	v.ExtendBaseWidget(v)
	return &viewportRenderer{v: v, objects: []fyne.CanvasObject{v.img, v.overlay}}
}

type viewportRenderer struct {
	v       *imageViewport
	objects []fyne.CanvasObject
}

func (r *viewportRenderer) Layout(size fyne.Size) {
	for _, o := range r.objects {
		o.Resize(size)
		o.Move(fyne.NewPos(0, 0))
	}
}
func (r *viewportRenderer) MinSize() fyne.Size           { return fyne.NewSize(512, 512) }
func (r *viewportRenderer) Refresh()                     { canvas.Refresh(r.v) }
func (r *viewportRenderer) Objects() []fyne.CanvasObject { return r.objects }
func (r *viewportRenderer) Destroy()                     {}

// stableMinLayout sizes its single child to the full container and reports a
// high-water-mark MinSize — the largest the child's min has ever been. It
// exists to keep rapidly changing status text from perturbing the window's
// minimum size: any canvas min-size change makes Fyne re-apply the window's
// size limits (EnsureMinSize → fitContent → SetSizeLimits), and GLFW's Windows
// implementation of SetSizeLimits is MoveWindow(..., repaint=TRUE) — a full
// repaint of the window including its OS frame. At cine-playback rate (the
// frame counter re-labels ~10×/s, and a proportional font gives "Frame 31 /
// 72" and "Frame 32 / 72" different widths) that is a storm of frame repaints,
// visible as flickering lines around and behind the window while a clip plays.
// The min may still grow the first few times a longer text appears; it never
// shrinks, which for a viewer window is harmless — and after one loop of a
// clip every width has been seen and it stops changing entirely.
type stableMinLayout struct {
	max fyne.Size
}

func (l *stableMinLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	for _, o := range objs {
		l.max = l.max.Max(o.MinSize())
	}
	return l.max
}

func (l *stableMinLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	for _, o := range objs {
		o.Resize(size)
		o.Move(fyne.Position{})
	}
}

// stableMin wraps a widget whose text changes at display rate (frame counter,
// W/L info, chapter label) so those changes cannot alter the window's minimum
// size — see stableMinLayout.
func stableMin(obj fyne.CanvasObject) fyne.CanvasObject {
	return container.New(&stableMinLayout{}, obj)
}

// openViewerWindow creates and shows the interactive DICOM image viewer window.
// Must be called from a non-UI goroutine; all widget creation is via fyne.Do.
//
// The window navigates in one of two modes, decided by the series itself:
//
//   - Slice mode, for a series of single-frame instances (CT, MR): one slider
//     position per image, as it has always been.
//   - Chapter mode, when any instance holds more than one frame: one chapter per
//     instance, the slider scoped to the chapter on screen, and a cine transport
//     with a filmstrip beneath. Flattening a 173-instance echo study onto one
//     5256-position slider — no seam between loops, no way to play any of them —
//     is what this exists to avoid.
func openViewerWindow(a fyne.App, title string, chapters []chapter, collectErr error) {
	// Route non-image modalities (SR, KO, AU, PR) to the document viewer.
	if collectErr == nil && len(chapters) > 0 {
		paths := chapterPaths(chapters)
		if mod := seriesModality(paths); isDocumentModality(mod) {
			openSRWindow(a, title, paths)
			return
		}
	}

	postUI(func() {
		win := a.NewWindow(title)

		if collectErr != nil || len(chapters) == 0 {
			msg := "No DICOM files found."
			if collectErr != nil {
				msg = collectErr.Error()
			}
			win.SetContent(container.NewCenter(widget.NewLabel(msg)))
			win.Resize(fyne.NewSize(420, 160))
			win.Show()
			return
		}

		// A series holding a multi-frame instance plays as chapters; anything else
		// keeps the plain one-position-per-image slider.
		chapterMode := anyMultiFrame(chapters)
		slices := expandFrames(chapters)
		if chapterMode {
			logInfo("viewer: chapter mode — %d chapters, %d frames (%s)",
				len(chapters), totalChapterFrames(chapters), title)
		}

		// A single-frame series covering the same slice stack several times
		// (in/out phase, diffusion b-values, dynamic timepoints) navigates one
		// phase at a time, with a dropdown and the P key flipping between phases
		// at the same slice — see mrphases.go. phases stays nil for ordinary
		// series, which keep the flat list.
		var phases []mrPhase
		curPhase := 0
		if !chapterMode {
			if phases = detectMRPhases(chapters); phases != nil {
				slices = phases[0].slices
				logInfo("viewer: phase mode — %d phases × %d slices (%s)",
					len(phases), len(phases[0].slices), title)
			}
		}

		// Navigation state. In slice mode current indexes slices (of the active
		// phase, when phased); in chapter mode it is the frame within the active
		// chapter and curChapter is the file.
		curChapter := len(chapters) / 2
		current := 0
		total := len(slices)
		if chapterMode {
			total = chapters[curChapter].frames
		} else {
			current = total / 2 // open at the middle slice
		}

		// Chapter mode's on-demand path, for frames the clip buffer has not
		// reached: one parsed file is kept, so scrubbing a multi-frame
		// acquisition decodes a single frame per step instead of re-parsing the
		// whole file. Slice mode has its own loader (see loadAndShow), since each
		// of its files holds one frame and a one-file cache never hits there.
		cache := &dicomFileCache{}

		viewport := newImageViewport()
		showAnn := a.Preferences().BoolWithFallback("showAnnotations", true)
		viewport.setShowAnn(showAnn)
		showOverlays := a.Preferences().BoolWithFallback("showOverlays", true)
		viewport.setShowOverlays(showOverlays)

		infoLabel := widget.NewLabel("")
		infoLabel.Alignment = fyne.TextAlignCenter

		counterLbl := widget.NewLabel(fmt.Sprintf("— / %d", total))
		counterLbl.Alignment = fyne.TextAlignCenter

		slider := widget.NewSlider(0, float64(maxInt(1, total)-1))
		slider.Step = 1
		sliderMuting := false // guards programmatic slider moves during playback

		// Window state shared across slices. userAdjusted means the user dragged
		// W/L (or it is otherwise custom); presetName tracks the active preset.
		// presetList is chosen from the frame's modality on first load.
		var curWC, curWW float64
		userAdjusted := false
		presetName := "Default"
		presetList := genericPresets
		currentModality := ""
		var presetMuting bool // guards programmatic preset changes

		presetSelect := widget.NewSelect(presetNames(presetList), nil)
		presetSelect.Selected = "Default"

		// Colour map state. The default map is chosen from the frame's modality
		// on first load (Hot Iron for PET/NM, Grayscale otherwise) and persists
		// across slices until the user picks another from the dropdown.
		curMapName := "Grayscale"
		var colorMuting bool
		colorSelect := widget.NewSelect(colorMapNames(), nil)
		colorSelect.Selected = curMapName
		colorSelect.OnChanged = func(name string) {
			if colorMuting {
				return
			}
			curMapName = name
			viewport.setColorMap(colorMapByName(name))
		}

		setInfo := func(df *decodedFrame, wc, ww float64) {
			if df.windowable() {
				infoLabel.SetText(fmt.Sprintf("%d × %d   W:%.0f  L:%.0f", df.cols, df.rows, ww, wc))
			} else {
				infoLabel.SetText(fmt.Sprintf("%d × %d", df.cols, df.rows))
			}
		}

		// targetWindow decides the window for a newly loaded frame so that a drag
		// or preset persists as the user scrolls through the series.
		targetWindow := func(df *decodedFrame) (float64, float64) {
			if !df.windowable() {
				return 0, 0
			}
			switch {
			case userAdjusted:
				return curWC, curWW
			case presetName != "" && presetName != "Default":
				return resolvePreset(presetList, presetName, df)
			default:
				return df.wc, df.ww
			}
		}

		// Forward-declared so the display path can reference them before their
		// full initialisation.
		var overlayCheck *widget.Check
		var cineRow *fyne.Container
		var strip *chapterStrip
		var playBtn *widget.Button
		var sweepCheck *widget.Check
		var fpsSelect *widget.Select
		var prevBtn, nextBtn *widget.Button
		var chapterLbl *widget.Label
		var updateCounter func()
		var selectChapter func(index int)

		// ── Cine state (chapter mode only) ────────────────────────────────────
		//
		// Each chapter remembers where it was left, at what rate, and whether it
		// was playing, so leaving a loop and coming back resumes it as it was.
		type chapterViewState struct {
			frame   int
			fps     float64
			bounce  bool
			playing bool
		}
		chapterStates := make([]chapterViewState, len(chapters))
		for i, c := range chapters {
			// Opening on the middle frame matches the filmstrip thumbnail and the
			// "open at the middle slice" rule the viewer already follows.
			chapterStates[i] = chapterViewState{frame: c.frames / 2, fps: c.fps, bounce: c.bounce}
		}
		player := newCinePlayer()
		// The buffer is read by the player's readiness gate on its own goroutine,
		// so it is held in an atomic rather than a plain variable.
		var clipRef atomic.Pointer[clipBuffer]
		lastCounterSync := time.Now()

		// applyState puts a fully decoded frame on screen: the full path, taken on
		// every load that may change modality, dimensions or window.
		applyState := func(st viewerState, idx, span int, keepView bool) {
			// Pick the modality-appropriate preset set and colour map on the
			// first frame (and on the rare chance the modality changes mid-series).
			if st.frame.modality != currentModality {
				currentModality = st.frame.modality
				presetList = presetsForModality(currentModality)
				presetMuting = true
				presetSelect.Options = presetNames(presetList)
				presetSelect.SetSelected("Default")
				presetSelect.Refresh()
				presetMuting = false
				presetName = "Default"
				userAdjusted = false

				curMapName = defaultColorMapForModality(currentModality)
				colorMuting = true
				colorSelect.SetSelected(curMapName)
				colorMuting = false
				viewport.curMap = colorMapByName(curMapName) // used by setContent below
			}
			wc, ww := targetWindow(st.frame)
			curWC, curWW = wc, ww
			viewport.setContent(st.frame, wc, ww, st.ann, idx, span, keepView)
			setInfo(st.frame, wc, ww)
			if len(st.frame.overlays) > 0 {
				overlayCheck.Show()
			}
			if st.frame.windowable() {
				presetSelect.Enable()
				colorSelect.Enable()
			} else {
				presetSelect.Disable()
				colorSelect.Disable()
			}
		}

		// ── Slice mode display ────────────────────────────────────────────────

		// Slices come from a loader (sliceloader.go): a cached slice shows at
		// once, anything else is decoded ahead of the read-ahead and delivered
		// only if it is still the one on the slider — so a fast scroll neither
		// queues a decode per step nor comes to rest on a stale slice.
		// scrollDir is the direction of the last move, which aims the read-ahead;
		// pendingKeepView belongs to the request awaiting delivery.
		scrollDir := 1
		pendingKeepView := false
		var loader *sliceLoader
		if !chapterMode {
			loader = newSliceLoader(sliceCacheBudget, decodeViewerSlice,
				func(key viewerSlice, st viewerState, err error) {
					postUI(func() {
						if current >= len(slices) || slices[current] != key {
							return // the user has moved on; it waits in the cache
						}
						counterLbl.SetText(fmt.Sprintf("%d / %d", current+1, total))
						if err != nil {
							infoLabel.SetText("Error: " + err.Error())
							return
						}
						applyState(st, current, total, pendingKeepView)
					})
				})
		}
		loadAndShow := func(idx int, keepView bool) {
			key := slices[idx]
			var others [][]viewerSlice
			for p := range phases {
				if p != curPhase {
					others = append(others, phases[p].slices)
				}
			}
			if st, ok := loader.get(key); ok {
				applyState(st, idx, total, keepView)
				counterLbl.SetText(fmt.Sprintf("%d / %d", idx+1, total))
			} else {
				pendingKeepView = keepView
				counterLbl.SetText(fmt.Sprintf("%d / %d  (loading…)", idx+1, total))
			}
			loader.request(key, sliceReadAhead(slices, idx, scrollDir, others))
		}

		// ── Phase switching (phased slice mode only) ──────────────────────────
		//
		// Switching phases keeps the slice index — detection guarantees index i
		// is the same anatomical position in every phase — and keeps the view
		// (zoom/pan) and window, so flipping phases is a same-slice comparison,
		// which is what an in/out-phase or b-value pair exists for.
		var phaseSelect *widget.Select
		var phaseMuting bool
		switchPhase := func(p int) {
			if phases == nil || p < 0 || p >= len(phases) || p == curPhase {
				return
			}
			curPhase = p
			slices = phases[p].slices
			phaseMuting = true
			phaseSelect.SetSelected(phases[p].label)
			phaseMuting = false
			loadAndShow(current, true)
		}
		if phases != nil {
			labels := make([]string, len(phases))
			for i, p := range phases {
				labels[i] = p.label
			}
			phaseSelect = widget.NewSelect(labels, func(name string) {
				if phaseMuting {
					return
				}
				for i, p := range phases {
					if p.label == name {
						switchPhase(i)
						return
					}
				}
			})
			phaseSelect.Selected = phases[0].label
		}

		// ── Chapter mode display ──────────────────────────────────────────────

		// showChapterFrame shows a frame of the active chapter. A buffered frame
		// is applied synchronously — that is the playback path, and it must not
		// queue a goroutine per frame; anything else (a frame the buffer has not
		// reached, or the first frame of a newly selected chapter) takes the
		// ordinary on-demand route, so a chapter is scrubbable while it buffers.
		showChapterFrame := func(frame int, keepView bool) {
			c := chapters[curChapter]
			clip := clipRef.Load()
			if df := clip.frame(frame); df != nil {
				ann := imageAnnotations{}
				if a := clip.annotations(); a != nil {
					ann = *a
				}
				applyState(viewerState{frame: df, ann: ann}, frame, c.frames, keepView)
				updateCounter()
				return
			}
			counterLbl.SetText(fmt.Sprintf("Frame %d / %d  (loading…)", frame+1, c.frames))
			path := c.path
			go func() {
				st, err := cache.load(path, frame)
				postUI(func() {
					if path != chapters[curChapter].path {
						return // the user moved on while this was loading
					}
					if err != nil {
						infoLabel.SetText("Error: " + err.Error())
						updateCounter()
						return
					}
					applyState(st, frame, chapters[curChapter].frames, keepView)
					updateCounter()
				})
			}()
		}

		// updateCounter writes "Frame 31 / 72", plus the buffer's state while it
		// still matters: filling progress, or — for a clip too large to hold
		// whole — how much of it the player can actually loop.
		updateCounter = func() {
			if !chapterMode {
				counterLbl.SetText(fmt.Sprintf("%d / %d", current+1, total))
				return
			}
			c := chapters[curChapter]
			text := fmt.Sprintf("Frame %d / %d", current+1, c.frames)
			if clip := clipRef.Load(); clip != nil {
				switch {
				case !clip.isComplete():
					text += fmt.Sprintf("   (buffering %d / %d)", clip.decodedCount(), clip.capacity())
				case clip.truncated():
					text += fmt.Sprintf("   (too large to buffer whole — looping frames 1–%d)", clip.capacity())
				}
			}
			counterLbl.SetText(text)
		}

		setPlaying := func(playing bool) {
			if playing {
				playBtn.SetIcon(theme.MediaPauseIcon())
			} else {
				playBtn.SetIcon(theme.MediaPlayIcon())
			}
		}

		// ── Player wiring ─────────────────────────────────────────────────────

		player.setReadiness(func(frame int) bool {
			clip := clipRef.Load()
			return clip == nil || clip.isReady(frame)
		})
		player.setOnFrame(func(frame int) {
			// Posted from the player goroutine; a post that survives a chapter
			// change or a stop is stale and must be dropped rather than shown
			// against the wrong clip.
			if !chapterMode || !player.isRunning() || frame >= chapters[curChapter].frames {
				return
			}
			clip := clipRef.Load()
			df := clip.frame(frame)
			if df == nil {
				return
			}
			current = frame
			viewport.setPlaybackFrame(df, frame)
			// The slider and counter are text and layout work; at 30-60 fps they
			// cost more than the image swap and add nothing between refreshes, so
			// they follow at ~10 Hz. Both are synced exactly when playback stops.
			if time.Since(lastCounterSync) >= 100*time.Millisecond {
				lastCounterSync = time.Now()
				sliderMuting = true
				slider.SetValue(float64(frame))
				sliderMuting = false
				updateCounter()
			}
		})

		// syncAfterPlayback brings the slider, counter and on-image annotation
		// back into exact agreement with the frame on screen once the throttled
		// updates above stop arriving.
		syncAfterPlayback := func() {
			sliderMuting = true
			slider.SetValue(float64(current))
			sliderMuting = false
			viewport.idx = current
			viewport.refreshOverlay()
			updateCounter()
		}

		pausePlayback := func() {
			if !player.isRunning() {
				return
			}
			player.stopPlayback()
			chapterStates[curChapter].playing = false
			setPlaying(false)
			syncAfterPlayback()
		}

		// ── Chapter selection ─────────────────────────────────────────────────

		selectChapter = func(index int) {
			if index < 0 || index >= len(chapters) || index == curChapter {
				return
			}
			// Save the outgoing chapter so returning to it resumes where it was.
			// curChapter is -1 on the opening switch, when there is no outgoing
			// chapter and nothing to carry over.
			resumePlaying := false
			if curChapter >= 0 {
				resumePlaying = player.isRunning()
				chapterStates[curChapter] = chapterViewState{
					frame:   current,
					fps:     player.currentFPS(),
					bounce:  player.isBounce(),
					playing: resumePlaying,
				}
			}
			player.stopPlayback()
			if old := clipRef.Load(); old != nil {
				old.cancel()
			}

			curChapter = index
			c := chapters[index]
			remembered := chapterStates[index]
			current = clampInt(remembered.frame, 0, maxInt(0, c.frames-1))
			total = c.frames

			clipRef.Store(startClipBuffer(c, func(decoded int) {
				clip := clipRef.Load()
				if clip == nil || clip.chapter.path != chapters[curChapter].path {
					return // progress from a chapter already switched away from
				}
				if clip.truncated() {
					// Confine the loop to the frames that fit rather than letting
					// the player stall at the boundary. Frames past the buffer stay
					// reachable by scrubbing.
					player.setRange(0, clip.capacity()-1)
				}
				updateCounter()
			}))

			slider.Max = float64(maxInt(1, c.frames) - 1)
			sliderMuting = true
			slider.SetValue(float64(current))
			sliderMuting = false
			slider.Refresh()

			player.configure(c.loopFrom, c.loopTo, remembered.fps, remembered.bounce, current)

			// Transport reflects the incoming chapter, muted so restoring a
			// remembered rate does not read as a user change.
			playable := c.playable()
			if playable {
				playBtn.Enable()
				sweepCheck.Enable()
				fpsSelect.Enable()
			} else {
				playBtn.Disable()
				sweepCheck.Disable()
				fpsSelect.Disable()
			}
			sweepCheck.OnChanged = nil
			sweepCheck.SetChecked(remembered.bounce)
			sweepCheck.OnChanged = func(checked bool) {
				player.setBounce(checked)
				chapterStates[curChapter].bounce = checked
			}
			onFPSChanged := fpsSelect.OnChanged
			fpsSelect.OnChanged = nil
			fpsSelect.SetSelected(fpsOptionFor(remembered.fps, c.fps))
			fpsSelect.OnChanged = onFPSChanged

			prevBtn.Enable()
			nextBtn.Enable()
			if index == 0 {
				prevBtn.Disable()
			}
			if index == len(chapters)-1 {
				nextBtn.Disable()
			}
			chapterLbl.SetText(fmt.Sprintf("Chapter %d / %d  —  %s",
				index+1, len(chapters), c.label))
			if strip != nil {
				strip.selectIndex(index)
			}

			// The incoming instance may differ in modality, dimensions and window,
			// so the first frame of a new chapter takes the full path.
			showChapterFrame(current, false)

			resumePlaying = resumePlaying && playable
			if resumePlaying {
				player.start()
			}
			setPlaying(resumePlaying)
		}

		// ── Shared navigation ─────────────────────────────────────────────────

		// gotoPosition moves to a slider position in whichever mode is active.
		gotoPosition := func(pos int) {
			if chapterMode {
				if pos == current {
					return
				}
				current = pos
				// Scrubbing while a chapter plays does not fight the player: it is
				// told where the user went and carries on from there.
				player.setCurrentFrame(pos)
				showChapterFrame(pos, true)
				return
			}
			if pos == current {
				return
			}
			if pos < current {
				scrollDir = -1
			} else {
				scrollDir = 1
			}
			current = pos
			loadAndShow(pos, true)
		}

		slider.OnChanged = func(vf float64) {
			if sliderMuting {
				return
			}
			gotoPosition(int(vf))
		}

		viewport.onScroll = func(delta int) {
			nv := current + delta
			if nv < 0 || nv >= total {
				return
			}
			slider.SetValue(float64(nv))
		}
		viewport.onWLChanged = func(wc, ww float64) {
			curWC, curWW = wc, ww
			if viewport.frame != nil {
				setInfo(viewport.frame, wc, ww)
			}
		}
		viewport.onUserWindow = func() {
			userAdjusted = true
			if presetName != "" {
				presetName = ""
				presetMuting = true
				presetSelect.ClearSelected()
				presetMuting = false
			}
		}

		presetSelect.OnChanged = func(name string) {
			if presetMuting || viewport.frame == nil || !viewport.frame.windowable() {
				return
			}
			presetName = name
			userAdjusted = false // a preset drives subsequent slices until a drag
			wc, ww := resolvePreset(presetList, name, viewport.frame)
			viewport.reWindow(wc, ww)
		}

		annCheck := widget.NewCheck("Annotations", func(checked bool) {
			a.Preferences().SetBool("showAnnotations", checked)
			viewport.setShowAnn(checked)
		})
		annCheck.SetChecked(showAnn)

		overlayCheck = widget.NewCheck("Overlays", func(checked bool) {
			a.Preferences().SetBool("showOverlays", checked)
			viewport.setShowOverlays(checked)
		})
		overlayCheck.SetChecked(showOverlays)
		overlayCheck.Hide()

		resetBtn := widget.NewButton("Reset", func() {
			viewport.resetView()
			presetSelect.SetSelected("Default") // fires OnChanged → reset window
		})

		// ── Transport row and filmstrip ───────────────────────────────────────

		togglePlay := func() {
			if !chapterMode || !chapters[curChapter].playable() {
				return
			}
			if player.isRunning() {
				pausePlayback()
				return
			}
			player.setCurrentFrame(current)
			player.start()
			chapterStates[curChapter].playing = true
			setPlaying(true)
		}

		playBtn = widget.NewButtonWithIcon("", theme.MediaPlayIcon(), togglePlay)
		sweepCheck = widget.NewCheck("Sweep", nil)
		fpsSelect = widget.NewSelect(fpsOptions, nil)
		fpsSelect.OnChanged = func(opt string) {
			if !chapterMode {
				return
			}
			fps := fpsFromOption(opt, chapters[curChapter].fps)
			player.setFPS(fps)
			chapterStates[curChapter].fps = fps
		}
		prevBtn = widget.NewButtonWithIcon("", theme.NavigateBackIcon(), func() {
			selectChapter(curChapter - 1)
		})
		nextBtn = widget.NewButtonWithIcon("", theme.NavigateNextIcon(), func() {
			selectChapter(curChapter + 1)
		})
		chapterLbl = widget.NewLabel("")

		if chapterMode {
			transport := container.NewHBox(
				playBtn, sweepCheck,
				widget.NewLabel("Rate:"), fpsSelect,
				widget.NewLabel("  "), prevBtn, nextBtn, stableMin(chapterLbl),
			)
			// A one-chapter series (a single multi-frame NM/SPECT file, typically)
			// gains the transport but has nothing to pick from, so the filmstrip
			// would be a single cell of clutter.
			if len(chapters) > 1 {
				strip = newChapterStrip(chapters, func(index int) { selectChapter(index) })
				cineRow = container.NewVBox(transport, strip.object())
			} else {
				cineRow = container.NewVBox(transport)
			}
		}

		// Keyboard: arrows/page = frame navigation; +/- = zoom; R = reset window;
		// Home/F = reset zoom & pan; Space = play/pause; P = next phase.
		win.Canvas().SetOnTypedKey(func(e *fyne.KeyEvent) {
			switch e.Name {
			case fyne.KeyUp, fyne.KeyLeft, fyne.KeyPageUp:
				if current > 0 {
					slider.SetValue(float64(current - 1))
				}
			case fyne.KeyDown, fyne.KeyRight, "Next": // "Next" = Page Down
				if current < total-1 {
					slider.SetValue(float64(current + 1))
				}
			case fyne.KeySpace:
				togglePlay()
			case fyne.KeyP:
				// Cycle phases at the same slice — the flicker comparison an
				// in/out-phase pair is read with.
				if phases != nil {
					switchPhase((curPhase + 1) % len(phases))
				}
			case fyne.KeyPlus, fyne.KeyEqual:
				viewport.zoom = clampFloat(viewport.zoom*1.25, 1, 16)
				viewport.applyDisplay()
			case fyne.KeyMinus:
				viewport.zoom = clampFloat(viewport.zoom/1.25, 1, 16)
				viewport.applyDisplay()
			case fyne.KeyHome, fyne.KeyF:
				viewport.resetView()
			case fyne.KeyR:
				presetSelect.SetSelected("Default")
			}
		})
		if chapterMode {
			win.Canvas().AddShortcut(
				&desktop.CustomShortcut{KeyName: fyne.KeyLeft, Modifier: fyne.KeyModifierControl},
				func(fyne.Shortcut) { selectChapter(curChapter - 1) })
			win.Canvas().AddShortcut(
				&desktop.CustomShortcut{KeyName: fyne.KeyRight, Modifier: fyne.KeyModifierControl},
				func(fyne.Shortcut) { selectChapter(curChapter + 1) })
		}

		controlItems := []fyne.CanvasObject{}
		if phaseSelect != nil {
			controlItems = append(controlItems, widget.NewLabel("Phase:"), phaseSelect)
		}
		controlItems = append(controlItems,
			widget.NewLabel("Window:"), presetSelect,
			widget.NewLabel("Colour:"), colorSelect,
			annCheck, overlayCheck, resetBtn,
		)
		controls := container.NewHBox(controlItems...)
		bottomItems := []fyne.CanvasObject{
			stableMin(counterLbl), // counterLbl centres its own text
			slider,
			container.NewBorder(nil, nil, controls, nil, stableMin(infoLabel)),
		}
		if cineRow != nil {
			bottomItems = append(bottomItems, cineRow)
		}
		bottom := container.NewVBox(bottomItems...)

		// Playback and thumbnail decoding must not outlive the window: a closed
		// viewer that keeps a ticker and a decode pool running would hold the
		// process busy for the rest of the session.
		win.SetOnClosed(func() {
			if loader != nil {
				// On its own goroutine: stop waits for a decode in flight, and
				// this runs on the UI goroutine.
				go loader.stop()
			}
			player.stopPlayback()
			if clip := clipRef.Load(); clip != nil {
				clip.cancel()
			}
			if strip != nil {
				strip.stop()
			}
		})

		win.SetContent(container.NewBorder(nil, bottom, nil, nil, viewport))
		win.Resize(fyne.NewSize(640, 720))
		if chapterMode {
			win.Resize(fyne.NewSize(760, 880)) // room for the transport and filmstrip
		}
		win.Show()

		if chapterMode {
			// selectChapter does the full switch, so aim it at a chapter index that
			// cannot match the one it is asked for.
			opening := curChapter
			curChapter = -1
			selectChapter(opening)
		} else {
			slider.SetValue(float64(current))
			loadAndShow(current, false)
		}
	})
}

// fpsOptions are the playback rates offered in the transport, "Clip rate" being
// whatever the instance itself states (which is what a chapter opens at).
var fpsOptions = []string{"Clip rate", "5 fps", "10 fps", "15 fps", "20 fps", "24 fps", "30 fps", "60 fps"}

// fpsFromOption resolves a transport selection to a rate, falling back to the
// clip's own stated rate.
func fpsFromOption(opt string, clipFPS float64) float64 {
	n, err := strconv.Atoi(strings.TrimSuffix(opt, " fps"))
	if err != nil || n <= 0 {
		return clipFPS
	}
	return float64(n)
}

// fpsOptionFor picks the transport entry matching a rate, preferring "Clip
// rate" when the rate is the one the instance stated.
func fpsOptionFor(fps, clipFPS float64) string {
	if math.Abs(fps-clipFPS) < 0.01 {
		return fpsOptions[0]
	}
	want := fmt.Sprintf("%d fps", int(fps+0.5))
	for _, opt := range fpsOptions[1:] {
		if opt == want {
			return opt
		}
	}
	return fpsOptions[0]
}
