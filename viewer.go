package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// dicomInstance pairs a .dcm file path with its InstanceNumber for sorting.
type dicomInstance struct {
	path           string
	instanceNumber int
}

// dicomInstanceNumber parses InstanceNumber from a DICOM file without pixel data.
func dicomInstanceNumber(path string) int {
	ds, err := sdicom.ParseFile(path, nil, sdicom.SkipPixelData())
	if err != nil {
		return 0
	}
	elem, err := ds.FindElementByTag(tag.InstanceNumber)
	if err != nil {
		return 0
	}
	strs := sdicom.MustGetStrings(elem.Value)
	if len(strs) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(strs[0]))
	return n
}

// collectDicomFiles walks dir and returns .dcm file paths sorted by InstanceNumber.
func collectDicomFiles(dir string) ([]string, error) {
	var instances []dicomInstance
	err := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".dcm") {
			instances = append(instances, dicomInstance{
				path:           path,
				instanceNumber: dicomInstanceNumber(path),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(instances) == 0 {
		return nil, errors.New("no DICOM files found in: " + dir)
	}
	sort.SliceStable(instances, func(i, j int) bool {
		if instances[i].instanceNumber != instances[j].instanceNumber {
			return instances[i].instanceNumber < instances[j].instanceNumber
		}
		return instances[i].path < instances[j].path
	})
	paths := make([]string, len(instances))
	for i, inst := range instances {
		paths[i] = inst.path
	}
	return paths, nil
}

// sortDicomByInstance returns paths sorted by DICOM InstanceNumber.
func sortDicomByInstance(rawPaths []string) []string {
	instances := make([]dicomInstance, len(rawPaths))
	for i, p := range rawPaths {
		instances[i] = dicomInstance{path: p, instanceNumber: dicomInstanceNumber(p)}
	}
	sort.SliceStable(instances, func(i, j int) bool {
		if instances[i].instanceNumber != instances[j].instanceNumber {
			return instances[i].instanceNumber < instances[j].instanceNumber
		}
		return instances[i].path < instances[j].path
	})
	sorted := make([]string, len(instances))
	for i, inst := range instances {
		sorted[i] = inst.path
	}
	return sorted
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
var unsupportedTransferSyntaxNames = map[string]string{
	"1.2.840.10008.1.2.4.57": "JPEG Lossless Non-Hierarchical",
	"1.2.840.10008.1.2.4.70": "JPEG Lossless (Process 14, SV1)",
	"1.2.840.10008.1.2.4.80": "JPEG-LS Lossless",
	"1.2.840.10008.1.2.4.81": "JPEG-LS Near-Lossless",
	"1.2.840.10008.1.2.4.90": "JPEG 2000 Lossless",
	"1.2.840.10008.1.2.4.91": "JPEG 2000",
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
	gray       []float32   // rescaled grayscale values, len rows*cols; nil for colour
	colorImg   image.Image // non-nil for RGB / JPEG-decoded frames (not windowable)
	invert     bool        // MONOCHROME1 — invert the display ramp

	wc, ww         float64 // default window centre/width (from tags or auto)
	lo, hi         float64 // full rescaled data range, for the "Full range" preset
	windowFromTags bool    // true if wc/ww came from DICOM Window tags
	modality       string  // DICOM Modality (CT, PT, MR, …); selects the preset set
}

// windowable reports whether window/level adjustment affects this frame.
func (d *decodedFrame) windowable() bool { return d != nil && d.colorImg == nil }

// render produces a displayable image at the given window centre/width.
func (d *decodedFrame) render(wc, ww float64) image.Image {
	if d.colorImg != nil {
		return d.colorImg
	}
	img := image.NewGray(image.Rect(0, 0, d.cols, d.rows))
	d.renderInto(img, wc, ww)
	return img
}

// renderInto windows the frame into an existing *image.Gray (dst must be
// cols×rows). Reusing a buffer across window changes avoids per-drag allocation
// and keeps the canvas.Image backing pointer stable, which is important for
// flicker-free interactive window/level dragging.
func (d *decodedFrame) renderInto(dst *image.Gray, wc, ww float64) {
	if d.gray == nil {
		return
	}
	if ww < 1 {
		ww = 1
	}
	lower := wc - ww/2
	inv := d.invert
	// Divide by ww before scaling to 255 (rather than premultiplying 255/ww) so
	// that a pixel exactly at the top of the window maps to 255, not 254.
	// dst.Stride == cols for a cols×rows Gray image, so Pix[i] == pixel i.
	for i, v := range d.gray {
		out := clampToUint8((float64(v) - lower) / ww * 255)
		if inv {
			out = 255 - out
		}
		dst.Pix[i] = out
	}
}

// computeDefaultWindow fills wc/ww/lo/hi for a freshly decoded grayscale frame.
// If the file carried explicit Window tags they win; otherwise the window is
// derived from the 1st–99th percentile of the rescaled values (good contrast
// for PET/NM and other modalities lacking window tags); failing that, the full
// data range is used.
func (d *decodedFrame) computeDefaultWindow(hasWindow bool, wc, ww float64) {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range d.gray {
		f := float64(v)
		if f < lo {
			lo = f
		}
		if f > hi {
			hi = f
		}
	}
	if math.IsInf(lo, 1) {
		lo, hi = 0, 0
	}
	d.lo, d.hi = lo, hi

	if hasWindow && ww > 0 {
		d.wc, d.ww, d.windowFromTags = wc, ww, true
		return
	}
	if len(d.gray) > 0 && hi > lo {
		cp := make([]float64, len(d.gray))
		for i, v := range d.gray {
			cp[i] = float64(v)
		}
		sort.Float64s(cp)
		n := len(cp)
		plo, phi := cp[n/100], cp[(n*99)/100]
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
// Modality code (CT, PT = PET, MR), falling back to a generic set.
func presetsForModality(mod string) []wlPreset {
	switch strings.ToUpper(strings.TrimSpace(mod)) {
	case "CT":
		return ctPresets
	case "PT":
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

	gray := make([]float32, pixelsPerFrame)
	if bitsAlloc <= 8 {
		for i := 0; i < pixelsPerFrame; i++ {
			gray[i] = float32(float64(data[i])*slope + intercept)
		}
	} else {
		for i := 0; i < pixelsPerFrame; i++ {
			raw := float64(binary.LittleEndian.Uint16(data[i*2:]))
			if isSigned {
				raw = float64(int16(binary.LittleEndian.Uint16(data[i*2:])))
			}
			gray[i] = float32(raw*slope + intercept)
		}
	}

	df := &decodedFrame{rows: rows, cols: cols, gray: gray, invert: photometric == "MONOCHROME1"}
	df.computeDefaultWindow(hasWindow, wc, ww)
	return df, nil
}

// loadDicomImage parses a DICOM file and returns a windowed image.Image.
// Only the first frame of multi-frame objects is rendered.
func loadDicomImage(path string) (viewerState, error) {
	frameCh := make(chan *frame.Frame, 8)
	ds, err := sdicom.ParseFile(path, frameCh)
	if err != nil {
		for range frameCh {
		}
		return viewerState{}, err
	}
	var frames []*frame.Frame
	for f := range frameCh {
		frames = append(frames, f)
	}
	if len(frames) == 0 {
		return viewerState{}, errors.New("no pixel data in file")
	}

	// Detect encapsulated transfer syntaxes the built-in viewer cannot decode
	// before calling renderDicomFrame, so the user gets a clear message instead
	// of a raw "missing SOI marker" JPEG error from the underlying library.
	if e, err2 := ds.FindElementByTag(tag.TransferSyntaxUID); err2 == nil {
		if strs := sdicom.MustGetStrings(e.Value); len(strs) > 0 {
			if name, unsup := unsupportedTransferSyntaxNames[strings.TrimSpace(strs[0])]; unsup {
				return viewerState{}, fmt.Errorf(
					"%s compressed images cannot be decoded by the built-in viewer\n\nUse Open in Viewer to open this file in an external DICOM viewer.", name)
			}
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

	df, err := decodeFrame(frames[0], hasWindow, wc, ww, slope, intercept, isSigned, bitsAlloc, photometric)

	// Fallback: some DICOM implementations store uncompressed pixel data with
	// an undefined-length VL, which the library mistakes for encapsulated (JPEG)
	// data. When jpeg.Decode fails, re-interpret the raw bytes natively.
	if err != nil && frames[0].IsEncapsulated() && rows > 0 && cols > 0 {
		df, err = decodeRawPixelFallback(
			frames[0].EncapsulatedData.Data,
			rows, cols, samplesPerPixel, bitsAlloc,
			hasWindow, wc, ww, slope, intercept, isSigned, photometric,
		)
	}
	if err != nil {
		return viewerState{}, err
	}

	img := df.render(df.wc, df.ww)
	b := img.Bounds()
	label := fmt.Sprintf("%d × %d", b.Dx(), b.Dy())
	ann := extractAnnotationsFromDataset(ds)
	df.modality = ann.modality
	if df.windowable() {
		label += fmt.Sprintf("   W:%.0f  L:%.0f", df.ww, df.wc)
		ann.windowStr = fmt.Sprintf("W: %.0f  L: %.0f", df.ww, df.wc)
	}
	return viewerState{img: img, frame: df, label: label, ann: ann}, nil
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

// decodeFrame converts a parsed DICOM frame into a decodedFrame. Grayscale
// pixels are rescaled (slope/intercept) into a float buffer once so the viewer
// can re-window them cheaply; colour frames are rendered directly and are not
// windowable. The default window comes from DICOM Window tags when present,
// otherwise from the 1st–99th percentile of the rescaled values.
func decodeFrame(f *frame.Frame, hasWindow bool, wc, ww, slope, intercept float64, isSigned bool, bitsAlloc int, photometric string) (*decodedFrame, error) {
	if f.IsEncapsulated() {
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

	// --- Grayscale (1 sample per pixel): rescale into a float buffer ---
	gray := make([]float32, rows*cols)
	switch data := rawData.(type) {
	case []uint8:
		for i, v := range data {
			gray[i] = float32(float64(v)*slope + intercept)
		}
	case []uint16:
		for i, v := range data {
			raw := float64(v)
			if isSigned {
				raw = float64(int16(v))
			}
			gray[i] = float32(raw*slope + intercept)
		}
	default:
		img2, err := nf.GetImage()
		if err != nil {
			return nil, err
		}
		return &decodedFrame{rows: rows, cols: cols, colorImg: img2}, nil
	}

	df := &decodedFrame{rows: rows, cols: cols, gray: gray, invert: photometric == "MONOCHROME1"}
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

// seriesThumb bundles a display label and the pre-sorted file paths for one
// series, used by the study overview window.
type seriesThumb struct {
	label string
	paths []string // sorted by InstanceNumber
}

// thumbnailCell is a widget displaying a single DICOM thumbnail image with a
// label below it. Double-tapping opens the full series viewer.
type thumbnailCell struct {
	widget.BaseWidget
	imgObj *canvas.Image
	lbl    *widget.Label
	paths  []string
	title  string
	app    fyne.App
}

func newThumbnailCell(img image.Image, label, title string, paths []string, app fyne.App) *thumbnailCell {
	c := &thumbnailCell{
		imgObj: canvas.NewImageFromImage(img),
		lbl:    widget.NewLabelWithStyle(label, fyne.TextAlignCenter, fyne.TextStyle{}),
		paths:  paths,
		title:  title,
		app:    app,
	}
	c.imgObj.FillMode = canvas.ImageFillContain
	c.imgObj.SetMinSize(fyne.NewSize(180, 180))
	c.lbl.Truncation = fyne.TextTruncateEllipsis
	c.ExtendBaseWidget(c)
	return c
}

// DoubleTapped opens the full series viewer for this thumbnail's files.
func (c *thumbnailCell) DoubleTapped(_ *fyne.PointEvent) {
	paths, title, app := c.paths, c.title, c.app
	go openViewerWindow(app, title, paths, nil)
}

func (c *thumbnailCell) CreateRenderer() fyne.WidgetRenderer {
	c.ExtendBaseWidget(c)
	return widget.NewSimpleRenderer(
		container.NewBorder(nil, c.lbl, nil, nil, c.imgObj),
	)
}

// showStudyOverviewWindow opens a grid window showing the middle slice of each
// series for a study. Thumbnails are loaded in parallel. Double-clicking any
// thumbnail opens the full series viewer for that series.
// Must be called from a non-UI goroutine.
func showStudyOverviewWindow(a fyne.App, title string, series []seriesThumb) {
	if len(series) == 0 {
		fyne.Do(func() {
			win := a.NewWindow(title)
			win.SetContent(container.NewCenter(widget.NewLabel("No series found.")))
			win.Resize(fyne.NewSize(420, 160))
			win.Show()
		})
		return
	}

	// Load the middle slice of every series in parallel.
	thumbs := make([]viewerState, len(series))
	var wg sync.WaitGroup
	for i, s := range series {
		wg.Add(1)
		i, s := i, s
		go func() {
			defer wg.Done()
			if len(s.paths) == 0 {
				return
			}
			vs, err := loadDicomImage(s.paths[len(s.paths)/2])
			if err == nil {
				thumbs[i] = vs
			}
		}()
	}
	wg.Wait()

	fyne.Do(func() {
		win := a.NewWindow(title)

		cells := make([]fyne.CanvasObject, len(series))
		for i, s := range series {
			img := thumbs[i].img
			if img == nil {
				img = image.NewGray(image.Rect(0, 0, 1, 1))
			}
			cells[i] = newThumbnailCell(img, s.label, "DICOM Preview — "+s.label, s.paths, a)
		}

		cols := 3
		if len(cells) < cols {
			cols = len(cells)
		}
		grid := container.NewGridWithColumns(cols, cells...)

		hint := widget.NewLabelWithStyle(
			"Double-click a thumbnail to open the full series viewer.",
			fyne.TextAlignCenter, fyne.TextStyle{Italic: true},
		)

		win.SetContent(container.NewBorder(hint, nil, nil, nil, container.NewScroll(grid)))
		win.Resize(fyne.NewSize(float32(cols)*200+40, 560))
		win.Show()
	})
}

// showDicomViewer opens the DICOM preview window for all images in folder.
// Must be called from a non-UI goroutine.
func showDicomViewer(a fyne.App, folder string) {
	paths, collectErr := collectDicomFiles(folder)
	openViewerWindow(a, "DICOM Preview — "+filepath.Base(folder), paths, collectErr)
}

// showDicomViewerPaths opens the DICOM preview window for a specific set of files.
// Paths are sorted by InstanceNumber before display.
// Must be called from a non-UI goroutine.
func showDicomViewerPaths(a fyne.App, title string, rawPaths []string) {
	if len(rawPaths) == 0 {
		fyne.Do(func() {
			win := a.NewWindow(title)
			win.SetContent(container.NewCenter(widget.NewLabel("No images in this selection.")))
			win.Resize(fyne.NewSize(420, 160))
			win.Show()
		})
		return
	}
	openViewerWindow(a, title, sortDicomByInstance(rawPaths), nil)
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
// (SubImage) so the displayed image always fills the viewport without overflow.
type imageViewport struct {
	widget.BaseWidget

	img     *canvas.Image
	overlay *fyne.Container

	frame *decodedFrame
	base  image.Image // frame rendered at current wc/ww (full frame, pre-crop)
	buf   *image.Gray // reused windowing buffer for grayscale frames (flicker-free drag)
	wc    float64
	ww    float64

	zoom         float64 // 1 = fit; >1 = magnified
	panCX, panCY float64 // crop centre in source-pixel coordinates
	wlSens       float64 // window/level units per dragged pixel

	ann   imageAnnotations
	idx   int
	total int

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
		img:    canvas.NewImageFromImage(image.NewGray(image.Rect(0, 0, 1, 1))),
		zoom:   1,
		wlSens: 1,
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

// renderBase produces v.base for the current frame at (wc, ww). For grayscale
// frames it windows into the reused v.buf buffer (allocated on demand) so that
// rapid window changes neither allocate nor swap the backing image pointer.
func (v *imageViewport) renderBase(wc, ww float64) {
	df := v.frame
	if df == nil {
		return
	}
	if !df.windowable() {
		v.base = df.render(wc, ww) // colour frame: render returns colorImg
		return
	}
	if v.buf == nil || v.buf.Rect.Dx() != df.cols || v.buf.Rect.Dy() != df.rows {
		v.buf = image.NewGray(image.Rect(0, 0, df.cols, df.rows))
	}
	df.renderInto(v.buf, wc, ww)
	v.base = v.buf
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
	if sub, ok := v.base.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		v.img.Image = sub.SubImage(crop)
	} else {
		v.img.Image = v.base
	}
	v.img.Refresh()
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

// openViewerWindow creates and shows the interactive DICOM image viewer window.
// Must be called from a non-UI goroutine; all widget creation is via fyne.Do.
func openViewerWindow(a fyne.App, title string, paths []string, collectErr error) {
	fyne.Do(func() {
		win := a.NewWindow(title)

		if collectErr != nil || len(paths) == 0 {
			msg := "No DICOM files found."
			if collectErr != nil {
				msg = collectErr.Error()
			}
			win.SetContent(container.NewCenter(widget.NewLabel(msg)))
			win.Resize(fyne.NewSize(420, 160))
			win.Show()
			return
		}

		total := len(paths)
		current := total / 2 // open at the middle slice

		viewport := newImageViewport()
		showAnn := a.Preferences().BoolWithFallback("showAnnotations", true)
		viewport.setShowAnn(showAnn)

		infoLabel := widget.NewLabel("")
		infoLabel.Alignment = fyne.TextAlignCenter

		counterLbl := widget.NewLabel(fmt.Sprintf("— / %d", total))
		counterLbl.Alignment = fyne.TextAlignCenter

		slider := widget.NewSlider(0, float64(total-1))
		slider.Step = 1

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

		loadAndShow := func(idx int, keepView bool) {
			counterLbl.SetText(fmt.Sprintf("%d / %d  (loading…)", idx+1, total))
			go func() {
				st, err := loadDicomImage(paths[idx])
				fyne.Do(func() {
					if err != nil {
						infoLabel.SetText("Error: " + err.Error())
						counterLbl.SetText(fmt.Sprintf("%d / %d", idx+1, total))
						return
					}
					// Pick the modality-appropriate preset set on the first frame
					// (and on the rare chance the modality changes mid-series).
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
					}
					wc, ww := targetWindow(st.frame)
					curWC, curWW = wc, ww
					viewport.setContent(st.frame, wc, ww, st.ann, idx, total, keepView)
					setInfo(st.frame, wc, ww)
					if st.frame.windowable() {
						presetSelect.Enable()
					} else {
						presetSelect.Disable()
					}
					counterLbl.SetText(fmt.Sprintf("%d / %d", idx+1, total))
				})
			}()
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

		slider.OnChanged = func(vf float64) {
			idx := int(vf)
			if idx == current {
				return
			}
			current = idx
			loadAndShow(idx, true)
		}

		annCheck := widget.NewCheck("Annotations", func(checked bool) {
			a.Preferences().SetBool("showAnnotations", checked)
			viewport.setShowAnn(checked)
		})
		annCheck.SetChecked(showAnn)

		resetBtn := widget.NewButton("Reset", func() {
			viewport.resetView()
			presetSelect.SetSelected("Default") // fires OnChanged → reset window
		})

		// Keyboard: arrows/page = slice navigation; +/- = zoom; R = reset window;
		// Home/F = reset zoom & pan.
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

		controls := container.NewHBox(
			widget.NewLabel("Window:"), presetSelect, annCheck, resetBtn,
		)
		bottom := container.NewVBox(
			container.NewCenter(counterLbl),
			slider,
			container.NewBorder(nil, nil, controls, nil, infoLabel),
		)

		// Position the slider at the middle slice before showing. When current is
		// nonzero this is a no-op for loading (guard below), so load explicitly.
		slider.SetValue(float64(current))

		win.SetContent(container.NewBorder(nil, bottom, nil, nil, viewport))
		win.Resize(fyne.NewSize(640, 720))
		win.Show()

		loadAndShow(current, false)
	})
}
