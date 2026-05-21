package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/grailbio/go-dicom/dicomlog"
	sqweekdialog "github.com/sqweek/dialog"
)

const version = "0.1.0"

// buildDate is injected at link time: -ldflags "-X main.buildDate=YYYY-MM-DD"
var buildDate string

// connState represents the application connection lifecycle.
type connState int

const (
	stateDisconnected connState = iota
	stateConnected
	stateBusy
)

// rowLayout adds vertical padding around a single child (shared by queryRow).
type rowLayout struct{}

func (rowLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	pad := theme.TextSize() / 4
	for _, o := range objects {
		o.Move(fyne.NewPos(0, pad))
		o.Resize(fyne.NewSize(size.Width, size.Height-pad*2))
	}
}

func (rowLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	pad := theme.TextSize() / 4
	if len(objects) == 0 {
		return fyne.NewSize(0, pad*2)
	}
	s := objects[0].MinSize()
	return fyne.NewSize(s.Width, s.Height+pad*2)
}

func main() {
	dicomlog.SetLevel(2)
	setupLogFile()
	a := app.NewWithID("com.jeffreyleal.dicomqr")
	w := a.NewWindow("dicomqr")
	w.Resize(fyne.NewSize(900, 650))

	ensureDefaultSettings()
	cfg := loadSettings()

	currentTheme := newAppTheme(cfg.DarkTheme)
	if cfg.FontName != "" {
		if path := fontPathByName(cfg.FontName); path != "" {
			if res, err := loadFontResource(path); err == nil {
				currentTheme.font = res
				currentTheme.fontName = cfg.FontName
			}
		}
	}
	a.Settings().SetTheme(currentTheme)

	var (
		state         connState
		client        *DicomClient
		activeProfile ServerProfile
		scp           *StorageSCP
		cancelQuery   context.CancelFunc
	)

	// ── Status bar ──────────────────────────────────────────────────────────
	statusLabel := widget.NewLabel("v" + version)
	clockLabel := widget.NewLabel("")
	progressBar := widget.NewProgressBar()
	progressBar.Hide()

	setStatus := func(msg string) { fyne.Do(func() { statusLabel.SetText(msg) }) }

	go func() {
		for {
			fyne.Do(func() { clockLabel.SetText(time.Now().Format("2006-01-02  15:04:05")) })
			time.Sleep(time.Second)
		}
	}()

	statusBar := container.NewVBox(
		container.NewHBox(statusLabel, layout.NewSpacer(), clockLabel),
		progressBar,
	)

	// ── Results tree ────────────────────────────────────────────────────────
	model := newResultsModel()
	selectedNodes := make(map[string]bool)

	// tree is declared here so onTapped can reference it before widget.NewTree returns.
	var tree *widget.Tree

	onTapped := func(id string) {
		if selectedNodes[id] {
			delete(selectedNodes, id)
		} else {
			selectedNodes[id] = true
		}
		tree.Refresh()
	}

	onMenu := func(id string, pos fyne.Position) {
		_, studyUID, seriesUID, _ := model.uidsForNode(id)
		uid := seriesUID
		if uid == "" {
			uid = studyUID
		}
		copyUID := fyne.NewMenuItem("Copy UID", func() { w.Clipboard().SetContent(uid) })
		copyLabel := fyne.NewMenuItem("Copy label", func() { w.Clipboard().SetContent(model.labelFor(id)) })
		popup := widget.NewPopUpMenu(fyne.NewMenu("", copyUID, copyLabel), w.Canvas())
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
				row.ct.Color = theme.Color(theme.ColorNamePrimary)
				row.ct.TextStyle = fyne.TextStyle{Bold: true}
			} else {
				row.ct.Color = theme.Color(theme.ColorNameForeground)
				row.ct.TextStyle = fyne.TextStyle{}
			}
			row.Refresh()
		},
	)

	w.Canvas().AddShortcut(&fyne.ShortcutCopy{}, func(_ fyne.Shortcut) {
		for id := range selectedNodes {
			w.Clipboard().SetContent(model.labelFor(id))
			break
		}
	})

	// ── Query panel ─────────────────────────────────────────────────────────
	patientNameEntry := widget.NewEntry()
	patientNameEntry.SetPlaceHolder("DOE^JOHN or DOE*")
	patientIDEntry := widget.NewEntry()
	patientIDEntry.SetPlaceHolder("Patient ID")
	accessionEntry := widget.NewEntry()
	accessionEntry.SetPlaceHolder("Accession number")
	studyDateFromEntry := widget.NewEntry()
	studyDateFromEntry.SetPlaceHolder("YYYYMMDD")
	studyDateToEntry := widget.NewEntry()
	studyDateToEntry.SetPlaceHolder("YYYYMMDD")
	modalitySelect := widget.NewSelect([]string{"(Any)", "CT", "MR", "PT", "NM", "US", "CR", "DX", "XA", "RF"}, nil)
	modalitySelect.SetSelected("(Any)")

	doSearch := func() {
		if state != stateConnected || client == nil {
			dialog.ShowInformation("Not connected", "Connect to a DICOM server first.", w)
			return
		}
		model.clear()
		tree.Refresh()
		progressBar.Show()
		setStatus("Querying…")

		params := map[string]string{
			"PatientName":    patientNameEntry.Text,
			"PatientID":      patientIDEntry.Text,
			"AccessionNumber": accessionEntry.Text,
			"StudyDateFrom":  studyDateFromEntry.Text,
			"StudyDateTo":    studyDateToEntry.Text,
		}
		if modalitySelect.Selected != "(Any)" {
			params["ModalitiesInStudy"] = modalitySelect.Selected
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancelQuery = cancel

		go func() {
			defer cancel()
			ch, err := client.Find(ctx, activeProfile.InfoModel, params)
			if err != nil {
				setStatus("Query error: " + err.Error())
				fyne.Do(func() { progressBar.Hide() })
				return
			}
			var results []FindResult
			var queryErr error
			for r := range ch {
				if r.Err != nil {
					queryErr = r.Err
					break
				}
				results = append(results, r)
			}
			for range ch {} // drain so Find's goroutine can exit
			if queryErr != nil {
				fyne.Do(func() {
					progressBar.Hide()
					statusLabel.SetText("Query error: " + queryErr.Error())
				})
				return
			}
				fyne.Do(func() {
				for _, r := range results {
					model.addStudy(r.PatientName, r.PatientID, r.StudyInstanceUID,
						r.StudyDate, r.StudyDescription, r.AccessionNumber, r.ModalitiesInStudy)
				}
				tree.Refresh()
				tree.OpenAllBranches()
				progressBar.Hide()
				statusLabel.SetText(fmt.Sprintf("Query complete — %d studies", len(results)))
			})
		}()
	}

	doClearQuery := func() {
		patientNameEntry.SetText("")
		patientIDEntry.SetText("")
		accessionEntry.SetText("")
		studyDateFromEntry.SetText("")
		studyDateToEntry.SetText("")
		modalitySelect.SetSelected("(Any)")
		model.clear()
		selectedNodes = make(map[string]bool)
		tree.Refresh()
		setStatus("v" + version)
	}

	filtersBtn := widget.NewButton("Filters ▾", nil) // handler wired after connPanel
	var searchPopup *widget.PopUp

	doSearchAndClose := func() {
		if searchPopup != nil {
			searchPopup.Hide()
		}
		doSearch()
	}
	searchBtn := widget.NewButton("Search", doSearchAndClose)
	searchTopBtn := widget.NewButton("Search", doSearchAndClose)
	clearBtn := widget.NewButton("Clear", doClearQuery)

	patientNameEntry.OnSubmitted = func(_ string) {
		if searchPopup != nil {
			searchPopup.Hide()
		}
		doSearch()
	}
	patientIDEntry.OnSubmitted = func(_ string) {
		if searchPopup != nil {
			searchPopup.Hide()
		}
		doSearch()
	}
	accessionEntry.OnSubmitted = func(_ string) {
		if searchPopup != nil {
			searchPopup.Hide()
		}
		doSearch()
	}

	// ── Connection panel ─────────────────────────────────────────────────────
	profileNames := func() []string {
		names := make([]string, len(cfg.Profiles))
		for i, p := range cfg.Profiles {
			names[i] = p.Name
		}
		return names
	}

	profileSelect := widget.NewSelect(profileNames(), nil)
	if len(cfg.Profiles) > 0 {
		profileSelect.SetSelected(cfg.Profiles[0].Name)
	}

	connectBtn := widget.NewButton("Connect", nil)
	disconnectBtn := widget.NewButton("Disconnect", nil)
	echoBtn := widget.NewButton("Test (C-ECHO)", nil)
	disconnectBtn.Disable()
	echoBtn.Disable()

	setConnState := func(s connState, msg string) {
		state = s
		fyne.Do(func() {
			switch s {
			case stateDisconnected:
				connectBtn.Enable()
				disconnectBtn.Disable()
				echoBtn.Disable()
				searchBtn.Disable()
				searchTopBtn.Disable()
			case stateConnected:
				connectBtn.Disable()
				disconnectBtn.Enable()
				echoBtn.Enable()
				searchBtn.Enable()
				searchTopBtn.Enable()
			case stateBusy:
				connectBtn.Disable()
				disconnectBtn.Disable()
				echoBtn.Disable()
			}
			statusLabel.SetText(msg)
		})
	}
	setConnState(stateDisconnected, "v"+version)
	searchBtn.Disable()
	searchTopBtn.Disable()

	connectBtn.OnTapped = func() {
		idx := -1
		for i, p := range cfg.Profiles {
			if p.Name == profileSelect.Selected {
				idx = i
				break
			}
		}
		if idx < 0 {
			dialog.ShowInformation("No server selected", "Select a server profile before connecting.", w)
			return
		}
		profile := cfg.Profiles[idx]
		setConnState(stateBusy, "Connecting…")

		go func() {
			c := NewDicomClient(profile, cfg.LocalAETitle)
			if err := c.Echo(context.Background()); err != nil {
				setConnState(stateDisconnected, "Connection failed: "+err.Error())
				return
			}
			client = c
			activeProfile = profile

			s := NewStorageSCP(cfg.LocalAETitle, cfg.LocalSCPPort, cfg.DownloadDir)
			s.OnFileReceived = func(path string) {
				fyne.Do(func() { statusLabel.SetText("Received: " + path) })
			}
			if err := s.Start(); err != nil {
				setConnState(stateDisconnected, "SCP error: "+err.Error())
				return
			}
			scp = s

			setConnState(stateConnected, fmt.Sprintf("Connected: %s@%s:%d  |  SCP %s (AE: %s)",
					profile.RemoteAETitle, profile.Host, profile.Port, s.ListenAddr(), cfg.LocalAETitle))
		}()
	}

	disconnectBtn.OnTapped = func() {
		if cancelQuery != nil {
			cancelQuery()
		}
		if scp != nil {
			scp.Stop()
			scp = nil
		}
		client = nil
		activeProfile = ServerProfile{}
		setConnState(stateDisconnected, "Disconnected")
	}

	echoBtn.OnTapped = func() {
		if client == nil {
			return
		}
		go func() {
			if err := client.Echo(context.Background()); err != nil {
				setStatus("C-ECHO failed: " + err.Error())
			} else {
				setStatus("C-ECHO success")
			}
		}()
	}

	connPanel := container.NewVBox(
		container.NewBorder(nil, nil,
			container.NewHBox(widget.NewLabel("Server:"), profileSelect, searchTopBtn),
			container.NewHBox(filtersBtn, connectBtn, disconnectBtn, echoBtn),
		),
		widget.NewSeparator(),
	)

	filtersBtn.OnTapped = func() {
		if searchPopup != nil && searchPopup.Visible() {
			searchPopup.Hide()
			return
		}
		if searchPopup == nil {
			popupContent := container.NewVBox(
				widget.NewForm(
					widget.NewFormItem("Patient Name", patientNameEntry),
					widget.NewFormItem("Patient ID", patientIDEntry),
					widget.NewFormItem("Accession No", accessionEntry),
					widget.NewFormItem("Study Date From", studyDateFromEntry),
					widget.NewFormItem("Study Date To", studyDateToEntry),
					widget.NewFormItem("Modality", modalitySelect),
				),
				container.NewHBox(layout.NewSpacer(), searchBtn, clearBtn),
			)
			searchPopup = widget.NewPopUp(container.NewPadded(popupContent), w.Canvas())
		}
		pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(connPanel)
		searchPopup.ShowAtPosition(fyne.NewPos(pos.X, pos.Y+connPanel.Size().Height))
	}

	// ── Retrieve panel ───────────────────────────────────────────────────────
	downloadDirEntry := widget.NewEntry()
	downloadDirEntry.SetText(cfg.DownloadDir)
	downloadDirEntry.SetPlaceHolder("Select download folder…")
	downloadDirEntry.OnChanged = func(dir string) {
		cfg.DownloadDir = dir
		if scp != nil {
			scp.downloadDir = dir
		}
		saveSettings(cfg)
	}

	browseBtn := widget.NewButton("Browse…", func() {
		go func() {
			dir, err := sqweekdialog.Directory().Browse()
			if err != nil {
				return
			}
			// SetText triggers downloadDirEntry.OnChanged which updates cfg and scp.
			fyne.Do(func() { downloadDirEntry.SetText(dir) })
		}()
	})

	var cancelRetrieve context.CancelFunc

	retrieveBtn := widget.NewButton("Retrieve Selected", func() {
		if state != stateConnected || client == nil {
			dialog.ShowInformation("Not connected", "Connect to a DICOM server first.", w)
			return
		}
		if scp == nil || !scp.IsRunning() {
			dialog.ShowInformation("SCP not running",
				fmt.Sprintf("The local C-STORE SCP is not listening.\n\nDisconnect and reconnect to restart it.\nExpected port: %d  AE title: %s", cfg.LocalSCPPort, cfg.LocalAETitle), w)
			return
		}

		// Collect unique (patientID, studyUID) pairs from selected nodes.
		// Selecting a patient node expands to all its study children.
		type studyTarget struct{ patientID, studyUID string }
		seen := make(map[string]bool)
		var targets []studyTarget
		for id := range selectedNodes {
			patID, studyUID, _, _ := model.uidsForNode(id)
			if studyUID != "" {
				if !seen[studyUID] {
					seen[studyUID] = true
					targets = append(targets, studyTarget{patID, studyUID})
				}
			} else {
				// Patient node — expand to study children.
				for _, childID := range model.childUIDs(id) {
					childPatID, childStudyUID, _, _ := model.uidsForNode(childID)
					if childStudyUID != "" && !seen[childStudyUID] {
						seen[childStudyUID] = true
						targets = append(targets, studyTarget{childPatID, childStudyUID})
					}
				}
			}
		}
		if len(targets) == 0 {
			dialog.ShowInformation("Nothing selected", "Click one or more studies in the results list first.", w)
			return
		}

		count := len(targets)
		log.Printf("retrieve: %d studies, destAE=%s port=%d", count, cfg.LocalAETitle, cfg.LocalSCPPort)
		for i, t := range targets {
			log.Printf("  study[%d]: patientID=%s studyUID=%s", i, t.patientID, t.studyUID)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancelRetrieve = cancel
		progressBar.SetValue(0)
		progressBar.Show()
		statusLabel.SetText(fmt.Sprintf("Starting retrieve of %d studies…", count))

		go func() {
			defer cancel()

			// Count files received during this retrieve operation.
			var fileCount int64
			origOnFileReceived := scp.OnFileReceived
			scp.OnFileReceived = func(path string) {
				atomic.AddInt64(&fileCount, 1)
				fyne.Do(func() { statusLabel.SetText("Received: " + path) })
			}

			var cancelled bool
			for i, tgt := range targets {
				if ctx.Err() != nil {
					cancelled = true
					break
				}
				idx := i + 1
				fyne.Do(func() {
					statusLabel.SetText(fmt.Sprintf("Retrieving study %d/%d…", idx, count))
				})
				err := client.Move(ctx, "STUDY", tgt.patientID, tgt.studyUID, "", cfg.LocalAETitle, func(p MoveProgress) {
					sub := p.Remaining + p.Completed + p.Failed + p.Warning
					if sub > 0 {
						frac := (float64(i) + float64(p.Completed)/float64(sub)) / float64(count)
						fyne.Do(func() { progressBar.SetValue(frac) })
					}
				})
				if err != nil {
					if ctx.Err() != nil {
						cancelled = true
						break
					}
					msg := err.Error()
					scp.OnFileReceived = origOnFileReceived
					fyne.Do(func() {
						progressBar.Hide()
						statusLabel.SetText("Retrieve error: " + msg)
					})
					return
				}
			}

			scp.OnFileReceived = origOnFileReceived
			// Capture fileCount before fyne.Do; defer cancel() fires on return,
			// after which ctx.Err() would appear non-nil even on a clean completion.
			n := atomic.LoadInt64(&fileCount)
			fyne.Do(func() {
				progressBar.Hide()
				if cancelled {
					statusLabel.SetText("Retrieve cancelled")
				} else {
					statusLabel.SetText(fmt.Sprintf("Retrieved %d files successfully", n))
				}
			})
		}()
	})

	cancelRetrieveBtn := widget.NewButton("Cancel", func() {
		if cancelRetrieve != nil {
			cancelRetrieve()
		}
	})

	retrievePanel := container.NewVBox(
		widget.NewSeparator(),
		container.NewBorder(nil, nil,
			widget.NewLabel("Download to:"),
			browseBtn,
			downloadDirEntry,
		),
		container.NewHBox(retrieveBtn, cancelRetrieveBtn),
	)

	// ── Search bar (filter above tree) ───────────────────────────────────────
	filterEntry := widget.NewEntry()
	filterEntry.SetPlaceHolder("Filter results…")
	filterEntry.OnChanged = func(s string) {
		model.setFilter(s)
		if s != "" {
			tree.OpenAllBranches()
		}
		tree.Refresh()
	}
	filterBar := container.NewBorder(nil, nil, nil,
		widget.NewButton("Clear", func() {
			filterEntry.SetText("")
			model.setFilter("")
			tree.Refresh()
		}),
		filterEntry,
	)

	// ── Keyboard shortcuts ────────────────────────────────────────────────────
	w.Canvas().AddShortcut(
		&desktop.CustomShortcut{KeyName: fyne.KeyReturn, Modifier: fyne.KeyModifierShortcutDefault},
		func(_ fyne.Shortcut) { doSearch() },
	)
	w.Canvas().AddShortcut(
		&desktop.CustomShortcut{KeyName: fyne.KeyF, Modifier: fyne.KeyModifierShortcutDefault},
		func(_ fyne.Shortcut) { w.Canvas().Focus(patientNameEntry) },
	)

	// ── Menus ─────────────────────────────────────────────────────────────────
	fileMenu := fyne.NewMenu("File",
		fyne.NewMenuItem("Connect", func() { connectBtn.OnTapped() }),
		fyne.NewMenuItem("Disconnect", func() { disconnectBtn.OnTapped() }),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Preferences…", func() {
			showPreferencesDialog(a, w, currentTheme, &cfg, func(updated Settings) {
				cfg = updated
				profileSelect.Options = profileNames()
				profileSelect.Refresh()
			})
		}),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Quit", func() { a.Quit() }),
	)

	queryMenu := fyne.NewMenu("Query",
		fyne.NewMenuItem("Search", doSearch),
		fyne.NewMenuItem("Clear results", doClearQuery),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Retrieve Selected", func() { retrieveBtn.OnTapped() }),
		fyne.NewMenuItem("Cancel retrieve", func() {
			if cancelRetrieve != nil {
				cancelRetrieve()
			}
		}),
	)

	bd := buildDate
	if bd == "" {
		bd = "unknown"
	}
	helpMenu := fyne.NewMenu("Help",
		fyne.NewMenuItem("About", func() {
			lbl := widget.NewLabel(fmt.Sprintf(
				"dicomqr\nVersion %s\nBuild date: %s\n\nDICOM Q/R client for querying and\nretrieving studies from a PACS server.",
				version, bd))
			dialog.ShowCustom("About dicomqr", "OK", lbl, w)
		}),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Client info…", func() {
			info := fmt.Sprintf("Local AE Title:  %s\nLocal SCP port:  %d\nLocal IP:        %s\n\nRegister these on your PACS to enable C-MOVE.",
				cfg.LocalAETitle, cfg.LocalSCPPort, localIP())
			lbl := widget.NewLabel(info)
			lbl.TextStyle = fyne.TextStyle{Monospace: true}
			d := dialog.NewCustom("Client Info", "Close", container.NewPadded(lbl), w)
			d.Show()
		}),
	)

	w.SetMainMenu(fyne.NewMainMenu(fileMenu, queryMenu, helpMenu))

	// ── Layout ────────────────────────────────────────────────────────────────
	top := container.NewVBox(connPanel, filterBar)
	bottom := container.NewVBox(retrievePanel, statusBar)

	w.SetContent(container.NewBorder(top, bottom, nil, nil, tree))
	w.ShowAndRun()
}

// setupLogFile redirects the standard log package output to both stderr and
// ~/.dicomqr/dicom.log so that DICOM protocol messages (from the grailbio
// dicomlog package) are captured even in windowsgui builds.
func setupLogFile() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".dicomqr")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	logPath := filepath.Join(dir, "dicom.log")
	os.Remove(logPath)
	f, err := os.Create(logPath)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	log.SetFlags(log.Ltime | log.Lmicroseconds)
}
