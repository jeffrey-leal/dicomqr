package main

// Push negotiation: each file is offered in the transfer syntax it is stored
// in, verbatim, so compressed bytes are never sent under an uncompressed
// label, and any SOP class can be pushed.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	netdicom "github.com/algm/go-netdicom"
	"github.com/algm/go-netdicom/dimse"
)

const (
	testCTClass     = "1.2.840.10008.5.1.4.1.1.2"     // CT Image Storage
	testDoseClass   = "1.2.840.10008.5.1.4.1.1.88.67" // X-Ray Radiation Dose SR Storage
	testVendorClass = "1.3.6.1.4.1.99999.1.1"         // a private class on no list
)

// writeRawPart10 writes a Part-10 file by hand: preamble, File Meta group,
// then dataset exactly as given — so a test can label any bytes with any
// transfer syntax and check they arrive unchanged.
func writeRawPart10(t *testing.T, path, sopClass, sopInstance, ts string, dataset []byte) {
	t.Helper()
	element := func(el uint16, vr string, value []byte) []byte {
		if len(value)%2 == 1 {
			value = append(slices.Clone(value), 0)
		}
		var b bytes.Buffer
		_ = binary.Write(&b, binary.LittleEndian, [2]uint16{0x0002, el})
		b.WriteString(vr)
		if vr == "OB" {
			b.Write([]byte{0, 0})
			_ = binary.Write(&b, binary.LittleEndian, uint32(len(value)))
		} else {
			_ = binary.Write(&b, binary.LittleEndian, uint16(len(value)))
		}
		b.Write(value)
		return b.Bytes()
	}
	var meta bytes.Buffer
	meta.Write(element(0x0001, "OB", []byte{0, 1}))
	meta.Write(element(0x0002, "UI", []byte(sopClass)))
	meta.Write(element(0x0003, "UI", []byte(sopInstance)))
	meta.Write(element(0x0010, "UI", []byte(ts)))
	groupLength := binary.LittleEndian.AppendUint32(nil, uint32(meta.Len()))

	var out bytes.Buffer
	out.Write(make([]byte, 128))
	out.WriteString("DICM")
	out.Write(element(0x0000, "UL", groupLength))
	out.Write(meta.Bytes())
	out.Write(dataset)
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

type pushReceived struct {
	sopClass, transferSyntax string
	data                     []byte
}

// startPushRecorder runs a C-STORE provider on loopback that records what it
// receives. accepted restricts its transfer syntaxes, in preference order;
// nil accepts the first one offered in each context, as dcm4che-based
// archives such as dicomsrv do.
func startPushRecorder(t *testing.T, accepted []string) (port int, received func() []pushReceived) {
	t.Helper()
	var mu sync.Mutex
	var got []pushReceived
	sp, err := netdicom.NewServiceProvider(netdicom.ServiceProviderParams{
		AETitle:                  "TESTSCP",
		AcceptedTransferSyntaxes: accepted,
		CStore: func(_ context.Context, _ netdicom.ConnectionState, ts, sopClass, _ string,
			data io.Reader, _ int64) dimse.Status {
			b, err := io.ReadAll(data)
			if err != nil {
				return dimse.Status{Status: dimse.CStoreCannotUnderstand}
			}
			mu.Lock()
			got = append(got, pushReceived{sopClass, ts, b})
			mu.Unlock()
			return dimse.Success
		},
	}, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go sp.Run(ctx)
	t.Cleanup(func() {
		cancel()
		_ = sp.Close()
	})
	time.Sleep(100 * time.Millisecond)
	return sp.ListenAddr().(*net.TCPAddr).Port, func() []pushReceived {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

func pushAll(t *testing.T, port int, paths []string) []StoreProgress {
	t.Helper()
	client := NewDicomClient(ServerProfile{Host: "127.0.0.1", Port: port, RemoteAETitle: "TESTSCP"}, "TESTSCU")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var progress []StoreProgress
	if _, err := client.StoreFiles(ctx, paths, 0, func(p StoreProgress) { progress = append(progress, p) }); err != nil {
		t.Fatalf("StoreFiles: %v", err)
	}
	return progress
}

func TestStoreFilesSendsEveryFileInItsStoredSyntax(t *testing.T) {
	port, received := startPushRecorder(t, nil)
	dir := t.TempDir()
	j2kData := []byte("JPEG 2000 data set bytes, never re-encoded")
	files := []struct{ class, ts string }{
		{testCTClass, tsJPEG2000LL},
		{testCTClass, tsExplicitVRLE},
		{testDoseClass, tsExplicitVRLE},
		{testVendorClass, tsImplicitVRLE},
	}
	var paths []string
	for i, f := range files {
		p := filepath.Join(dir, fmt.Sprintf("f%d.dcm", i))
		data := []byte(fmt.Sprintf("data set %d", i))
		if f.ts == tsJPEG2000LL {
			data = j2kData
		}
		writeRawPart10(t, p, f.class, fmt.Sprintf("1.2.3.%d", i), f.ts, data)
		paths = append(paths, p)
	}

	for _, p := range pushAll(t, port, paths) {
		if p.Err != nil {
			t.Errorf("%s: %v", filepath.Base(p.Path), p.Err)
		}
	}

	got := received()
	if len(got) != len(files) {
		t.Fatalf("server received %d files, want %d", len(got), len(files))
	}
	for i, f := range files {
		if got[i].sopClass != f.class || got[i].transferSyntax != f.ts {
			t.Errorf("file %d arrived as %s in %s, want %s in %s", i, got[i].sopClass, got[i].transferSyntax, f.class, f.ts)
		}
	}
	if !bytes.Equal(got[0].data, j2kData) {
		t.Error("the JPEG 2000 data set did not arrive byte for byte")
	}
}

// The defect: offered through the old canonicalised list, the server accepted
// "Explicit VR LE" for CT and the push sent JPEG 2000 bytes under that label.
// A compressed file the server will not take must be converted or reported —
// here the bytes are not a real JPEG 2000 stream, so conversion fails — and
// never sent as something it is not.
func TestStoreFilesNeverSendsCompressedBytesUnderAnUncompressedLabel(t *testing.T) {
	port, received := startPushRecorder(t, []string{tsExplicitVRLE, tsImplicitVRLE})
	p := filepath.Join(t.TempDir(), "j2k.dcm")
	writeRawPart10(t, p, testCTClass, "1.2.3.1", tsJPEG2000LL, []byte("not really JPEG 2000"))

	progress := pushAll(t, port, []string{p})

	if len(progress) != 1 || progress[0].Err == nil {
		t.Fatalf("progress %+v: want the file reported as failed", progress)
	}
	if !strings.Contains(progress[0].Err.Error(), "cannot convert") {
		t.Errorf("error %q should say the file could not be converted", progress[0].Err)
	}
	if got := received(); len(got) != 0 {
		t.Errorf("server received %d data sets, want none: %+v", len(got), got)
	}
}

func TestStoreFilesConvertsARefusedSyntaxToAnAcceptedOne(t *testing.T) {
	port, received := startPushRecorder(t, []string{tsExplicitVRLE})
	dir := t.TempDir()
	src := filepath.Join(dir, "explicit.dcm")
	writeModifyTestDICOM(t, src)
	implicit, changed, err := transcodeDICOMFileToTemp(src, tsImplicitVRLE, dir)
	if err != nil || !changed {
		t.Fatalf("make Implicit VR source: changed=%v err=%v", changed, err)
	}

	progress := pushAll(t, port, []string{implicit})

	if progress[0].Err != nil {
		t.Fatalf("push: %v", progress[0].Err)
	}
	got := received()
	if len(got) != 1 || got[0].transferSyntax != tsExplicitVRLE {
		t.Fatalf("received %+v, want one data set in Explicit VR Little Endian", got)
	}
}

// TestSamplePushStudy pushes every file under a real study folder to a running
// server. Field hook: DICOMQR_PUSH_SAMPLE=<folder>
// DICOMQR_PUSH_SAMPLE_SERVER=<host>:<port>:<AE title>, optionally
// DICOMQR_PUSH_SAMPLE_PARALLEL=<1-4> for the server's Parallel transfers —
// run it at 1 and then at 3 to measure what parallel associations buy on that
// server. It asserts only that every file was sent; what arrived, and in which
// syntax, is checked on the receiving side.
func TestSamplePushStudy(t *testing.T) {
	folder, server := os.Getenv("DICOMQR_PUSH_SAMPLE"), os.Getenv("DICOMQR_PUSH_SAMPLE_SERVER")
	if folder == "" || server == "" {
		t.Skip("set DICOMQR_PUSH_SAMPLE and DICOMQR_PUSH_SAMPLE_SERVER")
	}
	parts := strings.Split(server, ":")
	if len(parts) != 3 {
		t.Fatalf("DICOMQR_PUSH_SAMPLE_SERVER %q: want host:port:AE", server)
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	err = filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			paths = append(paths, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	parallel, _ := strconv.Atoi(os.Getenv("DICOMQR_PUSH_SAMPLE_PARALLEL"))
	client := NewDicomClient(ServerProfile{Host: parts[0], Port: port, RemoteAETitle: parts[2],
		ParallelTransfers: parallel}, "DICOMQR")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	start := time.Now()
	var mu sync.Mutex
	failed := 0
	rep, err := client.StoreFiles(ctx, paths, defaultStallTimeout, func(p StoreProgress) {
		if p.Err != nil {
			mu.Lock()
			failed++
			mu.Unlock()
			t.Errorf("%s: %v", p.Path, p.Err)
		}
	})
	if err != nil {
		t.Fatalf("StoreFiles: %v", err)
	}
	t.Logf("pushed %d files in %v at up to %d association(s), %d failed%s", len(paths),
		time.Since(start).Round(time.Millisecond), rep.limit, failed, describeParallelDegrade(rep.degradedBy))
}

func TestPushPresentationContexts(t *testing.T) {
	ident := func(class, ts string) pushIdentity {
		return pushIdentity{dicomFileIdentity: dicomFileIdentity{sopClassUID: class, transferSyntaxUID: ts}}
	}
	describe := func(contexts []netdicom.PresentationContextSpec) []string {
		var out []string
		for _, c := range contexts {
			out = append(out, c.AbstractSyntax+": "+strings.Join(c.TransferSyntaxes, ", "))
		}
		return out
	}

	got := describe(pushPresentationContexts([]pushIdentity{
		ident(testCTClass, tsJPEG2000LL),
		ident(testCTClass, tsExplicitVRLE),
		ident(testCTClass, tsJPEG2000LL),
		{err: fmt.Errorf("unreadable")},
		ident(testDoseClass, tsImplicitVRLE),
		ident(testVendorClass, tsJPEGBaseline),
	}))
	want := []string{
		testCTClass + ": " + tsJPEG2000LL,
		testCTClass + ": " + tsExplicitVRLE + ", " + tsImplicitVRLE,
		testDoseClass + ": " + tsImplicitVRLE + ", " + tsExplicitVRLE,
		testVendorClass + ": " + tsJPEGBaseline,
		testVendorClass + ": " + tsExplicitVRLE + ", " + tsImplicitVRLE,
	}
	if !slices.Equal(got, want) {
		t.Errorf("contexts\n got %q\nwant %q", got, want)
	}

	// 50 classes x 2 compressed syntaxes = 150 contexts, over the limit: each
	// class falls back to its uncompressed context alone.
	var many []pushIdentity
	for i := 1; i <= 50; i++ {
		class := fmt.Sprintf("1.2.3.%d", i)
		many = append(many, ident(class, tsJPEG2000LL), ident(class, tsJPEGBaseline))
	}
	contexts := pushPresentationContexts(many)
	if len(contexts) != 50 {
		t.Fatalf("%d contexts, want 50", len(contexts))
	}
	for _, c := range contexts {
		if !slices.Equal(c.TransferSyntaxes, []string{tsExplicitVRLE, tsImplicitVRLE}) {
			t.Fatalf("over the limit, %s offered %v", c.AbstractSyntax, c.TransferSyntaxes)
		}
	}

	if got := pushPresentationContexts([]pushIdentity{{err: fmt.Errorf("unreadable")}}); len(got) != 0 {
		t.Errorf("no readable file should propose nothing, got %v", got)
	}
}
