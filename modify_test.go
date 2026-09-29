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

// embeddedModConfigs unmarshals the compiled-in default profiles.
func embeddedModConfigs(t *testing.T) ModProfileConfig {
	t.Helper()
	var profiles ModProfileConfig
	if err := json.Unmarshal(defaultModProfilesJSON, &profiles); err != nil {
		t.Fatalf("unmarshal embedded profiles.json: %v", err)
	}
	return profiles
}

func TestResolveModProfileEmbeddedDefaults(t *testing.T) {
	profiles := embeddedModConfigs(t)

	base, err := resolveModProfile("base-deident", profiles)
	if err != nil {
		t.Fatalf("resolve base-deident: %v", err)
	}
	// The set values lead with Patient Name — the order the profile stores is
	// the order the editor and the Modification dialog present.
	if len(base.Sets) != 4 || base.Sets[0] != "0010,0010=ANON" {
		t.Errorf("base-deident sets = %v, want 4 entries led by Patient Name", base.Sets)
	}
	if base.DOB != "YYYY0101" || !base.Priv || !base.RemapUIDs || base.FixVR != "correct" || !base.NoOverlays {
		t.Errorf("base-deident options = dob %q priv %v remap %v fixvr %q nooverlays %v",
			base.DOB, base.Priv, base.RemapUIDs, base.FixVR, base.NoOverlays)
	}
	if want := []string{"SR", "PR", "KO", "OT"}; !slices.Equal(base.IgnoreModalities, want) {
		t.Errorf("base-deident ignoremodality = %v, want %v", base.IgnoreModalities, want)
	}

	// deident-US layers the calibrated-ultrasound mask rule on the base
	// profile; everything else is inherited unchanged.
	us, err := resolveModProfile("deident-US", profiles)
	if err != nil {
		t.Fatalf("resolve deident-US: %v", err)
	}
	if len(us.MaskRegions) != 1 || maskRegionMode(us.MaskRegions[0]) != maskModeOutsideUS {
		t.Errorf("deident-US mask regions = %+v, want one %s rule", us.MaskRegions, maskModeOutsideUS)
	}
	if us.DOB != "YYYY0101" || !us.RemapUIDs || !us.NoOverlays || len(us.Removes) != len(base.Removes) {
		t.Errorf("deident-US inherited options = dob %q remap %v nooverlays %v removes %d (base %d)",
			us.DOB, us.RemapUIDs, us.NoOverlays, len(us.Removes), len(base.Removes))
	}
}

func TestCompileModifyParamsValidation(t *testing.T) {

	cases := []struct {
		name string
		p    ModProfile
		want string // substring of the expected error; "" = must succeed
	}{
		{"bad dob length", ModProfile{DOB: "1980"}, "8 characters"},
		// The uid suffix option is removed: a profile still carrying one is
		// refused loudly rather than run with the entry silently ignored —
		// at the top level and inside a hand-authored per-modality block.
		{"uid suffix removed", ModProfile{UIDSuffix: "7"}, "has been removed"},
		{"per-modality uid suffix removed",
			ModProfile{PerModality: map[string]ModProfile{"CT": {UIDSuffix: "7"}}}, "has been removed"},
		{"bad fixvr", ModProfile{FixVR: "maybe"}, "must be correct"},
		{"bad shiftdays", ModProfile{ShiftDays: "abc"}, "must be an integer"},
		{"bad transfersyntax", ModProfile{TransferSyntax: "jpeg2000"}, "must be explicit-le"},
		{"no action", ModProfile{}, "no actionable parameter"},
		{"zip alone is not an action", ModProfile{Zip: true}, "no actionable parameter"},
		{"flat alone is not an action", ModProfile{Flat: true}, "no actionable parameter"},
		// Unlike zip, a transfer syntax alone is a real transformation: export is
		// the only place the application can convert a file.
		{"transfersyntax only is actionable", ModProfile{TransferSyntax: tsPrefImplicitLE}, ""},
		{"nooverlays only is actionable", ModProfile{NoOverlays: true}, ""},
		// Like transfersyntax, a bare dicomdir request is a real standalone
		// export operation (unmodified files plus an index), not a no-op.
		{"dicomdir only is actionable", ModProfile{Dicomdir: true}, ""},
		{"shiftdays only is actionable", ModProfile{ShiftDays: "-45"}, ""},
		{"shiftdays zero accepted", ModProfile{ShiftDays: "0"}, ""},
		// A bare keyword used to resolve through tags.json. With aliases gone it
		// is simply not a tag, and must be reported rather than quietly matching
		// nothing.
		{"keyword is no longer a tag", ModProfile{Sets: []string{"PatientName=X"}}, "invalid tag"},
		{"canonical set", ModProfile{Sets: []string{"0010,0010=X"}}, ""},
		{"short-form remove", ModProfile{Removes: []string{"8,80"}}, ""},
	}
	for _, tc := range cases {
		_, err := compileModifyParams(tc.p)
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
// writeModifyTestDICOM writes the fixture as CT — a modality the shipped
// base-deident processes. Its ignoremodality list skips SR/PR/KO/OT, so the
// fixture's original OT made every end-to-end run of the shipped profile a
// silent skip; tests that need an excluded modality ask for it explicitly.
func writeModifyTestDICOM(t *testing.T, path string) (hasPrivate bool) {
	return writeModifyTestDICOMModality(t, path, "CT")
}

func writeModifyTestDICOMModality(t *testing.T, path, modality string) (hasPrivate bool) {
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
		mustTestElement(t, tag.Modality, []string{modality}),
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

// TestRunModificationNoOverlays: nooverlays removes every overlay-plane group
// (6000–60FE, even) — the bitmap channel a vendor can burn patient text into,
// which noprivate never touches (even groups) and no per-tag rule reaches
// practically — and leaves the planes alone when unset, since a profile shared
// with dicomtool must not change meaning by being run here.
func TestRunModificationNoOverlays(t *testing.T) {
	overlayRows := tag.Tag{Group: 0x6000, Element: 0x0010}
	overlayData := tag.Tag{Group: 0x6000, Element: 0x3000}

	writeFixture := func(t *testing.T, path string) {
		writeModifyTestDICOM(t, path)
		ds, err := sdicom.ParseFile(path, nil)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		if err := setElementValue(&ds, overlayRows, []int{2}); err != nil {
			t.Fatalf("set overlay rows: %v", err)
		}
		if err := setElementValue(&ds, overlayData, []byte{0x03}); err != nil {
			t.Fatalf("set overlay data: %v", err)
		}
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		defer f.Close()
		if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}

	for _, tc := range []struct {
		name     string
		profile  ModProfile
		wantGone bool
	}{
		{"removed when set", ModProfile{NoOverlays: true}, true},
		{"kept when unset", ModProfile{Sets: []string{"0010,0010=ANON"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rootDir, outDir := t.TempDir(), t.TempDir()
			srcPath := filepath.Join(rootDir, "ov.dcm")
			writeFixture(t, srcPath)

			params, err := compileModifyParams(tc.profile)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
			if res.Processed != 1 || res.Failed != 0 {
				t.Fatalf("result = %+v (%v), want the file exported", res, res.Failures)
			}
			out, err := sdicom.ParseFile(filepath.Join(outDir, "ov.dcm"), nil)
			if err != nil {
				t.Fatalf("parse export: %v", err)
			}
			overlayLeft := false
			for _, el := range out.Elements {
				if isOverlayGroup(el.Tag.Group) {
					overlayLeft = true
				}
			}
			if tc.wantGone && overlayLeft {
				t.Errorf("overlay-plane elements survived nooverlays")
			}
			if !tc.wantGone && !overlayLeft {
				t.Errorf("overlay-plane elements missing from an export that never asked for nooverlays")
			}
			if _, ferr := out.FindElementByTag(tag.PatientID); ferr != nil {
				t.Errorf("PatientID missing — nooverlays must remove only overlay groups")
			}
		})
	}

	// The group test is the whole safety boundary: odd 60xx groups are private
	// tags (noprivate's business), and nearby even groups are not overlays.
	if isOverlayGroup(0x6001) || isOverlayGroup(0x5000) || isOverlayGroup(0x6100) {
		t.Errorf("isOverlayGroup admits a non-overlay group")
	}
	if !isOverlayGroup(0x6000) || !isOverlayGroup(0x60FE) {
		t.Errorf("isOverlayGroup rejects a real overlay group")
	}
}

func TestRunModificationBaseDeident(t *testing.T) {
	profiles := embeddedModConfigs(t)
	resolved, err := resolveModProfile("base-deident", profiles)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	params, err := compileModifyParams(resolved)
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

// The shipped profile's ignoremodality list (SR/PR/KO/OT) skips such files
// rather than exporting them — a skip, never a failure.
func TestRunModificationBaseDeidentSkipsOT(t *testing.T) {
	profiles := embeddedModConfigs(t)
	resolved, err := resolveModProfile("base-deident", profiles)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	params, err := compileModifyParams(resolved)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir := t.TempDir()
	srcPath := filepath.Join(rootDir, "ot.dcm")
	writeModifyTestDICOMModality(t, srcPath, "OT")
	res := runModification(context.Background(), []string{srcPath}, rootDir, t.TempDir(), params, nil, nil)
	if res.Skipped != 1 || res.Processed != 0 || res.Failed != 0 {
		t.Fatalf("result = %+v, want the OT file skipped", res)
	}
}

func TestRunModificationToZip(t *testing.T) {
	profiles := embeddedModConfigs(t)
	resolved, err := resolveModProfile("base-deident", profiles)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	params, err := compileModifyParams(resolved)
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

// TestRunModificationDicomdir: a folder-mode run with Dicomdir set writes a
// DICOMDIR at the export root that references the file actually written —
// the folder-export counterpart of TestRunModificationToZipDicomdir.
func TestRunModificationDicomdir(t *testing.T) {
	params, err := compileModifyParams(ModProfile{Sets: []string{"0010,0010=ANON"}, Dicomdir: true})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
	subDir := filepath.Join(rootDir, "PAT", "STUDY")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(subDir, "img1.dcm")
	writeModifyTestDICOM(t, srcPath)

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v, want 1 processed 0 failed", res)
	}
	if !res.DicomdirWritten || res.DicomdirError != "" {
		t.Fatalf("DicomdirWritten=%v DicomdirError=%q, want written with no error", res.DicomdirWritten, res.DicomdirError)
	}

	records, firstIdx, _ := parseDICOMDIR(t, mustReadFile(t, filepath.Join(outDir, "DICOMDIR")))
	// patient → study → series → image: three .child hops down from the root.
	studyIdx := records[firstIdx].child
	seriesIdx := records[studyIdx].child
	imageIdx := records[seriesIdx].child
	imgRecs := walkSiblings(records, imageIdx)
	if len(imgRecs) != 1 || records[imgRecs[0]].recordType != "IMAGE" {
		t.Fatalf("image records = %+v, want exactly one IMAGE record", imgRecs)
	}
	refID := records[imgRecs[0]].fields[tag.ReferencedFileID]
	if got := filepath.Join(refID...); got != filepath.Join("PAT", "STUDY", "img1.dcm") {
		t.Errorf("ReferencedFileID = %v, want PAT/STUDY/img1.dcm", refID)
	}
	if _, err := os.Stat(filepath.Join(outDir, filepath.Join(refID...))); err != nil {
		t.Errorf("DICOMDIR references a file that was not written: %v", err)
	}
}

// TestRunModificationDicomdirNoFiles: a run where the only file is skipped
// leaves no DICOMDIR behind — an index with nothing in it is worse than no
// index, since its mere presence would claim the export succeeded.
func TestRunModificationDicomdirNoFiles(t *testing.T) {
	profiles := embeddedModConfigs(t)
	resolved, err := resolveModProfile("base-deident", profiles) // ignores OT
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	resolved.Dicomdir = true
	params, err := compileModifyParams(resolved)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
	srcPath := filepath.Join(rootDir, "ot.dcm")
	writeModifyTestDICOMModality(t, srcPath, "OT")

	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
	if res.Skipped != 1 || res.Processed != 0 {
		t.Fatalf("result = %+v, want the OT file skipped", res)
	}
	if res.DicomdirWritten {
		t.Error("DicomdirWritten = true, want false — nothing was exported to index")
	}
	if _, err := os.Stat(filepath.Join(outDir, "DICOMDIR")); !os.IsNotExist(err) {
		t.Errorf("DICOMDIR exists for a run that wrote nothing: %v", err)
	}
}

// TestRunModificationToZipDicomdir: with Zip export and Dicomdir both set,
// the DICOMDIR lands as its own entry inside the archive — the combination
// dicomtool's CLI refuses but dicomqr supports, since it is the shape a
// CD/DVD-burning workflow actually wants.
func TestRunModificationToZipDicomdir(t *testing.T) {
	params, err := compileModifyParams(ModProfile{Sets: []string{"0010,0010=ANON"}, Dicomdir: true})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
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
	if !res.DicomdirWritten || res.DicomdirError != "" {
		t.Fatalf("DicomdirWritten=%v DicomdirError=%q, want written with no error", res.DicomdirWritten, res.DicomdirError)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	if len(zr.File) != 2 {
		names := make([]string, 0, len(zr.File))
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		t.Fatalf("zip entries = %v, want 2 (the file and DICOMDIR)", names)
	}
	var ddEntry *zip.File
	for _, f := range zr.File {
		if f.Name == "DICOMDIR" {
			ddEntry = f
		}
	}
	if ddEntry == nil {
		t.Fatal("no DICOMDIR entry in the archive")
	}
	rc, err := ddEntry.Open()
	if err != nil {
		t.Fatalf("open DICOMDIR entry: %v", err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read DICOMDIR entry: %v", err)
	}
	records, firstIdx, _ := parseDICOMDIR(t, data)
	// patient → study → series → image: three .child hops down from the root.
	studyIdx := records[firstIdx].child
	seriesIdx := records[studyIdx].child
	imageIdx := records[seriesIdx].child
	imgRecs := walkSiblings(records, imageIdx)
	if len(imgRecs) != 1 || records[imgRecs[0]].recordType != "IMAGE" {
		t.Fatalf("image records = %+v, want exactly one IMAGE record", imgRecs)
	}
	if got := records[imgRecs[0]].fields[tag.ReferencedFileID]; filepath.ToSlash(filepath.Join(got...)) != "PAT/STUDY/img1.dcm" {
		t.Errorf("ReferencedFileID = %v, want PAT/STUDY/img1.dcm", got)
	}
}

// TestRunModificationFlat: a folder-mode run with a flat exportLayout writes
// every file directly into outDir, named after its SOP Instance UID, with no
// patient/study/series subfolders — even though the two source files here
// share a bare file name ("1.dcm") in different series, which would collide
// under a naive flatten-by-source-name approach.
func TestRunModificationFlat(t *testing.T) {
	params, err := compileModifyParams(ModProfile{Sets: []string{"0010,0010=ANON"}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir := t.TempDir()
	patient := "DOE^JANE (PID123)"
	studyA, seriesA := "CT ABDOMEN (20240101)", "AX W CONTRAST (2)"
	studyB, seriesB := "CT CHEST (20230601)", "AX (3)"
	srcA := filepath.Join(rootDir, patient, studyA, seriesA, "1.dcm")
	srcB := filepath.Join(rootDir, patient, studyB, seriesB, "1.dcm")
	writeExportLayoutFixture(t, srcA, "1.2.1", "1.2.1.1", "1.2.1.1.1", "CT ABDOMEN", "20240101", "AX W CONTRAST", "2")
	writeExportLayoutFixture(t, srcB, "1.2.2", "1.2.2.1", "1.2.2.1.1", "CT CHEST", "20230601", "AX", "3")

	outDir := t.TempDir()
	layout := &exportLayout{flat: true, names: &flatNames{}}
	res := runModification(context.Background(), []string{srcA, srcB}, rootDir, outDir, params, layout, nil)
	if res.Failed != 0 || res.Processed != 2 {
		t.Fatalf("result = %+v (%v), want 2 processed 0 failed", res, res.Failures)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read outDir: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("flat export left a subfolder: %s", e.Name())
		}
		got[e.Name()] = true
	}
	for _, want := range []string{"1.2.1.1.1.dcm", "1.2.2.1.1.dcm"} {
		if !got[want] {
			t.Errorf("outDir entries = %v, want %s among them", got, want)
		}
	}
}

// TestRunModificationToZipFlat is TestRunModificationFlat's zip counterpart:
// entries land at the archive root (no "/" in the name) instead of under a
// patient/study/series path.
func TestRunModificationToZipFlat(t *testing.T) {
	params, err := compileModifyParams(ModProfile{Sets: []string{"0010,0010=ANON"}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir := t.TempDir()
	subDir := filepath.Join(rootDir, "PAT", "STUDY", "SERIES")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(subDir, "img1.dcm")
	writeModifyTestDICOM(t, srcPath)

	outDir := t.TempDir()
	zipPath := filepath.Join(outDir, "export.zip")
	layout := &exportLayout{flat: true, names: &flatNames{}}
	res := runModificationToZip(context.Background(), []string{srcPath}, rootDir, zipPath, params, layout, nil)
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
	if name := zr.File[0].Name; strings.ContainsAny(name, `/\`) {
		t.Errorf("entry name = %q, want no path separators (flat export)", name)
	}
}

// TestRunModificationFlatDicomdir: flat export and Include DICOMDIR compose —
// the file-set's ReferencedFileID becomes a single component (no patient/
// study/series path), while the DICOMDIR's own patient/study/series/image
// record hierarchy is unaffected, since that comes from the dataset's tags
// rather than from the file's path (buildPatientsFromSources, dicomdir.go).
func TestRunModificationFlatDicomdir(t *testing.T) {
	params, err := compileModifyParams(ModProfile{Sets: []string{"0010,0010=ANON"}, Dicomdir: true})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir, outDir := t.TempDir(), t.TempDir()
	subDir := filepath.Join(rootDir, "PAT", "STUDY")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(subDir, "img1.dcm")
	writeModifyTestDICOM(t, srcPath)

	layout := &exportLayout{flat: true, names: &flatNames{}}
	res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, layout, nil)
	if res.Failed != 0 || res.Processed != 1 {
		t.Fatalf("result = %+v, want 1 processed 0 failed", res)
	}
	if !res.DicomdirWritten || res.DicomdirError != "" {
		t.Fatalf("DicomdirWritten=%v DicomdirError=%q, want written with no error", res.DicomdirWritten, res.DicomdirError)
	}

	records, firstIdx, _ := parseDICOMDIR(t, mustReadFile(t, filepath.Join(outDir, "DICOMDIR")))
	studyIdx := records[firstIdx].child
	seriesIdx := records[studyIdx].child
	imageIdx := records[seriesIdx].child
	imgRecs := walkSiblings(records, imageIdx)
	if len(imgRecs) != 1 || records[imgRecs[0]].recordType != "IMAGE" {
		t.Fatalf("image records = %+v, want exactly one IMAGE record", imgRecs)
	}
	refID := records[imgRecs[0]].fields[tag.ReferencedFileID]
	if len(refID) != 1 {
		t.Fatalf("ReferencedFileID = %v, want a single component (flat export)", refID)
	}
	if _, err := os.Stat(filepath.Join(outDir, refID[0])); err != nil {
		t.Errorf("DICOMDIR references a file that was not written: %v", err)
	}
}

// TestRunModificationSurvivesPanic drives the worker pool's per-file backstop.
// The DICOM parser panics rather than erroring on some malformed datasets, and
// the pool runs on background goroutines where an escaped panic kills the whole
// application mid-export. A panicking file must instead become one recorded
// failure while every other file in the run still exports.
//
// The panic is injected through processFileFn rather than by crafting a file
// that really panics the library: which corruption panics is a property of the
// library, not of this code, and a fixture chosen today could quietly stop
// panicking on an upgrade — leaving a test that passes without testing anything.
func TestRunModificationSurvivesPanic(t *testing.T) {
	rootDir, outDir := t.TempDir(), t.TempDir()
	goodPath := filepath.Join(rootDir, "good.dcm")
	badPath := filepath.Join(rootDir, "bad.dcm")
	writeModifyTestDICOM(t, goodPath)
	writeModifyTestDICOM(t, badPath)

	real := processFileFn
	t.Cleanup(func() { processFileFn = real })
	processFileFn = func(src *os.File, p modifyParams, r *uidRemapper) (bool, sdicom.Dataset, fileNotes, error) {
		// Close before panicking: the real implementation defers its close, and
		// an open handle would block t.TempDir cleanup on Windows.
		name := src.Name()
		src.Close()
		if filepath.Base(name) == "bad.dcm" {
			panic("synthetic parser panic")
		}
		f, err := os.Open(name)
		if err != nil {
			t.Fatalf("reopen %s: %v", name, err)
		}
		return real(f, p, r)
	}

	params, err := compileModifyParams(ModProfile{Sets: []string{"0010,0010=ANON"}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res := runModification(context.Background(), []string{goodPath, badPath}, rootDir, outDir, params, nil, nil)

	if res.Processed != 1 || res.Failed != 1 {
		t.Fatalf("result = %+v, want 1 processed and 1 failed", res)
	}
	if len(res.Failures) != 1 || !strings.Contains(res.Failures[0].Error, "panic") {
		t.Fatalf("failures = %+v, want one naming the panic", res.Failures)
	}
	if !strings.Contains(res.Failures[0].File, "bad.dcm") {
		t.Errorf("failure names %q, want the panicking file", res.Failures[0].File)
	}
	// The run continued: the other file is in the export, the panicking one is
	// not. A file that failed must never leave a partial behind.
	if _, err := os.Stat(filepath.Join(outDir, "good.dcm")); err != nil {
		t.Errorf("the surviving file was not exported: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "bad.dcm")); !os.IsNotExist(err) {
		t.Errorf("the panicking file left something in the export: %v", err)
	}
}

// TestHasNestedTag pins the "below the top level" meaning: an element present
// only at the top level must NOT count, since that is exactly the one the
// birth-date mask already rewrites.
func TestHasNestedTag(t *testing.T) {
	nested := []*sdicom.Element{
		mustTestElement(t, tag.PatientBirthDate, []string{"19800615"}),
		mustTestElement(t, tag.Tag{Group: 0x0400, Element: 0x0561}, [][]*sdicom.Element{{
			mustTestElement(t, tag.Tag{Group: 0x0400, Element: 0x0550}, [][]*sdicom.Element{{
				mustTestElement(t, tag.PatientBirthDate, []string{"19800615"}),
			}}),
		}}),
	}
	if !hasNestedTag(nested, tag.PatientBirthDate) {
		t.Error("a birth date two sequences deep was not found")
	}

	topOnly := []*sdicom.Element{
		mustTestElement(t, tag.PatientBirthDate, []string{"19800615"}),
		mustTestElement(t, tag.PatientName, []string{"DOE^JANE"}),
	}
	if hasNestedTag(topOnly, tag.PatientBirthDate) {
		t.Error("a top-level-only birth date counted as nested")
	}
}

// writeNestedDOBFixture writes a DICOM file carrying a birth date both at the
// top level and inside an Original Attributes Sequence — the shape a study
// that has already been de-identified once arrives in.
func writeNestedDOBFixture(t *testing.T, path string) {
	t.Helper()
	writeModifyTestDICOM(t, path)
	ds, err := sdicom.ParseFile(path, nil)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	origAttrs := mustTestElement(t, tag.Tag{Group: 0x0400, Element: 0x0561}, [][]*sdicom.Element{{
		mustTestElement(t, tag.Tag{Group: 0x0400, Element: 0x0550}, [][]*sdicom.Element{{
			mustTestElement(t, tag.PatientBirthDate, []string{"19800615"}),
		}}),
	}})
	ds.Elements = append(ds.Elements, origAttrs)

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// TestRunModificationNestedDOB: the birth-date mask rewrites the top-level
// element only, so a copy inside Original Attributes Sequence survives it. The
// run must say so — and must stop saying so once the profile removes the
// sequence, which is the fix the advisory points at.
func TestRunModificationNestedDOB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile ModProfile
		want    int
	}{
		{"mask without the removal", ModProfile{DOB: "YYYY0101"}, 1},
		{"mask with the removal",
			ModProfile{DOB: "YYYY0101", Removes: []string{"0400,0561"}}, 0},
		// No mask requested, so a nested birth date is not a surprise and the
		// traversal is skipped entirely.
		{"no mask", ModProfile{Sets: []string{"0010,0010=ANON"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rootDir, outDir := t.TempDir(), t.TempDir()
			srcPath := filepath.Join(rootDir, "nested.dcm")
			writeNestedDOBFixture(t, srcPath)

			params, err := compileModifyParams(tc.profile)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			res := runModification(context.Background(), []string{srcPath}, rootDir, outDir, params, nil, nil)
			if res.Processed != 1 || res.Failed != 0 {
				t.Fatalf("result = %+v (%v), want the file exported", res, res.Failures)
			}
			if res.NestedDOBKept != tc.want {
				t.Errorf("NestedDOBKept = %d, want %d", res.NestedDOBKept, tc.want)
			}
			// Whatever the count, the file still exports — this is a disclosure,
			// not a failure.
			out, err := sdicom.ParseFile(filepath.Join(outDir, "nested.dcm"), nil)
			if err != nil {
				t.Fatalf("parse export: %v", err)
			}
			if tc.profile.DOB != "" {
				e, ferr := out.FindElementByTag(tag.PatientBirthDate)
				if ferr != nil {
					t.Fatal("top-level birth date missing from the export")
				}
				if got := strings.TrimSpace(sdicom.MustGetStrings(e.Value)[0]); got != "19800101" {
					t.Errorf("top-level birth date = %q, want it masked to 19800101", got)
				}
			}
		})
	}
}

// TestRunModificationMemoryBudgetEquivalence is the test that matters for the
// memory gating: it must change *when* work happens, never *what* is produced.
//
// The budget is squeezed to a single byte, so every file is clamped to the whole
// budget and the pool is forced to run them strictly one at a time — the extreme
// of the gating path. The exports must still be byte-identical to the same run
// at the normal budget.
func TestRunModificationMemoryBudgetEquivalence(t *testing.T) {
	// A transfer-syntax conversion is enough to arm the limiter (params.targetTS
	// non-empty), without needing a compressed fixture.
	profile := ModProfile{
		Sets:           []string{"0010,0010=ANON"},
		TransferSyntax: tsPrefImplicitLE,
	}

	run := func(t *testing.T, budget int64) map[string][]byte {
		t.Helper()
		original := modifyMemoryBudget
		t.Cleanup(func() { modifyMemoryBudget = original })
		modifyMemoryBudget = budget

		rootDir, outDir := t.TempDir(), t.TempDir()
		var paths []string
		for _, name := range []string{"a.dcm", "b.dcm", "c.dcm", "d.dcm", "e.dcm"} {
			p := filepath.Join(rootDir, name)
			writeModifyTestDICOM(t, p)
			paths = append(paths, p)
		}

		params, err := compileModifyParams(profile)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		res := runModification(context.Background(), paths, rootDir, outDir, params, nil, nil)
		if res.Processed != len(paths) || res.Failed != 0 {
			t.Fatalf("result = %+v (%v), want all %d exported", res, res.Failures, len(paths))
		}

		out := map[string][]byte{}
		entries, err := os.ReadDir(outDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			out[e.Name()] = mustReadFile(t, filepath.Join(outDir, e.Name()))
		}
		return out
	}

	normal := run(t, modifyMemoryBudget)
	squeezed := run(t, 1)

	if len(normal) != len(squeezed) {
		t.Fatalf("file counts differ: %d at the normal budget, %d squeezed", len(normal), len(squeezed))
	}
	for name, want := range normal {
		got, ok := squeezed[name]
		if !ok {
			t.Errorf("%s missing from the squeezed run", name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs between budgets (%d vs %d bytes) — gating must not change output",
				name, len(got), len(want))
		}
	}
}

// TestFileMemoryWeight: the weight the pool reserves is the projected pixel size
// with the overhead factor, and an unreadable file weighs nothing rather than
// being guessed at.
func TestFileMemoryWeight(t *testing.T) {
	dir := t.TempDir()

	dicomPath := filepath.Join(dir, "ok.dcm")
	writeModifyTestDICOM(t, dicomPath) // 2×2, 8-bit, 1 sample
	if got, want := fileMemoryWeight(dicomPath), int64(2*2*modifyMemoryOverheadFactor); got != want {
		t.Errorf("weight = %d, want %d", got, want)
	}

	junkPath := filepath.Join(dir, "junk.dcm")
	if err := os.WriteFile(junkPath, []byte("not a dicom file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := fileMemoryWeight(junkPath); got != 0 {
		t.Errorf("weight of an unreadable file = %d, want 0", got)
	}
}

// mustReadFile reads path or fails the test.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// A profile's transfer syntax converts the export while the de-identification
// still applies, and the source in the download folder is left in its own
// syntax — the whole point of converting here rather than on the retrieve.
func TestRunModificationTransferSyntax(t *testing.T) {

	// The fixture is Explicit VR LE, so Implicit VR LE is a real conversion.
	params, err := compileModifyParams(ModProfile{
		Sets:           []string{"0010,0010=ANON"},
		TransferSyntax: tsPrefImplicitLE,
	})
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
	params, err := compileModifyParams(ModProfile{TransferSyntax: tsPrefImplicitLE})
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
	params, err := compileModifyParams(ModProfile{TransferSyntax: tsPrefExplicitLE})
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
		mustTestElement(t, tag.Modality, []string{"CT"}),
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

// TestExportLayoutRelFor verifies the PHI-safe export layout: the export root
// stands in for the patient folder (dropDirs 1) or patient+study (dropDirs
// 2); every folder and the file name below it keep their source name unless
// the export-name tags actually changed between before and after, in which
// case that one component is rebuilt with the same rules organizeFilePath
// uses for the download folder.
func TestExportLayoutRelFor(t *testing.T) {
	root := filepath.Join("dl")
	patient := "DOE^JOHN (MRN12345)"
	studyOrig := "CT ABDOMEN (20240101)"
	handMade := "My Renamed Study Folder"
	series := "AX W CONTRAST (2)"
	uidA, uidB := "1.2.840.10.1", "1.2.840.10.2"

	names := func(desc, date, sdesc, snum, uid string) exportNames {
		return exportNames{studyDesc: desc, studyDate: date, seriesDesc: sdesc, seriesNumber: snum, sopUID: uid}
	}
	before := names("CT ABDOMEN", "20240101", "AX W CONTRAST", "2", uidA)

	tests := []struct {
		name     string
		dropDirs int
		src      string
		before   exportNames
		after    exportNames
		want     string
	}{
		{"study level drops patient and study, keeps series", 2,
			filepath.Join(root, patient, studyOrig, series, "1.dcm"), before, before,
			filepath.Join(series, "1.dcm")},
		{"patient level keeps an unaffected study folder verbatim", 1,
			filepath.Join(root, patient, studyOrig, series, "1.dcm"), before, before,
			filepath.Join(studyOrig, series, "1.dcm")},
		{"a hand-renamed study folder survives when its tags are untouched", 1,
			filepath.Join(root, patient, handMade, series, "1.dcm"), before, before,
			filepath.Join(handMade, series, "1.dcm")},
		{"a shifted StudyDate rebuilds only the study component", 1,
			filepath.Join(root, patient, studyOrig, series, "1.dcm"), before,
			names("CT ABDOMEN", "20240103", "AX W CONTRAST", "2", uidA),
			filepath.Join("CT ABDOMEN (20240103)", series, "1.dcm")},
		{"a replaced StudyDescription rebuilds the study component", 1,
			filepath.Join(root, patient, studyOrig, series, "1.dcm"), before,
			names("ANONYMIZED", "20240101", "AX W CONTRAST", "2", uidA),
			filepath.Join("ANONYMIZED (20240101)", series, "1.dcm")},
		{"a removed StudyDescription falls back to Unknown Study", 1,
			filepath.Join(root, patient, studyOrig, series, "1.dcm"), before,
			names("", "20240101", "AX W CONTRAST", "2", uidA),
			filepath.Join("Unknown Study (20240101)", series, "1.dcm")},
		{"a replaced SeriesDescription rebuilds only the series component", 2,
			filepath.Join(root, patient, studyOrig, series, "1.dcm"), before,
			names("CT ABDOMEN", "20240101", "SERIES", "2", uidA),
			filepath.Join("SERIES (2)", "1.dcm")},
		{"deeper nesting is preserved verbatim", 1,
			filepath.Join(root, patient, studyOrig, series, "sub", "1.dcm"), before, before,
			filepath.Join(studyOrig, series, "sub", "1.dcm")},
		{"a changed SOP Instance UID renames the file", 2,
			filepath.Join(root, patient, studyOrig, series, "1.dcm"), before,
			names("CT ABDOMEN", "20240101", "AX W CONTRAST", "2", uidB),
			filepath.Join(series, sanitize(uidB)+".dcm")},
		{"an unchanged SOP Instance UID keeps the original file name", 2,
			filepath.Join(root, patient, studyOrig, series, "1.dcm"), before, before,
			filepath.Join(series, "1.dcm")},
		{"the flat fallback maps to the bare file name", 1,
			filepath.Join(root, "4.dcm"), before, before, "4.dcm"},
		{"the flat fallback still renames on a changed UID", 1,
			filepath.Join(root, "4.dcm"), before,
			names("CT ABDOMEN", "20240101", "AX W CONTRAST", "2", uidB),
			sanitize(uidB) + ".dcm"},
		{"a path outside root maps to the bare file name", 1,
			filepath.Join("elsewhere", "5.dcm"), before, before, "5.dcm"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := exportLayout{dropDirs: tc.dropDirs}
			if got := l.relFor(tc.src, root, tc.before, tc.after); got != tc.want {
				t.Errorf("relFor() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExportLayoutRelForFlat verifies flat mode: every file lands directly at
// the export root named after its SOP Instance UID, regardless of dropDirs or
// nesting depth, and a name collision — a duplicate UID or a file missing one
// entirely — is disambiguated rather than silently overwriting the first
// claimant.
func TestExportLayoutRelForFlat(t *testing.T) {
	root := filepath.Join("dl")
	patient := "DOE^JOHN (MRN12345)"
	study := "CT ABDOMEN (20240101)"
	series := "AX W CONTRAST (2)"
	uidA, uidB := "1.2.840.10.1", "1.2.840.10.2"

	names := func(uid string) exportNames { return exportNames{sopUID: uid} }

	t.Run("names by SOP Instance UID regardless of nesting or dropDirs", func(t *testing.T) {
		l := exportLayout{dropDirs: 1, flat: true, names: &flatNames{}}
		src := filepath.Join(root, patient, study, series, "sub", "orig.dcm")
		if got, want := l.relFor(src, root, names(uidA), names(uidA)), sanitize(uidA)+".dcm"; got != want {
			t.Errorf("relFor() = %q, want %q", got, want)
		}
	})

	t.Run("a remapped UID is named after the new value", func(t *testing.T) {
		l := exportLayout{flat: true, names: &flatNames{}}
		src := filepath.Join(root, patient, study, series, "orig.dcm")
		if got, want := l.relFor(src, root, names(uidA), names(uidB)), sanitize(uidB)+".dcm"; got != want {
			t.Errorf("relFor() = %q, want %q", got, want)
		}
	})

	t.Run("no SOP Instance UID falls back to the source name", func(t *testing.T) {
		l := exportLayout{flat: true, names: &flatNames{}}
		src := filepath.Join(root, patient, study, series, "orig.dcm")
		if got, want := l.relFor(src, root, names(""), names("")), "orig.dcm"; got != want {
			t.Errorf("relFor() = %q, want %q", got, want)
		}
	})

	t.Run("a nil names map still works — no dedup, no panic", func(t *testing.T) {
		l := exportLayout{flat: true}
		src := filepath.Join(root, "orig.dcm")
		if got, want := l.relFor(src, root, names(uidA), names(uidA)), sanitize(uidA)+".dcm"; got != want {
			t.Errorf("relFor() = %q, want %q", got, want)
		}
	})

	t.Run("a duplicated UID gets a numbered suffix rather than colliding", func(t *testing.T) {
		l := exportLayout{flat: true, names: &flatNames{}}
		first := l.relFor(filepath.Join(root, patient, study, series, "a.dcm"), root, names(uidA), names(uidA))
		second := l.relFor(filepath.Join(root, patient, study, series, "b.dcm"), root, names(uidA), names(uidA))
		if first == second {
			t.Fatalf("two files resolved to the same name: %q", first)
		}
		if want := sanitize(uidA) + ".dcm"; first != want {
			t.Errorf("first = %q, want %q", first, want)
		}
		if want := sanitize(uidA) + " (2).dcm"; second != want {
			t.Errorf("second = %q, want %q", second, want)
		}
	})
}

// writeExportLayoutFixture writes a minimal CT file carrying the identifiers
// an export path is built from, for TestRunModificationExportLayout.
func writeExportLayoutFixture(t *testing.T, path, studyUID, seriesUID, sopUID,
	studyDesc, studyDate, seriesDesc, seriesNum string) {
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
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{sopUID}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{tsExplicitVRLE}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{sopUID}),
		mustTestElement(t, tag.PatientName, []string{"DOE^JANE"}),
		mustTestElement(t, tag.PatientID, []string{"PID123"}),
		mustTestElement(t, tag.PatientBirthDate, []string{"19800615"}),
		mustTestElement(t, tag.Modality, []string{"CT"}),
		mustTestElement(t, tag.StudyDate, []string{studyDate}),
		mustTestElement(t, tag.StudyDescription, []string{studyDesc}),
		mustTestElement(t, tag.SeriesDescription, []string{seriesDesc}),
		mustTestElement(t, tag.SeriesNumber, []string{seriesNum}),
		mustTestElement(t, tag.StudyInstanceUID, []string{studyUID}),
		mustTestElement(t, tag.SeriesInstanceUID, []string{seriesUID}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{"MONOCHROME2"}),
		mustTestElement(t, tag.Rows, []int{2}),
		mustTestElement(t, tag.Columns, []int{2}),
		mustTestElement(t, tag.BitsAllocated, []int{8}),
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.HighBit, []int{7}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		mustTestElement(t, tag.SamplesPerPixel, []int{1}),
		pd,
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, sdicom.Dataset{Elements: elems},
		sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write test DICOM: %v", err)
	}
}

// TestRunModificationExportLayout is the end-to-end counterpart to
// TestExportLayoutRelFor: base-deident remaps UIDs but never touches
// Study/SeriesDescription or Study/SeriesDate/Number, so a two-study export
// keeps both studies' real folder names — never study-01/study-02 — while
// every file is renamed after its remapped SOP Instance UID.
func TestRunModificationExportLayout(t *testing.T) {
	profiles := embeddedModConfigs(t)
	resolved, err := resolveModProfile("base-deident", profiles)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	params, err := compileModifyParams(resolved)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rootDir := t.TempDir()
	patient := "DOE^JANE (PID123)"
	studyA, seriesA := "CT ABDOMEN (20240101)", "AX W CONTRAST (2)"
	studyB, seriesB := "CT CHEST (20230601)", "AX (3)"
	srcA := filepath.Join(rootDir, patient, studyA, seriesA, "1.dcm")
	srcB := filepath.Join(rootDir, patient, studyB, seriesB, "1.dcm")
	writeExportLayoutFixture(t, srcA, "1.2.1", "1.2.1.1", "1.2.1.1.1", "CT ABDOMEN", "20240101", "AX W CONTRAST", "2")
	writeExportLayoutFixture(t, srcB, "1.2.2", "1.2.2.1", "1.2.2.1.1", "CT CHEST", "20230601", "AX", "3")

	outDir := t.TempDir()
	layout := &exportLayout{dropDirs: 1}
	res := runModification(context.Background(), []string{srcA, srcB}, rootDir, outDir, params, layout, nil)
	if res.Failed != 0 || res.Processed != 2 {
		t.Fatalf("result = %+v (%v), want 2 processed 0 failed", res, res.Failures)
	}

	studyADir := filepath.Join(outDir, studyA, seriesA)
	studyBDir := filepath.Join(outDir, studyB, seriesB)
	entriesA, err := os.ReadDir(studyADir)
	if err != nil || len(entriesA) != 1 {
		t.Fatalf("read %s: %v (entries=%v)", studyADir, err, entriesA)
	}
	entriesB, err := os.ReadDir(studyBDir)
	if err != nil || len(entriesB) != 1 {
		t.Fatalf("read %s: %v (entries=%v)", studyBDir, err, entriesB)
	}

	dsA, err := sdicom.ParseFile(filepath.Join(studyADir, entriesA[0].Name()), nil)
	if err != nil {
		t.Fatalf("parse exported A: %v", err)
	}
	e, err := dsA.FindElementByTag(tag.SOPInstanceUID)
	if err != nil {
		t.Fatalf("SOPInstanceUID missing: %v", err)
	}
	remappedUID := strings.TrimSpace(sdicom.MustGetStrings(e.Value)[0])
	if remappedUID == "1.2.1.1.1" {
		t.Fatalf("SOPInstanceUID unchanged — remapuids should have replaced it")
	}
	if want := sanitize(remappedUID) + ".dcm"; entriesA[0].Name() != want {
		t.Errorf("exported file name = %q, want %q (matching the remapped SOP Instance UID)", entriesA[0].Name(), want)
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
	params, err := compileModifyParams(ModProfile{ShiftDays: "-45"})
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

	rootDir := t.TempDir()
	srcPath := filepath.Join(rootDir, "img1.dcm")
	writeModifyTestDICOM(t, srcPath) // fixture Modality is CT

	params, err := compileModifyParams(ModProfile{
		ShiftDays:   "-45",
		PerModality: map[string]ModProfile{"CT": {ShiftDays: "10"}},
	})
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
		PerModality: map[string]ModProfile{"CT": {ShiftDays: "x"}},
	})
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

// The ignoresopclass filter skips by SOP Class UID: the test file is a
// Secondary Capture object labelled with an imaging modality, which is exactly
// the shape of a scanned document the ImageType and Modality filters miss.
func TestRunModificationIgnoreSOPClass(t *testing.T) {
	const sc = "1.2.840.10008.5.1.4.1.1.7"
	run := func(t *testing.T, p ModProfile, modality string) modifyResult {
		t.Helper()
		params, err := compileModifyParams(p)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		rootDir := t.TempDir()
		srcPath := filepath.Join(rootDir, "sc.dcm")
		writeModifyTestDICOMModality(t, srcPath, modality)
		return runModification(context.Background(), []string{srcPath}, rootDir, t.TempDir(), params, nil, nil)
	}

	// Profile-wide: the listed class is skipped, an unlisted one is processed.
	if res := run(t, ModProfile{IgnoreSOPClasses: []string{sc}}, "CT"); res.Skipped != 1 || res.Processed != 0 || res.Failed != 0 {
		t.Errorf("profile-wide filter: %+v, want the file skipped", res)
	}
	if res := run(t, ModProfile{Sets: []string{"0010,0010=ANON"}, IgnoreSOPClasses: []string{"1.2.840.10008.5.1.4.1.1.2"}}, "CT"); res.Processed != 1 || res.Skipped != 0 {
		t.Errorf("unlisted class: %+v, want the file processed", res)
	}

	// Per-modality: the CT override's list reaches a CT file and not an MR one.
	perMod := ModProfile{
		Sets:        []string{"0010,0010=ANON"},
		PerModality: map[string]ModProfile{"CT": {IgnoreSOPClasses: []string{sc}}},
	}
	if res := run(t, perMod, "CT"); res.Skipped != 1 || res.Processed != 0 {
		t.Errorf("CT override on a CT file: %+v, want skipped", res)
	}
	if res := run(t, perMod, "MR"); res.Processed != 1 || res.Skipped != 0 {
		t.Errorf("CT override on an MR file: %+v, want processed", res)
	}

	// A malformed entry is refused at compile time rather than matching nothing.
	if _, err := compileModifyParams(ModProfile{IgnoreSOPClasses: []string{"SC"}}); err == nil || !strings.Contains(err.Error(), "not a UID") {
		t.Errorf("compile accepted a non-UID entry: %v", err)
	}
	if _, err := compileModifyParams(ModProfile{Sets: []string{"0010,0010=ANON"},
		PerModality: map[string]ModProfile{"CT": {IgnoreSOPClasses: []string{"SC"}}}}); err == nil || !strings.Contains(err.Error(), "modality CT") {
		t.Errorf("compile accepted a non-UID override entry: %v", err)
	}
}
