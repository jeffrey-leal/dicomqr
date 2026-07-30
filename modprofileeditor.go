package main

// Modification-profile editor — opened from Preferences > Modification &
// Export. Edits one named ModProfile from ~/.dicomqr/profiles.json. Only the
// core fields have controls; per-modality overrides, ignoretype /
// ignoremodality filters, dicomdir and verbose are preserved unchanged, so a
// hand-authored (or dicomtool-authored) profile survives a round-trip through
// the editor.

import (
	"fmt"
	"image/color"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
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

// showModProfileEditor opens an edit dialog for the modification profile named
// name (empty for a new profile). cfg supplies the other profiles for the Base
// select and the rename-collision and base-cycle checks; it is not modified.
// onSave receives the (possibly renamed) profile after validation passes.
func showModProfileEditor(w fyne.Window, name string, p ModProfile, cfg ModProfileConfig,
	onSave func(newName string, updated ModProfile)) {

	// Tag aliases let Removes/Keep/Sets reference tags by phrase ("patient
	// name") instead of GGGG,EEEE — needed to validate those fields.
	aliases := TagConfig{}
	if tagsPath, err := modifyTagsPath(); err == nil {
		if loaded, lerr := loadTagConfig(tagsPath); lerr == nil {
			aliases = loaded
		}
	}

	nameEntry := widget.NewEntry()
	nameEntry.SetText(name)

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
	baseSelect := widget.NewSelect(append([]string{modProfileNoBaseLabel}, others...), nil)
	if p.Base != "" {
		baseSelect.SetSelected(p.Base)
	} else {
		baseSelect.SetSelected(modProfileNoBaseLabel)
	}

	setsEntry := widget.NewMultiLineEntry()
	setsEntry.SetMinRowsVisible(5)
	setsEntry.SetText(strings.Join(p.Sets, "\n"))
	setsEntry.SetPlaceHolder("One TAG=VALUE per line, e.g.\npatient name=ANONYMOUS\n0010,0020=ID0000")

	removesEntry := widget.NewMultiLineEntry()
	removesEntry.SetMinRowsVisible(5)
	removesEntry.SetText(strings.Join(p.Removes, "\n"))
	removesEntry.SetPlaceHolder("One tag or alias per line, e.g.\nother patient ids\n0010,1000")

	keepEntry := widget.NewMultiLineEntry()
	keepEntry.SetMinRowsVisible(3)
	keepEntry.SetText(strings.Join(p.Keep, "\n"))
	keepEntry.SetPlaceHolder("Tags to keep even when the base profile removes them")

	dobEntry := widget.NewEntry()
	dobEntry.SetText(p.DOB)
	dobEntry.SetPlaceHolder("YYYYMMDD — empty = no masking")

	uidEntry := widget.NewEntry()
	uidEntry.SetText(p.UIDSuffix)
	uidEntry.SetPlaceHolder("digits 1-9 — empty = none")

	remapCheck := widget.NewCheck("", nil)
	remapCheck.SetChecked(p.RemapUIDs)
	privCheck := widget.NewCheck("", nil)
	privCheck.SetChecked(p.Priv)
	keepPrivCheck := widget.NewCheck("", nil)
	keepPrivCheck.SetChecked(p.KeepPrivate)

	maskEntry := widget.NewEntry()
	if p.MaskRows > 0 {
		maskEntry.SetText(strconv.Itoa(p.MaskRows))
	}
	maskEntry.SetPlaceHolder("0")

	fixvrSelect := widget.NewSelect([]string{fixvrOffLabel, "correct", "skip", "passthrough"}, nil)
	if m := strings.ToLower(strings.TrimSpace(p.FixVR)); m != "" {
		fixvrSelect.SetSelected(m)
	} else {
		fixvrSelect.SetSelected(fixvrOffLabel)
	}

	form := widget.NewForm(
		widget.NewFormItem("Profile name", nameEntry),
		widget.NewFormItem("Base profile", baseSelect),
		widget.NewFormItem("Set values", setsEntry),
		widget.NewFormItem("Remove tags", removesEntry),
		widget.NewFormItem("Keep tags", keepEntry),
		widget.NewFormItem("Birth date mask", dobEntry),
		widget.NewFormItem("UID suffix", uidEntry),
		widget.NewFormItem("Remap UIDs", remapCheck),
		widget.NewFormItem("Remove private tags", privCheck),
		widget.NewFormItem("Keep private tags", keepPrivCheck),
		widget.NewFormItem("Mask top pixel rows", maskEntry),
		widget.NewFormItem("Fix VR", fixvrSelect),
	)

	sections := []fyne.CanvasObject{form}
	var preserved []string
	if len(p.PerModality) > 0 {
		mods := make([]string, 0, len(p.PerModality))
		for k := range p.PerModality {
			mods = append(mods, k)
		}
		sort.Strings(mods)
		preserved = append(preserved, "per-modality overrides ("+strings.Join(mods, ", ")+")")
	}
	if len(p.IgnoreTypes) > 0 {
		preserved = append(preserved, "ignoretype "+strings.Join(p.IgnoreTypes, ", "))
	}
	if len(p.IgnoreModalities) > 0 {
		preserved = append(preserved, "ignoremodality "+strings.Join(p.IgnoreModalities, ", "))
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

	// Custom buttons instead of NewCustomConfirm: a confirm dialog hides before
	// its callback runs, so a validation failure would throw away every edit.
	// Here the dialog stays open until the profile actually validates.
	var dlg dialog.Dialog
	cancelBtn := widget.NewButton("Cancel", func() { dlg.Hide() })
	saveBtn := widget.NewButton("Save", nil)
	saveBtn.Importance = widget.HighImportance
	buttonRow := container.NewBorder(
		widget.NewSeparator(), nil, nil, nil,
		container.NewPadded(container.NewHBox(layout.NewSpacer(), cancelBtn, saveBtn)),
	)

	formScroll := container.NewVScroll(container.NewVBox(sections...))
	formScroll.SetMinSize(fyne.NewSize(0, 520))
	minWidth := canvas.NewRectangle(color.Transparent)
	minWidth.SetMinSize(fyne.NewSize(640, 0))
	content := container.NewStack(minWidth,
		container.NewBorder(nil, buttonRow, nil, nil, formScroll))
	title := "Edit Modification Profile"
	if name == "" {
		title = "New Modification Profile"
	}
	dlg = dialog.NewCustomWithoutButtons(title, content, w)

	saveBtn.OnTapped = func() {
		newName := strings.TrimSpace(nameEntry.Text)
		if newName == "" {
			dialog.ShowError(fmt.Errorf("the profile name must not be empty"), w)
			return
		}
		if newName != name {
			if _, exists := cfg[newName]; exists {
				dialog.ShowError(fmt.Errorf("a profile named %q already exists", newName), w)
				return
			}
		}
		checkTags := func(field, text string) ([]string, error) {
			lines := splitProfileLines(text)
			for _, line := range lines {
				if _, err := parseTagString(aliases.Resolve(line)); err != nil {
					return nil, fmt.Errorf("%s: %q is neither a GGGG,EEEE tag nor a tags.json alias", field, line)
				}
			}
			return lines, nil
		}
		removes, err := checkTags("Remove tags", removesEntry.Text)
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		keeps, err := checkTags("Keep tags", keepEntry.Text)
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		sets := splitProfileLines(setsEntry.Text)
		for _, s := range sets {
			tagStr, _, ok := strings.Cut(s, "=")
			tagStr = strings.TrimSpace(tagStr)
			if !ok || tagStr == "" {
				dialog.ShowError(fmt.Errorf("Set values: %q is not of the form TAG=VALUE", s), w)
				return
			}
			if _, err := parseTagString(aliases.Resolve(tagStr)); err != nil {
				dialog.ShowError(fmt.Errorf("Set values: tag %q is neither a GGGG,EEEE tag nor a tags.json alias", tagStr), w)
				return
			}
		}
		dob := strings.TrimSpace(dobEntry.Text)
		if dob != "" && len(dob) != 8 {
			dialog.ShowError(fmt.Errorf("the birth date mask must be exactly 8 characters (YYYYMMDD), got %d", len(dob)), w)
			return
		}
		uidSfx := strings.TrimSpace(uidEntry.Text)
		for _, c := range uidSfx {
			if c < '1' || c > '9' {
				dialog.ShowError(fmt.Errorf("the UID suffix may contain only the digits 1-9"), w)
				return
			}
		}
		if remapCheck.Checked && uidSfx != "" {
			dialog.ShowError(fmt.Errorf("Remap UIDs and a UID suffix cannot be combined — clear one of them"), w)
			return
		}
		maskRows := 0
		if s := strings.TrimSpace(maskEntry.Text); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				dialog.ShowError(fmt.Errorf("mask top pixel rows must be a whole number ≥ 0"), w)
				return
			}
			maskRows = n
		}

		updated := p // fields without controls carry through untouched
		if sel := baseSelect.Selected; sel == "" || sel == modProfileNoBaseLabel {
			updated.Base = ""
		} else {
			updated.Base = sel
		}
		updated.Sets = sets
		updated.Removes = removes
		updated.Keep = keeps
		updated.DOB = dob
		updated.UIDSuffix = uidSfx
		updated.RemapUIDs = remapCheck.Checked
		updated.Priv = privCheck.Checked
		updated.KeepPrivate = keepPrivCheck.Checked
		updated.MaskRows = maskRows
		if sel := fixvrSelect.Selected; sel == "" || sel == fixvrOffLabel {
			updated.FixVR = ""
		} else {
			updated.FixVR = sel
		}

		// The base chain must still resolve with this edit applied — catches
		// cycles and references to profiles that no longer exist.
		temp := maps.Clone(cfg)
		if temp == nil {
			temp = ModProfileConfig{}
		}
		delete(temp, name)
		temp[newName] = updated
		if _, err := resolveModProfile(newName, temp); err != nil {
			dialog.ShowError(err, w)
			return
		}

		dlg.Hide()
		onSave(newName, updated)
	}

	dlg.Show()
}
