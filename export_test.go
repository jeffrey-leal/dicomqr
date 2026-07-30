package main

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// exportTestElement builds an element with an explicit VR — NewElement leaves
// RawValueRepresentation to the dictionary, but the export reads the raw VR
// exactly as the tag viewer stores it from parsed files.
func exportTestElement(t *testing.T, tg tag.Tag, vr string, data interface{}) *sdicom.Element {
	t.Helper()
	el, err := sdicom.NewElement(tg, data)
	if err != nil {
		t.Fatalf("NewElement(%v): %v", tg, err)
	}
	el.RawValueRepresentation = vr
	return el
}

// exportTestModel builds a one-instance tag tree: nine top-level elements
// including one sequence with a single item holding one nested element.
func exportTestModel(t *testing.T) *tagTreeModel {
	t.Helper()
	inner := exportTestElement(t, tag.Tag{Group: 0x0008, Element: 0x0100}, "SH", []string{"T-A0100"})
	seq := exportTestElement(t, tag.Tag{Group: 0x0008, Element: 0x1140}, "SQ", [][]*sdicom.Element{{inner}})
	ds := sdicom.Dataset{Elements: []*sdicom.Element{
		exportTestElement(t, tag.Tag{Group: 0x0010, Element: 0x0010}, "PN", []string{"DOE^JANE"}),
		exportTestElement(t, tag.Tag{Group: 0x0010, Element: 0x0020}, "LO", []string{"MRN1"}),
		exportTestElement(t, tag.Tag{Group: 0x0008, Element: 0x0018}, "UI", []string{"1.2.3.4.5"}),
		exportTestElement(t, tag.Tag{Group: 0x0020, Element: 0x000D}, "UI", []string{"1.2.3"}),
		exportTestElement(t, tag.Tag{Group: 0x0020, Element: 0x000E}, "UI", []string{"1.2.3.1"}),
		exportTestElement(t, tag.Tag{Group: 0x0008, Element: 0x1030}, "LO", []string{"CT HEAD"}),
		exportTestElement(t, tag.Tag{Group: 0x0008, Element: 0x103E}, "LO", []string{"AX"}),
		exportTestElement(t, tag.Tag{Group: 0x0020, Element: 0x0013}, "IS", []string{"1"}),
		seq,
	}}
	model := newTagTreeModel()
	model.addDataset(ds)
	return model
}

// exportTestInstanceID descends the fixed root → patient → study → series →
// instance chain and returns the instance node's id.
func exportTestInstanceID(t *testing.T, m *tagTreeModel) string {
	t.Helper()
	id := ""
	for depth := 0; depth < 4; depth++ {
		kids := m.allChildIDs(id)
		if len(kids) != 1 {
			t.Fatalf("depth %d: %d children, want 1", depth, len(kids))
		}
		id = kids[0]
	}
	if !m.isInstanceNode(id) {
		t.Fatalf("node %q at instance depth is not flagged as an instance", id)
	}
	return id
}

func findExportElement(els []tagExportElement, tagStr string) *tagExportElement {
	for i := range els {
		if els[i].Tag == tagStr {
			return &els[i]
		}
	}
	return nil
}

// TestBuildInstanceTagExport verifies the single-instance export: element
// columns, the zero-filled "[GGGG,EEEE]" tag format (square brackets — Excel
// parses parenthesized tags as negative numbers), sequence nesting, and the
// element count.
func TestBuildInstanceTagExport(t *testing.T) {
	model := exportTestModel(t)
	els := buildInstanceTagExport(model, exportTestInstanceID(t, model))
	if len(els) != 9 {
		t.Fatalf("top-level elements = %d, want 9", len(els))
	}

	pn := findExportElement(els, "[0010,0010]")
	if pn == nil {
		t.Fatal("PatientName element missing from export — tag not formatted as zero-filled [0010,0010]?")
	}
	if pn.VR != "PN" || pn.Value != "DOE^JANE" || pn.Name == "" {
		t.Errorf("PatientName element = %+v, want VR PN, value DOE^JANE, non-empty name", pn)
	}

	// Every tag must match the "[GGGG,EEEE]" template: 11 characters,
	// square-bracketed, comma-separated, 4 hex digits per field.
	for _, e := range els {
		if len(e.Tag) != 11 || e.Tag[0] != '[' || e.Tag[5] != ',' || e.Tag[10] != ']' {
			t.Errorf("tag %q does not match the [GGGG,EEEE] template", e.Tag)
		}
	}

	sq := findExportElement(els, "[0008,1140]")
	if sq == nil || sq.VR != "SQ" {
		t.Fatalf("sequence element missing or wrong VR: %+v", sq)
	}
	if sq.Value != "" {
		t.Errorf("sequence element carries a value %q, want empty (items instead)", sq.Value)
	}
	if len(sq.Items) != 1 || len(sq.Items[0]) != 1 {
		t.Fatalf("sequence items = %+v, want one item with one element", sq.Items)
	}
	if nested := sq.Items[0][0]; nested.Tag != "[0008,0100]" || nested.Value != "T-A0100" {
		t.Errorf("nested element = %+v, want [0008,0100] = T-A0100", nested)
	}

	if got := countTagExportElements(els); got != 10 {
		t.Errorf("countTagExportElements = %d, want 10 (9 top-level + 1 nested)", got)
	}
}

// TestInstanceExportIgnoresFilter verifies the export always covers the whole
// instance — the search filter only affects the display.
func TestInstanceExportIgnoresFilter(t *testing.T) {
	model := exportTestModel(t)
	instID := exportTestInstanceID(t, model)
	model.setFilter("0010,0010")
	els := buildInstanceTagExport(model, instID)
	if len(els) != 9 {
		t.Errorf("filtered model exported %d top-level elements, want all 9", len(els))
	}
}

// TestResolveExportPath covers the format-from-extension mapping used by the
// save dialog's file-type selector, including the fallback when the typed
// filename carries no (or an unrelated) extension.
func TestResolveExportPath(t *testing.T) {
	cases := []struct {
		path, preferred string
		wantPath        string
		wantFormat      string
	}{
		{`C:\out\tags.csv`, "json", `C:\out\tags.csv`, "csv"},
		{`C:\out\tags.JSON`, "csv", `C:\out\tags.JSON`, "json"},
		{`C:\out\tags`, "json", `C:\out\tags.json`, "json"},
		{`C:\out\tags`, "csv", `C:\out\tags.csv`, "csv"},
		{`C:\out\tags`, "", `C:\out\tags.csv`, "csv"},
		{`C:\out\tags.txt`, "json", `C:\out\tags.txt.json`, "json"},
	}
	for _, c := range cases {
		gotPath, gotFormat := resolveExportPath(c.path, c.preferred)
		if gotPath != c.wantPath || gotFormat != c.wantFormat {
			t.Errorf("resolveExportPath(%q, %q) = (%q, %q), want (%q, %q)",
				c.path, c.preferred, gotPath, gotFormat, c.wantPath, c.wantFormat)
		}
	}
}

// TestExportTagsToCSV verifies the flat CSV shape, including the sequence
// path in the Tag column. Tags use square brackets so no field ever needs an
// Excel-safety wrapper — the file stays clean in text editors.
func TestExportTagsToCSV(t *testing.T) {
	model := exportTestModel(t)
	els := buildInstanceTagExport(model, exportTestInstanceID(t, model))
	path := filepath.Join(t.TempDir(), "out.csv")
	if err := exportTagsToCSV(path, els); err != nil {
		t.Fatalf("exportTagsToCSV: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("re-reading exported CSV: %v", err)
	}
	if len(rows) != 11 { // header + 9 top-level + 1 nested
		t.Fatalf("CSV has %d rows, want 11", len(rows))
	}
	if got, want := strings.Join(rows[0], ","), "Tag,VR,Name,Value"; got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
	findRow := func(tagField string) []string {
		for _, r := range rows[1:] {
			if r[0] == tagField {
				return r
			}
		}
		return nil
	}
	pn := findRow("[0010,0010]")
	if pn == nil {
		t.Fatal("PatientName row missing — Tag column not in plain [GGGG,EEEE] form?")
	}
	if pn[1] != "PN" || pn[3] != "DOE^JANE" {
		t.Errorf("PatientName row = %v, want VR PN, value DOE^JANE", pn)
	}
	if findRow("[0008,1140] > Item 1 > [0008,0100]") == nil {
		t.Error("nested sequence row missing or its path wrongly formatted")
	}
	// No cell may carry the ="…" formula wrapper — square brackets make it
	// unnecessary, keeping the raw file artifact-free.
	for _, r := range rows {
		for _, cell := range r {
			if strings.HasPrefix(cell, `="`) {
				t.Errorf("cell %q carries a formula wrapper", cell)
			}
		}
	}
}

// TestExportTagsToJSON verifies the JSON file round-trips into the same
// element list.
func TestExportTagsToJSON(t *testing.T) {
	model := exportTestModel(t)
	els := buildInstanceTagExport(model, exportTestInstanceID(t, model))
	path := filepath.Join(t.TempDir(), "out.json")
	if err := exportTagsToJSON(path, els); err != nil {
		t.Fatalf("exportTagsToJSON: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back []tagExportElement
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("exported JSON does not parse: %v", err)
	}
	if len(back) != 9 {
		t.Fatalf("round-trip elements = %d, want 9", len(back))
	}
	pn := findExportElement(back, "[0010,0010]")
	if pn == nil || pn.Value != "DOE^JANE" {
		t.Errorf("round-trip PatientName = %+v, want DOE^JANE", pn)
	}
	sq := findExportElement(back, "[0008,1140]")
	if sq == nil || len(sq.Items) != 1 || len(sq.Items[0]) != 1 || sq.Items[0][0].Value != "T-A0100" {
		t.Errorf("round-trip sequence = %+v, want one item with T-A0100", sq)
	}
}
