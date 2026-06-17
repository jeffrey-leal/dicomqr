package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
	sqweekdialog "github.com/sqweek/dialog"
)

// importOneFile copies a single DICOM file from srcPath into downloadDir using
// the same organised subfolder hierarchy as the C-STORE SCP.
// Returns (true, nil) when the file was copied, (false, nil) when it was
// already present, or (false, err) on failure.
func importOneFile(srcPath, downloadDir string) (copied bool, err error) {
	ds, parseErr := sdicom.ParseFile(srcPath, nil, sdicom.SkipPixelData())
	if parseErr != nil {
		return false, parseErr
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

	dest := organizeFilePath(
		downloadDir,
		getString(tag.PatientName),
		getString(tag.PatientID),
		getString(tag.StudyDescription),
		getString(tag.StudyDate),
		getString(tag.SeriesDescription),
		getString(tag.SeriesNumber),
		getString(tag.SOPInstanceUID),
	)

	if _, statErr := os.Stat(dest); statErr == nil {
		return false, nil // already present
	}

	if mkErr := os.MkdirAll(filepath.Dir(dest), 0o755); mkErr != nil {
		return false, mkErr
	}

	return true, scpCopyFile(srcPath, dest)
}

// buildImportContent constructs the Import tab.
// Returns (content, refreshFn) where refreshFn updates the destination-folder
// label when Preferences change.
func buildImportContent(a fyne.App, w fyne.Window, cfg *Settings) (fyne.CanvasObject, func()) {
	model := newResultsModel()
	selectedNodes := make(map[string]bool)
	seriesFiles := make(map[string][]string)

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
		if selectedNodes[id] {
			clearSubtree(id)
		} else {
			selectSubtree(id)
		}
	}

	onMenu := func(id string, pos fyne.Position) {
		rawPaths := filesForNode(id, model, seriesFiles)
		capturedPaths := make([]string, len(rawPaths))
		copy(capturedPaths, rawPaths)
		previewTitle := "DICOM Preview — " + model.labelFor(id)

		previewItem := fyne.NewMenuItem("Preview Images", func() {
			go showDicomViewerPaths(a, previewTitle, capturedPaths)
		})
		_, studyUID, seriesUID, _ := model.uidsForNode(id)
		uid := seriesUID
		if uid == "" {
			uid = studyUID
		}
		copyUID := fyne.NewMenuItem("Copy UID", func() { w.Clipboard().SetContent(uid) })
		copyLbl := fyne.NewMenuItem("Copy label", func() { w.Clipboard().SetContent(model.labelFor(id)) })
		popup := widget.NewPopUpMenu(fyne.NewMenu("",
			previewItem,
			fyne.NewMenuItemSeparator(),
			copyUID, copyLbl,
		), w.Canvas())
		popup.ShowAtPosition(pos)
	}

	tree = widget.NewTree(
		model.childUIDs,
		model.isBranch,
		func(_ bool) fyne.CanvasObject { return newQueryRow(w.Canvas(), onTapped, onMenu) },
		func(id widget.TreeNodeID, _ bool, node fyne.CanvasObject) {
			row := node.(*queryRow)
			row.nodeID = id
			row.tooltipText = model.tooltipFor(id)
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

	scanStatusLbl := widget.NewLabel("Select a source folder and click Scan.")
	importStatusLbl := widget.NewLabel("")
	progressBar := widget.NewProgressBar()
	progressBar.Hide()

	srcEntry := widget.NewEntry()
	srcEntry.SetPlaceHolder("Source folder containing DICOM files to import…")

	scanBtn := widget.NewButton("Scan", func() {
		dir := strings.TrimSpace(srcEntry.Text)
		if dir == "" {
			return
		}
		model.clear()
		selectedNodes = make(map[string]bool)
		seriesFiles = make(map[string][]string)
		importStatusLbl.SetText("")
		tree.Refresh()
		scanStatusLbl.SetText("Scanning…")

		go func() {
			studies, series, files, err := scanLocalFolder(dir, func(n int) {
				fyne.Do(func() { scanStatusLbl.SetText(fmt.Sprintf("Scanning… %d files read", n)) })
			})
			fyne.Do(func() {
				if err != nil {
					scanStatusLbl.SetText("Scan error: " + err.Error())
					return
				}
				seriesFiles = files
				for _, s := range studies {
					model.addStudy(s.patientName, s.patientID, s.studyUID, s.studyDate,
						s.studyDesc, s.accession, s.modalities)
				}
				for _, sr := range series {
					model.addSeries(sr.studyUID, sr.seriesUID, sr.modality,
						sr.seriesNumber, sr.seriesDesc, sr.numInstances)
				}
				model.applyFilter()
				tree.Refresh()
				noun := "studies"
				if len(studies) == 1 {
					noun = "study"
				}
				scanStatusLbl.SetText(fmt.Sprintf("Found %d %s, %d series in %s",
					len(studies), noun, len(series), filepath.Base(dir)))
			})
		}()
	})

	srcEntry.OnSubmitted = func(_ string) { scanBtn.OnTapped() }

	browseBtn := widget.NewButton("Browse…", func() {
		go func() {
			dir, err := sqweekdialog.Directory().Browse()
			if err != nil {
				return
			}
			fyne.Do(func() { srcEntry.SetText(dir) })
		}()
	})

	openSrcBtn := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() {
		dir := strings.TrimSpace(srcEntry.Text)
		if dir == "" {
			return
		}
		go exec.Command("explorer", dir).Start()
	})

	destLabel := widget.NewLabel(cfg.DownloadDir)
	destLabel.Truncation = fyne.TextTruncateEllipsis

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
			widget.NewButton("Collapse All", func() { tree.CloseAllBranches() }),
			widget.NewButton("Clear", func() {
				filterEntry.SetText("")
				model.setFilter("")
				tree.Refresh()
			}),
		),
		filterEntry,
	)

	importBtn := widget.NewButton("Import Selected", func() {
		// Collect unique file paths across all selected nodes. Because
		// selectSubtree marks both parents and children, iterate the map and
		// deduplicate so each file is only copied once.
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

		if len(paths) == 0 {
			importStatusLbl.SetText("Nothing selected — click tree items to select them first.")
			return
		}

		destDir := cfg.DownloadDir
		if destDir == "" {
			importStatusLbl.SetText("Download folder not configured — open Preferences to set it.")
			return
		}

		total := len(paths)
		importStatusLbl.SetText(fmt.Sprintf("Importing 0 / %d…", total))
		progressBar.SetValue(0)
		progressBar.Show()

		go func() {
			var nImported, nSkipped, nFailed int
			for i, p := range paths {
				copied, err := importOneFile(p, destDir)
				switch {
				case err != nil:
					nFailed++
				case copied:
					nImported++
				default:
					nSkipped++
				}
				done := i + 1
				if done%10 == 0 || done == total {
					fyne.Do(func() {
						progressBar.SetValue(float64(done) / float64(total))
						importStatusLbl.SetText(fmt.Sprintf("Importing %d / %d…", done, total))
					})
				}
			}
			fyne.Do(func() {
				progressBar.Hide()
				msg := fmt.Sprintf("Done — %d imported", nImported)
				if nSkipped > 0 {
					msg += fmt.Sprintf(", %d already present", nSkipped)
				}
				if nFailed > 0 {
					msg += fmt.Sprintf(", %d failed", nFailed)
				}
				importStatusLbl.SetText(msg)
			})
		}()
	})

	topBar := container.NewVBox(
		container.NewBorder(nil, nil,
			widget.NewLabel("Source:"),
			container.NewHBox(openSrcBtn, browseBtn, scanBtn),
			srcEntry,
		),
		container.NewBorder(nil, nil,
			widget.NewLabel("Destination:"),
			nil,
			destLabel,
		),
		filterBar,
	)

	bottomBar := container.NewVBox(
		widget.NewSeparator(),
		container.NewHBox(
			importBtn,
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
		progressBar,
		importStatusLbl,
		scanStatusLbl,
	)

	content := container.NewBorder(topBar, bottomBar, nil, nil, tree)

	return content, func() {
		destLabel.SetText(cfg.DownloadDir)
		tree.Refresh()
	}
}
