package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
)

//go:embed defaults/settings.json
var defaultSettingsJSON []byte

// Settings holds all persisted application preferences.
type Settings struct {
	DarkTheme       bool            `json:"darkTheme"`
	FontName        string          `json:"fontName"`
	LocalAETitle    string          `json:"localAETitle"`
	LocalSCPPort    int             `json:"localSCPPort"`
	DownloadDir     string          `json:"downloadDir"`
	SubfolderFormat string          `json:"subfolderFormat"`
	Profiles        []ServerProfile `json:"profiles"`
}

func appSettingsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".dicomqr"), nil
}

func appSettingsPath() (string, error) {
	dir, err := appSettingsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// ensureDefaultSettings creates ~/.dicomqr/settings.json with compiled-in
// defaults if the file does not already exist.
func ensureDefaultSettings() {
	path, err := appSettingsPath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(defaultSettingsJSON)
}

// loadSettings reads ~/.dicomqr/settings.json, overlaying saved values on top
// of compiled-in defaults so new fields always have a sensible value.
func loadSettings() Settings {
	var s Settings
	json.Unmarshal(defaultSettingsJSON, &s)

	path, err := appSettingsPath()
	if err != nil {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	json.Unmarshal(data, &s)
	return s
}

// saveSettings writes s to ~/.dicomqr/settings.json as indented JSON.
func saveSettings(s Settings) {
	path, err := appSettingsPath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if s.Profiles == nil {
		s.Profiles = []ServerProfile{}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(path, data, 0o644)
}
