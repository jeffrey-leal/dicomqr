package main

// Tag Highlights and Tag Profiles for the tag viewer (View Tags window),
// ported from the dicomhdr application (profiles.go). A TagProfile associates
// a display colour with a named set of DICOM tags; the tag tree renders a
// matching tag's row in the first enabled profile's colour (the malformed-VR
// highlight always wins). The JSON wire format is identical to dicomhdr's, so
// profile blocks can be copied between the two applications' settings files.

import (
	"encoding/json"
	"fmt"
	"image/color"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// TagProfile associates a display color with a named set of DICOM tags.
type TagProfile struct {
	Name    string     `json:"name"`
	Color   color.RGBA `json:"color"`
	Tags    []tag.Tag  `json:"tags"`
	Enabled bool       `json:"enabled"`
	// Modalities limits the profile to files whose Modality (0008,0060) is one
	// of these codes — "PT", not "PET" — so a modality's profile colours its
	// own files only. Empty applies it to every file, which is what a profile
	// such as PHI means. Without it, matching was by tag alone: a tag two
	// modalities share took the earlier profile's colour in both, so a CT
	// file's Convolution Kernel and Table Height showed in NM's colour.
	// dicomqr-only: dicomhdr ignores the field, so a block copied there applies
	// to every file.
	Modalities []string `json:"modalities,omitempty"`
}

// tagProfileWire is the JSON representation of TagProfile.
// Tags are stored as "GGGG,EEEE" hex strings — the same format the user types
// in the Preferences dialog — so manually edited settings.json files are
// consistent with what the application displays and accepts.
type tagProfileWire struct {
	Name       string     `json:"name"`
	Color      color.RGBA `json:"color"`
	Tags       []string   `json:"tags"`
	Enabled    bool       `json:"enabled"`
	Modalities []string   `json:"modalities,omitempty"`
}

// appliesTo reports whether the profile colours a file of the given Modality.
func (p TagProfile) appliesTo(modality string) bool {
	if len(p.Modalities) == 0 {
		return true
	}
	modality = strings.ToUpper(strings.TrimSpace(modality))
	for _, m := range p.Modalities {
		if m == modality {
			return true
		}
	}
	return false
}

// normalizeModalities upper-cases and trims modality codes, dropping empty and
// repeated ones; nil when none remain, so an unrestricted profile marshals
// without the key.
func normalizeModalities(in []string) []string {
	var out []string
	for _, m := range in {
		m = strings.ToUpper(strings.TrimSpace(m))
		if m != "" && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// parseModalityList reads the editor's Modalities field: codes separated by
// commas or spaces. A code is what Modality (0008,0060) holds — a DICOM Code
// String of capitals, digits and underscores, at most 16 long — so "pet" is
// accepted as typed but a stray word is reported rather than stored as a code
// no file can carry.
func parseModalityList(text string) ([]string, error) {
	fields := strings.FieldsFunc(text, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	for _, f := range fields {
		u := strings.ToUpper(f)
		if len(u) > 16 || strings.IndexFunc(u, func(r rune) bool {
			return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_')
		}) >= 0 {
			return nil, fmt.Errorf("%q is not a modality code (letters, digits and _ only, as in CT, MR, NM, PT, US)", f)
		}
	}
	return normalizeModalities(fields), nil
}

func (p TagProfile) MarshalJSON() ([]byte, error) {
	w := tagProfileWire{
		Name:       p.Name,
		Color:      p.Color,
		Enabled:    p.Enabled,
		Tags:       make([]string, len(p.Tags)),
		Modalities: normalizeModalities(p.Modalities),
	}
	for i, t := range p.Tags {
		w.Tags[i] = fmt.Sprintf("%04X,%04X", t.Group, t.Element)
	}
	return json.Marshal(w)
}

func (p *TagProfile) UnmarshalJSON(b []byte) error {
	var w tagProfileWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	p.Name = w.Name
	p.Color = w.Color
	p.Enabled = w.Enabled
	p.Modalities = normalizeModalities(w.Modalities)
	tags, err := parseTags(strings.Join(w.Tags, "\n"))
	if err != nil {
		return err
	}
	p.Tags = tags
	return nil
}

// matches reports whether p colours tag t in a file of the given modality:
// enabled, applicable to that modality (see TagProfile.Modalities), and
// listing the tag.
func (p TagProfile) matches(t tag.Tag, modality string) bool {
	if !p.Enabled || !p.appliesTo(modality) {
		return false
	}
	for _, pt := range p.Tags {
		if pt == t {
			return true
		}
	}
	return false
}

// matchProfile returns the color of the first profile that matches t in a
// file of the given modality.
func matchProfile(t tag.Tag, hasTag bool, modality string, profiles []TagProfile) (color.Color, bool) {
	if !hasTag {
		return nil, false
	}
	for i := range profiles {
		if profiles[i].matches(t, modality) {
			return profiles[i].Color, true
		}
	}
	return nil, false
}

// matchingProfileNames names every profile that matches t in a file of the
// given modality, in profile order — so the first is the one whose colour
// matchProfile gives the row, and the rest are the ones it overrides.
func matchingProfileNames(t tag.Tag, hasTag bool, modality string, profiles []TagProfile) []string {
	if !hasTag {
		return nil
	}
	var names []string
	for i := range profiles {
		if profiles[i].matches(t, modality) {
			names = append(names, profiles[i].Name)
		}
	}
	return names
}

// malformedTagColor returns the configured colour for tags whose VR violates
// the standard, falling back to dicomhdr's default red when unset.
func malformedTagColor(cfg *Settings) color.Color {
	if cfg.MalformedColor != "" {
		return hexToColor(cfg.MalformedColor)
	}
	return color.RGBA{R: 0xE5, G: 0x45, B: 0x45, A: 0xFF}
}

// parseTags parses newline-separated "GGGG,EEEE" hex tag specs.
// Surrounding parentheses and whitespace are stripped from each line.
func parseTags(input string) ([]tag.Tag, error) {
	var tags []tag.Tag
	for _, line := range strings.Split(input, "\n") {
		line = strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "()"))
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ",", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid tag %q: expected GGGG,EEEE", line)
		}
		g, err1 := strconv.ParseUint(strings.TrimSpace(parts[0]), 16, 16)
		e, err2 := strconv.ParseUint(strings.TrimSpace(parts[1]), 16, 16)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("invalid hex value in tag %q", line)
		}
		tags = append(tags, tag.Tag{Group: uint16(g), Element: uint16(e)})
	}
	return tags, nil
}

func formatTags(tags []tag.Tag) string {
	lines := make([]string, len(tags))
	for i, t := range tags {
		lines[i] = fmt.Sprintf("%04X,%04X", t.Group, t.Element)
	}
	return strings.Join(lines, "\n")
}

// showTagProfileEditor opens a window for creating or editing a TagProfile,
// owned by and blocking parent (the Preferences window): Save writes into
// Preferences' pending list, which must not change underneath, and closing
// Preferences takes this window with it. Unkeyed — blocking already makes a
// second one unreachable. onSave is called with the updated profile once it
// validates; until then the window stays open, so a bad tag line never throws
// away what was typed (the confirm dialog this replaced hid first).
func showTagProfileEditor(a fyne.App, parent fyne.Window, p TagProfile, onSave func(TagProfile)) {
	var w fyne.Window // this editor's own window, assigned as it opens

	nameEntry := widget.NewEntry()
	nameEntry.SetText(p.Name)

	pendingColor := p.Color
	swatch := canvas.NewRectangle(pendingColor)
	swatch.SetMinSize(fyne.NewSize(48, theme.TextSize()+8))
	changeColorBtn := widget.NewButton("Change…", func() {
		picker := dialog.NewColorPicker("Profile colour",
			"Colour applied to the profile's tags in the tag viewer", func(c color.Color) {
				if c == nil {
					return
				}
				// Normalize to color.RGBA — the profile stores that concrete
				// type, and the picker may return color.NRGBA or a
				// Fyne-internal type.
				r, g, b, a := c.RGBA()
				pendingColor = color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
				swatch.FillColor = pendingColor
				swatch.Refresh()
			}, w)
		picker.Advanced = true
		picker.Show()
	})

	modalitiesEntry := widget.NewEntry()
	modalitiesEntry.SetText(strings.Join(p.Modalities, ", "))
	modalitiesEntry.SetPlaceHolder("Empty = every file; e.g. CT, or NM, PT")

	tagEntry := widget.NewMultiLineEntry()
	tagEntry.SetMinRowsVisible(6)
	tagEntry.SetText(formatTags(p.Tags))
	tagEntry.SetPlaceHolder("One tag per line, e.g.\n0008,0020\n0010,0010")

	form := widget.NewForm(
		widget.NewFormItem("Name", nameEntry),
		widget.NewFormItem("Color", container.NewHBox(swatch, changeColorBtn)),
		widget.NewFormItem("Modalities", modalitiesEntry),
		widget.NewFormItem("Tags", tagEntry),
	)

	cancelBtn := widget.NewButton("Cancel", func() { w.Close() })
	saveBtn := widget.NewButton("Save", func() {
		modalities, err := parseModalityList(modalitiesEntry.Text)
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		tags, err := parseTags(tagEntry.Text)
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		// Built from p, so a field this window does not edit survives.
		updated := p
		updated.Name = nameEntry.Text
		updated.Color = pendingColor
		updated.Modalities = modalities
		updated.Tags = tags
		w.Close()
		onSave(updated)
	})
	saveBtn.Importance = widget.HighImportance
	buttonRow := container.NewBorder(
		widget.NewSeparator(), nil, nil, nil,
		container.New(
			layout.NewCustomPaddedLayout(modEditorMargin, modEditorMargin, modEditorMargin, modEditorMargin),
			container.NewHBox(layout.NewSpacer(), cancelBtn, saveBtn)),
	)
	// The Tags entry scrolls on its own, so the body keeps the scroll gutter
	// on its right (scrollgutter.go).
	body := container.NewVScroll(container.New(
		layout.NewCustomPaddedLayout(modEditorMargin, modEditorMargin, modEditorMargin,
			max(modEditorMargin, scrollGutterWidth())),
		form))
	body.SetMinSize(fyne.NewSize(0, 200))
	minWidth := canvas.NewRectangle(color.Transparent)
	minWidth.SetMinSize(fyne.NewSize(460, 0))

	title := "Edit Tag Profile"
	if p.Name != "" {
		title += " — " + p.Name
	}
	openOwnedWindow(a, windowSpec{
		Title:    title,
		Size:     fyne.NewSize(520, 440),
		Parent:   parent,
		Blocking: true,
	}, func(win fyne.Window) fyne.CanvasObject {
		w = win
		return container.NewStack(minWidth, container.NewBorder(nil, buttonRow, nil, nil, body))
	})
}
