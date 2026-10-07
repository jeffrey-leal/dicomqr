package main

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	netdicom "github.com/algm/go-netdicom"
	"github.com/algm/go-netdicom/dimse"
	dicom "github.com/grailbio/go-dicom"
	"github.com/grailbio/go-dicom/dicomtag"
)

// silentPeer accepts connections and reads everything sent, but never
// answers — not even the association request: a server that has hung.
func silentPeer(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(io.Discard, conn); conn.Close() }()
		}
	}()
	return "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
}

func withQueryIdleTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := queryIdleTimeout
	queryIdleTimeout = d
	t.Cleanup(func() { queryIdleTimeout = old })
}

// drain collects a query's in-band errors, failing if the channel does not
// close in time — the hang this whole mechanism exists to prevent.
func drainFind(t *testing.T, ch <-chan FindResult) (results []FindResult, errs []error) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return results, errs
			}
			if r.Err != nil {
				errs = append(errs, r.Err)
			} else {
				results = append(results, r)
			}
		case <-deadline:
			t.Fatal("query channel still open after 10 s — the query hung")
		}
	}
}

func TestFindTimesOutOnSilentServer(t *testing.T) {
	withQueryIdleTimeout(t, 300*time.Millisecond)
	host, port := silentPeer(t)
	cl := NewDicomClient(ServerProfile{Host: host, Port: port, RemoteAETitle: "SILENT"}, "TESTSCU")
	ch, err := cl.Find(context.Background(), "STUDY", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	_, errs := drainFind(t, ch)
	var nr errQueryNoResponse
	if len(errs) != 1 || !errors.As(errs[0], &nr) {
		t.Fatalf("errors %v, want one 'stopped responding' error", errs)
	}
}

func TestFindWorklistTimesOutOnSilentServer(t *testing.T) {
	withQueryIdleTimeout(t, 300*time.Millisecond)
	host, port := silentPeer(t)
	cl := NewDicomClient(ServerProfile{Host: host, Port: port, RemoteAETitle: "SILENT"}, "TESTSCU")
	ch, err := cl.FindWorklist(context.Background(), map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	var errs []error
	for done := false; !done; {
		select {
		case r, ok := <-ch:
			if !ok {
				done = true
			} else if r.Err != nil {
				errs = append(errs, r.Err)
			}
		case <-deadline:
			t.Fatal("worklist query still open after 10 s — it hung, which left Query disabled for good")
		}
	}
	var nr errQueryNoResponse
	if len(errs) != 1 || !errors.As(errs[0], &nr) {
		t.Fatalf("errors %v, want one 'stopped responding' error", errs)
	}
}

// Cancelling must end the query at once, not only stop forwarding results:
// the association is aborted, so nothing is left blocked behind it.
func TestFindCancelEndsSilentQuery(t *testing.T) {
	withQueryIdleTimeout(t, time.Minute)
	host, port := silentPeer(t)
	cl := NewDicomClient(ServerProfile{Host: host, Port: port, RemoteAETitle: "SILENT"}, "TESTSCU")
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := cl.Find(ctx, "STUDY", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	drainFind(t, ch)
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("query took %v to end after cancel", el)
	}
}

func TestFindReportsRefusedConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens there now
	cl := NewDicomClient(ServerProfile{Host: "127.0.0.1", Port: port, RemoteAETitle: "GONE"}, "TESTSCU")
	ch, err := cl.Find(context.Background(), "STUDY", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	_, errs := drainFind(t, ch)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "connect to") {
		t.Fatalf("errors %v, want one connect error", errs)
	}
}

// End to end: the study query asks for the study's size, and a server that
// answers it gets those numbers through to the FindResult.
func TestFindReturnsStudyCounts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	asked := make(chan []*dicom.Element, 1)
	params := netdicom.ServiceProviderParams{
		AETitle: "COUNTS",
		CEcho:   func(netdicom.ConnectionState) dimse.Status { return dimse.Success },
		CFind: func(_ netdicom.ConnectionState, _, _ string, filter []*dicom.Element, ch chan netdicom.CFindResult) {
			asked <- filter
			ch <- netdicom.CFindResult{Elements: []*dicom.Element{
				dicom.MustNewElement(dicomtag.PatientName, "DOE^JANE"),
				dicom.MustNewElement(dicomtag.PatientID, "P1"),
				dicom.MustNewElement(dicomtag.StudyInstanceUID, "1.2.3"),
				dicom.MustNewElement(dicomtag.NumberOfStudyRelatedSeries, "3"),
				dicom.MustNewElement(dicomtag.NumberOfStudyRelatedInstances, "412 "),
			}}
			close(ch)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go netdicom.RunProviderForConn(ctx, conn, params)
		}
	}()

	cl := NewDicomClient(ServerProfile{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, RemoteAETitle: "COUNTS"}, "TESTSCU")
	ch, err := cl.Find(context.Background(), "STUDY", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	results, errs := drainFind(t, ch)
	if len(errs) != 0 || len(results) != 1 {
		t.Fatalf("results %v, errors %v", results, errs)
	}
	if r := results[0]; r.NumStudySeries != 3 || r.NumStudyInstances != 412 {
		t.Errorf("counts %d series / %d instances, want 3 / 412", r.NumStudySeries, r.NumStudyInstances)
	}
	filter := <-asked
	var hasSeries, hasInstances bool
	for _, e := range filter {
		hasSeries = hasSeries || e.Tag == dicomtag.NumberOfStudyRelatedSeries
		hasInstances = hasInstances || e.Tag == dicomtag.NumberOfStudyRelatedInstances
	}
	if !hasSeries || !hasInstances {
		t.Error("the study query does not ask for the study's series and instance counts")
	}
}

func TestSetStudyCountsLabel(t *testing.T) {
	m := newResultsModel()
	for _, uid := range []string{"A", "B", "C", "D"} {
		m.addStudy("DOE^JANE", "P1", uid, "20260101", "CT HEAD", "", "CT")
	}
	m.setStudyCounts("A", 3, 412)
	m.setStudyCounts("B", 1, 1)
	m.setStudyCounts("C", 0, 0)
	m.setStudyCounts("D", 0, 57)
	m.setStudyCounts("A", 3, 412) // a duplicate response must not add it twice
	want := map[string]string{
		"A": "(3 series, 412 images)",
		"B": "(1 series, 1 image)",
		"D": "(57 images)",
	}
	for uid, suffix := range want {
		if l := m.nodes["S:"+uid].label; !strings.HasSuffix(l, suffix) || strings.Count(l, "(") != 1 {
			t.Errorf("study %s label %q, want one %q", uid, l, suffix)
		}
	}
	if l := m.nodes["S:C"].label; strings.Contains(l, "(") {
		t.Errorf("unreported counts changed the label: %q", l)
	}
	m.setStudyCounts("missing", 1, 1) // ignored
}
