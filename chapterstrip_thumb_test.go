package main

import (
	"image"
	"image/color"
	"testing"
)

// referenceThumb is scaleToThumb as it was — img.At per source pixel,
// averaged at 16 bits — kept as the oracle.
func referenceThumb(img image.Image, side int) *image.RGBA {
	b := img.Bounds()
	scale := min(float64(side)/float64(b.Dx()), float64(side)/float64(b.Dy()), 1)
	w := maxInt(1, int(float64(b.Dx())*scale))
	h := maxInt(1, int(float64(b.Dy())*scale))
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy0 := b.Min.Y + y*b.Dy()/h
		sy1 := maxInt(sy0+1, b.Min.Y+(y+1)*b.Dy()/h)
		for x := 0; x < w; x++ {
			sx0 := b.Min.X + x*b.Dx()/w
			sx1 := maxInt(sx0+1, b.Min.X+(x+1)*b.Dx()/w)
			var r, g, bl, a, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					cr, cg, cb, ca := img.At(sx, sy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = uint8(r/n>>8), uint8(g/n>>8), uint8(bl/n>>8), uint8(a/n>>8)
		}
	}
	return dst
}

// The byte-level box filter must give the thumbnails the per-pixel At version
// gave: identical for an RGBA frame (what the viewer now hands it), and within
// one level of rounding for a frame it converts first (averaging 8-bit rather
// than 16-bit values) — invisible in an 84-pixel tile. Includes a frame whose
// bounds do not start at the origin.
func TestScaleToThumbMatchesReference(t *testing.T) {
	rgba := image.NewRGBA(image.Rect(0, 0, 301, 199))
	for y := 0; y < 199; y++ {
		for x := 0; x < 301; x++ {
			rgba.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y * 3), uint8(x ^ y), 255})
		}
	}
	ycc := image.NewYCbCr(rgba.Rect, image.YCbCrSubsampleRatio420)
	for i := range ycc.Y {
		ycc.Y[i] = uint8(i * 7)
	}
	for i := range ycc.Cb {
		ycc.Cb[i], ycc.Cr[i] = uint8(100+i%50), uint8(140-i%40)
	}
	for name, tc := range map[string]struct {
		img     image.Image
		maxDiff int
	}{
		"RGBA":            {rgba, 0},
		"RGBA off-origin": {rgba.SubImage(image.Rect(40, 30, 280, 190)), 0},
		"YCbCr (JPEG)":    {ycc, 1},
	} {
		got, want := scaleToThumb(tc.img, 84), referenceThumb(tc.img, 84)
		if got.Rect != want.Rect {
			t.Fatalf("%s: thumbnail %v, want %v", name, got.Rect, want.Rect)
		}
		for i := range want.Pix {
			if d := int(got.Pix[i]) - int(want.Pix[i]); d > tc.maxDiff || -d > tc.maxDiff {
				t.Fatalf("%s: byte %d = %d, want %d (±%d)", name, i, got.Pix[i], want.Pix[i], tc.maxDiff)
			}
		}
	}
}
