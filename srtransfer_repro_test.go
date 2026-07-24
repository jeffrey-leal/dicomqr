package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	netdicom "github.com/algm/go-netdicom"
	"github.com/algm/go-netdicom/dimse"
	"github.com/algm/go-netdicom/sopclass"
	dicom "github.com/grailbio/go-dicom"
)

// storeSRViaLoopback plays the PACS's role in a C-MOVE sub-operation: it
// C-STOREs the given file to a live StorageSCP over a real association.
// Returns the elapsed error from CStore, or a timeout error if any stage
// hangs.
func storeSRViaLoopback(t *testing.T, port int, requiredTS string, srcPath string) error {
	t.Helper()
	dir := t.TempDir()
	scp := NewStorageSCP("TESTSCP", port, dir)
	scp.SetTransferPolicy(requiredTS)
	if err := scp.Start(); err != nil {
		t.Fatalf("scp start: %v", err)
	}
	defer scp.Stop()

	type outcome struct{ err error }
	done := make(chan outcome, 1)
	go func() {
		su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
			CalledAETitle:  "TESTSCP",
			CallingAETitle: "TESTSCU",
			SOPClasses:     sopclass.StorageClasses,
		})
		if err != nil {
			done <- outcome{err}
			return
		}
		defer su.Release()
		su.Connect(scp.ListenAddr())

		ds, err := dicom.ReadDataSetFromFile(srcPath, dicom.ReadOptions{})
		if err != nil {
			done <- outcome{err}
			return
		}
		done <- outcome{su.CStore(ds)}
	}()

	select {
	case o := <-done:
		return o.err
	case <-time.After(15 * time.Second):
		t.Fatal("HANG: C-STORE of SR did not complete within 15s")
		return nil
	}
}

// storeOnceToSCP plays the PACS role for a single C-STORE against an
// already-running SCP at addr. Blocks until the CStore RSP is received, by
// which point the SCP's onFileReceived callback has already run.
func storeOnceToSCP(t *testing.T, addr, calledAE, srcPath string) error {
	t.Helper()
	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
		CalledAETitle:  calledAE,
		CallingAETitle: "TESTSCU",
		SOPClasses:     sopclass.StorageClasses,
	})
	if err != nil {
		return err
	}
	defer su.Release()
	su.Connect(addr)
	ds, err := dicom.ReadDataSetFromFile(srcPath, dicom.ReadOptions{})
	if err != nil {
		return err
	}
	return su.CStore(ds)
}

// A C-STORE whose destination already exists on disk in the required (or any,
// when unrestricted) transfer syntax is skipped silently: the onFileReceived
// callback fires only for newly written files and the skip must not duplicate
// the file on disk. Strict negotiation makes a re-received file byte-compatible
// with the existing copy, so there is nothing to replace or report.
func TestCStoreSkipIsSilent(t *testing.T) {
	cases := []struct {
		name       string
		port       int
		requiredTS string
	}{
		{"strict explicit", 11181, tsExplicitVRLE},
		{"unrestricted", 11182, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			scp := NewStorageSCP("TESTSCP", tc.port, dir)
			scp.SetTransferPolicy(tc.requiredTS)

			var mu sync.Mutex
			var received []string
			scp.SetOnFileReceived(func(p string) {
				mu.Lock()
				received = append(received, p)
				mu.Unlock()
			})
			if err := scp.Start(); err != nil {
				t.Fatalf("scp start: %v", err)
			}
			defer scp.Stop()

			// First store writes the file; second store hits the skip branch.
			for i := 0; i < 2; i++ {
				if err := storeOnceToSCP(t, scp.ListenAddr(), "TESTSCP", srTestFile); err != nil {
					t.Fatalf("store %d: %v", i+1, err)
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if len(received) != 1 {
				t.Fatalf("callback fired %d times (%v), want 1 (silent skip)", len(received), received)
			}
			if got := countDCM(t, dir); got != 1 {
				t.Errorf("on-disk file count = %d, want 1 (the skip must not duplicate)", got)
			}
		})
	}
}

func countDCM(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(path) == ".dcm" {
			n++
		}
		return nil
	})
	return n
}

const srTestFile = "thirdparty/go-netdicom/testdata/reportsi.dcm"

// Unrestricted policy — the default "as stored" path.
func TestSRReceiveUnrestricted(t *testing.T) {
	if err := storeSRViaLoopback(t, 11177, "", srTestFile); err != nil {
		t.Fatalf("C-STORE failed: %v", err)
	}
}

// Strict single-syntax policy — the sending SCU must deliver Explicit VR LE
// because that is the only accepted presentation-context transfer syntax.
func TestSRReceiveStrictExplicit(t *testing.T) {
	if err := storeSRViaLoopback(t, 11178, tsExplicitVRLE, srTestFile); err != nil {
		t.Fatalf("C-STORE failed: %v", err)
	}
}

// A file arriving in a non-required (but accepted) transfer syntax must be
// converted on receipt: the sender is limited to proposing Explicit VR LE
// while the SCP requires Implicit, so the file crosses the wire as Explicit
// and must land on disk as Implicit, with the conversion counted.
func TestCStoreConvertsToRequiredSyntax(t *testing.T) {
	dir := t.TempDir()
	scp := NewStorageSCP("TESTSCP", 11183, dir)
	scp.SetTransferPolicy(tsImplicitVRLE)

	var mu sync.Mutex
	var received []string
	scp.SetOnFileReceived(func(p string) {
		mu.Lock()
		received = append(received, p)
		mu.Unlock()
	})
	if err := scp.Start(); err != nil {
		t.Fatalf("scp start: %v", err)
	}
	defer scp.Stop()

	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
		CalledAETitle:    "TESTSCP",
		CallingAETitle:   "TESTSCU",
		SOPClasses:       sopclass.StorageClasses,
		TransferSyntaxes: []string{tsExplicitVRLE},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer su.Release()
	su.Connect(scp.ListenAddr())
	ds, err := dicom.ReadDataSetFromFile(srTestFile, dicom.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := su.CStore(ds); err != nil {
		t.Fatalf("C-STORE failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("callback fired %d times, want 1", len(received))
	}
	if got := fileTransferSyntaxUID(received[0]); got != tsImplicitVRLE {
		t.Errorf("on-disk transfer syntax = %q, want %q (converted on receipt)", got, tsImplicitVRLE)
	}
	if got := scp.ConvertedCount(); got != 1 {
		t.Errorf("ConvertedCount = %d, want 1", got)
	}
}

// A PACS whose C-MOVE agent stalls (accepts the connection, then goes silent)
// must not hang the client forever: cancelling the context aborts the
// association and Move returns promptly instead of leaking the goroutine.
func TestMoveAbortsOnSilentServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			// Swallow everything, answer nothing — the stalled-server shape.
			go io.Copy(io.Discard, conn)
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	client := NewDicomClient(ServerProfile{
		Name: "SILENT", RemoteAETitle: "SILENT", Host: "127.0.0.1", Port: port,
	}, "TESTSCU")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	err = client.Move(ctx, "STUDY", "", "1.2.3.4", "", "TESTSCU", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Move against a silent server must fail")
	}
	// ctx deadline (2s) + abortAndReap bound (≤5s) — anything near the 15s
	// mark means the abort did not reap the blocked goroutine.
	if elapsed > 10*time.Second {
		t.Fatalf("Move took %v to give up — abort did not unstick the association", elapsed)
	}
	t.Logf("Move returned after %v with: %v", elapsed, err)
}

// An object that arrives in an accepted syntax but cannot be converted locally
// (e.g. a screenshot or vendor graphic whose pixel data the built-in decoders
// reject) must be skipped: Success returned so the PACS keeps sending the rest
// of the retrieve, nothing saved, and the skip counted for the summary — not a
// failed sub-operation, which would abort the whole retrieve.
func TestCStoreSkipsUnconvertibleObject(t *testing.T) {
	dir := t.TempDir()
	scp := NewStorageSCP("TESTSCP", 11184, dir)
	scp.SetTransferPolicy(tsExplicitVRLE)
	scp.SetOnFileReceived(func(p string) { t.Errorf("onFileReceived fired for skipped object: %s", p) })

	// Claims JPEG Baseline on the association, but the payload is garbage the
	// parser rejects — the conversion attempt must fail and the object skip.
	payload := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 64)
	st := scp.handleCStore(tsJPEGBaseline, "1.2.840.10008.5.1.4.1.1.7", "1.2.3.4.5.6.7", bytes.NewReader(payload))
	if st.Status != dimse.Success.Status {
		t.Fatalf("status = %+v, want Success (a skip must not fail the sub-operation)", st)
	}
	if got := scp.SkippedCount(); got != 1 {
		t.Errorf("SkippedCount = %d, want 1", got)
	}
	if got := scp.ConvertedCount(); got != 0 {
		t.Errorf("ConvertedCount = %d, want 0", got)
	}
	if n := countDCM(t, dir); n != 0 {
		t.Errorf("%d .dcm file(s) written, want 0", n)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".recv_*.tmp")); len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

// The C-GET save path must skip unconvertible objects the same way: no error
// (an error would fail the sub-operation and abort the retrieve), skipped
// reported true, nothing written.
func TestSaveGetFileSkipsUnconvertible(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 64)
	path, converted, skipped, err := saveGetFile(dir, tsJPEGBaseline, "1.2.840.10008.5.1.4.1.1.7", "1.2.3.4.5.6.8", payload, tsExplicitVRLE)
	if err != nil {
		t.Fatalf("saveGetFile error = %v, want nil (skip, not failure)", err)
	}
	if !skipped {
		t.Error("skipped = false, want true")
	}
	if converted || path != "" {
		t.Errorf("path = %q, converted = %v — want empty and false for a skipped object", path, converted)
	}
	if n := countDCM(t, dir); n != 0 {
		t.Errorf("%d .dcm file(s) written, want 0", n)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".recv_*.tmp")); len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}
