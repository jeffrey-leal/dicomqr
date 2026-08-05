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
	_, aliases := embeddedModConfigs(t)

	for _, target := range []struct {
		pref string
		uid  string
	}{
		{tsPrefExplicitLE, tsExplicitVRLE},
		{tsPrefImplicitLE, tsImplicitVRLE},
	} {
		t.Run(transferSyntaxLabel(target.uid), func(t *testing.T) {
			params, err := compileModifyParams(ModProfile{
				Sets:           []string{"PatientName=ANON"},
				TransferSyntax: target.pref,
			}, aliases)
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
	_, aliases := embeddedModConfigs(t)
	params, err := compileModifyParams(ModProfile{Sets: []string{"PatientName=ANON"}}, aliases)
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
