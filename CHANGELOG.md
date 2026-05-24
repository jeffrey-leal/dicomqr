# Changelog

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
