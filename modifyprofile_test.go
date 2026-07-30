package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

// TestSaveModProfileConfigRoundTrip proves the Preferences editor's save path
// preserves the embedded default profiles exactly: every field that dicomtool
// understands survives save → load unchanged (the two tools share the store
// format, so a lossy round-trip would corrupt copied profiles).
func TestSaveModProfileConfigRoundTrip(t *testing.T) {
	original, _ := embeddedModConfigs(t)

	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := saveModProfileConfig(path, original); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := loadModProfileConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(original, loaded) {
		t.Errorf("round-trip changed the config:\noriginal: %+v\nloaded:   %+v", original, loaded)
	}
}

// TestModProfileEditPreservesUneditedFields simulates what the Preferences
// editor does — copy the profile, overwrite only the fields it has controls
// for — and verifies the fields without controls (per-modality overrides,
// ignore filters, dicomdir, verbose) survive the edit and a disk round-trip.
func TestModProfileEditPreservesUneditedFields(t *testing.T) {
	cfg := ModProfileConfig{
		"full": {
			Sets:             []string{"0010,0010=ANON"},
			Removes:          []string{"0010,1000"},
			DOB:              "YYYY0101",
			Dicomdir:         true,
			Verbose:          true,
			IgnoreTypes:      []string{"SECONDARY"},
			IgnoreModalities: []string{"SR", "PR"},
			PerModality: map[string]ModProfile{
				"CT": {Removes: []string{"0018,1030"}},
			},
		},
	}

	edited := cfg["full"] // the editor starts from the existing profile
	edited.Sets = []string{"0010,0010=CHANGED", "0010,0020=ID0000"}
	cfg["full"] = edited

	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := saveModProfileConfig(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := loadModProfileConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	got := loaded["full"]
	if !got.Dicomdir || !got.Verbose {
		t.Errorf("dicomdir/verbose lost: %+v", got)
	}
	if !reflect.DeepEqual(got.IgnoreTypes, []string{"SECONDARY"}) ||
		!reflect.DeepEqual(got.IgnoreModalities, []string{"SR", "PR"}) {
		t.Errorf("ignore filters lost: types %v modalities %v", got.IgnoreTypes, got.IgnoreModalities)
	}
	if !reflect.DeepEqual(got.PerModality, cfg["full"].PerModality) {
		t.Errorf("per-modality overrides lost: %+v", got.PerModality)
	}
	if len(got.Sets) != 2 || got.Sets[0] != "0010,0010=CHANGED" {
		t.Errorf("edited sets not saved: %v", got.Sets)
	}
}

// TestDefaultSettingsExportFormat guards the defaults/settings.json entry the
// Export dialog preselection relies on.
func TestDefaultSettingsExportFormat(t *testing.T) {
	var s Settings
	if err := json.Unmarshal(defaultSettingsJSON, &s); err != nil {
		t.Fatalf("unmarshal embedded settings.json: %v", err)
	}
	if s.ExportFormat != "csv" {
		t.Errorf("default exportFormat = %q, want %q", s.ExportFormat, "csv")
	}
	if s.ModifyOutputDir != "" {
		t.Errorf("default modifyOutputDir = %q, want empty", s.ModifyOutputDir)
	}
}
