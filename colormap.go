package main

import "strings"

// colorMap is a 256-entry RGB lookup table applied to the windowed 8-bit
// intensity, used to pseudo-colour grayscale images (notably PET/SPECT uptake).
// Grayscale is the identity table, so a single render path covers both
// grayscale and colour display.
type colorMap struct {
	name string
	lut  [256][3]uint8
}

// cmStop is a control point used to build a map by linear interpolation.
type cmStop struct {
	pos     float64 // 0..1 position along the ramp
	r, g, b uint8
}

// buildColorMap fills a 256-entry table by linearly interpolating between stops.
// stops must be sorted by pos and span 0..1.
//
// These are faithful, visually-matching renditions of the DICOM PS3.6 Annex B
// well-known palettes (Hot Iron, PET, Hot Metal Blue, PET 20 Step) generated
// from piecewise-linear ramps — not the verbatim standard byte tables. They are
// intended for display/triage; if byte-exact equivalence with another viewer is
// ever required, the published tables can be dropped in here unchanged.
func buildColorMap(name string, stops []cmStop) colorMap {
	cm := colorMap{name: name}
	for i := 0; i < 256; i++ {
		t := float64(i) / 255

		s0, s1 := stops[0], stops[len(stops)-1]
		for j := 0; j+1 < len(stops); j++ {
			if t >= stops[j].pos && t <= stops[j+1].pos {
				s0, s1 = stops[j], stops[j+1]
				break
			}
		}
		f := 0.0
		if span := s1.pos - s0.pos; span > 0 {
			f = (t - s0.pos) / span
		}
		cm.lut[i] = [3]uint8{
			lerpU8(s0.r, s1.r, f),
			lerpU8(s0.g, s1.g, f),
			lerpU8(s0.b, s1.b, f),
		}
	}
	return cm
}

func lerpU8(a, b uint8, f float64) uint8 {
	return uint8(float64(a) + (float64(b)-float64(a))*f + 0.5)
}

// quantizeColorMap collapses a continuous map into a given number of flat
// colour bands — used for the stepped PET 20 Step palette.
func quantizeColorMap(name string, src colorMap, steps int) colorMap {
	cm := colorMap{name: name}
	for i := 0; i < 256; i++ {
		band := i * steps / 256
		center := (band*256 + 128) / steps
		if center > 255 {
			center = 255
		}
		cm.lut[i] = src.lut[center]
	}
	return cm
}

func identityColorMap(name string, invert bool) colorMap {
	cm := colorMap{name: name}
	for i := 0; i < 256; i++ {
		v := uint8(i)
		if invert {
			v = uint8(255 - i)
		}
		cm.lut[i] = [3]uint8{v, v, v}
	}
	return cm
}

var (
	grayscaleMap   = identityColorMap("Grayscale", false)
	inverseGrayMap = identityColorMap("Inverse Grayscale", true)

	// Black → red → yellow → white.
	hotIronMap = buildColorMap("Hot Iron", []cmStop{
		{0.00, 0, 0, 0},
		{0.40, 255, 0, 0},
		{0.75, 255, 255, 0},
		{1.00, 255, 255, 255},
	})

	// Black → blue → purple → red → orange → yellow → white.
	petMap = buildColorMap("PET", []cmStop{
		{0.000, 0, 0, 0},
		{0.125, 0, 0, 153},
		{0.250, 76, 0, 204},
		{0.375, 153, 0, 204},
		{0.500, 204, 0, 153},
		{0.625, 255, 0, 0},
		{0.750, 255, 128, 0},
		{0.875, 255, 255, 0},
		{1.000, 255, 255, 255},
	})

	// Like PET but with blue rising earlier (cool shadows, hot highlights).
	hotMetalBlueMap = buildColorMap("Hot Metal Blue", []cmStop{
		{0.00, 0, 0, 0},
		{0.20, 0, 0, 153},
		{0.35, 102, 0, 204},
		{0.50, 178, 0, 153},
		{0.65, 230, 0, 0},
		{0.80, 255, 150, 0},
		{0.90, 255, 255, 0},
		{1.00, 255, 255, 255},
	})

	// 20 discrete colour bands sampled from the PET palette.
	pet20Map = quantizeColorMap("PET 20 Step", petMap, 20)
)

// colorMaps is the ordered list offered in the viewer's Colour dropdown.
var colorMaps = []colorMap{
	grayscaleMap,
	inverseGrayMap,
	hotIronMap,
	petMap,
	hotMetalBlueMap,
	pet20Map,
}

// colorMapNames returns the ordered map names for the dropdown.
func colorMapNames() []string {
	out := make([]string, len(colorMaps))
	for i, m := range colorMaps {
		out[i] = m.name
	}
	return out
}

// colorMapByName returns the named map, falling back to Grayscale.
func colorMapByName(name string) *colorMap {
	for i := range colorMaps {
		if colorMaps[i].name == name {
			return &colorMaps[i]
		}
	}
	return &colorMaps[0]
}

// defaultColorMapForModality returns the colour map to select automatically for
// a DICOM Modality: nuclear-medicine functional images (PET, SPECT/NM) default
// to Hot Iron; all other modalities default to Grayscale.
func defaultColorMapForModality(mod string) string {
	switch strings.ToUpper(strings.TrimSpace(mod)) {
	case "PT", "NM":
		return "Hot Iron"
	default:
		return "Grayscale"
	}
}
