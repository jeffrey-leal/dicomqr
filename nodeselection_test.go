package main

import (
	"strconv"
	"testing"

	"fyne.io/fyne/v2"
)

func newTestSelection(m *resultsModel) *nodeSelection {
	return newNodeSelection(m, func(string) {}, func() {})
}

// buildStudyWithSeries adds one study with the given series, numbered 1..N in
// the order given, so childUIDs(study) sorts back into that same order —
// addSeries sorts by series number, and an empty/equal number would collapse
// every insertion to the same sort key.
func buildStudyWithSeries(t *testing.T, patientID, studyUID string, seriesUIDs ...string) *resultsModel {
	t.Helper()
	m := newResultsModel()
	m.addStudy("Doe^John", patientID, studyUID, "20240101", "", "", "")
	for i, sr := range seriesUIDs {
		m.addSeries(studyUID, sr, "CT", strconv.Itoa(i+1), "", 0)
	}
	return m
}

// TestNodeSelection_DeselectSeriesNarrowsStudy is the reported bug: selecting
// a study fans out to every series, and deselecting one of them must not
// leave the study itself (or the other series) reading as selected.
func TestNodeSelection_DeselectSeriesNarrowsStudy(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2", "R3")
	sel := newTestSelection(m)

	sel.Toggle("S:S1")
	sel.Toggle("R:R2")

	if sel.Selected("S:S1") {
		t.Error("study still flagged selected after deselecting one of its series")
	}
	if sel.Selected("R:R2") {
		t.Error("R2 still flagged selected after being deselected")
	}
	if !sel.Selected("R:R1") || !sel.Selected("R:R3") {
		t.Error("R1 and R3 should remain selected")
	}

	files := map[string][]string{
		"R1": {"r1a.dcm"},
		"R2": {"r2a.dcm"},
		"R3": {"r3a.dcm"},
	}
	paths := sel.Paths(files)
	if len(paths) != 2 {
		t.Fatalf("expected 2 files (R1+R3), got %d: %v", len(paths), paths)
	}
	for _, p := range paths {
		if p == "r2a.dcm" {
			t.Errorf("deselected series' file %q reappeared via the stale study flag", p)
		}
	}
}

// TestNodeSelection_ReselectSeriesAfterNarrow confirms that re-selecting a
// narrowed-out series restores full coverage in Paths, even though the
// study's own convenience flag (set only when the study row itself is
// clicked) is not retroactively restored.
func TestNodeSelection_ReselectSeriesAfterNarrow(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2", "R3")
	sel := newTestSelection(m)

	sel.Toggle("S:S1")
	sel.Toggle("R:R2") // narrow
	sel.Toggle("R:R2") // re-select just this series

	if !sel.Selected("R:R1") || !sel.Selected("R:R2") || !sel.Selected("R:R3") {
		t.Fatal("expected all three series selected again")
	}

	files := map[string][]string{
		"R1": {"r1a.dcm"},
		"R2": {"r2a.dcm"},
		"R3": {"r3a.dcm"},
	}
	if paths := sel.Paths(files); len(paths) != 3 {
		t.Fatalf("expected all 3 files back, got %d: %v", len(paths), paths)
	}
}

// TestNodeSelection_PatientLevelNarrowing exercises the multi-level ancestor
// walk: deselecting a series two levels below a selected patient must clear
// both the study and the patient flags, not just the immediate parent.
func TestNodeSelection_PatientLevelNarrowing(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2")
	sel := newTestSelection(m)

	sel.Toggle("P:P1")
	sel.Toggle("R:R1")

	if sel.Selected("P:P1") {
		t.Error("patient still flagged selected after deselecting a grandchild series")
	}
	if sel.Selected("S:S1") {
		t.Error("study still flagged selected after deselecting a child series")
	}
	if sel.Selected("R:R1") {
		t.Error("R1 should be deselected")
	}
	if !sel.Selected("R:R2") {
		t.Error("R2 should remain selected")
	}
}

// TestNodeSelection_NarrowingDoesNotLeakAcrossSiblings ensures the ancestor
// repair triggered by deselecting a node under one patient never touches an
// unrelated, independently-selected patient's subtree.
func TestNodeSelection_NarrowingDoesNotLeakAcrossSiblings(t *testing.T) {
	m := newResultsModel()
	m.addStudy("Doe^John", "P1", "S1", "20240101", "", "", "")
	m.addSeries("S1", "R1", "CT", "", "", 0)
	m.addSeries("S1", "R2", "CT", "", "", 0)
	m.addStudy("Roe^Jane", "P2", "S2", "20240101", "", "", "")
	m.addSeries("S2", "R3", "CT", "", "", 0)
	m.addSeries("S2", "R4", "CT", "", "", 0)

	sel := newTestSelection(m)
	sel.SelectAll(m.childUIDs(""))
	sel.Toggle("R:R1")

	if sel.Selected("P:P1") || sel.Selected("S:S1") {
		t.Error("patient 1 / study 1 should be narrowed after deselecting R1")
	}
	if !sel.Selected("R:R2") {
		t.Error("R2 should remain selected")
	}
	if !sel.Selected("P:P2") || !sel.Selected("S:S2") || !sel.Selected("R:R3") || !sel.Selected("R:R4") {
		t.Error("patient 2's untouched subtree should remain fully selected")
	}
}

// expectSelected fails for every id in want that is not selected and every id
// in unwanted that is.
func expectSelected(t *testing.T, sel *nodeSelection, want, unwanted []string) {
	t.Helper()
	for _, id := range want {
		if !sel.Selected(id) {
			t.Errorf("expected %s selected", id)
		}
	}
	for _, id := range unwanted {
		if sel.Selected(id) {
			t.Errorf("%s should not be selected", id)
		}
	}
}

// TestNodeSelection_PlainClickReplaces is the core of the convention: a click
// without Ctrl or Shift deselects everything else, so multiple selection
// always takes a modifier.
func TestNodeSelection_PlainClickReplaces(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2", "R3")
	sel := newTestSelection(m)

	sel.Click("R:R1", 0)
	sel.Click("R:R2", 0)
	expectSelected(t, sel, []string{"R:R2"}, []string{"R:R1", "R:R3", "S:S1"})

	// A branch still fans out to its children...
	sel.Click("S:S1", 0)
	expectSelected(t, sel, []string{"S:S1", "R:R1", "R:R2", "R:R3"}, nil)

	// ...and a plain click on one of them narrows to just that child rather
	// than toggling it out of the study's selection.
	sel.Click("R:R3", 0)
	expectSelected(t, sel, []string{"R:R3"}, []string{"S:S1", "R:R1", "R:R2"})
}

// TestNodeSelection_PlainClickOnSoleSelectionDeselects: re-clicking the row that
// is the whole selection deselects it — field-reported that a lone selected
// study could otherwise only be dropped with Clear Selection. A branch counts
// as the whole selection together with the children its selection fanned out
// to, and a series inside it remains a way to narrow rather than deselect.
func TestNodeSelection_PlainClickOnSoleSelectionDeselects(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2")
	sel := newTestSelection(m)

	sel.Click("R:R1", 0)
	sel.Click("R:R1", 0)
	expectSelected(t, sel, nil, []string{"R:R1", "R:R2"})

	// A study selected alone (with its fanned-out series) deselects the same way.
	sel.Click("S:S1", 0)
	expectSelected(t, sel, []string{"S:S1", "R:R1", "R:R2"}, nil)
	sel.Click("S:S1", 0)
	expectSelected(t, sel, nil, []string{"S:S1", "R:R1", "R:R2"})

	// Clicking one of the study's series narrows to it instead: the study is
	// selected too, so the series is not the whole selection.
	sel.Click("S:S1", 0)
	sel.Click("R:R2", 0)
	expectSelected(t, sel, []string{"R:R2"}, []string{"S:S1", "R:R1"})
}

// TestNodeSelection_PlainClickAmongSeveralNarrows: with more than one row
// selected, a plain click on one of them narrows the selection to that row —
// the Windows convention — rather than deselecting it.
func TestNodeSelection_PlainClickAmongSeveralNarrows(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2", "R3")
	sel := newTestSelection(m)

	sel.Click("R:R1", 0)
	sel.Click("R:R3", fyne.KeyModifierShortcutDefault)
	sel.Click("R:R3", 0)
	expectSelected(t, sel, []string{"R:R3"}, []string{"R:R1", "R:R2"})

	// ...and the row now alone deselects on the next plain click, leaving it
	// the anchor for a Shift+click range.
	sel.Click("R:R3", 0)
	expectSelected(t, sel, nil, []string{"R:R1", "R:R2", "R:R3"})
	sel.Click("R:R1", fyne.KeyModifierShift)
	expectSelected(t, sel, []string{"R:R1", "R:R2", "R:R3"}, nil)
}

// TestNodeSelection_PlainClickRefreshesDeselectedRows: rows dropped by a
// replacing click can be anywhere in the tree, so the whole tree must be
// repainted — but only when there was something to drop.
func TestNodeSelection_PlainClickRefreshesDeselectedRows(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2")
	fullRefreshes := 0
	sel := newNodeSelection(m, func(string) {}, func() { fullRefreshes++ })

	sel.Click("R:R1", 0)
	if fullRefreshes != 0 {
		t.Errorf("first click on an empty selection refreshed the whole tree %d time(s)", fullRefreshes)
	}
	sel.Click("R:R2", 0)
	if fullRefreshes != 1 {
		t.Errorf("replacing click: expected 1 full refresh, got %d", fullRefreshes)
	}
}

// TestNodeSelection_CtrlClickAccumulates: Ctrl+click adds and removes single
// items without disturbing the rest of the selection.
func TestNodeSelection_CtrlClickAccumulates(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2", "R3")
	sel := newTestSelection(m)
	ctrl := fyne.KeyModifierShortcutDefault

	sel.Click("R:R1", 0)
	sel.Click("R:R3", ctrl)
	expectSelected(t, sel, []string{"R:R1", "R:R3"}, []string{"R:R2"})

	sel.Click("R:R1", ctrl)
	expectSelected(t, sel, []string{"R:R3"}, []string{"R:R1", "R:R2"})
}

// TestNodeSelection_ShiftRangeWithinStudy covers the shift-click range:
// extending from one series to another selects everything between them, in
// either click order.
func TestNodeSelection_ShiftRangeWithinStudy(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2", "R3", "R4", "R5")

	forward := newTestSelection(m)
	forward.Click("R:R2", 0)
	forward.Click("R:R4", fyne.KeyModifierShift)
	expectSelected(t, forward, []string{"R:R2", "R:R3", "R:R4"}, []string{"R:R1", "R:R5"})

	backward := newTestSelection(m)
	backward.Click("R:R4", 0)
	backward.Click("R:R2", fyne.KeyModifierShift)
	expectSelected(t, backward, []string{"R:R2", "R:R3", "R:R4"}, []string{"R:R1", "R:R5"})
}

// TestNodeSelection_ShiftRangeReplaces: a Shift+click range replaces the
// selection, dropping anything selected outside it.
func TestNodeSelection_ShiftRangeReplaces(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2", "R3", "R4", "R5")
	sel := newTestSelection(m)
	ctrl := fyne.KeyModifierShortcutDefault

	sel.Click("R:R5", 0)
	sel.Click("R:R1", ctrl)
	sel.Click("R:R3", fyne.KeyModifierShift)
	expectSelected(t, sel, []string{"R:R1", "R:R2", "R:R3"}, []string{"R:R4", "R:R5"})
}

// TestNodeSelection_ShiftKeepsAnchor: consecutive shift-clicks re-draw the
// range from the same anchor, so a second shift-click in the other direction
// swaps which side of the anchor is selected.
func TestNodeSelection_ShiftKeepsAnchor(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2", "R3", "R4", "R5")
	sel := newTestSelection(m)

	sel.Click("R:R3", 0)
	sel.Click("R:R5", fyne.KeyModifierShift)
	sel.Click("R:R1", fyne.KeyModifierShift)
	expectSelected(t, sel, []string{"R:R1", "R:R2", "R:R3"}, []string{"R:R4", "R:R5"})
}

// TestNodeSelection_CtrlShiftRangeIsAdditive: Ctrl+Shift+click adds the range
// and never deselects anything outside it.
func TestNodeSelection_CtrlShiftRangeIsAdditive(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2", "R3", "R4", "R5")
	sel := newTestSelection(m)
	ctrl := fyne.KeyModifierShortcutDefault

	sel.Click("R:R5", 0)
	sel.Click("R:R1", ctrl)
	sel.Click("R:R3", ctrl|fyne.KeyModifierShift)
	expectSelected(t, sel, []string{"R:R1", "R:R2", "R:R3", "R:R5"}, []string{"R:R4"})
}

// TestNodeSelection_ShiftWithoutAnchorSelectsOne covers a shift-click with
// nothing clicked yet: it behaves like the same click without Shift.
func TestNodeSelection_ShiftWithoutAnchorSelectsOne(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2")

	sel := newTestSelection(m)
	sel.Click("R:R1", fyne.KeyModifierShift)
	expectSelected(t, sel, []string{"R:R1"}, []string{"R:R2"})

	additive := newTestSelection(m)
	additive.Click("R:R1", fyne.KeyModifierShortcutDefault|fyne.KeyModifierShift)
	expectSelected(t, additive, []string{"R:R1"}, []string{"R:R2"})
}

// TestNodeSelection_ShiftAcrossStudiesSelectsOne ensures a shift-click
// spanning two studies never guesses at a cross-study range: it falls back to
// a single-item click — replacing under Shift, toggling under Ctrl+Shift.
func TestNodeSelection_ShiftAcrossStudiesSelectsOne(t *testing.T) {
	m := newResultsModel()
	m.addStudy("Doe^John", "P1", "S1", "20240101", "", "", "")
	m.addSeries("S1", "R1", "CT", "", "", 0)
	m.addSeries("S1", "R2", "CT", "", "", 0)
	m.addStudy("Doe^John", "P1", "S2", "20240201", "", "", "")
	m.addSeries("S2", "R3", "CT", "", "", 0)
	m.addSeries("S2", "R4", "CT", "", "", 0)

	sel := newTestSelection(m)
	sel.Click("R:R1", 0)
	sel.Click("R:R3", fyne.KeyModifierShift)
	expectSelected(t, sel, []string{"R:R3"}, []string{"R:R1", "R:R2", "R:R4"})

	additive := newTestSelection(m)
	additive.Click("R:R1", 0)
	additive.Click("R:R3", fyne.KeyModifierShortcutDefault|fyne.KeyModifierShift)
	expectSelected(t, additive, []string{"R:R1", "R:R3"}, []string{"R:R2", "R:R4"})
}

// TestNodeSelection_OneRefreshPerOperation pins the fix for a quadratic freeze:
// Fyne's RefreshItem runs the tree's whole Layout, so repainting row by row cost
// a full tree walk per selected node. Every operation must now repaint once —
// the single row when only one changed, otherwise one full refresh — however
// many rows its fan-out touched.
func TestNodeSelection_OneRefreshPerOperation(t *testing.T) {
	series := make([]string, 50)
	for i := range series {
		series[i] = "R" + strconv.Itoa(i)
	}
	m := buildStudyWithSeries(t, "P1", "S1", series...)
	var items, full int
	sel := newNodeSelection(m, func(string) { items++ }, func() { full++ })
	expect := func(what string, wantItems, wantFull int) {
		t.Helper()
		if items != wantItems || full != wantFull {
			t.Errorf("%s: %d row refresh(es) + %d full, want %d + %d", what, items, full, wantItems, wantFull)
		}
		items, full = 0, 0
	}

	sel.SelectAll(m.activeRoots())
	expect("Select All over a patient with 50 series", 0, 1)

	sel.Toggle("R:R7")
	expect("Ctrl-deselecting one series (also clears its study and patient)", 0, 1)

	sel.Toggle("R:R7")
	expect("Ctrl-reselecting that one series", 1, 0)

	sel.Clear()
	expect("Clear", 0, 1)

	sel.Click("R:R3", 0)
	expect("plain click on an empty selection", 1, 0)

	sel.Click("R:R9", fyne.KeyModifierShift)
	expect("shift-click range over seven series", 0, 1)

	sel.SelectAll(m.activeRoots())
	sel.SelectAll(m.activeRoots())
	expect("Select All twice (the second changes nothing)", 0, 1)
}
