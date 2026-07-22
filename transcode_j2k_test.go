//go:build openjpeg

package main

import (
	"os"
	"path/filepath"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
	tagpkg "github.com/suyashkumar/dicom/pkg/tag"
)

// writeJ2KTestDICOM writes an 8×8 8-bit monochrome file whose pixel data is
// the embedded lossless ramp8.j2k codestream, encapsulated under the JPEG 2000
// Lossless transfer syntax.
func writeJ2KTestDICOM(t *testing.T, dir string) string {
	t.Helper()
	// DICOM fragments must have even length (PS3.5 §A.4); pad the codestream
	// with a trailing zero byte like a real sender would.
	codestream := append([]byte(nil), ramp8J2K...)
	if len(codestream)%2 != 0 {
		codestream = append(codestream, 0)
	}
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		IsEncapsulated: true,
		Frames: []*frame.Frame{{
			Encapsulated:     true,
			EncapsulatedData: frame.EncapsulatedFrame{Data: codestream},
		}},
	})
	if err != nil {
		t.Fatalf("NewElement(PixelData): %v", err)
	}
	// Encapsulated pixel data is written with undefined length and VR OB.
	pd.ValueLength = tagpkg.VLUndefinedLength
	pd.RawValueRepresentation = "OB"

	ds := sdicom.Dataset{Elements: []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.6"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{"1.2.840.10008.1.2.4.90"}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.6"}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{"MONOCHROME2"}),
		mustTestElement(t, tag.Rows, []int{8}),
		mustTestElement(t, tag.Columns, []int{8}),
		mustTestElement(t, tag.BitsAllocated, []int{8}),
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.HighBit, []int{7}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		mustTestElement(t, tag.SamplesPerPixel, []int{1}),
		mustTestElement(t, tag.NumberOfFrames, []string{"1"}),
		pd,
	}}

	path := filepath.Join(dir, "j2k.dcm")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, ds); err != nil {
		t.Fatalf("write J2K test DICOM: %v", err)
	}
	return path
}

func TestTranscodeJPEG2000ToExplicitLE(t *testing.T) {
	path := writeJ2KTestDICOM(t, t.TempDir())

	if got := fileTransferSyntaxUID(path); got != "1.2.840.10008.1.2.4.90" {
		t.Fatalf("precondition: transfer syntax = %q", got)
	}

	changed, err := transcodeDICOMFile(path)
	if err != nil {
		t.Fatalf("transcodeDICOMFile: %v", err)
	}
	if !changed {
		t.Fatal("J2K file should have been rewritten")
	}

	// Second run must be a no-op.
	changed, err = transcodeDICOMFile(path)
	if err != nil {
		t.Fatalf("transcodeDICOMFile(again): %v", err)
	}
	if changed {
		t.Error("second transcode must be a no-op")
	}

	if got := fileTransferSyntaxUID(path); got != tsExplicitVRLE {
		t.Fatalf("after transcode: transfer syntax = %q, want %q", got, tsExplicitVRLE)
	}

	// The decompressed pixels must be the exact lossless ramp.
	ds, err := sdicom.ParseFile(path, nil)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	pdElem, err := ds.FindElementByTag(tag.PixelData)
	if err != nil {
		t.Fatalf("PixelData missing: %v", err)
	}
	info := pdElem.Value.GetValue().(sdicom.PixelDataInfo)
	if info.IsEncapsulated || len(info.Frames) != 1 {
		t.Fatalf("expected 1 native frame, got encapsulated=%v frames=%d", info.IsEncapsulated, len(info.Frames))
	}
	nf, err := info.Frames[0].GetNativeFrame()
	if err != nil {
		t.Fatalf("GetNativeFrame: %v", err)
	}
	if nf.Rows() != 8 || nf.Cols() != 8 {
		t.Fatalf("frame %dx%d, want 8x8", nf.Rows(), nf.Cols())
	}
	raw, ok := nf.RawDataSlice().([]uint8)
	if !ok {
		t.Fatalf("RawDataSlice type %T, want []uint8", nf.RawDataSlice())
	}
	want := []uint8{0, 32, 64, 96, 128, 160, 192, 224}
	for x := 0; x < 8; x++ {
		if raw[x] != want[x] {
			t.Errorf("row0[%d] = %d, want %d", x, raw[x], want[x])
		}
	}
}
