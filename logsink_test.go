package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

// Ordinary lines are batched — not on disk the moment they are written — and
// land, in order, once the batch flushes on its own timer.
func TestFileLogSinkBatchesThenFlushesOnTimer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dicom.log")
	s := &fileLogSink{path: path}
	s.Write([]byte("07:00:00.000001 [I] one\n"))
	s.Write([]byte("07:00:00.000002 dicom.stateMachine: two\n"))
	if got := readLog(t, path); got != "" {
		t.Fatalf("lines reached the file before any flush: %q", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for readLog(t, path) == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got, want := readLog(t, path), "07:00:00.000001 [I] one\n07:00:00.000002 dicom.stateMachine: two\n"; got != want {
		t.Fatalf("after the timed flush the file holds %q, want %q", got, want)
	}
}

// An error line is written through at once, with everything queued ahead of it
// — so the lines that explain a crash are on disk even if the process dies
// before the next timed flush.
func TestFileLogSinkWritesErrorLinesThrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dicom.log")
	s := &fileLogSink{path: path}
	s.Write([]byte("07:00:00.000001 [W] context\n"))
	s.Write([]byte("07:00:00.000002 [E] c-get: PANIC saving x\n"))
	if got := readLog(t, path); !strings.Contains(got, "[W] context") || !strings.Contains(got, "[E] c-get") {
		t.Fatalf("error line not written through with its context: %q", got)
	}
}

// A burst larger than the batch limit flushes early rather than growing the
// batch without bound.
func TestFileLogSinkFlushesEarlyWhenLarge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dicom.log")
	s := &fileLogSink{path: path}
	line := []byte(strings.Repeat("x", 1023) + "\n")
	for i := 0; i < logFlushBytes/len(line)+1; i++ {
		s.Write(line)
	}
	if got := len(readLog(t, path)); got < logFlushBytes {
		t.Fatalf("file holds %d bytes after a %d-byte burst, want the batch flushed early", got, logFlushBytes)
	}
}

// Writers on many goroutines, with timed, size and explicit flushes all racing:
// every line lands exactly once and each goroutine's lines stay in its order.
// (Run under -race this also checks the sink's locking.)
func TestFileLogSinkConcurrentWritersKeepOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dicom.log")
	s := &fileLogSink{path: path}
	const writers, perWriter = 8, 500
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				s.Write([]byte(fmt.Sprintf("w%d %06d %s\n", w, i, strings.Repeat("p", 200))))
				if i%97 == 0 {
					s.flush()
				}
			}
		}()
	}
	wg.Wait()
	s.flush()

	next := make([]int, writers)
	lines := strings.Split(strings.TrimSuffix(readLog(t, path), "\n"), "\n")
	if len(lines) != writers*perWriter {
		t.Fatalf("file holds %d lines, want %d", len(lines), writers*perWriter)
	}
	for _, l := range lines {
		var w, i int
		if _, err := fmt.Sscanf(l, "w%d %d", &w, &i); err != nil {
			t.Fatalf("garbled line %q", l)
		}
		if i != next[w] {
			t.Fatalf("writer %d: line %d arrived when %d was expected", w, i, next[w])
		}
		next[w]++
	}
}

func TestIsErrorLogLine(t *testing.T) {
	for line, want := range map[string]bool{
		"07:53:54.123456 [E] catalog: open: x\n":     true,
		"[E] no timestamp\n":                         true,
		"07:53:54.123456 [W] retrieve: slow\n":       false,
		"07:53:54.123456 dicom.stateMachine: [E] \n": false, // the tag only counts where logError puts it
	} {
		if got := isErrorLogLine([]byte(line)); got != want {
			t.Errorf("isErrorLogLine(%q) = %v, want %v", line, got, want)
		}
	}
}
