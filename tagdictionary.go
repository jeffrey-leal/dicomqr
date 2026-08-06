package main

// DICOM tag dictionary enumeration, backing the tag picker.
//
// suyashkumar/dicom keeps its dictionary in an unexported map (pkg/tag, var
// tagDict) and exports no iterator — only Find, MustFind, FindByKeyword and
// FindByName — while the individual tags are package variables, which Go
// cannot reflect over. The only way to obtain the whole dictionary is
// therefore to probe Find across every group the standard defines.
// dicomStandardGroups is that list, and TestDictionaryGroupsCoverLibrary
// checks it against the library's own source so a group introduced by a
// library upgrade fails a test rather than silently vanishing from the picker.

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/suyashkumar/dicom/pkg/tag"
)

// dicomStandardGroups lists every group present in the pinned library's
// dictionary. All are even — private (odd) groups carry no standard tags, and
// the DIMSE command group (0000) belongs to the network stack, not to files.
var dicomStandardGroups = []uint16{
	0x0002, 0x0004, 0x0006, 0x0008, 0x0010, 0x0012, 0x0014, 0x0016,
	0x0018, 0x0020, 0x0022, 0x0024, 0x0028, 0x0032, 0x0034, 0x0038,
	0x003A, 0x0040, 0x0042, 0x0044, 0x0046, 0x0048, 0x0050, 0x0052,
	0x0054, 0x0060, 0x0062, 0x0064, 0x0066, 0x0068, 0x006A, 0x0070,
	0x0072, 0x0074, 0x0076, 0x0078, 0x0080, 0x0082, 0x0088, 0x0100,
	0x0400, 0x1000, 0x1010, 0x2000, 0x2010, 0x2020, 0x2030, 0x2040,
	0x2050, 0x2100, 0x2110, 0x2120, 0x2130, 0x2200, 0x3002, 0x3004,
	0x3006, 0x3008, 0x300A, 0x300C, 0x300E, 0x3010, 0x4000, 0x4008,
	0x4010, 0x4FFE, 0x5000, 0x5200, 0x5400, 0x5600, 0x6000, 0x7F00,
	0x7FE0, 0xFFFA, 0xFFFC, 0xFFFE,
}

var (
	dictOnce sync.Once
	dictTags []tag.Info
)

// probeGroup returns every dictionary entry in one group, in element order.
func probeGroup(group uint16) []tag.Info {
	var found []tag.Info
	for e := 0; e <= 0xFFFF; e++ {
		info, err := tag.Find(tag.Tag{Group: group, Element: uint16(e)})
		if err != nil {
			continue
		}
		// Find synthesises a "Generic Group Length" entry for element 0000 of
		// every even group, so a bare probe would invent one phantom tag per
		// group. Real group-length entries (e.g. File Meta Information Group
		// Length) carry their own keyword and are kept.
		if e == 0x0000 && info.Keyword == "GenericGroupLength" {
			continue
		}
		found = append(found, info)
	}
	return found
}

// dictionaryTags returns every tag in the standard dictionary, ordered by
// group then element. Runs once per process; callers may treat the result as
// read-only shared state.
//
// The probe is ~5M lookups of which all but ~5k miss, and tag.Find formats an
// error on every miss — enough allocation to cost seconds single-threaded. It
// is also embarrassingly parallel (a read-only map lookup per group), so the
// groups are swept concurrently, which brings a cold start back under half a
// second. showModProfileEditor warms it in the background besides.
func dictionaryTags() []tag.Info {
	dictOnce.Do(func() {
		groups := dicomStandardGroups
		perGroup := make([][]tag.Info, len(groups))
		workers := runtime.NumCPU()
		if workers > len(groups) {
			workers = len(groups)
		}
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(start int) {
				defer wg.Done()
				for i := start; i < len(groups); i += workers {
					perGroup[i] = probeGroup(groups[i])
				}
			}(w)
		}
		wg.Wait()

		dictTags = make([]tag.Info, 0, 5200)
		for _, infos := range perGroup {
			dictTags = append(dictTags, infos...)
		}
		// perGroup follows dicomStandardGroups and each probe ascends by
		// element, so this is already ordered; sorting keeps that true even if
		// the group list is ever edited out of order.
		sort.Slice(dictTags, func(i, j int) bool {
			if dictTags[i].Tag.Group != dictTags[j].Tag.Group {
				return dictTags[i].Tag.Group < dictTags[j].Tag.Group
			}
			return dictTags[i].Tag.Element < dictTags[j].Tag.Element
		})
	})
	return dictTags
}

// formatTagRef renders t the way the profile store writes tags: zero-padded
// 4-digit uppercase hex. This is the one spelling a profile holds — see
// canonicalTagRef, which every stored reference goes through.
func formatTagRef(t tag.Tag) string {
	return fmt.Sprintf("%04X,%04X", t.Group, t.Element)
}

// tagLineGap separates a stored tag reference from the descriptive name shown
// beside it in the profile editor's Remove/Keep lists. Two spaces matches the
// picker's row format and splits back unambiguously, since dictionary names
// contain single spaces ("Other Patient IDs").
const tagLineGap = "  "

// stripTagDecoration returns just the stored reference from a display line,
// discarding any name appended for readability.
func stripTagDecoration(line string) string {
	line = strings.TrimSpace(line)
	if i := strings.Index(line, tagLineGap); i >= 0 {
		return strings.TrimSpace(line[:i])
	}
	return line
}

// strippedTagLines splits a decorated tag-list entry back into the references
// to store, one per line. Empty input yields nil, not an empty slice, matching
// splitProfileLines: an empty-but-non-nil list would differ from a stored nil
// under reflect.DeepEqual, and Preferences uses exactly that comparison to
// decide whether profiles.json needs rewriting at all.
func strippedTagLines(text string) []string {
	var out []string
	for _, line := range splitProfileLines(text) {
		if ref := stripTagDecoration(line); ref != "" {
			out = append(out, ref)
		}
	}
	return out
}

// decorateTagList renders stored entries for display, one per line, as a
// zero-padded GGGG,EEEE reference with the tag's name appended — the same form
// the picker shows, so a profile hand-written as "8,80" still reads as
// "0008,0080  Institution Name". A reference that resolves to nothing is shown
// as written, for validation to report.
//
// The name is display only and never reaches the profile: checkTagLines strips
// it and stores the canonical reference. Applying this to already-decorated
// text is a no-op.
func decorateTagList(entries []string) string {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		ref := stripTagDecoration(entry)
		if ref == "" {
			continue
		}
		t, err := parseTagString(ref)
		if err != nil {
			lines = append(lines, ref)
			continue
		}
		line := formatTagRef(t)
		if name := tagDisplayName(t); name != "" {
			line += tagLineGap + name
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// tagPickerLabel renders one dictionary entry as a picker row.
func tagPickerLabel(info tag.Info) string {
	label := formatTagRef(info.Tag)
	if info.Name != "" {
		label += "  " + info.Name
	}
	if info.Retired {
		label += "  (retired)"
	}
	return label
}

// dicomGroupNames labels the groups a user is likely to browse. The DICOM
// standard no longer names groups normatively, so these are the conventional
// names; anything unlisted falls back to its number.
var dicomGroupNames = map[uint16]string{
	0x0002: "File Meta",
	0x0004: "Directory",
	0x0008: "Identifying",
	0x0010: "Patient",
	0x0012: "Clinical Trial",
	0x0014: "Non-destructive Testing",
	0x0018: "Acquisition",
	0x0020: "Relationship",
	0x0022: "Ophthalmology",
	0x0024: "Visual Field",
	0x0028: "Image Presentation",
	0x0032: "Study",
	0x0038: "Visit / Admission",
	0x003A: "Waveform",
	0x0040: "Procedure Step",
	0x0042: "Encapsulated Document",
	0x0044: "Product",
	0x0046: "Visual Correction",
	0x0048: "Slide Microscopy",
	0x0050: "Device",
	0x0052: "Intravascular OCT",
	0x0054: "Nuclear Medicine",
	0x0060: "Histogram",
	0x0062: "Segmentation",
	0x0064: "Deformable Registration",
	0x0066: "Surface Mesh",
	0x0068: "Implant Template",
	0x0070: "Presentation State",
	0x0072: "Hanging Protocol",
	0x0074: "Unified Procedure Step",
	0x0076: "Implant Assembly",
	0x0078: "Implant Template Group",
	0x0088: "Storage",
	0x0100: "Authorization",
	0x0400: "Digital Signature",
	0x2000: "Film Session",
	0x2010: "Film Box",
	0x2020: "Image Box",
	0x2030: "Annotation",
	0x2040: "Overlay Box",
	0x2050: "Presentation LUT",
	0x2100: "Print Job",
	0x2110: "Printer",
	0x2120: "Print Queue",
	0x2130: "Print Content",
	0x2200: "Media Creation",
	0x3002: "RT Image",
	0x3004: "RT Dose",
	0x3006: "RT Structure Set",
	0x3008: "RT Treatment",
	0x300A: "RT Plan",
	0x300C: "RT Relationship",
	0x300E: "RT Approval",
	0x3010: "RT Radiation",
	0x4000: "Text",
	0x4008: "Results",
	0x4010: "Threat Detection",
	0x4FFE: "MAC Parameters",
	0x5000: "Curve",
	0x5200: "Functional Groups",
	0x5400: "Waveform Data",
	0x5600: "Spectroscopy",
	0x6000: "Overlay",
	0x7F00: "Variable Pixel Data",
	0x7FE0: "Pixel Data",
	0xFFFA: "Digital Signatures",
	0xFFFC: "Trailing Padding",
	0xFFFE: "Item Delimitation",
}

// groupPickerLabel renders one group row: its number, conventional name, how
// many tags it holds under the active filter, and how many of the group's tags
// are checked — the last so a collapsed group still shows that it contains
// part of the current selection.
func groupPickerLabel(group uint16, shown, selected int) string {
	label := fmt.Sprintf("%04X", group)
	if name, ok := dicomGroupNames[group]; ok {
		label += "  " + name
	}
	if selected > 0 {
		return label + fmt.Sprintf("  (%d, %d selected)", shown, selected)
	}
	return label + fmt.Sprintf("  (%d)", shown)
}

// tagMatchesQuery reports whether info matches a lowercased search query,
// tested against the tag's name, keyword and GGGG,EEEE reference.
func tagMatchesQuery(info tag.Info, query string) bool {
	if query == "" {
		return true
	}
	if strings.Contains(strings.ToLower(info.Name), query) ||
		strings.Contains(strings.ToLower(info.Keyword), query) {
		return true
	}
	ref := strings.ToLower(formatTagRef(info.Tag))
	// Match the comma form ("0010,0010"), a bare group or element ("0010"),
	// and the run-together form some people type ("00100010").
	return strings.Contains(ref, query) ||
		strings.Contains(strings.ReplaceAll(ref, ",", ""), query)
}
