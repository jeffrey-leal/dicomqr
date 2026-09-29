//go:build openjpeg

package main

import (
	"slices"
	"testing"
)

// j2kRamp encodes a w×h 12-bit greyscale ramp as a lossless codestream.
func j2kRamp(t *testing.T, w, h int) ([]byte, []int32) {
	t.Helper()
	samples := make([]int32, w*h)
	for i := range samples {
		samples[i] = int32((i*7 + i/w*13) % 4096)
	}
	data, err := encodeJPEG2000Lossless(samples, w, h, 1, 12, false, false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return data, samples
}

// Decoding on several threads must produce exactly what one thread does — the
// threads only split independent code-blocks between them.
func TestDecodeJPEG2000ThreadsMatchesSingleThread(t *testing.T) {
	// Logged rather than asserted: without thread support the option is
	// ignored and decoding is still correct, just not faster.
	t.Logf("OpenJPEG thread support: %v", jpeg2000ThreadSupport())
	data, want := j2kRamp(t, 300, 200)
	w, h, _, _, _, got, err := decodeJPEG2000Opts(data, frameDecodeOpts{threads: 8})
	if err != nil {
		t.Fatalf("threaded decode: %v", err)
	}
	if w != 300 || h != 200 || !slices.Equal(got, want) {
		t.Fatalf("threaded decode = %d×%d, samples equal %v; want the lossless 300×200 original",
			w, h, slices.Equal(got, want))
	}
}

// A thumbnail decode drops resolution levels while the image stays at least
// maxSide on its longer side, and never goes below it; a maxSide the image is
// already smaller than decodes in full.
func TestDecodeJPEG2000ReducedForThumbnail(t *testing.T) {
	data, _ := j2kRamp(t, 512, 384)
	for _, tc := range []struct {
		maxSide      int
		wantW, wantH int
	}{
		{0, 512, 384},   // full resolution
		{600, 512, 384}, // larger than the image: nothing to drop
		{256, 256, 192}, // one level
		{100, 128, 96},  // two levels: 64 would be below 100
		{64, 64, 48},    // three
	} {
		w, h, _, _, _, s, err := decodeJPEG2000Opts(data, frameDecodeOpts{maxSide: tc.maxSide})
		if err != nil {
			t.Fatalf("maxSide %d: %v", tc.maxSide, err)
		}
		if w != tc.wantW || h != tc.wantH || len(s) != w*h {
			t.Errorf("maxSide %d decoded %d×%d (%d samples), want %d×%d",
				tc.maxSide, w, h, len(s), tc.wantW, tc.wantH)
		}
	}
}

// A reduced frame reaching the viewer's decode keeps working end to end, with
// the reduced dimensions its samples actually have.
func TestDecodeJPEG2000FrameReduced(t *testing.T) {
	data, _ := j2kRamp(t, 512, 384)
	df, err := decodeJPEG2000Frame(data, frameDecodeOpts{maxSide: 128}, 1, 0, false, 0, 0, "MONOCHROME2")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if df.cols != 128 || df.rows != 96 || len(df.displayValues()) != 128*96 {
		t.Fatalf("reduced frame %d×%d with %d samples, want 128×96", df.cols, df.rows, len(df.displayValues()))
	}
}
