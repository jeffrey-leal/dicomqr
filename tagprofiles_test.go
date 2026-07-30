package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/color"
	"testing"

	"github.com/suyashkumar/dicom/pkg/tag"
)

// TestDefaultSettingsTagViewer verifies the embedded defaults carry the tag
// viewer preferences: highlights on, dicomhdr's red, and the stock profiles
// (PHI, NM, PET), all enabled with non-empty, duplicate-free tag lists.
func TestDefaultSettingsTagViewer(t *testing.T) {
	var s Settings
	if err := json.Unmarshal(defaultSettingsJSON, &s); err != nil {
		t.Fatalf("defaults/settings.json does not parse: %v", err)
	}
	if !s.ItalicPrivate {
		t.Error("ItalicPrivate should default to true")
	}
	if s.MalformedColor != "E54545FF" {
		t.Errorf("MalformedColor = %q, want E54545FF", s.MalformedColor)
	}
	wantNames := []string{"PHI", "NM", "PET"}
	if len(s.TagProfiles) != len(wantNames) {
		t.Fatalf("expected %d default profiles %v, got %+v", len(wantNames), wantNames, s.TagProfiles)
	}
	for i, want := range wantNames {
		p := s.TagProfiles[i]
		if p.Name != want {
			t.Errorf("profile %d = %q, want %q", i, p.Name, want)
		}
		if !p.Enabled {
			t.Errorf("profile %q should default to enabled", p.Name)
		}
		if len(p.Tags) == 0 {
			t.Errorf("profile %q has no tags", p.Name)
		}
		seen := make(map[tag.Tag]bool, len(p.Tags))
		for _, pt := range p.Tags {
			if seen[pt] {
				t.Errorf("profile %q lists tag %04X,%04X more than once", p.Name, pt.Group, pt.Element)
			}
			seen[pt] = true
		}
	}
	phi := s.TagProfiles[0]
	// Spot-check PatientName (0010,0010) is covered and drives matchProfile.
	c, ok := matchProfile(tag.Tag{Group: 0x0010, Element: 0x0010}, true, s.TagProfiles)
	if !ok {
		t.Fatal("PatientName (0010,0010) should match the PHI profile")
	}
	if c != phi.Color {
		t.Errorf("matchProfile colour = %v, want %v", c, phi.Color)
	}
	// Structural nodes (hasTag=false) must never match.
	if _, ok := matchProfile(tag.Tag{Group: 0x0010, Element: 0x0010}, false, s.TagProfiles); ok {
		t.Error("matchProfile must not match when hasTag is false")
	}
}

// TestDefaultSettingsStrict decodes the embedded defaults with unknown fields
// disallowed, so a hand-edited defaults/settings.json with a misspelled key
// fails here instead of being silently ignored at run time. TagProfile's
// custom unmarshaller escapes the strict decoder, so profile blocks are
// re-checked against the wire struct explicitly.
func TestDefaultSettingsStrict(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader(defaultSettingsJSON))
	dec.DisallowUnknownFields()
	var s Settings
	if err := dec.Decode(&s); err != nil {
		t.Fatalf("defaults/settings.json has an unknown or malformed field: %v", err)
	}

	var raw struct {
		TagProfiles []json.RawMessage `json:"tagProfiles"`
	}
	if err := json.Unmarshal(defaultSettingsJSON, &raw); err != nil {
		t.Fatalf("extracting tagProfiles: %v", err)
	}
	for i, block := range raw.TagProfiles {
		pd := json.NewDecoder(bytes.NewReader(block))
		pd.DisallowUnknownFields()
		var w tagProfileWire
		if err := pd.Decode(&w); err != nil {
			t.Errorf("tagProfiles[%d] has an unknown or malformed field: %v", i, err)
		}
	}
}

// TestDefaultSettingsRoundTrip verifies the embedded defaults survive the
// app's own save path: unmarshal into Settings, marshal back, unmarshal again,
// and compare the tag profiles — proving nothing in the defaults file is
// dropped or mangled once a new installation first saves its settings.
func TestDefaultSettingsRoundTrip(t *testing.T) {
	var s Settings
	if err := json.Unmarshal(defaultSettingsJSON, &s); err != nil {
		t.Fatalf("defaults/settings.json does not parse: %v", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var s2 Settings
	if err := json.Unmarshal(data, &s2); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if fmt.Sprintf("%+v", s.TagProfiles) != fmt.Sprintf("%+v", s2.TagProfiles) {
		t.Errorf("tag profiles changed across save round-trip:\n%+v\n->\n%+v", s.TagProfiles, s2.TagProfiles)
	}
	if fmt.Sprintf("%+v", s.Profiles) != fmt.Sprintf("%+v", s2.Profiles) {
		t.Errorf("server profiles changed across save round-trip:\n%+v\n->\n%+v", s.Profiles, s2.Profiles)
	}
}

// TestTagProfileJSONRoundTrip verifies the "GGGG,EEEE" wire format survives a
// marshal/unmarshal cycle unchanged (the format dicomhdr uses, so profiles can
// be copied between the two applications' settings files).
func TestTagProfileJSONRoundTrip(t *testing.T) {
	in := TagProfile{
		Name:    "Test",
		Color:   color.RGBA{R: 1, G: 2, B: 3, A: 255},
		Enabled: true,
		Tags: []tag.Tag{
			{Group: 0x0008, Element: 0x0020},
			{Group: 0x0040, Element: 0xA730},
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out TagProfile
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Name != in.Name || out.Color != in.Color || out.Enabled != in.Enabled {
		t.Errorf("round trip changed fields: %+v -> %+v", in, out)
	}
	if len(out.Tags) != 2 || out.Tags[0] != in.Tags[0] || out.Tags[1] != in.Tags[1] {
		t.Errorf("round trip changed tags: %v -> %v", in.Tags, out.Tags)
	}
}

// TestParseTags covers the accepted spellings and the error cases.
func TestParseTags(t *testing.T) {
	got, err := parseTags(" (0008,0020) \n\n0010,0010\n  0040 , a730 ")
	if err != nil {
		t.Fatalf("parseTags: %v", err)
	}
	want := []tag.Tag{
		{Group: 0x0008, Element: 0x0020},
		{Group: 0x0010, Element: 0x0010},
		{Group: 0x0040, Element: 0xA730},
	}
	if len(got) != len(want) {
		t.Fatalf("parseTags returned %d tags, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tag %d = %v, want %v", i, got[i], want[i])
		}
	}
	if _, err := parseTags("00080020"); err == nil {
		t.Error("missing comma should be rejected")
	}
	if _, err := parseTags("ZZZZ,0020"); err == nil {
		t.Error("non-hex group should be rejected")
	}
}
