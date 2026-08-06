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
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
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
	ds, err := sdicom.ParseFile(path, nil, sdicom.SkipPixelData())
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

// maskGeometryClass identifies one group of images that a fractional rectangle
// lands on identically.
//
// Every file of the group is kept, not just a representative. Images of one
// size are interchangeable for a rectangle *drawn to fit the size*, but not for
// judging whether it covers what it needs to: an ultrasound study's analysis
// screens share their dimensions with nothing else in the study and print their
// text wherever the vendor chose, differently from one screen to the next. The
// only way to know a mask covers them is to look at each one.
type maskGeometryClass struct {
	modality   string
	cols, rows int
	// calibrated distinguishes ultrasound images that declare their image
	// region from those that do not. They are separate classes even at
	// identical dimensions, because the two are masked by entirely different
	// means — the file's own geometry against the profile's rectangles — and
	// grouping them would hide the ones needing attention among the ones that
	// need none.
	calibrated bool
	files      []string
}

func (c maskGeometryClass) count() int { return len(c.files) }

// How far a rectangle drawn in the review window reaches. Image-only is the
// default: see scopeSelect.
const (
	maskScopeImageLabel = "This image only"
	maskScopeGroupLabel = "This group"
	maskScopeAllLabel   = "All images"
)

// label names the class for the preview's selector.
func (c maskGeometryClass) label() string {
	mod := c.modality
	if mod == "" {
		mod = "(no modality)"
	}
	label := fmt.Sprintf("%s  %d × %d  — %d file(s)", mod, c.cols, c.rows, c.count())
	if strings.EqualFold(c.modality, "US") && !c.calibrated {
		label += "  · no calibrated region"
	}
	return label
}

// scanMaskGeometryClasses groups files by modality and image size, reading
// headers only. Every file is examined rather than a sample: a class present in
// one series out of twenty is exactly the one a fractional rectangle gets
// wrong, and it is the reason to look at all.
func scanMaskGeometryClasses(files []string, progress func(done, total int)) []maskGeometryClass {
	type key struct {
		modality   string
		cols, rows int
		calibrated bool
	}
	var (
		mu      sync.Mutex
		classes = map[key]*maskGeometryClass{}
		done    int
	)
	workers := min(4, len(files))
	if workers == 0 {
		return nil
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				ds, err := sdicom.ParseFile(path, nil, sdicom.SkipPixelData())
				mu.Lock()
				done++
				d := done
				if err == nil {
					_, calibrated := ultrasoundRegionBounds(&ds)
					k := key{
						modality:   datasetFirstString(&ds, tag.Modality),
						cols:       datasetInt(&ds, tag.Columns, 0),
						rows:       datasetInt(&ds, tag.Rows, 0),
						calibrated: calibrated,
					}
					if k.cols > 0 && k.rows > 0 {
						if c, ok := classes[k]; ok {
							c.files = append(c.files, path)
						} else {
							classes[k] = &maskGeometryClass{
								modality: k.modality, cols: k.cols, rows: k.rows,
								calibrated: k.calibrated, files: []string{path},
							}
						}
					}
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

	out := make([]maskGeometryClass, 0, len(classes))
	for _, c := range classes {
		out = append(out, *c)
	}
	// Largest group first: the geometry most of the export is made of is the
	// one to check before the outliers.
	sort.Slice(out, func(i, j int) bool {
		if out[i].count() != out[j].count() {
			return out[i].count() > out[j].count()
		}
		return out[i].label() < out[j].label()
	})
	return out
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

// showMaskPreview opens the review window for a run: every file grouped by
// image geometry, one displayed at a time with the regions the export would
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
		classes := scanMaskGeometryClasses(files, func(done, total int) {
			if total > 0 && (done%25 == 0 || done == total) {
				fyne.Do(func() { prog.SetValue(float64(done) / float64(total)) })
			}
		})
		fyne.Do(func() {
			busy.Hide()
			if len(classes) == 0 {
				dialog.ShowInformation("Mask preview",
					"None of the selected files carry an image to preview.", parent)
				return
			}
			buildMaskPreview(a, parent, profileName, classes, resolved, onApply)
		})
	}()
}

// buildMaskPreview opens the review window over an already-scanned selection.
func buildMaskPreview(a fyne.App, parent fyne.Window, profileName string,
	classes []maskGeometryClass, resolved ModProfile, onApply func(ModProfile)) {

	work := newMaskWorkingSet(resolved)

	openOwnedWindow(a, windowSpec{
		Title:    "Review masking — " + profileName,
		Size:     fyne.NewSize(940, 820),
		Parent:   parent,
		Blocking: true,
	}, func(win fyne.Window) fyne.CanvasObject {
		imageCanvas := newMaskCanvas(nil, maskCanvasPreview)
		note := widget.NewLabel("")
		note.Wrapping = fyne.TextWrapWord

		labels := make([]string, len(classes))
		for i, c := range classes {
			labels[i] = c.label()
		}
		classSelect := widget.NewSelect(labels, nil)
		posLabel := widget.NewLabel("")
		prevBtn := widget.NewButton("< Previous", nil)
		nextBtn := widget.NewButton("Next >", nil)
		drawCheck := widget.NewCheck("Draw rectangles", nil)
		undoBtn := widget.NewButton("Undo", nil)
		exemptBtn := widget.NewButton("Needs no masking", nil)

		// How far a drawn rectangle reaches. The default is the image it was
		// drawn on and nothing else: an analysis screen's layout says nothing
		// about the next screen's, and blanking the same area across a group
		// destroys report content the export exists to keep. Widening is a
		// deliberate choice, never the assumption.
		scopeSelect := widget.NewSelect(
			[]string{maskScopeImageLabel, maskScopeGroupLabel, maskScopeAllLabel}, nil)
		scopeSelect.SetSelected(maskScopeImageLabel)

		// Current position: which class, and which file within it.
		classIdx, fileIdx := 0, 0
		// The dataset on display, for the SOP Instance UID an image-scoped
		// region needs.
		var current *sdicom.Dataset

		// scopeFor builds the scope a new region gets from the selector.
		scopeFor := func(cls maskGeometryClass) *MaskScope {
			switch scopeSelect.Selected {
			case maskScopeAllLabel:
				return nil
			case maskScopeGroupLabel:
				s := &MaskScope{Modality: cls.modality, Cols: cls.cols, Rows: cls.rows}
				// Match the group the window shows, which splits ultrasound on
				// whether the file states its own region.
				if strings.EqualFold(cls.modality, "US") {
					s.USRegion = usRegionAbsent
					if cls.calibrated {
						s.USRegion = usRegionDeclared
					}
				}
				return s
			default:
				uid := ""
				if current != nil {
					uid = strings.TrimSpace(datasetFirstString(current, tag.SOPInstanceUID))
				}
				if uid == "" {
					// No identity to pin it to; fall back to the group rather
					// than silently applying it everywhere.
					return &MaskScope{Modality: cls.modality, Cols: cls.cols, Rows: cls.rows}
				}
				return &MaskScope{SOPInstanceUID: uid}
			}
		}

		// show renders the current file with the regions currently in force.
		var show func()
		show = func() {
			if classIdx < 0 || classIdx >= len(classes) {
				return
			}
			cls := classes[classIdx]
			fileIdx = clampInt(fileIdx, 0, cls.count()-1)
			path := cls.files[fileIdx]

			posLabel.SetText(fmt.Sprintf("Image %d of %d — %s",
				fileIdx+1, cls.count(), filepath.Base(path)))
			prevBtn.Enable()
			nextBtn.Enable()
			if fileIdx == 0 {
				prevBtn.Disable()
			}
			if fileIdx >= cls.count()-1 {
				nextBtn.Disable()
			}

			img, ds, err := loadMaskFrame(path)
			if err != nil {
				current = nil
				imageCanvas.setRects(nil)
				note.SetText(fmt.Sprintf("%s could not be displayed: %v", filepath.Base(path), err))
				return
			}
			current = ds
			imageCanvas.setImage(img)
			undoBtn.Enable()
			if !work.canUndo() {
				undoBtn.Disable()
			}

			regions, override := work.governing(cls.modality)
			var parts []string
			if override != "" {
				parts = append(parts, "Using the "+override+" override's regions")
			}
			if len(regions) == 0 {
				imageCanvas.setRects(nil)
				parts = append(parts, "No masking applies to these images")
			} else {
				// Resolved by the engine's own code, so what is drawn here is
				// what the export blanks.
				// The file here is unmodified, so its own identity is the
				// source identity the export will resolve against.
				res, rerr := maskRects(newMaskSource(ds), regions, cls.cols, cls.rows)
				switch {
				case rerr != nil:
					imageCanvas.setRects(nil)
					parts = append(parts, "This image cannot be masked and would fail the export: "+rerr.Error())
				case len(res.rects) == 0:
					imageCanvas.setRects(nil)
					parts = append(parts, "Nothing is masked on this image")
				default:
					imageCanvas.setRects(pixelRectsToRegions(res.rects, cls.cols, cls.rows))
					if res.usFellBack {
						parts = append(parts, "No calibrated ultrasound region, so the profile's rectangles mask it")
					}
				}
			}
			if drawCheck.Checked {
				parts = append(parts, "Drag on the image to mask an area of "+
					strings.ToLower(scopeSelect.Selected))
			}
			note.SetText(strings.Join(parts, ". ") + ".")
		}

		classSelect.OnChanged = func(sel string) {
			for i, l := range labels {
				if l == sel {
					classIdx, fileIdx = i, 0
					show()
					return
				}
			}
		}
		prevBtn.OnTapped = func() { fileIdx--; show() }
		nextBtn.OnTapped = func() { fileIdx++; show() }
		undoBtn.OnTapped = func() {
			work.undo()
			show()
		}
		scopeSelect.OnChanged = func(string) { show() }
		drawCheck.OnChanged = func(on bool) {
			if on {
				imageCanvas.setMode(maskCanvasEdit)
			} else {
				imageCanvas.setMode(maskCanvasPreview)
			}
			show()
		}
		// A drawn rectangle joins the run's regions rather than the canvas's
		// own list, so the next redraw shows it resolved alongside everything
		// else — and carries the scope chosen above, which decides whether the
		// next image sees it at all.
		imageCanvas.onDraw = func(r MaskRegion) {
			cls := classes[classIdx]
			r.AppliesTo = scopeFor(cls)
			work.add(cls.modality, r)
			show()
		}
		// Marking an image as needing no masking is a statement, not a
		// no-op: without it an ultrasound image that declares no calibrated
		// region fails the export, which is right until somebody has looked
		// at it and decided otherwise.
		exemptBtn.OnTapped = func() {
			cls := classes[classIdx]
			work.add(cls.modality, MaskRegion{Mode: maskModeNone, AppliesTo: scopeFor(cls)})
			show()
		}

		classSelect.SetSelected(labels[0])

		caption := widget.NewLabel("Every selected image, grouped by modality, size, and — for ultrasound — whether " +
			"the file states its own image region. Step through them and check what will be blanked. Tick Draw " +
			"rectangles to mark an area on the image in front of you; Applies to decides how far that rectangle " +
			"reaches, and defaults to the one image, since analysis screens are laid out differently from one " +
			"another. Needs no masking records that an image was reviewed and requires none, which is what lets an " +
			"ultrasound image with no stated region be exported rather than failed.")
		caption.TextStyle = fyne.TextStyle{Italic: true}
		caption.Wrapping = fyne.TextWrapWord

		cancelBtn := widget.NewButton("Cancel", func() { win.Close() })
		applyBtn := widget.NewButton("Use these regions", func() {
			win.Close()
			onApply(work.applyTo(resolved))
		})
		applyBtn.Importance = widget.HighImportance

		nav := container.NewHBox(prevBtn, nextBtn, posLabel, layout.NewSpacer(),
			drawCheck, widget.NewLabel("Applies to"), scopeSelect, exemptBtn, undoBtn)
		head := container.NewVBox(caption, classSelect, widget.NewSeparator())
		foot := container.NewBorder(widget.NewSeparator(), nil, nil, nil, container.NewPadded(
			container.NewVBox(nav, note,
				container.NewHBox(layout.NewSpacer(), cancelBtn, applyBtn))))
		return container.NewBorder(head, foot, nil, nil, imageCanvas)
	})
}
