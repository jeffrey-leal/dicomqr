package main

// Regression tests for the study-preview freeze on multiframe files (NM/SPECT
// projection data). loadDicomImage used to drain the parser's frame channel
// only after ParseFile returned, but the parser sends each frame with a
// blocking send during the parse — so any file with more frames than the
// channel buffer (8) deadlocked the load. NM/SPECT stores the whole
// acquisition as one 40-240-frame file, freezing the study preview mid-load;
// verified against a real SPECT/CT study before the fix. The error path had
// the same latent hang: the library closes the channel only on success, so
// draining after a mid-parse error blocked forever.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// loadWithTimeout fails the test instead of hanging it when loadDicomImage
// never returns.
func loadWithTimeout(t *testing.T, path string) (viewerState, error) {
	t.Helper()
	var (
		vs   viewerState
		lerr error
	)
	done := make(chan struct{})
	go func() {
		vs, lerr = loadDicomImage(path)
		close(done)
	}()
	select {
	case <-done:
		return vs, lerr
	case <-time.After(30 * time.Second):
		t.Fatal("loadDicomImage deadlocked (frame channel not drained concurrently)")
		return viewerState{}, nil
	}
}

func TestLoadDicomImageManyFrames(t *testing.T) {
	const nFrames = 24 // well past the 8-slot frame channel buffer
	frames := make([]*frame.Frame, nFrames)
	for i := range frames {
		nf := frame.NewNativeFrame[uint8](8, 2, 2, 4, 1)
		copy(nf.RawData, []uint8{10, 20, 30, byte(40 + i)})
		frames[i] = &frame.Frame{Encapsulated: false, NativeData: nf}
	}
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		IsEncapsulated: false,
		Frames:         frames,
	})
	if err != nil {
		t.Fatalf("NewElement(PixelData): %v", err)
	}
	elems := []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.20"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.6"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{tsExplicitVRLE}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.20"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.6"}),
		mustTestElement(t, tag.Modality, []string{"NM"}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{"MONOCHROME2"}),
		mustTestElement(t, tag.NumberOfFrames, []string{"24"}),
		mustTestElement(t, tag.Rows, []int{2}),
		mustTestElement(t, tag.Columns, []int{2}),
		mustTestElement(t, tag.BitsAllocated, []int{8}),
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.HighBit, []int{7}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		mustTestElement(t, tag.SamplesPerPixel, []int{1}),
		pd,
	}

	path := filepath.Join(t.TempDir(), "multiframe.dcm")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	writeErr := sdicom.Write(f, sdicom.Dataset{Elements: elems},
		sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification())
	f.Close()
	if writeErr != nil {
		t.Fatalf("write multiframe test DICOM: %v", writeErr)
	}

	vs, lerr := loadWithTimeout(t, path)
	if lerr != nil {
		t.Fatalf("loadDicomImage: %v", lerr)
	}
	if vs.img == nil {
		t.Error("loadDicomImage returned no image for multiframe file")
	}
}

func TestLoadDicomImageParseErrorNoHang(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.dcm")
	if err := os.WriteFile(path, []byte("this is not a DICOM file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, lerr := loadWithTimeout(t, path); lerr == nil {
		t.Error("loadDicomImage succeeded on a corrupt file, want error")
	}
}
