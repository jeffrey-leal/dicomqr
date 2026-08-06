package main

// Coverage for the picker and preview geometry. The arithmetic that maps a
// pointer position to a fraction of the image is where this feature would fail
// invisibly: a rectangle dragged over the letterboxed margin of a portrait
// image, converted as though the image filled the widget, lands somewhere else
// entirely on export — and looks plausible on screen while doing it.

import (
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// testCanvas returns a canvas holding a cols×rows image, sized to w×h.
func testCanvas(t *testing.T, cols, rows int, w, h float32) *maskCanvas {
	t.Helper()
	c := newMaskCanvas(image.NewGray(image.Rect(0, 0, cols, rows)), maskCanvasEdit)
	c.Resize(fyne.NewSize(w, h))
	return c
}

// The image is centred and letterboxed, so the widget's coordinates are not
// the image's.
func TestMaskCanvasDisplayArea(t *testing.T) {
	test.NewApp()
	cases := []struct {
		name                   string
		cols, rows             int
		w, h                   float32
		offX, offY, dispW, dis float32
	}{
		{"square image in a wide widget", 100, 100, 400, 200, 100, 0, 200, 200},
		{"wide image in a square widget", 200, 100, 300, 300, 0, 75, 300, 150},
		{"exact fit", 100, 50, 400, 200, 0, 0, 400, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testCanvas(t, tc.cols, tc.rows, tc.w, tc.h)
			offX, offY, dw, dh := c.displayArea(fyne.NewSize(tc.w, tc.h))
			if offX != tc.offX || offY != tc.offY || dw != tc.dispW || dh != tc.dis {
				t.Errorf("displayArea = %v,%v %v×%v; want %v,%v %v×%v",
					offX, offY, dw, dh, tc.offX, tc.offY, tc.dispW, tc.dis)
			}
		})
	}
}

// A position inside the letterbox maps to the image edge, not past it: a drag
// that starts in the margin still produces a rectangle on the image.
func TestMaskCanvasFractionAt(t *testing.T) {
	test.NewApp()
	// 100×100 image in a 400×200 widget: displayed 200×200 at x=100.
	c := testCanvas(t, 100, 100, 400, 200)
	cases := []struct {
		name   string
		pos    fyne.Position
		fx, fy float64
	}{
		{"image centre", fyne.NewPos(200, 100), 0.5, 0.5},
		{"image top-left", fyne.NewPos(100, 0), 0, 0},
		{"image bottom-right", fyne.NewPos(300, 200), 1, 1},
		{"left letterbox clamps", fyne.NewPos(0, 100), 0, 0.5},
		{"right letterbox clamps", fyne.NewPos(400, 100), 1, 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx, fy := c.fractionAt(tc.pos)
			if fx != tc.fx || fy != tc.fy {
				t.Errorf("fractionAt(%v) = %v,%v; want %v,%v", tc.pos, fx, fy, tc.fx, tc.fy)
			}
		})
	}
}

// Dragging produces a fractional rectangle, normalized so that dragging up and
// to the left works as well as down and to the right.
func TestMaskCanvasDragProducesRegion(t *testing.T) {
	test.NewApp()
	c := testCanvas(t, 100, 100, 200, 200) // 1:1, no letterbox

	drag := func(from, to fyne.Position) {
		c.Dragged(&fyne.DragEvent{PointEvent: fyne.PointEvent{Position: from}})
		c.Dragged(&fyne.DragEvent{PointEvent: fyne.PointEvent{Position: to}})
		c.DragEnd()
	}

	drag(fyne.NewPos(0, 0), fyne.NewPos(200, 40))
	if len(c.rects) != 1 {
		t.Fatalf("rects = %d, want 1", len(c.rects))
	}
	if got := c.rects[0]; got.X != 0 || got.Y != 0 || got.W != 1 || got.H != 0.2 {
		t.Errorf("region = %+v, want the top 20%% band", got)
	}

	// Backwards drag: same rectangle.
	c.clear()
	drag(fyne.NewPos(150, 100), fyne.NewPos(50, 20))
	if len(c.rects) != 1 {
		t.Fatalf("rects = %d, want 1", len(c.rects))
	}
	if got := c.rects[0]; got.X != 0.25 || got.Y != 0.1 || got.W != 0.5 || got.H != 0.4 {
		t.Errorf("region = %+v, want it normalized", got)
	}

	// A click is not a rectangle.
	c.clear()
	drag(fyne.NewPos(100, 100), fyne.NewPos(100, 100))
	if len(c.rects) != 0 {
		t.Errorf("a click produced %d rectangle(s)", len(c.rects))
	}
}

// Screen coordinates are float32, so an unsnapped drag stores fractions like
// 0.20000000298023224 — which reaches profiles.json and the editor's fields.
// Snapping to the image's pixel grid keeps the stored value exact and gives it
// a meaning: the rectangle drawn, to the nearest pixel.
func TestMaskCanvasDragSnapsToPixels(t *testing.T) {
	test.NewApp()
	c := testCanvas(t, 800, 600, 400, 300) // displayed at half size

	c.Dragged(&fyne.DragEvent{PointEvent: fyne.PointEvent{Position: fyne.NewPos(0, 0)}})
	c.Dragged(&fyne.DragEvent{PointEvent: fyne.PointEvent{Position: fyne.NewPos(400, 24)}})
	c.DragEnd()

	if len(c.rects) != 1 {
		t.Fatalf("rects = %d, want 1", len(c.rects))
	}
	got := c.rects[0]
	// 24 display pixels = 48 image pixels of 600.
	if got.X != 0 || got.Y != 0 || got.W != 1 || got.H != 0.08 {
		t.Errorf("region = %+v, want an exact 8%% band", got)
	}
	if formatPercent(got.H) != "8" {
		t.Errorf("height displays as %q, want \"8\"", formatPercent(got.H))
	}
}

func TestMaskCanvasUndoAndClear(t *testing.T) {
	test.NewApp()
	c := testCanvas(t, 100, 100, 200, 200)
	c.setRects([]MaskRegion{{W: 1, H: 0.1}, {W: 1, H: 0.2}})
	c.undo()
	if len(c.rects) != 1 || c.rects[0].H != 0.1 {
		t.Errorf("after undo: %+v, want only the first", c.rects)
	}
	c.clear()
	if len(c.rects) != 0 {
		t.Errorf("after clear: %+v, want none", c.rects)
	}
}

// The preview draws what the engine resolved, so pixel rectangles have to come
// back as the fractions the canvas speaks.
func TestPixelRectsToRegions(t *testing.T) {
	got := pixelRectsToRegions([]pixelRect{{0, 0, 100, 8}, {50, 25, 100, 50}}, 100, 100)
	want := []MaskRegion{
		{Mode: maskModeRect, X: 0, Y: 0, W: 1, H: 0.08},
		{Mode: maskModeRect, X: 0.5, Y: 0.25, W: 0.5, H: 0.25},
	}
	if len(got) != len(want) {
		t.Fatalf("regions = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("region %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// writeSizedTestDICOM writes a minimal file of the given modality and size.
func writeSizedTestDICOM(t *testing.T, path, modality string, cols, rows int) {
	t.Helper()
	nf := frame.NewNativeFrame[uint8](8, rows, cols, cols*rows, 1)
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		Frames: []*frame.Frame{{Encapsulated: false, NativeData: nf}},
	})
	if err != nil {
		t.Fatalf("NewElement(PixelData): %v", err)
	}
	ds := sdicom.Dataset{Elements: []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3." + filepath.Base(path)}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{tsExplicitVRLE}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3." + filepath.Base(path)}),
		mustTestElement(t, tag.Modality, []string{modality}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{"MONOCHROME2"}),
		mustTestElement(t, tag.Rows, []int{rows}),
		mustTestElement(t, tag.Columns, []int{cols}),
		mustTestElement(t, tag.BitsAllocated, []int{8}),
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.HighBit, []int{7}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		mustTestElement(t, tag.SamplesPerPixel, []int{1}),
		pd,
	}}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// The preview groups by modality and size, largest group first — the whole
// point being that a study mixing sizes shows every one of them.
func TestScanMaskGeometryClasses(t *testing.T) {
	dir := t.TempDir()
	var files []string
	add := func(name, modality string, cols, rows int) {
		p := filepath.Join(dir, name)
		writeSizedTestDICOM(t, p, modality, cols, rows)
		files = append(files, p)
	}
	add("us1.dcm", "US", 800, 600)
	add("us2.dcm", "US", 800, 600)
	add("us3.dcm", "US", 800, 600)
	add("sc1.dcm", "SC", 1024, 768)
	add("ct1.dcm", "CT", 512, 512)
	add("ct2.dcm", "CT", 512, 512)

	classes := scanMaskGeometryClasses(files, nil)
	if len(classes) != 3 {
		t.Fatalf("classes = %d (%+v), want 3", len(classes), classes)
	}
	if classes[0].modality != "US" || classes[0].count() != 3 || classes[0].cols != 800 {
		t.Errorf("first class = %+v, want the 3 ultrasound files", classes[0])
	}
	if classes[1].modality != "CT" || classes[1].count() != 2 {
		t.Errorf("second class = %+v, want the 2 CT files", classes[1])
	}
	// The lone secondary capture — a different size from everything else, and
	// exactly the file a rectangle judged on the ultrasound loops would miss.
	if classes[2].modality != "SC" || classes[2].count() != 1 || classes[2].cols != 1024 {
		t.Errorf("third class = %+v, want the single 1024×768 capture", classes[2])
	}
	for _, c := range classes {
		if len(c.files) == 0 {
			t.Errorf("class %+v has no files to display", c)
		}
	}
}

// Ultrasound images that state no calibrated region are their own class even
// at identical dimensions: the two are masked by entirely different means, and
// grouping them would hide the ones needing a rectangle among the ones that
// need none. This is the study shape that prompted stepping through files at
// all — 88 calibrated loops and a handful of analysis screens.
func TestScanMaskGeometryClassesSeparatesUncalibratedUltrasound(t *testing.T) {
	dir := t.TempDir()
	var files []string
	add := func(name string, calibrated bool) {
		p := filepath.Join(dir, name)
		writeSizedTestDICOM(t, p, "US", 800, 600)
		if calibrated {
			ds, err := sdicom.ParseFile(p, nil)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			ds.Elements = append(ds.Elements, usRegionElement(t, 10, 40, 789, 559))
			f, err := os.Create(p)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
				t.Fatalf("write: %v", err)
			}
			f.Close()
		}
		files = append(files, p)
	}
	add("loop1.dcm", true)
	add("loop2.dcm", true)
	add("loop3.dcm", true)
	add("worksheet1.dcm", false)
	add("worksheet2.dcm", false)

	classes := scanMaskGeometryClasses(files, nil)
	if len(classes) != 2 {
		t.Fatalf("classes = %d (%+v), want the calibrated and uncalibrated groups apart", len(classes), classes)
	}
	if !classes[0].calibrated || classes[0].count() != 3 {
		t.Errorf("first class = %+v, want the 3 calibrated loops", classes[0])
	}
	if classes[1].calibrated || classes[1].count() != 2 {
		t.Errorf("second class = %+v, want the 2 analysis screens", classes[1])
	}
	// The label has to say why they are separate, or the split looks arbitrary.
	if !strings.Contains(classes[1].label(), "no calibrated region") {
		t.Errorf("label = %q, want it to name the reason", classes[1].label())
	}
	// Every file is kept so the window can step through them, not just one.
	if len(classes[1].files) != 2 {
		t.Errorf("files = %v, want both analysis screens steppable", classes[1].files)
	}
}

// The working set decides which list a drawn rectangle joins, which depends on
// whether an override governs the image it was drawn on.
func TestMaskWorkingSet(t *testing.T) {
	p := ModProfile{
		MaskRegions: []MaskRegion{{Mode: maskModeOutsideUS}},
		PerModality: map[string]ModProfile{
			"SC": {MaskRegions: []MaskRegion{{Mode: maskModeRect, W: 1, H: 0.1}}},
			"CT": {Removes: []string{"0008,1030"}}, // no regions: the profile governs
		},
	}
	w := newMaskWorkingSet(p)

	if regions, code := w.governing("US"); code != "" || len(regions) != 1 {
		t.Errorf("US governed by %q with %+v, want the profile's own", code, regions)
	}
	if regions, code := w.governing("sc"); code != "SC" || len(regions) != 1 {
		t.Errorf("SC governed by %q with %+v, want its override", code, regions)
	}
	// An override that states no regions does not take over.
	if _, code := w.governing("CT"); code != "" {
		t.Errorf("CT governed by %q, want the profile's own", code)
	}

	w.add("US", MaskRegion{Mode: maskModeRect, W: 1, H: 0.08})
	w.add("SC", MaskRegion{Mode: maskModeRect, W: 0.5, H: 0.5})
	if len(w.profile) != 2 || len(w.perMod["SC"]) != 2 {
		t.Fatalf("after add: profile %+v, SC %+v", w.profile, w.perMod["SC"])
	}

	// Applying must not write through to the caller's profile.
	out := w.applyTo(p)
	if len(out.MaskRegions) != 2 || len(out.PerModality["SC"].MaskRegions) != 2 {
		t.Errorf("applyTo = %+v / %+v", out.MaskRegions, out.PerModality["SC"].MaskRegions)
	}
	if len(p.MaskRegions) != 1 || len(p.PerModality["SC"].MaskRegions) != 1 {
		t.Errorf("applyTo mutated the source profile: %+v / %+v",
			p.MaskRegions, p.PerModality["SC"].MaskRegions)
	}
	if out.PerModality["CT"].Removes == nil {
		t.Error("an override without regions lost its other settings")
	}

	// Undo takes back this session's last edit wherever it went — here the SC
	// rectangle, not the US one added before it.
	w.undo()
	if len(w.perMod["SC"]) != 1 || len(w.profile) != 2 {
		t.Errorf("after undo: profile %+v, SC %+v; want the SC rectangle taken back",
			w.profile, w.perMod["SC"])
	}
	w.undo()
	if len(w.profile) != 1 || maskRegionMode(w.profile[0]) != maskModeOutsideUS {
		t.Errorf("after second undo: %+v, want only the ultrasound rule left", w.profile)
	}
	// Nothing this session added remains, so undo must stop rather than start
	// eating the regions the profile arrived with — an ultrasound rule nobody
	// drew, whose silent loss changes what gets masked everywhere.
	if w.canUndo() {
		t.Error("canUndo is true with nothing left from this session")
	}
	w.undo()
	if len(w.profile) != 1 {
		t.Errorf("undo removed a region it did not add: %+v", w.profile)
	}

	// After both undos the applied result is back to what the profile carried.
	out = w.applyTo(p)
	if len(out.MaskRegions) != 1 || len(out.PerModality["SC"].MaskRegions) != 1 {
		t.Errorf("applyTo after undo = %+v / %+v", out.MaskRegions, out.PerModality["SC"].MaskRegions)
	}
}

// An unreadable file must not sink the scan: the preview is advisory, and one
// bad file should not stop the rest being checked.
func TestScanMaskGeometryClassesSkipsUnreadable(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.dcm")
	writeSizedTestDICOM(t, good, "US", 640, 480)
	bad := filepath.Join(dir, "bad.dcm")
	if err := os.WriteFile(bad, []byte("not a dicom file"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	classes := scanMaskGeometryClasses([]string{good, bad}, nil)
	if len(classes) != 1 || classes[0].count() != 1 || classes[0].cols != 640 {
		t.Errorf("classes = %+v, want just the readable file", classes)
	}
}
