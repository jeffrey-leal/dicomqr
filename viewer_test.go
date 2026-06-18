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

func TestDecodedFrameRenderWindowMapping(t *testing.T) {
	// Window centre 100, width 200 → maps [0,200] across [0,255].
	df := newTestFrame(0, 100, 200, -50, 300)
	img := df.render(100, 200).(*image.Gray)

	want := []uint8{0, 127, 255, 0, 255} // -50 clamps low, 300 clamps high
	for i, w := range want {
		if got := img.Pix[i]; got != w {
			t.Errorf("pixel %d: got %d, want %d", i, got, w)
		}
	}
}

func TestDecodedFrameRenderMonochrome1Inverts(t *testing.T) {
	df := newTestFrame(0, 100, 200)
	df.invert = true
	img := df.render(100, 200).(*image.Gray)

	// Same window as above but inverted: 0→255, 127→128, 255→0.
	want := []uint8{255, 128, 0}
	for i, w := range want {
		if got := img.Pix[i]; got != w {
			t.Errorf("pixel %d: got %d, want %d", i, got, w)
		}
	}
}

func TestDecodedFrameRenderColorIgnoresWindow(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	df := &decodedFrame{rows: 1, cols: 2, colorImg: src}
	if df.windowable() {
		t.Fatal("colour frame should not be windowable")
	}
	if got := df.render(9999, 1); got != image.Image(src) {
		t.Error("render of a colour frame should return the original image unchanged")
	}
}

func TestDecodedFrameRenderZeroWidthDoesNotPanic(t *testing.T) {
	df := newTestFrame(0, 50, 100)
	// ww < 1 is clamped to 1 internally; just assert it produces a full image.
	img := df.render(50, 0).(*image.Gray)
	if len(img.Pix) != 3 {
		t.Fatalf("expected 3 pixels, got %d", len(img.Pix))
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
