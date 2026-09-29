package main

import (
	"image"
	"image/color"
	"testing"

	"fyne.io/fyne/v2/test"
)

// coordRGBA is a w x h RGBA image whose pixel (x, y) is (x, y, x^y, 255), so
// any pixel read from the wrong place in a crop gives itself away.
func coordRGBA(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), uint8(x ^ y), 255})
		}
	}
	return img
}

// assertPackedCrop checks got is exactly what Fyne's texture upload can take —
// bounds from (0,0) and rows packed with no gap — holding src's crop pixel for
// pixel.
func assertPackedCrop(t *testing.T, got *image.RGBA, src image.Image, crop image.Rectangle) {
	t.Helper()
	if got.Rect.Min != (image.Point{}) || got.Rect.Size() != crop.Size() {
		t.Fatalf("crop bounds = %v, want %v from the origin", got.Rect, crop.Size())
	}
	if got.Stride != 4*crop.Dx() {
		t.Fatalf("crop stride = %d, want %d: rows must be packed, since Fyne uploads Pix ignoring Stride",
			got.Stride, 4*crop.Dx())
	}
	for y := 0; y < crop.Dy(); y++ {
		for x := 0; x < crop.Dx(); x++ {
			want := color.RGBAModel.Convert(src.At(crop.Min.X+x, crop.Min.Y+y)).(color.RGBA)
			if g := got.RGBAAt(x, y); g != want {
				t.Fatalf("crop pixel (%d,%d) = %v, want %v (source pixel (%d,%d))",
					x, y, g, want, crop.Min.X+x, crop.Min.Y+y)
			}
		}
	}
}

// The zoom display bug: the crop was handed to the canvas as a SubImage, whose
// rows sit a full frame's width apart, and Fyne uploaded it as though they were
// packed — the zoomed image sheared. cropPacked must produce a packed copy from
// every image type the viewer displays: RGBA (rendered greyscale, colour with
// overlays), NRGBA (J2K / JPEG Lossless colour) and YCbCr (JPEG Baseline).
func TestCropPacked(t *testing.T) {
	crop := image.Rect(3, 2, 10, 7) // off the origin on both axes, odd width

	rgba := coordRGBA(16, 12)
	nrgba := image.NewNRGBA(rgba.Rect)
	copy(nrgba.Pix, rgba.Pix)
	ycc := image.NewYCbCr(rgba.Rect, image.YCbCrSubsampleRatio444)
	for y := 0; y < 12; y++ {
		for x := 0; x < 16; x++ {
			ycc.Y[ycc.YOffset(x, y)] = uint8(x * 16)
			ycc.Cb[ycc.COffset(x, y)] = uint8(128 + y)
			ycc.Cr[ycc.COffset(x, y)] = uint8(100 + x)
		}
	}

	for name, src := range map[string]image.Image{"RGBA": rgba, "NRGBA": nrgba, "YCbCr": ycc} {
		t.Run(name, func(t *testing.T) {
			assertPackedCrop(t, cropPacked(nil, src, crop), src, crop)
		})
	}
}

// The buffer is reused while the crop keeps its size — pan and window/level
// drags refresh at pointer rate — and replaced when the size changes.
func TestCropPackedReusesBuffer(t *testing.T) {
	src := coordRGBA(16, 12)
	first := cropPacked(nil, src, image.Rect(0, 0, 8, 6))
	panned := cropPacked(first, src, image.Rect(5, 4, 13, 10))
	if panned != first {
		t.Error("a same-size crop allocated a new buffer")
	}
	assertPackedCrop(t, panned, src, image.Rect(5, 4, 13, 10))
	if resized := cropPacked(panned, src, image.Rect(0, 0, 4, 3)); resized == panned {
		t.Error("a differently sized crop reused the old buffer")
	}
}

// End to end through the viewport: zoomed in and panned away from the corner,
// the image on the canvas must be the packed crop of the rendered frame.
func TestViewportZoomDisplaysPackedCrop(t *testing.T) {
	test.NewApp()
	const cols, rows = 40, 30
	gray := make([]float32, cols*rows)
	for i := range gray {
		gray[i] = float32(i % 251)
	}
	df := &decodedFrame{rows: rows, cols: cols, gray: gray, lo: 0, hi: 250}
	v := newImageViewport()
	v.setContent(df, 125, 250, imageAnnotations{}, 0, 1, false)

	v.zoom = 2
	v.panCX, v.panCY = 27, 18
	v.applyDisplay()

	shown, ok := v.img.Image.(*image.RGBA)
	if !ok {
		t.Fatalf("zoomed canvas image is %T, want *image.RGBA", v.img.Image)
	}
	// A cols/2 x rows/2 crop centred on the pan point: (27-10, 18-7).
	assertPackedCrop(t, shown, v.base, image.Rect(17, 11, 37, 26))

	v.zoom = 1
	v.applyDisplay()
	if v.img.Image != v.base {
		t.Error("at fit zoom the canvas should show the rendered frame itself")
	}
}
