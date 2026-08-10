//go:build openjpeg

package main

// End-to-end coverage of a modification profile converting compressed input.
// Until the transfer-syntax option existed nothing exercised the modification
// engine on encapsulated pixel data at all — every other fixture is native —
// so this is the only test that proves the parse/transform/decompress/write
// chain survives a real codestream.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	color2 "image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
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

// maskedRamp8 is the 8×8 ramp with its top two rows blanked — what a
// full-width H=0.25 rectangle leaves behind.
func maskedRamp8() []uint8 {
	want := ramp8Pixels()
	for i := range 16 {
		want[i] = 0
	}
	return want
}

// decodeExportedJ2KFrames parses an export, requires it encapsulated, and
// decodes every frame with the same decoder the viewer uses.
func decodeExportedJ2KFrames(t *testing.T, path string) [][]uint8 {
	t.Helper()
	ds, err := sdicom.ParseFile(path, nil)
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
	if !info.IsEncapsulated {
		t.Fatal("exported pixel data is not encapsulated")
	}
	frames := make([][]uint8, 0, len(info.Frames))
	for i, fr := range info.Frames {
		w, h, nc, _, _, samples, err := decodeJPEG2000(fr.EncapsulatedData.Data)
		if err != nil {
			t.Fatalf("decode exported frame %d: %v", i+1, err)
		}
		if w != 8 || h != 8 || nc != 1 {
			t.Fatalf("frame %d decoded as %dx%d nc=%d, want 8x8 nc=1", i+1, w, h, nc)
		}
		px := make([]uint8, len(samples))
		for j, s := range samples {
			px[j] = uint8(s)
		}
		frames = append(frames, px)
	}
	return frames
}

// Masking compressed input whose syntax is losslessly re-encodable: the run
// decompresses, masks, and recompresses straight back into the source syntax
// (verified bit-identical by the engine itself), so the export keeps the
// encoding it arrived in. Reported in MaskRecompressed, with MaskDecompressed
// reserved for files that really do leave uncompressed. This is the test that
// drives the decompress-mask-recompress chain with a real codestream.
func TestRunModificationMasksJ2KAndRecompresses(t *testing.T) {
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
	if res.MaskRecompressed != 1 {
		t.Errorf("MaskRecompressed = %d, want 1 — the rewrite must be reported", res.MaskRecompressed)
	}
	if res.MaskDecompressed != 0 {
		t.Errorf("MaskDecompressed = %d, want 0 — the file did not leave uncompressed", res.MaskDecompressed)
	}

	outPath := filepath.Join(outDir, filepath.Base(srcPath))
	if got := fileTransferSyntaxUID(outPath); got != tsJPEG2000LL {
		t.Errorf("exported transfer syntax = %q, want the source's %q", got, tsJPEG2000LL)
	}
	// The source keeps its compression: masking is an export-time operation.
	if got := fileTransferSyntaxUID(srcPath); got != tsJPEG2000LL {
		t.Errorf("source transfer syntax = %q, want it untouched at %q", got, tsJPEG2000LL)
	}

	frames := decodeExportedJ2KFrames(t, outPath)
	if len(frames) != 1 {
		t.Fatalf("exported %d frames, want 1", len(frames))
	}
	// The 8×8 ramp with its top two rows blanked and the rest bit-exact.
	if !slices.Equal(frames[0], maskedRamp8()) {
		t.Errorf("exported pixels = %v, want the ramp with rows 0-1 masked %v", frames[0], maskedRamp8())
	}
}

// A multi-frame file recompresses frame by frame — one fragment per frame,
// every frame masked and every frame bit-exact outside the rectangle.
func TestRunModificationMasksMultiframeJ2KAndRecompresses(t *testing.T) {
	params, err := compileModifyParams(ModProfile{
		MaskRegions: []MaskRegion{{Mode: maskModeRect, W: 1, H: 0.25}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath := writeJ2KFixture(t, rootDir, j2kFixtureOpts{name: "j2k-mf.dcm", tsUID: tsJPEG2000LL, frames: 2})

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v (%v), want 1 processed 0 failed", res, res.Failures)
	}
	if res.MaskRecompressed != 1 || res.MaskDecompressed != 0 {
		t.Errorf("MaskRecompressed = %d, MaskDecompressed = %d, want 1 and 0",
			res.MaskRecompressed, res.MaskDecompressed)
	}

	outPath := filepath.Join(outDir, filepath.Base(srcPath))
	if got := fileTransferSyntaxUID(outPath); got != tsJPEG2000LL {
		t.Errorf("exported transfer syntax = %q, want the source's %q", got, tsJPEG2000LL)
	}
	frames := decodeExportedJ2KFrames(t, outPath)
	if len(frames) != 2 {
		t.Fatalf("exported %d frames, want 2", len(frames))
	}
	for i, px := range frames {
		if !slices.Equal(px, maskedRamp8()) {
			t.Errorf("frame %d pixels = %v, want the masked ramp %v", i+1, px, maskedRamp8())
		}
	}
}

// j2kFixtureOpts are writeJ2KFixture's departures from the standard
// single-frame lossless fixture writeJ2KTestDICOM builds.
type j2kFixtureOpts struct {
	name     string
	tsUID    string
	frames   int
	omitDims bool // drop Rows/Columns so the header gate cannot settle scopes
}

// writeJ2KFixture writes an 8×8 8-bit monochrome file carrying opts.frames
// copies of the ramp codestream, encapsulated under opts.tsUID.
func writeJ2KFixture(t *testing.T, dir string, opts j2kFixtureOpts) string {
	t.Helper()
	codestream := append([]byte(nil), ramp8J2K...)
	if len(codestream)%2 != 0 {
		codestream = append(codestream, 0)
	}
	frames := make([]*frame.Frame, 0, opts.frames)
	for range opts.frames {
		frames = append(frames, &frame.Frame{
			Encapsulated:     true,
			EncapsulatedData: frame.EncapsulatedFrame{Data: codestream},
		})
	}
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		IsEncapsulated: true,
		Frames:         frames,
	})
	if err != nil {
		t.Fatalf("NewElement(PixelData): %v", err)
	}
	pd.ValueLength = tag.VLUndefinedLength
	pd.RawValueRepresentation = "OB"

	elems := []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.7"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{opts.tsUID}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.7"}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{"MONOCHROME2"}),
	}
	if !opts.omitDims {
		elems = append(elems,
			mustTestElement(t, tag.Rows, []int{8}),
			mustTestElement(t, tag.Columns, []int{8}))
	}
	elems = append(elems,
		mustTestElement(t, tag.BitsAllocated, []int{8}),
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.HighBit, []int{7}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		mustTestElement(t, tag.SamplesPerPixel, []int{1}),
		mustTestElement(t, tag.NumberOfFrames, []string{fmt.Sprintf("%d", opts.frames)}),
		pd)

	ds := sdicom.Dataset{Elements: elems}
	path := filepath.Join(dir, opts.name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write J2K fixture: %v", err)
	}
	return path
}

// When recompression fails the file must still export — uncompressed, masked,
// and reported in MaskDecompressed exactly as before recompression existed.
// The seam stands in for a real encoder failure without a broken codec build.
func TestRunModificationMaskFallsBackWhenRecompressFails(t *testing.T) {
	orig := encodeMaskedFrame
	encodeMaskedFrame = func(string, []int32, int, int, int, int, bool, bool) ([]byte, error) {
		return nil, errors.New("forced encoder failure")
	}
	t.Cleanup(func() { encodeMaskedFrame = orig })

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
		t.Fatalf("result = %+v (%v), want 1 processed 0 failed — a failed recompression is a fallback, not a failure", res, res.Failures)
	}
	if res.MaskDecompressed != 1 || res.MaskRecompressed != 0 {
		t.Errorf("MaskDecompressed = %d, MaskRecompressed = %d, want 1 and 0",
			res.MaskDecompressed, res.MaskRecompressed)
	}

	outPath := filepath.Join(outDir, filepath.Base(srcPath))
	if got := fileTransferSyntaxUID(outPath); got != tsExplicitVRLE {
		t.Errorf("exported transfer syntax = %q, want the fallback %q", got, tsExplicitVRLE)
	}
	ds, err := sdicom.ParseFile(outPath, nil)
	if err != nil {
		t.Fatalf("parse export: %v", err)
	}
	if got := modifyTestPixels(t, &ds); !slices.Equal(got, maskedRamp8()) {
		t.Errorf("exported pixels = %v, want the masked ramp %v — the fallback must still be masked", got, maskedRamp8())
	}
}

// A file the generous header gate admits but the per-frame resolution finds
// nothing to mask on must leave with its original codestream bytes, not a
// decode/re-encode of them. The fixture has no Rows/Columns, so the header
// gate cannot settle the scope and admits it; the scope then matches nothing.
func TestRunModificationRestoresUnmaskedPixelsVerbatim(t *testing.T) {
	params, err := compileModifyParams(ModProfile{
		MaskRegions: []MaskRegion{
			{Mode: maskModeRect, W: 1, H: 0.25, AppliesTo: &MaskScope{SOPInstanceUID: "1.9.9.9"}},
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath := writeJ2KFixture(t, rootDir,
		j2kFixtureOpts{name: "j2k-noheader.dcm", tsUID: tsJPEG2000LL, frames: 1, omitDims: true})

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v (%v), want 1 processed 0 failed", res, res.Failures)
	}
	if res.MaskDecompressed != 0 || res.MaskRecompressed != 0 {
		t.Errorf("MaskDecompressed = %d, MaskRecompressed = %d, want 0 and 0 — nothing was masked",
			res.MaskDecompressed, res.MaskRecompressed)
	}

	outPath := filepath.Join(outDir, filepath.Base(srcPath))
	if got := fileTransferSyntaxUID(outPath); got != tsJPEG2000LL {
		t.Errorf("exported transfer syntax = %q, want %q", got, tsJPEG2000LL)
	}
	outFrame := exportedFrameData(t, outPath)
	srcFrame := exportedFrameData(t, srcPath)
	if !slices.Equal(outFrame, srcFrame) {
		t.Error("exported codestream differs from the source — an unmasked file must carry its original bytes")
	}
}

// A lossy source is never re-encoded lossily — a second lossy generation
// would degrade every pixel, not just the masked ones. Its masked frames are
// re-encoded to JPEG 2000 Lossless instead: no loss beyond the decode masking
// forced, and the syntax change carries its own count. The fixture's
// codestream happens to be reversible; the gate is the declared transfer
// syntax, which is all the engine can honestly go by.
func TestRunModificationRecodesLossyJ2KToLossless(t *testing.T) {
	params, err := compileModifyParams(ModProfile{
		MaskRegions: []MaskRegion{{Mode: maskModeRect, W: 1, H: 0.25}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath := writeJ2KFixture(t, rootDir, j2kFixtureOpts{name: "j2k-lossy.dcm", tsUID: tsJPEG2000, frames: 1})

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v (%v), want 1 processed 0 failed", res, res.Failures)
	}
	if res.MaskRecodedLossless != 1 || res.MaskRecompressed != 0 || res.MaskDecompressed != 0 {
		t.Errorf("MaskRecodedLossless = %d, MaskRecompressed = %d, MaskDecompressed = %d, want 1, 0, 0 — "+
			"a lossy source is re-encoded losslessly, never round-tripped or left uncompressed",
			res.MaskRecodedLossless, res.MaskRecompressed, res.MaskDecompressed)
	}

	outPath := filepath.Join(outDir, filepath.Base(srcPath))
	if got := fileTransferSyntaxUID(outPath); got != tsJPEG2000LL {
		t.Errorf("exported transfer syntax = %q, want %q", got, tsJPEG2000LL)
	}
	frames := decodeExportedJ2KFrames(t, outPath)
	if len(frames) != 1 || !slices.Equal(frames[0], maskedRamp8()) {
		t.Errorf("exported pixels = %v, want the masked ramp %v", frames, maskedRamp8())
	}
}

// JPEG Baseline input — the BRYAN echo case. Masked frames must come back as
// JPEG 2000 Lossless holding exactly the pixels the Baseline decode produced,
// masked — bit-identical outside the rectangle, since the lossless encode may
// add nothing to the loss the source already carried.
func TestRunModificationRecodesJPEGBaselineToLossless(t *testing.T) {
	params, err := compileModifyParams(ModProfile{
		MaskRegions: []MaskRegion{{Mode: maskModeRect, W: 1, H: 0.25}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath, want := writeJPEGBaselineTestDICOM(t, rootDir, false)
	for i := range 16 {
		want[i] = 0
	}

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v (%v), want 1 processed 0 failed", res, res.Failures)
	}
	if res.MaskRecodedLossless != 1 || res.MaskDecompressed != 0 {
		t.Errorf("MaskRecodedLossless = %d, MaskDecompressed = %d, want 1 and 0",
			res.MaskRecodedLossless, res.MaskDecompressed)
	}

	outPath := filepath.Join(outDir, filepath.Base(srcPath))
	if got := fileTransferSyntaxUID(outPath); got != tsJPEG2000LL {
		t.Errorf("exported transfer syntax = %q, want %q", got, tsJPEG2000LL)
	}
	w, h, nc, _, _, samples, err := decodeJPEG2000(exportedFrameData(t, outPath))
	if err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if w != 8 || h != 8 || nc != 1 {
		t.Fatalf("export decoded as %dx%d nc=%d, want 8x8 nc=1", w, h, nc)
	}
	for i := range want {
		if samples[i] != want[i] {
			t.Fatalf("sample %d = %d, want %d — export must hold the masked Baseline decode bit-exact",
				i, samples[i], want[i])
		}
	}
}

// Colour JPEG Baseline input: the export is labelled YBR_RCT — the conformant
// name for a J2K stream carrying the reversible colour transform — and the
// decoded RGB samples match the masked decode of the source exactly.
func TestRunModificationRecodesColorJPEGBaselineToLossless(t *testing.T) {
	params, err := compileModifyParams(ModProfile{
		MaskRegions: []MaskRegion{{Mode: maskModeRect, W: 1, H: 0.25}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath, want := writeJPEGBaselineTestDICOM(t, rootDir, true)
	// want is planar RGB; blank rows 0-1 of every plane.
	for c := range 3 {
		for i := range 16 {
			want[c*64+i] = 0
		}
	}

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v (%v), want 1 processed 0 failed", res, res.Failures)
	}
	if res.MaskRecodedLossless != 1 || res.MaskDecompressed != 0 {
		t.Errorf("MaskRecodedLossless = %d, MaskDecompressed = %d, want 1 and 0",
			res.MaskRecodedLossless, res.MaskDecompressed)
	}

	outPath := filepath.Join(outDir, filepath.Base(srcPath))
	if got := fileTransferSyntaxUID(outPath); got != tsJPEG2000LL {
		t.Errorf("exported transfer syntax = %q, want %q", got, tsJPEG2000LL)
	}
	ds, err := sdicom.ParseFile(outPath, nil)
	if err != nil {
		t.Fatalf("parse export: %v", err)
	}
	photElem, err := ds.FindElementByTag(tag.PhotometricInterpretation)
	if err != nil {
		t.Fatalf("PhotometricInterpretation missing: %v", err)
	}
	if got := strings.TrimSpace(sdicom.MustGetStrings(photElem.Value)[0]); got != "YBR_RCT" {
		t.Errorf("PhotometricInterpretation = %q, want YBR_RCT — the label must match the transform the encode applied", got)
	}
	w, h, nc, _, _, samples, err := decodeJPEG2000(exportedFrameData(t, outPath))
	if err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if w != 8 || h != 8 || nc != 3 {
		t.Fatalf("export decoded as %dx%d nc=%d, want 8x8 nc=3", w, h, nc)
	}
	for i := range want {
		if samples[i] != want[i] {
			t.Fatalf("sample %d = %d, want %d — export must hold the masked Baseline decode bit-exact",
				i, samples[i], want[i])
		}
	}
}

// writeJPEGBaselineTestDICOM writes an 8×8 file whose pixel data is a real
// JPEG Baseline stream from the Go encoder — the same decoder family the
// engine uses to read it back, so the expected pixels can be computed here
// with the identical conversion. Returns the file path and the expected
// decoded samples in planar layout (one plane for grayscale, three for
// colour), before masking.
func writeJPEGBaselineTestDICOM(t *testing.T, dir string, color bool) (string, []int32) {
	t.Helper()

	var (
		buf bytes.Buffer
		enc error
	)
	if color {
		img := image.NewRGBA(image.Rect(0, 0, 8, 8))
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				img.Set(x, y, color2.RGBA{R: uint8(x * 32), G: uint8(y * 32), B: uint8((x + y) * 16), A: 255})
			}
		}
		enc = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	} else {
		img := image.NewGray(image.Rect(0, 0, 8, 8))
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				img.SetGray(x, y, color2.Gray{Y: uint8(x * 32)})
			}
		}
		enc = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	}
	if enc != nil {
		t.Fatalf("encode fixture jpeg: %v", enc)
	}
	codestream := buf.Bytes()
	if len(codestream)%2 != 0 {
		codestream = append(codestream, 0)
	}

	// The expected pixels are the decode of that stream, converted exactly as
	// jpegFrameToNative converts it (RGBA()>>8 for colour, Gray pixels as-is).
	decoded, err := jpeg.Decode(bytes.NewReader(codestream))
	if err != nil {
		t.Fatalf("decode fixture jpeg: %v", err)
	}
	spp := 1
	if color {
		spp = 3
	}
	want := make([]int32, 64*spp)
	b := decoded.Bounds()
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			if color {
				r, g, bl, _ := decoded.At(b.Min.X+x, b.Min.Y+y).RGBA()
				want[0*64+y*8+x] = int32(r >> 8)
				want[1*64+y*8+x] = int32(g >> 8)
				want[2*64+y*8+x] = int32(bl >> 8)
			} else {
				gray := color2.GrayModel.Convert(decoded.At(b.Min.X+x, b.Min.Y+y)).(color2.Gray)
				want[y*8+x] = int32(gray.Y)
			}
		}
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

	photometric := "MONOCHROME2"
	if color {
		photometric = "YBR_FULL_422"
	}
	ds := sdicom.Dataset{Elements: []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.10"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{tsJPEGBaseline}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.10"}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{photometric}),
		mustTestElement(t, tag.Rows, []int{8}),
		mustTestElement(t, tag.Columns, []int{8}),
		mustTestElement(t, tag.BitsAllocated, []int{8}),
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.HighBit, []int{7}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		mustTestElement(t, tag.SamplesPerPixel, []int{spp}),
		mustTestElement(t, tag.NumberOfFrames, []string{"1"}),
		pd,
	}}

	name := "jpg-gray.dcm"
	if color {
		name = "jpg-color.dcm"
	}
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write JPEG Baseline test DICOM: %v", err)
	}
	return path, want
}

// exportedFrameData returns the first encapsulated frame's bytes.
func exportedFrameData(t *testing.T, path string) []byte {
	t.Helper()
	ds, err := sdicom.ParseFile(path, nil)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	pdElem, err := ds.FindElementByTag(tag.PixelData)
	if err != nil {
		t.Fatalf("PixelData missing: %v", err)
	}
	info, ok := pdElem.Value.GetValue().(sdicom.PixelDataInfo)
	if !ok || !info.IsEncapsulated || len(info.Frames) == 0 {
		t.Fatalf("pixel data is not encapsulated (ok=%v)", ok)
	}
	return info.Frames[0].EncapsulatedData.Data
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
	if res.MaskRecompressed != 1 || res.MaskDecompressed != 0 {
		t.Errorf("MaskRecompressed = %d, MaskDecompressed = %d, want 1 and 0 — only the named image is rewritten",
			res.MaskRecompressed, res.MaskDecompressed)
	}

	for i, uid := range uids {
		outPath := filepath.Join(outDir, uid+".dcm")
		if got := fileTransferSyntaxUID(outPath); got != tsJPEG2000LL {
			t.Errorf("%s exported as %s, want %s", uid, transferSyntaxLabel(got), transferSyntaxLabel(tsJPEG2000LL))
		}
		if i != 1 {
			// Files the region does not apply to are never decoded at all, so
			// their codestream bytes must survive verbatim.
			if !slices.Equal(exportedFrameData(t, outPath), exportedFrameData(t, paths[i])) {
				t.Errorf("%s codestream differs from its source — an untouched file must not be re-encoded", uid)
			}
		}
	}
	// The named image really is masked in its recompressed form.
	frames := decodeExportedJ2KFrames(t, filepath.Join(outDir, uids[1]+".dcm"))
	if len(frames) != 1 || !slices.Equal(frames[0], maskedRamp8()) {
		t.Errorf("masked image pixels = %v, want the masked ramp %v", frames, maskedRamp8())
	}
}
