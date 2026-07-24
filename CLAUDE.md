# dicomqr

A Fyne-based Windows GUI application for querying and retrieving DICOM files from a PACS server.

## Build

Requires CGO and the mingw64 GCC toolchain. **Must be built from an MSYS2 MinGW64 terminal.**

JPEG 2000 decoding (`-tags openjpeg`) requires the OpenJPEG library, installed once with:
```bash
pacman -S --needed mingw-w64-x86_64-openjpeg2
```
Release builds enable the `openjpeg` tag (libopenjp2 is statically linked — single self-contained exe). Omitting the tag still builds; JPEG 2000 files then report "support not built in" and suggest the external viewer.

Open MSYS2 MinGW64, then:

```bash
export PATH="/c/Program Files/Go/bin:$PATH"
cd /c/Users/jeffr/source/repos/dicomqr
```

Release build:
```bash
CGO_ENABLED=1 CC=/c/msys64/mingw64/bin/gcc.exe GOAMD64=v3 \
  go build -tags openjpeg -ldflags="-s -w -H windowsgui -X main.buildDate=$(date +%Y-%m-%d)" -o dicomqr.exe .
```

Development build (with debug info):
```bash
CGO_ENABLED=1 CC=/c/msys64/mingw64/bin/gcc.exe \
  go build -tags openjpeg -ldflags="-X main.buildDate=$(date +%Y-%m-%d)" -o dicomqr.exe .
```

## Project structure

| File | Purpose |
|---|---|
| `main.go` | App entry point, window layout, tab container, menu bar, connection panel, query panel, retrieve panel, status bar, connection/SCP LEDs |
| `resultsmodel.go` | `resultsModel` — tree data structure for C-FIND query results |
| `queryrow.go` | `queryRow` — Fyne widget for results tree rows (hover tooltip, right-click menu) |
| `dicomnet.go` | `DicomClient` — SCU wrapper for C-ECHO, C-FIND, C-MOVE, C-GET, C-STORE, Modality Worklist |
| `storagescp.go` | `StorageSCP` — embedded C-STORE SCP listener that receives C-MOVE deliveries |
| `localbrowse.go` | Local Browse tab — scan download folder, push to PACS, delete local files, preview routing |
| `catalog.go` | `catalog` — persistent SQLite index (`.dicomqr-index.db` in the download folder) backing the Local Browse tree; nil-safe, WAL, single connection |
| `importtab.go` | Import tab — scan external folder and copy selected files into the download folder |
| `worklist.go` | Worklist tab — Modality Worklist C-FIND with independent server selector |
| `viewer.go` | Internal image viewer, study overview grid, DICOM annotation overlay, thumbnail widget |
| `colormap.go` | Colour lookup tables (DICOM-standard palettes) for PET/SPECT pseudo-colour |
| `jpeg2000_openjpeg.go` | CGO JPEG 2000 decoder via OpenJPEG (`//go:build openjpeg`) |
| `jpeg2000_stub.go` | Fallback when built without the `openjpeg` tag |
| `logcapture.go` | In-memory log ring buffer and Activity Log dialog |
| `settings.go` | `Settings` struct, load/save, embedded defaults |
| `serverprofile.go` | `ServerProfile` struct for saved server connections |
| `preferences.go` | `appTheme`, system font scanner, preferences dialog |
| `dicomfile.go` | `isDICOMFile` — magic-byte verification |
| `transfersyntax.go` | Transfer syntax UID constants, labels, and `fileTransferSyntaxUID` (meta-only parse) |
| `transcode.go` | Local conversion enforcing a profile's required syntax (Explicit/Implicit VR LE): `acceptedSyntaxesFor` builds the negotiable set (required first, then locally convertible — other LE VR, JPEG Baseline/Extended, JPEG 2000), and the receive path transcodes any non-required arrival in place before it reaches the download folder; objects that cannot be obtained in the required syntax — whether the server cannot deliver them in a negotiable syntax (failed sub-operations) or a delivered object fails local conversion (e.g. a screenshot with undecodable pixel data) — are skipped and reported (Activity Log + retrieve summary) while the rest of the retrieve continues; only a retrieve where the server delivers nothing at all raises an error dialog |
| `export.go` | CSV and JSON export of query results |

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
| `CREDITS.md` | Full attribution — developer, AI assistance, DICOM standard reference, open-source libraries |
| `CHANGELOG.md` | Version history |
| `dicomqr-user-manual.md` | End-user guide |

## Notes

- App ID: `com.jeffreyleal.dicomqr`
- Settings persisted to `~/.dicomqr/settings.json`
- C-MOVE requires the embedded C-STORE SCP listener (default port 11112) to receive files
- The local AE Title (default `DICOMQR`) must be registered on the PACS as a known destination
- The app "owns" the download folder: downloads, imports, and manual scans keep `.dicomqr-index.db` in sync with the folder contents, and touching a tree entry whose files were removed externally prunes it from both the tree and the index
