# Changelog

## [1.2.0] — 2026-06-17

### Added

- **Request uncompressed transfer syntax** — each server profile now has a "Request uncompressed transfer syntax only" checkbox in the profile editor. When enabled, dicomqr restricts the A-ASSOCIATE-RQ presentation contexts to Explicit VR Little Endian and Implicit VR Little Endian only, causing a conformant PACS to transcode compressed pixel data before delivery. Applies to both C-GET (SCU transfer syntax list) and C-MOVE (embedded C-STORE SCP rejects compressed syntaxes at association time with `PresentationContextProviderRejectionTransferSyntaxNotSupported`).

### Fixed

- **Image viewer: compressed pixel data error** — files using JPEG 2000 (Lossless/Lossy), JPEG-LS (Lossless/Near-Lossless), JPEG Lossless Non-Hierarchical, or RLE Lossless transfer syntaxes now display a clear, actionable message ("use Open in Viewer") instead of the cryptic "Invalid JPEG Format: missing SOI marker" error that appeared because the suyashkumar/dicom library unconditionally passes all encapsulated frames to `jpeg.Decode`
- **Image viewer: undefined-length uncompressed pixel data** — DICOM files from certain vendors that store uncompressed pixel data with an undefined-length VL are now decoded correctly. The suyashkumar/dicom library previously treated undefined-length pixel data as encapsulated (JPEG), causing the same JPEG decode failure on some systems. A raw-pixel fallback path now reinterprets the bytes natively using the image dimensions, bit depth, rescale parameters, and photometric interpretation from the dataset; this was the root cause of the "same data works on one system but not another" behaviour

### Internal

- Vendored `thirdparty/go-netdicom` extended: `ServiceProviderParams.AcceptedTransferSyntaxes []string` — when non-empty, the service provider selects only offered transfer syntaxes that appear in the list during A-ASSOCIATE-RQ negotiation, rejecting contexts where none match; `contextmanager.go` patched to iterate all offered syntaxes rather than blindly picking the first
- `viewer.go` — `unsupportedTransferSyntaxNames` map for proactive transfer syntax detection; `renderRawPixelFallback` for native pixel decode of misidentified encapsulated frames; `dicomIntParam` helper for integer dataset lookup

---

## [1.1.0] — 2026-06-17

### Added

- **Local Browse tab** — browse the configured download folder as a Patient > Study > Series tree; scan, filter, select, and act on local DICOM files without leaving the application; bottom bar provides Select All, Clear Selection, Push Selected, and Delete Selected
- **Import tab** — scan any folder for DICOM files; select studies or series and import them into the configured download folder, deduplicated and organised into the standard subfolder hierarchy
- **Worklist tab** — query any configured server as a Modality Worklist SCP (DICOM SOP class 1.2.840.10008.5.1.4.31, PS3.4 K.4) independently of the active PACS connection; has its own server profile selector; query fields: patient name, MRN, accession, modality, and scheduled date (calendar date picker with "Today only" shortcut); results shown in a table with Copy Accession and Copy Patient buttons
- **Internal image viewer** — Preview Images on a Local Browse series node opens a slider viewer with W/L windowing, rescale slope/intercept, and 1–99th-percentile auto-windowing for files without embedded window tags; opens at the middle slice of the series
- **Study overview window** — Preview Images on a Local Browse study node shows a grid of middle-slice thumbnails (one per series) loaded in parallel; double-clicking a thumbnail opens the full series viewer for that series; a hint label prompts the user to double-click
- **DICOM image annotation overlay** — viewer overlay shows patient identity (top-left), study context (top-right, right-aligned), series identity (bottom-left), and image geometry including W/L and slice location (bottom-right, right-aligned); anatomical orientation markers (R/L, A/P, H/F) centred on the four image edges, derived from `ImageOrientationPatient` direction cosines; all text constrained to the `FillContain` image area and never rendered into the letterbox bars
- **Annotation toggle** — Annotations checkbox in the viewer bottom bar shows or hides the overlay; state persists between sessions via Fyne application preferences
- **Push to PACS (C-STORE SCU)** — Local Browse right-click menu ("Push to PACS…") and "Push Selected…" bottom bar button send any selection of local DICOM files to any configured server profile via C-STORE SCU; progress dialog with per-file counter and cancel support
- **Delete local files** — Local Browse right-click menu ("Delete…") and "Delete Selected…" button permanently delete selected DICOM files after a confirmation dialog showing file count and total size; empty directories are pruned; the tree auto-rescans on completion
- **Connection status LED** — small coloured square (gray = disconnected, amber = connecting, green = connected) prepended to the status bar text shows connection state at a glance
- **SCP status indicator** — a second row in the connection panel shows the embedded C-STORE SCP state with its own LED (gray = not running, green = listening, red = error) and the listening address and AE Title; updates automatically when the SCP starts, errors, or is stopped
- **Activity log** — Help → Activity Log… opens a scrollable live view of the DICOM protocol log (last 500 lines); buttons: Refresh, Copy All, Clear; auto-refreshes once per second while the dialog is open; backed by a thread-safe ring buffer wired into the standard log package
- **Patient/Study Only info model** — server profiles now offer `patient-study-only` as a third Query/Retrieve information model alongside Study Root and Patient Root; suppresses the SERIES-level lazy-load on branch expand since that model does not support SERIES queries
- **External DICOM viewer integration** — Preferences → Image Viewer configures the path to an external DICOM viewer executable; Browse… and Auto-detect buttons are provided (auto-detect checks for MicroDicom and RadiAnt DICOM Viewer); "Open in Viewer" buttons open a folder in the configured viewer

### Changed

- **Main window layout** — content is now organised into four tabs: PACS Query, Worklist, Local Browse, Import
- **PACS Query right-click menu** — removed Preview Images and Open in Viewer; those actions are available in Local Browse where files are guaranteed to be on disk
- **PACS Query retrieve panel** — removed the Preview button for the same reason; after retrieving, scan the Local Browse tab to preview
- **Open in Viewer buttons and menu items** — disabled (not just dialog-blocked) when no external viewer path is configured; re-enabled immediately when a path is applied in Preferences
- **Local Browse preview** — Preview Images is disabled at the patient level (too broad to be useful); enabled at study (overview grid) and series (slider viewer) levels
- **Connected status bar text** — shortened to `Connected: <AE>@<host>:<port>`; SCP details moved to the dedicated SCP indicator row in the connection panel
- **Info model selector** — now offers three options: study, patient, patient-study-only

### Internal

- Vendored `thirdparty/go-netdicom` extended with `QRLevelPatientStudyOnly` (maps to retired DICOM SOP class 1.2.840.10008.5.1.4.1.2.3.x) and `QRLevelWorklist` (maps to 1.2.840.10008.5.1.4.31, omits `QueryRetrieveLevel` attribute from the C-FIND identifier dataset)
- `logcapture.go` — `logCapture` thread-safe 500-line ring buffer implementing `io.Writer`, wired into `io.MultiWriter` alongside the log file
- `viewer.go` — `imageAnnLayout` positions eight annotation objects within the FillContain image rect (not the widget bounds); `rightVBoxLayout` gives each line the container width so `TextAlignTrailing` on `canvas.Text` produces true right-alignment; `thumbnailCell` implements `fyne.DoubleTappable`

---

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
