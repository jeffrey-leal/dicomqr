# Changelog

## [0.2.0] — 2026-06-03

### Added
- **Export results** — Query menu → **Export…** saves the current results tree to CSV or JSON; studies with no series loaded produce one row each, studies with series loaded produce one row per series
- **Connect timeout** — each server profile has a **Connect timeout (s)** field (default 10 s); prevents indefinite hangs on unreachable servers; the timeout can be overridden per profile in **File > Preferences… > Connections > Edit**
- **Cancel connect** — the **Disconnect** button changes to **Cancel** while a connection is in progress and cancels the in-flight C-ECHO; clicking it sets the app back to disconnected immediately
- **Retry failed retrieve targets** — when one or more targets error during a retrieve a confirmation dialog offers to retry only the failed targets without re-downloading the successful ones
- **Profile reordering** — Up/Down buttons in the Connections preference list reorder server profiles
- **Ctrl+R shortcut** — keyboard shortcut to trigger Retrieve Selected (mirrors the menu item)
- **Indeterminate progress bar during query** — a separate infinite-progress bar animates during C-FIND; the deterministic retrieve progress bar is hidden while querying
- **Live loading count** — the status bar updates every 10 studies while the results batch is being inserted (`Loading results… N/total`)
- **Download directory default** — if no download directory is configured the app defaults to `~/DICOM Downloads` instead of leaving the field blank
- **Filter debounce** — the results filter waits 150 ms after the last keystroke before re-running, preventing redundant redraws on fast typing
- **Air-gapped IP detection** — `localIP()` falls back to enumerating network interfaces when the UDP trick to `8.8.8.8` fails (no internet connection)
- **Inline server profile validation** — AE Title and Port fields in the server editor show inline error hints and block save if the values are invalid (AE title: required, ≤ 16 chars; Port: 1–65535)

### Changed
- **Filter popup** — the Filters dropdown is now a modal popup that auto-dismisses on outside click (was a non-modal `widget.PopUp`)
- **About dialog** — removed the "UI Template / dicomhdr" attribution section

### Fixed
- **Path-length guard** — each folder component in the download directory hierarchy is truncated to 64 characters; the full path falls back to a flat `<downloadDir>/<sopInstanceUID>.dcm` layout when it would exceed 255 characters
- **Windows reserved device names** — path components matching `CON`, `NUL`, `PRN`, `AUX`, `COM1`–`COM9`, `LPT1`–`LPT9` are prefixed with `_` to prevent Windows from rejecting the path

### Internal (Phases 1–2, shipped as 0.1.4/0.1.5)
- Data race fixes: `state`, `StorageSCP.downloadDir`, `scp.OnFileReceived` all guarded by mutexes
- Atomic settings write (temp-file + `os.Rename`) to prevent settings corruption on crash
- `sort.Search`-based sorted insert replaces `sort.Slice`-on-every-insert (O(N log N) vs O(N² log N))
- `applyFilter` deferred out of per-insert path — called once per batch
- `tree.RefreshItem(id)` replaces full `tree.Refresh()` after series lazy-load

---

## [0.1.3] — 2026-05-26

### Added
- **Per-profile retrieve method** — each server profile now has a **Retrieve method** setting (C-MOVE / C-GET / Auto), configurable in **File > Preferences… > Connections > Edit**
- **C-GET retrieval** — C-GET transfers files back over the same association; no inbound C-STORE SCP port or PACS-side registration of a destination AE is required
- **Auto mode** — attempts C-GET first; falls back to C-MOVE automatically if the PACS rejects or does not support C-GET
- **App icon** — application icon embedded in the Windows executable (shown in Explorer and the taskbar); icon also displayed in the **Help > About** dialog
- **App icon in user manual** — icon shown on the title page of the user manual
- **Automatic wildcard search** — Patient Name, Patient ID, and Accession Number fields automatically append `*` on search if the value does not already end with one; empty fields remain unconstrained
- **Results tree sorting** — patients sorted alphabetically by name; studies within a patient sorted chronologically by date; series within a study sorted numerically by series number

### Fixed
- **Filters popup validation error** — re-opening the Filters panel after a search no longer shows a spurious date-parse error on empty date fields; root cause was `widget.Form`'s validation machinery calling the broken `DateEntry` validator via its helper-text system; replaced with a plain `layout.NewFormLayout` container that has no validation hooks
- **Cancel retrieve** — pressing Cancel during a retrieve now reliably shows "Retrieve cancelled" and hides the progress bar; previously the status was immediately overwritten by "Received: …" messages because the C-STORE SCP callback and the C-GET callback continued queuing status updates after the context was cancelled; both callbacks now check `ctx.Err()` before posting a UI update, and the SCP callback is silenced on restore after any retrieve ends
- **Results tree click responsiveness** — selecting or deselecting a row now refreshes only that row (`tree.RefreshItem`) rather than redrawing the entire tree, eliminating the perceptible lag on click
- **Error 45056 (C-MOVE warning) recovery** — DICOM warning status 0xB000 ("Sub-operations Complete — One or more Failures") is no longer treated as a fatal error; multi-series retrieves continue to the next series rather than aborting; the final status message reports how many targets had warnings alongside the total file count

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
