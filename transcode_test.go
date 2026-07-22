package main

import (
	"os"
	"path/filepath"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
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

func TestMigrateProfile(t *testing.T) {
	// Old-style flag maps to Explicit VR LE preferred + on-disk guarantee.
	p := ServerProfile{TransferUncompressed: true}
	migrateProfile(&p)
	if p.TransferSyntax != tsPrefExplicitLE || !p.EnsureUncompressed || p.TransferUncompressed {
		t.Errorf("migrated profile = %+v, want explicit-le + ensure, old flag cleared", p)
	}

	// A profile that already has the new field wins over the old flag.
	p = ServerProfile{TransferUncompressed: true, TransferSyntax: tsPrefImplicitLE}
	migrateProfile(&p)
	if p.TransferSyntax != tsPrefImplicitLE || p.EnsureUncompressed {
		t.Errorf("migrate must not override explicit new-style settings: %+v", p)
	}

	// Untouched default profile stays "as stored".
	p = ServerProfile{}
	migrateProfile(&p)
	if p.TransferSyntax != tsPrefAny || p.EnsureUncompressed {
		t.Errorf("default profile changed by migration: %+v", p)
	}
}

func TestPreferredTransferSyntaxes(t *testing.T) {
	p := ServerProfile{TransferSyntax: tsPrefExplicitLE}
	if got := p.preferredTransferSyntaxes(); len(got) != 2 || got[0] != tsExplicitVRLE || got[1] != tsImplicitVRLE {
		t.Errorf("explicit-le preference = %v", got)
	}
	p.TransferSyntax = tsPrefImplicitLE
	if got := p.preferredTransferSyntaxes(); len(got) != 2 || got[0] != tsImplicitVRLE || got[1] != tsExplicitVRLE {
		t.Errorf("implicit-le preference = %v", got)
	}
	p.TransferSyntax = tsPrefAny
	if got := p.preferredTransferSyntaxes(); got != nil {
		t.Errorf("any preference = %v, want nil (accept all)", got)
	}
	if !(ServerProfile{TransferSyntax: tsPrefExplicitLE}).wantsUncompressed() {
		t.Error("explicit-le must report wantsUncompressed")
	}
	if (ServerProfile{}).wantsUncompressed() {
		t.Error("default profile must not report wantsUncompressed")
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

// writeTestDICOM writes a minimal native 8-bit 2×2 Explicit VR LE file and
// returns its path.
func writeTestDICOM(t *testing.T, dir string) string {
	t.Helper()
	nf := frame.NewNativeFrame[uint8](8, 2, 2, 4, 1)
	copy(nf.RawData, []uint8{10, 20, 30, 40})
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		IsEncapsulated: false,
		Frames:         []*frame.Frame{{Encapsulated: false, NativeData: nf}},
	})
	if err != nil {
		t.Fatalf("NewElement(PixelData): %v", err)
	}
	ds := sdicom.Dataset{Elements: []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.5"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{tsExplicitVRLE}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.5"}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{"MONOCHROME2"}),
		mustTestElement(t, tag.Rows, []int{2}),
		mustTestElement(t, tag.Columns, []int{2}),
		mustTestElement(t, tag.BitsAllocated, []int{8}),
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.HighBit, []int{7}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		mustTestElement(t, tag.SamplesPerPixel, []int{1}),
		pd,
	}}

	path := filepath.Join(dir, "native.dcm")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, ds); err != nil {
		t.Fatalf("write test DICOM: %v", err)
	}
	return path
}

func mustTestElement(t *testing.T, tg tag.Tag, data any) *sdicom.Element {
	t.Helper()
	e, err := sdicom.NewElement(tg, data)
	if err != nil {
		t.Fatalf("NewElement(%v): %v", tg, err)
	}
	return e
}

func TestFileTransferSyntaxUID(t *testing.T) {
	path := writeTestDICOM(t, t.TempDir())
	if got := fileTransferSyntaxUID(path); got != tsExplicitVRLE {
		t.Errorf("fileTransferSyntaxUID = %q, want %q", got, tsExplicitVRLE)
	}
	if got := fileTransferSyntaxUID(filepath.Join(t.TempDir(), "missing.dcm")); got != "" {
		t.Errorf("missing file gave %q, want empty", got)
	}
}

func TestTranscodeNoOpOnUncompressed(t *testing.T) {
	path := writeTestDICOM(t, t.TempDir())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := transcodeDICOMFile(path)
	if err != nil {
		t.Fatalf("transcodeDICOMFile: %v", err)
	}
	if changed {
		t.Error("uncompressed file must not be rewritten")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("file content changed by a no-op transcode")
	}
}
