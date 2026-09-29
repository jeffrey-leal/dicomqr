package main

import (
	"os"
	"path/filepath"
	"testing"
)

func testStudies() []localStudy {
	return []localStudy{
		{patientName: "DOE^JANE", patientID: "MRN1", studyUID: "1.2.3", studyDate: "20260101",
			studyDesc: "CT CHEST", accession: "ACC1", modalities: "CT"},
		{patientName: "ROE^JOHN", patientID: "MRN2", studyUID: "4.5.6", studyDate: "20260202",
			studyDesc: "PET WB", accession: "ACC2", modalities: "PT"},
	}
}

func testSeries() []localSeries {
	return []localSeries{
		{studyUID: "1.2.3", seriesUID: "1.2.3.1", modality: "CT", seriesNumber: "1", seriesDesc: "AXIAL", numInstances: 2},
		{studyUID: "1.2.3", seriesUID: "1.2.3.2", modality: "CT", seriesNumber: "2", seriesDesc: "SCOUT", numInstances: 1},
		{studyUID: "4.5.6", seriesUID: "4.5.6.1", modality: "PT", seriesNumber: "1", seriesDesc: "WB", numInstances: 1},
	}
}

func testFiles(dir string) map[string][]string {
	return map[string][]string{
		"1.2.3.1": {filepath.Join(dir, "a1.dcm"), filepath.Join(dir, "a2.dcm")},
		"1.2.3.2": {filepath.Join(dir, "b1.dcm")},
		"4.5.6.1": {filepath.Join(dir, "c1.dcm")},
	}
}

func openTestCatalog(t *testing.T) (*catalog, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := openCatalog(dir)
	if err != nil {
		t.Fatalf("openCatalog: %v", err)
	}
	t.Cleanup(c.Close)
	return c, dir
}

func TestCatalogRoundTrip(t *testing.T) {
	c, dir := openTestCatalog(t)

	if err := c.replaceAll(testStudies(), testSeries(), testFiles(dir)); err != nil {
		t.Fatalf("replaceAll: %v", err)
	}

	studies, series, files, err := c.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(studies) != 2 {
		t.Fatalf("got %d studies, want 2", len(studies))
	}
	if len(series) != 3 {
		t.Fatalf("got %d series, want 3", len(series))
	}
	byUID := make(map[string]localStudy)
	for _, s := range studies {
		byUID[s.studyUID] = s
	}
	s := byUID["1.2.3"]
	if s.patientName != "DOE^JANE" || s.patientID != "MRN1" || s.studyDesc != "CT CHEST" ||
		s.studyDate != "20260101" || s.accession != "ACC1" || s.modalities != "CT" {
		t.Errorf("study 1.2.3 fields mismatch: %+v", s)
	}
	for _, sr := range series {
		want := map[string]int{"1.2.3.1": 2, "1.2.3.2": 1, "4.5.6.1": 1}[sr.seriesUID]
		if sr.numInstances != want {
			t.Errorf("series %s: got %d instances, want %d", sr.seriesUID, sr.numInstances, want)
		}
	}
	if got := len(files["1.2.3.1"]); got != 2 {
		t.Errorf("series 1.2.3.1: got %d paths, want 2", got)
	}
}

func TestCatalogReplaceAllWipesPrevious(t *testing.T) {
	c, dir := openTestCatalog(t)

	if err := c.replaceAll(testStudies(), testSeries(), testFiles(dir)); err != nil {
		t.Fatalf("replaceAll: %v", err)
	}
	// Second scan found only one study.
	if err := c.replaceAll(testStudies()[:1], testSeries()[:1],
		map[string][]string{"1.2.3.1": {filepath.Join(dir, "a1.dcm")}}); err != nil {
		t.Fatalf("replaceAll(2): %v", err)
	}

	studies, series, files, err := c.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(studies) != 1 || len(series) != 1 || len(files["1.2.3.1"]) != 1 {
		t.Fatalf("got %d studies, %d series, %d paths — want 1, 1, 1",
			len(studies), len(series), len(files["1.2.3.1"]))
	}
}

func TestCatalogRemovePathsPrunesEmptyParents(t *testing.T) {
	c, dir := openTestCatalog(t)
	files := testFiles(dir)

	if err := c.replaceAll(testStudies(), testSeries(), files); err != nil {
		t.Fatalf("replaceAll: %v", err)
	}

	// Remove one of two instances in series 1.2.3.1: series must survive.
	if n := c.removePaths(files["1.2.3.1"][:1]); n != 1 {
		t.Fatalf("removePaths: got %d removed, want 1", n)
	}
	studies, series, _, _ := c.load()
	if len(studies) != 2 || len(series) != 3 {
		t.Fatalf("after partial removal: got %d studies, %d series — want 2, 3", len(studies), len(series))
	}

	// Remove the rest of patient MRN1's files: both series, the study, and the
	// patient must be pruned.
	c.removePaths(append(files["1.2.3.1"][1:], files["1.2.3.2"]...))
	studies, series, paths, _ := c.load()
	if len(studies) != 1 || studies[0].studyUID != "4.5.6" {
		t.Fatalf("after full removal: studies = %+v, want only 4.5.6", studies)
	}
	if len(series) != 1 || series[0].seriesUID != "4.5.6.1" {
		t.Fatalf("after full removal: series = %+v, want only 4.5.6.1", series)
	}
	if len(paths) != 1 {
		t.Fatalf("after full removal: got %d path groups, want 1", len(paths))
	}

	// Removing a path that is not indexed is a no-op.
	if n := c.removePaths([]string{filepath.Join(dir, "nope.dcm")}); n != 0 {
		t.Fatalf("removePaths(unknown): got %d, want 0", n)
	}
}

func TestCatalogUpsertMeta(t *testing.T) {
	c, dir := openTestCatalog(t)

	m := fileMeta{
		path:        filepath.Join(dir, "x1.dcm"),
		patientName: "DOE^JANE", patientID: "MRN1",
		studyUID: "1.2.3", studyDate: "20260101", studyDesc: "CT CHEST",
		seriesUID: "1.2.3.1", modality: "CT", seriesNumber: "1", seriesDesc: "AXIAL",
	}
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	u, err := newMetaUpserter(tx)
	if err != nil {
		t.Fatalf("newMetaUpserter: %v", err)
	}
	if err := u.upsert(m); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Upserting the same file again must not duplicate anything.
	if err := u.upsert(m); err != nil {
		t.Fatalf("upsert(again): %v", err)
	}
	u.close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	studies, series, files, err := c.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(studies) != 1 || len(series) != 1 || len(files["1.2.3.1"]) != 1 {
		t.Fatalf("got %d studies, %d series, %d paths — want 1, 1, 1",
			len(studies), len(series), len(files["1.2.3.1"]))
	}
	if series[0].numInstances != 1 {
		t.Fatalf("numInstances = %d, want 1", series[0].numInstances)
	}
}

// The upserter skips parent rows it has already written in the batch. That
// must be invisible: the first file's patient, study and series metadata still
// wins — within one batch, and against a later batch — and every file of the
// series is still indexed.
func TestCatalogUpsertMetasFirstWriteWins(t *testing.T) {
	c, dir := openTestCatalog(t)
	file := func(name, studyDesc, seriesDesc string) fileMeta {
		return fileMeta{
			path: filepath.Join(dir, name), patientName: "DOE^JANE", patientID: "MRN1",
			studyUID: "1.2.3", studyDesc: studyDesc,
			seriesUID: "1.2.3.1", modality: "CT", seriesNumber: "1", seriesDesc: seriesDesc,
		}
	}
	if n := c.upsertMetas([]fileMeta{
		file("a.dcm", "FIRST STUDY", "FIRST SERIES"),
		file("b.dcm", "SECOND STUDY", "SECOND SERIES"),
		file("c.dcm", "THIRD STUDY", "THIRD SERIES"),
	}); n != 3 {
		t.Fatalf("first batch wrote %d rows, want 3", n)
	}
	if n := c.upsertMetas([]fileMeta{file("d.dcm", "LATER STUDY", "LATER SERIES")}); n != 1 {
		t.Fatalf("second batch wrote %d rows, want 1", n)
	}

	studies, series, files, err := c.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(studies) != 1 || studies[0].studyDesc != "FIRST STUDY" {
		t.Errorf("studies = %+v, want one, described by the first file", studies)
	}
	if len(series) != 1 || series[0].seriesDesc != "FIRST SERIES" {
		t.Errorf("series = %+v, want one, described by the first file", series)
	}
	if got := len(files["1.2.3.1"]); got != 4 {
		t.Errorf("series holds %d files, want all 4", got)
	}
}

func TestCatalogPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	c, err := openCatalog(dir)
	if err != nil {
		t.Fatalf("openCatalog: %v", err)
	}
	if err := c.replaceAll(testStudies(), testSeries(), testFiles(dir)); err != nil {
		t.Fatalf("replaceAll: %v", err)
	}
	c.Close()

	c2, err := openCatalog(dir)
	if err != nil {
		t.Fatalf("openCatalog(2): %v", err)
	}
	defer c2.Close()
	studies, series, _, err := c2.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(studies) != 2 || len(series) != 3 {
		t.Fatalf("after reopen: got %d studies, %d series — want 2, 3", len(studies), len(series))
	}
}

func TestCatalogReopenSwitchesDirectory(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	c, err := openCatalog(dirA)
	if err != nil {
		t.Fatalf("openCatalog: %v", err)
	}
	defer c.Close()
	if err := c.replaceAll(testStudies(), testSeries(), testFiles(dirA)); err != nil {
		t.Fatalf("replaceAll: %v", err)
	}

	if err := c.Reopen(dirB); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if c.Dir() != dirB {
		t.Fatalf("Dir() = %q, want %q", c.Dir(), dirB)
	}
	studies, _, _, err := c.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(studies) != 0 {
		t.Fatalf("fresh directory index has %d studies, want 0", len(studies))
	}
	if _, err := os.Stat(filepath.Join(dirB, catalogFileName)); err != nil {
		t.Fatalf("index file not created in new directory: %v", err)
	}
}

func TestCatalogNilSafety(t *testing.T) {
	var c *catalog
	c.Close()
	c.Reopen(t.TempDir())
	if c.Dir() != "" {
		t.Error("nil catalog Dir() should be empty")
	}
	if err := c.replaceAll(nil, nil, nil); err != nil {
		t.Errorf("nil replaceAll: %v", err)
	}
	if n := c.ingestPaths([]string{"x"}); n != 0 {
		t.Errorf("nil ingestPaths: %d", n)
	}
	if n := c.removePaths([]string{"x"}); n != 0 {
		t.Errorf("nil removePaths: %d", n)
	}
	if _, _, _, err := c.load(); err != nil {
		t.Errorf("nil load: %v", err)
	}
	if stamps, err := c.fileStamps(); err != nil || len(stamps) != 0 {
		t.Errorf("nil fileStamps: %v %v", stamps, err)
	}
	if n := c.upsertMetas([]fileMeta{{path: "x"}}); n != 0 {
		t.Errorf("nil upsertMetas: %d", n)
	}
}

// TestCatalogStampsRoundTrip covers the two columns the incremental scan rests
// on. They have been written since the index was added and never read back, so
// this is the first thing that depends on them being right.
func TestCatalogStampsRoundTrip(t *testing.T) {
	c, _ := openTestCatalog(t)

	metas := []fileMeta{
		{path: `C:\dl\a.dcm`, studyUID: "1.2", seriesUID: "1.2.1", size: 111, mtime: 222},
		{path: `C:\dl\b.dcm`, studyUID: "1.2", seriesUID: "1.2.1", size: 333, mtime: 444},
	}
	if n := c.upsertMetas(metas); n != len(metas) {
		t.Fatalf("upsertMetas wrote %d rows, want %d", n, len(metas))
	}

	stamps, err := c.fileStamps()
	if err != nil {
		t.Fatalf("fileStamps: %v", err)
	}
	if len(stamps) != len(metas) {
		t.Fatalf("stamps = %d, want %d", len(stamps), len(metas))
	}
	for _, m := range metas {
		st, ok := stamps[m.path]
		if !ok {
			t.Errorf("%s missing from the stamps", m.path)
			continue
		}
		if st.size != m.size || st.mtime != m.mtime {
			t.Errorf("%s stamp = %+v, want size %d mtime %d", m.path, st, m.size, m.mtime)
		}
	}

	// A re-upsert with a new stamp replaces rather than duplicates — this is
	// what a changed file does on the next scan.
	metas[0].size = 999
	if n := c.upsertMetas(metas[:1]); n != 1 {
		t.Fatalf("re-upsert wrote %d rows, want 1", n)
	}
	stamps, err = c.fileStamps()
	if err != nil {
		t.Fatalf("fileStamps after re-upsert: %v", err)
	}
	if len(stamps) != len(metas) {
		t.Errorf("stamps = %d after a re-upsert, want %d — the row was duplicated", len(stamps), len(metas))
	}
	if stamps[metas[0].path].size != 999 {
		t.Errorf("size = %d, want the updated 999", stamps[metas[0].path].size)
	}
}
