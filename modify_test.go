package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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
		{"no action", ModProfile{}, "no actionable parameter"},
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
