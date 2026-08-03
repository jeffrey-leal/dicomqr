package main

// Tests for MR phase detection: the structural rules (blocked and interleaved
// repeats, rejections), the labelling priorities, and the end-to-end path from
// files on disk. Synthetic chapters are built directly — detection reads only
// chapter fields, so no files are needed for the rules themselves.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

const testOrientation = `1\0\0\0\1\0`

// phasedChapters builds a blocked K-phase × N-slice series, customised per
// chapter by mutate(phase, slice, *chapter).
func phasedChapters(k, n int, mutate func(p, s int, c *chapter)) []chapter {
	var out []chapter
	for p := 0; p < k; p++ {
		for s := 0; s < n; s++ {
			c := chapter{
				path:        fmt.Sprintf("p%d-s%d.dcm", p, s),
				frames:      1,
				sliceLoc:    float64(100 - 3*s),
				hasSliceLoc: true,
				orientation: testOrientation,
			}
			if mutate != nil {
				mutate(p, s, &c)
			}
			out = append(out, c)
		}
	}
	return out
}

func TestDetectMRPhasesBlockedEchoPair(t *testing.T) {
	// The in/out-phase shape: 2 echoes × 5 slices, stored phase-by-phase.
	chapters := phasedChapters(2, 5, func(p, s int, c *chapter) {
		c.echoNumber = p + 1
		c.echoTime = []string{"2.38", "4.62"}[p]
	})
	phases := detectMRPhases(chapters)
	if len(phases) != 2 {
		t.Fatalf("got %d phases, want 2", len(phases))
	}
	if phases[0].label != "Echo 1 (TE 2.38 ms)" || phases[1].label != "Echo 2 (TE 4.62 ms)" {
		t.Errorf("labels = %q, %q", phases[0].label, phases[1].label)
	}
	for p, ph := range phases {
		if len(ph.slices) != 5 {
			t.Fatalf("phase %d has %d slices, want 5", p, len(ph.slices))
		}
		for s, sl := range ph.slices {
			if want := fmt.Sprintf("p%d-s%d.dcm", p, s); sl.path != want {
				t.Errorf("phase %d slice %d = %s, want %s", p, s, sl.path, want)
			}
		}
	}
}

func TestDetectMRPhasesInterleaved(t *testing.T) {
	// Same pair stored slice-by-slice: s0p0, s0p1, s1p0, s1p1, ...
	var chapters []chapter
	for s := 0; s < 4; s++ {
		for p := 0; p < 2; p++ {
			chapters = append(chapters, chapter{
				path:        fmt.Sprintf("p%d-s%d.dcm", p, s),
				frames:      1,
				sliceLoc:    float64(50 - 4*s),
				hasSliceLoc: true,
				orientation: testOrientation,
				echoNumber:  p + 1,
			})
		}
	}
	phases := detectMRPhases(chapters)
	if len(phases) != 2 {
		t.Fatalf("got %d phases, want 2", len(phases))
	}
	for p, ph := range phases {
		if len(ph.slices) != 4 {
			t.Fatalf("phase %d has %d slices, want 4", p, len(ph.slices))
		}
		for s, sl := range ph.slices {
			if want := fmt.Sprintf("p%d-s%d.dcm", p, s); sl.path != want {
				t.Errorf("phase %d slice %d = %s, want %s", p, s, sl.path, want)
			}
		}
	}
}

func TestDetectMRPhasesLabelFallbacks(t *testing.T) {
	// Same echo attributes on every phase (the diffusion case): SequenceName
	// distinguishes, with the vendor's leading * stripped.
	seq := phasedChapters(2, 3, func(p, s int, c *chapter) {
		c.echoNumber = 1
		c.echoTime = "57"
		c.seqName = []string{"*ep_b50t", "*ep_b800t"}[p]
	})
	phases := detectMRPhases(seq)
	if len(phases) != 2 || phases[0].label != "ep_b50t" || phases[1].label != "ep_b800t" {
		t.Errorf("sequence-name labels: %+v", phaseLabelsOf(phases))
	}

	// Nothing distinguishes the groups: plain ordinals.
	plain := phasedChapters(3, 2, nil)
	phases = detectMRPhases(plain)
	if len(phases) != 3 || phases[0].label != "Phase 1" || phases[2].label != "Phase 3" {
		t.Errorf("ordinal labels: %+v", phaseLabelsOf(phases))
	}

	// Temporal positions distinguish.
	temporal := phasedChapters(2, 3, func(p, s int, c *chapter) { c.temporalPos = p + 1 })
	phases = detectMRPhases(temporal)
	if len(phases) != 2 || phases[0].label != "Phase 1" || phases[1].label != "Phase 2" {
		t.Errorf("temporal labels: %+v", phaseLabelsOf(phases))
	}
}

func phaseLabelsOf(phases []mrPhase) []string {
	out := make([]string, len(phases))
	for i, p := range phases {
		out[i] = p.label
	}
	return out
}

func TestDetectMRPhasesRejections(t *testing.T) {
	base := func() []chapter {
		return phasedChapters(2, 4, func(p, s int, c *chapter) { c.echoNumber = p + 1 })
	}
	if detectMRPhases(base()) == nil {
		t.Fatal("baseline series must detect (the rejection cases below rely on it)")
	}

	tests := []struct {
		name string
		mod  func([]chapter) []chapter
	}{
		{"multi-frame instance", func(c []chapter) []chapter { c[3].frames = 24; return c }},
		{"missing slice location", func(c []chapter) []chapter { c[2].hasSliceLoc = false; return c }},
		{"mixed orientation", func(c []chapter) []chapter { c[5].orientation = `0\1\0\0\0\-1`; return c }},
		{"uneven phases", func(c []chapter) []chapter { return c[:7] }},
		{"single phase", func(c []chapter) []chapter { return c[:4] }},
		{"too few files", func(c []chapter) []chapter { return c[:3] }},
		{"duplicate location inside a run", func(c []chapter) []chapter {
			c[1].sliceLoc = c[0].sliceLoc
			c[5].sliceLoc = c[4].sliceLoc
			return c
		}},
		{"location drift between runs", func(c []chapter) []chapter { c[6].sliceLoc += 5; return c }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectMRPhases(tc.mod(base())); got != nil {
				t.Errorf("detected %d phases, want flat", len(got))
			}
		})
	}

	// A bolus-tracking series: many timepoints of ONE location must stay flat
	// (the slider is already the time axis).
	bolus := make([]chapter, 26)
	for i := range bolus {
		bolus[i] = chapter{
			path: fmt.Sprintf("t%d.dcm", i), frames: 1,
			sliceLoc: 28.6558, hasSliceLoc: true, orientation: testOrientation,
			acqNumber: i + 1,
		}
	}
	if got := detectMRPhases(bolus); got != nil {
		t.Errorf("bolus series detected %d phases, want flat", len(got))
	}
}

// writeMRTestFile writes one single-frame MR-shaped file carrying the phase
// attributes the header pass must pick up.
func writeMRTestFile(t *testing.T, dir string, inst, echo int, te string, loc float64) string {
	t.Helper()
	nf := frame.NewNativeFrame[uint8](8, 2, 2, 4, 1)
	copy(nf.RawData, []uint8{10, 20, 30, 40})
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		IsEncapsulated: false,
		Frames:         []*frame.Frame{{Encapsulated: false, NativeData: nf}},
	})
	if err != nil {
		t.Fatalf("NewElement(PixelData): %v", err)
	}
	elems := []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.4"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{fmt.Sprintf("1.2.3.5.%d", inst)}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{tsExplicitVRLE}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.4"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{fmt.Sprintf("1.2.3.5.%d", inst)}),
		mustTestElement(t, tag.Modality, []string{"MR"}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{"MONOCHROME2"}),
		mustTestElement(t, tag.InstanceNumber, []string{fmt.Sprint(inst)}),
		mustTestElement(t, tag.EchoNumbers, []string{fmt.Sprint(echo)}),
		mustTestElement(t, tag.EchoTime, []string{te}),
		mustTestElement(t, tag.SliceLocation, []string{fmt.Sprintf("%g", loc)}),
		mustTestElement(t, tag.ImageOrientationPatient, []string{"1", "0", "0", "0", "1", "0"}),
		mustTestElement(t, tag.Rows, []int{2}),
		mustTestElement(t, tag.Columns, []int{2}),
		mustTestElement(t, tag.BitsAllocated, []int{8}),
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.HighBit, []int{7}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		mustTestElement(t, tag.SamplesPerPixel, []int{1}),
		pd,
	}
	path := filepath.Join(dir, fmt.Sprintf("mr-%03d.dcm", inst))
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	writeErr := sdicom.Write(f, sdicom.Dataset{Elements: elems},
		sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification())
	f.Close()
	if writeErr != nil {
		t.Fatalf("write MR test DICOM: %v", writeErr)
	}
	return path
}

// The end-to-end path: files on disk → scanChapters → detectMRPhases.
func TestScanChaptersFeedsPhaseDetection(t *testing.T) {
	dir := t.TempDir()
	// 2 echoes × 3 slices, blocked, instance numbers in storage order.
	inst := 1
	for p := 0; p < 2; p++ {
		for s := 0; s < 3; s++ {
			writeMRTestFile(t, dir, inst, p+1, []string{"2.38", "4.62"}[p], float64(90-3*s))
			inst++
		}
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.dcm"))
	if err != nil || len(paths) != 6 {
		t.Fatalf("glob: %v (%d files)", err, len(paths))
	}
	scanned := scanChapters(paths, nil)
	phases := detectMRPhases(scanned)
	if len(phases) != 2 {
		t.Fatalf("got %d phases from files, want 2", len(phases))
	}
	if phases[0].label != "Echo 1 (TE 2.38 ms)" || phases[1].label != "Echo 2 (TE 4.62 ms)" {
		t.Errorf("labels = %v", phaseLabelsOf(phases))
	}
	if len(phases[0].slices) != 3 || len(phases[1].slices) != 3 {
		t.Errorf("slice counts = %d, %d, want 3, 3", len(phases[0].slices), len(phases[1].slices))
	}
}

// TestSampleMRPhaseSeries runs detection over a real multi-phase series. Run
// manually:
//
//	DICOMQR_MRPHASE_SAMPLE="…/In Out Phase BH (5)" go test -run TestSampleMRPhase -v .
func TestSampleMRPhaseSeries(t *testing.T) {
	dir := os.Getenv("DICOMQR_MRPHASE_SAMPLE")
	if dir == "" {
		t.Skip("set DICOMQR_MRPHASE_SAMPLE to a multi-phase MR series folder")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read sample folder: %v", err)
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".dcm") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	if len(paths) == 0 {
		t.Fatalf("no .dcm files in %s", dir)
	}

	chapters := scanChapters(paths, nil)
	phases := detectMRPhases(chapters)
	if len(phases) < 2 {
		t.Fatalf("no phase structure detected in %d files", len(paths))
	}
	n := len(phases[0].slices)
	for p, ph := range phases {
		if len(ph.slices) != n {
			t.Errorf("phase %d has %d slices, want %d", p, len(ph.slices), n)
		}
	}
	if len(phases)*n != len(paths) {
		t.Errorf("%d phases × %d slices ≠ %d files", len(phases), n, len(paths))
	}
	// Same index must address different files across phases (the toggle shows
	// a different image), and each phase must not repeat a file.
	if phases[0].slices[n/2].path == phases[1].slices[n/2].path {
		t.Error("phases share a file at the same slice index")
	}
	t.Logf("%d files → %d phases × %d slices; labels: %v",
		len(paths), len(phases), n, phaseLabelsOf(phases))
}
