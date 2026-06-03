package main

import (
	"fmt"
	"sort"
	"strconv"
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
	sortKey  string // patient: lower-case name; study: YYYYMMDD date; series: zero-padded number

	// DICOM identifiers used when building C-MOVE query datasets
	patientID         string
	studyInstanceUID  string
	seriesInstanceUID string
	sopInstanceUID    string

	seriesLoaded bool // true once a series C-FIND has been fired for this study node

	// Raw DICOM field values retained for CSV/JSON export (Phase 4-A).
	patientName  string // patient nodes
	studyDate    string // study nodes
	studyDesc    string // study nodes
	accession    string // study nodes
	modalities   string // study nodes
	seriesNumber string // series nodes
	modality     string // series nodes
	numInstances int    // series nodes
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

// sortedInsert uses binary search to find the correct position for id in list
// and inserts it there. list is assumed to be already sorted by sortKey.
func (m *resultsModel) sortedInsert(list *[]string, id string) {
	pos := sort.Search(len(*list), func(i int) bool {
		return m.nodes[(*list)[i]].sortKey >= m.nodes[id].sortKey
	})
	*list = append((*list)[:pos], append([]string{id}, (*list)[pos:]...)...)
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
		m.nodes[patID] = &resultNode{
			id: patID, kind: kindPatient, label: label,
			patientID:   patientID,
			patientName: patientName,
			sortKey:     strings.ToLower(patientName),
		}
		m.sortedInsert(&m.roots, patID)
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
			sortKey:    studyDate,
			studyDate:  studyDate,
			studyDesc:  studyDesc,
			accession:  accession,
			modalities: modalities,
		}
		parent := m.nodes[patID]
		m.sortedInsert(&parent.children, sID)
	}
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
		n, _ := strconv.Atoi(strings.TrimSpace(seriesNumber))
		m.nodes[rID] = &resultNode{
			id: rID, kind: kindSeries, label: label, tooltip: tooltip,
			patientID:         study.patientID, // propagate so retrieve can build C-MOVE filters
			studyInstanceUID:  studyUID,
			seriesInstanceUID: seriesUID,
			sortKey:           fmt.Sprintf("%010d", n),
			seriesNumber:      seriesNumber,
			modality:          modality,
			numInstances:      numInstances,
		}
		m.sortedInsert(&study.children, rID)
	}
}

func (m *resultsModel) isSeriesLoaded(id string) bool {
	n, ok := m.nodes[id]
	return ok && n.seriesLoaded
}

func (m *resultsModel) markSeriesLoaded(id string) {
	if n, ok := m.nodes[id]; ok {
		n.seriesLoaded = true
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
	// Patient and study nodes are always branches; study nodes show an expand
	// arrow before series are loaded so OnBranchOpened can trigger lazy loading.
	return n.kind == kindPatient || n.kind == kindStudy
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

// ExportRow is one flat record produced for CSV/JSON export.
type ExportRow struct {
	PatientName  string `json:"patientName"`
	PatientID    string `json:"patientID"`
	StudyDate    string `json:"studyDate"`
	StudyDesc    string `json:"studyDescription"`
	Accession    string `json:"accessionNumber"`
	Modalities   string `json:"modalities"`
	StudyUID     string `json:"studyInstanceUID"`
	SeriesUID    string `json:"seriesInstanceUID"`
	SeriesNumber string `json:"seriesNumber"`
	Modality     string `json:"modality"`
	NumInstances int    `json:"numInstances"`
}

// exportRows returns a flat list of all visible study/series nodes for export.
// Studies with no series loaded produce one row each; studies with series loaded
// produce one row per series.
func (m *resultsModel) exportRows() []ExportRow {
	var rows []ExportRow
	for _, patID := range m.roots {
		pat, ok := m.nodes[patID]
		if !ok {
			continue
		}
		for _, studyID := range pat.children {
			study, ok := m.nodes[studyID]
			if !ok {
				continue
			}
			base := ExportRow{
				PatientName: pat.patientName,
				PatientID:   pat.patientID,
				StudyDate:   study.studyDate,
				StudyDesc:   study.studyDesc,
				Accession:   study.accession,
				Modalities:  study.modalities,
				StudyUID:    study.studyInstanceUID,
			}
			if len(study.children) == 0 {
				rows = append(rows, base)
				continue
			}
			for _, seriesID := range study.children {
				sr, ok := m.nodes[seriesID]
				if !ok {
					continue
				}
				row := base
				row.SeriesUID = sr.seriesInstanceUID
				row.SeriesNumber = sr.seriesNumber
				row.Modality = sr.modality
				row.NumInstances = sr.numInstances
				rows = append(rows, row)
			}
		}
	}
	return rows
}
