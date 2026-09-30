package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A push reads every file's identity once, up front and in parallel, then
// reads each file's bytes one ahead of the send. End to end against the
// application's own SCP on loopback: every valid file arrives, a file that is
// not DICOM is reported through progress without stopping the push, and
// progress comes in order, once per file.
func TestStoreFilesPushesEveryFile(t *testing.T) {
	const port = 11196
	recvDir := t.TempDir()
	scp := NewStorageSCP("TESTSCP", port, recvDir)
	var mu sync.Mutex
	received := map[string]bool{}
	scp.SetOnFileReceived(func(p string) { mu.Lock(); received[filepath.Base(p)] = true; mu.Unlock() })
	if err := scp.Start(); err != nil {
		t.Fatalf("scp start: %v", err)
	}
	defer scp.Stop()

	srcDir := t.TempDir()
	var paths []string
	for i := 0; i < 6; i++ {
		p := filepath.Join(srcDir, fmt.Sprintf("img%d.dcm", i))
		writeSizedTestDICOM(t, p, "CT", 16, 16, "1.2.3.9", 1, i+1)
		paths = append(paths, p)
	}
	bad := filepath.Join(srcDir, "notdicom.dcm")
	if err := os.WriteFile(bad, []byte("not a DICOM file"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths = append(paths[:3], append([]string{bad}, paths[3:]...)...) // in the middle

	client := NewDicomClient(ServerProfile{Host: "127.0.0.1", Port: port, RemoteAETitle: "TESTSCP"}, "TESTSCU")
	var progress []StoreProgress
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.StoreFiles(ctx, paths, func(p StoreProgress) { progress = append(progress, p) }); err != nil {
		t.Fatalf("StoreFiles: %v", err)
	}

	if len(progress) != len(paths) {
		t.Fatalf("%d progress reports, want %d", len(progress), len(paths))
	}
	for i, p := range progress {
		if p.Done != i+1 || p.Path != paths[i] {
			t.Errorf("report %d = done %d, %s; want %d, %s", i, p.Done, filepath.Base(p.Path), i+1, filepath.Base(paths[i]))
		}
		if (p.Path == bad) != (p.Err != nil) {
			t.Errorf("%s: err %v", filepath.Base(p.Path), p.Err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n == 6 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 6 {
		t.Errorf("SCP received %d files, want all 6 valid ones: %v", len(received), received)
	}
}
