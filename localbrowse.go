package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

type localStudy struct {
	patientName string
	patientID   string
	studyUID    string
	studyDate   string
	studyDesc   string
	accession   string
	modalities  string
}

type localSeries struct {
	studyUID     string
	seriesUID    string
	modality     string
	seriesNumber string
	seriesDesc   string
	numInstances int
}

// fileMeta is the metadata of one DICOM file needed to place it in the
// Patient → Study → Series hierarchy (tree model and SQLite catalog).
type fileMeta struct {
	path  string
	size  int64
	mtime int64

	patientName string
	patientID   string

	studyUID   string
	studyDate  string
	studyDesc  string
	accession  string
	modalities string

	seriesUID    string
	modality     string
	seriesNumber string
	seriesDesc   string
}

// parseLocalFileMeta parses one DICOM file (skipping pixel data) and returns
// its hierarchy metadata. ok is false when the file is unparsable or missing
// the Study/Series Instance UIDs.
func parseLocalFileMeta(path string) (m fileMeta, ok bool) {
	ds, parseErr := sdicom.ParseFile(path, nil, sdicom.SkipPixelData())
	if parseErr != nil {
		return fileMeta{}, false
	}

	getString := func(t tag.Tag) string {
		e, findErr := ds.FindElementByTag(t)
		if findErr != nil {
			return ""
		}
		strs := sdicom.MustGetStrings(e.Value)
		if len(strs) == 0 {
			return ""
		}
		return strings.TrimSpace(strs[0])
	}

	m = fileMeta{
		path:         path,
		patientName:  getString(tag.PatientName),
		patientID:    getString(tag.PatientID),
		studyUID:     getString(tag.StudyInstanceUID),
		studyDate:    getString(tag.StudyDate),
		studyDesc:    getString(tag.StudyDescription),
		accession:    getString(tag.AccessionNumber),
		modalities:   getString(tag.ModalitiesInStudy),
		seriesUID:    getString(tag.SeriesInstanceUID),
		modality:     getString(tag.Modality),
		seriesNumber: getString(tag.SeriesNumber),
		seriesDesc:   getString(tag.SeriesDescription),
	}
	if m.studyUID == "" || m.seriesUID == "" {
		return fileMeta{}, false
	}
	if m.modality == "" {
		m.modality = m.modalities
	}
	if info, statErr := os.Stat(path); statErr == nil {
		m.size = info.Size()
		m.mtime = info.ModTime().Unix()
	}
	return m, true
}

// scanLocalFolder walks dir and returns studies, series, and a map of
// seriesUID → file paths for every .dcm file found.
func scanLocalFolder(dir string, progress func(int)) ([]localStudy, []localSeries, map[string][]string, error) {
	type seriesKey struct{ studyUID, seriesUID string }

	studyMap := make(map[string]localStudy)
	seriesMap := make(map[seriesKey]*localSeries)
	filesByUID := make(map[string][]string) // seriesUID → paths
	fileCount := 0

	err := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".dcm") {
			return nil
		}

		m, ok := parseLocalFileMeta(path)
		if !ok {
			return nil
		}

		if _, exists := studyMap[m.studyUID]; !exists {
			studyMap[m.studyUID] = localStudy{
				patientName: m.patientName,
				patientID:   m.patientID,
				studyUID:    m.studyUID,
				studyDate:   m.studyDate,
				studyDesc:   m.studyDesc,
				accession:   m.accession,
				modalities:  m.modalities,
			}
		}

		k := seriesKey{m.studyUID, m.seriesUID}
		if sr, exists := seriesMap[k]; exists {
			sr.numInstances++
		} else {
			seriesMap[k] = &localSeries{
				studyUID:     m.studyUID,
				seriesUID:    m.seriesUID,
				modality:     m.modality,
				seriesNumber: m.seriesNumber,
				seriesDesc:   m.seriesDesc,
				numInstances: 1,
			}
		}

		filesByUID[m.seriesUID] = append(filesByUID[m.seriesUID], path)

		fileCount++
		if progress != nil && fileCount%25 == 0 {
			progress(fileCount)
		}
		return nil
	})

	var studies []localStudy
	for _, s := range studyMap {
		studies = append(studies, s)
	}
	var series []localSeries
	for _, sr := range seriesMap {
		series = append(series, *sr)
	}
	return studies, series, filesByUID, err
}

// filesForNode collects the file paths for a tree node from the seriesFiles map.
// Series → direct lookup. Study/patient → union of all descendant series files.
func filesForNode(id string, m *resultsModel, seriesFiles map[string][]string) []string {
	n, ok := m.nodes[id]
	if !ok {
		return nil
	}
	switch n.kind {
	case kindSeries:
		return seriesFiles[n.seriesInstanceUID]
	case kindStudy:
		var paths []string
		for _, childID := range m.childUIDs(id) {
			paths = append(paths, filesForNode(childID, m, seriesFiles)...)
		}
		return paths
	case kindPatient:
		var paths []string
		for _, studyID := range m.childUIDs(id) {
			paths = append(paths, filesForNode(studyID, m, seriesFiles)...)
		}
		return paths
	}
	return nil
}

// pruneEmptyDirs walks up from dir toward root, removing each directory that
// is empty after the previous removal. Stops at root or at the first non-empty
// or unremovable directory.
func pruneEmptyDirs(dir, root string) {
	for {
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			break
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			break
		}
		parent := filepath.Dir(dir)
		if err := os.Remove(dir); err != nil {
			break
		}
		dir = parent
	}
}

// formatBytes returns a human-readable byte count.
func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// showDeleteDialog confirms and executes deletion of paths from local storage.
// onDeleted is called on the main goroutine after the deletion completes so the
// caller can trigger a rescan.
func showDeleteDialog(w fyne.Window, cfg *Settings, paths []string, description string, onDeleted func()) {
	if len(paths) == 0 {
		return
	}

	// Deduplicate paths and calculate total size.
	seen := make(map[string]bool)
	var unique []string
	var totalBytes int64
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			unique = append(unique, p)
			if info, err := os.Stat(p); err == nil {
				totalBytes += info.Size()
			}
		}
	}

	msgLbl := widget.NewLabel(fmt.Sprintf(
		"%s\n\n%d file(s) (%s) will be permanently deleted from disk. This cannot be undone.",
		description, len(unique), formatBytes(totalBytes)))
	msgLbl.Wrapping = fyne.TextWrapWord

	statusLbl := widget.NewLabel("")
	statusLbl.Wrapping = fyne.TextWrapWord
	statusLbl.Hide()

	deleteBtn := widget.NewButton("Delete", nil)
	deleteBtn.Importance = widget.DangerImportance
	cancelBtn := widget.NewButton("Cancel", nil)

	content := container.NewVBox(
		msgLbl,
		statusLbl,
		widget.NewSeparator(),
		container.NewHBox(layout.NewSpacer(), cancelBtn, deleteBtn),
	)

	dlg := dialog.NewCustomWithoutButtons("Confirm Delete", container.NewPadded(content), w)
	dlg.Resize(fyne.NewSize(420, 0))

	cancelBtn.OnTapped = func() { dlg.Hide() }

	deleteBtn.OnTapped = func() {
		deleteBtn.Disable()
		cancelBtn.Disable()
		msgLbl.Hide()
		statusLbl.SetText(fmt.Sprintf("Deleting %d file(s)…", len(unique)))
		statusLbl.Show()

		go func() {
			var nOK, nFail int
			dirs := make(map[string]bool)
			for _, p := range unique {
				if err := os.Remove(p); err != nil {
					nFail++
				} else {
					nOK++
					dirs[filepath.Dir(p)] = true
				}
			}
			root := cfg.DownloadDir
			for dir := range dirs {
				pruneEmptyDirs(dir, root)
			}
			fyne.Do(func() {
				dlg.Hide()
				if nFail > 0 {
					dialog.ShowError(fmt.Errorf(
						"deleted %d file(s); %d could not be deleted", nOK, nFail), w)
				}
				if onDeleted != nil {
					onDeleted()
				}
			})
		}()
	}

	dlg.Show()
}

// showPushDialog presents a profile-selection + progress dialog for sending
// paths to a DICOM destination via C-STORE SCU.
func showPushDialog(w fyne.Window, cfg *Settings, paths []string, description string) {
	if len(paths) == 0 {
		return
	}
	if len(cfg.Profiles) == 0 {
		dialog.ShowInformation("No Profiles",
			"No server profiles are configured.\nAdd one in Preferences.", w)
		return
	}

	names := make([]string, len(cfg.Profiles))
	for i, p := range cfg.Profiles {
		names[i] = p.Name
	}

	destSelect := widget.NewSelect(names, nil)
	destSelect.SetSelectedIndex(0)

	progressBar := widget.NewProgressBar()
	statusLbl := widget.NewLabel(description)
	statusLbl.Wrapping = fyne.TextWrapWord

	pushBtn := widget.NewButton("Push", nil)
	pushBtn.Importance = widget.HighImportance
	cancelBtn := widget.NewButton("Cancel", nil)

	content := container.NewVBox(
		container.NewBorder(nil, nil, widget.NewLabel("Destination:"), nil, destSelect),
		progressBar,
		statusLbl,
		widget.NewSeparator(),
		container.NewHBox(layout.NewSpacer(), cancelBtn, pushBtn),
	)

	dlg := dialog.NewCustomWithoutButtons("Push to PACS", container.NewPadded(content), w)
	dlg.Resize(fyne.NewSize(460, 0))

	ctx, cancel := context.WithCancel(context.Background())
	cancelBtn.OnTapped = func() { cancel(); dlg.Hide() }

	pushBtn.OnTapped = func() {
		profileName := destSelect.Selected
		var chosen *ServerProfile
		for i := range cfg.Profiles {
			if cfg.Profiles[i].Name == profileName {
				chosen = &cfg.Profiles[i]
				break
			}
		}
		if chosen == nil {
			return
		}

		pushBtn.Disable()
		cancelBtn.Disable()
		destSelect.Disable()
		progressBar.SetValue(0)
		statusLbl.SetText(fmt.Sprintf("Connecting to %s…", chosen.Name))

		client := NewDicomClient(*chosen, cfg.LocalAETitle)
		total := len(paths)

		go func() {
			var nOK, nFail int
			_ = client.StoreFiles(ctx, paths, func(p StoreProgress) {
				if p.Err != nil {
					nFail++
					logError("push: failed %s: %v", filepath.Base(p.Path), p.Err)
				} else {
					nOK++
				}
				if p.Done%10 == 0 || p.Done == total {
					fyne.Do(func() {
						progressBar.SetValue(float64(p.Done) / float64(total))
						statusLbl.SetText(fmt.Sprintf("Sending %d / %d…", p.Done, total))
					})
				}
			})
			fyne.Do(func() {
				progressBar.SetValue(1)
				msg := fmt.Sprintf("Done — %d sent", nOK)
				if nFail > 0 {
					msg += fmt.Sprintf(", %d failed (see Activity Log)", nFail)
				}
				statusLbl.SetText(msg)
				pushBtn.Hide()
				cancelBtn.SetText("Close")
				cancelBtn.OnTapped = func() { dlg.Hide() }
				cancelBtn.Enable()
			})
		}()
	}

	dlg.Show()
}

// buildLocalBrowseContent constructs the Local Browse tab.
// Returns the tab content, a refresh func that re-renders the tree (call it
// after applying theme/selection preferences; it also reopens the catalog when
// the download directory changed), and a reload func that repopulates the tree
// from the catalog (call it after downloads or imports add files).
func buildLocalBrowseContent(a fyne.App, w fyne.Window, cfg *Settings, cat *catalog, openInViewer func(string)) (fyne.CanvasObject, func(), func()) {
	model := newResultsModel()
	selectedNodes := make(map[string]bool)
	seriesFiles := make(map[string][]string)

	var doScan func()
	var reloadFromDB func(status string)
	var pruneMissing func(paths []string)
	var verifyNode func(id string)
	var tree *widget.Tree

	var clearSubtree func(string)
	clearSubtree = func(id string) {
		if selectedNodes[id] {
			delete(selectedNodes, id)
			tree.RefreshItem(id)
		}
		for _, child := range model.childUIDs(id) {
			clearSubtree(child)
		}
	}

	var selectSubtree func(string)
	selectSubtree = func(id string) {
		selectedNodes[id] = true
		tree.RefreshItem(id)
		for _, child := range model.childUIDs(id) {
			selectSubtree(child)
		}
	}

	onTapped := func(id string) {
		verifyNode(id)
		if selectedNodes[id] {
			clearSubtree(id)
		} else {
			selectSubtree(id)
		}
	}

	var scanDir string

	onMenu := func(id string, pos fyne.Position) {
		verifyNode(id)
		// Collect the exact files for this node so Preview is scoped correctly.
		rawPaths := filesForNode(id, model, seriesFiles)
		capturedPaths := make([]string, len(rawPaths))
		copy(capturedPaths, rawPaths)
		previewTitle := "DICOM Preview — " + model.labelFor(id)

		localFolder := model.localFolderFor(id, scanDir)
		if localFolder == "" {
			localFolder = scanDir
		}

		_, studyUID, seriesUID, _ := model.uidsForNode(id)
		uid := seriesUID
		if uid == "" {
			uid = studyUID
		}

		previewItem := fyne.NewMenuItem("Preview Images", func() {
			if seriesUID != "" {
				// Series node: open the regular linear viewer.
				go showDicomViewerPaths(a, w, previewTitle, capturedPaths)
				return
			}
			// Study node: show the middle slice from each series in a grid.
			// Only cheap model lookups happen here on the UI goroutine; the
			// expensive work — sorting parses an InstanceNumber out of every
			// file in the study — runs inside showStudyOverviewWindow behind
			// its busy dialog.
			var thumbs []seriesThumb
			for _, childID := range model.childUIDs(id) {
				ps := filesForNode(childID, model, seriesFiles)
				if len(ps) == 0 {
					continue
				}
				thumbs = append(thumbs, seriesThumb{
					label:    model.labelFor(childID),
					modality: model.modalityFor(childID),
					paths:    ps,
				})
			}
			go showStudyOverviewWindow(a, w, previewTitle, thumbs)
		})
		previewItem.Disabled = studyUID == "" // patient-level: too broad to preview

		capturedLabel := model.labelFor(id)
		// Tag-level review: the dicomhdr-style tag inspector over this node's
		// files, in its own window. Enabled at every level — files are parsed
		// concurrently and the tree populates incrementally, so even a whole
		// patient loads progressively rather than blocking.
		tagsItem := fyne.NewMenuItem("View Tags", func() {
			go showTagViewerWindow(a, cfg, "DICOM Tags — "+capturedLabel, capturedPaths)
		})
		tagsItem.Disabled = len(capturedPaths) == 0

		viewerItem := fyne.NewMenuItem("Open in Viewer", func() { openInViewer(localFolder) })
		viewerItem.Disabled = cfg.ViewerPath == ""
		capturedFolder := localFolder
		openFolderItem := fyne.NewMenuItem("Open folder", func() {
			go exec.Command("explorer", capturedFolder).Start()
		})
		pushItem := fyne.NewMenuItem("Push to PACS…", func() {
			showPushDialog(w, cfg, capturedPaths,
				fmt.Sprintf("Push %d file(s) from %q to a DICOM destination.",
					len(capturedPaths), model.labelFor(id)))
		})
		deleteItem := fyne.NewMenuItem("Delete…", func() {
			showDeleteDialog(w, cfg, capturedPaths,
				fmt.Sprintf("Delete %q from local storage.", capturedLabel),
				func() { pruneMissing(capturedPaths) })
		})
		// Modification: apply a de-identification profile to this patient/study.
		// Profiles are re-read on every menu open so hand-edits to
		// ~/.dicomqr/profiles.json take effect immediately (dicomtool semantics).
		modItem := fyne.NewMenuItem("Modification", nil)
		modItem.Disabled = seriesUID != "" // patient and study levels only
		if !modItem.Disabled {
			var children []*fyne.MenuItem
			if profPath, err := modifyProfilesPath(); err == nil {
				if profCfg, err := loadModProfileConfig(profPath); err == nil {
					names := make([]string, 0, len(profCfg))
					for name := range profCfg {
						names = append(names, name)
					}
					sort.Strings(names)
					capturedRoot := scanDir
					if capturedRoot == "" {
						capturedRoot = cfg.DownloadDir
					}
					capturedStudyLevel := studyUID != ""
					for _, name := range names {
						children = append(children, fyne.NewMenuItem(name, func() {
							showModificationDialog(w, cfg, name, capturedLabel, capturedPaths, capturedRoot, capturedStudyLevel)
						}))
					}
				}
			}
			if len(children) == 0 {
				none := fyne.NewMenuItem("(no profiles defined)", nil)
				none.Disabled = true
				children = []*fyne.MenuItem{none}
			}
			modItem.ChildMenu = fyne.NewMenu("", children...)
		}
		copyUID := fyne.NewMenuItem("Copy UID", func() { w.Clipboard().SetContent(uid) })
		copyLabel := fyne.NewMenuItem("Copy label", func() { w.Clipboard().SetContent(model.labelFor(id)) })
		popup := widget.NewPopUpMenu(fyne.NewMenu("",
			previewItem,
			tagsItem,
			viewerItem,
			openFolderItem,
			pushItem,
			modItem,
			deleteItem,
			fyne.NewMenuItemSeparator(),
			copyUID, copyLabel,
		), w.Canvas())
		popup.ShowAtPosition(pos)
	}

	tree = widget.NewTree(
		model.childUIDs,
		model.isBranch,
		func(_ bool) fyne.CanvasObject { return newQueryRow(onTapped, onMenu) },
		func(id widget.TreeNodeID, _ bool, node fyne.CanvasObject) {
			row := node.(*queryRow)
			row.nodeID = id
			row.ct.Text = model.labelFor(id)
			row.ct.TextSize = theme.TextSize()
			if selectedNodes[id] {
				if cfg.SelectionColor != "" {
					row.ct.Color = hexToColor(cfg.SelectionColor)
				} else {
					row.ct.Color = theme.Color(theme.ColorNamePrimary)
				}
				row.ct.TextStyle = fyne.TextStyle{Bold: cfg.SelectionBold, Italic: cfg.SelectionItalic}
			} else {
				row.ct.Color = theme.Color(theme.ColorNameForeground)
				row.ct.TextStyle = fyne.TextStyle{}
			}
			row.Refresh()
		},
	)
	treeCollapseFix(tree)

	scanStatusLbl := widget.NewLabel("Click Scan to index the download folder.")

	folderLabel := widget.NewLabel(cfg.DownloadDir)
	folderLabel.Truncation = fyne.TextTruncateEllipsis

	// applyData replaces the tree contents, dropping selections for nodes that
	// no longer exist. Runs on the UI goroutine.
	applyData := func(studies []localStudy, series []localSeries, files map[string][]string) {
		seriesFiles = files
		model.clear()
		for _, s := range studies {
			model.addStudy(s.patientName, s.patientID, s.studyUID, s.studyDate,
				s.studyDesc, s.accession, s.modalities)
		}
		for _, sr := range series {
			model.addSeries(sr.studyUID, sr.seriesUID, sr.modality,
				sr.seriesNumber, sr.seriesDesc, sr.numInstances)
		}
		model.applyFilter()
		for id := range selectedNodes {
			if _, ok := model.nodes[id]; !ok {
				delete(selectedNodes, id)
			}
		}
		tree.Refresh()
	}

	// reloadFromDB repopulates the tree from the catalog. Safe to call from any
	// goroutine. A non-empty status overrides the default "N studies" message.
	reloadFromDB = func(status string) {
		if cat == nil {
			return
		}
		go func() {
			studies, series, files, err := cat.load()
			fyne.Do(func() {
				if err != nil {
					scanStatusLbl.SetText("Index error: " + err.Error())
					return
				}
				scanDir = cfg.DownloadDir
				applyData(studies, series, files)
				msg := status
				if msg == "" {
					if len(studies) == 0 {
						msg = "Click Scan to index the download folder."
					} else {
						noun := "studies"
						if len(studies) == 1 {
							noun = "study"
						}
						msg = fmt.Sprintf("%d %s, %d series in local index", len(studies), noun, len(series))
					}
				}
				scanStatusLbl.SetText(msg)
			})
		}()
	}

	statMissing := func(paths []string) []string {
		var missing []string
		for _, p := range paths {
			if _, err := os.Stat(p); err != nil {
				missing = append(missing, p)
			}
		}
		return missing
	}

	// pruneMissing stats paths in the background and removes those no longer on
	// disk from the catalog and tree, keeping both consistent with the folder.
	pruneMissing = func(paths []string) {
		captured := make([]string, len(paths))
		copy(captured, paths)
		go func() {
			if missing := statMissing(captured); len(missing) > 0 {
				n := cat.removePaths(missing)
				reloadFromDB(fmt.Sprintf("Removed %d file(s) no longer on disk", n))
			}
		}()
	}

	// verifyNode checks in the background that a touched node's files still
	// exist, pruning entries that were removed outside the app. A per-node
	// in-flight guard stops repeated taps from re-statting large subtrees.
	var verifyMu sync.Mutex
	verifying := make(map[string]bool)
	verifyNode = func(id string) {
		if cat == nil {
			return
		}
		paths := filesForNode(id, model, seriesFiles)
		if len(paths) == 0 {
			return
		}
		verifyMu.Lock()
		inFlight := verifying[id]
		verifying[id] = true
		verifyMu.Unlock()
		if inFlight {
			return
		}
		captured := make([]string, len(paths))
		copy(captured, paths)
		go func() {
			defer func() {
				verifyMu.Lock()
				delete(verifying, id)
				verifyMu.Unlock()
			}()
			if missing := statMissing(captured); len(missing) > 0 {
				n := cat.removePaths(missing)
				reloadFromDB(fmt.Sprintf("Removed %d file(s) no longer on disk", n))
			}
		}()
	}

	doScan = func() {
		dir := cfg.DownloadDir
		if dir == "" {
			return
		}
		scanDir = dir
		model.clear()
		selectedNodes = make(map[string]bool)
		seriesFiles = make(map[string][]string)
		tree.Refresh()
		scanStatusLbl.SetText("Scanning…")

		go func() {
			studies, series, files, err := scanLocalFolder(dir, func(n int) {
				fyne.Do(func() { scanStatusLbl.SetText(fmt.Sprintf("Scanning… %d files read", n)) })
			})
			if err == nil {
				if dbErr := cat.replaceAll(studies, series, files); dbErr != nil {
					logError("catalog: replace after scan: %v", dbErr)
				}
			}
			fyne.Do(func() {
				if err != nil {
					scanStatusLbl.SetText("Scan error: " + err.Error())
					return
				}
				applyData(studies, series, files)
				noun := "studies"
				if len(studies) == 1 {
					noun = "study"
				}
				scanStatusLbl.SetText(fmt.Sprintf("Found %d %s, %d series in %s",
					len(studies), noun, len(series), filepath.Base(dir)))
			})
		}()
	}
	scanBtn := widget.NewButton("Scan", doScan)

	openFolderBtn := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() {
		if cfg.DownloadDir == "" {
			return
		}
		go exec.Command("explorer", cfg.DownloadDir).Start()
	})

	dirBar := container.NewBorder(nil, nil,
		widget.NewLabel("Folder:"),
		container.NewHBox(openFolderBtn, scanBtn),
		folderLabel,
	)

	filterEntry := widget.NewEntry()
	filterEntry.SetPlaceHolder("Filter results…")
	var filterDebounce *time.Timer
	filterEntry.OnChanged = func(s string) {
		if filterDebounce != nil {
			filterDebounce.Stop()
		}
		filterDebounce = time.AfterFunc(150*time.Millisecond, func() {
			fyne.Do(func() {
				model.setFilter(s)
				if s != "" {
					tree.OpenAllBranches()
				}
				tree.Refresh()
			})
		})
	}

	filterBar := container.NewBorder(nil, nil, nil,
		container.NewHBox(
			widget.NewButton("Expand All", func() { tree.OpenAllBranches() }),
			widget.NewButton("Collapse All", func() { collapseAllTree(tree) }),
			widget.NewButton("Clear", func() {
				filterEntry.SetText("")
				model.setFilter("")
				tree.Refresh()
			}),
		),
		filterEntry,
	)

	collectSelected := func() []string {
		seen := make(map[string]bool)
		var paths []string
		for id := range selectedNodes {
			for _, p := range filesForNode(id, model, seriesFiles) {
				if !seen[p] {
					seen[p] = true
					paths = append(paths, p)
				}
			}
		}
		return paths
	}

	pushSelectedBtn := widget.NewButton("Push Selected…", func() {
		paths := collectSelected()
		if len(paths) == 0 {
			scanStatusLbl.SetText("Nothing selected — click tree items to select them first.")
			return
		}
		showPushDialog(w, cfg, paths,
			fmt.Sprintf("Push %d selected file(s) to a DICOM destination.", len(paths)))
	})

	deleteSelectedBtn := widget.NewButton("Delete Selected…", func() {
		paths := collectSelected()
		if len(paths) == 0 {
			scanStatusLbl.SetText("Nothing selected — click tree items to select them first.")
			return
		}
		showDeleteDialog(w, cfg, paths,
			fmt.Sprintf("Delete %d selected file(s) from local storage.", len(paths)),
			func() { pruneMissing(paths) })
	})

	localOpenInViewerBtn := widget.NewButton("Open in Viewer", func() { openInViewer(scanDir) })
	if cfg.ViewerPath == "" {
		localOpenInViewerBtn.Disable()
	}

	bottomBar := container.NewVBox(
		widget.NewSeparator(),
		container.NewHBox(
			widget.NewButton("Preview", func() {
				if scanDir == "" {
					return
				}
				go showDicomViewer(a, w, scanDir)
			}),
			localOpenInViewerBtn,
			pushSelectedBtn,
			deleteSelectedBtn,
			layout.NewSpacer(),
			widget.NewButton("Select All", func() {
				for _, id := range model.activeRoots() {
					selectSubtree(id)
				}
			}),
			widget.NewButton("Clear Selection", func() {
				selectedNodes = make(map[string]bool)
				tree.Refresh()
			}),
		),
		scanStatusLbl,
	)

	content := container.NewBorder(
		container.NewVBox(dirBar, filterBar),
		bottomBar,
		nil, nil,
		tree,
	)

	// Populate the tree from the persisted index at startup, so the previous
	// session's contents appear without a disk rescan.
	reloadFromDB("")

	return content, func() {
		folderLabel.SetText(cfg.DownloadDir)
		if cfg.ViewerPath == "" {
			localOpenInViewerBtn.Disable()
		} else {
			localOpenInViewerBtn.Enable()
		}
		// The download directory changed in Preferences: switch to that
		// directory's own index file and reload the tree from it.
		if cat != nil && cfg.DownloadDir != "" && cat.Dir() != cfg.DownloadDir {
			go func() {
				if err := cat.Reopen(cfg.DownloadDir); err != nil {
					logError("catalog: reopen %s: %v", cfg.DownloadDir, err)
					return
				}
				reloadFromDB("")
			}()
		}
		tree.Refresh()
	}, func() { reloadFromDB("") }
}
