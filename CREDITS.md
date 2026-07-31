# Credits

## Attribution

dicomqr is a human–AI collaboration. Credit is given by role, reflecting
how the work was actually divided.

### Architecture & Direction

**Jeffrey Leal**
Email: jeffrey.leal@gmail.com
GitHub: https://github.com/jeffrey-leal

Program concept and architecture, feature design and requirements, field
testing against clinical PACS systems, and release decisions. The
application is built, tested, and published by Jeffrey Leal, who remains
responsible for the software.

### Implementation

**Claude** by [Anthropic](https://www.anthropic.com)

All application code, tests, and documentation were written by Claude
through [Claude Code](https://claude.ai/code), working to Jeffrey Leal's
architecture and direction — code generation, DICOM standard research,
debugging against field evidence, and the user manual and credits text.

## UI Template

This application's structure, conventions, and UI patterns are derived from
**dicomhdr** — a Fyne-based DICOM file inspector by the same developer.

https://github.com/jeffrey-leal/dicomhdr

## DICOM Standard Reference

Protocol implementation follows the DICOM Standard published by NEMA:

**DICOM PS3 (2024b)**
https://dicom.nema.org/medical/dicom/current

Sections referenced:
- PS3.4 — Service Class Specifications (Query/Retrieve, C.4)
- PS3.7 — Message Exchange (DIMSE-C services: C-ECHO, C-FIND, C-MOVE, C-STORE)
- PS3.8 — Network Communication / DICOM Upper Layer Protocol

## Open-Source Libraries

| Library | Author / Maintainer | License | Purpose |
|---|---|---|---|
| [fyne.io/fyne/v2](https://fyne.io) v2.7.3 | Fyne.io contributors | BSD 3-Clause | GUI framework |
| [algm/go-netdicom](https://github.com/algm/go-netdicom) v0.1.0 | Alan Griffin (fork of grailbio) | Apache 2.0 | DICOM network protocol (C-ECHO, C-FIND, C-MOVE, C-STORE SCP) |
| [grailbio/go-netdicom](https://github.com/grailbio/go-netdicom) | Yasushi Saito / GRAIL Inc. | Apache 2.0 | Original DICOM networking library (base of go-netdicom fork) |
| [grailbio/go-dicom](https://github.com/grailbio/go-dicom) | GRAIL Inc. | Apache 2.0 | DICOM dataset encoding / file header writing |
| [suyashkumar/dicom](https://github.com/suyashkumar/dicom) v1.1.0 | Suyash Kumar | MIT | DICOM file parsing for received files |
| [sqweek/dialog](https://github.com/sqweek/dialog) | sqweek | ISC | Native Windows file/folder picker dialogs |
| [catppuccin/fyne](https://github.com/catppuccin/fyne) v1.0.0 | Catppuccin community | MIT | Catppuccin colour themes (Preferences > Colour theme) |
| [fyne.io/x/fyne](https://github.com/fyne-io/fyne-x) (vendored excerpt) | Fyne.io contributors | BSD 3-Clause | Adwaita colour scheme (`adwaitatheme.go`) |

A vendored copy of `algm/go-netdicom` is included under `thirdparty/go-netdicom`
with its original Apache 2.0 licence intact. The Adwaita colour tables in
`adwaitatheme.go` are vendored from the fyne-x community repository (BSD
3-Clause) rather than imported, so the fyne-x module's tracking of Fyne's
development branch cannot silently upgrade the pinned GUI framework.

---

## Project License

dicomqr's own source code is released under the MIT License — see the
[LICENSE](LICENSE) file.

The third-party libraries listed above are used under their respective licenses
(BSD 3-Clause, Apache 2.0, MIT, and ISC), each of which permits this use with
attribution. The vendored copy of go-netdicom in `thirdparty/go-netdicom` is
distributed under the Apache License 2.0, with its original `LICENSE` file
retained.
