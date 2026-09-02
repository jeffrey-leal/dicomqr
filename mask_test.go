package main

// Pixel-masking coverage. Two properties matter more than any other and are
// asserted directly rather than through a rendered image: every pixel inside a
// region is the fill value, and every pixel outside it is bit-identical to what
// it was. A mask that bleeds is a corrupted export; a mask that falls short is
// a disclosure.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// maskTestDataset builds a native dataset with a known ramp in every sample, so
// any pixel the mask touches is identifiable by value alone.
func maskTestDataset(t *testing.T, cols, rows, spp int, photometric string, extra ...*sdicom.Element) (*sdicom.Dataset, *frame.NativeFrame[uint8]) {
	t.Helper()
	nf := frame.NewNativeFrame[uint8](8, rows, cols, cols*rows, spp)
	for i := range nf.RawData {
		nf.RawData[i] = uint8(i%254 + 1) // never 0, so a zero fill is unmistakable
	}
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		IsEncapsulated: false,
		Frames:         []*frame.Frame{{Encapsulated: false, NativeData: nf}},
	})
	if err != nil {
		t.Fatalf("NewElement(PixelData): %v", err)
	}
	elems := []*sdicom.Element{
		mustTestElement(t, tag.PhotometricInterpretation, []string{photometric}),
		mustTestElement(t, tag.Rows, []int{rows}),
		mustTestElement(t, tag.Columns, []int{cols}),
		mustTestElement(t, tag.BitsAllocated, []int{8}),
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.HighBit, []int{7}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		mustTestElement(t, tag.SamplesPerPixel, []int{spp}),
	}
	elems = append(elems, extra...)
	elems = append(elems, pd)
	return &sdicom.Dataset{Elements: elems}, nf
}

// sampleAt indexes an interleaved frame.
func sampleAt(raw []uint8, cols, spp, x, y, s int) uint8 { return raw[(y*cols+x)*spp+s] }

func TestValidateMaskRegions(t *testing.T) {
	cases := []struct {
		name string
		r    MaskRegion
		want string // substring of the expected error; "" = must be accepted
	}{
		{"top banner", MaskRegion{Mode: maskModeRect, X: 0, Y: 0, W: 1, H: 0.08}, ""},
		{"mode defaults to rect", MaskRegion{W: 0.5, H: 0.5}, ""},
		{"whole image", MaskRegion{Mode: maskModeRect, W: 1, H: 1}, ""},
		{"ultrasound", MaskRegion{Mode: maskModeOutsideUS}, ""},
		{"mode is case-insensitive", MaskRegion{Mode: "Outside-US-Regions"}, ""},
		{"unknown mode", MaskRegion{Mode: "rows", H: 0.1}, "must be rect"},
		{"negative origin", MaskRegion{Mode: maskModeRect, X: -0.1, W: 0.5, H: 0.5}, "between 0 and 1"},
		{"fraction over one", MaskRegion{Mode: maskModeRect, W: 1.5, H: 0.5}, "between 0 and 1"},
		{"zero width", MaskRegion{Mode: maskModeRect, W: 0, H: 0.5}, "greater than 0"},
		{"runs off the edge", MaskRegion{Mode: maskModeRect, X: 0.8, W: 0.4, H: 0.5}, "past the image"},
		{"us rule with geometry", MaskRegion{Mode: maskModeOutsideUS, H: 0.1}, "takes no x/y/w/h"},
		{"exemption", MaskRegion{Mode: maskModeNone,
			AppliesTo: &MaskScope{SOPInstanceUID: "1.2.3"}}, ""},
		{"scoped rectangle", MaskRegion{Mode: maskModeRect, W: 1, H: 0.1,
			AppliesTo: &MaskScope{Modality: "US", Cols: 800, Rows: 600}}, ""},
		{"bad usregion token", MaskRegion{Mode: maskModeRect, W: 1, H: 0.1,
			AppliesTo: &MaskScope{USRegion: "maybe"}}, "must be declared or absent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMaskRegion(tc.r)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("rejected a valid region: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("accepted an invalid region, want error containing %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// Fractions must round outward: leaving half a row of text behind is the
// failure this whole feature exists to prevent.
func TestFractionRectRoundsOutward(t *testing.T) {
	cases := []struct {
		name       string
		r          MaskRegion
		cols, rows int
		want       pixelRect
	}{
		{"exact", MaskRegion{W: 0.5, H: 0.5}, 100, 100, pixelRect{0, 0, 50, 50}},
		{"start floors, end ceils", MaskRegion{X: 0.101, Y: 0.101, W: 0.4, H: 0.4}, 100, 100,
			pixelRect{10, 10, 51, 51}},
		{"sub-pixel band still covers a row", MaskRegion{W: 1, H: 0.001}, 100, 100, pixelRect{0, 0, 100, 1}},
		{"whole image", MaskRegion{W: 1, H: 1}, 37, 19, pixelRect{0, 0, 37, 19}},
		{"clamped to the frame", MaskRegion{X: 0.5, Y: 0.5, W: 0.5, H: 0.5}, 3, 3, pixelRect{1, 1, 3, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fractionRect(tc.r, tc.cols, tc.rows); got != tc.want {
				t.Errorf("fractionRect = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The core property: inside the rectangle is fill, outside is untouched.
func TestApplyPixelMaskMonochrome(t *testing.T) {
	ds, nf := maskTestDataset(t, 10, 10, 1, "MONOCHROME2")
	before := slices.Clone(nf.RawData)

	masked, err := applyPixelMask(ds, []MaskRegion{{Mode: maskModeRect, X: 0, Y: 0, W: 1, H: 0.2}}, newMaskSource(ds))
	if err != nil {
		t.Fatalf("applyPixelMask: %v", err)
	}
	if !masked.masked {
		t.Fatal("reported no pixels masked")
	}
	for y := range 10 {
		for x := range 10 {
			got := sampleAt(nf.RawData, 10, 1, x, y, 0)
			if y < 2 {
				if got != 0 {
					t.Fatalf("pixel (%d,%d) = %d, want it blanked to 0", x, y, got)
				}
				continue
			}
			if want := sampleAt(before, 10, 1, x, y, 0); got != want {
				t.Fatalf("pixel (%d,%d) = %d, want it untouched at %d", x, y, got, want)
			}
		}
	}
}

// MONOCHROME1 displays inverted, so its black is the maximum stored value. A
// constant zero fill would paint a white box that reads as intact annotation.
func TestApplyPixelMaskMonochrome1FillsWhiteValue(t *testing.T) {
	ds, nf := maskTestDataset(t, 4, 4, 1, "MONOCHROME1")
	if _, err := applyPixelMask(ds, []MaskRegion{{W: 1, H: 0.25}}, newMaskSource(ds)); err != nil {
		t.Fatalf("applyPixelMask: %v", err)
	}
	for x := range 4 {
		if got := sampleAt(nf.RawData, 4, 1, x, 0, 0); got != 255 {
			t.Errorf("pixel (%d,0) = %d, want 255 (black under MONOCHROME1)", x, got)
		}
	}
}

func TestApplyPixelMaskRGBInterleaved(t *testing.T) {
	ds, nf := maskTestDataset(t, 4, 4, 3, "RGB")
	before := slices.Clone(nf.RawData)

	if _, err := applyPixelMask(ds, []MaskRegion{{X: 0.5, Y: 0, W: 0.5, H: 0.5}}, newMaskSource(ds)); err != nil {
		t.Fatalf("applyPixelMask: %v", err)
	}
	for y := range 4 {
		for x := range 4 {
			for s := range 3 {
				got := sampleAt(nf.RawData, 4, 3, x, y, s)
				if x >= 2 && y < 2 {
					if got != 0 {
						t.Fatalf("sample (%d,%d,%d) = %d, want 0", x, y, s, got)
					}
					continue
				}
				if want := sampleAt(before, 4, 3, x, y, s); got != want {
					t.Fatalf("sample (%d,%d,%d) = %d, want untouched %d", x, y, s, got, want)
				}
			}
		}
	}
}

// Planar data is stored plane-major, and the parsing library fills its raw
// slice straight from the stream — so masking must index by plane or it shifts
// colour across the image instead of blanking it.
func TestApplyPixelMaskRGBPlanar(t *testing.T) {
	ds, nf := maskTestDataset(t, 4, 4, 3, "RGB",
		mustTestElement(t, tag.PlanarConfiguration, []int{1}))
	before := slices.Clone(nf.RawData)

	if _, err := applyPixelMask(ds, []MaskRegion{{W: 1, H: 0.25}}, newMaskSource(ds)); err != nil {
		t.Fatalf("applyPixelMask: %v", err)
	}
	plane := 4 * 4
	for s := range 3 {
		for x := range 4 {
			if got := nf.RawData[s*plane+x]; got != 0 {
				t.Fatalf("plane %d row 0 col %d = %d, want 0", s, x, got)
			}
		}
		// Row 1 of each plane must be untouched — the bug this guards against
		// blanks a third of every plane instead of the first row of each.
		for x := range 4 {
			idx := s*plane + 4 + x
			if got, want := nf.RawData[idx], before[idx]; got != want {
				t.Fatalf("plane %d row 1 col %d = %d, want untouched %d", s, x, got, want)
			}
		}
	}
}

// Every frame of a multi-frame clip must be masked: an echo loop hides the
// banner on all 53 frames, not just the one on screen.
func TestApplyPixelMaskAllFrames(t *testing.T) {
	frames := make([]*frame.Frame, 0, 3)
	natives := make([]*frame.NativeFrame[uint8], 0, 3)
	for range 3 {
		nf := frame.NewNativeFrame[uint8](8, 4, 4, 16, 1)
		for i := range nf.RawData {
			nf.RawData[i] = 200
		}
		natives = append(natives, nf)
		frames = append(frames, &frame.Frame{Encapsulated: false, NativeData: nf})
	}
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{Frames: frames})
	if err != nil {
		t.Fatalf("NewElement(PixelData): %v", err)
	}
	ds := &sdicom.Dataset{Elements: []*sdicom.Element{
		mustTestElement(t, tag.PhotometricInterpretation, []string{"MONOCHROME2"}),
		mustTestElement(t, tag.Rows, []int{4}),
		mustTestElement(t, tag.Columns, []int{4}),
		mustTestElement(t, tag.BitsAllocated, []int{8}),
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.SamplesPerPixel, []int{1}),
		mustTestElement(t, tag.NumberOfFrames, []string{"3"}),
		pd,
	}}

	if _, err := applyPixelMask(ds, []MaskRegion{{W: 1, H: 0.25}}, newMaskSource(ds)); err != nil {
		t.Fatalf("applyPixelMask: %v", err)
	}
	for i, nf := range natives {
		for x := range 4 {
			if got := nf.RawData[x]; got != 0 {
				t.Errorf("frame %d pixel (%d,0) = %d, want 0", i+1, x, got)
			}
		}
		if got := nf.RawData[4]; got != 200 {
			t.Errorf("frame %d row 1 = %d, want it untouched at 200", i+1, got)
		}
	}
}

// A file with no pixel data (a report, a key-object selection) is not a
// masking failure — there is nothing burned in to remove.
func TestApplyPixelMaskWithoutPixelDataIsNoOp(t *testing.T) {
	ds := &sdicom.Dataset{Elements: []*sdicom.Element{
		mustTestElement(t, tag.Modality, []string{"SR"}),
	}}
	masked, err := applyPixelMask(ds, []MaskRegion{{W: 1, H: 0.1}}, newMaskSource(ds))
	if err != nil {
		t.Fatalf("applyPixelMask: %v", err)
	}
	if masked.masked {
		t.Error("reported pixels masked in a dataset with no pixel data")
	}
}

// Still-compressed pixels must be refused rather than silently skipped: the
// caller is responsible for decompressing, and a quiet no-op here would export
// the banner intact.
func TestApplyPixelMaskRejectsEncapsulated(t *testing.T) {
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		IsEncapsulated: true,
		Frames: []*frame.Frame{{Encapsulated: true,
			EncapsulatedData: frame.EncapsulatedFrame{Data: []byte{0xFF, 0xD8}}}},
	})
	if err != nil {
		t.Fatalf("NewElement(PixelData): %v", err)
	}
	ds := &sdicom.Dataset{Elements: []*sdicom.Element{pd}}
	if _, err := applyPixelMask(ds, []MaskRegion{{W: 1, H: 0.1}}, newMaskSource(ds)); err == nil {
		t.Fatal("masked encapsulated pixel data without complaint")
	}
}

// Chroma-subsampled data has no one-triple-per-pixel layout to index, so it is
// refused with an instruction rather than smeared.
func TestApplyPixelMaskRejectsSubsampledChroma(t *testing.T) {
	ds, _ := maskTestDataset(t, 4, 4, 3, "YBR_FULL_422")
	_, err := applyPixelMask(ds, []MaskRegion{{W: 1, H: 0.25}}, newMaskSource(ds))
	if err == nil {
		t.Fatal("masked chroma-subsampled pixel data without complaint")
	}
	if !strings.Contains(err.Error(), "output transfer syntax") {
		t.Errorf("error = %q, want it to say how to proceed", err)
	}
}

// usRegionElement builds a calibration sequence stating one region's pixel
// bounds — the attributes a real echo file carries.
func usRegionElement(t *testing.T, minX, minY, maxX, maxY int) *sdicom.Element {
	t.Helper()
	e, err := sdicom.NewElement(tag.SequenceOfUltrasoundRegions, [][]*sdicom.Element{{
		mustTestElement(t, tag.RegionLocationMinX0, []int{minX}),
		mustTestElement(t, tag.RegionLocationMinY0, []int{minY}),
		mustTestElement(t, tag.RegionLocationMaxX1, []int{maxX}),
		mustTestElement(t, tag.RegionLocationMaxY1, []int{maxY}),
		mustTestElement(t, tag.RegionDataType, []int{1}),
	}})
	if err != nil {
		t.Fatalf("NewElement(SequenceOfUltrasoundRegions): %v", err)
	}
	return e
}

func TestUltrasoundMaskKeepsTheImageRegion(t *testing.T) {
	// A 10×10 image whose calibrated region is the 6×6 block at (2,2)–(7,7):
	// everything outside it is the banner area.
	ds, nf := maskTestDataset(t, 10, 10, 1, "MONOCHROME2",
		mustTestElement(t, tag.Modality, []string{"US"}),
		usRegionElement(t, 2, 2, 7, 7))
	before := slices.Clone(nf.RawData)

	masked, err := applyPixelMask(ds, []MaskRegion{{Mode: maskModeOutsideUS}}, newMaskSource(ds))
	if err != nil {
		t.Fatalf("applyPixelMask: %v", err)
	}
	if !masked.masked {
		t.Fatal("reported no pixels masked")
	}
	for y := range 10 {
		for x := range 10 {
			inside := x >= 2 && x <= 7 && y >= 2 && y <= 7
			got := sampleAt(nf.RawData, 10, 1, x, y, 0)
			if inside {
				if want := sampleAt(before, 10, 1, x, y, 0); got != want {
					t.Fatalf("image pixel (%d,%d) = %d, want untouched %d", x, y, got, want)
				}
				continue
			}
			if got != 0 {
				t.Fatalf("banner pixel (%d,%d) = %d, want 0", x, y, got)
			}
		}
	}
}

// Two calibrated panes (a duplex study) mask to their bounding box, so the gap
// between them keeps its image content.
func TestUltrasoundMaskUsesBoundingBoxOfAllRegions(t *testing.T) {
	seq, err := sdicom.NewElement(tag.SequenceOfUltrasoundRegions, [][]*sdicom.Element{
		{
			mustTestElement(t, tag.RegionLocationMinX0, []int{0}),
			mustTestElement(t, tag.RegionLocationMinY0, []int{4}),
			mustTestElement(t, tag.RegionLocationMaxX1, []int{3}),
			mustTestElement(t, tag.RegionLocationMaxY1, []int{9}),
		},
		{
			mustTestElement(t, tag.RegionLocationMinX0, []int{6}),
			mustTestElement(t, tag.RegionLocationMinY0, []int{4}),
			mustTestElement(t, tag.RegionLocationMaxX1, []int{9}),
			mustTestElement(t, tag.RegionLocationMaxY1, []int{9}),
		},
	})
	if err != nil {
		t.Fatalf("NewElement(SequenceOfUltrasoundRegions): %v", err)
	}
	ds, nf := maskTestDataset(t, 10, 10, 1, "MONOCHROME2",
		mustTestElement(t, tag.Modality, []string{"US"}), seq)
	before := slices.Clone(nf.RawData)

	if _, err := applyPixelMask(ds, []MaskRegion{{Mode: maskModeOutsideUS}}, newMaskSource(ds)); err != nil {
		t.Fatalf("applyPixelMask: %v", err)
	}
	// The 4-pixel gap between the panes is inside the bounding box and keeps
	// its content; the banner above the panes is gone.
	for x := 4; x < 6; x++ {
		idx := 5*10 + x
		if got, want := nf.RawData[idx], before[idx]; got != want {
			t.Errorf("gap pixel (%d,5) = %d, want untouched %d", x, got, want)
		}
	}
	for x := range 10 {
		if got := nf.RawData[x]; got != 0 {
			t.Errorf("banner pixel (%d,0) = %d, want 0", x, got)
		}
	}
}

// An ultrasound file that declares no region cannot have the rule applied, and
// must fail rather than export unmasked. A non-ultrasound file in the same run
// is simply not what the rule describes.
func TestUltrasoundMaskWithoutRegions(t *testing.T) {
	t.Run("ultrasound fails", func(t *testing.T) {
		ds, _ := maskTestDataset(t, 4, 4, 1, "MONOCHROME2",
			mustTestElement(t, tag.Modality, []string{"US"}))
		if _, err := applyPixelMask(ds, []MaskRegion{{Mode: maskModeOutsideUS}}, newMaskSource(ds)); err == nil {
			t.Fatal("exported an ultrasound image with no calibrated region and no mask")
		}
	})
	t.Run("other modality is inert", func(t *testing.T) {
		ds, nf := maskTestDataset(t, 4, 4, 1, "MONOCHROME2",
			mustTestElement(t, tag.Modality, []string{"CT"}))
		before := slices.Clone(nf.RawData)
		masked, err := applyPixelMask(ds, []MaskRegion{{Mode: maskModeOutsideUS}}, newMaskSource(ds))
		if err != nil {
			t.Fatalf("applyPixelMask: %v", err)
		}
		if masked.masked || !slices.Equal(nf.RawData, before) {
			t.Error("an ultrasound rule masked a CT image")
		}
	})
}

// An ultrasound image with no calibrated region is usually an analysis or
// measurement screen — content worth exporting — so the profile's manual
// rectangles stand in for the rule it could not resolve, and the substitution
// is reported rather than assumed.
func TestUltrasoundMaskFallsBackToManualRectangles(t *testing.T) {
	ds, nf := maskTestDataset(t, 10, 10, 1, "MONOCHROME2",
		mustTestElement(t, tag.Modality, []string{"US"}))
	before := slices.Clone(nf.RawData)

	out, err := applyPixelMask(ds, []MaskRegion{
		{Mode: maskModeOutsideUS},
		{Mode: maskModeRect, W: 1, H: 0.2},
	}, newMaskSource(ds))
	if err != nil {
		t.Fatalf("applyPixelMask: %v", err)
	}
	if !out.masked || !out.usFellBack {
		t.Fatalf("outcome = %+v, want masked with the fallback reported", out)
	}
	// The rectangle masked the banner; the measurements below it survive.
	for x := range 10 {
		if got := sampleAt(nf.RawData, 10, 1, x, 1, 0); got != 0 {
			t.Fatalf("banner pixel (%d,1) = %d, want 0", x, got)
		}
		idx := 5*10 + x
		if got, want := nf.RawData[idx], before[idx]; got != want {
			t.Fatalf("analysis pixel (%d,5) = %d, want untouched %d", x, got, want)
		}
	}

	// A calibrated file uses its own geometry and reports no fallback.
	calibrated, _ := maskTestDataset(t, 10, 10, 1, "MONOCHROME2",
		mustTestElement(t, tag.Modality, []string{"US"}),
		usRegionElement(t, 2, 2, 7, 7))
	out, err = applyPixelMask(calibrated, []MaskRegion{
		{Mode: maskModeOutsideUS},
		{Mode: maskModeRect, W: 1, H: 0.2},
	}, newMaskSource(calibrated))
	if err != nil {
		t.Fatalf("applyPixelMask: %v", err)
	}
	if out.usFellBack {
		t.Error("reported a fallback for a file that states its own region")
	}
}

// The fallback is counted per run, because a file masked by generic geometry
// carries a weaker guarantee than one masked by its own stated layout.
func TestRunModificationReportsUltrasoundFallback(t *testing.T) {
	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath := filepath.Join(rootDir, "us.dcm")
	writeModifyTestDICOM(t, srcPath)
	ds, err := sdicom.ParseFile(srcPath, nil)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if err := setElementValue(&ds, tag.Modality, []string{"US"}); err != nil {
		t.Fatalf("set modality: %v", err)
	}
	f, err := os.Create(srcPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f.Close()

	params, err := compileModifyParams(ModProfile{MaskRegions: []MaskRegion{
		{Mode: maskModeOutsideUS},
		{Mode: maskModeRect, W: 1, H: 0.5},
	}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Processed != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v (%v), want the file exported", res, res.Failures)
	}
	if res.MaskUSFallback != 1 {
		t.Errorf("MaskUSFallback = %d, want 1", res.MaskUSFallback)
	}
	outDS, err := sdicom.ParseFile(filepath.Join(outDir, "us.dcm"), nil)
	if err != nil {
		t.Fatalf("parse export: %v", err)
	}
	if got, want := modifyTestPixels(t, &outDS), []uint8{0, 0, 30, 40}; !slices.Equal(got, want) {
		t.Errorf("exported pixels = %v, want the rectangle applied %v", got, want)
	}
}

// Scope is what stops a rectangle drawn on one analysis screen from blanking
// the report content of the ten beside it — the defect that made scoping
// necessary in the first place.
func TestMaskRegionScopeLimitsWhichImagesAreMasked(t *testing.T) {
	// Two ultrasound images: same modality, different sizes, different UIDs.
	screen, screenFrame := maskTestDataset(t, 10, 10, 1, "MONOCHROME2",
		mustTestElement(t, tag.Modality, []string{"US"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.5"}))
	other, otherFrame := maskTestDataset(t, 20, 10, 1, "MONOCHROME2",
		mustTestElement(t, tag.Modality, []string{"US"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.6"}))
	otherBefore := slices.Clone(otherFrame.RawData)

	// The profile a review session produces: the ultrasound rule it started
	// with, plus one rectangle drawn on one screen.
	regions := []MaskRegion{
		{Mode: maskModeOutsideUS},
		{Mode: maskModeRect, W: 1, H: 0.2, AppliesTo: &MaskScope{SOPInstanceUID: "1.2.3.4.5"}},
	}

	if _, err := applyPixelMask(screen, regions, newMaskSource(screen)); err != nil {
		t.Fatalf("applyPixelMask(screen): %v", err)
	}
	if got := sampleAt(screenFrame.RawData, 10, 1, 0, 0, 0); got != 0 {
		t.Errorf("the named image was not masked: pixel (0,0) = %d", got)
	}

	// The other image is not the one the rectangle was drawn on. It must not be
	// masked by it — and since it states no region of its own and nothing
	// applies to it, it fails rather than exporting unmasked.
	out, err := applyPixelMask(other, regions, newMaskSource(other))
	if err == nil {
		t.Fatal("an ultrasound image with no applicable rectangle was exported")
	}
	if out.masked || !slices.Equal(otherFrame.RawData, otherBefore) {
		t.Error("a region scoped to another image masked this one")
	}
}

// "Current Series/Chapter" reaches the images of one modality and size within
// one series, and stops at both boundaries.
func TestMaskRegionSeriesSizeScope(t *testing.T) {
	group := &MaskScope{Series: "1.2.3.1", Modality: "US", Cols: 10, Rows: 10, USRegion: usRegionAbsent}
	region := MaskRegion{Mode: maskModeRect, W: 1, H: 0.2, AppliesTo: group}

	inGroup, inFrame := maskTestDataset(t, 10, 10, 1, "MONOCHROME2",
		mustTestElement(t, tag.Modality, []string{"US"}),
		mustTestElement(t, tag.SeriesInstanceUID, []string{"1.2.3.1"}))
	if _, err := applyPixelMask(inGroup, []MaskRegion{region}, newMaskSource(inGroup)); err != nil {
		t.Fatalf("applyPixelMask: %v", err)
	}
	if got := sampleAt(inFrame.RawData, 10, 1, 0, 0, 0); got != 0 {
		t.Errorf("an image of the group was not masked: pixel (0,0) = %d", got)
	}

	// Same modality, size and calibration, but a different series — the
	// cross-series bleed this scope exists to stop.
	otherSeries, otherFrame := maskTestDataset(t, 10, 10, 1, "MONOCHROME2",
		mustTestElement(t, tag.Modality, []string{"US"}),
		mustTestElement(t, tag.SeriesInstanceUID, []string{"1.2.3.2"}))
	otherBefore := slices.Clone(otherFrame.RawData)
	if _, err := applyPixelMask(otherSeries, []MaskRegion{region}, newMaskSource(otherSeries)); err != nil {
		t.Fatalf("applyPixelMask(otherSeries): %v", err)
	}
	if !slices.Equal(otherFrame.RawData, otherBefore) {
		t.Error("a region scoped to one series masked an image of another series")
	}

	// Same series and size, but it states its own region: a different group
	// in the review window, and a different group here.
	calibrated, calFrame := maskTestDataset(t, 10, 10, 1, "MONOCHROME2",
		mustTestElement(t, tag.Modality, []string{"US"}),
		mustTestElement(t, tag.SeriesInstanceUID, []string{"1.2.3.1"}),
		usRegionElement(t, 2, 2, 7, 7))
	before := slices.Clone(calFrame.RawData)
	if _, err := applyPixelMask(calibrated, []MaskRegion{region}, newMaskSource(calibrated)); err != nil {
		t.Fatalf("applyPixelMask(calibrated): %v", err)
	}
	if !slices.Equal(calFrame.RawData, before) {
		t.Error("a region scoped to the uncalibrated group masked a calibrated image")
	}

	// A different size, same series, is a different group.
	bigger, bigFrame := maskTestDataset(t, 20, 10, 1, "MONOCHROME2",
		mustTestElement(t, tag.Modality, []string{"CT"}),
		mustTestElement(t, tag.SeriesInstanceUID, []string{"1.2.3.1"}))
	bigBefore := slices.Clone(bigFrame.RawData)
	if _, err := applyPixelMask(bigger, []MaskRegion{region}, newMaskSource(bigger)); err != nil {
		t.Fatalf("applyPixelMask(bigger): %v", err)
	}
	if !slices.Equal(bigFrame.RawData, bigBefore) {
		t.Error("a group-scoped region reached an image of another group")
	}
}

// MaskScope.matches directly, for the Series field: empty matches any series,
// and a stated one narrows exactly like the neighbouring fields it composes
// with.
func TestMaskScopeMatchesSeries(t *testing.T) {
	src := maskSource{seriesInstanceUID: "1.2.3.1", modality: "US"}
	cases := []struct {
		name string
		s    *MaskScope
		want bool
	}{
		{"no series stated matches anything", &MaskScope{Modality: "US"}, true},
		{"matching series", &MaskScope{Series: "1.2.3.1"}, true},
		{"different series", &MaskScope{Series: "1.2.3.2"}, false},
		{"matching series and modality", &MaskScope{Series: "1.2.3.1", Modality: "US"}, true},
		{"matching series, wrong modality", &MaskScope{Series: "1.2.3.1", Modality: "CT"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.matches(src, 800, 600); got != tc.want {
				t.Errorf("matches = %v, want %v", got, tc.want)
			}
		})
	}
}

// An image reviewed and found to need no masking exports rather than failing —
// the ten screens beside the one that carried the banner.
func TestMaskModeNoneExemptsAnImage(t *testing.T) {
	ds, nf := maskTestDataset(t, 10, 10, 1, "MONOCHROME2",
		mustTestElement(t, tag.Modality, []string{"US"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.7"}))
	before := slices.Clone(nf.RawData)

	regions := []MaskRegion{
		{Mode: maskModeOutsideUS},
		{Mode: maskModeNone, AppliesTo: &MaskScope{SOPInstanceUID: "1.2.3.4.7"}},
	}
	out, err := applyPixelMask(ds, regions, newMaskSource(ds))
	if err != nil {
		t.Fatalf("an exempted image still failed: %v", err)
	}
	if out.masked || !slices.Equal(nf.RawData, before) {
		t.Error("an exempted image was masked anyway")
	}
	if out.usFellBack {
		t.Error("an exemption reported as a fallback")
	}

	// The exemption is scoped, so it does not excuse the next image.
	otherDS, _ := maskTestDataset(t, 10, 10, 1, "MONOCHROME2",
		mustTestElement(t, tag.Modality, []string{"US"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.8"}))
	if _, err := applyPixelMask(otherDS, regions, newMaskSource(otherDS)); err == nil {
		t.Error("one image's exemption excused another")
	}
}

// Regression: a scoped region must survive the steps that run before masking.
//
// Masking is the last step in processFile, and UID remapping — which the
// shipped base-deident profile turns on — rewrites SOP Instance UID before it.
// Resolving the scope against the transformed dataset therefore matched
// nothing: every rectangle drawn on a named image silently stopped applying,
// and an ultrasound image left with no rectangle failed and vanished from the
// export. Nothing in the engine noticed, because a scope that matches nothing
// is indistinguishable from a region that was never meant for this file.
func TestScopedMaskSurvivesUIDRemapping(t *testing.T) {
	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath := filepath.Join(rootDir, "us.dcm")
	writeModifyTestDICOM(t, srcPath)

	// The fixture as the review window would have seen it: an ultrasound image
	// declaring no calibrated region, identified by its SOP Instance UID.
	ds, err := sdicom.ParseFile(srcPath, nil)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if err := setElementValue(&ds, tag.Modality, []string{"US"}); err != nil {
		t.Fatalf("set modality: %v", err)
	}
	sourceUID := strings.TrimSpace(datasetFirstString(&ds, tag.SOPInstanceUID))
	if sourceUID == "" {
		t.Fatal("fixture has no SOP Instance UID to scope to")
	}
	f, err := os.Create(srcPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f.Close()

	for _, tc := range []struct {
		name    string
		profile ModProfile
	}{
		{"remap UIDs", ModProfile{RemapUIDs: true}},
		// A removal rule can delete the attributes a scope keys on, which is
		// the same failure by another route.
		{"modality removed", ModProfile{Removes: []string{"0008,0060"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(outDir, tc.name)
			p := tc.profile
			p.MaskRegions = []MaskRegion{
				{Mode: maskModeOutsideUS},
				{Mode: maskModeRect, W: 1, H: 0.5, AppliesTo: &MaskScope{SOPInstanceUID: sourceUID}},
			}
			params, err := compileModifyParams(p)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}

			res := runModification(context.Background(), []string{srcPath}, rootDir, out, params, nil, nil)
			if res.Processed != 1 || res.Failed != 0 {
				t.Fatalf("result = %+v (%v), want the file exported", res, res.Failures)
			}
			outDS, perr := sdicom.ParseFile(filepath.Join(out, "us.dcm"), nil)
			if perr != nil {
				t.Fatalf("parse export: %v", perr)
			}
			if got, want := modifyTestPixels(t, &outDS), []uint8{0, 0, 30, 40}; !slices.Equal(got, want) {
				t.Errorf("exported pixels = %v, want the scoped rectangle applied %v", got, want)
			}
		})
	}
}

// The same hazard for an exemption: it is keyed on the source image too, and a
// remapped UID must not turn "reviewed, needs no masking" back into a failure.
func TestExemptionSurvivesUIDRemapping(t *testing.T) {
	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath := filepath.Join(rootDir, "us.dcm")
	writeModifyTestDICOM(t, srcPath)
	ds, err := sdicom.ParseFile(srcPath, nil)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if err := setElementValue(&ds, tag.Modality, []string{"US"}); err != nil {
		t.Fatalf("set modality: %v", err)
	}
	sourceUID := strings.TrimSpace(datasetFirstString(&ds, tag.SOPInstanceUID))
	f, err := os.Create(srcPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f.Close()

	params, err := compileModifyParams(ModProfile{
		RemapUIDs: true,
		MaskRegions: []MaskRegion{
			{Mode: maskModeOutsideUS},
			{Mode: maskModeNone, AppliesTo: &MaskScope{SOPInstanceUID: sourceUID}},
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Processed != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v (%v), want the exempted file exported", res, res.Failures)
	}
	outDS, err := sdicom.ParseFile(filepath.Join(outDir, "us.dcm"), nil)
	if err != nil {
		t.Fatalf("parse export: %v", err)
	}
	if got, want := modifyTestPixels(t, &outDS), []uint8{10, 20, 30, 40}; !slices.Equal(got, want) {
		t.Errorf("exported pixels = %v, want them untouched %v", got, want)
	}
}

func TestCompileModifyParamsMaskValidation(t *testing.T) {
	cases := []struct {
		name string
		p    ModProfile
		want string
	}{
		{"mask alone is actionable", ModProfile{MaskRegions: []MaskRegion{{W: 1, H: 0.1}}}, ""},
		{"bad mode reported", ModProfile{MaskRegions: []MaskRegion{{Mode: "rows", H: 0.1}}}, "must be rect"},
		{"bad geometry reported", ModProfile{MaskRegions: []MaskRegion{{W: 2, H: 0.1}}}, "between 0 and 1"},
		{"index in message", ModProfile{MaskRegions: []MaskRegion{
			{W: 1, H: 0.1}, {Mode: "nonsense"}}}, "mask region 2"},
		{"per-modality validated too", ModProfile{PerModality: map[string]ModProfile{
			"US": {MaskRegions: []MaskRegion{{Mode: "nonsense"}}}}}, "modality US"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compileModifyParams(tc.p)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("rejected a valid profile: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("accepted an invalid profile, want error containing %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A per-modality block's regions replace the profile's rather than adding to
// them: geometry describes one modality's screen layout.
func TestMaskRegionsReplaceOnMerge(t *testing.T) {
	base := ModProfile{MaskRegions: []MaskRegion{{W: 1, H: 0.5}}}
	override := ModProfile{MaskRegions: []MaskRegion{{Mode: maskModeOutsideUS}}}

	merged := mergeModProfiles(base, override)
	if len(merged.MaskRegions) != 1 || maskRegionMode(merged.MaskRegions[0]) != maskModeOutsideUS {
		t.Fatalf("merged regions = %+v, want only the override's", merged.MaskRegions)
	}
	// A profile that states none keeps its base's.
	kept := mergeModProfiles(base, ModProfile{})
	if len(kept.MaskRegions) != 1 || kept.MaskRegions[0].H != 0.5 {
		t.Errorf("regions = %+v, want the base's preserved", kept.MaskRegions)
	}
}

// End to end: the exported file's pixels are masked and the source is not.
func TestRunModificationMasksPixels(t *testing.T) {
	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath := filepath.Join(rootDir, "img.dcm")
	writeModifyTestDICOM(t, srcPath)

	// The fixture is 2×2, so half the image is the top row.
	params, err := compileModifyParams(ModProfile{
		Sets:        []string{"0010,0010=ANON"},
		MaskRegions: []MaskRegion{{Mode: maskModeRect, W: 1, H: 0.5}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v (%v), want 1 processed 0 failed", res, res.Failures)
	}
	if res.MaskDecompressed != 0 {
		t.Errorf("MaskDecompressed = %d, want 0 for native input", res.MaskDecompressed)
	}

	outDS, err := sdicom.ParseFile(filepath.Join(outDir, "img.dcm"), nil)
	if err != nil {
		t.Fatalf("parse export: %v", err)
	}
	// writeModifyTestDICOM stores the ramp 10,20,30,40.
	if got, want := modifyTestPixels(t, &outDS), []uint8{0, 0, 30, 40}; !slices.Equal(got, want) {
		t.Errorf("exported pixels = %v, want %v", got, want)
	}

	srcDS, err := sdicom.ParseFile(srcPath, nil)
	if err != nil {
		t.Fatalf("parse source: %v", err)
	}
	if got, want := modifyTestPixels(t, &srcDS), []uint8{10, 20, 30, 40}; !slices.Equal(got, want) {
		t.Errorf("source pixels = %v, want them untouched at %v", got, want)
	}
}

// A file that cannot be masked fails rather than being exported intact — the
// invariant the whole feature rests on.
func TestRunModificationFailsFileThatCannotBeMasked(t *testing.T) {
	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath := filepath.Join(rootDir, "us.dcm")
	writeModifyTestDICOM(t, srcPath)
	// Re-label the fixture as ultrasound without a calibration sequence, which
	// is precisely the case the ultrasound rule cannot resolve.
	ds, err := sdicom.ParseFile(srcPath, nil)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if err := setElementValue(&ds, tag.Modality, []string{"US"}); err != nil {
		t.Fatalf("set modality: %v", err)
	}
	f, err := os.Create(srcPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f.Close()

	params, err := compileModifyParams(ModProfile{MaskRegions: []MaskRegion{{Mode: maskModeOutsideUS}}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Processed != 0 || res.Failed != 1 {
		t.Fatalf("result = %+v, want 0 processed 1 failed", res)
	}
	if _, err := os.Stat(filepath.Join(outDir, "us.dcm")); !os.IsNotExist(err) {
		t.Error("an unmaskable file was written to the export")
	}
	if len(res.Failures) != 1 || !strings.Contains(res.Failures[0].Error, "pixel masking") {
		t.Errorf("failures = %+v, want one naming pixel masking", res.Failures)
	}
}
