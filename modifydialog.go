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
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	sqweekdialog "github.com/sqweek/dialog"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// fixvrOffLabel is the Select option representing "no fixvr handling".
const fixvrOffLabel = "(off)"

// validateExportFolderName checks that name is usable as a single Windows
// folder component. The name replaces the original patient/study folder names
// in the export, so it is always typed by the user rather than derived from
// the (PHI-bearing) source folders.
func validateExportFolderName(name string) error {
	if name == "" {
		return fmt.Errorf("enter an export folder name — the original study folder name is not reused because it often contains PHI")
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
// profileName to files (the local files of one Patient or Study node).
// rootDir is the download-folder root the files live under; studyLevel is
// true when the selection is a Study node, false for a Patient node — it
// drives the PHI-safe output layout (see exportRelPaths).
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
	aliases := TagConfig{}
	if tagsPath, err := modifyTagsPath(); err == nil {
		var lerr error
		if aliases, lerr = loadTagConfig(tagsPath); lerr != nil {
			dialog.ShowError(fmt.Errorf("loading tags.json: %w", lerr), w)
			return
		}
	}
	resolved, err := resolveModProfile(profileName, profCfg)
	if err != nil {
		dialog.ShowError(err, w)
		return
	}
	buildModificationDialog(w, cfg, profileName, nodeLabel, files, rootDir, studyLevel, resolved, aliases)
}

// buildModificationDialog constructs and shows the confirmation dialog for an
// already-resolved profile.
func buildModificationDialog(w fyne.Window, cfg *Settings, profileName, nodeLabel string,
	files []string, rootDir string, studyLevel bool, resolved ModProfile, aliases TagConfig) {

	header := widget.NewLabel(fmt.Sprintf("Profile %q — %d file(s) from %s",
		profileName, len(files), nodeLabel))
	header.Wrapping = fyne.TextWrapWord

	// tagLabel renders a tag reference as "Name (GGGG,EEEE)" where resolvable.
	tagLabel := func(ref string) string {
		ref = strings.TrimSpace(ref)
		t, err := parseTagString(aliases.Resolve(ref))
		if err != nil {
			return ref
		}
		if name := tagDisplayName(t, aliases); name != "" {
			return fmt.Sprintf("%s (%04X,%04X)", name, t.Group, t.Element)
		}
		return fmt.Sprintf("%04X,%04X", t.Group, t.Element)
	}

	// Set values — one editable row per effective "TAG=VALUE" entry.
	// The Patient Name and Patient ID rows are remembered so entering a
	// Patient Name can auto-populate the Patient ID and export folder name.
	type setRow struct {
		tagStr string
		entry  *widget.Entry
	}
	var setRows []setRow
	var patNameEntry, patIDEntry *widget.Entry
	setForm := widget.NewForm()
	for _, s := range resolved.Sets {
		tagStr, value, ok := strings.Cut(s, "=")
		if !ok || strings.TrimSpace(tagStr) == "" {
			logWarn("modify: ignoring malformed set entry %q in profile %q", s, profileName)
			continue
		}
		entry := widget.NewEntry()
		entry.SetText(value)
		setForm.Append(tagLabel(tagStr), entry)
		setRows = append(setRows, setRow{tagStr: strings.TrimSpace(tagStr), entry: entry})
		if t, terr := parseTagString(aliases.Resolve(strings.TrimSpace(tagStr))); terr == nil {
			switch {
			case t == tag.PatientName && patNameEntry == nil:
				patNameEntry = entry
			case t == tag.PatientID && patIDEntry == nil:
				patIDEntry = entry
			}
		}
	}

	// Options — effective profile values; unset fields show the application
	// defaults (empty masks, no date shift, fixvr off, booleans false).
	remapCheck := widget.NewCheck("", nil)
	remapCheck.SetChecked(resolved.RemapUIDs)
	privCheck := widget.NewCheck("", nil)
	privCheck.SetChecked(resolved.Priv)
	dobEntry := widget.NewEntry()
	dobEntry.SetText(resolved.DOB)
	dobEntry.SetPlaceHolder("YYYYMMDD — empty = no masking")
	uidEntry := widget.NewEntry()
	uidEntry.SetText(resolved.UIDSuffix)
	uidEntry.SetPlaceHolder("digits 1-9 — empty = none")
	shiftEntry := widget.NewEntry()
	shiftEntry.SetText(resolved.ShiftDays)
	shiftEntry.SetPlaceHolder("e.g. -45 — shifts all dates except birth date")
	fixvrSelect := widget.NewSelect([]string{fixvrOffLabel, "correct", "skip", "passthrough"}, nil)
	if m := strings.ToLower(strings.TrimSpace(resolved.FixVR)); m != "" {
		fixvrSelect.SetSelected(m)
	} else {
		fixvrSelect.SetSelected(fixvrOffLabel)
	}
	optionsForm := widget.NewForm(
		widget.NewFormItem("Remap UIDs", remapCheck),
		widget.NewFormItem("Remove private tags", privCheck),
		widget.NewFormItem("Birth date mask", dobEntry),
		widget.NewFormItem("UID suffix", uidEntry),
		widget.NewFormItem("Shift dates (days)", shiftEntry),
		widget.NewFormItem("Fix VR", fixvrSelect),
	)

	// Tags removed — read-only list (already keep-filtered by base resolution).
	removeLines := make([]string, 0, len(resolved.Removes))
	for _, r := range resolved.Removes {
		line := strings.TrimSpace(r)
		if t, err := parseTagString(aliases.Resolve(line)); err == nil {
			if name := tagDisplayName(t, aliases); name != "" {
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

	sections := []fyne.CanvasObject{header}
	if len(setRows) > 0 {
		sections = append(sections, boldLabel("Set values"), widget.NewSeparator(), setForm)
	}
	sections = append(sections,
		boldLabel("Options"), widget.NewSeparator(), optionsForm,
		boldLabel(fmt.Sprintf("Tags removed (%d)", len(removeLines))), widget.NewSeparator(), removeBox,
	)

	// Settings applied as-is without dialog controls.
	var notes []string
	if len(resolved.IgnoreTypes) > 0 {
		notes = append(notes, "skip Image Type "+strings.Join(resolved.IgnoreTypes, ", "))
	}
	if len(resolved.IgnoreModalities) > 0 {
		notes = append(notes, "skip Modality "+strings.Join(resolved.IgnoreModalities, ", "))
	}
	if len(resolved.PerModality) > 0 {
		mods := make([]string, 0, len(resolved.PerModality))
		for k := range resolved.PerModality {
			mods = append(mods, k)
		}
		sort.Strings(mods)
		notes = append(notes, "per-modality overrides: "+strings.Join(mods, ", "))
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
	// by the user (default: profile + timestamp) and replaces the original
	// patient/study folder names in the output, which often contain PHI.
	exportNameEntry := widget.NewEntry()
	exportNameEntry.SetText(sanitize(profileName) + "-" + time.Now().Format("20060102-150405"))

	// Entering a Patient Name auto-populates the Patient ID and the export
	// folder name with the same value, since all three usually carry the new
	// anonymized identity. Each target follows only while the user has not
	// edited it directly: once Patient ID or the export name diverges from the
	// last auto-filled value, later Patient Name edits leave that field alone.
	// Clearing the Patient Name restores the profile-plus-timestamp export
	// default rather than leaving an empty (invalid) folder name.
	if patNameEntry != nil {
		defaultExportName := exportNameEntry.Text
		lastAutoID := ""
		if patIDEntry != nil {
			lastAutoID = patIDEntry.Text
		}
		lastAutoExport := defaultExportName
		patNameEntry.OnChanged = func(v string) {
			v = strings.TrimSpace(v)
			if patIDEntry != nil && patIDEntry.Text == lastAutoID {
				lastAutoID = v
				patIDEntry.SetText(v)
			}
			if exportNameEntry.Text == lastAutoExport {
				name := strings.TrimRight(sanitize(v), ". ")
				if name == "" {
					name = defaultExportName
				}
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
				"the output folder must be outside the download folder (%s) — modified files are never mixed into the local index", cfg.DownloadDir), w)
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
			dialog.ShowError(fmt.Errorf("saving the default output folder: %w", err), w)
		}
	}
	changeOutDirBtn := widget.NewButton("Change…", func() {
		go func() {
			picker := sqweekdialog.Directory().Title("Choose output folder for modified files")
			if chosenOutDir != "" {
				picker = picker.SetStartDir(chosenOutDir)
			}
			dir, derr := picker.Browse()
			if derr != nil {
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
	exportForm := widget.NewForm(
		widget.NewFormItem("Export folder name", exportNameEntry),
		widget.NewFormItem("Output folder", container.NewBorder(nil, nil, nil, changeOutDirBtn, outDirLabel)),
		widget.NewFormItem("Zip export", zipCheck),
	)
	exportNote := widget.NewLabel("Files are written under <output folder>\\<export folder name> — " +
		"or, with Zip export, into a compressed <output folder>\\<export folder name>.zip. " +
		"The original patient and study folder names are never reused — they often contain PHI.")
	exportNote.TextStyle = fyne.TextStyle{Italic: true}
	exportNote.Wrapping = fyne.TextWrapWord
	sections = append(sections, boldLabel("Export"), widget.NewSeparator(), exportForm, exportNote)

	var dlg dialog.Dialog
	cancelBtn := widget.NewButton("Cancel", func() { dlg.Hide() })
	modifyBtn := widget.NewButton("Modify…", nil)
	modifyBtn.Importance = widget.HighImportance
	sections = append(sections, widget.NewSeparator(),
		container.NewHBox(layout.NewSpacer(), cancelBtn, modifyBtn))

	minWidth := canvas.NewRectangle(color.Transparent)
	minWidth.SetMinSize(fyne.NewSize(640, 0))
	content := container.NewStack(minWidth, container.NewVBox(sections...))
	dlg = dialog.NewCustomWithoutButtons("Modification — "+profileName, container.NewPadded(content), w)

	modifyBtn.OnTapped = func() {
		// Validate the editable fields; compileModifyParams re-checks, but
		// friendly messages belong here where the user can correct them.
		exportName := strings.TrimSpace(exportNameEntry.Text)
		if err := validateExportFolderName(exportName); err != nil {
			dialog.ShowError(err, w)
			return
		}
		dob, err := validateDOBMask(dobEntry.Text)
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		uidSfx, err := validateUIDSuffix(uidEntry.Text)
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		if remapCheck.Checked && uidSfx != "" {
			dialog.ShowError(fmt.Errorf("Remap UIDs and a UID suffix cannot be combined — clear one of them"), w)
			return
		}
		shift, err := validateShiftDays(shiftEntry.Text)
		if err != nil {
			dialog.ShowError(err, w)
			return
		}

		edited := resolved
		edited.Sets = make([]string, 0, len(setRows))
		for _, row := range setRows {
			edited.Sets = append(edited.Sets, row.tagStr+"="+row.entry.Text)
		}
		edited.DOB = dob
		edited.UIDSuffix = uidSfx
		edited.ShiftDays = shift
		edited.RemapUIDs = remapCheck.Checked
		edited.Priv = privCheck.Checked
		if sel := fixvrSelect.Selected; sel == "" || sel == fixvrOffLabel {
			edited.FixVR = ""
		} else {
			edited.FixVR = sel
		}

		params, err := compileModifyParams(edited, aliases)
		if err != nil {
			dialog.ShowError(err, w)
			return
		}

		// startRun launches the modification into outBase\exportName — a folder
		// tree, or a single .zip archive when Zip export is checked — using the
		// PHI-safe layout (runs on the UI goroutine).
		startRun := func(outBase string) {
			if !validOutDir(outBase) {
				return
			}
			rels := exportRelPaths(files, rootDir, studyLevel)
			if zipCheck.Checked {
				zipName := exportName
				if !strings.EqualFold(filepath.Ext(zipName), ".zip") {
					zipName += ".zip"
				}
				zipPath := filepath.Join(outBase, zipName)
				begin := func() {
					dlg.Hide()
					showModificationRunDialog(w, profileName, files, rootDir, zipPath, params, rels, true)
				}
				if _, serr := os.Stat(zipPath); serr == nil {
					dialog.ShowConfirm("Zip file exists",
						fmt.Sprintf("%s\nalready exists and will be replaced. Continue?", zipPath),
						func(ok bool) {
							if ok {
								begin()
							}
						}, w)
					return
				}
				begin()
				return
			}
			exportRoot := filepath.Join(outBase, exportName)
			begin := func() {
				dlg.Hide()
				showModificationRunDialog(w, profileName, files, rootDir, exportRoot, params, rels, false)
			}
			if entries, rerr := os.ReadDir(exportRoot); rerr == nil && len(entries) > 0 {
				dialog.ShowConfirm("Export folder exists",
					fmt.Sprintf("%s\nalready exists and is not empty. Files with matching names will be overwritten. Continue?", exportRoot),
					func(ok bool) {
						if ok {
							begin()
						}
					}, w)
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
			picker := sqweekdialog.Directory().Title("Choose output folder for modified files")
			outDir, derr := picker.Browse()
			if derr != nil {
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

	dlg.Show()
}

// showModificationRunDialog runs the modification in the background with a
// progress bar and cancel support, then shows the summary. rels maps each
// file to its PHI-safe path under outDir (see exportRelPaths). With asZip,
// outDir is the path of the single .zip archive the run writes into instead
// of a folder tree.
func showModificationRunDialog(w fyne.Window, profileName string, files []string,
	rootDir, outDir string, params modifyParams, rels map[string]string, asZip bool) {

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
		onProgress := func(done, tot int) {
			if done%10 == 0 || done == tot {
				fyne.Do(func() {
					progressBar.SetValue(float64(done) / float64(tot))
					statusLbl.SetText(fmt.Sprintf("Processing %d / %d…", done, tot))
				})
			}
		}
		var res modifyResult
		if asZip {
			res = runModificationToZip(ctx, files, rootDir, outDir, params, rels, onProgress)
		} else {
			res = runModification(ctx, files, rootDir, outDir, params, rels, onProgress)
		}
		logInfo("modify: %q finished — %d written, %d skipped, %d failed, cancelled=%v → %s",
			profileName, res.Processed, res.Skipped, res.Failed, res.Canceled, outDir)
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
				msg += fmt.Sprintf(", %d failed (see Activity Log)", res.Failed)
			}
			statusLbl.SetText(msg)
			cancelBtn.SetText("Close")
			cancelBtn.OnTapped = func() { dlg.Hide() }
			cancelBtn.Enable()
		})
	}()

	dlg.Show()
}
