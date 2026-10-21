package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// defaultViewerCandidates lists well-known DICOM viewer executables, checked in
// order. The first one that exists on disk becomes the default ViewerPath.
var defaultViewerCandidates = []string{
	`C:\Program Files\MicroDicom\mDicom.exe`,
	`C:\Program Files (x86)\MicroDicom\mDicom.exe`,
	`C:\Program Files\RadiAnt DICOM Viewer\RadiAntViewer.exe`,
	`C:\Program Files (x86)\RadiAnt DICOM Viewer\RadiAntViewer.exe`,
}

// DetectDefaultViewer returns the path of the first known DICOM viewer found
// on disk, or "" if none is installed.
func DetectDefaultViewer() string {
	for _, p := range defaultViewerCandidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

//go:embed defaults/settings.json
var defaultSettingsJSON []byte

// Settings holds all persisted application preferences.
type Settings struct {
	DarkTheme    bool            `json:"darkTheme"`
	UITheme      string          `json:"uiTheme"` // colour theme pack ("" = stock Fyne theme; see themePackBase)
	FontName     string          `json:"fontName"`
	LocalAETitle string          `json:"localAETitle"`
	LocalSCPPort int             `json:"localSCPPort"`
	DownloadDir  string          `json:"downloadDir"`
	Profiles     []ServerProfile `json:"profiles"`

	// Window size persisted across sessions (Phase 5-2B); zero means use
	// default. These are Fyne's scaled units, the ones Window.Resize takes.
	WindowWidth  float32 `json:"windowWidth"`
	WindowHeight float32 `json:"windowHeight"`

	// Window position persisted across sessions, in physical pixels — the
	// units the Windows API works in, since Fyne cannot position a window at
	// all (see windowplacement.go). WindowPosSaved distinguishes "never
	// placed" from a window legitimately sitting at 0,0.
	WindowX        int32 `json:"windowX"`
	WindowY        int32 `json:"windowY"`
	WindowPosSaved bool  `json:"windowPosSaved"`

	// Appearance of selected tree rows (Phase 5-2E). SelectionColor is an
	// RRGGBBAA hex string; empty means follow the theme's primary colour.
	SelectionColor  string `json:"selectionColor"`
	SelectionBold   bool   `json:"selectionBold"`
	SelectionItalic bool   `json:"selectionItalic"`

	// ViewerPath is the full path to an external DICOM viewer executable.
	// Empty means no external viewer is configured.
	ViewerPath string `json:"viewerPath"`

	// Tag viewer (View Tags window) appearance, ported from dicomhdr.
	// ItalicPrivate renders private tags in italic; MalformedColor is the
	// RRGGBBAA colour for public tags whose VR violates the standard;
	// TagProfiles colour user-defined tag sets (first enabled profile
	// containing a tag wins; the malformed highlight takes precedence).
	ItalicPrivate  bool         `json:"italicPrivate"`
	MalformedColor string       `json:"malformedColor"`
	TagProfiles    []TagProfile `json:"tagProfiles"`

	// RetrieveStallTimeoutSec aborts a retrieve when no progress response and
	// no received file arrives for this many seconds — recovery from PACS
	// servers whose C-MOVE agent stalls on non-image objects (SR/PR). 0 uses
	// the default (120 s); negative disables stall detection (e.g. for slow
	// tape archives).
	// The same limit stops a push from waiting forever on a server that has
	// stopped answering (stallTimeoutFor).
	RetrieveStallTimeoutSec int `json:"retrieveStallTimeoutSec,omitempty"`

	// ModifyOutputDir is the default output folder for modification exports,
	// used directly by the modification dialog so no folder picker appears on
	// routine runs (Change… overrides it for a single run). Empty means the
	// dialog asks on the first run and persists that choice here. Must lie
	// outside the download folder — enforced on Apply and again at
	// modification time.
	ModifyOutputDir string `json:"modifyOutputDir"`

	// ModProfileOrder is the order the modification profiles are listed in —
	// Preferences and both Modification menus — by name. It lives here rather
	// than in profiles.json because that file is a name-keyed map shared with
	// dicomtool: Go writes a map sorted by key, and any key added to hold an
	// order would read there as one more profile. Nil means alphabetical; a
	// profile it does not name follows the ones it does, and a name no longer
	// in the store is ignored (orderedModProfileNames).
	ModProfileOrder []string `json:"modProfileOrder,omitempty"`

	// ShippedProfilesSeen records, per shipped profile ("modification:<name>"
	// or "tag:<name>"), the digest of the version the user was last shown at
	// startup (shippedprofiles.go). A new release offers a shipped profile
	// again only when its digest has changed since — so a profile deliberately
	// deleted or edited is not offered back at every start, only when the
	// release actually changes it.
	ShippedProfilesSeen map[string]string `json:"shippedProfilesSeen,omitempty"`

	// PrefListHeights remembers how tall the user dragged each Preferences
	// list ("servers", "tagProfiles", "modProfiles"), in Fyne's scaled units
	// like WindowWidth/WindowHeight. Window geometry, not a preference: kept
	// whether Preferences closes by Apply or Cancel. Absent = the default.
	PrefListHeights map[string]float32 `json:"prefListHeights,omitempty"`

	// WindowPlacements remembers where every window other than the main one
	// was and how big, by window type ("preferences", "viewer", …; never a
	// title, which can carry patient data), in physical pixels like WindowX/Y.
	// windowmemory.go holds the live copy; it is written back here only as the
	// application closes (snapshotWindowPlacements), so a save mid-session
	// writes the placements as they were at startup — the closing save
	// supersedes it.
	WindowPlacements map[string]windowPlacement `json:"windowPlacements,omitempty"`

	// ExportFormat ("csv" or "json") is the file type listed first in the
	// tag viewer's Export Tags… save dialog, and the format applied when the
	// typed filename has no extension.
	ExportFormat string `json:"exportFormat"`

	// ImportSourceDir is the folder the Import tab last imported from. It
	// pre-fills the source field at startup and is where its folder chooser
	// opens, so importing repeatedly from one place (a CD drive, a
	// department share) does not mean navigating there every session.
	ImportSourceDir string `json:"importSourceDir,omitempty"`
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
// If DownloadDir is still empty after loading, it defaults to ~/DICOM Downloads.
// defaultStallTimeout is the Retrieve stall timeout a setting of 0 means.
const defaultStallTimeout = 120 * time.Second

// stallTimeoutFor turns RetrieveStallTimeoutSec into a duration: 0 is the
// default, a positive value that many seconds, and a negative one — stall
// detection disabled, for a slow tape archive — comes back as 0.
func stallTimeoutFor(sec int) time.Duration {
	switch {
	case sec < 0:
		return 0
	case sec == 0:
		return defaultStallTimeout
	}
	return time.Duration(sec) * time.Second
}

func loadSettings() Settings {
	var s Settings
	json.Unmarshal(defaultSettingsJSON, &s)

	path, err := appSettingsPath()
	if err == nil {
		if data, err := os.ReadFile(path); err == nil {
			json.Unmarshal(data, &s)
		}
	}

	if s.DownloadDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			s.DownloadDir = filepath.Join(home, "DICOM Downloads")
		}
	}

	// Migrate profiles saved by versions that used the TransferUncompressed flag.
	for i := range s.Profiles {
		migrateProfile(&s.Profiles[i])
	}
	return s
}

// saveSettings writes s via saveSettingsE and logs any failure. Call sites
// with a window to report to (the Preferences dialog) use saveSettingsE
// directly and surface the error to the user — a settings save must never
// fail silently again (field evidence 2026-07-23: mid-session preference
// changes were lost without a trace).
func saveSettings(s Settings) {
	if err := saveSettingsE(s); err != nil {
		logError("settings: save failed: %v", err)
	}
}

// saveSettingsE writes s to ~/.dicomqr/settings.json as indented JSON
// atomically (write-to-temp + rename) to prevent corruption on crash
// mid-write. The final rename is retried briefly: antivirus and indexing
// tools open freshly written files and can hold the destination just long
// enough to fail a single attempt.
func saveSettingsE(s Settings) error {
	path, err := appSettingsPath()
	if err != nil {
		return fmt.Errorf("locate settings file: %w", err)
	}
	if s.Profiles == nil {
		s.Profiles = []ServerProfile{}
	}
	if s.TagProfiles == nil {
		s.TagProfiles = []TagProfile{}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWriteJSON(path, data); err != nil {
		return err
	}
	logInfo("settings: saved %s", path)
	return nil
}

// atomicWriteJSON writes data to path via a temp file + rename (atomic on
// NTFS) so a crash mid-write never corrupts the destination. The final rename
// is retried briefly: antivirus and indexing tools open freshly written files
// and can hold the destination just long enough to fail a single attempt.
func atomicWriteJSON(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", filepath.Base(path), err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"_*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	var renameErr error
	for attempt := 0; attempt < 3; attempt++ {
		if renameErr = os.Rename(tmpPath, path); renameErr == nil {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	os.Remove(tmpPath)
	return fmt.Errorf("replace %s: %w", path, renameErr)
}
