package main

import (
	"context"
	"fmt"
	"image/color"
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

const version = "1.1.0"

// LED colours for connection and SCP state indicators.
var (
	ledGray  = color.NRGBA{R: 0x88, G: 0x88, B: 0x88, A: 0xFF}
	ledAmber = color.NRGBA{R: 0xFF, G: 0xAA, B: 0x00, A: 0xFF}
	ledGreen = color.NRGBA{R: 0x00, G: 0xBB, B: 0x44, A: 0xFF}
	ledRed   = color.NRGBA{R: 0xCC, G: 0x22, B: 0x22, A: 0xFF}
)

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

	ensureDefaultSettings()
	cfg := loadSettings()

	// Restore the persisted window size, falling back to the default for fresh
	// installs or implausibly small saved values (Phase 5-2B).
	if cfg.WindowWidth > 200 && cfg.WindowHeight > 150 {
		w.Resize(fyne.NewSize(cfg.WindowWidth, cfg.WindowHeight))
	} else {
		w.Resize(fyne.NewSize(900, 650))
	}

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
		cancelQuery   context.CancelFunc // UI-goroutine only
		cancelConnect context.CancelFunc // UI-goroutine only
		connCtx       context.Context
		cancelConn    context.CancelFunc
		connMu        sync.Mutex // guards client, scp, activeProfile, connCtx, cancelConn

		// refreshLocalTree re-renders the Local Browse tab tree after preferences change.
		// Assigned once buildLocalBrowseContent is called during layout setup.
		refreshLocalTree = func() {}

		// refreshImportContent updates the Import tab destination label after preferences change.
		// Assigned once buildImportContent is called during layout setup.
		refreshImportContent = func() {}

		// refreshWorklist updates the Worklist tab's server profile dropdown after preferences change.
		// Assigned once buildWorklistContent is called during layout setup.
		refreshWorklist = func() {}
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

	// Thread-safe accessors for the active connection objects (Phase 5-1A).
	// These are written by the connect goroutine and read by the query, retrieve,
	// echo, and branch-open goroutines. disconnect nils them on the UI goroutine
	// while a retrieve may still be reading, so every access is guarded by connMu.
	getClient := func() *DicomClient {
		connMu.Lock()
		defer connMu.Unlock()
		return client
	}
	getSCP := func() *StorageSCP {
		connMu.Lock()
		defer connMu.Unlock()
		return scp
	}
	getActiveProfile := func() ServerProfile {
		connMu.Lock()
		defer connMu.Unlock()
		return activeProfile
	}
	getConnCtx := func() context.Context {
		connMu.Lock()
		defer connMu.Unlock()
		return connCtx
	}
	setConn := func(c *DicomClient, s *StorageSCP, p ServerProfile, ctx context.Context, cancel context.CancelFunc) {
		connMu.Lock()
		defer connMu.Unlock()
		client, scp, activeProfile, connCtx, cancelConn = c, s, p, ctx, cancel
	}
	// clearConn nils every connection field and returns the SCP and cancel func
	// so the caller can stop them outside the lock.
	clearConn := func() (*StorageSCP, context.CancelFunc) {
		connMu.Lock()
		defer connMu.Unlock()
		s, cancel := scp, cancelConn
		client, scp, activeProfile, connCtx, cancelConn = nil, nil, ServerProfile{}, nil, nil
		return s, cancel
	}

	// shutdownSCP stops the embedded C-STORE listener and releases its port. It
	// must run on every termination path (window close, Quit menu, app-stopped
	// lifecycle hook) so the SCP never outlives the app and holds the port against
	// a restart (Phase 5-2F). It is safe to call multiple times — clearConn
	// returns a nil SCP after the first call.
	shutdownSCP := func() {
		if s, cancel := clearConn(); s != nil {
			if cancel != nil {
				cancel()
			}
			s.Stop()
		}
	}

	// ── Status bar ──────────────────────────────────────────────────────────
	statusLabel := widget.NewLabel("v" + version)
	clockLabel := widget.NewLabel("")
	queryProgress := widget.NewProgressBarInfinite()
	queryProgress.Hide()
	progressBar := widget.NewProgressBar()
	progressBar.Hide()

	// connLED is the small coloured square preceding the status text.
	// gray = disconnected, amber = connecting, green = connected.
	connLED := canvas.NewRectangle(ledGray)
	connLED.SetMinSize(fyne.NewSize(12, 12))

	// scpLED and scpStatusLbl show the embedded C-STORE SCP state in the connection panel.
	scpLED := canvas.NewRectangle(ledGray)
	scpLED.SetMinSize(fyne.NewSize(12, 12))
	scpStatusLbl := widget.NewLabel("SCP: not running")

	setStatus := func(msg string) { fyne.Do(func() { statusLabel.SetText(msg) }) }

	go func() {
		for {
			fyne.Do(func() { clockLabel.SetText(time.Now().Format("2006-01-02  15:04:05")) })
			time.Sleep(time.Second)
		}
	}()

	statusBar := container.NewVBox(
		container.NewHBox(connLED, statusLabel, layout.NewSpacer(), clockLabel),
		queryProgress,
		progressBar,
	)

	// ── Results tree ────────────────────────────────────────────────────────
	model := newResultsModel()
	selectedNodes := make(map[string]bool)

	// tree is declared here so the selection helpers can reference it before
	// widget.NewTree returns.
	var tree *widget.Tree

	// clearSubtree removes id and every loaded descendant from selectedNodes.
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

	// selectSubtree adds id and every loaded descendant to selectedNodes.
	var selectSubtree func(string)
	selectSubtree = func(id string) {
		selectedNodes[id] = true
		tree.RefreshItem(id)
		for _, child := range model.childUIDs(id) {
			selectSubtree(child)
		}
	}

	// nodeOrAncestorSelected reports whether id or any of its ancestors is selected.
	nodeOrAncestorSelected := func(id string) bool {
		for cur := id; cur != ""; cur = model.parentOf(cur) {
			if selectedNodes[cur] {
				return true
			}
		}
		return false
	}

	onTapped := func(id string) {
		// Find the outermost selected ancestor (if any).
		topAncestor := ""
		for anc := model.parentOf(id); anc != ""; anc = model.parentOf(anc) {
			if selectedNodes[anc] {
				topAncestor = anc
			}
		}

		if topAncestor != "" && selectedNodes[id] {
			// Node is selected and an ancestor is also selected (node was
			// auto-selected when the parent was chosen). The user wants to
			// deselect just this node: clear it and its loaded descendants,
			// then deselect every ancestor up to and including topAncestor.
			clearSubtree(id)
			for anc := model.parentOf(id); anc != ""; anc = model.parentOf(anc) {
				if selectedNodes[anc] {
					delete(selectedNodes, anc)
					tree.RefreshItem(anc)
				}
				if anc == topAncestor {
					break
				}
			}
			return
		}

		if topAncestor != "" {
			// Node is unselected but an ancestor is selected. Narrow the
			// selection down to just this node's subtree.
			clearSubtree(topAncestor)
			selectSubtree(id)
			return
		}

		if selectedNodes[id] {
			// Node is selected with no selected ancestors: toggle it off
			// together with all loaded descendants.
			clearSubtree(id)
			return
		}

		// Node is unselected with no selected ancestors: select it and all
		// loaded descendants.
		selectSubtree(id)
	}

	// selectAll selects every currently visible (filtered) root and its loaded
	// descendants; clearSelection drops the whole selection (Phase 5-2C).
	selectAll := func() {
		for _, id := range model.activeRoots() {
			selectSubtree(id)
		}
	}
	clearSelection := func() {
		if len(selectedNodes) == 0 {
			return
		}
		selectedNodes = make(map[string]bool)
		tree.Refresh()
	}

	// startRetrieve is assigned below after the retrieve variables are in scope.
	var startRetrieve func(nodeIDs []string)

	// openInViewer launches the configured external DICOM viewer with path as its
	// argument. path may be a file or a directory; most viewers accept both.
	openInViewer := func(path string) {
		vp := cfg.ViewerPath
		if vp == "" {
			dialog.ShowInformation("No viewer configured",
				"Set an external DICOM viewer in File → Preferences → Image Viewer.", w)
			return
		}
		go exec.Command(vp, path).Start()
	}

	onMenu := func(id string, pos fyne.Position) {
		_, studyUID, seriesUID, _ := model.uidsForNode(id)
		uid := seriesUID
		if uid == "" {
			uid = studyUID
		}
		retrieveItem := fyne.NewMenuItem("Retrieve", func() { startRetrieve([]string{id}) })
		copyUID := fyne.NewMenuItem("Copy UID", func() { w.Clipboard().SetContent(uid) })
		copyLabel := fyne.NewMenuItem("Copy label", func() { w.Clipboard().SetContent(model.labelFor(id)) })
		popup := widget.NewPopUpMenu(fyne.NewMenu("",
			retrieveItem,
			fyne.NewMenuItemSeparator(),
			copyUID, copyLabel,
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
				// Selected rows use the user-configured appearance (Phase 5-2E);
				// an empty SelectionColor follows the theme's primary colour.
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

	tree.OnBranchOpened = func(id string) {
		if !strings.HasPrefix(id, "S:") || model.isSeriesLoaded(id) {
			return
		}
		// Patient/Study Only information model has no SERIES level — suppress lazy load.
		if getActiveProfile().InfoModel == "patient-study-only" {
			return
		}
		model.markSeriesLoaded(id) // mark before goroutine to prevent duplicate queries
		_, studyUID, _, _ := model.uidsForNode(id)
		go func() {
			cctx := getConnCtx()
			cl := getClient()
			if cctx == nil || cl == nil {
				return
			}
			ch, err := cl.Find(cctx, "SERIES", map[string]string{"StudyInstanceUID": studyUID})
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
				// Auto-select newly loaded series if the study or any ancestor is selected.
				if nodeOrAncestorSelected(id) {
					for _, child := range model.childUIDs(id) {
						selectedNodes[child] = true
					}
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
		if getState() != stateConnected || getClient() == nil {
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

				cl := getClient()
				prof := getActiveProfile()
				if cl == nil {
					fyne.Do(func() {
						queryProgress.Hide()
						statusLabel.SetText("Not connected")
					})
					return
				}

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
						ch, err := cl.Find(ctx, prof.InfoModel, p)
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
				// Insert results in batches across successive UI frames so the
				// window stays responsive on large result sets and the live count
				// actually paints; inserting everything in one fyne.Do would freeze
				// the UI thread for the whole batch (Phase 5-1C).
				total := len(allResults)
				const insertBatch = 200
				for start := 0; start < total; start += insertBatch {
					end := start + insertBatch
					if end > total {
						end = total
					}
					batch, shown := allResults[start:end], end
					fyne.Do(func() {
						for _, r := range batch {
							model.addStudy(r.PatientName, r.PatientID, r.StudyInstanceUID,
								r.StudyDate, r.StudyDescription, r.AccessionNumber, r.ModalitiesInStudy)
						}
						statusLabel.SetText(fmt.Sprintf("Loading results… %d/%d", shown, total))
						tree.Refresh()
					})
					time.Sleep(10 * time.Millisecond) // yield so the UI can paint between batches
				}
				fyne.Do(func() {
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
				connLED.FillColor = ledGray
				connectBtn.Enable()
				disconnectBtn.SetText("Disconnect")
				disconnectBtn.Disable()
				echoBtn.Disable()
				searchBtn.Disable()
				searchTopBtn.Disable()
			case stateConnected:
				connLED.FillColor = ledGreen
				connectBtn.Disable()
				disconnectBtn.SetText("Disconnect")
				disconnectBtn.Enable()
				echoBtn.Enable()
				searchBtn.Enable()
				searchTopBtn.Enable()
			case stateBusy:
				connLED.FillColor = ledAmber
				connectBtn.Disable()
				disconnectBtn.SetText("Cancel")
				disconnectBtn.Enable()
				echoBtn.Disable()
			}
			connLED.Refresh()
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

			s := NewStorageSCP(cfg.LocalAETitle, cfg.LocalSCPPort, cfg.DownloadDir)
			if err := s.Start(); err != nil {
				fyne.Do(func() {
					scpLED.FillColor = ledRed
					scpLED.Refresh()
					scpStatusLbl.SetText("SCP: error — " + err.Error())
				})
				setConnState(stateDisconnected, "SCP error: "+err.Error())
				// Show the full message in a dialog — the status bar truncates the
				// actionable "port in use" guidance.
				fyne.Do(func() { dialog.ShowError(err, w) })
				return
			}
			fyne.Do(func() {
				scpLED.FillColor = ledGreen
				scpLED.Refresh()
				scpStatusLbl.SetText(fmt.Sprintf("SCP: listening on %s (AE: %s)", s.ListenAddr(), cfg.LocalAETitle))
			})
			cctx, cancelC := context.WithCancel(context.Background())
			setConn(c, s, profile, cctx, cancelC)

			setConnState(stateConnected, fmt.Sprintf("Connected: %s@%s:%d",
				profile.RemoteAETitle, profile.Host, profile.Port))
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
		s, cancelC := clearConn()
		if cancelC != nil {
			cancelC()
		}
		if s != nil {
			s.Stop()
		}
		scpLED.FillColor = ledGray
		scpLED.Refresh()
		scpStatusLbl.SetText("SCP: not running")
		setConnState(stateDisconnected, "Disconnected")
	}

	echoBtn.OnTapped = func() {
		cl := getClient()
		if cl == nil {
			return
		}
		go func() {
			if err := cl.Echo(context.Background()); err != nil {
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
		container.NewHBox(scpLED, scpStatusLbl),
		widget.NewSeparator(),
	)

	filtersBtn.OnTapped = func() {
		if searchPopup != nil && searchPopup.Visible() {
			searchPopup.Hide()
			return
		}
		if searchPopup == nil {
			closeBtn := widget.NewButton("Close", func() { searchPopup.Hide() })
			popupContent := container.NewVBox(
				container.New(layout.NewFormLayout(),
					widget.NewLabel("Patient Name"), patientNameEntry,
					widget.NewLabel("Patient ID"), patientIDEntry,
					widget.NewLabel("Accession No"), accessionEntry,
					widget.NewLabel("Study Date From"), studyDateFromEntry,
					widget.NewLabel("Study Date To"), studyDateToEntry,
					widget.NewLabel("Modality"), modalityCheck,
				),
				container.NewHBox(layout.NewSpacer(), searchBtn, clearBtn, closeBtn),
			)
			searchPopup = widget.NewModalPopUp(container.NewPadded(popupContent), w.Canvas())
		}
		pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(connPanel)
		searchPopup.ShowAtPosition(fyne.NewPos(pos.X, pos.Y+connPanel.Size().Height))
	}

	// ── Retrieve panel ───────────────────────────────────────────────────────
	// Download folder is configured in Preferences; displayed here read-only.
	folderLabel := widget.NewLabel(cfg.DownloadDir)
	folderLabel.Truncation = fyne.TextTruncateEllipsis

	openFolderBtn := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() {
		if cfg.DownloadDir == "" {
			return
		}
		go exec.Command("explorer", cfg.DownloadDir).Start()
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
		cl := getClient()
		sc := getSCP()
		if getState() != stateConnected || cl == nil {
			dialog.ShowInformation("Not connected", "Connect to a DICOM server first.", w)
			return
		}
		method := getActiveProfile().RetrieveMethod
		if method == "" {
			method = "MOVE"
		}
		if method == "MOVE" || method == "AUTO" {
			if sc == nil || !sc.IsRunning() {
				dialog.ShowInformation("SCP not running",
					fmt.Sprintf("The local C-STORE SCP is not listening.\n\nDisconnect and reconnect to restart it.\nExpected port: %d  AE title: %s", cfg.LocalSCPPort, cfg.LocalAETitle), w)
				return
			}
		}

		// Fail fast if the download directory cannot be written (Phase 5-2D),
		// rather than surfacing a C-STORE error per received file.
		if err := dirWritable(cfg.DownloadDir); err != nil {
			dialog.ShowError(err, w)
			return
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

			// For C-MOVE: intercept sc.OnFileReceived to count and report files.
			var origOnFileReceived func(string)
			if sc != nil && (method == "MOVE" || method == "AUTO") {
				origOnFileReceived = sc.OnFileReceived()
				sc.SetOnFileReceived(func(path string) {
					atomic.AddInt64(&fileCount, 1)
					if ctx.Err() == nil {
						fyne.Do(func() { statusLabel.SetText("Received: " + path) })
					}
				})
			}
			restoreSCP := func() {
				if sc != nil {
					sc.SetOnFileReceived(origOnFileReceived)
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
					err = cl.Get(ctx, tgt.level, tgt.patientID, tgt.studyUID, tgt.seriesUID, getCallback)
				case "AUTO":
					err = cl.Get(ctx, tgt.level, tgt.patientID, tgt.studyUID, tgt.seriesUID, getCallback)
					if err != nil && ctx.Err() == nil {
						log.Printf("retrieve: c-get failed (%v), falling back to c-move", err)
						err = cl.Move(ctx, tgt.level, tgt.patientID, tgt.studyUID, tgt.seriesUID, cfg.LocalAETitle, func(p MoveProgress) {
							sub := p.Remaining + p.Completed + p.Failed + p.Warning
							if sub > 0 {
								frac := (float64(i) + float64(p.Completed)/float64(sub)) / float64(count)
								fyne.Do(func() { progressBar.SetValue(frac) })
							}
						})
					}
				default: // "MOVE"
					err = cl.Move(ctx, tgt.level, tgt.patientID, tgt.studyUID, tgt.seriesUID, cfg.LocalAETitle, func(p MoveProgress) {
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

				// Advance the bar per completed target. C-MOVE also updates it
				// finely via its progress callback above; this guarantees C-GET
				// (which carries no sub-operation count) still shows progress and
				// the bar reaches 100% on the final target (Phase 5-2A).
				frac := float64(idx) / float64(count)
				fyne.Do(func() { progressBar.SetValue(frac) })
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
		if getState() != stateConnected || getClient() == nil {
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

	openInViewerBtn := widget.NewButton("Open in Viewer", func() { openInViewer(cfg.DownloadDir) })
	if cfg.ViewerPath == "" {
		openInViewerBtn.Disable()
	}

	retrievePanel := container.NewVBox(
		widget.NewSeparator(),
		container.NewBorder(nil, nil,
			widget.NewLabel("Download folder:"),
			openFolderBtn,
			folderLabel,
		),
		container.NewHBox(
			retrieveBtn, cancelRetrieveBtn,
			openInViewerBtn,
			layout.NewSpacer(),
			widget.NewButton("Select All", selectAll),
			widget.NewButton("Clear Selection", clearSelection),
		),
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
	w.Canvas().AddShortcut(
		&desktop.CustomShortcut{KeyName: fyne.KeyEscape},
		func(_ fyne.Shortcut) { clearSelection() },
	)

	// ── Menus ─────────────────────────────────────────────────────────────────
	fileMenu := fyne.NewMenu("File",
		fyne.NewMenuItem("Connect", func() { connectBtn.OnTapped() }),
		fyne.NewMenuItem("Disconnect", func() { disconnectBtn.OnTapped() }),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Preferences…", func() {
			showPreferencesDialog(a, w, currentTheme, &cfg, func(updated Settings) {
				cfg = updated
				folderLabel.SetText(cfg.DownloadDir)
				if sc := getSCP(); sc != nil {
					sc.SetDownloadDir(cfg.DownloadDir)
				}
				if cfg.ViewerPath == "" {
					openInViewerBtn.Disable()
				} else {
					openInViewerBtn.Enable()
				}
				profileSelect.Options = profileNames()
				profileSelect.Refresh()
				tree.Refresh()
				refreshLocalTree()
				refreshImportContent()
				refreshWorklist()
			})
		}),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Quit", func() { saveSettings(cfg); shutdownSCP(); a.Quit() }),
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
		fyne.NewMenuItem("Activity Log…", func() { showLogDialog(w) }),
		fyne.NewMenuItemSeparator(),
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
			info := fmt.Sprintf("%14s: %s\n%14s: %d\n%14s: %s\n\nRegister these on your PACS to enable C-MOVE.",
				"Local AE Title", cfg.LocalAETitle,
				"Local SCP port", cfg.LocalSCPPort,
				"Local IP", localIP())
			lbl := widget.NewLabel(info)
			lbl.TextStyle = fyne.TextStyle{Monospace: true}
			d := dialog.NewCustom("Client Info", "Close", container.NewPadded(lbl), w)
			d.Show()
		}),
	)

	w.SetMainMenu(fyne.NewMainMenu(fileMenu, queryMenu, helpMenu))

	// ── Layout ────────────────────────────────────────────────────────────────
	pacsContent := container.NewBorder(
		container.NewVBox(connPanel, filterBar),
		retrievePanel,
		nil, nil,
		tree,
	)

	var localContent fyne.CanvasObject
	localContent, refreshLocalTree = buildLocalBrowseContent(a, w, &cfg, openInViewer)

	var importContent fyne.CanvasObject
	importContent, refreshImportContent = buildImportContent(a, w, &cfg)

	var worklistContent fyne.CanvasObject
	worklistContent, refreshWorklist = buildWorklistContent(w, &cfg)

	tabs := container.NewAppTabs(
		container.NewTabItem("PACS Query", pacsContent),
		container.NewTabItem("Worklist", worklistContent),
		container.NewTabItem("Local Browse", localContent),
		container.NewTabItem("Import", importContent),
	)

	w.SetContent(container.NewBorder(nil, statusBar, nil, nil, tabs))

	// Persist settings on close and stop the SCP so the port is released before
	// the window — and the app — closes. Window size is only updated when valid
	// (non-zero) so a minimised or off-screen close does not clobber the saved size.
	w.SetCloseIntercept(func() {
		sz := w.Canvas().Size()
		if sz.Width > 200 && sz.Height > 150 {
			cfg.WindowWidth = sz.Width
			cfg.WindowHeight = sz.Height
		}
		saveSettings(cfg)
		shutdownSCP()
		w.Close()
	})

	// Safety net: stop the SCP if the app terminates by any route that bypasses
	// the close intercept above (Phase 5-2F).
	a.Lifecycle().SetOnStopped(shutdownSCP)

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
	os.Rename(logPath, filepath.Join(dir, "dicom.log.1")) // rotate previous session; ignore error
	f, err := os.Create(logPath)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f, appLog))
	log.SetFlags(log.Ltime | log.Lmicroseconds)
}
