package main

// Two windows that make pixel masking something you can see rather than
// estimate: the region picker, opened from the profile editor to define a
// profile's rectangles on any image, and the review window, opened from the
// Modification dialog to walk the images of an actual run and mark what the
// profile does not already cover.
//
// The review window is where masking is really done. Fractional geometry fails
// in one way — a study that mixes image sizes — so files are grouped by
// modality, dimensions, and (for ultrasound) whether they declare a calibrated
// region, and *every* file of a group can be stepped through. One
// representative per group is not enough: an echo study's analysis screens
// share dimensions with nothing else and print their text wherever the vendor
// chose, differently from one screen to the next. Drawing happens on the image
// in front of you, and what is drawn joins the run's regions immediately, so
// stepping on to the next image shows whether it is covered too.

import (
	"fmt"
	"image"
	"image/color"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	sqweekdialog "github.com/sqweek/dialog"
	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// loadMaskFrame parses path and renders one frame for display — the middle one
// for a multi-frame file, since the first frame of a cine loop is often blank.
func loadMaskFrame(path string) (image.Image, *sdicom.Dataset, error) {
	p, err := parseDicomFile(path)
	if err != nil {
		return nil, nil, err
	}
	if len(p.frames) == 0 {
		return nil, nil, fmt.Errorf("%s has no image data", filepath.Base(path))
	}
	state, err := p.frameState(len(p.frames) / 2)
	if err != nil {
		return nil, nil, err
	}
	ds, err := safeParseFile(path, nil, sdicom.SkipPixelData())
	if err != nil {
		return nil, nil, err
	}
	return state.img, &ds, nil
}

// showMaskRegionPicker opens a DICOM file of the user's choosing and lets
// rectangles be dragged onto it. current seeds the canvas with the profile's
// existing rectangles so an edit starts from what is already there; onApply
// receives the full replacement set of rectangles.
//
// Only rectangles are picked. The ultrasound rule has no geometry to draw — the
// file supplies it — so its rows are left alone by the caller.
func showMaskRegionPicker(a fyne.App, parent fyne.Window, startDir string,
	current []MaskRegion, onApply func([]MaskRegion)) {

	go func() {
		picker := sqweekdialog.File().Title("Choose an image to mark up").
			Filter("DICOM files", "dcm", "DCM").Filter("All files", "*")
		if startDir != "" {
			picker = picker.SetStartDir(startDir)
		}
		path, err := picker.Load()
		if err != nil {
			return // cancelled
		}
		img, ds, lerr := loadMaskFrame(path)
		fyne.Do(func() {
			if lerr != nil {
				dialog.ShowError(fmt.Errorf("opening %s: %w", filepath.Base(path), lerr), parent)
				return
			}
			buildMaskRegionPicker(a, parent, path, img, ds, current, onApply)
		})
	}()
}

// buildMaskRegionPicker opens the picker window on an already-loaded frame.
func buildMaskRegionPicker(a fyne.App, parent fyne.Window, path string, img image.Image,
	ds *sdicom.Dataset, current []MaskRegion, onApply func([]MaskRegion)) {

	canvasWidget := newMaskCanvas(img, maskCanvasEdit)
	// Only rectangles are editable here; a US rule carries no geometry to draw.
	rects := make([]MaskRegion, 0, len(current))
	for _, r := range current {
		if maskRegionMode(r) == maskModeRect {
			rects = append(rects, r)
		}
	}
	canvasWidget.setRects(rects)

	openOwnedWindow(a, windowSpec{
		Title:    "Mark masked areas — " + filepath.Base(path),
		Size:     fyne.NewSize(900, 760),
		Parent:   parent,
		Blocking: true,
	}, func(win fyne.Window) fyne.CanvasObject {
		countLbl := widget.NewLabel("")
		updateCount := func() {
			switch n := len(canvasWidget.rects); n {
			case 0:
				countLbl.SetText("No rectangles — drag across the image to add one.")
			case 1:
				countLbl.SetText("1 rectangle")
			default:
				countLbl.SetText(fmt.Sprintf("%d rectangles", n))
			}
		}
		canvasWidget.onChange = updateCount
		updateCount()

		// What the file says about itself, so a rectangle is never drawn on an
		// image whose identity is unclear — and, for ultrasound, whether the
		// calibrated region rule would have covered it anyway.
		info := []string{fmt.Sprintf("%d × %d", canvasWidget.imgW, canvasWidget.imgH)}
		if m := datasetFirstString(ds, tag.Modality); m != "" {
			info = append(info, m)
		}
		if isUltrasoundModality(ds) {
			if _, ok := ultrasoundRegionBounds(ds); ok {
				info = append(info, "declares a calibrated region — Outside ultrasound region would mask this file without a rectangle")
			} else {
				info = append(info, "declares no calibrated region — a rectangle is the only way to mask this file")
			}
		}
		infoLbl := widget.NewLabel(strings.Join(info, " · "))
		infoLbl.TextStyle = fyne.TextStyle{Italic: true}
		infoLbl.Wrapping = fyne.TextWrapWord

		undoBtn := widget.NewButton("Undo last", canvasWidget.undo)
		clearBtn := widget.NewButton("Clear all", canvasWidget.clear)
		cancelBtn := widget.NewButton("Cancel", func() { win.Close() })
		applyBtn := widget.NewButton("Apply", func() {
			win.Close()
			onApply(canvasWidget.rects)
		})
		applyBtn.Importance = widget.HighImportance

		buttons := container.NewBorder(widget.NewSeparator(), nil, nil, nil,
			container.NewPadded(container.NewHBox(
				undoBtn, clearBtn, layout.NewSpacer(), countLbl, cancelBtn, applyBtn)))
		head := container.NewVBox(infoLbl, widget.NewSeparator())
		return container.NewBorder(head, buttons, nil, nil, canvasWidget)
	})
}

// maskSeriesFile is one image of a series, as the review window needs it:
// enough header to order it within its series, resolve a scope against it, and
// say which masking means applies — all read in the scan, before any pixel is
// decoded.
type maskSeriesFile struct {
	path     string
	instance int
	modality string
	cols     int
	rows     int
	// calibrated: an ultrasound file declaring its own image region. Read per
	// file even though the window no longer groups by it — it decides what the
	// note says about the image on screen, and what a size-scoped rectangle
	// drawn on that image means (see MaskScope.USRegion).
	calibrated bool
	// chap is the file as the viewer's filmstrip understands it — frame
	// count, label, cine facts — built in the same header pass, so the review
	// window's filmstrip shows exactly the cells the viewer would.
	chap chapter
	// src is the file's masking identity, snapshotted at scan time so the
	// filmstrip can resolve every file's regions to a status without parsing
	// anything again.
	src maskSource
	// hasChapters: this file's series has distinguishable chapters — the same
	// test that decides whether it gets a filmstrip (seriesHasChapters). Set
	// once the whole series has been scanned, since it depends on every file
	// in the series, not just this one. It decides what "Current
	// Series/Chapter" means for a rectangle drawn on this file: confined to
	// the one chapter when true, or the whole series when false — a plain
	// slice stack has nothing a rectangle could be "confined" to below the
	// series, since none of its files are individually distinguishable.
	hasChapters bool
}

// maskSeries is one series of the run, its files in instance order — the unit
// the review window walks.
//
// Series presentation replaced grouping by image geometry. Geometry classes
// were built for the echo case (analysis screens hiding among loops) but cut
// across series in ways that read as arbitrary on other modalities —
// field-reported as confusing — where a series is the natural unit of review
// and instance order is acquisition order. The geometry facts the classes
// carried are kept per file above, because scopes still resolve by them.
type maskSeries struct {
	uid      string
	number   int
	modality string
	desc     string
	files    []maskSeriesFile
}

func (s maskSeries) count() int { return len(s.files) }

// seriesHasChapters reports whether files' chapters are individually
// distinguishable — the same test the filmstrip uses to decide whether to
// show one cell per file (rebuildStrip). A plain single-frame slice stack
// fails it: hundreds of near-identical stills are one continuous acquisition,
// not separate chapters, and get no filmstrip either. A series with at least
// one clip to tell apart passes it and gets both a filmstrip and, for a
// region drawn there, a per-chapter scope (newDrawnScope).
func seriesHasChapters(files []maskSeriesFile) bool {
	if len(files) <= 1 {
		return false
	}
	chaps := make([]chapter, len(files))
	for i, f := range files {
		chaps[i] = f.chap
	}
	return anyMultiFrame(chaps)
}

// How far a rectangle drawn in the review window reaches. Image-only is the
// default: see scopeSelect. The middle option means the current series when
// the series has no distinguishable chapters (a plain slice stack, where a
// repeating layout is a statement about the whole acquisition) and the
// current chapter alone when it does (a rectangle drawn on one clip is not a
// statement about a different view that happens to share its geometry) — see
// newDrawnScope. Either way it stops at the series boundary: the review
// window shows one series at a time, and nothing on screen would reveal a
// rectangle silently reaching into the next one.
//
// The middle option shows whichever of its two faces applies to the series
// being reviewed, so the dropdown always states what a drawn rectangle will
// actually reach — the single static label it replaced never said which of
// the two the user was getting.
const (
	maskScopeImageLabel   = "This image only"
	maskScopeChapterLabel = "Current chapter"
	maskScopeSeriesLabel  = "Current series"
	maskScopeAllLabel     = "All images"
)

// isGroupScopeLabel reports whether label names the middle Applies-to option,
// whichever face it is showing ("Current Series/Chapter" is the pre-1.19
// static label, still honoured so nothing holding the old string changes
// meaning).
func isGroupScopeLabel(label string) bool {
	switch label {
	case maskScopeChapterLabel, maskScopeSeriesLabel, "Current Series/Chapter":
		return true
	}
	return false
}

// groupScopeLabelFor is the face the middle option shows for the file on
// screen: the chapter when its series has chapters to confine a rectangle to,
// the series when it does not.
func groupScopeLabelFor(f maskSeriesFile) string {
	if f.hasChapters {
		return maskScopeChapterLabel
	}
	return maskScopeSeriesLabel
}

// newDrawnScope builds the MaskScope a region drawn on f gets under the
// selected Applies-to label. Identity comes from f's own scan header (its
// maskSource), never a re-parsed dataset — the header pass already captured
// it, and it is the same snapshot the export resolves against.
//
// Narrowing is the safe direction when identity is missing: an image left
// uncovered by a fallback shows up in the filmstrip as red or bare and can be
// marked by hand, where silently widening would blank another series' content
// with nothing on screen to say so.
func newDrawnScope(label string, f maskSeriesFile) *MaskScope {
	sizeGroup := func() *MaskScope {
		s := &MaskScope{Modality: f.modality, Cols: f.cols, Rows: f.rows}
		// For ultrasound the size group also splits on whether the file
		// states its own region — the two are masked by different means, and
		// a rectangle drawn for one is not a statement about the other.
		if strings.EqualFold(f.modality, "US") {
			s.USRegion = usRegionAbsent
			if f.calibrated {
				s.USRegion = usRegionDeclared
			}
		}
		return s
	}
	imageOnly := func() *MaskScope {
		if f.src.sopInstanceUID == "" {
			// No identity to pin it to; fall back to the size group rather
			// than silently applying it everywhere.
			return sizeGroup()
		}
		return &MaskScope{SOPInstanceUID: f.src.sopInstanceUID}
	}

	switch {
	case label == maskScopeAllLabel:
		return nil
	case isGroupScopeLabel(label):
		if f.hasChapters {
			// Distinguishable chapters share this series — a rectangle drawn
			// on one is not a statement about a different view that happens
			// to share its geometry, so it stays confined to the chapter it
			// was drawn on.
			return imageOnly()
		}
		if f.src.seriesInstanceUID == "" {
			return imageOnly()
		}
		s := sizeGroup()
		s.Series = f.src.seriesInstanceUID
		return s
	default:
		return imageOnly()
	}
}

// label names the series for the preview's selector.
func (s maskSeries) label() string {
	var parts []string
	if s.number > 0 {
		parts = append(parts, fmt.Sprintf("Series %d", s.number))
	}
	if s.modality != "" {
		parts = append(parts, s.modality)
	}
	if s.desc != "" {
		parts = append(parts, s.desc)
	}
	if len(parts) == 0 {
		parts = append(parts, "(no series information)")
	}
	return strings.Join(parts, "  ") + fmt.Sprintf("  —  %d file(s)", s.count())
}

// scanMaskSeries reads every file's header and groups them by series, files in
// instance order and series in series-number order. Every file is examined
// rather than a sample: the one image a fractional rectangle gets wrong is the
// reason to look at all. skipped counts files that could not be read or carry
// no image dimensions; they are reported rather than silently absent, because
// a review window that quietly omits files reads as coverage it is not.
func scanMaskSeries(files []string, progress func(done, total int)) (series []maskSeries, skipped int) {
	var (
		mu    sync.Mutex
		byUID = map[string]*maskSeries{}
		done  int
	)
	workers := min(4, len(files))
	if workers == 0 {
		return nil, 0
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				ds, err := safeParseFile(path, nil, sdicom.SkipPixelData())
				mu.Lock()
				done++
				d := done
				cols, rows := 0, 0
				if err == nil {
					cols = datasetInt(&ds, tag.Columns, 0)
					rows = datasetInt(&ds, tag.Rows, 0)
				}
				if err != nil || cols <= 0 || rows <= 0 {
					skipped++
				} else {
					uid := strings.TrimSpace(datasetFirstString(&ds, tag.SeriesInstanceUID))
					sr, ok := byUID[uid]
					if !ok {
						sr = &maskSeries{
							uid:      uid,
							number:   datasetInt(&ds, tag.SeriesNumber, 0),
							modality: datasetFirstString(&ds, tag.Modality),
							desc:     datasetFirstString(&ds, tag.SeriesDescription),
						}
						byUID[uid] = sr
					}
					_, calibrated := ultrasoundRegionBounds(&ds)
					sr.files = append(sr.files, maskSeriesFile{
						path:       path,
						instance:   datasetInt(&ds, tag.InstanceNumber, 0),
						modality:   datasetFirstString(&ds, tag.Modality),
						cols:       cols,
						rows:       rows,
						calibrated: calibrated,
						chap:       chapterFromHeader(0, path, &ds),
						src:        newMaskSource(&ds),
					})
				}
				mu.Unlock()
				if progress != nil {
					progress(d, len(files))
				}
			}
		}()
	}
	for _, f := range files {
		jobs <- f
	}
	close(jobs)
	wg.Wait()

	out := make([]maskSeries, 0, len(byUID))
	for _, sr := range byUID {
		sort.Slice(sr.files, func(i, j int) bool {
			if sr.files[i].instance != sr.files[j].instance {
				return sr.files[i].instance < sr.files[j].instance
			}
			return sr.files[i].path < sr.files[j].path
		})
		// Depends on every file in the series, so it can only be set once
		// sorting (and the series' full file list) is settled.
		hasChapters := seriesHasChapters(sr.files)
		for i := range sr.files {
			sr.files[i].hasChapters = hasChapters
		}
		out = append(out, *sr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].number != out[j].number {
			return out[i].number < out[j].number
		}
		return out[i].label() < out[j].label()
	})
	return out, skipped
}

// Filmstrip status colours — what this run's masking will do to each image,
// painted along the bottom edge of its thumbnail. Fixed rather than themed:
// they are semantic (masked / weaker guarantee / reviewed / would fail), sit
// on the cell's own dark backdrop in either theme, and match the language the
// note under the image already uses.
var (
	maskStatusMasked   = color.NRGBA{R: 0x2E, G: 0xB2, B: 0x5C, A: 0xFF} // green: regions resolve, image is masked
	maskStatusFallback = color.NRGBA{R: 0xE0, G: 0xA0, B: 0x20, A: 0xFF} // amber: masked by fallback rectangles — worth reviewing
	maskStatusExempt   = color.NRGBA{R: 0x4A, G: 0x90, B: 0xD9, A: 0xFF} // blue: reviewed, marked as needing none
	maskStatusFail     = color.NRGBA{R: 0xD6, G: 0x45, B: 0x45, A: 0xFF} // red: cannot be masked, would fail the export
)

// maskFileStatus is what this run's masking will do to one file — the semantic
// value behind every stripe colour, count and jump in the review window, so
// that all of them come from the one resolution the export itself runs.
type maskFileStatus int

const (
	// maskFileUntouched: nothing applies and nothing fails — exports as-is.
	maskFileUntouched maskFileStatus = iota
	maskFileMasked
	maskFileFallback // masked by fallback rectangles — worth reviewing
	maskFileExempt   // reviewed, marked as needing none
	maskFileFail     // cannot be masked, would fail the export
)

// maskFileStatusOf resolves one file's regions — the same resolution the
// export runs — into its status.
func maskFileStatusOf(f maskSeriesFile, regions []MaskRegion) maskFileStatus {
	res, err := maskRects(f.src, regions, f.cols, f.rows)
	switch {
	case err != nil:
		return maskFileFail
	case res.usFellBack:
		return maskFileFallback
	case len(res.rects) > 0:
		return maskFileMasked
	case res.exempt:
		return maskFileExempt
	default:
		return maskFileUntouched
	}
}

// color is the status's stripe and swatch colour. Transparent for untouched:
// a file the run leaves alone needs no flag.
func (s maskFileStatus) color() color.Color {
	switch s {
	case maskFileMasked:
		return maskStatusMasked
	case maskFileFallback:
		return maskStatusFallback
	case maskFileExempt:
		return maskStatusExempt
	case maskFileFail:
		return maskStatusFail
	default:
		return color.Transparent
	}
}

// maskFileStatusColor is the stripe colour a file's filmstrip cell shows.
func maskFileStatusColor(f maskSeriesFile, regions []MaskRegion) color.Color {
	return maskFileStatusOf(f, regions).color()
}

// runStatusCounts resolves every file of the run to its status — the numbers
// behind the review window's counts line and Next failure button. govern is
// the working set's regions-for-modality lookup, passed in so this stays pure.
// Header-only arithmetic over the scan results, cheap enough to recompute on
// every edit.
func runStatusCounts(series []maskSeries, govern func(modality string) []MaskRegion) map[maskFileStatus]int {
	counts := map[maskFileStatus]int{}
	for _, sr := range series {
		for _, f := range sr.files {
			counts[maskFileStatusOf(f, govern(f.modality))]++
		}
	}
	return counts
}

// nextFailingFile finds the first file after (fromSeries, fromFile) that this
// run cannot mask, wrapping past the end of the last series; the starting
// position itself is checked last, so repeated jumps cycle through every
// failure rather than sticking on the first. ok is false when the run has
// none — the state in which it exports clean.
func nextFailingFile(series []maskSeries, govern func(modality string) []MaskRegion,
	fromSeries, fromFile int) (s, f int, ok bool) {

	type pos struct{ s, f int }
	var all []pos
	start := -1
	for si, sr := range series {
		for fi := range sr.files {
			if si == fromSeries && fi == fromFile {
				start = len(all)
			}
			all = append(all, pos{si, fi})
		}
	}
	for i := 1; i <= len(all); i++ {
		// start is -1 when the current position is not in the run (defensive);
		// the +len keeps the index positive and the scan then simply begins at
		// the first file.
		p := all[(start+i+len(all))%len(all)]
		file := series[p.s].files[p.f]
		if maskFileStatusOf(file, govern(file.modality)) == maskFileFail {
			return p.s, p.f, true
		}
	}
	return 0, 0, false
}

// maskWorkingSet is the run's mask regions while they are being reviewed: the
// profile's own list, plus any per-modality override that states its own. An
// override replaces rather than extends, so which list a drawn rectangle joins
// depends on the image it was drawn on.
type maskWorkingSet struct {
	profile []MaskRegion
	perMod  map[string][]MaskRegion // upper-case modality code → regions
	// added is this session's edits, newest last, so undo takes back what the
	// user just did rather than whatever happens to be last in a list.
	added []maskEdit
}

// maskEdit is one region added in this session and the list it went into.
type maskEdit struct {
	code   string // per-modality override code, or "" for the profile's own list
	region MaskRegion
}

func newMaskWorkingSet(p ModProfile) *maskWorkingSet {
	w := &maskWorkingSet{profile: append([]MaskRegion(nil), p.MaskRegions...)}
	for code, ov := range p.PerModality {
		if len(ov.MaskRegions) > 0 {
			if w.perMod == nil {
				w.perMod = map[string][]MaskRegion{}
			}
			w.perMod[strings.ToUpper(code)] = append([]MaskRegion(nil), ov.MaskRegions...)
		}
	}
	return w
}

// governing returns the regions that apply to a modality and the override code
// they came from ("" when the profile's own list governs).
func (w *maskWorkingSet) governing(modality string) ([]MaskRegion, string) {
	code := strings.ToUpper(strings.TrimSpace(modality))
	if regions, ok := w.perMod[code]; ok && len(regions) > 0 {
		return regions, code
	}
	return w.profile, ""
}

// add appends a region to whichever list governs this modality, recording it
// so undo can take back exactly what this session added.
func (w *maskWorkingSet) add(modality string, r MaskRegion) {
	_, code := w.governing(modality)
	if code != "" {
		w.perMod[code] = append(w.perMod[code], r)
	} else {
		w.profile = append(w.profile, r)
	}
	w.added = append(w.added, maskEdit{code: code, region: r})
}

// undo takes back the most recent region added in this session, wherever it
// went. Undoing by position in the list instead would eventually reach the
// regions the profile arrived with — including an ultrasound rule nobody drew,
// whose silent loss changes what gets masked everywhere.
func (w *maskWorkingSet) undo() {
	if len(w.added) == 0 {
		return
	}
	last := w.added[len(w.added)-1]
	w.added = w.added[:len(w.added)-1]

	list := w.profile
	if last.code != "" {
		list = w.perMod[last.code]
	}
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] != last.region {
			continue
		}
		trimmed := append(append([]MaskRegion(nil), list[:i]...), list[i+1:]...)
		if last.code != "" {
			w.perMod[last.code] = trimmed
		} else {
			w.profile = trimmed
		}
		return
	}
}

// canUndo reports whether this session has anything to take back.
func (w *maskWorkingSet) canUndo() bool { return len(w.added) > 0 }

// applyTo returns a copy of p carrying the working regions. The per-modality
// map is cloned rather than written through: the caller's resolved profile is
// shared with the dialog that built it.
func (w *maskWorkingSet) applyTo(p ModProfile) ModProfile {
	out := p
	out.MaskRegions = w.profile
	if len(w.perMod) == 0 {
		return out
	}
	perMod := make(map[string]ModProfile, len(p.PerModality))
	for code, ov := range p.PerModality {
		if regions, ok := w.perMod[strings.ToUpper(code)]; ok {
			ov.MaskRegions = regions
		}
		perMod[code] = ov
	}
	out.PerModality = perMod
	return out
}

// showMaskPreview opens the review window for a run: every file presented
// series by series, one displayed at a time with the regions the export would
// apply drawn over it, and the means to draw more on the image in front of you.
//
// onApply receives the profile with the reviewed regions, for this run only.
func showMaskPreview(a fyne.App, parent fyne.Window, profileName string,
	files []string, resolved ModProfile, onApply func(ModProfile)) {

	prog := widget.NewProgressBar()
	status := widget.NewLabel("Reading image headers…")
	busy := dialog.NewCustom("Preparing preview", "Cancel",
		container.NewVBox(status, prog), parent)
	busy.Show()

	go func() {
		series, skipped := scanMaskSeries(files, func(done, total int) {
			if total > 0 && (done%25 == 0 || done == total) {
				fyne.Do(func() { prog.SetValue(float64(done) / float64(total)) })
			}
		})
		fyne.Do(func() {
			busy.Hide()
			if len(series) == 0 {
				dialog.ShowInformation("Mask preview",
					"None of the selected files carry an image to preview.", parent)
				return
			}
			buildMaskPreview(a, parent, profileName, series, skipped, resolved, onApply)
		})
	}()
}

// buildMaskPreview opens the review window over an already-scanned selection.
func buildMaskPreview(a fyne.App, parent fyne.Window, profileName string,
	series []maskSeries, skipped int, resolved ModProfile, onApply func(ModProfile)) {

	work := newMaskWorkingSet(resolved)

	// The filmstrip outlives the builder closure — its thumbnail decoders must
	// stop when the window goes, exactly as the viewer stops its strip.
	var strip *chapterStrip

	openOwnedWindow(a, windowSpec{
		Title:    "Review masking — " + profileName,
		Size:     fyne.NewSize(940, 880),
		Parent:   parent,
		Blocking: true,
		OnClosed: func() {
			if strip != nil {
				strip.stop()
			}
		},
	}, func(win fyne.Window) fyne.CanvasObject {
		imageCanvas := newMaskCanvas(nil, maskCanvasPreview)
		note := widget.NewLabel("")
		note.Wrapping = fyne.TextWrapWord

		// The filmstrip sits above the slider for any series with clips in it
		// — the same gate, cells and thumbnails as the viewer's, plus a status
		// stripe per image saying what this run's masking will do to it. Its
		// helpers are assigned once navigation exists below.
		stripHolder := container.NewVBox()
		var refreshStripStatuses, rebuildStrip func()

		// Previous/Next step the series; the slider below owns navigation
		// within one. A dropdown listing every series sat at the top of the
		// window, far from the image and from everything else the user
		// touches — the controls are gathered under the image instead, in the
		// order the work is done: pick a series, scrub its images, mark what
		// needs masking, apply.
		seriesLabel := widget.NewLabel("")
		seriesLabel.Truncation = fyne.TextTruncateEllipsis
		posLabel := widget.NewLabel("")
		posLabel.Truncation = fyne.TextTruncateEllipsis
		prevBtn := widget.NewButton("< Series", nil)
		nextBtn := widget.NewButton("Series >", nil)
		imgPrevBtn := widget.NewButton("<", nil)
		imgNextBtn := widget.NewButton(">", nil)
		// The one navigation that is not positional: straight to the next file
		// that would fail the export — the files this window exists to find,
		// and on a plain slice stack the only flag they get, since there is no
		// filmstrip there to carry a red stripe.
		nextFailBtn := widget.NewButton("Next failure", nil)
		// statusCounts is the run-wide tally, one swatch-and-count per status —
		// which also makes it the colour legend, in the same colours the
		// filmstrip stripes use, because both come from maskFileStatus.color.
		statusCounts := container.NewHBox()
		drawCheck := widget.NewCheck("Draw rectangles", nil)
		undoBtn := widget.NewButton("Undo", nil)
		exemptBtn := widget.NewButton("Needs no masking", nil)

		// How far a drawn rectangle reaches. The default is the image it was
		// drawn on and nothing else: an analysis screen's layout says nothing
		// about the next screen's, and blanking the same area across a group
		// destroys report content the export exists to keep. Widening is a
		// deliberate choice, never the assumption. The middle option is seeded
		// from the first file to be shown and then tracks the displayed image
		// (syncScopeOptions).
		scopeSelect := widget.NewSelect(
			[]string{maskScopeImageLabel, groupScopeLabelFor(series[0].files[0]), maskScopeAllLabel}, nil)
		scopeSelect.SetSelected(maskScopeImageLabel)

		// The slider scrubs the selected series — the quick review a large
		// series needs, where Previous/Next is the careful one. Muted while it
		// is moved programmatically, like every synced control in the viewer.
		slider := widget.NewSlider(0, 0)
		slider.Step = 1
		sliderMuting := false

		// Selection is what the controls point at; shownFile/current are what
		// the canvas actually shows. The two differ while a decode is in
		// flight, and everything that acts on "this image" — a drawn
		// rectangle, an exemption, a scope — keys off the DISPLAYED image,
		// because drawing happens on what is in front of you.
		seriesIdx, fileIdx := 0, 0
		var current *sdicom.Dataset  // dataset of the displayed image
		var shownFile maskSeriesFile // scanned header of the displayed image
		haveShown := false

		// scopeFor builds the scope a new region gets from the selector,
		// resolved against the displayed image's own scan header.
		scopeFor := func() *MaskScope { return newDrawnScope(scopeSelect.Selected, shownFile) }

		// governRegions is the working set's regions-for-modality lookup, in
		// the shape the pure status helpers take.
		governRegions := func(modality string) []MaskRegion {
			regions, _ := work.governing(modality)
			return regions
		}

		// syncScopeOptions keeps the middle Applies-to option showing the face
		// that applies to the DISPLAYED image — chapter or series — preserving
		// whichever option is selected across the swap. Called where shownFile
		// is assigned; drawing is keyed off shownFile too (newDrawnScope), so a
		// label mid-swap can never change what a rectangle reaches.
		syncScopeOptions := func() {
			want := groupScopeLabelFor(shownFile)
			if len(scopeSelect.Options) == 3 && scopeSelect.Options[1] == want {
				return
			}
			wasGroup := isGroupScopeLabel(scopeSelect.Selected)
			scopeSelect.Options = []string{maskScopeImageLabel, want, maskScopeAllLabel}
			if wasGroup {
				scopeSelect.Selected = want
			}
			scopeSelect.Refresh()
		}

		// updateStatus recomputes the run-wide tally: the counts line (one
		// swatch and count per status actually present, doubling as the colour
		// legend) and the Next failure button's count. Cheap — header-only
		// arithmetic — so every edit refreshes it.
		updateStatus := func() {
			counts := runStatusCounts(series, governRegions)
			entries := []struct {
				st   maskFileStatus
				word string
			}{
				{maskFileMasked, "masked"},
				{maskFileFallback, "fallback"},
				{maskFileExempt, "exempt"},
				{maskFileFail, "fail"},
				{maskFileUntouched, "untouched"},
			}
			objs := make([]fyne.CanvasObject, 0, 2*len(entries))
			for _, e := range entries {
				n := counts[e.st]
				if n == 0 {
					continue
				}
				if c := e.st.color(); c != color.Color(color.Transparent) {
					// A fixed-size swatch in the status colour; Center gives it
					// the vertical middle of the row the HBox hands it.
					swatch := canvas.NewRectangle(c)
					objs = append(objs, container.NewCenter(
						container.NewGridWrap(fyne.NewSize(10, 10), swatch)))
				}
				objs = append(objs, widget.NewLabel(fmt.Sprintf("%d %s", n, e.word)))
			}
			statusCounts.Objects = objs
			statusCounts.Refresh()

			if fails := counts[maskFileFail]; fails > 0 {
				nextFailBtn.SetText(fmt.Sprintf("Next failure (%d)", fails))
				nextFailBtn.Enable()
			} else {
				nextFailBtn.SetText("No failures")
				nextFailBtn.Disable()
			}
		}

		// render paints the displayed image with the regions currently in
		// force. Separate from loading so a drawn rectangle, an undo or an
		// exemption re-resolves the image on screen without another decode.
		render := func() {
			undoBtn.Enable()
			if !work.canUndo() {
				undoBtn.Disable()
			}
			// Every edit changes what the regions resolve to on every image of
			// the run, so the filmstrip's stripes, the counts line and the
			// failure count re-resolve with the note.
			updateStatus()
			if refreshStripStatuses != nil {
				refreshStripStatuses()
			}
			if !haveShown || current == nil {
				return
			}
			regions, override := work.governing(shownFile.modality)
			var parts []string
			if override != "" {
				parts = append(parts, "Using the "+override+" override's regions")
			}
			if len(regions) == 0 {
				imageCanvas.setRects(nil)
				parts = append(parts, "No masking applies to these images")
			} else {
				// Resolved by the engine's own code, so what is drawn here is
				// what the export blanks. The file here is unmodified, so its
				// own identity is the source identity the export will resolve
				// against.
				res, rerr := maskRects(newMaskSource(current), regions, shownFile.cols, shownFile.rows)
				switch {
				case rerr != nil:
					imageCanvas.setRects(nil)
					parts = append(parts, "This image cannot be masked and would fail the export: "+rerr.Error())
				case len(res.rects) == 0:
					imageCanvas.setRects(nil)
					if res.exempt {
						parts = append(parts, "Marked as needing no masking")
					} else {
						parts = append(parts, "Nothing is masked on this image")
					}
				default:
					imageCanvas.setRects(pixelRectsToRegions(res.rects, shownFile.cols, shownFile.rows))
					if res.usFellBack {
						parts = append(parts, "No calibrated ultrasound region, so the profile's rectangles mask it")
					}
				}
			}
			if drawCheck.Checked {
				parts = append(parts, "Drag on the image to mask an area of "+scopeFor().describe())
			}
			note.SetText(strings.Join(parts, ". ") + ".")
		}

		// chrome syncs the two labels, the slider and the series buttons to the
		// selection. Cheap and synchronous, so scrubbing feels immediate even
		// while the decode behind it lags.
		chrome := func() {
			sr := series[seriesIdx]
			fileIdx = clampInt(fileIdx, 0, sr.count()-1)
			seriesLabel.SetText(fmt.Sprintf("%s   (series %d of %d)",
				sr.label(), seriesIdx+1, len(series)))
			posLabel.SetText(fmt.Sprintf("Image %d of %d — %s",
				fileIdx+1, sr.count(), filepath.Base(sr.files[fileIdx].path)))
			prevBtn.Enable()
			nextBtn.Enable()
			if seriesIdx == 0 {
				prevBtn.Disable()
			}
			if seriesIdx >= len(series)-1 {
				nextBtn.Disable()
			}
			imgPrevBtn.Enable()
			imgNextBtn.Enable()
			if fileIdx == 0 {
				imgPrevBtn.Disable()
			}
			if fileIdx >= sr.count()-1 {
				imgNextBtn.Disable()
			}
			sliderMuting = true
			slider.Max = float64(maxInt(1, sr.count()) - 1)
			slider.SetValue(float64(fileIdx))
			sliderMuting = false
			if sr.count() > 1 {
				slider.Enable()
			} else {
				slider.Disable()
			}
			slider.Refresh()
			if strip != nil {
				strip.selectIndex(fileIdx)
			}
		}

		// One decode in flight at a time: scrubbing the slider fires a change
		// per tick, and a parse-and-decode per tick would pile up behind the
		// slowest file. A finished decode is displayed (a frame along the
		// scrub path is exactly the quick look the slider exists for), then
		// the load chases the selection until they agree. A result from a
		// series switched away from is dropped rather than shown against the
		// wrong chrome.
		inFlight := false
		var requestLoad func()
		requestLoad = func() {
			if inFlight {
				return // the completion below chases the selection
			}
			sr := series[seriesIdx]
			idxS, idxF := seriesIdx, clampInt(fileIdx, 0, sr.count()-1)
			f := sr.files[idxF]
			if haveShown && shownFile.path == f.path {
				render()
				return
			}
			inFlight = true
			go func() {
				img, ds, err := loadMaskFrame(f.path)
				fyne.Do(func() {
					inFlight = false
					if idxS == seriesIdx {
						shownFile = f
						syncScopeOptions()
						if err != nil {
							current = nil
							haveShown = false
							imageCanvas.setRects(nil)
							note.SetText(fmt.Sprintf("%s could not be displayed: %v", filepath.Base(f.path), err))
						} else {
							current = ds
							haveShown = true
							imageCanvas.setImage(img)
							render()
						}
					}
					if idxS != seriesIdx || idxF != fileIdx {
						requestLoad() // the user moved on while decoding
					}
				})
			}()
		}

		goTo := func(idx int) {
			idx = clampInt(idx, 0, series[seriesIdx].count()-1)
			if idx == fileIdx {
				return
			}
			fileIdx = idx
			chrome()
			requestLoad()
		}
		// jumpTo lands on any file of any series — what the Next failure
		// button needs, where goTo/selectSeries are the positional steps.
		jumpTo := func(s, f int) {
			if s < 0 || s >= len(series) {
				return
			}
			sameSeries := s == seriesIdx
			seriesIdx = s
			fileIdx = clampInt(f, 0, series[s].count()-1)
			if !sameSeries && rebuildStrip != nil {
				rebuildStrip()
			}
			chrome()
			requestLoad()
		}
		selectSeries := func(idx int) {
			if idx == seriesIdx {
				return
			}
			jumpTo(idx, 0)
		}

		// The filmstrip helpers, now that navigation exists to hang them on.
		// Statuses re-resolve against the working set, so a rectangle drawn
		// two images ago shows on every cell it reaches.
		refreshStripStatuses = func() {
			if strip == nil {
				return
			}
			sr := series[seriesIdx]
			colors := make([]color.Color, len(sr.files))
			for i, f := range sr.files {
				regions, _ := work.governing(f.modality)
				colors[i] = maskFileStatusColor(f, regions)
			}
			strip.setStatuses(colors)
		}
		rebuildStrip = func() {
			if strip != nil {
				strip.stop()
				strip = nil
			}
			stripHolder.Objects = nil
			sr := series[seriesIdx]
			chaps := make([]chapter, len(sr.files))
			for i, f := range sr.files {
				chaps[i] = f.chap
			}
			// The viewer's gate, applied per series: a filmstrip for anything
			// with clips to tell apart, and none for a plain single-frame
			// stack, whose hundreds of near-identical thumbnails would cost
			// decode time and say nothing. The same test decides what
			// "Current Series/Chapter" reaches (newDrawnScope) — a plain
			// stack has no distinguishable chapter to confine a rectangle to
			// below the series.
			if seriesHasChapters(sr.files) {
				strip = newChapterStrip(chaps, func(index int) { goTo(index) })
				strip.selectIndex(fileIdx)
				refreshStripStatuses()
				stripHolder.Objects = []fyne.CanvasObject{strip.object()}
			}
			stripHolder.Refresh()
		}
		prevBtn.OnTapped = func() { selectSeries(seriesIdx - 1) }
		nextBtn.OnTapped = func() { selectSeries(seriesIdx + 1) }
		imgPrevBtn.OnTapped = func() { goTo(fileIdx - 1) }
		imgNextBtn.OnTapped = func() { goTo(fileIdx + 1) }
		nextFailBtn.OnTapped = func() {
			if s, f, ok := nextFailingFile(series, governRegions, seriesIdx, fileIdx); ok {
				jumpTo(s, f)
			}
		}
		slider.OnChanged = func(v float64) {
			if sliderMuting {
				return
			}
			goTo(int(v))
		}
		undoBtn.OnTapped = func() {
			work.undo()
			render()
		}
		scopeSelect.OnChanged = func(string) { render() }
		drawCheck.OnChanged = func(on bool) {
			if on {
				imageCanvas.setMode(maskCanvasEdit)
			} else {
				imageCanvas.setMode(maskCanvasPreview)
			}
			render()
		}
		// A drawn rectangle joins the run's regions rather than the canvas's
		// own list, so the next redraw shows it resolved alongside everything
		// else — and carries the scope chosen above, which decides whether the
		// next image sees it at all.
		imageCanvas.onDraw = func(r MaskRegion) {
			if !haveShown {
				return // nothing on screen to draw on
			}
			r.AppliesTo = scopeFor()
			work.add(shownFile.modality, r)
			render()
		}
		// A drag with Draw rectangles unticked does nothing, and doing nothing
		// silently reads as broken — say why instead. The next render clears
		// the hint.
		imageCanvas.onBlockedDrag = func() {
			note.SetText("Tick Draw rectangles to mark an area on this image.")
		}
		// Marking an image as needing no masking is a statement, not a
		// no-op: without it an ultrasound image that declares no calibrated
		// region fails the export, which is right until somebody has looked
		// at it and decided otherwise.
		exemptBtn.OnTapped = func() {
			if !haveShown {
				return
			}
			work.add(shownFile.modality, MaskRegion{Mode: maskModeNone, AppliesTo: scopeFor()})
			render()
		}

		// The buttons step series, so the keyboard keeps the per-image stepping
		// a careful review still needs: arrows move one image, Ctrl+arrows one
		// series — the same keys, and the same division, as the image viewer.
		win.Canvas().SetOnTypedKey(func(e *fyne.KeyEvent) {
			switch e.Name {
			case fyne.KeyLeft, fyne.KeyUp:
				goTo(fileIdx - 1)
			case fyne.KeyRight, fyne.KeyDown:
				goTo(fileIdx + 1)
			}
		})
		win.Canvas().AddShortcut(
			&desktop.CustomShortcut{KeyName: fyne.KeyLeft, Modifier: fyne.KeyModifierControl},
			func(fyne.Shortcut) { selectSeries(seriesIdx - 1) })
		win.Canvas().AddShortcut(
			&desktop.CustomShortcut{KeyName: fyne.KeyRight, Modifier: fyne.KeyModifierControl},
			func(fyne.Shortcut) { selectSeries(seriesIdx + 1) })

		rebuildStrip()
		chrome()
		// Counts come from the scan, not the decode, so they are right before
		// the first image has even loaded — and stay right if it never does.
		updateStatus()
		requestLoad()

		// Two sentences, not the essay this used to be: the counts line teaches
		// the colours, the labelled buttons teach the navigation, and the note
		// under the image states what the export will do and why — the window
		// explains itself where each thing happens instead of all at once up
		// here. The full account lives in the manual (§8.9).
		captionText := "Every image of the run, series by series — what is shown on the image is exactly what " +
			"the export will blank, and the note below says why. Arrow keys step images, Ctrl+arrows step series."
		if skipped > 0 {
			captionText += fmt.Sprintf(" %d file(s) in the selection could not be read or hold no image, and are not shown.", skipped)
		}
		caption := widget.NewLabel(captionText)
		caption.TextStyle = fyne.TextStyle{Italic: true}
		caption.Wrapping = fyne.TextWrapWord

		// A session's drawn regions and exemptions are minutes of careful work;
		// one stray click on Cancel or the title-bar X must not silently take
		// them. Programmatic closes — the Apply path, a parent cascading shut —
		// bypass the intercept, which is right: those are not stray clicks.
		confirmClose := func() {
			if !work.canUndo() {
				win.Close()
				return
			}
			msg := widget.NewLabel(fmt.Sprintf(
				"Discard the %d change(s) marked in this session?", len(work.added)))
			msg.Wrapping = fyne.TextWrapWord
			dialog.NewCustomConfirm("Discard changes", "Discard", "Keep working", msg,
				func(discard bool) {
					if discard {
						win.Close()
					}
				}, win).Show()
		}
		win.SetCloseIntercept(confirmClose)

		cancelBtn := widget.NewButton("Cancel", confirmClose)
		applyBtn := widget.NewButton("Use these regions", func() {
			win.Close()
			onApply(work.applyTo(resolved))
		})
		applyBtn.Importance = widget.HighImportance

		// Everything the user touches sits under the image, in the order the
		// work is done: scrub the series, step to another series, see which
		// image is on screen, mark it, apply. Each row is left-aligned, so the
		// eye follows one edge down rather than hunting across the window.
		// Border, not HBox: a Label with Truncation reports a minimal MinSize,
		// and HBox gives every child exactly its MinSize — which collapsed the
		// series name to a bare ellipsis. As the centre of a Border it takes
		// the width the buttons leave and truncates only if it genuinely runs
		// out, which is how the download-folder labels are laid out too.
		sliderRow := container.NewBorder(nil, nil, imgPrevBtn, imgNextBtn, slider)
		seriesRow := container.NewBorder(nil, nil,
			container.NewHBox(prevBtn, nextBtn), nextFailBtn, seriesLabel)
		statusRow := container.NewBorder(nil, nil, nil, statusCounts, posLabel)
		editRow := container.NewHBox(drawCheck,
			widget.NewLabel("Applies to"), scopeSelect, exemptBtn, undoBtn)
		// The note shares the button row rather than owning one: it is the
		// running commentary on what masking will do to this image, and it
		// belongs beside the button that commits it. Border keeps the buttons
		// hard right whatever the note says.
		actionRow := container.NewBorder(nil, nil, nil,
			container.NewHBox(cancelBtn, applyBtn), note)

		head := container.NewVBox(caption, widget.NewSeparator())
		foot := container.NewBorder(widget.NewSeparator(), nil, nil, nil, container.NewPadded(
			container.NewVBox(stripHolder, sliderRow, seriesRow, statusRow, editRow, actionRow)))
		return container.NewBorder(head, foot, nil, nil, imageCanvas)
	})
}
