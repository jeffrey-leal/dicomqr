package main

// Mask region editor — one row per masking rule of a modification profile.
//
// Geometry is entered as percentages of the image and stored as fractions,
// which is the only form that survives a study whose series differ in size.
// Percentages are what a user can reason about ("the top 8%"); fractions are
// what the engine resolves against each frame.
//
// The same list serves the profile editor and the per-modality sub-editor:
// where an image prints its banner is a property of the modality and the
// vendor, so regions are exactly the kind of setting an override may vary.

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// Mode labels for the row select. The stored tokens are the maskMode*
// constants; these are only ever shown.
const (
	maskRectLabel = "Rectangle"
	maskUSLabel   = "Outside ultrasound region"
)

// maskRegionRow is one rule: a mode and, for a rectangle, its four percentage
// entries. The entries persist across a mode change rather than being cleared,
// so flipping to the ultrasound rule and back does not lose typed geometry.
type maskRegionRow struct {
	mode       *widget.Select
	x, y, w, h *widget.Entry
	// scope is carried through untouched. The review window writes scoped
	// regions (this image, this group), and this editor has no controls for
	// them — dropping the scope on save would silently widen a rectangle from
	// one screen to every image in the study, which is the failure the scope
	// exists to prevent.
	scope *MaskScope
	// exempt marks a "needs no masking" record, likewise editable only where it
	// is created but never discarded here.
	exempt bool
}

// maskRegionList is the rebuilt-VBox row list, the same pattern setValueList
// and the per-modality override list use. It needs no window: unlike the tag
// lists it opens nothing, which keeps validate() drivable by tests with no
// canvas.
type maskRegionList struct {
	rows []*maskRegionRow
	box  *fyne.Container

	// The picker needs a window to parent to, supplied after construction for
	// the same reason setValueList's is: both editors build their controls
	// before the window that owns them exists, and construction has to stay
	// window-free for validate() to remain test-drivable.
	a        fyne.App
	parent   fyne.Window
	startDir string
}

// newMaskRegionList builds the list from a profile's stored regions.
func newMaskRegionList(regions []MaskRegion) *maskRegionList {
	l := &maskRegionList{box: container.NewVBox()}
	for _, r := range regions {
		l.rows = append(l.rows, newMaskRegionRow(r))
	}
	l.rebuild()
	return l
}

// attach supplies the app, the window the picker opens against, and the folder
// its file chooser starts in. Until it is called the Pick button is inert
// rather than crashing.
func (l *maskRegionList) attach(a fyne.App, parent fyne.Window, startDir string) {
	l.a, l.parent, l.startDir = a, parent, startDir
}

// pickFromImage opens the region picker on a file of the user's choosing,
// seeded with the rectangles already in the list. Applying replaces every
// rectangle row while leaving the ultrasound rules alone — those have no
// geometry to draw, so the picker never sees them.
func (l *maskRegionList) pickFromImage() {
	if l.a == nil || l.parent == nil {
		return // not attached to a window (tests)
	}
	current, err := l.regions()
	if err != nil {
		// Draw on the image anyway: a row that does not parse is exactly what
		// the picker is for, and blocking here would leave no way out of it.
		current = nil
	}
	showMaskRegionPicker(l.a, l.parent, l.startDir, current, func(picked []MaskRegion) {
		kept := make([]*maskRegionRow, 0, len(l.rows)+len(picked))
		for _, row := range l.rows {
			if row.mode.Selected == maskUSLabel {
				kept = append(kept, row)
			}
		}
		for _, r := range picked {
			kept = append(kept, newMaskRegionRow(r))
		}
		l.rows = kept
		l.rebuild()
	})
}

// newMaskRegionRow builds the controls for one stored region.
func newMaskRegionRow(r MaskRegion) *maskRegionRow {
	row := &maskRegionRow{
		mode:  widget.NewSelect([]string{maskRectLabel, maskUSLabel}, nil),
		x:     maskPercentEntry("x %"),
		y:     maskPercentEntry("y %"),
		w:     maskPercentEntry("w %"),
		h:     maskPercentEntry("h %"),
		scope: r.AppliesTo,
	}
	switch maskRegionMode(r) {
	case maskModeNone:
		row.exempt = true
		row.mode.SetSelected(maskRectLabel)
	case maskModeOutsideUS:
		row.mode.SetSelected(maskUSLabel)
	default:
		row.mode.SetSelected(maskRectLabel)
		row.x.SetText(formatPercent(r.X))
		row.y.SetText(formatPercent(r.Y))
		row.w.SetText(formatPercent(r.W))
		row.h.SetText(formatPercent(r.H))
	}
	row.mode.OnChanged = func(string) { row.syncEnabled() }
	row.syncEnabled()
	return row
}

// maskPercentEntry is one geometry field.
func maskPercentEntry(placeholder string) *widget.Entry {
	e := widget.NewEntry()
	e.SetPlaceHolder(placeholder)
	return e
}

// syncEnabled greys the geometry fields out for the ultrasound rule, which
// takes its geometry from the file rather than from these numbers — the same
// visible-exclusion approach the Remap UIDs / UID suffix pair uses.
func (r *maskRegionRow) syncEnabled() {
	rect := r.mode.Selected != maskUSLabel
	for _, e := range []*widget.Entry{r.x, r.y, r.w, r.h} {
		if rect {
			e.Enable()
		} else {
			e.Disable()
		}
	}
}

// region reads the row back into a MaskRegion, validating as it goes. The scope
// and the exemption flag ride along untouched: this editor cannot express them
// and must not be the place they are lost.
func (r *maskRegionRow) region() (MaskRegion, error) {
	if r.exempt {
		return MaskRegion{Mode: maskModeNone, AppliesTo: r.scope}, nil
	}
	if r.mode.Selected == maskUSLabel {
		return MaskRegion{Mode: maskModeOutsideUS, AppliesTo: r.scope}, nil
	}
	out := MaskRegion{Mode: maskModeRect, AppliesTo: r.scope}
	for _, f := range []struct {
		name  string
		entry *widget.Entry
		dst   *float64
	}{
		{"x", r.x, &out.X}, {"y", r.y, &out.Y},
		{"w", r.w, &out.W}, {"h", r.h, &out.H},
	} {
		v, err := parsePercent(f.entry.Text)
		if err != nil {
			return out, fmt.Errorf("%s: %w", f.name, err)
		}
		*f.dst = v
	}
	return out, validateMaskRegion(out)
}

// parsePercent reads a percentage into a fraction. An empty field is 0, which
// validation then rejects for width and height with a message about size —
// more useful than "please enter a number" when a row was simply left blank.
func parsePercent(text string) (float64, error) {
	s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "%"))
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", text)
	}
	return v / 100, nil
}

// formatPercent renders a stored fraction as a percentage, rounded enough to
// undo the float noise of the multiplication (0.29 × 100 is 28.999999999999996)
// while keeping the precision a picked region needs.
func formatPercent(v float64) string {
	p := math.Round(v*100*1e6) / 1e6
	return strconv.FormatFloat(p, 'f', -1, 64)
}

func (l *maskRegionList) rebuild() {
	l.box.RemoveAll()
	if len(l.rows) == 0 {
		hint := widget.NewLabel("No masking — exported images keep every pixel.")
		hint.TextStyle = fyne.TextStyle{Italic: true}
		l.box.Add(hint)
	}
	for i := range l.rows {
		r := l.rows[i]
		del := widget.NewButtonWithIcon("", theme.DeleteIcon(), func() {
			for j, row := range l.rows {
				if row == r {
					l.rows = append(l.rows[:j], l.rows[j+1:]...)
					break
				}
			}
			l.rebuild()
		})
		// A row the review window created carries a scope this editor has no
		// controls for, and an exemption has no geometry at all. Both are shown
		// as read-only text rather than hidden: a rule that quietly applies to
		// one image while looking like it applies to all is the confusion the
		// scope was added to remove.
		if r.exempt || r.scope != nil {
			what := "Rectangle"
			if r.exempt {
				what = "Needs no masking"
			} else if r.mode.Selected == maskUSLabel {
				what = "Outside the ultrasound region"
			} else {
				what = fmt.Sprintf("Rectangle  x %s%%  y %s%%  w %s%%  h %s%%",
					r.x.Text, r.y.Text, r.w.Text, r.h.Text)
			}
			lbl := widget.NewLabel(what + "  —  " + r.scope.describe())
			lbl.TextStyle = fyne.TextStyle{Italic: true}
			lbl.Wrapping = fyne.TextWrapWord
			l.box.Add(container.NewBorder(nil, nil, nil, del, lbl))
			continue
		}
		geometry := container.NewGridWithColumns(4, r.x, r.y, r.w, r.h)
		l.box.Add(container.NewBorder(nil, nil, r.mode, del, geometry))
	}
	l.box.Refresh()
}

// add appends an empty rectangle row — the common case, and the one the
// geometry fields are for.
func (l *maskRegionList) add() {
	l.rows = append(l.rows, newMaskRegionRow(MaskRegion{Mode: maskModeRect}))
	l.rebuild()
}

// canvasObject is the control to place in a form: the rows above the button
// that adds another.
func (l *maskRegionList) canvasObject() fyne.CanvasObject {
	add := widget.NewButton("Add region", l.add)
	pick := widget.NewButton("Pick from image…", l.pickFromImage)
	return container.NewBorder(nil, container.NewHBox(add, pick), nil, nil, l.box)
}

// regions validates every row and returns what to store.
//
// An empty list returns nil rather than an empty slice: Preferences detects
// changes with DeepEqual, and an empty non-nil slice would differ from the nil
// a profile without regions holds, rewriting profiles.json on every Apply.
func (l *maskRegionList) regions() ([]MaskRegion, error) {
	if len(l.rows) == 0 {
		return nil, nil
	}
	out := make([]MaskRegion, 0, len(l.rows))
	for i, row := range l.rows {
		r, err := row.region()
		if err != nil {
			return nil, fmt.Errorf("Mask regions: region %d: %w", i+1, err)
		}
		out = append(out, r)
	}
	return out, nil
}
