package main

// DICOMDIR builder — ported from dicomtool's cmd/dicomdir.go so a
// modification export can carry a File-set index (PS3.10) alongside the
// transformed files, letting a DICOM viewer or a CD/DVD-burning workflow
// browse the export without a database. Requested per profile via
// ModProfile.Dicomdir (modifyprofile.go) and built by
// runModificationImpl (modifyengine.go) from the in-memory datasets
// processFile already transformed — one dicomdirSource per successfully
// written file, collected as the run completes, so the export tree is never
// re-parsed to build it.
//
// The two-pass approach below (serialise with every offset field zeroed,
// locate each directory record's byte position, patch the offsets in place)
// is inherent to the DICOMDIR format: PATIENT/STUDY/SERIES/IMAGE records
// link to each other by absolute byte offset within the file, which can only
// be known once the file has been serialised once.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// dicomdirSOPClassUID is the Media Storage Directory Storage SOP Class.
// implementationClassUID identifies this writer — a fixed 2.25 UUID-rooted
// UID, not derived from anything file-specific — shared with dicomtool's own
// DICOMDIR writer, which this one is ported from.
const (
	dicomdirSOPClassUID    = "1.2.840.10008.1.3.10"
	implementationClassUID = "2.25.311926237095024698369566570265386635591"
)

// Byte patterns used to locate UL offset fields in the serialised DICOMDIR.
// Each is the 8-byte explicit-VR LE header for the element:
//
//	tag(4) + VR "UL"(2) + length 0x0004(2)
var (
	patFirstRecord = []byte{0x04, 0x00, 0x00, 0x12, 0x55, 0x4C, 0x04, 0x00} // (0004,1200)
	patLastRecord  = []byte{0x04, 0x00, 0x02, 0x12, 0x55, 0x4C, 0x04, 0x00} // (0004,1202)
	patNextRecord  = []byte{0x04, 0x00, 0x00, 0x14, 0x55, 0x4C, 0x04, 0x00} // (0004,1400)
	patLowerLevel  = []byte{0x04, 0x00, 0x20, 0x14, 0x55, 0x4C, 0x04, 0x00} // (0004,1420)
)

// ----------------------------------------------------------------------------
// Hierarchy types
// ----------------------------------------------------------------------------

type dicomdirImageInfo struct {
	fileComponents        []string // one element per path component
	sopClass, sopInstance string
	transferSyntax        string
	instanceNumber        string
}

type dicomdirSeriesInfo struct {
	uid, modality, number string
	images                []*dicomdirImageInfo
}

type dicomdirStudyInfo struct {
	uid, date, time, accession, id string
	series                         []*dicomdirSeriesInfo
}

type dicomdirPatientInfo struct {
	id, name string
	studies  []*dicomdirStudyInfo
}

// ----------------------------------------------------------------------------
// Flat record type used during construction
// ----------------------------------------------------------------------------

// recEntry holds a single directory record's elements and its position in the
// PATIENT → STUDY → SERIES → IMAGE linked-list tree.
type recEntry struct {
	items []*sdicom.Element
	next  int // index of next sibling in flat slice; -1 = none
	child int // index of first child in flat slice; -1 = none
}

// ----------------------------------------------------------------------------
// Source metadata
// ----------------------------------------------------------------------------

// dicomdirSource is the minimal per-file metadata needed to build a DICOMDIR,
// extracted once from a dataset processFile has already transformed. rel is
// the file's final path relative to the export root — the same path it was
// written under — and becomes its ReferencedFileID.
type dicomdirSource struct {
	rel                                                string
	patientID, patientName                             string
	studyUID, studyDate, studyTime, accession, studyID string
	seriesUID, modality, seriesNumber                  string
	sopClass, sopInstance, transferSyntax, instanceNum string
}

// extractDicomdirSource pulls the DICOMDIR-relevant fields from a parsed and
// transformed dataset, using dicomqr's existing dataset readers (chapters.go,
// transcode.go) rather than a duplicate string-value helper.
func extractDicomdirSource(ds *sdicom.Dataset, rel string) dicomdirSource {
	ts := datasetTransferSyntaxUID(ds)
	if ts == "" {
		ts = tsExplicitVRLE
	}
	return dicomdirSource{
		rel:            rel,
		patientID:      datasetString(ds, tag.PatientID),
		patientName:    datasetString(ds, tag.PatientName),
		studyUID:       datasetString(ds, tag.StudyInstanceUID),
		studyDate:      datasetString(ds, tag.StudyDate),
		studyTime:      datasetString(ds, tag.StudyTime),
		accession:      datasetString(ds, tag.AccessionNumber),
		studyID:        datasetString(ds, tag.StudyID),
		seriesUID:      datasetString(ds, tag.SeriesInstanceUID),
		modality:       datasetString(ds, tag.Modality),
		seriesNumber:   datasetString(ds, tag.SeriesNumber),
		sopClass:       datasetString(ds, tag.SOPClassUID),
		sopInstance:    datasetString(ds, tag.SOPInstanceUID),
		transferSyntax: ts,
		instanceNum:    datasetString(ds, tag.InstanceNumber),
	}
}

// ----------------------------------------------------------------------------
// Tree collection
// ----------------------------------------------------------------------------

// buildPatientsFromSources assembles the patient / study / series / image
// hierarchy from per-file sources. sources must already be sorted by rel, so
// patient/study/series ordering is deterministic regardless of which worker
// finished processing which file first.
func buildPatientsFromSources(sources []dicomdirSource) []*dicomdirPatientInfo {
	patMap := map[string]*dicomdirPatientInfo{}
	var patOrder []string

	for _, src := range sources {
		// DICOM ReferencedFileID is multi-valued CS, one component per path segment.
		components := strings.Split(filepath.ToSlash(src.rel), "/")

		pat, exists := patMap[src.patientID]
		if !exists {
			pat = &dicomdirPatientInfo{id: src.patientID, name: src.patientName}
			patMap[src.patientID] = pat
			patOrder = append(patOrder, src.patientID)
		}

		var study *dicomdirStudyInfo
		for _, s := range pat.studies {
			if s.uid == src.studyUID {
				study = s
				break
			}
		}
		if study == nil {
			study = &dicomdirStudyInfo{
				uid:       src.studyUID,
				date:      src.studyDate,
				time:      src.studyTime,
				accession: src.accession,
				id:        src.studyID,
			}
			pat.studies = append(pat.studies, study)
		}

		var series *dicomdirSeriesInfo
		for _, s := range study.series {
			if s.uid == src.seriesUID {
				series = s
				break
			}
		}
		if series == nil {
			series = &dicomdirSeriesInfo{
				uid:      src.seriesUID,
				modality: src.modality,
				number:   src.seriesNumber,
			}
			study.series = append(study.series, series)
		}

		series.images = append(series.images, &dicomdirImageInfo{
			fileComponents: components,
			sopClass:       src.sopClass,
			sopInstance:    src.sopInstance,
			transferSyntax: src.transferSyntax,
			instanceNumber: src.instanceNum,
		})
	}

	out := make([]*dicomdirPatientInfo, 0, len(patOrder))
	for _, id := range patOrder {
		out = append(out, patMap[id])
	}
	return out
}

// ----------------------------------------------------------------------------
// Flat record construction
// ----------------------------------------------------------------------------

// buildDicomdirRecords flattens the patient tree into a depth-first ordered
// []recEntry and wires the sibling/child index links.
// Returns the slice and the flat-slice index of the last PATIENT record.
func buildDicomdirRecords(patients []*dicomdirPatientInfo) ([]recEntry, int) {
	var recs []recEntry
	var patIndices []int

	for _, pat := range patients {
		pi := len(recs)
		patIndices = append(patIndices, pi)
		recs = append(recs, recEntry{items: buildPatientRecord(pat), next: -1, child: -1})

		var studyIndices []int
		for _, study := range pat.studies {
			si := len(recs)
			studyIndices = append(studyIndices, si)
			recs = append(recs, recEntry{items: buildStudyRecord(study), next: -1, child: -1})

			var seriesIndices []int
			for _, series := range study.series {
				sri := len(recs)
				seriesIndices = append(seriesIndices, sri)
				recs = append(recs, recEntry{items: buildSeriesRecord(series), next: -1, child: -1})

				var imgIndices []int
				for _, img := range series.images {
					ii := len(recs)
					imgIndices = append(imgIndices, ii)
					recs = append(recs, recEntry{items: buildImageRecord(img), next: -1, child: -1})
				}
				linkSiblings(recs, imgIndices)
				if len(imgIndices) > 0 {
					recs[sri].child = imgIndices[0]
				}
			}
			linkSiblings(recs, seriesIndices)
			if len(seriesIndices) > 0 {
				recs[si].child = seriesIndices[0]
			}
		}
		linkSiblings(recs, studyIndices)
		if len(studyIndices) > 0 {
			recs[pi].child = studyIndices[0]
		}
	}
	linkSiblings(recs, patIndices)

	lastPat := -1
	if len(patIndices) > 0 {
		lastPat = patIndices[len(patIndices)-1]
	}
	return recs, lastPat
}

// linkSiblings sets the .next field of each entry in indices to the following one.
func linkSiblings(recs []recEntry, indices []int) {
	for i := 0; i+1 < len(indices); i++ {
		recs[indices[i]].next = indices[i+1]
	}
}

// ----------------------------------------------------------------------------
// Dataset builder
// ----------------------------------------------------------------------------

func buildDicomdirDataset(sopInstanceUID string, recs []recEntry) sdicom.Dataset {
	seqItems := make([][]*sdicom.Element, len(recs))
	for i, r := range recs {
		seqItems[i] = r.items
	}

	var ds sdicom.Dataset
	ds.Elements = []*sdicom.Element{
		// File Meta (group 0002) — written by the library's own writer in
		// Explicit VR LE.
		mustElem(tag.FileMetaInformationVersion, []byte{0x00, 0x01}),
		mustElem(tag.MediaStorageSOPClassUID, []string{dicomdirSOPClassUID}),
		mustElem(tag.MediaStorageSOPInstanceUID, []string{sopInstanceUID}),
		mustElem(tag.TransferSyntaxUID, []string{tsExplicitVRLE}),
		mustElem(tag.ImplementationClassUID, []string{implementationClassUID}),
		mustElem(tag.ImplementationVersionName, []string{"DICOMQR_V1"}),
		// Group 0004 — ascending tag order required by standard.
		mustElem(tag.FileSetID, []string{""}),
		mustElem(tag.OffsetOfTheFirstDirectoryRecordOfTheRootDirectoryEntity, []int{0}),
		mustElem(tag.OffsetOfTheLastDirectoryRecordOfTheRootDirectoryEntity, []int{0}),
		mustElem(tag.FileSetConsistencyFlag, []int{0}),
		mustElem(tag.DirectoryRecordSequence, seqItems),
	}
	return ds
}

// ----------------------------------------------------------------------------
// Per-level record element builders (elements in ascending tag order)
// ----------------------------------------------------------------------------

func buildPatientRecord(p *dicomdirPatientInfo) []*sdicom.Element {
	return []*sdicom.Element{
		mustElem(tag.OffsetOfTheNextDirectoryRecord, []int{0}),              // (0004,1400)
		mustElem(tag.RecordInUseFlag, []int{0xFFFF}),                        // (0004,1410)
		mustElem(tag.OffsetOfReferencedLowerLevelDirectoryEntity, []int{0}), // (0004,1420)
		mustElem(tag.DirectoryRecordType, []string{"PATIENT"}),              // (0004,1430)
		mustElem(tag.PatientName, []string{p.name}),                         // (0010,0010)
		mustElem(tag.PatientID, []string{p.id}),                             // (0010,0020)
	}
}

func buildStudyRecord(s *dicomdirStudyInfo) []*sdicom.Element {
	return []*sdicom.Element{
		mustElem(tag.OffsetOfTheNextDirectoryRecord, []int{0}),
		mustElem(tag.RecordInUseFlag, []int{0xFFFF}),
		mustElem(tag.OffsetOfReferencedLowerLevelDirectoryEntity, []int{0}),
		mustElem(tag.DirectoryRecordType, []string{"STUDY"}),
		mustElem(tag.StudyDate, []string{s.date}),            // (0008,0020)
		mustElem(tag.StudyTime, []string{s.time}),            // (0008,0030)
		mustElem(tag.AccessionNumber, []string{s.accession}), // (0008,0050)
		mustElem(tag.StudyInstanceUID, []string{s.uid}),      // (0020,000D)
		mustElem(tag.StudyID, []string{s.id}),                // (0020,0010)
	}
}

func buildSeriesRecord(s *dicomdirSeriesInfo) []*sdicom.Element {
	return []*sdicom.Element{
		mustElem(tag.OffsetOfTheNextDirectoryRecord, []int{0}),
		mustElem(tag.RecordInUseFlag, []int{0xFFFF}),
		mustElem(tag.OffsetOfReferencedLowerLevelDirectoryEntity, []int{0}),
		mustElem(tag.DirectoryRecordType, []string{"SERIES"}),
		mustElem(tag.Modality, []string{s.modality}),     // (0008,0060)
		mustElem(tag.SeriesInstanceUID, []string{s.uid}), // (0020,000E)
		mustElem(tag.SeriesNumber, []string{s.number}),   // (0020,0011)
	}
}

func buildImageRecord(img *dicomdirImageInfo) []*sdicom.Element {
	return []*sdicom.Element{
		mustElem(tag.OffsetOfTheNextDirectoryRecord, []int{0}),
		mustElem(tag.RecordInUseFlag, []int{0xFFFF}),
		mustElem(tag.OffsetOfReferencedLowerLevelDirectoryEntity, []int{0}),
		mustElem(tag.DirectoryRecordType, []string{"IMAGE"}),
		mustElem(tag.ReferencedFileID, img.fileComponents),                            // (0004,1500) CS multi-value
		mustElem(tag.ReferencedSOPClassUIDInFile, []string{img.sopClass}),             // (0004,1510)
		mustElem(tag.ReferencedSOPInstanceUIDInFile, []string{img.sopInstance}),       // (0004,1511)
		mustElem(tag.ReferencedTransferSyntaxUIDInFile, []string{img.transferSyntax}), // (0004,1512)
		mustElem(tag.InstanceNumber, []string{img.instanceNumber}),                    // (0020,0013)
	}
}

// ----------------------------------------------------------------------------
// Public entry points
// ----------------------------------------------------------------------------

// buildDICOMDIRBytes builds a conformant DICOMDIR from sources and returns
// its serialised bytes, patched with real intra-file offsets. Returns
// (nil, nil) when sources is empty — nothing to index.
//
// Two passes: first serialise with every offset field zeroed, then locate
// every sequence-item start (FE FF 00 E0) in the buffer — each corresponds to
// one directory record in depth-first order — and patch the UL offset fields
// in place with the real byte positions.
func buildDICOMDIRBytes(sources []dicomdirSource) ([]byte, error) {
	sorted := make([]dicomdirSource, len(sources))
	copy(sorted, sources)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].rel < sorted[j].rel })

	patients := buildPatientsFromSources(sorted)
	if len(patients) == 0 {
		return nil, nil
	}

	sopInstanceUID := generateUID()
	recs, lastPatIdx := buildDicomdirRecords(patients)
	ds := buildDicomdirDataset(sopInstanceUID, recs)

	// Pass 1: serialise with all offsets = 0.
	var buf bytes.Buffer
	if err := sdicom.Write(&buf, ds); err != nil {
		return nil, fmt.Errorf("DICOMDIR first-pass write: %w", err)
	}
	data := buf.Bytes()

	// Locate every sequence-item start byte.
	positions := findSequenceItemPositions(data)
	if len(positions) != len(recs) {
		return nil, fmt.Errorf("DICOMDIR: record count mismatch — expected %d records, found %d item markers in output",
			len(recs), len(positions))
	}

	// Patch root-level first / last record pointers.
	patchUL32(data, 0, len(data), patFirstRecord, uint32(positions[0]))
	patchUL32(data, 0, len(data), patLastRecord, uint32(positions[lastPatIdx]))

	// Patch the next-sibling and first-child pointers inside each item.
	for i, rec := range recs {
		start := positions[i]
		end := len(data)
		if i+1 < len(positions) {
			end = positions[i+1]
		}

		nextOff := uint32(0)
		if rec.next >= 0 {
			nextOff = uint32(positions[rec.next])
		}
		childOff := uint32(0)
		if rec.child >= 0 {
			childOff = uint32(positions[rec.child])
		}

		patchUL32(data, start, end, patNextRecord, nextOff)
		patchUL32(data, start, end, patLowerLevel, childOff)
	}

	return data, nil
}

// writeDICOMDIRFile writes an already-built DICOMDIR (see buildDICOMDIRBytes)
// to outputDir/DICOMDIR — the folder-export counterpart of a zipSink entry
// write. A nil data is a no-op.
func writeDICOMDIRFile(outputDir string, data []byte) error {
	if data == nil {
		return nil
	}
	dest := filepath.Join(outputDir, "DICOMDIR")
	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create DICOMDIR: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write DICOMDIR: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------------------
// Two-pass helpers
// ----------------------------------------------------------------------------

// findSequenceItemPositions returns the byte offset of every DICOM sequence-item
// start tag (FFFE,E000 = FE FF 00 E0 in little-endian) found in data.
// Our DICOMDIR content (UIDs, names, dates) never contains this byte sequence,
// so all matches are genuine item boundaries.
func findSequenceItemPositions(data []byte) []int {
	marker := []byte{0xFE, 0xFF, 0x00, 0xE0}
	var positions []int
	// Advance by one past each hit: the 4-byte marker cannot self-overlap, so
	// this finds exactly the same set of starts as a per-byte scan, but lets the
	// optimized bytes.Index scanner do the work.
	for off := 0; off <= len(data)-len(marker); {
		i := bytes.Index(data[off:], marker)
		if i < 0 {
			break
		}
		positions = append(positions, off+i)
		off += i + 1
	}
	return positions
}

// patchUL32 locates the first occurrence of pattern within data[start:end] and
// overwrites the four bytes immediately following it with value (little-endian).
func patchUL32(data []byte, start, end int, pattern []byte, value uint32) {
	if end > len(data) {
		end = len(data)
	}
	idx := bytes.Index(data[start:end], pattern)
	if idx < 0 {
		return
	}
	off := start + idx + len(pattern)
	binary.LittleEndian.PutUint32(data[off:off+4], value)
}

// ----------------------------------------------------------------------------
// Utility
// ----------------------------------------------------------------------------

// mustElem creates an Element via the library's tag dictionary. Panics on
// error — every call site here passes a tag.* constant with a value of the
// VR's own kind, so a failure would mean the dictionary changed underfoot.
func mustElem(t tag.Tag, data any) *sdicom.Element {
	e, err := sdicom.NewElement(t, data)
	if err != nil {
		panic(fmt.Sprintf("mustElem %v: %v", t, err))
	}
	return e
}
