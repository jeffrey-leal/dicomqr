package main

// Parallel push (pushrun.go) end to end against a loopback C-STORE provider
// that can be capped at a number of associations, slowed down, made to refuse
// chosen objects, or made to go silent — the server behaviours the push has to
// survive without losing or duplicating a file.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	netdicom "github.com/algm/go-netdicom"
	"github.com/algm/go-netdicom/dimse"
)

// fakeStoreSCP is the provider. maxAssoc > 0 refuses associations beyond it
// with A-ASSOCIATE-RJ (refuseAssociation, fakepacs_test.go); fail answers the
// listed SOP instances with a failure status; block, when set, holds every
// C-STORE until it is closed.
type fakeStoreSCP struct {
	ln       net.Listener
	maxAssoc int32
	delay    time.Duration
	fail     map[string]bool
	block    chan struct{}
	accept   []string // transfer syntaxes it accepts; nil takes the first offered
	onStore  func()   // called during every C-STORE, before its delay

	active, storing, peak, refused atomic.Int32

	mu     sync.Mutex
	got    map[string]int // SOP Instance UID -> times stored
	syntax map[string]int // transfer syntax -> data sets received in it
}

func startFakeStoreSCP(t *testing.T, f *fakeStoreSCP) *fakeStoreSCP {
	t.Helper()
	f.got = map[string]int{}
	f.syntax = map[string]int{}
	params := netdicom.ServiceProviderParams{
		AETitle:                  "STORESCP",
		AcceptedTransferSyntaxes: f.accept,
		CStore: func(_ context.Context, _ netdicom.ConnectionState, ts, _, sopInstance string,
			data io.Reader, _ int64) dimse.Status {
			_, _ = io.Copy(io.Discard, data)
			n := f.storing.Add(1)
			defer f.storing.Add(-1)
			for p := f.peak.Load(); n > p && !f.peak.CompareAndSwap(p, n); p = f.peak.Load() {
			}
			if f.block != nil {
				<-f.block
			}
			if f.onStore != nil {
				f.onStore()
			}
			time.Sleep(f.delay)
			if f.fail[sopInstance] {
				return dimse.Status{Status: dimse.CStoreOutOfResources}
			}
			f.mu.Lock()
			f.got[sopInstance]++
			f.syntax[ts]++
			f.mu.Unlock()
			return dimse.Success
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		if f.block != nil {
			select {
			case <-f.block:
			default:
				close(f.block)
			}
		}
		cancel()
		ln.Close()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if f.maxAssoc > 0 && f.active.Load() >= f.maxAssoc {
				// As fakePACS: give a just-released connection a moment to
				// finish closing on this side before counting it.
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

func (f *fakeStoreSCP) client(parallel int) *DicomClient {
	port := f.ln.Addr().(*net.TCPAddr).Port
	return NewDicomClient(ServerProfile{Name: "fake", Host: "127.0.0.1", Port: port,
		RemoteAETitle: "STORESCP", ParallelTransfers: parallel}, "TESTSCU")
}

func (f *fakeStoreSCP) stored() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.got))
	for k, v := range f.got {
		out[k] = v
	}
	return out
}

// pushStudy writes a study of the given series sizes, one folder per series
// as the download folder lays them out, and returns the paths in tree order
// with each file's SOP Instance UID.
func pushStudy(t *testing.T, series ...int) (paths []string, uids map[string]string) {
	t.Helper()
	root := t.TempDir()
	uids = map[string]string{}
	// (0008,0060) Modality CS "CT", Explicit VR Little Endian: the provider
	// reads the bytes and does not parse them.
	dataset := []byte{0x08, 0x00, 0x60, 0x00, 'C', 'S', 0x02, 0x00, 'C', 'T'}
	for s, n := range series {
		dir := filepath.Join(root, fmt.Sprintf("series%02d", s+1))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := range n {
			p := filepath.Join(dir, fmt.Sprintf("img%03d.dcm", i+1))
			uid := fmt.Sprintf("1.2.826.0.1.3680043.77.%d.%d", s+1, i+1)
			writeRawPart10(t, p, testCTClass, uid, tsExplicitVRLE, dataset)
			paths = append(paths, p)
			uids[p] = uid
		}
	}
	return paths, uids
}

// collectPush runs StoreFiles and returns its reports, which it checks arrive
// numbered 1..n, one per file.
func collectPush(t *testing.T, cl *DicomClient, ctx context.Context, paths []string,
	idle time.Duration) ([]StoreProgress, pushReport, error) {
	t.Helper()
	var mu sync.Mutex
	var reports []StoreProgress
	rep, err := cl.StoreFiles(ctx, paths, idle, func(p StoreProgress) {
		mu.Lock()
		reports = append(reports, p)
		mu.Unlock()
	})
	seen := map[string]bool{}
	for k, p := range reports {
		if p.Done != k+1 || p.Total != len(paths) {
			t.Errorf("report %d: done %d of %d, want %d of %d", k, p.Done, p.Total, k+1, len(paths))
		}
		if seen[p.Path] {
			t.Errorf("%s reported twice", filepath.Base(p.Path))
		}
		seen[p.Path] = true
	}
	return reports, rep, err
}

func TestPushTargets(t *testing.T) {
	paths := func(series ...int) []string {
		var out []string
		for s, n := range series {
			for i := range n {
				out = append(out, filepath.Join("study", fmt.Sprintf("s%d", s), fmt.Sprintf("%d.dcm", i)))
			}
		}
		return out
	}
	check := func(name string, p []string, limit int, wantSizes []int) {
		t.Helper()
		targets := pushTargets(p, limit)
		var flat []int
		var sizes []int
		for _, tg := range targets {
			flat = append(flat, tg...)
			sizes = append(sizes, len(tg))
		}
		want := make([]int, len(p))
		for i := range want {
			want[i] = i
		}
		if !slices.Equal(flat, want) {
			t.Errorf("%s: targets %v do not cover every file once, in order", name, targets)
		}
		if !slices.Equal(sizes, wantSizes) {
			t.Errorf("%s: target sizes %v, want %v", name, sizes, wantSizes)
		}
	}
	// One association: the whole push on it, as before parallel push.
	check("one association", paths(30, 6, 4), 1, []int{40})
	// Size max(8, ceil(100/6)) = 17: the big series is split, a series that
	// would overflow the open target starts its own, small ones stay whole.
	check("large series split", paths(80, 12, 8), 3, []int{17, 17, 17, 17, 12, 12, 8})
	// Size 8: two-file series are packed four to a target, never split.
	check("small series packed", paths(2, 2, 2, 2, 2, 2, 2, 2, 2, 2), 2, []int{8, 8, 4})
	// Below the minimum target size a push is not split at all.
	check("small push", paths(5), 4, []int{5})
	if got := pushTargets(nil, 3); got != nil {
		t.Errorf("no files: %v", got)
	}
}

// Every file arrives exactly once and the associations really overlap.
func TestParallelPushStoresEveryFileOnce(t *testing.T) {
	scp := startFakeStoreSCP(t, &fakeStoreSCP{delay: 20 * time.Millisecond})
	paths, uids := pushStudy(t, 30, 6, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	reports, rep, err := collectPush(t, scp.client(3), ctx, paths, 5*time.Second)
	if err != nil {
		t.Fatalf("StoreFiles: %v", err)
	}
	if rep.limit != 3 || rep.degradedBy != nil {
		t.Errorf("report: limit %d degraded %v, want 3 and none", rep.limit, rep.degradedBy)
	}
	if len(reports) != len(paths) {
		t.Fatalf("%d reports for %d files", len(reports), len(paths))
	}
	for _, p := range reports {
		if p.Err != nil {
			t.Errorf("%s: %v", filepath.Base(p.Path), p.Err)
		}
	}
	stored := scp.stored()
	for _, p := range paths {
		if n := stored[uids[p]]; n != 1 {
			t.Errorf("%s stored %d times", filepath.Base(p), n)
		}
	}
	if peak := scp.peak.Load(); peak < 2 {
		t.Errorf("at most %d store at a time — the associations did not overlap", peak)
	}
}

// A server that takes one association refuses the others: the push drops to
// one at a time, re-runs the refused parts, and still stores every file once.
func TestParallelPushDegradesAgainstOneAssociationServer(t *testing.T) {
	// Slow enough that a part outlasts the server's 100 ms grace for a
	// closing connection, so the extra associations really are refused.
	scp := startFakeStoreSCP(t, &fakeStoreSCP{maxAssoc: 1, delay: 30 * time.Millisecond})
	paths, uids := pushStudy(t, 30, 6, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	reports, rep, err := collectPush(t, scp.client(3), ctx, paths, 5*time.Second)
	if err != nil {
		t.Fatalf("StoreFiles: %v", err)
	}
	var rj *netdicom.AssociationRejectedError
	if !errors.As(rep.degradedBy, &rj) {
		t.Errorf("degraded by %v, want the server's refusal", rep.degradedBy)
	}
	if scp.refused.Load() == 0 {
		t.Error("the server refused nothing — the test did not exercise the fallback")
	}
	for _, p := range reports {
		if p.Err != nil {
			t.Errorf("%s: %v", filepath.Base(p.Path), p.Err)
		}
	}
	stored := scp.stored()
	for _, p := range paths {
		if n := stored[uids[p]]; n != 1 {
			t.Errorf("%s stored %d times", filepath.Base(p), n)
		}
	}
}

// A server refusing some objects is those files' failure — never a reason to
// drop parallel associations, and the rest still go.
func TestParallelPushFileRefusalIsNotAnAssociationFailure(t *testing.T) {
	paths, uids := pushStudy(t, 30, 6, 4)
	fail := map[string]bool{uids[paths[3]]: true, uids[paths[20]]: true, uids[paths[35]]: true}
	scp := startFakeStoreSCP(t, &fakeStoreSCP{fail: fail})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	reports, rep, err := collectPush(t, scp.client(3), ctx, paths, 5*time.Second)
	if err != nil {
		t.Fatalf("StoreFiles: %v", err)
	}
	if rep.degradedBy != nil {
		t.Errorf("degraded by %v — a refused file is not an association failure", rep.degradedBy)
	}
	failed := 0
	for _, p := range reports {
		if p.Err == nil {
			continue
		}
		failed++
		var st *netdicom.CStoreStatusError
		if !fail[uids[p.Path]] || !errors.As(p.Err, &st) {
			t.Errorf("%s: unexpected failure %v", filepath.Base(p.Path), p.Err)
		}
	}
	if failed != len(fail) || len(reports) != len(paths) {
		t.Errorf("%d failed of %d reported, want %d of %d", failed, len(reports), len(fail), len(paths))
	}
	if n := len(scp.stored()); n != len(paths)-len(fail) {
		t.Errorf("%d stored, want %d", n, len(paths)-len(fail))
	}
}

// A server that goes silent ends the push after the idle timeout, every file
// reported as not answered — whether it never answers the association request
// or stops answering in the middle of a C-STORE.
func TestPushAbortsSilentServer(t *testing.T) {
	const idle = 300 * time.Millisecond
	assertSilent := func(t *testing.T, cl *DicomClient, paths []string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		start := time.Now()
		reports, _, err := collectPush(t, cl, ctx, paths, idle)
		if err != nil {
			t.Fatalf("StoreFiles: %v", err)
		}
		if took := time.Since(start); took > 10*time.Second {
			t.Errorf("took %v to give up on a silent server", took)
		}
		if len(reports) != len(paths) {
			t.Fatalf("%d reports for %d files", len(reports), len(paths))
		}
		for _, p := range reports {
			var nr errNoResponse
			if !errors.As(p.Err, &nr) {
				t.Errorf("%s: err %v, want the server not responding", filepath.Base(p.Path), p.Err)
			}
		}
	}

	t.Run("handshake", func(t *testing.T) {
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
				go func() { defer conn.Close(); _, _ = io.Copy(io.Discard, conn) }()
			}
		}()
		paths, _ := pushStudy(t, 4)
		cl := NewDicomClient(ServerProfile{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port,
			RemoteAETitle: "SILENT"}, "TESTSCU")
		assertSilent(t, cl, paths)
	})
	t.Run("mid-store", func(t *testing.T) {
		scp := startFakeStoreSCP(t, &fakeStoreSCP{block: make(chan struct{})})
		paths, _ := pushStudy(t, 4)
		assertSilent(t, scp.client(1), paths)
	})
}

// Cancel ends a push at once even with the idle timeout disabled, aborting the
// associations rather than waiting on a server that will never answer.
func TestPushCancelAbortsPromptly(t *testing.T) {
	scp := startFakeStoreSCP(t, &fakeStoreSCP{block: make(chan struct{})})
	paths, _ := pushStudy(t, 20, 20)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	reports, _, err := collectPush(t, scp.client(2), ctx, paths, 0)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want cancelled", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("cancel took %v", took)
	}
	for _, p := range reports {
		if p.Err == nil {
			t.Errorf("%s reported sent by a server that stored nothing", filepath.Base(p.Path))
		}
	}
}

// slowConversions replaces the conversion with one that takes d longer and
// counts how many run at once — inside the bound convertForPush applies, so
// the peak is what the bound let through.
func slowConversions(t *testing.T, d time.Duration) (active, peak *atomic.Int32) {
	t.Helper()
	active, peak = new(atomic.Int32), new(atomic.Int32)
	saved := pushConvertFn
	t.Cleanup(func() { pushConvertFn = saved })
	pushConvertFn = func(path string, ident dicomFileIdentity, target string) ([]byte, dicomFileIdentity, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(d)
		return saved(path, ident, target)
	}
	return active, peak
}

// A server that does not accept the files' stored syntax has every file
// converted before it is sent — several at once across the associations, as
// far as the conversion limit allows, and every file arriving in the syntax
// the server accepted.
func TestParallelPushConvertsInParallel(t *testing.T) {
	_, peak := slowConversions(t, 40*time.Millisecond)
	scp := startFakeStoreSCP(t, &fakeStoreSCP{accept: []string{tsImplicitVRLE}})
	paths, _ := pushStudy(t, 16, 8, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	reports, rep, err := collectPush(t, scp.client(4), ctx, paths, 5*time.Second)
	if err != nil {
		t.Fatalf("StoreFiles: %v", err)
	}
	for _, p := range reports {
		if p.Err != nil {
			t.Errorf("%s: %v", filepath.Base(p.Path), p.Err)
		}
	}
	scp.mu.Lock()
	syntax := maps.Clone(scp.syntax)
	scp.mu.Unlock()
	if syntax[tsImplicitVRLE] != len(paths) || len(syntax) != 1 {
		t.Errorf("received by syntax %v, want all %d in Implicit VR Little Endian", syntax, len(paths))
	}
	limit := pushConversionLimit(rep.limit)
	if got := peak.Load(); got > int32(limit) || (limit > 1 && got < 2) {
		t.Errorf("at most %d conversions at once, with a limit of %d", got, limit)
	}
}

// The conversion limit holds however many associations there are.
func TestPushConversionLimitHolds(t *testing.T) {
	t.Setenv("DICOMQR_MODIFY_WORKERS", "2")
	_, peak := slowConversions(t, 30*time.Millisecond)
	scp := startFakeStoreSCP(t, &fakeStoreSCP{accept: []string{tsImplicitVRLE}})
	paths, _ := pushStudy(t, 16, 8, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, _, err := collectPush(t, scp.client(4), ctx, paths, 5*time.Second); err != nil {
		t.Fatalf("StoreFiles: %v", err)
	}
	if got := peak.Load(); got > 2 {
		t.Errorf("%d conversions at once, limit 2", got)
	}
}

// With one association, the next file is converted while the current one is
// being stored — the conversion no longer follows the send.
func TestPushConvertsAheadOfTheSend(t *testing.T) {
	active, _ := slowConversions(t, 60*time.Millisecond)
	var overlapped atomic.Int32
	scp := startFakeStoreSCP(t, &fakeStoreSCP{
		accept: []string{tsImplicitVRLE},
		delay:  30 * time.Millisecond,
		onStore: func() {
			time.Sleep(30 * time.Millisecond)
			if active.Load() > 0 {
				overlapped.Add(1)
			}
		},
	})
	paths, _ := pushStudy(t, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, _, err := collectPush(t, scp.client(1), ctx, paths, 5*time.Second); err != nil {
		t.Fatalf("StoreFiles: %v", err)
	}
	if overlapped.Load() == 0 {
		t.Error("no conversion ran while the server was storing — the push converts only after each send")
	}
}

// The read-ahead budget is shared by every association; at its smallest each
// file is admitted alone, and the push must still finish rather than deadlock.
func TestParallelPushUnderTinyMemoryBudget(t *testing.T) {
	saved := modifyMemoryBudget
	modifyMemoryBudget = 1
	t.Cleanup(func() { modifyMemoryBudget = saved })

	scp := startFakeStoreSCP(t, &fakeStoreSCP{delay: 2 * time.Millisecond})
	paths, _ := pushStudy(t, 20, 10, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	reports, _, err := collectPush(t, scp.client(4), ctx, paths, 5*time.Second)
	if err != nil {
		t.Fatalf("StoreFiles: %v", err)
	}
	if len(reports) != len(paths) || len(scp.stored()) != len(paths) {
		t.Errorf("%d reported, %d stored, want %d", len(reports), len(scp.stored()), len(paths))
	}
}
