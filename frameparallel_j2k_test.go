//go:build openjpeg

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Masking a multi-frame JPEG 2000 file decodes, masks, re-encodes and verifies
// every frame. With frame parallelism the frames of a file spread across the
// run's idle cores; the export must be byte-for-byte what the serial path
// writes. An allowance of one leaves no free token, so that run is serial; an
// allowance of four for a single file leaves three free, so its frames fan
// out — which the test confirms happened, since a run that quietly stayed
// serial would pass the comparison without testing anything.
func TestParallelFramesMatchSerialExport(t *testing.T) {
	rootDir := t.TempDir()
	src := writeJ2KFixture(t, rootDir, j2kFixtureOpts{name: "mf.dcm", tsUID: tsJPEG2000LL, frames: 24})
	params, err := compileModifyParams(ModProfile{
		MaskRegions: []MaskRegion{{Mode: maskModeRect, W: 1, H: 0.25}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Count the frames encoded at once, through the encoder seam.
	var probe concurrencyProbe
	orig := encodeMaskedFrame
	t.Cleanup(func() { encodeMaskedFrame = orig })
	encodeMaskedFrame = func(ts string, planar []int32, cols, rows, spp, prec int, signed, mct bool) ([]byte, error) {
		probe.enter()
		defer probe.leave()
		time.Sleep(2 * time.Millisecond) // long enough for helpers to overlap
		return orig(ts, planar, cols, rows, spp, prec, signed, mct)
	}

	export := func(workers string) []byte {
		t.Helper()
		t.Setenv("DICOMQR_MODIFY_WORKERS", workers)
		probe.peak.Store(0)
		out := t.TempDir()
		res := runModification(context.Background(), []string{src}, rootDir, out, params, nil, nil)
		if res.Failed != 0 || res.Processed != 1 || res.MaskRecompressed != 1 {
			t.Fatalf("%s worker(s): result = %+v, want the file masked and recompressed", workers, res)
		}
		data, err := os.ReadFile(filepath.Join(out, "mf.dcm"))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	serial := export("1")
	if p := probe.peak.Load(); p != 1 {
		t.Fatalf("allowance 1: %d frames encoded at once, want 1", p)
	}
	parallel := export("4")
	if p := probe.peak.Load(); p < 2 || p > 4 {
		t.Fatalf("allowance 4: %d frames encoded at once, want 2..4", p)
	}
	if !bytes.Equal(serial, parallel) {
		t.Fatal("frame-parallel export differs from the serial one")
	}
}
