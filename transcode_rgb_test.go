package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// referenceRGB is the conversion interleavedRGB replaced, kept verbatim as the
// oracle: img.At(x, y).RGBA() per pixel, each channel shifted to 8 bits.
func referenceRGB(img image.Image) []uint8 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]uint8, 0, w*h*3)
	for i := 0; i < w*h; i++ {
		r, g, bl, _ := img.At(b.Min.X+i%w, b.Min.Y+i/w).RGBA()
		out = append(out, uint8(r>>8), uint8(g>>8), uint8(bl>>8))
	}
	return out
}

// The fast path must produce exactly the samples the old per-component At()
// loop did — a masked JPEG export re-encodes these, and decode-back
// verification compares against them. Covers what Go's JPEG decoder really
// returns (a chroma-subsampled *image.YCbCr), a YCbCr whose bounds do not start
// at the origin (so the plane offsets are exercised), and the generic fallback.
func TestInterleavedRGBMatchesPerPixelAt(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 37, 23)) // odd sizes: partial chroma blocks
	for y := 0; y < 23; y++ {
		for x := 0; x < 37; x++ {
			src.SetRGBA(x, y, color.RGBA{uint8(x * 7), uint8(y * 11), uint8((x ^ y) * 5), 255})
		}
	}
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(&enc)
	if err != nil {
		t.Fatal(err)
	}
	ycc, ok := decoded.(*image.YCbCr)
	if !ok {
		t.Fatalf("decoder returned %T, want *image.YCbCr", decoded)
	}
	if ycc.SubsampleRatio == image.YCbCrSubsampleRatio444 {
		t.Fatal("fixture is not chroma-subsampled; the chroma offsets would go untested")
	}

	nrgba := image.NewNRGBA(src.Rect)
	copy(nrgba.Pix, src.Pix)

	for name, img := range map[string]image.Image{
		"decoded YCbCr":    ycc,
		"YCbCr off-origin": ycc.SubImage(image.Rect(5, 3, 30, 20)),
		"generic (NRGBA)":  nrgba,
	} {
		t.Run(name, func(t *testing.T) {
			got, want := interleavedRGB(img), referenceRGB(img)
			if !bytes.Equal(got, want) {
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("sample %d (pixel %d, channel %d) = %d, want %d", i, i/3, i%3, got[i], want[i])
					}
				}
				t.Fatalf("length %d, want %d", len(got), len(want))
			}
		})
	}
}
