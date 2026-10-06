package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	netdicom "github.com/algm/go-netdicom"
	"github.com/algm/go-netdicom/dimse"
	dicom "github.com/grailbio/go-dicom"
	"github.com/grailbio/go-dicom/dicomtag"
)

// fakePACS is a loopback Q/R server for the parallel-retrieve tests: it
// serves C-MOVE, C-GET and C-ECHO from a fixed set of generated studies, and
// can be capped at maxAssoc concurrent associations, refusing any beyond that
// with A-ASSOCIATE-RJ "local limit exceeded" — what a real archive's per-AE
// limit looks like on the wire. Test-only use of grailbio: the vendored
// provider's C-MOVE/C-GET results carry its data sets, as srtransfer_repro_test
// already relies on.
type fakePACS struct {
	ln       net.Listener
	maxAssoc int32 // 0 = unlimited
	delay    time.Duration

	// active counts open connections (what maxAssoc limits); serving and
	// peak count C-MOVE/C-GET operations in progress — the parallelism the
	// tests assert. A released association's connection lingers a moment on
	// this side after the client has finished with it, so connections are the
	// wrong thing to measure parallel transfers by.
	active, serving, peak atomic.Int32
	refused               atomic.Int32
	studies               map[string][]string // StudyInstanceUID -> file paths
}

func (f *fakePACS) addr() (string, int) {
	a := f.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

// newFakePACS generates nStudies studies of perStudy files each and starts
// serving them. destAEs maps C-MOVE destination AE titles to host:port.
func newFakePACS(t *testing.T, nStudies, perStudy int, maxAssoc int32, destAEs map[string]string) *fakePACS {
	t.Helper()
	srcDir := t.TempDir()
	f := &fakePACS{maxAssoc: maxAssoc, delay: 150 * time.Millisecond, studies: map[string][]string{}}
	for s := range nStudies {
		uid := fmt.Sprintf("1.2.826.0.1.3680043.99.%d", s+1)
		for i := range perStudy {
			p := filepath.Join(srcDir, fmt.Sprintf("%d%03d.dcm", s+1, i+1))
			writeSizedTestDICOM(t, p, "CT", 8, 8, uid+".1", 1, i+1)
			f.studies[uid] = append(f.studies[uid], p)
		}
	}

	serve := func(filter []*dicom.Element, ch chan netdicom.CMoveResult) {
		defer close(ch)
		n := f.serving.Add(1)
		defer f.serving.Add(-1)
		for {
			p := f.peak.Load()
			if n <= p || f.peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(f.delay) // hold the association long enough to overlap
		var paths []string
		if e, err := dicom.FindElementByTag(filter, dicomtag.StudyInstanceUID); err == nil {
			if uid, err := e.GetString(); err == nil {
				paths = f.studies[uid]
			}
		}
		for i, p := range paths {
			ds, err := dicom.ReadDataSetFromFile(p, dicom.ReadOptions{})
			ch <- netdicom.CMoveResult{Remaining: len(paths) - i - 1, Path: p, DataSet: ds, Err: err}
		}
	}
	params := netdicom.ServiceProviderParams{
		AETitle:   "FAKEPACS",
		RemoteAEs: destAEs,
		CEcho:     func(netdicom.ConnectionState) dimse.Status { return dimse.Success },
		CMove: func(_ netdicom.ConnectionState, _, _ string, filter []*dicom.Element, ch chan netdicom.CMoveResult) {
			serve(filter, ch)
		},
		CGet: func(_ netdicom.ConnectionState, _, _ string, filter []*dicom.Element, ch chan netdicom.CMoveResult) {
			serve(filter, ch)
		},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if f.maxAssoc > 0 && f.active.Load() >= f.maxAssoc {
				// Over the limit: give a connection the client has already
				// released a moment to finish closing on this side before
				// counting it, as a server tracking releases would.
				deadline := time.Now().Add(100 * time.Millisecond)
				for f.active.Load() >= f.maxAssoc && time.Now().Before(deadline) {
					time.Sleep(2 * time.Millisecond)
				}
				if f.active.Load() >= f.maxAssoc {
					f.refused.Add(1)
					go refuseAssociation(conn)
					continue
				}
			}
			f.active.Add(1)
			go func() {
				defer f.active.Add(-1)
				netdicom.RunProviderForConn(ctx, conn, params)
			}()
		}
	}()
	return f
}

// refuseAssociation reads an A-ASSOCIATE-RQ and answers A-ASSOCIATE-RJ:
// transient, from the presentation service provider, local limit exceeded.
func refuseAssociation(conn net.Conn) {
	defer conn.Close()
	var hdr [6]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return
	}
	if _, err := io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint32(hdr[2:]))); err != nil {
		return
	}
	conn.Write([]byte{0x03, 0x00, 0, 0, 0, 4, 0x00, 2, 3, 2})
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	io.Copy(io.Discard, conn)
}

func (f *fakePACS) studyUIDs() []string {
	var uids []string
	for s := 1; s <= len(f.studies); s++ {
		uids = append(uids, fmt.Sprintf("1.2.826.0.1.3680043.99.%d", s))
	}
	return uids
}

// startTestSCP starts the application's own C-STORE SCP and returns a function
// reporting how many distinct files it has received.
func startTestSCP(t *testing.T, ae string, port int) func() int {
	t.Helper()
	scp := NewStorageSCP(ae, port, t.TempDir())
	var mu sync.Mutex
	received := map[string]bool{}
	scp.SetOnFileReceived(func(p string) { mu.Lock(); received[filepath.Base(p)] = true; mu.Unlock() })
	if err := scp.Start(); err != nil {
		t.Fatalf("scp start: %v", err)
	}
	t.Cleanup(func() { scp.Stop() })
	return func() int { mu.Lock(); defer mu.Unlock(); return len(received) }
}

func waitForCount(count func() int, want int) int {
	deadline := time.Now().Add(10 * time.Second)
	for count() < want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return count()
}

// runParallelRetrieve drives the scheduler with the real DicomClient, as the
// retrieve loop does: one association per study, up to limit at once.
func runParallelRetrieve(t *testing.T, cl *DicomClient, uids []string, limit int, method string,
	onStore func(tsUID, scUID, siUID string, data []byte) error) *retrieveRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sched := newRetrieveRun(len(uids), limit)
	sched.run(ctx, func(tctx context.Context, i int) error {
		if method == "GET" {
			return cl.Get(tctx, "STUDY", "", uids[i], "", onStore, nil)
		}
		return cl.Move(tctx, "STUDY", "", uids[i], "", "TESTSCU", nil)
	})
	if ctx.Err() != nil {
		t.Fatal("retrieve timed out")
	}
	return sched
}

func assertAllDone(t *testing.T, sched *retrieveRun) {
	t.Helper()
	for i, r := range sched.outcome() {
		if r.state != targetDone {
			t.Errorf("target %d: state %v, err %v", i, r.state, r.err)
		}
	}
}

func TestParallelMoveAgainstFakePACS(t *testing.T) {
	const scpPort = 11197
	received := startTestSCP(t, "TESTSCU", scpPort)
	pacs := newFakePACS(t, 6, 3, 0, map[string]string{"TESTSCU": fmt.Sprintf("127.0.0.1:%d", scpPort)})
	host, port := pacs.addr()
	cl := NewDicomClient(ServerProfile{Host: host, Port: port, RemoteAETitle: "FAKEPACS"}, "TESTSCU")

	sched := runParallelRetrieve(t, cl, pacs.studyUIDs(), 3, "MOVE", nil)
	assertAllDone(t, sched)
	if got := pacs.peak.Load(); got != 3 {
		t.Errorf("peak concurrent associations %d, want 3", got)
	}
	if err := sched.degradedBy(); err != nil {
		t.Errorf("run degraded against an unlimited server: %v", err)
	}
	if n := waitForCount(received, 18); n != 18 {
		t.Errorf("SCP received %d files, want 18", n)
	}
}

func TestParallelGetAgainstFakePACS(t *testing.T) {
	pacs := newFakePACS(t, 6, 3, 0, nil)
	host, port := pacs.addr()
	cl := NewDicomClient(ServerProfile{Host: host, Port: port, RemoteAETitle: "FAKEPACS"}, "TESTSCU")

	var mu sync.Mutex
	got := map[string]bool{}
	sched := runParallelRetrieve(t, cl, pacs.studyUIDs(), 3, "GET",
		func(_, _, siUID string, data []byte) error {
			mu.Lock()
			got[siUID] = true
			mu.Unlock()
			return nil
		})
	assertAllDone(t, sched)
	if p := pacs.peak.Load(); p != 3 {
		t.Errorf("peak concurrent associations %d, want 3", p)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 18 {
		t.Errorf("received %d instances over C-GET, want 18", len(got))
	}
}

// A server allowing one association refuses the extra ones; the run drops to
// one at a time, re-runs the refused studies, and still delivers everything.
func TestParallelMoveDegradesAgainstLimitedPACS(t *testing.T) {
	const scpPort = 11198
	received := startTestSCP(t, "TESTSCU", scpPort)
	pacs := newFakePACS(t, 5, 2, 1, map[string]string{"TESTSCU": fmt.Sprintf("127.0.0.1:%d", scpPort)})
	host, port := pacs.addr()
	cl := NewDicomClient(ServerProfile{Host: host, Port: port, RemoteAETitle: "FAKEPACS"}, "TESTSCU")

	sched := runParallelRetrieve(t, cl, pacs.studyUIDs(), 3, "MOVE", nil)
	assertAllDone(t, sched)
	if pacs.refused.Load() == 0 {
		t.Fatal("the limited server refused nothing; the test did not exercise the fallback")
	}
	var rj *netdicom.AssociationRejectedError
	if err := sched.degradedBy(); !errors.As(err, &rj) || !rj.ResourceLimited() {
		t.Errorf("degradedBy = %v, want the server's local-limit refusal", err)
	}
	if clause := describeParallelDegrade(sched.degradedBy()); clause == "" {
		t.Error("no summary clause for a degraded run")
	}
	if n := waitForCount(received, 10); n != 10 {
		t.Errorf("SCP received %d files, want 10", n)
	}
}

func TestEchoConcurrentAgainstFakePACS(t *testing.T) {
	open := newFakePACS(t, 0, 0, 0, nil)
	host, port := open.addr()
	errs := NewDicomClient(ServerProfile{Host: host, Port: port, RemoteAETitle: "FAKEPACS"}, "TESTSCU").
		EchoConcurrent(context.Background(), 4)
	for i, err := range errs {
		if err != nil {
			t.Errorf("unlimited server, echo %d: %v", i+1, err)
		}
	}

	limited := newFakePACS(t, 0, 0, 2, nil)
	host, port = limited.addr()
	errs = NewDicomClient(ServerProfile{Host: host, Port: port, RemoteAETitle: "FAKEPACS"}, "TESTSCU").
		EchoConcurrent(context.Background(), 4)
	var ok, refused int
	for _, err := range errs {
		var rj *netdicom.AssociationRejectedError
		switch {
		case err == nil:
			ok++
		case errors.As(err, &rj):
			refused++
		default:
			t.Errorf("limited server: unexpected error %v", err)
		}
	}
	if ok != 2 || refused != 2 {
		t.Errorf("limited to 2: %d accepted, %d refused; want 2 and 2 (associations must be held open together)", ok, refused)
	}
	if text := describeConcurrentEcho(errs); text == "" {
		t.Error("empty Test report")
	}
}
