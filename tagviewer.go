package main

// Tag-level review of local DICOM files, ported from the dicomhdr application
// (dicomtree.go + treerow.go + the folder loader). showTagViewerWindow opens a
// dedicated window presenting a Patient → Study → Series → Instance → element
// hierarchy for a set of files, with tag search, expand/collapse, DICOM
// dictionary hover tooltips, and copy actions. Private tags render italic;
// public tags whose VR contradicts the DICOM dictionary render in the theme
// error colour. The model and semantics are kept identical to dicomhdr so the
// two tools present the same review of the same files.

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	sqweekdialog "github.com/sqweek/dialog"
	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// ── Tree model (port of dicomhdr's dicomTreeModel) ────────────────────────────

type tagNode struct {
	label       string
	lower       string            // label lowercased, filled on first use by buildVisible
	key         string            // identity used to match datasets to this node; not displayed
	children    []string          // child node IDs, in display order
	childKeys   map[string]string // key -> child ID, for O(1) findOrCreate (structural nodes only; lazily created)
	sortKey     float64
	isPrivate   bool
	isMalformed bool
	isInstance  bool // true on instance-level structural nodes (export scope)
	elmTag      tag.Tag
	hasTag      bool
	// Structured element fields kept alongside the display label so Export…
	// can emit real columns instead of re-parsing labels. Empty on structural
	// nodes (patient/study/series/instance/sequence items).
	vr      string // raw VR as read from the file
	tagName string // dictionary name ("Unknown" when not in the dictionary)
	value   string // formatted value; empty for SQ elements
}

type tagTreeModel struct {
	mu         sync.RWMutex
	nodes      map[string]*tagNode
	counter    int
	filterText string

	// The filtered view, built once per (filter, tree version) — see
	// childUIDs. version counts changes to the tree (addDataset), so a view
	// built while files were still loading is rebuilt when more arrive.
	version       int
	visible       map[string][]string // node ID -> its children that match or lead to a match
	visibleFilter string
	visibleOf     int // the version visible was built for
	visibleValid  bool
}

func newTagTreeModel() *tagTreeModel {
	return &tagTreeModel{
		nodes: map[string]*tagNode{
			"": {label: "", children: nil},
		},
	}
}

func (m *tagTreeModel) nextID() string {
	m.counter++
	return fmt.Sprintf("%d", m.counter)
}

// setFilter updates the text used to filter the tree. Case-insensitive.
func (m *tagTreeModel) setFilter(text string) {
	m.mu.Lock()
	m.filterText = strings.ToLower(text)
	m.mu.Unlock()
}

// childUIDs returns id's children as the tree shows them: all of them, or with
// a search active, those whose own label or some descendant's contains the
// search text.
//
// The filtered view is built in one pass over the whole tree and cached until
// the search text or the tree changes. Fyne asks for the children of every open
// branch on every layout — every scroll frame — and after a search the viewer
// opens every branch, so the old per-call check (recursing into each child's
// subtree, lowercasing every label as it went) redid the whole tree's matching,
// and allocated a lowercase copy of every label, once per branch per frame: on
// a large study, hundreds of thousands of element nodes, scrolling crawled.
func (m *tagTreeModel) childUIDs(id string) []string {
	m.mu.RLock()
	n, ok := m.nodes[id]
	if !ok {
		m.mu.RUnlock()
		return nil
	}
	if m.filterText == "" {
		m.mu.RUnlock()
		return n.children
	}
	if m.visibleValid && m.visibleFilter == m.filterText && m.visibleOf == m.version {
		v := m.visible[id]
		m.mu.RUnlock()
		return v
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if !(m.visibleValid && m.visibleFilter == m.filterText && m.visibleOf == m.version) {
		m.buildVisible()
	}
	return m.visible[id]
}

// buildVisible computes the filtered view: a post-order walk marks each node
// matching when its label or any descendant's contains the filter, recording
// the matching children of each node on the way. Caller holds the write lock.
func (m *tagTreeModel) buildVisible() {
	visible := make(map[string][]string)
	var walk func(id string) bool
	walk = func(id string) bool {
		n := m.nodes[id]
		if n == nil {
			return false
		}
		var kept []string
		for _, c := range n.children {
			if walk(c) {
				kept = append(kept, c)
			}
		}
		if len(kept) > 0 {
			visible[id] = kept
		}
		if n.lower == "" && n.label != "" {
			n.lower = strings.ToLower(n.label) // labels never change once created
		}
		return len(kept) > 0 || strings.Contains(n.lower, m.filterText)
	}
	walk("")
	m.visible, m.visibleFilter, m.visibleOf, m.visibleValid = visible, m.filterText, m.version, true
}

// tooltipFor returns a formatted DICOM standard description for the node's tag.
// Returns "" for structural nodes (patient/study/series/instance) and unknown tags.
func (m *tagTreeModel) tooltipFor(id string) string {
	m.mu.RLock()
	n, ok := m.nodes[id]
	m.mu.RUnlock()
	if !ok || !n.hasTag {
		return ""
	}
	info, err := tag.Find(n.elmTag)
	if err != nil {
		return "" // private or non-standard tag — no dictionary entry
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Tag:     (%04X,%04X)\n", n.elmTag.Group, n.elmTag.Element)
	if info.Name != "" {
		fmt.Fprintf(&sb, "Name:    %s\n", info.Name)
	}
	if info.Keyword != "" {
		fmt.Fprintf(&sb, "Keyword: %s\n", info.Keyword)
	}
	if len(info.VRs) > 0 {
		fmt.Fprintf(&sb, "VR:      %s\n", strings.Join(info.VRs, ", "))
	}
	if info.VM != "" {
		fmt.Fprintf(&sb, "VM:      %s", info.VM)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (m *tagTreeModel) isBranch(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.nodes[id]
	if !ok {
		return false
	}
	return id == "" || len(n.children) > 0
}

func (m *tagTreeModel) labelFor(id string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if n, ok := m.nodes[id]; ok {
		return n.label
	}
	return id
}

func (m *tagTreeModel) isPrivateNode(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if n, ok := m.nodes[id]; ok {
		return n.isPrivate
	}
	return false
}

func (m *tagTreeModel) isMalformedNode(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if n, ok := m.nodes[id]; ok {
		return n.isMalformed
	}
	return false
}

func (m *tagTreeModel) nodeTag(id string) (tag.Tag, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if n, ok := m.nodes[id]; ok {
		return n.elmTag, n.hasTag
	}
	return tag.Tag{}, false
}

func (m *tagTreeModel) addDataset(ds sdicom.Dataset) {
	patientName := stringVal(ds, tag.Tag{Group: 0x0010, Element: 0x0010})
	studyDesc := stringVal(ds, tag.Tag{Group: 0x0008, Element: 0x1030})
	seriesDesc := stringVal(ds, tag.Tag{Group: 0x0008, Element: 0x103E})
	instanceNum := stringVal(ds, tag.Tag{Group: 0x0020, Element: 0x0013})
	sliceLoc := stringVal(ds, tag.Tag{Group: 0x0020, Element: 0x1041})

	studyDateRaw := stringVal(ds, tag.Tag{Group: 0x0008, Element: 0x0020})
	studyLabel := studyDesc
	studySortKey := math.MaxFloat64
	if d := formatDICOMDate(studyDateRaw); d != "" {
		studyLabel = "[" + d + "] " + studyDesc
		if v, err := strconv.ParseFloat(studyDateRaw, 64); err == nil {
			studySortKey = v
		}
	}

	seriesTimeRaw := stringVal(ds, tag.Tag{Group: 0x0008, Element: 0x0031})
	seriesLabel := seriesDesc
	seriesSortKey := math.MaxFloat64
	if t := formatDICOMTime(seriesTimeRaw); t != "" {
		seriesLabel = "[" + t + "] " + seriesDesc
		if v, err := strconv.ParseFloat(seriesTimeRaw[:6], 64); err == nil {
			seriesSortKey = v
		}
	}

	// Identity keys uniquely distinguish nodes that may share a display label.
	// Each falls back to its label when the UID is absent, so files lacking the
	// preferred identifier still group sensibly instead of all collapsing into
	// one node. SOP Instance UID in particular ensures resliced/reformatted
	// series (where Slice Location and Instance Number are often missing or
	// duplicated) list every image rather than merging into a single instance.
	patientKey := firstNonUnknown(stringVal(ds, tag.Tag{Group: 0x0010, Element: 0x0020}), patientName)
	studyKey := firstNonUnknown(stringVal(ds, tag.Tag{Group: 0x0020, Element: 0x000D}), studyDesc)
	seriesKey := firstNonUnknown(stringVal(ds, tag.Tag{Group: 0x0020, Element: 0x000E}), seriesDesc)

	// Choose the instance label and sort key, in order of preference:
	//   1. Orientation-aware position — derive the image plane from Image
	//      Orientation Patient (0020,0037) and use the matching component of
	//      Image Position Patient (0020,0032): Z (axial), X (sagittal),
	//      Y (coronal). This gives reslices a meaningful, monotonic position.
	//   2. Slice Location (0020,1041) — the legacy scalar, when position/
	//      orientation are unavailable.
	//   3. Instance Number (0020,0013) — last resort.
	sortKey := math.MaxFloat64
	var instanceLabel string
	iop := floatsVal(ds, tag.Tag{Group: 0x0020, Element: 0x0037})
	ipp := floatsVal(ds, tag.Tag{Group: 0x0020, Element: 0x0032})
	if plane, axis := orientationPlane(iop); plane != "" && axis < len(ipp) {
		coord := ipp[axis]
		instanceLabel = fmt.Sprintf("%s location: %s", plane, strconv.FormatFloat(coord, 'f', -1, 64))
		sortKey = coord
	} else if sliceLoc != "Unknown" {
		instanceLabel = "Slice location: " + sliceLoc
		if f, err := strconv.ParseFloat(strings.TrimSpace(sliceLoc), 64); err == nil {
			sortKey = f
		}
	} else {
		instanceLabel = "Instance number: " + instanceNum
		if f, err := strconv.ParseFloat(strings.TrimSpace(instanceNum), 64); err == nil {
			sortKey = f
		}
	}
	instanceKey := firstNonUnknown(stringVal(ds, tag.Tag{Group: 0x0008, Element: 0x0018}), instanceLabel)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.version++ // the tree is changing: any cached filtered view is stale

	patientID := m.findOrCreate("", patientKey, patientName)
	studyID := m.findOrCreateSorted(patientID, studyKey, studyLabel, studySortKey)
	seriesID := m.findOrCreateSorted(studyID, seriesKey, seriesLabel, seriesSortKey)
	instanceID := m.findOrCreateSorted(seriesID, instanceKey, instanceLabel, sortKey)
	m.nodes[instanceID].isInstance = true

	for _, el := range ds.Elements {
		m.addElement(instanceID, el)
	}
}

// firstNonUnknown returns primary unless it is the "Unknown" sentinel returned
// by stringVal for a missing/empty tag, in which case it returns fallback.
func firstNonUnknown(primary, fallback string) string {
	if primary != "Unknown" {
		return primary
	}
	return fallback
}

// floatsVal reads a numeric multi-valued element as []float64. DICOM DS/IS
// values arrive as strings; numeric VRs may arrive as []int or []float64.
// Returns nil if the tag is absent or any value cannot be parsed.
func floatsVal(ds sdicom.Dataset, t tag.Tag) []float64 {
	el, err := ds.FindElementByTag(t)
	if err != nil || el == nil {
		return nil
	}
	switch v := el.Value.GetValue().(type) {
	case []float64:
		return v
	case []int:
		out := make([]float64, len(v))
		for i, n := range v {
			out[i] = float64(n)
		}
		return out
	case []string:
		out := make([]float64, 0, len(v))
		for _, s := range v {
			f, perr := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if perr != nil {
				return nil
			}
			out = append(out, f)
		}
		return out
	}
	return nil
}

// orientationPlane derives the image plane from the Image Orientation Patient
// (0020,0037) direction cosines: the slice normal is the cross product of the
// row and column vectors, and the patient axis it aligns with most strongly
// names the plane. It returns the plane label ("Axial", "Sagittal", "Coronal")
// and the index into Image Position Patient (0020,0032) whose component is the
// slice position along that normal (Z=2 axial, X=0 sagittal, Y=1 coronal).
// Oblique acquisitions are classified by their dominant axis. Returns ("", 0)
// when orientation data is missing or malformed.
func orientationPlane(iop []float64) (plane string, axis int) {
	if len(iop) < 6 {
		return "", 0
	}
	rowX, rowY, rowZ := iop[0], iop[1], iop[2]
	colX, colY, colZ := iop[3], iop[4], iop[5]
	// normal = row × col
	nx := math.Abs(rowY*colZ - rowZ*colY)
	ny := math.Abs(rowZ*colX - rowX*colZ)
	nz := math.Abs(rowX*colY - rowY*colX)
	switch {
	case nx >= ny && nx >= nz:
		return "Sagittal", 0
	case ny >= nx && ny >= nz:
		return "Coronal", 1
	default:
		return "Axial", 2
	}
}

func (m *tagTreeModel) findOrCreate(parentID, key, label string) string {
	parent := m.nodes[parentID]
	if id, ok := parent.childKeys[key]; ok {
		return id
	}
	id := m.nextID()
	m.nodes[id] = &tagNode{label: label, key: key}
	parent.children = append(parent.children, id)
	if parent.childKeys == nil {
		parent.childKeys = make(map[string]string)
	}
	parent.childKeys[key] = id
	return id
}

func (m *tagTreeModel) findOrCreateSorted(parentID, key, label string, sortKey float64) string {
	parent := m.nodes[parentID]
	if id, ok := parent.childKeys[key]; ok {
		return id
	}
	id := m.nextID()
	m.nodes[id] = &tagNode{label: label, key: key, sortKey: sortKey}
	// Keep children ordered by (sortKey, key) via binary-search insertion (the
	// slice is always sorted because every instance is inserted this way). The
	// key (the SOP Instance UID, when present) is the tiebreaker so images
	// sharing a sort position — e.g. reformats with no Slice Location or
	// Instance Number — get a stable, deterministic order even under concurrent
	// insertion, rather than depending on which worker happens to insert first.
	idx := sort.Search(len(parent.children), func(i int) bool {
		c := m.nodes[parent.children[i]]
		if c.sortKey != sortKey {
			return c.sortKey > sortKey
		}
		return c.key > key
	})
	parent.children = append(parent.children, "")
	copy(parent.children[idx+1:], parent.children[idx:])
	parent.children[idx] = id
	if parent.childKeys == nil {
		parent.childKeys = make(map[string]string)
	}
	parent.childKeys[key] = id
	return id
}

// vrMatchesStandard returns false (malformed) when a known tag carries a VR
// that is not in the standard's acceptable list for that tag.
// Empty VR is exempt: it appears when the parser cannot determine the VR
// (e.g. unknown tag in implicit transfer syntax).
// "UN" is intentionally NOT exempt: in explicit VR transfer syntax a known
// public tag encoded as UN is a real VR mismatch and should be flagged.
func vrMatchesStandard(vr string, acceptable []string) bool {
	if vr == "" {
		return true
	}
	for _, v := range acceptable {
		if v == vr {
			return true
		}
	}
	return false
}

func (m *tagTreeModel) addElement(parentID string, el *sdicom.Element) {
	info, err := tag.Find(el.Tag)
	name := info.Name
	if name == "" {
		name = "Unknown"
	}
	prefix := fmt.Sprintf("(%04X,%04X) [%s] %s", el.Tag.Group, el.Tag.Element, el.RawValueRepresentation, name)

	private := el.Tag.Group%2 != 0
	// Malformed: public tag found in the DICOM dictionary but carrying a VR
	// that is not acceptable per the standard. Private tags and tags absent
	// from the dictionary are not flagged — they get other treatment (italic /
	// no highlight) or cannot be checked against a standard entry.
	malformed := !private && err == nil && !vrMatchesStandard(el.RawValueRepresentation, info.VRs)

	if el.RawValueRepresentation == "SQ" {
		seqID := m.nextID()
		m.nodes[seqID] = &tagNode{label: prefix, isPrivate: private, isMalformed: malformed, elmTag: el.Tag, hasTag: true,
			vr: el.RawValueRepresentation, tagName: name}
		m.nodes[parentID].children = append(m.nodes[parentID].children, seqID)

		if items, ok := el.Value.GetValue().([]*sdicom.SequenceItemValue); ok {
			for i, item := range items {
				itemID := m.nextID()
				m.nodes[itemID] = &tagNode{label: fmt.Sprintf("Item %d", i+1)}
				m.nodes[seqID].children = append(m.nodes[seqID].children, itemID)
				if subEls, ok := item.GetValue().([]*sdicom.Element); ok {
					for _, subEl := range subEls {
						m.addElement(itemID, subEl)
					}
				}
			}
		}
	} else {
		leafID := m.nextID()
		val := formatValue(el)
		m.nodes[leafID] = &tagNode{label: prefix + ": " + val, isPrivate: private, isMalformed: malformed, elmTag: el.Tag, hasTag: true,
			vr: el.RawValueRepresentation, tagName: name, value: val}
		m.nodes[parentID].children = append(m.nodes[parentID].children, leafID)
	}
}

// tagNodeSnapshot is a copy of one node's exportable fields.
type tagNodeSnapshot struct {
	label, vr, name, value string
	tag                    tag.Tag
	hasTag                 bool
}

// snapshot returns a copy of the node's exportable fields (zero value for
// unknown ids).
func (m *tagTreeModel) snapshot(id string) tagNodeSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if n, ok := m.nodes[id]; ok {
		return tagNodeSnapshot{label: n.label, vr: n.vr, name: n.tagName, value: n.value, tag: n.elmTag, hasTag: n.hasTag}
	}
	return tagNodeSnapshot{}
}

// isInstanceNode reports whether id is an instance-level structural node —
// the only nodes offering Export Tags… in the context menu.
func (m *tagTreeModel) isInstanceNode(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if n, ok := m.nodes[id]; ok {
		return n.isInstance
	}
	return false
}

// allChildIDs returns a copy of the node's children regardless of the active
// search filter — the export always covers an instance's complete tag set.
func (m *tagTreeModel) allChildIDs(id string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if n, ok := m.nodes[id]; ok {
		return append([]string(nil), n.children...)
	}
	return nil
}

// formatDICOMDate converts a DICOM DA value (YYYYMMDD) to YYYY-MM-DD.
// Returns "" when the input is absent, non-numeric, or not exactly 8 digits.
func formatDICOMDate(s string) string {
	if len(s) != 8 {
		return ""
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return s[0:4] + "-" + s[4:6] + "-" + s[6:8]
}

// formatDICOMTime converts a DICOM TM value (HHMMSS[.frac]) to HH:MM:SS.
// Returns "" when the input is absent, shorter than 6 characters, or non-numeric.
func formatDICOMTime(s string) string {
	if len(s) < 6 {
		return ""
	}
	for _, c := range s[:6] {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return s[0:2] + ":" + s[2:4] + ":" + s[4:6]
}

func stringVal(ds sdicom.Dataset, t tag.Tag) string {
	el, err := ds.FindElementByTag(t)
	if err != nil || el == nil {
		return "Unknown"
	}
	vals, ok := el.Value.GetValue().([]string)
	if !ok || len(vals) == 0 || vals[0] == "" {
		return "Unknown"
	}
	return vals[0]
}

func formatValue(el *sdicom.Element) string {
	switch vals := el.Value.GetValue().(type) {
	case []string:
		cleaned := make([]string, len(vals))
		for i, s := range vals {
			cleaned[i] = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
		}
		return strings.Join(cleaned, `\`)
	case []int:
		parts := make([]string, len(vals))
		for i, n := range vals {
			parts[i] = fmt.Sprintf("%d", n)
		}
		return strings.Join(parts, ", ")
	case []float64:
		parts := make([]string, len(vals))
		for i, f := range vals {
			parts[i] = fmt.Sprintf("%g", f)
		}
		return strings.Join(parts, ", ")
	case []byte:
		return fmt.Sprintf("[binary, %d bytes]", len(vals))
	case sdicom.PixelDataInfo:
		if vals.IntentionallySkipped {
			return "[pixel data — not read]"
		}
		return fmt.Sprintf("[%d frame(s)]", len(vals.Frames))
	default:
		return fmt.Sprintf("%v", el.Value.GetValue())
	}
}

// ── Row widget (port of dicomhdr's treeRow) ───────────────────────────────────

// tagTreeRow is the canvas object used for each row in the tag tree.
// It wraps a canvas.Text with rowLayout, supports right-click context menus
// via fyne.SecondaryTappable, and shows a DICOM standard tooltip on hover.
type tagTreeRow struct {
	widget.BaseWidget
	ct          *canvas.Text
	nodeID      string
	tooltipText string        // pre-computed in the tree update callback; empty → no tooltip
	cv          fyne.Canvas   // used to anchor the popup
	hoverPos    fyne.Position // updated by MouseMoved; read by the timer callback
	hoverTimer  *time.Timer
	showPending bool // cleared by MouseOut to cancel an in-flight timer
	tooltipPop  *widget.PopUp
	onMenu      func(id string, pos fyne.Position)
}

func newTagTreeRow(cv fyne.Canvas, onMenu func(id string, pos fyne.Position)) *tagTreeRow {
	tr := &tagTreeRow{
		ct:     canvas.NewText("", theme.Color(theme.ColorNameForeground)),
		cv:     cv,
		onMenu: onMenu,
	}
	tr.ExtendBaseWidget(tr)
	return tr
}

func (tr *tagTreeRow) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(container.New(rowLayout{}, tr.ct))
}

func (tr *tagTreeRow) TappedSecondary(e *fyne.PointEvent) {
	if tr.onMenu != nil && tr.nodeID != "" {
		tr.onMenu(tr.nodeID, e.AbsolutePosition)
	}
}

func (tr *tagTreeRow) MouseIn(e *desktop.MouseEvent) {
	tr.hideTooltip()
	if tr.tooltipText == "" {
		return
	}
	tr.showPending = true
	tr.hoverPos = e.AbsolutePosition
	tr.hoverTimer = time.AfterFunc(600*time.Millisecond, func() {
		fyne.Do(func() {
			if tr.showPending {
				tr.showTooltip()
			}
		})
	})
}

func (tr *tagTreeRow) MouseMoved(e *desktop.MouseEvent) {
	// Track position so the tooltip appears where the cursor settled, not
	// where it first entered the row.
	tr.hoverPos = e.AbsolutePosition
}

func (tr *tagTreeRow) MouseOut() {
	tr.hideTooltip()
}

func (tr *tagTreeRow) showTooltip() {
	if tr.cv == nil || tr.tooltipText == "" {
		return
	}
	lbl := widget.NewLabel(tr.tooltipText)
	lbl.TextStyle = fyne.TextStyle{Monospace: true}
	tr.tooltipPop = widget.NewPopUp(container.NewPadded(lbl), tr.cv)
	tr.tooltipPop.ShowAtPosition(fyne.NewPos(tr.hoverPos.X+12, tr.hoverPos.Y+16))
}

func (tr *tagTreeRow) hideTooltip() {
	tr.showPending = false
	if tr.hoverTimer != nil {
		tr.hoverTimer.Stop()
		tr.hoverTimer = nil
	}
	if tr.tooltipPop != nil {
		tr.tooltipPop.Hide()
		tr.tooltipPop = nil
	}
}

// ── Parsing and the viewer window ─────────────────────────────────────────────

// parseDICOMTags parses one file for tag review: full element tree, pixel data
// skipped, lenient about the common standard violations the download folder can
// contain. Parser panics are handled by safeParseFile (dicomsafe.go), so one
// malformed file cannot crash the app.
func parseDICOMTags(path string) (sdicom.Dataset, error) {
	return safeParseFile(path, nil,
		sdicom.SkipPixelData(),
		sdicom.AllowMismatchPixelDataLength(),
		sdicom.AllowMissingMetaElementGroupLength(),
		sdicom.AllowUnknownSpecificCharacterSet(),
	)
}

// openTagTrees tracks the tree of every open tag viewer window so preference
// changes (Tag Highlights, Tag Profiles) re-style them immediately.
var (
	openTagTreesMu sync.Mutex
	openTagTrees   = map[fyne.Window]*widget.Tree{}
)

// refreshOpenTagViewers re-renders every open tag viewer window. Called from
// the Preferences Apply handler (UI goroutine) after settings change.
func refreshOpenTagViewers() {
	openTagTreesMu.Lock()
	trees := make([]*widget.Tree, 0, len(openTagTrees))
	for _, t := range openTagTrees {
		trees = append(trees, t)
	}
	openTagTreesMu.Unlock()
	for _, t := range trees {
		t.Refresh()
	}
}

// showTagViewerWindow opens a dicomhdr-style tag review window over paths:
// Patient → Study → Series → Instance nodes with every instance's full element
// list. Files are parsed concurrently across a worker pool while the tree
// populates incrementally, so large studies appear progressively rather than
// blocking. Row styling follows cfg: private tags italic when ItalicPrivate,
// VR-violating tags in MalformedColor, and Tag Profile colours for matching
// tags. Must be called from a non-UI goroutine.
// startInstanceTagExport exports one instance's complete element list to CSV
// or JSON. Reached from the tag tree's right-click menu on an instance row —
// the application's only export, deliberately scoped to a single image. The
// native save dialog opens directly (no intermediary format dialog): its
// file-type selector chooses the format, the exportFormat preference decides
// which type is listed first (Windows preselects the first filter) and
// covers filenames typed without an extension. The active search filter
// changes what the tree displays, never what is exported.
func startInstanceTagExport(win fyne.Window, cfg *Settings, model *tagTreeModel, instanceID string) {
	preferred := cfg.ExportFormat
	go func() {
		dlg := sqweekdialog.File().Title("Export tags")
		if strings.EqualFold(preferred, "json") {
			dlg = dlg.Filter("JSON file (*.json)", "json").Filter("CSV file (*.csv)", "csv")
		} else {
			dlg = dlg.Filter("CSV file (*.csv)", "csv").Filter("JSON file (*.json)", "json")
		}
		path, err := dlg.Save()
		if err != nil {
			return // save dialog cancelled
		}
		path, format := resolveExportPath(path, preferred)
		els := buildInstanceTagExport(model, instanceID)
		var writeErr error
		if format == "json" {
			writeErr = exportTagsToJSON(path, els)
		} else {
			writeErr = exportTagsToCSV(path, els)
		}
		n := countTagExportElements(els)
		fyne.Do(func() {
			if writeErr != nil {
				dialog.ShowError(writeErr, win)
			} else {
				dialog.ShowInformation("Export complete",
					fmt.Sprintf("Exported %d tag(s) to:\n%s", n, path), win)
			}
		})
	}()
}

func showTagViewerWindow(a fyne.App, cfg *Settings, title string, paths []string) {
	model := newTagTreeModel()
	total := len(paths)

	var filesLoaded, errCount atomic.Int64

	// Widgets are created on the UI goroutine below; the loader goroutines only
	// touch them inside fyne.Do closures, which the FIFO queue guarantees run
	// after the creation closure.
	var tree *widget.Tree
	var win fyne.Window
	statusLbl := widget.NewLabel(fmt.Sprintf("Loading %d file(s)…", total))
	progress := widget.NewProgressBarInfinite()

	updateStatus := func(final bool, elapsed time.Duration) {
		n := filesLoaded.Load()
		e := errCount.Load()
		text := fmt.Sprintf("Files loaded: %d / %d", n, total)
		if e > 0 {
			text += fmt.Sprintf("  |  Unreadable: %d", e)
		}
		if final {
			if elapsed < time.Second {
				text += fmt.Sprintf("  |  loaded in %d ms", elapsed.Milliseconds())
			} else {
				text += fmt.Sprintf("  |  loaded in %.2f s", elapsed.Seconds())
			}
		}
		statusLbl.SetText(text)
	}

	fyne.Do(func() {
		win = a.NewWindow(title)

		// Right-click on a row: copy the whole row or just the value part.
		// Instance rows additionally offer Export Tags… — the application's
		// only export, deliberately scoped to a single image instance.
		onMenu := func(id string, pos fyne.Position) {
			label := model.labelFor(id)
			copyRow := fyne.NewMenuItem("Copy row", func() {
				win.Clipboard().SetContent(label)
			})
			copyValue := fyne.NewMenuItem("Copy value", func() {
				if i := strings.Index(label, ": "); i >= 0 {
					win.Clipboard().SetContent(label[i+2:])
				} else {
					win.Clipboard().SetContent(label)
				}
			})
			items := []*fyne.MenuItem{copyRow, copyValue}
			if model.isInstanceNode(id) {
				items = append(items, fyne.NewMenuItemSeparator(),
					fyne.NewMenuItem("Export Tags…", func() {
						startInstanceTagExport(win, cfg, model, id)
					}))
			}
			popup := widget.NewPopUpMenu(fyne.NewMenu("", items...), win.Canvas())
			popup.ShowAtPosition(pos)
		}

		tree = widget.NewTree(
			model.childUIDs,
			model.isBranch,
			func(_ bool) fyne.CanvasObject { return newTagTreeRow(win.Canvas(), onMenu) },
			func(id widget.TreeNodeID, _ bool, node fyne.CanvasObject) {
				row := node.(*tagTreeRow)
				row.nodeID = id
				row.tooltipText = model.tooltipFor(id)
				row.ct.Text = model.labelFor(id)
				row.ct.TextSize = theme.TextSize()
				row.ct.TextStyle = fyne.TextStyle{Italic: cfg.ItalicPrivate && model.isPrivateNode(id)}
				elmTag, hasTag := model.nodeTag(id)
				if model.isMalformedNode(id) {
					row.ct.Color = malformedTagColor(cfg)
				} else if c, ok := matchProfile(elmTag, hasTag, cfg.TagProfiles); ok {
					row.ct.Color = c
				} else {
					row.ct.Color = theme.Color(theme.ColorNameForeground)
				}
				row.Refresh()
			},
		)
		treeCollapseFix(tree)

		// Ctrl+C copies the full label of the currently selected tree row.
		var selectedNodeID string
		tree.OnSelected = func(id widget.TreeNodeID) { selectedNodeID = id }
		win.Canvas().AddShortcut(&fyne.ShortcutCopy{}, func(_ fyne.Shortcut) {
			if selectedNodeID != "" {
				win.Clipboard().SetContent(model.labelFor(selectedNodeID))
			}
		})

		// Search bar: filters the tree when Search is clicked or Enter is pressed.
		searchEntry := widget.NewEntry()
		searchEntry.SetPlaceHolder("Search tags…")
		doSearch := func() {
			model.setFilter(searchEntry.Text)
			if searchEntry.Text != "" {
				tree.OpenAllBranches()
			}
			tree.Refresh()
		}
		searchEntry.OnSubmitted = func(_ string) { doSearch() }
		searchBar := container.NewBorder(
			nil, nil,
			container.NewHBox(
				widget.NewButton("Expand All", func() { tree.OpenAllBranches() }),
				widget.NewButton("Collapse All", func() { collapseAllTree(tree) }),
			),
			container.NewHBox(
				widget.NewButton("Search", doSearch),
				widget.NewButton("Clear", func() {
					searchEntry.SetText("")
					model.setFilter("")
					tree.Refresh()
				}),
			),
			searchEntry,
		)

		statusBar := container.NewVBox(statusLbl, progress)

		// widget.Tree is itself a scrolling, virtualizing container — it must be
		// placed directly in the layout, not wrapped in container.NewScroll.
		win.SetContent(container.NewBorder(searchBar, statusBar, nil, nil, tree))
		win.Resize(fyne.NewSize(900, 650))

		openTagTreesMu.Lock()
		openTagTrees[win] = tree
		openTagTreesMu.Unlock()
		win.SetOnClosed(func() {
			openTagTreesMu.Lock()
			delete(openTagTrees, win)
			openTagTreesMu.Unlock()
		})

		win.Show()
	})

	// Parse the files concurrently across a worker pool — parsing is the
	// dominant per-file cost and is independent per file. Tree insertion
	// (model.addDataset) is serialised by the model's own mutex, and node
	// identity is key-based, so insertion order does not affect the result.
	// A background ticker refreshes the tree at most once per refreshInterval
	// while the workers run, keeping rows appearing incrementally.
	const refreshInterval = 150 * time.Millisecond
	start := time.Now()

	pathCh := make(chan string, 256)
	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range pathCh {
				ds, perr := parseDICOMTags(p)
				if perr != nil {
					errCount.Add(1)
					continue
				}
				model.addDataset(ds)
				filesLoaded.Add(1)
			}
		}()
	}

	stopRefresh := make(chan struct{})
	var refreshWG sync.WaitGroup
	refreshWG.Add(1)
	go func() {
		defer refreshWG.Done()
		t := time.NewTicker(refreshInterval)
		defer t.Stop()
		for {
			select {
			case <-stopRefresh:
				return
			case <-t.C:
				fyne.Do(func() {
					updateStatus(false, 0)
					tree.Refresh()
				})
			}
		}
	}()

	for _, p := range paths {
		pathCh <- p
	}
	close(pathCh)
	wg.Wait()
	close(stopRefresh)
	refreshWG.Wait()

	elapsed := time.Since(start)
	fyne.Do(func() {
		updateStatus(true, elapsed)
		tree.Refresh()
		progress.Hide()
	})
}
