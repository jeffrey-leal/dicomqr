package main

// Tests for the raw-bytes push path (DICOM library policy, Phase 1): the
// hand-rolled File Meta scan that locates the dataset bytes, and the
// copy-based transcode used when the server negotiates a different syntax.

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestFileMetaIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.dcm")
	writeModifyTestDICOM(t, path)

	id, err := fileMetaIdentity(path)
	if err != nil {
		t.Fatalf("fileMetaIdentity: %v", err)
	}
	if id.sopClassUID != "1.2.840.10008.5.1.4.1.1.7" {
		t.Errorf("sopClassUID = %q", id.sopClassUID)
	}
	if id.sopInstanceUID != "1.2.3.4.5" {
		t.Errorf("sopInstanceUID = %q", id.sopInstanceUID)
	}
	if id.transferSyntaxUID != tsExplicitVRLE {
		t.Errorf("transferSyntaxUID = %q, want %q", id.transferSyntaxUID, tsExplicitVRLE)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if id.datasetOffset <= 132 || id.datasetOffset >= int64(len(raw)-2) {
		t.Fatalf("datasetOffset %d out of range (file %d bytes)", id.datasetOffset, len(raw))
	}
	// The offset must land exactly on the first dataset element: past the meta
	// group, and (for this file) on group 0008.
	group := binary.LittleEndian.Uint16(raw[id.datasetOffset:])
	if group == 0x0002 {
		t.Error("datasetOffset still inside the File Meta group")
	}
	if group != 0x0008 {
		t.Errorf("dataset starts with group %04X, want 0008", group)
	}
}

func TestFileMetaIdentityRejectsNonDICOM(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.bin")
	if err := os.WriteFile(bad, []byte("this is not a DICOM part-10 file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fileMetaIdentity(bad); err == nil {
		t.Error("expected an error for a non-DICOM file")
	}
}

func TestTranscodeDICOMFileToTempLeavesSourceUntouched(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.dcm")
	writeModifyTestDICOM(t, src)
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	tmpPath, changed, err := transcodeDICOMFileToTemp(src, tsImplicitVRLE, t.TempDir())
	if err != nil || !changed || tmpPath == "" {
		t.Fatalf("convert: tmp=%q changed=%v err=%v", tmpPath, changed, err)
	}
	defer os.Remove(tmpPath)
	if got := fileTransferSyntaxUID(tmpPath); got != tsImplicitVRLE {
		t.Errorf("converted temp transfer syntax = %q, want %q", got, tsImplicitVRLE)
	}
	after, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("source file was modified by the copy-based transcode")
	}

	// Already in the target syntax: no temp file, no change, no error.
	if p, ch, err := transcodeDICOMFileToTemp(src, tsExplicitVRLE, t.TempDir()); err != nil || ch || p != "" {
		t.Errorf("already-target: tmp=%q changed=%v err=%v, want no-op", p, ch, err)
	}
}
