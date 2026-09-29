package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// TestSampleIgnoreSOPClassStudy runs the SOP-class filter over a real study
// through the actual engine and checks that exactly the Secondary Capture and
// ignored-modality files are skipped, that everything else exports, and that
// no exported file is a Secondary Capture object. Field evidence for the
// filter rather than a unit test — opt-in like the other sample hooks:
//
//	DICOMQR_SOPCLASS_SAMPLE="…/CT OUTSIDE STUDY BODY (20260119)" go test -run TestSampleIgnoreSOPClass -v .
//
// The study this was written against (GHAFOURI, 2026-01-19) carries an eight
// page kiosk scan and a dose screen as Secondary Capture labelled CT, beside
// 2,366 CT images and twenty PR/KO objects.
func TestSampleIgnoreSOPClassStudy(t *testing.T) {
	dir := os.Getenv("DICOMQR_SOPCLASS_SAMPLE")
	if dir == "" {
		t.Skip("set DICOMQR_SOPCLASS_SAMPLE to a study folder")
	}
	var files []string
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(strings.ToLower(p), ".dcm") {
			files = append(files, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no .dcm files under %s", dir)
	}

	headerString := func(t *testing.T, path string, tg tag.Tag) string {
		t.Helper()
		ds, err := safeParseFile(path, nil, sdicom.SkipPixelData())
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		e, err := ds.FindElementByTag(tg)
		if err != nil {
			return ""
		}
		vals := sdicom.MustGetStrings(e.Value)
		if len(vals) == 0 {
			return ""
		}
		return strings.TrimSpace(vals[0])
	}

	ignoredMods := map[string]bool{"SR": true, "PR": true, "KO": true, "OT": true}
	isSC := func(uid string) bool { return matchesSOPClass(uid, secondaryCaptureSOPClasses) }
	wantSkipped, wantProcessed := 0, 0
	scByModality := map[string]int{}
	for _, f := range files {
		sop := headerString(t, f, tag.SOPClassUID)
		mod := headerString(t, f, tag.Modality)
		switch {
		case ignoredMods[mod]:
			wantSkipped++
		case isSC(sop):
			wantSkipped++
			scByModality[mod]++
		default:
			wantProcessed++
		}
	}
	t.Logf("%d files: expect %d skipped (Secondary Capture by modality: %v), %d exported",
		len(files), wantSkipped, scByModality, wantProcessed)

	profile := ModProfile{
		Sets:             []string{"0010,0010=ANON", "0010,0020=ANON"},
		IgnoreModalities: []string{"SR", "PR", "KO", "OT"},
		IgnoreSOPClasses: secondaryCaptureSOPClasses,
	}
	params, err := compileModifyParams(profile)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	outDir := t.TempDir()
	res := runModification(context.Background(), files, dir, outDir, params, nil, nil)
	t.Logf("result: processed=%d skipped=%d failed=%d", res.Processed, res.Skipped, res.Failed)
	if res.Failed != 0 {
		t.Errorf("%d files failed", res.Failed)
	}
	if res.Skipped != wantSkipped || res.Processed != wantProcessed {
		t.Errorf("processed=%d skipped=%d, want processed=%d skipped=%d",
			res.Processed, res.Skipped, wantProcessed, wantSkipped)
	}

	// Nothing Secondary Capture may have reached the export, and every export
	// must be a natively classed object.
	exported := 0
	if err := filepath.WalkDir(outDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		exported++
		if sop := headerString(t, p, tag.SOPClassUID); isSC(sop) {
			t.Errorf("exported a Secondary Capture object: %s", p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk output: %v", err)
	}
	if exported != wantProcessed {
		t.Errorf("%d files in the export, want %d", exported, wantProcessed)
	}
}
