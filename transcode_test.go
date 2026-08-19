package main

import (
	"os"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// TestProjectedPixelBytes covers the figure the modification pool sizes itself
// by. The multi-frame case is the one that matters: it is the difference
// between a file that looks small on disk and one that needs hundreds of
// megabytes decoded.
func TestProjectedPixelBytes(t *testing.T) {
	build := func(t *testing.T, elems ...*sdicom.Element) sdicom.Dataset {
		t.Helper()
		return sdicom.Dataset{Elements: elems}
	}

	t.Run("single-frame 8-bit greyscale", func(t *testing.T) {
		ds := build(t,
			mustTestElement(t, tag.Columns, []int{512}),
			mustTestElement(t, tag.Rows, []int{512}),
			mustTestElement(t, tag.BitsAllocated, []int{8}),
			mustTestElement(t, tag.SamplesPerPixel, []int{1}),
		)
		if got, want := projectedPixelBytes(&ds), int64(512*512); got != want {
			t.Errorf("= %d, want %d", got, want)
		}
	})

	t.Run("16-bit doubles it", func(t *testing.T) {
		ds := build(t,
			mustTestElement(t, tag.Columns, []int{512}),
			mustTestElement(t, tag.Rows, []int{512}),
			mustTestElement(t, tag.BitsAllocated, []int{16}),
			mustTestElement(t, tag.SamplesPerPixel, []int{1}),
		)
		if got, want := projectedPixelBytes(&ds), int64(512*512*2); got != want {
			t.Errorf("= %d, want %d", got, want)
		}
	})

	t.Run("colour triples it", func(t *testing.T) {
		ds := build(t,
			mustTestElement(t, tag.Columns, []int{640}),
			mustTestElement(t, tag.Rows, []int{480}),
			mustTestElement(t, tag.BitsAllocated, []int{8}),
			mustTestElement(t, tag.SamplesPerPixel, []int{3}),
		)
		if got, want := projectedPixelBytes(&ds), int64(640*480*3); got != want {
			t.Errorf("= %d, want %d", got, want)
		}
	})

	// The case the budget exists for: a 240-frame acquisition is three orders of
	// magnitude past a single frame, and NumberOfFrames is stored as IS (a
	// string), which datasetInt has to tolerate.
	t.Run("multi-frame scales by NumberOfFrames", func(t *testing.T) {
		ds := build(t,
			mustTestElement(t, tag.Columns, []int{1024}),
			mustTestElement(t, tag.Rows, []int{1024}),
			mustTestElement(t, tag.BitsAllocated, []int{16}),
			mustTestElement(t, tag.SamplesPerPixel, []int{1}),
			mustTestElement(t, tag.NumberOfFrames, []string{"240"}),
		)
		want := int64(1024) * 1024 * 2 * 240
		if got := projectedPixelBytes(&ds); got != want {
			t.Errorf("= %d MB, want %d MB", got>>20, want>>20)
		}
	})

	// A report or key-object selection has no pixels, so it weighs nothing —
	// the honest answer rather than a guessed default.
	t.Run("no pixel geometry weighs nothing", func(t *testing.T) {
		ds := build(t, mustTestElement(t, tag.PatientName, []string{"DOE^JANE"}))
		if got := projectedPixelBytes(&ds); got != 0 {
			t.Errorf("= %d, want 0", got)
		}
	})
}

func TestIsUncompressedOnDisk(t *testing.T) {
	for uid, want := range map[string]bool{
		"1.2.840.10008.1.2":      true,  // Implicit VR LE
		"1.2.840.10008.1.2.1":    true,  // Explicit VR LE
		"1.2.840.10008.1.2.2":    false, // Explicit VR BE (retired)
		"1.2.840.10008.1.2.4.90": false, // JPEG 2000 Lossless
		"1.2.840.10008.1.2.5":    false, // RLE
		"":                       false,
	} {
		if got := isUncompressedOnDisk(uid); got != want {
			t.Errorf("isUncompressedOnDisk(%q) = %v, want %v", uid, got, want)
		}
	}
}

func TestCanDecompressSyntax(t *testing.T) {
	for uid, want := range map[string]bool{
		"1.2.840.10008.1.2.4.50": true,                  // JPEG Baseline
		"1.2.840.10008.1.2.4.51": true,                  // JPEG Extended
		"1.2.840.10008.1.2.4.90": jpeg2000Available,     // JPEG 2000 Lossless
		"1.2.840.10008.1.2.4.91": jpeg2000Available,     // JPEG 2000
		"1.2.840.10008.1.2.4.57": jpegLosslessAvailable, // JPEG Lossless
		"1.2.840.10008.1.2.4.70": jpegLosslessAvailable, // JPEG Lossless SV1
		"1.2.840.10008.1.2.5":    false,                 // RLE
		"1.2.840.10008.1.2.1":    false,                 // not compressed at all
	} {
		if got := canDecompressSyntax(uid); got != want {
			t.Errorf("canDecompressSyntax(%q) = %v, want %v", uid, got, want)
		}
	}
}

func TestNewNativeFromSamples(t *testing.T) {
	// 16-bit: negative int32 samples survive as two's-complement uint16.
	nf, err := newNativeFromSamples(16, 1, 2, 1, func(i int) int32 { return int32(-2 + i) })
	if err != nil {
		t.Fatalf("newNativeFromSamples(16): %v", err)
	}
	raw16, ok := nf.RawDataSlice().([]uint16)
	if !ok || len(raw16) != 2 {
		t.Fatalf("16-bit RawDataSlice = %T len %d", nf.RawDataSlice(), len(raw16))
	}
	if int16(raw16[0]) != -2 || int16(raw16[1]) != -1 {
		t.Errorf("signed round-trip = [%d %d], want [-2 -1]", int16(raw16[0]), int16(raw16[1]))
	}

	// 8-bit interleaved RGB geometry.
	nf, err = newNativeFromSamples(8, 2, 2, 3, func(i int) int32 { return int32(i) })
	if err != nil {
		t.Fatalf("newNativeFromSamples(8): %v", err)
	}
	if nf.Rows() != 2 || nf.Cols() != 2 || nf.SamplesPerPixel() != 3 || nf.BitsPerSample() != 8 {
		t.Errorf("frame geometry %dx%d spp=%d bps=%d", nf.Rows(), nf.Cols(), nf.SamplesPerPixel(), nf.BitsPerSample())
	}
	raw8 := nf.RawDataSlice().([]uint8)
	if len(raw8) != 12 || raw8[11] != 11 {
		t.Errorf("interleaved data = %v", raw8)
	}

	if _, err := newNativeFromSamples(12, 1, 1, 1, func(int) int32 { return 0 }); err == nil {
		t.Error("BitsAllocated 12 must be rejected")
	}
}

func TestTranscodeNoOpOnTargetSyntax(t *testing.T) {
	// writeTestDICOM produces Explicit VR LE; targeting the same syntax is a no-op.
	path := writeTestDICOM(t, t.TempDir())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := transcodeDICOMFile(path, tsExplicitVRLE)
	if err != nil {
		t.Fatalf("transcodeDICOMFile: %v", err)
	}
	if changed {
		t.Error("file already in the target syntax must not be rewritten")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("file content changed by a no-op transcode")
	}
}

// dicomPixels re-parses a native (uncompressed) file and returns its first
// frame's 8-bit sample data.
func dicomPixels(t *testing.T, path string) []uint8 {
	t.Helper()
	ds, err := sdicom.ParseFile(path, nil)
	if err != nil {
		t.Fatalf("re-parse %s: %v", path, err)
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
	raw, ok := nf.RawDataSlice().([]uint8)
	if !ok {
		t.Fatalf("RawDataSlice type %T, want []uint8", nf.RawDataSlice())
	}
	return raw
}

// A file already uncompressed but in the other VR encoding must still be
// re-encoded to the required syntax (lossless, pixels preserved) — this is
// what the receive path relies on when a server sends the other uncompressed
// VR instead of the required one.
func TestTranscodeUncompressedVRConversion(t *testing.T) {
	path := writeTestDICOM(t, t.TempDir()) // Explicit VR LE, pixels 10,20,30,40
	wantPixels := []uint8{10, 20, 30, 40}

	// Explicit VR LE -> Implicit VR LE.
	changed, err := transcodeDICOMFile(path, tsImplicitVRLE)
	if err != nil {
		t.Fatalf("Explicit->Implicit: %v", err)
	}
	if !changed {
		t.Fatal("Explicit->Implicit should have rewritten the file")
	}
	if got := fileTransferSyntaxUID(path); got != tsImplicitVRLE {
		t.Fatalf("after Explicit->Implicit: transfer syntax = %q, want %q", got, tsImplicitVRLE)
	}
	if got := dicomPixels(t, path); len(got) < 4 ||
		got[0] != wantPixels[0] || got[1] != wantPixels[1] || got[2] != wantPixels[2] || got[3] != wantPixels[3] {
		t.Errorf("pixels after Explicit->Implicit = %v, want %v", got, wantPixels)
	}

	// Implicit VR LE -> Explicit VR LE round-trips losslessly.
	changed, err = transcodeDICOMFile(path, tsExplicitVRLE)
	if err != nil {
		t.Fatalf("Implicit->Explicit: %v", err)
	}
	if !changed {
		t.Fatal("Implicit->Explicit should have rewritten the file")
	}
	if got := fileTransferSyntaxUID(path); got != tsExplicitVRLE {
		t.Fatalf("after Implicit->Explicit: transfer syntax = %q, want %q", got, tsExplicitVRLE)
	}
	if got := dicomPixels(t, path); len(got) < 4 ||
		got[0] != wantPixels[0] || got[1] != wantPixels[1] || got[2] != wantPixels[2] || got[3] != wantPixels[3] {
		t.Errorf("pixels after Implicit->Explicit = %v, want %v", got, wantPixels)
	}
}

// A compressed syntax with no built-in decoder must fail with a clear error
// and leave the original untouched — the receive path turns this into a failed
// sub-operation.
func TestTranscodeRejectsUndecodableSyntax(t *testing.T) {
	path := writeTestDICOM(t, t.TempDir())
	// Rewrite the meta TS to JPEG-LS (no decoder) without touching pixel data.
	ds, err := sdicom.ParseFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := setElementValue(&ds, tag.TransferSyntaxUID, []string{"1.2.840.10008.1.2.4.80"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification())
	f.Close()
	if writeErr != nil {
		// The library refuses to write native pixels under an encapsulated
		// meta syntax — the fixture cannot be built this way; not a defect in
		// the code under test.
		t.Skipf("cannot build JPEG-LS-labelled fixture: %v", writeErr)
	}

	if _, err := transcodeDICOMFile(path, tsExplicitVRLE); err == nil {
		t.Fatal("JPEG-LS source must be rejected (no built-in decoder)")
	}
}

// convertDatasetSyntax is the conversion itself, reached both by the receive
// path (wrapped in file I/O) and by a modification profile, which holds only a
// parsed dataset. These assertions are on the dataset contract the second
// caller depends on.
func TestConvertDatasetSyntax(t *testing.T) {
	load := func(t *testing.T) sdicom.Dataset {
		t.Helper()
		ds, err := sdicom.ParseFile(writeTestDICOM(t, t.TempDir()), nil)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		return ds
	}

	t.Run("already in target is a no-op", func(t *testing.T) {
		ds := load(t)
		changed, err := convertDatasetSyntax(&ds, tsExplicitVRLE, tsExplicitVRLE)
		if err != nil {
			t.Fatalf("convertDatasetSyntax: %v", err)
		}
		if changed {
			t.Error("reported a change converting to the syntax it already has")
		}
	})

	t.Run("rewrites the meta transfer syntax", func(t *testing.T) {
		ds := load(t)
		changed, err := convertDatasetSyntax(&ds, tsExplicitVRLE, tsImplicitVRLE)
		if err != nil {
			t.Fatalf("convertDatasetSyntax: %v", err)
		}
		if !changed {
			t.Error("reported no change converting Explicit -> Implicit")
		}
		if got := datasetTransferSyntaxUID(&ds); got != tsImplicitVRLE {
			t.Errorf("dataset transfer syntax = %q, want %q", got, tsImplicitVRLE)
		}
	})

	t.Run("undecodable source is rejected", func(t *testing.T) {
		ds := load(t)
		// The gate is on the source syntax and fires before any decode, so the
		// fixture's actual pixel encoding is irrelevant here.
		if _, err := convertDatasetSyntax(&ds, "1.2.840.10008.1.2.4.80", tsExplicitVRLE); err == nil {
			t.Fatal("JPEG-LS source must be rejected (no built-in decoder)")
		}
	})

	t.Run("unknown source syntax is rejected", func(t *testing.T) {
		ds := load(t)
		if _, err := convertDatasetSyntax(&ds, "", tsExplicitVRLE); err == nil {
			t.Fatal("an empty source syntax must be an error, not a silent pass-through")
		}
	})
}

// datasetTransferSyntaxUID must agree with the file-based reader, since the
// modification engine relies on it to decide what a file is being converted
// from.
func TestDatasetTransferSyntaxUID(t *testing.T) {
	path := writeTestDICOM(t, t.TempDir())
	ds, err := sdicom.ParseFile(path, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := datasetTransferSyntaxUID(&ds), fileTransferSyntaxUID(path); got != want {
		t.Errorf("datasetTransferSyntaxUID = %q, fileTransferSyntaxUID = %q — must agree", got, want)
	}
	if got := datasetTransferSyntaxUID(&sdicom.Dataset{}); got != "" {
		t.Errorf("empty dataset yielded %q, want \"\"", got)
	}
}
