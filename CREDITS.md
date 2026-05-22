# Credits

## Developer

**Jeffrey Leal**
Email: jeffrey.leal@gmail.com
GitHub: https://github.com/jeffrey-leal

## AI Assistance

This application was designed and developed with the assistance of
**Claude Sonnet 4.6** by [Anthropic](https://www.anthropic.com).

Architecture planning, code generation, DICOM standard research, and
documentation were produced in collaboration with Claude Code.

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
| [algm/go-netdicom](https://github.com/algm/go-netdicom) v0.1.0 | Alan Griffin (fork of grailbio) | BSD 3-Clause | DICOM network protocol (C-ECHO, C-FIND, C-MOVE, C-STORE SCP) |
| [grailbio/go-netdicom](https://github.com/grailbio/go-netdicom) | Yasushi Saito / GRAIL Inc. | BSD 3-Clause | Original DICOM networking library (base of go-netdicom fork) |
| [grailbio/go-dicom](https://github.com/grailbio/go-dicom) | GRAIL Inc. | Apache 2.0 | DICOM dataset encoding / file header writing |
| [suyashkumar/dicom](https://github.com/suyashkumar/dicom) v1.1.0 | Suyash Kumar | MIT | DICOM file parsing for received files |
| [sqweek/dialog](https://github.com/sqweek/dialog) | sqweek | ISC | Native Windows file/folder picker dialogs |

A vendored copy of `algm/go-netdicom` is included under `thirdparty/go-netdicom`
with its original BSD 3-Clause licence intact.
