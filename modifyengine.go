package main

// Modification engine — applies a de-identification profile to DICOM files.
// Ported from the dicomtool CLI's modify command so both tools transform files
// identically. Per-file order of operations (matching dicomtool):
//
//	parse → ignoretype → ignoremodality → per-modality overrides → fixvr →
//	remove + noprivate + nooverlays → date shift → dob mask → uid remap → set →
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
	"runtime/debug"
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
	edits    []tagEdit
	removals []tag.Tag
	dobMask  string
	// shiftDays stays a string: "" means no shift while "0" is an accepted
	// (no-op) action, matching dicomtool. Atoi-validated by compileModifyParams.
	shiftDays     string
	fixvrMode     string
	removePrivate bool
	// removeOverlays drops every overlay-plane group (6000–60FE, even) — the
	// bitmap channel a vendor can burn annotations into that no per-tag rule
	// reaches practically (16 repeating groups) and pixel masking cannot touch.
	// Profile-wide, like noprivate is in the editor.
	removeOverlays   bool
	ignoreTypes      []string
	ignoreModalities []string
	perMod           map[string]modalityOverride
	remapUIDs        bool
	// targetTS is the transfer syntax UID every output is written in, or "" to
	// write each file in the syntax it was stored in. Profile-wide: unlike the
	// tag rules it is never overridden per modality.
	targetTS string
	// maskRegions blanks burned-in PHI in the pixels themselves; per-modality
	// overrides replace it wholesale. mayMask is true when this profile or any
	// of its overrides can mask, which decides the write options for the run —
	// masking may force a decompression the profile did not ask for.
	maskRegions []MaskRegion
	mayMask     bool
	// dicomdir requests a DICOMDIR (PS3.10 File-set) index alongside the
	// export — one per run, referencing every file actually written, built
	// from dicomdirSource records the run collects as files succeed. Unlike
	// zip (a pure destination choice made in modifydialog.go, never threaded
	// through modifyParams) this is an engine-level option both
	// runModification and runModificationToZip honor identically.
	dicomdir bool
}

// compileModifyParams validates p and parses its tag references into a
// modifyParams. The validation rules match dicomtool's modify command exactly.
func compileModifyParams(p ModProfile) (modifyParams, error) {
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
	// The UID suffix option is gone — Remap UIDs replaced it outright. A profile
	// still carrying one is refused rather than run: silently ignoring the entry
	// would export original UIDs from a profile whose author asked for them
	// changed, which is exactly the quiet no-op a de-identification engine must
	// not have. The stored `uid` value itself is preserved by load/save so the
	// entry can be seen and deleted, never stripped behind the user's back.
	if strings.TrimSpace(p.UIDSuffix) != "" {
		return mp, errors.New("the uid suffix option has been removed — use Remap UIDs instead")
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
		t, err := parseTagString(strings.TrimSpace(r))
		if err != nil {
			return mp, fmt.Errorf("invalid remove tag %q: %w", r, err)
		}
		mp.removals = append(mp.removals, t)
	}

	// Resolve set-value references first, so nothing downstream — buildElement
	// least of all — ever sees a "[GGGG,EEEE]" placeholder, whichever path
	// reached here.
	sets, err := resolveSetReferences(p.Sets)
	if err != nil {
		return mp, err
	}

	mp.edits = make([]tagEdit, 0, len(sets))
	for _, s := range sets {
		tagStr, value, ok := strings.Cut(s, "=")
		if !ok || tagStr == "" {
			return mp, fmt.Errorf("invalid set value %q: expected <tag>=<value>", s)
		}
		t, err := parseTagString(strings.TrimSpace(tagStr))
		if err != nil {
			return mp, fmt.Errorf("invalid tag %q: %w", tagStr, err)
		}
		mp.edits = append(mp.edits, tagEdit{tag: t, value: value})
	}

	mp.removePrivate = p.Priv
	mp.removeOverlays = p.NoOverlays

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

	// Mask regions are validated rather than dropped on the floor, at the top
	// level and inside every override: buildModalityOverrides skips unparsable
	// tag entries for dicomtool parity, but a mask rule that silently does
	// nothing exports the PHI it was written to remove.
	if err := validateMaskRegions(p.MaskRegions); err != nil {
		return mp, err
	}
	mp.maskRegions = p.MaskRegions
	mp.mayMask = len(p.MaskRegions) > 0

	mp.dicomdir = p.Dicomdir

	if len(p.PerModality) > 0 {
		normalized := make(map[string]ModProfile, len(p.PerModality))
		for k, v := range p.PerModality {
			modKey := strings.ToUpper(k)
			if err := validateMaskRegions(v.MaskRegions); err != nil {
				return mp, fmt.Errorf("modality %s: %w", modKey, err)
			}
			// The same refusal as the top level: a hand-authored per-modality
			// uid entry would otherwise become a silent no-op.
			if strings.TrimSpace(v.UIDSuffix) != "" {
				return mp, fmt.Errorf("modality %s: the uid suffix option has been removed — use Remap UIDs instead", modKey)
			}
			if len(v.MaskRegions) > 0 {
				mp.mayMask = true
			}
			normalized[modKey] = v
		}
		mp.perMod = buildModalityOverrides(normalized)
	}

	// A transfer syntax alone is actionable: converting a study to an
	// uncompressed syntax is a legitimate standalone operation, and export is
	// the only place the application can perform one.
	hasAction := len(mp.edits) > 0 || len(mp.removals) > 0 ||
		mp.dobMask != "" || mp.shiftDays != "" ||
		mp.removePrivate || mp.removeOverlays || mp.fixvrMode != "" || mp.targetTS != "" ||
		len(mp.ignoreTypes) > 0 || len(mp.ignoreModalities) > 0 ||
		len(mp.perMod) > 0 || mp.remapUIDs || mp.mayMask || mp.dicomdir
	if !hasAction {
		return mp, errors.New("the profile contains no actionable parameter (set, remove, dob, shiftdays, noprivate, nooverlays, fixvr, remapuids, transfersyntax, maskregions, dicomdir)")
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
	// MaskDecompressed counts files whose compressed pixel data had to be
	// decompressed so that pixel masking could be applied, when the profile
	// requested no conversion of its own, and which could not be recompressed
	// (lossy source, or the lossless re-encode failed). Reported rather than
	// left silent: those files leave in a different encoding than the profile
	// states.
	MaskDecompressed int
	// MaskRecompressed counts masked files whose pixels were re-encoded back
	// into their original compressed transfer syntax, verified bit-identical —
	// the export keeps the encoding it arrived in.
	MaskRecompressed int
	// MaskRecodedLossless counts masked files whose lossy source syntax was
	// re-encoded to JPEG 2000 Lossless instead: no loss added beyond the
	// decode masking forced, export stays compressed, syntax change disclosed.
	MaskRecodedLossless int
	// MaskUSFallback counts ultrasound files that declared no calibrated region
	// and were masked with the profile's manual rectangles instead of their own
	// stated geometry — a weaker guarantee, so the run says how many.
	MaskUSFallback int
	// NestedDOBKept counts exported files that still carry a birth date inside
	// a sequence after a birth-date mask was applied. The mask rewrites the
	// top-level element only, so this is the evidence — as opposed to the
	// profile-shape advisory in the editors — that an export actually carries
	// more than the profile implies.
	NestedDOBKept int
	// DicomdirWritten reports whether a DICOMDIR (PS3.10 File-set) index was
	// requested and written alongside the export.
	DicomdirWritten bool
	// DicomdirError holds the reason DICOMDIR generation failed when it was
	// requested but not written. The exported DICOM files are unaffected —
	// this never turns an otherwise successful run into a failed one, the same
	// tier as the mask-outcome fields above.
	DicomdirError string
}

// exportNames holds every value an export path component is built from, as a
// file carried them at one moment — see readExportNames and exportLayout.
type exportNames struct {
	studyDesc, studyDate     string
	seriesDesc, seriesNumber string
	sopUID                   string
}

// readExportNames reads the tag values export path components are built
// from. processFile calls it once right after parsing (the file's original
// values) and again just before a successful return (the values after every
// attribute step), so exportLayout.relFor can tell which components the
// profile actually changed.
func readExportNames(ds *sdicom.Dataset) exportNames {
	return exportNames{
		studyDesc:    datasetString(ds, tag.StudyDescription),
		studyDate:    datasetString(ds, tag.StudyDate),
		seriesDesc:   datasetString(ds, tag.SeriesDescription),
		seriesNumber: datasetString(ds, tag.SeriesNumber),
		sopUID:       datasetString(ds, tag.SOPInstanceUID),
	}
}

func sameStudyName(a, b exportNames) bool {
	return a.studyDesc == b.studyDesc && a.studyDate == b.studyDate
}

func sameSeriesName(a, b exportNames) bool {
	return a.seriesDesc == b.seriesDesc && a.seriesNumber == b.seriesNumber
}

// renamedFile returns original unchanged unless the SOP Instance UID changed
// between before and after, in which case the file is renamed after the new
// UID (the original extension kept), so no original UID survives an export
// that remapped it.
func renamedFile(original string, before, after exportNames) string {
	if after.sopUID == "" || after.sopUID == before.sopUID {
		return original
	}
	return sanitize(after.sopUID) + filepath.Ext(original)
}

// sourceRel maps srcPath to its path relative to rootDir, falling back to the
// bare file name when it is not (usefully) inside rootDir. This is what a nil
// *exportLayout uses outright, and what exportLayout.relFor starts from.
func sourceRel(srcPath, rootDir string) string {
	rel, err := filepath.Rel(rootDir, srcPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return filepath.Base(srcPath)
	}
	return rel
}

// exportLayout maps a source file's path into its path under the export
// root. The export root already stands in for the leading dropDirs source
// folder components — 1 (patient only) for a patient-level run, 2 (patient
// and study) for a study-level run, see showModificationDialog — because the
// user types that folder's name directly in the Modification dialog. Every
// folder below it keeps its source name, unless the tag(s) that name is
// built from changed between before and after, in which case the component
// is rebuilt from the new values with the same rules organizeFilePath uses
// to build the download folder; the file name is rebuilt from the SOP
// Instance UID the same way. A nil *exportLayout (used by the engine's own
// tests) skips all of this and mirrors sourceRel with no renaming.
//
// flat drops every folder below the root: every file is written directly
// into the export root (or the archive root, with Zip export), named after
// its SOP Instance UID rather than kept under its source name — the
// hierarchy a source name relied on for context is exactly what flat
// removes, so keeping the source name would reintroduce the collisions the
// hierarchy existed to prevent (two series each holding a "1.dcm", say).
// names deduplicates what flatFileName cannot rule out by construction.
type exportLayout struct {
	dropDirs int
	flat     bool
	names    *flatNames
}

// flatNames deduplicates the file names a flat exportLayout hands out. A SOP
// Instance UID is unique per instance, so a collision here means a
// genuinely duplicated instance, or a file with no SOP Instance UID at all
// falling back to its (possibly non-unique) source name — rare either way.
// The second file to claim a name gets a " (2)" suffix rather than silently
// overwriting the first (folder mode) or landing as an indistinguishable
// second archive entry (zip mode); which of the two duplicates gets the
// suffix depends on worker completion order, but both are still written in
// full. A nil *flatNames reserves nothing and returns every name unchanged —
// what a flat exportLayout gets if it does not set one, and what every
// non-flat exportLayout uses implicitly by never calling reserve at all.
type flatNames struct {
	mu    sync.Mutex
	taken map[string]bool
}

func (n *flatNames) reserve(name string) string {
	if n == nil {
		return name
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.taken == nil {
		n.taken = make(map[string]bool)
	}
	key := strings.ToLower(name)
	if !n.taken[key] {
		n.taken[key] = true
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if key := strings.ToLower(candidate); !n.taken[key] {
			n.taken[key] = true
			return candidate
		}
	}
}

// flatFileName names a flat-mode export entry after its SOP Instance UID —
// the same naming organizeFilePath already gives every file in the download
// folder, so a source file's existing name and its flat-export name coincide
// in the ordinary case. Flat mode applies it unconditionally, not only when
// the UID changed as renamedFile does, since the folder context a kept
// source name relied on no longer exists in a flat export to disambiguate
// it. The extension is always literal ".dcm" rather than derived from the
// source path, which also means the result can never be nothing but dots —
// unlike a folder name (see safePathComponent) a bare sanitize(sopUID)
// could be, and the fixed suffix means this never needs that guard. fallback
// (renamedFile's ordinary result) covers the one case a SOP Instance UID
// cannot: a file that does not carry one at all.
func flatFileName(after exportNames, fallback string) string {
	name := sanitize(after.sopUID) + ".dcm"
	if name == ".dcm" {
		return fallback
	}
	return name
}

// relFor returns f's path under the export root. before and after are the
// export name fields as processFile read them right after parsing and right
// before returning — see fileNotes.namesBefore/namesAfter.
func (l exportLayout) relFor(srcPath, rootDir string, before, after exportNames) string {
	rel := sourceRel(srcPath, rootDir)
	parts := strings.Split(rel, string(filepath.Separator))
	last := len(parts) - 1

	if l.flat {
		return l.names.reserve(flatFileName(after, renamedFile(parts[last], before, after)))
	}

	// Shallower than patient/study/file — the shallow-source fallback (a
	// bare file name) lands here too, along with any path too short to
	// carry a study or series component at all.
	if last < 2 {
		return renamedFile(parts[last], before, after)
	}

	// Absolute source index: 0 = patient, 1 = study, 2 = series, deeper =
	// copied verbatim. index 1 is only ever the study folder (rather than
	// the file itself) when there is at least one component after it;
	// likewise index 2 for the series folder.
	if l.dropDirs == 1 && !sameStudyName(before, after) {
		parts[1] = studyFolderName(after.studyDesc, after.studyDate)
	}
	if last > 2 && !sameSeriesName(before, after) {
		parts[2] = seriesFolderName(after.seriesDesc, after.seriesNumber)
	}
	parts[last] = renamedFile(parts[last], before, after)
	return filepath.Join(parts[l.dropDirs:]...)
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

// writeRaw serialises data directly into a new zip entry, with none of
// write's dataset encoding — used for the DICOMDIR entry, whose bytes are
// already a complete, offset-patched DICOM file (see dicomdir.go).
func (z *zipSink) writeRaw(rel string, data []byte) error {
	z.mu.Lock()
	defer z.mu.Unlock()
	w, err := z.zw.Create(filepath.ToSlash(rel))
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// runModification applies params to every file in files, writing results under
// outDir. Each file's output path within outDir comes from layout (built from
// processFile's before/after exportNames — see exportLayout.relFor); a nil
// layout falls back to the file's path relative to rootDir (mirroring the
// download-folder layout), which is what the engine's own tests use. Per-file
// failures are collected and never abort the run; progress is reported after
// every file. Cancelling ctx stops feeding new files; files already in flight
// complete.
func runModification(ctx context.Context, files []string, rootDir, outDir string,
	params modifyParams, layout *exportLayout, progress func(done, total int)) modifyResult {
	return runModificationImpl(ctx, files, rootDir, outDir, params, layout, progress, nil)
}

// runModificationToZip runs the same pipeline as runModification but writes
// every output into a single zip archive at zipPath, with entry paths laid
// out exactly as the folder export would be (layout, forward-slashed). The
// archive is built as a hidden temp file and renamed into place when the run
// ends with at least one file written — including a cancelled run, which
// keeps the files completed before the cancel, mirroring folder-mode
// semantics. A run that writes nothing leaves no archive behind, and a
// finalize or rename failure converts the run's written count into failures,
// since the archive holding those files is lost with it.
func runModificationToZip(ctx context.Context, files []string, rootDir, zipPath string,
	params modifyParams, layout *exportLayout, progress func(done, total int)) modifyResult {

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

	res := runModificationImpl(ctx, files, rootDir, "", params, layout, progress, &zipSink{zw: zw})

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
		// The archive itself is gone, so any DICOMDIR entry it held goes with
		// it — a written-then-lost index must not be reported as written.
		res.DicomdirWritten = false
		res.DicomdirError = ""
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
	params modifyParams, layout *exportLayout, progress func(done, total int), zsink *zipSink) modifyResult {

	var res modifyResult
	if len(files) == 0 {
		return res
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
	numWorkers := min(runtime.NumCPU(), 4, len(files))

	// A run that can decompress is admitted by weight as well as by count. Four
	// workers is the right width for tag edits, where a file costs what it costs
	// on disk, but decompressing a multi-frame acquisition can cost a hundred
	// times that — and running out of memory kills the export outright instead
	// of failing one file. A run that touches no pixels gets no limiter and
	// behaves exactly as before, down to not paying for the header read below.
	var budget *memBudget
	if params.mayMask || params.targetTS != "" {
		budget = newMemBudget(modifyMemoryBudget)
		logInfo("modify: memory budget %d MB for in-flight pixel data across %d worker(s)",
			modifyMemoryBudget>>20, numWorkers)
	}

	var (
		mu   sync.Mutex
		done int
	)
	total := len(files)
	recordFailure := func(path string, err error) {
		logError("modify: failed %s: %v", path, err)
		mu.Lock()
		res.Failed++
		res.Failures = append(res.Failures, modifyFailure{File: path, Error: err.Error()})
		mu.Unlock()
	}

	// DICOMDIR sources are collected from the in-memory transformed dataset of
	// every successfully written file, under their own mutex — ddSources is
	// only ever read after wg.Wait(), but is written concurrently by every
	// worker as files finish.
	var (
		ddMu      sync.Mutex
		ddSources []dicomdirSource
	)
	recordDicomdirSource := func(ds sdicom.Dataset, rel string) {
		if !params.dicomdir {
			return
		}
		src := extractDicomdirSource(&ds, rel)
		ddMu.Lock()
		ddSources = append(ddSources, src)
		ddMu.Unlock()
	}

	jobCh := make(chan string)
	var wg sync.WaitGroup
	for range numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobCh {
				func() {
					// partial is the output file while it is mid-write, so a
					// panic can take the half-written file with it rather than
					// leaving a truncated study member in the export. The handle
					// has to be closed before the remove: Windows will not delete
					// an open file, and a panic skips the normal Close.
					var partial *os.File

					// Backstop. safeParse already turns a parse panic into an
					// ordinary per-file error, but decoding, masking, re-encoding
					// and writing all reach library and cgo code that can panic
					// on a malformed file too — and on a worker goroutine that
					// kills the process. The engine already has a "this file
					// failed, the run continues" concept, so a panic becomes one
					// more entry in the failure dialog, naming the file.
					//
					// A zip run is the one case this cannot fully clean up: the
					// archive keeps whatever fragment of the entry was written
					// before the panic. The file is still reported as failed, so
					// a member that will not open is accounted for rather than
					// silently present.
					//
					// recordFailure takes mu, which is safe here: every mu
					// critical section in this closure is a bare counter
					// increment, so a panic can never be raised while it is held.
					defer func() {
						if r := recover(); r != nil {
							if partial != nil {
								name := partial.Name()
								partial.Close()
								os.Remove(name)
							}
							recordFailure(path, fmt.Errorf("panic: %v\n%s", r, debug.Stack()))
						}
					}()

					srcFile, ferr := openDICOMFile(path)
					if ferr != nil {
						recordFailure(path, fmt.Errorf("open: %w", ferr))
						return
					}
					if srcFile == nil { // no DICM magic — not a DICOM file
						mu.Lock()
						res.Skipped++
						mu.Unlock()
						return
					}
					// Weigh the file before allocating anything for it. The
					// header read never touches pixel data, so it stays cheap
					// on exactly the large files this exists to hold back, and
					// the reservation is held until the write has finished —
					// the decoded frames stay alive until sdicom.Write has
					// serialised them.
					//
					// A header that will not parse weighs nothing and goes
					// through: processFile will fail it properly a moment
					// later, and inventing a weight for a file we cannot read
					// would be worse than not gating it.
					if budget != nil {
						defer budget.acquire(fileMemoryWeight(path))()
					}
					skipped, ds, notes, perr := processFileFn(srcFile, params, uidRemap)
					if perr != nil {
						recordFailure(path, fmt.Errorf("process: %w", perr))
						return
					}
					if notes.maskRecompressErr != "" {
						// The file still exported (uncompressed), so a warning
						// rather than a failure — but the reason must be on
						// record, since the encoding differs from the source's.
						logWarn("modify: %s masked, but %s — exported as %s instead",
							path, notes.maskRecompressErr, transferSyntaxLabel(tsExplicitVRLE))
					}
					if notes.maskRecompressed {
						logInfo("modify: %s recompressed to its original transfer syntax after masking",
							path)
						mu.Lock()
						res.MaskRecompressed++
						mu.Unlock()
					}
					if notes.maskRecodedLossless {
						logInfo("modify: %s re-encoded to %s after masking — its own syntax is lossy, and the lossless encode adds no further loss",
							path, transferSyntaxLabel(tsJPEG2000LL))
						mu.Lock()
						res.MaskRecodedLossless++
						mu.Unlock()
					}
					if notes.maskDecompressed {
						logInfo("modify: %s decompressed to %s so its burned-in pixels could be masked",
							path, transferSyntaxLabel(tsExplicitVRLE))
						mu.Lock()
						res.MaskDecompressed++
						mu.Unlock()
					}
					if notes.maskUSFallback {
						// Warning, not info: this file was masked by generic
						// geometry because it did not state its own, which is a
						// weaker guarantee than the rest of the export carries.
						logWarn("modify: %s declares no calibrated ultrasound region — masked with the profile's rectangles instead",
							path)
						mu.Lock()
						res.MaskUSFallback++
						mu.Unlock()
					}
					if notes.nestedDOBKept {
						// Warning, not info: the export carries a birth date the
						// profile's mask did not reach, which is the one thing a
						// masked export is assumed not to contain.
						logWarn("modify: %s still carries a birth date inside a sequence — the birth date mask rewrites the top-level element only",
							path)
						mu.Lock()
						res.NestedDOBKept++
						mu.Unlock()
					}
					if skipped {
						mu.Lock()
						res.Skipped++
						mu.Unlock()
						return
					}
					rel := sourceRel(path, rootDir)
					if layout != nil {
						rel = layout.relFor(path, rootDir, notes.namesBefore, notes.namesAfter)
					}
					if zsink != nil {
						if zerr := zsink.write(rel, ds, writeOpts); zerr != nil {
							recordFailure(path, fmt.Errorf("zip write: %w", zerr))
						} else {
							mu.Lock()
							res.Processed++
							mu.Unlock()
							recordDicomdirSource(ds, rel)
						}
						return
					}
					outFile := filepath.Join(outDir, rel)
					if merr := os.MkdirAll(filepath.Dir(outFile), 0o755); merr != nil {
						recordFailure(path, fmt.Errorf("create output dir: %w", merr))
						return
					}
					f, cerr := os.Create(outFile)
					if cerr != nil {
						recordFailure(path, fmt.Errorf("create output file: %w", cerr))
						return
					}
					// From here until the write completes, this file is
					// incomplete on disk; the deferred recover closes and removes
					// it if the encode panics.
					partial = f
					bw := bufio.NewWriterSize(f, 1<<20)
					werr := sdicom.Write(bw, ds, writeOpts...)
					fherr := bw.Flush()
					clerr := f.Close()
					partial = nil
					switch {
					case werr != nil:
						recordFailure(path, fmt.Errorf("write: %w", werr))
					case fherr != nil:
						recordFailure(path, fmt.Errorf("flush: %w", fherr))
					case clerr != nil:
						recordFailure(path, fmt.Errorf("close: %w", clerr))
					default:
						mu.Lock()
						res.Processed++
						mu.Unlock()
						recordDicomdirSource(ds, rel)
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
	for _, path := range files {
		select {
		case <-ctx.Done():
			res.Canceled = true
			break feed
		case jobCh <- path:
		}
	}
	close(jobCh)
	wg.Wait()

	// DICOMDIR is built once, after every worker has finished, from whichever
	// files actually succeeded — a cancelled or partially-failed run still
	// gets an index for what it did write, matching runModificationToZip's
	// existing partial-success semantics for the archive itself.
	if params.dicomdir && len(ddSources) > 0 {
		data, err := buildDICOMDIRBytes(ddSources)
		switch {
		case err != nil:
			res.DicomdirError = err.Error()
			logWarn("modify: DICOMDIR generation failed: %v", err)
		case zsink != nil:
			if werr := zsink.writeRaw("DICOMDIR", data); werr != nil {
				res.DicomdirError = werr.Error()
				logWarn("modify: writing DICOMDIR into the archive failed: %v", werr)
			} else {
				res.DicomdirWritten = true
				logInfo("modify: DICOMDIR index written into the archive")
			}
		default:
			if werr := writeDICOMDIRFile(outDir, data); werr != nil {
				res.DicomdirError = werr.Error()
				logWarn("modify: writing DICOMDIR failed: %v", werr)
			} else {
				res.DicomdirWritten = true
				logInfo("modify: DICOMDIR index written to %s", outDir)
			}
		}
	}

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
	// Masking relaxes verification for the same reason a conversion does: it
	// can force a compressed file to be written out uncompressed, and emitting
	// Explicit VR for elements parsed under Implicit VR fails files that write
	// fine untouched.
	if p.targetTS == "" && !p.mayMask {
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
	shiftDays     string
	fixvrMode     string
	removePrivate bool
	keepPrivate   bool
	// maskRegions replaces the profile's regions outright for this modality
	// rather than adding to them — see mergeModProfiles.
	maskRegions []MaskRegion
}

// buildModalityOverrides converts the per-modality profile map (already keyed
// in uppercase) into pre-parsed modalityOverride values ready for runtime use.
// Entries with unparsable tags are silently dropped, matching dicomtool.
func buildModalityOverrides(perMod map[string]ModProfile) map[string]modalityOverride {
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
			t, err := parseTagString(tagStr)
			if err != nil {
				continue
			}
			ov.edits = append(ov.edits, tagEdit{tag: t, value: value})
		}
		for _, r := range p.Removes {
			t, err := parseTagString(r)
			if err != nil {
				continue
			}
			ov.removals = append(ov.removals, t)
		}
		for _, k := range p.Keep {
			t, err := parseTagString(k)
			if err != nil {
				continue
			}
			ov.keep = append(ov.keep, t)
		}
		ov.dobMask = p.DOB
		ov.shiftDays = p.ShiftDays
		ov.fixvrMode = p.FixVR
		ov.removePrivate = p.Priv
		ov.keepPrivate = p.KeepPrivate
		ov.maskRegions = p.MaskRegions
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
// fileNotes records what processFile had to do beyond the profile's literal
// instructions — things the run reports because the export differs from what
// the profile alone describes — plus what it observed, for the caller to
// route the output through exportLayout.
type fileNotes struct {
	// namesBefore/namesAfter are the export path components' source tag
	// values as the file carried them right after parsing and again right
	// before a successful return, so exportLayout.relFor can tell which
	// folder-name and file-name components the profile actually changed.
	// Left zero on a skip or failure, neither of which reaches relFor.
	namesBefore, namesAfter exportNames
	// maskDecompressed: compressed pixel data was decompressed so masking could
	// write to it, without the profile requesting a conversion, and could not
	// be recompressed (lossy source, or recompression failed) — the file left
	// as Explicit VR LE.
	maskDecompressed bool
	// maskRecompressed: the decompression masking forced was undone on the way
	// out — the masked frames were re-encoded, verified bit-identical, back
	// into the file's own transfer syntax.
	maskRecompressed bool
	// maskRecodedLossless: the file's own syntax is lossy, so the masked
	// frames were re-encoded to JPEG 2000 Lossless instead — no loss added
	// beyond the decode masking forced, but the syntax changed, so it is
	// counted apart from the round-trip case.
	maskRecodedLossless bool
	// maskRecompressErr: recompression to the source syntax was attempted and
	// failed; the file fell back to Explicit VR LE (maskDecompressed) and the
	// worker logs this reason.
	maskRecompressErr string
	// maskUSFallback: an ultrasound image declared no calibrated region, so the
	// profile's manual rectangles masked it instead of its own stated geometry.
	maskUSFallback bool
	// nestedDOBKept: a birth-date mask was applied and a birth date still
	// survives inside a sequence in the exported file — see hasNestedTag.
	nestedDOBKept bool
}

// hasNestedTag reports whether t appears anywhere BELOW the top level of
// elements, descending through sequence items to any depth. The top level is
// deliberately excluded: the caller is asking what the recursing transforms
// would have reached and the non-recursing ones did not.
func hasNestedTag(elements []*sdicom.Element, t tag.Tag) bool {
	for _, elem := range elements {
		if elem.Value == nil || elem.Value.ValueType() != sdicom.Sequences {
			continue
		}
		items, ok := elem.Value.GetValue().([]*sdicom.SequenceItemValue)
		if !ok {
			continue
		}
		for _, item := range items {
			sub, ok := item.GetValue().([]*sdicom.Element)
			if !ok {
				continue
			}
			for _, e := range sub {
				if e.Tag == t {
					return true
				}
			}
			if hasNestedTag(sub, t) {
				return true
			}
		}
	}
	return false
}

// modifyMemoryOverheadFactor scales a file's projected pixel size into what
// processing it actually costs at peak. Masking a compressed file holds three
// things at once: the frames decompressPixelData decoded, the frames
// recompressPixelData encoded from them, and — via pixelStateSnapshot — the
// original codestream, kept so a file nothing masked can be restored
// byte-identical. The decoded frames dominate, and the rest come to roughly as
// much again, so the projection is about half the real peak.
const modifyMemoryOverheadFactor = 2

// fileMemoryWeight is what one file should reserve from the run's budget: the
// memory its pixels will occupy decoded, with the overhead above. Reads the
// header only (SkipPixelData never touches the pixels), so it stays cheap on
// precisely the files it is there to weigh. Returns 0 for a file it cannot read
// or one with no pixel data — see the call site.
func fileMemoryWeight(path string) int64 {
	ds, err := safeParseFile(path, nil, sdicom.SkipPixelData())
	if err != nil {
		return 0
	}
	return projectedPixelBytes(&ds) * modifyMemoryOverheadFactor
}

// processFileFn is the per-file transform the worker pool calls, a variable so
// tests can substitute a panicking implementation and prove the worker's
// backstop turns it into a recorded failure — the same seam encodeMaskedFrame
// uses in recompress.go.
var processFileFn = processFile

func processFile(src *os.File, p modifyParams, uidRemap *uidRemapper) (skipped bool, ds sdicom.Dataset, notes fileNotes, err error) {
	// Backstop for the panic path: the parser panics on some malformed
	// datasets, and the handle has to be released then too — which is what
	// makes the "src is always closed" contract above true. The explicit close
	// after the parse still runs on every normal path, so the handle is not
	// held open across the transform; closing twice is harmless.
	defer src.Close()

	// Copy to locals: per-modality overrides layer onto these per file, and p's
	// slices are shared across concurrent workers.
	edits := p.edits
	removals := p.removals
	dobMask := p.dobMask
	shiftDaysStr := p.shiftDays
	fixvrMode := p.fixvrMode
	removePrivate := p.removePrivate
	maskRegions := p.maskRegions

	info, err := src.Stat()
	if err != nil {
		return false, ds, notes, fmt.Errorf("stat: %w", err)
	}
	br := bufio.NewReaderSize(src, 1<<20)
	ds, err = safeParse(br, info.Size(), src.Name(), nil)
	src.Close()
	if err != nil {
		return false, ds, notes, fmt.Errorf("parse: %w", err)
	}
	notes.namesBefore = readExportNames(&ds)

	// Masking runs last but keys on the file as it arrived, so its identity is
	// captured here, before anything below can rewrite it. UID remapping
	// replaces SOP Instance UID and a removal rule can delete Modality or the
	// ultrasound region sequence — resolve a mask against the transformed
	// dataset and every image-scoped region quietly stops matching the image it
	// was drawn on.
	maskSrc := newMaskSource(&ds)

	if len(p.ignoreTypes) > 0 {
		if elem, err := ds.FindElementByTag(imageTypeTag); err == nil {
			for _, component := range elemStringComponents(elem) {
				for _, ignore := range p.ignoreTypes {
					if strings.EqualFold(component, strings.TrimSpace(ignore)) {
						return true, ds, notes, nil
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
						return true, ds, notes, nil
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
					// Replace, never append: a modality's regions describe that
					// modality's screen layout, which has nothing to do with
					// the profile-wide geometry they stand in for.
					if len(ov.maskRegions) > 0 {
						maskRegions = ov.maskRegions
					}
					break
				}
			}
		}
	}

	if fixvrMode == "correct" || fixvrMode == "skip" {
		applyFixVR(&ds, fixvrMode)
	}

	if removePrivate || p.removeOverlays || len(removals) > 0 {
		removalSet := make(map[tag.Tag]struct{}, len(removals))
		for _, t := range removals {
			removalSet[t] = struct{}{}
		}
		ds.Elements = pruneElements(ds.Elements, removalSet, removePrivate, p.removeOverlays)
	}

	if shiftDaysStr != "" {
		// Re-validated defensively: compileModifyParams checks the top-level
		// value, but a hand-authored per-modality block can carry garbage.
		// Failing the file (rather than silently skipping the shift) matches
		// dicomtool and keeps real dates from shipping unnoticed.
		n, err := strconv.Atoi(shiftDaysStr)
		if err != nil {
			return false, ds, notes, fmt.Errorf("shiftdays %q must be an integer", shiftDaysStr)
		}
		applyDateShift(ds.Elements, n)
	}

	if dobMask != "" {
		if err := applyDOBMask(&ds, dobMask); err != nil {
			return false, ds, notes, err
		}
	}

	if uidRemap != nil {
		applyUIDRemap(ds.Elements, uidRemap)
	}

	for _, e := range edits {
		newElem, err := buildElement(&ds, e)
		if err != nil {
			return false, ds, notes, err
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
			return false, ds, notes, fmt.Errorf("convert to %s: %w", transferSyntaxLabel(p.targetTS), err)
		}
	}

	// Pixel masking comes after the conversion, not before it: masking writes
	// sample values and needs them native, which the conversion is what
	// produces. It writes no attribute, so the conversion still has the final
	// say over everything describing the pixels.
	//
	// Masking a compressed file forces a decompression even when the profile
	// asked for none — the alternative is to fail every compressed file. When
	// the source syntax is lossless (JPEG 2000 Lossless, JPEG Lossless) the
	// masked frames are recompressed straight back into it, verified
	// bit-identical, so the export keeps the encoding it arrived in. A lossy
	// source is re-encoded to JPEG 2000 Lossless instead — returning to the
	// lossy syntax would degrade every pixel a second time, while the
	// lossless encode adds nothing beyond the decode masking forced. Only a
	// source with no encoder path, or an encode that fails, leaves as
	// Explicit VR Little Endian. Each outcome has its own count in the run
	// summary — an export whose encoding changed never does so silently.
	//
	// Which is why the regions are resolved against the header FIRST, before a
	// single frame is decoded. A profile whose rectangles are scoped to a few
	// images would otherwise decompress and re-encode every other file for
	// nothing: measured on a 177-file echo study with one image-scoped
	// rectangle, that was 23 of every 25 files decompressed, a tenfold export
	// and minutes of CPU spent producing pixels identical to the ones already
	// on disk.
	if len(maskRegions) > 0 && maskingApplies(&ds, maskRegions, maskSrc) {
		var snap *pixelStateSnapshot
		if encapsulatedPixelData(&ds) {
			s := snapshotPixelState(&ds)
			if _, err := convertDatasetSyntax(&ds, s.sourceTS, tsExplicitVRLE); err != nil {
				return false, ds, notes, fmt.Errorf("decompress for pixel masking: %w", err)
			}
			snap = &s
			notes.maskDecompressed = true
		}
		outcome, err := applyPixelMask(&ds, maskRegions, maskSrc)
		if err != nil {
			return false, ds, notes, fmt.Errorf("pixel masking: %w", err)
		}
		notes.maskUSFallback = outcome.usFellBack
		if snap != nil {
			if !outcome.masked {
				// The generous header gate admitted a file the authoritative
				// per-frame resolution then found nothing to mask on. Put the
				// original codestream back verbatim — no pixel changed, so the
				// export must not change encoding (or bytes) either.
				if rerr := snap.restoreOriginalPixels(&ds); rerr != nil {
					return false, ds, notes, fmt.Errorf("restore unmasked pixel data: %w", rerr)
				}
				notes.maskDecompressed = false
			} else if target, ok := recompressTargetFor(snap.sourceTS); ok {
				if rerr := recompressPixelData(&ds, *snap, target); rerr != nil {
					// Fall back to the uncompressed export; the worker logs
					// this and maskDecompressed keeps it in the summary.
					notes.maskRecompressErr = fmt.Sprintf("recompress to %s: %v",
						transferSyntaxLabel(target), rerr)
				} else if target == snap.sourceTS {
					notes.maskDecompressed = false
					notes.maskRecompressed = true
				} else {
					// A lossy source re-encoded losslessly: no loss added, but
					// the syntax changed, which gets its own disclosure.
					notes.maskDecompressed = false
					notes.maskRecodedLossless = true
				}
			}
		}
	}

	// Measured on the finished dataset, so it reports what actually ships: a
	// profile that removed Original Attributes Sequence no longer counts, and
	// neither does one that never asked for the birth date to be masked — a
	// nested birth date is only a surprise when the top-level one was rewritten.
	// Gating on dobMask also keeps the extra traversal off every other run.
	if dobMask != "" && hasNestedTag(ds.Elements, tag.PatientBirthDate) {
		notes.nestedDOBKept = true
	}

	notes.namesAfter = readExportNames(&ds)
	return false, ds, notes, nil
}

// encapsulatedPixelData reports whether ds still holds compressed pixel data.
func encapsulatedPixelData(ds *sdicom.Dataset) bool {
	elem, err := ds.FindElementByTag(tag.PixelData)
	if err != nil {
		return false
	}
	info, ok := elem.Value.GetValue().(sdicom.PixelDataInfo)
	return ok && info.IsEncapsulated
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

// isOverlayGroup reports whether g is a standard overlay-plane repeating group
// (6000–60FE, even — PS3.5 §7.6). Odd 60xx groups are ordinary private tags,
// which are noprivate's business, not this check's.
func isOverlayGroup(g uint16) bool { return g&0xFF00 == 0x6000 && g%2 == 0 }

// pruneElements recursively drops elements whose tag is in removalSet, plus any
// odd-group (private) element when removePrivate is set and any overlay-plane
// group when removeOverlays is set, at every nesting depth. Sequence elements
// that are not themselves removed are recursed into and rebuilt so that nested
// identifiers are scrubbed as well. The removalSet lookup keeps this an
// O(elements) pass regardless of how many tags are being removed.
func pruneElements(elements []*sdicom.Element, removalSet map[tag.Tag]struct{}, removePrivate, removeOverlays bool) []*sdicom.Element {
	filtered := elements[:0] // reuse backing array, matching the existing in-place style
	for _, elem := range elements {
		if removePrivate && elem.Tag.Group%2 == 1 {
			continue
		}
		if removeOverlays && isOverlayGroup(elem.Tag.Group) {
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
						rebuilt = append(rebuilt, pruneElements(itemElems, removalSet, removePrivate, removeOverlays))
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
