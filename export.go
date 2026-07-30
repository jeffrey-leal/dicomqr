package main

// CSV and JSON export of a single instance's DICOM elements from the tag
// viewer (right-click an instance row → Export Tags…) — the application's
// only export, deliberately scoped to one image. The export always contains
// the instance's complete element list: an active search filter changes what
// the tree displays, never what is exported. Tags are written as
// "[GGGG,EEEE]" — group and element each 4 hex digits, left-filled with '0'.
// Square brackets, not DICOM's conventional parentheses: Excel parses
// "(0020,0001)" as the negative number -20,001 (parentheses read as a
// negative sign, the comma as a thousands separator) regardless of CSV
// quoting, while a leading '[' keeps the value text in every locale — no
// ="…" formula wrapper polluting the raw file.

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveExportPath maps the save-dialog result to the export format implied
// by its file extension — the format is chosen via the dialog's file-type
// selector, with no separate format prompt. The native dialog does not append
// the selected type to a bare filename (and does not report which type was
// selected), so a path ending in neither .csv nor .json falls back to the
// preferred format (the exportFormat setting) and gains its extension.
func resolveExportPath(path, preferred string) (finalPath, format string) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".csv":
		return path, "csv"
	case ".json":
		return path, "json"
	}
	format = "csv"
	if strings.EqualFold(preferred, "json") {
		format = "json"
	}
	return path + "." + format, format
}

// tagExportElement is one DICOM element. Sequence (SQ) elements carry Items —
// one element list per sequence item — instead of a value.
type tagExportElement struct {
	Tag   string               `json:"tag"`
	VR    string               `json:"vr,omitempty"`
	Name  string               `json:"name,omitempty"`
	Value string               `json:"value,omitempty"`
	Items [][]tagExportElement `json:"items,omitempty"`
}

// buildInstanceTagExport converts the complete element list under an instance
// node (or, recursively, a sequence-item node) into export elements,
// bypassing the tree's search filter.
func buildInstanceTagExport(m *tagTreeModel, parentID string) []tagExportElement {
	var out []tagExportElement
	for _, elID := range m.allChildIDs(parentID) {
		n := m.snapshot(elID)
		e := tagExportElement{
			VR:    n.vr,
			Name:  n.name,
			Value: n.value,
		}
		if n.hasTag {
			e.Tag = fmt.Sprintf("[%04X,%04X]", n.tag.Group, n.tag.Element)
		}
		if n.vr == "SQ" {
			for _, itemID := range m.allChildIDs(elID) {
				e.Items = append(e.Items, buildInstanceTagExport(m, itemID))
			}
		}
		out = append(out, e)
	}
	return out
}

// countTagExportElements counts every element in the export, including those
// nested inside sequence items.
func countTagExportElements(els []tagExportElement) int {
	n := 0
	for _, e := range els {
		n++
		for _, item := range e.Items {
			n += countTagExportElements(item)
		}
	}
	return n
}

// exportTagsToCSV writes the export as flat rows, one per element:
// Tag, VR, Name, Value. Elements nested in sequences carry the path in the
// Tag column, e.g. "[0040,0275] > Item 1 > [0040,0007]".
func exportTagsToCSV(path string, els []tagExportElement) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"Tag", "VR", "Name", "Value"})
	var writeEls func(tagPrefix string, els []tagExportElement)
	writeEls = func(tagPrefix string, els []tagExportElement) {
		for _, e := range els {
			tagPath := e.Tag
			if tagPrefix != "" {
				tagPath = tagPrefix + " > " + e.Tag
			}
			w.Write([]string{tagPath, e.VR, e.Name, e.Value})
			for i, item := range e.Items {
				writeEls(fmt.Sprintf("%s > Item %d", tagPath, i+1), item)
			}
		}
	}
	writeEls("", els)
	w.Flush()
	return w.Error()
}

// exportTagsToJSON writes the element list as indented JSON (sequence items
// nested inside their element).
func exportTagsToJSON(path string, els []tagExportElement) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(els)
}
