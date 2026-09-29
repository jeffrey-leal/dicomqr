package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// writeLocalMetaFixture writes a DICOM file from the given elements, in the
// order given. The library's writeDataset emits ds.Elements in order and does
// not sort, which is what lets the fallback test below build a file whose tags
// are deliberately out of order.
func writeLocalMetaFixture(t *testing.T, path string, elems []*sdicom.Element) {
	t.Helper()
	meta := []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.5"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{tsExplicitVRLE}),
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, sdicom.Dataset{Elements: append(meta, elems...)},
		sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// conformantMetaElements is a normally-ordered file carrying every tag the scan
// reads, plus enough bulk past group 0020 that stopping early actually matters.
func conformantMetaElements(t *testing.T) []*sdicom.Element {
	t.Helper()
	return []*sdicom.Element{
		mustTestElement(t, tag.StudyDate, []string{"20240102"}),
		mustTestElement(t, tag.AccessionNumber, []string{"ACC42"}),
		mustTestElement(t, tag.Modality, []string{"CT"}),
		mustTestElement(t, tag.ModalitiesInStudy, []string{"CT\\SR"}),
		mustTestElement(t, tag.StudyDescription, []string{"Chest CT"}),
		mustTestElement(t, tag.SeriesDescription, []string{"Axial 1mm"}),
		mustTestElement(t, tag.PatientName, []string{"DOE^JANE"}),
		mustTestElement(t, tag.PatientID, []string{"PID123"}),
		mustTestElement(t, tag.StudyInstanceUID, []string{"1.2.3.4"}),
		mustTestElement(t, tag.SeriesInstanceUID, []string{"1.2.3.4.1"}),
		mustTestElement(t, tag.SeriesNumber, []string{"2"}),
		// Past the stop group — the bytes the streaming scan must not read.
		mustTestElement(t, tag.Rows, []int{64}),
		mustTestElement(t, tag.Columns, []int{64}),
	}
}

// TestLocalFileMetaPathsAgree is the assertion that matters: the streaming scan
// and the full parse must produce the same metadata for a normal file. If they
// ever diverge, a folder scan and a catalog ingest would disagree about what a
// study contains.
func TestLocalFileMetaPathsAgree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conformant.dcm")
	writeLocalMetaFixture(t, path, conformantMetaElements(t))

	streamed, streamOK := scanLocalFileMeta(path)
	full, fullOK := fullLocalFileMeta(path)

	if !streamOK || !fullOK {
		t.Fatalf("ok flags differ or both failed: streaming=%v full=%v", streamOK, fullOK)
	}
	if !reflect.DeepEqual(streamed, full) {
		t.Errorf("paths disagree:\n streaming %+v\n full      %+v", streamed, full)
	}

	// And the values are the ones written, not merely equal to each other.
	if streamed.patientName != "DOE^JANE" || streamed.patientID != "PID123" ||
		streamed.studyUID != "1.2.3.4" || streamed.seriesUID != "1.2.3.4.1" ||
		streamed.modality != "CT" || streamed.seriesNumber != "2" ||
		streamed.studyDesc != "Chest CT" || streamed.seriesDesc != "Axial 1mm" ||
		streamed.studyDate != "20240102" || streamed.accession != "ACC42" {
		t.Errorf("streamed metadata = %+v", streamed)
	}
	if streamed.size <= 0 || streamed.mtime <= 0 {
		t.Errorf("size/mtime not populated: size=%d mtime=%d", streamed.size, streamed.mtime)
	}
}

// TestLocalFileMetaOutOfOrderFallback: a file whose Series Instance UID sits
// after a group-0028 element breaks the ascending-order assumption the streaming
// scan rests on. The streaming path is expected to miss it — that is the whole
// reason the fallback exists — and parseLocalFileMeta must still index the file,
// because a file with no identifiers is dropped from the tree entirely.
func TestLocalFileMetaOutOfOrderFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outoforder.dcm")
	writeLocalMetaFixture(t, path, []*sdicom.Element{
		mustTestElement(t, tag.PatientName, []string{"DOE^JANE"}),
		mustTestElement(t, tag.PatientID, []string{"PID123"}),
		mustTestElement(t, tag.Modality, []string{"CT"}),
		mustTestElement(t, tag.StudyInstanceUID, []string{"1.2.3.4"}),
		mustTestElement(t, tag.Rows, []int{64}),
		mustTestElement(t, tag.Columns, []int{64}),
		// Non-conformant: an identifier after a higher group.
		mustTestElement(t, tag.SeriesInstanceUID, []string{"1.2.3.4.1"}),
	})

	if _, ok := scanLocalFileMeta(path); ok {
		t.Log("streaming scan happened to find the out-of-order tag — the fallback is then untested here")
	}

	m, ok := parseLocalFileMeta(path)
	if !ok {
		t.Fatal("an out-of-order file was dropped — the fallback did not run")
	}
	if m.seriesUID != "1.2.3.4.1" || m.studyUID != "1.2.3.4" {
		t.Errorf("fallback metadata = %+v", m)
	}
}

// TestLocalFileMetaOddVRDoesNotPanic: one of the scanned tags stored as bytes
// rather than a string. The previous lookup used sdicom.MustGetStrings after the
// parse had returned — outside the parse boundary's recover — so this shape
// panicked on whichever goroutine was scanning.
func TestLocalFileMetaOddVRDoesNotPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oddvr.dcm")
	oddValue, err := sdicom.NewValue([]byte{0x01, 0x02})
	if err != nil {
		t.Fatalf("NewValue: %v", err)
	}
	odd := &sdicom.Element{
		Tag:                    tag.SeriesDescription,
		ValueRepresentation:    tag.VRBytes,
		RawValueRepresentation: "OB",
		Value:                  oddValue,
	}
	writeLocalMetaFixture(t, path, []*sdicom.Element{
		mustTestElement(t, tag.PatientName, []string{"DOE^JANE"}),
		mustTestElement(t, tag.StudyInstanceUID, []string{"1.2.3.4"}),
		mustTestElement(t, tag.SeriesInstanceUID, []string{"1.2.3.4.1"}),
		odd,
	})

	m, ok := parseLocalFileMeta(path)
	if !ok {
		t.Fatal("file rejected; it carries both identifiers")
	}
	if m.seriesDesc != "" {
		t.Errorf("seriesDesc = %q, want empty for a non-string value", m.seriesDesc)
	}
}

// The placement rules buildFileMeta carries, unchanged from the original
// implementation.
func TestBuildFileMetaRules(t *testing.T) {
	t.Run("missing study UID is not placeable", func(t *testing.T) {
		_, ok := buildFileMeta("x.dcm", map[tag.Tag]string{
			tag.SeriesInstanceUID: "1.2.3.4.1",
		}, 1, 1)
		if ok {
			t.Error("a file with no Study Instance UID was accepted")
		}
	})
	t.Run("missing series UID is not placeable", func(t *testing.T) {
		_, ok := buildFileMeta("x.dcm", map[tag.Tag]string{
			tag.StudyInstanceUID: "1.2.3.4",
		}, 1, 1)
		if ok {
			t.Error("a file with no Series Instance UID was accepted")
		}
	})
	t.Run("modality falls back to modalities in study", func(t *testing.T) {
		m, ok := buildFileMeta("x.dcm", map[tag.Tag]string{
			tag.StudyInstanceUID:  "1.2.3.4",
			tag.SeriesInstanceUID: "1.2.3.4.1",
			tag.ModalitiesInStudy: "MR",
		}, 1, 1)
		if !ok || m.modality != "MR" {
			t.Errorf("modality = %q, want the ModalitiesInStudy fallback", m.modality)
		}
	})
}

// scanFixtureFolder writes n files spread over two series of one study, so a
// scan has both grouping and ordering to get right.
func scanFixtureFolder(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	for i := range n {
		series := "1.2.3.4.1"
		if i%2 == 1 {
			series = "1.2.3.4.2"
		}
		writeLocalMetaFixture(t, filepath.Join(dir, fmt.Sprintf("f%04d.dcm", i)), []*sdicom.Element{
			mustTestElement(t, tag.PatientName, []string{"DOE^JANE"}),
			mustTestElement(t, tag.PatientID, []string{"PID"}),
			mustTestElement(t, tag.Modality, []string{"CT"}),
			mustTestElement(t, tag.StudyInstanceUID, []string{"1.2.3.4"}),
			mustTestElement(t, tag.SeriesInstanceUID, []string{series}),
		})
	}
	return dir
}

// TestScanLocalFolderIsDeterministic is the load-bearing test for reading files
// in parallel: the worker count must change when the work happens, never what
// comes out of it.
//
// The tree presents files in the order the scan returns them, and a study takes
// its metadata from the first file seen, so a merge that depended on which
// worker finished first would reorder the tree between runs on the same folder.
func TestScanLocalFolderIsDeterministic(t *testing.T) {
	dir := scanFixtureFolder(t, 40)

	scanWith := func(t *testing.T, workers string) ([]localStudy, []localSeries, map[string][]string) {
		t.Helper()
		t.Setenv("DICOMQR_SCAN_WORKERS", workers)
		studies, series, files, err := scanLocalFolder(dir, nil)
		if err != nil {
			t.Fatalf("scan with %s workers: %v", workers, err)
		}
		return studies, series, files
	}

	serialStudies, serialSeries, serialFiles := scanWith(t, "1")
	parStudies, parSeries, parFiles := scanWith(t, "8")

	if !reflect.DeepEqual(serialStudies, parStudies) {
		t.Errorf("studies differ between 1 and 8 workers:\n serial %+v\n par    %+v", serialStudies, parStudies)
	}
	if !reflect.DeepEqual(serialSeries, parSeries) {
		t.Errorf("series differ between 1 and 8 workers:\n serial %+v\n par    %+v", serialSeries, parSeries)
	}
	if !reflect.DeepEqual(serialFiles, parFiles) {
		t.Errorf("file lists differ between 1 and 8 workers")
	}

	// Asserted explicitly rather than left to the deep-equal above: file order
	// inside a series is what the tree shows, and it is the thing a worker pool
	// would most plausibly scramble.
	for uid, list := range parFiles {
		if !sort.StringsAreSorted(list) {
			t.Errorf("series %s files are not in walk order: %v", uid, list)
		}
	}
	if len(parFiles) != 2 {
		t.Errorf("series count = %d, want 2", len(parFiles))
	}
}

// TestScanProgressContract covers the pacing and the shape of progress reports.
//
// The callers hand each report to fyne.Do, whose queue is unbounded and never
// blocks, so the number of reports is the number of repaints the UI goroutine
// must work through before it reaches the callback that populates the tree —
// which is what once buried the tree behind hundreds of queued updates. Pacing
// is by ticker, so it stays tied to how fast a person reads rather than to how
// fast the disk runs.
func TestScanProgressContract(t *testing.T) {
	const files = 40
	dir := scanFixtureFolder(t, files)

	type report struct{ done, total int }
	run := func(t *testing.T, interval time.Duration) []report {
		t.Helper()
		original := scanProgressInterval
		t.Cleanup(func() { scanProgressInterval = original })
		scanProgressInterval = interval

		var mu sync.Mutex
		var got []report
		if _, _, _, err := scanLocalFolder(dir, func(phase scanPhase, done, total int) {
			if phase != scanPhaseRead {
				return // the walk has no total to assert against
			}
			mu.Lock()
			got = append(got, report{done, total})
			mu.Unlock()
		}); err != nil {
			t.Fatalf("scan: %v", err)
		}
		return got
	}

	// An interval longer than the scan can take: the ticker never fires, so the
	// only report is the closing one. On a real folder this is the difference
	// between a handful of updates and several hundred.
	reports := run(t, time.Hour)
	if len(reports) != 1 {
		t.Errorf("reports with a one-hour interval = %d, want only the closing one", len(reports))
	}

	// Whatever the pacing, the contract closes at done == total and every report
	// carries the same total — the count comes from the walk, before any parsing.
	for _, r := range reports {
		if r.total != files {
			t.Errorf("report total = %d, want %d for every report", r.total, files)
		}
	}
	if last := reports[len(reports)-1]; last.done != files {
		t.Errorf("final report done = %d, want %d", last.done, files)
	}

	// A zero interval must not panic — time.NewTicker rejects a non-positive
	// duration, so the reporter has to be skipped rather than started.
	zero := run(t, 0)
	if len(zero) != 1 || zero[0].done != files {
		t.Errorf("reports with a zero interval = %+v, want just the closing one at %d", zero, files)
	}
}

// TestScanWorkers covers the resolver: the override wins when sane, and the
// count never exceeds the work available or the clamp.
func TestScanWorkers(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("DICOMQR_SCAN_WORKERS", "")
		if got := scanWorkers(1000); got != scanDefaultWorkers {
			t.Errorf("workers = %d, want the %d default", got, scanDefaultWorkers)
		}
	})
	t.Run("override", func(t *testing.T) {
		t.Setenv("DICOMQR_SCAN_WORKERS", "3")
		if got := scanWorkers(1000); got != 3 {
			t.Errorf("workers = %d, want the override 3", got)
		}
	})
	t.Run("never more than the files to read", func(t *testing.T) {
		t.Setenv("DICOMQR_SCAN_WORKERS", "32")
		if got := scanWorkers(5); got != 5 {
			t.Errorf("workers = %d, want 5 (one per file)", got)
		}
	})
	t.Run("clamped and never zero", func(t *testing.T) {
		t.Setenv("DICOMQR_SCAN_WORKERS", "9999")
		if got := scanWorkers(1000); got != 64 {
			t.Errorf("workers = %d, want the 64 clamp", got)
		}
		t.Setenv("DICOMQR_SCAN_WORKERS", "garbage")
		if got := scanWorkers(1000); got != scanDefaultWorkers {
			t.Errorf("workers with a malformed override = %d, want the default", got)
		}
	})
}

// writeOneScanFile writes a single indexable file, optionally padded so a
// rewrite changes its size.
func writeOneScanFile(t *testing.T, path, seriesUID string, pad int) {
	t.Helper()
	elems := []*sdicom.Element{
		mustTestElement(t, tag.PatientName, []string{"DOE^JANE"}),
		mustTestElement(t, tag.PatientID, []string{"PID"}),
		mustTestElement(t, tag.Modality, []string{"CT"}),
		mustTestElement(t, tag.StudyInstanceUID, []string{"1.2.3.4"}),
		mustTestElement(t, tag.SeriesInstanceUID, []string{seriesUID}),
	}
	if pad > 0 {
		elems = append(elems, mustTestElement(t, tag.StudyDescription,
			[]string{strings.Repeat("X", pad)}))
	}
	writeLocalMetaFixture(t, path, elems)
}

// scanTestCatalog opens an index in its own directory, separate from the folder
// being scanned so the .db file never turns up in the walk. Wraps the shared
// helper in catalog_test.go, whose second return is that directory.
func scanTestCatalog(t *testing.T) *catalog {
	t.Helper()
	cat, _ := openTestCatalog(t)
	return cat
}

// TestSyncLocalFolderReadsOnlyChanges is the point of the incremental scan: a
// folder nothing has happened to costs a directory walk and no file reads at
// all, which is what turns a re-scan from minutes into seconds on a cold disk.
func TestSyncLocalFolderReadsOnlyChanges(t *testing.T) {
	dir := t.TempDir()
	cat := scanTestCatalog(t)
	for i := range 6 {
		writeOneScanFile(t, filepath.Join(dir, fmt.Sprintf("f%02d.dcm", i)), "1.2.3.4.1", 0)
	}

	// First sync populates the index: everything is new.
	_, _, _, counts, err := syncLocalFolder(cat, dir, nil)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if counts.added != 6 || counts.unchanged != 0 {
		t.Fatalf("first sync counts = %+v, want 6 added", counts)
	}

	// Second sync over an untouched folder must read nothing.
	studies, series, files, counts, err := syncLocalFolder(cat, dir, nil)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if counts.added != 0 || counts.removed != 0 {
		t.Errorf("second sync counts = %+v, want nothing read or removed", counts)
	}
	if counts.unchanged != 6 {
		t.Errorf("unchanged = %d, want all 6 recognised from the index", counts.unchanged)
	}
	// And it still returns the folder's contents, from the index.
	if len(studies) != 1 || len(series) != 1 || len(files["1.2.3.4.1"]) != 6 {
		t.Errorf("hierarchy after an unchanged sync = %d studies, %d series, %d files",
			len(studies), len(series), len(files["1.2.3.4.1"]))
	}
}

// TestSyncLocalFolderTracksChanges: a file added, a file rewritten, and a file
// deleted must each be picked up.
func TestSyncLocalFolderTracksChanges(t *testing.T) {
	dir := t.TempDir()
	cat := scanTestCatalog(t)
	paths := make([]string, 3)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("f%02d.dcm", i))
		writeOneScanFile(t, paths[i], "1.2.3.4.1", 0)
	}
	if _, _, _, c, err := syncLocalFolder(cat, dir, nil); err != nil || c.added != 3 {
		t.Fatalf("seed sync: counts %+v err %v", c, err)
	}

	t.Run("added", func(t *testing.T) {
		writeOneScanFile(t, filepath.Join(dir, "new.dcm"), "1.2.3.4.2", 0)
		_, _, files, c, err := syncLocalFolder(cat, dir, nil)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		if c.added != 1 || c.unchanged != 3 {
			t.Errorf("counts = %+v, want 1 added and 3 unchanged", c)
		}
		if len(files["1.2.3.4.2"]) != 1 {
			t.Errorf("the new file did not reach the tree: %v", files)
		}
	})

	t.Run("changed", func(t *testing.T) {
		// Rewritten to a different length, so the stored size no longer matches
		// whatever the timestamp resolution happens to be.
		writeOneScanFile(t, paths[0], "1.2.3.4.1", 64)
		_, _, _, c, err := syncLocalFolder(cat, dir, nil)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		if c.added != 1 {
			t.Errorf("counts = %+v, want the rewritten file re-read", c)
		}
	})

	t.Run("removed", func(t *testing.T) {
		if err := os.Remove(paths[1]); err != nil {
			t.Fatal(err)
		}
		_, _, files, c, err := syncLocalFolder(cat, dir, nil)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		if c.removed != 1 {
			t.Errorf("counts = %+v, want the deleted file pruned", c)
		}
		for _, list := range files {
			if slices.Contains(list, paths[1]) {
				t.Errorf("a deleted file is still in the tree: %s", paths[1])
			}
		}
	})
}

// TestSyncMatchesFullScan: Scan and Rebuild must agree about what a folder
// contains, or the fast path would quietly disagree with the slow one.
func TestSyncMatchesFullScan(t *testing.T) {
	dir := scanFixtureFolder(t, 20)
	cat := scanTestCatalog(t)

	syncStudies, syncSeries, syncFiles, _, err := syncLocalFolder(cat, dir, nil)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	fullStudies, fullSeries, fullFiles, err := scanLocalFolder(dir, nil)
	if err != nil {
		t.Fatalf("full scan: %v", err)
	}

	if !reflect.DeepEqual(syncStudies, fullStudies) {
		t.Errorf("studies differ:\n sync %+v\n full %+v", syncStudies, fullStudies)
	}
	if !reflect.DeepEqual(syncSeries, fullSeries) {
		t.Errorf("series differ:\n sync %+v\n full %+v", syncSeries, fullSeries)
	}
	if !reflect.DeepEqual(syncFiles, fullFiles) {
		t.Errorf("file lists differ:\n sync %v\n full %v", syncFiles, fullFiles)
	}
}

// TestFilesToRemove covers the rule that decides what a sync prunes — most
// importantly that a walk which could not read part of the tree prunes nothing.
//
// Tested directly rather than through a real permissions failure, which is not
// portably reproducible: the rule is what matters, and getting it wrong would
// strip live files out of the index on the strength of a transient error.
func TestFilesToRemove(t *testing.T) {
	stamps := map[string]fileStamp{
		"a.dcm": {1, 1},
		"b.dcm": {1, 1},
		"c.dcm": {1, 1},
	}

	t.Run("prunes what is genuinely gone", func(t *testing.T) {
		onDisk := map[string]bool{"a.dcm": true, "c.dcm": true}
		got := filesToRemove(stamps, onDisk, false)
		if !reflect.DeepEqual(got, []string{"b.dcm"}) {
			t.Errorf("= %v, want [b.dcm]", got)
		}
	})

	t.Run("prunes nothing when the walk failed", func(t *testing.T) {
		// Same input, but the walk could not read part of the tree — the two
		// missing files are indistinguishable from unreadable ones.
		onDisk := map[string]bool{"a.dcm": true}
		if got := filesToRemove(stamps, onDisk, true); got != nil {
			t.Errorf("= %v, want nothing removed after a failed walk", got)
		}
	})

	t.Run("nothing missing, nothing removed", func(t *testing.T) {
		onDisk := map[string]bool{"a.dcm": true, "b.dcm": true, "c.dcm": true}
		if got := filesToRemove(stamps, onDisk, false); got != nil {
			t.Errorf("= %v, want nothing removed", got)
		}
	})
}

// localMetaMaxGroup must actually cover every tag the scan reads — the property
// that keeps the early exit correct as the tag set changes.
func TestLocalMetaMaxGroupCoversEveryTag(t *testing.T) {
	for tg := range localMetaTags {
		if tg.Group > localMetaMaxGroup {
			t.Errorf("%v is in group %04X, past the %04X the scan stops at",
				tg, tg.Group, localMetaMaxGroup)
		}
	}
}

// scopeFixture is one patient with two studies (three series and one series)
// plus a second patient with one series.
func scopeFixture() *resultsModel {
	m := newResultsModel()
	m.addStudy("Doe^John", "P1", "S1", "20240101", "", "", "")
	m.addSeries("S1", "R1", "CT", "1", "", 0)
	m.addSeries("S1", "R2", "CT", "2", "", 0)
	m.addSeries("S1", "R3", "CT", "3", "", 0)
	m.addStudy("Doe^John", "P1", "S2", "20240201", "", "", "")
	m.addSeries("S2", "R4", "MR", "1", "", 0)
	m.addStudy("Roe^Jane", "P2", "S3", "20240301", "", "", "")
	m.addSeries("S3", "R5", "CT", "1", "", 0)
	return m
}

// TestModificationScope covers how a Local Browse selection maps onto a
// Modification run: a subset of one study's series is a study-level run, a
// selection across studies of one patient is patient-level, and one spanning
// patients is refused.
func TestModificationScope(t *testing.T) {
	m := scopeFixture()
	cases := []struct {
		name       string
		ids        []string
		studyLevel bool
		labelTail  string // suffix the label must carry; "" = exactly the node label
		wantErr    bool
	}{
		{name: "series subset of one study", ids: []string{"R:R1", "R:R3"},
			studyLevel: true, labelTail: " (2 of 3 series selected)"},
		{name: "whole study via its node", ids: []string{"S:S1", "R:R1", "R:R2", "R:R3"},
			studyLevel: true},
		{name: "series across two studies", ids: []string{"R:R1", "R:R4"},
			labelTail: " (selection from 2 studies)"},
		{name: "whole patient", ids: []string{"P:P1", "S:S1", "S:S2", "R:R1", "R:R2", "R:R3", "R:R4"}},
		{name: "two patients", ids: []string{"R:R1", "R:R5"}, wantErr: true},
		{name: "nothing known", ids: []string{"R:gone"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label, studyLevel, err := modificationScope(m, tc.ids)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got label %q", label)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if studyLevel != tc.studyLevel {
				t.Errorf("studyLevel = %v, want %v", studyLevel, tc.studyLevel)
			}
			if !strings.HasSuffix(label, tc.labelTail) {
				t.Errorf("label %q lacks suffix %q", label, tc.labelTail)
			}
			if tc.labelTail == "" && strings.Contains(label, "(") && strings.Contains(label, "selected") {
				t.Errorf("whole-node label %q should not describe a partial selection", label)
			}
		})
	}
}
