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
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
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
}

// tagProfileWire is the JSON representation of TagProfile.
// Tags are stored as "GGGG,EEEE" hex strings — the same format the user types
// in the Preferences dialog — so manually edited settings.json files are
// consistent with what the application displays and accepts.
type tagProfileWire struct {
	Name    string     `json:"name"`
	Color   color.RGBA `json:"color"`
	Tags    []string   `json:"tags"`
	Enabled bool       `json:"enabled"`
}

func (p TagProfile) MarshalJSON() ([]byte, error) {
	w := tagProfileWire{
		Name:    p.Name,
		Color:   p.Color,
		Enabled: p.Enabled,
		Tags:    make([]string, len(p.Tags)),
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
	tags, err := parseTags(strings.Join(w.Tags, "\n"))
	if err != nil {
		return err
	}
	p.Tags = tags
	return nil
}

// matchProfile returns the color of the first enabled profile containing t.
func matchProfile(t tag.Tag, hasTag bool, profiles []TagProfile) (color.Color, bool) {
	if !hasTag {
		return nil, false
	}
	for i := range profiles {
		if !profiles[i].Enabled {
			continue
		}
		for _, pt := range profiles[i].Tags {
			if pt == t {
				return profiles[i].Color, true
			}
		}
	}
	return nil, false
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

// showTagProfileEditor opens a dialog for creating or editing a TagProfile.
// onSave is called with the updated profile when the user confirms.
func showTagProfileEditor(w fyne.Window, p TagProfile, onSave func(TagProfile)) {
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

	tagEntry := widget.NewMultiLineEntry()
	tagEntry.SetMinRowsVisible(6)
	tagEntry.SetText(formatTags(p.Tags))
	tagEntry.SetPlaceHolder("One tag per line, e.g.\n0008,0020\n0010,0010")

	form := container.NewVBox(
		widget.NewForm(
			widget.NewFormItem("Name", nameEntry),
			widget.NewFormItem("Color", container.NewHBox(swatch, changeColorBtn)),
			widget.NewFormItem("Tags", tagEntry),
		),
	)

	dialog.ShowCustomConfirm("Edit Tag Profile", "Save", "Cancel", form, func(save bool) {
		if !save {
			return
		}
		tags, err := parseTags(tagEntry.Text)
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		onSave(TagProfile{
			Name:    nameEntry.Text,
			Color:   pendingColor,
			Tags:    tags,
			Enabled: p.Enabled,
		})
	}, w)
}
