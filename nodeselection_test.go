package main

import (
	"strconv"
	"testing"
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

// TestNodeSelection_ExtendToRangeWithinStudy covers the shift-click
// accelerator: extending from one series to another selects everything
// between them, in either click order.
func TestNodeSelection_ExtendToRangeWithinStudy(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2", "R3", "R4", "R5")

	forward := newTestSelection(m)
	forward.Toggle("R:R2")
	forward.ExtendTo("R:R4")
	for _, want := range []string{"R:R2", "R:R3", "R:R4"} {
		if !forward.Selected(want) {
			t.Errorf("forward range: expected %s selected", want)
		}
	}
	for _, unwanted := range []string{"R:R1", "R:R5"} {
		if forward.Selected(unwanted) {
			t.Errorf("forward range: %s should not be selected", unwanted)
		}
	}

	backward := newTestSelection(m)
	backward.Toggle("R:R4")
	backward.ExtendTo("R:R2")
	for _, want := range []string{"R:R2", "R:R3", "R:R4"} {
		if !backward.Selected(want) {
			t.Errorf("backward range: expected %s selected", want)
		}
	}
}

// TestNodeSelection_ExtendToIsAdditive confirms a shift-click range never
// deselects anything outside the range.
func TestNodeSelection_ExtendToIsAdditive(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2", "R3", "R4", "R5")
	sel := newTestSelection(m)

	sel.Toggle("R:R5")
	sel.Toggle("R:R1")
	sel.ExtendTo("R:R3")

	for _, want := range []string{"R:R1", "R:R2", "R:R3", "R:R5"} {
		if !sel.Selected(want) {
			t.Errorf("expected %s selected", want)
		}
	}
	if sel.Selected("R:R4") {
		t.Error("R4 was never in range and should not be selected")
	}
}

// TestNodeSelection_ExtendToWithoutAnchorFallsBackToToggle covers a
// shift-click with nothing clicked yet: it should behave like a plain click.
func TestNodeSelection_ExtendToWithoutAnchorFallsBackToToggle(t *testing.T) {
	m := buildStudyWithSeries(t, "P1", "S1", "R1", "R2")
	sel := newTestSelection(m)

	sel.ExtendTo("R:R1")
	if !sel.Selected("R:R1") {
		t.Error("expected R1 selected via toggle fallback")
	}
	if sel.Selected("R:R2") {
		t.Error("R2 should be untouched")
	}
}

// TestNodeSelection_ExtendToAcrossStudiesFallsBackToToggle ensures a
// shift-click spanning two studies never guesses at a cross-study range.
func TestNodeSelection_ExtendToAcrossStudiesFallsBackToToggle(t *testing.T) {
	m := newResultsModel()
	m.addStudy("Doe^John", "P1", "S1", "20240101", "", "", "")
	m.addSeries("S1", "R1", "CT", "", "", 0)
	m.addSeries("S1", "R2", "CT", "", "", 0)
	m.addStudy("Doe^John", "P1", "S2", "20240201", "", "", "")
	m.addSeries("S2", "R3", "CT", "", "", 0)
	m.addSeries("S2", "R4", "CT", "", "", 0)

	sel := newTestSelection(m)
	sel.Toggle("R:R1")
	sel.ExtendTo("R:R3")

	if !sel.Selected("R:R1") {
		t.Error("R1 should remain selected")
	}
	if !sel.Selected("R:R3") {
		t.Error("R3 should be selected via toggle fallback")
	}
	if sel.Selected("R:R2") || sel.Selected("R:R4") {
		t.Error("no node outside the two clicked series should be touched")
	}
}
