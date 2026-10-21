package main

import (
	"encoding/json"
	"fmt"
	"image/color"
	"reflect"
	"strings"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// Modalities are stored canonically, and a profile without any marshals
// exactly as before — the block dicomhdr reads, unchanged.
func TestTagProfileModalitiesWire(t *testing.T) {
	p := TagProfile{Name: "NM", Tags: []tag.Tag{{Group: 0x0054, Element: 0x0012}},
		Enabled: true, Modalities: []string{" nm ", "pt", "NM"}}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"modalities":["NM","PT"]`) {
		t.Errorf("marshalled %s", data)
	}
	var back TagProfile
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Modalities, []string{"NM", "PT"}) {
		t.Errorf("round trip modalities = %v", back.Modalities)
	}

	plain, _ := json.Marshal(TagProfile{Name: "PHI", Tags: []tag.Tag{{Group: 0x0010, Element: 0x0010}}})
	if strings.Contains(string(plain), "modalities") {
		t.Errorf("an unrestricted profile writes the key: %s", plain)
	}
	var old TagProfile
	if err := json.Unmarshal([]byte(`{"name":"PHI","color":{"R":1,"G":2,"B":3,"A":255},"tags":["0010,0010"],"enabled":true}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Modalities != nil || !old.appliesTo("CT") || !old.appliesTo("") {
		t.Errorf("a profile written before the field must apply to every file")
	}
}

func TestParseModalityList(t *testing.T) {
	got, err := parseModalityList("ct,  pt nm\tCT")
	if err != nil || !reflect.DeepEqual(got, []string{"CT", "PT", "NM"}) {
		t.Errorf("parse = %v, %v", got, err)
	}
	if got, err := parseModalityList("  "); err != nil || got != nil {
		t.Errorf("empty = %v, %v", got, err)
	}
	for _, bad := range []string{"C-T", "PET/CT", strings.Repeat("A", 17)} {
		if _, err := parseModalityList(bad); err == nil {
			t.Errorf("%q accepted as a modality code", bad)
		}
	}
}

// A tag two modalities share takes the colour of the profile for the file's
// own modality; an unrestricted profile still wins wherever it comes first.
func TestMatchProfileByModality(t *testing.T) {
	kernel := tag.Tag{Group: 0x0018, Element: 0x1210}
	name := tag.Tag{Group: 0x0010, Element: 0x0010}
	phi := color.RGBA{R: 255, G: 140, A: 255}
	nm := color.RGBA{R: 255, G: 152, A: 255}
	ct := color.RGBA{G: 158, A: 255}
	profiles := []TagProfile{
		{Name: "PHI", Color: phi, Tags: []tag.Tag{name}, Enabled: true},
		{Name: "NM", Color: nm, Tags: []tag.Tag{kernel}, Enabled: true, Modalities: []string{"NM"}},
		{Name: "CT", Color: ct, Tags: []tag.Tag{kernel}, Enabled: true, Modalities: []string{"CT"}},
	}
	for _, tc := range []struct {
		modality string
		tg       tag.Tag
		want     color.Color
	}{
		{"CT", kernel, ct},
		{"nm", kernel, nm},
		{"MR", kernel, nil},
		{"", kernel, nil},
		{"MR", name, phi},
		{"", name, phi},
	} {
		c, ok := matchProfile(tc.tg, true, tc.modality, profiles)
		if (tc.want == nil) == ok || (ok && c != tc.want) {
			t.Errorf("modality %q, tag %v: got %v, %v; want %v", tc.modality, tc.tg, c, ok, tc.want)
		}
	}
}

// The shipped profiles: PHI applies everywhere, each modality profile to its
// own files, and the shared CT/NM tags now show CT's colour in a CT file.
func TestDefaultTagProfilesByModality(t *testing.T) {
	var s Settings
	if err := json.Unmarshal(defaultSettingsJSON, &s); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"PHI": nil, "NM": {"NM"}, "PET": {"PT"}, "CT": {"CT"}, "MR": {"MR"}, "US": {"US", "IVUS"}}
	colours := map[string]color.Color{}
	for _, p := range s.TagProfiles {
		if !reflect.DeepEqual(p.Modalities, want[p.Name]) {
			t.Errorf("%s modalities = %v, want %v", p.Name, p.Modalities, want[p.Name])
		}
		colours[p.Name] = p.Color
	}
	for _, tc := range []struct {
		modality string
		tg       tag.Tag
		profile  string
	}{
		{"CT", tag.Tag{Group: 0x0018, Element: 0x1210}, "CT"},  // Convolution Kernel, also an NM attribute
		{"CT", tag.Tag{Group: 0x0018, Element: 0x1130}, "CT"},  // Table Height
		{"NM", tag.Tag{Group: 0x0018, Element: 0x1130}, "NM"},  // the same tag in an NM file
		{"MR", tag.Tag{Group: 0x0018, Element: 0x0022}, "MR"},  // Scan Options, also a CT attribute
		{"US", tag.Tag{Group: 0x0018, Element: 0x1088}, "US"},  // Heart Rate, also NM, PET and MR
		{"PT", tag.Tag{Group: 0x0054, Element: 0x1001}, "PET"}, // Units
		{"PT", tag.Tag{Group: 0x0010, Element: 0x0010}, "PHI"}, // Patient's Name
	} {
		c, ok := matchProfile(tc.tg, true, tc.modality, s.TagProfiles)
		if !ok || c != colours[tc.profile] {
			t.Errorf("%s file, %04X,%04X: colour %v, want %s's %v", tc.modality, tc.tg.Group, tc.tg.Element,
				c, tc.profile, colours[tc.profile])
		}
	}
}

// The hover tooltip names the profile whose colour the row shows, then any
// others that also match in this file — and only those that apply to the
// file's modality. A private tag gets a tooltip only when a profile lists it.
func TestTagTooltipNamesProfiles(t *testing.T) {
	name := tag.Tag{Group: 0x0010, Element: 0x0010}
	units := tag.Tag{Group: 0x0054, Element: 0x1001}
	kernel := tag.Tag{Group: 0x0018, Element: 0x1210}
	listed := tag.Tag{Group: 0x0009, Element: 0x1001}
	unlisted := tag.Tag{Group: 0x0009, Element: 0x1002}
	profiles := []TagProfile{
		{Name: "PHI", Tags: []tag.Tag{name}, Enabled: true},
		{Name: "PET", Tags: []tag.Tag{name, units}, Enabled: true, Modalities: []string{"PT"}},
		{Name: "CT", Tags: []tag.Tag{kernel}, Enabled: true, Modalities: []string{"CT"}},
		{Name: "Off", Tags: []tag.Tag{name}, Enabled: false},
		{Name: "Vendor", Tags: []tag.Tag{listed}, Enabled: true},
	}
	privateEl := func(tg tag.Tag) *sdicom.Element {
		v, err := sdicom.NewValue([]string{"X"})
		if err != nil {
			t.Fatal(err)
		}
		return &sdicom.Element{Tag: tg, ValueRepresentation: tag.VRStringList, RawValueRepresentation: "LO", Value: v}
	}
	m := newTagTreeModel()
	m.addDataset(sdicom.Dataset{Elements: []*sdicom.Element{
		mustTestElement(t, tag.Modality, []string{"PT"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3"}),
		mustTestElement(t, name, []string{"DOE^JANE"}),
		mustTestElement(t, units, []string{"BQML"}),
		mustTestElement(t, kernel, []string{"STANDARD"}),
		privateEl(listed),
		privateEl(unlisted),
	}})
	tips := map[tag.Tag]string{}
	for id, n := range m.nodes {
		if n.hasTag {
			tips[n.elmTag] = m.tooltipFor(id, profiles)
		}
	}
	profileLines := func(tip string) []string {
		var out []string
		for _, l := range strings.Split(tip, "\n") {
			if strings.HasPrefix(l, "Profile: ") || strings.HasPrefix(l, "Also in: ") {
				out = append(out, l)
			}
		}
		return out
	}
	for _, tc := range []struct {
		tg   tag.Tag
		want []string
	}{
		{name, []string{"Profile: PHI", "Also in: PET"}},
		{units, []string{"Profile: PET"}},
		{kernel, nil}, // CT's profile does not apply to a PT file
	} {
		tip := tips[tc.tg]
		if !strings.HasPrefix(tip, fmt.Sprintf("Tag:     (%04X,%04X)\nName:    ", tc.tg.Group, tc.tg.Element)) {
			t.Errorf("%v: dictionary entry missing from %q", tc.tg, tip)
		}
		if got := profileLines(tip); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v: profile lines %q, want %q", tc.tg, got, tc.want)
		}
	}
	if got, want := tips[listed], "Tag:     (0009,1001)\nProfile: Vendor"; got != want {
		t.Errorf("listed private tag: %q, want %q", got, want)
	}
	if got := tips[unlisted]; got != "" {
		t.Errorf("unlisted private tag has a tooltip: %q", got)
	}
}

// Every element row knows its file's modality, nested ones included; the
// structural rows above them do not carry one.
func TestTagTreeRecordsModality(t *testing.T) {
	m := newTagTreeModel()
	m.addDataset(sdicom.Dataset{Elements: []*sdicom.Element{
		mustTestElement(t, tag.Modality, []string{"ct "}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3"}),
		mustTestElement(t, tag.Tag{Group: 0x0008, Element: 0x1140}, [][]*sdicom.Element{{
			mustTestElement(t, tag.ReferencedSOPInstanceUID, []string{"1.2.4"}),
		}}),
	}})
	elements, nested := 0, 0
	for id, n := range m.nodes {
		if !n.hasTag {
			if m.nodeModality(id) != "" {
				t.Errorf("structural row %q carries modality %q", n.label, m.nodeModality(id))
			}
			continue
		}
		elements++
		if n.elmTag == tag.ReferencedSOPInstanceUID {
			nested++
		}
		if got := m.nodeModality(id); got != "CT" {
			t.Errorf("row %q: modality %q, want CT", n.label, got)
		}
	}
	if elements != 4 || nested != 1 {
		t.Errorf("rows: %d elements, %d nested; want 4 and 1", elements, nested)
	}
}
