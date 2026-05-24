# dicomqr User Manual

Version 0.1.2

---

## 1. Overview

dicomqr is a Windows desktop application for querying and retrieving DICOM medical imaging studies from a PACS (Picture Archiving and Communication System) server. It communicates with the PACS using the standard DICOM networking services — C-FIND for searching and C-MOVE for retrieval — and includes an embedded C-STORE listener that receives files pushed by the PACS directly to a configured download folder.

Key capabilities:

- Connect to any DICOM-compatible PACS server using configurable server profiles
- Search for studies by patient name, patient ID, accession number, date range, and modality
- Browse query results in an expandable Patient > Study > Series tree
- Retrieve entire studies or individual series to a local folder
- Automatically organise downloaded files by patient, study, and series
- Support for multiple saved server profiles with independent connection settings

---

## 2. Getting Started

### 2.1 System Requirements

- Windows 10 or later (64-bit)
- Network access to a DICOM PACS server
- A configured PACS that accepts DICOM associations from this workstation

### 2.2 PACS Registration

Before connecting, the PACS administrator must register this workstation as a known Application Entity (AE). The required details are shown in **Help > Client info…** once the application is running:

| Field | Default value | Description |
|---|---|---|
| Local AE Title | `DICOMQR` | The name the PACS uses to identify this workstation. |
| Local SCP port | `11112` | The TCP port on which dicomqr listens for incoming file transfers. |
| Local IP | Detected automatically | The IP address of this workstation as seen by the PACS. |

The AE Title and port can be changed in **File > Preferences… > Retrieve**.

For C-MOVE (file retrieval) to work, the PACS must be able to initiate an outbound TCP connection from its own network address to the Local IP and Local SCP port shown in Client info. Ensure that any firewall on this workstation permits inbound connections on that port.

### 2.3 Starting the Application

Double-click `dicomqr.exe` to launch the application. The main window opens with an empty results tree and the status bar showing the application version.

---

## 3. The Main Window

The main window is divided into four areas stacked vertically:

**Server row** — the topmost bar. Contains the server profile selector, a Search button, a Filters toggle button, and the Connect, Disconnect, and Test (C-ECHO) buttons.

**Filter bar** — a text field and Clear button that narrows the results tree to rows whose label contains the typed text.

**Results tree** — the main area of the window. Displays query results organised hierarchically as Patient > Study > Series. Expands to fill all available space between the server row and the retrieve panel.

**Retrieve panel and status bar** — the bottom area. Contains the download folder field, the Retrieve Selected and Cancel buttons, and the status bar.

---

## 4. Connecting to a PACS Server

### 4.1 Server Profiles

A server profile stores the connection details for one PACS destination. Profiles are managed in **File > Preferences… > Connections**. Each profile records:

| Field | Description |
|---|---|
| Profile name | A label used to identify the server in the dropdown. |
| Remote AE Title | The Application Entity Title of the PACS (case-sensitive). |
| Host | The hostname or IP address of the PACS server. |
| Port | The TCP port on which the PACS listens (typically 104 or 11112). |
| Info model | The DICOM Query/Retrieve information model to use: **study** (Study Root) or **patient** (Patient Root). Use the model required by your PACS; Study Root is most common. |

The first profile in the list is selected by default when the application starts.

### 4.2 Connecting

Select a server profile from the dropdown in the server row, then click **Connect** (or select **File > Connect**). The application sends a C-ECHO to verify basic DICOM connectivity before marking the session as connected. The status bar updates to show the connected server, its address, and the address and AE Title of the local SCP listener.

If the C-ECHO fails, the status bar shows a connection error and the application remains in the disconnected state.

### 4.3 Testing Connectivity

Click **Test (C-ECHO)** at any time while connected to send a C-ECHO ping to the PACS. The status bar reports success or failure. This is useful for confirming the PACS is still reachable without running a full query.

### 4.4 Disconnecting

Click **Disconnect** (or select **File > Disconnect**) to close the session. Any in-progress query is cancelled and the local SCP listener is stopped. The results tree is not cleared automatically; use **Query > Clear results** to remove the current results.

---

## 5. Searching for Studies

### 5.1 Opening the Filters Panel

Click **Filters ▾** in the server row to open the search criteria panel. The panel floats over the results tree. Clicking **Filters ▾** again, or clicking anywhere outside the panel, closes it. Any values typed in the fields are preserved between open and close cycles.

### 5.2 Search Fields

| Field | Description |
|---|---|
| Patient Name | Matches the DICOM Patient Name attribute. Supports DICOM wildcard characters: `*` matches any sequence of characters, `?` matches a single character. Format: `FAMILY^GIVEN` or a partial name with wildcards (e.g. `DOE*`). Leave blank to match all patients. |
| Patient ID | Matches the DICOM Patient ID (MRN). Supports wildcards. Leave blank to match all IDs. |
| Accession No | Matches the DICOM Accession Number. Leave blank to match all accession numbers. |
| Study Date From | The start of the study date range. Click the calendar icon to open a month-view date picker and select a date, or type directly into the field. Leave blank for no lower bound. |
| Study Date To | The end of the study date range. Click the calendar icon to open a month-view date picker and select a date, or type directly into the field. Leave blank for no upper bound. |
| Modality | Restricts results to one or more imaging modalities. Tick any combination of the checkboxes: CT, MR, PT, NM, US, CR, DX, XA, RF. When multiple modalities are ticked, a separate query is sent for each and the results are merged. Leave all checkboxes unticked to include all modalities. |

At least one field should be populated before searching. Sending a completely unconstrained query (all fields blank, no modalities ticked) may return a very large result set or be rejected by the PACS.

### 5.3 Running a Search

With the Filters panel open, click **Search** inside the panel, or click the **Search** button in the server row, or press `Ctrl+Enter`. The panel closes, the results tree clears, and the query is sent to the PACS. The status bar shows "Querying…" during the search and reports the number of studies returned when complete.

Pressing `Enter` while the cursor is in the Patient Name, Patient ID, or Accession No field also runs the search and closes the panel.

### 5.4 Clearing the Search

Click **Clear** inside the Filters panel to reset all search fields to their defaults and clear the results tree. Alternatively, select **Query > Clear results**.

---

## 6. Query Results

### 6.1 Tree Structure

Results are displayed in an expandable tree with three levels:

**Patient** — one node per unique patient. The label shows the patient name and, where present, the patient ID in parentheses.

**Study** — one or more studies under each patient. The label shows the study date, study description, accession number, and the set of modalities present in the study.

**Series** — one or more series under each study. The label shows the series number, modality, series description, and image count.

The tree starts fully collapsed after each search. Click the expand arrow next to a patient node to reveal its studies. Click the expand arrow next to a study node to load its series — dicomqr sends a separate C-FIND query to the PACS at this point to retrieve series-level information. The series list is fetched once per study per session; collapsing and re-expanding a study does not repeat the query.

### 6.2 Filtering Results

Type any text into the filter bar above the results tree. The tree immediately redraws to show only rows whose label contains the typed text (case-insensitive). Parent nodes that contain a matching descendant are always shown. Click **Clear** at the right of the filter bar to remove the filter and restore the full tree.

The filter acts on the already-loaded results and does not send a new query to the PACS.

### 6.3 Selecting Items for Retrieval

Click any row in the results tree to select it. Selected rows are shown in bold in the primary accent color. Click the same row again to deselect it. Multiple rows at any level (patient, study, or series) may be selected simultaneously.

Series nodes are only visible after a study has been expanded. Expand a study first, then select individual series for retrieval.

Selection behaviour during retrieval:

- **Patient node selected** — all studies under that patient are retrieved.
- **Study node selected** — the entire study is retrieved as a single C-MOVE request.
- **Series node(s) selected** — each selected series is retrieved individually.
- **Mixed selection** — if a study and one or more of its series are both selected, the study-level C-MOVE takes precedence and the series are not sent as duplicate requests.

Press `Ctrl+C` to copy the full label text of any selected row to the clipboard.

### 6.4 Right-Click Context Menu

Right-clicking any row in the results tree opens a small context menu with two options:

| Option | Action |
|---|---|
| Copy UID | Copies the Study Instance UID or Series Instance UID of the row to the clipboard. |
| Copy label | Copies the full display label of the row to the clipboard. |

### 6.5 Tooltips

Hovering the mouse cursor over a study or series row for approximately 0.6 seconds displays a tooltip showing the Study Instance UID and Accession Number (for study rows) or the Series Instance UID and Modality (for series rows). Moving the cursor off the row dismisses the tooltip immediately.

---

## 7. Retrieving Files

### 7.1 Prerequisites

The following conditions must be met before a retrieval can proceed:

1. The application must be connected to a PACS server (status bar shows "Connected").
2. The embedded C-STORE listener must be running. It starts automatically when a connection is established. If it is not running, disconnect and reconnect to restart it.
3. The PACS must have the local AE Title, IP address, and port registered as a known destination. See Section 2.2.
4. The download folder must be configured. Click the folder icon next to the **Download to** field in the retrieve panel.
5. At least one item must be selected in the results tree.

### 7.2 Starting a Retrieve

Select one or more rows in the results tree, then click **Retrieve Selected** (or select **Query > Retrieve Selected**). dicomqr sends a C-MOVE request to the PACS for each selected item. Selecting a study retrieves all of its series in a single C-MOVE; selecting individual series sends one C-MOVE per series. The PACS responds by pushing the DICOM files to the local SCP listener, which writes them to the download folder.

A progress bar appears below the retrieve buttons and advances as each study or series is transferred. The status bar shows which item is currently being retrieved.

### 7.3 Progress

The progress bar tracks completion across all selected studies and series. As each file arrives, the status bar briefly shows the path of the received file.

### 7.4 Completion

When all files have been received, the progress bar disappears and the status bar shows:

> Retrieved *N* files successfully

where *N* is the total number of DICOM files written to disk during this retrieve operation.

### 7.5 Cancelling a Retrieve

Click **Cancel** in the retrieve panel (or select **Query > Cancel retrieve**) to abort an in-progress retrieval. Files that have already been written to disk are not removed. The status bar shows:

> Retrieve cancelled

---

## 8. Downloaded Files

Files are written to the folder specified in the **Download to** field. Within that folder, dicomqr creates a three-level subfolder structure:

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

If a metadata field is absent from the DICOM file, the corresponding folder component falls back to a descriptive placeholder: `Unknown Patient`, `Unknown Study`, or `Unknown Series`. Characters that are not permitted in Windows file or folder names are replaced with underscores.

Each SOP Instance UID is unique, so files from different studies that share the same patient ID and series number are written to separate subfolders and are never overwritten.

---

## 9. Menus

### 9.1 File Menu

| Item | Description |
|---|---|
| Connect | Connects to the currently selected server profile. Equivalent to clicking the Connect button. |
| Disconnect | Ends the current PACS session and stops the local SCP listener. |
| Preferences… | Opens the Preferences dialog. See Section 11. |
| Quit | Exits the application. |

### 9.2 Query Menu

| Item | Description |
|---|---|
| Search | Runs the current search using the values in the Filters panel. |
| Clear results | Resets all search fields and removes all results from the tree. |
| Retrieve Selected | Starts retrieval of all currently selected tree nodes. |
| Cancel retrieve | Cancels an in-progress retrieval. |

### 9.3 Help Menu

| Item | Description |
|---|---|
| About | Displays the application version and build date. |
| Client info… | Displays the local AE Title, SCP port, and detected IP address of this workstation. These values must be registered on the PACS to enable C-MOVE file delivery. |

---

## 10. Keyboard Shortcuts

| Shortcut | Action |
|---|---|
| `Ctrl+Enter` | Run the current search. |
| `Ctrl+F` | Move the input focus to the Patient Name field in the Filters panel. |
| `Ctrl+C` | Copy the full label of the currently selected result row to the clipboard. |

---

## 11. Preferences

Open the Preferences dialog from **File > Preferences…**. Changes take effect when **Apply** is clicked and are written immediately to disk. Clicking **Cancel** discards all pending changes.

### 11.1 UI Section

| Setting | Description |
|---|---|
| Theme | Selects the application color theme. Choose **Light** or **Dark**. The change applies immediately to all UI elements. |
| Tree font | Selects the font used to render rows in the results tree. The dropdown lists all TrueType (.ttf) and OpenType (.otf) fonts installed on the system. Select **(default)** to use the application's built-in font. |

### 11.2 Connections Section

The Connections section lists all saved server profiles. Each entry shows the profile name and its connection details.

Click **Edit** on any entry to open the profile editor. Click **Delete** to remove a profile from the list. Click **Add server…** to create a new profile.

**Profile editor fields:**

| Field | Description |
|---|---|
| Profile name | A descriptive label for this server, used in the server dropdown. |
| Remote AE Title | The Application Entity Title of the PACS server (case-sensitive, uppercase recommended). |
| Host | The hostname or IP address of the PACS server. |
| Port | The TCP port of the PACS DICOM service (commonly 104 or 11112). |
| Info model | The DICOM Q/R information model: **study** (Study Root) or **patient** (Patient Root). |

### 11.3 Retrieve Section

| Setting | Description |
|---|---|
| Local AE Title | The Application Entity Title this workstation presents to the PACS. Must match the AE Title registered on the PACS. Default: `DICOMQR`. |
| Local SCP port | The TCP port on which dicomqr listens for incoming file transfers from the PACS. The PACS must be able to reach this port. Default: `11112`. |

Changes to the AE Title or SCP port take effect the next time a connection is established (disconnect and reconnect after applying).

---

## 12. Status Bar

The status bar at the bottom of the window provides real-time feedback on the application state. Messages shown include:

| Situation | Status bar text |
|---|---|
| Application started, not connected | `v0.1.2` |
| Connecting to server | `Connecting…` |
| Connected | `Connected: <AE>@<host>:<port>  \|  SCP <address> (AE: <title>)` |
| Connection failed | `Connection failed: <reason>` |
| Query in progress | `Querying…` |
| Query complete | `Query complete — <N> studies` |
| Query error | `Query error: <reason>` |
| Disconnected | `Disconnected` |
| Retrieve starting (studies) | `Starting retrieve of <N> studies…` |
| Retrieve starting (single study) | `Starting retrieve of 1 study…` |
| Retrieve starting (series) | `Starting retrieve of <N> series…` |
| Retrieve in progress (study) | `Retrieving study <N>/<total>…` |
| Retrieve in progress (series) | `Retrieving series <N>/<total>…` |
| File received | `Received: <file path>` |
| Retrieve complete | `Retrieved <N> files successfully` |
| Retrieve cancelled | `Retrieve cancelled` |
| Retrieve error | `Retrieve error: <reason>` |
| C-ECHO test passed | `C-ECHO success` |
| C-ECHO test failed | `C-ECHO failed: <reason>` |

---

## Appendix A: Application Settings

Application settings are persisted to `%USERPROFILE%\.dicomqr\settings.json`. This file is created automatically on first launch with the compiled-in defaults shown below. It can be edited manually; it is standard JSON.

| Setting (JSON key) | Default value | Description |
|---|---|---|
| `darkTheme` | `false` | Color theme selection. `false` = Light, `true` = Dark. |
| `fontName` | `""` | Name of the system font used for result tree rows. An empty string selects the application's built-in font. |
| `localAETitle` | `"DICOMQR"` | The AE Title presented to the PACS during DICOM associations. |
| `localSCPPort` | `11112` | The TCP port on which the embedded C-STORE listener accepts incoming connections. |
| `downloadDir` | `""` | The absolute path of the folder where retrieved DICOM files are written. |
| `profiles` | See below | Array of saved server profile objects. |

Each entry in the `profiles` array has the following fields:

| Field (JSON key) | Description |
|---|---|
| `name` | The display name of the profile. |
| `remoteAETitle` | The AE Title of the PACS server. |
| `host` | The hostname or IP address of the PACS server. |
| `port` | The TCP port of the PACS DICOM service. |
| `infoModel` | `"study"` or `"patient"`. |

---

## Appendix B: PACS Configuration Notes

The following points are commonly required when configuring a PACS to work with dicomqr:

**AE Title registration** — The PACS must have a record of the local AE Title (default `DICOMQR`) associated with the workstation's IP address and SCP port. The exact menu path depends on the PACS software; look for "Known Destinations", "Remote AE Configuration", or similar.

**C-MOVE destination** — For file delivery to work, the PACS must be configured to push files to the local SCP address. The PACS initiates the outbound connection; the workstation must be reachable at the IP and port shown in **Help > Client info…**

**Windows Firewall** — An inbound rule permitting TCP connections on the SCP port (default 11112) is required. Without it, the PACS connection attempt will be refused and no files will be delivered.

**Information model** — Different PACS products implement either Study Root or Patient Root query models, or both. If queries return no results or an error, try changing the Info model field in the server profile from **study** to **patient**, or vice versa.

**IPv4 connectivity** — dicomqr listens on an IPv4 socket only. The PACS must connect to the workstation's IPv4 address. Ensure the address shown in **Help > Client info…** is the correct IPv4 address on the same network as the PACS.

---

## Appendix C: Credits and Acknowledgements

### Developer

**Jeffrey Leal**
Email: jeffrey.leal@gmail.com
GitHub: https://github.com/jeffrey-leal

### AI Assistance

This application was designed and developed with the assistance of **Claude Sonnet 4.6** by [Anthropic](https://www.anthropic.com). Architecture planning, code generation, DICOM standard research, and documentation were produced in collaboration with Claude Code.

### UI Template

This application's structure, conventions, and UI patterns are derived from **dicomhdr** — a Fyne-based DICOM file inspector by the same developer.
https://github.com/jeffrey-leal/dicomhdr

### DICOM Standard Reference

Protocol implementation follows the DICOM Standard published by NEMA:

**DICOM PS3 (2024b)** — https://dicom.nema.org/medical/dicom/current

Sections referenced:
- PS3.4 — Service Class Specifications (Query/Retrieve, C.4)
- PS3.7 — Message Exchange (DIMSE-C services: C-ECHO, C-FIND, C-MOVE, C-STORE)
- PS3.8 — Network Communication / DICOM Upper Layer Protocol

### Open-Source Libraries

| Library | Author / Maintainer | Licence | Purpose |
|---|---|---|---|
| [fyne.io/fyne/v2](https://fyne.io) v2.7.3 | Fyne.io contributors | BSD 3-Clause | GUI framework |
| [algm/go-netdicom](https://github.com/algm/go-netdicom) v0.1.0 | Alan Griffin (fork of grailbio) | BSD 3-Clause | DICOM network protocol (C-ECHO, C-FIND, C-MOVE, C-STORE SCP) |
| [grailbio/go-netdicom](https://github.com/grailbio/go-netdicom) | Yasushi Saito / GRAIL Inc. | BSD 3-Clause | Original DICOM networking library (base of go-netdicom fork) |
| [grailbio/go-dicom](https://github.com/grailbio/go-dicom) | GRAIL Inc. | Apache 2.0 | DICOM dataset encoding / file header writing |
| [suyashkumar/dicom](https://github.com/suyashkumar/dicom) v1.1.0 | Suyash Kumar | MIT | DICOM file parsing for received files |
| [sqweek/dialog](https://github.com/sqweek/dialog) | sqweek | ISC | Native Windows file/folder picker dialogs |

All libraries are used under their respective open-source licences. A vendored copy of `algm/go-netdicom` is included under `thirdparty/go-netdicom` with its original BSD 3-Clause licence intact.
