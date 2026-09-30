package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// filesMissingOnDisk lists each folder once instead of stat-ing each file; it
// must report exactly the files that are gone — deleted from a folder that is
// still there, or in a folder that is gone — and resolve names the way Windows
// does, ignoring case.
func TestFilesMissingOnDisk(t *testing.T) {
	root := t.TempDir()
	series := filepath.Join(root, "PAT", "STUDY", "SERIES")
	if err := os.MkdirAll(series, 0o755); err != nil {
		t.Fatal(err)
	}
	present := filepath.Join(series, "a.dcm")
	deleted := filepath.Join(series, "b.dcm")
	upper := filepath.Join(series, "C.DCM")
	for _, p := range []string{present, upper} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lowerRef := filepath.Join(series, "c.dcm") // same file, as the index might spell it
	goneDir := filepath.Join(root, "PAT", "OTHER", "x.dcm")

	got := filesMissingOnDisk([]string{present, deleted, lowerRef, goneDir})
	slices.Sort(got)
	want := []string{deleted, goneDir}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("missing = %v, want %v", names(got), names(want))
	}
	if got := filesMissingOnDisk(nil); len(got) != 0 {
		t.Errorf("no paths: missing %v", got)
	}
}

func names(paths []string) string {
	var out []string
	for _, p := range paths {
		out = append(out, filepath.Base(filepath.Dir(p))+"/"+filepath.Base(p))
	}
	return strings.Join(out, ", ")
}
