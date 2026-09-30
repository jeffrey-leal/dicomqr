//go:build jpeglossless

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"testing"
)

func encodeTestJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// smoothRGBA is a gently varying colour image — what a clinical frame looks
// like to a JPEG decoder, as opposed to a synthetic pattern of hard colour
// edges that exaggerates the decoders' different chroma upsampling.
func smoothRGBA(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				uint8(128 + 100*math.Sin(float64(x)/40)),
				uint8(128 + 100*math.Cos(float64(y)/30)),
				uint8((x + y) / 6),
				255})
		}
	}
	return img
}

// libjpeg-turbo and Go's decoder must agree on what a JPEG shows, to within
// what two lossy decoders legitimately differ by (IDCT and chroma upsampling
// details): the display path switches decoders, and nothing on screen may
// visibly change. Colour comes back as RGBA, greyscale as Gray — the kinds the
// display path already handles.
func TestDecodeJPEGForDisplayAgreesWithGoDecoder(t *testing.T) {
	gray := image.NewGray(image.Rect(0, 0, 320, 240))
	for i := range gray.Pix {
		gray.Pix[i] = uint8(i % 320 * 255 / 320)
	}
	for name, src := range map[string]image.Image{"colour": smoothRGBA(320, 240), "greyscale": gray} {
		data := encodeTestJPEG(t, src)
		got, err := decodeJPEGForDisplay(data, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ref, _ := jpeg.Decode(bytes.NewReader(data))
		if got.Bounds() != ref.Bounds() {
			t.Fatalf("%s: bounds %v, want %v", name, got.Bounds(), ref.Bounds())
		}
		switch name {
		case "colour":
			if _, ok := got.(*image.RGBA); !ok {
				t.Errorf("colour decoded as %T, want *image.RGBA", got)
			}
		case "greyscale":
			if _, ok := got.(*image.Gray); !ok {
				t.Errorf("greyscale decoded as %T, want *image.Gray", got)
			}
		}
		gr, rr := toRGBA(got), toRGBA(ref)
		var sum, worst int
		for i := range gr.Pix {
			d := int(gr.Pix[i]) - int(rr.Pix[i])
			if d < 0 {
				d = -d
			}
			sum += d
			worst = max(worst, d)
		}
		mean := float64(sum) / float64(len(gr.Pix))
		if mean > 1.0 || worst > 12 {
			t.Errorf("%s: mean difference %.2f, worst %d — want the decoders to agree closely", name, mean, worst)
		}
	}
}

// A thumbnail decode picks the smallest libjpeg scale that still covers the
// requested size, and never goes below it.
func TestDecodeJPEGForDisplayReducedScale(t *testing.T) {
	data := encodeTestJPEG(t, smoothRGBA(800, 600))
	for _, tc := range []struct{ maxSide, wantW int }{{0, 800}, {1000, 800}, {400, 400}, {168, 200}, {90, 100}} {
		img, err := decodeJPEGForDisplay(data, tc.maxSide)
		if err != nil {
			t.Fatal(err)
		}
		if w := img.Bounds().Dx(); w != tc.wantW {
			t.Errorf("maxSide %d: width %d, want %d", tc.maxSide, w, tc.wantW)
		}
	}
}

// What libjpeg-turbo declines comes back as an error, so the viewer falls back
// to Go's decoder instead of showing nothing.
func TestDecodeJPEGForDisplayRejectsJunk(t *testing.T) {
	if _, err := decodeJPEGForDisplay([]byte{0xFF, 0xD8, 1, 2, 3, 4, 5}, 0); err == nil {
		t.Error("junk decoded without error")
	}
}
