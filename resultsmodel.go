package main

import (
	"fmt"
	"strings"
)

// node kinds
const (
	kindPatient = "P"
	kindStudy   = "S"
	kindSeries  = "R"
	kindImage   = "I"
)

// resultNode is one node in the query results tree.
type resultNode struct {
	id       string
	kind     string
	label    string
	tooltip  string
	children []string

	// DICOM identifiers used when building C-MOVE query datasets
	patientID        string
	studyInstanceUID string
	seriesInstanceUID string
	sopInstanceUID   string
}

// resultsModel is the data model backing the Fyne widget.Tree for query results.
type resultsModel struct {
	nodes    map[string]*resultNode
	roots    []string
	filter   string
	filtered []string // root-level IDs that survive the filter
}

func newResultsModel() *resultsModel {
	return &resultsModel{nodes: make(map[string]*resultNode)}
}

func (m *resultsModel) clear() {
	m.nodes = make(map[string]*resultNode)
	m.roots = nil
	m.filtered = nil
}

// addStudy inserts a study node (and its parent patient node if needed).
func (m *resultsModel) addStudy(patientName, patientID, studyUID, studyDate, studyDesc, accession, modalities string) {
	patID := "P:" + patientID
	if _, ok := m.nodes[patID]; !ok {
		label := fmt.Sprintf("Patient: %s", patientName)
		if patientID != "" {
			label += fmt.Sprintf("  (ID: %s)", patientID)
		}
		m.nodes[patID] = &resultNode{id: patID, kind: kindPatient, label: label, patientID: patientID}
		m.roots = append(m.roots, patID)
	}

	sID := "S:" + studyUID
	if _, ok := m.nodes[sID]; !ok {
		label := fmt.Sprintf("Study: %s", studyDate)
		if studyDesc != "" {
			label += "  " + studyDesc
		}
		if accession != "" {
			label += fmt.Sprintf("  ACC: %s", accession)
		}
		if modalities != "" {
			label += fmt.Sprintf("  [%s]", modalities)
		}
		tooltip := fmt.Sprintf("Study Instance UID: %s\nAccession: %s", studyUID, accession)
		m.nodes[sID] = &resultNode{
			id: sID, kind: kindStudy, label: label, tooltip: tooltip,
			patientID: patientID, studyInstanceUID: studyUID,
		}
		parent := m.nodes[patID]
		parent.children = append(parent.children, sID)
	}
	m.applyFilter()
}

// addSeries inserts a series node under an existing study node.
func (m *resultsModel) addSeries(studyUID, seriesUID, modality, seriesNumber, seriesDesc string, numInstances int) {
	sID := "S:" + studyUID
	rID := "R:" + seriesUID
	if _, ok := m.nodes[rID]; !ok {
		label := fmt.Sprintf("Series %s: %s", seriesNumber, modality)
		if seriesDesc != "" {
			label += "  " + seriesDesc
		}
		if numInstances > 0 {
			label += fmt.Sprintf("  [%d images]", numInstances)
		}
		tooltip := fmt.Sprintf("Series Instance UID: %s\nModality: %s", seriesUID, modality)
		study, ok := m.nodes[sID]
		if !ok {
			return
		}
		m.nodes[rID] = &resultNode{
			id: rID, kind: kindSeries, label: label, tooltip: tooltip,
			studyInstanceUID: studyUID, seriesInstanceUID: seriesUID,
		}
		study.children = append(study.children, rID)
	}
}

// setFilter stores a filter string and recomputes the visible root list.
func (m *resultsModel) setFilter(f string) {
	m.filter = strings.ToLower(f)
	m.applyFilter()
}

func (m *resultsModel) applyFilter() {
	if m.filter == "" {
		m.filtered = nil
		return
	}
	var visible []string
	for _, id := range m.roots {
		if m.nodeMatchesFilter(id) {
			visible = append(visible, id)
		}
	}
	m.filtered = visible
}

func (m *resultsModel) nodeMatchesFilter(id string) bool {
	n, ok := m.nodes[id]
	if !ok {
		return false
	}
	if strings.Contains(strings.ToLower(n.label), m.filter) {
		return true
	}
	for _, child := range n.children {
		if m.nodeMatchesFilter(child) {
			return true
		}
	}
	return false
}

func (m *resultsModel) activeRoots() []string {
	if m.filter != "" && m.filtered != nil {
		return m.filtered
	}
	return m.roots
}

// --- Fyne widget.Tree interface ---

func (m *resultsModel) childUIDs(id string) []string {
	if id == "" {
		return m.activeRoots()
	}
	n, ok := m.nodes[id]
	if !ok {
		return nil
	}
	return n.children
}

func (m *resultsModel) isBranch(id string) bool {
	if id == "" {
		// Virtual root must be a branch so Tree.walk recurses into childUIDs("").
		return true
	}
	n, ok := m.nodes[id]
	if !ok {
		return false
	}
	// Patient nodes are always expandable (they always contain study children).
	// Study and series nodes are leaves unless series/image children are present.
	return n.kind == kindPatient || len(n.children) > 0
}

func (m *resultsModel) labelFor(id string) string {
	if n, ok := m.nodes[id]; ok {
		return n.label
	}
	return id
}

func (m *resultsModel) tooltipFor(id string) string {
	if n, ok := m.nodes[id]; ok {
		return n.tooltip
	}
	return ""
}

// uidsForNode returns the DICOM UIDs for a given tree node ID.
func (m *resultsModel) uidsForNode(id string) (patientID, studyUID, seriesUID, sopUID string) {
	n, ok := m.nodes[id]
	if !ok {
		return
	}
	return n.patientID, n.studyInstanceUID, n.seriesInstanceUID, n.sopInstanceUID
}
