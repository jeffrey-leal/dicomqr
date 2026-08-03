package main

// Tests for the chapter model: how a series is split into playable units, how
// each one is labelled, and how its playback timing is read. The labelling and
// timing rules are exercised against synthesised headers so they need no files.

import (
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// testDataset builds a Dataset from elements, the way a parsed header arrives.
func testDataset(t *testing.T, elems ...*sdicom.Element) *sdicom.Dataset {
	t.Helper()
	return &sdicom.Dataset{Elements: elems}
}

// usRegionSeq builds a SequenceOfUltrasoundRegions with one item per stated
// RegionDataType, the attribute a real echo file actually carries.
func usRegionSeq(t *testing.T, dataTypes ...int) *sdicom.Element {
	t.Helper()
	items := make([][]*sdicom.Element, 0, len(dataTypes))
	for _, dt := range dataTypes {
		items = append(items, []*sdicom.Element{mustTestElement(t, tag.RegionDataType, []int{dt})})
	}
	e, err := sdicom.NewElement(tag.SequenceOfUltrasoundRegions, items)
	if err != nil {
		t.Fatalf("NewElement(SequenceOfUltrasoundRegions): %v", err)
	}
	return e
}

func TestChapterLabelPrefersImageComments(t *testing.T) {
	ds := testDataset(t,
		mustTestElement(t, tag.ImageComments, []string{"Apical 4 chamber"}),
		mustTestElement(t, tag.ProtocolName, []string{"TTE"}),
	)
	if got, want := chapterLabel(ds, 3, 60), "3. Apical 4 chamber"; got != want {
		t.Errorf("label = %q, want %q", got, want)
	}
}

// The echo study this was built against carries none of the naming attributes,
// so the region calibration sequence is what distinguishes one clip from
// another in the label.
func TestChapterLabelFallsBackToUltrasoundContent(t *testing.T) {
	tests := []struct {
		name      string
		dataTypes []int
		colour    int
		want      string
	}{
		{"2D only", []int{1}, 0, "7. 2D"},
		{"colour flow", []int{2}, 0, "7. Colour flow"},
		{"duplex PW", []int{1, 3}, 0, "7. 2D + PW Doppler"},
		{"colour + CW", []int{2, 4}, 0, "7. Colour flow + CW Doppler"},
		{"repeated regions collapse", []int{1, 1}, 0, "7. 2D"},
		{"non-image regions ignored", []int{1, 5, 6}, 0, "7. 2D"},
		{"colour flagged without a region", []int{1}, 1, "7. 2D + Colour flow"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ds := testDataset(t,
				usRegionSeq(t, tc.dataTypes...),
				mustTestElement(t, tag.UltrasoundColorDataPresent, []int{tc.colour}),
			)
			if got := chapterLabel(ds, 7, 60); got != tc.want {
				t.Errorf("label = %q, want %q", got, tc.want)
			}
		})
	}
}

// A letterless attribute is an identifier, not a description: the echo study
// writes "0001" in ImageType and the SPECT study writes "15003.0.68612207@" in
// ProtocolName. Both must fall through to the next candidate.
func TestChapterLabelRejectsIdentifierShapedCandidates(t *testing.T) {
	numeric := testDataset(t, mustTestElement(t, tag.ImageType,
		[]string{"DERIVED", "PRIMARY", "0001"}))
	if got, want := chapterLabel(numeric, 4, 60), "4. Clip"; got != want {
		t.Errorf("numeric ImageType detail: label = %q, want %q", got, want)
	}
	named := testDataset(t, mustTestElement(t, tag.ImageType,
		[]string{"DERIVED", "PRIMARY", "COLOR_FLOW"}))
	if got, want := chapterLabel(named, 4, 60), "4. COLOR_FLOW"; got != want {
		t.Errorf("named ImageType detail: label = %q, want %q", got, want)
	}

	// A bare ProtocolName code must not win over a real description further down.
	coded := testDataset(t,
		mustTestElement(t, tag.ProtocolName, []string{"15003.0.68612207@"}),
		mustTestElement(t, tag.ImageType, []string{"ORIGINAL", "PRIMARY", "RECON TOMO"}),
	)
	if got, want := chapterLabel(coded, 1, 42), "1. RECON TOMO"; got != want {
		t.Errorf("coded ProtocolName: label = %q, want %q", got, want)
	}
	if got, want := chapterLabel(testDataset(t,
		mustTestElement(t, tag.ProtocolName, []string{"Apical 2 chamber"}),
	), 1, 42), "1. Apical 2 chamber"; got != want {
		t.Errorf("real ProtocolName: label = %q, want %q", got, want)
	}
}

func TestChapterLabelFallbackNamesStillsAndClips(t *testing.T) {
	empty := testDataset(t)
	if got, want := chapterLabel(empty, 9, 1), "9. Still"; got != want {
		t.Errorf("single frame: label = %q, want %q", got, want)
	}
	if got, want := chapterLabel(empty, 9, 40), "9. Clip"; got != want {
		t.Errorf("multi frame: label = %q, want %q", got, want)
	}
}

func TestCineFPSPriority(t *testing.T) {
	tests := []struct {
		name  string
		elems []*sdicom.Element
		want  float64
	}{
		{
			name: "RecommendedDisplayFrameRate wins",
			elems: []*sdicom.Element{
				mustTestElement(t, tag.RecommendedDisplayFrameRate, []string{"25"}),
				mustTestElement(t, tag.CineRate, []string{"30"}),
				mustTestElement(t, tag.FrameTime, []string{"10"}),
			},
			want: 25,
		},
		{
			name: "CineRate next",
			elems: []*sdicom.Element{
				mustTestElement(t, tag.CineRate, []string{"30"}),
				mustTestElement(t, tag.FrameTime, []string{"10"}),
			},
			want: 30,
		},
		{
			name:  "FrameTime converts from ms",
			elems: []*sdicom.Element{mustTestElement(t, tag.FrameTime, []string{"33.333333"})},
			want:  30,
		},
		{
			name:  "FrameTimeVector averages, ignoring the leading zero",
			elems: []*sdicom.Element{mustTestElement(t, tag.FrameTimeVector, []string{"0", "20", "20", "20"})},
			want:  50,
		},
		{
			name:  "ActualFrameDuration last",
			elems: []*sdicom.Element{mustTestElement(t, tag.ActualFrameDuration, []string{"100"})},
			want:  10,
		},
		{
			name:  "nothing stated",
			elems: nil,
			want:  defaultCineFPS,
		},
		{
			name:  "zero rate is not usable",
			elems: []*sdicom.Element{mustTestElement(t, tag.CineRate, []string{"0"})},
			want:  defaultCineFPS,
		},
		{
			name:  "negative rate is not usable",
			elems: []*sdicom.Element{mustTestElement(t, tag.CineRate, []string{"-5"})},
			want:  defaultCineFPS,
		},
		{
			name:  "absurd rate is clamped",
			elems: []*sdicom.Element{mustTestElement(t, tag.CineRate, []string{"9999"})},
			want:  maxCineFPS,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cineFPS(testDataset(t, tc.elems...))
			if diff := got - tc.want; diff > 0.01 || diff < -0.01 {
				t.Errorf("cineFPS = %v, want %v", got, tc.want)
			}
		})
	}
}

// CineRate, RecommendedDisplayFrameRate and ActualFrameDuration are IS in the
// dictionary; a reader that only understands decimals silently skips them and
// falls through to a lower-priority attribute.
func TestCineFPSReadsIntegerStrings(t *testing.T) {
	ds := testDataset(t,
		mustTestElement(t, tag.CineRate, []string{"30"}),
		mustTestElement(t, tag.FrameTime, []string{"100"}), // 10 fps, must lose
	)
	if got := cineFPS(ds); got != 30 {
		t.Errorf("cineFPS = %v, want 30 (integer-string CineRate must win over FrameTime)", got)
	}
}

func TestCineBounce(t *testing.T) {
	sweeping := testDataset(t, mustTestElement(t, tag.PreferredPlaybackSequencing, []int{1}))
	if !cineBounce(sweeping) {
		t.Error("PreferredPlaybackSequencing = 1 must sweep")
	}
	looping := testDataset(t, mustTestElement(t, tag.PreferredPlaybackSequencing, []int{0}))
	if cineBounce(looping) {
		t.Error("PreferredPlaybackSequencing = 0 must loop")
	}
	if cineBounce(testDataset(t)) {
		t.Error("an absent PreferredPlaybackSequencing must loop")
	}
}

func TestCineLoopRange(t *testing.T) {
	tests := []struct {
		name       string
		start      int
		stop       int
		frames     int
		wantFrom   int
		wantTo     int
		omitStart  bool
		omitStop   bool
		frameCount int
	}{
		{name: "no trim", omitStart: true, omitStop: true, frames: 60, wantFrom: 0, wantTo: 59},
		{name: "both stated", start: 10, stop: 40, frames: 60, wantFrom: 9, wantTo: 39},
		{name: "start only", start: 10, omitStop: true, frames: 60, wantFrom: 9, wantTo: 59},
		{name: "stop only", omitStart: true, stop: 40, frames: 60, wantFrom: 0, wantTo: 39},
		{name: "start past stop is ignored", start: 50, stop: 20, frames: 60, wantFrom: 0, wantTo: 19},
		{name: "out of range start", start: 99, omitStop: true, frames: 60, wantFrom: 0, wantTo: 59},
		{name: "out of range stop", omitStart: true, stop: 99, frames: 60, wantFrom: 0, wantTo: 59},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var elems []*sdicom.Element
			if !tc.omitStart {
				elems = append(elems, mustTestElement(t, tag.StartTrim, []string{itoaTest(tc.start)}))
			}
			if !tc.omitStop {
				elems = append(elems, mustTestElement(t, tag.StopTrim, []string{itoaTest(tc.stop)}))
			}
			from, to := cineLoopRange(testDataset(t, elems...), tc.frames)
			if from != tc.wantFrom || to != tc.wantTo {
				t.Errorf("cineLoopRange = %d..%d, want %d..%d", from, to, tc.wantFrom, tc.wantTo)
			}
		})
	}
}

func TestAnyMultiFrame(t *testing.T) {
	stills := []chapter{{frames: 1}, {frames: 1}}
	if anyMultiFrame(stills) {
		t.Error("an all-single-frame series must not enter chapter mode")
	}
	mixed := []chapter{{frames: 1}, {frames: 30}, {frames: 1}}
	if !anyMultiFrame(mixed) {
		t.Error("a series holding a clip must enter chapter mode")
	}
	if got, want := totalChapterFrames(mixed), 32; got != want {
		t.Errorf("totalChapterFrames = %d, want %d", got, want)
	}
}

// A series of clips and stills becomes one chapter per instance, in
// InstanceNumber order, each labelled and timed from its own header.
func TestScanChaptersOnFiles(t *testing.T) {
	dir := t.TempDir()
	clip := writeMultiframeTestFile(t, dir, 30, 2)
	still := writeMultiframeTestFile(t, dir, 1, 1)

	chapters := scanChapters([]string{clip, still}, nil)
	if len(chapters) != 2 {
		t.Fatalf("got %d chapters, want 2", len(chapters))
	}
	if chapters[0].path != still || chapters[1].path != clip {
		t.Error("chapters are not in InstanceNumber order")
	}
	if chapters[0].playable() {
		t.Error("a single-frame instance must not be playable")
	}
	if !chapters[1].playable() || chapters[1].frames != 30 {
		t.Errorf("clip chapter = %d frames, playable=%v; want 30, true",
			chapters[1].frames, chapters[1].playable())
	}
	if chapters[1].loopFrom != 0 || chapters[1].loopTo != 29 {
		t.Errorf("untrimmed clip loops %d..%d, want 0..29", chapters[1].loopFrom, chapters[1].loopTo)
	}
	if !anyMultiFrame(chapters) {
		t.Error("a series holding a 30-frame instance must enter chapter mode")
	}
}

// One unreadable instance must not cost the user the whole series.
func TestScanChaptersUnreadableFileIsOneFrameChapter(t *testing.T) {
	dir := t.TempDir()
	good := writeMultiframeTestFile(t, dir, 4, 1)
	bad := filepath.Join(dir, "corrupt.dcm")
	if err := os.WriteFile(bad, []byte("this is not a DICOM file"), 0o644); err != nil {
		t.Fatal(err)
	}

	chapters := scanChapters([]string{good, bad}, nil)
	if len(chapters) != 2 {
		t.Fatalf("got %d chapters, want 2", len(chapters))
	}
	var corrupt *chapter
	for i := range chapters {
		if chapters[i].path == bad {
			corrupt = &chapters[i]
		}
	}
	if corrupt == nil {
		t.Fatal("the unreadable file lost its chapter")
	}
	if corrupt.frames != 1 {
		t.Errorf("unreadable chapter holds %d frames, want 1", corrupt.frames)
	}
	if corrupt.fps != defaultCineFPS {
		t.Errorf("unreadable chapter fps = %v, want the default %v", corrupt.fps, defaultCineFPS)
	}
}

func TestScanChaptersReportsProgress(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		writeMultiframeTestFile(t, dir, 2, 1),
		writeMultiframeTestFile(t, dir, 2, 2),
		writeMultiframeTestFile(t, dir, 2, 3),
	}
	var seen []int
	scanChapters(paths, func(done int) { seen = append(seen, done) })
	if len(seen) != 3 || seen[0] != 1 || seen[2] != 3 {
		t.Errorf("progress = %v, want 1,2,3", seen)
	}
}

// TestSampleEchoSeries runs the chapter split, labelling, timing and clip
// buffering over a real ultrasound series — the synthetic files above are
// uncompressed greyscale, while echo is JPEG Baseline YBR colour. Run manually:
//
//	DICOMQR_ECHO_SAMPLE="…/ECHO TRANSTHORACIC (TTE) …/Unknown Series (4)" \
//	  go test -run TestSampleEchoSeries -v .
func TestSampleEchoSeries(t *testing.T) {
	dir := os.Getenv("DICOMQR_ECHO_SAMPLE")
	if dir == "" {
		t.Skip("set DICOMQR_ECHO_SAMPLE to an ultrasound series folder")
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

	start := time.Now()
	chapters := scanChapters(paths, nil)
	scanTime := time.Since(start)

	if !anyMultiFrame(chapters) {
		t.Fatal("an echo series must put the viewer into chapter mode")
	}
	clips, stills, labelled, timed := 0, 0, 0, 0
	for _, c := range chapters {
		if c.playable() {
			clips++
			if c.fps != defaultCineFPS {
				timed++ // a rate actually read from the file, not the fallback
			}
		} else {
			stills++
		}
		if c.label != "" && !strings.HasSuffix(c.label, ". Clip") && !strings.HasSuffix(c.label, ". Still") {
			labelled++
		}
	}
	t.Logf("%d chapters (%d clips, %d stills), %d frames; scanned in %v",
		len(chapters), clips, stills, totalChapterFrames(chapters), scanTime.Round(time.Millisecond))
	t.Logf("%d chapters carry a real description, %d clips state their own rate", labelled, timed)
	for i := 0; i < len(chapters) && i < 5; i++ {
		c := chapters[i]
		t.Logf("  %-28s %3d frames  %.0f fps  bounce=%v  loop %d..%d  %d bpm",
			c.label, c.frames, c.fps, c.bounce, c.loopFrom, c.loopTo, c.heartRate)
	}
	if clips == 0 {
		t.Fatal("no multi-frame instances found — not an echo series")
	}
	if labelled == 0 {
		t.Error("no chapter got a real description: the labelling fallbacks all failed")
	}

	// Every chapter must be one file, and every frame of it reachable.
	if got, want := len(expandFrames(chapters)), totalChapterFrames(chapters); got != want {
		t.Errorf("expandFrames produced %d slices, want %d", got, want)
	}

	// Buffer the longest clip and check it decodes whole, in order, distinctly.
	longest := 0
	for i, c := range chapters {
		if c.frames > chapters[longest].frames {
			longest = i
		}
	}
	c := chapters[longest]
	start = time.Now()
	b := startClipBuffer(c, nil)
	waitForBuffer(t, b)
	fillTime := time.Since(start)
	if b.isFailed() {
		t.Fatalf("buffering %s failed", c.label)
	}
	t.Logf("buffered %q: %d/%d frames in %v (%.1f ms/frame), truncated=%v",
		c.label, b.decodedCount(), c.frames, fillTime.Round(time.Millisecond),
		float64(fillTime.Milliseconds())/float64(maxInt(1, b.decodedCount())), b.truncated())

	if b.decodedCount() != b.capacity() {
		t.Errorf("decoded %d of a %d-frame capacity", b.decodedCount(), b.capacity())
	}
	var prev *decodedFrame
	for i := 0; i < b.capacity(); i++ {
		df := b.frame(i)
		if df == nil {
			t.Fatalf("frame %d of %d is missing from a complete buffer", i, b.capacity())
		}
		if df.colorImg == nil {
			t.Fatalf("frame %d decoded as greyscale; echo is colour", i)
		}
		if _, ok := df.colorImg.(*image.RGBA); !ok {
			t.Fatalf("frame %d is %T, want *image.RGBA converted in the buffer", i, df.colorImg)
		}
		if prev != nil && sameColourFrame(prev, df) {
			t.Errorf("frame %d is pixel-identical to frame %d", i, i-1)
		}
		prev = df
	}
}

func sameColourFrame(a, b *decodedFrame) bool {
	ra, okA := a.colorImg.(*image.RGBA)
	rb, okB := b.colorImg.(*image.RGBA)
	if !okA || !okB || len(ra.Pix) != len(rb.Pix) {
		return false
	}
	for i := range ra.Pix {
		if ra.Pix[i] != rb.Pix[i] {
			return false
		}
	}
	return true
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
