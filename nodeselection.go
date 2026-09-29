package main

import "fyne.io/fyne/v2"

// nodeSelection tracks multi-selection over a resultsModel-backed tree,
// keeping ancestor and descendant entries consistent: a branch (patient or
// study) is flagged selected only while every one of its currently-loaded
// children still is, so deselecting one child narrows the selection instead
// of leaving the branch's stale flag in place. Query Results, Local Browse
// and Import each build their own resultsModel/queryRow tree and previously
// carried an independent copy of this logic — only the Query Results copy
// had the ancestor bookkeeping, which is exactly how the other two came to
// let a deselected series' files reappear via its still-flagged parent.
type nodeSelection struct {
	model       *resultsModel
	refreshItem func(id string)
	refreshAll  func()

	nodes  map[string]bool
	anchor string // last click without Shift; shift-click range start

	// Rows whose selected state changed during the current operation, repainted
	// once by flush when it ends — see flush for why never row by row.
	dirty    int
	dirtyID  string // the changed row, when dirty == 1
	dirtyAll bool   // rows may have changed anywhere; repaint the whole tree
}

// markChanged records that id's selected state changed.
func (s *nodeSelection) markChanged(id string) {
	s.dirty++
	s.dirtyID = id
}

// flush repaints what the operation just ending changed: nothing, the one row,
// or the whole tree — never one RefreshItem per changed row.
//
// In Fyne 2.7.3 Tree.RefreshItem is not a cheap single-row repaint: it runs the
// tree's full Layout, which walks every open node, allocates its bookkeeping
// maps and refreshes the canvas. Refreshing row by row therefore cost a full
// tree walk per selected node, so Select All over a few thousand series in an
// expanded (or filtered, which opens every branch) tree was quadratic — tens of
// millions of steps, a multi-second freeze. Two RefreshItems already cost more
// than one Refresh, so anything past a single row takes the full refresh.
func (s *nodeSelection) flush() {
	switch {
	case s.dirtyAll || s.dirty > 1:
		s.refreshAll()
	case s.dirty == 1:
		s.refreshItem(s.dirtyID)
	}
	s.dirty, s.dirtyID, s.dirtyAll = 0, "", false
}

func newNodeSelection(m *resultsModel, refreshItem func(string), refreshAll func()) *nodeSelection {
	return &nodeSelection{
		model:       m,
		refreshItem: refreshItem,
		refreshAll:  refreshAll,
		nodes:       make(map[string]bool),
	}
}

func (s *nodeSelection) Selected(id string) bool { return s.nodes[id] }

// IDs returns every currently selected node id, in no particular order.
func (s *nodeSelection) IDs() []string {
	ids := make([]string, 0, len(s.nodes))
	for id := range s.nodes {
		ids = append(ids, id)
	}
	return ids
}

// selectSubtree and clearSubtree only change the map and record what changed;
// the public operation that called them repaints once, through flush.
func (s *nodeSelection) selectSubtree(id string) {
	if !s.nodes[id] {
		s.nodes[id] = true
		s.markChanged(id)
	}
	for _, child := range s.model.childUIDs(id) {
		s.selectSubtree(child)
	}
}

func (s *nodeSelection) clearSubtree(id string) {
	if s.nodes[id] {
		delete(s.nodes, id)
		s.markChanged(id)
	}
	for _, child := range s.model.childUIDs(id) {
		s.clearSubtree(child)
	}
}

// nodeOrAncestorSelected reports whether id or any of its ancestors is selected.
func (s *nodeSelection) nodeOrAncestorSelected(id string) bool {
	for cur := id; cur != ""; cur = s.model.parentOf(cur) {
		if s.nodes[cur] {
			return true
		}
	}
	return false
}

// MarkChildrenSelected flags every current child of id as selected, without
// recursing further or refreshing each child's row individually — used when
// a study's series load lazily after the study was already selected, so the
// newly-arrived children join the selection the same way selectSubtree's
// fan-out would have covered them had they been present from the start. The
// caller refreshes the study's own row once after every child is added.
func (s *nodeSelection) MarkChildrenSelected(id string) {
	for _, child := range s.model.childUIDs(id) {
		s.nodes[child] = true
	}
}

// Click applies a primary click on id under the standard multi-selection
// convention (Windows Explorer, Visual Studio's Solution Explorer): a plain
// click replaces the selection, Ctrl+click toggles one item, Shift+click
// selects a range in place of the selection, and Ctrl+Shift+click adds a
// range to it. Multiple selection therefore always takes a modifier — a
// plain click never accumulates. Ctrl is fyne.KeyModifierShortcutDefault,
// which is Control on Windows.
//
// One addition to the convention: a plain click on the row that is the whole
// selection deselects it. Explorer has empty space to click for that; a tree
// here usually has none, and a lone selected row could otherwise only be
// dropped with Ctrl+click or the Clear Selection button — field-reported as
// looking broken. Where several rows are selected a plain click still narrows
// to the one clicked, so the deselect only ever happens when the clicked row
// is all there is to lose. The branch arrow is Fyne's own control and never
// reaches here: it opens and closes the branch without touching selection.
func (s *nodeSelection) Click(id string, mods fyne.KeyModifier) {
	ctrl := mods&fyne.KeyModifierShortcutDefault != 0
	shift := mods&fyne.KeyModifierShift != 0
	switch {
	case shift:
		s.ExtendTo(id, ctrl)
	case ctrl:
		s.Toggle(id)
	case s.soleSelection(id):
		s.deselectSole(id)
	default:
		s.Select(id)
	}
}

// soleSelection reports whether id is the whole selection: it is selected, and
// everything else selected sits beneath it — the children its own selection
// fanned out to. A plain click on such a row deselects it (see Click), which is
// what a single selected row otherwise offered no plain-click way out of.
func (s *nodeSelection) soleSelection(id string) bool {
	if !s.nodes[id] {
		return false
	}
	for sel := range s.nodes {
		if !s.isSelfOrAncestor(id, sel) {
			return false
		}
	}
	return true
}

// isSelfOrAncestor reports whether anc is id itself or one of its ancestors.
func (s *nodeSelection) isSelfOrAncestor(anc, id string) bool {
	for cur := id; cur != ""; cur = s.model.parentOf(cur) {
		if cur == anc {
			return true
		}
	}
	return false
}

// deselectSole empties a selection that soleSelection found to be id alone. id
// stays the anchor, as any plain click leaves it, so a Shift+click straight
// afterwards still measures its range from the row just clicked.
func (s *nodeSelection) deselectSole(id string) {
	defer s.flush()
	s.clearSubtree(id)
	s.anchor = id
}

// Select makes id (and its loaded descendants) the whole selection,
// deselecting everything else — a plain click. Clicking a row that is
// selected alongside others keeps it (narrowing to it) rather than toggling it
// off; a plain click on the row that is the whole selection is Click's to
// handle, and deselects it. Also becomes the anchor for a subsequent
// shift-click range selection.
func (s *nodeSelection) Select(id string) {
	defer s.flush()
	if len(s.nodes) > 0 {
		// Previously selected rows may be anywhere in the tree; one full
		// refresh repaints them all as deselected.
		s.dirtyAll = true
	}
	s.nodes = make(map[string]bool)
	s.selectSubtree(id)
	s.anchor = id
}

// Toggle selects or deselects id without touching the rest of the selection
// (Ctrl+click), repairing ancestor flags so a branch is only ever flagged
// while every one of its children still is. Also becomes the anchor for a
// subsequent shift-click range selection.
func (s *nodeSelection) Toggle(id string) {
	defer s.flush()
	s.anchor = id

	// Find the outermost selected ancestor (if any).
	topAncestor := ""
	for anc := s.model.parentOf(id); anc != ""; anc = s.model.parentOf(anc) {
		if s.nodes[anc] {
			topAncestor = anc
		}
	}

	if topAncestor != "" && s.nodes[id] {
		// Node is selected and an ancestor is also selected (node was
		// auto-selected when the parent was chosen). The user wants to
		// deselect just this node: clear it and its loaded descendants,
		// then deselect every ancestor up to and including topAncestor.
		s.clearSubtree(id)
		for anc := s.model.parentOf(id); anc != ""; anc = s.model.parentOf(anc) {
			if s.nodes[anc] {
				delete(s.nodes, anc)
				s.markChanged(anc)
			}
			if anc == topAncestor {
				break
			}
		}
		return
	}

	if topAncestor != "" {
		// Node is unselected but an ancestor is selected. Narrow the
		// selection down to just this node's subtree.
		s.clearSubtree(topAncestor)
		s.selectSubtree(id)
		return
	}

	if s.nodes[id] {
		// Node is selected with no selected ancestors: toggle it off
		// together with all loaded descendants.
		s.clearSubtree(id)
		return
	}

	// Node is unselected with no selected ancestors: select it and all
	// loaded descendants.
	s.selectSubtree(id)
}

// ExtendTo performs a shift-click range selection from the anchor (the last
// click made without Shift) to id, inclusive, when both share a parent — e.g.
// two series in one study. With additive false (Shift+click) the range
// replaces the selection; with additive true (Ctrl+Shift+click) it is added
// to whatever is already selected and nothing outside it is touched. Either
// way the anchor is left in place, so a further shift-click re-draws the
// range from the same starting point rather than starting over.
//
// Falls back to a single-item click — Select, or Toggle when additive — when
// there is no usable anchor: no prior click, the anchor node no longer exists
// (a rescan can drop it — parentOf returns "" for an unknown id, which can
// never match a real parent), or the anchor and id are not siblings. A range
// spanning studies or patients has no unambiguous meaning once branches can
// be collapsed or filtered independently, so this never guesses at one — a
// deliberate departure from the convention, where a range runs across every
// visible row between the two clicks.
func (s *nodeSelection) ExtendTo(id string, additive bool) {
	single := s.Select
	if additive {
		single = s.Toggle
	}
	anchor := s.anchor
	if anchor == "" || anchor == id {
		single(id)
		return
	}
	parent := s.model.parentOf(id)
	if parent == "" || parent != s.model.parentOf(anchor) {
		single(id)
		return
	}
	siblings := s.model.childUIDs(parent)
	ai, bi := indexOfID(siblings, anchor), indexOfID(siblings, id)
	if ai < 0 || bi < 0 {
		single(id)
		return
	}
	if ai > bi {
		ai, bi = bi, ai
	}
	defer s.flush()
	if !additive {
		if len(s.nodes) > 0 {
			s.dirtyAll = true
		}
		s.nodes = make(map[string]bool)
	}
	for _, sib := range siblings[ai : bi+1] {
		s.selectSubtree(sib)
	}
}

func indexOfID(ids []string, id string) int {
	for i, cur := range ids {
		if cur == id {
			return i
		}
	}
	return -1
}

// SelectAll selects every node in roots and its loaded descendants.
func (s *nodeSelection) SelectAll(roots []string) {
	defer s.flush()
	for _, id := range roots {
		s.selectSubtree(id)
	}
	s.anchor = ""
}

// Clear drops the whole selection.
func (s *nodeSelection) Clear() {
	if len(s.nodes) == 0 {
		return
	}
	s.nodes = make(map[string]bool)
	s.anchor = ""
	s.refreshAll()
}

// Prune drops selected ids no longer present in the model, e.g. after a
// reload replaces the tree contents.
func (s *nodeSelection) Prune() {
	for id := range s.nodes {
		if _, ok := s.model.nodes[id]; !ok {
			delete(s.nodes, id)
		}
	}
}

// Paths returns the deduplicated set of file paths for every selected node,
// expanding branches to their current children via filesForNode.
func (s *nodeSelection) Paths(seriesFiles map[string][]string) []string {
	seen := make(map[string]bool)
	var paths []string
	for id := range s.nodes {
		for _, p := range filesForNode(id, s.model, seriesFiles) {
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	return paths
}
