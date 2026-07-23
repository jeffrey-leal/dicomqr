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
		mustTestElement(t, tag.TransferSyntaxUID, []string{tsJPEG2000LL}),
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
	if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write J2K test DICOM: %v", err)
	}
	return path
}

// ramp8Pixels returns the expected decoded pixels of ramp8.j2k: every row is
// the horizontal ramp 0,32,64,…,224.
func ramp8Pixels() []uint8 {
	px := make([]uint8, 64)
	for row := 0; row < 8; row++ {
		for col := 0; col < 8; col++ {
			px[row*8+col] = uint8(col * 32)
		}
	}
	return px
}

// J2K Lossless decodes bit-exactly straight to either required uncompressed
// target syntax.
func TestTranscodeJ2KToTargets(t *testing.T) {
	for _, target := range []string{tsExplicitVRLE, tsImplicitVRLE} {
		t.Run(transferSyntaxLabel(target), func(t *testing.T) {
			path := writeJ2KTestDICOM(t, t.TempDir())
			if got := fileTransferSyntaxUID(path); got != tsJPEG2000LL {
				t.Fatalf("fixture transfer syntax = %q, want %q", got, tsJPEG2000LL)
			}

			changed, err := transcodeDICOMFile(path, target)
			if err != nil {
				t.Fatalf("transcodeDICOMFile: %v", err)
			}
			if !changed {
				t.Fatal("J2K file must be rewritten")
			}
			if got := fileTransferSyntaxUID(path); got != target {
				t.Fatalf("transfer syntax after transcode = %q, want %q", got, target)
			}

			want := ramp8Pixels()
			got := dicomPixels(t, path)
			if len(got) != len(want) {
				t.Fatalf("pixel count = %d, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("pixel[%d] = %d, want %d (lossless decode must be bit-exact)", i, got[i], want[i])
				}
			}

			// Idempotent: a second pass is a no-op.
			changed, err = transcodeDICOMFile(path, target)
			if err != nil {
				t.Fatalf("transcodeDICOMFile(again): %v", err)
			}
			if changed {
				t.Error("already-converted file must not be rewritten")
			}
		})
	}
}
