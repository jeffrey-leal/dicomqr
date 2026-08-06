//go:build openjpeg

package main

// End-to-end coverage of a modification profile converting compressed input.
// Until the transfer-syntax option existed nothing exercised the modification
// engine on encapsulated pixel data at all — every other fixture is native —
// so this is the only test that proves the parse/transform/decompress/write
// chain survives a real codestream.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

func TestRunModificationConvertsJ2K(t *testing.T) {

	for _, target := range []struct {
		pref string
		uid  string
	}{
		{tsPrefExplicitLE, tsExplicitVRLE},
		{tsPrefImplicitLE, tsImplicitVRLE},
	} {
		t.Run(transferSyntaxLabel(target.uid), func(t *testing.T) {
			params, err := compileModifyParams(ModProfile{
				Sets:           []string{"0010,0010=ANON"},
				TransferSyntax: target.pref,
			})
			if err != nil {
				t.Fatalf("compile: %v", err)
			}

			rootDir, outDir := t.TempDir(), t.TempDir()
			srcPath := writeJ2KTestDICOM(t, rootDir)

			res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
			if res.Failed != 0 || res.Processed != 1 {
				t.Fatalf("result = %+v (%v), want 1 processed 0 failed", res, res.Failures)
			}

			outPath := filepath.Join(outDir, filepath.Base(srcPath))
			if got := fileTransferSyntaxUID(outPath); got != target.uid {
				t.Errorf("exported transfer syntax = %q, want %q", got, target.uid)
			}
			// The source keeps its compression: conversion happens on the way
			// out, never in the download folder.
			if got := fileTransferSyntaxUID(srcPath); got != tsJPEG2000LL {
				t.Errorf("source transfer syntax = %q, want it untouched at %q", got, tsJPEG2000LL)
			}

			ds, err := sdicom.ParseFile(outPath, nil)
			if err != nil {
				t.Fatalf("parse export: %v", err)
			}
			// De-identification and decompression both landed.
			e, err := ds.FindElementByTag(tag.PatientName)
			if err != nil {
				t.Fatalf("PatientName missing: %v", err)
			}
			if got := strings.TrimSpace(sdicom.MustGetStrings(e.Value)[0]); got != "ANON" {
				t.Errorf("PatientName = %q, want ANON", got)
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
			if got := modifyTestPixels(t, &ds); !slices.Equal(got, ramp8Pixels()) {
				t.Errorf("decoded pixels = %v, want the lossless ramp %v", got, ramp8Pixels())
			}
		})
	}
}

// Leaving the syntax as stored must not disturb compressed input either: the
// export is byte-identical to a plain copy apart from the tag edits, and the
// pixel data stays encapsulated.
func TestRunModificationKeepsJ2KWhenAsStored(t *testing.T) {
	params, err := compileModifyParams(ModProfile{Sets: []string{"0010,0010=ANON"}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath := writeJ2KTestDICOM(t, rootDir)

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v (%v), want 1 processed 0 failed", res, res.Failures)
	}

	outPath := filepath.Join(outDir, filepath.Base(srcPath))
	if got := fileTransferSyntaxUID(outPath); got != tsJPEG2000LL {
		t.Errorf("exported transfer syntax = %q, want it left at %q", got, tsJPEG2000LL)
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
	if !ok || !info.IsEncapsulated {
		t.Fatalf("exported pixel data is no longer encapsulated (ok=%v)", ok)
	}
	if _, err := os.Stat(srcPath); err != nil {
		t.Errorf("source disturbed: %v", err)
	}
}

// Masking compressed input. There are no encoders, so a masked file can only
// leave uncompressed: the run decompresses it even though the profile asked for
// no conversion, and reports that in MaskDecompressed rather than changing the
// export's encoding silently. This is the only test that drives the
// decompress-then-mask chain with a real codestream.
func TestRunModificationMasksJ2KByDecompressing(t *testing.T) {
	params, err := compileModifyParams(ModProfile{
		MaskRegions: []MaskRegion{{Mode: maskModeRect, W: 1, H: 0.25}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath := writeJ2KTestDICOM(t, rootDir)

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v (%v), want 1 processed 0 failed", res, res.Failures)
	}
	if res.MaskDecompressed != 1 {
		t.Errorf("MaskDecompressed = %d, want 1 — the encoding change must be reported", res.MaskDecompressed)
	}

	outPath := filepath.Join(outDir, filepath.Base(srcPath))
	if got := fileTransferSyntaxUID(outPath); got != tsExplicitVRLE {
		t.Errorf("exported transfer syntax = %q, want %q", got, tsExplicitVRLE)
	}
	// The source keeps its compression: masking is an export-time operation.
	if got := fileTransferSyntaxUID(srcPath); got != tsJPEG2000LL {
		t.Errorf("source transfer syntax = %q, want it untouched at %q", got, tsJPEG2000LL)
	}

	ds, err := sdicom.ParseFile(outPath, nil)
	if err != nil {
		t.Fatalf("parse export: %v", err)
	}
	// The 8×8 ramp with its top two rows blanked and the rest bit-exact.
	want := ramp8Pixels()
	for i := range 16 {
		want[i] = 0
	}
	if got := modifyTestPixels(t, &ds); !slices.Equal(got, want) {
		t.Errorf("exported pixels = %v, want the ramp with rows 0-1 masked %v", got, want)
	}
}

// A compressed file that no region applies to must keep its compression.
//
// Masking forces a decompression, and the engine used to force it on every
// compressed file the moment a profile carried any region at all — so a profile
// masking one analysis screen decompressed and re-encoded the entire study.
// Measured on a 177-file echo study: 23 of every 25 files decompressed for
// nothing, a tenfold export, and minutes of CPU spent reproducing pixels that
// were already on disk. Only the files a region actually resolves against are
// touched now, which is what keeps an export the size of its source.
func TestRunModificationLeavesUnmaskedFilesCompressed(t *testing.T) {
	rootDir, outDir := t.TempDir(), t.TempDir()

	// Three compressed files with distinct identities; the rectangle names one.
	uids := []string{"1.2.3.4.101", "1.2.3.4.102", "1.2.3.4.103"}
	paths := make([]string, len(uids))
	base := writeJ2KTestDICOM(t, rootDir)
	for i, uid := range uids {
		ds, err := sdicom.ParseFile(base, nil)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		if err := setElementValue(&ds, tag.SOPInstanceUID, []string{uid}); err != nil {
			t.Fatalf("set SOP Instance UID: %v", err)
		}
		paths[i] = filepath.Join(rootDir, uid+".dcm")
		f, err := os.Create(paths[i])
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		werr := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification())
		f.Close()
		if werr != nil {
			t.Fatalf("write fixture: %v", werr)
		}
	}
	if err := os.Remove(base); err != nil {
		t.Fatalf("remove base fixture: %v", err)
	}

	params, err := compileModifyParams(ModProfile{
		MaskRegions: []MaskRegion{
			{Mode: maskModeRect, W: 1, H: 0.25, AppliesTo: &MaskScope{SOPInstanceUID: uids[1]}},
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	res := runModification(context.Background(), paths, rootDir, outDir, params, nil, nil)
	if res.Processed != 3 || res.Failed != 0 {
		t.Fatalf("result = %+v (%v), want 3 processed", res, res.Failures)
	}
	if res.MaskDecompressed != 1 {
		t.Errorf("MaskDecompressed = %d, want 1 — only the named image needs decompressing",
			res.MaskDecompressed)
	}

	for i, uid := range uids {
		outPath := filepath.Join(outDir, uid+".dcm")
		got := fileTransferSyntaxUID(outPath)
		want := tsJPEG2000LL
		if i == 1 {
			want = tsExplicitVRLE // the masked one had to be written out uncompressed
		}
		if got != want {
			t.Errorf("%s exported as %s, want %s", uid, transferSyntaxLabel(got), transferSyntaxLabel(want))
		}
	}
}
