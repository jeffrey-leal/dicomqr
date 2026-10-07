package main

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

// runAuditFixture exports one generated file (optionally with extra elements
// set first) through the real engine and returns the parsed export.
func runAuditFixture(t *testing.T, p ModProfile, prep func(ds *sdicom.Dataset)) (sdicom.Dataset, string) {
	t.Helper()
	rootDir, outDir := t.TempDir(), t.TempDir()
	src := filepath.Join(rootDir, "a.dcm")
	writeModifyTestDICOM(t, src)
	// The shared fixture lists its elements out of tag order; put it right, so
	// the order checks below measure what the export adds, not the source.
	{
		ds, err := sdicom.ParseFile(src, nil)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		if prep != nil {
			prep(&ds)
		}
		sortElementsByTag(&ds)
		f, err := os.Create(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		f.Close()
	}
	params, err := compileModifyParams(p)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	params.auditProfile = "test-profile"
	res := runModification(context.Background(), []string{src}, rootDir, outDir, params, nil, nil)
	if res.Processed != 1 || res.Failed != 0 {
		t.Fatalf("result %+v (%v), want the file exported", res, res.Failures)
	}
	out := filepath.Join(outDir, "a.dcm")
	ds, err := sdicom.ParseFile(out, nil)
	if err != nil {
		t.Fatalf("parse export: %v", err)
	}
	return ds, out
}

func strValues(t *testing.T, ds *sdicom.Dataset, tg tag.Tag) []string {
	t.Helper()
	e, err := ds.FindElementByTag(tg)
	if err != nil {
		return nil
	}
	return elemStringComponents(e)
}

// assertAscending fails if the top-level elements are not in tag order — the
// writer emits them as they sit in the slice, and DICOM requires ascending.
func assertAscending(t *testing.T, ds *sdicom.Dataset) {
	t.Helper()
	for i := 1; i < len(ds.Elements); i++ {
		if !tagLess(ds.Elements[i-1].Tag, ds.Elements[i].Tag) {
			t.Fatalf("elements out of order: %v before %v", ds.Elements[i-1].Tag, ds.Elements[i].Tag)
		}
	}
}

func TestRunModificationAuditTags(t *testing.T) {
	ds, out := runAuditFixture(t, ModProfile{
		Sets:      []string{"0010,0010=ANON"},
		ShiftDays: "-10",
		AuditTags: true,
	}, nil)

	if got := strValues(t, &ds, patientIdentityRemovedTag); !slices.Equal(got, []string{"YES"}) {
		t.Errorf("Patient Identity Removed = %v, want YES", got)
	}
	want := "dicomqr " + version + " profile test-profile"
	if got := strValues(t, &ds, deidentificationMethodTag); !slices.Equal(got, []string{want}) {
		t.Errorf("De-identification Method = %q, want %q", got, want)
	}
	if got := strValues(t, &ds, longitudinalTemporalModTag); !slices.Equal(got, []string{"MODIFIED"}) {
		t.Errorf("Longitudinal Temporal Information Modified = %v, want MODIFIED after a date shift", got)
	}
	assertAscending(t, &ds)

	// Written in tag order, the tags are before the pixel data — where a
	// header-only reader, which stops there, can see them.
	hdr, err := readDicomHeader(out)
	if err != nil {
		t.Fatalf("readDicomHeader: %v", err)
	}
	if strValues(t, &hdr, patientIdentityRemovedTag) == nil {
		t.Error("Patient Identity Removed not visible to a header reader — written after the pixel data?")
	}
}

func TestRunModificationAuditTagsShiftFlagOnlyWhenShifted(t *testing.T) {
	for _, shift := range []string{"", "0"} {
		ds, _ := runAuditFixture(t, ModProfile{Sets: []string{"0010,0010=ANON"}, ShiftDays: shift, AuditTags: true}, nil)
		if got := strValues(t, &ds, longitudinalTemporalModTag); got != nil {
			t.Errorf("shiftdays %q: Longitudinal Temporal Information Modified = %v, want absent (dates are original)", shift, got)
		}
		if strValues(t, &ds, patientIdentityRemovedTag) == nil {
			t.Errorf("shiftdays %q: Patient Identity Removed missing", shift)
		}
	}
}

func TestRunModificationAuditTagsOff(t *testing.T) {
	ds, _ := runAuditFixture(t, ModProfile{Sets: []string{"0010,0010=ANON"}, ShiftDays: "-10"}, nil)
	for _, tg := range []tag.Tag{patientIdentityRemovedTag, deidentificationMethodTag, longitudinalTemporalModTag} {
		if got := strValues(t, &ds, tg); got != nil {
			t.Errorf("%v = %v in an export whose profile did not ask for audit tags", tg, got)
		}
	}
}

// A file de-identified once before keeps the record of that step; ours is
// added after it, and running the same profile again does not repeat it.
func TestRunModificationAuditTagsAppendToPriorMethod(t *testing.T) {
	prior := func(ds *sdicom.Dataset) {
		if err := setElementValue(ds, deidentificationMethodTag, []string{"SITE TOOL v2"}); err != nil {
			t.Fatal(err)
		}
	}
	ds, out := runAuditFixture(t, ModProfile{Sets: []string{"0010,0010=ANON"}, AuditTags: true}, prior)
	ours := "dicomqr " + version + " profile test-profile"
	if got := strValues(t, &ds, deidentificationMethodTag); !slices.Equal(got, []string{"SITE TOOL v2", ours}) {
		t.Fatalf("De-identification Method = %q, want the prior method then ours", got)
	}

	again := func(d *sdicom.Dataset) {
		if err := setElementValue(d, deidentificationMethodTag, []string{"SITE TOOL v2", ours}); err != nil {
			t.Fatal(err)
		}
	}
	assertAscending(t, &ds)
	_ = out
	ds2, _ := runAuditFixture(t, ModProfile{Sets: []string{"0010,0010=ANON"}, AuditTags: true}, again)
	if got := strValues(t, &ds2, deidentificationMethodTag); !slices.Equal(got, []string{"SITE TOOL v2", ours}) {
		t.Errorf("re-run gave %q; an identical method must not be repeated", got)
	}
}

// A profile that sets one of these tags itself keeps its own value.
func TestRunModificationAuditTagsYieldToSetValues(t *testing.T) {
	ds, _ := runAuditFixture(t, ModProfile{
		Sets:      []string{"0010,0010=ANON", "0012,0063=PROTOCOL 7"},
		AuditTags: true,
	}, nil)
	if got := strValues(t, &ds, deidentificationMethodTag); !slices.Equal(got, []string{"PROTOCOL 7"}) {
		t.Errorf("De-identification Method = %q, want the profile's own Set value", got)
	}
	if strValues(t, &ds, patientIdentityRemovedTag) == nil {
		t.Error("Patient Identity Removed missing")
	}
}

// Stamping files de-identified is not itself a de-identification: a profile
// with nothing else to do must still be refused.
func TestAuditTagsAloneAreNotActionable(t *testing.T) {
	if _, err := compileModifyParams(ModProfile{AuditTags: true}); err == nil {
		t.Error("a profile whose only option is audittags compiled; it would stamp unmodified files YES")
	}
}

// A Set value for a tag the file lacks used to be appended after the pixel
// data; it now goes in tag order, visible to header readers.
func TestSetValueForAbsentTagIsInsertedInOrder(t *testing.T) {
	ds, out := runAuditFixture(t, ModProfile{Sets: []string{"0010,1010=045Y"}}, nil)
	if got := strValues(t, &ds, tag.PatientAge); !slices.Equal(got, []string{"045Y"}) {
		t.Fatalf("Patient's Age = %v, want 045Y", got)
	}
	assertAscending(t, &ds)
	hdr, err := readDicomHeader(out)
	if err != nil {
		t.Fatalf("readDicomHeader: %v", err)
	}
	if strValues(t, &hdr, tag.PatientAge) == nil {
		t.Error("an added Set value is invisible to a header reader — written after the pixel data")
	}
}

func TestDeidentMethodText(t *testing.T) {
	if got := deidentMethodText(""); got != "dicomqr "+version {
		t.Errorf("no profile: %q", got)
	}
	if got := deidentMethodText(`site\one`); strings.Contains(got, `\`) {
		t.Errorf("%q keeps a backslash, which would split it into two values", got)
	}
	long := deidentMethodText(strings.Repeat("x", 200))
	if n := len([]rune(long)); n != deidentMethodMaxLen {
		t.Errorf("long name gave %d characters, want clipped to %d", n, deidentMethodMaxLen)
	}
}

func TestBaseDeidentEnablesAuditTags(t *testing.T) {
	resolved, err := resolveModProfile("base-deident", embeddedModConfigs(t))
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.AuditTags {
		t.Error("shipped base-deident does not mark exports as de-identified")
	}
}
