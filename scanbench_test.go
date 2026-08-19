package main

// Folder-scan benchmark, opt-in against a real study:
//
//	DICOMQR_SCAN_BENCH=<study folder> go test -run TestScanBench -v .
//
// It exists to keep one claim checkable rather than remembered: the scan reads
// the identifying tags out of the first few kilobytes of each file, not out of
// all of it. A full parse cannot, even with SkipPixelData, because the library's
// Reader.Skip is io.CopyN(io.Discard, …) rather than a seek — so the cost of the
// old path scaled with study size, and the new one scales with file count.
//
// The correctness half matters more than the timing half: it asserts the two
// parse paths agree file for file over real data, which is the property a folder
// scan and a catalog ingest both depend on. Nothing here asserts a wall-clock
// threshold — as the mask benchmark notes, those only produce flaky failures.

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// scanBenchDefaultFiles caps the corpus so a run stays a measurement rather than
// an errand. DICOMQR_SCAN_BENCH_FILES overrides it; 0 means every file.
const scanBenchDefaultFiles = 1500

func TestScanBench(t *testing.T) {
	root := os.Getenv("DICOMQR_SCAN_BENCH")
	if root == "" {
		t.Skip("DICOMQR_SCAN_BENCH not set — point it at a study folder to measure the folder scan")
	}
	limit := scanBenchDefaultFiles
	if v := os.Getenv("DICOMQR_SCAN_BENCH_FILES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			limit = n
		}
	}

	var paths []string
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".dcm") {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(paths) == 0 {
		t.Skipf("no .dcm files under %s", root)
	}
	total := len(paths)
	if limit > 0 && len(paths) > limit {
		paths = paths[:limit]
		t.Logf("NOTE: measuring %d of %d files (DICOMQR_SCAN_BENCH_FILES to change)", len(paths), total)
	}

	// Warm the OS cache and measure what reading the corpus costs at all, so the
	// two timings below compare the work done rather than which ran first.
	warmStart := time.Now()
	var onDisk int64
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		n, _ := io.Copy(io.Discard, f)
		f.Close()
		onDisk += n
	}
	t.Logf("corpus: %d files, %.1f MB, read in %v",
		len(paths), float64(onDisk)/1e6, time.Since(warmStart).Round(time.Millisecond))

	fullStart := time.Now()
	fullMetas := make([]fileMeta, len(paths))
	fullOK := make([]bool, len(paths))
	for i, p := range paths {
		fullMetas[i], fullOK[i] = fullLocalFileMeta(p)
	}
	fullDur := time.Since(fullStart)

	scanStart := time.Now()
	scanMetas := make([]fileMeta, len(paths))
	scanOK := make([]bool, len(paths))
	for i, p := range paths {
		scanMetas[i], scanOK[i] = scanLocalFileMeta(p)
	}
	scanDur := time.Since(scanStart)

	t.Logf("full parse   %v", fullDur.Round(time.Millisecond))
	t.Logf("early exit   %v  (%.1fx faster)", scanDur.Round(time.Millisecond),
		float64(fullDur)/float64(scanDur))

	// The assertion: over real data the two paths must agree. A file the
	// streaming scan misses is not a failure — that is what the fallback in
	// parseLocalFileMeta is for — but it is worth counting, because a corpus
	// where it happens often would mean the early exit is not earning its keep.
	var fellBack, disagreed int
	for i := range paths {
		switch {
		case fullOK[i] && !scanOK[i]:
			fellBack++
		case scanOK[i] != fullOK[i]:
			disagreed++
			t.Errorf("%s: ok flags differ — streaming=%v full=%v", paths[i], scanOK[i], fullOK[i])
		case scanOK[i] && !reflect.DeepEqual(scanMetas[i], fullMetas[i]):
			disagreed++
			t.Errorf("%s: metadata differs\n streaming %+v\n full      %+v",
				paths[i], scanMetas[i], fullMetas[i])
		}
	}
	t.Logf("%d files agreed, %d fell back to the full parse, %d disagreed",
		len(paths)-fellBack-disagreed, fellBack, disagreed)
}
