# dicomqr

A Fyne-based Windows GUI application for querying and retrieving DICOM files from a PACS server.

## Build

Requires CGO and the mingw64 GCC toolchain. **Must be built from an MSYS2 MinGW64 terminal.**

JPEG 2000 decoding (`-tags openjpeg`) requires the OpenJPEG library, and JPEG Lossless decoding (`-tags jpeglossless`) requires libjpeg-turbo 3.x, each installed once with:
```bash
pacman -S --needed mingw-w64-x86_64-openjpeg2
pacman -S --needed mingw-w64-x86_64-libjpeg-turbo
```
Release builds enable both tags (`-tags "openjpeg jpeglossless"`; libopenjp2 and libjpeg are statically linked — single self-contained exe). Omitting a tag still builds; the corresponding files then report "support not built in" and suggest the external viewer.

All builds must pass `-extldflags=-static` so GCC links its own runtime (libwinpthread, libgcc) statically; without it the exe imports `libwinpthread-1.dll` and fails to start on machines without MSYS2.

Open MSYS2 MinGW64, then:

```bash
export PATH="/c/Program Files/Go/bin:$PATH"
cd /c/Users/jeffr/source/repos/dicomqr
```

Release build:
```bash
CGO_ENABLED=1 CC=/c/msys64/mingw64/bin/gcc.exe GOAMD64=v3 \
  go build -tags "openjpeg jpeglossless" -ldflags="-s -w -H windowsgui -extldflags=-static -X main.buildDate=$(date +%Y-%m-%d)" -o dicomqr.exe .
```

Development build (with debug info):
```bash
CGO_ENABLED=1 CC=/c/msys64/mingw64/bin/gcc.exe \
  go build -tags "openjpeg jpeglossless" -ldflags="-extldflags=-static -X main.buildDate=$(date +%Y-%m-%d)" -o dicomqr.exe .
```

## Project structure

| File | Purpose |
|---|---|
| `main.go` | App entry point, window layout, tab container, menu bar, connection panel, query panel, retrieve panel, status bar, connection/SCP LEDs |
| `resultsmodel.go` | `resultsModel` — tree data structure for C-FIND query results |
| `queryrow.go` | `queryRow` — Fyne widget for results tree rows (hover tooltip, right-click menu) |
| `dicomnet.go` | `DicomClient` — SCU wrapper for C-ECHO, C-FIND, C-MOVE, C-GET, C-STORE, Modality Worklist |
| `storagescp.go` | `StorageSCP` — embedded C-STORE SCP listener that receives C-MOVE deliveries |
| `localbrowse.go` | Local Browse tab — scan download folder, push to PACS, delete local files, preview routing, Modification submenu |
| `catalog.go` | `catalog` — persistent SQLite index (`.dicomqr-index.db` in the download folder) backing the Local Browse tree; nil-safe, WAL, single connection |
| `modifyprofile.go` | Modification (de-identification) profiles — `ModProfile` + `TagConfig`, load/first-run seeding of `~/.dicomqr/profiles.json` and `tags.json`, base-chain resolution, tag parsing/display names |
| `modifyengine.go` | De-identification engine ported from dicomtool `modify` (identical semantics): set/remove/keep, DOB mask, UID suffix/remap, private-tag removal, fixvr, pixel-row masking, per-modality overrides; `runModification` worker pool writes to an export folder using the PHI-safe layout from `exportRelPaths` (original patient/study folder names never reused: study runs keep series subfolders only, patient runs get generic `study-NN`); `runModificationToZip` runs the same pipeline into a single zip archive (parallel modify, serialized entry writes; temp-file + rename so a failed/empty run leaves no archive) |
| `modifydialog.go` | Modification confirmation dialog (editable set values and options, removal-tag list), export destination (persistent default output folder used directly — picker only when unset, first choice saved as the default, Change… = single-run override, always outside the download folder; editable PHI-safe export folder name pre-filled with profile + timestamp; Patient Name set field auto-fills Patient ID and the export name until edited directly; Zip export checkbox writes a single `<export name>.zip` instead of a folder), background run with progress/cancel/summary |
| `modprofileeditor.go` | Modification-profile editor dialog (Preferences > Modification & Export) — core `ModProfile` fields with alias-aware validation and base-cycle checks; per-modality/ignore/dicomdir/verbose settings are preserved untouched |
| `tagviewer.go` | Tag-level review window (Local Browse right-click → View Tags) — port of dicomhdr's tag tree: Patient → Study → Series → Instance → elements with search, dictionary hover tooltips, settings-driven styling (private italics, malformed colour, tag profile colours; live-refreshes open windows on preference changes), copy actions, concurrent incremental load, instance right-click Export Tags… (single-instance CSV/JSON via export.go) |
| `tagprofiles.go` | Tag Highlights/Tag Profiles support (from dicomhdr) — `TagProfile` with dicomhdr-compatible JSON wire format, `matchProfile`, tag list parsing, profile editor dialog, malformed-colour default |
| `importtab.go` | Import tab — scan external folder and copy selected files into the download folder |
| `worklist.go` | Worklist tab — Modality Worklist C-FIND with independent server selector |
| `viewer.go` | Internal image viewer, study overview grid, DICOM annotation overlay, thumbnail widget |
| `colormap.go` | Colour lookup tables (DICOM-standard palettes) for PET/SPECT pseudo-colour |
| `jpeg2000_openjpeg.go` | CGO JPEG 2000 decoder via OpenJPEG (`//go:build openjpeg`) |
| `jpeg2000_stub.go` | Fallback when built without the `openjpeg` tag |
| `jpeglossless_turbo.go` | CGO JPEG Lossless (SOF3, .57/.70) decoder via libjpeg-turbo 3.x (`//go:build jpeglossless`) — viewer-only; the transcode/negotiation paths intentionally exclude JPEG Lossless |
| `jpeglossless_stub.go` | Fallback when built without the `jpeglossless` tag |
| `logcapture.go` | In-memory log ring buffer and Activity Log dialog |
| `settings.go` | `Settings` struct, load/save, embedded defaults |
| `serverprofile.go` | `ServerProfile` struct for saved server connections |
| `preferences.go` | `appTheme`, system font scanner, tabbed preferences dialog (SCP & Network / User Interface / Modification & Export; Apply copies current settings and overwrites only edited fields) |
| `transfersyntax.go` | Transfer syntax UID constants, labels, and `fileTransferSyntaxUID` (meta-only parse) |
| `transcode.go` | Local conversion enforcing a profile's required syntax (Explicit/Implicit VR LE): `acceptedSyntaxesFor` builds the negotiable set (required first, then locally convertible — other LE VR, JPEG Baseline/Extended, JPEG 2000), and the receive path transcodes any non-required arrival in place before it reaches the download folder; objects that cannot be obtained in the required syntax — whether the server cannot deliver them in a negotiable syntax (failed sub-operations) or a delivered object fails local conversion (e.g. a screenshot with undecodable pixel data) — are skipped and reported (Activity Log + retrieve summary) while the rest of the retrieve continues; only a retrieve where the server delivers nothing at all raises an error dialog |
| `export.go` | CSV and JSON export of a single instance's element list from the tag viewer (right-click an instance row → Export Tags…) — the application's only export; always the complete instance regardless of the search filter, tags formatted `[GGGG,EEEE]` zero-filled 4-digit hex (square brackets, not parentheses — Excel parses `(0020,0001)` as the negative number -20,001; brackets import as text with no formula wrapper needed) |

## Key dependencies

- `fyne.io/fyne/v2 v2.7.3` — GUI framework
- `github.com/algm/go-netdicom` — DICOM networking (C-ECHO, C-FIND, C-MOVE SCU, C-STORE SCP/SCU, Worklist); vendored under `thirdparty/go-netdicom` with local patches for `QRLevelPatientStudyOnly` and `QRLevelWorklist`
- `github.com/suyashkumar/dicom v1.1.0` — DICOM file parsing (local files, image rendering, annotation extraction)
- `github.com/grailbio/go-dicom` — DICOM dataset encoding used by the C-STORE SCU and SCP
- `github.com/sqweek/dialog` — native Windows file/folder picker
- `modernc.org/sqlite` — pure-Go SQLite (no CGO) for the Local Browse persistent index

## Documentation

| File | Purpose |
|---|---|
| `CREDITS.md` | Full attribution by role — Jeffrey Leal (architecture & direction), Claude by Anthropic (implementation), DICOM standard reference, open-source libraries |
| `CHANGELOG.md` | Version history |
| `dicomqr-user-manual.md` | End-user guide |

## Notes

- App ID: `com.jeffreyleal.dicomqr`
- Settings persisted to `~/.dicomqr/settings.json`
- C-MOVE requires the embedded C-STORE SCP listener (default port 11112) to receive files
- The local AE Title (default `DICOMQR`) must be registered on the PACS as a known destination
- The app "owns" the download folder: downloads, imports, and manual scans keep `.dicomqr-index.db` in sync with the folder contents, and touching a tree entry whose files were removed externally prunes it from both the tree and the index
- Modification (de-identification) profiles live in `~/.dicomqr/profiles.json` with tag aliases in `~/.dicomqr/tags.json`, seeded from `defaults/` on first run and never overwritten by seeding; the format and semantics match dicomtool, so profile files can be copied between the two tools. Profiles are managed in Preferences > Modification & Export — changes are written back only on Apply and only when something changed, and a profiles.json that fails to parse is never overwritten. Modified files always go to a user-chosen folder outside the download folder — the catalog is never touched
