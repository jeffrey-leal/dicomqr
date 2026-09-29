package main

import (
	"os"
	"path/filepath"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// wrongVR builds an element whose value has the type the tag's VR would never
// produce — what a file storing a tag under an unexpected VR parses into.
func wrongVR(t *testing.T, tg tag.Tag, vr string, v any) *sdicom.Element {
	t.Helper()
	val, err := sdicom.NewValue(v)
	if err != nil {
		t.Fatalf("NewValue(%v): %v", v, err)
	}
	return &sdicom.Element{Tag: tg, RawValueRepresentation: vr, Value: val}
}

// Every metadata reader the viewer runs after a parse used sdicom.MustGet*,
// which panics on a value of the wrong type — outside the parse boundary's
// recover and, for the clip buffer, filmstrip and slice loader, on a worker
// goroutine where that ends the process. Each must now read such a value as
// absent and fall back to its default.
func TestViewerMetadataReadersSurviveUnexpectedVRs(t *testing.T) {
	ds := sdicom.Dataset{Elements: []*sdicom.Element{
		wrongVR(t, tag.PatientName, "US", []int{7}),
		wrongVR(t, tag.PatientID, "US", []int{7}),
		wrongVR(t, tag.Modality, "US", []int{1}),
		wrongVR(t, tag.PixelSpacing, "US", []int{1, 2}),
		wrongVR(t, tag.ImageOrientationPatient, "US", []int{1, 0, 0, 0, 1, 0}),
		wrongVR(t, tag.WindowCenter, "US", []int{40}),
		wrongVR(t, tag.WindowWidth, "US", []int{400}),
		wrongVR(t, tag.RescaleSlope, "US", []int{1}),
		wrongVR(t, tag.RescaleIntercept, "US", []int{-1024}),
		wrongVR(t, tag.PhotometricInterpretation, "US", []int{2}),
		wrongVR(t, tag.TransferSyntaxUID, "US", []int{1}),
		// Integers stored as text: now read, where they used to panic.
		wrongVR(t, tag.BitsAllocated, "CS", []string{"8"}),
		wrongVR(t, tag.PixelRepresentation, "CS", []string{"1"}),
		wrongVR(t, tag.Rows, "CS", []string{"not a number"}),
	}}

	ann := extractAnnotationsFromDataset(ds)
	if ann.patientName != "" || ann.modality != "" || ann.pixelSpacing != "" || ann.orientTop != "" {
		t.Errorf("wrongly typed values produced annotations: %+v", ann)
	}
	if _, _, ok := dicomWindowParams(ds); ok {
		t.Error("window read from wrongly typed values")
	}
	if slope, intercept := dicomRescaleParams(ds); slope != 1 || intercept != 0 {
		t.Errorf("rescale = %v/%v, want the 1/0 default", slope, intercept)
	}
	if got := dicomPhotometricInterp(ds); got != "" {
		t.Errorf("photometric = %q, want empty", got)
	}
	if got := dicomBitsAllocated(ds); got != 8 {
		t.Errorf("BitsAllocated stored as text = %d, want 8", got)
	}
	if got := dicomPixelRepresentation(ds); got != 1 {
		t.Errorf("PixelRepresentation stored as text = %d, want 1", got)
	}
	if got := dicomIntParam(ds, tag.Rows); got != 0 {
		t.Errorf("unparsable Rows = %d, want 0", got)
	}
}

// End to end, through parseDicomFile — the path every viewer worker takes.
//
// A tag only the viewer reads (patient name, window) written under an
// unexpected VR must no longer panic after the parse: the file opens and
// decodes, the odd values simply absent. A tag the parsing library itself
// needs is a different matter: with BitsAllocated stored as text the library
// panics inside its own native-pixel reader — its MustGetInts, not ours — and
// the parse boundary (safeParseFile) turns that into an error. The file then
// fails to open, cleanly, rather than taking the process with it.
func TestParseDicomFileSurvivesUnexpectedVRs(t *testing.T) {
	t.Run("viewer-only tags", func(t *testing.T) {
		st := decodeOddFile(t, mustTestElement(t, tag.BitsAllocated, []int{8}))
		if got := st.frame.displayValues(); len(got) != 4 || got[3] != 40 {
			t.Errorf("decoded %v, want the four stored samples", got)
		}
	})
	t.Run("a tag the library needs", func(t *testing.T) {
		path := writeOddFile(t, wrongVR(t, tag.BitsAllocated, "CS", []string{"8"}))
		if _, err := parseDicomFile(path); err == nil {
			t.Error("parse succeeded; expected the library's panic to surface as an error")
		}
	})
}

// decodeOddFile writes writeOddFile's fixture and decodes its frame.
func decodeOddFile(t *testing.T, bitsAllocated *sdicom.Element) viewerState {
	t.Helper()
	p, err := parseDicomFile(writeOddFile(t, bitsAllocated))
	if err != nil {
		t.Fatalf("parseDicomFile: %v", err)
	}
	st, err := p.frameState(0)
	if err != nil {
		t.Fatalf("frameState: %v", err)
	}
	return st
}

// writeOddFile writes a 2×2 image whose patient name and window are stored
// under the wrong VR, with the BitsAllocated element given.
func writeOddFile(t *testing.T, bitsAllocated *sdicom.Element) string {
	t.Helper()
	nf := frame.NewNativeFrame[uint8](8, 2, 2, 4, 1)
	copy(nf.RawData, []uint8{10, 20, 30, 40})
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		Frames: []*frame.Frame{{Encapsulated: false, NativeData: nf}},
	})
	if err != nil {
		t.Fatal(err)
	}
	elems := []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{tsExplicitVRLE}),
		wrongVR(t, tag.PatientName, "US", []int{7}),
		wrongVR(t, tag.WindowCenter, "US", []int{40}),
		wrongVR(t, tag.WindowWidth, "US", []int{400}),
		mustTestElement(t, tag.SamplesPerPixel, []int{1}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{"MONOCHROME2"}),
		mustTestElement(t, tag.Rows, []int{2}),
		mustTestElement(t, tag.Columns, []int{2}),
		bitsAllocated,
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.HighBit, []int{7}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		pd,
	}
	path := filepath.Join(t.TempDir(), "odd.dcm")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sdicom.Write(f, sdicom.Dataset{Elements: elems},
		sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	return path
}
