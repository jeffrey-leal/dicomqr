package main

import (
	"slices"
	"strings"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// referenceVisible is the filter as it was before the cached view — each
// child kept when its label or any descendant's contains the filter,
// recomputed per call — kept as the oracle.
func referenceVisible(m *tagTreeModel, id, filter string) []string {
	var matches func(string) bool
	matches = func(id string) bool {
		n := m.nodes[id]
		if strings.Contains(strings.ToLower(n.label), filter) {
			return true
		}
		for _, c := range n.children {
			if matches(c) {
				return true
			}
		}
		return false
	}
	var out []string
	for _, c := range m.nodes[id].children {
		if matches(c) {
			out = append(out, c)
		}
	}
	return out
}

// checkAgainstReference compares childUIDs with the oracle for every node.
func checkAgainstReference(t *testing.T, m *tagTreeModel, filter string) {
	t.Helper()
	for id := range m.nodes {
		got := m.childUIDs(id)
		want := referenceVisible(m, id, filter)
		if !slices.Equal(got, want) {
			t.Errorf("filter %q, node %q (%q): children %v, want %v", filter, id, m.nodes[id].label, got, want)
		}
	}
}

// The cached filtered view must show exactly what the per-call filter did —
// for a leaf value, a nested sequence item, a structural label, mixed case,
// and a filter matching nothing — and follow a change of filter.
func TestTagFilterViewMatchesPerCallFilter(t *testing.T) {
	m := exportTestModel(t)
	for _, f := range []string{"doe", "T-A0100", "ct head", "(0020", "no such text"} {
		m.setFilter(f)
		checkAgainstReference(t, m, strings.ToLower(f))
	}
	m.setFilter("")
	root := m.nodes[""]
	if got := m.childUIDs(""); !slices.Equal(got, root.children) {
		t.Errorf("clearing the filter: root children %v, want all of %v", got, root.children)
	}
}

// Files keep loading while a search is active; the view must pick up what
// arrives rather than keep showing the tree as it was when first built.
func TestTagFilterViewFollowsLoading(t *testing.T) {
	m := exportTestModel(t)
	m.setFilter("second patient")
	if got := m.childUIDs(""); len(got) != 0 {
		t.Fatalf("root shows %v before the matching file loaded", got)
	}
	m.addDataset(sdicom.Dataset{Elements: []*sdicom.Element{
		exportTestElement(t, tag.Tag{Group: 0x0010, Element: 0x0010}, "PN", []string{"SECOND PATIENT"}),
		exportTestElement(t, tag.Tag{Group: 0x0010, Element: 0x0020}, "LO", []string{"MRN2"}),
		exportTestElement(t, tag.Tag{Group: 0x0008, Element: 0x0018}, "UI", []string{"9.9.9.9"}),
	}})
	if got := m.childUIDs(""); len(got) != 1 {
		t.Fatalf("root shows %v after the matching file loaded, want its patient", got)
	}
	checkAgainstReference(t, m, "second patient")
}
