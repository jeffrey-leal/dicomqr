# dicomqr

A Fyne-based Windows GUI application for querying and retrieving DICOM files from a PACS server.

## Build

Requires CGO and the mingw64 GCC toolchain. **Must be built from an MSYS2 MinGW64 terminal.**

Open MSYS2 MinGW64, then:

```bash
export PATH="/c/Program Files/Go/bin:$PATH"
cd /c/Users/jeffr/source/repos/dicomqr
```

Release build:
```bash
CGO_ENABLED=1 CC=/c/msys64/mingw64/bin/gcc.exe GOAMD64=v3 \
  go build -ldflags="-s -w -H windowsgui -X main.buildDate=$(date +%Y-%m-%d)" -o dicomqr.exe .
```

Development build (with debug info):
```bash
CGO_ENABLED=1 CC=/c/msys64/mingw64/bin/gcc.exe \
  go build -ldflags="-X main.buildDate=$(date +%Y-%m-%d)" -o dicomqr.exe .
```

## Project structure

| File | Purpose |
|---|---|
| `main.go` | App entry point, window layout, menu bar, connection panel, query panel, retrieve panel, status bar |
| `resultsmodel.go` | `resultsModel` — tree data structure for C-FIND query results |
| `queryrow.go` | `queryRow` — Fyne widget for results tree rows (hover tooltip, right-click menu) |
| `dicomnet.go` | `DicomClient` — SCU wrapper for C-ECHO, C-FIND, C-MOVE |
| `storagescp.go` | `StorageSCP` — embedded C-STORE SCP listener that receives C-MOVE deliveries |
| `settings.go` | `Settings` struct, load/save, embedded defaults |
| `serverprofile.go` | `ServerProfile` struct for saved server connections |
| `preferences.go` | `appTheme`, system font scanner, preferences dialog |
| `dicomfile.go` | `isDICOMFile` — magic-byte verification |

## Key dependencies

- `fyne.io/fyne/v2 v2.7.3` — GUI framework
- `github.com/algm/go-netdicom` — DICOM networking (C-ECHO, C-FIND, C-MOVE SCU, C-STORE SCP)
- `github.com/suyashkumar/dicom v1.1.0` — DICOM file parsing (for received files)
- `github.com/sqweek/dialog` — native Windows file/folder picker

## Notes

- App ID: `com.jeffreyleal.dicomqr`
- Settings persisted to `~/.dicomqr/settings.json`
- C-MOVE requires the embedded C-STORE SCP listener (default port 11112) to receive files
- The local AE Title (default `DICOMQR`) must be registered on the PACS as a known destination
