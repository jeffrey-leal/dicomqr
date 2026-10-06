package main

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// The PHI screen: a header-only look at a Modification run's files for content
// the profile's tag rules and pixel masking would leave untouched. It exists
// because a profile is written for the images its author had in mind, and
// the objects that carry identity outside the tags are exactly the ones nobody
// had in mind — the 2026-09 SOP-class survey found scanned forms, dose pages
// and saved screens filed as CT, MR, NM, PT or US, with the patient's name in
// the pixels. Nothing in the export would say they went out unmasked.
//
// It is an advisory, never a proof. It reads headers only, so it cannot see
// text burned into an ordinary greyscale slice; what it can do is name the
// kinds of object where burned-in text and embedded documents actually live
// and say whether the run does anything about them. The same classification
// runs twice: before the run, in the Modification dialog, over every file's
// header (screenPHIFiles), and during the run, over each exported file
// (processFile → fileNotes.phiRisk → modifyResult.PHIRisks), so the completion
// summary states what actually shipped rather than repeating the prediction.

// phiRisk is one reason a file may carry identity the run does not reach. A
// file's risks are a bit set; the pixel reasons are exclusive (the most
// specific one is reported), the other two independent of them.
type phiRisk uint8

const (
	// phiRiskBurnedIn: the file itself declares Burned In Annotation = YES and
	// no mask region covers it — the one pixel reason that is the vendor's
	// statement rather than an inference.
	phiRiskBurnedIn phiRisk = 1 << iota
	// phiRiskUltrasound: an ultrasound image no mask region covers. Ultrasound
	// screens carry a banner with the patient's name as a rule.
	phiRiskUltrasound
	// phiRiskCapture: a Secondary Capture object, or a colour image in a
	// modality that acquires greyscale, that no mask region covers — what
	// scanned forms, dose pages, saved screens and 3D renders look like.
	phiRiskCapture
	// phiRiskDocument: an encapsulated document (PDF, CDA, STL). The document
	// travels as an opaque value no tag rule reads into and no mask reaches, so
	// its contents leave exactly as they arrived.
	phiRiskDocument
	// phiRiskOverlay: overlay planes survive. Overlays are a bitmap channel
	// beside the pixels — annotations and text live there too — and only
	// Remove overlay planes reaches them.
	phiRiskOverlay
	// phiRiskText: one of phiTextTags carries a value and the run neither
	// removes nor replaces it — a comment field, or a demographic identifier
	// the profile's author did not think to list.
	phiRiskText
)

// phiRiskKinds lists the reasons in display order, strongest first.
var phiRiskKinds = []phiRisk{phiRiskBurnedIn, phiRiskDocument, phiRiskText, phiRiskUltrasound, phiRiskCapture, phiRiskOverlay}

// phiRiskPixel is the set of reasons pixel masking can clear.
const phiRiskPixel = phiRiskBurnedIn | phiRiskUltrasound | phiRiskCapture

// finding is the dialog's line for n files carrying the reason, before a run.
func (r phiRisk) finding(n int) string {
	switch r {
	case phiRiskBurnedIn:
		return plural(n, "image declares", "images declare") +
			" burned-in annotation (Burned In Annotation = YES) and no mask covers " + itThem(n) + "."
	case phiRiskDocument:
		return plural(n, "encapsulated document", "encapsulated documents") +
			" (PDF, CDA or STL): the document inside is exported as it arrived — no tag rule or mask reaches into it."
	case phiRiskUltrasound:
		return plural(n, "ultrasound image has", "ultrasound images have") +
			" no mask. Ultrasound screens nearly always carry a banner with the patient's name."
	case phiRiskCapture:
		return plural(n, "screen capture or colour image", "screen captures or colour images") +
			" in a greyscale modality with no mask — typically dose pages, scanned forms, saved screens or 3D renders, " +
			"where text is part of the image."
	case phiRiskOverlay:
		return plural(n, "file carries", "files carry") +
			" overlay planes, which can hold annotations, and Remove overlay planes is off."
	case phiRiskText:
		return plural(n, "file keeps", "files keep") +
			" values in free-text or identifying fields this profile neither removes nor replaces — " +
			"free text can hold names, dates and record numbers:"
	}
	return ""
}

// shipped is the completion summary's clause for n exported files that still
// carried the reason when they were written.
func (r phiRisk) shipped(n int) string {
	switch r {
	case phiRiskBurnedIn:
		return plural(n, "exported image declares", "exported images declare") + " burned-in annotation with no mask applied"
	case phiRiskDocument:
		return plural(n, "exported file holds", "exported files hold") + " an encapsulated document, exported as it arrived"
	case phiRiskUltrasound:
		return plural(n, "exported ultrasound image was", "exported ultrasound images were") + " not masked"
	case phiRiskCapture:
		return plural(n, "exported screen capture or colour image was", "exported screen captures or colour images were") +
			" not masked"
	case phiRiskOverlay:
		return plural(n, "exported file keeps", "exported files keep") + " overlay planes"
	case phiRiskText:
		return plural(n, "exported file keeps", "exported files keep") + " values in free-text or identifying fields"
	}
	return ""
}

// phiTextTags are the fields the text check looks at: the free-text comment
// fields and the demographic identifiers beyond name, ID and birth date that
// the DICOM Basic Application Level Confidentiality Profile (PS3.15 Table
// E.1-1) removes. Name, ID and birth date themselves are left out — every
// profile deals with those, and flagging them would only be noise — as are
// descriptions (Study, Series, Protocol), which carry names rarely enough
// that flagging every file would teach the user to ignore the check.
// Retired tags stay in: an old study still carries them. Checked at any depth,
// since removals and Set values recurse and a comment can sit inside a
// request sequence.
var phiTextTags = []tag.Tag{
	// Demographics a profile may not think to list.
	tag.OtherPatientIDs,
	tag.OtherPatientIDsSequence,
	tag.OtherPatientNames,
	tag.PatientBirthName,
	tag.PatientMotherBirthName,
	tag.PatientAddress,
	tag.PatientTelephoneNumbers,
	tag.PatientTelecomInformation,
	tag.MedicalRecordLocator,
	tag.AdmissionID,
	// Free text.
	tag.PatientComments,
	tag.AdditionalPatientHistory,
	tag.ImageComments,
	tag.StudyComments,
	tag.IdentifyingComments,
	tag.VisitComments,
	tag.CommentsOnThePerformedProcedureStep,
	tag.RequestedProcedureComments,
	tag.ImagingServiceRequestComments,
	tag.TextValue,
	tag.UnformattedTextValue,
}

// phiTextTagSet indexes phiTextTags.
var phiTextTagSet = func() map[tag.Tag]bool {
	m := make(map[tag.Tag]bool, len(phiTextTags))
	for _, t := range phiTextTags {
		m[t] = true
	}
	return m
}()

// textFieldsWithValues returns which phiTextTags carry a value anywhere in
// elements, sequence items included, in phiTextTags order. An element holding
// only blanks is empty; a sequence counts once any item holds an element.
func textFieldsWithValues(elements []*sdicom.Element) []tag.Tag {
	found := map[tag.Tag]bool{}
	var walk func([]*sdicom.Element)
	walk = func(elems []*sdicom.Element) {
		for _, e := range elems {
			if e == nil || e.Value == nil {
				continue
			}
			if e.Value.ValueType() == sdicom.Sequences {
				items, _ := e.Value.GetValue().([]*sdicom.SequenceItemValue)
				for _, item := range items {
					if item == nil {
						continue
					}
					sub, ok := item.GetValue().([]*sdicom.Element)
					if !ok {
						continue
					}
					if len(sub) > 0 && phiTextTagSet[e.Tag] {
						found[e.Tag] = true
					}
					walk(sub)
				}
				continue
			}
			if !phiTextTagSet[e.Tag] || found[e.Tag] {
				continue
			}
			if strs, ok := e.Value.GetValue().([]string); ok {
				for _, s := range strs {
					if strings.TrimSpace(strings.Trim(s, "\x00")) != "" {
						found[e.Tag] = true
						break
					}
				}
			} else if b, ok := e.Value.GetValue().([]byte); ok && len(strings.TrimSpace(strings.Trim(string(b), "\x00"))) > 0 {
				found[e.Tag] = true
			}
		}
	}
	walk(elements)
	var out []tag.Tag
	for _, t := range phiTextTags {
		if found[t] {
			out = append(out, t)
		}
	}
	return out
}

// textRulesFor returns the tags a run removes or replaces in a file of the
// given modality — the profile's removals and Set values with the matching
// override layered on exactly as processFile layers them (override edits
// merged in, override removals added, override keep list cancelling). A
// field in this set does not ship with its original value.
func textRulesFor(p modifyParams, modality string) map[tag.Tag]bool {
	edits, removals := p.edits, p.removals
	if ov, ok := p.perMod[strings.ToUpper(strings.TrimSpace(modality))]; ok {
		edits = mergeEdits(edits, ov.edits)
		removals = filterKeep(append(append([]tag.Tag(nil), removals...), ov.removals...), ov.keep)
	}
	handled := make(map[tag.Tag]bool, len(edits)+len(removals))
	for _, e := range edits {
		handled[e.tag] = true
	}
	for _, t := range removals {
		handled[t] = true
	}
	return handled
}

// plural renders "1 thing" or "N things".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func itThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// phiRiskSummary is the completion summary's clause for a run's counts, or ""
// when nothing risky shipped.
func phiRiskSummary(counts map[phiRisk]int) string {
	var parts []string
	for _, k := range phiRiskKinds {
		if n := counts[k]; n > 0 {
			parts = append(parts, k.shipped(n))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "; review before sharing: " + strings.Join(parts, ", ")
}

// greyscaleModalities are the modalities whose acquisitions are greyscale, so
// a colour image filed under one is a render, a fused display or a capture of
// a screen rather than the acquisition itself. NM and PT are included: their
// stored images are greyscale, and a colour one is a fused or annotated
// render — worth a look even when legitimate.
var greyscaleModalities = map[string]bool{
	"CT": true, "MR": true, "CR": true, "DX": true, "MG": true,
	"NM": true, "PT": true, "XA": true, "RF": true,
}

// encapsulatedDocumentPrefix is the standard's Encapsulated Document family
// (PDF .104.1, CDA .104.2, STL .104.3, and the later OBJ/MTL/…).
const encapsulatedDocumentPrefix = "1.2.840.10008.5.1.4.1.1.104."

// phiHeader is what the classification needs to know about one file, read
// from its header as it arrived.
type phiHeader struct {
	sopClass    string
	photometric string
	burnedIn    string
	cols, rows  int
	document    bool // carries an encapsulated document
	overlay     bool // carries overlay-plane elements
	// text: the phiTextTags carrying a value, in phiTextTags order.
	text []tag.Tag
	src  maskSource
}

// readPHIHeader reads a file's classification facts from a parsed dataset —
// a header read or a full parse alike, since everything it looks at precedes
// the pixel data.
func readPHIHeader(ds *sdicom.Dataset) phiHeader {
	sop := strings.TrimSpace(datasetFirstString(ds, tag.SOPClassUID))
	if sop == "" {
		sop = strings.TrimSpace(datasetFirstString(ds, tag.MediaStorageSOPClassUID))
	}
	h := phiHeader{
		sopClass:    sop,
		photometric: strings.ToUpper(strings.TrimSpace(datasetFirstString(ds, tag.PhotometricInterpretation))),
		burnedIn:    strings.ToUpper(strings.TrimSpace(datasetFirstString(ds, tag.BurnedInAnnotation))),
		cols:        datasetInt(ds, tag.Columns, 0),
		rows:        datasetInt(ds, tag.Rows, 0),
		src:         newMaskSource(ds),
	}
	h.document = strings.HasPrefix(sop, encapsulatedDocumentPrefix) || hasEncapsulatedDocument(ds)
	h.overlay = hasOverlayPlanes(ds)
	h.text = textFieldsWithValues(ds.Elements)
	return h
}

// hasEncapsulatedDocument reports whether ds carries an Encapsulated Document
// (0042,0011) element — the test for a private document class the UID prefix
// cannot recognise, and, after a run's removals, whether the document is
// still there.
func hasEncapsulatedDocument(ds *sdicom.Dataset) bool {
	_, err := ds.FindElementByTag(tag.EncapsulatedDocument)
	return err == nil
}

// hasOverlayPlanes reports whether ds carries any top-level overlay-plane
// element (the repeating groups 6000–601E, see isOverlayGroup).
func hasOverlayPlanes(ds *sdicom.Dataset) bool {
	for _, e := range ds.Elements {
		if isOverlayGroup(e.Tag.Group) {
			return true
		}
	}
	return false
}

// isImage reports whether the file has pixel dimensions — i.e. whether pixel
// masking is something that could apply to it at all.
func (h phiHeader) isImage() bool { return h.cols > 0 && h.rows > 0 }

// looksLikeCapture reports whether the image is a Secondary Capture object or
// a colour image in a modality that acquires greyscale.
func (h phiHeader) looksLikeCapture() bool {
	if matchesSOPClass(h.sopClass, secondaryCaptureSOPClasses) {
		return true
	}
	return isColourPhotometric(h.photometric) && greyscaleModalities[strings.ToUpper(h.src.modality)]
}

// isColourPhotometric reports whether a Photometric Interpretation is a colour
// one. An absent value is not: a file that does not say is not evidence.
func isColourPhotometric(p string) bool {
	switch p {
	case "", "MONOCHROME1", "MONOCHROME2":
		return false
	}
	return true
}

// maskHandles reports whether a run's governing mask regions deal with this
// image: they mask part of it, or mark it reviewed as needing none, or cannot
// resolve at all — which fails the file rather than exporting it, so it is
// not a leak (the review window shows it in red).
func maskHandles(src maskSource, regions []MaskRegion, cols, rows int) bool {
	res, err := maskRects(src, regions, cols, rows)
	return err != nil || len(res.rects) > 0 || res.exempt
}

// phiRules is what a run does to one file, as far as the classification
// cares: the mask regions governing it, whether overlay planes are removed,
// and the tags it removes or replaces (textRulesFor).
type phiRules struct {
	regions         []MaskRegion
	overlaysRemoved bool
	textHandled     map[tag.Tag]bool
}

// textLeft is the file's text fields the run leaves with their values.
func (h phiHeader) textLeft(handled map[tag.Tag]bool) []tag.Tag {
	var out []tag.Tag
	for _, t := range h.text {
		if !handled[t] {
			out = append(out, t)
		}
	}
	return out
}

// risks classifies the file against what the run does to it.
func (h phiHeader) risks(rules phiRules) phiRisk {
	var r phiRisk
	if h.document {
		r |= phiRiskDocument
	}
	if h.overlay && !rules.overlaysRemoved {
		r |= phiRiskOverlay
	}
	if len(h.textLeft(rules.textHandled)) > 0 {
		r |= phiRiskText
	}
	if h.isImage() && !maskHandles(h.src, rules.regions, h.cols, h.rows) {
		switch {
		case h.burnedIn == "YES":
			r |= phiRiskBurnedIn
		case h.src.isUltrasound():
			r |= phiRiskUltrasound
		case h.looksLikeCapture():
			r |= phiRiskCapture
		}
	}
	return r
}

// skippedByFilters reports whether a run's file filters (ignoretype,
// ignoremodality, ignoresopclass — the last unioned with the matching
// override's list) skip this file. processFile and the PHI screen share it, so
// the screen never flags a file the run would not export.
func skippedByFilters(ds *sdicom.Dataset, p modifyParams) bool {
	if len(p.ignoreTypes) > 0 {
		if elem, err := ds.FindElementByTag(imageTypeTag); err == nil {
			for _, component := range elemStringComponents(elem) {
				for _, ignore := range p.ignoreTypes {
					if strings.EqualFold(component, strings.TrimSpace(ignore)) {
						return true
					}
				}
			}
		}
	}
	if len(p.ignoreModalities) > 0 {
		if elem, err := ds.FindElementByTag(modalityTag); err == nil {
			for _, component := range elemStringComponents(elem) {
				for _, ignore := range p.ignoreModalities {
					if strings.EqualFold(component, strings.TrimSpace(ignore)) {
						return true
					}
				}
			}
		}
	}
	return skipsSOPClass(ds, p)
}

// phiScreenFile is one readable file of the run, as the screen read it.
type phiScreenFile struct {
	path string
	// skipped: the profile's filters skip this file, so it is never flagged.
	// Kept rather than dropped because the review window needs every file of
	// a series to decide what a series-wide rectangle reaches (reviewSetFor).
	skipped bool
	phiHeader
}

// screenPHIFiles reads every file's header on the scan worker pool and returns
// every readable one in the order given, each marked with whether the
// profile's filters skip it. unreadable counts files whose header would not parse — the run fails
// or skips those itself. stop, when set, ends the scan early (the dialog
// closed); what was read so far is returned.
func screenPHIFiles(files []string, filters modifyParams, stop *atomic.Bool,
	progress func(done, total int)) (out []phiScreenFile, unreadable int) {

	total := len(files)
	if total == 0 {
		return nil, 0
	}
	type result struct {
		file phiScreenFile
		bad  bool
	}
	results := make([]result, total)
	var done atomic.Int64
	var publish func()
	if progress != nil {
		publish = func() { progress(int(done.Load()), total) }
	}
	stopReporting := startPacedProgress(scanProgressInterval, publish)

	jobs := make(chan int)
	var wg sync.WaitGroup
	for range scanWorkers(total) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				ds, err := readDicomHeader(files[i])
				if err != nil {
					results[i].bad = true
				} else {
					results[i].file = phiScreenFile{path: files[i], skipped: skippedByFilters(&ds, filters),
						phiHeader: readPHIHeader(&ds)}
				}
				done.Add(1)
			}
		}()
	}
feed:
	for i := range files {
		if stop != nil && stop.Load() {
			break feed
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	stopReporting()
	if progress != nil {
		progress(int(done.Load()), total)
	}
	for _, r := range results {
		switch {
		case r.bad:
			unreadable++
		case r.file.path != "": // empty when the scan was stopped before it
			out = append(out, r.file)
		}
	}
	return out, unreadable
}

// phiFindings is the screen's result against the run's current settings: the
// files carrying each reason, in screen order.
type phiFindings struct {
	byRisk map[phiRisk][]string
	// textCounts is how many files leave each text field with its value —
	// the breakdown under the text finding, and the tags its remedy removes.
	textCounts map[tag.Tag]int
}

// evaluatePHIScreen classifies every screened file. rulesFor returns what the
// run does to a file of a modality (mask regions from the working set, the
// Remove overlay planes box, the compiled removals and Set values). Pure over
// the scan results, so the dialog re-runs it on every relevant edit.
func evaluatePHIScreen(files []phiScreenFile, rulesFor func(modality string) phiRules) phiFindings {
	f := phiFindings{byRisk: map[phiRisk][]string{}, textCounts: map[tag.Tag]int{}}
	for _, file := range files {
		if file.skipped {
			continue
		}
		rules := rulesFor(file.src.modality)
		r := file.risks(rules)
		for _, k := range phiRiskKinds {
			if r&k != 0 {
				f.byRisk[k] = append(f.byRisk[k], file.path)
			}
		}
		for _, t := range file.textLeft(rules.textHandled) {
			f.textCounts[t]++
		}
	}
	return f
}

// textTags is the flagged text fields in phiTextTags order.
func (f phiFindings) textTags() []tag.Tag {
	var out []tag.Tag
	for _, t := range phiTextTags {
		if f.textCounts[t] > 0 {
			out = append(out, t)
		}
	}
	return out
}

// textBreakdown names each flagged text field with its file count, e.g.
// "Image Comments (0020,4000) in 12 files".
func (f phiFindings) textBreakdown() string {
	var parts []string
	for _, t := range f.textTags() {
		parts = append(parts, phiTagLabel(t)+" in "+plural(f.textCounts[t], "file", "files"))
	}
	return strings.Join(parts, "; ")
}

// phiTagLabel renders a tag as "Name (GGGG,EEEE)", or just the number when
// the dictionary has no name for it.
func phiTagLabel(t tag.Tag) string {
	num := formatTagRef(t)
	if name := tagDisplayName(t); name != "" {
		return name + " (" + num + ")"
	}
	return num
}

// reviewSetFor is what the review window opens for a set of flagged files:
// every screened file of each series a flagged file belongs to, skipped ones
// included, in screen order. Whole series rather than just the flagged files
// because a rectangle's "Current series" scope reaches the series' files of
// the same geometry, and whether a series has chapters to confine it to is
// decided from all of its files (seriesHasChapters) — opened on a subset, the
// window could decide differently from the full review and silently reach
// files the user never saw. A flagged file with no series UID comes alone.
func reviewSetFor(screened []phiScreenFile, flagged []string) []string {
	want := make(map[string]bool, len(flagged))
	for _, p := range flagged {
		want[p] = true
	}
	series := map[string]bool{}
	for _, f := range screened {
		if want[f.path] && f.src.seriesInstanceUID != "" {
			series[f.src.seriesInstanceUID] = true
		}
	}
	var out []string
	for _, f := range screened {
		if want[f.path] || (f.src.seriesInstanceUID != "" && series[f.src.seriesInstanceUID]) {
			out = append(out, f.path)
		}
	}
	return out
}

// withoutFiles returns files minus every path in drop, order kept — how the
// dialog leaves flagged documents out of a run without widening a SOP-class
// filter to files nobody flagged.
func withoutFiles(files, drop []string) []string {
	if len(drop) == 0 {
		return files
	}
	skip := make(map[string]bool, len(drop))
	for _, p := range drop {
		skip[p] = true
	}
	out := make([]string, 0, len(files))
	for _, p := range files {
		if !skip[p] {
			out = append(out, p)
		}
	}
	return out
}

// pixelFiles is every file flagged for a reason masking can clear, once each
// and in screen order — what the dialog's review button opens.
func (f phiFindings) pixelFiles() []string {
	var out []string
	for _, k := range phiRiskKinds {
		if k&phiRiskPixel != 0 {
			out = append(out, f.byRisk[k]...)
		}
	}
	return out
}

// empty reports whether nothing was flagged.
func (f phiFindings) empty() bool {
	for _, files := range f.byRisk {
		if len(files) > 0 {
			return false
		}
	}
	return true
}
