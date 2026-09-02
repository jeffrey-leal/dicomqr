package main

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
	anchor string // last plain-clicked node; shift-click range start
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

func (s *nodeSelection) selectSubtree(id string) {
	s.nodes[id] = true
	s.refreshItem(id)
	for _, child := range s.model.childUIDs(id) {
		s.selectSubtree(child)
	}
}

func (s *nodeSelection) clearSubtree(id string) {
	if s.nodes[id] {
		delete(s.nodes, id)
		s.refreshItem(id)
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

// Toggle selects or deselects id, repairing ancestor flags so a branch is
// only ever flagged while every one of its children still is. Also becomes
// the anchor for a subsequent shift-click range selection.
func (s *nodeSelection) Toggle(id string) {
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
				s.refreshItem(anc)
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

// ExtendTo performs a shift-click range selection from the last plain-clicked
// node (the anchor) to id, inclusive, when both share a parent — e.g. two
// series in one study. Additive: the range is added to whatever is already
// selected, nothing outside it is touched, and the anchor is left in place so
// a further shift-click re-extends the same range rather than starting over.
//
// Falls back to Toggle when there is no usable anchor: no prior plain click,
// the anchor node no longer exists (a rescan can drop it — parentOf returns
// "" for an unknown id, which can never match a real parent), or the anchor
// and id are not siblings. A range spanning studies or patients has no
// unambiguous meaning once branches can be collapsed or filtered
// independently, so this never guesses at one.
func (s *nodeSelection) ExtendTo(id string) {
	anchor := s.anchor
	if anchor == "" || anchor == id {
		s.Toggle(id)
		return
	}
	parent := s.model.parentOf(id)
	if parent == "" || parent != s.model.parentOf(anchor) {
		s.Toggle(id)
		return
	}
	siblings := s.model.childUIDs(parent)
	ai, bi := indexOfID(siblings, anchor), indexOfID(siblings, id)
	if ai < 0 || bi < 0 {
		s.Toggle(id)
		return
	}
	if ai > bi {
		ai, bi = bi, ai
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
