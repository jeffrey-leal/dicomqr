package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	netdicom "github.com/algm/go-netdicom"
	"github.com/algm/go-netdicom/dimse"
	dicom "github.com/grailbio/go-dicom"
	"github.com/grailbio/go-dicom/dicomio"
	"github.com/grailbio/go-dicom/dicomtag"
)

// StorageSCP is an embedded C-STORE SCP that listens for files pushed by a
// PACS in response to a C-MOVE-RQ. It writes received DICOM files to a
// structured subfolder tree under DownloadDir.
type StorageSCP struct {
	localAETitle string
	port         int
	downloadDir  string
	listenAddr   string // actual bound address, set in Start()

	// OnFileReceived is called on the goroutine that received the file.
	// Use fyne.Do() inside the callback to update UI elements.
	OnFileReceived func(path string)

	running bool
	cancel  context.CancelFunc
}

// NewStorageSCP creates a C-STORE SCP that writes files to downloadDir.
func NewStorageSCP(localAETitle string, port int, downloadDir string) *StorageSCP {
	return &StorageSCP{
		localAETitle: localAETitle,
		port:         port,
		downloadDir:  downloadDir,
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

	params := netdicom.ServiceProviderParams{
		AETitle: s.localAETitle,
		// Respond to C-ECHO so PACS connectivity checks succeed.
		CEcho: func(_ netdicom.ConnectionState) dimse.Status {
			return dimse.Success
		},
		// CStore receives each DICOM object pushed by the PACS.
		CStore: func(ctx context.Context, _ netdicom.ConnectionState,
			transferSyntaxUID, sopClassUID, sopInstanceUID string,
			dataReader io.Reader, _ int64) dimse.Status {
			return s.handleCStore(transferSyntaxUID, sopClassUID, sopInstanceUID, dataReader)
		},
	}

	// Use "tcp4" to create an IPv4-only socket. net.Listen("tcp", ...) on
	// Windows binds to [::] (IPv6), and since Windows defaults to
	// IPV6_V6ONLY=1 that socket refuses IPv4 connections from the PACS.
	ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return fmt.Errorf("storage SCP: listen on port %d: %w", s.port, err)
	}
	s.listenAddr = ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.running = true

	// Close the listener when the context is cancelled (Stop() called).
	go func() { <-ctx.Done(); ln.Close() }()
	go func() {
		defer ln.Close()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go netdicom.RunProviderForConn(ctx, conn, params)
		}
	}()
	return nil
}

// ListenAddr returns the address the SCP is listening on (e.g. "0.0.0.0:11112").
func (s *StorageSCP) ListenAddr() string { return s.listenAddr }

// Stop shuts down the listener and cancels all in-flight connections.
func (s *StorageSCP) Stop() {
	if !s.running {
		return
	}
	s.running = false
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// IsRunning reports whether the SCP is currently listening.
func (s *StorageSCP) IsRunning() bool { return s.running }

// handleCStore writes one incoming DICOM object to a structured subfolder.
// Strategy: stream the payload to a temp file first (avoiding memory pressure
// from large pixel data), parse the metadata tags needed for the folder path
// (skipping pixel data), then rename the temp file to its final destination.
func (s *StorageSCP) handleCStore(
	transferSyntaxUID, sopClassUID, sopInstanceUID string,
	dataReader io.Reader,
) dimse.Status {
	// Create the temp file inside downloadDir so the later rename stays on the
	// same filesystem and avoids cross-device rename failures.
	tmpFile, err := os.CreateTemp(s.downloadDir, ".recv_*.tmp")
	if err != nil {
		return dimse.Status{Status: dimse.CStoreOutOfResources, ErrorComment: err.Error()}
	}
	tmpPath := tmpFile.Name()

	// Write DICOM File Meta Information (Group 2) — always ExplicitVRLittleEndian
	// per PS3.10 §7.1, regardless of the dataset transfer syntax.
	enc := dicomio.NewEncoderWithTransferSyntax(tmpFile, transferSyntaxUID)
	dicom.WriteFileHeader(enc, []*dicom.Element{
		dicom.MustNewElement(dicomtag.TransferSyntaxUID, transferSyntaxUID),
		dicom.MustNewElement(dicomtag.MediaStorageSOPClassUID, sopClassUID),
		dicom.MustNewElement(dicomtag.MediaStorageSOPInstanceUID, sopInstanceUID),
	})
	if encErr := enc.Error(); encErr != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return dimse.Status{Status: dimse.CStoreOutOfResources, ErrorComment: encErr.Error()}
	}

	// Stream the dataset payload (everything after Group 2) directly to disk.
	if _, err := io.Copy(tmpFile, dataReader); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return dimse.Status{Status: dimse.CStoreOutOfResources, ErrorComment: err.Error()}
	}
	tmpFile.Close()

	// Re-open the completed temp file (omitting pixel data) to extract the
	// metadata tags needed to build the organized subfolder path.
	var patientName, patientID, studyDesc, studyDate, seriesDesc, seriesNumber string
	if ds, parseErr := dicom.ReadDataSetFromFile(tmpPath, dicom.ReadOptions{DropPixelData: true}); parseErr == nil {
		patientName = scpStringTag(ds, dicomtag.PatientName)
		patientID = scpStringTag(ds, dicomtag.PatientID)
		studyDesc = scpStringTag(ds, dicomtag.StudyDescription)
		studyDate = scpStringTag(ds, dicomtag.StudyDate)
		seriesDesc = scpStringTag(ds, dicomtag.SeriesDescription)
		seriesNumber = scpStringTag(ds, dicomtag.SeriesNumber)
	}

	dest := s.organizeFilePath(patientName, patientID, studyDesc, studyDate, seriesDesc, seriesNumber, sopInstanceUID)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		os.Remove(tmpPath)
		return dimse.Status{Status: dimse.CStoreOutOfResources, ErrorComment: err.Error()}
	}

	// os.Rename is atomic on the same filesystem. Fall back to copy+delete if
	// downloadDir and the OS temp directory are on different volumes.
	if err := os.Rename(tmpPath, dest); err != nil {
		if copyErr := scpCopyFile(tmpPath, dest); copyErr != nil {
			os.Remove(tmpPath)
			return dimse.Status{Status: dimse.CStoreOutOfResources, ErrorComment: copyErr.Error()}
		}
		os.Remove(tmpPath)
	}

	if s.OnFileReceived != nil {
		s.OnFileReceived(dest)
	}
	return dimse.Success
}

// organizeFilePath builds the destination path for a received DICOM file using
// the fixed structure:
//
//	<DownloadDir>/<Patient Name> (MRN)/<Study Description> (StudyDate)/<Series Description> (SeriesNumber)/<sopInstanceUID>.dcm
func (s *StorageSCP) organizeFilePath(patientName, patientID, studyDesc, studyDate, seriesDesc, seriesNumber, sopInstanceUID string) string {
	if patientName == "" {
		patientName = "Unknown Patient"
	}
	patFolder := sanitize(patientName)
	if patientID != "" {
		patFolder += " (" + sanitize(patientID) + ")"
	}

	if studyDesc == "" {
		studyDesc = "Unknown Study"
	}
	studyFolder := sanitize(studyDesc)
	if studyDate != "" {
		studyFolder += " (" + sanitize(studyDate) + ")"
	}

	if seriesDesc == "" {
		seriesDesc = "Unknown Series"
	}
	seriesFolder := sanitize(seriesDesc)
	if seriesNumber != "" {
		seriesFolder += " (" + sanitize(seriesNumber) + ")"
	}

	filename := sanitize(sopInstanceUID) + ".dcm"
	if filename == ".dcm" {
		filename = fmt.Sprintf("%d.dcm", time.Now().UnixNano())
	}
	return filepath.Join(s.downloadDir, patFolder, studyFolder, seriesFolder, filename)
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

// scpStringTag returns the string value of a DICOM tag from a dataset, or ""
// if the tag is absent or cannot be decoded as a string.
func scpStringTag(ds *dicom.DataSet, tag dicomtag.Tag) string {
	elem, err := ds.FindElementByTag(tag)
	if err != nil {
		return ""
	}
	s, _ := elem.GetString()
	return s
}

// scpCopyFile copies src to dst byte-for-byte. Used as a fallback when
// os.Rename fails across filesystem boundaries.
func scpCopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
