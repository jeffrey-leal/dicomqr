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
	tsSelect := widget.NewSelect(modProfileTSLabels, nil)
	tsSelect.SetSelected(transferSyntaxPrefLabel(resolved.TransferSyntax))

	// Remap UIDs and the UID suffix share a row and grey out together, through
	// the same helpers the profile editor uses — the exclusion is visible
	// rather than a message on confirm.
	uidSuffixLabel := widget.NewLabel("UID suffix")
	syncUIDSuffixEnabled(remapCheck, uidSuffixLabel, uidEntry)
	uidRow := uidSuffixRow(remapCheck, uidSuffixLabel, uidEntry)

	// Row order is the profile editor's, deliberately — the two Options blocks
	// show the same fields and are read against each other, so they must not be
	// ordered differently. The one field the editor has here and this dialog
	// does not is Zip export: it lives in Export below, where the user is
	// naming the output and can see what the checkbox changes.
	optionsForm := widget.NewForm(
		widget.NewFormItem("Birth date mask", dobEntry),
		widget.NewFormItem("Remap UIDs", uidRow),
		widget.NewFormItem("Remove private tags", privCheck),
		widget.NewFormItem("Shift dates (days)", shiftEntry),
		widget.NewFormItem("Fix VR", fixvrSelect),
		widget.NewFormItem("Output transfer syntax", tsSelect),
	)

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
	sections = append(sections,
		prefSection("Options", optionsForm),
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
	sections = append(sections, prefSection("Export", exportForm, exportNote))

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
		if remapCheck.Checked {
			// The suffix entry is disabled while Remap UIDs is checked, so text
			// left in it is inert and must not reach the run — the engine
			// rejects the combination, and refusing here would be a dead end
			// because the field cannot be cleared while it is disabled.
			uidSfx = ""
		}
		shift, err := validateShiftDays(shiftEntry.Text)
		if err != nil {
			dialog.ShowError(err, w)
			return
		}

		// The rows hold resolved literals, so each value can be checked against
		// its tag's value representation here — the last place a per-run edit
		// can put a word into a date field before it reaches every output file.
		edited := resolved
		edited.Sets = make([]string, 0, len(setRows))
		for _, row := range setRows {
			if row.parsed {
				if verr := validateSetValue(row.t, row.entry.Text); verr != nil {
					dialog.ShowError(fmt.Errorf("Set values: %w", verr), w)
					return
				}
			}
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
		edited.TransferSyntax = transferSyntaxPrefFromLabel(tsSelect.Selected)

		params, err := compileModifyParams(edited)
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
				msg += fmt.Sprintf(", %d failed", res.Failed)
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
