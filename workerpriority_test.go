package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdicom "github.com/suyashkumar/dicom"
)

// A worker's thread runs below normal priority for as long as the worker lives,
// and is back at normal priority before it is released to the runtime — a
// lowered thread returned to the pool would slow whatever goroutine ran on it
// next.
func TestLowerWorkerPriority(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		// Hold the thread ourselves too, so after restore unlocks its own
		// hold this goroutine is still on the thread whose priority it checks.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		restore := lowerWorkerPriority()
		during, err := currentThreadPriority()
		if err != nil {
			done <- err
			return
		}
		restore()
		after, err := currentThreadPriority()
		if err != nil {
			done <- err
			return
		}
		if during != threadPriorityBelowNormal || after != threadPriorityNormal {
			done <- fmt.Errorf("priority %d while working, %d after; want %d then %d",
				during, after, threadPriorityBelowNormal, threadPriorityNormal)
			return
		}
		done <- nil
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// CPU-heavy runs take about one worker per physical core within [2, 8];
// tag-only folder exports keep 4; never more than there are files; and the
// environment override is honoured, clamped.
func TestModifyWorkerCount(t *testing.T) {
	cpus := runtime.NumCPU()
	if got, want := modifyWorkerCount(true, 1000), max(2, min(cpus/2, 8)); got != want {
		t.Errorf("CPU-heavy run: %d workers on %d logical processors, want %d", got, cpus, want)
	}
	if got, want := modifyWorkerCount(false, 1000), min(4, cpus); got != want {
		t.Errorf("tag-only run: %d workers, want %d", got, want)
	}
	if got := modifyWorkerCount(true, 1); got != 1 {
		t.Errorf("one file: %d workers, want 1", got)
	}
	t.Setenv("DICOMQR_MODIFY_WORKERS", "3")
	if got := modifyWorkerCount(true, 1000); got != 3 {
		t.Errorf("override 3: %d workers", got)
	}
	t.Setenv("DICOMQR_MODIFY_WORKERS", "500")
	if got := modifyWorkerCount(false, 1000); got != 64 {
		t.Errorf("override 500: %d workers, want the 64 clamp", got)
	}
	t.Setenv("DICOMQR_MODIFY_WORKERS", "nonsense")
	if got := modifyWorkerCount(false, 1000); got != min(4, cpus) {
		t.Errorf("malformed override: %d workers, want the default", got)
	}
}

// Tag-only runs are now admitted by weight too, so a wider pool can never hold
// more than the memory budget. With a budget smaller than any one file, every
// file's weight clamps to the whole budget and files run strictly one at a
// time — and every one still exports.
func TestTagOnlyRunIsGatedByMemoryBudget(t *testing.T) {
	t.Setenv("DICOMQR_MODIFY_WORKERS", "4")
	original := modifyMemoryBudget
	t.Cleanup(func() { modifyMemoryBudget = original })
	modifyMemoryBudget = 1

	rootDir := t.TempDir()
	var files []string
	for i := 0; i < 8; i++ {
		p := filepath.Join(rootDir, fmt.Sprintf("f%d.dcm", i))
		writeRawPixelFixture(t, p, tsExplicitVRLE)
		files = append(files, p)
	}

	var inFlight, peak atomic.Int32
	var mu sync.Mutex
	real := processFileFn
	t.Cleanup(func() { processFileFn = real })
	processFileFn = func(src *os.File, p modifyParams, r *uidRemapper) (bool, sdicom.Dataset, fileNotes, error) {
		n := inFlight.Add(1)
		mu.Lock()
		if n > peak.Load() {
			peak.Store(n)
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond) // give a second worker the chance to overlap
		defer inFlight.Add(-1)
		return real(src, p, r)
	}

	params, err := compileModifyParams(ModProfile{Sets: []string{"0010,0010=ANON"}})
	if err != nil {
		t.Fatal(err)
	}
	res := runModification(context.Background(), files, rootDir, t.TempDir(), params, nil, nil)
	if res.Failed != 0 || res.Processed != len(files) {
		t.Fatalf("result = %+v, want every file exported", res)
	}
	if p := peak.Load(); p != 1 {
		t.Errorf("%d files processed at once under a budget smaller than one file, want 1", p)
	}
}
