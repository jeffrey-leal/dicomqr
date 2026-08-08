package main

// Set values editor — one row per "TAG=VALUE" entry of a modification profile.
//
// This replaced a free-text box of TAG=VALUE lines. The tag half is now chosen
// from the dictionary through the tag picker rather than typed, so it cannot be
// misspelled, and the value is checked against the tag's value representation
// (see validateSetValue) rather than accepted and written into every exported
// file. A profile's set values are what replace the patient's identity, so a
// typo here is not cosmetic.

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// setValueRow is one TAG=VALUE entry. A row whose reference does not parse
// keeps its original text and is reported on save rather than dropped — a
// silently discarded line from a de-identification profile is the failure this
// code must not have, and a hand-edited profiles.json can produce one.
type setValueRow struct {
	ref   string // canonical GGGG,EEEE, or the original text when !parsed
	t     tag.Tag
	ok    bool
	value *widget.Entry
}

// setValueList is the rebuilt-VBox row list, the same pattern the per-modality
// override list and the Preferences profile list use.
//
// The owning window is attached separately, not passed to the constructor: both
// editors build their controls before openOwnedWindow creates the window the
// picker must be parented to, and keeping construction window-free is what lets
// validate() — the whole save path — be driven by tests with no canvas.
type setValueList struct {
	a      fyne.App
	parent fyne.Window
	rows   []*setValueRow
	box    *fyne.Container
}

// newSetValueList builds the list from a profile's stored Sets, in the order
// the profile stores them. The rows were briefly sorted by group then element;
// reverted, because the stored order is the author's presentation choice — the
// shipped base-deident leads with Patient Name and the Patient ID that follows
// it, which sorting filed behind a blank Accession Number — and it is the
// order the Modification dialog already presents. Sorting also meant Apply
// rewrote a hand-ordered profile even when nothing was edited.
func newSetValueList(sets []string) *setValueList {
	l := &setValueList{box: container.NewVBox()}
	for _, s := range sets {
		l.rows = append(l.rows, newSetValueRow(s))
	}
	l.rebuild()
	return l
}

// attach supplies the app and the window the tag picker is opened against.
// Until it is called the Choose tags… button is inert rather than crashing.
func (l *setValueList) attach(a fyne.App, parent fyne.Window) {
	l.a, l.parent = a, parent
}

// newSetValueRow parses one stored entry. Everything after the first "=" is the
// value, so a value containing "=" survives intact.
func newSetValueRow(entry string) *setValueRow {
	tagStr, value, hasEq := strings.Cut(entry, "=")
	r := &setValueRow{value: widget.NewEntry()}
	r.value.SetText(value)
	r.value.SetPlaceHolder("empty = blank the element · [0010,0010] = copy another set value")
	if !hasEq {
		r.ref = strings.TrimSpace(entry)
		return r
	}
	t, err := parseTagString(strings.TrimSpace(tagStr))
	if err != nil {
		r.ref = strings.TrimSpace(tagStr)
		return r
	}
	r.t, r.ok, r.ref = t, true, formatTagRef(t)
	return r
}

// label renders the row's tag the way the picker and the Remove/Keep lists do.
func (r *setValueRow) label() string {
	if !r.ok {
		return r.ref
	}
	if name := tagDisplayName(r.t); name != "" {
		return r.ref + tagLineGap + name
	}
	return r.ref
}

// entry renders the row back into its stored form.
func (r *setValueRow) entry() string { return r.ref + "=" + r.value.Text }

func (l *setValueList) rebuild() {
	l.box.RemoveAll()
	if len(l.rows) == 0 {
		hint := widget.NewLabel("No set values — Choose tags… adds one per tag.")
		hint.TextStyle = fyne.TextStyle{Italic: true}
		l.box.Add(hint)
	}
	for i := range l.rows {
		r := l.rows[i]
		name := widget.NewLabel(r.label())
		if !r.ok {
			// An unresolvable reference is kept and flagged, never silently
			// dropped; save reports it.
			name.TextStyle = fyne.TextStyle{Italic: true}
		}
		del := widget.NewButtonWithIcon("", theme.DeleteIcon(), func() {
			for j, row := range l.rows {
				if row == r {
					l.rows = append(l.rows[:j], l.rows[j+1:]...)
					break
				}
			}
			l.rebuild()
		})
		l.box.Add(container.NewBorder(nil, nil, name, del, r.value))
	}
	l.box.Refresh()
}

// chooseTags opens the tag picker with the current tags checked, so it both
// adds rows and — by unchecking — removes them. A tag already present keeps its
// value; a newly checked one starts empty. Row order follows the refs the
// picker hands back — mergeTagSelection keeps surviving entries in their
// stored positions and appends additions in dictionary order.
func (l *setValueList) chooseTags() {
	if l.a == nil || l.parent == nil {
		return // not attached to a window (tests)
	}
	current := make([]string, 0, len(l.rows))
	for _, r := range l.rows {
		current = append(current, r.ref)
	}
	showTagPicker(l.a, l.parent, "Choose tags to set", strings.Join(current, "\n"),
		func(refs []string) {
			existing := make(map[string]*setValueRow, len(l.rows))
			for _, r := range l.rows {
				if _, dup := existing[r.ref]; !dup {
					existing[r.ref] = r
				}
			}
			rows := make([]*setValueRow, 0, len(refs))
			for _, ref := range refs {
				canonical, _ := canonicalTagRef(ref)
				if r, ok := existing[canonical]; ok {
					rows = append(rows, r) // keep the value already typed
					continue
				}
				rows = append(rows, newSetValueRow(canonical+"="))
			}
			l.rows = rows
			l.rebuild()
		})
}

// canvasObject is the control to place in a form: the rows above the button
// that edits which tags are in them.
func (l *setValueList) canvasObject() fyne.CanvasObject {
	btn := widget.NewButton("Choose tags…", l.chooseTags)
	return container.NewBorder(nil, container.NewHBox(btn), nil, nil, l.box)
}

// entries validates every row and returns the entries to store. The tag
// references are canonical by construction; the values are checked against each
// tag's VR, which is the check the engine never had.
func (l *setValueList) entries() ([]string, error) {
	var out []string
	for _, r := range l.rows {
		if !r.ok {
			return nil, fmt.Errorf("Set values: %q is not a GGGG,EEEE tag", r.ref)
		}
		// A value bracketed like a reference but naming no tag is a typo, not a
		// literal: storing "[patient name]" as the value would write it into
		// every exported file.
		if v := strings.TrimSpace(r.value.Text); strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
			if _, ok := setValueReference(v); !ok {
				return nil, fmt.Errorf("Set values: %q is not a tag reference — use [GGGG,EEEE], e.g. [0010,0010]", v)
			}
		}
		if err := validateSetValue(r.t, r.value.Text); err != nil {
			return nil, fmt.Errorf("Set values: %w", err)
		}
		out = append(out, r.entry())
	}
	return out, nil
}
