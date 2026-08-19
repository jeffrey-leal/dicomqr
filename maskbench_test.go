package main

// Pixel-masking performance harness, gated on DICOMQR_MASK_BENCH so a normal
// test run never sees it. Point it at any study folder to compare how the three
// masking workloads behave on that modality:
//
//	DICOMQR_MASK_BENCH="F:/DICOM Downloads/BRYAN^SYLVIA (E0092066)" \
//	  go test -tags "openjpeg jpeglossless" -run TestMaskBench -v -timeout 60m .
//
// Build with the release tags: without them a JPEG 2000 or JPEG Lossless study
// cannot be decompressed, and every file that needs masking fails instead of
// being measured.
//
// It reports rather than asserts anything about speed. Wall-clock thresholds in
// a test only produce flaky failures on a busy machine; the numbers are here to
// be read and compared between modalities. The correctness invariants it does
// assert are the cheap ones worth having: nothing fails, and the file a scoped
// rectangle names really is masked in the export.
//
// Sizing: masking writes uncompressed pixels, so a study that decompresses to
// tens of gigabytes would fill the disk before the run finished. Files are
// admitted until their projected uncompressed size reaches a budget (2 GB by
// default, DICOMQR_MASK_BENCH_BUDGET_MB to change it), and the count admitted
// out of the study total is always logged — a truncated measurement that looks
// like a whole-study one is worse than no measurement.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// maskBenchDefaultBudgetMB caps the projected uncompressed size of the files a
// scenario will process.
const maskBenchDefaultBudgetMB = 2048

// maskBenchFile is one admitted file and what it costs uncompressed.
type maskBenchFile struct {
	path       string
	modality   string
	cols, rows int
	frames     int
	syntax     string
	sopUID     string
	calibrated bool
	rawBytes   int64
}

// maskBenchStudy is the admitted subset plus what was left out of it.
type maskBenchStudy struct {
	root       string
	files      []maskBenchFile
	totalFiles int
	rawBytes   int64
}

// loadMaskBenchStudy walks the study, reading headers only, and admits files
// until the uncompressed budget is reached.
func loadMaskBenchStudy(t *testing.T) maskBenchStudy {
	t.Helper()
	root := os.Getenv("DICOMQR_MASK_BENCH")
	if root == "" {
		t.Skip("DICOMQR_MASK_BENCH not set — point it at a study folder to measure masking")
	}
	// Megabytes throughout, decimal — the same unit every size below is
	// reported in, so the budget and the totals can be compared by eye.
	budget := int64(maskBenchDefaultBudgetMB) * 1e6
	if v := os.Getenv("DICOMQR_MASK_BENCH_BUDGET_MB"); v != "" {
		if mb, err := strconv.ParseInt(v, 10, 64); err == nil && mb > 0 {
			budget = mb * 1e6
		}
	}

	var (
		study   = maskBenchStudy{root: root}
		skipped int
	)
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(p), ".db") {
			return nil // the catalog index, not a DICOM file
		}
		study.totalFiles++
		ds, perr := sdicom.ParseFile(p, nil, sdicom.SkipPixelData())
		if perr != nil {
			return nil
		}
		f := maskBenchFile{
			path:     p,
			modality: datasetFirstString(&ds, tag.Modality),
			cols:     datasetInt(&ds, tag.Columns, 0),
			rows:     datasetInt(&ds, tag.Rows, 0),
			frames:   datasetInt(&ds, tag.NumberOfFrames, 1),
			syntax:   transferSyntaxLabel(datasetTransferSyntaxUID(&ds)),
			sopUID:   strings.TrimSpace(datasetFirstString(&ds, tag.SOPInstanceUID)),
		}
		_, f.calibrated = ultrasoundRegionBounds(&ds)
		f.rawBytes = projectedPixelBytes(&ds)

		if study.rawBytes+f.rawBytes > budget && len(study.files) > 0 {
			skipped++
			return nil
		}
		study.rawBytes += f.rawBytes
		study.files = append(study.files, f)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(study.files) == 0 {
		t.Skipf("no readable DICOM files under %s", root)
	}
	if skipped > 0 {
		t.Logf("NOTE: %d of %d files left out to stay within the %d MB uncompressed budget "+
			"(DICOMQR_MASK_BENCH_BUDGET_MB to raise it)",
			skipped, study.totalFiles, budget/1e6)
	}
	return study
}

// paths is the admitted files in walk order.
func (s maskBenchStudy) paths() []string {
	out := make([]string, len(s.files))
	for i, f := range s.files {
		out[i] = f.path
	}
	return out
}

// sourceBytes is what the admitted files occupy on disk as stored.
func (s maskBenchStudy) sourceBytes(t *testing.T) int64 {
	var total int64
	for _, f := range s.files {
		if info, err := os.Stat(f.path); err == nil {
			total += info.Size()
		}
	}
	return total
}

// dirBytes totals a directory tree.
func maskBenchDirBytes(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// firstMaskableImage returns a file worth scoping a rectangle to: an image,
// preferring one that declares no ultrasound region, since those are the files
// a hand-drawn rectangle exists for.
func (s maskBenchStudy) firstMaskableImage() (maskBenchFile, bool) {
	var fallback maskBenchFile
	var haveFallback bool
	for _, f := range s.files {
		if f.cols == 0 || f.rows == 0 || f.sopUID == "" {
			continue // not an image, or nothing to key a scope on
		}
		if !f.calibrated {
			return f, true
		}
		if !haveFallback {
			fallback, haveFallback = f, true
		}
	}
	return fallback, haveFallback
}

// runMaskBench times one profile over the study and reports what it cost.
func runMaskBench(t *testing.T, name string, study maskBenchStudy, p ModProfile) (modifyResult, string) {
	t.Helper()
	params, err := compileModifyParams(p)
	if err != nil {
		t.Fatalf("%s: compile: %v", name, err)
	}
	out := t.TempDir()

	start := time.Now()
	res := runModification(context.Background(), study.paths(), study.root, out, params, nil, nil)
	elapsed := time.Since(start)

	exported := maskBenchDirBytes(out)
	source := study.sourceBytes(t)
	perFile := time.Duration(0)
	if res.Processed > 0 {
		perFile = elapsed / time.Duration(res.Processed)
	}
	t.Logf("%-18s %8s  %6.1f MB/s in  |  %d processed, %d failed, %d recompressed, %d recoded-lossless, %d decompressed, %d us-fallback  |  "+
		"export %.0f MB from %.0f MB source (x%.1f)  |  %v/file",
		name, elapsed.Round(time.Millisecond), float64(source)/1e6/elapsed.Seconds(),
		res.Processed, res.Failed, res.MaskRecompressed, res.MaskRecodedLossless, res.MaskDecompressed, res.MaskUSFallback,
		float64(exported)/1e6, float64(source)/1e6, float64(exported)/float64(max(source, 1)),
		perFile.Round(time.Millisecond))
	for _, f := range res.Failures {
		t.Logf("    failed: %s — %s", filepath.Base(f.File), f.Error)
	}
	return res, out
}

// TestMaskBenchStudyProfile describes what the study is made of, which is the
// context every timing below has to be read against.
func TestMaskBenchStudyProfile(t *testing.T) {
	study := loadMaskBenchStudy(t)

	type key struct {
		modality, syntax string
		cols, rows       int
		calibrated       bool
	}
	counts := map[key]int{}
	var frames int
	for _, f := range study.files {
		counts[key{f.modality, f.syntax, f.cols, f.rows, f.calibrated}]++
		frames += f.frames
	}
	t.Logf("%s", study.root)
	t.Logf("%d of %d files admitted, %d frames, %.0f MB stored, %.0f MB uncompressed",
		len(study.files), study.totalFiles, frames,
		float64(study.sourceBytes(t))/1e6, float64(study.rawBytes)/1e6)
	for k, n := range counts {
		note := ""
		if strings.EqualFold(k.modality, "US") {
			note = "  calibrated region: no"
			if k.calibrated {
				note = "  calibrated region: yes"
			}
		}
		t.Logf("  %-4s %-30s %4d x %-4d  n=%d%s", k.modality, k.syntax, k.cols, k.rows, n, note)
	}
}

// TestMaskBenchScenarios is the comparison: the same study under no masking, a
// mask scoped to one image, and a mask that applies to everything.
func TestMaskBenchScenarios(t *testing.T) {
	study := loadMaskBenchStudy(t)
	t.Logf("%d of %d files, %.0f MB stored, %.0f MB uncompressed",
		len(study.files), study.totalFiles,
		float64(study.sourceBytes(t))/1e6, float64(study.rawBytes)/1e6)

	// The baseline: tag edits only. Whatever this costs is the floor the
	// masking scenarios are measured against.
	t.Run("no masking", func(t *testing.T) {
		runMaskBench(t, "tags only", study, ModProfile{Sets: []string{"0010,0010=ANON"}})
	})

	// The targeted workflow: one image carries a banner, the rest are left
	// alone. Every other file must keep its stored encoding — the regression
	// this harness exists to catch is that number climbing back up.
	target, ok := study.firstMaskableImage()
	if !ok {
		t.Log("no image with a SOP Instance UID to scope to — skipping the scoped scenario")
	} else {
		t.Run("one image scoped", func(t *testing.T) {
			res, out := runMaskBench(t, "one image", study, ModProfile{
				Sets: []string{"0010,0010=ANON"},
				MaskRegions: []MaskRegion{{Mode: maskModeRect, W: 1, H: 0.08,
					AppliesTo: &MaskScope{SOPInstanceUID: target.sopUID}}},
			})
			if res.Failed != 0 {
				t.Errorf("%d file(s) failed", res.Failed)
			}
			// Only the named image should have needed its pixels rewritten
			// (recompressed into its own syntax, re-encoded lossless from a
			// lossy source, or decompressed when neither is possible) — unless
			// it was stored uncompressed to begin with, in which case none did.
			if rewritten := res.MaskRecompressed + res.MaskRecodedLossless + res.MaskDecompressed; rewritten > 1 {
				t.Errorf("recompressed+recoded+decompressed = %d, want at most 1: files no region applies to "+
					"must keep their stored encoding", rewritten)
			}
			assertMaskBenchMasked(t, study, out, target)
		})
	}

	// The unavoidable end: a rectangle on every image. This is the cost that
	// cannot be optimised away, only paid.
	t.Run("all images", func(t *testing.T) {
		res, _ := runMaskBench(t, "every image", study, ModProfile{
			Sets:        []string{"0010,0010=ANON"},
			MaskRegions: []MaskRegion{{Mode: maskModeRect, W: 1, H: 0.08}},
		})
		if res.Failed != 0 {
			t.Errorf("%d file(s) failed", res.Failed)
		}
	})
}

// assertMaskBenchMasked confirms the scoped file really was masked: skipping
// the files a region does not apply to must never skip the one it does.
func assertMaskBenchMasked(t *testing.T, study maskBenchStudy, outDir string, target maskBenchFile) {
	t.Helper()
	rel, err := filepath.Rel(study.root, target.path)
	if err != nil {
		t.Fatalf("relative path: %v", err)
	}
	ds, err := sdicom.ParseFile(filepath.Join(outDir, rel), nil)
	if err != nil {
		t.Fatalf("parse masked export: %v", err)
	}
	pd, err := ds.FindElementByTag(tag.PixelData)
	if err != nil {
		t.Fatalf("masked export has no pixel data: %v", err)
	}
	info, ok := pd.Value.GetValue().(sdicom.PixelDataInfo)
	if !ok || len(info.Frames) == 0 {
		t.Fatalf("unexpected pixel data in the masked export")
	}
	// A lossless source comes back recompressed into its own syntax; anything
	// else leaves uncompressed. Check row 1 either way — it sits inside the
	// top 8% of any image tall enough to bother masking.
	if info.IsEncapsulated {
		tsUID := datasetTransferSyntaxUID(&ds)
		var (
			w, nc   int
			samples []int32
		)
		switch tsUID {
		case tsJPEG2000LL:
			w, _, nc, _, _, samples, err = decodeJPEG2000(info.Frames[0].EncapsulatedData.Data)
		case tsJPEGLossless, tsJPEGLosslessSV1:
			w, _, nc, _, _, samples, err = decodeJPEGLossless(info.Frames[0].EncapsulatedData.Data)
		default:
			t.Fatalf("the masked export is still compressed as %s, so nothing was written to it",
				transferSyntaxLabel(tsUID))
		}
		if err != nil {
			t.Fatalf("decode masked export: %v", err)
		}
		// Decoded samples are planar: nc planes of w*h each.
		pixels := len(samples) / max(nc, 1)
		for c := 0; c < nc; c++ {
			if v := samples[c*pixels+1*w+w/2]; v != 0 {
				t.Errorf("%s was not masked: plane %d sample %d at mid-width, row 1",
					filepath.Base(target.path), c, v)
				return
			}
		}
		return
	}
	nf, err := info.Frames[0].GetNativeFrame()
	if err != nil {
		t.Fatalf("native frame: %v", err)
	}
	px, err := nf.GetPixel(nf.Cols()/2, 1)
	if err != nil {
		t.Fatalf("GetPixel: %v", err)
	}
	for _, v := range px {
		if v != 0 {
			t.Errorf("%s was not masked: samples %v at mid-width, row 1",
				filepath.Base(target.path), px)
			return
		}
	}
}
