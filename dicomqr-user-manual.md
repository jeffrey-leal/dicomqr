# dicomqr

**User Manual  v1.21.0**

September 30, 2026

A Windows desktop application for querying, retrieving, and managing DICOM medical imaging studies.

---


## 1  Overview

dicomqr is a Windows desktop application for querying, retrieving, and managing DICOM medical imaging studies. It communicates with PACS servers using standard DICOM networking services and includes tools for browsing, previewing, importing, routing, and deleting local DICOM files.

Key capabilities:

- Connect to any DICOM-compatible PACS server using configurable server profiles
- Search for studies by patient name, patient ID, accession number, date range, and modality
- Browse query results in an expandable Patient > Study > Series tree, sorted alphabetically, chronologically, and numerically
- Retrieve entire studies or individual series to a local folder using C-MOVE or C-GET
- Automatically organise downloaded files by patient, study, and series
- Query a Modality Worklist server independently of the active PACS connection
- Browse local DICOM files in the download folder — backed by a persistent index so the tree survives restarts — and push them to any PACS via C-STORE or delete them
- Preview DICOM images in the built-in viewer with interactive window/level, zoom and pan, modality-specific W/L presets, colour maps for PET/SPECT, DICOM annotation overlays, DICOM overlay plane compositing, and study overview grids; decodes JPEG Baseline, JPEG Lossless, JPEG 2000, and uncompressed pixel data
- View Structured Reports (SR), Key Object Selections (KO), and other non-image DICOM objects in a dedicated scrollable document viewer that renders the SR Content Sequence as formatted text
- Import DICOM files from external folders into the organised download folder
- Support for multiple saved server profiles with independent connection and retrieve settings
- Optionally require a specific uncompressed transfer syntax per server profile — Explicit or Implicit VR Little Endian — guaranteed on disk: the server sends it directly, or files arriving in a decodable syntax (JPEG Baseline/Extended, JPEG 2000, the other uncompressed VR) are converted on receipt; objects that can be neither delivered nor converted are skipped and reported while the rest of the study is retrieved
- Automatic wildcard search — trailing `*` appended to text fields so partial names match without manual wildcarding
- Customisable appearance — selection colour, font style, external viewer path, and window size are remembered between sessions

## 2  Getting Started


### 2.1  System Requirements

- Windows 10 or later (64-bit)
- Network access to a DICOM PACS server
- A configured PACS that accepts DICOM associations from this workstation

### 2.2  PACS Registration

Before connecting, the PACS administrator must register this workstation as a known Application Entity (AE). The required details are shown in Help > Client info… once the application is running:

| Field | Default | Description |
|---|---|---|
| Local AE Title | DICOMQR | The name the PACS uses to identify this workstation. |
| Local SCP port | 11112 | The TCP port on which dicomqr listens for incoming file transfers. |
| Local IP | Detected automatically | The IP address of this workstation as seen by the PACS. |

The AE Title and port can be changed in File > Preferences… > SCP & Network.

For C-MOVE file retrieval to work, the PACS must be able to initiate an outbound TCP connection from its own network address to the Local IP and Local SCP port shown in Client info. Ensure that any firewall on this workstation permits inbound connections on that port.


### 2.3  Starting the Application

Double-click `dicomqr.exe` to launch the application. The main window opens on the Local Browse tab, showing the indexed contents of the download folder; the status bar shows the application version. The window reopens at the size and position it had when last closed, on the monitor it was left on — a window parked on a second display stays there across sessions. On a first run it opens at 60% of the screen width by 80% of its height on the primary display, centred vertically and left of centre horizontally, leaving the right-hand part of the screen clear for a viewer window or another application. Either way the window is kept wholly inside its monitor's work area, so it is never placed under the taskbar — and a position saved on a monitor that has since been disconnected moves to the nearest remaining display rather than opening out of reach.


## 3  The Main Window

The main window is divided into a connection panel at the top, a tab area in the centre, and a status bar at the bottom.

Connection panel — the topmost area, visible from all tabs. Left side: server profile selector, Filters button, Search button. Right side: Connect, Disconnect, and Test (C-ECHO) buttons. A second row shows the SCP status indicator.

Tab area — four tabs, in this order:

| Tab | Purpose |
|---|---|
| Local Browse | Browse, preview, push, and delete files in the download folder. |
| PACS Query | Search a remote PACS and retrieve studies. |
| Import | Copy DICOM files from an external folder into the download folder. |
| Worklist | Query a Modality Worklist server for scheduled procedures. |

The Local Browse tab is active when the application starts.

Status bar — the bottom strip. A coloured LED indicator precedes the status text. A clock shows the current date and time. A progress bar appears during queries and retrieves.


## 4  Connecting to a PACS Server


### 4.1  Server Profiles

A server profile stores the connection details for one PACS destination. Profiles are managed in File > Preferences… > SCP & Network. Each profile records:

| Field | Description |
|---|---|
| Profile name | A label used to identify the server in the dropdown. |
| Remote AE Title | The Application Entity Title of the PACS (case-sensitive). |
| Host | The hostname or IP address of the PACS server. |
| Port | The TCP port on which the PACS listens (typically 104 or 11112). |
| Info model | The DICOM Query/Retrieve information model. `study` = Study Root (most common). `patient` = Patient Root. `patient-study-only` = legacy retired model used by some older systems; SERIES-level queries are not available with this model. |
| Retrieve method | C-MOVE (default) instructs the PACS to push files to the local C-STORE SCP listener. C-GET requests that the PACS return files over the same association — no inbound port or PACS-side destination registration is required. Auto tries C-GET first and falls back to C-MOVE if the PACS rejects it. |
| Connect timeout | Seconds to wait for the initial C-ECHO before reporting a failure. Default: 10 s. |
| Transfer syntax | 'As stored (server decides)' accepts whatever the PACS prefers — a JPEG 2000 archive will typically send JPEG 2000. The two uncompressed options guarantee every retrieved file ends up in exactly the selected syntax on disk — Explicit VR Little Endian (`1.2.840.10008.1.2.1`) or Implicit VR Little Endian (`1.2.840.10008.1.2`). Negotiation offers the required syntax first, so a transcoding-capable PACS sends it directly; a PACS that only serves objects in their stored form may instead send any syntax the application can decode (the other uncompressed VR, JPEG Baseline/Extended, JPEG 2000), and each such file is converted to the required syntax on receipt, before it reaches the download folder — in the background, after the server has been told the file arrived, so the transfer is not held up waiting for each conversion. The status bar reports how many files needed local conversion. Objects that cannot be obtained in the required syntax do not stop the retrieve: an object the server cannot deliver in any negotiable syntax (e.g. the archive stores JPEG-LS or RLE and does not transcode), or one that arrives but fails local conversion (e.g. a screenshot with undecodable pixel data), is skipped — reported in the Activity Log and counted in the completion status — while every deliverable object is retrieved and stored in the required syntax. Only when the server can deliver nothing at all in a negotiable syntax does an error dialog appear, naming the required syntax; switch the profile back to 'As stored' to retrieve such data in its stored form. |

The first profile in the list is selected by default when the application starts.


### 4.2  Connection Indicators

The coloured LED to the left of the status bar text shows the connection state:

| Colour | Meaning |
|---|---|
| Gray | Disconnected |
| Amber | Connecting (C-ECHO in progress) |
| Green | Connected |

A second indicator in the connection panel row below the server selector shows the embedded C-STORE SCP state:

| Colour | Meaning |
|---|---|
| Gray | Not running |
| Green | Listening — shows the bound address and local AE Title |
| Red | Error — shows the error reason |


### 4.3  Connecting

Select a server profile from the dropdown in the server row, then click Connect (or select File > Connect). The application sends a C-ECHO to verify basic DICOM connectivity. If the C-ECHO succeeds, dicomqr starts the embedded C-STORE listener and the connection LED turns green.

If the C-ECHO fails, the status bar shows a connection error and the application remains disconnected.

If the SCP port is already in use — most often because a previous copy of dicomqr was force-closed — a dialog reports "port N is already in use". Close the other instance and click Connect again.


### 4.4  Testing Connectivity

Click Test (C-ECHO) at any time while connected to send a C-ECHO to the PACS. The status bar reports success or failure.


### 4.5  Disconnecting

Click Disconnect (or select File > Disconnect) to close the session. Any in-progress query is cancelled and the local SCP listener is stopped.


## 5  Searching for Studies


### 5.1  Opening the Filters Panel

Click Filters ▾ in the server row to open the search criteria panel. The panel floats over the results tree and contains the search fields along with Search, Clear, and Close buttons. Click Filters ▾ again, or click Close inside the panel, to dismiss it. Values typed in the fields are preserved between open and close cycles.


### 5.2  Search Fields

| Field | Description |
|---|---|
| Patient Name | Matches the DICOM Patient Name attribute. Supports DICOM wildcard characters: `*` matches any sequence of characters, `?` matches a single character. Format: FAMILY^GIVEN or a partial name with wildcards (e.g. DOE*). A trailing `*` is appended automatically if the value does not already end with one. Leave blank to match all patients. |
| Patient ID | Matches the DICOM Patient ID (MRN). Supports wildcards. A trailing `*` is appended automatically. Leave blank to match all IDs. |
| Accession No | Matches the DICOM Accession Number. Supports wildcards. A trailing `*` is appended automatically. Leave blank to match all accession numbers. |
| Study Date From | The start of the study date range. Click the calendar icon to open a month-view date picker and select a date, or type directly into the field. Leave blank for no lower bound. |
| Study Date To | The end of the study date range. Click the calendar icon to open a month-view date picker and select a date, or type directly into the field. Leave blank for no upper bound. |
| Modality | Restricts results to one or more imaging modalities. Tick any combination: CT, MR, PT, NM, US, CR, DX, XA, RF. When multiple modalities are ticked, a separate query is sent for each and the results are merged. Leave all checkboxes unticked to include all modalities. |

At least one field should be populated before searching. Sending a completely unconstrained query (all fields blank, no modalities ticked) may return a very large result set or be rejected by the PACS.


### 5.3  Running a Search

With the Filters panel open, click Search inside the panel, or click the Search button in the server row, or press Ctrl+Enter. The panel closes, the results tree clears, and the query is sent to the PACS. The status bar shows "Querying…" during the search and reports the number of studies returned when complete.

Pressing Enter while the cursor is in the Patient Name, Patient ID, or Accession No field also runs the search and closes the panel.


### 5.4  Clearing the Search

Click Clear inside the Filters panel to reset all search fields to their defaults and clear the results tree. Alternatively, select Query > Clear results.


## 6  Query Results


### 6.1  Tree Structure

Results are displayed in an expandable tree with three levels:

Patient — one node per unique patient. The label shows the patient name and, where present, the patient ID in parentheses.

Study — one or more studies under each patient. The label shows the study date, study description, accession number, and the set of modalities present in the study.

Series — one or more series under each study. The label shows the series number, modality, series description, and image count.

Results are sorted automatically: patients alphabetically by name, studies within a patient chronologically by date (oldest first), and series within a study numerically by series number.

The tree starts fully collapsed after each search. Click the expand arrow next to a patient node to reveal its studies. Click the expand arrow next to a study node to load its series — dicomqr sends a separate C-FIND query to the PACS at this point to retrieve series-level information. The series list is fetched once per study per session; collapsing and re-expanding a study does not repeat the query.

The Expand All and Collapse All buttons above the tree open or close every branch at once. Expanding all branches also triggers the series C-FIND for any study that has not yet loaded its series.

Large result sets are inserted into the tree in batches so the window stays responsive; the status bar shows a `Loading results… N/total` count while the batch is being added.


### 6.2  Filtering Results

Type any text into the filter bar above the results tree. The tree immediately redraws to show only rows whose label contains the typed text (case-insensitive). Parent nodes that contain a matching descendant are always shown. Click Clear at the right of the filter bar to remove the filter and restore the full tree.

The filter acts on the already-loaded results and does not send a new query to the PACS.


### 6.3  Selecting Items for Retrieval

Click any row in the results tree to select it. Selected rows are highlighted using the colour and font style configured in Preferences > User Interface (by default, bold in the theme's primary accent colour — see Section 11.1). A plain click always replaces the selection: whatever was selected before is deselected. Clicking the row that is the only selection deselects it (a selected patient or study counts as one row together with the series beneath it); when several rows are selected, clicking one of them keeps just that one. Clicking the arrow beside a patient or study opens or closes it without changing the selection. Selecting more than one row takes a modifier key, as in Windows Explorer:

| Mouse | Action |
|---|---|
| Click | Select that row (and everything beneath it), deselecting everything else. Clicking the row that is the only selection deselects it. |
| Ctrl+click | Add the row to the selection, or remove it if it is already selected; the rest of the selection is untouched. |
| Shift+click | Select every row from the last click to this one, replacing the selection. |
| Ctrl+Shift+click | Add every row from the last click to this one to the existing selection. |

Rows at any level (patient, study, or series) may be selected together. Selecting a patient or study also selects every series beneath it; Ctrl+click one of those series to deselect just that series — the patient or study is no longer treated as fully selected, but its other series stay selected.

A Shift range runs between series of the same study (or studies of the same patient). A Shift+click on a row that is not a sibling of the last click — a series in a different study, for example — selects just that row instead of guessing at a range across branches.

The Select All button (in the retrieve panel) selects every currently visible row and its loaded descendants; Clear Selection clears the entire selection. Pressing Esc also clears the current selection.

Series nodes are only visible after a study has been expanded. Expand a study first, then select individual series for retrieval.

Selection behaviour during retrieval:

- Patient node selected — all studies under that patient are retrieved.
- Study node selected — the entire study is retrieved as a single C-MOVE request.
- Series node(s) selected — each selected series is retrieved individually.
- Mixed selection — if a study and one or more of its series are both selected, the study-level retrieve takes precedence and the series are not sent as duplicate requests.
Press Ctrl+C to copy the full label text of any selected row to the clipboard.


### 6.4  Right-Click Context Menu

Right-clicking any row in the results tree opens a context menu:

| Option | Action |
|---|---|
| Retrieve | Retrieves the right-clicked node directly, regardless of the current selection. |
| Copy UID | Copies the Study Instance UID or Series Instance UID of the row to the clipboard. |
| Copy label | Copies the full display label of the row to the clipboard. |


## 7  Retrieving Files


### 7.1  Prerequisites

The conditions required depend on the Retrieve method configured for the server profile (see Section 4.1).

For C-MOVE (default):

- The application must be connected to a PACS server (status bar shows "Connected").
- The embedded C-STORE listener must be running. It starts automatically when a connection is established.
- The PACS must have the local AE Title, IP address, and port registered as a known destination. See Section 2.2.
- The download folder must be configured. Click Browse… next to the Download to field in the retrieve panel.
- At least one item must be selected in the results tree.
For C-GET:

- The application must be connected to a PACS server.
- The download folder must be configured.
- At least one item must be selected.
No inbound SCP port or PACS-side destination AE registration is required for C-GET. The PACS returns files over the existing outbound connection.

For Auto: dicomqr attempts C-GET first. If the PACS rejects C-GET, it retries each item using C-MOVE. The C-MOVE prerequisites above apply as a fallback.

Regardless of method, dicomqr verifies that the download folder exists and is writable before a retrieve begins. If it is not, an error dialog is shown and no retrieve is started.


### 7.2  Starting a Retrieve

Select one or more rows in the results tree, then click Retrieve Selected (or select Query > Retrieve Selected). dicomqr issues a retrieve request for each selected item using the method configured in the server profile:

- C-MOVE — dicomqr sends a C-MOVE request; the PACS pushes files to the local C-STORE SCP listener, which writes them to the download folder.
- C-GET — dicomqr sends a C-GET request; the PACS streams files back over the same association directly.
- Auto — dicomqr attempts C-GET; if the PACS rejects it, the request is retried using C-MOVE.
Selecting a study retrieves all of its series in one request; selecting individual series retrieves each independently. A progress bar appears and advances as each study or series is transferred.

One retrieve runs at a time. Starting another while one is in progress — from the button, the menu, or the results tree's right-click — is refused with a message rather than queued: wait for the running retrieve to finish, or cancel it, then try again. Disconnecting cancels a retrieve in progress along with the connection.


### 7.3  Progress

The progress bar tracks completion across all selected studies and series, advancing as each study or series finishes. For both C-MOVE and C-GET the bar also advances within a study as the server reports sub-operation progress. As each file arrives, the status bar briefly shows the path of the received file.


### 7.4  Completion

When all files have been received successfully, the progress bar disappears and the status bar shows:

```
Retrieved N files successfully
```

If one or more targets encountered a recoverable DICOM error (for example, a warning status from the PACS indicating that some sub-operations failed), the status bar shows the number of files received alongside the number of targets that had problems:

```
Retrieved N files (X/Y targets had errors — see log)
```

In this case a dialog also appears offering to retry only the failed targets. Accepting re-runs the retrieve loop for just those items, leaving already-retrieved files in place. Details of the errors are written to `dicom.log` in `%USERPROFILE%\.dicomqr\`. The log records the DICOM protocol exchange — association negotiation (each presentation context with the offered and chosen transfer syntaxes, and any rejections), every DICOM message, and per-file receipt — and the two previous sessions are kept as `dicom.log.1` and `dicom.log.2`, so evidence of a failed or stalled transfer survives an application restart. For a deeper protocol investigation, starting dicomqr with the environment variable `DICOMQR_DICOMLOG_LEVEL=3` also records every network packet (PDU) and each step of the connection's state — too much to leave on routinely, since it slows large transfers.

When the profile requires a specific transfer syntax, the completion status also accounts for objects that could not be obtained in it — the retrieve continues past them rather than aborting. Files that arrived in a different syntax and were converted locally are counted as '(N converted locally to …)'. An object that arrives but cannot be converted (for example a screenshot or vendor graphic whose pixel data has no built-in decoder) is skipped — it never reaches the download folder — and the status appends '— N unconvertible object(s) skipped, see Activity Log'. An object the server could not deliver in any negotiable syntax appends '— N not delivered by the server, see Activity Log'. Each skipped or undelivered object is logged with its SOP Instance UID and series so it can be identified afterwards, and every file that does reach the download folder is guaranteed to be in the required syntax. Only when the server delivers nothing at all in a negotiable syntax (zero files received) does an error dialog appear, naming the required syntax and recommending the 'As stored' setting for that server.

Files that need converting are converted in the background once they have arrived, so the server can go straight on to the next file. When the server has finished, the retrieve waits for the last conversions before it reports — the status bar shows 'Converting N received file(s) to …' meanwhile — so every converted file is counted and appears in Local Browse. Because the server has already been told such a file arrived, a converted file that then cannot be saved (for example, a folder that cannot be created) is reported locally instead: the status appends '— N received file(s) could not be saved after conversion, see Activity Log'. If dicomqr is closed while conversions are still running, nothing is lost: the received files wait in the download folder and are converted, filed and indexed the next time dicomqr starts.


### 7.5  Cancelling a Retrieve

Click Cancel in the retrieve panel (or select Query > Cancel retrieve) to abort an in-progress retrieval. Files that have already been written to disk are not removed. The status bar shows:

```
Retrieve cancelled
```


## 8  Local Browse Tab

The Local Browse tab lets you work with DICOM files already in the download folder — browse the tree, preview images, push to a remote PACS, or delete files — without running a query.


### 8.1  The Persistent Index

The Local Browse tree is backed by a persistent index — a SQLite database file (`.dicomqr-index.db`) stored inside the download folder — so the Patient > Study > Series tree from the previous session appears immediately at startup, with no rescan. The index updates automatically when a retrieve or an import adds files, and each download folder carries its own index with it.

Click Scan to bring the tree and the index into line with what is actually on disk. dicomqr walks the download directory and reads only the files that are new, or whose size or timestamp shows they have changed since they were last read — every other file is already described in the index and does not need opening again. On a folder nothing has been added to, that makes a scan a matter of seconds even when the files are not already in the operating system's cache. The status line reports each stage and finishes by saying what changed, for example Found 12 studies, 100 series — 43 added, 2 removed. Use Scan whenever files were added to the folder outside the application.

Click Rebuild to read every file again and replace the index outright. Scan trusts what the index already says about a file, so Rebuild is the answer when it is the index you doubt rather than the folder — after copying files in by hand while the application was closed, for instance, or if the tree ever looks wrong in a way a Scan does not put right. It takes as long as a scan used to. If a scan cannot read part of the folder — a permissions problem, or a drive that has gone away mid-scan — it leaves the index alone rather than treating the files it could not see as deleted, and says so in the Activity Log. The folder button opens the download directory in Windows Explorer.

If files are removed outside the application, clicking or right-clicking an affected patient, study, or series verifies its files in the background and prunes the missing entries from both the tree and the index automatically.


### 8.2  Filtering and Navigation

Type in the filter bar to narrow the tree. Expand All, Collapse All, and Clear buttons are provided. The filter acts on the already-loaded tree and does not rescan the disk.


### 8.3  Previewing Images

Right-click any node and select Preview Images:

- Series node — opens the series viewer (see Section 8.3.1).
- Study node — opens the study overview grid (see Section 8.3.4).
- Patient node — Preview Images is disabled (too many files to be useful at this level).

#### 8.3.1  Series Viewer

The series viewer displays one image at a time and opens at the middle slice. It supports interactive window/level, zoom and pan, and slice navigation by mouse or keyboard.

Multi-frame files are navigated frame by frame. Most modalities (CT, MR) store one image per file, but ultrasound and nuclear medicine do not: an echo study stores each cine loop as a multi-frame file, and a SPECT reconstruction or projection set is one file holding the whole acquisition. A series containing any such file opens in chapter mode (see Section 8.3.7); a series of ordinary single-frame images keeps the plain one-position-per-image slider described here. Either way nothing is re-read from disk as you scroll: each file is parsed once and its frames decoded as they are reached.

The bottom bar contains an image counter (e.g. `45 / 120`, counting frames), a navigation slider, a Window preset dropdown (see Section 8.3.2), a Colour map dropdown (see Section 8.3.3), an Annotations checkbox (see Section 8.3.5), an Overlays checkbox (see Section 8.3.6, shown only when overlay planes are present), a Reset button, and an info label showing pixel dimensions and the current W/L values.

Multi-phase MR series — a series that covers the same stack of slice positions several times over, such as an in-phase/out-of-phase pair, the b-values of a diffusion acquisition, or dynamic timepoints — opens in phase mode: the slider spans one phase's slices, and a Phase dropdown appears at the left of the bottom bar naming each phase from what the files state (echo number and TE, the pulse-sequence name carrying a b-value, temporal position or acquisition number). Switching phases — from the dropdown, or by pressing P to cycle — keeps the slice, zoom, pan and window, so it toggles between phases at the same anatomical position; this is the flicker comparison an in/out-phase sequence is read with. A series without this structure (including localizers with mixed planes and bolus-tracking series, which repeat one position over time) keeps the ordinary slider covering every image.

Mouse controls:

| Action | Effect |
|---|---|
| Left-drag | Adjust window/level — horizontal changes the window width, vertical changes the level (centre). The adjustment is anchored to the point where the drag began. |
| Right-drag | Zoom — drag up to magnify, down to zoom out (up to 16×). |
| Middle-drag | Pan the image when zoomed in. |
| Mouse wheel | Step to the previous / next slice in the series. |
| Double-click | Reset zoom and pan to fit the window. |

Keyboard controls (while the viewer window is focused):

| Key | Effect |
|---|---|
| Up / Left / Page Up | Previous slice. |
| Down / Right / Page Down | Next slice. |
| `+` / `-` | Zoom in / out. |
| P | Next phase (multi-phase MR series only) — toggles at the same slice. |
| Home or F | Reset zoom and pan to fit. |
| R | Reset the window to the default (clears any preset or manual adjustment). |

The Reset button resets both the view (zoom/pan) and the window to the default. Window/level changes made by dragging or by selecting a preset persist as you scroll through the series.

Compressed pixel data — the built-in viewer decodes JPEG Baseline, JPEG Lossless (Process 14 / SV1, common on ultrasound still captures), JPEG 2000 (lossless and lossy), and uncompressed (native) pixel data. Files stored in JPEG-LS or RLE Lossless formats cannot be decoded and display a message suggesting Open in Viewer; to view those, either use an external viewer or require an uncompressed transfer syntax in the server profile before retrieving (see Section 4.1). Note that JPEG Lossless support is view-only: a retrieve that requires an uncompressed transfer syntax still skips JPEG Lossless objects the server cannot convert (see Section 4.1).


#### 8.3.2  Window/Level Presets

The Window dropdown in the viewer bottom bar offers preset windows tailored to the image's modality. Selecting a preset applies it to the current slice and to subsequent slices until you adjust the window manually. Default restores the image's own window (from the DICOM Window tags, or an automatic 1st–99th percentile window when absent); Full range maps the entire pixel value range.

| Modality | Presets offered |
|---|---|
| CT | Default, Full range, plus Hounsfield windows: Brain, Subdural, Soft tissue, Liver, Mediastinum, Bone, Lung. |
| PET (PT) | Default, Full range, plus 0 → 75% / 50% / 40% / 30% / 20% windows expressed as a fraction of the peak value (a lower percentage raises contrast in low-uptake regions). |
| MR | Default, Full range, plus Lower / Higher / Highest contrast windows scaled relative to the image's own window (MR intensities have no absolute scale). |
| Other | Default, Full range, Lower contrast, Higher contrast. |


#### 8.3.3  Colour Maps

The Colour dropdown in the viewer bottom bar applies a colour lookup table to the windowed image — useful for nuclear-medicine studies (PET and SPECT/NM), which are conventionally read in pseudo-colour rather than grayscale. The colour map is applied on top of the current window/level and persists as you scroll through the series until you change it.

For PET (PT) and nuclear-medicine (NM) studies the viewer selects Hot Iron automatically; all other modalities default to Grayscale. The same default colour map is applied to the study overview thumbnails (Section 8.3.4) so the overview matches the viewer.

| Colour map | Description |
|---|---|
| Grayscale | Standard grayscale (default for CT, MR, and most modalities). |
| Inverse Grayscale | Grayscale with the intensity ramp inverted. |
| Hot Iron | Black → red → yellow → white. Default for PET/NM. |
| PET | Black → blue → purple → red → orange → yellow → white. |
| Hot Metal Blue | Like PET but with blue rising earlier (cool shadows, hot highlights). |
| PET 20 Step | The PET palette quantised into 20 discrete colour bands. |

Colour maps apply only to grayscale (monochrome) images; for images already stored in colour the dropdown is disabled. The maps are faithful renditions of the DICOM standard palettes intended for display and triage.


#### 8.3.4  Study Overview Grid

The overview window shows one thumbnail per series — the middle slice of each series rendered in parallel; for a series held in a single multi-frame file (typically SPECT/NM) that is the middle frame of the acquisition. Thumbnails flow from the top-left, wrapping into as many columns as fit the window, and reflow when the window is resized. Double-click any thumbnail to open that series in the full series viewer.

A series with nothing to display — SR, KO, PR and other non-image objects, or pixel data the built-in decoders cannot render — appears as a white tile with its modality in large bold black text instead of an image. Double-clicking the tile still opens the appropriate viewer for the series.

While the overview is generated, a progress dialog counts the series as they load — studies with thousands of images can take several seconds. Series previews and the folder Preview button show the same dialog while large image sets are scanned and sorted; the application remains responsive throughout.


#### 8.3.5  DICOM Annotation Overlay

When Annotations is checked in the series viewer, a four-corner overlay is drawn within the actual image area (never in the letterbox bars):

| Corner | Content |
|---|---|
| Top-left | Patient name, MRN, date of birth, sex and age |
| Top-right (right-aligned) | Institution, study date/time, accession number, study description, referring physician |
| Bottom-left | Modality, series number and description, slice thickness, protocol |
| Bottom-right (right-aligned) | Instance number / total, slice location, pixel spacing, W/L values |

Anatomical orientation markers (R/L, A/P, H/F) are centred on the four image edges and derived from the ImageOrientationPatient direction cosines in DICOM LPS patient coordinates.

The Annotations checkbox state persists between sessions.


#### 8.3.6  DICOM Overlay Planes

Some images — particularly CT and MR "protocol" series — contain one or more 1-bit bitmap overlay planes embedded in the DICOM file alongside the pixel data (groups 6000–60FE). These planes carry annotations such as scan limits, region-of-interest boundaries, or facility branding burned in at the modality.

When overlay planes are present, dicomqr composites them onto the image in opaque yellow after applying window/level, so they remain visible at any W/L setting. Multiple overlay planes are all composited in the same pass.

The Overlays checkbox in the viewer bottom bar toggles this compositing on and off. The checkbox is hidden for series that contain no overlay planes and appears automatically on the first frame in which overlays are detected. The toggle state persists between sessions.

Note: the obsolete encoding in which overlay bits are packed inside unused bits of the pixel data words (OverlayBitPosition > 0, retired in the DICOM 2004 edition) is not supported and is silently skipped.


#### 8.3.7  Cine Playback and Chapters

A series that contains a multi-frame instance opens in chapter mode: one chapter per instance, with the frame slider scoped to the chapter on screen rather than spanning the whole series. This is how ultrasound studies are meant to be read — an echo study is a set of independently playable cine loops, and a real one holds 88 clips and 85 stills across 173 instances, which flattened onto a single slider is one 5,256-position scrub with no seam between loops. A SPECT reconstruction or projection set, held in a single file, becomes a single chapter that plays as a cine through its slices.

Beneath the ordinary bottom bar, chapter mode adds a transport row and a filmstrip:

| Control | Effect |
|---|---|
| Play / Pause | Plays the chapter on screen as a cine loop. Disabled for a still. Space does the same. |
| Sweep | Plays forward then backward instead of looping back to the first frame. Set automatically when the file asks for it. |
| Rate | Playback rate. Clip rate — the default — is whatever rate the file itself states; the fixed rates override it for that chapter. |
| Previous / Next chapter | Steps to the neighbouring instance. Ctrl+Left and Ctrl+Right do the same. |
| Filmstrip | One thumbnail per chapter — its middle frame — with the chapter's label and frame count. Click one to switch to it. The active chapter is outlined and is scrolled into view as you step through. Shown only when the series has more than one chapter. |

Each chapter remembers its own frame position, rate, sweep setting and whether it was playing, so leaving a loop and returning to it resumes where you were. The frame counter reads `Frame 31 / 72` within the chapter, and the chapter label beside the transport reads `Chapter 3 / 173`.

Playback rate is read from the file — RecommendedDisplayFrameRate, CineRate, FrameTime, FrameTimeVector or ActualFrameDuration, whichever it carries, in that order of preference — and a file stating none plays at 15 fps. Where the file states a preferred playback range (StartTrim / StopTrim), the loop is confined to it while the slider still spans every frame, so trimming never hides frames from you.

Chapter names come from whatever the file actually says: image comments, protocol name or a stated view code where present; failing those, what kind of ultrasound image it is (2D, Colour flow, PW Doppler, and combinations for duplex) taken from the region calibration sequence. Many vendors write none of the naming attributes on echo clips, in which case the thumbnail is what distinguishes one loop from another.

So that playback runs at the rate the clip asks for, the chapter on screen is decoded into memory in the background — a second or so for a long loop, less for a typical one — and plays from there. Buffering progress appears in the frame counter, and you can scrub and play before it finishes. One clip is held at a time; an unusually long one is capped, in which case the counter says which frames the loop is confined to, and the rest of the clip is still reachable by scrubbing.

On NVIDIA GPUs, dicomqr automatically turns off the graphics driver's "Threaded optimization" feature for its own executable at startup (it corrupts rendering when one window animates while others are open, appearing as streaks of light across the other window). This is written once to the NVIDIA driver profile — the same setting reachable manually via NVIDIA Control Panel > Manage 3D Settings > Program Settings — and takes effect at latest from the next launch. If streak artefacts ever appear regardless, check the Activity Log: a warning there means the driver profile could not be written and gives the manual steps.


#### 8.3.8  Structured Report Viewer

Series whose modality is SR (Structured Report), KO (Key Object Selection), AU (Audio), or PR (Presentation State) contain no pixel data and are automatically opened in the document viewer instead of the image viewer.

The document viewer window has three areas:

- Header — patient name, MRN, date of birth, sex and age; study date, accession number and description; modality, series information, content date/time, and the DICOM completion and verification status flags (e.g. COMPLETE · VERIFIED)
- Body — the SR Content Sequence rendered as scrollable formatted text. CONTAINER items appear as section headings; leaf items appear as labelled value pairs. Supported value types: TEXT, NUM (with measurement units), CODE, DATE, TIME, PNAME, UIDREF, IMAGE (shown as a UID reference), and nested CONTAINERs
- Footer — Prev / Next buttons to step through multi-file series; a counter showing the current document position; a Copy text button that places a plain-text version of the document body on the clipboard
The viewer opens at document 1 of the series and loads subsequent documents in the background as you navigate.


### 8.4  Opening in External Viewer

The Open in Viewer button in the bottom bar and the right-click menu item open the node's folder in the configured external DICOM viewer. These controls are disabled when no viewer path is configured in Preferences. Open folder opens the folder in Windows Explorer instead.


### 8.5  Pushing to a PACS

Right-click any node and select Push to PACS…, or select items and click Push Selected…, to send files to a remote PACS via C-STORE SCU.

A dialog appears with a destination selector (any configured server profile), a progress bar and per-file counter, and a Cancel button. The push creates a new association per operation and does not require the PACS tab to be connected.

Files are sent exactly as stored: the association offers each file's own transfer syntax, and when the server accepts it the stored bytes go out verbatim — no re-encoding, byte-perfect, including compressed files when the destination accepts their syntax. If the server negotiates a different syntax instead, the file is converted locally to the negotiated uncompressed syntax (on a temporary copy — the local file is never modified) and sent in that form; a file that cannot be converted is skipped and counted as failed. Every failed file is recorded in the Activity Log with the actual reason.


### 8.6  Deleting Local Files

Right-click any node and select Delete…, or select items and click Delete Selected…, to permanently remove files from disk. A confirmation dialog shows the file count and total size. After deletion, empty directories are pruned and the deleted entries are removed from the tree and its index automatically — no rescan needed.

Warning: Deletion is permanent. Files are not moved to the Recycle Bin.


### 8.7  Selection Controls

Selection works as in the Query Results tree (Section 6.3): a plain click selects one row and deselects everything else, Ctrl+click adds or removes a single row, Shift+click selects the range from the last click, and Ctrl+Shift+click adds that range to the selection. Selecting a patient or study selects every series beneath it — Ctrl+click one of those series to deselect just that series, leaving the rest of the patient or study selected.

| Control | Action |
|---|---|
| Select All | Selects every currently visible (filtered) root node and all its descendants. |
| Clear Selection | Deselects everything. |
| Push Selected… | Pushes all selected files to a chosen server. |
| Modify Selected… | Runs a modification (de-identification) profile over exactly the selected files — choose the profile from the menu that opens above the button. See below. |
| Delete Selected… | Deletes all selected files after confirmation. |

Modify Selected… is how to de-identify and export only some of a study's series: select them, click Modify Selected…, and choose a profile. The right-click Modification submenu always takes the whole patient or study it was opened on; Modify Selected… takes the selection instead, and opens the same Modification window (Section 14.3), whose header says how much of the study or patient is included — for example "3 of 7 series selected". Series chosen from one study make a study-level run, exported exactly as a right-click on that study would be, just with fewer series in it; a selection covering several studies of one patient makes a patient-level run. A selection spanning more than one patient is refused: an export stands in for a single patient under one export folder name, and the profile's set values — a new Patient Name, and the Patient ID that follows it — would give every patient in it the same identity. Run each patient separately instead.


### 8.8  Tag Review and Export (View Tags)

Right-click any node and select View Tags to open the tag review window over exactly that node's files. The window presents the Patient → Study → Series → Instance hierarchy with every DICOM element of each instance, sequences nested item by item. Hovering a row shows the tag's DICOM dictionary entry (tag, name, keyword, VR, VM); private tags, malformed VRs, and Tag Profile colours are styled as configured in Preferences (Section 14.2). The search bar filters the tree, Expand All / Collapse All control the branches, right-click offers Copy row and Copy value, and Ctrl+C copies the selected row.

Right-click an instance row and choose Export Tags… to write that image's complete element list to a CSV or JSON file — this is the application's only export function, deliberately scoped to a single image instance. The export always contains the instance's full tag set: an active search changes what the tree displays, never what is exported. Tags are written as [GGGG,EEEE], each field 4 hexadecimal digits left-filled with 0. CSV produces one row per element with columns Tag, VR, Name, Value; elements nested inside sequences show their path in the Tag column, e.g. [0040,0275] > Item 1 > [0040,0007]. JSON is an array of elements with sequence items nested inside their element. The native save dialog opens directly — the format is chosen with its file-type selector (Save as type). The Default export format preference (Section 14.3) decides which type is listed first and is used when the typed filename has no extension.

Exported tags use square brackets rather than DICOM's conventional parentheses deliberately: Excel silently parses a parenthesized tag like (0020,0001) as the negative number -20,001 (parentheses read as a negative sign, the comma as a thousands separator), while [0020,0001] is imported as plain text in every locale. The file therefore opens correctly in spreadsheets and stays free of formula artifacts in text editors; the on-screen tag tree keeps the standard (GGGG,EEEE) notation.


## 9  Import Tab

The Import tab copies DICOM files from any folder into the organised download folder, applying the same Patient / Study / Series subfolder structure used by retrieval.


### 9.1  Scanning a Source Folder

Type a source folder into the field, or click Browse… to choose one, then click Scan. dicomqr walks the folder and builds a tree of studies and series found in it. The destination folder (the configured download folder) is shown read-only below the source field.

The folder imported from is remembered: it is filled in when the application next starts, and the folder chooser opens there, so importing again from the same disc or department share is a click on Scan. Like every folder chooser in the application, it opens in front of the window that asked for it and starts at the folder currently in the field.


### 9.2  Selecting and Importing

Click rows in the tree to select them, the same way as in the Query Results tree (Section 6.3): a plain click selects one row and deselects everything else, Ctrl+click adds or removes a row, Shift+click selects the range from the last click, and Ctrl+Shift+click adds that range. Selecting a patient or study selects every series beneath it; Ctrl+click one of those series to deselect just that series. Click Import Selected to copy the selected files. Files already present in the destination (same SOP Instance UID at the same destination path) are skipped; the status label reports imported, already-present, and failed counts.

Select All and Clear Selection buttons are provided. The filter bar narrows the tree in the same way as the other tabs.


## 10  Worklist Tab

The Worklist tab queries a Modality Worklist server for scheduled imaging procedures. It operates independently of the PACS Query tab — it does not require a PACS connection and can target a different server (typically a RIS or MWL broker).


### 10.1  Selecting a Worklist Server

Choose any configured server profile from the Worklist server dropdown. The dropdown updates when server profiles are added or removed in Preferences. The query connects to the selected profile's host, port, and AE Title for each query and releases the association immediately after.

Note: the Modality Worklist SOP class (1.2.840.10008.5.1.4.31) must be enabled on the target server. In most environments this is a separate system from the PACS — configure a server profile pointing to that system.


### 10.2  Query Fields

| Field | Description |
|---|---|
| Patient Name | Wildcard-capable patient name match. A trailing `*` is appended automatically. Leave blank to match all patients. |
| MRN | Wildcard-capable Patient ID match. |
| Accession | Wildcard-capable Accession Number match. |
| Modality | Restricts results to one modality. Select (any) to include all modalities. |
| Scheduled date | Today only (checked by default) — restricts to today's scheduled date. Uncheck to select a specific date using the calendar picker. Leave blank (unchecked, no date selected) to return all scheduled dates. |

Click Query Worklist or press Enter in any text field to run the query. Click Clear to reset all fields and clear the results.


### 10.3  Results Table

Results are shown in a table with columns: Patient, MRN, Accession, Date, Time, Modality, Procedure, and Station. Click any row to select it.

Copy Accession and Copy Patient buttons copy the selected row's values to the clipboard. The status label shows the number of worklist items returned, or any error message.


### 10.4  Typical Use Cases

- Verify a scheduled procedure — query by patient name or accession to confirm an order reached the worklist server before the patient arrives at the scanner.
- Diagnose "patient not on scanner" — if a technologist cannot find a patient on the modality's worklist, query here; if the entry appears, the problem is in the scanner's MWL configuration; if it does not, the order was not transmitted to the worklist server.
- Look up an accession number — copy the accession and switch to PACS Query to search for the matching study.

## 11  Downloaded Files

Files are written to the folder specified in the Download to field. Within that folder, dicomqr creates a three-level subfolder structure:

```
<Download folder>\
    <Patient Name> (<Patient ID>)\
        <Study Description> (<Study Date>)\
            <Series Description> (<Series Number>)\
                <SOP Instance UID>.dcm
```

For example:

```
Downloads\
    Doe^John (MRN12345)\
        Chest CT (20240115)\
            Chest W Contrast (2)\
                1.2.840.10008.5.1.4.1.1.2.dcm
```

If a metadata field is absent from the DICOM file, the corresponding folder component falls back to a descriptive placeholder: Unknown Patient, Unknown Study, or Unknown Series. Characters that are not permitted in Windows file or folder names are replaced with underscores.

Each SOP Instance UID is unique, so files from different studies that share the same patient ID and series number are written to separate subfolders and are never overwritten.


## 12  Menus


### 12.1  File Menu

| Item | Description |
|---|---|
| Connect | Connects to the currently selected server profile. |
| Disconnect | Ends the current session and stops the local SCP listener. |
| Preferences… | Opens the Preferences window. See Section 14. |
| Quit | Exits the application. |


### 12.2  Query Menu

| Item | Description |
|---|---|
| Search | Runs the current search. |
| Clear results | Resets all search fields and removes all results from the tree. |
| Retrieve Selected | Starts retrieval of all currently selected tree nodes. |
| Cancel retrieve | Cancels an in-progress retrieval. |


### 12.3  Help Menu

| Item | Description |
|---|---|
| Activity Log… | Opens the in-app activity log (the last 5000 captured lines), always starting at the Errors only view — problems first, verbosity on demand. A step-wise level selector scales the view through Errors + warnings and Activity (one line per meaningful operation) up to Everything (full DICOM protocol detail); a filter box narrows the view to lines containing a substring, and a counter shows how many lines each severity holds. Both filters apply to the display only — capture is always complete, so raising the level retroactively reveals the full detail of something that already happened, and dicom.log always records everything. The list renders only the visible rows, so switching levels is instant even with the ring full; right-click any line to copy it. Buttons: Refresh (manual update), Copy Shown (copies the entire filtered view to the clipboard), Clear. The log auto-refreshes once per second while it is open. It opens as its own window, so it can be moved, resized, or left open on a second monitor while you carry on working in the main window; choosing the menu item again brings the open log to the front rather than opening a second one. |
| About | Displays the application version, build date, and library credits. |
| Client info… | Displays the local AE Title, SCP port, and detected IP address. |


## 13  Keyboard Shortcuts

| Shortcut | Action |
|---|---|
| Ctrl+Enter | Run the current search. |
| Ctrl+F | Move focus to the Patient Name field in the Filters panel. |
| Ctrl+R | Retrieve the currently selected items. |
| Ctrl+C | Copy the full label of the currently selected result row to the clipboard. |
| Esc | Clear the current selection in the results tree. |

In the Query Results, Local Browse and Import trees, clicks select as follows:

| Mouse | Action |
|---|---|
| Click | Select that row (and everything beneath it), deselecting everything else. Clicking the row that is the only selection deselects it. |
| Ctrl+click | Add the row to the selection, or remove it if it is already selected; the rest of the selection is untouched. |
| Shift+click | Select every row from the last click to this one, replacing the selection. |
| Ctrl+Shift+click | Add every row from the last click to this one to the existing selection. |


## 14  Preferences

Open Preferences from File > Preferences…. It is organized into three tabs — SCP & Network, User Interface, and Modification & Export. Changes take effect when Apply is clicked and are written immediately to disk.

Preferences opens as its own window, so it can be moved and resized, but the main window is dimmed and inactive while it is open: Apply writes back a complete snapshot of the settings, so anything changed underneath it would be silently overwritten. Closing Preferences also closes any modification-profile editor opened from it, since profile edits are only committed through Preferences' own Apply button.


### 14.1  SCP & Network Tab

Everything DICOM-network related: the identity this workstation presents, where received files go, and the saved server profiles.

| Setting | Description |
|---|---|
| Local AE Title | The AE Title this workstation presents during DICOM associations. Default: DICOMQR. |
| Local SCP port | The TCP port on which the embedded C-STORE listener accepts incoming connections. Default: 11112. |
| Download folder | The root folder where retrieved and imported DICOM files are written. |
| Retrieve stall timeout (s) | Abort a retrieve when no progress response and no received file arrives for this many seconds. Blank uses the default (120 s); a negative value disables stall detection (e.g. for slow tape archives). See Appendix B. |

Changes to AE Title or SCP port take effect the next time a connection is established.

Server Profiles — lists all saved server profiles. Click Edit to modify, Delete to remove, or Add server… to create a new profile. The Up/Down buttons reorder the list; the first profile is the default selection when the application starts.

Profile editor fields:

| Field | Description |
|---|---|
| Profile name | A descriptive label shown in the server dropdown and the Worklist tab. |
| Remote AE Title | The AE Title of the PACS or MWL server (case-sensitive, uppercase recommended). |
| Host | The hostname or IP address of the server. |
| Port | The TCP port of the DICOM service (commonly 104 or 11112). |
| Info model | `study` — Study Root (default, most common). `patient` — Patient Root. `patient-study-only` — legacy retired model; SERIES-level lazy-load is suppressed automatically. |
| Retrieve method | C-MOVE / C-GET / Auto — see Section 4.1. |
| Connect timeout | Seconds before a connection attempt is considered failed. |
| Transfer syntax | 'As stored' or one of the two guaranteed uncompressed syntaxes (converted locally on receipt when the server does not send it) — see Section 4.1 for full details and caveats. |


### 14.2  User Interface Tab

Appearance:

| Setting | Description |
|---|---|
| Colour theme | Selects the colour theme pack: Default (the stock Fyne palette), Adwaita (the GNOME colour specification), or one of the four Catppuccin flavours — Latte (light), Frappé, Macchiato, and Mocha (dark). Default and Adwaita respond to the Light/Dark choice below; the Catppuccin flavours are fixed palettes, so the Theme radio is disabled while one is selected. |
| Theme | Selects the light or dark variant of the colour theme (Default and Adwaita packs only). |
| Tree font | Selects the font used for results tree rows. Select (default) to use the application's built-in font. |
| Selection colour | The colour applied to selected rows. Click Choose colour… to open a colour picker. If unset, selected rows follow the theme's primary accent colour. |
| Selection style | The font style applied to selected rows: Bold and/or Italic. |

Image Viewer:

| Setting | Description |
|---|---|
| External viewer | Full path to an external DICOM viewer executable. Click Browse… to locate it, or Auto-detect to search for MicroDicom or RadiAnt DICOM Viewer in the standard installation locations. When left empty, the Open in Viewer buttons and menu items are disabled. |

Tag Highlights — styling rules applied in the tag review window (Local Browse right-click > View Tags):

| Setting | Description |
|---|---|
| Private tags | When Italicize is checked, private (odd-group) tags are rendered in italic. |
| Malformed tag | The colour applied to tags whose value representation violates the DICOM standard. Default: red. |

Tag Profiles — named tag sets coloured in the View Tags window. Each profile has a name, a colour, an enabled checkbox, and a tag list (one GGGG,EEEE per line in the editor). The first enabled profile containing a tag determines its colour; the malformed-tag highlight always takes precedence. The default PHI profile colours protected-health-information tags orange. The JSON wire format matches the dicomhdr application, so profile blocks can be copied between the two tools' settings files.


### 14.3  Modification & Export Tab

Modification Profiles — the de-identification recipes applied from the Local Browse right-click Modification submenu or its Modify Selected… button. Profiles are stored in `%USERPROFILE%\.dicomqr\profiles.json` in the same format as the dicomtool CLI, so profile files can be copied between the two tools (with deliberate divergences: the dicomqr-only settings — Zip export, Flat export, Output transfer syntax, the export folder name, pixel masking, Remove overlay planes, and Ignore SOP classes — are ignored by dicomtool, and dicomtool's maskrows parameter is not supported by dicomqr). The list shows each profile with its set/remove counts, base profile, and per-modality override count; Edit and Add profile… open the profile editor. Changes are committed to profiles.json only when Apply is clicked, and only when something actually changed — a hand-edited file is never rewritten gratuitously. If profiles.json cannot be parsed, the list is replaced by an explanatory message and Apply leaves the file untouched.

Deleting a profile that other profiles use as their base prompts for confirmation; renaming a profile automatically updates the base reference in profiles that inherit from it.

The profile editor opens as its own window, so it can be moved aside and resized — useful when working through a long removal list. It is not modal: Preferences stays usable behind it, choosing Edit again on a profile already open raises that window rather than opening a second copy of it, and closing Preferences closes any editor still open, since edits are only committed through Preferences' own Apply button.

Profile editor fields:

| Field | Description |
|---|---|
| Profile name | The name shown in the Modification submenu and used as the base reference by inheriting profiles. |
| Base profile | Another profile whose settings this one inherits and overrides: override wins for scalar values, removals are a union, and this profile's Keep entries subtract from the merged removal list. |
| Set values | The tags whose values are replaced, one row each: the tag on the left, the value on the right, listed in the order the profile stores them — tags added through the picker join at the end. Choose tags… opens the tag picker to add rows (and, by unchecking, remove them). An empty value blanks the element rather than setting it. A value of [GGGG,EEEE] copies whatever this profile sets that tag to — see below. Values are checked against the tag's DICOM value representation on save, so a date field will not accept a word. |
| Remove tags | Tags deleted from every file — one per line, each shown with its name (0008,0080  Institution Name). Choose… opens the tag picker (see below) instead of typing tag numbers. |
| Keep tags | Tags retained even when the base profile removes them, shown in the same way. Choose… opens the tag picker. |
| Birth date mask | 8-character positional pattern applied to Patient Birth Date: digit positions replace, any other character preserves the original digit (e.g. YYYY0101 keeps the year and sets January 1st). Empty = no masking. The mask rewrites the Patient Birth Date at the top of the file and nothing else — see the note below about Original Attributes Sequence. |
| Remap UIDs | Replace every site-generated UID with a fresh consistent value — the same source UID always maps to the same replacement within a run, keeping cross-references intact. Standard and structural UIDs are never touched. This replaces the retired UID suffix option: a profile still carrying a uid entry — hand-authored, or shared from dicomtool — is refused when run, with a message naming Remap UIDs as the replacement. The entry itself is preserved in profiles.json and disclosed by the editor, never silently dropped: quietly ignoring it would export original UIDs from a profile whose author asked for them changed. |
| Remove private tags | Delete all private (odd-group) tags. |
| Remove overlay planes | Delete every overlay-plane group (6000–60FE). Overlay Data is a bitmap drawn over the image, and some equipment burns patient text into it; Remove private tags never touches these groups (they are even-numbered) and pixel masking writes only the image's own pixels, so this option is the one way to clear them. Off unless asked for, so a profile shared with dicomtool keeps its meaning. |
| Shift dates (days) | Shift every DA (Date) and DT (DateTime) field by this many days — negative, zero, or positive, at any nesting depth. DT time/fraction/timezone suffixes are preserved, and Patient Birth Date is never shifted (the birth date mask is that field's dedicated control). |
| Fix VR | Handling of value-representation violations: (off), correct, skip, or passthrough. |
| Output transfer syntax | The transfer syntax exported files are written in. As stored (the default) copies each file's own encoding through untouched; Explicit VR Little Endian and Implicit VR Little Endian convert it, decompressing compressed pixel data on the way out. Only these two uncompressed targets are offered — the application can decompress but never compress. A file whose compression it cannot decode is reported as a failure rather than exported in the wrong syntax. |
| Zip export | Pre-check the Modification dialog's Zip export option for runs with this profile — the run is written as a single <export folder name>.zip. The dialog checkbox still decides per run. |
| Flat export | Pre-check the Modification dialog's Flat export option for runs with this profile — every file is written directly into the export root (or the archive root, with Zip export also set), with no patient/study/series folders, and named after its SOP Instance UID rather than kept under its source name. The dialog checkbox still decides per run. |
| Ignore image types | Comma-separated values; a file whose ImageType (0008,0008) contains any of them is skipped entirely. |
| Ignore modalities | Comma-separated values; a file whose Modality (0008,0060) matches one is skipped entirely. |
| Ignore SOP classes | Comma-separated SOP Class UIDs; a file whose SOP Class UID (0008,0016) matches one is skipped entirely. This is the filter for scanned documents, dose and protocol pages and saved screens: they arrive as Secondary Capture objects (`1.2.840.10008.5.1.4.1.1.7`, or its multi-frame variants `.7.1` to `.7.4`) yet carry the study's imaging modality, so neither filter above sees them — one scanned form in the field carried no Image Type at all. The line beneath the field names each class typed, or says that an entry is not a UID. Also available inside a per-modality override, where it adds to the profile's list for that modality alone. Note what else is Secondary Capture: fused PET/CT renders, MIP cines, MR Care Bolus monitoring frames and an ultrasound study's measurement screens are all dropped by the same entry, and nothing in the header tells them from a document. |
| Pixel masking | Areas of the image itself blanked in the export — see below. Add region adds a row; each row is a rectangle in percentages of the image, or the Outside ultrasound region rule, which takes its geometry from the file. |

The birth date mask and Original Attributes Sequence. The mask replaces the Patient Birth Date recorded at the top of the file. That is all an ordinary image carries — patient details are not repeated inside the nested structures that make up the rest of a DICOM file. There is one exception. A study that has already been de-identified somewhere else may carry an Original Attributes Sequence (0400,0561), which is a record of what that earlier process changed, and it holds the values as they were before. A real birth date can sit in that record while the visible Patient Birth Date reads as masked.

Both the profile editor and the Modification dialog show a note beside the Birth date mask when a profile masks the birth date without removing 0400,0561, naming the tag to add to Remove tags. It is advice, not an obstacle: most studies carry no such sequence, and neither the editor nor a run is blocked. The shipped profiles already remove it, so the note appears only on a profile written by hand or brought over from dicomtool. After a run, the completion summary reports how many exported files actually still carried a birth date inside a sequence — counted from the files themselves rather than inferred from the profile, so it is a statement about that export rather than a warning about a possibility.


#### 14.3.1  Masking Burned-In Patient Information

Some images carry patient identity in the pixels rather than in the tags: ultrasound machines print a banner across the top of every frame, and secondary captures are often photographs of a screen that included one. No tag rule can reach that text — removing Patient's Name from the header leaves the name that was drawn into the image. Pixel masking blanks those areas in the exported copy. The files in the download folder are never altered.

A region is either a rectangle or the Outside ultrasound region rule.

A region also carries how far it reaches. One defined in the profile editor applies to every image, which is what a profile-wide rule such as "blank the top 8%" should do. One drawn in the review window during a run (Section 8.9) can instead be limited to a single image, to the series being reviewed, or to one chapter within it — necessary because the images that need a hand-drawn rectangle are usually the ones whose layout is unique, and a rectangle that suits one of them will cover something important on the next.

A rectangle is given as percentages of each image's width and height — x and y locate its top-left corner, w and h its size — so 0, 0, 100, 8 blanks the top 8% of every image regardless of its resolution. Percentages rather than pixel coordinates matter because one study routinely mixes sizes: a rectangle measured on an 800×600 loop would miss the banner on a 1024×768 capture in the same export. Rectangles are rounded outward when they land between pixels, on the principle that half a row of leftover text is still identifiable.

Outside ultrasound region takes its geometry from the file instead. Ultrasound images declare the calibrated region their image data occupies, and the vendor's banner is always outside it — so this one rule masks the banner on files whose layout differs, without measuring anything. A study with two calibrated panes side by side (a duplex measurement) masks around both together, keeping the gap between them. A file of any other modality is not what the rule describes and passes through it untouched, so the rule is safe to leave in a profile used on mixed studies.

Ultrasound images that declare no calibrated region are usually analysis or measurement screens — a worksheet of quantitative results rather than a captured image — and those are often exactly what a de-identified dataset needs to keep. Where a rectangle applies to such a file, it masks the file instead of the rule that could not be resolved: the banner goes, the measurements stay, and the file is exported. The substitution is counted in the run's completion message and logged per file, because those images were masked by geometry measured elsewhere rather than by their own stated layout. A file with no rectangle applying to it has nothing to fall back on, and is reported as a failure rather than exported with its banner intact — unless it has been marked as needing no masking in the review window (Section 8.9), which is how an image that carries no patient information is exported untouched.

Drawing a rectangle instead of measuring one. Pick from image… beside Add region opens any DICOM file and shows it full size; drag across the image to mark an area, Undo last removes the most recent, and Apply replaces the profile's rectangles with what is on screen (Outside ultrasound region rows are left alone, having no geometry to draw). Rectangles snap to the image's own pixel grid, so what is stored is the area marked, to the nearest pixel, expressed as percentages that then apply to every image size in the study. The window also states what the file declares about itself — its size, its modality, and for ultrasound whether it carries a calibrated region, which tells you whether a rectangle is needed for that file at all.

Reviewing and masking a run. The Modification dialog's Pixel masking section has a Review masking… button, and for a study with burned-in annotation this is where the work is done. It reads the header of every selected file and presents them series by series, in acquisition order — for an ultrasound study that is clip by clip, each clip shown at its middle frame. Every control sits beneath the image, in the order the work is done: the slider scrubs the images of the current series, with < and > beside it stepping a single image; < Series and Series > step to another series (named beside them), and the row under those says which image is on screen. The arrow keys also step a single image and Ctrl with an arrow key a series, for review that needs to be precise rather than quick. Each image is shown with the areas that will be blanked drawn in black, resolved by the same code the export uses, so what is shown is what will happen. Files that cannot be read, or that hold no image, are counted in the window's caption rather than silently left out.

Two things keep a large run from having to be combed by hand. A counts line beside the image position tallies the whole run by outcome, in the same colours the filmstrip stripes use — how many images are masked, masked by fallback rectangles, marked as needing none, would fail the export, or are untouched — recomputed as regions are drawn and undone, so it always states what Use these regions would produce. And a Next failure button jumps straight to the next image that cannot be masked and would fail the export, in whatever series it sits, wrapping past the end of the run — the count on the button reaching zero is the sign the run exports clean. The button is the one flag failing images get in a series without a filmstrip, where there is no red stripe to spot.

A series that contains clips also gets the viewer's filmstrip, between the image and the slider: one thumbnail per image of the series, decoded in the background, with the current image outlined — click any thumbnail to jump straight to it, exactly as in the viewer. A coloured stripe along the bottom edge of each thumbnail says what this run's masking will do to that image, resolved by the export's own rules and updated as regions are drawn or undone: green means the image is masked; amber means it is masked by the profile's rectangles standing in for a calibrated ultrasound region it does not declare, and is worth a look; blue means it has been marked as needing no masking; red means it cannot be masked and would fail the export; no stripe means nothing applies and the image exports untouched. For an echo study this is the quick pass the window exists for — the amber and red cells are the analysis screens needing a hand-drawn rectangle or an exemption, visible at a glance instead of found by scrubbing.

Tick Draw rectangles to mark an area on the image in front of you: drag across it and the rectangle joins the regions this run will apply (dragging with the box unticked does nothing, and the note beneath the image says so). How far the rectangle reaches is set by Applies to beside it — the middle option reads Current chapter or Current series, whichever applies to the series being reviewed:

| Applies to | Effect |
|---|---|
| This image only | The image on screen and no other. The default, and the right choice for an analysis or measurement screen: the next screen is laid out differently, and blanking the same area there would destroy report content the export exists to keep. |
| Current chapter | Shown for a series with separate chapters to tell apart — the ones that get their own filmstrip, such as an ultrasound series mixing clips and stills. The rectangle reaches every image of this image's modality and pixel dimensions within the one chapter being reviewed, and nowhere else: a rectangle drawn on one clip is not a statement about a different view that happens to share its geometry. |
| Current series | Shown for a series with no separate chapters (a plain single-frame slice stack, such as most CT or MR series). The rectangle reaches every image of this image's modality and pixel dimensions across the whole series — there a repeating layout is a statement about the acquisition as a whole — but never another series or the rest of the run. For ultrasound it also matches on whether the file states its own image region, because calibrated loops and analysis screens are masked by different means. |
| All images | Every image in the run, whatever its modality or size. The same reach a rectangle defined in the profile editor has. |

Undo takes back the most recent thing added in this session, wherever it went; it never removes a region the profile arrived with. Needs no masking records that the image on screen (or its group, following the same Applies to setting) was reviewed and requires no mask — which is what lets an ultrasound image with no stated region be exported instead of failed. Step to the next image to see whether it is covered too: that loop, draw and check, is the point of the window. Use these regions applies everything marked to this run; Cancel discards it — asking first when the session has drawn regions or exemptions to lose, so a stray click cannot take minutes of review with it.

Regions marked here apply to this run only, like every other control in the Modification dialog — nothing is written back to the profile. Where the same layout recurs, for instance because a department's machines always print the same banner, define it once in the profile editor (Section 14.3) so every run starts with it in place.

Regions may also be set on a per-modality override, where they replace the profile's regions for files of that modality rather than adding to them — the layout of an ultrasound frame has nothing to do with the layout of a secondary capture, so there is nothing to combine.

Two consequences worth knowing before enabling masking. Because writing pixels requires them to be uncompressed, a compressed file is decompressed on export whenever masking applies to it — even when Output transfer syntax is As stored — and then re-encoded so the export stays compressed. When the file's own compression is lossless (JPEG 2000 Lossless or JPEG Lossless), the masked image is recompressed straight back into that same syntax. When it is lossy (JPEG Baseline, JPEG Extended or lossy JPEG 2000), the masked image is re-encoded to JPEG 2000 Lossless instead: re-entering the lossy format would degrade every pixel in the image a second time, while the lossless encode adds nothing beyond the decode masking already required — the transfer syntax changes, the image does not, and the file stays a fraction of its uncompressed size. In both cases the result is decoded again and verified bit-for-bit against the masked pixels before it is written; a file whose re-encode fails leaves as Explicit VR Little Endian. All three counts are reported in the run's completion message and the Activity Log. And because a mask that fails to apply is indistinguishable, in the finished export, from a profile that never asked for one, any file that cannot be masked is reported as a failure and left out of the export rather than written with the annotation intact.

Masked areas are filled with black as the image's own photometric interpretation defines it, so a redaction reads as a redaction: the maximum stored value on an inverted greyscale image, neutral chroma on a colour-difference image, and the darkest entry of the palette on a palette-colour image. The one format that cannot be masked directly is uncompressed chroma-subsampled ultrasound, whose samples are not stored one set per pixel; setting Output transfer syntax to either uncompressed option rewrites it in a form that masks normally, and the error says so.

Per-modality overrides — the editor lists each override with Edit/Delete buttons and an Add modality override… button. An override varies only what is genuinely modality-specific, so its dialog offers the modality code, the set/remove/keep tag lists, Keep private tags, and Ignore SOP classes; on a file whose Modality matches, these layer on top of the profile. Ignore SOP classes here adds to the profile's SOP class filter for that modality alone — the way to skip Secondary Capture for CT, MR, NM and PT while an ultrasound study's measurement screens, Secondary Capture too, stay for the mask review. Keep private tags cancels the profile's private-tag removal for that modality — it is meaningful only here, which is why the main editor has no such control. Everything else (birth date mask, date shift, remove private tags, remove overlay planes, fix VR, output transfer syntax) is a profile-wide decision and is set once in the main editor.

Tag picker — the Choose… button beside Remove tags and Keep tags opens the whole DICOM dictionary in its own window, as a tree of groups, each group opening to its tags with a checkbox. The window can be moved and resized, which is worth doing when working through a long list; the profile editor behind it is dimmed while it is open, because the picker takes a copy of the field's contents as it opens and anything typed into that field behind it would be overwritten on Apply. The search box matches on tag name, keyword or number ("patient", "0010", "InstitutionName" all work) and automatically opens the groups holding matches; Hide retired omits tags the standard has withdrawn. Apply writes the result back.

The field's current tags open already checked. Because a selection scattered through 5,000-odd tags would be invisible behind collapsed groups, a field that already has entries opens with Show selected only ticked, listing just those tags; clear that box to browse the full dictionary and add more. Every group header also carries its own tally — "0010  Patient  (72, 3 selected)" — so a collapsed group still shows that part of the selection is inside it. Unticking a tag in Show selected only leaves its row in place until the view is rebuilt, so a mis-click can be undone without searching for the tag again.

The Remove tags and Keep tags lists show every entry in one consistent form — a zero-padded four-digit group and element with the tag's name beside it, "0008,0080  Institution Name" — matching the tag picker. The name is shown for readability and is not stored; saving keeps the tag number alone.

Tags may be typed in any form — 8,80 and 0008,0080 are the same tag — and are stored in the padded form. This is safe because a profile's Keep list cancels its base profile's Remove list by comparing the tags themselves rather than the text spelling them, so the two files do not have to agree on how a tag is written for the inheritance to work.

A profile may name tags the application's DICOM dictionary does not list — the shipped base-deident profile names three. Those have no row to tick, so the counter reports them separately and they are written back exactly as they appear in the field.

One thing the picker deliberately does not do: checkboxes are on individual tags only, never on a group. The profile format has no group wildcard, so checking a group would expand into hundreds of separate entries — group 0018 alone holds 895 tags — and removing an entire group is nearly always wrong, since group 0028 carries Rows, Columns and Bits Allocated and group 0008 carries the SOP Instance UIDs. Bulk selection is offered only over an active search, where the scope has been stated.

Linking one set value to another. A set value written as a tag in square brackets — [0010,0010] — takes whatever this same profile sets that tag to. The shipped base-deident profile uses it to tie Patient ID to Patient Name: enter a new name in the Modification dialog and the ID follows it as you type, until you edit the ID directly, after which it is yours. Any field can reference any other this way, and several fields may reference the same one.

A reference only ever reads values this profile sets. It cannot read the value in the file being modified — that would copy the real patient name forward into whichever field referenced it, once per file and invisibly, which is the opposite of what a de-identification profile is for. References also do not chain: pointing at a field that is itself a reference is reported as an error rather than followed.

The verbose flag, and any other hand-authored setting without a control, is preserved unchanged so a dicomtool-authored profile survives a round-trip through the editor. Both editors list such values in an italic note rather than hiding them: in an override the note distinguishes settings the modification engine ignores inside a per-modality block from profile-wide options that a hand-authored block still applies.

Defaults:

| Setting | Description |
|---|---|
| Default output folder | Where modification exports are written. When set, the Modification dialog uses it directly — no folder picker appears; the dialog's Change… button overrides it for a single run. When empty, the dialog asks on the first run and saves that choice here. Must be outside the download folder — modified files are never mixed into the local index. |
| Default export format | The file type listed first in the View Tags Export Tags… save dialog, and the format used when the typed filename has no extension (Section 8.8). |

Running a modification (Local Browse right-click > Modification > profile, or Modify Selected… > profile for just the selected series — Section 8.7) opens a confirmation window showing what the profile will do, with everything on it editable for that run alone. The window can be moved and resized, and its contents scroll, so a profile with a long list of set values or removed tags still shows its Modify… and Cancel buttons. The export is written to <output folder>\<export folder name>, and the export folder name is entered here. Its default comes from the profile: base-deident names the export after the new patient name, so typing a name into the Patient Name set field fills the Patient ID and the export folder name with it as you type. A profile that names no export default falls back to a profile-name-plus-timestamp suggestion. Any of these fields stops following the moment it is edited directly, so a suggested value can simply be overtyped. The export folder name replaces the original patient folder — and, for a Study-level run, the study folder too — because a patient folder always identifies someone. Folders below it (the study folder on a Patient-level run, and every series folder) keep their original names, unless the profile deletes or replaces the value a name is built from — a removed Study Description, a replaced Series Description, a shifted Study Date — in which case just that folder is rebuilt from the new value, following the same naming the download folder itself uses. A file whose SOP Instance UID is remapped is renamed to match, so no original UID survives the export either.

The dialog's Options section shows the effective settings and lets any of them be changed for this run only — nothing typed there is written back to the profile. It presents the same controls in the same order as the profile editor's Options section, Output transfer syntax included, so an export can be converted (or left as stored) without editing the profile it came from. The three controls positioned differently are Zip export, Flat export and Include DICOMDIR, which sit in this dialog's Export section beside the output folder and export folder name they change.

Converting on export, rather than on retrieve. A transfer syntax can be required in two independent places, and they answer different questions. The server profile's Transfer syntax (Section 4.1) constrains what a retrieve is allowed to receive, and converts on the way in; the modification profile's Output transfer syntax converts on the way out, when files are exported. Setting the server profile to 'As stored' and the modification profile to an uncompressed syntax keeps the download folder in the archive's own encoding — the original bytes, retrieved once — and pays the conversion cost only for the files actually exported. A file that will not convert is then one reported failure in an export that otherwise completes, instead of an object dropped during a retrieve. The trade-off is that only the retrieve side can stop a PACS sending something the application cannot read at all (JPEG-LS, RLE, MPEG); with no requirement in the server profile, such files can reach the download folder, where they can be neither displayed nor converted on export.

The Pixel masking section lists the areas of the image this run will blank (Section 14.3.1), and its Review masking… button opens the images themselves to check that coverage and add to it. The section is always present, even for a profile that masks nothing: a study whose analysis screens carry a patient banner is exactly the case where the absence of masking is what needs to be noticed. A profile whose per-modality overrides carry their own regions says so beneath, since those replace the ones listed rather than adding to them.

If any file fails during a run, the progress dialog is replaced by a dialog listing what failed and why — a file missing from an export is otherwise easy to miss, and a failed conversion means the export is incomplete. Up to twenty failures are listed by name; the Activity Log holds the full list. Every other file in the run is still exported.

Checking Zip export in the dialog (pre-checked when the profile's Zip export option is set) writes the run into a single compressed <output folder>\<export folder name>.zip instead of a folder, with the same PHI-safe layout inside the archive. The archive is assembled as a hidden temporary file and renamed into place when the run finishes, so a cancelled run keeps the files completed before the cancel, while a run that writes nothing — or fails while finalizing the archive — leaves no zip behind. An existing zip of the same name is replaced after confirmation.

Checking Flat export (pre-checked when the profile's Flat export option is set) writes every file directly into the export root — or, with Zip export also checked, into the archive root — instead of the usual patient/study/series folders. Files are named after their SOP Instance UID rather than kept under their source name, since the folder a source name relied on for context and uniqueness no longer exists to disambiguate it; with Remap UIDs also on, that is the new UID, so no original UID appears anywhere in a flat export. This is the shape a CD/DVD-burning workflow or a database-free viewer typically expects, and it composes with Include DICOMDIR below — the index still lists every file correctly, just with a single-component path to each.

Checking Include DICOMDIR (pre-checked when the profile's Include DICOMDIR option is set) adds a DICOMDIR index to the export — a standard PS3.10 File-set directory listing every exported file's patient, study, series and instance, which lets a DICOM viewer or a CD/DVD-burning workflow browse the export without a database. It is built once the run finishes, from whichever files it actually wrote, so a cancelled or partially-failed run still gets an index of what it did write. With Zip export also checked, the index is written as a DICOMDIR entry inside the archive rather than a separate file. Unlike a failed conversion or a masking failure, a DICOMDIR that could not be written never marks the run as failed — the exported DICOM files themselves are unaffected — but the completion summary says so.


## 15  Status Bar

The status bar at the bottom of the window provides real-time feedback. A coloured LED indicator (gray / amber / green) precedes the status text.

| Situation | Status bar text |
|---|---|
| Application started, not connected | `v1.21.0` |
| Connecting to server | `Connecting…` |
| Connected | `Connected: <AE>@<host>:<port>` |
| Connection cancelled | `Connection cancelled` |
| Connection failed | `Connection failed: <reason>` |
| Disconnected | `Disconnected` |
| Query in progress | `Querying…` |
| Loading results into the tree | `Loading results… <N>/<total>` |
| Query complete | `Query complete — <N> studies` |
| Query error | `Query error: <reason>` |
| Retrieve starting | `Starting retrieve of <N> studies…` |
| Retrieve in progress | `Retrieving study <N>/<total>…` |
| File received | `Received: <file path>` |
| Retrieve complete | `Retrieved <N> files successfully` |
| Retrieve complete, objects skipped (required syntax) | `Retrieved <N> files successfully — <M> unconvertible object(s) skipped, see Activity Log` |
| Retrieve complete, objects undeliverable (required syntax) | `Retrieved <N> files successfully — <M> not delivered by the server, see Activity Log` |
| Finishing conversions (required syntax) | `Converting <N> received file(s) to <syntax>…` |
| Retrieve complete, converted files not saved (required syntax) | `Retrieved <N> files successfully — <M> received file(s) could not be saved after conversion, see Activity Log` |
| Retrieve failed — nothing deliverable in the required syntax | `Retrieve failed — the server could not deliver any of <M> object(s) in <syntax>` |
| Retrieve complete with warnings | `Retrieved <N> files (<X>/<total> targets had errors — see log)` |
| Retrieve cancelled | `Retrieve cancelled` |
| C-ECHO test passed | `C-ECHO success` |
| C-ECHO test failed | `C-ECHO failed: <reason>` |

The SCP status indicator row in the connection panel shows:

| Situation | SCP indicator text |
|---|---|
| Not connected | SCP: not running |
| SCP listening | SCP: listening on 0.0.0.0:<port> (AE: <title>) |
| SCP failed to start | SCP: error — <reason> |


---


## Appendix A  Application Settings

Application settings are persisted to `%USERPROFILE%\.dicomqr\settings.json`. This file is created automatically on first launch with the compiled-in defaults shown below.

| JSON key | Default | Description |
|---|---|---|
| `darkTheme` | `false` | Colour theme. false = Light, true = Dark. |
| `fontName` | `""` | System font for result tree rows. Empty = built-in font. |
| `localAETitle` | `"DICOMQR"` | The AE Title presented during DICOM associations. |
| `localSCPPort` | `11112` | TCP port for the embedded C-STORE listener. |
| `downloadDir` | `""` | Absolute path of the download folder. Defaults to ~/DICOM Downloads. |
| `modifyOutputDir` | `""` | Default output folder for modification exports, used directly by the Modification dialog. Must be outside the download folder. Empty = the dialog asks once and saves the choice here. |
| `exportFormat` | `"csv"` | File type listed first in the View Tags Export Tags… save dialog ("csv" or "json"); also the format applied when the typed filename has no extension. |
| `viewerPath` | `""` | Full path to an external DICOM viewer executable. Empty disables the Open in Viewer controls. |
| `selectionColor` | `""` | Colour applied to selected tree rows (RRGGBBAA hex). Empty follows the theme primary colour. |
| `selectionBold` | `true` | Whether selected rows are drawn in bold. |
| `selectionItalic` | `false` | Whether selected rows are drawn in italic. |
| `windowWidth` | `0` | Saved window width in pixels. 0 uses the default; updated automatically on close. |
| `windowHeight` | `0` | Saved window height in pixels. |
| `retrieveStallTimeoutSec` | `0` | Abort a retrieve when no progress response and no received file arrives for this many seconds. 0 uses the default (120 s); -1 disables stall detection. Recovers from PACS servers whose C-MOVE agent hangs on non-image objects (SR/PR). Editable in Preferences > SCP & Network. |
| `uiTheme` | `""` | Colour theme pack: empty (stock theme), `adwaita`, `catppuccin-latte`, `catppuccin-frappe`, `catppuccin-macchiato`, or `catppuccin-mocha`. |
| `italicPrivate` | `true` | Render private tags in italic in the View Tags window. |
| `malformedColor` | `"E54545FF"` | RRGGBBAA colour for tags whose value representation violates the standard. |
| `tagProfiles` | PHI profile | Array of tag-highlight profiles (name, colour, enabled, tag list) — see Section 14.2. Wire format matches dicomhdr. |
| `profiles` | `[]` | Array of saved server profile objects (see below). |

Each entry in the `profiles` array:

| JSON key | Description |
|---|---|
| `name` | Display name of the profile. |
| `remoteAETitle` | AE Title of the PACS or MWL server. |
| `host` | Hostname or IP address. |
| `port` | TCP port. |
| `infoModel` | `"study"`, `"patient"`, or `"patient-study-only"`. |
| `retrieveMethod` | `"MOVE"`, `"GET"`, or `"AUTO"`. Omitting defaults to C-MOVE. |
| `connectTimeout` | Connection timeout in seconds. 0 uses the default (10 s). |
| `transferSyntax` | `""` (as stored, default), `"explicit-le"`, or `"implicit-le"`. The non-empty values guarantee every retrieved file is stored in that syntax: negotiation offers it first plus the locally decodable syntaxes, files arriving in any other accepted syntax are converted on receipt, and objects that can be neither delivered nor converted are skipped and reported while the retrieve continues. Re-retrieves replace existing on-disk copies whose transfer syntax differs from the required one. |
| `ensureUncompressed` | Obsolete (v1.7.0 only) and ignored: the local-decompression guarantee was replaced by strict single-syntax negotiation via `transferSyntax`. |
| `transferUncompressed` | Deprecated (pre-v1.7). When true it is migrated on load to `transferSyntax: "explicit-le"`. |

The Annotations and Overlays toggles are stored in the application's Fyne preferences (not in settings.json) and persist automatically between sessions.


---


## Appendix B  PACS Configuration Notes

AE Title registration — The PACS must have a record of the local AE Title (default DICOMQR) associated with the workstation's IP address and SCP port. Look for "Known Destinations", "Remote AE Configuration", or similar.

C-MOVE destination — For file delivery the PACS must be configured to push files to the local SCP address. The workstation must be reachable at the IP and port shown in Help > Client info…

Windows Firewall — An inbound rule permitting TCP connections on the SCP port (default 11112) is required.

Information model — If queries return no results, try changing the Info model in the server profile. Some PACS require Study Root, others Patient Root. A small number of legacy systems require the Patient/Study Only model (patient-study-only).

Worklist server — The Modality Worklist SOP class is typically served by a RIS or dedicated MWL broker, not the PACS itself. Create a separate server profile pointing to that system and select it in the Worklist tab.

Compressed pixel data — the built-in viewer decodes JPEG Baseline, JPEG Lossless, and JPEG 2000. Downstream consumers that require `1.2.840.10008.1.2` / `1.2.840.10008.1.2.1` files should require an uncompressed transfer syntax in the server profile: every file on disk is then guaranteed to be in the selected syntax — sent that way by the PACS, or converted on receipt from JPEG Baseline/Extended, JPEG 2000, or the other uncompressed VR — and re-retrieves replace older copies stored in a different syntax. Objects stored in a format the receive path cannot convert (e.g. JPEG-LS, JPEG Lossless, RLE) are skipped and reported in the Activity Log while the rest of the study is retrieved; if nothing at all can be delivered in a negotiable syntax, an error dialog names the required syntax — switch back to 'As stored' and use the external viewer integration for such data.

IPv4 connectivity — dicomqr listens on an IPv4 socket only. Ensure the address shown in Help > Client info… is the correct IPv4 address on the same network as the PACS.

Retrieve stalls on non-image series — some PACS servers' C-MOVE agents fail while sending objects without pixel data (Structured Reports, Presentation States, encapsulated PDFs): the association stays open but no further data ever arrives. dicomqr detects this — if no progress response and no received file arrives for 120 seconds (configurable via the Retrieve stall timeout in Preferences > SCP & Network), the retrieve is aborted with an explanatory message rather than hanging forever. If a server does this repeatedly, set the profile's Retrieve method to C-GET or Auto — the same servers usually deliver non-image objects correctly over C-GET. For genuinely slow servers (e.g. tape archives), raise the timeout, or set it negative to disable stall detection.


---


## Appendix C  Credits and Acknowledgements

dicomqr is a human–AI collaboration. Credit is given by role, reflecting how the work was actually divided.


### Architecture & Design

Jeffrey Leal <jeffrey.leal@gmail.com>

https://github.com/jeffrey-leal


### Implementation

Claude by Anthropic (https://anthropic.com)

Application code and documentation


### DICOM Standard Reference

Protocol implementation follows the DICOM Standard published by NEMA:

DICOM PS3 (2024b) — https://dicom.nema.org/medical/dicom/current

Sections referenced:

- PS3.4 — Service Class Specifications (Query/Retrieve C.4; Modality Worklist K.4; Storage B.5)
- PS3.7 — Message Exchange (DIMSE-C services: C-ECHO, C-FIND, C-MOVE, C-GET, C-STORE)
- PS3.8 — Network Communication / DICOM Upper Layer Protocol

### Open-Source Libraries

| Library | Author / Maintainer | Licence | Purpose |
|---|---|---|---|
| fyne.io/fyne/v2 v2.7.3 | Fyne.io contributors | BSD 3-Clause | GUI framework |
| algm/go-netdicom v0.1.0 | Alan Griffin (fork of grailbio) | BSD 3-Clause | DICOM networking (C-ECHO, C-FIND, C-MOVE, C-GET, C-STORE SCP/SCU, Worklist) |
| grailbio/go-netdicom | Yasushi Saito / GRAIL Inc. | BSD 3-Clause | Original DICOM networking library (base of go-netdicom fork) |
| grailbio/go-dicom | GRAIL Inc. | Apache 2.0 | DICOM dataset encoding / file header writing |
| suyashkumar/dicom v1.1.0 | Suyash Kumar | MIT | DICOM file parsing, image rendering, annotation extraction |
| sqweek/dialog | sqweek | ISC | Native Windows file/folder picker dialogs |

A vendored copy of `algm/go-netdicom` is included under `thirdparty/go-netdicom` with its original BSD 3-Clause licence intact.

