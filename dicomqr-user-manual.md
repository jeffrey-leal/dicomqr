# dicomqr

**User Manual  v1.1.0**

June 17, 2026

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
- Browse local DICOM files in the download folder; push them to any PACS via C-STORE or delete them
- Preview DICOM images in the built-in viewer with W/L windowing, DICOM annotation overlays, and study overview grids
- Import DICOM files from external folders into the organised download folder
- Support for multiple saved server profiles with independent connection and retrieve settings
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

The AE Title and port can be changed in File > Preferences… > Retrieve.

For C-MOVE file retrieval to work, the PACS must be able to initiate an outbound TCP connection from its own network address to the Local IP and Local SCP port shown in Client info. Ensure that any firewall on this workstation permits inbound connections on that port.


### 2.3  Starting the Application

Double-click `dicomqr.exe` to launch the application. The main window opens and the status bar shows the application version. The window is restored to the size it had when last closed.


## 3  The Main Window

The main window is divided into a connection panel at the top, a tab area in the centre, and a status bar at the bottom.

**Connection panel** — the topmost area, visible from all tabs. Left side: server profile selector, Filters button, Search button. Right side: Connect, Disconnect, and Test (C-ECHO) buttons. A second row shows the SCP status indicator.

**Tab area** — four tabs:

| Tab | Purpose |
|---|---|
| PACS Query | Search a remote PACS and retrieve studies. |
| Worklist | Query a Modality Worklist server for scheduled procedures. |
| Local Browse | Browse, preview, push, and delete files in the download folder. |
| Import | Copy DICOM files from an external folder into the download folder. |

**Status bar** — the bottom strip. A coloured LED indicator precedes the status text. A clock shows the current date and time. A progress bar appears during queries and retrieves.


## 4  Connecting to a PACS Server


### 4.1  Server Profiles

A server profile stores the connection details for one PACS destination. Profiles are managed in File > Preferences… > Connections. Each profile records:

| Field | Description |
|---|---|
| Profile name | A label used to identify the server in the dropdown. |
| Remote AE Title | The Application Entity Title of the PACS (case-sensitive). |
| Host | The hostname or IP address of the PACS server. |
| Port | The TCP port on which the PACS listens (typically 104 or 11112). |
| Info model | The DICOM Query/Retrieve information model. `study` = Study Root (most common). `patient` = Patient Root. `patient-study-only` = legacy retired model used by some older systems; SERIES-level queries are not available with this model. |
| Retrieve method | C-MOVE (default) instructs the PACS to push files to the local C-STORE SCP listener. C-GET requests that the PACS return files over the same association — no inbound port or PACS-side destination registration is required. Auto tries C-GET first and falls back to C-MOVE if the PACS rejects it. |
| Connect timeout | Seconds to wait for the initial C-ECHO before reporting a failure. Default: 10 s. |

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

Click Filters ▾ in the server row to open the search criteria panel. The panel floats over the results tree and contains the search fields along with Search, Clear, and Close buttons. Click Filters ▾ again, or click Close, to dismiss it. Values are preserved between open and close cycles.


### 5.2  Search Fields

| Field | Description |
|---|---|
| Patient Name | Matches the DICOM Patient Name attribute. Supports wildcard characters: `*` matches any sequence, `?` matches one character. Format: FAMILY^GIVEN or a partial name (e.g. DOE*). A trailing `*` is appended automatically. Leave blank to match all patients. |
| Patient ID | Matches the DICOM Patient ID (MRN). Supports wildcards. A trailing `*` is appended automatically. |
| Accession No | Matches the DICOM Accession Number. Supports wildcards. A trailing `*` is appended automatically. |
| Study Date From | Start of study date range. Click the calendar icon to open a date picker, or type directly. Leave blank for no lower bound. |
| Study Date To | End of study date range. Leave blank for no upper bound. |
| Modality | Restricts results to one or more modalities. Tick any combination: CT, MR, PT, NM, US, CR, DX, XA, RF. Multiple modalities are queried concurrently and merged. Leave all unticked to include all modalities. |


### 5.3  Running a Search

Click Search inside the panel or in the server row, or press Ctrl+Enter. The panel closes and the query is sent. Pressing Enter in the Patient Name, Patient ID, or Accession No field also runs the search.


### 5.4  Clearing the Search

Click Clear inside the Filters panel to reset all fields and clear the results tree. Alternatively, select Query > Clear results.


## 6  Query Results


### 6.1  Tree Structure

Results are displayed in an expandable tree with three levels:

**Patient** — one node per unique patient.

**Study** — one or more studies under each patient. The label shows the study date, description, accession number, and modalities.

**Series** — one or more series under each study. The label shows the series number, modality, description, and image count. Series are loaded on demand when a study node is expanded.

Results are sorted: patients alphabetically, studies chronologically (oldest first), series numerically by series number.


### 6.2  Filtering Results

Type any text into the filter bar above the results tree to show only rows whose label contains the typed text (case-insensitive). Click Clear at the right of the filter bar to remove the filter.


### 6.3  Selecting Items for Retrieval

Click any row to select it; click again to deselect. Multiple rows at any level may be selected simultaneously. Selected rows are highlighted in the configured colour (see Section 14.1).

Select All / Clear Selection buttons are in the retrieve panel. Esc also clears the selection. Press Ctrl+C to copy the label of any selected row to the clipboard.


### 6.4  Right-Click Context Menu

| Option | Action |
|---|---|
| Retrieve | Retrieves the right-clicked node directly, regardless of the current selection. |
| Copy UID | Copies the Study or Series Instance UID to the clipboard. |
| Copy label | Copies the full display label to the clipboard. |


### 6.5  Tooltips

Hovering over a study or series row for approximately 0.6 seconds shows a tooltip with UID and accession or modality information.


## 7  Retrieving Files


### 7.1  Prerequisites

For C-MOVE: the application must be connected, the SCP must be running, the PACS must have the local AE Title / IP / port registered, and the download folder must be configured and writable.

For C-GET: the application must be connected and the download folder must be configured. No inbound port or destination registration is required.

For Auto: C-GET is tried first; if the PACS rejects it, the request falls back to C-MOVE.


### 7.2  Starting a Retrieve

Select one or more rows, then click Retrieve Selected (or Query > Retrieve Selected). A progress bar appears. The status bar shows each file path as it arrives.


### 7.3  Completion and Errors

On success the status bar shows `Retrieved N files successfully`. If some targets failed, a dialog offers to retry only the failed targets.


### 7.4  Cancelling

Click Cancel in the retrieve panel to abort. Files already written to disk are not removed.


## 8  Local Browse Tab

The Local Browse tab lets you work with DICOM files already in the download folder — browse the tree, preview images, push to a remote PACS, or delete files — without running a query.


### 8.1  Scanning the Download Folder

Click Scan (or the folder icon to open the folder first). dicomqr walks the download directory, parses each `.dcm` file (skipping pixel data for speed), and builds a Patient > Study > Series tree. The status label shows progress and a file count. The folder button opens the download directory in Windows Explorer.


### 8.2  Filtering and Navigation

Type in the filter bar to narrow the tree. Expand All, Collapse All, and Clear buttons are provided. The filter acts on the already-loaded tree and does not rescan the disk.


### 8.3  Previewing Images

Right-click any node and select **Preview Images**:

- **Series node** — opens the series viewer (see Section 8.3.1).
- **Study node** — opens the study overview grid (see Section 8.3.2).
- **Patient node** — Preview Images is disabled (too many files to be useful at this level).

#### 8.3.1  Series Viewer

The series viewer displays one image at a time with a slider to navigate through the sorted series. It opens at the middle slice.

The bottom bar contains:
- An instance counter (e.g. `45 / 120`)
- A navigation slider
- An **Annotations** checkbox (see Section 8.3.3)
- An info label showing pixel dimensions and W/L values

#### 8.3.2  Study Overview Grid

The overview window shows one thumbnail per series — the middle slice of each series rendered in parallel. Thumbnails are arranged in a three-column grid. Double-click any thumbnail to open that series in the full series viewer.

#### 8.3.3  DICOM Annotation Overlay

When Annotations is checked in the series viewer, a four-corner overlay is drawn within the actual image area (never in the letterbox bars):

| Corner | Content |
|---|---|
| Top-left | Patient name, MRN, date of birth, sex and age |
| Top-right (right-aligned) | Institution, study date/time, accession number, study description, referring physician |
| Bottom-left | Modality, series number and description, slice thickness, protocol |
| Bottom-right (right-aligned) | Instance number / total, slice location, pixel spacing, W/L values |

Anatomical orientation markers (R/L, A/P, H/F) are centred on the four image edges and derived from the `ImageOrientationPatient` direction cosines in DICOM LPS patient coordinates.

The Annotations checkbox state persists between sessions.


### 8.4  Opening in External Viewer

The **Open in Viewer** button in the bottom bar and the right-click menu item open the node's folder in the configured external DICOM viewer. These controls are disabled when no viewer path is configured in Preferences. **Open folder** opens the folder in Windows Explorer instead.


### 8.5  Pushing to a PACS

Right-click any node and select **Push to PACS…**, or select items and click **Push Selected…**, to send files to a remote PACS via C-STORE SCU.

A dialog appears with:
- A destination selector (any configured server profile)
- A progress bar and per-file counter
- A Cancel button

The push creates a new association per operation and does not require the PACS tab to be connected.


### 8.6  Deleting Local Files

Right-click any node and select **Delete…**, or select items and click **Delete Selected…**, to permanently remove files from disk. A confirmation dialog shows the file count and total size. After deletion, empty directories are pruned and the tree is rescanned automatically.

**Warning:** Deletion is permanent. Files are not moved to the Recycle Bin.


### 8.7  Selection Controls

The bottom bar provides:

| Control | Action |
|---|---|
| Select All | Selects every currently visible (filtered) root node and all its descendants. |
| Clear Selection | Deselects everything. |
| Push Selected… | Pushes all selected files to a chosen server. |
| Delete Selected… | Deletes all selected files after confirmation. |


## 9  Import Tab

The Import tab copies DICOM files from any folder into the organised download folder, applying the same Patient / Study / Series subfolder structure used by retrieval.


### 9.1  Scanning a Source Folder

Enter or browse to a source folder and click Scan. dicomqr walks the folder and builds a tree of studies and series found in it. The destination folder (the configured download folder) is shown read-only below the source field.


### 9.2  Selecting and Importing

Click rows in the tree to select them. Click **Import Selected** to copy the selected files. Files already present in the destination (same SOP Instance UID at the same destination path) are skipped; the status label reports imported, already-present, and failed counts.

Select All and Clear Selection buttons are provided. The filter bar narrows the tree in the same way as the other tabs.


## 10  Worklist Tab

The Worklist tab queries a Modality Worklist server for scheduled imaging procedures. It operates independently of the PACS Query tab — it does not require a PACS connection and can target a different server (typically a RIS or MWL broker).


### 10.1  Selecting a Worklist Server

Choose any configured server profile from the **Worklist server** dropdown. The dropdown updates when server profiles are added or removed in Preferences. The query connects to the selected profile's host, port, and AE Title for each query and releases the association immediately after.

Note: the Modality Worklist SOP class (1.2.840.10008.5.1.4.31) must be enabled on the target server. In most environments this is a separate system from the PACS (a RIS or dedicated MWL service) — configure a server profile pointing to that system.


### 10.2  Query Fields

| Field | Description |
|---|---|
| Patient Name | Wildcard-capable patient name match. A trailing `*` is appended automatically. Leave blank to match all patients. |
| MRN | Wildcard-capable Patient ID match. |
| Accession | Wildcard-capable Accession Number match. |
| Modality | Restricts results to one modality. Select `(any)` to include all modalities. |
| Scheduled date | **Today only** (checked by default) — restricts to today's scheduled date. Uncheck to select a specific date using the calendar picker. Leave blank (unchecked, no date selected) to return all scheduled dates. |

Click **Query Worklist** or press Enter in any text field to run the query. Click **Clear** to reset all fields and clear the results.


### 10.3  Results Table

Results are shown in a table with columns: Patient, MRN, Accession, Date, Time, Modality, Procedure, and Station. Click any row to select it.

**Copy Accession** and **Copy Patient** buttons copy the selected row's values to the clipboard.

The status label shows the number of worklist items returned, or any error message.


### 10.4  Typical Use Cases

- **Verify a scheduled procedure** — query by patient name or accession to confirm an order reached the worklist server before the patient arrives at the scanner.
- **Diagnose "patient not on scanner"** — if a technologist cannot find a patient on the modality's worklist, query here; if the entry appears, the problem is in the scanner's MWL configuration; if it does not, the order was not transmitted to the worklist server.
- **Look up an accession number** — copy the accession and switch to PACS Query to search for the matching study.


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

If a metadata field is absent, the corresponding folder falls back to a descriptive placeholder: Unknown Patient, Unknown Study, or Unknown Series. Characters not permitted in Windows file names are replaced with underscores, folder components are truncated to 64 characters, and the path falls back to a flat layout if it would exceed 255 characters.

Each SOP Instance UID is unique, so files from different studies with the same patient ID and series number are never overwritten.


## 12  Menus


### 12.1  File Menu

| Item | Description |
|---|---|
| Connect | Connects to the selected server profile. |
| Disconnect | Ends the current session and stops the local SCP listener. |
| Preferences… | Opens the Preferences dialog. See Section 14. |
| Quit | Exits the application. |


### 12.2  Query Menu

| Item | Description |
|---|---|
| Search | Runs the current search. |
| Clear results | Resets all search fields and removes all results from the tree. |
| Export… | Saves the current results tree to CSV or JSON. |
| Retrieve Selected | Starts retrieval of all currently selected tree nodes. |
| Cancel retrieve | Cancels an in-progress retrieval. |


### 12.3  Help Menu

| Item | Description |
|---|---|
| Activity Log… | Opens the in-app activity log showing the last 500 lines of the DICOM protocol log. Buttons: Refresh (manual update), Copy All (clipboard), Clear. The log auto-refreshes once per second while the dialog is open. |
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


## 14  Preferences

Open the Preferences dialog from File > Preferences…. Changes take effect when Apply is clicked and are written immediately to disk.


### 14.1  UI Section

| Setting | Description |
|---|---|
| Theme | Selects the application colour theme: Light or Dark. |
| Tree font | Selects the font used for results tree rows. Select (default) to use the application's built-in font. |
| Selection colour | The colour applied to selected rows. Click Choose colour… to open a colour picker. If unset, selected rows follow the theme's primary accent colour. |
| Selection style | The font style applied to selected rows: Bold and/or Italic. |


### 14.2  Connections Section

Lists all saved server profiles. Click Edit to modify, Delete to remove, or Add server… to create a new profile. The Up/Down buttons reorder the list; the first profile is the default selection when the application starts.

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


### 14.3  Retrieve Section

| Setting | Description |
|---|---|
| Local AE Title | The AE Title this workstation presents during DICOM associations. Default: DICOMQR. |
| Local SCP port | The TCP port on which the embedded C-STORE listener accepts incoming connections. Default: 11112. |
| Download folder | The root folder where retrieved and imported DICOM files are written. |

Changes to AE Title or SCP port take effect the next time a connection is established.


### 14.4  Image Viewer Section

| Setting | Description |
|---|---|
| External viewer | Full path to an external DICOM viewer executable. Click Browse… to locate it, or Auto-detect to search for MicroDicom or RadiAnt DICOM Viewer in the standard installation locations. When left empty, the Open in Viewer buttons and menu items are disabled. |


## 15  Status Bar

The status bar at the bottom of the window provides real-time feedback. A coloured LED indicator (gray / amber / green) precedes the status text.

| Situation | Status bar text |
|---|---|
| Application started, not connected | `v1.1.0` |
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
| Retrieve complete with warnings | `Retrieved <N> files (<X>/<total> targets had errors — see log)` |
| Retrieve cancelled | `Retrieve cancelled` |
| C-ECHO test passed | `C-ECHO success` |
| C-ECHO test failed | `C-ECHO failed: <reason>` |

The SCP status indicator row in the connection panel shows:

| Situation | SCP indicator text |
|---|---|
| Not connected | `SCP: not running` |
| SCP listening | `SCP: listening on 0.0.0.0:<port> (AE: <title>)` |
| SCP failed to start | `SCP: error — <reason>` |


---


## Appendix A  Application Settings

Application settings are persisted to `%USERPROFILE%\.dicomqr\settings.json`. This file is created automatically on first launch with the compiled-in defaults shown below.

| JSON key | Default | Description |
|---|---|---|
| `darkTheme` | `false` | Colour theme. `false` = Light, `true` = Dark. |
| `fontName` | `""` | System font for result tree rows. Empty = built-in font. |
| `localAETitle` | `"DICOMQR"` | The AE Title presented during DICOM associations. |
| `localSCPPort` | `11112` | TCP port for the embedded C-STORE listener. |
| `downloadDir` | `""` | Absolute path of the download folder. Defaults to `~/DICOM Downloads`. |
| `viewerPath` | `""` | Full path to an external DICOM viewer executable. Empty disables the Open in Viewer controls. |
| `selectionColor` | `""` | Colour applied to selected tree rows (RRGGBBAA hex). Empty follows the theme primary colour. |
| `selectionBold` | `true` | Whether selected rows are drawn in bold. |
| `selectionItalic` | `false` | Whether selected rows are drawn in italic. |
| `windowWidth` | `0` | Saved window width in pixels. 0 uses the default; updated automatically on close. |
| `windowHeight` | `0` | Saved window height in pixels. |
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

The **Annotations** overlay toggle is stored in the application's Fyne preferences (not in `settings.json`) and persists automatically between sessions.


---


## Appendix B  PACS Configuration Notes

**AE Title registration** — The PACS must have a record of the local AE Title (default DICOMQR) associated with the workstation's IP address and SCP port. Look for "Known Destinations", "Remote AE Configuration", or similar.

**C-MOVE destination** — For file delivery the PACS must be configured to push files to the local SCP address. The workstation must be reachable at the IP and port shown in Help > Client info…

**Windows Firewall** — An inbound rule permitting TCP connections on the SCP port (default 11112) is required.

**Information model** — If queries return no results, try changing the Info model in the server profile. Some PACS require Study Root, others Patient Root. A small number of legacy systems require the Patient/Study Only model (`patient-study-only`).

**Worklist server** — The Modality Worklist SOP class is typically served by a RIS or dedicated MWL broker, not the PACS itself. Create a separate server profile pointing to that system and select it in the Worklist tab.

**IPv4 connectivity** — dicomqr listens on an IPv4 socket only. Ensure the address shown in Help > Client info… is the correct IPv4 address on the same network as the PACS.


---


## Appendix C  Credits and Acknowledgements


### Developer

Jeffrey Leal

Email: jeffrey.leal@gmail.com

GitHub: https://github.com/jeffrey-leal


### AI Assistance

This application was designed and developed with the assistance of Claude Sonnet 4.6 by Anthropic, accessed through Claude Code (https://claude.ai/code). Architecture planning, code generation, DICOM standard research, and documentation were produced in collaboration with Claude Code.


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
