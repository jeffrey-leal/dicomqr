package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/grailbio/go-dicom/dicomlog"
	sqweekdialog "github.com/sqweek/dialog"
)

const version = "0.2.0"

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
	a.SetIcon(appIcon)
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
		stateMu       sync.Mutex
		client        *DicomClient
		activeProfile ServerProfile
		scp           *StorageSCP
		cancelQuery   context.CancelFunc
		cancelConnect context.CancelFunc
		connCtx       context.Context
		cancelConn    context.CancelFunc
	)

	// Thread-safe state accessors (Phase 1-A)
	getState := func() connState {
		stateMu.Lock()
		defer stateMu.Unlock()
		return state
	}
	setState := func(s connState) {
		stateMu.Lock()
		defer stateMu.Unlock()
		state = s
	}

	// ── Status bar ──────────────────────────────────────────────────────────
	statusLabel := widget.NewLabel("v" + version)
	clockLabel := widget.NewLabel("")
	queryProgress := widget.NewProgressBarInfinite()
	queryProgress.Hide()
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
		queryProgress,
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
		tree.RefreshItem(id)
	}

	// startRetrieve is assigned below after the retrieve variables are in scope.
	var startRetrieve func(nodeIDs []string)

	onMenu := func(id string, pos fyne.Position) {
		_, studyUID, seriesUID, _ := model.uidsForNode(id)
		uid := seriesUID
		if uid == "" {
			uid = studyUID
		}
		retrieveItem := fyne.NewMenuItem("Retrieve", func() { startRetrieve([]string{id}) })
		copyUID := fyne.NewMenuItem("Copy UID", func() { w.Clipboard().SetContent(uid) })
		copyLabel := fyne.NewMenuItem("Copy label", func() { w.Clipboard().SetContent(model.labelFor(id)) })
		popup := widget.NewPopUpMenu(fyne.NewMenu("", retrieveItem, fyne.NewMenuItemSeparator(), copyUID, copyLabel), w.Canvas())
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

	tree.OnBranchOpened = func(id string) {
		if !strings.HasPrefix(id, "S:") || model.isSeriesLoaded(id) {
			return
		}
		model.markSeriesLoaded(id) // mark before goroutine to prevent duplicate queries
		_, studyUID, _, _ := model.uidsForNode(id)
		go func() {
			if connCtx == nil {
				return
			}
			ch, err := client.Find(connCtx, "SERIES", map[string]string{"StudyInstanceUID": studyUID})
			if err != nil {
				return
			}
			var series []FindResult
			for r := range ch {
				if r.Err == nil {
					series = append(series, r)
				}
			}
			for range ch {
			}
			fyne.Do(func() {
				for _, r := range series {
					model.addSeries(r.StudyInstanceUID, r.SeriesInstanceUID,
						r.Modality, r.SeriesNumber, r.SeriesDescription, r.NumInstances)
				}
				model.applyFilter()
				tree.RefreshItem(id)
			})
		}()
	}

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
	studyDateFromEntry := widget.NewDateEntry()
	studyDateFromEntry.Validator = nil
	studyDateToEntry := widget.NewDateEntry()
	studyDateToEntry.Validator = nil
	modalityCheck := widget.NewCheckGroup(
		[]string{"CT", "MR", "PT", "NM", "US", "CR", "DX", "XA", "RF"}, nil)
	modalityCheck.Horizontal = true

	doSearch := func() {
		if getState() != stateConnected || client == nil {
			dialog.ShowInformation("Not connected", "Connect to a DICOM server first.", w)
			return
		}

		runSearch := func() {
			model.clear()
			tree.Refresh()
			queryProgress.Show()
			setStatus("Querying…")

			dateFrom := ""
			if studyDateFromEntry.Date != nil {
				dateFrom = studyDateFromEntry.Date.Format("20060102")
			}
			dateTo := ""
			if studyDateToEntry.Date != nil {
				dateTo = studyDateToEntry.Date.Format("20060102")
			}
			wildcard := func(s string) string {
				if s == "" || strings.HasSuffix(s, "*") {
					return s
				}
				return s + "*"
			}
			baseParams := map[string]string{
				"PatientName":     wildcard(patientNameEntry.Text),
				"PatientID":       wildcard(patientIDEntry.Text),
				"AccessionNumber": wildcard(accessionEntry.Text),
				"StudyDateFrom":   dateFrom,
				"StudyDateTo":     dateTo,
			}

			// Build one param set per selected modality so each query is a single
			// modality filter — PACS multi-value matching is unreliable across vendors.
			// Results are merged client-side and deduplicated by StudyInstanceUID.
			var paramSets []map[string]string
			if len(modalityCheck.Selected) == 0 {
				paramSets = []map[string]string{baseParams}
			} else {
				for _, mod := range modalityCheck.Selected {
					p := make(map[string]string, len(baseParams)+1)
					for k, v := range baseParams {
						p[k] = v
					}
					p["ModalitiesInStudy"] = mod
					paramSets = append(paramSets, p)
				}
			}

			if cancelQuery != nil {
				cancelQuery()
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancelQuery = cancel

			go func() {
				defer cancel()

				type queryResult struct {
					results []FindResult
					err     error
				}
				resultsCh := make(chan queryResult, len(paramSets))

				var wg sync.WaitGroup
				for _, params := range paramSets {
					wg.Add(1)
					go func(p map[string]string) {
						defer wg.Done()
						ch, err := client.Find(ctx, activeProfile.InfoModel, p)
						if err != nil {
							resultsCh <- queryResult{err: err}
							return
						}
						var results []FindResult
						var findErr error
						for r := range ch {
							if r.Err != nil {
								findErr = r.Err
								break
							}
							results = append(results, r)
						}
						for range ch {
						} // drain so Find's goroutine can exit
						resultsCh <- queryResult{results: results, err: findErr}
					}(params)
				}
				go func() { wg.Wait(); close(resultsCh) }()

				seen := make(map[string]bool)
				var allResults []FindResult
				var firstErr error
				for qr := range resultsCh {
					if qr.err != nil {
						if firstErr == nil {
							firstErr = qr.err
						}
						continue
					}
					for _, r := range qr.results {
						if !seen[r.StudyInstanceUID] {
							seen[r.StudyInstanceUID] = true
							allResults = append(allResults, r)
						}
					}
				}

				if firstErr != nil && len(allResults) == 0 {
					fyne.Do(func() {
						queryProgress.Hide()
						statusLabel.SetText("Query error: " + firstErr.Error())
					})
					return
				}
				fyne.Do(func() {
					total := len(allResults)
					for i, r := range allResults {
						model.addStudy(r.PatientName, r.PatientID, r.StudyInstanceUID,
							r.StudyDate, r.StudyDescription, r.AccessionNumber, r.ModalitiesInStudy)
						if (i+1)%10 == 0 {
							statusLabel.SetText(fmt.Sprintf("Loading results… %d/%d", i+1, total))
						}
					}
					model.applyFilter()
					tree.Refresh()
					queryProgress.Hide()
					statusLabel.SetText(fmt.Sprintf("Query complete — %d studies", total))
				})
			}()
		}

		noParams := patientNameEntry.Text == "" &&
			patientIDEntry.Text == "" &&
			accessionEntry.Text == "" &&
			studyDateFromEntry.Date == nil &&
			studyDateToEntry.Date == nil &&
			len(modalityCheck.Selected) == 0
		if noParams {
			dialog.ShowConfirm("Warning",
				"No search parameters have been specified.\n\nRunning an unconstrained query may retrieve a large number of results and place unnecessary load on the PACS server.\n\nDo you want to proceed?",
				func(ok bool) {
					if ok {
						runSearch()
					}
				}, w)
			return
		}
		runSearch()
	}

	doClearQuery := func() {
		if cancelQuery != nil {
			cancelQuery()
		}
		patientNameEntry.SetText("")
		patientIDEntry.SetText("")
		accessionEntry.SetText("")
		studyDateFromEntry.Validator = nil
		studyDateFromEntry.SetDate(nil)
		studyDateToEntry.Validator = nil
		studyDateToEntry.SetDate(nil)
		modalityCheck.SetSelected(nil)
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
		setState(s)
		fyne.Do(func() {
			switch s {
			case stateDisconnected:
				connectBtn.Enable()
				disconnectBtn.SetText("Disconnect")
				disconnectBtn.Disable()
				echoBtn.Disable()
				searchBtn.Disable()
				searchTopBtn.Disable()
			case stateConnected:
				connectBtn.Disable()
				disconnectBtn.SetText("Disconnect")
				disconnectBtn.Enable()
				echoBtn.Enable()
				searchBtn.Enable()
				searchTopBtn.Enable()
			case stateBusy:
				connectBtn.Disable()
				disconnectBtn.SetText("Cancel")
				disconnectBtn.Enable()
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

		timeout := time.Duration(profile.ConnectTimeout) * time.Second
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		cancelConnect = cancel

		go func() {
			defer cancel()
			c := NewDicomClient(profile, cfg.LocalAETitle)
			if err := c.Echo(ctx); err != nil {
				if ctx.Err() != nil {
					setConnState(stateDisconnected, "Connection cancelled")
				} else {
					setConnState(stateDisconnected, "Connection failed: "+err.Error())
				}
				return
			}
			if ctx.Err() != nil {
				setConnState(stateDisconnected, "Connection cancelled")
				return
			}
			client = c
			activeProfile = profile

			s := NewStorageSCP(cfg.LocalAETitle, cfg.LocalSCPPort, cfg.DownloadDir)
			if err := s.Start(); err != nil {
				setConnState(stateDisconnected, "SCP error: "+err.Error())
				return
			}
			scp = s
			connCtx, cancelConn = context.WithCancel(context.Background())

			setConnState(stateConnected, fmt.Sprintf("Connected: %s@%s:%d  |  SCP %s (AE: %s)",
				profile.RemoteAETitle, profile.Host, profile.Port, s.ListenAddr(), cfg.LocalAETitle))
		}()
	}

	disconnectBtn.OnTapped = func() {
		if cancelConnect != nil {
			cancelConnect()
			cancelConnect = nil
		}
		if cancelQuery != nil {
			cancelQuery()
		}
		if cancelConn != nil {
			cancelConn()
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
			container.NewHBox(widget.NewLabel("Server:"), profileSelect, filtersBtn, searchTopBtn),
			container.NewHBox(connectBtn, disconnectBtn, echoBtn),
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
				container.New(layout.NewFormLayout(),
					widget.NewLabel("Patient Name"), patientNameEntry,
					widget.NewLabel("Patient ID"), patientIDEntry,
					widget.NewLabel("Accession No"), accessionEntry,
					widget.NewLabel("Study Date From"), studyDateFromEntry,
					widget.NewLabel("Study Date To"), studyDateToEntry,
					widget.NewLabel("Modality"), modalityCheck,
				),
				container.NewHBox(layout.NewSpacer(), searchBtn, clearBtn),
			)
			searchPopup = widget.NewModalPopUp(container.NewPadded(popupContent), w.Canvas())
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
			scp.SetDownloadDir(dir)
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

	openFolderBtn := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() {
		dir := cfg.DownloadDir
		if dir == "" {
			dialog.ShowInformation("No folder set", "Select a download folder first.", w)
			return
		}
		go exec.Command("explorer", dir).Start()
	})

	var cancelRetrieve context.CancelFunc

	type retrieveTarget struct {
		level     string // "STUDY" or "SERIES"
		patientID string
		studyUID  string
		seriesUID string
	}

	// startRetrieveTargets runs the retrieve loop for an already-resolved target
	// list. Extracted so the retry dialog can re-invoke it with only the failed
	// targets without duplicating the full retrieve loop (Phase 4-E).
	var startRetrieveTargets func(targets []retrieveTarget)
	startRetrieveTargets = func(targets []retrieveTarget) {
		if getState() != stateConnected || client == nil {
			dialog.ShowInformation("Not connected", "Connect to a DICOM server first.", w)
			return
		}
		method := activeProfile.RetrieveMethod
		if method == "" {
			method = "MOVE"
		}
		if method == "MOVE" || method == "AUTO" {
			if scp == nil || !scp.IsRunning() {
				dialog.ShowInformation("SCP not running",
					fmt.Sprintf("The local C-STORE SCP is not listening.\n\nDisconnect and reconnect to restart it.\nExpected port: %d  AE title: %s", cfg.LocalSCPPort, cfg.LocalAETitle), w)
				return
			}
		}

		count := len(targets)
		log.Printf("retrieve: %d targets, destAE=%s port=%d", count, cfg.LocalAETitle, cfg.LocalSCPPort)
		for i, t := range targets {
			log.Printf("  target[%d]: level=%s patientID=%s studyUID=%s seriesUID=%s", i, t.level, t.patientID, t.studyUID, t.seriesUID)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancelRetrieve = cancel
		progressBar.SetValue(0)
		progressBar.Show()
		startNoun := "studies"
		if count == 1 {
			if targets[0].level == "SERIES" {
				startNoun = "series"
			} else {
				startNoun = "study"
			}
		}
		statusLabel.SetText(fmt.Sprintf("Starting retrieve of %d %s…", count, startNoun))

		go func() {
			defer cancel()

			var fileCount int64

			// For C-MOVE: intercept scp.OnFileReceived to count and report files.
			var origOnFileReceived func(string)
			if scp != nil && (method == "MOVE" || method == "AUTO") {
				origOnFileReceived = scp.OnFileReceived()
				scp.SetOnFileReceived(func(path string) {
					atomic.AddInt64(&fileCount, 1)
					if ctx.Err() == nil {
						fyne.Do(func() { statusLabel.SetText("Received: " + path) })
					}
				})
			}
			restoreSCP := func() {
				if scp != nil {
					scp.SetOnFileReceived(origOnFileReceived)
				}
			}

			// For C-GET: callback writes each received instance to the download folder.
			getCallback := func(txUID, scUID, siUID string, data []byte) error {
				path, saveErr := saveGetFile(cfg.DownloadDir, txUID, scUID, siUID, data)
				if saveErr != nil {
					log.Printf("c-get: save file: %v", saveErr)
					return saveErr
				}
				atomic.AddInt64(&fileCount, 1)
				if ctx.Err() == nil {
					fyne.Do(func() { statusLabel.SetText("Received: " + path) })
				}
				return nil
			}

			var cancelled bool
			var errCount int
			var failed []retrieveTarget
			for i, tgt := range targets {
				if ctx.Err() != nil {
					cancelled = true
					break
				}
				idx := i + 1
				label := "study"
				if tgt.level == "SERIES" {
					label = "series"
				}
				fyne.Do(func() {
					statusLabel.SetText(fmt.Sprintf("Retrieving %s %d/%d…", label, idx, count))
				})

				var err error
				switch method {
				case "GET":
					err = client.Get(ctx, tgt.level, tgt.patientID, tgt.studyUID, tgt.seriesUID, getCallback)
				case "AUTO":
					err = client.Get(ctx, tgt.level, tgt.patientID, tgt.studyUID, tgt.seriesUID, getCallback)
					if err != nil && ctx.Err() == nil {
						log.Printf("retrieve: c-get failed (%v), falling back to c-move", err)
						err = client.Move(ctx, tgt.level, tgt.patientID, tgt.studyUID, tgt.seriesUID, cfg.LocalAETitle, func(p MoveProgress) {
							sub := p.Remaining + p.Completed + p.Failed + p.Warning
							if sub > 0 {
								frac := (float64(i) + float64(p.Completed)/float64(sub)) / float64(count)
								fyne.Do(func() { progressBar.SetValue(frac) })
							}
						})
					}
				default: // "MOVE"
					err = client.Move(ctx, tgt.level, tgt.patientID, tgt.studyUID, tgt.seriesUID, cfg.LocalAETitle, func(p MoveProgress) {
						sub := p.Remaining + p.Completed + p.Failed + p.Warning
						if sub > 0 {
							frac := (float64(i) + float64(p.Completed)/float64(sub)) / float64(count)
							fyne.Do(func() { progressBar.SetValue(frac) })
						}
					})
				}

				if err != nil {
					if ctx.Err() != nil {
						cancelled = true
						break
					}
					log.Printf("retrieve: %s %d/%d error (continuing): %v", label, idx, count, err)
					errCount++
					failed = append(failed, tgt)
				}
			}

			restoreSCP()
			n := atomic.LoadInt64(&fileCount)
			fyne.Do(func() {
				progressBar.Hide()
				switch {
				case cancelled:
					statusLabel.SetText("Retrieve cancelled")
				case errCount > 0:
					statusLabel.SetText(fmt.Sprintf("Retrieved %d files (%d/%d targets had errors — see log)", n, errCount, count))
					// Offer to retry only the failed targets (Phase 4-E).
					dialog.ShowConfirm("Retrieve errors",
						fmt.Sprintf("%d of %d targets failed.\nRetry failed targets only?", len(failed), count),
						func(ok bool) {
							if ok {
								startRetrieveTargets(failed)
							}
						}, w)
				default:
					statusLabel.SetText(fmt.Sprintf("Retrieved %d files successfully", n))
				}
			})
		}()
	}

	startRetrieve = func(nodeIDs []string) {
		if getState() != stateConnected || client == nil {
			dialog.ShowInformation("Not connected", "Connect to a DICOM server first.", w)
			return
		}

		// Collect retrieve targets from the supplied node IDs.
		// Patient → all study children at STUDY level.
		// Study   → STUDY level.
		// Series  → SERIES level, unless its parent study is also in the list.
		studySeen := make(map[string]bool)
		var targets []retrieveTarget
		var pendingSeries []retrieveTarget
		for _, id := range nodeIDs {
			patID, studyUID, seriesUID, _ := model.uidsForNode(id)
			switch {
			case studyUID == "":
				// Patient node — expand to study children.
				for _, childID := range model.childUIDs(id) {
					cp, cs, _, _ := model.uidsForNode(childID)
					if cs != "" && !studySeen[cs] {
						studySeen[cs] = true
						targets = append(targets, retrieveTarget{"STUDY", cp, cs, ""})
					}
				}
			case seriesUID == "":
				// Study node.
				if !studySeen[studyUID] {
					studySeen[studyUID] = true
					targets = append(targets, retrieveTarget{"STUDY", patID, studyUID, ""})
				}
			default:
				// Series node — defer until we know which studies are in the list.
				pendingSeries = append(pendingSeries, retrieveTarget{"SERIES", patID, studyUID, seriesUID})
			}
		}
		// Add series targets only when their parent study was not included directly.
		for _, t := range pendingSeries {
			if !studySeen[t.studyUID] {
				targets = append(targets, t)
			}
		}
		if len(targets) == 0 {
			dialog.ShowInformation("Nothing selected", "Click one or more studies or series in the results list first.", w)
			return
		}
		startRetrieveTargets(targets)
	}

	retrieveBtn := widget.NewButton("Retrieve Selected", func() {
		ids := make([]string, 0, len(selectedNodes))
		for id := range selectedNodes {
			ids = append(ids, id)
		}
		startRetrieve(ids)
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
			container.NewHBox(openFolderBtn, browseBtn),
			downloadDirEntry,
		),
		container.NewHBox(retrieveBtn, cancelRetrieveBtn),
	)

	// ── Search bar (filter above tree) ───────────────────────────────────────
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
	w.Canvas().AddShortcut(
		&desktop.CustomShortcut{KeyName: fyne.KeyR, Modifier: fyne.KeyModifierShortcutDefault},
		func(_ fyne.Shortcut) { retrieveBtn.OnTapped() },
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
		fyne.NewMenuItem("Export…", func() {
			if len(model.roots) == 0 {
				dialog.ShowInformation("No results", "Run a query first, then export.", w)
				return
			}
			formatSelect := widget.NewSelect([]string{"CSV", "JSON"}, nil)
			formatSelect.SetSelected("CSV")
			d := dialog.NewCustomConfirm("Export results", "Export", "Cancel",
				container.NewVBox(widget.NewLabel("Export format:"), formatSelect),
				func(ok bool) {
					if !ok {
						return
					}
					go func() {
						ext := strings.ToLower(formatSelect.Selected)
						path, err := sqweekdialog.File().Filter("Export file", ext).Save()
						if err != nil {
							return
						}
						if !strings.HasSuffix(strings.ToLower(path), "."+ext) {
							path += "." + ext
						}
						rows := model.exportRows()
						var writeErr error
						if formatSelect.Selected == "CSV" {
							writeErr = exportToCSV(path, rows)
						} else {
							writeErr = exportToJSON(path, rows)
						}
						fyne.Do(func() {
							if writeErr != nil {
								dialog.ShowError(writeErr, w)
							} else {
								dialog.ShowInformation("Export complete",
									fmt.Sprintf("Exported %d rows to:\n%s", len(rows), path), w)
							}
						})
					}()
				}, w)
			d.Show()
		}),
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
			iconImg := canvas.NewImageFromResource(appIcon)
			iconImg.FillMode = canvas.ImageFillContain
			iconImg.SetMinSize(fyne.NewSize(240, 240))

			// Top-left: key info + credits, wrapped to fit beside the icon.
			topLbl := widget.NewLabel(fmt.Sprintf(
				"dicomqr  v%s  (built %s)\n"+
					"DICOM Query/Retrieve client — query and retrieve studies from a PACS server.\n"+
					"Implements DICOM PS3.4/PS3.7: C-ECHO, C-FIND, C-MOVE, C-STORE SCP.\n\n"+
					"Developer\n"+
					"  Jeffrey Leal  <jeffrey.leal@gmail.com>\n"+
					"  https://github.com/jeffrey-leal\n\n"+
					"AI Assistance\n"+
					"  Claude Sonnet 4.6 by Anthropic  (https://anthropic.com)\n"+
					"  Architecture, code generation, and DICOM standard research.\n\n"+
					"DICOM Standard Reference\n"+
					"  DICOM PS3 (2024b) — https://dicom.nema.org/medical/dicom/current",
				version, bd))
			topLbl.TextStyle = fyne.TextStyle{Monospace: true}
			topLbl.Wrapping = fyne.TextWrapWord

			// Bottom: open-source library table, full dialog width.
			bottomLbl := widget.NewLabel(
				"Open-Source Libraries\n" +
					"  fyne.io/fyne/v2 v2.7.3              Fyne.io — GUI framework (BSD 3-Clause)\n" +
					"  github.com/algm/go-netdicom v0.1.0  Alan Griffin — DICOM networking (BSD 3-Clause)\n" +
					"  github.com/grailbio/go-netdicom     Yasushi Saito / GRAIL — base networking lib\n" +
					"  github.com/grailbio/go-dicom        GRAIL Inc. — DICOM encoding (Apache 2.0)\n" +
					"  github.com/suyashkumar/dicom        Suyash Kumar — DICOM parsing (MIT)\n" +
					"  github.com/sqweek/dialog            sqweek — native file dialogs (ISC)\n\n" +
					"Full credits: CREDITS.md in the project repository.")
			bottomLbl.TextStyle = fyne.TextStyle{Monospace: true}

			// Icon anchored to top of right column; VBox does not stretch items
			// downward, so the image sits at the top even when the left text is taller.
			iconCol := container.NewVBox(iconImg)
			topSection := container.NewBorder(nil, nil, nil, iconCol, topLbl)
			content := container.NewPadded(container.NewVBox(topSection, bottomLbl))
			d := dialog.NewCustom("About dicomqr", "OK", content, w)
			d.Resize(fyne.NewSize(720, 0))
			d.Show()
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
