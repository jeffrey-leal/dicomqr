package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

const (
	sopCTImage = "1.2.840.10008.5.1.4.1.1.2"
	sopSC      = "1.2.840.10008.5.1.4.1.1.7"
	sopPDF     = "1.2.840.10008.5.1.4.1.1.104.1"
)

// writePHIFixture writes a small file of the given class and modality. A
// non-empty photometric gives it 4×4 8-bit pixel data; extra elements are
// added as given. Elements are written in ascending tag order, as the
// standard requires and the header reader relies on.
func writePHIFixture(t *testing.T, path, sopClass, modality, photometric string, extra ...*sdicom.Element) {
	t.Helper()
	uid := "1.2.3." + strings.TrimSuffix(filepath.Base(path), ".dcm")
	elems := []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{sopClass}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{uid}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{tsExplicitVRLE}),
		mustTestElement(t, tag.SOPClassUID, []string{sopClass}),
		mustTestElement(t, tag.SOPInstanceUID, []string{uid}),
		mustTestElement(t, tag.Modality, []string{modality}),
		mustTestElement(t, tag.PatientName, []string{"DOE^JANE"}),
		mustTestElement(t, tag.PatientID, []string{"PID123"}),
		mustTestElement(t, tag.StudyInstanceUID, []string{"1.2.3.4"}),
		mustTestElement(t, tag.SeriesInstanceUID, []string{"1.2.3.4.1"}),
	}
	if photometric != "" {
		samples := 1
		if photometric == "RGB" {
			samples = 3
		}
		nf := frame.NewNativeFrame[uint8](8, 4, 4, 16*samples, samples)
		pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
			Frames: []*frame.Frame{{Encapsulated: false, NativeData: nf}},
		})
		if err != nil {
			t.Fatalf("NewElement(PixelData): %v", err)
		}
		elems = append(elems,
			mustTestElement(t, tag.PhotometricInterpretation, []string{photometric}),
			mustTestElement(t, tag.SamplesPerPixel, []int{samples}),
			mustTestElement(t, tag.Rows, []int{4}),
			mustTestElement(t, tag.Columns, []int{4}),
			mustTestElement(t, tag.BitsAllocated, []int{8}),
			mustTestElement(t, tag.BitsStored, []int{8}),
			mustTestElement(t, tag.HighBit, []int{7}),
			mustTestElement(t, tag.PixelRepresentation, []int{0}),
			pd,
		)
		if samples == 3 {
			elems = append(elems, mustTestElement(t, tag.PlanarConfiguration, []int{0}))
		}
	}
	elems = append(elems, extra...)
	ds := sdicom.Dataset{Elements: elems}
	sortElementsByTag(&ds)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// The classification itself: which files the screen flags, for which reason,
// and what a run's masking and overlay removal clear.
func TestPHIRiskClassification(t *testing.T) {
	img := func(modality, sop, photometric string) phiHeader {
		return phiHeader{sopClass: sop, photometric: photometric, cols: 100, rows: 80,
			src: maskSource{modality: modality}}
	}
	whole := []MaskRegion{{X: 0, Y: 0, W: 0.1, H: 0.1}}
	reviewed := []MaskRegion{{Mode: maskModeNone}}
	usRule := []MaskRegion{{Mode: maskModeOutsideUS}}

	burned := img("CT", sopCTImage, "MONOCHROME2")
	burned.burnedIn = "YES"
	burnedUS := img("US", sopSC, "RGB")
	burnedUS.burnedIn = "YES"
	doc := phiHeader{sopClass: sopPDF, document: true, src: maskSource{modality: "OT"}}
	overlayCT := img("CT", sopCTImage, "MONOCHROME2")
	overlayCT.overlay = true

	for _, tc := range []struct {
		name     string
		h        phiHeader
		regions  []MaskRegion
		overlays bool // Remove overlay planes
		want     phiRisk
	}{
		{"plain CT slice", img("CT", sopCTImage, "MONOCHROME2"), nil, false, 0},
		{"Secondary Capture labelled CT", img("CT", sopSC, "MONOCHROME2"), nil, false, phiRiskCapture},
		{"colour image in a greyscale modality", img("CT", sopCTImage, "RGB"), nil, false, phiRiskCapture},
		{"colour endoscopy is not a capture", img("ES", "1.2.840.10008.5.1.4.1.1.77.1.1", "RGB"), nil, false, 0},
		{"palette colour counts as colour", img("NM", "1.2.840.10008.5.1.4.1.1.20", "PALETTE COLOR"), nil, false, phiRiskCapture},
		{"capture masked", img("CT", sopSC, "MONOCHROME2"), whole, false, 0},
		{"capture reviewed as needing none", img("CT", sopSC, "MONOCHROME2"), reviewed, false, 0},
		{"ultrasound unmasked", img("US", "1.2.840.10008.5.1.4.1.1.6.1", "RGB"), nil, false, phiRiskUltrasound},
		// A US rule with nothing to resolve against fails the file in the
		// export — it never ships, so it is not a leak.
		{"ultrasound the run would fail", img("US", "1.2.840.10008.5.1.4.1.1.6.1", "RGB"), usRule, false, 0},
		{"declared burned-in annotation", burned, nil, false, phiRiskBurnedIn},
		{"burned-in outranks ultrasound", burnedUS, nil, false, phiRiskBurnedIn},
		{"burned-in masked", burned, whole, false, 0},
		{"document", doc, nil, false, phiRiskDocument},
		{"masking does not reach a document", doc, whole, false, phiRiskDocument},
		{"overlays kept", overlayCT, nil, false, phiRiskOverlay},
		{"overlays removed", overlayCT, nil, true, 0},
		{"no dimensions, no pixel reason", phiHeader{sopClass: sopSC, src: maskSource{modality: "CT"}}, nil, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.h.risks(tc.regions, tc.overlays); got != tc.want {
				t.Errorf("risks = %05b, want %05b", got, tc.want)
			}
		})
	}
}

// Every reason has both a finding line and a shipped clause — a reason added
// to phiRiskKinds without text would put an empty bullet in the dialog.
func TestPHIRiskTextCoversEveryKind(t *testing.T) {
	for _, k := range phiRiskKinds {
		if k.finding(2) == "" || k.shipped(2) == "" || k.finding(1) == "" || k.shipped(1) == "" {
			t.Errorf("risk %05b has no text", k)
		}
	}
	if got := phiRiskSummary(nil); got != "" {
		t.Errorf("summary with nothing shipped = %q, want empty", got)
	}
	got := phiRiskSummary(map[phiRisk]int{phiRiskOverlay: 3, phiRiskBurnedIn: 1})
	if !strings.Contains(got, "1 exported image declares") || !strings.Contains(got, "3 exported files keep") ||
		strings.Index(got, "burned-in") > strings.Index(got, "overlay") {
		t.Errorf("summary = %q, want both clauses, strongest first", got)
	}
}

// The screen reads headers, applies the profile's own filters (so it never
// flags a file the run skips), counts what it could not read, and keeps the
// order it was given.
func TestScreenPHIFiles(t *testing.T) {
	dir := t.TempDir()
	path := func(n string) string { return filepath.Join(dir, n) }
	writePHIFixture(t, path("a-ct.dcm"), sopCTImage, "CT", "MONOCHROME2")
	writePHIFixture(t, path("b-sc.dcm"), sopSC, "CT", "MONOCHROME2",
		mustTestElement(t, tag.BurnedInAnnotation, []string{"YES"}))
	writePHIFixture(t, path("c-pdf.dcm"), sopPDF, "OT", "",
		mustTestElement(t, tag.EncapsulatedDocument, []byte("%PDF-1.4 fake")))
	writePHIFixture(t, path("d-sr.dcm"), "1.2.840.10008.5.1.4.1.1.88.11", "SR", "")
	if err := os.WriteFile(path("e-junk.dcm"), []byte("not dicom"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []string{path("a-ct.dcm"), path("b-sc.dcm"), path("c-pdf.dcm"), path("d-sr.dcm"), path("e-junk.dcm")}

	filters, err := compileModifyParams(ModProfile{IgnoreModalities: []string{"SR"}})
	if err != nil {
		t.Fatal(err)
	}
	screened, unreadable := screenPHIFiles(files, filters, nil, nil)
	if unreadable != 1 {
		t.Errorf("unreadable = %d, want 1", unreadable)
	}
	var got, skipped []string
	for _, f := range screened {
		got = append(got, filepath.Base(f.path))
		if f.skipped {
			skipped = append(skipped, filepath.Base(f.path))
		}
	}
	if want := []string{"a-ct.dcm", "b-sc.dcm", "c-pdf.dcm", "d-sr.dcm"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("screened = %v, want %v (junk unreadable, order kept)", got, want)
	}
	if want := []string{"d-sr.dcm"}; !reflect.DeepEqual(skipped, want) {
		t.Fatalf("skipped = %v, want %v (the profile's filter)", skipped, want)
	}
	if screened[1].burnedIn != "YES" || !screened[2].document || screened[2].isImage() {
		t.Errorf("header facts not read: %+v / %+v", screened[1].phiHeader, screened[2].phiHeader)
	}

	none := func(string) []MaskRegion { return nil }
	f := evaluatePHIScreen(screened, none, false)
	if len(f.byRisk[phiRiskBurnedIn]) != 1 || len(f.byRisk[phiRiskDocument]) != 1 || len(f.byRisk[phiRiskCapture]) != 0 {
		t.Errorf("findings = %v", f.byRisk)
	}
	if pix := f.pixelFiles(); len(pix) != 1 || filepath.Base(pix[0]) != "b-sc.dcm" {
		t.Errorf("pixelFiles = %v, want just the burned-in capture", pix)
	}
	// Masking whatever governs CT clears the pixel finding; the document stays.
	masked := func(m string) []MaskRegion {
		if m == "CT" {
			return []MaskRegion{{X: 0, Y: 0, W: 0.5, H: 0.25}}
		}
		return nil
	}
	f = evaluatePHIScreen(screened, masked, false)
	if len(f.pixelFiles()) != 0 || len(f.byRisk[phiRiskDocument]) != 1 {
		t.Errorf("after masking, findings = %v", f.byRisk)
	}
	if left := withoutFiles(files, f.byRisk[phiRiskDocument]); len(left) != 4 || filepath.Base(left[2]) != "d-sr.dcm" {
		t.Errorf("withoutFiles = %v", left)
	}
}

// The review window opens on the flagged files' whole series — a series-wide
// rectangle's reach depends on every file of the series, skipped ones
// included — and a flagged file with no series comes alone.
func TestReviewSetForTakesWholeSeries(t *testing.T) {
	f := func(path, series string, skipped bool) phiScreenFile {
		return phiScreenFile{path: path, skipped: skipped,
			phiHeader: phiHeader{src: maskSource{seriesInstanceUID: series}}}
	}
	screened := []phiScreenFile{
		f("s1-a", "1.1", false), f("s2-a", "1.2", false), f("s1-b", "1.1", true),
		f("s2-b", "1.2", false), f("lone", "", false), f("other-lone", "", false),
	}
	got := reviewSetFor(screened, []string{"s1-b", "lone"})
	if want := []string{"s1-a", "s1-b", "lone"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reviewSetFor = %v, want %v", got, want)
	}
}

// The evidence half: the run counts what the written files still carried,
// so the completion summary is a fact about the export. A removal rule that
// takes the document out, and masking that covers the image, clear their
// counts; a file the run fails or skips is never counted.
func TestModificationCountsShippedPHIRisks(t *testing.T) {
	rootDir := t.TempDir()
	sc := filepath.Join(rootDir, "sc.dcm")
	pdf := filepath.Join(rootDir, "pdf.dcm")
	ct := filepath.Join(rootDir, "ct.dcm")
	writePHIFixture(t, sc, sopSC, "CT", "MONOCHROME2")
	writePHIFixture(t, pdf, sopPDF, "OT", "",
		mustTestElement(t, tag.EncapsulatedDocument, []byte("%PDF-1.4 fake")))
	writePHIFixture(t, ct, sopCTImage, "CT", "MONOCHROME2",
		mustTestElement(t, tag.Tag{Group: 0x6000, Element: 0x0010}, []int{4}))
	files := []string{sc, pdf, ct}

	for _, tc := range []struct {
		name    string
		profile ModProfile
		want    map[phiRisk]int
	}{
		{"nothing done about any of it", ModProfile{Sets: []string{"0010,0010=ANON"}},
			map[phiRisk]int{phiRiskCapture: 1, phiRiskDocument: 1, phiRiskOverlay: 1}},
		{"masked, document removed, overlays removed", ModProfile{
			MaskRegions: []MaskRegion{{X: 0, Y: 0, W: 0.5, H: 0.5}},
			Removes:     []string{"0042,0011"},
			NoOverlays:  true,
		}, nil},
		{"capture skipped by SOP class", ModProfile{Sets: []string{"0010,0010=ANON"}, IgnoreSOPClasses: []string{sopSC}},
			map[phiRisk]int{phiRiskDocument: 1, phiRiskOverlay: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params, err := compileModifyParams(tc.profile)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			res := runModification(context.Background(), files, rootDir, t.TempDir(), params, nil, nil)
			if res.Failed != 0 {
				t.Fatalf("failures: %v", res.Failures)
			}
			if !reflect.DeepEqual(res.PHIRisks, tc.want) {
				t.Errorf("PHIRisks = %v, want %v", res.PHIRisks, tc.want)
			}
		})
	}
}
