package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grailbio/go-dicom/dicomio"
	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

func TestMigrateProfile(t *testing.T) {
	// Old-style flag maps to requiring Explicit VR LE.
	p := ServerProfile{TransferUncompressed: true}
	migrateProfile(&p)
	if p.TransferSyntax != tsPrefExplicitLE || p.TransferUncompressed {
		t.Errorf("migrated profile = %+v, want explicit-le with old flag cleared", p)
	}

	// A profile that already has the new field wins over the old flag.
	p = ServerProfile{TransferUncompressed: true, TransferSyntax: tsPrefImplicitLE}
	migrateProfile(&p)
	if p.TransferSyntax != tsPrefImplicitLE {
		t.Errorf("migrate must not override explicit new-style settings: %+v", p)
	}

	// Untouched default profile stays "as stored".
	p = ServerProfile{}
	migrateProfile(&p)
	if p.TransferSyntax != tsPrefAny {
		t.Errorf("default profile changed by migration: %+v", p)
	}
}

func TestRequiredTransferSyntax(t *testing.T) {
	cases := map[string]string{
		tsPrefExplicitLE: tsExplicitVRLE,
		tsPrefImplicitLE: tsImplicitVRLE,
		tsPrefAny:        "",
		"bogus":          "",
	}
	for pref, want := range cases {
		if got := (ServerProfile{TransferSyntax: pref}).requiredTransferSyntax(); got != want {
			t.Errorf("requiredTransferSyntax(%q) = %q, want %q", pref, got, want)
		}
	}
}

// The negotiable set for a required syntax leads with that syntax (so a
// transcoding server picks it) followed only by syntaxes the receive path can
// convert locally; nothing else may appear or non-convertible files could land
// on disk.
func TestAcceptedSyntaxesFor(t *testing.T) {
	if got := acceptedSyntaxesFor(""); got != nil {
		t.Errorf("acceptedSyntaxesFor(\"\") = %v, want nil (accept all)", got)
	}
	for req, other := range map[string]string{
		tsImplicitVRLE: tsExplicitVRLE,
		tsExplicitVRLE: tsImplicitVRLE,
	} {
		got := acceptedSyntaxesFor(req)
		if len(got) < 4 || got[0] != req || got[1] != other {
			t.Fatalf("acceptedSyntaxesFor(%s) = %v, want [%s %s baseline extended ...]", req, got, req, other)
		}
		for _, uid := range got[2:] {
			if !canDecompressSyntax(uid) {
				t.Errorf("acceptedSyntaxesFor(%s) includes %s with no local decoder", req, uid)
			}
		}
	}
}

// C-GET proposals mirror the accepted set: required syntax first plus the
// locally convertible fallbacks; the unrestricted default stays the standard
// (all-uncompressed) set.
func TestProposedTransferSyntaxes(t *testing.T) {
	for _, pref := range []string{tsPrefImplicitLE, tsPrefExplicitLE} {
		p := ServerProfile{TransferSyntax: pref}
		want := acceptedSyntaxesFor(p.requiredTransferSyntax())
		got := proposedTransferSyntaxes(p)
		if len(got) != len(want) {
			t.Fatalf("%s proposal = %v, want %v", pref, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s proposal[%d] = %s, want %s", pref, i, got[i], want[i])
			}
		}
	}
	got := proposedTransferSyntaxes(ServerProfile{TransferSyntax: tsPrefAny})
	if len(got) != len(dicomio.StandardTransferSyntaxes) {
		t.Errorf("as-stored proposal = %v, want the standard set", got)
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
