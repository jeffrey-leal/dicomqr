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
	DarkTheme    bool            `json:"darkTheme"`
	FontName     string          `json:"fontName"`
	LocalAETitle string          `json:"localAETitle"`
	LocalSCPPort int             `json:"localSCPPort"`
	DownloadDir  string          `json:"downloadDir"`
	Profiles     []ServerProfile `json:"profiles"`
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

// saveSettings writes s to ~/.dicomqr/settings.json as indented JSON atomically
// (write-to-temp + rename) to prevent corruption on crash mid-write.
func saveSettings(s Settings) {
	path, err := appSettingsPath()
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	if s.Profiles == nil {
		s.Profiles = []ServerProfile{}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}

	// Write to temp file first; os.Rename is atomic on NTFS.
	tmp, err := os.CreateTemp(dir, ".settings_*.json.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return
	}
	// os.Rename is atomic on NTFS; overwrites destination atomically.
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
	}
}
