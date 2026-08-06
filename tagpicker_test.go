package main

// Tests for the tag dictionary enumeration and the picker's selection merge.
// The merge tests are the important ones: what the picker writes back into a
// profile's Remove or Keep list is what mergeModProfiles then matches a base
// profile against, and a Keep entry that stops cancelling its base's Removes
// deletes a tag the user asked to preserve.

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/suyashkumar/dicom/pkg/tag"
)

func TestDictionaryTags(t *testing.T) {
	tags := dictionaryTags()
	if len(tags) < 5000 {
		t.Fatalf("dictionaryTags returned %d entries, want the full dictionary (~5100)", len(tags))
	}

	// Ordered by group then element, and free of duplicates.
	for i := 1; i < len(tags); i++ {
		prev, cur := tags[i-1].Tag, tags[i].Tag
		if cur.Group < prev.Group || (cur.Group == prev.Group && cur.Element <= prev.Element) {
			t.Fatalf("entries %d..%d out of order or duplicated: %v then %v", i-1, i, prev, cur)
		}
	}

	// Well-known tags must be present with their names.
	for _, want := range []struct {
		t    tag.Tag
		name string
	}{
		{tag.PatientName, "Patient's Name"},
		{tag.PatientID, "Patient ID"},
		{tag.StudyInstanceUID, "Study Instance UID"},
		{tag.PixelData, "Pixel Data"},
	} {
		found := false
		for _, info := range tags {
			if info.Tag == want.t {
				found = true
				if info.Name != want.name {
					t.Errorf("%v name = %q, want %q", want.t, info.Name, want.name)
				}
				break
			}
		}
		if !found {
			t.Errorf("dictionary is missing %v (%s)", want.t, want.name)
		}
	}

	// tag.Find synthesises "Generic Group Length" for element 0000 of every
	// even group; those phantoms must not reach the picker.
	for _, info := range tags {
		if info.Keyword == "GenericGroupLength" {
			t.Errorf("synthesised group-length entry leaked into the dictionary: %v", info.Tag)
		}
	}

	// Retired tags are included (a de-identification list may well need them)
	// and flagged, so the picker can offer to hide them.
	retired := 0
	for _, info := range tags {
		if info.Retired {
			retired++
		}
	}
	if retired == 0 {
		t.Errorf("no retired tags found — the Hide retired toggle would be pointless")
	}
}

// TestDictionaryGroupsCoverLibrary guards the hand-maintained group list: the
// dictionary is unexported and has no iterator, so the picker probes tag.Find
// over dicomStandardGroups. A group added by a library upgrade would silently
// disappear from the picker, so compare against the library's own source.
func TestDictionaryGroupsCoverLibrary(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/suyashkumar/dicom").Output()
	if err != nil {
		t.Skipf("cannot locate the dicom module (offline module cache?): %v", err)
	}
	src := filepath.Join(strings.TrimSpace(string(out)), "pkg", "tag", "tag_definitions.go")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("cannot read %s: %v", src, err)
	}

	known := make(map[uint16]bool, len(dicomStandardGroups))
	for _, g := range dicomStandardGroups {
		known[g] = true
	}

	re := regexp.MustCompile(`=\s*Tag\{0x([0-9A-Fa-f]{4}),\s*0x[0-9A-Fa-f]{4}\}`)
	matches := re.FindAllSubmatch(data, -1)
	if len(matches) < 5000 {
		t.Fatalf("found only %d tag definitions in %s — the source layout changed, "+
			"so this guard is no longer checking anything", len(matches), src)
	}
	missing := map[uint16]bool{}
	for _, m := range matches {
		v, err := strconv.ParseUint(string(m[1]), 16, 16)
		if err != nil {
			continue
		}
		if g := uint16(v); !known[g] {
			missing[g] = true
		}
	}
	for g := range missing {
		t.Errorf("group %04X exists in the library dictionary but is absent from "+
			"dicomStandardGroups — the picker would never show its tags", g)
	}
}

func TestTagMatchesQuery(t *testing.T) {
	info, err := tag.Find(tag.PatientName) // 0010,0010 "Patient's Name"
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	for _, q := range []string{"", "patient", "PATIENT", "patientname", "0010,0010", "0010", "00100010"} {
		if !tagMatchesQuery(info, strings.ToLower(q)) {
			t.Errorf("tagMatchesQuery(PatientName, %q) = false, want true", q)
		}
	}
	for _, q := range []string{"institution", "9999"} {
		if tagMatchesQuery(info, q) {
			t.Errorf("tagMatchesQuery(PatientName, %q) = true, want false", q)
		}
	}
}

func TestTagSelectionFromLines(t *testing.T) {
	// A zero-stripped reference, a padded one, and an unresolvable line.
	got := tagSelectionFromLines([]string{"8,80", "0010,0010", "garbage"})
	want := map[tag.Tag]bool{
		{Group: 0x0008, Element: 0x0080}: true,
		{Group: 0x0010, Element: 0x0010}: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("selection = %v, want %v", got, want)
	}
}

// The picker used to preserve each surviving line's spelling, because
// mergeModProfiles compared tag references as literal strings and rewriting
// "8,80" as "0008,0080" would stop a Keep list cancelling its base's Removes.
// That comparison now parses the reference, so the picker canonicalises
// instead — and this test is the guard that it does, since a half-canonical
// file is what the old fragility fed on.
func TestMergeTagSelectionCanonicalises(t *testing.T) {
	existing := []string{"8,80", "10,10", "0018,1030"}
	selected := map[tag.Tag]bool{
		{Group: 0x0008, Element: 0x0080}: true,  // kept, rewritten canonically
		{Group: 0x0010, Element: 0x0010}: true,  // kept, rewritten canonically
		{Group: 0x0018, Element: 0x1030}: false, // unchecked — dropped
		{Group: 0x0010, Element: 0x1000}: true,  // newly added
	}
	got := mergeTagSelection(existing, selected)
	want := []string{"0008,0080", "0010,0010", "0010,1000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged = %v, want %v", got, want)
	}
}

func TestMergeTagSelectionEdgeCases(t *testing.T) {
	t.Run("unresolvable lines survive for validation", func(t *testing.T) {
		got := mergeTagSelection([]string{"notatag"}, map[tag.Tag]bool{})
		if !reflect.DeepEqual(got, []string{"notatag"}) {
			t.Errorf("merged = %v, want [notatag] preserved", got)
		}
	})

	t.Run("empty selection clears resolvable lines", func(t *testing.T) {
		if got := mergeTagSelection([]string{"8,80"}, map[tag.Tag]bool{}); got != nil {
			t.Errorf("merged = %v, want nil", got)
		}
	})

	t.Run("additions are sorted by group then element", func(t *testing.T) {
		selected := map[tag.Tag]bool{
			{Group: 0x0018, Element: 0x1030}: true,
			{Group: 0x0008, Element: 0x0080}: true,
			{Group: 0x0008, Element: 0x0020}: true,
		}
		want := []string{"0008,0020", "0008,0080", "0018,1030"}
		if got := mergeTagSelection(nil, selected); !reflect.DeepEqual(got, want) {
			t.Errorf("merged = %v, want %v", got, want)
		}
	})

	t.Run("two spellings of one tag collapse to one canonical entry", func(t *testing.T) {
		selected := map[tag.Tag]bool{{Group: 0x0008, Element: 0x0080}: true}
		got := mergeTagSelection([]string{"8,80", "0008,0080"}, selected)
		if !reflect.DeepEqual(got, []string{"0008,0080"}) {
			t.Errorf("merged = %v, want [0008,0080]", got)
		}
	})

	t.Run("false entries are not treated as selected", func(t *testing.T) {
		selected := map[tag.Tag]bool{{Group: 0x0008, Element: 0x0080}: false}
		if got := mergeTagSelection(nil, selected); got != nil {
			t.Errorf("merged = %v, want nil", got)
		}
	})
}

// TestMergeTagSelectionRoundTripsThroughValidation proves the picker's output
// is accepted by the editor's own tag-list validation.
func TestMergeTagSelectionRoundTripsThroughValidation(t *testing.T) {
	merged := mergeTagSelection([]string{"8,80", "10,10"},
		map[tag.Tag]bool{
			{Group: 0x0008, Element: 0x0080}: true,
			{Group: 0x0010, Element: 0x0010}: true,
			{Group: 0x0010, Element: 0x1000}: true,
		})
	lines, err := checkTagLines("Remove tags", strings.Join(merged, "\n"))
	if err != nil {
		t.Fatalf("picker output rejected by checkTagLines: %v", err)
	}
	if !reflect.DeepEqual(lines, merged) {
		t.Errorf("validated lines = %v, want %v", lines, merged)
	}
}

// TestTagPickerOpensOnCurrentEntries is the end-to-end pre-selection check: a
// profile's existing Remove list must arrive checked, be countable per group,
// and be reachable without opening 76 collapsed groups by hand.
func TestTagPickerOpensOnCurrentEntries(t *testing.T) {
	profiles := embeddedModConfigs(t)
	base, err := resolveModProfile("base-deident", profiles)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(base.Removes) == 0 {
		t.Fatal("base-deident has no remove entries to pre-select")
	}

	// Exactly what the Choose… button does with the field's text.
	field := strings.Join(base.Removes, "\n")
	m := newTagPickerModel(tagSelectionFromLines(splitProfileLines(field)))

	if got := m.selectedCount(); got != len(base.Removes) {
		t.Errorf("pre-selected %d tags, want all %d profile entries", got, len(base.Removes))
	}
	// A representative entry, written zero-stripped as "8,80" in the defaults.
	inst := tag.Tag{Group: 0x0008, Element: 0x0080}
	if !m.selected[inst] {
		t.Errorf("InstitutionName not pre-selected from its 8,80 spelling")
	}
	if _, ok := m.byID[tagNodeID(inst)]; !ok {
		t.Errorf("InstitutionName has no row to show its checked state")
	}

	// Per-group tallies must account for every selected tag, so a collapsed
	// group still advertises the selection inside it.
	tally := 0
	for _, n := range m.selByGroup {
		tally += n
	}
	if tally != m.selectedCount() {
		t.Errorf("per-group tally = %d, want %d", tally, m.selectedCount())
	}
	if m.selByGroup[0x0008] == 0 {
		t.Errorf("group 0008 reports no selection despite holding InstitutionName")
	}
	if got := groupPickerLabel(0x0008, 264, m.selByGroup[0x0008]); !strings.Contains(got, "selected") {
		t.Errorf("group label %q does not surface the selection", got)
	}

	// Selected-only mode is how those entries become visible on open. The
	// shipped profile names three tags the library's dictionary lacks
	// (0010,3020 and two in group 0070), which have no row to show.
	if m.selectedUnknown() == 0 {
		t.Errorf("expected base-deident to name tags absent from the dictionary")
	}
	m.showSelectedOnly = true
	m.rebuild()
	if want := m.selectedCount() - m.selectedUnknown(); m.visibleCount() != want {
		t.Errorf("selected-only shows %d tags, want %d (selected minus those with no row)",
			m.visibleCount(), want)
	}
	for _, infos := range m.byGroup {
		for _, info := range infos {
			if !m.selected[info.Tag] {
				t.Fatalf("unselected tag %v shown in selected-only mode", info.Tag)
			}
		}
	}

	// Applying without touching anything must leave the profile unchanged. The
	// shipped defaults are already canonical, so this is now a byte-identical
	// round trip rather than a spelling-preserving one.
	merged := mergeTagSelection(splitProfileLines(field), m.selected)
	if !reflect.DeepEqual(merged, base.Removes) {
		t.Errorf("round-trip through the picker changed the entries:\n got %v\nwant %v", merged, base.Removes)
	}
}

// TestTagPickerSelectionTally covers the incremental per-group counter, which
// is maintained by hand rather than recomputed on every row render.
func TestTagPickerSelectionTally(t *testing.T) {
	m := newTagPickerModel(map[tag.Tag]bool{})
	inst := tag.Tag{Group: 0x0008, Element: 0x0080}
	name := tag.Tag{Group: 0x0008, Element: 0x0090}

	m.setSelected(inst, true)
	m.setSelected(name, true)
	if got := m.selByGroup[0x0008]; got != 2 {
		t.Errorf("group tally = %d, want 2", got)
	}
	m.setSelected(inst, true) // idempotent — must not double-count
	if got := m.selByGroup[0x0008]; got != 2 {
		t.Errorf("re-selecting changed the tally to %d, want 2", got)
	}
	m.setSelected(inst, false)
	m.setSelected(name, false)
	if _, ok := m.selByGroup[0x0008]; ok {
		t.Errorf("emptied group still present in the tally")
	}
	if got := m.selectedCount(); got != 0 {
		t.Errorf("selectedCount = %d, want 0", got)
	}

	// Clearing drops non-dictionary tags too (a hand-typed private tag has no
	// row to untick), so the tally and the selection stay consistent.
	priv := tag.Tag{Group: 0x0009, Element: 0x0010}
	m.setSelected(priv, true)
	m.setSelected(inst, true)
	m.clearSelection()
	if m.selectedCount() != 0 || len(m.selByGroup) != 0 {
		t.Errorf("clearSelection left %d selected across %d groups", m.selectedCount(), len(m.selByGroup))
	}
}

// TestDecorateTagList covers the display/storage split for the editor's tag
// lists: names are appended for readability and stripped again on save, and
// unresolvable entries are shown bare.
func TestDecorateTagList(t *testing.T) {
	// Every resolvable entry displays as a zero-padded GGGG,EEEE reference,
	// whatever spelling the profile stores; an unresolvable one shows as-is.
	got := decorateTagList([]string{"8,80", "0010,1000", "10,10", "notatag"})
	want := strings.Join([]string{
		"0008,0080  Institution Name",
		"0010,1000  Other Patient IDs",
		"0010,0010  Patient's Name",
		"notatag",
	}, "\n")
	if got != want {
		t.Errorf("decorated:\n%s\nwant:\n%s", got, want)
	}

	// Decorating already-decorated text must not append the name twice.
	if again := decorateTagList(strippedTagLines(got)); again != want {
		t.Errorf("re-decorating changed the text:\n%s", again)
	}
}

// The name shown beside a tag is display only and must never reach
// profiles.json; what is stored is the canonical reference.
func TestTagListDecorationNeverReachesStorage(t *testing.T) {
	stored := []string{"8,80", "10,10", "0010,1000"}

	displayed := decorateTagList(stored)
	for _, wantLine := range []string{
		"0008,0080  Institution Name",
		"0010,0010  Patient's Name",
		"0010,1000  Other Patient IDs",
	} {
		if !strings.Contains(displayed, wantLine) {
			t.Errorf("display is missing %q:\n%s", wantLine, displayed)
		}
	}

	// Saving strips the names and canonicalises the references.
	lines, err := checkTagLines("Remove tags", displayed)
	if err != nil {
		t.Fatalf("checkTagLines rejected decorated text: %v", err)
	}
	want := []string{"0008,0080", "0010,0010", "0010,1000"}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("saved lines = %v, want %v", lines, want)
	}

	// A decorated line whose reference is bad is still reported, and the error
	// names the reference rather than the appended description.
	if _, err := checkTagLines("Remove tags", "notatag  Something"); err == nil {
		t.Errorf("unresolvable decorated line accepted")
	} else if !strings.Contains(err.Error(), `"notatag"`) {
		t.Errorf("error names the wrong text: %v", err)
	}
}

// TestFieldSetRoundTripsDecoratedLists drives the editor end to end: a profile
// opens with names shown and saves byte-identical when nothing is edited.
func TestFieldSetRoundTripsDecoratedLists(t *testing.T) {
	test.NewApp()
	// The shipped defaults, which are canonical on disk.
	profiles := embeddedModConfigs(t)
	base, err := resolveModProfile("base-deident", profiles)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	cfg := ModProfileConfig{"p": base}
	ed := newModProfileEditor("p", base, cfg)

	// The profile stores "8,80"; the field must show it zero-padded with its
	// name, and every displayed reference must be 4+4 digits.
	if !strings.HasPrefix(ed.fields.removes.Text, "0008,0080  Institution Name") {
		t.Errorf("Remove tags field is not padded/named: %q",
			strings.SplitN(ed.fields.removes.Text, "\n", 2)[0])
	}
	for _, line := range strings.Split(ed.fields.removes.Text, "\n") {
		ref := stripTagDecoration(line)
		if len(ref) != 9 || ref[4] != ',' {
			t.Errorf("displayed reference %q is not GGGG,EEEE", ref)
		}
	}

	_, updated, err := ed.validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !reflect.DeepEqual(updated.Removes, base.Removes) {
		t.Errorf("Removes changed by an untouched round trip:\n got %v\nwant %v",
			updated.Removes, base.Removes)
	}
	if !reflect.DeepEqual(updated.Keep, base.Keep) {
		t.Errorf("Keep changed by an untouched round trip:\n got %v\nwant %v",
			updated.Keep, base.Keep)
	}
}

// TestTagPickerWindow covers the picker as an owned window: it belongs to the
// editor that opened it and blocks it, because the picker snapshots the
// field's text on opening and editing that field behind it would be
// overwritten by Apply.
func TestTagPickerWindow(t *testing.T) {
	a := test.NewApp()
	resetOwnedWindows(t)

	editor := a.NewWindow("editor")
	editor.SetContent(widget.NewLabel("editor"))

	showTagPicker(a, editor, "Choose tags to remove", "8,80", func([]string) {
		t.Fatal("onApply ran without Apply being pressed")
	})

	children := ownedChildren(editor)
	if len(children) != 1 {
		t.Fatalf("picker windows open = %d, want 1", len(children))
	}
	if got := len(editor.Canvas().Overlays().List()); got != 1 {
		t.Errorf("the picker did not block its editor (overlays = %d)", got)
	}

	// Closing releases the editor rather than leaving it inert.
	children[0].Close()
	if got := len(editor.Canvas().Overlays().List()); got != 0 {
		t.Errorf("editor still blocked after the picker closed (overlays = %d)", got)
	}
	if got := ownedChildCount(editor); got != 0 {
		t.Errorf("editor children = %d after the picker closed, want 0", got)
	}
}

func TestTagPickerModelFilter(t *testing.T) {
	m := newTagPickerModel(map[tag.Tag]bool{})
	total := m.visibleCount()
	if total < 5000 {
		t.Fatalf("unfiltered model shows %d tags, want the whole dictionary", total)
	}
	if len(m.groups) < 50 {
		t.Errorf("unfiltered model shows %d groups, want ~76", len(m.groups))
	}

	m.filter = "patient's name"
	m.rebuild()
	if got := m.visibleCount(); got == 0 || got > 20 {
		t.Errorf("filtered count = %d, want a small non-zero set", got)
	}
	if _, ok := m.byID[tagNodeID(tag.PatientName)]; !ok {
		t.Errorf("filter %q did not match PatientName", m.filter)
	}
	// Only groups with matches remain, and each is a branch.
	for _, g := range m.groups {
		if len(m.byGroup[g]) == 0 {
			t.Errorf("group %04X kept with no matching tags", g)
		}
		if !m.isBranch(groupNodeID(g)) {
			t.Errorf("group node %s is not a branch", groupNodeID(g))
		}
	}

	m.filter = ""
	m.hideRetired = true
	m.rebuild()
	if got := m.visibleCount(); got >= total {
		t.Errorf("hiding retired tags showed %d of %d — nothing was hidden", got, total)
	}
	for _, infos := range m.byGroup {
		for _, info := range infos {
			if info.Retired {
				t.Fatalf("retired tag %v shown while hideRetired is set", info.Tag)
			}
		}
	}

	// The virtual root must be a branch or widget.Tree never walks the model.
	if !m.isBranch("") {
		t.Errorf("virtual root is not a branch")
	}
	if got := len(m.childUIDs("")); got != len(m.groups) {
		t.Errorf("root children = %d, want %d groups", got, len(m.groups))
	}
	if kids := m.childUIDs(tagNodeID(tag.PatientName)); kids != nil {
		t.Errorf("tag node reported children: %v", kids)
	}
}
