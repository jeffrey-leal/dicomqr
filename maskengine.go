package main

// Pixel masking — blanking burned-in PHI in the exported copy of an image.
//
// This is the only part of the application that writes pixel values. It works
// on native (uncompressed) pixel data only — the caller must have decompressed
// first. processFile does exactly that, which is why masking runs after the
// transfer-syntax conversion rather than before it; afterwards the masked
// frames are re-encoded — back into a lossless source's own syntax, or into
// JPEG 2000 Lossless for a lossy source (recompress.go) — so decompression
// here no longer dictates what the export looks like.
//
// The load-bearing rule: if masking was asked for and cannot be carried out on
// a file that has pixel data, the file must fail. Exporting it unmasked would
// disclose the very thing the profile was written to remove, and nothing later
// in the pipeline would notice.

import (
	"errors"
	"fmt"
	"strings"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// pixelSample is the set of element types suyashkumar's native frames use for
// their raw sample slice, keyed by BitsAllocated (plus the int variant its
// fallback path builds).
type pixelSample interface {
	~uint8 | ~uint16 | ~uint32 | ~int
}

// maskingApplies reports whether any region resolves to a rectangle for this
// file, judged from the header alone — before a single frame is decoded.
//
// It exists purely to spare files that nothing applies to the cost of being
// decompressed and rewritten uncompressed, which for a profile masking a
// handful of images is nearly every file in the study. It is deliberately
// generous: anything it cannot settle from Rows and Columns returns true, and
// the authoritative per-frame resolution inside applyPixelMask decides. A file
// that cannot be masked at all also returns true, so the failure is reported
// from the one place that reports it rather than being quietly skipped here.
func maskingApplies(ds *sdicom.Dataset, regions []MaskRegion, src maskSource) bool {
	cols := datasetInt(ds, tag.Columns, 0)
	rows := datasetInt(ds, tag.Rows, 0)
	if cols <= 0 || rows <= 0 {
		return true
	}
	res, err := maskRects(src, regions, cols, rows)
	if err != nil {
		return true
	}
	return len(res.rects) > 0
}

// maskOutcome reports what masking one file did.
type maskOutcome struct {
	masked bool // at least one pixel was written
	// usFellBack: an ultrasound rule could not be resolved and the profile's
	// manual rectangles masked the file instead (see maskRects).
	usFellBack bool
}

// applyPixelMask blanks every region in ds's pixel data. A dataset with no
// pixel data is not an error — a report or key-object selection has no
// burned-in annotation to remove — but encapsulated pixel data is, because the
// caller was supposed to decompress it.
// src is the file's masking identity captured *before* the modification
// pipeline ran — see maskSource. Passing the live dataset instead would resolve
// scopes against remapped UIDs and removed attributes, which silently masks
// nothing.
func applyPixelMask(ds *sdicom.Dataset, regions []MaskRegion, src maskSource) (maskOutcome, error) {
	var out maskOutcome
	if len(regions) == 0 {
		return out, nil
	}
	pdElem, err := ds.FindElementByTag(tag.PixelData)
	if err != nil {
		return out, nil
	}
	info, ok := pdElem.Value.GetValue().(sdicom.PixelDataInfo)
	if !ok {
		return out, fmt.Errorf("unexpected PixelData value type %T", pdElem.Value.GetValue())
	}
	if info.IsEncapsulated {
		return out, errors.New("pixel data is still compressed — it must be decompressed before masking")
	}
	if len(info.Frames) == 0 {
		return out, nil
	}

	photometric := datasetFirstString(ds, tag.PhotometricInterpretation)
	bitsAlloc := datasetInt(ds, tag.BitsAllocated, 8)
	bitsStored := datasetInt(ds, tag.BitsStored, bitsAlloc)
	signed := datasetInt(ds, tag.PixelRepresentation, 0) == 1
	planar := datasetInt(ds, tag.PlanarConfiguration, 0) == 1

	for i, fr := range info.Frames {
		if fr == nil {
			continue
		}
		if fr.IsEncapsulated() {
			return out, fmt.Errorf("frame %d is still compressed", i+1)
		}
		nf, nerr := fr.GetNativeFrame()
		if nerr != nil {
			return out, fmt.Errorf("frame %d: %w", i+1, nerr)
		}
		cols, rows, spp := nf.Cols(), nf.Rows(), nf.SamplesPerPixel()

		fill, ferr := maskFillSamples(ds, photometric, spp, bitsStored, signed)
		if ferr != nil {
			return out, ferr
		}
		// Geometry is resolved per frame rather than once: the dimensions come
		// from the frame itself, so a file whose frames disagree with Rows and
		// Columns still masks the right pixels.
		res, rerr := maskRects(src, regions, cols, rows)
		if rerr != nil {
			return out, rerr
		}
		out.usFellBack = out.usFellBack || res.usFellBack
		if len(res.rects) == 0 {
			continue
		}

		var werr error
		switch raw := nf.RawDataSlice().(type) {
		case []uint8:
			werr = fillRects(raw, cols, rows, spp, planar, res.rects, fill)
		case []uint16:
			werr = fillRects(raw, cols, rows, spp, planar, res.rects, fill)
		case []uint32:
			werr = fillRects(raw, cols, rows, spp, planar, res.rects, fill)
		case []int:
			werr = fillRects(raw, cols, rows, spp, planar, res.rects, fill)
		default:
			werr = fmt.Errorf("unsupported pixel sample type %T", raw)
		}
		if werr != nil {
			return out, fmt.Errorf("frame %d: %w", i+1, werr)
		}
		out.masked = true
	}
	return out, nil
}

// fillRects writes fill into every pixel of every rectangle. The frame's raw
// slice is written in place, and the writer reads that same slice back, so no
// element has to be rebuilt.
//
// Sample layout follows PlanarConfiguration. The parsing library fills the raw
// slice sequentially from the stream, so for planar data the slice is
// plane-major exactly as stored — the index arithmetic here is what makes that
// case correct rather than a colour-shifted smear.
func fillRects[I pixelSample](raw []I, cols, rows, spp int, planar bool, rects []pixelRect, fill []uint64) error {
	if spp <= 0 {
		return fmt.Errorf("samples per pixel %d", spp)
	}
	if len(fill) != spp {
		return fmt.Errorf("%d fill samples for %d samples per pixel", len(fill), spp)
	}
	if want := cols * rows * spp; len(raw) < want {
		return fmt.Errorf("pixel buffer holds %d samples, need %d", len(raw), want)
	}
	plane := cols * rows
	for _, rect := range rects {
		for y := rect.y0; y < rect.y1; y++ {
			for x := rect.x0; x < rect.x1; x++ {
				for s := 0; s < spp; s++ {
					var idx int
					if planar {
						idx = s*plane + y*cols + x
					} else {
						idx = (y*cols+x)*spp + s
					}
					raw[idx] = I(fill[s])
				}
			}
		}
	}
	return nil
}

// maskFillSamples is the value written into a masked pixel: black, expressed in
// whatever the file's photometric interpretation calls black. A constant 0
// would paint a white box on a MONOCHROME1 image and a green one on YBR — both
// of which read as an intact annotation rather than a redaction.
func maskFillSamples(ds *sdicom.Dataset, photometric string, spp, bitsStored int, signed bool) ([]uint64, error) {
	if bitsStored <= 0 || bitsStored > 32 {
		return nil, fmt.Errorf("bits stored %d", bitsStored)
	}
	zeros := make([]uint64, spp)

	switch pi := strings.ToUpper(strings.TrimSpace(photometric)); pi {
	case "MONOCHROME1":
		// Inverted greyscale: the maximum stored value displays as black.
		var maxVal uint64
		if signed {
			maxVal = (uint64(1) << (bitsStored - 1)) - 1
		} else {
			maxVal = (uint64(1) << bitsStored) - 1
		}
		for i := range zeros {
			zeros[i] = maxVal
		}
		return zeros, nil

	case "YBR_FULL":
		if spp != 3 {
			return nil, fmt.Errorf("%s with %d samples per pixel", pi, spp)
		}
		half := uint64(1) << (bitsStored - 1)
		return []uint64{0, half, half}, nil

	case "YBR_FULL_422", "YBR_PARTIAL_422", "YBR_PARTIAL_420":
		// Chroma is subsampled, so the raw slice is not one triple per pixel
		// and the index arithmetic above does not describe it. Rather than
		// smear colour across the image, refuse: converting the export to an
		// uncompressed syntax rewrites such data as RGB, which masks fine.
		return nil, fmt.Errorf("photometric interpretation %s is chroma-subsampled and cannot be masked directly — "+
			"set an output transfer syntax so the export is written as RGB", pi)

	case "PALETTE COLOR":
		if spp != 1 {
			return nil, fmt.Errorf("%s with %d samples per pixel", pi, spp)
		}
		return []uint64{paletteBlackIndex(ds)}, nil

	default:
		// MONOCHROME2, RGB, and the reversible/irreversible YBR variants a
		// decompressed frame carries all have black at zero. An unrecognised
		// interpretation also lands here: zero may not be black in it, but the
		// pixels are still overwritten, which is what matters.
		return zeros, nil
	}
}

// paletteBlackIndex finds the palette entry that displays darkest, so a masked
// rectangle in a PALETTE COLOR image reads as a redaction rather than as a
// block of whatever colour index 0 happens to hold. Falls back to the first
// stored value when the lookup tables cannot be read.
func paletteBlackIndex(ds *sdicom.Dataset) uint64 {
	first := 0
	if desc := datasetInts(ds, tag.RedPaletteColorLookupTableDescriptor); len(desc) >= 2 {
		first = desc[1]
	}
	r := datasetInts(ds, tag.RedPaletteColorLookupTableData)
	g := datasetInts(ds, tag.GreenPaletteColorLookupTableData)
	b := datasetInts(ds, tag.BluePaletteColorLookupTableData)
	n := min(len(r), len(g), len(b))
	if n == 0 {
		return uint64(max(first, 0))
	}
	darkest, best := 0, -1
	for i := range n {
		// Rec. 601 luma, integer weights — only the ordering matters.
		lum := 299*r[i] + 587*g[i] + 114*b[i]
		if best < 0 || lum < best {
			darkest, best = i, lum
		}
	}
	return uint64(max(first+darkest, 0))
}

// datasetFirstString returns the first string value of t, or "".
func datasetFirstString(ds *sdicom.Dataset, t tag.Tag) string {
	if v := datasetStrings(ds, t); len(v) > 0 {
		return v[0]
	}
	return ""
}

// datasetInts returns t's integer values, or nil when it is absent or holds
// something else. Palette lookup tables are the reason this exists: they are
// long integer arrays rather than the single values datasetInt reads.
func datasetInts(ds *sdicom.Dataset, t tag.Tag) []int {
	elem, err := ds.FindElementByTag(t)
	if err != nil {
		return nil
	}
	v, ok := elem.Value.GetValue().([]int)
	if !ok {
		return nil
	}
	return v
}
