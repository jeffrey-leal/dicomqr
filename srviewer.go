package main

import (
	"fmt"
	"os"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// ── Modality detection ────────────────────────────────────────────────────────

// isDocumentModality returns true for modalities that carry no pixel data and
// should be rendered in the SR document viewer instead of the image viewer.
func isDocumentModality(mod string) bool {
	switch strings.ToUpper(strings.TrimSpace(mod)) {
	case "SR", "KO", "AU", "PR":
		return true
	}
	return false
}

// seriesModality returns the DICOM Modality string from the first file in
// paths using a fast early-exit streaming parse (stops after group 0x0008).
func seriesModality(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	f, err := os.Open(paths[0])
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	p, err := sdicom.NewParser(f, info.Size(), nil, sdicom.SkipPixelData())
	if err != nil {
		return ""
	}
	for {
		elem, err := p.Next()
		if err != nil {
			break
		}
		if elem.Tag.Group > 0x0008 {
			break
		}
		if elem.Tag == tag.Modality {
			if strs, ok := elem.Value.GetValue().([]string); ok && len(strs) > 0 {
				return strings.TrimSpace(strs[0])
			}
		}
	}
	return ""
}

// ── Data types ────────────────────────────────────────────────────────────────

// srEntry is one structured content item from the SR Content Sequence, flattened
// into a form suitable for markdown rendering and plain-text clipboard export.
type srEntry struct {
	depth   int
	heading bool   // true for CONTAINER type items
	label   string // ConceptName CodeMeaning (may be empty)
	value   string // formatted value (may be empty for CONTAINER)
}

// srDoc holds the parsed content of one SR (or KO/AU) document instance.
type srDoc struct {
	ann         imageAnnotations
	contentDate string // formatted content date/time (from 0008,0023/0033)
	status      string // e.g. "COMPLETE · VERIFIED"
	entries     []srEntry
}

// ── File parsing ──────────────────────────────────────────────────────────────

// parseSRFile parses a DICOM SR/KO/AU file and returns structured header info
// plus a flat list of content entries ready for rendering.
func parseSRFile(path string) (srDoc, error) {
	ds, err := sdicom.ParseFile(path, nil)
	if err != nil {
		return srDoc{}, err
	}

	ann := extractAnnotationsFromDataset(ds)

	// Content Date / Time (distinct from Study Date, reflects when the SR was authored)
	contentDate := ""
	if e, err2 := ds.FindElementByTag(tag.ContentDate); err2 == nil {
		if strs, ok := e.Value.GetValue().([]string); ok && len(strs) > 0 {
			contentDate = formatDicomDate(strings.TrimSpace(strs[0]))
		}
	}
	if e, err2 := ds.FindElementByTag(tag.ContentTime); err2 == nil {
		if strs, ok := e.Value.GetValue().([]string); ok && len(strs) > 0 {
			if t := formatDicomTime(strings.TrimSpace(strs[0])); t != "" {
				if contentDate != "" {
					contentDate += "  " + t
				} else {
					contentDate = t
				}
			}
		}
	}

	// SR workflow flags
	var flags []string
	for _, flagTag := range []tag.Tag{tag.CompletionFlag, tag.VerificationFlag} {
		if e, err2 := ds.FindElementByTag(flagTag); err2 == nil {
			if strs, ok := e.Value.GetValue().([]string); ok && len(strs) > 0 {
				if s := strings.TrimSpace(strs[0]); s != "" {
					flags = append(flags, s)
				}
			}
		}
	}

	var entries []srEntry
	walkSRContentSeq(ds, 0, &entries)

	return srDoc{
		ann:         ann,
		contentDate: contentDate,
		status:      strings.Join(flags, " · "),
		entries:     entries,
	}, nil
}

// walkSRContentSeq recursively extracts content items from the DICOM SR Content
// Sequence (0040,A730) and appends them to out. depth tracks the nesting level.
func walkSRContentSeq(ds sdicom.Dataset, depth int, out *[]srEntry) {
	seqElem, err := ds.FindElementByTag(tag.ContentSequence)
	if err != nil {
		return
	}
	items, ok := seqElem.Value.GetValue().([]*sdicom.SequenceItemValue)
	if !ok {
		return
	}
	for _, item := range items {
		elems, ok2 := item.GetValue().([]*sdicom.Element)
		if !ok2 {
			continue
		}
		itemDS := sdicom.Dataset{Elements: elems}

		vt := strings.ToUpper(srDSStr(itemDS, tag.ValueType))
		conceptName := srCodeMeaning(itemDS, tag.ConceptNameCodeSequence)
		isContainer := vt == "CONTAINER"

		var value string
		switch vt {
		case "TEXT":
			// Collapse internal whitespace — DICOM UT values sometimes contain raw
			// CR/LF from dictation software that would break markdown rendering.
			value = strings.Join(strings.Fields(srDSStr(itemDS, tag.TextValue)), " ")
		case "NUM":
			num := srDSStr(itemDS, tag.NumericValue)
			units := srCodeMeaning(itemDS, tag.MeasurementUnitsCodeSequence)
			if units != "" && units != "1" { // "1" is the DICOM no-units sentinel
				value = num + " " + units
			} else {
				value = num
			}
		case "CODE":
			value = srCodeMeaning(itemDS, tag.ConceptCodeSequence)
		case "DATE":
			value = formatDicomDate(srDSStr(itemDS, tag.Date))
		case "TIME":
			value = formatDicomTime(srDSStr(itemDS, tag.Time))
		case "PNAME":
			value = formatDicomPersonName(srDSStr(itemDS, tag.PersonName))
		case "UIDREF":
			value = srDSStr(itemDS, tag.UID)
		case "IMAGE":
			value = srImageRef(itemDS)
		}

		if isContainer || conceptName != "" || value != "" {
			*out = append(*out, srEntry{
				depth:   depth,
				heading: isContainer,
				label:   conceptName,
				value:   value,
			})
		}

		if isContainer {
			walkSRContentSeq(itemDS, depth+1, out)
		}
	}
}

// ── SR parsing helpers ────────────────────────────────────────────────────────

// srDSStr returns the first trimmed string value of tag t in ds, or "".
func srDSStr(ds sdicom.Dataset, t tag.Tag) string {
	e, err := ds.FindElementByTag(t)
	if err != nil {
		return ""
	}
	strs, ok := e.Value.GetValue().([]string)
	if !ok || len(strs) == 0 {
		return ""
	}
	return strings.TrimSpace(strs[0])
}

// srCodeMeaning returns the CodeMeaning string from the first item of a DICOM
// code sequence element (e.g. ConceptNameCodeSequence, ConceptCodeSequence).
func srCodeMeaning(ds sdicom.Dataset, seqTag tag.Tag) string {
	e, err := ds.FindElementByTag(seqTag)
	if err != nil {
		return ""
	}
	items, ok := e.Value.GetValue().([]*sdicom.SequenceItemValue)
	if !ok || len(items) == 0 {
		return ""
	}
	elems, ok2 := items[0].GetValue().([]*sdicom.Element)
	if !ok2 {
		return ""
	}
	return srDSStr(sdicom.Dataset{Elements: elems}, tag.CodeMeaning)
}

// srImageRef builds a short display string for an IMAGE type content item.
func srImageRef(ds sdicom.Dataset) string {
	e, err := ds.FindElementByTag(tag.ReferencedSOPSequence)
	if err != nil {
		return "[Image]"
	}
	items, ok := e.Value.GetValue().([]*sdicom.SequenceItemValue)
	if !ok || len(items) == 0 {
		return "[Image]"
	}
	elems, ok2 := items[0].GetValue().([]*sdicom.Element)
	if !ok2 {
		return "[Image]"
	}
	uid := srDSStr(sdicom.Dataset{Elements: elems}, tag.ReferencedSOPInstanceUID)
	if uid != "" {
		return "[Image: " + uid + "]"
	}
	return "[Image]"
}

// ── Formatting helpers ────────────────────────────────────────────────────────

// srEntriesToMarkdown converts SR entries into a Fyne-compatible markdown string.
// CONTAINER items become headings; leaf items render as "**label:** value" pairs.
func srEntriesToMarkdown(entries []srEntry) string {
	var sb strings.Builder
	for _, e := range entries {
		if e.heading {
			level := e.depth + 2 // depth 0 → "##", depth 1 → "###", depth 2+ → "####"
			if level > 4 {
				level = 4
			}
			name := e.label
			if name == "" {
				name = "Section"
			}
			fmt.Fprintf(&sb, "%s %s\n\n", strings.Repeat("#", level), srMDEscape(name))
		} else if e.label != "" && e.value != "" {
			fmt.Fprintf(&sb, "**%s:** %s\n\n", srMDEscape(e.label), e.value)
		} else if e.value != "" {
			fmt.Fprintf(&sb, "%s\n\n", e.value)
		} else if e.label != "" {
			fmt.Fprintf(&sb, "*%s*\n\n", srMDEscape(e.label))
		}
	}
	return strings.TrimSpace(sb.String())
}

// srEntriesToPlainText converts SR entries to plain text for clipboard export.
func srEntriesToPlainText(entries []srEntry) string {
	var sb strings.Builder
	for _, e := range entries {
		indent := strings.Repeat("  ", e.depth)
		if e.heading {
			name := e.label
			if name == "" {
				name = "Section"
			}
			fmt.Fprintf(&sb, "%s%s\n", indent, strings.ToUpper(name))
		} else if e.label != "" && e.value != "" {
			fmt.Fprintf(&sb, "%s%s: %s\n", indent, e.label, e.value)
		} else if e.value != "" {
			fmt.Fprintf(&sb, "%s%s\n", indent, e.value)
		}
	}
	return sb.String()
}

// srMDEscape escapes markdown special characters found in DICOM text content.
func srMDEscape(s string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`*`, `\*`,
		`_`, `\_`,
		`#`, `\#`,
		"`", "\\`",
		`[`, `\[`,
		`]`, `\]`,
	).Replace(s)
}

// srFormatHeader builds the multi-line header string shown above the SR content.
func srFormatHeader(doc srDoc) string {
	ann := doc.ann
	var lines []string

	var patParts []string
	if ann.patientName != "" {
		patParts = append(patParts, ann.patientName)
	}
	if ann.patientID != "" {
		patParts = append(patParts, "MRN: "+ann.patientID)
	}
	if ann.patientDOB != "" {
		patParts = append(patParts, "DOB: "+ann.patientDOB)
	}
	if ann.patientSexAge != "" {
		patParts = append(patParts, ann.patientSexAge)
	}
	if len(patParts) > 0 {
		lines = append(lines, strings.Join(patParts, "   "))
	}

	var studyParts []string
	if ann.studyDate != "" {
		studyParts = append(studyParts, ann.studyDate)
	}
	if ann.accession != "" {
		studyParts = append(studyParts, "Acc: "+ann.accession)
	}
	if ann.studyDesc != "" {
		studyParts = append(studyParts, ann.studyDesc)
	}
	if len(studyParts) > 0 {
		lines = append(lines, strings.Join(studyParts, "   "))
	}

	var serParts []string
	if ann.modality != "" {
		serParts = append(serParts, ann.modality)
	}
	if ann.seriesInfo != "" {
		serParts = append(serParts, ann.seriesInfo)
	}
	if doc.contentDate != "" && doc.contentDate != ann.studyDate {
		serParts = append(serParts, "Content: "+doc.contentDate)
	}
	if doc.status != "" {
		serParts = append(serParts, doc.status)
	}
	if len(serParts) > 0 {
		lines = append(lines, strings.Join(serParts, "   "))
	}

	return strings.Join(lines, "\n")
}

// ── Viewer window ─────────────────────────────────────────────────────────────

// openSRWindow opens a document viewer window for SR, KO, and other non-image
// DICOM objects. Must be called from a non-UI goroutine.
func openSRWindow(a fyne.App, title string, paths []string) {
	fyne.Do(func() {
		win := a.NewWindow(title)
		total := len(paths)
		current := 0

		headerLbl := widget.NewLabel("Loading…")
		headerLbl.Wrapping = fyne.TextWrapWord

		rt := widget.NewRichText()
		rt.Wrapping = fyne.TextWrapWord

		scroll := container.NewVScroll(rt)

		counterLbl := widget.NewLabel(fmt.Sprintf("1 / %d", total))
		counterLbl.Alignment = fyne.TextAlignCenter

		copyText := ""

		prevBtn := widget.NewButton("< Prev", nil)
		nextBtn := widget.NewButton("Next >", nil)
		copyBtn := widget.NewButton("Copy text", func() {
			win.Clipboard().SetContent(copyText)
		})

		if total <= 1 {
			prevBtn.Disable()
			nextBtn.Disable()
		}

		var loadDoc func(idx int)
		loadDoc = func(idx int) {
			counterLbl.SetText(fmt.Sprintf("Loading… (%d / %d)", idx+1, total))
			p := paths[idx]
			go func() {
				doc, err := parseSRFile(p)
				fyne.Do(func() {
					counterLbl.SetText(fmt.Sprintf("%d / %d", idx+1, total))
					if err != nil {
						headerLbl.SetText("Error loading document: " + err.Error())
						rt.ParseMarkdown("")
						return
					}

					headerLbl.SetText(srFormatHeader(doc))

					md := srEntriesToMarkdown(doc.entries)
					if md == "" {
						md = "*(No structured content found in this document.)*"
					}
					rt.ParseMarkdown(md)
					scroll.ScrollToTop()

					copyText = srEntriesToPlainText(doc.entries)

					if idx > 0 {
						prevBtn.Enable()
					} else {
						prevBtn.Disable()
					}
					if idx < total-1 {
						nextBtn.Enable()
					} else {
						nextBtn.Disable()
					}
				})
			}()
		}

		prevBtn.OnTapped = func() {
			if current > 0 {
				current--
				loadDoc(current)
			}
		}
		nextBtn.OnTapped = func() {
			if current < total-1 {
				current++
				loadDoc(current)
			}
		}

		header := container.NewVBox(headerLbl, widget.NewSeparator())
		footer := container.NewBorder(nil, nil,
			container.NewHBox(prevBtn, nextBtn),
			copyBtn,
			container.NewCenter(counterLbl),
		)

		win.SetContent(container.NewBorder(header, footer, nil, nil, scroll))
		win.Resize(fyne.NewSize(700, 700))
		win.Show()

		loadDoc(0)
	})
}
