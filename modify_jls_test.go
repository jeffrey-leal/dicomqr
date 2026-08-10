//go:build jpeglossless

package main

// End-to-end coverage of the decompress-mask-recompress chain on JPEG
// Lossless input, for both of its transfer syntax UIDs — each must come back
// as itself. The fixture codestream is built with encodeJPEGLossless, which
// is fair game: the encoder/decoder pair is proven by its own round-trip
// test, and the decoder is the one field-validated against real Philips echo
// studies.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// writeJLSTestDICOM writes an 8×8 8-bit monochrome file whose pixel data is
// the ramp encoded as lossless SOF3, encapsulated under tsUID (.57 or .70).
func writeJLSTestDICOM(t *testing.T, dir, tsUID string) string {
	t.Helper()
	ramp := ramp8PlanarInt32()
	codestream, err := encodeJPEGLossless(ramp, 8, 8, 1, 8)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
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
	pd.ValueLength = tag.VLUndefinedLength
	pd.RawValueRepresentation = "OB"

	ds := sdicom.Dataset{Elements: []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.9"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{tsUID}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.9"}),
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

	path := filepath.Join(dir, "jls.dcm")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write JLS test DICOM: %v", err)
	}
	return path
}

// ramp8PlanarInt32 is ramp8Pixels' pattern as the planar int32 layout the
// encoder takes — defined here rather than reusing ramp8Pixels because that
// helper lives behind the openjpeg tag.
func ramp8PlanarInt32() []int32 {
	px := make([]int32, 64)
	for row := 0; row < 8; row++ {
		for col := 0; col < 8; col++ {
			px[row*8+col] = int32(col * 32)
		}
	}
	return px
}

func TestRunModificationMasksJPEGLosslessAndRecompresses(t *testing.T) {
	for _, tsUID := range []string{tsJPEGLossless, tsJPEGLosslessSV1} {
		t.Run(transferSyntaxLabel(tsUID), func(t *testing.T) {
			params, err := compileModifyParams(ModProfile{
				MaskRegions: []MaskRegion{{Mode: maskModeRect, W: 1, H: 0.25}},
			})
			if err != nil {
				t.Fatalf("compile: %v", err)
			}

			rootDir, outDir := t.TempDir(), t.TempDir()
			srcPath := writeJLSTestDICOM(t, rootDir, tsUID)

			res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
			if res.Failed != 0 || res.Processed != 1 {
				t.Fatalf("result = %+v (%v), want 1 processed 0 failed", res, res.Failures)
			}
			if res.MaskRecompressed != 1 || res.MaskDecompressed != 0 {
				t.Errorf("MaskRecompressed = %d, MaskDecompressed = %d, want 1 and 0",
					res.MaskRecompressed, res.MaskDecompressed)
			}

			outPath := filepath.Join(outDir, filepath.Base(srcPath))
			// Each UID must come back as itself — .57 in means .57 out.
			if got := fileTransferSyntaxUID(outPath); got != tsUID {
				t.Errorf("exported transfer syntax = %q, want the source's %q", got, tsUID)
			}

			ds, err := sdicom.ParseFile(outPath, nil)
			if err != nil {
				t.Fatalf("parse export: %v", err)
			}
			pdElem, err := ds.FindElementByTag(tag.PixelData)
			if err != nil {
				t.Fatalf("PixelData missing: %v", err)
			}
			info, ok := pdElem.Value.GetValue().(sdicom.PixelDataInfo)
			if !ok || !info.IsEncapsulated || len(info.Frames) != 1 {
				t.Fatalf("exported pixel data is not one encapsulated frame (ok=%v)", ok)
			}
			w, h, nc, _, _, samples, err := decodeJPEGLossless(info.Frames[0].EncapsulatedData.Data)
			if err != nil {
				t.Fatalf("decode exported frame: %v", err)
			}
			if w != 8 || h != 8 || nc != 1 {
				t.Fatalf("frame decoded as %dx%d nc=%d, want 8x8 nc=1", w, h, nc)
			}
			want := ramp8PlanarInt32()
			for i := range 16 {
				want[i] = 0
			}
			if !slices.Equal(samples[:64], want) {
				t.Errorf("exported pixels = %v, want the ramp with rows 0-1 masked %v", samples[:64], want)
			}
		})
	}
}
