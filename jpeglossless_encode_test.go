//go:build jpeglossless

package main

import "testing"

// Round-trip coverage for the SOF3 encoder: every shape the masking
// recompression path can feed it must decode back bit-identical. Samples are
// raw stored bits — T.81 carries no signedness — so a signed DICOM image is
// represented here by its unsigned two's-complement container values.
func TestEncodeJPEGLosslessRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		w, h   int
		nc     int
		prec   int
		sample func(c, i int) int32
	}{
		{"gray8", 8, 8, 1, 8,
			func(c, i int) int32 { return int32((i * 37) % 256) }},
		{"gray12 odd size", 7, 5, 1, 12,
			func(c, i int) int32 { return int32((i * 311) % 4096) }},
		{"gray16", 9, 11, 1, 16,
			func(c, i int) int32 { return int32((i * 613) % 65536) }},
		{"rgb8", 8, 8, 3, 8,
			func(c, i int) int32 { return int32((i*31 + c*77) % 256) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pixels := tc.w * tc.h
			samples := make([]int32, pixels*tc.nc)
			for c := 0; c < tc.nc; c++ {
				for i := 0; i < pixels; i++ {
					samples[c*pixels+i] = tc.sample(c, i)
				}
			}
			data, err := encodeJPEGLossless(samples, tc.w, tc.h, tc.nc, tc.prec)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			w, h, nc, prec, _, got, err := decodeJPEGLossless(data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if w != tc.w || h != tc.h || nc != tc.nc || prec != tc.prec {
				t.Fatalf("decoded as %dx%d nc=%d prec=%d, want %dx%d nc=%d prec=%d",
					w, h, nc, prec, tc.w, tc.h, tc.nc, tc.prec)
			}
			for i := range samples {
				if got[i] != samples[i] {
					t.Fatalf("sample %d = %d, want %d — round trip is not lossless", i, got[i], samples[i])
				}
			}
		})
	}
}
