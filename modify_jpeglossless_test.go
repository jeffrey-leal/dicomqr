//go:build jpeglossless

package main

// JPEG Lossless is decompressible for export only: a modification profile can
// convert a .57/.70 file that is already on disk, but the syntax is never
// negotiated (TestAcceptedSyntaxesFor asserts that half). Without this, a study
// the viewer displays perfectly could not be exported at all.

import (
	"context"
	"path/filepath"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

func TestRunModificationConvertsJPEGLossless(t *testing.T) {
	params, err := compileModifyParams(ModProfile{TransferSyntax: tsPrefExplicitLE})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
	// The fixture splits one codestream across two fragments, so this also
	// covers fragment reassembly on the conversion path.
	srcPath := writeJPEGLosslessTestDICOM(t, rootDir)

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v (%v), want 1 processed 0 failed", res, res.Failures)
	}

	outPath := filepath.Join(outDir, filepath.Base(srcPath))
	if got := fileTransferSyntaxUID(outPath); got != tsExplicitVRLE {
		t.Errorf("exported transfer syntax = %q, want %q", got, tsExplicitVRLE)
	}
	if got := fileTransferSyntaxUID(srcPath); got != tsJPEGLosslessSV1 {
		t.Errorf("source transfer syntax = %q, want it untouched at %q", got, tsJPEGLosslessSV1)
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
	if !ok {
		t.Fatalf("unexpected PixelData value type %T", pdElem.Value.GetValue())
	}
	if info.IsEncapsulated {
		t.Fatal("exported pixel data is still encapsulated")
	}

	// Lossless means bit-exact: the decoder emits interleaved RGB, so pixel
	// (x,y) occupies three consecutive samples.
	got := modifyTestPixels(t, &ds)
	if len(got) != 8*8*3 {
		t.Fatalf("sample count = %d, want %d", len(got), 8*8*3)
	}
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			i := (y*8 + x) * 3
			wantR, wantG, wantB := uint8(x*32), uint8(y*32), uint8((x+y)*16)
			if got[i] != wantR || got[i+1] != wantG || got[i+2] != wantB {
				t.Fatalf("pixel (%d,%d) = R%d G%d B%d, want R%d G%d B%d",
					x, y, got[i], got[i+1], got[i+2], wantR, wantG, wantB)
			}
		}
	}
}
