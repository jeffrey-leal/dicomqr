package main

// Tag picker — the "Choose…" dialog beside the Remove tags and Keep tags
// fields of the modification-profile editor. Presents the whole DICOM
// dictionary as a tree of groups, each opening to its tags with a checkbox,
// plus a search box over tag names, keywords and numbers.
//
// One deliberate design constraint: checkboxes are on tags only, never on
// groups. The profile format has no group wildcard (removals are individual
// tags; noprivate is the only wildcard the engine knows), so a group checkbox
// would expand into hundreds of literal entries — group 0018 alone holds 895
// tags — and removing a whole group is nearly always wrong: 0028 carries Rows,
// Columns and BitsAllocated, 0008 carries the SOP UIDs. Bulk selection is
// offered only over an active search, where the user has stated the scope.
//
// Entries used to be merged rather than regenerated, to protect the spellings
// mergeModProfiles compared as literal strings. That comparison now parses the
// reference, so the picker writes every surviving tag canonically; only a line
// that does not parse at all is preserved verbatim, so a typo is reported by
// validation rather than silently deleted.

import (
	"fmt"
	"image/color"
	"sort"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// tagSelectionFromLines resolves each profile tag-list line to the tag it
// names, so the picker can open with the profile's current entries already
// checked. Lines that do not resolve are ignored here and preserved by
// mergeTagSelection.
func tagSelectionFromLines(lines []string) map[tag.Tag]bool {
	selected := make(map[tag.Tag]bool, len(lines))
	for _, line := range lines {
		if t, err := parseTagString(strings.TrimSpace(line)); err == nil {
			selected[t] = true
		}
	}
	return selected
}

// mergeTagSelection reconciles the picker's selection with the lines already in
// the field. A line whose tag is still selected is rewritten canonically and
// keeps its position, a line whose tag was unchecked is dropped, and a line
// that resolves to nothing is kept untouched so a typo is reported by
// validation rather than silently deleted. Newly checked tags are appended in
// dictionary order.
func mergeTagSelection(existing []string, selected map[tag.Tag]bool) []string {
	var out []string
	seen := make(map[tag.Tag]bool, len(selected))
	for _, line := range existing {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		t, err := parseTagString(line)
		if err != nil {
			out = append(out, line) // unresolvable — leave it for validation
			continue
		}
		if !selected[t] {
			continue // unchecked in the picker
		}
		if seen[t] {
			continue // a tag already kept
		}
		seen[t] = true
		out = append(out, formatTagRef(t))
	}

	added := make([]tag.Tag, 0, len(selected))
	for t, on := range selected {
		if on && !seen[t] {
			added = append(added, t)
		}
	}
	sort.Slice(added, func(i, j int) bool {
		if added[i].Group != added[j].Group {
			return added[i].Group < added[j].Group
		}
		return added[i].Element < added[j].Element
	})
	for _, t := range added {
		out = append(out, formatTagRef(t))
	}
	return out
}

// tagPickerModel is the filtered view of the dictionary backing the picker
// tree. Rebuilt whenever the search text or either toggle changes.
type tagPickerModel struct {
	all              []tag.Info
	selected         map[tag.Tag]bool
	filter           string
	hideRetired      bool
	showSelectedOnly bool

	groups  []uint16              // visible groups, in dictionary order
	byGroup map[uint16][]tag.Info // visible tags of each group
	byID    map[widget.TreeNodeID]tag.Info
	// selByGroup counts checked tags per group over the whole dictionary, not
	// just the visible slice, so a collapsed or filtered-out group still
	// reports its share of the selection. Maintained incrementally by
	// setSelected — recounting over 5k entries per row render would not scale.
	selByGroup map[uint16]int
	// inDict is every tag the library knows. A profile may legitimately list
	// tags it does not — the shipped base-deident names three (0010,3020 and
	// two in group 0070) — and those have no row to tick, so the counts say so
	// rather than appearing to lose them.
	inDict map[tag.Tag]bool
}

func newTagPickerModel(selected map[tag.Tag]bool) *tagPickerModel {
	m := &tagPickerModel{
		all:        dictionaryTags(),
		selected:   selected,
		selByGroup: make(map[uint16]int),
	}
	m.inDict = make(map[tag.Tag]bool, len(m.all))
	for _, info := range m.all {
		m.inDict[info.Tag] = true
	}
	for t, on := range selected {
		if on {
			m.selByGroup[t.Group]++
		}
	}
	m.rebuild()
	return m
}

// setSelected checks or unchecks one tag, keeping the per-group tally in step.
func (m *tagPickerModel) setSelected(t tag.Tag, on bool) {
	if m.selected[t] == on {
		return
	}
	m.selected[t] = on
	if on {
		m.selByGroup[t.Group]++
	} else {
		m.selByGroup[t.Group]--
		if m.selByGroup[t.Group] <= 0 {
			delete(m.selByGroup, t.Group)
		}
	}
}

// clearSelection unchecks everything, including any tag not in the dictionary
// (a private tag the user typed by hand, which has no row to untick).
func (m *tagPickerModel) clearSelection() {
	m.selected = map[tag.Tag]bool{}
	m.selByGroup = map[uint16]int{}
}

func groupNodeID(group uint16) string { return fmt.Sprintf("g:%04X", group) }
func tagNodeID(t tag.Tag) string      { return "t:" + formatTagRef(t) }

// rebuild recomputes the visible tree from the current filter and toggle.
// m.all is already in dictionary order, so the derived slices inherit it.
func (m *tagPickerModel) rebuild() {
	query := strings.ToLower(strings.TrimSpace(m.filter))
	m.groups = nil
	m.byGroup = make(map[uint16][]tag.Info)
	m.byID = make(map[widget.TreeNodeID]tag.Info)
	for _, info := range m.all {
		if m.hideRetired && info.Retired {
			continue
		}
		if m.showSelectedOnly && !m.selected[info.Tag] {
			continue
		}
		if !tagMatchesQuery(info, query) {
			continue
		}
		g := info.Tag.Group
		if _, ok := m.byGroup[g]; !ok {
			m.groups = append(m.groups, g)
		}
		m.byGroup[g] = append(m.byGroup[g], info)
		m.byID[tagNodeID(info.Tag)] = info
	}
}

func (m *tagPickerModel) childUIDs(id widget.TreeNodeID) []widget.TreeNodeID {
	if id == "" {
		ids := make([]widget.TreeNodeID, 0, len(m.groups))
		for _, g := range m.groups {
			ids = append(ids, groupNodeID(g))
		}
		return ids
	}
	if !strings.HasPrefix(id, "g:") {
		return nil
	}
	var group uint16
	if _, err := fmt.Sscanf(id[2:], "%04X", &group); err != nil {
		return nil
	}
	infos := m.byGroup[group]
	ids := make([]widget.TreeNodeID, 0, len(infos))
	for _, info := range infos {
		ids = append(ids, tagNodeID(info.Tag))
	}
	return ids
}

// isBranch reports group rows as branches. The virtual root must answer true
// or widget.Tree never walks into it.
func (m *tagPickerModel) isBranch(id widget.TreeNodeID) bool {
	return id == "" || strings.HasPrefix(id, "g:")
}

// visibleCount is how many tags the current filter shows.
func (m *tagPickerModel) visibleCount() int {
	n := 0
	for _, infos := range m.byGroup {
		n += len(infos)
	}
	return n
}

// selectedCount counts checked tags, including any the filter is hiding.
func (m *tagPickerModel) selectedCount() int {
	n := 0
	for _, on := range m.selected {
		if on {
			n++
		}
	}
	return n
}

// selectedUnknown counts checked tags the library's dictionary does not list.
// They stay selected and survive Apply untouched, but no row exists to untick
// them — the count label reports them so the totals add up.
func (m *tagPickerModel) selectedUnknown() int {
	n := 0
	for t, on := range m.selected {
		if on && !m.inDict[t] {
			n++
		}
	}
	return n
}

// showTagPicker opens the picker as a window owned by the editor that asked
// for it. current is the field's text, descriptive names and all; onApply
// receives the merged list of stored references, for the caller to render
// however it displays them.
func showTagPicker(a fyne.App, parent fyne.Window, title, current string,
	onApply func([]string)) {
	existing := strippedTagLines(current)
	m := newTagPickerModel(tagSelectionFromLines(existing))

	countLabel := widget.NewLabel("")
	var tree *widget.Tree
	var selectAllBtn *widget.Button
	refreshCounts := func() {
		text := fmt.Sprintf("%d selected · %d of %d tags shown",
			m.selectedCount(), m.visibleCount(), len(m.all))
		if u := m.selectedUnknown(); u > 0 {
			text += fmt.Sprintf(" · %d selected tag(s) absent from the dictionary, kept as written", u)
		}
		countLabel.SetText(text)
		if selectAllBtn != nil {
			// Bulk selection only over a deliberate search: with no filter
			// this would be "check the entire dictionary".
			if strings.TrimSpace(m.filter) == "" {
				selectAllBtn.SetText("Select all matches")
				selectAllBtn.Disable()
			} else {
				selectAllBtn.SetText(fmt.Sprintf("Select all %d matches", m.visibleCount()))
				selectAllBtn.Enable()
			}
		}
	}

	tree = widget.NewTree(
		func(id widget.TreeNodeID) []widget.TreeNodeID { return m.childUIDs(id) },
		func(id widget.TreeNodeID) bool { return m.isBranch(id) },
		func(branch bool) fyne.CanvasObject {
			if branch {
				return widget.NewLabel("")
			}
			return widget.NewCheck("", nil)
		},
		func(id widget.TreeNodeID, branch bool, o fyne.CanvasObject) {
			if branch {
				lbl, ok := o.(*widget.Label)
				if !ok {
					return
				}
				var group uint16
				if _, err := fmt.Sscanf(strings.TrimPrefix(id, "g:"), "%04X", &group); err != nil {
					return
				}
				lbl.SetText(groupPickerLabel(group, len(m.byGroup[group]), m.selByGroup[group]))
				return
			}
			chk, ok := o.(*widget.Check)
			if !ok {
				return
			}
			info, ok := m.byID[id]
			if !ok {
				return
			}
			// Rows are recycled as the tree scrolls, so the handler must be
			// detached before the state is rebound — SetChecked would fire it
			// and silently toggle whichever tag was previously in this row.
			chk.OnChanged = nil
			chk.Text = tagPickerLabel(info)
			chk.Checked = m.selected[info.Tag]
			chk.Refresh()
			chk.OnChanged = func(v bool) {
				m.setSelected(info.Tag, v)
				refreshCounts()
				// Repaint just this group's header so its "n selected" tally
				// follows; a whole-tree Refresh here would fight the click.
				tree.RefreshItem(groupNodeID(info.Tag.Group))
			}
		},
	)
	treeCollapseFix(tree)

	applyFilter := func() {
		m.rebuild()
		tree.Refresh()
		// A narrowed tree holds only rows the user asked to see, so open it;
		// the unnarrowed dictionary is 5k rows and stays collapsed.
		if strings.TrimSpace(m.filter) == "" && !m.showSelectedOnly {
			collapseAllTree(tree)
		} else {
			tree.OpenAllBranches()
		}
		refreshCounts()
	}

	searchEntry := widget.NewEntry()
	searchEntry.SetPlaceHolder("Search by name, keyword or number — e.g. patient, 0010, InstitutionName")
	searchEntry.OnChanged = func(s string) {
		m.filter = s
		applyFilter()
	}

	retiredCheck := widget.NewCheck("Hide retired", func(v bool) {
		m.hideRetired = v
		applyFilter()
	})
	// Opens showing the profile's current entries rather than a wall of
	// collapsed groups; unticking reveals the whole dictionary to add more.
	// Unchecking a tag in this mode leaves its row in place until the view is
	// rebuilt, so a mis-click can be undone without hunting for the tag again.
	selectedOnlyCheck := widget.NewCheck("Show selected only", func(v bool) {
		m.showSelectedOnly = v
		applyFilter()
	})

	selectAllBtn = widget.NewButton("Select all matches", func() {
		for _, infos := range m.byGroup {
			for _, info := range infos {
				m.setSelected(info.Tag, true)
			}
		}
		tree.Refresh()
		refreshCounts()
	})
	clearBtn := widget.NewButton("Clear selection", func() {
		m.clearSelection()
		// In selected-only mode the tree would otherwise still list the tags
		// just cleared, so rebuild rather than merely repaint.
		applyFilter()
	})

	var win fyne.Window
	cancelBtn := widget.NewButton("Cancel", func() { win.Close() })
	applyBtn := widget.NewButton("Apply", nil)
	applyBtn.Importance = widget.HighImportance

	top := container.NewVBox(
		container.NewBorder(nil, nil, nil,
			container.NewHBox(selectedOnlyCheck, retiredCheck), searchEntry),
		countLabel,
	)
	bottom := container.NewBorder(
		widget.NewSeparator(), nil, nil, nil,
		container.NewPadded(container.NewHBox(
			clearBtn, selectAllBtn, layout.NewSpacer(), cancelBtn, applyBtn)),
	)

	// widget.Tree scrolls and virtualises itself, so it goes straight into the
	// layout; the transparent rectangle supplies the size it would otherwise
	// have to derive from its content. A modest height floor keeps the window
	// shrinkable — the tree takes whatever height it is given.
	minSize := canvas.NewRectangle(color.Transparent)
	minSize.SetMinSize(fyne.NewSize(620, 320))
	content := container.NewStack(minSize,
		container.NewBorder(top, bottom, nil, nil, tree))

	applyBtn.OnTapped = func() {
		merged := mergeTagSelection(existing, m.selected)
		win.Close()
		onApply(merged)
	}

	// Open on the profile's current entries when it has any: the dictionary is
	// 5k rows across 76 collapsed groups, so a selection scattered through it
	// is otherwise invisible until every group is opened by hand.
	if m.selectedCount() > 0 {
		selectedOnlyCheck.SetChecked(true) // fires applyFilter
	} else {
		refreshCounts()
	}

	// Blocking: the picker took a snapshot of the field's text on opening, so
	// editing that field behind it would be overwritten by Apply. No key —
	// blocking the editor already makes a second picker unreachable, and a key
	// shared between two editors would raise the wrong window.
	win = openOwnedWindow(a, windowSpec{
		Title:    title,
		Size:     fyne.NewSize(700, 660),
		Parent:   parent,
		Blocking: true,
	}, func(fyne.Window) fyne.CanvasObject { return content })
}

// tagListField pairs a tag-list entry with the Choose… button that opens the
// picker over it. Used for both Remove tags and Keep tags, in the top-level
// editor and the per-modality sub-editor alike.
func tagListField(a fyne.App, parent fyne.Window, entry *widget.Entry, title string) fyne.CanvasObject {

	btn := widget.NewButton("Choose…", func() {
		showTagPicker(a, parent, title, entry.Text, func(entries []string) {
			entry.SetText(decorateTagList(entries))
		})
	})
	// NewVBox keeps the button at its natural height beside the multi-line
	// entry rather than stretching it down the whole field.
	return container.NewBorder(nil, nil, nil, container.NewVBox(btn), entry)
}
