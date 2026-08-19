package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/suyashkumar/dicom/pkg/tag"
)

func TestCanonicalTagRef(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"8,80", "0008,0080", true},
		{"0008,0080", "0008,0080", true},
		{" 40,a730 ", "0040,A730", true}, // trimmed and upper-cased
		{"0010,0010", "0010,0010", true},
		{"notatag", "notatag", false}, // kept verbatim for validation to report
		{"0010", "0010", false},       // no element
		{"zz,00", "zz,00", false},     // not hex
	} {
		got, ok := canonicalTagRef(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("canonicalTagRef(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// normalizeModProfile canonicalises at every depth, and — the part that matters
// for a de-identification profile — never drops an entry it cannot parse.
func TestNormalizeModProfile(t *testing.T) {
	got := normalizeModProfile(ModProfile{
		Sets:    []string{"10,10=ANON", "8,50=", "notatag=X"},
		Removes: []string{"8,80", "0010,1000", "garbage"},
		Keep:    []string{"40,275"},
		PerModality: map[string]ModProfile{
			"CT": {Removes: []string{"18,1030"}, Sets: []string{"8,1030=STUDY"}},
		},
	})

	if want := []string{"0010,0010=ANON", "0008,0050=", "notatag=X"}; !reflect.DeepEqual(got.Sets, want) {
		t.Errorf("Sets = %v, want %v", got.Sets, want)
	}
	if want := []string{"0008,0080", "0010,1000", "garbage"}; !reflect.DeepEqual(got.Removes, want) {
		t.Errorf("Removes = %v, want %v", got.Removes, want)
	}
	if want := []string{"0040,0275"}; !reflect.DeepEqual(got.Keep, want) {
		t.Errorf("Keep = %v, want %v", got.Keep, want)
	}
	ct := got.PerModality["CT"]
	if want := []string{"0018,1030"}; !reflect.DeepEqual(ct.Removes, want) {
		t.Errorf("per-modality Removes = %v, want %v", ct.Removes, want)
	}
	if want := []string{"0008,1030=STUDY"}; !reflect.DeepEqual(ct.Sets, want) {
		t.Errorf("per-modality Sets = %v, want %v", ct.Sets, want)
	}

	// A value containing "=" must survive intact — only the tag half is touched.
	eq := normalizeModProfile(ModProfile{Sets: []string{"10,4000=a=b=c"}})
	if want := []string{"0010,4000=a=b=c"}; !reflect.DeepEqual(eq.Sets, want) {
		t.Errorf("Sets = %v, want %v", eq.Sets, want)
	}
}

// This is the bug the canonicalisation exists to kill. A Keep list cancels its
// base profile's Removes list; when that comparison was textual, two files
// spelling one tag differently silently stopped the cancellation, and a tag the
// user asked to preserve was deleted from every exported file instead.
func TestMergeCancelsKeepAcrossSpellings(t *testing.T) {
	for _, tc := range []struct{ remove, keep string }{
		{"40,275", "0040,0275"},    // base short, child padded
		{"0040,0275", "40,275"},    // base padded, child short
		{"0040,a730", "0040,A730"}, // differing case
		{"40,275", "40,275"},       // identical — must still work
	} {
		cfg := ModProfileConfig{
			"base":  {Removes: []string{tc.remove, "0010,1000"}},
			"child": {Base: "base", Keep: []string{tc.keep}},
		}
		resolved, err := resolveModProfile("child", cfg)
		if err != nil {
			t.Fatalf("resolve (%s / %s): %v", tc.remove, tc.keep, err)
		}
		for _, r := range resolved.Removes {
			if kt, err1 := parseTagString(r); err1 == nil {
				if wt, err2 := parseTagString(tc.keep); err2 == nil && kt == wt {
					t.Errorf("remove %q spelled %q was not cancelled by keep %q",
						r, tc.remove, tc.keep)
				}
			}
		}
		// The unrelated removal must survive.
		if len(resolved.Removes) != 1 {
			t.Errorf("removes = %v, want only the uncancelled entry", resolved.Removes)
		}
	}
}

// TestModProfileDOBAdvisory covers the safeguard for the one thing the
// birth-date mask cannot reach: a copy nested inside Original Attributes
// Sequence. Every case here is driven through resolveModProfile, because the
// advisory is only correct against a resolved profile — the base supplies the
// removal for most real profiles, and a Keep list can take it away again.
func TestModProfileDOBAdvisory(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  ModProfileConfig
		want bool // an advisory is expected
	}{
		{"no dob mask, no removal",
			ModProfileConfig{"p": {Removes: []string{"0010,1000"}}}, false},
		{"dob mask without the removal",
			ModProfileConfig{"p": {DOB: "YYYY0101"}}, true},
		{"dob mask with the removal, padded",
			ModProfileConfig{"p": {DOB: "YYYY0101", Removes: []string{"0400,0561"}}}, false},
		// Spelling must not matter: the profile code compares parsed tags.
		{"dob mask with the removal, short form",
			ModProfileConfig{"p": {DOB: "YYYY0101", Removes: []string{"400,561"}}}, false},
		{"removal inherited from the base",
			ModProfileConfig{
				"base": {Removes: []string{"0400,0561"}},
				"p":    {Base: "base", DOB: "YYYY0101"},
			}, false},
		// The case that makes resolving mandatory: the child cancels the base's
		// removal, so the sequence survives and the advisory must come back.
		{"keep cancels the base's removal",
			ModProfileConfig{
				"base": {Removes: []string{"0400,0561"}},
				"p":    {Base: "base", DOB: "YYYY0101", Keep: []string{"400,561"}},
			}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := resolveModProfile("p", tc.cfg)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			got := modProfileDOBAdvisory(resolved)
			if (got != "") != tc.want {
				t.Errorf("advisory = %q, want present=%v", got, tc.want)
			}
			if tc.want && !strings.Contains(got, "0400,0561") {
				t.Errorf("advisory does not name the tag to remove: %q", got)
			}
		})
	}
}

// The shipped profiles remove Original Attributes Sequence, so neither may
// raise the advisory — if that removal is ever dropped from defaults this test
// is what says so.
func TestEmbeddedDefaultsRaiseNoDOBAdvisory(t *testing.T) {
	profiles := embeddedModConfigs(t)
	for name := range profiles {
		resolved, err := resolveModProfile(name, profiles)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if adv := modProfileDOBAdvisory(resolved); adv != "" {
			t.Errorf("shipped profile %q raises the birth-date advisory: %s", name, adv)
		}
	}
}

// The same applies to Set values, which are matched on the tag half alone so an
// override replaces the base's value rather than appending a second Set.
func TestMergeSetOverrideAcrossSpellings(t *testing.T) {
	cfg := ModProfileConfig{
		"base":  {Sets: []string{"10,10=BASE"}},
		"child": {Base: "base", Sets: []string{"0010,0010=CHILD"}},
	}
	resolved, err := resolveModProfile("child", cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := []string{"0010,0010=CHILD"}; !reflect.DeepEqual(resolved.Sets, want) {
		t.Errorf("Sets = %v, want %v — the override must replace, not duplicate", resolved.Sets, want)
	}
}

// The shipped defaults must already be canonical, so the file cannot drift back
// into the mixed spellings that made the merge fragile.
func TestEmbeddedDefaultsAreCanonical(t *testing.T) {
	for name, p := range embeddedModConfigs(t) {
		check := func(field string, refs []string) {
			for _, r := range refs {
				ref := r
				if field == "set" {
					ref, _, _ = strings.Cut(r, "=")
				}
				canonical, ok := canonicalTagRef(ref)
				if !ok {
					t.Errorf("%s/%s: %q does not parse as a tag", name, field, ref)
					continue
				}
				if ref != canonical {
					t.Errorf("%s/%s: %q is not canonical, want %q", name, field, ref, canonical)
				}
			}
		}
		check("set", p.Sets)
		check("remove", p.Removes)
		check("keep", p.Keep)
	}
}

func TestSetValueReference(t *testing.T) {
	want := tag.Tag{Group: 0x0010, Element: 0x0010}
	for _, in := range []string{"[0010,0010]", "[10,10]", "[ 0010 , 0010 ]", "  [0010,0010]  ", "[0010,0010]"} {
		got, ok := setValueReference(in)
		if !ok || got != want {
			t.Errorf("setValueReference(%q) = (%v, %v), want (%v, true)", in, got, ok, want)
		}
	}
	// Only a whole value is a reference: no substring templating, so nothing
	// needs escaping and a value that merely contains brackets stays literal.
	for _, in := range []string{
		"", "ANON", "0010,0010", "[0010,0010]x", "x[0010,0010]",
		"[notatag]", "[0010]", "[]", "[0010,0010][0020,000D]",
	} {
		if got, ok := setValueReference(in); ok {
			t.Errorf("setValueReference(%q) = (%v, true), want it treated as a literal", in, got)
		}
	}
}

func TestResolveSetReferences(t *testing.T) {
	t.Run("resolves against the profile's own values", func(t *testing.T) {
		got, err := resolveSetReferences([]string{
			"0010,0010=ANON^PATIENT",
			"0010,0020=[0010,0010]",
			"0040,1008=Y",
		})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		want := []string{"0010,0010=ANON^PATIENT", "0010,0020=ANON^PATIENT", "0040,1008=Y"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("resolved = %v, want %v", got, want)
		}
	})

	t.Run("a target defined after the referrer still resolves", func(t *testing.T) {
		got, err := resolveSetReferences([]string{"0010,0020=[0010,0010]", "0010,0010=X"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if want := []string{"0010,0020=X", "0010,0010=X"}; !reflect.DeepEqual(got, want) {
			t.Errorf("resolved = %v, want %v", got, want)
		}
	})

	t.Run("an empty target resolves to empty, not an error", func(t *testing.T) {
		got, err := resolveSetReferences([]string{"0010,0010=", "0010,0020=[0010,0010]"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if want := []string{"0010,0010=", "0010,0020="}; !reflect.DeepEqual(got, want) {
			t.Errorf("resolved = %v, want %v", got, want)
		}
	})

	t.Run("values containing = survive", func(t *testing.T) {
		got, err := resolveSetReferences([]string{"0010,4000=a=b=c", "0010,0020=[0010,4000]"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if want := []string{"0010,4000=a=b=c", "0010,0020=a=b=c"}; !reflect.DeepEqual(got, want) {
			t.Errorf("resolved = %v, want %v", got, want)
		}
	})

	for _, tc := range []struct {
		name string
		sets []string
		want string
	}{
		{"missing target", []string{"0010,0020=[0010,0010]"}, "does not set"},
		{"self reference", []string{"0010,0010=[0010,0010]"}, "refers to itself"},
		{"chained reference", []string{
			"0010,0010=X", "0010,0020=[0010,0010]", "0008,0050=[0010,0020]",
		}, "cannot be chained"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveSetReferences(tc.sets)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestSetValueFollowers(t *testing.T) {
	got := setValueFollowers([]string{
		"0010,0010=ANON",
		"0010,0020=[0010,0010]",
		"0010,1000=[0010,0010]",
		"0040,1008=Y",
		"0008,0050=[0010,0010]",
	})
	want := map[tag.Tag][]tag.Tag{
		{Group: 0x0010, Element: 0x0010}: {
			{Group: 0x0010, Element: 0x0020},
			{Group: 0x0010, Element: 0x1000},
			{Group: 0x0008, Element: 0x0050},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("followers = %v, want %v", got, want)
	}
	if got := setValueFollowers([]string{"0010,0010=ANON"}); got != nil {
		t.Errorf("followers with no references = %v, want nil", got)
	}
}

// compileModifyParams must resolve before parsing, so the engine's edits carry
// the literal and buildElement never sees the placeholder syntax.
func TestCompileModifyParamsResolvesReferences(t *testing.T) {
	mp, err := compileModifyParams(ModProfile{Sets: []string{
		"0010,0010=ANON^PATIENT",
		"0010,0020=[0010,0010]",
	}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, e := range mp.edits {
		if e.tag == (tag.Tag{Group: 0x0010, Element: 0x0020}) {
			if e.value != "ANON^PATIENT" {
				t.Errorf("Patient ID edit = %q, want the resolved ANON^PATIENT", e.value)
			}
			return
		}
	}
	t.Error("no Patient ID edit was produced")
}

// The shipped profile declares the Patient Name → Patient ID link and names the
// export after the same value. Both must resolve, or a fresh install opens the
// Modification dialog on an error.
func TestEmbeddedDefaultsReferencesResolve(t *testing.T) {
	profiles := embeddedModConfigs(t)
	base, err := resolveModProfile("base-deident", profiles)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	resolvedSets, err := resolveSetReferences(base.Sets)
	if err != nil {
		t.Fatalf("the shipped set values do not resolve: %v", err)
	}
	var patientID string
	for _, s := range resolvedSets {
		if tg, v, ok := splitSetEntry(s); ok && tg == (tag.Tag{Group: 0x0010, Element: 0x0020}) {
			patientID = v
		}
	}
	if patientID != "ANON" {
		t.Errorf("Patient ID resolved to %q, want the Patient Name value ANON", patientID)
	}

	// The export name references a tag the profile actually sets.
	target, isRef := setValueReference(base.ExportName)
	if !isRef {
		t.Fatalf("exportname = %q, want a [GGGG,EEEE] reference", base.ExportName)
	}
	found := false
	for _, s := range base.Sets {
		if tg, _, ok := splitSetEntry(s); ok && tg == target {
			found = true
		}
	}
	if !found {
		t.Errorf("exportname references %s, which the profile does not set", formatTagRef(target))
	}
}

func TestValidateSetValue(t *testing.T) {
	da := tag.Tag{Group: 0x0008, Element: 0x0020}      // Study Date, VR DA
	tm := tag.Tag{Group: 0x0008, Element: 0x0030}      // Study Time, VR TM
	dt := tag.Tag{Group: 0x0008, Element: 0x002A}      // Acquisition DateTime, VR DT
	ui := tag.Tag{Group: 0x0020, Element: 0x000D}      // Study Instance UID, VR UI
	is := tag.Tag{Group: 0x0020, Element: 0x0013}      // Instance Number, VR IS
	pn := tag.Tag{Group: 0x0010, Element: 0x0010}      // Patient's Name, VR PN
	cs := tag.Tag{Group: 0x0008, Element: 0x0060}      // Modality, VR CS
	as := tag.Tag{Group: 0x0010, Element: 0x1010}      // Patient's Age, VR AS
	unknown := tag.Tag{Group: 0x0010, Element: 0x3020} // absent from the dictionary

	for _, tc := range []struct {
		name  string
		t     tag.Tag
		value string
		ok    bool
	}{
		{"empty is always allowed", da, "", true},
		{"empty blanks a UID too", ui, "", true},
		{"valid date", da, "20200115", true},
		{"date words rejected", da, "ANON", false},
		{"date out of range rejected", da, "20201345", false},
		{"date wrong length rejected", da, "2020011", false},
		{"valid time", tm, "120000", true},
		{"time with fraction", tm, "120000.500000", true},
		{"time hours only", tm, "12", true},
		{"bad time rejected", tm, "25:00", false},
		{"valid datetime", dt, "20200115120000.000000+0000", true},
		{"datetime date only", dt, "20200115", true},
		{"bad datetime rejected", dt, "ANON", false},
		{"valid uid", ui, "1.2.840.10008.1.2.1", true},
		{"uid with letters rejected", ui, "1.2.abc", false},
		{"over-long uid rejected", ui, strings.Repeat("1", 65), false},
		{"valid integer string", is, "42", true},
		{"non-numeric IS rejected", is, "x", false},
		{"person name accepted", pn, "ANON^PATIENT", true},
		{"over-long person name rejected", pn, strings.Repeat("A", 65), false},
		{"code string accepted", cs, "CT", true},
		{"over-long code string rejected", cs, strings.Repeat("A", 17), false},
		{"valid age", as, "045Y", true},
		{"bad age rejected", as, "45", false},
		{"unknown tag accepts anything", unknown, "whatever", true},
		// A reference is a placeholder; what it resolves to is what gets
		// checked. Rejecting it here would make a date field unreferenceable.
		{"reference accepted in a date field", da, "[0010,0010]", true},
		{"reference accepted in a UID field", ui, "[0010,0010]", true},
	} {
		err := validateSetValue(tc.t, tc.value)
		if (err == nil) != tc.ok {
			t.Errorf("%s: validateSetValue(%v, %q) = %v, want ok=%v", tc.name, tc.t, tc.value, err, tc.ok)
		}
	}
}

// TestSaveModProfileConfigRoundTrip proves the Preferences editor's save path
// preserves the embedded default profiles exactly: every field that dicomtool
// understands survives save → load unchanged (the two tools share the store
// format, so a lossy round-trip would corrupt copied profiles).
func TestSaveModProfileConfigRoundTrip(t *testing.T) {
	original := embeddedModConfigs(t)

	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := saveModProfileConfig(path, original); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := loadModProfileConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(original, loaded) {
		t.Errorf("round-trip changed the config:\noriginal: %+v\nloaded:   %+v", original, loaded)
	}
}

// TestModProfileEditPreservesUneditedFields simulates what the Preferences
// editor does — copy the profile, overwrite only the fields it has controls
// for — and verifies the fields without controls (per-modality overrides,
// ignore filters, dicomdir, verbose) survive the edit and a disk round-trip.
func TestModProfileEditPreservesUneditedFields(t *testing.T) {
	cfg := ModProfileConfig{
		"full": {
			Sets:             []string{"0010,0010=ANON"},
			Removes:          []string{"0010,1000"},
			DOB:              "YYYY0101",
			Dicomdir:         true,
			Verbose:          true,
			IgnoreTypes:      []string{"SECONDARY"},
			IgnoreModalities: []string{"SR", "PR"},
			PerModality: map[string]ModProfile{
				"CT": {Removes: []string{"0018,1030"}},
			},
		},
	}

	edited := cfg["full"] // the editor starts from the existing profile
	edited.Sets = []string{"0010,0010=CHANGED", "0010,0020=ID0000"}
	cfg["full"] = edited

	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := saveModProfileConfig(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := loadModProfileConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	got := loaded["full"]
	if !got.Dicomdir || !got.Verbose {
		t.Errorf("dicomdir/verbose lost: %+v", got)
	}
	if !reflect.DeepEqual(got.IgnoreTypes, []string{"SECONDARY"}) ||
		!reflect.DeepEqual(got.IgnoreModalities, []string{"SR", "PR"}) {
		t.Errorf("ignore filters lost: types %v modalities %v", got.IgnoreTypes, got.IgnoreModalities)
	}
	if !reflect.DeepEqual(got.PerModality, cfg["full"].PerModality) {
		t.Errorf("per-modality overrides lost: %+v", got.PerModality)
	}
	if len(got.Sets) != 2 || got.Sets[0] != "0010,0010=CHANGED" {
		t.Errorf("edited sets not saved: %v", got.Sets)
	}
}

// TestDefaultSettingsExportFormat guards the defaults/settings.json entry the
// Export dialog preselection relies on.
func TestDefaultSettingsExportFormat(t *testing.T) {
	var s Settings
	if err := json.Unmarshal(defaultSettingsJSON, &s); err != nil {
		t.Fatalf("unmarshal embedded settings.json: %v", err)
	}
	if s.ExportFormat != "csv" {
		t.Errorf("default exportFormat = %q, want %q", s.ExportFormat, "csv")
	}
	if s.ModifyOutputDir != "" {
		t.Errorf("default modifyOutputDir = %q, want empty", s.ModifyOutputDir)
	}
}
