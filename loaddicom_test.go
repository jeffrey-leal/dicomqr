package main

// Regression tests for the study-preview freeze on multiframe files (NM/SPECT
// projection data). loadDicomImage used to drain the parser's frame channel
// only after ParseFile returned, but the parser sends each frame with a
// blocking send during the parse — so any file with more frames than the
// channel buffer (8) deadlocked the load. NM/SPECT stores the whole
// acquisition as one 40-240-frame file, freezing the study preview mid-load;
// verified against a real SPECT/CT study before the fix. The error path had
// the same latent hang: the library closes the channel only on success, so
// draining after a mid-parse error blocked forever.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// loadWithTimeout fails the test instead of hanging it when loadDicomImage
// never returns.
func loadWithTimeout(t *testing.T, path string) (viewerState, error) {
	t.Helper()
	var (
		vs   viewerState
		lerr error
	)
	done := make(chan struct{})
	go func() {
		vs, lerr = loadDicomImage(path)
		close(done)
	}()
	select {
	case <-done:
		return vs, lerr
	case <-time.After(30 * time.Second):
		t.Fatal("loadDicomImage deadlocked (frame channel not drained concurrently)")
		return viewerState{}, nil
	}
}

// writeMultiframeTestFile writes an NM-style multi-frame file of nFrames 2×2
// frames into dir. Every frame carries the same first three pixels and a
// distinct fourth (40+frame index), so a test can tell which frame it got.
func writeMultiframeTestFile(t *testing.T, dir string, nFrames, instanceNumber int) string {
	t.Helper()
	frames := make([]*frame.Frame, nFrames)
	for i := range frames {
		nf := frame.NewNativeFrame[uint8](8, 2, 2, 4, 1)
		copy(nf.RawData, []uint8{10, 20, 30, byte(40 + i)})
		frames[i] = &frame.Frame{Encapsulated: false, NativeData: nf}
	}
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		IsEncapsulated: false,
		Frames:         frames,
	})
	if err != nil {
		t.Fatalf("NewElement(PixelData): %v", err)
	}
	elems := []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.20"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.6"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{tsExplicitVRLE}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.20"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.6"}),
		mustTestElement(t, tag.Modality, []string{"NM"}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{"MONOCHROME2"}),
		mustTestElement(t, tag.NumberOfFrames, []string{strconv.Itoa(nFrames)}),
		mustTestElement(t, tag.InstanceNumber, []string{strconv.Itoa(instanceNumber)}),
		mustTestElement(t, tag.Rows, []int{2}),
		mustTestElement(t, tag.Columns, []int{2}),
		mustTestElement(t, tag.BitsAllocated, []int{8}),
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.HighBit, []int{7}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		mustTestElement(t, tag.SamplesPerPixel, []int{1}),
		pd,
	}

	path := filepath.Join(dir, fmt.Sprintf("multiframe-%d.dcm", instanceNumber))
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	writeErr := sdicom.Write(f, sdicom.Dataset{Elements: elems},
		sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification())
	f.Close()
	if writeErr != nil {
		t.Fatalf("write multiframe test DICOM: %v", writeErr)
	}
	return path
}

// frameMarker returns the distinguishing pixel of a frame written by
// writeMultiframeTestFile, i.e. 40 + the frame index it holds.
func frameMarker(t *testing.T, vs viewerState) int {
	t.Helper()
	if vs.frame == nil || len(vs.frame.gray) != 4 {
		t.Fatalf("frame not decoded as a 2×2 grayscale frame: %+v", vs.frame)
	}
	return int(vs.frame.gray[3])
}

func TestLoadDicomImageManyFrames(t *testing.T) {
	// 24 frames is well past the 8-slot frame channel buffer.
	path := writeMultiframeTestFile(t, t.TempDir(), 24, 1)

	vs, lerr := loadWithTimeout(t, path)
	if lerr != nil {
		t.Fatalf("loadDicomImage: %v", lerr)
	}
	if vs.img == nil {
		t.Error("loadDicomImage returned no image for multiframe file")
	}
}

// TestParsedDicomExposesEveryFrame is the regression test for multi-frame
// files previewing as their first frame only: every frame of the file must be
// individually decodable, not just frames[0].
func TestParsedDicomExposesEveryFrame(t *testing.T) {
	const nFrames = 24
	path := writeMultiframeTestFile(t, t.TempDir(), nFrames, 1)

	p, err := parseDicomFile(path)
	if err != nil {
		t.Fatalf("parseDicomFile: %v", err)
	}
	if p.frameCount() != nFrames {
		t.Fatalf("frameCount = %d, want %d", p.frameCount(), nFrames)
	}
	for i := 0; i < nFrames; i++ {
		vs, err := p.frameState(i)
		if err != nil {
			t.Fatalf("frameState(%d): %v", i, err)
		}
		if vs.img == nil {
			t.Fatalf("frameState(%d) returned no image", i)
		}
		if got, want := frameMarker(t, vs), 40+i; got != want {
			t.Errorf("frameState(%d) decoded marker %d, want %d", i, got, want)
		}
	}
}

// An out-of-range frame index is clamped rather than failing, so a file whose
// NumberOfFrames overstates what the parser delivered still shows an image.
func TestParsedDicomFrameStateClampsIndex(t *testing.T) {
	const nFrames = 4
	path := writeMultiframeTestFile(t, t.TempDir(), nFrames, 1)

	p, err := parseDicomFile(path)
	if err != nil {
		t.Fatalf("parseDicomFile: %v", err)
	}
	vs, err := p.frameState(99)
	if err != nil {
		t.Fatalf("frameState(99): %v", err)
	}
	if got, want := frameMarker(t, vs), 40+nFrames-1; got != want {
		t.Errorf("frameState(99) decoded marker %d, want last frame %d", got, want)
	}
	vs, err = p.frameState(-3)
	if err != nil {
		t.Fatalf("frameState(-3): %v", err)
	}
	if got, want := frameMarker(t, vs), 40; got != want {
		t.Errorf("frameState(-3) decoded marker %d, want first frame %d", got, want)
	}
}

// The viewer navigates frames, so its slice list must hold one entry per frame
// of every file, in InstanceNumber order.
func TestSortDicomSlicesExpandsFrames(t *testing.T) {
	dir := t.TempDir()
	second := writeMultiframeTestFile(t, dir, 3, 2)
	first := writeMultiframeTestFile(t, dir, 2, 1)

	slices := sortDicomSlices([]string{second, first})
	want := []viewerSlice{
		{path: first, frame: 0},
		{path: first, frame: 1},
		{path: second, frame: 0},
		{path: second, frame: 1},
		{path: second, frame: 2},
	}
	if len(slices) != len(want) {
		t.Fatalf("got %d slices, want %d: %+v", len(slices), len(want), slices)
	}
	for i := range want {
		if slices[i] != want[i] {
			t.Errorf("slice %d = %+v, want %+v", i, slices[i], want[i])
		}
	}
	if got := slicePaths(slices); len(got) != 2 || got[0] != first || got[1] != second {
		t.Errorf("slicePaths = %v, want [%s %s]", got, first, second)
	}
}

func TestDicomInstanceInfoReadsFrameCount(t *testing.T) {
	path := writeMultiframeTestFile(t, t.TempDir(), 7, 5)
	num, frames := dicomInstanceInfo(path)
	if num != 5 {
		t.Errorf("instanceNumber = %d, want 5", num)
	}
	if frames != 7 {
		t.Errorf("frames = %d, want 7", frames)
	}
}

// A file that cannot be parsed still occupies exactly one slice, so the viewer
// can select it and report its own load error.
func TestDicomInstanceInfoUnreadableFileIsOneFrame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.dcm")
	if err := os.WriteFile(path, []byte("this is not a DICOM file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, frames := dicomInstanceInfo(path); frames != 1 {
		t.Errorf("frames = %d for an unreadable file, want 1", frames)
	}
}

func TestExpandFramesSingleFrameFiles(t *testing.T) {
	slices := expandFrames([]dicomInstance{
		{path: "a.dcm", frames: 1},
		{path: "b.dcm", frames: 0}, // absent/invalid NumberOfFrames still yields one slice
	})
	want := []viewerSlice{{path: "a.dcm", frame: 0}, {path: "b.dcm", frame: 0}}
	if len(slices) != len(want) {
		t.Fatalf("got %+v, want %+v", slices, want)
	}
	for i := range want {
		if slices[i] != want[i] {
			t.Errorf("slice %d = %+v, want %+v", i, slices[i], want[i])
		}
	}
}

// The viewer's cache must parse a file once and then serve any frame of it, and
// must re-parse when navigation moves to a different file.
func TestDicomFileCacheServesFramesFromOneParse(t *testing.T) {
	dir := t.TempDir()
	fileA := writeMultiframeTestFile(t, dir, 5, 1)
	fileB := writeMultiframeTestFile(t, dir, 5, 2)

	c := &dicomFileCache{}
	for _, idx := range []int{0, 3, 1, 4} {
		vs, err := c.load(fileA, idx)
		if err != nil {
			t.Fatalf("load(%s, %d): %v", fileA, idx, err)
		}
		if got, want := frameMarker(t, vs), 40+idx; got != want {
			t.Errorf("load frame %d decoded marker %d, want %d", idx, got, want)
		}
	}
	parsedA := c.parsed
	if _, err := c.load(fileA, 2); err != nil {
		t.Fatalf("second load of the same file: %v", err)
	}
	if c.parsed != parsedA {
		t.Error("cache re-parsed a file it already held")
	}
	if _, err := c.load(fileB, 0); err != nil {
		t.Fatalf("load(%s, 0): %v", fileB, err)
	}
	if c.parsed == parsedA || c.path != fileB {
		t.Error("cache did not re-parse after navigating to a different file")
	}
}

// A failed load must not leave the previous file cached under the new path.
func TestDicomFileCacheDropsEntryOnError(t *testing.T) {
	dir := t.TempDir()
	good := writeMultiframeTestFile(t, dir, 3, 1)
	bad := filepath.Join(dir, "corrupt.dcm")
	if err := os.WriteFile(bad, []byte("this is not a DICOM file"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &dicomFileCache{}
	if _, err := c.load(good, 0); err != nil {
		t.Fatalf("load(good): %v", err)
	}
	if _, err := c.load(bad, 0); err == nil {
		t.Fatal("load(corrupt) succeeded, want error")
	}
	if c.parsed != nil || c.path != "" {
		t.Errorf("cache kept an entry after a failed parse: path=%q parsed=%v", c.path, c.parsed != nil)
	}
	if _, err := c.load(good, 2); err != nil {
		t.Fatalf("load(good) after a failed parse: %v", err)
	}
}

// TestSampleMultiframeFile runs the viewer's frame path over a real
// multi-frame file — the synthetic files above are native (uncompressed), while
// real NM/SPECT reconstructions are usually JPEG 2000. Run manually:
//
//	DICOMQR_MULTIFRAME_SAMPLE="…/BRAIN SPECT_CT [IRAC - AC ] (1000)/….dcm" \
//	  go test -tags "openjpeg jpeglossless" -run TestSampleMultiframeFile -v .
//
// Without the decoder tags the decode half is skipped and only the navigation
// half (frame count → one slice per frame) is checked.
func TestSampleMultiframeFile(t *testing.T) {
	path := os.Getenv("DICOMQR_MULTIFRAME_SAMPLE")
	if path == "" {
		t.Skip("set DICOMQR_MULTIFRAME_SAMPLE to a multi-frame DICOM file")
	}

	_, headerFrames := dicomInstanceInfo(path)
	if headerFrames < 2 {
		t.Fatalf("%s declares %d frames — not a multi-frame file", path, headerFrames)
	}
	slices := sortDicomSlices([]string{path})
	if len(slices) != headerFrames {
		t.Fatalf("got %d navigable slices, want one per frame (%d)", len(slices), headerFrames)
	}
	t.Logf("%s: %d frames → %d slices", filepath.Base(path), headerFrames, len(slices))

	p, err := parseDicomFile(path)
	if err != nil {
		t.Fatalf("parseDicomFile: %v", err)
	}
	if p.frameCount() != headerFrames {
		t.Fatalf("parser delivered %d frames, header declares %d", p.frameCount(), headerFrames)
	}

	if isJPEG2000TransferSyntax(p.transferSyntax) && !jpeg2000Available {
		t.Skip("JPEG 2000 decoder not built in — rebuild with -tags openjpeg to check decoding")
	}
	if isJPEGLosslessTransferSyntax(p.transferSyntax) && !jpegLosslessAvailable {
		t.Skip("JPEG Lossless decoder not built in — rebuild with -tags jpeglossless to check decoding")
	}

	// Every frame must decode, and consecutive frames that carry signal must not
	// be pixel-identical — that was the visible symptom: scrolling showed frame
	// 0 over and over. Uniform frames are exempt: a reconstruction volume
	// legitimately begins and ends with all-zero slices outside the patient
	// (verified on this study: only the blank end slices repeat).
	uniform := func(g []float32) bool {
		for _, v := range g {
			if v != g[0] {
				return false
			}
		}
		return true
	}
	var prev []float32
	identical := 0
	for i := 0; i < p.frameCount(); i++ {
		vs, err := p.frameState(i)
		if err != nil {
			t.Fatalf("frameState(%d): %v", i, err)
		}
		if vs.img == nil {
			t.Fatalf("frameState(%d) returned no image", i)
		}
		gray := vs.frame.gray
		if prev != nil && len(gray) == len(prev) && !uniform(gray) {
			same := true
			for j := range gray {
				if gray[j] != prev[j] {
					same = false
					break
				}
			}
			if same {
				t.Errorf("frame %d decoded identically to frame %d", i, i-1)
				identical++
			}
		}
		prev = gray
	}
	t.Logf("decoded %d frames, %d duplicated a previous frame", p.frameCount(), identical)
}

func TestLoadDicomImageParseErrorNoHang(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.dcm")
	if err := os.WriteFile(path, []byte("this is not a DICOM file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, lerr := loadWithTimeout(t, path); lerr == nil {
		t.Error("loadDicomImage succeeded on a corrupt file, want error")
	}
}
