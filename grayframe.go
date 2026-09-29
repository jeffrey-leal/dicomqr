package main

import (
	"encoding/binary"
	"image"
	"slices"
	"sync"
)

// The greyscale sample store and the window/level lookup table.
//
// Every greyscale decoder computes a pixel's display value the same way: the
// stored integer, rescaled — float32(float64(stored)*slope + intercept). The
// viewer used to hold that float32 per pixel and redo the windowing arithmetic
// for every pixel on every window/level change: a subtract, a divide, a
// multiply and a clamp per pixel, on the UI goroutine, at pointer rate during a
// drag — about 1 ms for a 512×512 slice and tens of milliseconds for a large
// CR or mammogram.
//
// A frame's stored values almost always span far fewer than 65,536 values (8
// to 16 bits stored), so the frame keeps each pixel as a 16-bit offset into
// that range instead (rawIdx), and windowing becomes: build one table over the
// range — one entry per possible stored value, each computed with exactly the
// per-pixel expression above — then look every pixel up. The output cannot
// differ from the float path, because every table entry is that float path
// evaluated on the same float32. It also halves a greyscale frame's memory
// (2 bytes a pixel rather than 4), which doubles what the clip buffer's budget
// holds of an XA or SPECT clip.
//
// A frame whose stored values span more than 65,536 (32-bit data, or a
// pathological J2K precision) keeps the float32 form (gray), windowed per pixel
// as before.

// storedSample is every integer type a decoder hands over stored values in.
type storedSample interface {
	~uint8 | ~uint16 | ~int16 | ~int32 | ~int64
}

// newGrayFrame builds a greyscale frame from stored integer values, choosing
// the indexed form when the values' range allows it. The caller applies any
// sign extension first and runs computeDefaultWindow afterwards, as it did when
// it filled gray itself.
//
// samples shorter than rows*cols (a truncated frame) take the float form, with
// the missing pixels zero — which is what the decoders produced before.
func newGrayFrame[T storedSample](rows, cols int, samples []T, slope, intercept float64, invert bool) *decodedFrame {
	n := rows * cols
	df := &decodedFrame{rows: rows, cols: cols, invert: invert, slope: slope, intercept: intercept}
	if n <= 0 {
		df.gray = []float32{}
		return df
	}
	if len(samples) >= n {
		samples = samples[:n]
		lo, hi := int64(samples[0]), int64(samples[0])
		for _, s := range samples {
			v := int64(s)
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
		}
		if hi-lo < 1<<16 {
			idx := make([]uint16, n)
			for i, s := range samples {
				idx[i] = uint16(int64(s) - lo)
			}
			df.rawIdx, df.rawBase, df.rawSpan = idx, lo, int(hi-lo+1)
			return df
		}
	}
	gray := make([]float32, n)
	for i, s := range samples[:min(len(samples), n)] {
		gray[i] = float32(float64(s)*slope + intercept)
	}
	df.gray = gray
	return df
}

// hasSamples reports whether the frame holds greyscale samples in either form.
func (d *decodedFrame) hasSamples() bool { return d.rawIdx != nil || d.gray != nil }

// rescaled is the display value of the k-th stored value of an indexed frame —
// the exact expression every decoder used to fill gray with.
func (d *decodedFrame) rescaled(k int) float32 {
	return float32(float64(d.rawBase+int64(k))*d.slope + d.intercept)
}

// sampleBytes is the memory the frame's samples occupy.
func (d *decodedFrame) sampleBytes() int {
	return len(d.rawIdx)*2 + len(d.gray)*4
}

// windowLUTPool recycles the lookup table, which is rebuilt on every
// window/level change — at pointer rate during a drag — and would otherwise be
// up to 256 KB of garbage per tick.
var windowLUTPool = sync.Pool{New: func() any { s := make([]uint32, 0, 4096); return &s }}

// renderIndexed windows an indexed frame into dst through a lookup table: see
// the top of this file. Each entry is packed RGBA (little-endian, so it stores
// as R, G, B, A), with opaque alpha as renderInto writes.
func (d *decodedFrame) renderIndexed(dst *image.RGBA, cm *colorMap, wc, ww float64) {
	lower := wc - ww/2
	inv := d.invert
	lp := windowLUTPool.Get().(*[]uint32)
	lut := slices.Grow((*lp)[:0], d.rawSpan)[:d.rawSpan]
	for k := range lut {
		idx := clampToUint8((float64(d.rescaled(k)) - lower) / ww * 255)
		if inv {
			idx = 255 - idx
		}
		c := cm.lut[idx]
		lut[k] = uint32(c[0]) | uint32(c[1])<<8 | uint32(c[2])<<16 | 0xFF<<24
	}
	pix := dst.Pix
	for i, k := range d.rawIdx {
		binary.LittleEndian.PutUint32(pix[i*4:], lut[k])
	}
	*lp = lut
	windowLUTPool.Put(lp)
}

// grayRange is the smallest and largest display value in the frame.
func (d *decodedFrame) grayRange() (lo, hi float64, ok bool) {
	if d.rawIdx != nil {
		if len(d.rawIdx) == 0 {
			return 0, 0, false
		}
		// rescaled is monotone in k (non-decreasing for slope ≥ 0, non-
		// increasing otherwise), so the extremes sit at the range's two ends —
		// and the range is exactly the frame's min..max stored value.
		a, b := float64(d.rescaled(0)), float64(d.rescaled(d.rawSpan-1))
		return min(a, b), max(a, b), true
	}
	if len(d.gray) == 0 {
		return 0, 0, false
	}
	lo, hi = float64(d.gray[0]), float64(d.gray[0])
	for _, v := range d.gray {
		f := float64(v)
		lo, hi = min(lo, f), max(hi, f)
	}
	return lo, hi, true
}

// percentiles returns the display values at ranks n/100 and n*99/100 of the
// frame's sorted pixels — the order statistics computeDefaultWindow's 1st–99th
// percentile window is built from.
//
// It used to copy every pixel into a []float64 and sort it: for a 9-megapixel
// image, a 72 MB allocation and about a second, on every frame the clip buffer
// decoded without window tags (common for ultrasound, NM and some DX). An
// indexed frame answers from a histogram of stored values, walked in display
// order — ascending stored value, or descending when the slope is negative —
// which yields exactly the same order statistics, since rescaled is monotone. A
// float frame sorts a float32 copy: half the memory, and the same values, since
// widening float32 to float64 is exact and preserves order.
func (d *decodedFrame) percentiles() (plo, phi float64) {
	if d.rawIdx != nil {
		n := len(d.rawIdx)
		counts := make([]uint32, d.rawSpan)
		for _, k := range d.rawIdx {
			counts[k]++
		}
		rLo, rHi := n/100, (n*99)/100
		var seen int
		found := 0
		visit := func(k int) bool {
			c := int(counts[k])
			if c == 0 {
				return false
			}
			if found == 0 && rLo < seen+c {
				plo = float64(d.rescaled(k))
				found = 1
			}
			if found == 1 && rHi < seen+c {
				phi = float64(d.rescaled(k))
				return true
			}
			seen += c
			return false
		}
		if d.slope >= 0 {
			for k := 0; k < d.rawSpan && !visit(k); k++ {
			}
		} else {
			for k := d.rawSpan - 1; k >= 0 && !visit(k); k-- {
			}
		}
		return plo, phi
	}
	cp := slices.Clone(d.gray)
	slices.Sort(cp)
	n := len(cp)
	return float64(cp[n/100]), float64(cp[(n*99)/100])
}
