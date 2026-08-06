package main

// Pixel masking regions — where burned-in PHI sits in an image, expressed so
// that one profile can be applied to a whole study.
//
// Geometry is stored as fractions of the image (0.0–1.0), never as pixel
// coordinates: a single study routinely mixes image sizes (an 800×600 echo
// loop beside a 1024×768 secondary capture), and a pixel rectangle measured on
// one of them silently misses the banner on the other. Fractions resolve
// against each frame's own dimensions.
//
// Rounding is deliberately outward — the start of a rectangle floors, the end
// ceils — because half a pixel row of leftover text is still PHI. Masking
// slightly more than asked is a cosmetic cost; masking slightly less is a
// disclosure.

import (
	"errors"
	"fmt"
	"math"
	"strings"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// Mask modes. An unrecognised mode is always an error rather than a rule that
// quietly does nothing: a mask that fails to apply is indistinguishable, in
// the exported file, from a profile that never asked for one.
const (
	// maskModeRect blanks one fractional rectangle.
	maskModeRect = "rect"
	// maskModeOutsideUS blanks everything outside the calibrated ultrasound
	// region(s) the file itself declares, which is where a vendor's patient
	// banner lives. The region geometry varies by vendor and preset; the
	// calibration sequence does not, so this generalises where a hand-drawn
	// rectangle does not.
	maskModeOutsideUS = "outside-us-regions"
	// maskModeNone masks nothing. It exists so that "these images were looked
	// at and need no mask" can be stated rather than assumed: an ultrasound
	// image with no calibrated region and no rectangle otherwise fails the
	// export, which is right when nobody has looked and wrong once somebody
	// has. Only meaningful with a scope.
	maskModeNone = "none"
)

// MaskScope narrows which images a region applies to. An empty field matches
// anything; every stated field must match. A region with no scope applies to
// every image, which is what a rectangle defined in the profile editor means.
//
// This exists because a rectangle drawn on one image is not a statement about
// every image: an ultrasound study's analysis screens are laid out differently
// from each other, and blanking the same area on all of them destroys the
// report content the export was made to keep.
type MaskScope struct {
	// SOPInstanceUID restricts a region to exactly one image.
	SOPInstanceUID string `json:"sopinstance,omitempty"`
	// Modality, Cols and Rows restrict it to one group of like images.
	Modality string `json:"modality,omitempty"`
	Cols     int    `json:"cols,omitempty"`
	Rows     int    `json:"rows,omitempty"`
	// USRegion is "declared" or "absent" to restrict a region to ultrasound
	// images that do or do not carry a calibration sequence — the same split
	// the review window groups by, so "this group" means what it appears to.
	USRegion string `json:"usregion,omitempty"`
}

// USRegion tokens.
const (
	usRegionDeclared = "declared"
	usRegionAbsent   = "absent"
)

// maskSource is everything mask resolution needs to know about a file *as it
// arrived*, captured before the modification pipeline runs.
//
// Masking is the last step, and the steps before it rewrite the very things a
// mask is keyed on: UID remapping replaces SOP Instance UID, and a removal rule
// can delete Modality or the ultrasound region sequence. Resolving against the
// live dataset therefore meant a region scoped to one image stopped matching
// the moment the profile remapped UIDs — silently, since a scope that matches
// nothing simply masks nothing. The shipped base-deident profile remaps UIDs,
// so this was every run made with it.
type maskSource struct {
	sopInstanceUID string
	modality       string
	usBounds       pixelRect
	usDeclared     bool
}

// newMaskSource snapshots a dataset's masking identity. Call it on the file as
// parsed, before anything modifies it.
func newMaskSource(ds *sdicom.Dataset) maskSource {
	src := maskSource{
		sopInstanceUID: strings.TrimSpace(datasetFirstString(ds, tag.SOPInstanceUID)),
		modality:       strings.TrimSpace(datasetFirstString(ds, tag.Modality)),
	}
	src.usBounds, src.usDeclared = ultrasoundRegionBounds(ds)
	return src
}

// isUltrasound reports whether the source file declared Modality US.
func (s maskSource) isUltrasound() bool { return strings.EqualFold(s.modality, "US") }

// matches reports whether s admits this image.
func (s *MaskScope) matches(src maskSource, cols, rows int) bool {
	if s == nil {
		return true
	}
	if s.SOPInstanceUID != "" && !strings.EqualFold(src.sopInstanceUID, s.SOPInstanceUID) {
		return false
	}
	if s.Modality != "" && !strings.EqualFold(src.modality, s.Modality) {
		return false
	}
	if s.Cols != 0 && s.Cols != cols {
		return false
	}
	if s.Rows != 0 && s.Rows != rows {
		return false
	}
	if s.USRegion != "" {
		if (s.USRegion == usRegionDeclared) != src.usDeclared {
			return false
		}
	}
	return true
}

// describe renders a scope for a list row or a log line.
func (s *MaskScope) describe() string {
	if s == nil {
		return "all images"
	}
	if s.SOPInstanceUID != "" {
		return "this image only"
	}
	var parts []string
	if s.Modality != "" {
		parts = append(parts, s.Modality)
	}
	if s.Cols != 0 && s.Rows != 0 {
		parts = append(parts, fmt.Sprintf("%d × %d", s.Cols, s.Rows))
	}
	if s.USRegion == usRegionAbsent {
		parts = append(parts, "no calibrated region")
	}
	if len(parts) == 0 {
		return "all images"
	}
	return strings.Join(parts, " ")
}

// MaskRegion is one masking rule in a modification profile. It is a
// dicomqr-only profile field (dicomtool ignores `maskregions`, and drops it
// when it saves), and it replaces dicomtool's `maskrows`, which only ever
// masked whole rows from the top of the image.
type MaskRegion struct {
	// Mode is one of the maskMode* constants. Empty means maskModeRect, so a
	// hand-written rule carrying only geometry still parses.
	Mode string `json:"mode,omitempty"`

	// X, Y, W, H are fractions of the image width/height in [0,1], used by
	// maskModeRect only. Origin is the top-left corner.
	X float64 `json:"x,omitempty"`
	Y float64 `json:"y,omitempty"`
	W float64 `json:"w,omitempty"`
	H float64 `json:"h,omitempty"`

	// AppliesTo restricts the region to some of the images rather than all of
	// them. Nil means every image, which is what a profile-level rule is.
	AppliesTo *MaskScope `json:"appliesto,omitempty"`
}

// maskRegionMode normalises r's mode for comparison.
func maskRegionMode(r MaskRegion) string {
	m := strings.ToLower(strings.TrimSpace(r.Mode))
	if m == "" {
		return maskModeRect
	}
	return m
}

// validateMaskRegions checks every region, reporting the 1-based index of the
// first bad one so a profiles.json entry can be found by eye.
func validateMaskRegions(regions []MaskRegion) error {
	for i, r := range regions {
		if err := validateMaskRegion(r); err != nil {
			return fmt.Errorf("mask region %d: %w", i+1, err)
		}
	}
	return nil
}

// validateMaskRegion checks one region's mode and geometry.
func validateMaskRegion(r MaskRegion) error {
	switch maskRegionMode(r) {
	case maskModeRect:
		if err := validateMaskScope(r.AppliesTo); err != nil {
			return err
		}
		for _, f := range []struct {
			name string
			v    float64
		}{{"x", r.X}, {"y", r.Y}, {"w", r.W}, {"h", r.H}} {
			if math.IsNaN(f.v) || math.IsInf(f.v, 0) {
				return fmt.Errorf("%s is not a number", f.name)
			}
			if f.v < 0 || f.v > 1 {
				return fmt.Errorf("%s = %g: fractions of the image must be between 0 and 1", f.name, f.v)
			}
		}
		if r.W <= 0 || r.H <= 0 {
			return fmt.Errorf("w and h must be greater than 0 (got %g × %g) — a rectangle of no size masks nothing", r.W, r.H)
		}
		// A rectangle running off the edge is a typo worth reporting rather
		// than clamping silently: the author meant some other geometry.
		if r.X+r.W > 1+maskFractionEpsilon || r.Y+r.H > 1+maskFractionEpsilon {
			return fmt.Errorf("rectangle extends past the image (x+w = %g, y+h = %g)", r.X+r.W, r.Y+r.H)
		}
		return nil
	case maskModeOutsideUS, maskModeNone:
		if r.X != 0 || r.Y != 0 || r.W != 0 || r.H != 0 {
			return fmt.Errorf("%s takes no x/y/w/h", maskRegionMode(r))
		}
		return validateMaskScope(r.AppliesTo)
	default:
		return fmt.Errorf("mode %q: must be %s, %s or %s",
			r.Mode, maskModeRect, maskModeOutsideUS, maskModeNone)
	}
}

// validateMaskScope checks the one field of a scope that has a fixed
// vocabulary. An unrecognised token would silently match nothing, which for a
// masking rule means silently masking nothing.
func validateMaskScope(s *MaskScope) error {
	if s == nil {
		return nil
	}
	switch s.USRegion {
	case "", usRegionDeclared, usRegionAbsent:
	default:
		return fmt.Errorf("usregion %q: must be %s or %s", s.USRegion, usRegionDeclared, usRegionAbsent)
	}
	if s.Cols < 0 || s.Rows < 0 {
		return fmt.Errorf("cols/rows must not be negative (got %d × %d)", s.Cols, s.Rows)
	}
	return nil
}

// maskFractionEpsilon absorbs the rounding of a fraction that was computed
// from pixel coordinates (the region picker divides one integer by another),
// so 0.9999999999999999 + 0.0000000000000001 does not read as out of bounds.
const maskFractionEpsilon = 1e-9

// maskRegionSummary renders a region as one line for a UI list or a log entry.
// The scope is always stated: a rectangle's reach is as much a part of what it
// does as its geometry, and the two read wrongly apart.
func maskRegionSummary(r MaskRegion) string {
	var what string
	switch maskRegionMode(r) {
	case maskModeOutsideUS:
		what = "Outside the ultrasound region"
	case maskModeNone:
		what = "No masking needed"
	default:
		what = fmt.Sprintf("Rectangle  x %.1f%%  y %.1f%%  w %.1f%%  h %.1f%%",
			r.X*100, r.Y*100, r.W*100, r.H*100)
	}
	return what + "  —  " + r.AppliesTo.describe()
}

// pixelRect is a half-open rectangle in pixels: columns [x0,x1) and rows
// [y0,y1).
type pixelRect struct{ x0, y0, x1, y1 int }

// empty reports whether the rectangle covers no pixels.
func (r pixelRect) empty() bool { return r.x1 <= r.x0 || r.y1 <= r.y0 }

// maskResolution is what a profile's regions resolve to against one frame.
type maskResolution struct {
	rects []pixelRect
	// usFellBack records that an ultrasound rule could not be resolved — the
	// file declares no calibrated region — and the profile's manual rectangles
	// masked the file instead. Reported per run: the file was masked by generic
	// geometry rather than by its own stated layout, which is a weaker claim
	// than the profile otherwise makes.
	usFellBack bool
}

// maskRects resolves every region against one frame's dimensions. Regions that
// resolve to nothing are dropped; a region that cannot be resolved at all is an
// error, so the caller can fail the file rather than export it unmasked.
//
// The one recoverable case is an ultrasound image with no calibrated region.
// Those are typically analysis or measurement screens rather than image
// captures — content worth keeping in the export — so rather than failing them
// outright, the profile's manual rectangles stand in when it has any. With no
// manual rectangle there is nothing to fall back on and the file still fails:
// exporting an unmasked banner is the one outcome never worth reaching for.
func maskRects(src maskSource, regions []MaskRegion, cols, rows int) (maskResolution, error) {
	var res maskResolution
	if cols <= 0 || rows <= 0 {
		return res, fmt.Errorf("image dimensions %d×%d", cols, rows)
	}
	usUnresolved, exempt := false, false
	for i, r := range regions {
		// Scope first: a region that does not apply to this image contributes
		// nothing, not even its failure modes.
		if !r.AppliesTo.matches(src, cols, rows) {
			continue
		}
		switch maskRegionMode(r) {
		case maskModeRect:
			if rect := fractionRect(r, cols, rows); !rect.empty() {
				res.rects = append(res.rects, rect)
			}
		case maskModeNone:
			exempt = true
		case maskModeOutsideUS:
			rects, resolved, err := ultrasoundMaskRects(src, cols, rows)
			if err != nil {
				return maskResolution{}, fmt.Errorf("mask region %d: %w", i+1, err)
			}
			if !resolved {
				usUnresolved = true
				continue
			}
			res.rects = append(res.rects, rects...)
		default:
			return maskResolution{}, fmt.Errorf("mask region %d: unknown mode %q", i+1, r.Mode)
		}
	}
	if usUnresolved && len(res.rects) == 0 && !exempt {
		return maskResolution{}, errors.New("this ultrasound image declares no calibrated region and no " +
			"rectangle applies to it — draw one, or mark the image as needing no masking, or it cannot be exported")
	}
	if usUnresolved && len(res.rects) > 0 {
		res.usFellBack = true
	}
	return res, nil
}

// fractionRect converts a fractional rectangle to pixels, rounding outward and
// clamping to the frame.
func fractionRect(r MaskRegion, cols, rows int) pixelRect {
	return pixelRect{
		x0: clampInt(int(math.Floor(r.X*float64(cols))), 0, cols),
		y0: clampInt(int(math.Floor(r.Y*float64(rows))), 0, rows),
		x1: clampInt(int(math.Ceil((r.X+r.W)*float64(cols))), 0, cols),
		y1: clampInt(int(math.Ceil((r.Y+r.H)*float64(rows))), 0, rows),
	}
}

// ultrasoundMaskRects returns the bands outside the file's calibrated
// ultrasound region — up to four rectangles around it.
//
// resolved is false when the rule does not apply to this file at all: either
// the file is not ultrasound (a CT in the same run is simply not what the rule
// is about, and failing it would push users towards turning masking off), or it
// is ultrasound but declares no calibrated region. maskRects distinguishes the
// two — the second is the case that falls back to the manual rectangles.
func ultrasoundMaskRects(src maskSource, cols, rows int) (rects []pixelRect, resolved bool, err error) {
	bounds, ok := src.usBounds, src.usDeclared
	if !ok {
		// Not ultrasound: inert, and not a fallback case either.
		if !src.isUltrasound() {
			return nil, true, nil
		}
		return nil, false, nil
	}

	x0 := clampInt(bounds.x0, 0, cols)
	y0 := clampInt(bounds.y0, 0, rows)
	x1 := clampInt(bounds.x1, 0, cols)
	y1 := clampInt(bounds.y1, 0, rows)
	if x1 <= x0 || y1 <= y0 {
		return nil, false, fmt.Errorf("calibrated region %d,%d–%d,%d does not fit the %d×%d image",
			bounds.x0, bounds.y0, bounds.x1, bounds.y1, cols, rows)
	}

	bands := []pixelRect{
		{0, 0, cols, y0},    // above
		{0, y1, cols, rows}, // below
		{0, y0, x0, y1},     // left
		{x1, y0, cols, y1},  // right
	}
	out := make([]pixelRect, 0, len(bands))
	for _, b := range bands {
		if !b.empty() {
			out = append(out, b)
		}
	}
	return out, true, nil
}

// ultrasoundRegionBounds is the bounding box of every calibrated region in the
// file, as a half-open pixel rectangle.
//
// The bounding box — rather than the union of the regions themselves — is the
// conservative choice: a duplex study calibrates two side-by-side panes, and
// masking the gap between them would blank image content to no benefit. PHI
// banners sit outside every region, which the bounding box still covers.
//
// Regions of every data type count, including the colour and grey bars: they
// are part of the displayed image, and no vendor writes patient identity
// inside a calibrated region.
func ultrasoundRegionBounds(ds *sdicom.Dataset) (pixelRect, bool) {
	var (
		bounds pixelRect
		found  bool
	)
	for _, region := range sequenceItems(ds, tag.SequenceOfUltrasoundRegions) {
		minX := datasetInt(region, tag.RegionLocationMinX0, -1)
		minY := datasetInt(region, tag.RegionLocationMinY0, -1)
		maxX := datasetInt(region, tag.RegionLocationMaxX1, -1)
		maxY := datasetInt(region, tag.RegionLocationMaxY1, -1)
		if minX < 0 || minY < 0 || maxX < minX || maxY < minY {
			continue
		}
		// The stated maxima are inclusive pixel indices (PS3.3 C.8.5.5);
		// pixelRect is half-open.
		r := pixelRect{x0: minX, y0: minY, x1: maxX + 1, y1: maxY + 1}
		if !found {
			bounds, found = r, true
			continue
		}
		bounds.x0 = min(bounds.x0, r.x0)
		bounds.y0 = min(bounds.y0, r.y0)
		bounds.x1 = max(bounds.x1, r.x1)
		bounds.y1 = max(bounds.y1, r.y1)
	}
	return bounds, found
}

// isUltrasoundModality reports whether the file declares Modality US.
func isUltrasoundModality(ds *sdicom.Dataset) bool {
	for _, m := range datasetStrings(ds, tag.Modality) {
		if strings.EqualFold(strings.TrimSpace(m), "US") {
			return true
		}
	}
	return false
}
