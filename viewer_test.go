package main

import (
	"image"
	"testing"
)

// ── decodedFrame.render ──────────────────────────────────────────────────────

// newTestFrame builds a 1-row grayscale frame from the given rescaled values.
func newTestFrame(vals ...float32) *decodedFrame {
	return &decodedFrame{rows: 1, cols: len(vals), gray: vals}
}

// grayOf returns the R channel of pixel i in an RGBA image (R==G==B for the
// grayscale colour map).
func grayOf(img *image.RGBA, i int) uint8 { return img.Pix[i*4] }

func TestDecodedFrameRenderWindowMapping(t *testing.T) {
	// Window centre 100, width 200 → maps [0,200] across [0,255].
	df := newTestFrame(0, 100, 200, -50, 300)
	img := df.render(&grayscaleMap, 100, 200).(*image.RGBA)

	want := []uint8{0, 127, 255, 0, 255} // -50 clamps low, 300 clamps high
	for i, w := range want {
		got := grayOf(img, i)
		if got != w {
			t.Errorf("pixel %d: got %d, want %d", i, got, w)
		}
		// Grayscale map: R==G==B, opaque alpha.
		if img.Pix[i*4+1] != got || img.Pix[i*4+2] != got || img.Pix[i*4+3] != 255 {
			t.Errorf("pixel %d not opaque grayscale: %v", i, img.Pix[i*4:i*4+4])
		}
	}
}

func TestDecodedFrameRenderMonochrome1Inverts(t *testing.T) {
	df := newTestFrame(0, 100, 200)
	df.invert = true
	img := df.render(&grayscaleMap, 100, 200).(*image.RGBA)

	// Same window as above but inverted: 0→255, 127→128, 255→0.
	want := []uint8{255, 128, 0}
	for i, w := range want {
		if got := grayOf(img, i); got != w {
			t.Errorf("pixel %d: got %d, want %d", i, got, w)
		}
	}
}

func TestDecodedFrameRenderAppliesColorMap(t *testing.T) {
	// Two pixels at the window extremes: index 0 → first LUT entry, 255 → last.
	df := newTestFrame(0, 200)
	img := df.render(&hotIronMap, 100, 200).(*image.RGBA)

	lo, hi := hotIronMap.lut[0], hotIronMap.lut[255]
	if got := [3]uint8{img.Pix[0], img.Pix[1], img.Pix[2]}; got != lo {
		t.Errorf("low pixel: got %v, want LUT[0] %v", got, lo)
	}
	if got := [3]uint8{img.Pix[4], img.Pix[5], img.Pix[6]}; got != hi {
		t.Errorf("high pixel: got %v, want LUT[255] %v", got, hi)
	}
}

func TestDecodedFrameRenderColorIgnoresWindow(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	df := &decodedFrame{rows: 1, cols: 2, colorImg: src}
	if df.windowable() {
		t.Fatal("colour frame should not be windowable")
	}
	if got := df.render(&grayscaleMap, 9999, 1); got != image.Image(src) {
		t.Error("render of a colour frame should return the original image unchanged")
	}
}

func TestDecodedFrameRenderZeroWidthDoesNotPanic(t *testing.T) {
	df := newTestFrame(0, 50, 100)
	// ww < 1 is clamped to 1 internally; just assert it produces a full image.
	img := df.render(&grayscaleMap, 50, 0).(*image.RGBA)
	if len(img.Pix) != 3*4 {
		t.Fatalf("expected 3 RGBA pixels, got %d bytes", len(img.Pix))
	}
}

// ── computeDefaultWindow ─────────────────────────────────────────────────────

func TestComputeDefaultWindowFromTags(t *testing.T) {
	df := newTestFrame(0, 1000, 2000, 3000, 4000)
	df.computeDefaultWindow(true, 350, 100)
	if !df.windowFromTags {
		t.Error("windowFromTags should be true when tags are supplied")
	}
	if df.wc != 350 || df.ww != 100 {
		t.Errorf("got wc=%v ww=%v, want 350/100", df.wc, df.ww)
	}
	if df.lo != 0 || df.hi != 4000 {
		t.Errorf("data range got lo=%v hi=%v, want 0/4000", df.lo, df.hi)
	}
}

func TestComputeDefaultWindowAutoUsesDataRange(t *testing.T) {
	// Without tags and with a tiny sample set, the percentile path collapses and
	// the full data range is used.
	df := newTestFrame(0, 50, 100)
	df.computeDefaultWindow(false, 0, 0)
	if df.windowFromTags {
		t.Error("windowFromTags should be false in the auto path")
	}
	if df.lo != 0 || df.hi != 100 {
		t.Errorf("data range got lo=%v hi=%v, want 0/100", df.lo, df.hi)
	}
	// Centre should sit within the data range.
	if df.wc < df.lo || df.wc > df.hi || df.ww <= 0 {
		t.Errorf("auto window out of range: wc=%v ww=%v", df.wc, df.ww)
	}
}

// ── presets (resolve / modality selection) ───────────────────────────────────

func TestResolvePresetCT(t *testing.T) {
	df := &decodedFrame{wc: 42, ww: 84, lo: -1000, hi: 3000}

	if wc, ww := resolvePreset(ctPresets, "Default", df); wc != 42 || ww != 84 {
		t.Errorf("Default: got %v/%v, want 42/84", wc, ww)
	}
	if wc, ww := resolvePreset(ctPresets, "Full range", df); wc != 1000 || ww != 4000 {
		t.Errorf("Full range: got %v/%v, want 1000/4000", wc, ww)
	}
	if wc, ww := resolvePreset(ctPresets, "Bone", df); wc != 480 || ww != 2500 {
		t.Errorf("Bone: got %v/%v, want 480/2500", wc, ww)
	}
	if wc, ww := resolvePreset(ctPresets, "Lung", df); wc != -600 || ww != 1500 {
		t.Errorf("Lung: got %v/%v, want -600/1500", wc, ww)
	}
	// Unknown preset falls back to the frame's own window.
	if wc, ww := resolvePreset(ctPresets, "Nonexistent", df); wc != 42 || ww != 84 {
		t.Errorf("unknown preset: got %v/%v, want 42/84", wc, ww)
	}
}

func TestResolvePresetPET(t *testing.T) {
	// PET "0 → X%" windows from 0 to a fraction of the peak (hi).
	df := &decodedFrame{wc: 5, ww: 10, lo: 0, hi: 1000}

	// 0 → 50% means upper = 500, so width 500 and centre 250 (window spans 0..500).
	wc, ww := resolvePreset(petPresets, "0 → 50%", df)
	if ww != 500 || wc != 250 {
		t.Errorf("0 → 50%%: got centre=%v width=%v, want 250/500", wc, ww)
	}
	// Lower bound of the window must be 0.
	if lower := wc - ww/2; lower != 0 {
		t.Errorf("0 → 50%%: window lower bound got %v, want 0", lower)
	}
	wc, ww = resolvePreset(petPresets, "0 → 20%", df)
	if ww != 200 || wc != 100 {
		t.Errorf("0 → 20%%: got centre=%v width=%v, want 100/200", wc, ww)
	}
}

func TestResolvePresetMR(t *testing.T) {
	// MR contrast presets scale the frame's own width, keeping the centre.
	df := &decodedFrame{wc: 300, ww: 400, lo: 0, hi: 1200}

	if wc, ww := resolvePreset(mrPresets, "Lower contrast", df); wc != 300 || ww != 800 {
		t.Errorf("Lower contrast: got %v/%v, want 300/800", wc, ww)
	}
	if wc, ww := resolvePreset(mrPresets, "Higher contrast", df); wc != 300 || ww != 200 {
		t.Errorf("Higher contrast: got %v/%v, want 300/200", wc, ww)
	}
	if wc, ww := resolvePreset(mrPresets, "Highest contrast", df); wc != 300 || ww != 100 {
		t.Errorf("Highest contrast: got %v/%v, want 300/100", wc, ww)
	}
}

func TestResolvePresetFullRangeDegenerate(t *testing.T) {
	// lo == hi (flat image): width must be clamped to at least 1.
	df := &decodedFrame{lo: 500, hi: 500}
	if _, ww := resolvePreset(ctPresets, "Full range", df); ww < 1 {
		t.Errorf("degenerate full range width should be >= 1, got %v", ww)
	}
}

func TestPresetsForModality(t *testing.T) {
	cases := []struct {
		mod      string
		wantLast string // last preset name in the expected list
	}{
		{"CT", ctPresets[len(ctPresets)-1].name},
		{"ct", ctPresets[len(ctPresets)-1].name}, // case-insensitive
		{"PT", petPresets[len(petPresets)-1].name},
		{"MR", mrPresets[len(mrPresets)-1].name},
		{"US", genericPresets[len(genericPresets)-1].name}, // unknown → generic
		{"", genericPresets[len(genericPresets)-1].name},
	}
	for _, c := range cases {
		got := presetsForModality(c.mod)
		if len(got) == 0 || got[len(got)-1].name != c.wantLast {
			t.Errorf("presetsForModality(%q): got last=%q, want %q", c.mod, got[len(got)-1].name, c.wantLast)
		}
	}
}

// ── clamp helpers ────────────────────────────────────────────────────────────

func TestClampInt(t *testing.T) {
	cases := []struct{ v, lo, hi, want int }{
		{5, 0, 10, 5},
		{-3, 0, 10, 0},
		{20, 0, 10, 10},
		{0, 0, 0, 0},
	}
	for _, c := range cases {
		if got := clampInt(c.v, c.lo, c.hi); got != c.want {
			t.Errorf("clampInt(%d,%d,%d)=%d, want %d", c.v, c.lo, c.hi, got, c.want)
		}
	}
}

func TestClampFloat(t *testing.T) {
	if got := clampFloat(2.0, 1, 16); got != 2.0 {
		t.Errorf("got %v, want 2.0", got)
	}
	if got := clampFloat(0.5, 1, 16); got != 1.0 {
		t.Errorf("zoom floor: got %v, want 1.0", got)
	}
	if got := clampFloat(99, 1, 16); got != 16.0 {
		t.Errorf("zoom ceil: got %v, want 16.0", got)
	}
}

// ── colour maps ──────────────────────────────────────────────────────────────

func TestGrayscaleMapIsIdentity(t *testing.T) {
	for i := 0; i < 256; i++ {
		c := grayscaleMap.lut[i]
		if c[0] != uint8(i) || c[1] != uint8(i) || c[2] != uint8(i) {
			t.Fatalf("grayscale[%d] = %v, want {%d,%d,%d}", i, c, i, i, i)
		}
	}
}

func TestInverseGrayMapIsInverted(t *testing.T) {
	for i := 0; i < 256; i++ {
		c := inverseGrayMap.lut[i]
		w := uint8(255 - i)
		if c[0] != w || c[1] != w || c[2] != w {
			t.Fatalf("inverse[%d] = %v, want {%d,%d,%d}", i, c, w, w, w)
		}
	}
}

func TestColorMapEndpoints(t *testing.T) {
	// Every continuous map starts at black and ends at white. (The quantised
	// PET 20 Step samples band centres, so its endpoints are not pure black/white.)
	for _, m := range []colorMap{hotIronMap, petMap, hotMetalBlueMap} {
		if m.lut[0] != [3]uint8{0, 0, 0} {
			t.Errorf("%s: lut[0] = %v, want black", m.name, m.lut[0])
		}
		if m.lut[255] != [3]uint8{255, 255, 255} {
			t.Errorf("%s: lut[255] = %v, want white", m.name, m.lut[255])
		}
	}
}

func TestColorMapByNameFallsBackToGrayscale(t *testing.T) {
	if colorMapByName("Hot Iron").name != "Hot Iron" {
		t.Error("expected Hot Iron by name")
	}
	if colorMapByName("nonexistent").name != "Grayscale" {
		t.Error("unknown map should fall back to Grayscale")
	}
}

func TestDefaultColorMapForModality(t *testing.T) {
	cases := map[string]string{
		"PT": "Hot Iron",
		"pt": "Hot Iron",
		"NM": "Hot Iron",
		"CT": "Grayscale",
		"MR": "Grayscale",
		"":   "Grayscale",
	}
	for mod, want := range cases {
		if got := defaultColorMapForModality(mod); got != want {
			t.Errorf("defaultColorMapForModality(%q) = %q, want %q", mod, got, want)
		}
	}
}

func TestIsJPEG2000TransferSyntax(t *testing.T) {
	yes := []string{"1.2.840.10008.1.2.4.90", "1.2.840.10008.1.2.4.91", " 1.2.840.10008.1.2.4.90 "}
	for _, ts := range yes {
		if !isJPEG2000TransferSyntax(ts) {
			t.Errorf("isJPEG2000TransferSyntax(%q) = false, want true", ts)
		}
	}
	no := []string{"1.2.840.10008.1.2.4.50", "1.2.840.10008.1.2.4.80", "1.2.840.10008.1.2.1", ""}
	for _, ts := range no {
		if isJPEG2000TransferSyntax(ts) {
			t.Errorf("isJPEG2000TransferSyntax(%q) = true, want false", ts)
		}
	}
}

func TestPet20MapHasSteps(t *testing.T) {
	// A quantised map must have far fewer distinct colours than a continuous one.
	distinct := map[[3]uint8]bool{}
	for i := 0; i < 256; i++ {
		distinct[pet20Map.lut[i]] = true
	}
	if len(distinct) > 20 {
		t.Errorf("PET 20 Step has %d distinct colours, want <= 20", len(distinct))
	}
}
