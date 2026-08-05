package main

// Modification engine — applies a de-identification profile to DICOM files.
// Ported from the dicomtool CLI's modify command so both tools transform files
// identically. Per-file order of operations (matching dicomtool):
//
//	parse → ignoretype → ignoremodality → per-modality overrides → fixvr →
//	remove + noprivate → date shift → dob mask → uid suffix / uid remap → set →
//	transfer syntax
//
// The transfer-syntax conversion is dicomqr's own step, with no dicomtool
// equivalent, and comes last deliberately: decompressing pixel data rewrites
// the attributes that describe it (Photometric Interpretation, Planar
// Configuration), so it must have the final say over them.
//
// Files are read from the download folder and written to a separate output
// folder preserving the relative folder structure; sources are never touched.

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// tagEdit holds a parsed tag and its replacement string value.
type tagEdit struct {
	tag   tag.Tag
	value string
}

// modifyParams is a fully parsed and validated set of modification parameters,
// ready for processFile. Built from a ModProfile by compileModifyParams.
type modifyParams struct {
	edits     []tagEdit
	removals  []tag.Tag
	dobMask   string
	uidSuffix string
	// shiftDays stays a string: "" means no shift while "0" is an accepted
	// (no-op) action, matching dicomtool. Atoi-validated by compileModifyParams.
	shiftDays        string
	fixvrMode        string
	removePrivate    bool
	ignoreTypes      []string
	ignoreModalities []string
	perMod           map[string]modalityOverride
	remapUIDs        bool
	// targetTS is the transfer syntax UID every output is written in, or "" to
	// write each file in the syntax it was stored in. Profile-wide: unlike the
	// tag rules it is never overridden per modality.
	targetTS string
}

// compileModifyParams validates p and parses its tag references (resolving
// aliases) into a modifyParams. The validation rules match dicomtool's modify
// command exactly.
func compileModifyParams(p ModProfile, aliases TagConfig) (modifyParams, error) {
	var mp modifyParams

	mp.fixvrMode = strings.ToLower(strings.TrimSpace(p.FixVR))
	if mp.fixvrMode != "" && mp.fixvrMode != "correct" && mp.fixvrMode != "skip" && mp.fixvrMode != "passthrough" {
		return mp, fmt.Errorf("fixvr %q: must be correct, skip, or passthrough", p.FixVR)
	}

	mp.shiftDays = strings.TrimSpace(p.ShiftDays)
	if mp.shiftDays != "" {
		if _, err := strconv.Atoi(mp.shiftDays); err != nil {
			return mp, fmt.Errorf("shiftdays %q must be an integer", p.ShiftDays)
		}
	}

	targetTS, tsOK := modProfileTargetSyntax(p)
	if !tsOK {
		return mp, fmt.Errorf("transfersyntax %q: must be %s or %s", p.TransferSyntax,
			tsPrefExplicitLE, tsPrefImplicitLE)
	}
	mp.targetTS = targetTS

	mp.remapUIDs = p.RemapUIDs
	mp.uidSuffix = strings.TrimSpace(p.UIDSuffix)
	if mp.remapUIDs && mp.uidSuffix != "" {
		return mp, errors.New("Remap UIDs and a UID suffix cannot be combined")
	}
	for _, c := range mp.uidSuffix {
		if c < '1' || c > '9' {
			return mp, fmt.Errorf("UID suffix %q must contain digits in the set [1..9] only", mp.uidSuffix)
		}
	}

	mp.dobMask = strings.TrimSpace(p.DOB)
	if mp.dobMask != "" && len(mp.dobMask) != 8 {
		return mp, fmt.Errorf("birth date mask must be exactly 8 characters (YYYYMMDD format), got %d", len(mp.dobMask))
	}

	// Exact-capacity allocations: processFile appends per-modality overrides to
	// these slices, and append must reallocate rather than write into a backing
	// array shared across concurrent workers.
	mp.removals = make([]tag.Tag, 0, len(p.Removes))
	for _, r := range p.Removes {
		t, err := parseTagString(aliases.Resolve(strings.TrimSpace(r)))
		if err != nil {
			return mp, fmt.Errorf("invalid remove tag %q: %w", r, err)
		}
		mp.removals = append(mp.removals, t)
	}

	mp.edits = make([]tagEdit, 0, len(p.Sets))
	for _, s := range p.Sets {
		tagStr, value, ok := strings.Cut(s, "=")
		if !ok || tagStr == "" {
			return mp, fmt.Errorf("invalid set value %q: expected <tag>=<value>", s)
		}
		t, err := parseTagString(aliases.Resolve(strings.TrimSpace(tagStr)))
		if err != nil {
			return mp, fmt.Errorf("invalid tag %q: %w", tagStr, err)
		}
		mp.edits = append(mp.edits, tagEdit{tag: t, value: value})
	}

	mp.removePrivate = p.Priv

	for _, v := range p.IgnoreTypes {
		if v = strings.TrimSpace(v); v != "" {
			mp.ignoreTypes = append(mp.ignoreTypes, v)
		}
	}
	for _, v := range p.IgnoreModalities {
		if v = strings.TrimSpace(v); v != "" {
			mp.ignoreModalities = append(mp.ignoreModalities, v)
		}
	}

	if len(p.PerModality) > 0 {
		normalized := make(map[string]ModProfile, len(p.PerModality))
		for k, v := range p.PerModality {
			normalized[strings.ToUpper(k)] = v
		}
		mp.perMod = buildModalityOverrides(normalized, aliases)
	}

	// A transfer syntax alone is actionable: converting a study to an
	// uncompressed syntax is a legitimate standalone operation, and export is
	// the only place the application can perform one.
	hasAction := len(mp.edits) > 0 || len(mp.removals) > 0 ||
		mp.dobMask != "" || mp.uidSuffix != "" || mp.shiftDays != "" ||
		mp.removePrivate || mp.fixvrMode != "" || mp.targetTS != "" ||
		len(mp.ignoreTypes) > 0 || len(mp.ignoreModalities) > 0 ||
		len(mp.perMod) > 0 || mp.remapUIDs
	if !hasAction {
		return mp, errors.New("the profile contains no actionable parameter (set, remove, dob, uid, shiftdays, noprivate, fixvr, remapuids, transfersyntax)")
	}

	return mp, nil
}

// modifyFailure records one file that could not be processed.
type modifyFailure struct {
	File  string
	Error string
}

// modifyResult summarises a runModification call.
type modifyResult struct {
	Processed int // files transformed and written
	Skipped   int // files skipped by ignoretype/ignoremodality or non-DICOM
	Failed    int
	Canceled  bool
	Failures  []modifyFailure
}

// exportRelPaths maps each file to its output path relative to the export
// root, replacing the PHI-bearing folder names the download-folder layout
// embeds (patient folder = name + MRN; study folder = description + date).
// For a study-level selection the patient and study components are dropped
// entirely — the caller's export folder stands in for them. For a
// patient-level selection each distinct study folder becomes a generic
// "study-NN" (numbered in sorted folder-name order, so the mapping is
// deterministic); series folders are kept, as series descriptions are the
// scanner's protocol names. Files not in the expected layout (e.g. the flat
// fallback for over-long paths) map to their bare file name.
func exportRelPaths(files []string, rootDir string, studyLevel bool) map[string]string {
	split := make(map[string][]string, len(files))
	studySet := map[string]bool{}
	for _, f := range files {
		rel, err := filepath.Rel(rootDir, f)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			split[f] = []string{filepath.Base(f)}
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		split[f] = parts
		if !studyLevel && len(parts) >= 3 {
			studySet[filepath.Join(parts[0], parts[1])] = true
		}
	}
	studyNames := make([]string, 0, len(studySet))
	for k := range studySet {
		studyNames = append(studyNames, k)
	}
	sort.Strings(studyNames)
	studyNum := make(map[string]string, len(studyNames))
	for i, k := range studyNames {
		studyNum[k] = fmt.Sprintf("study-%02d", i+1)
	}

	rels := make(map[string]string, len(files))
	for f, parts := range split {
		switch {
		case len(parts) < 3:
			rels[f] = parts[len(parts)-1]
		case studyLevel:
			rels[f] = filepath.Join(parts[2:]...)
		default:
			n := studyNum[filepath.Join(parts[0], parts[1])]
			rels[f] = filepath.Join(append([]string{n}, parts[2:]...)...)
		}
	}
	return rels
}

// zipSink serializes finished datasets into a single zip archive. Only the
// final encode is serialized — a zip writer supports one open entry at a
// time — so the parse/modify pipeline stays parallel across the worker pool.
type zipSink struct {
	mu sync.Mutex
	zw *zip.Writer
}

func (z *zipSink) write(rel string, ds sdicom.Dataset, opts []sdicom.WriteOption) error {
	z.mu.Lock()
	defer z.mu.Unlock()
	w, err := z.zw.Create(filepath.ToSlash(rel))
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(w, 1<<20)
	if err := sdicom.Write(bw, ds, opts...); err != nil {
		return err
	}
	return bw.Flush()
}

// runModification applies params to every file in files, writing results under
// outDir. Each file's output path within outDir comes from rels (as built by
// exportRelPaths); a nil rels falls back to the file's path relative to
// rootDir (mirroring the download-folder layout). Per-file failures are
// collected and never abort the run; progress is reported after every file.
// Cancelling ctx stops feeding new files; files already in flight complete.
func runModification(ctx context.Context, files []string, rootDir, outDir string,
	params modifyParams, rels map[string]string, progress func(done, total int)) modifyResult {
	return runModificationImpl(ctx, files, rootDir, outDir, params, rels, progress, nil)
}

// runModificationToZip runs the same pipeline as runModification but writes
// every output into a single zip archive at zipPath, with entry paths laid
// out exactly as the folder export would be (rels, forward-slashed). The
// archive is built as a hidden temp file and renamed into place when the run
// ends with at least one file written — including a cancelled run, which
// keeps the files completed before the cancel, mirroring folder-mode
// semantics. A run that writes nothing leaves no archive behind, and a
// finalize or rename failure converts the run's written count into failures,
// since the archive holding those files is lost with it.
func runModificationToZip(ctx context.Context, files []string, rootDir, zipPath string,
	params modifyParams, rels map[string]string, progress func(done, total int)) modifyResult {

	fail := func(err error) modifyResult {
		logError("modify: zip export %s failed: %v", zipPath, err)
		return modifyResult{Failed: len(files),
			Failures: []modifyFailure{{File: zipPath, Error: err.Error()}}}
	}
	if err := os.MkdirAll(filepath.Dir(zipPath), 0o755); err != nil {
		return fail(fmt.Errorf("create output folder: %w", err))
	}
	tmp, err := os.CreateTemp(filepath.Dir(zipPath), "."+filepath.Base(zipPath)+"_*.tmp")
	if err != nil {
		return fail(fmt.Errorf("create zip: %w", err))
	}
	tmpPath := tmp.Name()
	zw := zip.NewWriter(tmp)

	res := runModificationImpl(ctx, files, rootDir, "", params, rels, progress, &zipSink{zw: zw})

	ferr := zw.Close()
	if cerr := tmp.Close(); ferr == nil {
		ferr = cerr
	}
	if ferr == nil && res.Processed > 0 {
		ferr = os.Rename(tmpPath, zipPath)
	}
	if ferr != nil {
		os.Remove(tmpPath)
		logError("modify: zip export %s failed: %v", zipPath, ferr)
		res.Failures = append(res.Failures, modifyFailure{File: zipPath, Error: "finalize zip: " + ferr.Error()})
		res.Failed += res.Processed
		res.Processed = 0
		return res
	}
	if res.Processed == 0 {
		os.Remove(tmpPath)
	}
	return res
}

// runModificationImpl is the shared pipeline: with a nil zsink each output is
// written to its own file under outDir; with a zsink every output goes into
// the archive instead and outDir is unused.
func runModificationImpl(ctx context.Context, files []string, rootDir, outDir string,
	params modifyParams, rels map[string]string, progress func(done, total int), zsink *zipSink) modifyResult {

	var res modifyResult
	if len(files) == 0 {
		return res
	}

	type fileJob struct{ path, rel string }
	jobs := make([]fileJob, 0, len(files))
	for _, f := range files {
		rel := ""
		if rels != nil {
			rel = rels[f]
		}
		if rel == "" {
			r, err := filepath.Rel(rootDir, f)
			if err != nil || r == "." || strings.HasPrefix(r, "..") {
				r = filepath.Base(f)
			}
			rel = r
		}
		jobs = append(jobs, fileJob{f, rel})
	}

	// Write options are constant for the run; computing them once keeps the
	// per-file path free of the fixvr/transfer-syntax reasoning.
	writeOpts := modifyWriteOpts(params)

	// One shared UID remapper for the whole run guarantees that the same source
	// UID maps to the same replacement everywhere it appears across all files.
	var uidRemap *uidRemapper
	if params.remapUIDs {
		uidRemap = newUIDRemapper()
	}

	// Modest worker pool: the pipeline is disk-bound for typical studies and
	// the UI goroutine should keep breathing room (dicomtool, a batch CLI,
	// uses the full CPU count).
	numWorkers := min(runtime.NumCPU(), 4, len(jobs))

	var (
		mu   sync.Mutex
		done int
	)
	total := len(jobs)
	recordFailure := func(path string, err error) {
		logError("modify: failed %s: %v", path, err)
		mu.Lock()
		res.Failed++
		res.Failures = append(res.Failures, modifyFailure{File: path, Error: err.Error()})
		mu.Unlock()
	}

	jobCh := make(chan fileJob)
	var wg sync.WaitGroup
	for range numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				func() {
					srcFile, ferr := openDICOMFile(job.path)
					if ferr != nil {
						recordFailure(job.path, fmt.Errorf("open: %w", ferr))
						return
					}
					if srcFile == nil { // no DICM magic — not a DICOM file
						mu.Lock()
						res.Skipped++
						mu.Unlock()
						return
					}
					skipped, ds, perr := processFile(srcFile, params, uidRemap)
					if perr != nil {
						recordFailure(job.path, fmt.Errorf("process: %w", perr))
						return
					}
					if skipped {
						mu.Lock()
						res.Skipped++
						mu.Unlock()
						return
					}
					if zsink != nil {
						if zerr := zsink.write(job.rel, ds, writeOpts); zerr != nil {
							recordFailure(job.path, fmt.Errorf("zip write: %w", zerr))
						} else {
							mu.Lock()
							res.Processed++
							mu.Unlock()
						}
						return
					}
					outFile := filepath.Join(outDir, job.rel)
					if merr := os.MkdirAll(filepath.Dir(outFile), 0o755); merr != nil {
						recordFailure(job.path, fmt.Errorf("create output dir: %w", merr))
						return
					}
					f, cerr := os.Create(outFile)
					if cerr != nil {
						recordFailure(job.path, fmt.Errorf("create output file: %w", cerr))
						return
					}
					bw := bufio.NewWriterSize(f, 1<<20)
					werr := sdicom.Write(bw, ds, writeOpts...)
					fherr := bw.Flush()
					clerr := f.Close()
					switch {
					case werr != nil:
						recordFailure(job.path, fmt.Errorf("write: %w", werr))
					case fherr != nil:
						recordFailure(job.path, fmt.Errorf("flush: %w", fherr))
					case clerr != nil:
						recordFailure(job.path, fmt.Errorf("close: %w", clerr))
					default:
						mu.Lock()
						res.Processed++
						mu.Unlock()
					}
				}()
				mu.Lock()
				done++
				d := done
				mu.Unlock()
				if progress != nil {
					progress(d, total)
				}
			}
		}()
	}

feed:
	for _, job := range jobs {
		select {
		case <-ctx.Done():
			res.Canceled = true
			break feed
		case jobCh <- job:
		}
	}
	close(jobCh)
	wg.Wait()
	return res
}

// modalityTag is (0008,0060) — Modality.
var modalityTag = tag.Tag{Group: 0x0008, Element: 0x0060}

// imageTypeTag is (0008,0008) — Image Type.
var imageTypeTag = tag.Tag{Group: 0x0008, Element: 0x0008}

// elemStringComponents extracts all string components from a DICOM element,
// handling the three ways the library may store a CS/LO/SH value:
//   - []string  — each element may itself be a backslash-delimited multi-value
//   - []byte    — raw bytes (implicit-VR files); split on backslash
//   - anything else — stringify via fmt, strip brackets, split on backslash
//
// Each component is trimmed of ASCII spaces and null-byte padding.
func elemStringComponents(elem *sdicom.Element) []string {
	if elem == nil || elem.Value == nil {
		return nil
	}
	var raw string
	switch v := elem.Value.GetValue().(type) {
	case []string:
		raw = strings.Join(v, `\`)
	case []byte:
		raw = string(v)
	default:
		// Fallback: use the library's own string representation and strip any
		// surrounding brackets that some versions add (e.g. "[VAL1 VAL2]").
		raw = strings.Trim(fmt.Sprintf("%v", v), "[]")
	}
	var components []string
	for _, part := range strings.Split(raw, `\`) {
		part = strings.Trim(part, " \x00")
		if part != "" {
			components = append(components, part)
		}
	}
	return components
}

// fixvrWriteOpts returns WriteOptions that match the fixvr mode.
// correct/skip: applyFixVR pre-processes top-level elements, but nested
// sequence elements are not recursed into, so we still need SkipVRVerification
// at write time to prevent the writer from erroring on those.
// passthrough: additionally skips value-type verification since elements are
// written as-is without any pre-processing.
func fixvrWriteOpts(mode string) []sdicom.WriteOption {
	switch mode {
	case "correct", "skip":
		return []sdicom.WriteOption{sdicom.SkipVRVerification()}
	case "passthrough":
		return []sdicom.WriteOption{sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()}
	default:
		return nil
	}
}

// modifyWriteOpts returns the WriteOptions for one run: the fixvr mode's
// options, relaxed further when the profile converts the transfer syntax.
//
// A conversion to Explicit VR makes the writer emit a VR for every element,
// including ones parsed from an implicit-VR source where the dictionary VR and
// the stored value may disagree; verification would reject those and fail files
// that write fine untouched. transcodeDICOMFileToTemp has always passed both
// skips for exactly this reason. Profiles that leave the syntax as stored keep
// the stricter behaviour they have today.
func modifyWriteOpts(p modifyParams) []sdicom.WriteOption {
	opts := fixvrWriteOpts(p.fixvrMode)
	if p.targetTS == "" {
		return opts
	}
	switch p.fixvrMode {
	case "passthrough": // already both
		return opts
	case "correct", "skip": // already SkipVRVerification
		return append(opts, sdicom.SkipValueTypeVerification())
	default:
		return []sdicom.WriteOption{sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()}
	}
}

// modalityOverride holds the pre-computed parameter overrides for one modality.
type modalityOverride struct {
	edits         []tagEdit
	removals      []tag.Tag
	keep          []tag.Tag
	dobMask       string
	uidSuffix     string
	shiftDays     string
	fixvrMode     string
	removePrivate bool
	keepPrivate   bool
}

// buildModalityOverrides converts the per-modality profile map (already keyed
// in uppercase) into pre-parsed modalityOverride values ready for runtime use.
// Entries with unparsable tags are silently dropped, matching dicomtool.
func buildModalityOverrides(perMod map[string]ModProfile, aliases TagConfig) map[string]modalityOverride {
	if len(perMod) == 0 {
		return nil
	}
	result := make(map[string]modalityOverride, len(perMod))
	for mod, p := range perMod {
		var ov modalityOverride
		for _, s := range p.Sets {
			tagStr, value, ok := strings.Cut(s, "=")
			if !ok || tagStr == "" {
				continue
			}
			t, err := parseTagString(aliases.Resolve(tagStr))
			if err != nil {
				continue
			}
			ov.edits = append(ov.edits, tagEdit{tag: t, value: value})
		}
		for _, r := range p.Removes {
			t, err := parseTagString(aliases.Resolve(r))
			if err != nil {
				continue
			}
			ov.removals = append(ov.removals, t)
		}
		for _, k := range p.Keep {
			t, err := parseTagString(aliases.Resolve(k))
			if err != nil {
				continue
			}
			ov.keep = append(ov.keep, t)
		}
		ov.dobMask = p.DOB
		ov.uidSuffix = p.UIDSuffix
		ov.shiftDays = p.ShiftDays
		ov.fixvrMode = p.FixVR
		ov.removePrivate = p.Priv
		ov.keepPrivate = p.KeepPrivate
		result[mod] = ov
	}
	return result
}

// mergeEdits combines two tagEdit slices; override entries win on tag collision.
func mergeEdits(base, override []tagEdit) []tagEdit {
	if len(override) == 0 {
		return base
	}
	overrideTags := make(map[tag.Tag]bool, len(override))
	for _, e := range override {
		overrideTags[e.tag] = true
	}
	result := make([]tagEdit, 0, len(base)+len(override))
	for _, e := range base {
		if !overrideTags[e.tag] {
			result = append(result, e)
		}
	}
	return append(result, override...)
}

// filterKeep returns removals with any tag present in keep removed.
func filterKeep(removals []tag.Tag, keep []tag.Tag) []tag.Tag {
	if len(keep) == 0 {
		return removals
	}
	keepSet := make(map[tag.Tag]bool, len(keep))
	for _, t := range keep {
		keepSet[t] = true
	}
	out := make([]tag.Tag, 0, len(removals))
	for _, t := range removals {
		if !keepSet[t] {
			out = append(out, t)
		}
	}
	return out
}

// processFile parses src and applies all modifications from p. It returns
// (true, zero, nil) when the file should be skipped (ignoretype /
// ignoremodality match), or (false, transformed dataset, nil) on success. The
// caller is responsible for writing the returned dataset to its destination.
// src is always closed.
func processFile(src *os.File, p modifyParams, uidRemap *uidRemapper) (skipped bool, ds sdicom.Dataset, err error) {
	// Copy to locals: per-modality overrides layer onto these per file, and p's
	// slices are shared across concurrent workers.
	edits := p.edits
	removals := p.removals
	dobMask := p.dobMask
	uidSuffix := p.uidSuffix
	shiftDaysStr := p.shiftDays
	fixvrMode := p.fixvrMode
	removePrivate := p.removePrivate

	info, err := src.Stat()
	if err != nil {
		src.Close()
		return false, ds, fmt.Errorf("stat: %w", err)
	}
	br := bufio.NewReaderSize(src, 1<<20)
	ds, err = sdicom.Parse(br, info.Size(), nil)
	src.Close()
	if err != nil {
		return false, ds, fmt.Errorf("parse: %w", err)
	}

	if len(p.ignoreTypes) > 0 {
		if elem, err := ds.FindElementByTag(imageTypeTag); err == nil {
			for _, component := range elemStringComponents(elem) {
				for _, ignore := range p.ignoreTypes {
					if strings.EqualFold(component, strings.TrimSpace(ignore)) {
						return true, ds, nil
					}
				}
			}
		}
	}

	if len(p.ignoreModalities) > 0 {
		if elem, err := ds.FindElementByTag(modalityTag); err == nil {
			for _, component := range elemStringComponents(elem) {
				for _, ignore := range p.ignoreModalities {
					if strings.EqualFold(component, strings.TrimSpace(ignore)) {
						return true, ds, nil
					}
				}
			}
		}
	}

	// Apply per-modality overrides: layer modality-specific settings on top of
	// the base parameters before any modifications are applied.
	if len(p.perMod) > 0 {
		if modElem, err := ds.FindElementByTag(modalityTag); err == nil {
			for _, mod := range elemStringComponents(modElem) {
				modKey := strings.ToUpper(strings.TrimSpace(mod))
				if ov, ok := p.perMod[modKey]; ok {
					edits = mergeEdits(edits, ov.edits)
					removals = append(removals, ov.removals...)
					removals = filterKeep(removals, ov.keep)
					if ov.dobMask != "" {
						dobMask = ov.dobMask
					}
					if ov.uidSuffix != "" {
						uidSuffix = ov.uidSuffix
					}
					if ov.shiftDays != "" {
						shiftDaysStr = ov.shiftDays
					}
					if ov.fixvrMode != "" {
						fixvrMode = ov.fixvrMode
					}
					if ov.removePrivate {
						removePrivate = true
					}
					if ov.keepPrivate {
						removePrivate = false
					}
					break
				}
			}
		}
	}

	if fixvrMode == "correct" || fixvrMode == "skip" {
		applyFixVR(&ds, fixvrMode)
	}

	if removePrivate || len(removals) > 0 {
		removalSet := make(map[tag.Tag]struct{}, len(removals))
		for _, t := range removals {
			removalSet[t] = struct{}{}
		}
		ds.Elements = pruneElements(ds.Elements, removalSet, removePrivate)
	}

	if shiftDaysStr != "" {
		// Re-validated defensively: compileModifyParams checks the top-level
		// value, but a hand-authored per-modality block can carry garbage.
		// Failing the file (rather than silently skipping the shift) matches
		// dicomtool and keeps real dates from shipping unnoticed.
		n, err := strconv.Atoi(shiftDaysStr)
		if err != nil {
			return false, ds, fmt.Errorf("shiftdays %q must be an integer", shiftDaysStr)
		}
		applyDateShift(ds.Elements, n)
	}

	if dobMask != "" {
		if err := applyDOBMask(&ds, dobMask); err != nil {
			return false, ds, err
		}
	}

	if uidSuffix != "" {
		applyUIDSuffix(&ds, uidSuffix)
	}

	if uidRemap != nil {
		applyUIDRemap(ds.Elements, uidRemap)
	}

	for _, e := range edits {
		newElem, err := buildElement(&ds, e)
		if err != nil {
			return false, ds, err
		}
		// Replace every occurrence at any nesting depth; append at the top level
		// only when the tag is absent throughout the dataset.
		if !replaceInElements(ds.Elements, newElem) {
			ds.Elements = append(ds.Elements, newElem)
		}
	}

	// Transfer syntax last, so the decompression's rewrite of the pixel-
	// describing attributes is what reaches disk. A source whose pixel data has
	// no built-in decoder fails the file rather than exporting it in a syntax
	// the profile did not ask for.
	if p.targetTS != "" {
		if _, err := convertDatasetSyntax(&ds, datasetTransferSyntaxUID(&ds), p.targetTS); err != nil {
			return false, ds, fmt.Errorf("convert to %s: %w", transferSyntaxLabel(p.targetTS), err)
		}
	}

	return false, ds, nil
}

// applyEdit replaces or inserts an element in ds for the given tagEdit.
func applyEdit(ds *sdicom.Dataset, e tagEdit) error {
	newElem, err := buildElement(ds, e)
	if err != nil {
		return err
	}

	for i, elem := range ds.Elements {
		if elem.Tag == e.tag {
			ds.Elements[i] = newElem
			return nil
		}
	}
	// Tag not present — append it.
	ds.Elements = append(ds.Elements, newElem)
	return nil
}

// buildElement creates a replacement *Element whose value encoding matches the
// VR of the existing element (or the standard VR if the tag is not present).
func buildElement(ds *sdicom.Dataset, e tagEdit) (*sdicom.Element, error) {
	// Determine the VR kind: prefer what's already in the dataset, fall back
	// to the standard tag dictionary.
	vrKind := tag.VRStringList
	if existing, err := ds.FindElementByTag(e.tag); err == nil {
		vrKind = existing.ValueRepresentation
	} else if info, err := tag.Find(e.tag); err == nil {
		vrKind = tag.GetVRKind(e.tag, info.VRs[0])
	}

	switch vrKind {
	case tag.VRUInt16List, tag.VRUInt32List, tag.VRInt16List, tag.VRInt32List:
		n, err := strconv.Atoi(e.value)
		if err != nil {
			return nil, fmt.Errorf("tag %s requires an integer value, got %q", e.tag, e.value)
		}
		return sdicom.NewElement(e.tag, []int{n})

	case tag.VRFloat32List, tag.VRFloat64List:
		f, err := strconv.ParseFloat(e.value, 64)
		if err != nil {
			return nil, fmt.Errorf("tag %s requires a float value, got %q", e.tag, e.value)
		}
		return sdicom.NewElement(e.tag, []float64{f})

	case tag.VRBytes:
		return sdicom.NewElement(e.tag, []byte(e.value))

	default:
		// VRStringList, VRString, VRDate, VRUnknown, etc.
		return sdicom.NewElement(e.tag, []string{e.value})
	}
}

// maxUIDLength is the maximum number of characters permitted in a DICOM UID
// (DICOM PS3.5 §9.1).
const maxUIDLength = 64

// applyUIDSuffix iterates every element in ds that carries a UI (UID) value
// and appends ".<suffix>" to it. If the resulting string would exceed
// maxUIDLength, the last dot-delimited component of the original UID is
// replaced with suffix instead.
func applyUIDSuffix(ds *sdicom.Dataset, suffix string) {
	for _, elem := range ds.Elements {
		if elem.RawValueRepresentation != "UI" {
			continue
		}
		// Transfer Syntax UIDs must not be modified — they describe the encoding
		// of the file itself and must remain valid, recognised UIDs.
		if elem.Tag == tag.TransferSyntaxUID || elem.Tag == tag.ReferencedTransferSyntaxUIDInFile {
			continue
		}
		vals, ok := elem.Value.GetValue().([]string)
		if !ok {
			continue
		}
		modified := make([]string, len(vals))
		for i, uid := range vals {
			candidate := uid + "." + suffix
			if len(candidate) <= maxUIDLength {
				modified[i] = candidate
			} else {
				// Replace the last component.
				if dot := strings.LastIndex(uid, "."); dot >= 0 {
					modified[i] = uid[:dot+1] + suffix
				} else {
					// No dot at all — just use the suffix directly.
					modified[i] = suffix
				}
			}
		}
		v, err := sdicom.NewValue(modified)
		if err != nil {
			continue
		}
		elem.Value = v
	}
}

// dicomOrgRoot prefixes all DICOM standard-defined UIDs (SOP Classes, Transfer
// Syntaxes, well-known UIDs). These describe object type/encoding, not the
// patient or study, and must never be remapped.
const dicomOrgRoot = "1.2.840.10008."

// uidRemapper assigns a fresh, stable replacement UID for each distinct source
// UID seen during a run. It is safe for concurrent use by the worker pool.
type uidRemapper struct {
	mu sync.Mutex
	m  map[string]string
}

func newUIDRemapper() *uidRemapper { return &uidRemapper{m: make(map[string]string)} }

// mapUID returns the replacement UID for original, generating and caching a new
// one on first sight so the same original always maps to the same replacement.
func (r *uidRemapper) mapUID(original string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.m[original]; ok {
		return v
	}
	v := generateUID()
	r.m[original] = v
	return v
}

// applyUIDRemap replaces every site-generated UI (UID) value at any nesting depth
// with its consistent remapped UID, leaving standard and structural UIDs
// untouched. Recursing into sequences keeps nested references (e.g.
// ReferencedSOPInstanceUID) consistent with the instances they point to.
func applyUIDRemap(elements []*sdicom.Element, r *uidRemapper) {
	for _, elem := range elements {
		if elem.Value != nil && elem.Value.ValueType() == sdicom.Sequences {
			if seqItems, ok := elem.Value.GetValue().([]*sdicom.SequenceItemValue); ok {
				for _, item := range seqItems {
					if itemElems, ok2 := item.GetValue().([]*sdicom.Element); ok2 {
						applyUIDRemap(itemElems, r)
					}
				}
			}
			continue
		}
		if elem.RawValueRepresentation != "UI" {
			continue
		}
		// Preserve structural/implementation UIDs: encoding and creating software,
		// not patient or study identity.
		if elem.Tag == tag.TransferSyntaxUID ||
			elem.Tag == tag.ReferencedTransferSyntaxUIDInFile ||
			elem.Tag == tag.ImplementationClassUID {
			continue
		}
		vals, ok := elem.Value.GetValue().([]string)
		if !ok {
			continue
		}
		changed := false
		out := make([]string, len(vals))
		for i, uid := range vals {
			if uid == "" || strings.HasPrefix(uid, dicomOrgRoot) {
				out[i] = uid // empty or standard UID (e.g. SOP Class) — keep
				continue
			}
			out[i] = r.mapUID(uid)
			changed = true
		}
		if changed {
			if v, err := sdicom.NewValue(out); err == nil {
				elem.Value = v
			}
		}
	}
}

// applyDateShift shifts every DA and DT element's leading YYYYMMDD date
// component by shiftDays (positive, negative, or zero) at any nesting depth.
// PatientBirthDate is always left untouched — that field is the dedicated
// responsibility of the dob mask, independent of shiftdays. Values that don't
// parse as a full 8-digit date are left unchanged.
func applyDateShift(elements []*sdicom.Element, shiftDays int) {
	for _, elem := range elements {
		if elem.Value != nil && elem.Value.ValueType() == sdicom.Sequences {
			if seqItems, ok := elem.Value.GetValue().([]*sdicom.SequenceItemValue); ok {
				for _, item := range seqItems {
					if itemElems, ok2 := item.GetValue().([]*sdicom.Element); ok2 {
						applyDateShift(itemElems, shiftDays)
					}
				}
			}
			continue
		}
		vr := elem.RawValueRepresentation
		if vr != "DA" && vr != "DT" {
			continue
		}
		if elem.Tag == tag.PatientBirthDate {
			continue
		}
		vals, ok := elem.Value.GetValue().([]string)
		if !ok {
			continue
		}
		changed := false
		out := make([]string, len(vals))
		for i, v := range vals {
			if shifted, ok := shiftDateString(v, shiftDays); ok {
				out[i] = shifted
				changed = true
			} else {
				out[i] = v
			}
		}
		if changed {
			if nv, err := sdicom.NewValue(out); err == nil {
				elem.Value = nv
			}
		}
	}
}

// shiftDateString shifts the leading YYYYMMDD component of v by shiftDays,
// preserving any trailing characters unchanged (the time/fraction/timezone
// suffix of a DT value). Returns ok=false — leave v unchanged — when v is
// shorter than 8 characters or the date portion fails to parse.
func shiftDateString(v string, shiftDays int) (string, bool) {
	if len(v) < 8 {
		return v, false
	}
	t, err := time.Parse("20060102", v[:8])
	if err != nil {
		return v, false
	}
	return t.AddDate(0, 0, shiftDays).Format("20060102") + v[8:], true
}

// generateUID returns a globally unique DICOM UID using the ISO 2.25 UUID
// root, matching dicomtool's UID generation.
func generateUID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(fmt.Sprintf("generateUID: %v", err))
	}
	var n big.Int
	n.SetBytes(b)
	return "2.25." + n.String()
}

// applyDOBMask reads the existing PatientBirthDate (0010,0030), applies mask,
// and writes the result back. Each mask character that is a digit (0-9)
// replaces the corresponding position; any other character preserves the
// original digit. Both the original value and mask must be 8 characters.
func applyDOBMask(ds *sdicom.Dataset, mask string) error {
	dobTag := tag.Tag{Group: 0x0010, Element: 0x0030}

	original := ""
	if elem, err := ds.FindElementByTag(dobTag); err == nil {
		if v, ok := elem.Value.GetValue().([]string); ok && len(v) > 0 {
			original = v[0]
		}
	}

	// Pad or truncate original to exactly 8 characters.
	src := []byte("00000000")
	copy(src, original)

	result := make([]byte, 8)
	for i := 0; i < 8; i++ {
		if mask[i] >= '0' && mask[i] <= '9' {
			result[i] = mask[i]
		} else {
			result[i] = src[i]
		}
	}

	return applyEdit(ds, tagEdit{tag: dobTag, value: string(result)})
}

// applyFixVR scans ds for elements whose VR does not match the DICOM standard
// dictionary and either removes them (mode "skip") or attempts to re-encode
// them with the correct VR (mode "correct", falling back to removal on failure).
// Sequence elements are recursed into so that nested elements are also fixed.
// Tags not found in the dictionary (private tags, unknown tags) are left as-is.
func applyFixVR(ds *sdicom.Dataset, mode string) {
	ds.Elements = fixVRElements(ds.Elements, mode)
}

// fixVRElements applies VR correction/removal to a flat element list, recursing
// into any sequence elements it encounters.
func fixVRElements(elements []*sdicom.Element, mode string) []*sdicom.Element {
	filtered := make([]*sdicom.Element, 0, len(elements))
	for _, elem := range elements {
		// Recurse into sequences before checking the element's own VR.
		if elem.Value != nil && elem.Value.ValueType() == sdicom.Sequences {
			seqItems, ok := elem.Value.GetValue().([]*sdicom.SequenceItemValue)
			if ok {
				rebuilt := make([][]*sdicom.Element, 0, len(seqItems))
				for _, item := range seqItems {
					itemElems, ok2 := item.GetValue().([]*sdicom.Element)
					if ok2 {
						rebuilt = append(rebuilt, fixVRElements(itemElems, mode))
					} else {
						rebuilt = append(rebuilt, nil)
					}
				}
				if newVal, err := sdicom.NewValue(rebuilt); err == nil {
					elem.Value = newVal
				}
			}
			filtered = append(filtered, elem)
			continue
		}

		if elem.RawValueRepresentation == "" {
			filtered = append(filtered, elem)
			continue
		}
		tagInfo, err := tag.Find(elem.Tag)
		if err != nil {
			// Unknown or private tag — cannot verify.
			filtered = append(filtered, elem)
			continue
		}
		vrOK := false
		for _, vr := range tagInfo.VRs {
			if vr == elem.RawValueRepresentation {
				vrOK = true
				break
			}
		}
		if vrOK {
			filtered = append(filtered, elem)
			continue
		}
		// VR mismatch.
		if mode == "skip" {
			continue
		}
		// mode == "correct": attempt re-encoding under the standard VR.
		corrected, cerr := rebuildWithCorrectVR(elem, tagInfo.VRs[0])
		if cerr != nil {
			continue
		}
		filtered = append(filtered, corrected)
	}
	return filtered
}

// pruneElements recursively drops elements whose tag is in removalSet, plus any
// odd-group (private) element when removePrivate is set, at every nesting depth.
// Sequence elements that are not themselves removed are recursed into and rebuilt
// so that nested identifiers are scrubbed as well. The removalSet lookup keeps
// this an O(elements) pass regardless of how many tags are being removed.
func pruneElements(elements []*sdicom.Element, removalSet map[tag.Tag]struct{}, removePrivate bool) []*sdicom.Element {
	filtered := elements[:0] // reuse backing array, matching the existing in-place style
	for _, elem := range elements {
		if removePrivate && elem.Tag.Group%2 == 1 {
			continue
		}
		if _, ok := removalSet[elem.Tag]; ok {
			continue // sequence removed wholesale; no recursion needed
		}
		if elem.Value != nil && elem.Value.ValueType() == sdicom.Sequences {
			if seqItems, ok := elem.Value.GetValue().([]*sdicom.SequenceItemValue); ok {
				rebuilt := make([][]*sdicom.Element, 0, len(seqItems))
				for _, item := range seqItems {
					if itemElems, ok2 := item.GetValue().([]*sdicom.Element); ok2 {
						rebuilt = append(rebuilt, pruneElements(itemElems, removalSet, removePrivate))
					} else {
						rebuilt = append(rebuilt, nil)
					}
				}
				if newVal, err := sdicom.NewValue(rebuilt); err == nil {
					elem.Value = newVal
				}
			}
		}
		filtered = append(filtered, elem)
	}
	return filtered
}

// replaceInElements replaces the value/VR of every element matching newElem.Tag
// at any nesting depth, returning true if at least one occurrence was replaced.
// Nested elements are shared *sdicom.Element pointers, so mutating their fields
// persists without rebuilding the sequence value.
func replaceInElements(elements []*sdicom.Element, newElem *sdicom.Element) bool {
	replaced := false
	for _, elem := range elements {
		if elem.Tag == newElem.Tag {
			elem.Value = newElem.Value
			elem.ValueRepresentation = newElem.ValueRepresentation
			elem.RawValueRepresentation = newElem.RawValueRepresentation
			replaced = true
			continue
		}
		if elem.Value != nil && elem.Value.ValueType() == sdicom.Sequences {
			if seqItems, ok := elem.Value.GetValue().([]*sdicom.SequenceItemValue); ok {
				for _, item := range seqItems {
					if itemElems, ok2 := item.GetValue().([]*sdicom.Element); ok2 {
						if replaceInElements(itemElems, newElem) {
							replaced = true
						}
					}
				}
			}
		}
	}
	return replaced
}

// rebuildWithCorrectVR creates a new element for elem.Tag using correctVR,
// converting the stored value to the appropriate Go type. Returns an error
// when the conversion is not possible.
func rebuildWithCorrectVR(elem *sdicom.Element, correctVR string) (*sdicom.Element, error) {
	vrKind := tag.GetVRKind(elem.Tag, correctVR)
	raw := elem.Value.GetValue()

	switch vrKind {
	case tag.VRStringList, tag.VRString, tag.VRDate:
		var strs []string
		switch v := raw.(type) {
		case []string:
			strs = v
		case []byte:
			for _, part := range strings.Split(strings.TrimRight(string(v), "\x00 "), `\`) {
				if s := strings.Trim(part, " \x00"); s != "" {
					strs = append(strs, s)
				}
			}
		default:
			strs = []string{strings.Trim(fmt.Sprintf("%v", v), "[]")}
		}
		return sdicom.NewElement(elem.Tag, strs)

	case tag.VRUInt16List, tag.VRUInt32List, tag.VRInt16List, tag.VRInt32List:
		switch v := raw.(type) {
		case []int:
			return sdicom.NewElement(elem.Tag, v)
		case []byte:
			var ints []int
			switch len(v) % 2 {
			case 0:
				for i := 0; i+1 < len(v); i += 2 {
					ints = append(ints, int(binary.LittleEndian.Uint16(v[i:])))
				}
			default:
				return nil, fmt.Errorf("byte length %d not a multiple of 2", len(v))
			}
			return sdicom.NewElement(elem.Tag, ints)
		case []string:
			var ints []int
			for _, s := range v {
				n, err := strconv.Atoi(strings.TrimSpace(s))
				if err != nil {
					return nil, fmt.Errorf("cannot parse %q as integer", s)
				}
				ints = append(ints, n)
			}
			return sdicom.NewElement(elem.Tag, ints)
		default:
			return nil, fmt.Errorf("cannot convert %T to integer list", raw)
		}

	case tag.VRFloat32List, tag.VRFloat64List:
		switch v := raw.(type) {
		case []float64:
			return sdicom.NewElement(elem.Tag, v)
		case []string:
			var floats []float64
			for _, s := range v {
				f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
				if err != nil {
					return nil, fmt.Errorf("cannot parse %q as float", s)
				}
				floats = append(floats, f)
			}
			return sdicom.NewElement(elem.Tag, floats)
		default:
			return nil, fmt.Errorf("cannot convert %T to float list", raw)
		}

	case tag.VRBytes:
		switch v := raw.(type) {
		case []byte:
			return sdicom.NewElement(elem.Tag, v)
		case []string:
			return sdicom.NewElement(elem.Tag, []byte(strings.Join(v, `\`)))
		default:
			return nil, fmt.Errorf("cannot convert %T to bytes", raw)
		}

	default:
		return nil, fmt.Errorf("unsupported VR kind for %s", correctVR)
	}
}

// dicomMagicOffset is the byte offset of the DICOM magic bytes.
const dicomMagicOffset = 128

// dicomMagic is the four-byte signature present in every valid DICOM file.
var dicomMagic = []byte{'D', 'I', 'C', 'M'}

// openDICOMFile opens path, verifies the DICOM magic bytes, seeks back to the
// start of the file, and returns the open *os.File. The caller must close it.
// Returns (nil, nil) when the file does not carry the DICOM magic signature.
func openDICOMFile(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, dicomMagicOffset+len(dicomMagic))
	n, err := io.ReadFull(f, buf)
	if err != nil || n < len(buf) {
		f.Close()
		return nil, nil
	}
	for i, b := range dicomMagic {
		if buf[dicomMagicOffset+i] != b {
			f.Close()
			return nil, nil
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
