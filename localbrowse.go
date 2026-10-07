package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

type localStudy struct {
	patientName string
	patientID   string
	studyUID    string
	studyDate   string
	studyDesc   string
	accession   string
	modalities  string
}

type localSeries struct {
	studyUID     string
	seriesUID    string
	modality     string
	seriesNumber string
	seriesDesc   string
	numInstances int
}

// fileMeta is the metadata of one DICOM file needed to place it in the
// Patient → Study → Series hierarchy (tree model and SQLite catalog).
type fileMeta struct {
	path  string
	size  int64
	mtime int64

	patientName string
	patientID   string

	studyUID   string
	studyDate  string
	studyDesc  string
	accession  string
	modalities string

	seriesUID    string
	modality     string
	seriesNumber string
	seriesDesc   string
}

// localMetaTags are the identifying values the folder scan and the catalog
// index a file by — the only reason either one opens it. Named once here
// because both parse paths below need the same set.
var localMetaTags = map[tag.Tag]bool{
	tag.PatientName:       true,
	tag.PatientID:         true,
	tag.StudyInstanceUID:  true,
	tag.StudyDate:         true,
	tag.StudyDescription:  true,
	tag.AccessionNumber:   true,
	tag.ModalitiesInStudy: true,
	tag.SeriesInstanceUID: true,
	tag.Modality:          true,
	tag.SeriesNumber:      true,
	tag.SeriesDescription: true,
}

// localMetaMaxGroup is the highest group any of those tags lives in, and so the
// point past which scanLocalFileMeta stops reading.
//
// Derived rather than written as 0x0020 on purpose. It is the one way this
// optimisation could rot: a tag added to the set above in a higher group would
// otherwise sit beyond where the scan stops looking and read as absent from
// every file, silently. Deriving it extends the scan instead.
var localMetaMaxGroup = func() uint16 {
	var max uint16
	for t := range localMetaTags {
		if t.Group > max {
			max = t.Group
		}
	}
	return max
}()

// parseLocalFileMeta reads one DICOM file's hierarchy metadata. ok is false when
// the file is unparsable or carries no Study/Series Instance UID.
//
// The streaming path is tried first and answers from the first few kilobytes.
// The full parse behind it exists for files that do not order their tags as the
// standard requires — see scanLocalFileMeta and fullLocalFileMeta.
func parseLocalFileMeta(path string) (fileMeta, bool) {
	if m, ok := scanLocalFileMeta(path); ok {
		return m, true
	}
	return fullLocalFileMeta(path)
}

// scanLocalFileMeta reads only as far as the identifying tags, stopping once the
// stream is past localMetaMaxGroup — the technique scpParseMetadata uses on the
// receive path.
//
// This is what keeps a folder scan proportional to the number of files rather
// than to their size. A full parse cannot do that even with SkipPixelData: the
// library's Reader.Skip is io.CopyN(io.Discard, …), not a seek, so it still
// pulls every pixel byte off the disk to throw it away. Measured over 1,500
// files totalling 13 GB, this reads 6.4 MB instead of all of it and finishes in
// 649 ms instead of 12.4 s.
func scanLocalFileMeta(path string) (fileMeta, bool) {
	// This path uses NewParser rather than safeParseFile, so it carries the
	// deferrable form of the parse-panic boundary itself (see dicomsafe.go).
	defer recoverParserPanic(path)

	f, err := os.Open(path)
	if err != nil {
		return fileMeta{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fileMeta{}, false
	}
	p, err := sdicom.NewParser(f, info.Size(), nil, sdicom.SkipPixelData())
	if err != nil {
		return fileMeta{}, false
	}

	vals := make(map[tag.Tag]string, len(localMetaTags))
	for {
		elem, err := p.Next()
		if err != nil {
			break
		}
		if elem.Tag.Group > localMetaMaxGroup {
			break
		}
		if localMetaTags[elem.Tag] {
			vals[elem.Tag] = firstElementString(elem)
		}
	}
	return buildFileMeta(path, vals, info.Size(), info.ModTime().Unix())
}

// fullLocalFileMeta parses the whole file and looks the tags up by name — the
// original implementation, kept as the fallback for a file whose tags are not in
// ascending order.
//
// The standard requires that order and every file measured so far honours it,
// but a file that does not would lose an identifier under the streaming scan,
// and a file with no identifiers is dropped from the tree entirely. Falling back
// costs nothing on a conformant file (it never runs) and only ever re-reads one
// that would otherwise have gone missing.
func fullLocalFileMeta(path string) (fileMeta, bool) {
	ds, parseErr := safeParseFile(path, nil, sdicom.SkipPixelData())
	if parseErr != nil {
		return fileMeta{}, false
	}
	vals := make(map[tag.Tag]string, len(localMetaTags))
	for t := range localMetaTags {
		if e, findErr := ds.FindElementByTag(t); findErr == nil {
			vals[t] = firstElementString(e)
		}
	}
	var size, mtime int64
	if info, statErr := os.Stat(path); statErr == nil {
		size, mtime = info.Size(), info.ModTime().Unix()
	}
	return buildFileMeta(path, vals, size, mtime)
}

// firstElementString reads an element's first string value, trimmed, or "" when
// it holds something else.
//
// A checked assertion rather than sdicom.MustGetStrings, which panics: the old
// lookup called it after the parse had returned, so it sat outside the parse
// boundary's recover and a file storing one of these tags under an unexpected VR
// would take down whichever goroutine was scanning.
func firstElementString(e *sdicom.Element) string {
	if e == nil || e.Value == nil {
		return ""
	}
	strs, ok := e.Value.GetValue().([]string)
	if !ok || len(strs) == 0 {
		return ""
	}
	return strings.TrimSpace(strs[0])
}

// buildFileMeta assembles the collected values, applying the rules both parse
// paths share: a file with no Study or Series Instance UID cannot be placed in
// the tree, and a missing Modality falls back to Modalities in Study.
func buildFileMeta(path string, vals map[tag.Tag]string, size, mtime int64) (fileMeta, bool) {
	m := fileMeta{
		path:         path,
		patientName:  vals[tag.PatientName],
		patientID:    vals[tag.PatientID],
		studyUID:     vals[tag.StudyInstanceUID],
		studyDate:    vals[tag.StudyDate],
		studyDesc:    vals[tag.StudyDescription],
		accession:    vals[tag.AccessionNumber],
		modalities:   vals[tag.ModalitiesInStudy],
		seriesUID:    vals[tag.SeriesInstanceUID],
		modality:     vals[tag.Modality],
		seriesNumber: vals[tag.SeriesNumber],
		seriesDesc:   vals[tag.SeriesDescription],
		size:         size,
		mtime:        mtime,
	}
	if m.studyUID == "" || m.seriesUID == "" {
		return fileMeta{}, false
	}
	if m.modality == "" {
		m.modality = m.modalities
	}
	return m, true
}

// scanProgressInterval is how often a scan may report progress.
//
// Paced by time rather than by a file count, because the callers hand the
// report to fyne.Do and Fyne's queue is unbounded and never blocks: a
// per-N-files trigger emits at whatever rate the scan happens to run at, and
// the UI goroutine has to drain every one of them. That was survivable while a
// scan of 13,000 files took 14 seconds; once the scan dropped to under a
// second the same 500-odd updates arrived in a burst, each one re-texting a
// label whose width changes — which on Windows repaints the whole window frame
// (see stableMin) — and the callback that actually populates the tree sat in
// the queue behind all of them. The tree looked like it took seconds to appear
// when it took no time at all.
//
// Ten a second is faster than anyone reads and slow enough to cost nothing. A
// var so tests can drive the throttle without needing thousands of files.
var scanProgressInterval = 100 * time.Millisecond

// scanDefaultWorkers is how many files a scan reads at once.
//
// A cold scan is bound by seek latency, not by bandwidth or by CPU: measured on
// a RAID-0 array of spinning disks, 35,489 files took 5½ minutes — 9.3 ms per
// file, one mechanical seek each — while the same scan warm took 10 seconds.
// After the early-exit change the scan reads only a few KB per file, so those
// minutes are almost entirely waiting for the head to arrive. Issuing several
// reads at once lets the drive (and, on an array, several spindles) overlap that
// waiting.
//
// Eight rather than the CPU count, which is what the modification engine and the
// mask scan key on: those pools are bound by compute and memory, where exceeding
// the core count buys nothing. These workers are asleep on the disk, so the right
// quantity is roughly the queue depth the storage can keep busy, and the CPU
// count is not that number.
//
// Deliberately provisional. Cold conditions are not reproducible on a machine
// with enough RAM to cache the corpus after one read, so this default is reasoned
// rather than measured; DICOMQR_SCAN_WORKERS is how it gets settled against real
// storage.
const scanDefaultWorkers = 8

// scanWorkers resolves the worker count for a scan of n files.
func scanWorkers(n int) int {
	w := scanDefaultWorkers
	if v := os.Getenv("DICOMQR_SCAN_WORKERS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			w = parsed
		}
	}
	return max(1, min(w, 64, n))
}

// scanResult is one file's parse, kept at its position in the walk order.
type scanResult struct {
	path string
	m    fileMeta
	ok   bool
}

// scanPhase names what a scan is doing, so the status line can distinguish the
// two very different halves. On a folder whose files have not changed the walk
// *is* the operation — every file is skipped — and a scan that says nothing
// while it runs is the one that looks like it never started.
type scanPhase int

const (
	// scanPhaseWalk is enumerating the folder; the total is not yet known, so
	// progress reports it as 0.
	scanPhaseWalk scanPhase = iota
	// scanPhaseRead is parsing files, with a known total.
	scanPhaseRead
)

// scanEntry is one file as the walk found it — the path plus the stamp an
// incremental scan compares against the index.
type scanEntry struct {
	path  string
	size  int64
	mtime int64
}

// walkDicomFiles enumerates the .dcm files under dir.
//
// WalkDir rather than Walk: Walk calls Lstat on every entry, while WalkDir
// hands back a DirEntry whose Info on Windows returns the data the directory
// enumeration already produced (os.dirEntry.Info returns a cached *fileStat, no
// syscall). So size and mtime — exactly what an incremental scan needs to
// compare — arrive for free, and a stat per file goes away.
//
// failed counts entries the walk could not read. A single unreadable subtree
// must not fail a scan, but the count matters to the caller: files under it are
// absent from the result and are indistinguishable from files that have been
// deleted, so nothing may be pruned on the strength of a walk that hit errors.
func walkDicomFiles(dir string, progress func(phase scanPhase, done, total int)) (entries []scanEntry, failed int, err error) {
	var lastReport time.Time
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			failed++
			return nil
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".dcm") {
			return nil
		}
		e := scanEntry{path: path}
		if info, infoErr := d.Info(); infoErr == nil {
			e.size, e.mtime = info.Size(), info.ModTime().Unix()
		} else {
			failed++
		}
		entries = append(entries, e)

		// Paced here rather than by a ticker: the walk has no workers to count
		// for it, and the count is the only thing moving.
		if progress != nil && scanProgressInterval > 0 && time.Since(lastReport) >= scanProgressInterval {
			lastReport = time.Now()
			progress(scanPhaseWalk, len(entries), 0)
		}
		return nil
	})
	return entries, failed, err
}

// parseFilesParallel reads each path's metadata on a worker pool and returns
// the results in the order the paths were given.
//
// The ordering is the point. First-write-wins study and series metadata, and
// the file order inside a series, are both what the tree presents, so they must
// not depend on which worker happened to finish first: parallelism changes when
// the reading happens, never what comes out of it. Each worker writes its own
// index, so the slice needs no lock and the caller can merge in order.
func parseFilesParallel(paths []string, progress func(phase scanPhase, done, total int)) []scanResult {
	results := make([]scanResult, len(paths))
	total := len(paths)
	if total == 0 {
		return results
	}
	var done atomic.Int64

	// One reporter goroutine publishes while the workers only count — the same
	// split clipBuffer uses. Posting from every worker would put an update in
	// flight per file, which is what previously buried the tree's own callback
	// behind hundreds of queued repaints.
	var publish func()
	if progress != nil {
		publish = func() { progress(scanPhaseRead, int(done.Load()), total) }
	}
	stopReporting := startPacedProgress(scanProgressInterval, publish)

	var next atomic.Int64
	var wg sync.WaitGroup
	for range scanWorkers(total) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= total {
					return
				}
				m, ok := parseLocalFileMeta(paths[i])
				results[i] = scanResult{path: paths[i], m: m, ok: ok}
				done.Add(1)
			}
		}()
	}
	wg.Wait()

	stopReporting()
	// The ticker can miss the end, so close the contract explicitly: the last
	// thing a caller sees is done == total.
	if progress != nil {
		progress(scanPhaseRead, total, total)
	}
	return results
}

// mergeScanResults assembles parsed files into the tree's shapes, in the order
// given — see parseFilesParallel on why the order is load-bearing.
func mergeScanResults(results []scanResult) ([]localStudy, []localSeries, map[string][]string) {
	type seriesKey struct{ studyUID, seriesUID string }
	studyMap := make(map[string]localStudy)
	seriesMap := make(map[seriesKey]*localSeries)
	filesByUID := make(map[string][]string) // seriesUID → paths

	for _, r := range results {
		if !r.ok {
			continue
		}
		m := r.m

		if _, exists := studyMap[m.studyUID]; !exists {
			studyMap[m.studyUID] = localStudy{
				patientName: m.patientName,
				patientID:   m.patientID,
				studyUID:    m.studyUID,
				studyDate:   m.studyDate,
				studyDesc:   m.studyDesc,
				accession:   m.accession,
				modalities:  m.modalities,
			}
		}

		k := seriesKey{m.studyUID, m.seriesUID}
		if sr, exists := seriesMap[k]; exists {
			sr.numInstances++
		} else {
			seriesMap[k] = &localSeries{
				studyUID:     m.studyUID,
				seriesUID:    m.seriesUID,
				modality:     m.modality,
				seriesNumber: m.seriesNumber,
				seriesDesc:   m.seriesDesc,
				numInstances: 1,
			}
		}

		filesByUID[m.seriesUID] = append(filesByUID[m.seriesUID], r.path)
	}

	var studies []localStudy
	for _, s := range studyMap {
		studies = append(studies, s)
	}
	var series []localSeries
	for _, sr := range seriesMap {
		series = append(series, *sr)
	}
	sortScanHierarchy(studies, series)
	return studies, series, filesByUID
}

// sortScanHierarchy orders the returned slices by UID.
//
// They come out of maps, whose iteration order Go randomises, so two scans of
// an unchanged folder returned the same studies in a different order — before
// any of this was parallel. Harmless in the two places they go (the tree
// re-sorts on insert by its own key, and the catalog does not care), but a
// function whose output order varies run to run is a trap for the next caller,
// and ordering a hundred series costs nothing. By UID rather than by anything
// displayed, so the order is stable whatever the metadata says.
func sortScanHierarchy(studies []localStudy, series []localSeries) {
	sort.Slice(studies, func(i, j int) bool { return studies[i].studyUID < studies[j].studyUID })
	sort.Slice(series, func(i, j int) bool {
		if series[i].studyUID != series[j].studyUID {
			return series[i].studyUID < series[j].studyUID
		}
		return series[i].seriesUID < series[j].seriesUID
	})
}

// scanLocalFolder walks dir and returns studies, series, and a map of
// seriesUID → file paths for every .dcm file found, reading every file.
//
// This is the full scan: the Import tab's source folder has no index to compare
// against, and Rebuild uses it deliberately. Local Browse's Scan goes through
// syncLocalFolder instead, which reads only what changed.
func scanLocalFolder(dir string, progress func(phase scanPhase, done, total int)) ([]localStudy, []localSeries, map[string][]string, error) {
	entries, _, err := walkDicomFiles(dir, progress)
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.path
	}
	studies, series, filesByUID := mergeScanResults(parseFilesParallel(paths, progress))
	return studies, series, filesByUID, err
}

// filesToRemove returns the indexed paths that are no longer on disk.
//
// walkFailed is decisive: when the walk could not read part of the tree, the
// files under it are missing from onDisk and look exactly like deleted ones.
// Pruning then would strip live files out of the index on the strength of a
// permissions error or a disconnected mount, so nothing is removed at all. An
// index that is briefly stale is recoverable; one that has silently dropped a
// study is not obviously wrong until someone goes looking for it.
func filesToRemove(stamps map[string]fileStamp, onDisk map[string]bool, walkFailed bool) []string {
	if walkFailed {
		return nil
	}
	var gone []string
	for path := range stamps {
		if !onDisk[path] {
			gone = append(gone, path)
		}
	}
	sort.Strings(gone)
	return gone
}

// syncCounts reports what a sync changed, which is the interesting part of its
// result — "nothing moved" is a different outcome from "43 files added".
type syncCounts struct{ added, removed, unchanged int }

// syncLocalFolder brings the index into line with the folder and returns the
// hierarchy, reading only files that are new or whose size or modification time
// no longer matches what was indexed.
//
// This is what makes a re-scan proportional to the folder's *changes* rather
// than its size: dicomqr writes each file once and never edits it, so an
// unchanged folder needs no file opened at all and the walk is the whole cost.
//
// Falls back to a full scan when there is no index to compare against — a sync
// that cannot read the index is simply a scan.
func syncLocalFolder(cat *catalog, dir string, progress func(phase scanPhase, done, total int)) (
	[]localStudy, []localSeries, map[string][]string, syncCounts, error) {

	entries, failed, err := walkDicomFiles(dir, progress)
	if err != nil {
		return nil, nil, nil, syncCounts{}, err
	}

	stamps, stampErr := cat.fileStamps()
	if cat == nil || stampErr != nil {
		if stampErr != nil {
			logWarn("catalog: reading file stamps failed, falling back to a full scan: %v", stampErr)
		}
		studies, series, files, scanErr := scanLocalFolder(dir, progress)
		return studies, series, files, syncCounts{added: len(entries)}, scanErr
	}

	onDisk := make(map[string]bool, len(entries))
	var toParse []string
	counts := syncCounts{}
	for _, e := range entries {
		onDisk[e.path] = true
		if st, known := stamps[e.path]; known && st.size == e.size && st.mtime == e.mtime {
			counts.unchanged++
			continue
		}
		toParse = append(toParse, e.path)
	}
	gone := filesToRemove(stamps, onDisk, failed > 0)
	if failed > 0 {
		logWarn("scan: %d folder entries could not be read — leaving the index alone rather than "+
			"treating their files as deleted", failed)
	}

	var metas []fileMeta
	for _, r := range parseFilesParallel(toParse, progress) {
		if r.ok {
			metas = append(metas, r.m)
		}
	}
	counts.added = cat.upsertMetas(metas)
	counts.removed = cat.removePaths(gone)

	// The index now matches the folder, so the hierarchy comes from the one
	// place that knows it rather than being merged a second way here.
	studies, series, files, loadErr := cat.load()
	sortScanHierarchy(studies, series)
	return studies, series, files, counts, loadErr
}

// filesForNode collects the file paths for a tree node from the seriesFiles map.
// Series → direct lookup. Study/patient → union of all descendant series files.
func filesForNode(id string, m *resultsModel, seriesFiles map[string][]string) []string {
	n, ok := m.nodes[id]
	if !ok {
		return nil
	}
	switch n.kind {
	case kindSeries:
		return seriesFiles[n.seriesInstanceUID]
	case kindStudy:
		var paths []string
		for _, childID := range m.childUIDs(id) {
			paths = append(paths, filesForNode(childID, m, seriesFiles)...)
		}
		return paths
	case kindPatient:
		var paths []string
		for _, studyID := range m.childUIDs(id) {
			paths = append(paths, filesForNode(studyID, m, seriesFiles)...)
		}
		return paths
	}
	return nil
}

// modificationScope maps a Local Browse selection onto the only two shapes a
// Modification run has: study-level (every selected node lies within one
// study — typically a subset of its series) or patient-level (several studies
// of one patient, or the patient node itself). There is deliberately no
// series-level run: the export root the user names stands in for the patient
// folder, or patient+study (see exportLayout), so a subset of series is just
// a study-level run over fewer files. A selection spanning patients is
// refused — one export root would merge their folders, and a profile's Set
// values (a new Patient Name, a Patient ID following it) would give every
// patient in it the same identity, a de-identification error rather than a
// layout one. label names the scope for the dialog header, saying how much
// of it the selection covers when that is not all of it.
func modificationScope(m *resultsModel, ids []string) (label string, studyLevel bool, err error) {
	patients := map[string]bool{}
	studies := map[string]bool{}
	selected := map[string]bool{}
	wholePatient := false
	for _, id := range ids {
		n, ok := m.nodes[id]
		if !ok {
			continue
		}
		selected[id] = true
		switch n.kind {
		case kindPatient:
			patients[id] = true
			wholePatient = true
		case kindStudy:
			studies[id] = true
			patients[n.parentID] = true
		case kindSeries:
			studies[n.parentID] = true
			patients[m.parentOf(n.parentID)] = true
		}
	}
	switch {
	case len(patients) == 0:
		return "", false, fmt.Errorf("nothing selected — click tree items to select them first")
	case len(patients) > 1:
		return "", false, fmt.Errorf("the selection spans %d patients. A Modification run exports one patient "+
			"under one export folder name, and its Set values (such as a new Patient Name) would give every "+
			"patient in it the same identity. Select series or studies of a single patient", len(patients))
	}
	var patientID string
	for id := range patients {
		patientID = id
	}

	if !wholePatient && len(studies) == 1 {
		var studyID string
		for id := range studies {
			studyID = id
		}
		label = m.labelFor(studyID)
		if !selected[studyID] {
			total, chosen := 0, 0
			for _, child := range m.childUIDs(studyID) {
				total++
				if selected[child] {
					chosen++
				}
			}
			label += fmt.Sprintf(" (%d of %d series selected)", chosen, total)
		}
		return label, true, nil
	}

	label = m.labelFor(patientID)
	if !selected[patientID] {
		label += fmt.Sprintf(" (selection from %d studies)", len(studies))
	}
	return label, false, nil
}

// modificationProfileItems returns one menu item per modification profile,
// sorted by name, each calling run with that name. profiles.json is re-read
// on every call so hand-edits take effect immediately (dicomtool semantics);
// a missing or unreadable file yields one disabled "(no profiles defined)"
// entry rather than an empty menu.
func modificationProfileItems(run func(name string)) []*fyne.MenuItem {
	var items []*fyne.MenuItem
	if profPath, err := modifyProfilesPath(); err == nil {
		if profCfg, err := loadModProfileConfig(profPath); err == nil {
			names := make([]string, 0, len(profCfg))
			for name := range profCfg {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				items = append(items, fyne.NewMenuItem(name, func() { run(name) }))
			}
		}
	}
	if len(items) == 0 {
		none := fyne.NewMenuItem("(no profiles defined)", nil)
		none.Disabled = true
		items = []*fyne.MenuItem{none}
	}
	return items
}

// pruneEmptyDirs walks up from dir toward root, removing each directory that
// is empty after the previous removal. Stops at root or at the first non-empty
// or unremovable directory.
func pruneEmptyDirs(dir, root string) {
	for {
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			break
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			break
		}
		parent := filepath.Dir(dir)
		if err := os.Remove(dir); err != nil {
			break
		}
		dir = parent
	}
}

// filesMissingOnDisk returns those of paths that are no longer on disk.
//
// It lists each folder once rather than stat-ing each file: the files of a
// series share one folder, so a patient of 20,000 files is a few dozen
// directory reads instead of 20,000 stats — on a cold disk, the difference
// between an instant check and many seconds of disk seeks every time a large
// node is touched. Names are compared case-insensitively, as Windows resolves
// them.
//
// Only a file genuinely absent counts as missing: absent from a folder that
// was read, or inside a folder that does not exist. A folder that exists but
// cannot be read (permissions, a drive that is momentarily unavailable) counts
// nothing as missing — the previous per-file check treated any stat error as
// "deleted" and pruned those files from the index, the unsafe direction.
func filesMissingOnDisk(paths []string) []string {
	byDir := make(map[string][]string)
	for _, p := range paths {
		d := filepath.Dir(p)
		byDir[d] = append(byDir[d], p)
	}
	var missing []string
	for dir, files := range byDir {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				missing = append(missing, files...)
			}
			continue
		}
		present := make(map[string]bool, len(entries))
		for _, e := range entries {
			present[strings.ToLower(e.Name())] = true
		}
		for _, p := range files {
			if !present[strings.ToLower(filepath.Base(p))] {
				missing = append(missing, p)
			}
		}
	}
	return missing
}

// formatBytes returns a human-readable byte count.
func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// showDeleteDialog confirms and executes deletion of paths from local storage.
// onDeleted is called on the main goroutine after the deletion completes so the
// caller can trigger a rescan.
func showDeleteDialog(w fyne.Window, cfg *Settings, paths []string, description string, onDeleted func()) {
	if len(paths) == 0 {
		return
	}

	// Deduplicate paths; the total size is worked out in the background below.
	seen := make(map[string]bool)
	var unique []string
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}

	message := func(size string) string {
		return fmt.Sprintf("%s\n\n%d file(s) (%s) will be permanently deleted from disk. This cannot be undone.",
			description, len(unique), size)
	}
	msgLbl := widget.NewLabel(message("calculating size…"))
	msgLbl.Wrapping = fyne.TextWrapWord

	// Summing the sizes used to stat every file on the UI goroutine before the
	// dialog appeared — a patient of 20,000 files froze the window for as long
	// as that took, on a cold disk many seconds. The dialog now opens at once
	// and fills the size in when it is known; Delete does not depend on it.
	sized := make(chan struct{})
	go func() {
		var total int64
		for _, p := range unique {
			select {
			case <-sized:
				return // deletion started or dialog cancelled: no longer needed
			default:
			}
			if info, err := os.Stat(p); err == nil {
				total += info.Size()
			}
		}
		fyne.Do(func() {
			select {
			case <-sized:
			default:
				msgLbl.SetText(message(formatBytes(total)))
			}
		})
	}()
	var stopSizing sync.Once
	stopSize := func() { stopSizing.Do(func() { close(sized) }) }

	statusLbl := widget.NewLabel("")
	statusLbl.Wrapping = fyne.TextWrapWord
	statusLbl.Hide()

	deleteBtn := widget.NewButton("Delete", nil)
	deleteBtn.Importance = widget.DangerImportance
	cancelBtn := widget.NewButton("Cancel", nil)

	content := container.NewVBox(
		msgLbl,
		statusLbl,
		widget.NewSeparator(),
		container.NewHBox(layout.NewSpacer(), cancelBtn, deleteBtn),
	)

	dlg := dialog.NewCustomWithoutButtons("Confirm Delete", container.NewPadded(content), w)
	dlg.Resize(fyne.NewSize(420, 0))

	cancelBtn.OnTapped = func() { stopSize(); dlg.Hide() }

	deleteBtn.OnTapped = func() {
		stopSize()
		deleteBtn.Disable()
		cancelBtn.Disable()
		msgLbl.Hide()
		statusLbl.SetText(fmt.Sprintf("Deleting %d file(s)…", len(unique)))
		statusLbl.Show()

		go func() {
			var nOK, nFail int
			dirs := make(map[string]bool)
			for _, p := range unique {
				if err := os.Remove(p); err != nil {
					nFail++
				} else {
					nOK++
					dirs[filepath.Dir(p)] = true
				}
			}
			root := cfg.DownloadDir
			for dir := range dirs {
				pruneEmptyDirs(dir, root)
			}
			fyne.Do(func() {
				dlg.Hide()
				if nFail > 0 {
					dialog.ShowError(fmt.Errorf(
						"deleted %d file(s); %d could not be deleted", nOK, nFail), w)
				}
				if onDeleted != nil {
					onDeleted()
				}
			})
		}()
	}

	dlg.Show()
}

// showPushDialog presents a profile-selection + progress dialog for sending
// paths to a DICOM destination via C-STORE SCU.
func showPushDialog(w fyne.Window, cfg *Settings, paths []string, description string) {
	if len(paths) == 0 {
		return
	}
	if len(cfg.Profiles) == 0 {
		dialog.ShowInformation("No Profiles",
			"No server profiles are configured.\nAdd one in Preferences.", w)
		return
	}

	names := make([]string, len(cfg.Profiles))
	for i, p := range cfg.Profiles {
		names[i] = p.Name
	}

	destSelect := widget.NewSelect(names, nil)
	destSelect.SetSelectedIndex(0)

	progressBar := widget.NewProgressBar()
	statusLbl := widget.NewLabel(description)
	statusLbl.Wrapping = fyne.TextWrapWord

	pushBtn := widget.NewButton("Push", nil)
	pushBtn.Importance = widget.HighImportance
	cancelBtn := widget.NewButton("Cancel", nil)

	content := container.NewVBox(
		container.NewBorder(nil, nil, widget.NewLabel("Destination:"), nil, destSelect),
		progressBar,
		statusLbl,
		widget.NewSeparator(),
		container.NewHBox(layout.NewSpacer(), cancelBtn, pushBtn),
	)

	dlg := dialog.NewCustomWithoutButtons("Push to PACS", container.NewPadded(content), w)
	dlg.Resize(fyne.NewSize(460, 0))

	ctx, cancel := context.WithCancel(context.Background())
	cancelBtn.OnTapped = func() { cancel(); dlg.Hide() }

	pushBtn.OnTapped = func() {
		profileName := destSelect.Selected
		var chosen *ServerProfile
		for i := range cfg.Profiles {
			if cfg.Profiles[i].Name == profileName {
				chosen = &cfg.Profiles[i]
				break
			}
		}
		if chosen == nil {
			return
		}

		pushBtn.Disable()
		cancelBtn.Disable()
		destSelect.Disable()
		progressBar.SetValue(0)
		statusLbl.SetText(fmt.Sprintf("Connecting to %s…", chosen.Name))

		client := NewDicomClient(*chosen, cfg.LocalAETitle)
		total := len(paths)

		go func() {
			var nOK, nFail int
			_ = client.StoreFiles(ctx, paths, func(p StoreProgress) {
				if p.Err != nil {
					nFail++
					logError("push: failed %s: %v", filepath.Base(p.Path), p.Err)
				} else {
					nOK++
				}
				if p.Done%10 == 0 || p.Done == total {
					fyne.Do(func() {
						progressBar.SetValue(float64(p.Done) / float64(total))
						statusLbl.SetText(fmt.Sprintf("Sending %d / %d…", p.Done, total))
					})
				}
			})
			fyne.Do(func() {
				progressBar.SetValue(1)
				msg := fmt.Sprintf("Done — %d sent", nOK)
				if nFail > 0 {
					msg += fmt.Sprintf(", %d failed (see Activity Log)", nFail)
				}
				statusLbl.SetText(msg)
				pushBtn.Hide()
				cancelBtn.SetText("Close")
				cancelBtn.OnTapped = func() { dlg.Hide() }
				cancelBtn.Enable()
			})
		}()
	}

	dlg.Show()
}

// buildLocalBrowseContent constructs the Local Browse tab.
// Returns the tab content, a refresh func that re-renders the tree (call it
// after applying theme/selection preferences; it also reopens the catalog when
// the download directory changed), and a reload func that repopulates the tree
// from the catalog (call it after downloads or imports add files).
func buildLocalBrowseContent(a fyne.App, w fyne.Window, cfg *Settings, cat *catalog, openInViewer func(string)) (fyne.CanvasObject, func(), func()) {
	model := newResultsModel()
	seriesFiles := make(map[string][]string)

	var doScan func()
	var reloadFromDB func(status string)
	var pruneMissing func(paths []string)
	var verifyNode func(id string)
	var tree *widget.Tree
	sel := newNodeSelection(model,
		func(id string) { tree.RefreshItem(id) },
		func() { tree.Refresh() },
	)

	onTapped := func(id string, mods fyne.KeyModifier) {
		verifyNode(id)
		sel.Click(id, mods)
	}

	var scanDir string

	// modificationRoot is the download-folder root a Modification run's
	// source paths are made relative to (see exportLayout).
	modificationRoot := func() string {
		if scanDir != "" {
			return scanDir
		}
		return cfg.DownloadDir
	}

	onMenu := func(id string, pos fyne.Position) {
		verifyNode(id)
		// Collect the exact files for this node so Preview is scoped correctly.
		rawPaths := filesForNode(id, model, seriesFiles)
		capturedPaths := make([]string, len(rawPaths))
		copy(capturedPaths, rawPaths)
		previewTitle := "DICOM Preview — " + model.labelFor(id)

		localFolder := model.localFolderFor(id, scanDir)
		if localFolder == "" {
			localFolder = scanDir
		}

		_, studyUID, seriesUID, _ := model.uidsForNode(id)
		uid := seriesUID
		if uid == "" {
			uid = studyUID
		}

		previewItem := fyne.NewMenuItem("Preview Images", func() {
			if seriesUID != "" {
				// Series node: open the regular linear viewer.
				go showDicomViewerPaths(a, w, previewTitle, capturedPaths)
				return
			}
			// Study node: show the middle slice from each series in a grid.
			// Only cheap model lookups happen here on the UI goroutine; the
			// expensive work — sorting parses an InstanceNumber out of every
			// file in the study — runs inside showStudyOverviewWindow behind
			// its busy dialog.
			var thumbs []seriesThumb
			for _, childID := range model.childUIDs(id) {
				ps := filesForNode(childID, model, seriesFiles)
				if len(ps) == 0 {
					continue
				}
				thumbs = append(thumbs, seriesThumb{
					label:    model.labelFor(childID),
					modality: model.modalityFor(childID),
					paths:    ps,
				})
			}
			go showStudyOverviewWindow(a, w, previewTitle, thumbs)
		})
		previewItem.Disabled = studyUID == "" // patient-level: too broad to preview

		capturedLabel := model.labelFor(id)
		// Tag-level review: the dicomhdr-style tag inspector over this node's
		// files, in its own window. Enabled at every level — files are parsed
		// concurrently and the tree populates incrementally, so even a whole
		// patient loads progressively rather than blocking.
		tagsItem := fyne.NewMenuItem("View Tags", func() {
			go showTagViewerWindow(a, cfg, "DICOM Tags — "+capturedLabel, capturedPaths)
		})
		tagsItem.Disabled = len(capturedPaths) == 0

		viewerItem := fyne.NewMenuItem("Open in Viewer", func() { openInViewer(localFolder) })
		viewerItem.Disabled = cfg.ViewerPath == ""
		capturedFolder := localFolder
		openFolderItem := fyne.NewMenuItem("Open folder", func() {
			go exec.Command("explorer", capturedFolder).Start()
		})
		pushItem := fyne.NewMenuItem("Push to PACS…", func() {
			showPushDialog(w, cfg, capturedPaths,
				fmt.Sprintf("Push %d file(s) from %q to a DICOM destination.",
					len(capturedPaths), model.labelFor(id)))
		})
		deleteItem := fyne.NewMenuItem("Delete…", func() {
			showDeleteDialog(w, cfg, capturedPaths,
				fmt.Sprintf("Delete %q from local storage.", capturedLabel),
				func() { pruneMissing(capturedPaths) })
		})
		// Modification: apply a de-identification profile to this patient/study.
		// Profiles are re-read on every menu open so hand-edits to
		// ~/.dicomqr/profiles.json take effect immediately (dicomtool semantics).
		modItem := fyne.NewMenuItem("Modification", nil)
		modItem.Disabled = seriesUID != "" // patient and study levels only
		if !modItem.Disabled {
			capturedRoot := modificationRoot()
			capturedStudyLevel := studyUID != ""
			modItem.ChildMenu = fyne.NewMenu("", modificationProfileItems(func(name string) {
				showModificationDialog(w, cfg, name, capturedLabel, capturedPaths, capturedRoot, capturedStudyLevel)
			})...)
		}
		copyUID := fyne.NewMenuItem("Copy UID", func() { w.Clipboard().SetContent(uid) })
		copyLabel := fyne.NewMenuItem("Copy label", func() { w.Clipboard().SetContent(model.labelFor(id)) })
		popup := widget.NewPopUpMenu(fyne.NewMenu("",
			previewItem,
			tagsItem,
			viewerItem,
			openFolderItem,
			pushItem,
			modItem,
			deleteItem,
			fyne.NewMenuItemSeparator(),
			copyUID, copyLabel,
		), w.Canvas())
		popup.ShowAtPosition(pos)
	}

	tree = widget.NewTree(
		model.childUIDs,
		model.isBranch,
		func(_ bool) fyne.CanvasObject { return newQueryRow(onTapped, onMenu) },
		func(id widget.TreeNodeID, _ bool, node fyne.CanvasObject) {
			row := node.(*queryRow)
			row.nodeID = id
			row.ct.Text = model.labelFor(id)
			row.ct.TextSize = theme.TextSize()
			if sel.Selected(id) {
				if cfg.SelectionColor != "" {
					row.ct.Color = hexToColor(cfg.SelectionColor)
				} else {
					row.ct.Color = theme.Color(theme.ColorNamePrimary)
				}
				row.ct.TextStyle = fyne.TextStyle{Bold: cfg.SelectionBold, Italic: cfg.SelectionItalic}
			} else {
				row.ct.Color = theme.Color(theme.ColorNameForeground)
				row.ct.TextStyle = fyne.TextStyle{}
			}
			row.Refresh()
		},
	)
	treeCollapseFix(tree)

	scanStatusLbl := widget.NewLabel("Click Scan to index the download folder.")

	folderLabel := widget.NewLabel(cfg.DownloadDir)
	folderLabel.Truncation = fyne.TextTruncateEllipsis

	// applyData replaces the tree contents, dropping selections for nodes that
	// no longer exist. Runs on the UI goroutine.
	applyData := func(studies []localStudy, series []localSeries, files map[string][]string) {
		seriesFiles = files
		model.clear()
		for _, s := range studies {
			model.addStudy(s.patientName, s.patientID, s.studyUID, s.studyDate,
				s.studyDesc, s.accession, s.modalities)
		}
		for _, sr := range series {
			model.addSeries(sr.studyUID, sr.seriesUID, sr.modality,
				sr.seriesNumber, sr.seriesDesc, sr.numInstances)
		}
		model.applyFilter()
		sel.Prune()
		tree.Refresh()
	}

	// reloadFromDB repopulates the tree from the catalog. Safe to call from any
	// goroutine. A non-empty status overrides the default "N studies" message.
	reloadFromDB = func(status string) {
		if cat == nil {
			return
		}
		go func() {
			studies, series, files, err := cat.load()
			fyne.Do(func() {
				if err != nil {
					scanStatusLbl.SetText("Index error: " + err.Error())
					return
				}
				scanDir = cfg.DownloadDir
				applyData(studies, series, files)
				msg := status
				if msg == "" {
					if len(studies) == 0 {
						msg = "Click Scan to index the download folder."
					} else {
						noun := "studies"
						if len(studies) == 1 {
							noun = "study"
						}
						msg = fmt.Sprintf("%d %s, %d series in local index", len(studies), noun, len(series))
					}
				}
				scanStatusLbl.SetText(msg)
			})
		}()
	}

	statMissing := filesMissingOnDisk

	// pruneMissing stats paths in the background and removes those no longer on
	// disk from the catalog and tree, keeping both consistent with the folder.
	pruneMissing = func(paths []string) {
		captured := make([]string, len(paths))
		copy(captured, paths)
		go func() {
			if missing := statMissing(captured); len(missing) > 0 {
				n := cat.removePaths(missing)
				reloadFromDB(fmt.Sprintf("Removed %d file(s) no longer on disk", n))
			}
		}()
	}

	// verifyNode checks in the background that a touched node's files still
	// exist, pruning entries that were removed outside the app. A per-node
	// in-flight guard stops repeated taps from re-statting large subtrees.
	// A node checked in the last verifyTTL is not checked again: every tap and
	// right-click touches a node, and without this, clicking around a large
	// patient re-read its folders each time (the guard above only stops
	// overlapping checks, not back-to-back ones).
	var verifyMu sync.Mutex
	verifying := make(map[string]bool)
	verifiedAt := make(map[string]time.Time)
	const verifyTTL = 30 * time.Second
	verifyNode = func(id string) {
		if cat == nil {
			return
		}
		paths := filesForNode(id, model, seriesFiles)
		if len(paths) == 0 {
			return
		}
		verifyMu.Lock()
		inFlight := verifying[id]
		recent := time.Since(verifiedAt[id]) < verifyTTL
		if !inFlight && !recent {
			verifying[id] = true
			verifiedAt[id] = time.Now()
		}
		verifyMu.Unlock()
		if inFlight || recent {
			return
		}
		captured := make([]string, len(paths))
		copy(captured, paths)
		go func() {
			defer func() {
				verifyMu.Lock()
				delete(verifying, id)
				verifyMu.Unlock()
			}()
			if missing := statMissing(captured); len(missing) > 0 {
				n := cat.removePaths(missing)
				reloadFromDB(fmt.Sprintf("Removed %d file(s) no longer on disk", n))
			}
		}()
	}

	// scanProgressText renders a progress report for the status line. The walk
	// has no total to work towards, so it counts; the read phase has one.
	scanProgressText := func(phase scanPhase, done, total int) string {
		if phase == scanPhaseWalk {
			return fmt.Sprintf("Checking %d files…", done)
		}
		return fmt.Sprintf("Reading %d of %d changed files…", done, total)
	}

	// beginScan resets the tree for a fresh scan of the download folder,
	// returning the folder or "" when there is none configured.
	beginScan := func() string {
		dir := cfg.DownloadDir
		if dir == "" {
			return ""
		}
		scanDir = dir
		model.clear()
		sel.Clear()
		seriesFiles = make(map[string][]string)
		tree.Refresh()
		scanStatusLbl.SetText("Scanning…")
		return dir
	}

	// finishScan puts the results on screen. extra names what a sync changed;
	// a rebuild leaves it empty, having read everything by definition.
	finishScan := func(dir string, studies []localStudy, series []localSeries,
		files map[string][]string, extra string, err error) {
		if err != nil {
			scanStatusLbl.SetText("Scan error: " + err.Error())
			return
		}
		applyData(studies, series, files)
		noun := "studies"
		if len(studies) == 1 {
			noun = "study"
		}
		msg := fmt.Sprintf("Found %d %s, %d series in %s",
			len(studies), noun, len(series), filepath.Base(dir))
		if extra != "" {
			msg += " — " + extra
		}
		scanStatusLbl.SetText(msg)
	}

	// doScan brings the index into line with the folder, reading only files
	// that are new or whose size or timestamp has changed. On a folder nothing
	// has happened to, that opens no files at all — the walk is the whole cost.
	doScan = func() {
		dir := beginScan()
		if dir == "" {
			return
		}
		go func() {
			studies, series, files, counts, err := syncLocalFolder(cat, dir,
				func(phase scanPhase, done, total int) {
					text := scanProgressText(phase, done, total)
					fyne.Do(func() { scanStatusLbl.SetText(text) })
				})
			// What changed is the useful part of a sync's result; "nothing" is
			// as much an answer as a list of files.
			extra := "no changes"
			if counts.added > 0 || counts.removed > 0 {
				extra = fmt.Sprintf("%d added, %d removed", counts.added, counts.removed)
			}
			fyne.Do(func() { finishScan(dir, studies, series, files, extra, err) })
		}()
	}
	scanBtn := widget.NewButton("Scan", doScan)

	// Rebuild re-reads every file and replaces the index outright — what Scan
	// used to do. It stays reachable because Scan now trusts the index, and
	// suspecting the index is exactly why somebody presses Scan.
	rebuildBtn := widget.NewButton("Rebuild", func() {
		dir := beginScan()
		if dir == "" {
			return
		}
		go func() {
			studies, series, files, err := scanLocalFolder(dir,
				func(phase scanPhase, done, total int) {
					text := scanProgressText(phase, done, total)
					if phase == scanPhaseRead {
						text = fmt.Sprintf("Reading %d of %d files…", done, total)
					}
					fyne.Do(func() { scanStatusLbl.SetText(text) })
				})
			if err == nil {
				if dbErr := cat.replaceAll(studies, series, files); dbErr != nil {
					logError("catalog: replace after rebuild: %v", dbErr)
				}
			}
			fyne.Do(func() { finishScan(dir, studies, series, files, "index rebuilt", err) })
		}()
	})

	// Reveals the download folder in Explorer — a plain folder view, not a
	// picker. Labelled so it can't be mistaken for the Preferences and Import
	// tab "Browse…" buttons, which do open a picker to choose a folder.
	openFolderBtn := widget.NewButtonWithIcon("Open in Explorer", theme.FolderOpenIcon(), func() {
		if cfg.DownloadDir == "" {
			return
		}
		go exec.Command("explorer", cfg.DownloadDir).Start()
	})

	dirBar := container.NewBorder(nil, nil,
		widget.NewLabel("Folder:"),
		container.NewHBox(openFolderBtn, scanBtn, rebuildBtn),
		folderLabel,
	)

	filterEntry := widget.NewEntry()
	filterEntry.SetPlaceHolder("Filter results…")
	var filterDebounce *time.Timer
	filterEntry.OnChanged = func(s string) {
		if filterDebounce != nil {
			filterDebounce.Stop()
		}
		filterDebounce = time.AfterFunc(150*time.Millisecond, func() {
			fyne.Do(func() {
				model.setFilter(s)
				if s != "" {
					tree.OpenAllBranches()
				}
				tree.Refresh()
			})
		})
	}

	filterBar := container.NewBorder(nil, nil, nil,
		container.NewHBox(
			widget.NewButton("Expand All", func() { tree.OpenAllBranches() }),
			widget.NewButton("Collapse All", func() { collapseAllTree(tree) }),
			widget.NewButton("Clear", func() {
				filterEntry.SetText("")
				model.setFilter("")
				tree.Refresh()
			}),
		),
		filterEntry,
	)

	collectSelected := func() []string { return sel.Paths(seriesFiles) }

	pushSelectedBtn := widget.NewButton("Push Selected…", func() {
		paths := collectSelected()
		if len(paths) == 0 {
			scanStatusLbl.SetText("Nothing selected — click tree items to select them first.")
			return
		}
		showPushDialog(w, cfg, paths,
			fmt.Sprintf("Push %d selected file(s) to a DICOM destination.", len(paths)))
	})

	deleteSelectedBtn := widget.NewButton("Delete Selected…", func() {
		paths := collectSelected()
		if len(paths) == 0 {
			scanStatusLbl.SetText("Nothing selected — click tree items to select them first.")
			return
		}
		showDeleteDialog(w, cfg, paths,
			fmt.Sprintf("Delete %d selected file(s) from local storage.", len(paths)),
			func() { pruneMissing(paths) })
	})

	// Modify Selected… runs a Modification over exactly the selected files —
	// the way to de-identify a subset of a study's series, which the
	// right-click submenu (whole patient or study) cannot. The profile is
	// chosen from a menu opened above the button (it sits in the bottom bar,
	// so below it there is no room), listing what the submenu lists.
	var modifySelectedBtn *widget.Button
	modifySelectedBtn = widget.NewButton("Modify Selected…", func() {
		ids := sel.IDs()
		if len(ids) == 0 {
			scanStatusLbl.SetText("Nothing selected — click tree items to select them first.")
			return
		}
		label, studyLevel, err := modificationScope(model, ids)
		if err != nil {
			dialog.ShowInformation("Modify Selected", err.Error()+".", w)
			return
		}
		paths := collectSelected()
		root := modificationRoot()
		menu := widget.NewPopUpMenu(fyne.NewMenu("", modificationProfileItems(func(name string) {
			showModificationDialog(w, cfg, name, label, paths, root, studyLevel)
		})...), w.Canvas())
		pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(modifySelectedBtn)
		menu.ShowAtPosition(fyne.NewPos(pos.X, max(0, pos.Y-menu.MinSize().Height)))
	})

	// Previewing and the external viewer are on each tree item's right-click
	// menu (Preview Images, Open in Viewer), acting on the item clicked. The
	// bottom bar once had Preview and Open in Viewer buttons too, but they
	// ignored the selection and acted on the whole download folder — Preview
	// read every file in it behind a busy dialog with no way to cancel — so
	// they were removed (user decision 2026-10-07).
	bottomBar := container.NewVBox(
		widget.NewSeparator(),
		container.NewHBox(
			pushSelectedBtn,
			modifySelectedBtn,
			deleteSelectedBtn,
			layout.NewSpacer(),
			widget.NewButton("Select All", func() { sel.SelectAll(model.activeRoots()) }),
			widget.NewButton("Clear Selection", func() { sel.Clear() }),
		),
		scanStatusLbl,
	)

	content := container.NewBorder(
		container.NewVBox(dirBar, filterBar),
		bottomBar,
		nil, nil,
		tree,
	)

	// Populate the tree from the persisted index at startup, so the previous
	// session's contents appear without a disk rescan.
	reloadFromDB("")

	return content, func() {
		folderLabel.SetText(cfg.DownloadDir)
		// The download directory changed in Preferences: switch to that
		// directory's own index file and reload the tree from it.
		if cat != nil && cfg.DownloadDir != "" && cat.Dir() != cfg.DownloadDir {
			go func() {
				if err := cat.Reopen(cfg.DownloadDir); err != nil {
					logError("catalog: reopen %s: %v", cfg.DownloadDir, err)
					return
				}
				reloadFromDB("")
			}()
		}
		tree.Refresh()
	}, func() { reloadFromDB("") }
}
