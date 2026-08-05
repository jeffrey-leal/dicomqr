package main

// Modification-profile editor — opened from Preferences > Modification &
// Export. Edits one named ModProfile from ~/.dicomqr/profiles.json. The
// top-level editor owns every de-identification option the engine honors: the
// tag lists, the scalar options (birth-date mask, UID suffix, date shift,
// fixvr, private-tag removal, UID remapping), the zip output default and the
// ignoretype/ignoremodality file filters. A per-modality override, edited in a
// nested sub-editor, may only vary what is genuinely modality-specific — the
// tag lists plus keepprivate — since the scalar options are profile-wide
// decisions; the engine would honor a hand-authored per-modality scalar, so
// the sub-editor discloses rather than hides one.
//
// Anything without a control is preserved unchanged, so a hand-authored (or
// dicomtool-authored) profile survives a round-trip through the editor:
// dicomdir and verbose (never read by the engine), a top-level keepprivate
// (honored only inside per-modality blocks), and any per-modality scalar.

import (
	"fmt"
	"image/color"
	"maps"
	"slices"
	"sort"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// modProfileNoBaseLabel is the Base select option meaning "no base profile".
const modProfileNoBaseLabel = "(none)"

// splitProfileLines converts a multi-line entry into a slice with one trimmed,
// non-empty element per line.
func splitProfileLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// splitCommaList converts a comma-separated entry into a slice with one
// trimmed, non-empty element per item.
func splitCommaList(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// checkTagLines validates a tag-list entry (one tag or alias per line) and
// returns the references to store; field names the entry in error messages.
//
// The editor displays every entry as a zero-padded GGGG,EEEE reference with
// the tag's name appended, so neither is what should be stored. The names are
// stripped, and any tag orig already listed is written back in orig's own
// spelling — a profile written as "8,80" or as a tags.json alias keeps that
// form, because mergeModProfiles cancels Keep against Removes by literal
// string equality. Tags not in orig are stored as the line reads.
func checkTagLines(field, text string, orig []string, aliases TagConfig) ([]string, error) {
	spelling := make(map[tag.Tag]string, len(orig))
	for _, o := range orig {
		o = strings.TrimSpace(o)
		t, err := parseTagString(aliases.Resolve(o))
		if err != nil {
			continue
		}
		if _, dup := spelling[t]; !dup {
			spelling[t] = o // first spelling wins, matching the stored order
		}
	}

	var out []string
	for _, line := range strippedTagLines(text) {
		t, err := parseTagString(aliases.Resolve(line))
		if err != nil {
			return nil, fmt.Errorf("%s: %q is neither a GGGG,EEEE tag nor a tags.json alias", field, line)
		}
		if stored, ok := spelling[t]; ok {
			out = append(out, stored)
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// modProfileFieldSet groups the three tag-list controls — the only ModProfile
// fields both the top-level editor and the per-modality sub-editor expose. The
// scalar de-identification options (birth-date mask, UID suffix, date shift,
// fixvr, private-tag removal) are profile-wide and belong to the top-level
// editor alone. Layout is left to the caller; the set owns construction,
// seeding, validation and write-back.
type modProfileFieldSet struct {
	sets, removes, keep *widget.Entry // multiline
	// The profile's stored tag lists, kept so an entry that was already there
	// is saved in its original spelling — the fields themselves display a
	// normalised, zero-padded form that must not reach the profile.
	origRemoves, origKeep []string
}

// newModProfileFieldSet builds the tag-list controls. The Remove and Keep
// lists are seeded with each tag's name appended for readability; the names
// are display only and stripped again on save.
func newModProfileFieldSet(p ModProfile, aliases TagConfig) *modProfileFieldSet {
	f := &modProfileFieldSet{origRemoves: p.Removes, origKeep: p.Keep}

	f.sets = widget.NewMultiLineEntry()
	f.sets.SetMinRowsVisible(5)
	f.sets.SetText(strings.Join(p.Sets, "\n"))
	f.sets.SetPlaceHolder("One TAG=VALUE per line, e.g.\npatient name=ANONYMOUS\n0010,0020=ID0000")

	f.removes = widget.NewMultiLineEntry()
	f.removes.SetMinRowsVisible(5)
	f.removes.SetText(decorateTagList(p.Removes, aliases))
	f.removes.SetPlaceHolder("One tag or alias per line, e.g.\nother patient ids\n0010,1000")

	f.keep = widget.NewMultiLineEntry()
	f.keep.SetMinRowsVisible(3)
	f.keep.SetText(decorateTagList(p.Keep, aliases))
	f.keep.SetPlaceHolder("Tags to keep even when the base profile removes them")

	return f
}

// applyValidated validates the tag lists and, only on success, overwrites the
// corresponding fields of dst; on error dst is untouched and the message is
// user-ready. The engine silently drops unparsable per-modality tag lines, so
// rejecting them here is the user's only feedback.
func (f *modProfileFieldSet) applyValidated(dst *ModProfile, aliases TagConfig) error {
	removes, err := checkTagLines("Remove tags", f.removes.Text, f.origRemoves, aliases)
	if err != nil {
		return err
	}
	keeps, err := checkTagLines("Keep tags", f.keep.Text, f.origKeep, aliases)
	if err != nil {
		return err
	}
	sets := splitProfileLines(f.sets.Text)
	for _, s := range sets {
		tagStr, _, ok := strings.Cut(s, "=")
		tagStr = strings.TrimSpace(tagStr)
		if !ok || tagStr == "" {
			return fmt.Errorf("Set values: %q is not of the form TAG=VALUE", s)
		}
		if _, err := parseTagString(aliases.Resolve(tagStr)); err != nil {
			return fmt.Errorf("Set values: tag %q is neither a GGGG,EEEE tag nor a tags.json alias", tagStr)
		}
	}

	dst.Sets = sets
	dst.Removes = removes
	dst.Keep = keeps
	return nil
}

// modProfileEditor holds the controls and working state of one top-level
// editor session. validate is the entire save path, so tests can drive it
// without a canvas.
type modProfileEditor struct {
	origName string
	orig     ModProfile
	cfg      ModProfileConfig
	aliases  TagConfig

	nameEntry  *widget.Entry
	baseSelect *widget.Select
	fields     *modProfileFieldSet
	// The scalar de-identification options are profile-wide: they have no
	// per-modality controls, so they live here rather than in the shared set.
	dob, uidSfx, shiftDays *widget.Entry
	fixvr                  *widget.Select
	privCheck              *widget.Check
	remapCheck             *widget.Check
	uidSuffixLabel         *widget.Label
	zipCheck               *widget.Check
	ignoreTypesEntry       *widget.Entry
	ignoreModsEntry        *widget.Entry
	// perMod is the working copy edited through the per-modality sub-editor.
	// The shallow clone is safe because sub-editor saves always build fresh
	// slices (updated := orig, whole-field overwrites) rather than mutating
	// nested slices in place. Cancelling the dialog simply discards it.
	perMod map[string]ModProfile
}

func newModProfileEditor(name string, p ModProfile, cfg ModProfileConfig, aliases TagConfig) *modProfileEditor {
	e := &modProfileEditor{origName: name, orig: p, cfg: cfg, aliases: aliases}

	e.nameEntry = widget.NewEntry()
	e.nameEntry.SetText(name)

	others := make([]string, 0, len(cfg))
	for n := range cfg {
		if n != name {
			others = append(others, n)
		}
	}
	if p.Base != "" && p.Base != name && !slices.Contains(others, p.Base) {
		others = append(others, p.Base) // dangling reference — keep it visible
	}
	sort.Strings(others)
	e.baseSelect = widget.NewSelect(append([]string{modProfileNoBaseLabel}, others...), nil)
	if p.Base != "" {
		e.baseSelect.SetSelected(p.Base)
	} else {
		e.baseSelect.SetSelected(modProfileNoBaseLabel)
	}

	e.fields = newModProfileFieldSet(p, aliases)

	e.dob = widget.NewEntry()
	e.dob.SetText(p.DOB)
	e.dob.SetPlaceHolder("YYYYMMDD — empty = no masking")

	e.uidSfx = widget.NewEntry()
	e.uidSfx.SetText(p.UIDSuffix)
	e.uidSfx.SetPlaceHolder("digits 1-9 — empty = none")

	e.shiftDays = widget.NewEntry()
	e.shiftDays.SetText(p.ShiftDays)
	e.shiftDays.SetPlaceHolder("e.g. -45 — shifts all dates except birth date")

	e.fixvr = widget.NewSelect([]string{fixvrOffLabel, "correct", "skip", "passthrough"}, nil)
	if m := strings.ToLower(strings.TrimSpace(p.FixVR)); m != "" {
		e.fixvr.SetSelected(m)
	} else {
		e.fixvr.SetSelected(fixvrOffLabel)
	}

	e.privCheck = widget.NewCheck("", nil)
	e.privCheck.SetChecked(p.Priv)

	// Remap UIDs and a UID suffix are mutually exclusive, so the suffix
	// control (sharing the Remap UIDs form row) is disabled — label greyed —
	// while remap is checked; validate() clears the inert text on save.
	e.remapCheck = widget.NewCheck("", nil)
	e.remapCheck.SetChecked(p.RemapUIDs)
	e.uidSuffixLabel = widget.NewLabel("UID suffix")
	e.remapCheck.OnChanged = func(bool) { e.syncUIDSuffixState() }
	e.syncUIDSuffixState()

	e.zipCheck = widget.NewCheck("", nil)
	e.zipCheck.SetChecked(p.Zip)

	e.ignoreTypesEntry = widget.NewEntry()
	e.ignoreTypesEntry.SetText(strings.Join(p.IgnoreTypes, ", "))
	e.ignoreTypesEntry.SetPlaceHolder("e.g. SECONDARY, LOCALIZER")

	e.ignoreModsEntry = widget.NewEntry()
	e.ignoreModsEntry.SetText(strings.Join(p.IgnoreModalities, ", "))
	e.ignoreModsEntry.SetPlaceHolder("e.g. SR, PR")

	e.perMod = maps.Clone(p.PerModality)
	return e
}

// syncUIDSuffixState enables or disables the UID suffix entry and greys its
// label to follow the Remap UIDs checkbox — the two options are mutually
// exclusive, so the suffix is editable only while remap is unchecked.
func (e *modProfileEditor) syncUIDSuffixState() {
	if e.remapCheck.Checked {
		e.uidSfx.Disable()
		e.uidSuffixLabel.Importance = widget.LowImportance
	} else {
		e.uidSfx.Enable()
		e.uidSuffixLabel.Importance = widget.MediumImportance
	}
	e.uidSuffixLabel.Refresh()
}

// validate checks every control and returns the (possibly renamed) profile to
// save. updated starts as the original, so fields without controls (dicomdir,
// verbose, top-level keepprivate) carry through untouched.
func (e *modProfileEditor) validate() (string, ModProfile, error) {
	newName := strings.TrimSpace(e.nameEntry.Text)
	if newName == "" {
		return "", ModProfile{}, fmt.Errorf("the profile name must not be empty")
	}
	if newName != e.origName {
		if _, exists := e.cfg[newName]; exists {
			return "", ModProfile{}, fmt.Errorf("a profile named %q already exists", newName)
		}
	}

	updated := e.orig
	if err := e.fields.applyValidated(&updated, e.aliases); err != nil {
		return "", ModProfile{}, err
	}
	dob, err := validateDOBMask(e.dob.Text)
	if err != nil {
		return "", ModProfile{}, err
	}
	uidSfx, err := validateUIDSuffix(e.uidSfx.Text)
	if err != nil {
		return "", ModProfile{}, err
	}
	shift, err := validateShiftDays(e.shiftDays.Text)
	if err != nil {
		return "", ModProfile{}, err
	}
	updated.DOB = dob
	updated.UIDSuffix = uidSfx
	updated.ShiftDays = shift
	updated.Priv = e.privCheck.Checked
	if sel := e.fixvr.Selected; sel == "" || sel == fixvrOffLabel {
		updated.FixVR = ""
	} else {
		updated.FixVR = sel
	}
	if sel := e.baseSelect.Selected; sel == "" || sel == modProfileNoBaseLabel {
		updated.Base = ""
	} else {
		updated.Base = sel
	}
	updated.RemapUIDs = e.remapCheck.Checked
	if updated.RemapUIDs {
		// The suffix entry is disabled while Remap UIDs is checked; any text
		// left in it is inert and must not reach the stored profile — the
		// engine rejects the combination.
		updated.UIDSuffix = ""
	}
	updated.Zip = e.zipCheck.Checked
	// The engine matches these filters against ImageType/Modality components
	// case-insensitively; casing is stored as typed. CS values cannot contain
	// commas, so the comma-joined display round-trips exactly.
	updated.IgnoreTypes = splitCommaList(e.ignoreTypesEntry.Text)
	updated.IgnoreModalities = splitCommaList(e.ignoreModsEntry.Text)
	updated.PerModality = e.perMod
	if len(updated.PerModality) == 0 {
		updated.PerModality = nil // keep omitempty round-trips byte-identical
	}

	// The base chain must still resolve with this edit applied — catches
	// cycles and references to profiles that no longer exist.
	temp := maps.Clone(e.cfg)
	if temp == nil {
		temp = ModProfileConfig{}
	}
	delete(temp, e.origName)
	temp[newName] = updated
	if _, err := resolveModProfile(newName, temp); err != nil {
		return "", ModProfile{}, err
	}
	return newName, updated, nil
}

// perModalitySummary renders the one-line list entry for a per-modality
// override, e.g. "CT  (2 set, 1 remove, dob, priv)".
func perModalitySummary(code string, p ModProfile) string {
	var parts []string
	if n := len(p.Sets); n > 0 {
		parts = append(parts, fmt.Sprintf("%d set", n))
	}
	if n := len(p.Removes); n > 0 {
		parts = append(parts, fmt.Sprintf("%d remove", n))
	}
	if n := len(p.Keep); n > 0 {
		parts = append(parts, fmt.Sprintf("%d keep", n))
	}
	if p.DOB != "" {
		parts = append(parts, "dob")
	}
	if p.UIDSuffix != "" {
		parts = append(parts, "uid")
	}
	if p.ShiftDays != "" {
		parts = append(parts, "shift "+p.ShiftDays+"d")
	}
	if p.FixVR != "" {
		parts = append(parts, "fixvr "+p.FixVR)
	}
	if p.Priv {
		parts = append(parts, "priv")
	}
	if p.KeepPrivate {
		parts = append(parts, "keeppriv")
	}
	if len(parts) == 0 {
		return code + "  (no overrides)"
	}
	return code + "  (" + strings.Join(parts, ", ") + ")"
}

// otherCodes returns the override codes in perMod except exclude — the
// collision set handed to the sub-editor.
func otherCodes(perMod map[string]ModProfile, exclude string) []string {
	out := make([]string, 0, len(perMod))
	for c := range perMod {
		if c != exclude {
			out = append(out, c)
		}
	}
	return out
}

// modProfileEditorKey identifies one editor window by the profile it edits
// ("" being the new-profile window), so a second Edit raises the window
// already open rather than starting a rival copy whose stale state would
// overwrite the first one's save.
func modProfileEditorKey(name string) string { return "modprofile:" + name }

// showModProfileEditor opens an editor window for the modification profile
// named name (empty for a new profile). cfg supplies the other profiles for
// the Base select and the rename-collision and base-cycle checks; it is not
// modified. onSave receives the (possibly renamed) profile after validation
// passes. parent owns the window — the Preferences window, which the editor
// blocks while open and which takes the editor with it when it closes: an
// editor outliving Preferences would save into a pending-profile map that is
// no longer going anywhere.
func showModProfileEditor(a fyne.App, parent fyne.Window, name string, p ModProfile, cfg ModProfileConfig,
	onSave func(newName string, updated ModProfile)) {

	if raiseOwnedWindow(modProfileEditorKey(name)) {
		return
	}

	// Tag aliases let Removes/Keep/Sets reference tags by phrase ("patient
	// name") instead of GGGG,EEEE — needed to validate those fields.
	aliases := TagConfig{}
	if tagsPath, err := modifyTagsPath(); err == nil {
		if loaded, lerr := loadTagConfig(tagsPath); lerr == nil {
			aliases = loaded
		}
	}

	// Warm the dictionary probe off the UI goroutine so the first Choose…
	// opens instantly; sync.Once makes a concurrent picker open harmless.
	go dictionaryTags()

	title := "New Modification Profile"
	if name != "" {
		title = "Edit Modification Profile — " + name
	}

	ed := newModProfileEditor(name, p, cfg, aliases)

	openOwnedWindow(a, windowSpec{
		Key:      modProfileEditorKey(name),
		Title:    title,
		Size:     fyne.NewSize(640, 760),
		Parent:   parent,
		Blocking: true,
	}, func(win fyne.Window) fyne.CanvasObject {
		return buildModProfileEditorContent(a, win, ed, p, aliases, onSave)
	})
}

// buildModProfileEditorContent lays the editor out for its own window. It runs
// inside openOwnedWindow's build callback because every child the editor opens
// — the tag picker, the per-modality sub-editor, validation errors — must be
// parented to that window rather than to whatever is behind it.
func buildModProfileEditorContent(a fyne.App, win fyne.Window, ed *modProfileEditor, p ModProfile,
	aliases TagConfig, onSave func(newName string, updated ModProfile)) fyne.CanvasObject {

	topRow := widget.NewForm(
		widget.NewFormItem("Profile name", ed.nameEntry),
		widget.NewFormItem("Base profile", ed.baseSelect))

	tagSection := prefSection("Tag rules", widget.NewForm(
		widget.NewFormItem("Set values", ed.fields.sets),
		widget.NewFormItem("Remove tags",
			tagListField(a, win, ed.fields.removes, "Choose tags to remove", aliases)),
		widget.NewFormItem("Keep tags",
			tagListField(a, win, ed.fields.keep, "Choose tags to keep", aliases))))

	// Remap UIDs and UID suffix share one row: checkbox, then the suffix
	// label + entry, which grey out while remap is checked.
	uidRow := container.NewBorder(nil, nil,
		container.NewHBox(ed.remapCheck, ed.uidSuffixLabel), nil, ed.uidSfx)
	optionsForm := widget.NewForm(
		widget.NewFormItem("Birth date mask", ed.dob),
		widget.NewFormItem("Remap UIDs", uidRow),
		widget.NewFormItem("Remove private tags", ed.privCheck),
		widget.NewFormItem("Shift dates (days)", ed.shiftDays),
		widget.NewFormItem("Fix VR", ed.fixvr),
		widget.NewFormItem("Zip export", ed.zipCheck))

	filterCaption := widget.NewLabel("Files matching either comma-separated filter are skipped entirely.")
	filterCaption.TextStyle = fyne.TextStyle{Italic: true}
	filterCaption.Wrapping = fyne.TextWrapWord
	filtersForm := widget.NewForm(
		widget.NewFormItem("Ignore image types", ed.ignoreTypesEntry),
		widget.NewFormItem("Ignore modalities", ed.ignoreModsEntry))

	optionsSection := prefSection("Options", optionsForm)
	filtersSection := prefSection("File filters", filterCaption, filtersForm)

	// Per-modality override list — the same rebuild-a-VBox pattern as the
	// profile list in Preferences.
	perModList := container.NewVBox()
	var rebuildPerModList func()
	rebuildPerModList = func() {
		codes := make([]string, 0, len(ed.perMod))
		for c := range ed.perMod {
			codes = append(codes, c)
		}
		sort.Strings(codes)
		rows := make([]fyne.CanvasObject, len(codes))
		for i, c := range codes {
			c := c
			editBtn := widget.NewButton("Edit", func() {
				showPerModalityEditor(a, win, c, ed.perMod[c], otherCodes(ed.perMod, c), aliases,
					func(newCode string, updated ModProfile) {
						if newCode != c {
							delete(ed.perMod, c)
						}
						ed.perMod[newCode] = updated
						rebuildPerModList()
					})
			})
			deleteBtn := widget.NewButton("Delete", func() {
				delete(ed.perMod, c)
				rebuildPerModList()
			})
			rows[i] = container.NewBorder(nil, nil, nil,
				container.NewHBox(editBtn, deleteBtn),
				widget.NewLabel(perModalitySummary(c, ed.perMod[c])))
		}
		perModList.Objects = rows
		perModList.Refresh()
	}
	rebuildPerModList()

	addOverrideBtn := widget.NewButton("Add modality override…", func() {
		showPerModalityEditor(a, win, "", ModProfile{}, otherCodes(ed.perMod, ""), aliases,
			func(newCode string, updated ModProfile) {
				if ed.perMod == nil {
					ed.perMod = map[string]ModProfile{}
				}
				ed.perMod[newCode] = updated
				rebuildPerModList()
			})
	})
	perModCaption := widget.NewLabel("Applied on top of the profile for files of a matching modality.")
	perModCaption.TextStyle = fyne.TextStyle{Italic: true}
	perModCaption.Wrapping = fyne.TextWrapWord
	perModSection := prefSection("Per-modality overrides", perModCaption, perModList, addOverrideBtn)

	sections := []fyne.CanvasObject{topRow, tagSection, optionsSection, filtersSection, perModSection}
	var preserved []string
	if p.KeepPrivate {
		preserved = append(preserved, "keepprivate (honored only inside per-modality overrides)")
	}
	if p.Dicomdir {
		preserved = append(preserved, "dicomdir")
	}
	if p.Verbose {
		preserved = append(preserved, "verbose")
	}
	if len(preserved) > 0 {
		note := widget.NewLabel("Also defined (preserved, not editable here): " + strings.Join(preserved, "; "))
		note.TextStyle = fyne.TextStyle{Italic: true}
		note.Wrapping = fyne.TextWrapWord
		sections = append(sections, note)
	}

	// The window stays open until the profile actually validates, so a
	// validation failure never throws away the edits — the reason this was
	// never a confirm dialog, which hides before its callback runs.
	cancelBtn := widget.NewButton("Cancel", func() { win.Close() })
	saveBtn := widget.NewButton("Save", nil)
	saveBtn.Importance = widget.HighImportance
	buttonRow := container.NewBorder(
		widget.NewSeparator(), nil, nil, nil,
		container.NewPadded(container.NewHBox(layout.NewSpacer(), cancelBtn, saveBtn)),
	)

	// A modest scroll floor rather than the full content height: it is what
	// stops the window being shrunk to nothing, so a tall value would forbid
	// the vertical resizing this window exists to allow. The width floor only
	// has to hold one column now, so it is well below what the two-column
	// layout needed.
	bodyScroll := container.NewVScroll(container.NewVBox(sections...))
	bodyScroll.SetMinSize(fyne.NewSize(0, 240))
	minWidth := canvas.NewRectangle(color.Transparent)
	minWidth.SetMinSize(fyne.NewSize(560, 0))

	saveBtn.OnTapped = func() {
		newName, updated, err := ed.validate()
		if err != nil {
			dialog.ShowError(err, win)
			return
		}
		win.Close()
		onSave(newName, updated)
	}

	return container.NewStack(minWidth,
		container.NewBorder(nil, buttonRow, nil, nil, bodyScroll))
}

// perModalityEditor holds the controls of one per-modality override session.
// An override may only vary what is genuinely modality-specific: the tag lists
// and the keep-private flag (which cancels the profile's private-tag removal
// for this modality). The scalar de-identification options are profile-wide
// and have no controls here even though the engine would honor them, as does
// anything a hand-authored block carries that the engine ignores per modality
// (base, remapuids, ignore filters, nested per-modality, dicomdir, verbose);
// every one of them carries through untouched via updated := orig.
type perModalityEditor struct {
	origCode string
	orig     ModProfile
	taken    []string // the other override codes (uppercase) — collision check
	aliases  TagConfig

	codeEntry     *widget.Entry
	fields        *modProfileFieldSet
	keepPrivCheck *widget.Check
}

func newPerModalityEditor(code string, p ModProfile, taken []string, aliases TagConfig) *perModalityEditor {
	e := &perModalityEditor{origCode: code, orig: p, taken: taken, aliases: aliases}

	e.codeEntry = widget.NewEntry()
	e.codeEntry.SetText(code)
	e.codeEntry.SetPlaceHolder("e.g. CT, US, MR")

	e.fields = newModProfileFieldSet(p, aliases)
	e.fields.keep.SetPlaceHolder("Tags to keep even when the profile removes them")

	e.keepPrivCheck = widget.NewCheck("", nil)
	e.keepPrivCheck.SetChecked(p.KeepPrivate)

	return e
}

// validate checks every control and returns the override to save under its
// (possibly renamed) uppercase modality code.
func (e *perModalityEditor) validate() (string, ModProfile, error) {
	newCode := strings.ToUpper(strings.TrimSpace(e.codeEntry.Text))
	if newCode == "" {
		return "", ModProfile{}, fmt.Errorf("the modality code must not be empty")
	}
	if strings.ContainsAny(newCode, " \t") {
		return "", ModProfile{}, fmt.Errorf("the modality code must be a single word, e.g. CT")
	}
	if slices.Contains(e.taken, newCode) {
		return "", ModProfile{}, fmt.Errorf("an override for modality %q already exists", newCode)
	}
	updated := e.orig
	if err := e.fields.applyValidated(&updated, e.aliases); err != nil {
		return "", ModProfile{}, err
	}
	updated.KeepPrivate = e.keepPrivCheck.Checked
	return newCode, updated, nil
}

// perModalityPreservedNote describes the fields of a hand-authored
// per-modality block that the sub-editor does not offer as controls, so a
// value the user cannot see is never mistaken for one that was lost. Two
// separate cases: options that are profile-wide here (the engine would still
// apply the override), and options the engine ignores per modality outright.
// Returns nil when the block carries neither.
func perModalityPreservedNote(p ModProfile) fyne.CanvasObject {
	italic := func(text string) *widget.Label {
		l := widget.NewLabel(text)
		l.TextStyle = fyne.TextStyle{Italic: true}
		l.Wrapping = fyne.TextWrapWord
		return l
	}

	// Honored by the engine per modality, but managed at the profile level.
	var profileWide []string
	if p.DOB != "" {
		profileWide = append(profileWide, "dob")
	}
	if p.UIDSuffix != "" {
		profileWide = append(profileWide, "uid")
	}
	if p.ShiftDays != "" {
		profileWide = append(profileWide, "shiftdays")
	}
	if p.FixVR != "" {
		profileWide = append(profileWide, "fixvr")
	}
	if p.Priv {
		profileWide = append(profileWide, "noprivate")
	}

	// Not read by the engine inside a per-modality block at all.
	var ignored []string
	if p.Base != "" {
		ignored = append(ignored, "base")
	}
	if p.RemapUIDs {
		ignored = append(ignored, "remapuids")
	}
	if p.Zip {
		ignored = append(ignored, "zip")
	}
	if len(p.IgnoreTypes) > 0 {
		ignored = append(ignored, "ignoretype")
	}
	if len(p.IgnoreModalities) > 0 {
		ignored = append(ignored, "ignoremodality")
	}
	if len(p.PerModality) > 0 {
		ignored = append(ignored, "nested per-modality")
	}
	if p.Dicomdir {
		ignored = append(ignored, "dicomdir")
	}
	if p.Verbose {
		ignored = append(ignored, "verbose")
	}

	var notes []fyne.CanvasObject
	if len(profileWide) > 0 {
		notes = append(notes, italic("Set on this override and still applied, but managed at the profile level "+
			"rather than per modality: "+strings.Join(profileWide, ", ")))
	}
	if len(ignored) > 0 {
		notes = append(notes, italic("Not honored inside a per-modality override, preserved unchanged: "+
			strings.Join(ignored, ", ")))
	}
	if len(notes) == 0 {
		return nil // explicit nil: a typed nil container would read as non-nil
	}
	return container.NewVBox(notes...)
}

// showPerModalityEditor opens the nested dialog editing one per-modality
// override on top of the main editor dialog. taken lists the other override
// codes for the collision check. onSave receives the validated block under
// its (possibly renamed) uppercase modality code.
func showPerModalityEditor(a fyne.App, w fyne.Window, code string, p ModProfile, taken []string,
	aliases TagConfig, onSave func(newCode string, updated ModProfile)) {

	ed := newPerModalityEditor(code, p, taken, aliases)

	form := widget.NewForm(
		widget.NewFormItem("Modality", ed.codeEntry),
		widget.NewFormItem("Set values", ed.fields.sets),
		widget.NewFormItem("Remove tags",
			tagListField(a, w, ed.fields.removes, "Choose tags to remove", aliases)),
		widget.NewFormItem("Keep tags",
			tagListField(a, w, ed.fields.keep, "Choose tags to keep", aliases)),
		widget.NewFormItem("Keep private tags", ed.keepPrivCheck),
	)

	sections := []fyne.CanvasObject{form}
	if note := perModalityPreservedNote(p); note != nil {
		sections = append(sections, note)
	}

	// Same stay-open-on-validation-failure button pattern as the main editor.
	var dlg dialog.Dialog
	cancelBtn := widget.NewButton("Cancel", func() { dlg.Hide() })
	saveBtn := widget.NewButton("Save", nil)
	saveBtn.Importance = widget.HighImportance
	buttonRow := container.NewBorder(
		widget.NewSeparator(), nil, nil, nil,
		container.NewPadded(container.NewHBox(layout.NewSpacer(), cancelBtn, saveBtn)),
	)

	formScroll := container.NewVScroll(container.NewVBox(sections...))
	formScroll.SetMinSize(fyne.NewSize(0, 340))
	minWidth := canvas.NewRectangle(color.Transparent)
	minWidth.SetMinSize(fyne.NewSize(520, 0))
	content := container.NewStack(minWidth,
		container.NewBorder(nil, buttonRow, nil, nil, formScroll))
	title := "Edit Modality Override"
	if code == "" {
		title = "New Modality Override"
	}
	dlg = dialog.NewCustomWithoutButtons(title, content, w)
	dlg.Resize(fyne.NewSize(560, 440))

	saveBtn.OnTapped = func() {
		newCode, updated, err := ed.validate()
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		dlg.Hide()
		onSave(newCode, updated)
	}

	dlg.Show()
}
