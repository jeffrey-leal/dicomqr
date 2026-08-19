package main

import (
	"bytes"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// dicomdirRecordItem is the flattened view of one parsed directory record —
// its own fields plus next/child as record indices (-1 = none), already
// resolved from the raw byte offsets a generic DICOM reader would see.
type dicomdirRecordItem struct {
	recordType string
	next       int
	child      int
	fields     map[tag.Tag][]string
}

// parseDICOMDIR re-parses built DICOMDIR bytes with the library's own reader
// (not dicomdir.go's own machinery) and resolves every declared offset back
// to a record index, so the tree can be walked purely from what a generic
// DICOM consumer would see. firstIdx/lastIdx are the root's declared
// first/last record, likewise resolved to indices.
func parseDICOMDIR(t *testing.T, data []byte) (records []dicomdirRecordItem, firstIdx, lastIdx int) {
	t.Helper()
	ds, err := sdicom.Parse(bytes.NewReader(data), int64(len(data)), nil)
	if err != nil {
		t.Fatalf("parse DICOMDIR: %v", err)
	}
	getUL := func(tg tag.Tag) uint32 {
		e, err := ds.FindElementByTag(tg)
		if err != nil {
			t.Fatalf("missing %v: %v", tg, err)
		}
		v, ok := e.Value.GetValue().([]int)
		if !ok || len(v) == 0 {
			t.Fatalf("%v not an int list: %#v", tg, e.Value.GetValue())
		}
		return uint32(v[0])
	}
	firstOff := getUL(tag.OffsetOfTheFirstDirectoryRecordOfTheRootDirectoryEntity)
	lastOff := getUL(tag.OffsetOfTheLastDirectoryRecordOfTheRootDirectoryEntity)

	seqElem, err := ds.FindElementByTag(tag.DirectoryRecordSequence)
	if err != nil {
		t.Fatalf("DirectoryRecordSequence missing: %v", err)
	}
	items, ok := seqElem.Value.GetValue().([]*sdicom.SequenceItemValue)
	if !ok {
		t.Fatalf("DirectoryRecordSequence not a sequence: %#v", seqElem.Value.GetValue())
	}

	// positions is computed independently of the writer's own bookkeeping —
	// it re-scans the raw bytes for item markers, the same way patchUL32's
	// caller does when it decides what value to write. Cross-checking every
	// declared offset against this independent scan is what makes the test
	// meaningful: it fails if a patched offset ever points anywhere but a
	// real item boundary.
	positions := findSequenceItemPositions(data)
	if len(positions) != len(items) {
		t.Fatalf("item marker count = %d, want %d (one per record)", len(positions), len(items))
	}
	offsetToIndex := make(map[uint32]int, len(positions))
	for i, pos := range positions {
		offsetToIndex[uint32(pos)] = i
	}
	resolve := func(offset uint32, context string) int {
		if offset == 0 {
			return -1
		}
		idx, known := offsetToIndex[offset]
		if !known {
			t.Fatalf("%s offset %d matches no item marker", context, offset)
		}
		return idx
	}

	records = make([]dicomdirRecordItem, len(items))
	for i, item := range items {
		elems, ok := item.GetValue().([]*sdicom.Element)
		if !ok {
			t.Fatalf("record %d: item not an element list", i)
		}
		rec := dicomdirRecordItem{next: -1, child: -1, fields: map[tag.Tag][]string{}}
		for _, e := range elems {
			switch e.Tag {
			case tag.OffsetOfTheNextDirectoryRecord:
				if v, ok := e.Value.GetValue().([]int); ok && len(v) > 0 {
					rec.next = resolve(uint32(v[0]), "next")
				}
			case tag.OffsetOfReferencedLowerLevelDirectoryEntity:
				if v, ok := e.Value.GetValue().([]int); ok && len(v) > 0 {
					rec.child = resolve(uint32(v[0]), "child")
				}
			case tag.DirectoryRecordType:
				if v, ok := e.Value.GetValue().([]string); ok && len(v) > 0 {
					rec.recordType = v[0]
				}
			default:
				if v, ok := e.Value.GetValue().([]string); ok {
					rec.fields[e.Tag] = v
				}
			}
		}
		records[i] = rec
	}

	firstIdx = resolve(firstOff, "root first-record")
	lastIdx = resolve(lastOff, "root last-record")
	return records, firstIdx, lastIdx
}

// walkSiblings follows .next from startIdx and returns every visited index in
// order — the record-index counterpart of walking a DICOMDIR level by hand.
func walkSiblings(records []dicomdirRecordItem, startIdx int) []int {
	var order []int
	for i := startIdx; i != -1; i = records[i].next {
		order = append(order, i)
	}
	return order
}

// TestBuildDICOMDIRBytesEmpty confirms an empty source list is a deliberate
// no-op, not an error — matching runModificationImpl's own gate of never
// calling this with zero collected sources.
func TestBuildDICOMDIRBytesEmpty(t *testing.T) {
	data, err := buildDICOMDIRBytes(nil)
	if err != nil || data != nil {
		t.Fatalf("buildDICOMDIRBytes(nil) = (%d bytes, %v), want (nil, nil)", len(data), err)
	}
}

// oneImageDicomdirSources is the smallest complete hierarchy: one patient, one
// study, one series, one image — four directory records.
func oneImageDicomdirSources() []dicomdirSource {
	return []dicomdirSource{{
		rel: "STUDY1/SERIES1/img1.dcm", patientID: "PAT1", patientName: "ONE^PATIENT",
		studyUID: "1.2.1", studyDate: "20240101", studyID: "S1",
		seriesUID: "1.2.1.1", modality: "CT", seriesNumber: "1",
		sopClass: "1.2.840.10008.5.1.4.1.1.7", sopInstance: "1.2.1.1.1",
		transferSyntax: tsExplicitVRLE, instanceNum: "1",
	}}
}

// TestBuildDICOMDIRUnpatchableOffsetFails covers the guard on patchUL32.
//
// The pat* patterns are the literal Explicit VR LE headers of the offset
// elements, so the builder asserts a byte-level encoding it cannot verify.
// Corrupting one stands in for the writer encoding a UL element differently —
// a library upgrade, or a switch to implicit VR. Without the guard the offsets
// would silently stay 0, which is a structurally valid DICOMDIR that parses,
// carries every name and UID, and reads as an EMPTY file-set: exactly the
// failure a "no error" test would miss. The build must fail instead.
//
// These subtests reassign package-level vars, so they must not run in parallel.
func TestBuildDICOMDIRUnpatchableOffsetFails(t *testing.T) {
	corrupt := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x55, 0x4C, 0x04, 0x00}

	for _, tc := range []struct {
		name string
		pat  *[]byte
	}{
		{"root first-record pointer", &patFirstRecord},
		{"root last-record pointer", &patLastRecord},
		{"per-record next-sibling pointer", &patNextRecord},
		{"per-record first-child pointer", &patLowerLevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := *tc.pat
			t.Cleanup(func() { *tc.pat = original })
			*tc.pat = corrupt

			data, err := buildDICOMDIRBytes(oneImageDicomdirSources())
			if err == nil {
				t.Fatalf("build succeeded with an unpatchable %s — an index whose offsets "+
					"stayed 0 would ship as an empty file-set", tc.name)
			}
			if data != nil {
				t.Errorf("build returned %d bytes alongside its error; a half-patched index must not escape", len(data))
			}
		})
	}

	// With every pattern restored the same fixture must build cleanly, so the
	// subtests above prove the guard rather than a broken fixture.
	if _, err := buildDICOMDIRBytes(oneImageDicomdirSources()); err != nil {
		t.Fatalf("build failed with the patterns restored: %v", err)
	}
}

// TestBuildDICOMDIRBytesTree builds a two-patient hierarchy — one patient
// with two series (one two images deep, so a series has more than one child
// and a study has more than one series), the other a single image — and
// walks the result purely by the offsets a generic DICOM reader would see,
// verifying the reconstructed tree matches what was fed in. This is the
// two-pass byte-patching's real risk surface, so a structural round trip
// matters more than a "no error" check.
func TestBuildDICOMDIRBytesTree(t *testing.T) {
	sources := []dicomdirSource{
		{
			rel: "STUDY1/SERIES1/img1.dcm", patientID: "PAT1", patientName: "ONE^PATIENT",
			studyUID: "1.2.1", studyDate: "20240101", studyID: "S1",
			seriesUID: "1.2.1.1", modality: "CT", seriesNumber: "1",
			sopClass: "1.2.840.10008.5.1.4.1.1.7", sopInstance: "1.2.1.1.1",
			transferSyntax: tsExplicitVRLE, instanceNum: "1",
		},
		{
			rel: "STUDY1/SERIES1/img2.dcm", patientID: "PAT1", patientName: "ONE^PATIENT",
			studyUID: "1.2.1", studyDate: "20240101", studyID: "S1",
			seriesUID: "1.2.1.1", modality: "CT", seriesNumber: "1",
			sopClass: "1.2.840.10008.5.1.4.1.1.7", sopInstance: "1.2.1.1.2",
			transferSyntax: tsExplicitVRLE, instanceNum: "2",
		},
		{
			rel: "STUDY1/SERIES2/img1.dcm", patientID: "PAT1", patientName: "ONE^PATIENT",
			studyUID: "1.2.1", studyDate: "20240101", studyID: "S1",
			seriesUID: "1.2.1.2", modality: "CT", seriesNumber: "2",
			sopClass: "1.2.840.10008.5.1.4.1.1.7", sopInstance: "1.2.1.2.1",
			transferSyntax: tsExplicitVRLE, instanceNum: "1",
		},
		{
			rel: "STUDY1/SERIES1/img1.dcm", patientID: "PAT2", patientName: "TWO^PATIENT",
			studyUID: "2.2.1", studyDate: "20240202", studyID: "S2",
			seriesUID: "2.2.1.1", modality: "MR", seriesNumber: "1",
			sopClass: "1.2.840.10008.5.1.4.1.1.4", sopInstance: "2.2.1.1.1",
			transferSyntax: tsExplicitVRLE, instanceNum: "1",
		},
	}

	data, err := buildDICOMDIRBytes(sources)
	if err != nil {
		t.Fatalf("buildDICOMDIRBytes: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("buildDICOMDIRBytes returned no bytes for a non-empty source list")
	}

	records, firstIdx, lastIdx := parseDICOMDIR(t, data)

	// 2 patients, 2 studies (one per patient), 3 series (two under PAT1's
	// study, one under PAT2's), 4 images.
	if wantTotal := 2 + 2 + 3 + 4; len(records) != wantTotal {
		t.Fatalf("record count = %d, want %d", len(records), wantTotal)
	}

	patientOrder := walkSiblings(records, firstIdx)
	if len(patientOrder) != 2 {
		t.Fatalf("patient chain length = %d, want 2", len(patientOrder))
	}
	if lastIdx != patientOrder[len(patientOrder)-1] {
		t.Errorf("root last-record index = %d, want the second patient (%d)", lastIdx, patientOrder[len(patientOrder)-1])
	}
	for _, pi := range patientOrder {
		if records[pi].recordType != "PATIENT" {
			t.Errorf("record %d type = %q, want PATIENT", pi, records[pi].recordType)
		}
	}
	if got := records[patientOrder[0]].fields[tag.PatientID][0]; got != "PAT1" {
		t.Errorf("first patient ID = %q, want PAT1", got)
	}
	if got := records[patientOrder[1]].fields[tag.PatientID][0]; got != "PAT2" {
		t.Errorf("second patient ID = %q, want PAT2", got)
	}

	// PAT1 → one study → two series → (2 images, 1 image).
	pat1Study := records[patientOrder[0]].child
	if records[pat1Study].recordType != "STUDY" || records[pat1Study].fields[tag.StudyInstanceUID][0] != "1.2.1" {
		t.Fatalf("PAT1's child = %+v, want STUDY 1.2.1", records[pat1Study])
	}
	seriesOrder := walkSiblings(records, records[pat1Study].child)
	if len(seriesOrder) != 2 {
		t.Fatalf("PAT1 study series count = %d, want 2", len(seriesOrder))
	}
	imageCounts := make([]int, 0, 2)
	for _, si := range seriesOrder {
		if records[si].recordType != "SERIES" {
			t.Errorf("record %d type = %q, want SERIES", si, records[si].recordType)
		}
		imageCounts = append(imageCounts, len(walkSiblings(records, records[si].child)))
	}
	if imageCounts[0] != 2 || imageCounts[1] != 1 {
		t.Errorf("PAT1 series image counts = %v, want [2 1]", imageCounts)
	}

	// The two images under the first series carry distinct ReferencedFileID /
	// SOP Instance UID — confirms sibling image records were not conflated.
	firstSeriesImages := walkSiblings(records, records[seriesOrder[0]].child)
	gotSOPs := map[string]bool{}
	for _, ii := range firstSeriesImages {
		if records[ii].recordType != "IMAGE" {
			t.Errorf("record %d type = %q, want IMAGE", ii, records[ii].recordType)
		}
		refID := records[ii].fields[tag.ReferencedFileID]
		if len(refID) != 3 || refID[0] != "STUDY1" || refID[1] != "SERIES1" {
			t.Errorf("ReferencedFileID = %v, want [STUDY1 SERIES1 <file>]", refID)
		}
		gotSOPs[records[ii].fields[tag.ReferencedSOPInstanceUIDInFile][0]] = true
	}
	if !gotSOPs["1.2.1.1.1"] || !gotSOPs["1.2.1.1.2"] {
		t.Errorf("series 1 image SOP instances = %v, want 1.2.1.1.1 and 1.2.1.1.2", gotSOPs)
	}

	// PAT2 → one study → one series → one image, entirely independent of PAT1's subtree.
	pat2Study := records[patientOrder[1]].child
	pat2Series := records[pat2Study].child
	pat2Images := walkSiblings(records, records[pat2Series].child)
	if len(pat2Images) != 1 {
		t.Fatalf("PAT2 image count = %d, want 1", len(pat2Images))
	}
	if got := records[pat2Images[0]].fields[tag.ReferencedTransferSyntaxUIDInFile][0]; got != tsExplicitVRLE {
		t.Errorf("PAT2 image transfer syntax = %q, want %q", got, tsExplicitVRLE)
	}
}
