package main

// Chapters: a series split into independently playable units, one per SOP
// instance, so an ultrasound study reads as the set of cine loops it actually
// is rather than as one undifferentiated stack of frames.
//
// Without this, a real echo study (173 instances — 88 clips of 30-60 frames
// interleaved with 85 stills) presents as a single 5256-position scrub with no
// seam between one acquisition and the next and no way to play any of them.
// Because a chapter is exactly one file, the chapter index is the file index:
// there is no flat-index arithmetic anywhere.
//
// A single-frame instance is a one-frame chapter rather than being merged into
// a neighbour — a still captured between loops is its own thing, and pretending
// otherwise would put unrelated images inside a loop's playback range.

import (
	"sort"
	"strconv"
	"strings"
	"unicode"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// ── Cine timing ───────────────────────────────────────────────────────────────
//
// No single attribute states playback rate universally: ultrasound cine
// typically writes CineRate and/or FrameTime, some vendors write only
// RecommendedDisplayFrameRate, variable-rate acquisitions use FrameTimeVector,
// and NM uses ActualFrameDuration. Every value is treated as untrusted — zero,
// negative and absurd rates all appear in real files and would divide by zero
// or spin the player.

const (
	// defaultCineFPS is used when an instance carries no usable timing
	// attribute. Deliberately conservative: it reads as motion without claiming
	// a precision the file never stated.
	defaultCineFPS = 15.0
	// minCineFPS — below 1 fps playback is indistinguishable from a stalled viewer.
	minCineFPS = 1.0
	// maxCineFPS — above the panel's refresh rate there is nothing left to
	// present; 120 also keeps a corrupt four-digit rate from spinning the player.
	maxCineFPS = 120.0
)

// chapter is one instance of a series presented as an independently playable
// unit: how many frames it holds, what to call it, and how it asks to be played.
//
// loopFrom/loopTo bound cine playback only — the frame slider always spans the
// whole clip, so an instance's preferred trim never hides frames from the user.
type chapter struct {
	path        string
	instanceNum int
	frames      int
	label       string
	fps         float64
	bounce      bool // sweep (forward then backward) rather than loop
	loopFrom    int
	loopTo      int
	heartRate   int // bpm, 0 when not stated

	// MR phase attributes, read in the same header pass — see mrphases.go.
	// sliceLoc/orientation drive the structural detection; the rest only name
	// the detected phases.
	sliceLoc    float64
	hasSliceLoc bool
	orientation string // ImageOrientationPatient values joined; "" when absent
	echoNumber  int    // EchoNumbers, 0 when absent
	echoTime    string // EchoTime as stated (DS), "" when absent
	seqName     string // SequenceName (vendor pulse-sequence id, e.g. *ep_b50t)
	temporalPos int    // TemporalPositionIdentifier, 0 when absent
	acqNumber   int    // AcquisitionNumber, 0 when absent
}

// playable reports whether there is anything to play: a still has not, so the
// transport controls disable for it rather than leaving a dead button.
func (c chapter) playable() bool { return c.frames > 1 }

// anyMultiFrame reports whether at least one instance holds more than one frame
// — the condition for the viewer to enter chapter mode at all. An ordinary
// all-single-frame CT/MR series reports false and keeps its plain
// one-slider-position-per-slice behaviour untouched.
func anyMultiFrame(chapters []chapter) bool {
	for _, c := range chapters {
		if c.playable() {
			return true
		}
	}
	return false
}

// totalChapterFrames is every frame of every chapter — the "N clips, M frames"
// summary, not a navigable index.
func totalChapterFrames(chapters []chapter) int {
	total := 0
	for _, c := range chapters {
		total += c.frames
	}
	return total
}

// scanChapters reads each file's header — pixel data skipped — and builds one
// chapter per file, ordered by InstanceNumber, reporting progress after each.
// This is the same single header pass the viewer already needed to order a
// series, so chapter information costs nothing extra.
//
// A file whose header cannot be read becomes a single-frame chapter: one
// unreadable instance must not cost the user the whole series, and the frame it
// occupies reports its own load error when selected.
func scanChapters(paths []string, progress func(done int)) []chapter {
	chapters := make([]chapter, len(paths))
	for i, p := range paths {
		ds, err := safeParseFile(p, nil, sdicom.SkipPixelData())
		if err != nil {
			chapters[i] = chapterFromHeader(i, p, nil)
		} else {
			chapters[i] = chapterFromHeader(i, p, &ds)
		}
		if progress != nil {
			progress(i + 1)
		}
	}
	sort.SliceStable(chapters, func(i, j int) bool {
		if chapters[i].instanceNum != chapters[j].instanceNum {
			return chapters[i].instanceNum < chapters[j].instanceNum
		}
		return chapters[i].path < chapters[j].path
	})
	return chapters
}

// chapterFromHeader builds one chapter from an already-parsed header. Split out
// from scanChapters so the labelling and timing rules are testable without
// writing files. A nil header (unreadable instance) yields a plain single-frame
// chapter at default timing.
func chapterFromHeader(fileIndex int, path string, ds *sdicom.Dataset) chapter {
	if ds == nil {
		return chapter{
			path: path, frames: 1,
			label: fallbackChapterLabel(fileIndex+1, 1),
			fps:   defaultCineFPS, loopTo: 0,
		}
	}
	frames := datasetInt(ds, tag.NumberOfFrames, 1)
	if frames < 1 {
		frames = 1
	}
	instance := datasetInt(ds, tag.InstanceNumber, fileIndex+1)
	from, to := cineLoopRange(ds, frames)
	sliceLoc, hasSliceLoc := datasetFloat(ds, tag.SliceLocation)
	return chapter{
		path:        path,
		instanceNum: instance,
		frames:      frames,
		label:       chapterLabel(ds, instance, frames),
		fps:         cineFPS(ds),
		bounce:      cineBounce(ds),
		loopFrom:    from,
		loopTo:      to,
		heartRate:   maxInt(0, datasetInt(ds, tag.HeartRate, 0)),
		sliceLoc:    sliceLoc,
		hasSliceLoc: hasSliceLoc,
		orientation: strings.Join(datasetStrings(ds, tag.ImageOrientationPatient), `\`),
		echoNumber:  datasetInt(ds, tag.EchoNumbers, 0),
		echoTime:    datasetString(ds, tag.EchoTime),
		seqName:     datasetString(ds, tag.SequenceName),
		temporalPos: datasetInt(ds, tag.TemporalPositionIdentifier, 0),
		acqNumber:   datasetInt(ds, tag.AcquisitionNumber, 0),
	}
}

// chapterLabel is "3. Apical 4 chamber" — the instance number (which is what
// the operator and the report refer to) followed by the most specific human
// description the header offers. The frame count is deliberately absent: the
// filmstrip shows it as its own badge, and repeating it here would only crowd
// the label.
//
// The named-view attributes are tried first but are frequently all absent — the
// real GE echo study this was checked against carries no ImageComments,
// ProtocolName, ViewCodeSequence or usable ImageType on any of its 88 clips.
// What such files do carry is what kind of ultrasound image it is, so
// ultrasoundContent derives that as the next-best descriptor before falling
// back to a bare "Clip". Identifying the view then rests on the filmstrip
// thumbnail, which is exactly what it is there for.
func chapterLabel(ds *sdicom.Dataset, instance, frames int) string {
	desc := firstDescription(
		datasetString(ds, tag.ImageComments),
		datasetString(ds, tag.ProtocolName),
		codeMeaning(ds, tag.ViewCodeSequence),
		ultrasoundContent(ds),
		imageTypeDetail(ds),
		datasetString(ds, tag.SeriesDescription),
	)
	if desc == "" {
		return fallbackChapterLabel(instance, frames)
	}
	return strconv.Itoa(instance) + ". " + desc
}

// firstDescription returns the first candidate that reads as a description
// rather than an identifier. A candidate with no letter in it is skipped: real
// files put bare codes in these attributes — "0001" in ImageType, and
// "15003.0.68612207@" in the ProtocolName of the SPECT study checked against
// this code — and showing one as a chapter's name is worse than falling through
// to whatever the next attribute says.
func firstDescription(candidates ...string) string {
	for _, c := range candidates {
		if s := strings.TrimSpace(c); s != "" && containsLetter(s) {
			return s
		}
	}
	return ""
}

func containsLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

func fallbackChapterLabel(instance, frames int) string {
	if frames > 1 {
		return strconv.Itoa(instance) + ". Clip"
	}
	return strconv.Itoa(instance) + ". Still"
}

// ultrasoundContent names what kind of ultrasound image this is, from the
// region calibration sequence: "2D", "2D + Colour flow", "PW Doppler" and so
// on. Only the four image-bearing region types are named — the others (ECG
// trace, grey bar, colour bar…) are display furniture that appears alongside
// the image and would only add noise to a label.
func ultrasoundContent(ds *sdicom.Dataset) string {
	var kinds []string
	add := func(kind string) {
		for _, k := range kinds {
			if k == kind {
				return
			}
		}
		kinds = append(kinds, kind)
	}
	for _, region := range sequenceItems(ds, tag.SequenceOfUltrasoundRegions) {
		switch datasetInt(region, tag.RegionDataType, -1) {
		case 1:
			add("2D")
		case 2:
			add("Colour flow")
		case 3:
			add("PW Doppler")
		case 4:
			add("CW Doppler")
		}
	}
	// Colour can be flagged on the image without a colour region being calibrated.
	if datasetInt(ds, tag.UltrasoundColorDataPresent, 0) != 0 {
		add("Colour flow")
	}
	return strings.Join(kinds, " + ")
}

// imageTypeDetail is ImageType's third value, which often names the
// acquisition ("2D", "COLOR_FLOW", "RECON TOMO"). A vendor that puts a bare
// numeric code there is filtered by firstDescription along with every other
// identifier-shaped candidate.
func imageTypeDetail(ds *sdicom.Dataset) string {
	values := datasetStrings(ds, tag.ImageType)
	if len(values) < 3 {
		return ""
	}
	return strings.TrimSpace(values[2])
}

// codeMeaning is the CodeMeaning of a code sequence's first item — the standard
// place a vendor names an echo view ("Apical four chamber"), when it names one.
func codeMeaning(ds *sdicom.Dataset, t tag.Tag) string {
	items := sequenceItems(ds, t)
	if len(items) == 0 {
		return ""
	}
	return datasetString(items[0], tag.CodeMeaning)
}

// cineFPS is the rate to play ds back at, in frames per second. Never returns
// zero, negative or non-finite — see the note at the top of this section. The
// priority order prefers the most explicitly display-oriented attribute first
// (RecommendedDisplayFrameRate says what to display at; the others say what was
// acquired).
func cineFPS(ds *sdicom.Dataset) float64 {
	if ds == nil {
		return defaultCineFPS
	}
	// What the object says it should be displayed at, when it says so at all.
	if rate, ok := datasetFloat(ds, tag.RecommendedDisplayFrameRate); ok && rate > 0 {
		return clampFPS(rate)
	}
	if rate, ok := datasetFloat(ds, tag.CineRate); ok && rate > 0 {
		return clampFPS(rate)
	}
	// Acquisition-side timings, converted from per-frame duration in ms.
	if ms, ok := datasetFloat(ds, tag.FrameTime); ok && ms > 0 {
		return clampFPS(1000 / ms)
	}
	// A variable-rate acquisition: play at the mean of the stated intervals. The
	// vector's first element is conventionally 0 (frame 1 has no preceding
	// interval), which the > 0 test discards along with any other bad entry.
	if intervals := datasetFloats(ds, tag.FrameTimeVector); len(intervals) > 0 {
		total, counted := 0.0, 0
		for _, interval := range intervals {
			if interval > 0 {
				total += interval
				counted++
			}
		}
		if counted > 0 {
			return clampFPS(1000 * float64(counted) / total)
		}
	}
	if ms, ok := datasetFloat(ds, tag.ActualFrameDuration); ok && ms > 0 {
		return clampFPS(1000 / ms)
	}
	return defaultCineFPS
}

// cineBounce reports whether the instance asks to be swept (played forward then
// backward) rather than looped. PreferredPlaybackSequencing (0018,1244) defines
// 0 = Looping, 1 = Sweeping; anything else — including the tag's absence —
// means the ordinary forward loop.
func cineBounce(ds *sdicom.Dataset) bool {
	return ds != nil && datasetInt(ds, tag.PreferredPlaybackSequencing, 0) == 1
}

// cineLoopRange is the preferred playback range as 0-based inclusive frame
// indices, from StartTrim/StopTrim (which are 1-based frame numbers). Absent,
// out of range, or inconsistent values mean the whole clip. The trim bounds the
// cine loop only; the frame slider still spans the whole clip, so trimming
// never hides frames from the user.
func cineLoopRange(ds *sdicom.Dataset, frames int) (from, to int) {
	last := maxInt(0, frames-1)
	if ds == nil || frames <= 0 {
		return 0, last
	}
	to = last
	if stop := datasetInt(ds, tag.StopTrim, 0); stop >= 1 && stop <= frames {
		to = stop - 1
	}
	if start := datasetInt(ds, tag.StartTrim, 0); start >= 1 && start <= frames {
		if start-1 <= to { // a start past the stop: ignore the trim
			from = start - 1
		}
	}
	return from, to
}

func clampFPS(fps float64) float64 {
	return clampFloat(fps, minCineFPS, maxCineFPS)
}

// ── Dataset readers ───────────────────────────────────────────────────────────

// datasetString reads the first string value of a tag, trimmed; "" when absent.
func datasetString(ds *sdicom.Dataset, t tag.Tag) string {
	values := datasetStrings(ds, t)
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func datasetStrings(ds *sdicom.Dataset, t tag.Tag) []string {
	if ds == nil {
		return nil
	}
	e, err := ds.FindElementByTag(t)
	if err != nil {
		return nil
	}
	if strs, ok := e.Value.GetValue().([]string); ok {
		return strs
	}
	return nil
}

// datasetFloat reads a numeric attribute whether it is stored as DS (decimal
// string), IS (integer string) or a binary integer VR. Three of the rate
// attributes — CineRate, RecommendedDisplayFrameRate and ActualFrameDuration —
// are IS in the dictionary; reading them only as decimals silently skips them
// in favour of a lower-priority attribute, which is exactly the kind of fault
// that reads as "playback is a bit fast" rather than as a bug.
func datasetFloat(ds *sdicom.Dataset, t tag.Tag) (float64, bool) {
	if ds == nil {
		return 0, false
	}
	e, err := ds.FindElementByTag(t)
	if err != nil {
		return 0, false
	}
	switch v := e.Value.GetValue().(type) {
	case []float64:
		if len(v) > 0 {
			return v[0], true
		}
	case []int:
		if len(v) > 0 {
			return float64(v[0]), true
		}
	case []string:
		if len(v) > 0 {
			if f, convErr := strconv.ParseFloat(strings.TrimSpace(v[0]), 64); convErr == nil {
				return f, true
			}
		}
	}
	return 0, false
}

// datasetFloats reads a multi-valued numeric attribute (FrameTimeVector is DS
// with one entry per frame). Unparseable entries are dropped rather than
// failing the whole vector.
func datasetFloats(ds *sdicom.Dataset, t tag.Tag) []float64 {
	if ds == nil {
		return nil
	}
	e, err := ds.FindElementByTag(t)
	if err != nil {
		return nil
	}
	switch v := e.Value.GetValue().(type) {
	case []float64:
		return v
	case []int:
		out := make([]float64, len(v))
		for i, n := range v {
			out[i] = float64(n)
		}
		return out
	case []string:
		out := make([]float64, 0, len(v))
		for _, s := range v {
			if f, convErr := strconv.ParseFloat(strings.TrimSpace(s), 64); convErr == nil {
				out = append(out, f)
			}
		}
		return out
	}
	return nil
}

// sequenceItems returns a sequence's items as datasets ready for the readers
// above, or nil when the tag is absent or is not a sequence.
func sequenceItems(ds *sdicom.Dataset, t tag.Tag) []*sdicom.Dataset {
	if ds == nil {
		return nil
	}
	e, err := ds.FindElementByTag(t)
	if err != nil {
		return nil
	}
	items, ok := e.Value.GetValue().([]*sdicom.SequenceItemValue)
	if !ok {
		return nil
	}
	out := make([]*sdicom.Dataset, 0, len(items))
	for _, item := range items {
		elems, ok := item.GetValue().([]*sdicom.Element)
		if !ok {
			continue
		}
		out = append(out, &sdicom.Dataset{Elements: elems})
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
