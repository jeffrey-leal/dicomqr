//go:build openjpeg

package main

import "testing"

// Round-trip coverage for the reversible J2K encoder: every shape the masking
// recompression path can feed it must decode back bit-identical, including
// geometry, precision and signedness — the decode-back verification in
// encodeAndVerifyFrame leans on exactly this.
func TestEncodeJPEG2000LosslessRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		w, h   int
		nc     int
		prec   int
		signed bool
		mct    bool
		sample func(c, i int) int32
	}{
		{"gray8", 8, 8, 1, 8, false, false,
			func(c, i int) int32 { return int32((i * 37) % 256) }},
		{"gray12 odd size", 7, 5, 1, 12, false, false,
			func(c, i int) int32 { return int32((i * 311) % 4096) }},
		{"gray16 signed", 9, 11, 1, 16, true, false,
			func(c, i int) int32 { return int32((i*613)%65536 - 32768) }},
		{"rgb8", 8, 8, 3, 8, false, false,
			func(c, i int) int32 { return int32((i*31 + c*77) % 256) }},
		{"rgb8 mct", 8, 8, 3, 8, false, true,
			func(c, i int) int32 { return int32((i*29 + c*101) % 256) }},
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
			data, err := encodeJPEG2000Lossless(samples, tc.w, tc.h, tc.nc, tc.prec, tc.signed, tc.mct)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			w, h, nc, prec, signed, got, err := decodeJPEG2000(data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if w != tc.w || h != tc.h || nc != tc.nc || prec != tc.prec || signed != tc.signed {
				t.Fatalf("decoded as %dx%d nc=%d prec=%d signed=%v, want %dx%d nc=%d prec=%d signed=%v",
					w, h, nc, prec, signed, tc.w, tc.h, tc.nc, tc.prec, tc.signed)
			}
			for i := range samples {
				if got[i] != samples[i] {
					t.Fatalf("sample %d = %d, want %d — round trip is not lossless", i, got[i], samples[i])
				}
			}
		})
	}
}
