//go:build openjpeg

package main

import (
	_ "embed"
	"testing"
)

// ramp8J2K is a lossless 8×8 8-bit grayscale JPEG 2000 codestream whose every
// row is the horizontal ramp 0,32,64,…,224 (generated with opj_compress -n 1).
//
//go:embed testdata/ramp8.j2k
var ramp8J2K []byte

func TestDecodeJPEG2000Geometry(t *testing.T) {
	w, h, nc, prec, signed, samples, err := decodeJPEG2000(ramp8J2K)
	if err != nil {
		t.Fatalf("decodeJPEG2000: %v", err)
	}
	if w != 8 || h != 8 {
		t.Fatalf("dimensions %d×%d, want 8×8", w, h)
	}
	if nc != 1 {
		t.Fatalf("numComps %d, want 1", nc)
	}
	if prec != 8 {
		t.Errorf("prec %d, want 8", prec)
	}
	if signed {
		t.Errorf("signed = true, want false")
	}
	if len(samples) != 64 {
		t.Fatalf("samples len %d, want 64", len(samples))
	}
	// Lossless: the first row must be the exact ramp.
	want := []int32{0, 32, 64, 96, 128, 160, 192, 224}
	for x := 0; x < 8; x++ {
		if samples[x] != want[x] {
			t.Errorf("row0[%d] = %d, want %d", x, samples[x], want[x])
		}
	}
}

func TestDecodeJPEG2000FrameMonochrome(t *testing.T) {
	// slope=1, intercept=0, no window → gray holds the raw ramp.
	df, err := decodeJPEG2000Frame(ramp8J2K, 1, 0, false, 0, 0, "MONOCHROME2")
	if err != nil {
		t.Fatalf("decodeJPEG2000Frame: %v", err)
	}
	if !df.windowable() {
		t.Fatal("monochrome J2K frame should be windowable")
	}
	if df.cols != 8 || df.rows != 8 || len(df.gray) != 64 {
		t.Fatalf("frame %d×%d, gray len %d", df.cols, df.rows, len(df.gray))
	}
	if df.gray[0] != 0 || df.gray[7] != 224 {
		t.Errorf("gray row0 ends = %v, want [0 … 224]", df.gray[:8])
	}
	if df.invert {
		t.Error("MONOCHROME2 must not invert")
	}
}
