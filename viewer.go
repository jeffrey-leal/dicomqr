package main

import (
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
	patientName string
	patientID   string
	patientDOB  string
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

// viewerState holds the rendered image and a display label for one DICOM instance.
type viewerState struct {
	img   image.Image
	label string
	ann   imageAnnotations
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

	wc, ww, hasWindow := dicomWindowParams(ds)
	slope, intercept := dicomRescaleParams(ds)
	isSigned := dicomPixelRepresentation(ds) == 1
	bitsAlloc := dicomBitsAllocated(ds)
	photometric := dicomPhotometricInterp(ds)

	img, err := renderDicomFrame(frames[0], wc, ww, hasWindow, slope, intercept, isSigned, bitsAlloc, photometric)
	if err != nil {
		return viewerState{}, err
	}

	b := img.Bounds()
	label := fmt.Sprintf("%d × %d", b.Dx(), b.Dy())
	if hasWindow {
		label += fmt.Sprintf("   W:%.0f  L:%.0f", ww, wc)
	}

	ann := extractAnnotationsFromDataset(ds)
	if hasWindow {
		ann.windowStr = fmt.Sprintf("W: %.0f  L: %.0f", ww, wc)
	}
	return viewerState{img: img, label: label, ann: ann}, nil
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

// renderDicomFrame converts a parsed DICOM frame to a displayable image.Image.
// For grayscale frames: if W/L tags are present they are used as-is; otherwise
// the window is derived from the 1st–99th percentile of the rescaled pixel values,
// which adapts well to modalities like PET/NM where per-image brightness varies.
func renderDicomFrame(f *frame.Frame, wc, ww float64, hasWindow bool, slope, intercept float64, isSigned bool, bitsAlloc int, photometric string) (image.Image, error) {
	if f.IsEncapsulated() {
		return f.GetImage() // JPEG: decoded by standard library
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
			return nf.GetImage()
		}
		return img, nil
	}

	// --- Grayscale (1 sample per pixel) ---

	// When no W/L is provided, derive the window from the pixel value
	// distribution of this image (1st–99th percentile of rescaled values).
	// This gives per-image optimal contrast without requiring pre-scanning the
	// series, and handles modalities like PET/NM where per-slice brightness
	// varies widely and a series-wide window would flatten most slices.
	if !hasWindow || ww <= 0 {
		var vals []float64
		switch data := rawData.(type) {
		case []uint8:
			vals = make([]float64, len(data))
			for i, v := range data {
				vals[i] = float64(v)*slope + intercept
			}
		case []uint16:
			vals = make([]float64, len(data))
			for i, v := range data {
				raw := float64(v)
				if isSigned {
					raw = float64(int16(v))
				}
				vals[i] = raw*slope + intercept
			}
		}
		if n := len(vals); n > 0 {
			sort.Float64s(vals)
			lo := vals[n/100]
			hi := vals[(n*99)/100]
			if hi > lo {
				wc = (lo + hi) / 2
				ww = hi - lo
				hasWindow = true
			}
		}
	}

	invert := photometric == "MONOCHROME1"
	img := image.NewGray(image.Rect(0, 0, cols, rows))
	lower := wc - ww/2

	apply := func(raw int) uint8 {
		var v float64
		if !hasWindow || ww <= 0 {
			// Genuine fallback: bit-depth linear scale (reached only when
			// pixel data is constant or empty and percentile could not be computed).
			maxRaw := float64(int(1)<<uint(bitsAlloc)) - 1
			if maxRaw <= 0 {
				maxRaw = 65535
			}
			if isSigned {
				v = (float64(raw) + maxRaw/2 + 1) / (maxRaw + 1) * 255
			} else {
				v = float64(raw) / maxRaw * 255
			}
		} else {
			rescaled := float64(raw)*slope + intercept
			v = (rescaled - lower) / ww * 255
		}
		out := clampToUint8(v)
		if invert {
			out = 255 - out
		}
		return out
	}

	// img.Stride == cols for a freshly created Gray image, so Pix[i] == pixel i.
	switch data := rawData.(type) {
	case []uint8:
		for i, v := range data {
			img.Pix[i] = apply(int(v))
		}
	case []uint16:
		for i, v := range data {
			if isSigned {
				img.Pix[i] = apply(int(int16(v)))
			} else {
				img.Pix[i] = apply(int(v))
			}
		}
	default:
		return nf.GetImage()
	}
	return img, nil
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

// openViewerWindow creates and shows the DICOM image viewer window.
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

		imgCanvas := canvas.NewImageFromImage(image.NewGray(image.Rect(0, 0, 1, 1)))
		imgCanvas.FillMode = canvas.ImageFillContain
		imgCanvas.SetMinSize(fyne.NewSize(512, 512))

		// Annotation overlay — rendered by imageAnnLayout, constrained to the
		// actual FillContain image rect (never into the letterbox bars).
		annOverlay := container.New(imageAnnLayout{img: imgCanvas})
		imgStack := container.NewStack(imgCanvas, annOverlay)

		showAnn := a.Preferences().BoolWithFallback("showAnnotations", true)
		if !showAnn {
			annOverlay.Hide()
		}

		infoLabel := widget.NewLabel("")
		infoLabel.Alignment = fyne.TextAlignCenter

		counterLbl := widget.NewLabel(fmt.Sprintf("— / %d", total))
		counterLbl.Alignment = fyne.TextAlignCenter

		slider := widget.NewSlider(0, float64(total-1))
		slider.Step = 1

		loadAndShow := func(idx int) {
			counterLbl.SetText(fmt.Sprintf("%d / %d  (loading…)", idx+1, total))
			go func() {
				st, err := loadDicomImage(paths[idx])
				fyne.Do(func() {
					if err != nil {
						infoLabel.SetText("Error: " + err.Error())
						counterLbl.SetText(fmt.Sprintf("%d / %d", idx+1, total))
						return
					}
					imgCanvas.Image = st.img
					imgCanvas.Refresh()
					infoLabel.SetText(st.label)
					counterLbl.SetText(fmt.Sprintf("%d / %d", idx+1, total))
					// Update overlay even when hidden so toggling on shows current data.
					annOverlay.Objects = buildAnnObjects(st.ann, idx, total)
					annOverlay.Refresh()
				})
			}()
		}

		slider.OnChanged = func(v float64) {
			idx := int(v)
			if idx == current {
				return
			}
			current = idx
			loadAndShow(idx)
		}

		annCheck := widget.NewCheck("Annotations", func(checked bool) {
			showAnn = checked
			a.Preferences().SetBool("showAnnotations", checked)
			if checked {
				annOverlay.Show()
			} else {
				annOverlay.Hide()
			}
		})
		annCheck.SetChecked(showAnn)

		bottom := container.NewVBox(
			container.NewCenter(counterLbl),
			slider,
			container.NewBorder(nil, nil, annCheck, nil, infoLabel),
		)

		// Position the slider at the middle slice before showing. OnChanged fires
		// but exits immediately (idx == current) so the image loads exactly once.
		slider.SetValue(float64(current))

		win.SetContent(container.NewBorder(nil, bottom, nil, nil, imgStack))
		win.Resize(fyne.NewSize(600, 650))
		win.Show()

		loadAndShow(current)
	})
}
