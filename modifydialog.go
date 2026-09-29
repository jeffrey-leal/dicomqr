package main

// Modification confirmation dialog — shown when a profile is chosen from the
// Local Browse "Modification" context submenu. Presents the effective profile
// (after base-chain resolution) for confirmation: editable set values and
// options, and a read-only list of the tags marked for removal. On confirm the
// user picks an output folder and the files are processed in the background
// with progress; the download folder and its catalog are never touched.

import (
	"context"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// fixvrOffLabel is the Select option representing "no fixvr handling".
const fixvrOffLabel = "(off)"

// validateExportFolderName checks that name is usable as a single Windows
// folder component. The name replaces the export root — the original patient
// folder, and for a Study-level selection its study folder too — so it is
// always typed by the user rather than derived from the (PHI-bearing) source
// folder; everything below it keeps its source name (see exportLayout).
func validateExportFolderName(name string) error {
	if name == "" {
		return fmt.Errorf("enter an export folder name — the original patient folder name is not reused because it often contains PHI")
	}
	if strings.ContainsAny(name, `\/:*?"<>|`) {
		return fmt.Errorf(`the export folder name cannot contain any of \ / : * ? " < > |`)
	}
	if strings.HasSuffix(name, ".") {
		return fmt.Errorf("the export folder name cannot end with a dot")
	}
	if windowsReserved[strings.ToUpper(strings.SplitN(name, ".", 2)[0])] {
		return fmt.Errorf("%q is a reserved Windows device name", name)
	}
	return nil
}

// pathWithinDir reports whether dir equals or lies inside root, comparing
// case-insensitively (Windows filesystems).
func pathWithinDir(dir, root string) bool {
	if root == "" {
		return false
	}
	d := strings.ToLower(filepath.Clean(dir))
	r := strings.ToLower(filepath.Clean(root))
	rel, err := filepath.Rel(r, d)
	if err != nil {
		return false
	}
	return rel == "." ||
		(rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// showModificationDialog opens the profile confirmation dialog for applying
// profileName to files — the local files of one Patient or Study node, or
// Local Browse's selection within one patient (see modificationScope).
// rootDir is the download-folder root the files live under; studyLevel is
// true when every file lies within one study, false when they span a
// patient's studies — it drives the PHI-safe output layout (see
// exportLayout).
func showModificationDialog(w fyne.Window, cfg *Settings, profileName, nodeLabel string, files []string, rootDir string, studyLevel bool) {
	if len(files) == 0 {
		dialog.ShowInformation("Modification",
			"No local files found for this item.", w)
		return
	}

	profCfg := ModProfileConfig{}
	if profPath, err := modifyProfilesPath(); err == nil {
		var lerr error
		if profCfg, lerr = loadModProfileConfig(profPath); lerr != nil {
			dialog.ShowError(fmt.Errorf("loading profiles.json: %w", lerr), w)
			return
		}
	}
	resolved, err := resolveModProfile(profileName, profCfg)
	if err != nil {
		dialog.ShowError(err, w)
		return
	}
	buildModificationDialog(w, cfg, profileName, nodeLabel, files, rootDir, studyLevel, resolved)
}

// buildModificationDialog constructs and shows the confirmation dialog for an
// already-resolved profile.
func buildModificationDialog(w fyne.Window, cfg *Settings, profileName, nodeLabel string,
	files []string, rootDir string, studyLevel bool, resolved ModProfile) {

	// win is this panel's own window, assigned as it opens at the foot of this
	// function. Every callback below parents its children to win rather than to
	// the window that opened it: a child parented to the latter would surface
	// behind the blocked Modification window with no way to reach it. All such
	// uses sit inside callbacks, so the late assignment is in place long before
	// any of them can run.
	var win fyne.Window

	header := widget.NewLabel(fmt.Sprintf("Profile %q — %d file(s) from %s",
		profileName, len(files), nodeLabel))
	header.Wrapping = fyne.TextWrapWord

	// tagLabel renders a tag reference as "Name (GGGG,EEEE)" where resolvable.
	tagLabel := func(ref string) string {
		ref = strings.TrimSpace(ref)
		t, err := parseTagString(ref)
		if err != nil {
			return ref
		}
		if name := tagDisplayName(t); name != "" {
			return fmt.Sprintf("%s (%04X,%04X)", name, t.Group, t.Element)
		}
		return fmt.Sprintf("%04X,%04X", t.Group, t.Element)
	}

	// Set values — one editable row per effective "TAG=VALUE" entry. A value
	// that is a "[GGGG,EEEE]" reference shows what it resolves to rather than
	// the syntax, and keeps following its target as that is typed (wired below).
	type setRow struct {
		tagStr string
		t      tag.Tag
		parsed bool
		entry  *widget.Entry
	}
	var setRows []setRow
	entryByTag := map[tag.Tag]*widget.Entry{}
	resolvedSets, refErr := resolveSetReferences(resolved.Sets)
	if refErr != nil {
		dialog.ShowError(fmt.Errorf("profile %q: %w", profileName, refErr), w)
		return
	}
	setForm := widget.NewForm()
	for i, s := range resolvedSets {
		tagStr, value, ok := strings.Cut(s, "=")
		if !ok || strings.TrimSpace(tagStr) == "" {
			logWarn("modify: ignoring malformed set entry %q in profile %q", s, profileName)
			continue
		}
		entry := widget.NewEntry()
		entry.SetText(value)
		// Label from the profile's own entry, so an unparsable tag still reads
		// as the user wrote it; resolvedSets carries the canonical form.
		setForm.Append(tagLabel(strings.SplitN(resolved.Sets[i], "=", 2)[0]), entry)
		row := setRow{tagStr: strings.TrimSpace(tagStr), entry: entry}
		if t, terr := parseTagString(row.tagStr); terr == nil {
			row.t, row.parsed = t, true
			if _, dup := entryByTag[t]; !dup {
				entryByTag[t] = entry
			}
		}
		setRows = append(setRows, row)
	}

	// Options — effective profile values; unset fields show the application
	// defaults (empty masks, no date shift, fixvr off, booleans false).
	remapCheck := widget.NewCheck("", nil)
	remapCheck.SetChecked(resolved.RemapUIDs)
	privCheck := widget.NewCheck("", nil)
	privCheck.SetChecked(resolved.Priv)
	overlaysCheck := widget.NewCheck("", nil)
	overlaysCheck.SetChecked(resolved.NoOverlays)
	dobEntry := widget.NewEntry()
	dobEntry.SetText(resolved.DOB)
	dobEntry.SetPlaceHolder("YYYYMMDD — empty = no masking")
	shiftEntry := widget.NewEntry()
	shiftEntry.SetText(resolved.ShiftDays)
	shiftEntry.SetPlaceHolder("e.g. -45 — shifts all dates except birth date")
	fixvrSelect := widget.NewSelect([]string{fixvrOffLabel, "correct", "skip", "passthrough"}, nil)
	if m := strings.ToLower(strings.TrimSpace(resolved.FixVR)); m != "" {
		fixvrSelect.SetSelected(m)
	} else {
		fixvrSelect.SetSelected(fixvrOffLabel)
	}
	tsSelect := widget.NewSelect(modProfileTSLabels, nil)
	tsSelect.SetSelected(transferSyntaxPrefLabel(resolved.TransferSyntax))

	// Row order is the profile editor's, deliberately — the two Options blocks
	// show the same fields and are read against each other, so they must not be
	// ordered differently. Three fields the editor has here and this dialog
	// does not are Zip export, Flat export and Include DICOMDIR: all three
	// live in Export below, where the user is naming the output and can see
	// what the checkboxes change, rather than among the per-tag transforms.
	optionsForm := widget.NewForm(
		widget.NewFormItem("Birth date mask", dobEntry),
		widget.NewFormItem("Remap UIDs", remapCheck),
		widget.NewFormItem("Remove private tags", privCheck),
		widget.NewFormItem("Remove overlay planes", overlaysCheck),
		widget.NewFormItem("Shift dates (days)", shiftEntry),
		widget.NewFormItem("Fix VR", fixvrSelect),
		widget.NewFormItem("Output transfer syntax", tsSelect),
	)

	// Birth-date advisory, under the row it is about. resolved already carries
	// the merged base chain, so only the mask itself can change the answer here
	// — the removal list on this panel is read-only — which is why a mask typed
	// for a single run raises the note too.
	dobAdvisoryLabel := widget.NewLabel("")
	dobAdvisoryLabel.TextStyle = fyne.TextStyle{Italic: true}
	dobAdvisoryLabel.Wrapping = fyne.TextWrapWord
	refreshDOBAdvisory := func() {
		candidate := resolved
		candidate.DOB = strings.TrimSpace(dobEntry.Text)
		if text := modProfileDOBAdvisory(candidate); text != "" {
			dobAdvisoryLabel.SetText(text)
			dobAdvisoryLabel.Show()
			return
		}
		dobAdvisoryLabel.Hide()
	}
	refreshDOBAdvisory()
	dobEntry.OnChanged = func(string) { refreshDOBAdvisory() }

	// Tags removed — read-only list (already keep-filtered by base resolution).
	removeLines := make([]string, 0, len(resolved.Removes))
	for _, r := range resolved.Removes {
		line := strings.TrimSpace(r)
		if t, err := parseTagString(line); err == nil {
			if name := tagDisplayName(t); name != "" {
				line = fmt.Sprintf("%04X,%04X — %s", t.Group, t.Element, name)
			} else {
				line = fmt.Sprintf("%04X,%04X", t.Group, t.Element)
			}
		}
		removeLines = append(removeLines, line)
	}
	removeList := widget.NewList(
		func() int { return len(removeLines) },
		func() fyne.CanvasObject { return widget.NewLabel("0000,0000 — placeholder") },
		func(i widget.ListItemID, o fyne.CanvasObject) { o.(*widget.Label).SetText(removeLines[i]) },
	)
	listHeight := canvas.NewRectangle(color.Transparent)
	listHeight.SetMinSize(fyne.NewSize(0, 180))
	removeBox := container.NewStack(listHeight, removeList)

	// Sections are built with prefSection, the same header/separator block the
	// profile editor and Preferences use, so the two Options blocks are framed
	// identically as well as ordered identically.
	sections := []fyne.CanvasObject{header}
	if len(setRows) > 0 {
		sections = append(sections, prefSection("Set values", setForm))
	}
	sections = append(sections, prefSection("Options", optionsForm, dobAdvisoryLabel))

	// Pixel masking. The regions are disclosed because this is the one part of a
	// profile that alters the image rather than the header and cannot be undone
	// in the export — a run that silently blanked pixels would be
	// indistinguishable from one that silently failed to. They are also
	// adjustable, in the review window rather than by typing: geometry is
	// something to judge against the images it will be applied to.
	//
	// runMasks holds what this run will use. It starts as the profile's and is
	// replaced when the review window applies — for this run only, like every
	// other control in this dialog.
	runMasks := resolved
	maskLines := func() string {
		regions, _ := (&maskWorkingSet{profile: runMasks.MaskRegions}).governing("")
		if len(regions) == 0 {
			return "No areas are masked."
		}
		lines := make([]string, 0, len(regions))
		for _, r := range regions {
			lines = append(lines, "• "+maskRegionSummary(r))
		}
		return strings.Join(lines, "\n")
	}
	maskList := widget.NewLabel(maskLines())
	maskNote := widget.NewLabel("These areas of every exported image are blanked permanently. " +
		"Compressed files are decompressed on the way out so their pixels can be written, " +
		"whatever the output transfer syntax says.")
	maskNote.TextStyle = fyne.TextStyle{Italic: true}
	maskNote.Wrapping = fyne.TextWrapWord
	// Review is where the geometry meets the images it will be applied to:
	// fractional rectangles fail on a study that mixes image sizes, and nothing
	// in the profile says so. It is offered even when the profile masks nothing,
	// because that is exactly when a study's analysis screens go out unmasked.
	reviewBtn := widget.NewButton("Review masking…", func() {
		showMaskPreview(fyne.CurrentApp(), win, profileName, files, runMasks, func(updated ModProfile) {
			runMasks = updated
			maskList.SetText(maskLines())
		})
	})
	sections = append(sections, prefSection("Pixel masking", maskList, maskNote,
		container.NewHBox(reviewBtn)))

	sections = append(sections,
		prefSection(fmt.Sprintf("Tags removed (%d)", len(removeLines)), removeBox),
	)

	// Settings applied as-is without dialog controls.
	var notes []string
	if len(resolved.IgnoreTypes) > 0 {
		notes = append(notes, "skip Image Type "+strings.Join(resolved.IgnoreTypes, ", "))
	}
	if len(resolved.IgnoreModalities) > 0 {
		notes = append(notes, "skip Modality "+strings.Join(resolved.IgnoreModalities, ", "))
	}
	if len(resolved.IgnoreSOPClasses) > 0 {
		notes = append(notes, "skip SOP Class "+sopClassListSummary(resolved.IgnoreSOPClasses))
	}
	// An override's SOP class filter adds to the profile's for its modality;
	// named per modality, since each can list different classes.
	if len(resolved.PerModality) > 0 {
		codes := make([]string, 0, len(resolved.PerModality))
		for k := range resolved.PerModality {
			codes = append(codes, k)
		}
		sort.Strings(codes)
		for _, k := range codes {
			if list := resolved.PerModality[k].IgnoreSOPClasses; len(list) > 0 {
				notes = append(notes, k+": skip SOP Class "+sopClassListSummary(list))
			}
		}
	}
	if len(runMasks.PerModality) > 0 {
		mods := make([]string, 0, len(runMasks.PerModality))
		maskMods := make([]string, 0, len(runMasks.PerModality))
		for k, ov := range runMasks.PerModality {
			mods = append(mods, k)
			if len(ov.MaskRegions) > 0 {
				maskMods = append(maskMods, k)
			}
		}
		sort.Strings(mods)
		notes = append(notes, "per-modality overrides: "+strings.Join(mods, ", "))
		// Named separately: an override's regions replace the profile's, so
		// the section above is not what those files get.
		if len(maskMods) > 0 {
			sort.Strings(maskMods)
			notes = append(notes, "own mask regions for "+strings.Join(maskMods, ", "))
		}
	}
	if len(notes) > 0 {
		noteLbl := widget.NewLabel("Also applied: " + strings.Join(notes, "; "))
		noteLbl.TextStyle = fyne.TextStyle{Italic: true}
		noteLbl.Wrapping = fyne.TextWrapWord
		sections = append(sections, noteLbl)
	}

	// Export destination. The output folder is the persisted default
	// (cfg.ModifyOutputDir) so no picker appears on routine runs; Change…
	// overrides it for this run only. The export folder name is always typed
	// by the user and replaces the original patient/study folder names in the
	// output, which often contain PHI. Its default is the profile's exportname
	// — usually a reference to the new patient name — falling back to profile
	// plus timestamp when that is unset or resolves to nothing.
	exportNameEntry := widget.NewEntry()
	defaultExportName := sanitize(profileName) + "-" + time.Now().Format("20060102-150405")
	exportNameFrom := func(v string) string {
		if name := strings.TrimRight(sanitize(strings.TrimSpace(v)), ". "); name != "" {
			return name
		}
		return defaultExportName
	}
	exportRefTag, exportIsRef := setValueReference(resolved.ExportName)
	switch {
	case exportIsRef:
		if e, ok := entryByTag[exportRefTag]; ok {
			exportNameEntry.SetText(exportNameFrom(e.Text))
		} else {
			exportNameEntry.SetText(defaultExportName)
		}
	case strings.TrimSpace(resolved.ExportName) != "":
		exportNameEntry.SetText(exportNameFrom(resolved.ExportName))
	default:
		exportNameEntry.SetText(defaultExportName)
	}

	// Live propagation. A field that declares itself a copy of another follows
	// that other field as it is typed — the relationship comes from the profile
	// rather than from hardcoded knowledge of which tags are related, which is
	// what made the old Patient Name → Patient ID link invisible when it failed.
	//
	// A target stops being followed once its own field is edited directly: each
	// follower remembers the last value written into it, and a field holding
	// anything else is the user's, not ours.
	lastAuto := map[*widget.Entry]string{}
	for _, e := range entryByTag {
		lastAuto[e] = e.Text
	}
	lastAutoExport := exportNameEntry.Text
	targets := setValueFollowers(resolved.Sets)
	if exportIsRef {
		// The export name may reference a tag no Set value does, and it still
		// has to follow it — so make sure that tag is wired even with no
		// followers of its own.
		if _, ok := targets[exportRefTag]; !ok {
			if targets == nil {
				targets = map[tag.Tag][]tag.Tag{}
			}
			targets[exportRefTag] = nil
		}
	}
	for target, referrers := range targets {
		source, ok := entryByTag[target]
		if !ok {
			continue
		}
		followers := make([]*widget.Entry, 0, len(referrers))
		for _, r := range referrers {
			if e, ok := entryByTag[r]; ok {
				followers = append(followers, e)
			}
		}
		alsoExport := exportIsRef && exportRefTag == target
		if len(followers) == 0 && !alsoExport {
			continue
		}
		source.OnChanged = func(v string) {
			v = strings.TrimSpace(v)
			for _, e := range followers {
				if e.Text == lastAuto[e] {
					lastAuto[e] = v
					e.SetText(v)
				}
			}
			if alsoExport && exportNameEntry.Text == lastAutoExport {
				name := exportNameFrom(v)
				lastAutoExport = name
				exportNameEntry.SetText(name)
			}
		}
	}

	chosenOutDir := cfg.ModifyOutputDir
	outDirLabel := widget.NewLabel("")
	outDirLabel.Truncation = fyne.TextTruncateEllipsis
	setOutDirLabel := func() {
		if chosenOutDir == "" {
			outDirLabel.SetText("(not set — you will be asked on Modify…)")
		} else {
			outDirLabel.SetText(chosenOutDir)
		}
	}
	setOutDirLabel()

	// validOutDir rejects folders inside the download folder with the standard
	// explanation; modified files are never mixed into the local index.
	validOutDir := func(dir string) bool {
		if pathWithinDir(dir, cfg.DownloadDir) {
			dialog.ShowError(fmt.Errorf(
				"the output folder must be outside the download folder (%s) — modified files are never mixed into the local index", cfg.DownloadDir), win)
			return false
		}
		return true
	}
	// rememberFirstOutDir persists dir as the default output folder when none
	// is configured yet, so later runs skip the picker entirely. A change made
	// while a default exists applies to this run only — the configured default
	// (Preferences > Modification & Export) is never overwritten here.
	rememberFirstOutDir := func(dir string) {
		if cfg.ModifyOutputDir != "" {
			return
		}
		cfg.ModifyOutputDir = dir
		if err := saveSettingsE(*cfg); err != nil {
			dialog.ShowError(fmt.Errorf("saving the default output folder: %w", err), win)
		}
	}
	changeOutDirBtn := widget.NewButton("Change…", func() {
		start := chosenOutDir
		go func() {
			dir, ok := browseFolder(win.Title(), "Choose output folder for modified files", start)
			if !ok {
				return // picker cancelled
			}
			fyne.Do(func() {
				if !validOutDir(dir) {
					return
				}
				rememberFirstOutDir(dir)
				chosenOutDir = dir
				setOutDirLabel()
			})
		}()
	})
	zipCheck := widget.NewCheck("Write a single <export folder name>.zip instead of a folder", nil)
	// The profile's zip field pre-checks the box; the checkbox remains the
	// per-run override and is never written back to the profile.
	zipCheck.SetChecked(resolved.Zip)
	flatCheck := widget.NewCheck("Write every file into the export root, with no patient/study/series folders", nil)
	flatCheck.SetChecked(resolved.Flat)
	dicomdirCheck := widget.NewCheck("Add a DICOMDIR index", nil)
	dicomdirCheck.SetChecked(resolved.Dicomdir)
	exportForm := widget.NewForm(
		widget.NewFormItem("Export folder name", exportNameEntry),
		widget.NewFormItem("Output folder", container.NewBorder(nil, nil, nil, changeOutDirBtn, outDirLabel)),
		widget.NewFormItem("Zip export", zipCheck),
		widget.NewFormItem("Flat export", flatCheck),
		widget.NewFormItem("Include DICOMDIR", dicomdirCheck),
	)
	exportReplaces := "the original patient folder name"
	if studyLevel {
		exportReplaces = "the original patient and study folder names"
	}
	exportNote := widget.NewLabel("")
	exportNote.TextStyle = fyne.TextStyle{Italic: true}
	exportNote.Wrapping = fyne.TextWrapWord
	// Flat export replaces the paragraph about folders below the export root
	// keeping their source names with one describing the flat layout instead
	// — a static note would otherwise contradict whichever way the box is
	// checked, the same reasoning behind refreshDOBAdvisory below.
	refreshExportNote := func() {
		text := "Files are written under <output folder>\\<export folder name> — " +
			"or, with Zip export, into a compressed <output folder>\\<export folder name>.zip. " +
			"The export folder name replaces " + exportReplaces + ", which often contain PHI. "
		if flatCheck.Checked {
			text += "Flat export is checked: every file is written directly into the export root (or the archive root), " +
				"named after its SOP Instance UID rather than kept under its source name, with no patient/study/series folders. "
		} else {
			text += "Folders below it keep their original names, except where this profile deletes or replaces the value a name is built from. "
		}
		text += "Include DICOMDIR adds a PS3.10 File-set index listing every exported file, letting a DICOM viewer or a CD/DVD-burning " +
			"workflow browse the export without a database — written as a DICOMDIR entry inside the archive when Zip export is also checked."
		exportNote.SetText(text)
	}
	refreshExportNote()
	flatCheck.OnChanged = func(bool) { refreshExportNote() }
	sections = append(sections, prefSection("Export", exportForm, exportNote))

	cancelBtn := widget.NewButton("Cancel", func() { win.Close() })
	modifyBtn := widget.NewButton("Modify…", nil)
	modifyBtn.Importance = widget.HighImportance

	minWidth := canvas.NewRectangle(color.Transparent)
	minWidth.SetMinSize(fyne.NewSize(640, 0))

	modifyBtn.OnTapped = func() {
		// Validate the editable fields; compileModifyParams re-checks, but
		// friendly messages belong here where the user can correct them.
		exportName := strings.TrimSpace(exportNameEntry.Text)
		if err := validateExportFolderName(exportName); err != nil {
			dialog.ShowError(err, win)
			return
		}
		dob, err := validateDOBMask(dobEntry.Text)
		if err != nil {
			dialog.ShowError(err, win)
			return
		}
		shift, err := validateShiftDays(shiftEntry.Text)
		if err != nil {
			dialog.ShowError(err, win)
			return
		}

		// The rows hold resolved literals, so each value can be checked against
		// its tag's value representation here — the last place a per-run edit
		// can put a word into a date field before it reaches every output file.
		// runMasks carries any regions added in the review window; everything
		// else still comes from the resolved profile plus the controls above.
		edited := runMasks
		edited.Sets = make([]string, 0, len(setRows))
		for _, row := range setRows {
			if row.parsed {
				if verr := validateSetValue(row.t, row.entry.Text); verr != nil {
					dialog.ShowError(fmt.Errorf("Set values: %w", verr), win)
					return
				}
			}
			edited.Sets = append(edited.Sets, row.tagStr+"="+row.entry.Text)
		}
		edited.DOB = dob
		edited.ShiftDays = shift
		edited.RemapUIDs = remapCheck.Checked
		edited.Priv = privCheck.Checked
		edited.NoOverlays = overlaysCheck.Checked
		if sel := fixvrSelect.Selected; sel == "" || sel == fixvrOffLabel {
			edited.FixVR = ""
		} else {
			edited.FixVR = sel
		}
		edited.TransferSyntax = transferSyntaxPrefFromLabel(tsSelect.Selected)
		// Unlike Zip and Flat (pure destination-shape choices read directly
		// into outLayout below, never part of modifyParams), DICOMDIR is an
		// engine-level option compileModifyParams reads — see modifyengine.go.
		edited.Dicomdir = dicomdirCheck.Checked

		params, err := compileModifyParams(edited)
		if err != nil {
			dialog.ShowError(err, win)
			return
		}

		// startRun launches the modification into outBase\exportName — a folder
		// tree, or a single .zip archive when Zip export is checked — using the
		// PHI-safe layout (runs on the UI goroutine). The export root stands in
		// for the patient folder alone, or patient+study for a study-level
		// selection — see exportLayout.
		dropDirs := 1
		if studyLevel {
			dropDirs = 2
		}
		outLayout := &exportLayout{dropDirs: dropDirs, flat: flatCheck.Checked, names: &flatNames{}}
		startRun := func(outBase string) {
			if !validOutDir(outBase) {
				return
			}
			if zipCheck.Checked {
				zipName := exportName
				if !strings.EqualFold(filepath.Ext(zipName), ".zip") {
					zipName += ".zip"
				}
				zipPath := filepath.Join(outBase, zipName)
				begin := func() {
					win.Close()
					showModificationRunDialog(w, profileName, files, rootDir, zipPath, params, outLayout, true)
				}
				if _, serr := os.Stat(zipPath); serr == nil {
					dialog.ShowConfirm("Zip file exists",
						fmt.Sprintf("%s\nalready exists and will be replaced. Continue?", zipPath),
						func(ok bool) {
							if ok {
								begin()
							}
						}, win)
					return
				}
				begin()
				return
			}
			exportRoot := filepath.Join(outBase, exportName)
			begin := func() {
				win.Close()
				showModificationRunDialog(w, profileName, files, rootDir, exportRoot, params, outLayout, false)
			}
			if entries, rerr := os.ReadDir(exportRoot); rerr == nil && len(entries) > 0 {
				dialog.ShowConfirm("Export folder exists",
					fmt.Sprintf("%s\nalready exists and is not empty. Files with matching names will be overwritten. Continue?", exportRoot),
					func(ok bool) {
						if ok {
							begin()
						}
					}, win)
				return
			}
			begin()
		}

		if chosenOutDir != "" {
			startRun(chosenOutDir)
			return
		}
		// No output folder configured yet — ask once; the choice is persisted
		// as the default so later runs skip the picker.
		go func() {
			outDir, ok := browseFolder(win.Title(), "Choose output folder for modified files", cfg.ModifyOutputDir)
			if !ok {
				return // picker cancelled
			}
			fyne.Do(func() {
				if !validOutDir(outDir) {
					return
				}
				rememberFirstOutDir(outDir)
				chosenOutDir = outDir
				setOutDirLabel()
				startRun(outDir)
			})
		}()
	}

	// A window rather than a dialog, and the reason is the button row: a Fyne
	// dialog is a canvas overlay sized to its content, so a profile with many
	// set values and a long removal list grew this panel past the bottom of the
	// screen, putting Modify… and Cancel where they could not be clicked. Here
	// the body scrolls and the buttons are pinned below it, so the panel fits
	// any screen and the window can still be moved and resized.
	openOwnedWindow(fyne.CurrentApp(), windowSpec{
		Title:    "Modification — " + profileName,
		Size:     fyne.NewSize(700, 700),
		Parent:   w,
		Blocking: true,
	}, func(owned fyne.Window) fyne.CanvasObject {
		win = owned
		// Right inset is the scroll gutter when that is wider: the Tags removed
		// list scrolls on its own, and its scrollbar must stay out of the body
		// scrollbar's grab zone (scrollgutter.go).
		body := container.NewVScroll(container.New(
			layout.NewCustomPaddedLayout(modEditorMargin, modEditorMargin, modEditorMargin,
				max(modEditorMargin, scrollGutterWidth())),
			container.NewVBox(sections...)))
		// A modest floor, as in the profile editor: it stops the window being
		// shrunk to nothing without forbidding the vertical resizing that makes
		// this a window in the first place.
		body.SetMinSize(fyne.NewSize(0, 240))
		buttonRow := container.NewBorder(
			widget.NewSeparator(), nil, nil, nil,
			container.New(
				layout.NewCustomPaddedLayout(modEditorMargin, modEditorMargin, modEditorMargin, modEditorMargin),
				container.NewHBox(layout.NewSpacer(), cancelBtn, modifyBtn)),
		)
		return container.NewStack(minWidth,
			container.NewBorder(nil, buttonRow, nil, nil, body))
	})
}

// showModificationRunDialog runs the modification in the background with a
// progress bar and cancel support, then shows the summary. outLayout maps
// each file to its PHI-safe path under outDir (see exportLayout). With asZip,
// outDir is the path of the single .zip archive the run writes into instead
// of a folder tree.
func showModificationRunDialog(w fyne.Window, profileName string, files []string,
	rootDir, outDir string, params modifyParams, outLayout *exportLayout, asZip bool) {

	total := len(files)
	progressBar := widget.NewProgressBar()
	statusLbl := widget.NewLabel(fmt.Sprintf("Modifying %d file(s)…", total))
	statusLbl.Wrapping = fyne.TextWrapWord
	cancelBtn := widget.NewButton("Cancel", nil)

	content := container.NewVBox(
		progressBar,
		statusLbl,
		widget.NewSeparator(),
		container.NewHBox(layout.NewSpacer(), cancelBtn),
	)
	dlg := dialog.NewCustomWithoutButtons("Modification — "+profileName, container.NewPadded(content), w)
	dlg.Resize(fyne.NewSize(460, 0))

	ctx, cancel := context.WithCancel(context.Background())
	cancelBtn.OnTapped = func() {
		cancel()
		cancelBtn.Disable()
		statusLbl.SetText("Cancelling — waiting for files in flight…")
	}

	go func() {
		logInfo("modify: profile %q, %d file(s) → %s", profileName, total, outDir)
		// The workers only record how far they have got; one reporter posts
		// it at a fixed rate (see startPacedProgress). A per-10-files trigger
		// fired at whatever rate files finished, and once a tag-only run
		// stopped decoding pixel data (rawPixelPassthrough) that became a burst
		// of updates the UI goroutine had to drain one by one.
		// Workers report concurrently and can arrive out of order, so keep
		// the highest count rather than the latest.
		var doneFiles atomic.Int64
		onProgress := func(done, _ int) {
			for {
				cur := doneFiles.Load()
				if int64(done) <= cur || doneFiles.CompareAndSwap(cur, int64(done)) {
					return
				}
			}
		}
		shown := int64(-1)
		publish := func() {
			done := doneFiles.Load()
			if done == shown {
				return
			}
			shown = done
			fyne.Do(func() {
				progressBar.SetValue(float64(done) / float64(total))
				if ctx.Err() == nil {
					statusLbl.SetText(fmt.Sprintf("Processing %d / %d…", done, total))
				}
			})
		}
		stopProgress := startPacedProgress(scanProgressInterval, publish)
		var res modifyResult
		if asZip {
			res = runModificationToZip(ctx, files, rootDir, outDir, params, outLayout, onProgress)
		} else {
			res = runModification(ctx, files, rootDir, outDir, params, outLayout, onProgress)
		}
		stopProgress()
		logInfo("modify: %q finished — %d written, %d skipped, %d failed, %d recompressed after masking, %d lossy re-encoded lossless, %d decompressed for masking, cancelled=%v → %s",
			profileName, res.Processed, res.Skipped, res.Failed, res.MaskRecompressed, res.MaskRecodedLossless, res.MaskDecompressed, res.Canceled, outDir)
		fyne.Do(func() {
			progressBar.SetValue(1)
			head := "Done"
			if res.Canceled {
				head = "Cancelled"
			}
			msg := fmt.Sprintf("%s — %d file(s) written to %s", head, res.Processed, outDir)
			if res.Skipped > 0 {
				msg += fmt.Sprintf(", %d skipped", res.Skipped)
			}
			if res.Failed > 0 {
				msg += fmt.Sprintf(", %d failed", res.Failed)
			}
			// Masked files whose source compression is lossless are re-encoded,
			// verified bit-identical, back into their own syntax — the pixels
			// changed (that was the point), the encoding did not. Stated so the
			// rewrite is on record.
			if res.MaskRecompressed > 0 {
				msg += fmt.Sprintf("; %d masked file(s) recompressed to their original transfer syntax",
					res.MaskRecompressed)
			}
			// Masked files whose source compression is lossy are re-encoded to
			// JPEG 2000 Lossless instead — re-entering the lossy syntax would
			// degrade every pixel a second time, while the lossless encode
			// adds nothing beyond the decode masking already forced. A syntax
			// change, so it gets its own clause rather than hiding in the one
			// above.
			if res.MaskRecodedLossless > 0 {
				msg += fmt.Sprintf("; %d masked lossy file(s) re-encoded to %s — no added loss",
					res.MaskRecodedLossless, transferSyntaxLabel(tsJPEG2000LL))
			}
			// Masking cannot be applied to compressed pixels, and a lossy source
			// cannot be recompressed without degrading every pixel a second
			// time, so those files left in a different encoding than the profile
			// asked for. Stating it is the whole reason the run does it rather
			// than failing the file.
			if res.MaskDecompressed > 0 {
				msg += fmt.Sprintf("; %d decompressed to %s so burned-in pixels could be masked",
					res.MaskDecompressed, transferSyntaxLabel(tsExplicitVRLE))
			}
			// A weaker guarantee than the rest of the export: those files were
			// masked by the profile's rectangles because they stated no region
			// of their own, so they are worth checking by eye.
			if res.MaskUSFallback > 0 {
				msg += fmt.Sprintf("; %d ultrasound file(s) declared no image region and were masked "+
					"with the profile's rectangles instead — worth reviewing", res.MaskUSFallback)
			}
			// The birth date mask rewrites the top-level element only, so a copy
			// nested in a sequence — Original Attributes Sequence being the one
			// that really carries one — survives it. Reported from what the run
			// actually found rather than from the profile's shape, so it is a
			// fact about this export rather than a warning about a possibility.
			if res.NestedDOBKept > 0 {
				msg += fmt.Sprintf("; %d file(s) still carry a birth date inside a sequence — "+
					"the birth date mask reaches the top-level field only, so removing 0400,0561 "+
					"(Original Attributes Sequence) is what clears it", res.NestedDOBKept)
			}
			// Informational, like the mask-outcome clauses above: the exported
			// DICOM files are unaffected either way, so a DICOMDIR failure never
			// routes through the hard-failure dialog below.
			if res.DicomdirWritten {
				msg += "; DICOMDIR index written"
			}
			if res.DicomdirError != "" {
				msg += fmt.Sprintf("; DICOMDIR index could not be written (%s)", res.DicomdirError)
			}
			// Failures have to be impossible to walk past: an export missing
			// files still looks finished, and a transfer-syntax conversion that
			// could not run is exactly the case where the user must know the
			// export is incomplete. Replace the progress dialog outright rather
			// than leaving the count in a status line nobody rereads.
			if res.Failed > 0 {
				dlg.Hide()
				showModificationFailureDialog(w, profileName, msg, res)
				return
			}
			statusLbl.SetText(msg)
			cancelBtn.SetText("Close")
			cancelBtn.OnTapped = func() { dlg.Hide() }
			cancelBtn.Enable()
		})
	}()

	dlg.Show()
}

// modifyFailureListCap bounds how many failures the warning dialog spells out.
// Beyond it the list stops being readable and the Activity Log — which holds
// every one of them — is the better place to look.
const modifyFailureListCap = 20

// showModificationFailureDialog reports a run that finished with per-file
// failures. It replaces the progress dialog, so the run cannot be dismissed
// without the failures having been on screen.
//
// Each line names the file and the reason the engine recorded, which for a
// transfer-syntax conversion is the syntax that has no built-in decoder — the
// one piece of information that tells the user whether the export can be
// repeated successfully at all.
func showModificationFailureDialog(w fyne.Window, profileName, summary string, res modifyResult) {
	head := widget.NewLabel(summary)
	head.Wrapping = fyne.TextWrapWord

	lead := widget.NewLabel(fmt.Sprintf("%d file(s) could not be written and are missing from the export:", res.Failed))
	lead.TextStyle = fyne.TextStyle{Bold: true}
	lead.Wrapping = fyne.TextWrapWord

	shown := res.Failures
	if len(shown) > modifyFailureListCap {
		shown = shown[:modifyFailureListCap]
	}
	lines := container.NewVBox()
	for _, f := range shown {
		l := widget.NewLabel(fmt.Sprintf("%s — %s", filepath.Base(f.File), f.Error))
		l.Wrapping = fyne.TextWrapWord
		lines.Add(l)
	}
	if extra := len(res.Failures) - len(shown); extra > 0 {
		more := widget.NewLabel(fmt.Sprintf("…and %d more.", extra))
		more.TextStyle = fyne.TextStyle{Italic: true}
		lines.Add(more)
	}
	listHeight := canvas.NewRectangle(color.Transparent)
	listHeight.SetMinSize(fyne.NewSize(560, 200))
	listBox := container.NewStack(listHeight, container.NewVScroll(lines))

	tail := widget.NewLabel("The Activity Log holds the full list with timestamps.")
	tail.TextStyle = fyne.TextStyle{Italic: true}
	tail.Wrapping = fyne.TextWrapWord

	content := container.NewVBox(head, widget.NewSeparator(), lead, listBox, tail)
	dialog.ShowCustom("Modification — "+profileName+" — completed with errors",
		"Close", container.NewPadded(content), w)
}
