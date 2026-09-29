package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// importOneFile copies a single DICOM file from srcPath into downloadDir using
// the same organised subfolder hierarchy as the C-STORE SCP.
// Returns (dest, true, nil) when the file was copied, (dest, false, nil) when
// it was already present, or ("", false, err) on failure.
func importOneFile(srcPath, downloadDir string) (dest string, copied bool, err error) {
	// Header only: the naming tags all precede the pixel data, which the copy
	// below reads anyway.
	ds, parseErr := readDicomHeader(srcPath)
	if parseErr != nil {
		return "", false, parseErr
	}

	// firstElementString rather than sdicom.MustGetStrings, which panics on a
	// tag stored under an unexpected VR.
	getString := func(t tag.Tag) string {
		e, findErr := ds.FindElementByTag(t)
		if findErr != nil {
			return ""
		}
		return firstElementString(e)
	}

	dest = organizeFilePath(
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
		return dest, false, nil // already present
	}

	if mkErr := os.MkdirAll(filepath.Dir(dest), 0o755); mkErr != nil {
		return "", false, mkErr
	}

	if copyErr := scpCopyFile(srcPath, dest); copyErr != nil {
		return "", false, copyErr
	}
	return dest, true, nil
}

// buildImportContent constructs the Import tab.
// Returns (content, refreshFn) where refreshFn updates the destination-folder
// label when Preferences change. Imported files are added to the catalog and
// reloadLocal is invoked so the Local Browse tree picks them up.
func buildImportContent(a fyne.App, w fyne.Window, cfg *Settings, cat *catalog, reloadLocal func()) (fyne.CanvasObject, func()) {
	model := newResultsModel()
	seriesFiles := make(map[string][]string)

	var tree *widget.Tree
	sel := newNodeSelection(model,
		func(id string) { tree.RefreshItem(id) },
		func() { tree.Refresh() },
	)

	onTapped := func(id string, mods fyne.KeyModifier) { sel.Click(id, mods) }

	onMenu := func(id string, pos fyne.Position) {
		rawPaths := filesForNode(id, model, seriesFiles)
		capturedPaths := make([]string, len(rawPaths))
		copy(capturedPaths, rawPaths)
		previewTitle := "DICOM Preview — " + model.labelFor(id)

		previewItem := fyne.NewMenuItem("Preview Images", func() {
			go showDicomViewerPaths(a, w, previewTitle, capturedPaths)
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
		func(_ bool) fyne.CanvasObject { return newQueryRow(onTapped, onMenu) },
		func(id widget.TreeNodeID, _ bool, node fyne.CanvasObject) {
			row := node.(*queryRow)
			row.nodeID = id
			row.ct.Text = model.labelFor(id)
			row.ct.TextSize = theme.TextSize()
			if sel.Selected(id) {
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

	scanStatusLbl := widget.NewLabel("Select a source folder and click Scan.")
	importStatusLbl := widget.NewLabel("")
	progressBar := widget.NewProgressBar()
	progressBar.Hide()

	srcEntry := widget.NewEntry()
	srcEntry.SetPlaceHolder("Source folder containing DICOM files to import…")
	// Opens on the folder last imported from, so a repeat import from the same
	// share or disc is a click on Scan rather than a navigation.
	srcEntry.SetText(cfg.ImportSourceDir)

	scanBtn := widget.NewButton("Scan", func() {
		dir := strings.TrimSpace(srcEntry.Text)
		if dir == "" {
			return
		}
		model.clear()
		sel.Clear()
		seriesFiles = make(map[string][]string)
		importStatusLbl.SetText("")
		tree.Refresh()
		scanStatusLbl.SetText("Scanning…")

		go func() {
			studies, series, files, err := scanLocalFolder(dir, func(phase scanPhase, done, total int) {
				// An import source folder has no index to compare against, so
				// every file is read; only the phase distinguishes the two
				// halves of the wait.
				text := fmt.Sprintf("Reading %d of %d files…", done, total)
				if phase == scanPhaseWalk {
					text = fmt.Sprintf("Checking %d files…", done)
				}
				fyne.Do(func() { scanStatusLbl.SetText(text) })
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

	// One button, labelled and iconed, for choosing the source folder. There
	// were two: this one, and a folder icon that opened the source folder in
	// Windows Explorer — which did nothing at all until a folder had been
	// chosen, so on a freshly opened tab (the only state it is ever seen in)
	// it read as broken. The Explorer shortcut is not worth a second
	// folder-shaped button beside the one that matters.
	browseBtn := widget.NewButtonWithIcon("Browse…", theme.FolderOpenIcon(), func() {
		start := strings.TrimSpace(srcEntry.Text)
		if start == "" {
			start = cfg.ImportSourceDir
		}
		go func() {
			dir, ok := browseFolder(w.Title(), "Choose the folder to import from", start)
			if !ok {
				return
			}
			fyne.Do(func() {
				srcEntry.SetText(dir)
				cfg.ImportSourceDir = dir
				saveSettings(*cfg)
			})
		}()
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
			widget.NewButton("Collapse All", func() { collapseAllTree(tree) }),
			widget.NewButton("Clear", func() {
				filterEntry.SetText("")
				model.setFilter("")
				tree.Refresh()
			}),
		),
		filterEntry,
	)

	importBtn := widget.NewButton("Import Selected", func() {
		paths := sel.Paths(seriesFiles)

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
			var destPaths []string
			// Copied one at a time, deliberately: the source is often a CD or
			// a USB stick, where parallel reads fight over one read head, and
			// two workers could race on the same destination when the source
			// holds a duplicate instance. Progress is paced by time — see
			// startPacedProgress.
			var doneCount atomic.Int64
			publish := func() {
				done := int(doneCount.Load())
				fyne.Do(func() {
					progressBar.SetValue(float64(done) / float64(total))
					importStatusLbl.SetText(fmt.Sprintf("Importing %d / %d…", done, total))
				})
			}
			stopReporting := startPacedProgress(scanProgressInterval, publish)
			for _, p := range paths {
				dest, copied, err := importOneFile(p, destDir)
				switch {
				case err != nil:
					nFailed++
				case copied:
					nImported++
					destPaths = append(destPaths, dest)
				default:
					// Already present on disk — index it anyway in case it was
					// placed there before the catalog existed.
					nSkipped++
					destPaths = append(destPaths, dest)
				}
				doneCount.Add(1)
			}
			stopReporting()
			publish()
			if len(destPaths) > 0 {
				cat.ingestPaths(destPaths)
				if reloadLocal != nil {
					reloadLocal()
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
			container.NewHBox(browseBtn, scanBtn),
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
			widget.NewButton("Select All", func() { sel.SelectAll(model.activeRoots()) }),
			widget.NewButton("Clear Selection", func() { sel.Clear() }),
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
