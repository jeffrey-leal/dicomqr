package main

// Tests for the modification-profile editor's save path. The editor structs
// (modProfileEditor, perModalityEditor) expose validate() precisely so these
// tests can drive the whole widgets-to-ModProfile pipeline through Fyne's
// headless test driver without a canvas.

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/suyashkumar/dicom/pkg/tag"
)

func TestModProfileScalarValidators(t *testing.T) {
	t.Run("dob", func(t *testing.T) {
		for _, tc := range []struct {
			in, want string
			ok       bool
		}{
			{"", "", true},
			{"  19000101 ", "19000101", true},
			{"1900010", "", false},
			{"190001012", "", false},
		} {
			got, err := validateDOBMask(tc.in)
			if (err == nil) != tc.ok || got != tc.want {
				t.Errorf("validateDOBMask(%q) = %q, %v; want %q, ok=%v", tc.in, got, err, tc.want, tc.ok)
			}
		}
	})
	t.Run("uid suffix", func(t *testing.T) {
		for _, tc := range []struct {
			in, want string
			ok       bool
		}{
			{"", "", true},
			{" 129 ", "129", true},
			{"0", "", false},
			{"a", "", false},
		} {
			got, err := validateUIDSuffix(tc.in)
			if (err == nil) != tc.ok || got != tc.want {
				t.Errorf("validateUIDSuffix(%q) = %q, %v; want %q, ok=%v", tc.in, got, err, tc.want, tc.ok)
			}
		}
	})
	t.Run("shift days", func(t *testing.T) {
		for _, tc := range []struct {
			in, want string
			ok       bool
		}{
			{"", "", true},
			{" -45 ", "-45", true},
			{"0", "0", true},
			{"+5", "+5", true},
			{"x", "", false},
			{"4.5", "", false},
		} {
			got, err := validateShiftDays(tc.in)
			if (err == nil) != tc.ok || got != tc.want {
				t.Errorf("validateShiftDays(%q) = %q, %v; want %q, ok=%v", tc.in, got, err, tc.want, tc.ok)
			}
		}
	})
	t.Run("comma list", func(t *testing.T) {
		if got := splitCommaList(" SR, PR ,"); !reflect.DeepEqual(got, []string{"SR", "PR"}) {
			t.Errorf("splitCommaList = %v, want [SR PR]", got)
		}
		if got := splitCommaList("  "); got != nil {
			t.Errorf("splitCommaList(blank) = %v, want nil", got)
		}
	})
}

// TestModProfileEditorRoundTrip drives the editor over a maximal profile and
// verifies edited fields land, control-less fields (dicomdir, verbose,
// top-level keepprivate) survive untouched, and the result still round-trips
// through the profile store unchanged.
func TestModProfileEditorRoundTrip(t *testing.T) {
	test.NewApp()
	p := ModProfile{
		Sets:             []string{"0010,0010=ANON"},
		Removes:          []string{"0010,1000"},
		Keep:             []string{"0010,0020"},
		DOB:              "19000101",
		UIDSuffix:        "99",
		ShiftDays:        "-30",
		Priv:             true,
		KeepPrivate:      true,
		Dicomdir:         true,
		Verbose:          true,
		Zip:              true,
		IgnoreTypes:      []string{"SECONDARY"},
		IgnoreModalities: []string{"SR"},
		FixVR:            "correct",
		TransferSyntax:   tsPrefImplicitLE,
		PerModality:      map[string]ModProfile{"CT": {Removes: []string{"0018,1030"}}},
	}
	cfg := ModProfileConfig{"full": p}

	ed := newModProfileEditor("full", p, cfg)
	// The Set values list is rows, not text: edit the existing row's value and
	// add a second tag the way Choose tags… would.
	ed.fields.sets.rows[0].value.SetText("CHANGED")
	ed.fields.sets.rows = append(ed.fields.sets.rows, newSetValueRow("0010,0020=ID0000"))
	ed.ignoreTypesEntry.SetText("SECONDARY, DERIVED")
	ed.ignoreModsEntry.SetText("SR, PR")

	newName, updated, err := ed.validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if newName != "full" {
		t.Errorf("newName = %q", newName)
	}
	if want := []string{"0010,0010=CHANGED", "0010,0020=ID0000"}; !reflect.DeepEqual(updated.Sets, want) {
		t.Errorf("Sets = %v, want %v", updated.Sets, want)
	}
	if want := []string{"SECONDARY", "DERIVED"}; !reflect.DeepEqual(updated.IgnoreTypes, want) {
		t.Errorf("IgnoreTypes = %v, want %v", updated.IgnoreTypes, want)
	}
	if want := []string{"SR", "PR"}; !reflect.DeepEqual(updated.IgnoreModalities, want) {
		t.Errorf("IgnoreModalities = %v, want %v", updated.IgnoreModalities, want)
	}
	if !updated.KeepPrivate || !updated.Dicomdir || !updated.Verbose {
		t.Errorf("control-less fields lost: keepprivate=%v dicomdir=%v verbose=%v",
			updated.KeepPrivate, updated.Dicomdir, updated.Verbose)
	}
	if !reflect.DeepEqual(updated.PerModality, p.PerModality) {
		t.Errorf("PerModality changed: %+v", updated.PerModality)
	}
	if updated.DOB != "19000101" || updated.UIDSuffix != "99" || updated.ShiftDays != "-30" ||
		updated.FixVR != "correct" || !updated.Priv || !updated.Zip ||
		updated.TransferSyntax != tsPrefImplicitLE {
		t.Errorf("unedited controlled fields changed: %+v", updated)
	}

	// The transfer syntax select stores a token, not the label it displays —
	// the token is what compileModifyParams and dicomtool interoperability see.
	ed.tsSelect.SetSelected(tsExportLabelExplicit)
	if _, edited, verr := ed.validate(); verr != nil {
		t.Fatalf("validate after syntax change: %v", verr)
	} else if edited.TransferSyntax != tsPrefExplicitLE {
		t.Errorf("TransferSyntax = %q, want %q", edited.TransferSyntax, tsPrefExplicitLE)
	}
	ed.tsSelect.SetSelected(tsExportLabelAny)
	if _, edited, verr := ed.validate(); verr != nil {
		t.Fatalf("validate after clearing the syntax: %v", verr)
	} else if edited.TransferSyntax != "" {
		t.Errorf("TransferSyntax = %q, want empty so the key stays out of profiles.json", edited.TransferSyntax)
	}
	// The disk round-trip below uses `updated`, captured before those edits, so
	// it covers a profile that does carry the key.

	path := filepath.Join(t.TempDir(), "profiles.json")
	out := ModProfileConfig{"full": updated}
	if err := saveModProfileConfig(path, out); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := loadModProfileConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(out, loaded) {
		t.Errorf("disk round-trip changed the profile:\nsaved:  %+v\nloaded: %+v", out, loaded)
	}
}

// Set rows read in group-then-element order regardless of how the profile
// stored them, which is the order the Remove and Keep lists already use.
func TestSetValueListSortsRows(t *testing.T) {
	test.NewApp()
	l := newSetValueList([]string{
		"0040,1008=Y",
		"0010,0020=[0010,0010]",
		"0008,0050=",
		"0010,0010=ANON",
	})
	got, err := l.entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	want := []string{"0008,0050=", "0010,0010=ANON", "0010,0020=[0010,0010]", "0040,1008=Y"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %v, want them sorted %v", got, want)
	}
}

// A row whose reference does not parse cannot be sorted and must not vanish:
// it sorts last and is still reported on save.
func TestSetValueListKeepsUnparsableRowsLast(t *testing.T) {
	test.NewApp()
	l := newSetValueList([]string{"garbage=X", "0010,0010=ANON"})
	if len(l.rows) != 2 {
		t.Fatalf("rows = %d, want 2 — an unparsable row must not be dropped", len(l.rows))
	}
	if l.rows[0].ref != "0010,0010" || l.rows[1].ref != "garbage" {
		t.Errorf("rows = %q, %q; want the parsable one first", l.rows[0].ref, l.rows[1].ref)
	}
	if _, err := l.entries(); err == nil {
		t.Error("an unparsable row must be reported on save")
	}
}

// The user's reproduction: deleting the Patient Name row and re-adding it
// through the picker used to break the link to Patient ID, which was inferred
// from hardcoded tag numbers. The link is now written in the profile, so it
// survives the row being removed and recreated.
func TestSetValueReferenceSurvivesDeleteAndReadd(t *testing.T) {
	test.NewApp()
	l := newSetValueList([]string{"0010,0010=ANON", "0010,0020=[0010,0010]"})

	// Delete the Patient Name row, exactly as its Delete button does.
	name := tag.Tag{Group: 0x0010, Element: 0x0010}
	for i, r := range l.rows {
		if r.t == name {
			l.rows = append(l.rows[:i], l.rows[i+1:]...)
			break
		}
	}
	l.rebuild()

	// Re-add it the way chooseTags does for a newly checked tag: canonical
	// reference, empty value.
	l.rows = append(l.rows, newSetValueRow("0010,0010="))
	l.sortRows()
	l.rebuild()

	saved, err := l.entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if want := []string{"0010,0010=", "0010,0020=[0010,0010]"}; !reflect.DeepEqual(saved, want) {
		t.Fatalf("saved = %v, want %v — the reference must survive", saved, want)
	}
	// And it still resolves: typing a name into the re-added row reaches
	// Patient ID, which is the behaviour that was lost.
	l.rows[0].value.SetText("NEW^NAME")
	saved, err = l.entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	resolvedSets, err := resolveSetReferences(saved)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := []string{"0010,0010=NEW^NAME", "0010,0020=NEW^NAME"}; !reflect.DeepEqual(resolvedSets, want) {
		t.Errorf("resolved = %v, want %v", resolvedSets, want)
	}
}

// A value bracketed like a reference but naming no tag is a typo, and storing
// it would write "[patient name]" into every exported file.
func TestSetValueListRejectsMalformedReference(t *testing.T) {
	test.NewApp()
	l := newSetValueList([]string{"0010,0020=[patient name]"})
	_, err := l.entries()
	if err == nil {
		t.Fatal("a bracketed non-reference must be rejected")
	}
	if !strings.Contains(err.Error(), "not a tag reference") {
		t.Errorf("error = %v, want it to explain the [GGGG,EEEE] form", err)
	}
}

// Whatever spelling is typed, what gets stored is canonical. This is the
// inverse of the behaviour the editor used to have — it preserved the typed
// form to protect literal string matching in mergeModProfiles, which now
// compares parsed tags instead.
func TestModProfileEditorCanonicalisesTypedTags(t *testing.T) {
	test.NewApp()
	ed := newModProfileEditor("a", ModProfile{}, ModProfileConfig{"a": {}})
	ed.fields.removes.SetText("8,80\n0010,0010\n40,a730")
	_, updated, err := ed.validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	want := []string{"0008,0080", "0010,0010", "0040,A730"}
	if !reflect.DeepEqual(updated.Removes, want) {
		t.Errorf("Removes = %v, want %v", updated.Removes, want)
	}
}

// A phrase that used to be a tags.json alias is now simply not a tag, and must
// be reported rather than silently stored as an entry that matches nothing.
func TestModProfileEditorRejectsNonTagText(t *testing.T) {
	test.NewApp()
	ed := newModProfileEditor("a", ModProfile{}, ModProfileConfig{"a": {}})
	ed.fields.removes.SetText("patient name")
	if _, _, err := ed.validate(); err == nil {
		t.Fatal("a non-tag phrase must be rejected now that aliases are gone")
	} else if !strings.Contains(err.Error(), "not a GGGG,EEEE tag") {
		t.Errorf("error = %v, want it to say the entry is not a tag", err)
	}
}

func TestModProfileEditorValidationErrors(t *testing.T) {
	test.NewApp()
	cases := []struct {
		name    string
		mutate  func(ed *modProfileEditor)
		errPart string
	}{
		{"empty name", func(ed *modProfileEditor) { ed.nameEntry.SetText("") }, "must not be empty"},
		{"rename collision", func(ed *modProfileEditor) { ed.nameEntry.SetText("other") }, "already exists"},
		// A row can only carry a non-tag reference if it came from a hand-edited
		// profiles.json; the editor reports it rather than dropping the line.
		{"unparsable set row", func(ed *modProfileEditor) {
			ed.fields.sets.rows = append(ed.fields.sets.rows, newSetValueRow("garbage=X"))
		}, "not a GGGG,EEEE tag"},
		{"set value violates the tag's VR", func(ed *modProfileEditor) {
			ed.fields.sets.rows = append(ed.fields.sets.rows, newSetValueRow("0008,0020=ANON"))
		}, "is a DA value"},
		{"bad remove tag", func(ed *modProfileEditor) { ed.fields.removes.SetText("notatag") }, "not a GGGG,EEEE tag"},
		{"base cycle", func(ed *modProfileEditor) { ed.baseSelect.SetSelected("other") }, "circular"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ModProfileConfig{
				"a":     {Sets: []string{"0010,0010=X"}},
				"other": {Base: "a", Removes: []string{"0010,1000"}},
			}
			ed := newModProfileEditor("a", cfg["a"], cfg)
			tc.mutate(ed)
			if _, _, err := ed.validate(); err == nil || !strings.Contains(err.Error(), tc.errPart) {
				t.Errorf("validate error = %v, want containing %q", err, tc.errPart)
			}
		})
	}
}

// TestModProfileEditorRemapDisablesSuffix covers the Remap UIDs / UID suffix
// mutual exclusion in the editor: checking remap disables the suffix entry and
// greys its label, saving with remap checked stores an empty suffix (the
// disabled entry's text is inert), and unchecking restores the typed suffix.
func TestModProfileEditorRemapDisablesSuffix(t *testing.T) {
	test.NewApp()
	p := ModProfile{UIDSuffix: "7"}
	ed := newModProfileEditor("a", p, ModProfileConfig{"a": p})

	if ed.uidSfx.Disabled() {
		t.Fatalf("suffix entry disabled while remap is unchecked")
	}
	if ed.uidSuffixLabel.Importance == widget.LowImportance {
		t.Fatalf("suffix label greyed while remap is unchecked")
	}

	ed.remapCheck.SetChecked(true)
	if !ed.uidSfx.Disabled() {
		t.Errorf("suffix entry still enabled with remap checked")
	}
	if ed.uidSuffixLabel.Importance != widget.LowImportance {
		t.Errorf("suffix label not greyed with remap checked")
	}
	_, updated, err := ed.validate()
	if err != nil {
		t.Fatalf("validate with remap checked: %v", err)
	}
	if !updated.RemapUIDs || updated.UIDSuffix != "" {
		t.Errorf("saved remap=%v suffix=%q, want remap with cleared suffix", updated.RemapUIDs, updated.UIDSuffix)
	}

	ed.remapCheck.SetChecked(false)
	if ed.uidSfx.Disabled() {
		t.Errorf("suffix entry still disabled after unchecking remap")
	}
	_, updated, err = ed.validate()
	if err != nil {
		t.Fatalf("validate with remap unchecked: %v", err)
	}
	if updated.RemapUIDs || updated.UIDSuffix != "7" {
		t.Errorf("saved remap=%v suffix=%q, want suffix 7 restored", updated.RemapUIDs, updated.UIDSuffix)
	}
}

// TestModProfileEditorWindowLifecycle covers what changed when the editor
// became a movable, resizable window instead of a modal dialog: it is no
// longer modal, so a second Edit on the same profile must raise the existing
// window rather than open a rival copy, and Preferences closing must take any
// open editor with it (an orphan would save into a discarded pending map).
func TestModProfileEditorWindowLifecycle(t *testing.T) {
	a := test.NewApp()
	resetOwnedWindows(t)

	// Opened through the helper exactly as showPreferencesDialog does, which
	// is what installs the close hook the cascade below depends on.
	root := a.NewWindow("main")
	prefs := openOwnedWindow(a, windowSpec{
		Key: "preferences", Title: "Preferences", Parent: root, Blocking: true,
	}, func(fyne.Window) fyne.CanvasObject { return widget.NewLabel("preferences") })
	cfg := ModProfileConfig{"a": {Sets: []string{"0010,0010=X"}}, "b": {}}
	noSave := func(string, ModProfile) { t.Fatal("onSave called unexpectedly") }

	showModProfileEditor(a, prefs, "a", cfg["a"], cfg, noSave)
	if got := ownedChildCount(prefs); got != 1 {
		t.Fatalf("editors open = %d, want 1", got)
	}
	// Editing a profile blocks Preferences: the editor saves back into the
	// pending list Preferences owns.
	if got := len(prefs.Canvas().Overlays().List()); got != 1 {
		t.Errorf("Preferences not blocked while its editor is open (overlays = %d)", got)
	}

	// Re-opening the same profile must raise the window, not duplicate it.
	showModProfileEditor(a, prefs, "a", cfg["a"], cfg, noSave)
	if got := ownedChildCount(prefs); got != 1 {
		t.Errorf("editors open = %d after re-opening the same profile, want 1", got)
	}

	// A different profile is a different window.
	showModProfileEditor(a, prefs, "b", cfg["b"], cfg, noSave)
	if got := ownedChildCount(prefs); got != 2 {
		t.Errorf("editors open = %d with two profiles, want 2", got)
	}

	// Closing Preferences takes every editor with it — an editor outliving it
	// would save into a pending map that is no longer going anywhere.
	prefs.Close()
	if raiseOwnedWindow(modProfileEditorKey("a")) || raiseOwnedWindow(modProfileEditorKey("b")) {
		t.Errorf("an editor survived its Preferences window")
	}
}

// TestPerModalityEditorSave checks code normalization, the collision check and
// that the four modality-specific fields land in the saved override. The
// scalar de-identification options are profile-wide and have no controls here.
func TestPerModalityEditorSave(t *testing.T) {
	test.NewApp()
	ed := newPerModalityEditor("", ModProfile{}, []string{"CT"})

	ed.codeEntry.SetText("ct") // normalizes to CT — collides with the taken code
	if _, _, err := ed.validate(); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("collision not detected: %v", err)
	}

	ed.codeEntry.SetText("us")
	ed.fields.sets.rows = append(ed.fields.sets.rows, newSetValueRow("0010,0010=X"))
	ed.fields.removes.SetText("0018,1030")
	ed.fields.keep.SetText("0010,0020")
	ed.keepPrivCheck.SetChecked(true)

	code, updated, err := ed.validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if code != "US" {
		t.Errorf("code = %q, want US", code)
	}
	want := ModProfile{
		Sets: []string{"0010,0010=X"}, Removes: []string{"0018,1030"}, Keep: []string{"0010,0020"},
		KeepPrivate: true,
	}
	if !reflect.DeepEqual(updated, want) {
		t.Errorf("override = %+v, want %+v", updated, want)
	}
}

// TestPerModalityEditorPreservesUnhonoredFields verifies a hand-authored block
// keeps every field the sub-editor has no controls for: both the profile-wide
// scalars (which the engine still applies per modality) and the ones the
// engine ignores inside an override.
func TestPerModalityEditorPreservesUnhonoredFields(t *testing.T) {
	test.NewApp()
	nested := ModProfile{
		Base: "x", RemapUIDs: true, Dicomdir: true, Removes: []string{"0018,1030"},
		DOB: "19000101", UIDSuffix: "7", ShiftDays: "-7", FixVR: "skip", Priv: true,
	}
	ed := newPerModalityEditor("CT", nested, nil)
	code, updated, err := ed.validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if code != "CT" {
		t.Errorf("code = %q, want CT", code)
	}
	if updated.Base != "x" || !updated.RemapUIDs || !updated.Dicomdir {
		t.Errorf("engine-ignored fields lost: %+v", updated)
	}
	if updated.DOB != "19000101" || updated.UIDSuffix != "7" || updated.ShiftDays != "-7" ||
		updated.FixVR != "skip" || !updated.Priv {
		t.Errorf("profile-wide scalars lost from the override: %+v", updated)
	}
	if !reflect.DeepEqual(updated.Removes, nested.Removes) {
		t.Errorf("Removes = %v, want %v", updated.Removes, nested.Removes)
	}

	// Such a block must be disclosed rather than silently uneditable.
	if note := perModalityPreservedNote(nested); note == nil {
		t.Errorf("preserved-fields note missing for a block carrying uneditable values")
	}
	if note := perModalityPreservedNote(ModProfile{Removes: []string{"0018,1030"}}); note != nil {
		t.Errorf("preserved-fields note shown for a block with nothing uneditable")
	}
}

func TestPerModalityEditorValidation(t *testing.T) {
	test.NewApp()
	cases := []struct {
		name    string
		mutate  func(ed *perModalityEditor)
		errPart string
	}{
		{"empty code", func(ed *perModalityEditor) { ed.codeEntry.SetText(" ") }, "must not be empty"},
		{"internal space", func(ed *perModalityEditor) { ed.codeEntry.SetText("C T") }, "single word"},
		// The engine silently drops unparsable per-modality tag lines — the
		// editor must reject them instead, so the user gets feedback.
		{"unparsable remove", func(ed *perModalityEditor) { ed.fields.removes.SetText("notatag") }, "not a GGGG,EEEE tag"},
		{"unparsable keep", func(ed *perModalityEditor) { ed.fields.keep.SetText("nope") }, "not a GGGG,EEEE tag"},
		{"unparsable set row", func(ed *perModalityEditor) {
			ed.fields.sets.rows = append(ed.fields.sets.rows, newSetValueRow("garbage=X"))
		}, "not a GGGG,EEEE tag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ed := newPerModalityEditor("CT", ModProfile{}, nil)
			tc.mutate(ed)
			if _, _, err := ed.validate(); err == nil || !strings.Contains(err.Error(), tc.errPart) {
				t.Errorf("validate error = %v, want containing %q", err, tc.errPart)
			}
		})
	}
}

// TestModProfileEditorPerModalityFlow feeds a sub-editor result into the main
// editor the way the dialog wiring does and checks the final profile, plus the
// empty-map-normalizes-to-nil rule.
func TestModProfileEditorPerModalityFlow(t *testing.T) {
	test.NewApp()
	p := ModProfile{Sets: []string{"0010,0010=ANON"}}
	ed := newModProfileEditor("prof", p, ModProfileConfig{"prof": p})

	sub := newPerModalityEditor("", ModProfile{}, otherCodes(ed.perMod, ""))
	sub.codeEntry.SetText("mr")
	sub.fields.removes.SetText("0018,1030")
	code, override, err := sub.validate()
	if err != nil {
		t.Fatalf("sub validate: %v", err)
	}
	if ed.perMod == nil {
		ed.perMod = map[string]ModProfile{}
	}
	ed.perMod[code] = override

	_, updated, err := ed.validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := updated.PerModality["MR"].Removes; !reflect.DeepEqual(got, []string{"0018,1030"}) {
		t.Errorf("PerModality[MR].Removes = %v, want [0018,1030]", got)
	}

	ed.perMod = map[string]ModProfile{}
	_, updated, err = ed.validate()
	if err != nil {
		t.Fatalf("validate after clear: %v", err)
	}
	if updated.PerModality != nil {
		t.Errorf("empty PerModality should normalize to nil, got %+v", updated.PerModality)
	}
}
