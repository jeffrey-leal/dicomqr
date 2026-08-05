package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// embeddedModConfigs unmarshals the compiled-in default profiles and aliases.
func embeddedModConfigs(t *testing.T) (ModProfileConfig, TagConfig) {
	t.Helper()
	var profiles ModProfileConfig
	if err := json.Unmarshal(defaultModProfilesJSON, &profiles); err != nil {
		t.Fatalf("unmarshal embedded profiles.json: %v", err)
	}
	var aliases TagConfig
	if err := json.Unmarshal(defaultModTagsJSON, &aliases); err != nil {
		t.Fatalf("unmarshal embedded tags.json: %v", err)
	}
	return profiles, aliases
}

func TestResolveModProfileEmbeddedDefaults(t *testing.T) {
	profiles, _ := embeddedModConfigs(t)

	base, err := resolveModProfile("base-deident", profiles)
	if err != nil {
		t.Fatalf("resolve base-deident: %v", err)
	}
	if len(base.Sets) != 4 {
		t.Errorf("base-deident sets = %d, want 4", len(base.Sets))
	}
	if base.DOB != "YYYY0101" || !base.Priv || !base.RemapUIDs || base.FixVR != "correct" {
		t.Errorf("base-deident options = dob %q priv %v remap %v fixvr %q",
			base.DOB, base.Priv, base.RemapUIDs, base.FixVR)
	}

	keepOrder, err := resolveModProfile("base-deident-keep-order", profiles)
	if err != nil {
		t.Fatalf("resolve base-deident-keep-order: %v", err)
	}
	// The derived profile keeps the 29 Group-0040 workflow tags: its effective
	// removal list must be exactly that much shorter, with no 0040,xxxx entry
	// from the keep list surviving.
	if want := len(base.Removes) - 29; len(keepOrder.Removes) != want {
		t.Errorf("keep-order removes = %d, want %d", len(keepOrder.Removes), want)
	}
	for _, r := range keepOrder.Removes {
		for _, k := range profiles["base-deident-keep-order"].Keep {
			if strings.EqualFold(strings.TrimSpace(r), strings.TrimSpace(k)) {
				t.Errorf("kept tag %q still present in removal list", r)
			}
		}
	}
	// Inherited scalars/booleans survive the merge.
	if keepOrder.DOB != "YYYY0101" || !keepOrder.RemapUIDs {
		t.Errorf("keep-order inherited options = dob %q remap %v", keepOrder.DOB, keepOrder.RemapUIDs)
	}
}

func TestCompileModifyParamsValidation(t *testing.T) {
	_, aliases := embeddedModConfigs(t)

	cases := []struct {
		name string
		p    ModProfile
		want string // substring of the expected error; "" = must succeed
	}{
		{"remap and suffix", ModProfile{RemapUIDs: true, UIDSuffix: "7"}, "cannot be combined"},
		{"bad dob length", ModProfile{DOB: "1980"}, "8 characters"},
		{"bad uid charset", ModProfile{UIDSuffix: "10"}, "digits in the set"},
		{"bad fixvr", ModProfile{FixVR: "maybe"}, "must be correct"},
		{"bad shiftdays", ModProfile{ShiftDays: "abc"}, "must be an integer"},
		{"bad transfersyntax", ModProfile{TransferSyntax: "jpeg2000"}, "must be explicit-le"},
		{"no action", ModProfile{}, "no actionable parameter"},
		{"zip alone is not an action", ModProfile{Zip: true}, "no actionable parameter"},
		// Unlike zip, a transfer syntax alone is a real transformation: export is
		// the only place the application can convert a file.
		{"transfersyntax only is actionable", ModProfile{TransferSyntax: tsPrefImplicitLE}, ""},
		{"shiftdays only is actionable", ModProfile{ShiftDays: "-45"}, ""},
		{"shiftdays zero accepted", ModProfile{ShiftDays: "0"}, ""},
		{"alias set", ModProfile{Sets: []string{"PatientName=X"}}, ""},
		{"short-form remove", ModProfile{Removes: []string{"8,80"}}, ""},
	}
	for _, tc := range cases {
		_, err := compileModifyParams(tc.p, aliases)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want substring %q", tc.name, err, tc.want)
		}
	}
}

// The transfer syntax inherits down a base chain like the other scalars: a
// child that names none keeps its base's, and one that names its own wins.
// This is how the shipped profiles are structured, so a syntax set once on a
// base has to reach every profile built on it.
func TestModProfileTransferSyntaxInheritance(t *testing.T) {
	cfg := ModProfileConfig{
		"base":     {TransferSyntax: tsPrefImplicitLE, Sets: []string{"0010,0010=ANON"}},
		"child":    {Base: "base"},
		"override": {Base: "base", TransferSyntax: tsPrefExplicitLE},
		"plain":    {Sets: []string{"0010,0010=ANON"}},
	}
	for name, want := range map[string]string{
		"base":     tsPrefImplicitLE,
		"child":    tsPrefImplicitLE,
		"override": tsPrefExplicitLE,
		"plain":    "",
	} {
		resolved, err := resolveModProfile(name, cfg)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if resolved.TransferSyntax != want {
			t.Errorf("%s resolved TransferSyntax = %q, want %q", name, resolved.TransferSyntax, want)
		}
	}
}

// modProfileTargetSyntax is the single mapping from stored token to the UID
// exports are written in; an unknown token must be reported, never treated as
// "as stored", or a hand-edited profile would silently skip its conversion.
func TestModProfileTargetSyntax(t *testing.T) {
	for token, want := range map[string]string{
		"":               "",
		tsPrefExplicitLE: tsExplicitVRLE,
		tsPrefImplicitLE: tsImplicitVRLE,
		"  ":             "",
	} {
		got, ok := modProfileTargetSyntax(ModProfile{TransferSyntax: token})
		if !ok || got != want {
			t.Errorf("modProfileTargetSyntax(%q) = (%q, %v), want (%q, true)", token, got, ok, want)
		}
	}
	if _, ok := modProfileTargetSyntax(ModProfile{TransferSyntax: "jpeg2000"}); ok {
		t.Error("an unknown token must not resolve — it would silently skip the conversion")
	}
}

// The label mapping used by both editors must round-trip, or a profile would
// change syntax merely by being opened and saved.
func TestTransferSyntaxPrefLabels(t *testing.T) {
	for _, token := range []string{"", tsPrefExplicitLE, tsPrefImplicitLE} {
		if got := transferSyntaxPrefFromLabel(transferSyntaxPrefLabel(token)); got != token {
			t.Errorf("token %q round-tripped to %q", token, got)
		}
	}
	if got := transferSyntaxPrefLabel("jpeg2000"); got != tsExportLabelAny {
		t.Errorf("unknown token displayed as %q, want %q", got, tsExportLabelAny)
	}
}

func TestPathWithinDir(t *testing.T) {
	root := `C:\Users\x\DICOM Downloads`
	cases := []struct {
		dir  string
		want bool
	}{
		{`C:\Users\x\DICOM Downloads`, true},
		{`c:\users\x\dicom downloads\sub`, true},
		{`C:\Users\x\deid-out`, false},
		{`C:\Users\x`, false},
	}
	for _, tc := range cases {
		if got := pathWithinDir(tc.dir, root); got != tc.want {
			t.Errorf("pathWithinDir(%q) = %v, want %v", tc.dir, got, tc.want)
		}
	}
}

// writeModifyTestDICOM writes a native 8-bit file carrying patient identity,
// a removable tag, and (when the library allows creating one) a private tag.
func writeModifyTestDICOM(t *testing.T, path string) (hasPrivate bool) {
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
	elems := []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.5"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{tsExplicitVRLE}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.5"}),
		mustTestElement(t, tag.PatientName, []string{"DOE^JANE"}),
		mustTestElement(t, tag.PatientID, []string{"PID123"}),
		mustTestElement(t, tag.PatientBirthDate, []string{"19800615"}),
		mustTestElement(t, tag.AccessionNumber, []string{"ACC42"}),
		mustTestElement(t, tag.InstitutionName, []string{"GENERAL HOSPITAL"}),
		mustTestElement(t, tag.Modality, []string{"OT"}),
		mustTestElement(t, tag.StudyDate, []string{"20240102"}),
		mustTestElement(t, tag.AcquisitionDateTime, []string{"20240102093000.000000+0000"}),
		// A sequence carrying a date proves the shift recurses; 0008,1140 is not
		// touched by the default profiles, so the other end-to-end tests are inert.
		mustTestElement(t, tag.Tag{Group: 0x0008, Element: 0x1140}, [][]*sdicom.Element{{
			mustTestElement(t, tag.StudyDate, []string{"20240102"}),
		}}),
		mustTestElement(t, tag.StudyInstanceUID, []string{"1.2.3.4"}),
		mustTestElement(t, tag.SeriesInstanceUID, []string{"1.2.3.4.1"}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{"MONOCHROME2"}),
		mustTestElement(t, tag.Rows, []int{2}),
		mustTestElement(t, tag.Columns, []int{2}),
		mustTestElement(t, tag.BitsAllocated, []int{8}),
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.HighBit, []int{7}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		mustTestElement(t, tag.SamplesPerPixel, []int{1}),
	}
	if priv, perr := sdicom.NewElement(tag.Tag{Group: 0x0009, Element: 0x0010}, []string{"PRIVATE"}); perr == nil {
		elems = append(elems, priv)
		hasPrivate = true
	}
	elems = append(elems, pd)

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, sdicom.Dataset{Elements: elems},
		sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write test DICOM: %v", err)
	}
	return hasPrivate
}

func TestRunModificationBaseDeident(t *testing.T) {
	profiles, aliases := embeddedModConfigs(t)
	resolved, err := resolveModProfile("base-deident", profiles)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	params, err := compileModifyParams(resolved, aliases)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir := t.TempDir()
	outDir := t.TempDir()
	subDir := filepath.Join(rootDir, "PAT", "STUDY")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(subDir, "img1.dcm")
	hasPrivate := writeModifyTestDICOM(t, srcPath)

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v, want 1 processed 0 failed", res)
	}

	// Output mirrors the source path relative to rootDir.
	outPath := filepath.Join(outDir, "PAT", "STUDY", "img1.dcm")
	ds, err := sdicom.ParseFile(outPath, nil)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	getStrings := func(tg tag.Tag) []string {
		e, err := ds.FindElementByTag(tg)
		if err != nil {
			return nil
		}
		return sdicom.MustGetStrings(e.Value)
	}
	getOne := func(tg tag.Tag) string {
		v := getStrings(tg)
		if len(v) == 0 {
			return ""
		}
		return strings.TrimSpace(v[0])
	}

	if got := getOne(tag.PatientName); got != "ANON" {
		t.Errorf("PatientName = %q, want ANON", got)
	}
	if got := getOne(tag.PatientID); got != "ANON" {
		t.Errorf("PatientID = %q, want ANON", got)
	}
	if got := getOne(tag.AccessionNumber); got != "" {
		t.Errorf("AccessionNumber = %q, want empty", got)
	}
	if got := getOne(tag.PatientBirthDate); got != "19800101" {
		t.Errorf("PatientBirthDate = %q, want 19800101 (year kept, day masked)", got)
	}
	if _, err := ds.FindElementByTag(tag.InstitutionName); err == nil {
		t.Errorf("InstitutionName still present — profile marks 0008,0080 for removal")
	}
	if hasPrivate {
		if _, err := ds.FindElementByTag(tag.Tag{Group: 0x0009, Element: 0x0010}); err == nil {
			t.Errorf("private tag still present — noprivate should remove odd groups")
		}
	}
	if got := getOne(tag.Tag{Group: 0x0040, Element: 0x1008}); got != "Y" {
		t.Errorf("ConfidentialityCode = %q, want Y", got)
	}

	// UID remapping: identity UIDs replaced consistently, structural UIDs kept.
	study, series := getOne(tag.StudyInstanceUID), getOne(tag.SeriesInstanceUID)
	if study == "1.2.3.4" || !strings.HasPrefix(study, "2.25.") {
		t.Errorf("StudyInstanceUID = %q, want fresh 2.25.* UID", study)
	}
	if series == "1.2.3.4.1" || !strings.HasPrefix(series, "2.25.") {
		t.Errorf("SeriesInstanceUID = %q, want fresh 2.25.* UID", series)
	}
	if study == series {
		t.Errorf("study and series UIDs remapped to the same value %q", study)
	}
	if sop, meta := getOne(tag.SOPInstanceUID), getOne(tag.MediaStorageSOPInstanceUID); sop != meta {
		t.Errorf("SOPInstanceUID %q != MediaStorageSOPInstanceUID %q — remap must be consistent", sop, meta)
	}
	if got := getOne(tag.SOPClassUID); got != "1.2.840.10008.5.1.4.1.1.7" {
		t.Errorf("SOPClassUID = %q, want unchanged standard UID", got)
	}
	if got := getOne(tag.TransferSyntaxUID); got != tsExplicitVRLE {
		t.Errorf("TransferSyntaxUID = %q, want unchanged %q", got, tsExplicitVRLE)
	}

	// Source untouched.
	src, err := sdicom.ParseFile(srcPath, nil)
	if err != nil {
		t.Fatalf("re-parse source: %v", err)
	}
	if e, err := src.FindElementByTag(tag.PatientName); err != nil ||
		strings.TrimSpace(sdicom.MustGetStrings(e.Value)[0]) != "DOE^JANE" {
		t.Errorf("source file was modified")
	}
}

func TestRunModificationToZip(t *testing.T) {
	profiles, aliases := embeddedModConfigs(t)
	resolved, err := resolveModProfile("base-deident", profiles)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	params, err := compileModifyParams(resolved, aliases)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir := t.TempDir()
	outDir := t.TempDir()
	subDir := filepath.Join(rootDir, "PAT", "STUDY")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(subDir, "img1.dcm")
	writeModifyTestDICOM(t, srcPath)

	zipPath := filepath.Join(outDir, "export.zip")
	res := runModificationToZip(context.Background(), []string{srcPath}, rootDir, zipPath, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v, want 1 processed 0 failed", res)
	}

	// The archive replaces the folder tree: one entry at the rels layout with
	// forward slashes, parseable, and de-identified.
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	if len(zr.File) != 1 {
		t.Fatalf("zip entries = %d, want 1", len(zr.File))
	}
	if got := zr.File[0].Name; got != "PAT/STUDY/img1.dcm" {
		t.Errorf("entry name = %q, want PAT/STUDY/img1.dcm", got)
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatalf("open entry: %v", err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}
	ds, err := sdicom.Parse(bytes.NewReader(data), int64(len(data)), nil)
	if err != nil {
		t.Fatalf("parse entry: %v", err)
	}
	e, err := ds.FindElementByTag(tag.PatientName)
	if err != nil {
		t.Fatalf("PatientName missing: %v", err)
	}
	if got := strings.TrimSpace(sdicom.MustGetStrings(e.Value)[0]); got != "ANON" {
		t.Errorf("PatientName = %q, want ANON", got)
	}

	// No temp archive left beside the result.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "export.zip" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("output folder = %v, want only export.zip", names)
	}
}

// A profile's transfer syntax converts the export while the de-identification
// still applies, and the source in the download folder is left in its own
// syntax — the whole point of converting here rather than on the retrieve.
func TestRunModificationTransferSyntax(t *testing.T) {
	_, aliases := embeddedModConfigs(t)

	// The fixture is Explicit VR LE, so Implicit VR LE is a real conversion.
	params, err := compileModifyParams(ModProfile{
		Sets:           []string{"PatientName=ANON"},
		TransferSyntax: tsPrefImplicitLE,
	}, aliases)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath := filepath.Join(rootDir, "img1.dcm")
	writeModifyTestDICOM(t, srcPath)

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v, want 1 processed 0 failed", res)
	}

	outPath := filepath.Join(outDir, "img1.dcm")
	if got := fileTransferSyntaxUID(outPath); got != tsImplicitVRLE {
		t.Errorf("exported transfer syntax = %q, want %q", got, tsImplicitVRLE)
	}
	if got := fileTransferSyntaxUID(srcPath); got != tsExplicitVRLE {
		t.Errorf("source transfer syntax = %q, want it untouched at %q", got, tsExplicitVRLE)
	}

	ds, err := sdicom.ParseFile(outPath, nil)
	if err != nil {
		t.Fatalf("parse export: %v", err)
	}
	e, err := ds.FindElementByTag(tag.PatientName)
	if err != nil {
		t.Fatalf("PatientName missing: %v", err)
	}
	if got := strings.TrimSpace(sdicom.MustGetStrings(e.Value)[0]); got != "ANON" {
		t.Errorf("PatientName = %q, want ANON — the tag rules must survive the conversion", got)
	}
	// Uncompressed-to-uncompressed is a re-encode, never a pixel transform.
	if got := modifyTestPixels(t, &ds); !slices.Equal(got, []uint8{10, 20, 30, 40}) {
		t.Errorf("pixels = %v, want [10 20 30 40] unchanged by the VR conversion", got)
	}
}

// The zip sink writes through the same dataset, so the archived entry has to
// carry the converted syntax too — it has its own write path.
func TestRunModificationToZipTransferSyntax(t *testing.T) {
	_, aliases := embeddedModConfigs(t)
	params, err := compileModifyParams(ModProfile{TransferSyntax: tsPrefImplicitLE}, aliases)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath := filepath.Join(rootDir, "img1.dcm")
	writeModifyTestDICOM(t, srcPath)

	zipPath := filepath.Join(outDir, "export.zip")
	res := runModificationToZip(context.Background(), []string{srcPath}, rootDir, zipPath, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v, want 1 processed 0 failed", res)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	if len(zr.File) != 1 {
		t.Fatalf("zip entries = %d, want 1", len(zr.File))
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatalf("open entry: %v", err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}
	ds, err := sdicom.Parse(bytes.NewReader(data), int64(len(data)), nil)
	if err != nil {
		t.Fatalf("parse entry: %v", err)
	}
	if got := datasetTransferSyntaxUID(&ds); got != tsImplicitVRLE {
		t.Errorf("archived transfer syntax = %q, want %q", got, tsImplicitVRLE)
	}
}

// A source whose pixel data has no built-in decoder fails that file rather
// than exporting it in a syntax the profile did not ask for. The failure has to
// name the syntax: that is what tells the user the export cannot simply be
// retried.
func TestRunModificationTransferSyntaxUndecodable(t *testing.T) {
	_, aliases := embeddedModConfigs(t)
	params, err := compileModifyParams(ModProfile{TransferSyntax: tsPrefExplicitLE}, aliases)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath := filepath.Join(rootDir, "rle.dcm")
	writeEncapsulatedTestDICOM(t, srcPath, "1.2.840.10008.1.2.5") // RLE — no decoder

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Processed != 0 || res.Failed != 1 {
		t.Fatalf("result = %+v, want 0 processed 1 failed", res)
	}
	if len(res.Failures) != 1 || !strings.Contains(res.Failures[0].Error, "RLE Lossless") {
		t.Errorf("failure = %+v, want one naming RLE Lossless", res.Failures)
	}
	// Nothing may be left in the export folder for a file that failed.
	if entries, rerr := os.ReadDir(outDir); rerr == nil && len(entries) != 0 {
		t.Errorf("export folder holds %d entries, want none", len(entries))
	}
}

// writeEncapsulatedTestDICOM writes a file whose pixel data is encapsulated
// under transferSyntax. The fragment content is arbitrary: the decoder gate
// rejects the syntax before anything is decoded, which is what the undecodable
// path exercises.
func writeEncapsulatedTestDICOM(t *testing.T, path, transferSyntax string) {
	t.Helper()
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		IsEncapsulated: true,
		Frames: []*frame.Frame{{
			Encapsulated:     true,
			EncapsulatedData: frame.EncapsulatedFrame{Data: []byte{0x00, 0x01, 0x02, 0x03}},
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
		mustTestElement(t, tag.TransferSyntaxUID, []string{transferSyntax}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.9"}),
		mustTestElement(t, tag.PatientName, []string{"DOE^JANE"}),
		mustTestElement(t, tag.Modality, []string{"OT"}),
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

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write encapsulated fixture: %v", err)
	}
}

// modifyTestPixels reads back the 4 native 8-bit samples writeModifyTestDICOM
// stores, so pixel-preservation assertions read as one line.
func modifyTestPixels(t *testing.T, ds *sdicom.Dataset) []uint8 {
	t.Helper()
	e, err := ds.FindElementByTag(tag.PixelData)
	if err != nil {
		t.Fatalf("PixelData missing: %v", err)
	}
	info, ok := e.Value.GetValue().(sdicom.PixelDataInfo)
	if !ok || len(info.Frames) != 1 {
		t.Fatalf("unexpected PixelData: ok=%v frames=%d", ok, len(info.Frames))
	}
	nf, ok := info.Frames[0].NativeData.(*frame.NativeFrame[uint8])
	if !ok {
		t.Fatalf("frame is not 8-bit native: %T", info.Frames[0].NativeData)
	}
	return nf.RawData
}

// TestExportRelPaths verifies the PHI-safe export layout: a study-level run
// drops the patient and study folder names entirely, a patient-level run
// replaces each study folder with a deterministic generic study-NN, and files
// outside the expected layout (flat fallback) map to their bare file name.
func TestExportRelPaths(t *testing.T) {
	root := filepath.Join("dl")
	patient := "DOE^JOHN (MRN12345)"
	studyA := "CT ABDOMEN (20240101)"
	studyB := "CT CHEST (20230601)"
	fA1 := filepath.Join(root, patient, studyA, "AX W CONTRAST (2)", "1.dcm")
	fA2 := filepath.Join(root, patient, studyA, "SCOUT (1)", "2.dcm")
	fB1 := filepath.Join(root, patient, studyB, "AX (3)", "sub", "3.dcm")
	flat := filepath.Join(root, "4.dcm")
	outside := filepath.Join("elsewhere", "5.dcm")

	// Study level: only the series structure survives.
	rels := exportRelPaths([]string{fA1, fA2, flat}, root, true)
	if got, want := rels[fA1], filepath.Join("AX W CONTRAST (2)", "1.dcm"); got != want {
		t.Errorf("study-level rel = %q, want %q", got, want)
	}
	if got, want := rels[fA2], filepath.Join("SCOUT (1)", "2.dcm"); got != want {
		t.Errorf("study-level rel = %q, want %q", got, want)
	}
	if got := rels[flat]; got != "4.dcm" {
		t.Errorf("flat-fallback rel = %q, want 4.dcm", got)
	}

	// Patient level: study folders become study-NN in sorted folder-name
	// order (studyA sorts before studyB), deeper nesting is preserved.
	rels = exportRelPaths([]string{fB1, fA1, outside}, root, false)
	if got, want := rels[fA1], filepath.Join("study-01", "AX W CONTRAST (2)", "1.dcm"); got != want {
		t.Errorf("patient-level rel = %q, want %q", got, want)
	}
	if got, want := rels[fB1], filepath.Join("study-02", "AX (3)", "sub", "3.dcm"); got != want {
		t.Errorf("patient-level rel = %q, want %q", got, want)
	}
	if got := rels[outside]; got != "5.dcm" {
		t.Errorf("outside-root rel = %q, want 5.dcm", got)
	}

	// No PHI-bearing component may survive in any mapped path.
	for _, m := range rels {
		if strings.Contains(m, patient) || strings.Contains(m, studyA) || strings.Contains(m, studyB) {
			t.Errorf("mapped path %q leaks a source folder name", m)
		}
	}
}

// TestShiftDateString mirrors dicomtool's table so the two implementations
// stay provably identical.
func TestShiftDateString(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		shiftDays int
		want      string
		wantOK    bool
	}{
		{"positive shift", "20200115", 10, "20200125", true},
		{"negative shift", "20200115", -10, "20200105", true},
		{"month rollover", "20200130", 5, "20200204", true},
		{"year rollover", "20201228", 10, "20210107", true},
		{"leap year Feb 29 plus one", "20200229", 1, "20200301", true},
		{"zero shift is a no-op", "20200115", 0, "20200115", true},
		{"DT value keeps time/fraction/zone suffix", "20200115120000.000000+0000", -1, "20200114120000.000000+0000", true},
		{"too short", "2020011", 1, "2020011", false},
		{"empty", "", 1, "", false},
		{"non-numeric date portion", "2020AB15", 1, "2020AB15", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := shiftDateString(tc.in, tc.shiftDays)
			if ok != tc.wantOK {
				t.Fatalf("shiftDateString(%q, %d) ok = %v, want %v", tc.in, tc.shiftDays, ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("shiftDateString(%q, %d) = %q, want %q", tc.in, tc.shiftDays, got, tc.want)
			}
		})
	}
}

// TestApplyDateShift covers the element walk: DA and DT shift (DT keeping its
// time suffix), PatientBirthDate and non-date elements stay, and the shift
// recurses into sequence items.
func TestApplyDateShift(t *testing.T) {
	findTag := func(elems []*sdicom.Element, tg tag.Tag) *sdicom.Element {
		for _, e := range elems {
			if e.Tag == tg {
				return e
			}
		}
		return nil
	}
	strValue := func(e *sdicom.Element) string {
		if e == nil {
			return ""
		}
		if v, ok := e.Value.GetValue().([]string); ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}

	seqTag := tag.Tag{Group: 0x0008, Element: 0x1140}
	nested := []*sdicom.Element{
		mustTestElement(t, tag.StudyDate, []string{"20200115"}),
	}
	elems := []*sdicom.Element{
		mustTestElement(t, tag.StudyDate, []string{"20200115"}),
		mustTestElement(t, tag.AcquisitionDateTime, []string{"20200115120000.000000+0000"}),
		mustTestElement(t, tag.PatientBirthDate, []string{"19800101"}),
		mustTestElement(t, tag.PatientName, []string{"Doe^Jane"}),
		mustTestElement(t, seqTag, [][]*sdicom.Element{nested}),
	}

	applyDateShift(elems, 10)

	if got := strValue(findTag(elems, tag.StudyDate)); got != "20200125" {
		t.Errorf("StudyDate = %q, want 20200125", got)
	}
	if got := strValue(findTag(elems, tag.AcquisitionDateTime)); got != "20200125120000.000000+0000" {
		t.Errorf("AcquisitionDateTime = %q, want date shifted with time preserved", got)
	}
	if got := strValue(findTag(elems, tag.PatientBirthDate)); got != "19800101" {
		t.Errorf("PatientBirthDate was shifted: %q, want unchanged", got)
	}
	if got := strValue(findTag(elems, tag.PatientName)); got != "Doe^Jane" {
		t.Errorf("PatientName was changed: %q", got)
	}
	items, ok := findTag(elems, seqTag).Value.GetValue().([]*sdicom.SequenceItemValue)
	if !ok || len(items) != 1 {
		t.Fatalf("sequence element lost its items")
	}
	itemElems, ok := items[0].GetValue().([]*sdicom.Element)
	if !ok {
		t.Fatalf("sequence item is not []*Element")
	}
	if got := strValue(findTag(itemElems, tag.StudyDate)); got != "20200125" {
		t.Errorf("nested StudyDate = %q, want 20200125 (recursion into sequence failed)", got)
	}
}

// TestRunModificationShiftDays runs the engine end-to-end with only a date
// shift: every DA/DT moves by the offset (including inside sequences), the
// birth date does not, and the source file is untouched.
func TestRunModificationShiftDays(t *testing.T) {
	_, aliases := embeddedModConfigs(t)
	params, err := compileModifyParams(ModProfile{ShiftDays: "-45"}, aliases)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir := t.TempDir()
	outDir := t.TempDir()
	srcPath := filepath.Join(rootDir, "img1.dcm")
	writeModifyTestDICOM(t, srcPath)

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v, want 1 processed 0 failed", res)
	}

	ds, err := sdicom.ParseFile(filepath.Join(outDir, "img1.dcm"), nil)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	getOne := func(tg tag.Tag) string {
		e, err := ds.FindElementByTag(tg)
		if err != nil {
			return ""
		}
		v := sdicom.MustGetStrings(e.Value)
		if len(v) == 0 {
			return ""
		}
		return strings.TrimSpace(v[0])
	}

	if got := getOne(tag.StudyDate); got != "20231118" {
		t.Errorf("StudyDate = %q, want 20231118 (20240102 - 45d)", got)
	}
	if got := getOne(tag.AcquisitionDateTime); got != "20231118093000.000000+0000" {
		t.Errorf("AcquisitionDateTime = %q, want shifted date with suffix intact", got)
	}
	if got := getOne(tag.PatientBirthDate); got != "19800615" {
		t.Errorf("PatientBirthDate = %q, want untouched 19800615", got)
	}
	seqElem, err := ds.FindElementByTag(tag.Tag{Group: 0x0008, Element: 0x1140})
	if err != nil {
		t.Fatalf("sequence missing from output: %v", err)
	}
	items, ok := seqElem.Value.GetValue().([]*sdicom.SequenceItemValue)
	if !ok || len(items) != 1 {
		t.Fatalf("sequence items lost in output")
	}
	itemElems, _ := items[0].GetValue().([]*sdicom.Element)
	found := ""
	for _, e := range itemElems {
		if e.Tag == tag.StudyDate {
			if v, ok := e.Value.GetValue().([]string); ok && len(v) > 0 {
				found = strings.TrimSpace(v[0])
			}
		}
	}
	if found != "20231118" {
		t.Errorf("nested StudyDate = %q, want 20231118", found)
	}

	// Source untouched.
	src, err := sdicom.ParseFile(srcPath, nil)
	if err != nil {
		t.Fatalf("re-parse source: %v", err)
	}
	if e, err := src.FindElementByTag(tag.StudyDate); err != nil ||
		strings.TrimSpace(sdicom.MustGetStrings(e.Value)[0]) != "20240102" {
		t.Errorf("source file was modified")
	}
}

// TestRunModificationShiftDaysPerModality proves a per-modality shiftdays
// override wins for a matching file, and that a garbage override value fails
// the file (rather than silently shipping unshifted dates) — dicomtool parity.
func TestRunModificationShiftDaysPerModality(t *testing.T) {
	_, aliases := embeddedModConfigs(t)

	rootDir := t.TempDir()
	srcPath := filepath.Join(rootDir, "img1.dcm")
	writeModifyTestDICOM(t, srcPath) // fixture Modality is OT

	params, err := compileModifyParams(ModProfile{
		ShiftDays:   "-45",
		PerModality: map[string]ModProfile{"OT": {ShiftDays: "10"}},
	}, aliases)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	outDir := t.TempDir()
	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v, want 1 processed 0 failed", res)
	}
	ds, err := sdicom.ParseFile(filepath.Join(outDir, "img1.dcm"), nil)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	e, err := ds.FindElementByTag(tag.StudyDate)
	if err != nil {
		t.Fatalf("StudyDate missing: %v", err)
	}
	if got := strings.TrimSpace(sdicom.MustGetStrings(e.Value)[0]); got != "20240112" {
		t.Errorf("StudyDate = %q, want 20240112 (override +10 wins over -45)", got)
	}

	// A hand-authored per-modality block with a garbage shiftdays fails the
	// file: compileModifyParams only validates the top-level value.
	params, err = compileModifyParams(ModProfile{
		Sets:        []string{"0010,0010=X"},
		PerModality: map[string]ModProfile{"OT": {ShiftDays: "x"}},
	}, aliases)
	if err != nil {
		t.Fatalf("compile with bad override: %v", err)
	}
	res = runModification(context.Background(), []string{srcPath}, rootDir, t.TempDir(), params, nil, nil)
	if res.Failed != 1 || res.Processed != 0 {
		t.Fatalf("result = %+v, want 0 processed 1 failed", res)
	}
	if len(res.Failures) != 1 || !strings.Contains(res.Failures[0].Error, "must be an integer") {
		t.Errorf("failure = %+v, want a 'must be an integer' entry", res.Failures)
	}
}

// TestValidateExportFolderName covers accepted names and each rejection rule.
func TestValidateExportFolderName(t *testing.T) {
	for _, ok := range []string{"anon-20260727-120000", "case 42", "a.b", "study_1"} {
		if err := validateExportFolderName(ok); err != nil {
			t.Errorf("validateExportFolderName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", `a\b`, "a/b", "a:b", "a*b", "a?b", `a"b`, "a<b", "a>b", "a|b", "name.", "CON", "con.export"} {
		if err := validateExportFolderName(bad); err == nil {
			t.Errorf("validateExportFolderName(%q) = nil, want error", bad)
		}
	}
}
