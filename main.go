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
	"runtime/debug"
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
)

const version = "1.12.0"

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

// treeCollapseFix works around a virtualization bug in Fyne's widget.Tree
// (present in v2.7.3): the tree culls rows against its own private copy of the
// scroll offset, synced only by the scroller's OnScrolled callback — but when
// closing a branch shrinks the content, the scroller clamps or resets its
// offset through paths that never fire OnScrolled. The tree then culls rows
// that are actually inside the viewport, leaving the top of the tree blank
// until an unrelated event happens to resync it. A Refresh queued on the next
// main-loop turn (after the close cascade has settled and recomputed the
// content min size) re-clamps the offset through the notifying path and
// redraws the visible rows. Shared by every tree in the application.
func treeCollapseFix(tree *widget.Tree) {
	tree.OnBranchClosed = func(widget.TreeNodeID) {
		fyne.Do(tree.Refresh)
	}
}

// collapseAllTree collapses every branch, resetting the scroll offset first:
// CloseAllBranches bypasses OnBranchClosed (so treeCollapseFix never runs) and
// does not clamp the offset, so a viewport scrolled past the new (shorter)
// content would be left blank.
func collapseAllTree(tree *widget.Tree) {
	tree.ScrollToTop()
	tree.CloseAllBranches()
}

// armExitWatchdog guarantees the process terminates once shutdown has been
// requested. Fyne 2.7's quit path has a race: the run loop drains its function
// queue and only afterwards sets the "drained" flag, so a function posted with
// wait semantics in that gap is stranded — nothing ever runs it or signals its
// poster. The lifecycle event goroutine posts the OnStopped hook at exactly
// that moment; when it loses the race it blocks forever, WaitForEvents never
// completes, ShowAndRun never returns, and a windowless dicomqr.exe lingers in
// Task Manager holding the SCP port. The watchdog is armed only after
// settings, catalog, and SCP cleanup have finished, so the forced exit loses
// nothing; on a normal shutdown the process is gone before the timer fires.
func armExitWatchdog() {
	time.AfterFunc(3*time.Second, func() {
		logError("exit watchdog: shutdown wedged 3s after close — forcing process exit")
		os.Exit(0)
	})
}

func main() {
	dicomlog.SetLevel(2)
	setupLogFile()
	// Before any GL context exists: make sure this executable's NVIDIA driver
	// profile has Threaded Optimization off (see nvthreadctl.go — it corrupts
	// multi-window rendering during cine playback). No-op on other GPUs.
	disableNvidiaThreadedOptimization()
	a := app.NewWithID("com.jeffreyleal.dicomqr")
	a.SetIcon(appIcon)
	w := a.NewWindow("dicomqr")

	ensureDefaultSettings()
	ensureDefaultModifyConfigs()
	cfg := loadSettings()
	// Record the session's starting configuration so a log file alone can
	// answer "what was the app actually configured to do" after the fact.
	logInfo("settings: %d profile(s), download dir %s, local AE %s, SCP port %d",
		len(cfg.Profiles), cfg.DownloadDir, cfg.LocalAETitle, cfg.LocalSCPPort)

	// Persistent SQLite index of the download directory backing the Local
	// Browse tree. A nil catalog (open failure) degrades gracefully — every
	// catalog method is nil-safe and the tab falls back to in-memory scans.
	cat, catErr := openCatalog(cfg.DownloadDir)
	if catErr != nil {
		logError("catalog: open: %v", catErr)
		cat = nil
	}

	// Restore the persisted window size, falling back for fresh installs or
	// implausibly small saved values (Phase 5-2B) to a share of the screen —
	// see placeMainWindow, which also positions the window, something Fyne
	// itself cannot do. The Resize below still runs in the no-saved-size case
	// so the window is sensible even if the native placement cannot be made.
	haveSavedSize := cfg.WindowWidth > 200 && cfg.WindowHeight > 150
	if haveSavedSize {
		w.Resize(fyne.NewSize(cfg.WindowWidth, cfg.WindowHeight))
	} else {
		w.Resize(fyne.NewSize(900, 650))
	}
	// Restore the position the window was last closed at, or place it by the
	// default rule on a first run. Size is only imposed when the user has not
	// already chosen one by resizing in an earlier session.
	var savedPos *winPoint
	if cfg.WindowPosSaved {
		savedPos = &winPoint{X: cfg.WindowX, Y: cfg.WindowY}
	}
	a.Lifecycle().SetOnStarted(func() {
		placeMainWindow("dicomqr", savedPos, 0.60, 0.80, !haveSavedSize)
	})

	currentTheme := newAppTheme(cfg.DarkTheme, cfg.UITheme)
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

		// reloadLocalBrowse repopulates the Local Browse tree from the catalog
		// after downloads or imports add files. Safe to call from any goroutine.
		// Assigned once buildLocalBrowseContent is called during layout setup.
		reloadLocalBrowse = func() {}

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
	// setActiveProfileTransfer updates the connected profile's transfer syntax
	// requirement in place when Preferences change, so the next retrieve
	// follows the edited settings without a reconnect.
	setActiveProfileTransfer := func(transferSyntax string) {
		connMu.Lock()
		defer connMu.Unlock()
		activeProfile.TransferSyntax = transferSyntax
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

	// clockDone is closed on every termination path so the clock goroutine
	// stops calling fyne.Do before Fyne's event loop drains for the last time.
	// Without this, the goroutine can race against Fyne's shutdown, post to the
	// internal task queue after it has stopped being drained, and block—which
	// prevents ShowAndRun from returning and leaves the process in Task Manager.
	clockDone := make(chan struct{})
	var clockOnce sync.Once
	stopClock := func() { clockOnce.Do(func() { close(clockDone) }) }

	// Crash safety net: a panic on the main goroutine (all Fyne event handlers
	// run here) unwinds past ShowAndRun without reaching the OnStopped hook, so
	// none of the normal shutdown paths would run. Record the panic in the log
	// file first — in -H windowsgui release builds stderr is an invalid handle
	// and an unlogged panic vanishes without a trace — then release the SCP
	// port, close the catalog, and re-raise. Settings are deliberately not
	// saved here: state mid-panic is not trustworthy.
	defer func() {
		if r := recover(); r != nil {
			logError("FATAL: panic on main goroutine: %v\n%s", r, debug.Stack())
			stopClock()
			shutdownSCP()
			cat.Close()
			panic(r)
		}
	}()

	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-clockDone:
				return
			case t := <-tick.C:
				ts := t.Format("2006-01-02  15:04:05")
				fyne.Do(func() { clockLabel.SetText(ts) })
			}
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
		func(_ bool) fyne.CanvasObject { return newQueryRow(onTapped, onMenu) },
		func(id widget.TreeNodeID, _ bool, node fyne.CanvasObject) {
			row := node.(*queryRow)
			row.nodeID = id
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
	treeCollapseFix(tree)

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
			s.SetTransferPolicy(profile.requiredTransferSyntax())
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
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := cl.Echo(ctx); err != nil {
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
		prof := getActiveProfile()
		method := prof.RetrieveMethod
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
		logInfo("retrieve: %d targets, destAE=%s port=%d method=%s requiredTS=%q",
			count, cfg.LocalAETitle, cfg.LocalSCPPort, method, prof.requiredTransferSyntax())
		for i, t := range targets {
			logInfo("  target[%d]: level=%s patientID=%s studyUID=%s seriesUID=%s", i, t.level, t.patientID, t.studyUID, t.seriesUID)
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

			// Received file paths are collected so the catalog (and the Local
			// Browse tree) can be updated once the retrieve completes.
			var recvMu sync.Mutex
			var recvPaths []string
			recordPath := func(path string) {
				recvMu.Lock()
				recvPaths = append(recvPaths, path)
				recvMu.Unlock()
			}

			// Stall watchdog: some PACS servers' C-MOVE agents hang while
			// sending non-image objects (SR, PR, encapsulated PDF) — the
			// association stays open but no further data or progress response
			// ever arrives, leaving the retrieve stuck forever. Track the last
			// activity (progress responses and received files) and abort the
			// retrieve when the configured window passes in silence.
			stallTimeout := 120 * time.Second
			if cfg.RetrieveStallTimeoutSec > 0 {
				stallTimeout = time.Duration(cfg.RetrieveStallTimeoutSec) * time.Second
			}
			stallDetection := cfg.RetrieveStallTimeoutSec >= 0
			var lastActivity atomic.Int64
			lastActivity.Store(time.Now().UnixNano())
			touch := func() { lastActivity.Store(time.Now().UnixNano()) }
			var stalled atomic.Bool

			// The required transfer syntax is enforced without aborting the
			// retrieve. Objects that arrive but fail local conversion are
			// skipped by the receive path (Success status, logged, counted);
			// objects the server cannot deliver in any negotiated syntax show
			// up as failed sub-operation counts in the C-MOVE/C-GET progress
			// responses and are tracked here, reported at the end. Only hard
			// target errors (refused association, protocol failure) take the
			// per-target error path with its retry offer.
			requiredTS := prof.requiredTransferSyntax()
			var getConverted, getSkipped atomic.Int64
			scpConvBase, scpSkipBase := int64(0), int64(0)
			if sc != nil {
				scpConvBase = sc.ConvertedCount()
				scpSkipBase = sc.SkippedCount()
			}
			// srvFailedCur holds the in-flight target's latest failed-sub-op
			// count (counts are cumulative within one target); the loop folds
			// it into srvFailedTotal when the target finishes. Written from the
			// progress callbacks on the association goroutine, hence atomic.
			var srvFailedTotal int64
			var srvFailedCur atomic.Int64
			trackSubOpFailures := func(p MoveProgress) {
				if p.Failed > 0 {
					srvFailedCur.Store(int64(p.Failed))
				}
			}
			watchdogDone := make(chan struct{})
			defer close(watchdogDone)
			if stallDetection {
				go func() {
					ticker := time.NewTicker(5 * time.Second)
					defer ticker.Stop()
					for {
						select {
						case <-watchdogDone:
							return
						case <-ticker.C:
							idle := time.Duration(time.Now().UnixNano() - lastActivity.Load())
							if idle > stallTimeout {
								stalled.Store(true)
								logWarn("retrieve: no server activity for %v — aborting stalled retrieve", stallTimeout)
								cancel()
								return
							}
						}
					}
				}()
			}

			// For C-MOVE: intercept sc.OnFileReceived to count and report files.
			var origOnFileReceived func(string)
			if sc != nil && (method == "MOVE" || method == "AUTO") {
				origOnFileReceived = sc.OnFileReceived()
				sc.SetOnFileReceived(func(path string) {
					atomic.AddInt64(&fileCount, 1)
					recordPath(path)
					touch()
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
				path, converted, skippedFile, saveErr := saveGetFile(cfg.DownloadDir, txUID, scUID, siUID, data, requiredTS)
				if saveErr != nil {
					logError("c-get: save file: %v", saveErr)
					return saveErr
				}
				if skippedFile {
					// Unconvertible object — logged by saveGetFile. Report
					// success for the sub-operation so the retrieve continues.
					getSkipped.Add(1)
					return nil
				}
				if converted {
					getConverted.Add(1)
				}
				atomic.AddInt64(&fileCount, 1)
				recordPath(path)
				touch()
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
				touch() // each target gets a fresh stall window
				fyne.Do(func() {
					statusLabel.SetText(fmt.Sprintf("Retrieving %s %d/%d…", label, idx, count))
				})

				// One progress callback per target: feeds the stall watchdog,
				// the failed-sub-op tracker, and the fine-grained progress bar
				// (C-GET reports counts too via CGetWithProgress).
				onProg := func(p MoveProgress) {
					touch()
					trackSubOpFailures(p)
					sub := p.Remaining + p.Completed + p.Failed + p.Warning
					if sub > 0 {
						frac := (float64(i) + float64(p.Completed)/float64(sub)) / float64(count)
						fyne.Do(func() { progressBar.SetValue(frac) })
					}
				}

				var err error
				switch method {
				case "GET":
					err = cl.Get(ctx, tgt.level, tgt.patientID, tgt.studyUID, tgt.seriesUID, getCallback, onProg)
				case "AUTO":
					err = cl.Get(ctx, tgt.level, tgt.patientID, tgt.studyUID, tgt.seriesUID, getCallback, onProg)
					if err != nil && ctx.Err() == nil {
						logWarn("retrieve: c-get failed (%v), falling back to c-move", err)
						// The C-MOVE retry re-delivers the whole target; counts
						// from the failed C-GET attempt are superseded.
						srvFailedCur.Store(0)
						err = cl.Move(ctx, tgt.level, tgt.patientID, tgt.studyUID, tgt.seriesUID, cfg.LocalAETitle, onProg)
					}
				default: // "MOVE"
					err = cl.Move(ctx, tgt.level, tgt.patientID, tgt.studyUID, tgt.seriesUID, cfg.LocalAETitle, onProg)
				}

				// Fold the finished target's failed-sub-op count into the
				// running total; counts are cumulative within a target, so the
				// latest value is its final one.
				if f := srvFailedCur.Swap(0); f > 0 {
					srvFailedTotal += f
					logWarn("retrieve: server could not deliver %d object(s) for %s %d/%d — continuing",
						f, label, idx, count)
				}

				if err != nil {
					if ctx.Err() != nil {
						cancelled = true
						break
					}
					logError("retrieve: %s %d/%d error (continuing): %v", label, idx, count, err)
					errCount++
					failed = append(failed, tgt)
				}

				// Advance the bar per completed target. The progress callback
				// above also updates it finely for both C-MOVE and C-GET; this
				// guarantees the bar reaches 100% on the final target even when
				// a server sends no sub-operation counts (Phase 5-2A).
				frac := float64(idx) / float64(count)
				fyne.Do(func() { progressBar.SetValue(frac) })
			}

			restoreSCP()

			recvMu.Lock()
			received := recvPaths
			recvPaths = nil
			recvMu.Unlock()

			// Index received files and refresh the Local Browse tree in the
			// background so the final status message is not delayed. No batch
			// post-processing is needed: when a transfer syntax is required,
			// the receive path has already converted every file to it before
			// it reached its destination.
			if len(received) > 0 {
				go func() {
					cat.ingestPaths(received)
					reloadLocalBrowse()
				}()
			}

			n := atomic.LoadInt64(&fileCount)
			convertedTotal := getConverted.Load()
			if sc != nil {
				convertedTotal += sc.ConvertedCount() - scpConvBase
			}
			if convertedTotal > 0 {
				logInfo("retrieve: %d of %d file(s) arrived in a non-required syntax and were converted locally to %s",
					convertedTotal, n, requiredTS)
			}
			skippedTotal := getSkipped.Load()
			if sc != nil {
				skippedTotal += sc.SkippedCount() - scpSkipBase
			}
			if skippedTotal > 0 {
				logWarn("retrieve: %d object(s) could not be converted to %s and were skipped (not saved) — see the SKIPPED entries above for details",
					skippedTotal, requiredTS)
			}
			if srvFailedTotal > 0 {
				if requiredTS != "" {
					logError("retrieve: the server could not deliver %d object(s) in %s or any locally convertible syntax — not received (likely stored in a format without a built-in decoder, e.g. JPEG-LS, RLE, JPEG Lossless)",
						srvFailedTotal, requiredTS)
				} else {
					logWarn("retrieve: the server reported %d failed sub-operation(s)", srvFailedTotal)
				}
			}
			fyne.Do(func() {
				progressBar.Hide()
				switch {
				case cancelled && stalled.Load():
					statusLabel.SetText(fmt.Sprintf(
						"Retrieve stalled — no data from the server for %.0f s; %d file(s) received before the stall",
						stallTimeout.Seconds(), n))
					dialog.ShowInformation("Retrieve stalled",
						fmt.Sprintf("The server stopped sending data for %.0f seconds, so the retrieve was aborted.\n\n"+
							"%d file(s) were received before the stall.\n\n"+
							"Some PACS servers fail to deliver non-image objects (SR, Presentation State, "+
							"encapsulated PDF) via C-MOVE — their sender never finishes the transfer. "+
							"If this keeps happening on such series, set the profile's Retrieve method "+
							"to C-GET or Auto in Preferences.\n\n"+
							"If this server is simply slow (e.g. a tape archive), raise the "+
							"Retrieve stall timeout in File > Preferences… > SCP & Network.",
							stallTimeout.Seconds(), n), w)
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
				case n == 0 && srvFailedTotal > 0 && requiredTS != "":
					// Nothing arrived and every sub-operation failed server-side:
					// the server cannot deliver ANY object in the required syntax
					// or a convertible fallback. Continuing silently would look
					// like an empty study, so this one case stays loud.
					tsName := transferSyntaxLabel(requiredTS)
					statusLabel.SetText(fmt.Sprintf(
						"Retrieve failed — the server could not deliver any of %d object(s) in %s", srvFailedTotal, tsName))
					dialog.ShowError(fmt.Errorf(
						"The server could not deliver any object in the required transfer syntax.\n\n"+
							"Required: %s (%s)\n"+
							"Failed sub-operations: %d\n\n"+
							"The server could not send the data in the required syntax or in any format "+
							"this application can convert locally (JPEG Baseline/Extended, JPEG 2000) — "+
							"it may store the data in a format without a built-in decoder (e.g. JPEG-LS, RLE) "+
							"and be unable to transcode. To retrieve this data anyway, "+
							"set the profile's Transfer syntax to \"As stored (server decides)\" in Preferences.",
						tsName, requiredTS, srvFailedTotal), w)
				default:
					msg := fmt.Sprintf("Retrieved %d files successfully", n)
					if convertedTotal > 0 {
						// The server did not honour the required syntax for
						// these files; the receive path converted them.
						msg += fmt.Sprintf(" (%d converted locally to %s)",
							convertedTotal, transferSyntaxLabel(requiredTS))
					}
					if skippedTotal > 0 {
						msg += fmt.Sprintf(" — %d unconvertible object(s) skipped, see Activity Log", skippedTotal)
					}
					if srvFailedTotal > 0 {
						msg += fmt.Sprintf(" — %d not delivered by the server, see Activity Log", srvFailedTotal)
					}
					statusLabel.SetText(msg)
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
			widget.NewButton("Collapse All", func() { collapseAllTree(tree) }),
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
				// Re-apply the active profile's transfer syntax requirement to
				// the running SCP so a preference change takes effect on the
				// next retrieve without reconnecting.
				if sc := getSCP(); sc != nil {
					active := getActiveProfile()
					for i := range cfg.Profiles {
						if cfg.Profiles[i].Name == active.Name {
							p := cfg.Profiles[i]
							sc.SetTransferPolicy(p.requiredTransferSyntax())
							setActiveProfileTransfer(p.TransferSyntax)
							break
						}
					}
				}
				tree.Refresh()
				refreshLocalTree()
				refreshImportContent()
				refreshWorklist()
			})
		}),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Quit", func() {
			saveSettings(cfg)
			stopClock()
			shutdownSCP()
			cat.Close()
			armExitWatchdog()
			a.Quit()
		}),
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
		fyne.NewMenuItem("Activity Log…", func() { showLogDialog(a, w) }),
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
					"Architecture & Direction\n"+
					"  Jeffrey Leal  <jeffrey.leal@gmail.com>\n"+
					"  https://github.com/jeffrey-leal\n"+
					"  Program architecture, feature design, field testing, release decisions.\n\n"+
					"Implementation\n"+
					"  Claude by Anthropic  (https://anthropic.com)\n"+
					"  All application code and documentation, written via Claude Code\n"+
					"  to Jeffrey Leal's architecture and direction.\n\n"+
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
	localContent, refreshLocalTree, reloadLocalBrowse = buildLocalBrowseContent(a, w, &cfg, cat, openInViewer)

	var importContent fyne.CanvasObject
	importContent, refreshImportContent = buildImportContent(a, w, &cfg, cat, func() { reloadLocalBrowse() })

	var worklistContent fyne.CanvasObject
	worklistContent, refreshWorklist = buildWorklistContent(w, &cfg)

	tabs := container.NewAppTabs(
		container.NewTabItem("Local Browse", localContent),
		container.NewTabItem("PACS Query", pacsContent),
		container.NewTabItem("Import", importContent),
		container.NewTabItem("Worklist", worklistContent),
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
		// Remember where the user left the window. Fyne cannot report a
		// window position, so this comes from Windows; a failure just leaves
		// the previous value, and the next launch falls back to the default
		// placement rather than moving the window somewhere wrong.
		if r, ok := mainWindowRect("dicomqr"); ok {
			cfg.WindowX, cfg.WindowY = r.Left, r.Top
			cfg.WindowPosSaved = true
		}
		saveSettings(cfg)
		stopClock()
		shutdownSCP()
		cat.Close()
		armExitWatchdog()
		w.Close()
	})

	// Safety net: stop the SCP if the app terminates by any route that bypasses
	// the close intercept above (Phase 5-2F). Note this hook is queued during
	// the racy quit window described on armExitWatchdog and is not guaranteed
	// to run — it must only repeat cleanup already done elsewhere.
	a.Lifecycle().SetOnStopped(func() { stopClock(); shutdownSCP(); cat.Close(); armExitWatchdog() })

	w.ShowAndRun()
}

// setupLogFile redirects the standard log package output to the in-app
// Activity Log ring, stderr, and ~/.dicomqr/dicom.log so that DICOM protocol
// messages (from the grailbio dicomlog package) are captured even in
// windowsgui builds.
// failsafeWriter wraps a log sink so a write error is swallowed instead of
// propagating. io.MultiWriter stops at the first writer that errors — and in
// release builds (-s -w -H windowsgui, launched from Explorer) os.Stderr is an
// invalid handle whose every write fails, which would silently discard ALL
// log output before it reached the other sinks.
type failsafeWriter struct{ w io.Writer }

func (s failsafeWriter) Write(p []byte) (int, error) {
	s.w.Write(p)
	return len(p), nil
}

// fileLogSink appends every write to the log file by path, opening and closing
// the file per write. Field evidence (2026-07-23): sessions holding one long-
// lived handle produced log files containing only the session-start header —
// every later write vanished without an error surfacing. Reopening per write
// makes the sink stateless, so nothing that happens to a previous handle
// (rotation by a second app instance, antivirus interference, a recreated
// directory) can silently kill logging for the rest of the session: each line
// either lands or fails alone, and the first failure is reported to the
// Activity Log ring.
type fileLogSink struct {
	path     string
	failures atomic.Int64
}

func (s *fileLogSink) Write(p []byte) (int, error) {
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err == nil {
		_, err = f.Write(p)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil && s.failures.Add(1) == 1 {
		// Report straight to the ring — routing through the log package here
		// would recurse into this sink. Tagged [E] so the line surfaces at the
		// errors-only view level.
		appLog.Write([]byte("[E] dicom.log unwritable: " + err.Error()))
	}
	return len(p), nil
}

func setupLogFile() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	// The ring and stderr sinks are wired unconditionally: a home-directory
	// problem must never cost the Activity Log its output (previously the
	// whole redirect was skipped, leaving windowsgui builds with no logging
	// at all).
	sinks := []io.Writer{failsafeWriter{appLog}, failsafeWriter{os.Stderr}}
	logPath := "(unavailable)"
	if home, err := os.UserHomeDir(); err == nil {
		dir := filepath.Join(home, ".dicomqr")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			// Keep the two previous sessions so diagnosing a hang survives a
			// couple of app restarts: dicom.log.1 → dicom.log.2,
			// dicom.log → dicom.log.1.
			logPath = filepath.Join(dir, "dicom.log")
			os.Remove(filepath.Join(dir, "dicom.log.2"))
			os.Rename(filepath.Join(dir, "dicom.log.1"), filepath.Join(dir, "dicom.log.2"))
			os.Rename(logPath, filepath.Join(dir, "dicom.log.1"))
			// The file sink comes first so protocol evidence lands on disk
			// before anything else can interfere.
			sinks = append([]io.Writer{&fileLogSink{path: logPath}}, sinks...)
		}
	}
	log.SetOutput(io.MultiWriter(sinks...))
	logInfo("dicomqr v%s (build %s) session start — log: %s", version, buildDate, logPath)
}
