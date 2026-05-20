package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StorageSCP is an embedded C-STORE SCP that listens for files pushed by a
// PACS in response to a C-MOVE-RQ. It writes received DICOM files to a
// structured subfolder tree under DownloadDir.
// TODO: replace stub bodies with algm/go-netdicom ServiceProvider calls.
type StorageSCP struct {
	localAETitle string
	port         int
	downloadDir  string
	subfolderFmt string

	// OnFileReceived is called on the goroutine that received the file.
	// Use fyne.Do() inside the callback to update UI elements.
	OnFileReceived func(path string)

	running bool
	stopCh  chan struct{}
}

// NewStorageSCP creates a C-STORE SCP that writes files to downloadDir.
func NewStorageSCP(localAETitle string, port int, downloadDir, subfolderFmt string) *StorageSCP {
	return &StorageSCP{
		localAETitle: localAETitle,
		port:         port,
		downloadDir:  downloadDir,
		subfolderFmt: subfolderFmt,
		stopCh:       make(chan struct{}),
	}
}

// Start begins listening on the configured port. Returns an error if the
// port is already in use or the download directory cannot be created.
func (s *StorageSCP) Start() error {
	if s.running {
		return nil
	}
	if s.downloadDir == "" {
		return errors.New("download directory is not configured")
	}
	if err := os.MkdirAll(s.downloadDir, 0o755); err != nil {
		return fmt.Errorf("cannot create download directory: %w", err)
	}
	s.running = true
	// TODO: start algm/go-netdicom ServiceProvider on s.port
	return nil
}

// Stop shuts down the listener.
func (s *StorageSCP) Stop() {
	if !s.running {
		return
	}
	s.running = false
	// TODO: stop algm/go-netdicom ServiceProvider
}

// IsRunning reports whether the SCP is currently listening.
func (s *StorageSCP) IsRunning() bool { return s.running }

// organizeFilePath builds the destination file path for a received DICOM file
// based on the configured subfolder format and the dataset metadata.
func (s *StorageSCP) organizeFilePath(studyDate, patientID, seriesNumber, sopInstanceUID string) string {
	sub := s.subfolderFmt
	sub = strings.ReplaceAll(sub, "StudyDate", sanitize(studyDate))
	sub = strings.ReplaceAll(sub, "PatientID", sanitize(patientID))
	sub = strings.ReplaceAll(sub, "SeriesNumber", sanitize(seriesNumber))

	filename := sanitize(sopInstanceUID) + ".dcm"
	if filename == ".dcm" {
		filename = fmt.Sprintf("%d.dcm", time.Now().UnixNano())
	}
	return filepath.Join(s.downloadDir, filepath.FromSlash(sub), filename)
}

// sanitize strips characters that are unsafe in path components.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
