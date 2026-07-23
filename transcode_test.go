package main

import (
	"os"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

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
		"1.2.840.10008.1.2.4.50": true,              // JPEG Baseline
		"1.2.840.10008.1.2.4.51": true,              // JPEG Extended
		"1.2.840.10008.1.2.4.90": jpeg2000Available, // JPEG 2000 Lossless
		"1.2.840.10008.1.2.4.91": jpeg2000Available, // JPEG 2000
		"1.2.840.10008.1.2.4.70": false,             // JPEG Lossless SV1
		"1.2.840.10008.1.2.5":    false,             // RLE
		"1.2.840.10008.1.2.1":    false,             // not compressed at all
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
