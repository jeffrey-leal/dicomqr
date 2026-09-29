package main

import (
	"bytes"
	"image"
	"math"
	"sort"
	"testing"
)

// displayValues returns every pixel's display value whichever form the frame
// holds — what tests compare against, now that gray is only one of two forms.
func (d *decodedFrame) displayValues() []float32 {
	if d.rawIdx == nil {
		return d.gray
	}
	out := make([]float32, len(d.rawIdx))
	for i, k := range d.rawIdx {
		out[i] = d.rescaled(int(k))
	}
	return out
}

// floatTwin is the same frame in the float form — the representation, and so
// the rendering path, the viewer used before the lookup table.
func floatTwin(d *decodedFrame) *decodedFrame {
	return &decodedFrame{rows: d.rows, cols: d.cols, invert: d.invert, gray: d.displayValues()}
}

// referenceDefaultWindow is computeDefaultWindow as it was before this change,
// kept verbatim as the oracle for the new range and percentile code.
func referenceDefaultWindow(vals []float32) (wc, ww, lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)
	for _, v := range vals {
		f := float64(v)
		lo, hi = math.Min(lo, f), math.Max(hi, f)
	}
	if math.IsInf(lo, 1) {
		lo, hi = 0, 0
	}
	if len(vals) > 0 && hi > lo {
		cp := make([]float64, len(vals))
		for i, v := range vals {
			cp[i] = float64(v)
		}
		sort.Float64s(cp)
		n := len(cp)
		plo, phi := cp[n/100], cp[(n*99)/100]
		if phi > plo {
			return (plo + phi) / 2, phi - plo, lo, hi
		}
	}
	if hi > lo {
		return (lo + hi) / 2, hi - lo, lo, hi
	}
	return lo, 1, lo, hi
}

// sampleFrames covers what the decoders hand newGrayFrame: 8-bit, unsigned and
// signed 16-bit, J2K-style int32, a negative slope (display order reverses the
// stored order), a fractional rescale whose float32 rounding makes neighbouring
// stored values collide, MONOCHROME1, and a skewed distribution so the 1st and
// 99th percentiles land somewhere other than the extremes.
func sampleFrames(t *testing.T) map[string]*decodedFrame {
	t.Helper()
	const rows, cols = 37, 41
	n := rows * cols
	u8 := make([]uint8, n)
	u16 := make([]uint16, n)
	s16 := make([]int16, n)
	i32 := make([]int32, n)
	for i := 0; i < n; i++ {
		u8[i] = uint8((i * 7) % 251)
		u16[i] = uint16((i*i*13 + 7) % 4096)
		s16[i] = int16((i*31)%3000 - 1024)
		i32[i] = int32(i%97*300 + 50000)
		if i%50 == 0 { // a sparse tail, so the percentiles are not the extremes
			u16[i], s16[i] = 4095, 2047
		}
	}
	return map[string]*decodedFrame{
		"uint8":                  newGrayFrame(rows, cols, u8, 1, 0, false),
		"uint16 CT rescale":      newGrayFrame(rows, cols, u16, 1, -1024, false),
		"int16 signed":           newGrayFrame(rows, cols, s16, 1, 0, false),
		"int16 negative slope":   newGrayFrame(rows, cols, s16, -2.5, 100, false),
		"uint16 fractional":      newGrayFrame(rows, cols, u16, 0.333333, 1e6, false),
		"int32 J2K":              newGrayFrame(rows, cols, i32, 0.01, 0, false),
		"uint16 MONOCHROME1":     newGrayFrame(rows, cols, u16, 1, 0, true),
		"uint8 constant (1 val)": newGrayFrame(rows, cols, make([]uint8, n), 1, 0, false),
	}
}

// The lookup table must be invisible: every frame, under every window and colour
// map, renders to exactly the bytes the per-pixel float path produces.
func TestIndexedRenderMatchesFloatRender(t *testing.T) {
	maps := []*colorMap{&grayscaleMap, colorMapByName("Hot Iron")}
	for name, df := range sampleFrames(t) {
		if df.rawIdx == nil {
			t.Fatalf("%s: took the float form; the test needs the indexed one", name)
		}
		twin := floatTwin(df)
		lo, hi, _ := df.grayRange()
		windows := [][2]float64{
			{(lo + hi) / 2, hi - lo + 1},    // the whole range
			{lo + (hi-lo)/3, (hi - lo) / 5}, // a narrow window clipping both ends
			{hi + 10, 0.5},                  // below 1: renderInto clamps the width
			{lo - 1e6, 3},                   // entirely off the data
		}
		for _, cm := range maps {
			for _, w := range windows {
				got := image.NewRGBA(image.Rect(0, 0, df.cols, df.rows))
				want := image.NewRGBA(got.Rect)
				df.renderInto(got, cm, w[0], w[1])
				twin.renderInto(want, cm, w[0], w[1])
				if !bytes.Equal(got.Pix, want.Pix) {
					t.Errorf("%s, window %v: lookup-table render differs from the float render", name, w)
				}
			}
		}
	}
}

// The default window — extremes and 1st/99th percentile — must come out exactly
// as the old sort-based computation did, from both forms.
func TestDefaultWindowMatchesReference(t *testing.T) {
	for name, df := range sampleFrames(t) {
		wc, ww, lo, hi := referenceDefaultWindow(df.displayValues())
		for form, f := range map[string]*decodedFrame{"indexed": df, "float": floatTwin(df)} {
			f.computeDefaultWindow(false, 0, 0)
			if f.wc != wc || f.ww != ww || f.lo != lo || f.hi != hi {
				t.Errorf("%s (%s): window %v/%v range %v..%v, want %v/%v range %v..%v",
					name, form, f.wc, f.ww, f.lo, f.hi, wc, ww, lo, hi)
			}
		}
	}
}

// Stored values spanning more than 16 bits cannot be indexed and keep the float
// form, with the same display values; a truncated frame does too, its missing
// pixels zero as before.
func TestNewGrayFrameFallsBackToFloat(t *testing.T) {
	wide := []int32{0, 70000, 5, -3}
	df := newGrayFrame(2, 2, wide, 0.5, 1, false)
	if df.rawIdx != nil || len(df.gray) != 4 {
		t.Fatalf("a 70,004-value range took the indexed form")
	}
	for i, v := range wide {
		if want := float32(float64(v)*0.5 + 1); df.gray[i] != want {
			t.Errorf("pixel %d = %v, want %v", i, df.gray[i], want)
		}
	}

	short := newGrayFrame(2, 2, []uint8{9, 8}, 1, 0, false)
	if short.rawIdx != nil || len(short.gray) != 4 || short.gray[0] != 9 || short.gray[3] != 0 {
		t.Errorf("truncated frame = %+v, want the float form with the missing pixels zero", short.gray)
	}
}

// The indexed form is what halves a greyscale frame's memory — and what the
// clip buffer's budget now counts.
func TestGrayFrameSampleBytes(t *testing.T) {
	df := newGrayFrame(10, 10, make([]uint16, 100), 1, 0, false)
	if got := df.sampleBytes(); got != 200 {
		t.Errorf("indexed 10×10 frame = %d sample bytes, want 200", got)
	}
	if got := frameBytes(df); got != 200+400 {
		t.Errorf("frameBytes = %d, want samples plus the RGBA it renders into (600)", got)
	}
}
