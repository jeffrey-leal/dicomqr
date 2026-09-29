package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// writeRawPixelFixture writes a 16-bit greyscale file under ts whose samples are
// a ramp wide enough that both bytes of every sample vary — so a swapped byte
// order or a dropped byte cannot go unnoticed.
func writeRawPixelFixture(t *testing.T, path, ts string) []uint16 {
	t.Helper()
	const rows, cols = 32, 48
	nf := frame.NewNativeFrame[uint16](16, rows, cols, rows*cols, 1)
	want := make([]uint16, rows*cols)
	for i := range want {
		want[i] = uint16(i*37 + 0x0102)
	}
	copy(nf.RawData, want)
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		Frames: []*frame.Frame{{Encapsulated: false, NativeData: nf}},
	})
	if err != nil {
		t.Fatalf("NewElement(PixelData): %v", err)
	}
	elems := []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.2"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.5.6"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{ts}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.2"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.5.6"}),
		mustTestElement(t, tag.Modality, []string{"CT"}),
		mustTestElement(t, tag.PatientName, []string{"DOE^JANE"}),
		mustTestElement(t, tag.StudyInstanceUID, []string{"1.2.3.4"}),
		mustTestElement(t, tag.SeriesInstanceUID, []string{"1.2.3.4.1"}),
		mustTestElement(t, tag.SamplesPerPixel, []int{1}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{"MONOCHROME2"}),
		mustTestElement(t, tag.Rows, []int{rows}),
		mustTestElement(t, tag.Columns, []int{cols}),
		mustTestElement(t, tag.BitsAllocated, []int{16}),
		mustTestElement(t, tag.BitsStored, []int{16}),
		mustTestElement(t, tag.HighBit, []int{15}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		pd,
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, sdicom.Dataset{Elements: elems},
		sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return want
}

// fileSamples reads path's first frame the ordinary way, as samples.
func fileSamples(t *testing.T, path string) []uint16 {
	t.Helper()
	ds, err := sdicom.ParseFile(path, nil)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	e, err := ds.FindElementByTag(tag.PixelData)
	if err != nil {
		t.Fatalf("%s has no pixel data", path)
	}
	info := sdicom.MustGetPixelDataInfo(e.Value)
	if len(info.Frames) == 0 || info.Frames[0].NativeData == nil {
		t.Fatalf("%s has no native frame", path)
	}
	s, ok := info.Frames[0].NativeData.RawDataSlice().([]uint16)
	if !ok {
		t.Fatalf("%s frame is not 16-bit", path)
	}
	return s
}

// filePixelBytes returns path's pixel data element value exactly as stored.
func filePixelBytes(t *testing.T, path string) []byte {
	t.Helper()
	ds, err := sdicom.ParseFile(path, nil, sdicom.SkipProcessingPixelDataValue())
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	e, err := ds.FindElementByTag(tag.PixelData)
	if err != nil {
		t.Fatalf("%s has no pixel data", path)
	}
	return sdicom.MustGetPixelDataInfo(e.Value).UnprocessedValueData
}

// A run that cannot mask carries native pixel data through as stored bytes —
// the tag-only speed-up. Every case must come out with the source's samples
// intact and byte-identical, and the conversions must still land in their
// target syntax.
func TestModificationRawPixelPassthrough(t *testing.T) {
	cases := []struct {
		name, sourceTS, pref, wantTS string
		wantSameBytes                bool
	}{
		{"tag-only", uid.ExplicitVRLittleEndian, "", uid.ExplicitVRLittleEndian, true},
		{"explicit to implicit", uid.ExplicitVRLittleEndian, tsPrefImplicitLE, uid.ImplicitVRLittleEndian, true},
		{"implicit to explicit", uid.ImplicitVRLittleEndian, tsPrefExplicitLE, uid.ExplicitVRLittleEndian, true},
		{"big endian kept", uid.ExplicitVRBigEndian, "", uid.ExplicitVRBigEndian, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params, err := compileModifyParams(ModProfile{
				Sets:           []string{"0010,0010=ANON"},
				TransferSyntax: tc.pref,
			})
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if !rawPixelPassthrough(params) {
				t.Fatal("a profile with no mask regions must take the raw path")
			}
			rootDir, outDir := t.TempDir(), t.TempDir()
			src := filepath.Join(rootDir, "img.dcm")
			want := writeRawPixelFixture(t, src, tc.sourceTS)

			res := runModification(context.Background(), []string{src}, rootDir, outDir, params, nil, nil)
			if res.Failed != 0 || res.Processed != 1 {
				t.Fatalf("result = %+v, want 1 processed 0 failed", res)
			}
			out := filepath.Join(outDir, "img.dcm")
			if got := fileTransferSyntaxUID(out); got != tc.wantTS {
				t.Errorf("exported syntax = %q, want %q", got, tc.wantTS)
			}
			got := fileSamples(t, out)
			if len(got) != len(want) {
				t.Fatalf("exported %d samples, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("sample %d = %#04x, want %#04x", i, got[i], want[i])
				}
			}
			if same := bytes.Equal(filePixelBytes(t, out), filePixelBytes(t, src)); same != tc.wantSameBytes {
				t.Errorf("pixel bytes identical to source = %v, want %v", same, tc.wantSameBytes)
			}
			ds, err := sdicom.ParseFile(out, nil, sdicom.SkipPixelData())
			if err != nil {
				t.Fatal(err)
			}
			if name := datasetFirstString(&ds, tag.PatientName); name != "ANON" {
				t.Errorf("PatientName = %q, want the tag rules applied", name)
			}
		})
	}
}

// Raw passthrough writes a Big Endian source's sample bytes as they were read,
// which is only right while nothing converts such a file to little-endian. Today
// convertDatasetSyntax refuses it. If that ever changes, this fails, and
// rawPixelPassthrough must learn to byte-swap (or decline) first.
func TestRawPassthroughReliesOnBigEndianRefusal(t *testing.T) {
	params, err := compileModifyParams(ModProfile{TransferSyntax: tsPrefExplicitLE})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	rootDir, outDir := t.TempDir(), t.TempDir()
	src := filepath.Join(rootDir, "be.dcm")
	writeRawPixelFixture(t, src, uid.ExplicitVRBigEndian)
	res := runModification(context.Background(), []string{src}, rootDir, outDir, params, nil, nil)
	if res.Failed != 1 {
		t.Fatalf("result = %+v: a Big Endian source converted to little-endian now exports, "+
			"and raw pixel passthrough would write its samples byte-swapped", res)
	}
}

// Masking writes sample values, so any run that may mask — through the profile
// or any per-modality override — must parse frames, not bytes.
func TestRawPixelPassthroughOffWhenMasking(t *testing.T) {
	for _, p := range []ModProfile{
		{MaskRegions: []MaskRegion{{Mode: maskModeRect, X: 0, Y: 0, W: 0.5, H: 0.1}}},
		{PerModality: map[string]ModProfile{
			"US": {MaskRegions: []MaskRegion{{Mode: maskModeRect, X: 0, Y: 0, W: 0.5, H: 0.1}}},
		}},
	} {
		params, err := compileModifyParams(p)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if rawPixelPassthrough(params) {
			t.Errorf("profile %+v may mask but took the raw path", p)
		}
	}
}
