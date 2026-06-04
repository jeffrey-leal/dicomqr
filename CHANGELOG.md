# Changelog

## [1.0.0] — 2026-06-03

Initial public release.

### Added
- **Per-profile retrieve method** — each server profile has a **Retrieve method** setting (C-MOVE / C-GET / Auto), configurable in **File > Preferences… > Connections > Edit**
- **C-GET retrieval** — C-GET transfers files back over the same association; no inbound C-STORE SCP port or PACS-side registration of a destination AE is required
- **Auto mode** — attempts C-GET first; falls back to C-MOVE automatically if the PACS rejects or does not support C-GET
- **Export results** — Query menu → **Export…** saves the current results tree to CSV or JSON; studies with no series loaded produce one row each, studies with series loaded produce one row per series
- **Series-level query and retrieve** — expanding a study fires a SERIES-level C-FIND and lets individual series be selected and retrieved
- **Multi-select modality filter** — modalities are ticked via checkboxes and each is dispatched as a concurrent single-modality C-FIND, merged and deduplicated client-side
- **Automatic wildcard search** — Patient Name, Patient ID, and Accession Number fields automatically append `*` on search when the value does not already end with one
- **Results tree sorting** — patients alphabetical by name; studies chronological by date; series numeric by series number
- **Select All / Clear Selection** — buttons in the retrieve panel; **Esc** also clears the current selection
- **Expand All / Collapse All** — buttons above the results tree
- **Customisable selection appearance** — **Preferences → UI** sets the colour and font style (bold / italic) applied to selected tree rows; an unset colour follows the theme's primary colour
- **Connect timeout** — per-profile **Connect timeout (s)** field (default 10 s) prevents indefinite hangs on unreachable servers
- **Cancel connect** — the **Disconnect** button becomes **Cancel** while connecting and aborts the in-flight C-ECHO
- **Retry failed retrieve targets** — after a partial retrieve a dialog offers to retry only the failed targets
- **Profile reordering** — Up/Down buttons in the Connections preference list reorder server profiles
- **Ctrl+R shortcut** — triggers Retrieve Selected (mirrors the menu item)
- **Progress reporting** — an indeterminate progress bar animates during C-FIND; the retrieve bar advances per target so C-GET (which carries no sub-operation count) also shows progress
- **Live loading count** — large result sets are inserted in batches across UI frames with a `Loading results… N/total` counter, keeping the window responsive
- **Window size persistence** — the window size is saved on exit and restored on the next launch
- **Download directory default** — defaults to `~/DICOM Downloads` when none is configured
- **App icon** — embedded in the Windows executable and shown in the **Help > About** dialog and the user manual title page
- **Filter debounce** — the results filter waits 150 ms after the last keystroke before re-running
- **Air-gapped IP detection** — `localIP()` enumerates network interfaces when the UDP probe to `8.8.8.8` fails
- **Inline server profile validation** — AE Title (required, ≤ 16 chars) and Port (1–65535) are validated in the server editor

### Changed
- **Results tree selection behaviour** — parent/child-aware: selecting a node selects all its loaded descendants; narrowing and per-child deselection behave intuitively; expanding a selected node auto-selects newly loaded children
- **Filter popup** — the Filters dropdown is a modal popup that auto-dismisses on outside click
- Results tree starts fully collapsed after each search; expanding a study triggers the series C-FIND

### Fixed
- **SCP port left locked on exit** — closing the window (X), File → Quit, and any other termination route now stop the embedded C-STORE listener and release its port; previously only the Disconnect button did, so the port could stay bound after the app closed and block a restart. `StorageSCP.Stop()` now closes the listener synchronously, and an app-stopped lifecycle hook acts as a final safety net. If the port is still in use when connecting (e.g. after an unclean kill that bypasses shutdown), the SCP retries the bind briefly and then reports a clear, actionable message ("port N is already in use — another copy of dicomqr may still be running…") in a dialog rather than a raw socket error
- **Connection-state data races** — the active client, C-STORE SCP, query context, and active profile are now guarded by a mutex, removing the race (and possible nil-deref) between disconnect on the UI goroutine and an in-flight retrieve
- **Download directory validated up front** — a retrieve now fails fast with a clear message if the download folder cannot be written, rather than erroring per received file
- **Filters popup validation error** — re-opening the Filters panel after a search no longer shows a spurious date-parse error on empty date fields
- **Cancel retrieve** — pressing Cancel reliably shows "Retrieve cancelled"; the C-STORE and C-GET callbacks check `ctx.Err()` before posting UI updates
- **Error 45056 (C-MOVE warning) recovery** — DICOM warning status 0xB000 is no longer treated as fatal; multi-series retrieves continue and the final status reports warning counts
- **Path-length guard** — each folder component is truncated to 64 characters; the path falls back to a flat `<downloadDir>/<sopInstanceUID>.dcm` layout when it would exceed 255 characters
- **Windows reserved device names** — path components matching `CON`, `NUL`, `PRN`, `AUX`, `COM1`–`COM9`, `LPT1`–`LPT9` are prefixed with `_`

### Internal
- Data-race fixes for `state`, `StorageSCP.downloadDir`, and `scp.OnFileReceived` (mutex-guarded)
- Atomic settings write (temp-file + `os.Rename`) to prevent settings corruption on crash
- `sort.Search`-based sorted insert replaces `sort.Slice`-on-every-insert (O(N log N) vs O(N² log N))
- `applyFilter` deferred out of the per-insert path — called once per batch
- `tree.RefreshItem(id)` replaces full `tree.Refresh()` after series lazy-load

---

## [0.1.2] — 2026-05-24

### Added
- **Series-level query and retrieve** — expanding a study node fires a
  C-FIND at SERIES level and populates series children on demand; individual
  series can be selected and retrieved via C-MOVE at SERIES level; mixed
  study + series selections are deduplicated automatically
- **Date picker controls** — Study Date From / To fields replaced with
  `widget.DateEntry` (calendar icon opens a month-view popup picker)
- **Multi-select modality filter** — single Select dropdown replaced with
  horizontal checkboxes; multiple modalities can be ticked simultaneously
- **Parallel modality queries** — when multiple modalities are selected each
  C-FIND is dispatched concurrently (`sync.WaitGroup`); results are merged
  and deduplicated client-side; multi-modality search time is now equal to
  a single-modality search rather than N × single search time
- **Open download folder button** — icon button in the retrieve panel opens
  the configured download folder in Windows Explorer
- **Right-click Retrieve** — results tree context menu now includes a
  Retrieve item that retrieves the right-clicked node directly, independent
  of the current selection
- **Credits** — About dialog and user manual Appendix C list the developer,
  AI assistance (Claude Sonnet 4.6 / Anthropic), and all open-source libraries

### Changed
- Results tree starts fully **collapsed** after each search; expanding a
  study node triggers the series C-FIND (previously all branches were
  auto-expanded, which prevented series from loading correctly)
- Cleared date fields no longer show a validation error

---

## [0.1.1] — 2026-05-22

### Added
- `CREDITS.md` — full attribution for developer, AI assistance, DICOM standard
  reference, and all open-source libraries used
- About dialog now displays complete credits including library versions,
  authors, and licences

### Changed
- Version bumped from 0.1.0 to 0.1.1

---

## [0.1.0] — 2026-05-20

### Added
- Initial scaffold based on dicomhdr project structure
- Fyne window layout: connection panel, filter bar, results tree,
  retrieve panel, status bar
- `resultsmodel.go` — tree data model for C-FIND results (Patient/Study/Series)
- `queryrow.go` — results tree row widget with hover tooltip and right-click menu
- `dicomnet.go` — `DicomClient` wrapping `algm/go-netdicom`:
  C-ECHO, C-FIND (study/series), C-MOVE with progress callbacks
- `storagescp.go` — embedded C-STORE SCP listener; writes received DICOM files
  to organised subfolder hierarchy; IPv4 explicit binding for Windows
- `settings.go` / `serverprofile.go` — JSON-persisted settings with embedded defaults
- `preferences.go` — theme, font, server profile, and retrieve settings dialog
- Lazy series loading — series C-FIND fired on study branch expand
- Multi-select retrieve — studies and series can be individually selected
  and retrieved in a single operation
- Unconstrained query guard — confirmation dialog before running an
  unrestricted C-FIND
- `dicom.log` written to `~/.dicomqr/` for protocol debugging
- GitHub repository: https://github.com/jeffrey-leal/dicomqr
