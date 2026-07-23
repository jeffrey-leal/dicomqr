# Changelog

## [1.8.0] — 2026-07-23

### Changed

- **The transfer syntax requirement is now enforced in the receive path, per file, before it reaches disk** — requiring 'Explicit VR LE' or 'Implicit VR LE' in a server profile guarantees every retrieved file is stored in exactly that syntax. Negotiation (the C-GET proposal list and the C-MOVE storage SCP's accepted presentation-context set) offers the required syntax first — a transcoding-capable server sends it and nothing needs converting — followed by the syntaxes the application can convert locally (the other uncompressed VR, JPEG Baseline/Extended, and JPEG 2000 in openjpeg builds). A file arriving in any non-required accepted syntax is transcoded **while still a temp file**, so the download folder only ever holds conforming files — there is no post-retrieve batch pass, no fire-and-forget goroutine, and no window where a wrong-syntax file exists on disk. A failed conversion fails that C-STORE sub-operation, and any failed sub-operation or target error aborts the whole retrieve with a dialog reporting the required syntax, the failure details, and how many (all-conforming) files arrived before the abort. The completion status reports how many files needed local conversion, making a non-transcoding server visible at a glance. This replaces v1.7.0's 'preferred' negotiation and its post-retrieve 'Guarantee uncompressed on disk' pass, which could silently leave a study as a mix of syntaxes (observed 2026-07-23: ~half a study left as JPEG 2000 despite the guarantee, and study- vs series-level retrieves gave differing results)
- **'Guarantee uncompressed on disk' checkbox removed** — conversion is now intrinsic to requiring a syntax. The `ensureUncompressed` settings field is ignored on load; the pre-v1.7 `transferUncompressed` flag now migrates to `transferSyntax: "explicit-le"` alone
- **Re-retrieves replace syntax-mismatched copies** — with a required syntax set, an existing on-disk file whose transfer syntax differs from the requirement is replaced by the incoming (converted-if-needed) copy; matching or unrestricted files are skipped silently as before

### Fixed

- **`dicom.log` and the Activity Log were always empty in release builds** — the log output went through `io.MultiWriter(os.Stderr, file, ring)`, and in a `-H windowsgui` build launched from Explorer, `os.Stderr` is an invalid handle whose writes fail; `io.MultiWriter` stops at the first failing writer, so nothing ever reached the log file or the Activity Log dialog. Every sink is now wrapped in a failsafe writer (file first), verified end-to-end against a detached GUI launch. Each session starts with a version/build header line, and two previous sessions are kept (`dicom.log.1`, `dicom.log.2`) so evidence of a hang survives restarts
- **`dicom.log` no longer goes silent after the session-start line** — even with the failsafe writers, real sessions were observed (2026-07-23) whose log files contained only the header: every later write to the session's long-lived file handle vanished without an error surfacing, which destroyed the evidence needed to diagnose that day's transfer-syntax incident. The file sink no longer holds a handle at all — it reopens the log in append mode for every write, so nothing that invalidates a previous handle (a second instance's log rotation, antivirus interference, a recreated directory) can kill file logging for the rest of a session; write failures are counted and the first is reported to the Activity Log ring. The ring and stderr sinks are also now wired even when the home directory is unavailable (previously the whole redirect was skipped and windowsgui builds lost all logging). Sessions additionally log their starting configuration (profile count, download folder, AE title, SCP port), each retrieve's method and required transfer syntax, and every settings save — so a log file alone can reconstruct what the app was configured to do
- **Settings saves no longer fail silently** — `saveSettings` returned on any error without a trace, so a failed write left `settings.json` stale while the app behaved as configured (observed 2026-07-23: mid-session preference changes that never persisted). The atomic-rename step is now retried (3 × 150 ms) to ride out antivirus/indexer holds on the destination, every save logs its outcome, and a save failure from the Preferences dialog shows an error dialog stating that the settings apply to this session only
- **Negotiation visibility** — the log now records, for every association in both directions, each presentation context's offered transfer syntaxes and the chosen one, rejections with the local accepted set (always logged), and the storage SCP's active transfer policy per incoming association — enough to diagnose transfer-syntax negotiation problems from the log alone

- **C-GET retrieves of whole studies (≥124 instances) no longer wedge partway through** — the DIMSE dispatcher kept locally-initiated commands and peer-initiated commands in one map keyed by message ID alone. Our C-GET command takes message ID 124, and the PACS numbers its C-STORE sub-operations from 1 upward — so the 124th instance of a retrieve collided with the C-GET command itself and was mis-routed to the C-GET response loop instead of the C-STORE handler, stalling the transfer. Series-level retrieves stayed under the threshold, which is why only study/patient selections failed. Message IDs are scoped to the initiating AE (PS3.7), so the dispatcher now keeps peer-initiated commands in their own keyspace; responses route only to local commands, requests only to peer commands
- **C-GET received files were stamped with the wrong transfer syntax** — every file received over C-GET was labelled with the *query context's* negotiated transfer syntax instead of the storage context the instance actually arrived on. Harmless while both were uncompressed variants, but corrupting once storage contexts can negotiate JPEG 2000 (v1.7.0 preference fallback): a JPEG 2000 payload would be saved with meta claiming Explicit VR LE. The C-STORE handler now takes the transfer syntax from its own sub-operation's presentation context
- **C-GET parity fixes** — final status 0xBxxx (sub-operations complete with failures) is now treated as a completed transfer with a logged warning, matching the C-MOVE behaviour, instead of failing the whole retrieve; a peer request with no registered handler is logged and dropped instead of crashing the process by invoking a nil callback
- **Retrieves no longer hang forever on non-image series (SR, Presentation State, encapsulated PDF)** — some PACS C-MOVE agents stall while sending objects that carry no pixel data: the association stays open but no further data or progress response ever arrives, and the retrieve previously hung until the application was killed. A stall watchdog now aborts the retrieve after 120 seconds of total silence (no C-MOVE-RSP, no received file) with a status message and a dialog explaining the likely cause and recommending Retrieve method C-GET or Auto for the affected server. The timeout is configurable via `retrieveStallTimeoutSec` in settings.json (-1 disables it, for slow tape archives)
- **Cancelled and stalled retrieves now tear the association down** — cancelling a retrieve (or a watchdog abort) previously left the DIMSE goroutine blocked forever and the TCP association open; it now sends A-ABORT, unblocks the pending operation, and reaps the goroutine. Applies to C-MOVE, C-GET, and Push to PACS
### Internal

- `thirdparty/go-netdicom/servicedispatcher.go` — `peerCommands` map + `serviceCommandState.peer` flag; `findOrCreateCommand` replaced by `findOrCreatePeerCommand`; `handleEvent` routes by the command-field response bit (0x8000); `close()` drains both maps; regression test pins the message-ID collision (`TestServiceDispatcher_PeerRequestMessageIDCollision`)
- `serverprofile.go` — `EnsureUncompressed` field removed; `preferredTransferSyntaxes`/`preferredUncompressedSyntax` replaced by `requiredTransferSyntax()` returning the single required UID or "" (as stored); the negotiable set derives from it via `acceptedSyntaxesFor` (`transcode.go`)
- `thirdparty/go-netdicom/serviceuser.go` — the C-MOVE 0xB000 warning log line printed the hex dump of `StatusCode.String()` (`%X` on a `fmt.Stringer`) instead of the code; now prints `0xB000` plus the failed/total sub-operation counts
- `thirdparty/go-netdicom` — new `ServiceUser.Abort()` (A-ABORT + transport close; unblocks any DIMSE call waiting on a response); `serviceDispatcher.close()` made idempotent so Release/Abort can race safely; the dispatcher's per-command event forward tolerates a concurrently-aborted command instead of panicking
- `dicomnet.go` — `abortAndReap` invoked from the `ctx.Done()` paths of `Move`, `Get`, and `StoreFiles`
- `main.go` — activity-based stall watchdog in the retrieve loop (progress responses, received C-MOVE files, and received C-GET instances all reset it; each target gets a fresh window)
- `transfersyntax.go` (split out of `transcode.go`) — the two requirable transfer syntax UID constants, `transferSyntaxLabel`, and `fileTransferSyntaxUID`; `transcode.go` keeps the conversion machinery (`transcodeDICOMFile` with target syntax, `decompressPixelData`, J2K/JPEG frame-to-native conversion) plus the new `acceptedSyntaxesFor(requiredTS)` that builds the negotiable set (required first, then locally convertible)
- `storagescp.go` — `SetTransferPolicy(requiredTS string)` replaces the (accepted list, replace flag) pair; the accepted presentation-context set is `acceptedSyntaxesFor(requiredTS)` or unrestricted; `handleCStore` and `saveGetFile` transcode a non-required arrival in place (temp file) before the rename into the tree and fail the sub-operation on conversion error; a `converted` counter (`ConvertedCount()`) lets the retrieve loop report server non-compliance; the exists-on-disk skip compares the existing file's transfer syntax against the requirement and replaces on mismatch, silently skips otherwise; stale `.transcode_*.tmp` files are cleaned at startup alongside `.recv_*.tmp`
- `main.go` — retrieve loop gains `checkMoveFailures`: with a required syntax, the first C-MOVE progress response reporting failed sub-operations cancels the retrieve; any target error likewise aborts instead of continuing to the next target, and a dedicated dialog explains the abort. The completion status appends "(N converted locally to …)" from the SCP counter plus the C-GET callback count. The post-retrieve pass is now catalog-ingest + tree-reload only
- `preferences.go` — guarantee checkbox removed; transfer syntax labels are now "Explicit/Implicit VR LE (uncompressed — convert locally if needed)"
- `srtransfer_repro_test.go` (new) — loopback regression tests: C-STORE of a real SR into the live StorageSCP under unrestricted and strict-Explicit negotiation; an SCU limited to Explicit against an Implicit-requiring SCP must land Implicit on disk with the conversion counted; C-MOVE against a silent server must return promptly on cancel; a re-received already-present file is skipped silently without duplicating
- `transfersyntax_test.go` / `transcode_test.go` / `transcode_j2k_test.go` — profile migration, `requiredTransferSyntax` mapping, `acceptedSyntaxesFor` contents, C-GET proposals, `fileTransferSyntaxUID`; VR round-trip preserves pixels, no-op on target syntax, undecodable syntax rejected; J2K decodes bit-exactly to either target and is idempotent
- `live_utility_test.go` (new) — env-gated (`DICOMQR_LIVE=1`) diagnostics that retrieve specific image/PR/PDF series from the local UTILITY archive
- `main.go` — `fileLogSink` (stateless per-write append, failure counter, first-failure report to the ring) replaces the long-lived `os.Create` handle in `setupLogFile`; ring/stderr sinks wired unconditionally
- `settings.go` — `saveSettingsE` returns the save error (rename retried 3×150 ms, outcome logged); `saveSettings` wraps it for the quit paths; the Preferences dialog calls `saveSettingsE` and surfaces failures

## [1.7.0] — 2026-07-22

### Added

- **Transfer syntax preference per server profile** — the 'Request uncompressed transfer syntax only' checkbox is replaced by a dropdown: 'As stored (server decides)', 'Uncompressed — Explicit VR LE preferred', or 'Uncompressed — Implicit VR LE preferred'. The preference now drives both the C-GET proposal order and the storage SCP's pick during C-MOVE negotiation (previously the sender's ordering decided between Explicit and Implicit VR LE). Old profiles are migrated automatically
- **Guarantee uncompressed on disk** — new per-profile option that ensures every retrieved file is stored as Implicit/Explicit VR Little Endian regardless of PACS behaviour: files that still arrive compressed are decompressed locally after the retrieve (JPEG Baseline/Extended via the Go JPEG decoder, JPEG 2000 via OpenJPEG — the same formats the built-in viewer decodes; JPEG 2000 Lossless converts bit-exactly), and files in formats without a built-in decoder are counted in the status line and logged
- **Post-retrieve verification** — when an uncompressed syntax is requested, the retrieve status now reports how many files were decompressed locally and how many remain compressed
- **Persistent local index (SQLite)** — the Local Browse tree is now backed by a SQLite database (`.dicomqr-index.db` inside the download folder), so the Patient → Study → Series hierarchy and the per-instance file paths survive restarts. On startup the tree is populated from the index immediately, with no disk rescan
- **Automatic index updates** — files received via C-MOVE/C-GET and files copied in through the Import tab are parsed and added to the index when the transfer completes, and the Local Browse tree refreshes itself; a manual Scan still performs a full walk of the folder and rebuilds the index from what is actually on disk
- **Self-healing tree** — clicking or right-clicking a patient, study, or series whose files were deleted outside the application verifies the files on disk in the background and prunes the missing entries from both the tree and the index (empty series/studies/patients are removed bottom-up). Deleting files from within the app updates the index directly instead of triggering a full rescan
- Changing the download folder in Preferences switches to that folder's own index file — each managed folder carries its index with it

### Fixed

- **Stale compressed copies were never replaced** — re-retrieving a study previously skipped any file already on disk, so studies downloaded as JPEG 2000 before enabling the uncompressed option silently stayed JPEG 2000. With the on-disk guarantee enabled, both the C-MOVE SCP and the C-GET path now replace an existing compressed copy instead of skipping it
- **Transfer syntax setting required a reconnect** — the storage SCP read the uncompressed restriction once at connect time, so a Preferences change mid-session had no effect until disconnect/reconnect. The SCP now re-reads the policy per incoming association, and saving Preferences reapplies the active profile's policy immediately

### Internal

- `transcode.go` (new) — `transcodeDICOMFile` decompresses a file in place to Explicit VR LE (temp file + atomic rename; original untouched on any error): J2K frames via `decodeJPEG2000` raw int32 samples, JPEG frames via the library's `GetImage`; single-frame multi-fragment codestreams are reassembled; colour output rewrites Photometric Interpretation to RGB; `fileTransferSyntaxUID` reads (0002,0010) via a meta-only parse
- `serverprofile.go` — `TransferSyntax` (`""`/`"explicit-le"`/`"implicit-le"`) and `EnsureUncompressed` fields; `migrateProfile` maps the deprecated `TransferUncompressed` to explicit-le + guarantee on load
- `storagescp.go` — `SetTransferPolicy` replaces `SetUncompressedOnly`; the accepted-syntax list and replace-compressed flag are mutex-guarded and re-read per association; `saveGetFile` gains a `replaceCompressed` parameter
- `thirdparty/go-netdicom/contextmanager.go` — provider now picks the transfer syntax by the accepted list's order (local preference) rather than the sender's offer order
- `jpeg2000_stub.go` / `jpeg2000_openjpeg.go` — `jpeg2000Available` compile-time flag; stub gains a `decodeJPEG2000` returning a clear error
- `catalog.go` (new) — `catalog` type wrapping `modernc.org/sqlite` (pure-Go SQLite, no new CGO surface): WAL mode, single connection, nil-safe methods; `replaceAll` (full-scan rebuild), `ingestPaths` (parse + upsert), `removePaths` (delete + bottom-up prune of empty parents), `load` (returns the same shapes as `scanLocalFolder` so tree population is shared)
- `localbrowse.go` — per-file metadata extraction factored into `parseLocalFileMeta`/`fileMeta` (shared by the folder scanner and the catalog ingester); `buildLocalBrowseContent` gains `applyData`/`reloadFromDB`/`pruneMissing`/`verifyNode` and returns a reload func; touch-verification is debounced per node with an in-flight guard
- `importtab.go` — `importOneFile` returns the destination path; completed imports are ingested into the catalog and the Local Browse tree reloads
- `main.go` — catalog opened at startup and closed on every termination path; retrieve completion ingests the received file paths in the background and reloads the Local Browse tree

## [1.6.0] — 2026-06-25

### Added

- **Structured Report (SR) and non-image document viewer** — series whose modality is SR, KO, AU, or PR are automatically routed to a dedicated document viewer instead of the image viewer. The document viewer renders the DICOM SR Content Sequence as formatted, scrollable markdown with section headings and labelled value pairs; Prev/Next buttons step through multi-file documents; a Copy text button places plain text on the clipboard; the header displays patient identity, study context, content date/time, and completion and verification status
- **DICOM overlay plane compositing** — CT, MR, and other images that contain 1-bit bitmap overlay planes (DICOM groups 6000–60FE) now render the overlays composited onto the image in opaque yellow. Overlay planes are decoded at load time from the packed LSB-first bit array and composited after windowing so they remain visible at any W/L setting. The deprecated "bit-position-in-pixel-data" encoding (OverlayBitPosition > 0, retired in DICOM 2004) is silently skipped
- **Overlays checkbox** — a new Overlays checkbox in the series viewer bottom bar (alongside Annotations) toggles overlay plane compositing on and off; the checkbox is hidden for series that contain no overlay planes; state persists between sessions

### Fixed

- **UI freeze on SR retrieve** — retrieving a Structured Report series or any other non-image DICOM object no longer causes the application to stop responding. The root cause was grailbio's `ReadDataSetFromFile` hanging on SR Content Sequences (0040,A730) — deeply nested SQ elements that the library had no path to skip. The fix replaces both call sites in the C-STORE handler with a fast suyashkumar streaming parse that stops after DICOM group 0x0020
- **Process remains in Task Manager after exit** — closing the application no longer leaves the process alive after the UI disappears. The intermittent hang was caused by the clock-display goroutine calling `fyne.Do` into Fyne's event queue after the queue had stopped being drained during shutdown — a race window of ~1 second that explains "sometimes but not always". The goroutine is now properly cancelled on all termination paths (window close, Quit menu, and the `OnStopped` lifecycle hook). The C-ECHO button also uses a bounded 30-second context rather than an unbounded `context.Background()`

### Internal

- `srviewer.go` (new) — `parseSRFile` full-dataset parse via suyashkumar/dicom; `walkSRContentSeq` recursive Content Sequence walker handling TEXT, NUM, CODE, DATE, TIME, PNAME, UIDREF, IMAGE, and CONTAINER value types; `srEntriesToMarkdown`/`srEntriesToPlainText` formatters; `openSRWindow` Fyne document viewer; `seriesModality` early-exit streaming parser that stops after group 0x0008; `isDocumentModality` dispatch guard
- `viewer.go` — `dicomOverlay` struct; `extractOverlays` single-pass scan of dataset elements for groups 6000–60FE with LSB-first bit unpack; `paintOverlays` method composites overlays onto `*image.RGBA`; `imageViewport.showOverlays` flag + `setShowOverlays` method; `renderBase` conditionally applies overlays; SR/document dispatch at `openViewerWindow` entry before `fyne.Do`; `overlayCheck` widget hidden until overlays are detected on the loaded frame
- `storagescp.go` — `scpParseMetadata` streaming parser replaces both `ReadDataSetFromFile` calls for post-receive metadata extraction; stops at group 0x0020, skipping all SQ elements including SR Content Sequence
- `main.go` — clock goroutine converted to `time.NewTicker` + `clockDone` channel (closed via `sync.Once` `stopClock` on all three termination paths); echo button bounded with a 30-second timeout context

---

## [1.5.0] — 2026-06-18

### Added

- **JPEG 2000 decoding in the built-in viewer** — DICOM images stored with the JPEG 2000 transfer syntaxes (lossless `1.2.840.10008.1.2.4.90` and lossy `…4.91`) now decode and display directly, including local files in Local Browse and Import. Monochrome J2K flows through the normal window/level pipeline (so presets and colour maps apply); colour J2K renders as RGB. Decoding is backed by the OpenJPEG library, statically linked so the executable stays self-contained
- Release and development build scripts (`build-release.sh`, `build-debug.sh`) and the CLAUDE.md build commands now enable the `openjpeg` build tag

### Changed

- JPEG 2000 is no longer listed among the "cannot be decoded" transfer syntaxes; the viewer's compressed-pixel-data notes and the user manual are updated accordingly

### Internal

- `jpeg2000_openjpeg.go` (`//go:build openjpeg`) — CGO wrapper around OpenJPEG (`libopenjp2`): decodes a J2K/JP2 codestream from memory via custom `opj_stream` read/skip/seek callbacks, returning planar `int32` samples; statically linked with `-DOPJ_STATIC -l:libopenjp2.a` (no DLL dependency, ~0.5 MB larger exe)
- `jpeg2000_stub.go` (`//go:build !openjpeg`) — fallback so the default build compiles without OpenJPEG; J2K files then report "support not built in"
- `decodeFrame` now takes the Transfer Syntax UID and routes JPEG 2000 to `decodeJPEG2000Frame`; the raw-pixel fallback is guarded against J2K codestreams; J2K removed from `unsupportedTransferSyntaxNames`
- Requires the MSYS2 package `mingw-w64-x86_64-openjpeg2`. `testdata/ramp8.j2k` is a lossless fixture (built with `opj_compress`) decoded by the tag-gated unit tests asserting exact pixel values

---

## [1.4.0] — 2026-06-18

### Added

- **Colour maps for nuclear-medicine studies** — the image viewer can apply a colour lookup table to grayscale images, so PET and SPECT/NM uptake can be read in pseudo-colour rather than grayscale:
  - A **Colour dropdown** in the viewer bottom bar offers six DICOM-standard palettes: **Grayscale**, **Inverse Grayscale**, **Hot Iron**, **PET**, **Hot Metal Blue**, and **PET 20 Step**
  - PET (`PT`) and nuclear-medicine (`NM`) studies default to **Hot Iron** automatically; all other modalities default to Grayscale
  - The colour map composes with window/level and persists as you scroll through a series; the **study overview thumbnails** use the same default map so the overview matches the viewer
  - The dropdown is disabled for images already stored in colour
- **SPECT/NM window presets** — `NM` studies now receive the same fraction-of-peak window presets as PET

### Internal

- `colormap.go` — `colorMap` 256-entry RGB LUT type with built-in DICOM-standard palettes (piecewise-linear renditions), `colorMapByName`, and `defaultColorMapForModality`
- `viewer.go` — `decodedFrame.render`/`renderInto` now produce `*image.RGBA` and apply the active colour map; the viewport carries a reusable RGBA buffer and `curMap`, and `setColorMap` re-renders without re-reading the file. RGBA buffers also upload to the GPU without per-refresh conversion
- `viewer_test.go` — tests for LUT application, grayscale identity / inverse, map endpoints, name lookup fallback, modality default map, and PET 20 Step quantisation

---

## [1.3.0] — 2026-06-18

### Added

- **Interactive image viewer** — the series viewer now supports direct mouse and keyboard manipulation:
  - **Window/level by left-drag** — horizontal adjusts window width, vertical adjusts level; the adjustment is anchored to the point where the drag began
  - **Zoom by right-drag** (up to 16×) and **pan by middle-drag** when zoomed in; **double-click** resets zoom/pan to fit
  - **Mouse wheel** steps through slices; **arrow keys / Page Up / Page Down** also navigate slices
  - **Keyboard**: `+`/`-` zoom, `Home`/`F` reset view, `R` reset window
  - A **Reset** button restores both the view and the window to default. Window/level and zoom/pan persist as you scroll through a series
- **Modality-specific window/level presets** — a Window dropdown in the viewer offers presets tailored to the image's DICOM Modality:
  - **CT** — Hounsfield windows: Brain, Subdural, Soft tissue, Liver, Mediastinum, Bone, Lung
  - **PET (PT)** — windows expressed as a fraction of the peak value: 0 → 75% / 50% / 40% / 30% / 20%
  - **MR** — contrast windows scaled relative to the image's own window (Lower / Higher / Highest contrast), since MR intensities have no absolute scale
  - All sets include Default (the image's own/auto window) and Full range (the entire pixel value range)

### Fixed

- **Results filter showed all patients when nothing matched** — typing a filter string in the PACS Query results that matched no node incorrectly displayed every patient instead of an empty tree. The filter now correctly distinguishes "no filter" from "filter matched nothing"
- **Smooth window/level dragging** — eliminated image flicker during a window/level drag. The viewer now scales on the GPU (avoiding a per-refresh CPU resample that starved the paint loop), re-windows into a reused buffer (no per-tick allocation), defers the annotation-overlay rebuild to drag end, and computes the window from absolute mouse displacement anchored to the press point (fixing a first-move jump and a Window/Level axis swap)

### Internal

- `viewer.go` — `decodedFrame` now separates pixel decode from windowing (`renderInto` into a reusable buffer), enabling cheap re-windowing; new `imageViewport` custom widget handles mouse/scroll/keyboard input, zoom/pan via `SubImage` cropping, and modality preset selection (`wlPreset`/`presetsForModality`)
- `viewer_test.go` — unit tests for window rendering, default-window computation, preset resolution (CT/PET/MR), modality selection, and clamp helpers

---

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
