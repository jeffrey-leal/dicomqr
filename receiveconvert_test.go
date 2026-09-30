package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/algm/go-netdicom/dimse"
	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

const (
	recvTestSOPClass    = "1.2.840.10008.5.1.4.1.1.7"
	recvTestSOPInstance = "1.2.3.4.5.6.77"
	recvTestPatient     = "DOE^JANE" // the fixture carries no Patient ID
)

// implicitPayload is a small image data set encoded Implicit VR Little Endian —
// the bytes a PACS sends after the File Meta group. Converting it to Explicit
// VR LE is a real conversion that needs no codec.
func implicitPayload(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.dcm")
	writeRawPixelFixture(t, path, tsImplicitVRLE)
	id, err := fileMetaIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data[id.datasetOffset:]
}

// holdAllTokens takes every CPU token of conv, so its queued jobs cannot start
// until release is called — letting a test look at the state between the
// C-STORE response and the conversion.
func holdAllTokens(conv *receiveConverter) (release func()) {
	var releases []func()
	for range cap(conv.tokens) {
		releases = append(releases, conv.tokens.hold())
	}
	return func() {
		for _, r := range releases {
			r()
		}
	}
}

func pendingFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, pendingConvertPrefix+"*.tmp"))
	return m
}

// The point of the change: an object needing conversion gets its Success
// response before the conversion runs, waits as a pending file, and lands —
// converted, counted and announced — once the converter gets to it.
func TestCStoreRepliesBeforeConverting(t *testing.T) {
	dir := t.TempDir()
	conv := newReceiveConverter()
	scp := NewStorageSCP("TESTSCP", 11190, dir)
	scp.SetTransferPolicy(tsExplicitVRLE)
	scp.SetReceiveConverter(conv)
	var mu sync.Mutex
	var announced []string
	scp.SetOnFileReceived(func(p string) { mu.Lock(); announced = append(announced, p); mu.Unlock() })

	release := holdAllTokens(conv)
	st := scp.handleCStore(tsImplicitVRLE, recvTestSOPClass, recvTestSOPInstance, bytes.NewReader(implicitPayload(t)))
	if st.Status != dimse.Success.Status {
		t.Fatalf("status = %+v, want Success", st)
	}
	if n := countDCM(t, dir); n != 0 {
		t.Fatalf("%d file(s) saved before the conversion ran", n)
	}
	if p := pendingFiles(t, dir); len(p) != 1 {
		t.Fatalf("pending files = %v, want one awaiting conversion", p)
	}

	release()
	conv.wait()
	if len(announced) != 1 {
		t.Fatalf("announced %v, want the one converted file", announced)
	}
	if got := fileTransferSyntaxUID(announced[0]); got != tsExplicitVRLE {
		t.Errorf("saved file is in %s, want the required %s", got, tsExplicitVRLE)
	}
	if scp.ConvertedCount() != 1 || scp.SkippedCount() != 0 || scp.FailedLocalCount() != 0 {
		t.Errorf("counts converted/skipped/failed = %d/%d/%d, want 1/0/0",
			scp.ConvertedCount(), scp.SkippedCount(), scp.FailedLocalCount())
	}
	if p := pendingFiles(t, dir); len(p) != 0 {
		t.Errorf("pending files left behind: %v", p)
	}
}

// An object that cannot be converted, now discovered after the response, is
// still a skip: counted, not saved, not announced.
func TestQueuedUnconvertibleObjectIsSkipped(t *testing.T) {
	dir := t.TempDir()
	conv := newReceiveConverter()
	scp := NewStorageSCP("TESTSCP", 11191, dir)
	scp.SetTransferPolicy(tsExplicitVRLE)
	scp.SetReceiveConverter(conv)
	scp.SetOnFileReceived(func(p string) { t.Errorf("announced a skipped object: %s", p) })

	payload := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 64)
	if st := scp.handleCStore(tsJPEGBaseline, recvTestSOPClass, "1.2.3.4.5.6.78", bytes.NewReader(payload)); st.Status != dimse.Success.Status {
		t.Fatalf("status = %+v, want Success", st)
	}
	conv.wait()
	if scp.SkippedCount() != 1 || scp.ConvertedCount() != 0 || countDCM(t, dir) != 0 {
		t.Errorf("skipped %d, converted %d, saved %d; want 1, 0, 0", scp.SkippedCount(), scp.ConvertedCount(), countDCM(t, dir))
	}
	if p := pendingFiles(t, dir); len(p) != 0 {
		t.Errorf("pending files left behind: %v", p)
	}
}

// A failure to save after the conversion — here the patient folder's name is
// taken by a file — cannot reach the PACS any more. It must be counted for the
// retrieve's summary instead, and nothing announced.
func TestQueuedSaveFailureIsCountedLocally(t *testing.T) {
	dir := t.TempDir()
	conv := newReceiveConverter()
	scp := NewStorageSCP("TESTSCP", 11192, dir)
	scp.SetTransferPolicy(tsExplicitVRLE)
	scp.SetReceiveConverter(conv)
	scp.SetOnFileReceived(func(p string) { t.Errorf("announced an object that was not saved: %s", p) })

	blocker := filepath.Join(dir, patientFolderName(recvTestPatient, ""))
	payload := implicitPayload(t)
	if !bytes.Contains(payload, []byte(recvTestPatient)) {
		t.Skip("fixture patient name changed; update recvTestPatient")
	}
	if err := os.WriteFile(blocker, []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := scp.handleCStore(tsImplicitVRLE, recvTestSOPClass, recvTestSOPInstance, bytes.NewReader(payload)); st.Status != dimse.Success.Status {
		t.Fatalf("status = %+v, want Success (the PACS is answered before the save)", st)
	}
	conv.wait()
	if got := scp.FailedLocalCount(); got != 1 {
		t.Errorf("FailedLocalCount = %d, want 1", got)
	}
}

// Nothing the PACS was told had arrived is lost to a quit mid-conversion: a
// pending file an earlier session left is finished at the next start, to the
// syntax its name records.
func TestRecoverPendingConversions(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, ".recv_left.tmp")
	writeRawPixelFixture(t, src, tsImplicitVRLE)
	pending, err := markPendingConversion(src, tsExplicitVRLE)
	if err != nil {
		t.Fatal(err)
	}
	if ts, ok := pendingConvertTarget(pending); !ok || ts != tsExplicitVRLE {
		t.Fatalf("pending name %s records %q, want %s", filepath.Base(pending), ts, tsExplicitVRLE)
	}
	// Junk with a pending name is discarded rather than left for ever.
	junk := filepath.Join(dir, pendingConvertPrefix+tsExplicitVRLE+"_junk.tmp")
	os.WriteFile(junk, []byte("not dicom"), 0o644)

	conv := newReceiveConverter()
	var mu sync.Mutex
	var saved []string
	found := recoverPendingConversions(dir, conv, func(d string) { mu.Lock(); saved = append(saved, d); mu.Unlock() })
	conv.wait()
	if found != 1 || len(saved) != 1 {
		t.Fatalf("found %d, saved %v; want the one real pending file", found, saved)
	}
	if got := fileTransferSyntaxUID(saved[0]); got != tsExplicitVRLE {
		t.Errorf("recovered file is in %s, want %s", got, tsExplicitVRLE)
	}
	if !strings.HasPrefix(saved[0], dir) || strings.HasPrefix(filepath.Base(saved[0]), ".") {
		t.Errorf("recovered to %s, want a file in the organized hierarchy", saved[0])
	}
	if p := pendingFiles(t, dir); len(p) != 0 {
		t.Errorf("pending files left: %v", p)
	}
	ds, err := sdicom.ParseFile(saved[0], nil, sdicom.SkipPixelData())
	if err != nil {
		t.Fatal(err)
	}
	if name := datasetString(&ds, tag.PatientName); name != recvTestPatient {
		t.Errorf("recovered file's patient = %q", name)
	}
}

// The C-GET path queues the same way and reports through done.
func TestSaveGetFileQueuesConversion(t *testing.T) {
	dir := t.TempDir()
	conv := newReceiveConverter()
	var got receiveResult
	var gotErr error
	res, queued, err := saveGetFile(dir, tsImplicitVRLE, recvTestSOPClass, recvTestSOPInstance, implicitPayload(t),
		tsExplicitVRLE, conv, func(r receiveResult, e error) { got, gotErr = r, e })
	if err != nil || !queued || res.dest != "" {
		t.Fatalf("saveGetFile = %+v, queued %v, err %v; want queued", res, queued, err)
	}
	conv.wait()
	if gotErr != nil || got.outcome != receiveSaved || !got.converted {
		t.Fatalf("done got %+v, %v; want a converted save", got, gotErr)
	}
	if ts := fileTransferSyntaxUID(got.dest); ts != tsExplicitVRLE {
		t.Errorf("saved in %s, want %s", ts, tsExplicitVRLE)
	}
}
