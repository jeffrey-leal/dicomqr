package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	netdicom "github.com/algm/go-netdicom"
	"github.com/algm/go-netdicom/dimse"
	dicom "github.com/grailbio/go-dicom"
	"github.com/grailbio/go-dicom/dicomio"
	"github.com/grailbio/go-dicom/dicomtag"
	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// StorageSCP is an embedded C-STORE SCP that listens for files pushed by a
// PACS in response to a C-MOVE-RQ. It writes received DICOM files to a
// structured subfolder tree under DownloadDir.
type StorageSCP struct {
	localAETitle string
	port         int

	// downloadDir is guarded by dirMu (Phase 1-B)
	dirMu       sync.RWMutex
	downloadDir string

	listenAddr string // actual bound address, set in Start()

	// onFileReceived is guarded by cbMu (Phase 1-C)
	cbMu           sync.Mutex
	onFileReceived func(path string)

	// running, cancel, and ln are guarded by cancelMu; running is also readable
	// via atomic load for lock-free hot-path checks.
	cancelMu sync.Mutex
	running  atomic.Bool
	cancel   context.CancelFunc
	ln       net.Listener

	// Transfer policy, guarded by tsMu. requiredTS, when non-empty, is the
	// transfer syntax every received file must be in on disk. Negotiation
	// offers it first, plus the syntaxes the receive path can convert locally
	// (see acceptedSyntaxesFor); anything arriving in a non-required accepted
	// syntax is transcoded before it reaches its destination. Empty accepts
	// everything as stored. Re-read per incoming connection, so a change
	// applies without restarting the listener.
	tsMu       sync.RWMutex
	requiredTS string

	// converted counts files transcoded locally since the SCP was created;
	// the retrieve loop reads deltas to report server non-compliance.
	converted atomic.Int64

	// skipped counts objects discarded because local conversion to the
	// required syntax failed (typically screenshots/graphics stored under an
	// image SOP class that the built-in decoders cannot handle); the retrieve
	// loop reads deltas to report them at the end of a retrieve.
	skipped atomic.Int64
}

// DownloadDir returns the download directory (thread-safe, Phase 1-B).
func (s *StorageSCP) DownloadDir() string {
	s.dirMu.RLock()
	defer s.dirMu.RUnlock()
	return s.downloadDir
}

// SetDownloadDir updates the download directory (thread-safe, Phase 1-B).
func (s *StorageSCP) SetDownloadDir(d string) {
	s.dirMu.Lock()
	defer s.dirMu.Unlock()
	s.downloadDir = d
}

// SetTransferPolicy sets the single transfer syntax UID required for incoming
// files ("" accepts everything). Safe to call while the SCP is running — new
// associations pick up the current value.
func (s *StorageSCP) SetTransferPolicy(requiredTS string) {
	s.tsMu.Lock()
	defer s.tsMu.Unlock()
	s.requiredTS = requiredTS
}

// transferPolicy returns the currently required transfer syntax UID ("" = any).
func (s *StorageSCP) transferPolicy() string {
	s.tsMu.RLock()
	defer s.tsMu.RUnlock()
	return s.requiredTS
}

// ConvertedCount returns the number of received files transcoded locally since
// the SCP was created. Callers snapshot it around a retrieve to report how
// many files the server did not deliver in the required syntax.
func (s *StorageSCP) ConvertedCount() int64 { return s.converted.Load() }

// SkippedCount returns the number of received objects discarded because they
// could not be converted to the required transfer syntax. Callers snapshot it
// around a retrieve to report the skips in the final summary.
func (s *StorageSCP) SkippedCount() int64 { return s.skipped.Load() }

// SetOnFileReceived sets the callback (thread-safe, Phase 1-C).
func (s *StorageSCP) SetOnFileReceived(fn func(path string)) {
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	s.onFileReceived = fn
}

// OnFileReceived returns the current callback (thread-safe, Phase 1-C).
func (s *StorageSCP) OnFileReceived() func(string) {
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	return s.onFileReceived
}

// callOnFileReceived invokes the callback if set (thread-safe, Phase 1-C).
func (s *StorageSCP) callOnFileReceived(path string) {
	s.cbMu.Lock()
	fn := s.onFileReceived
	s.cbMu.Unlock()
	if fn != nil {
		fn(path)
	}
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
	s.cancelMu.Lock()
	if s.running.Load() {
		s.cancelMu.Unlock()
		return nil
	}
	s.cancelMu.Unlock()

	if s.DownloadDir() == "" {
		return errors.New("download directory is not configured")
	}
	if err := os.MkdirAll(s.DownloadDir(), 0o755); err != nil {
		return fmt.Errorf("cannot create download directory: %w", err)
	}
	cleanupStaleTempFiles(s.DownloadDir())

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
	//
	// Retry the bind briefly to ride out the short window after an unclean exit
	// (e.g. the previous instance was killed via Task Manager, bypassing the
	// graceful shutdown) during which the OS may not yet have released the port.
	// If the port is still taken after the retries, return a clear, actionable
	// message rather than the raw socket error.
	addr := fmt.Sprintf(":%d", s.port)
	var ln net.Listener
	var err error
	for attempt := 0; ; attempt++ {
		ln, err = net.Listen("tcp4", addr)
		if err == nil || !errors.Is(err, syscall.EADDRINUSE) || attempt >= 2 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("port %d is already in use — another copy of dicomqr may still be running. "+
				"Close it (check Task Manager for dicomqr.exe) and try connecting again", s.port)
		}
		return fmt.Errorf("storage SCP: listen on port %d: %w", s.port, err)
	}
	s.listenAddr = ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	s.cancelMu.Lock()
	s.cancel = cancel
	s.ln = ln
	s.running.Store(true)
	s.cancelMu.Unlock()

	// Close the listener when the context is cancelled (Stop() called).
	go func() { <-ctx.Done(); ln.Close() }()
	go func() {
		defer ln.Close()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Re-read the transfer policy per association so a requirement
			// change made while running applies to the next retrieve.
			p := params
			policy := "all transfer syntaxes"
			if req := s.transferPolicy(); req != "" {
				p.AcceptedTransferSyntaxes = acceptedSyntaxesFor(req)
				policy = fmt.Sprintf("required transfer syntax %s (accepting %v for local conversion)",
					req, p.AcceptedTransferSyntaxes[1:])
			}
			logInfo("scp: association from %s (%s)", conn.RemoteAddr(), policy)
			go netdicom.RunProviderForConn(ctx, conn, p)
		}
	}()
	return nil
}

// ListenAddr returns the address the SCP is listening on (e.g. "0.0.0.0:11112").
func (s *StorageSCP) ListenAddr() string { return s.listenAddr }

// Stop shuts down the listener and cancels all in-flight connections.
func (s *StorageSCP) Stop() {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	if !s.running.Load() {
		return
	}
	s.running.Store(false)
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	// Close the listener synchronously so the port is released immediately on
	// return, rather than asynchronously via the context-cancel goroutine. This
	// guarantees the port is free for an immediate application restart.
	if s.ln != nil {
		s.ln.Close()
		s.ln = nil
	}
}

// IsRunning reports whether the SCP is currently listening.
func (s *StorageSCP) IsRunning() bool { return s.running.Load() }

// handleCStore writes one incoming DICOM object to a structured subfolder.
// Strategy: stream the payload to a temp file first (avoiding memory pressure
// from large pixel data), parse the metadata tags needed for the folder path
// (skipping pixel data), then rename the temp file to its final destination.
func (s *StorageSCP) handleCStore(
	transferSyntaxUID, sopClassUID, sopInstanceUID string,
	dataReader io.Reader,
) (st dimse.Status) {
	// The dispatcher runs this handler on its own goroutine, where an escaped
	// panic would kill the entire process (and with it the listener and every
	// other association). Malformed input can genuinely panic here: the header
	// encode uses MustNewElement and the transcode path parses with a library
	// that panics on corrupt datasets. Convert a panic into a C-STORE failure
	// response so the PACS sees the error and the app keeps running; any
	// orphaned .recv_*.tmp file is removed by cleanupStaleTempFiles on the
	// next Start.
	defer func() {
		if r := recover(); r != nil {
			logError("scp: PANIC receiving %s: %v\n%s", sopInstanceUID, r, debug.Stack())
			st = dimse.Status{Status: dimse.CStoreOutOfResources,
				ErrorComment: fmt.Sprintf("receiver internal error: %v", r)}
		}
	}()
	// Create the temp file inside downloadDir so the later rename stays on the
	// same filesystem and avoids cross-device rename failures.
	tmpFile, err := os.CreateTemp(s.DownloadDir(), ".recv_*.tmp")
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

	// Re-open the completed temp file to extract the metadata tags needed to
	// build the organized subfolder path. The streaming parser stops at group
	// 0x0020, so SR Content Sequences (0x0040+) are never visited.
	patientName, patientID, studyDesc, studyDate, seriesDesc, seriesNumber := scpParseMetadata(tmpPath)

	dest := organizeFilePath(s.DownloadDir(), patientName, patientID, studyDesc, studyDate, seriesDesc, seriesNumber, sopInstanceUID)

	// Skip writing if the file already exists; discard the temp file and return
	// success so the PACS doesn't retry. callOnFileReceived is not invoked for
	// a skipped file — the UI file count reflects only newly written files.
	// Exception: when a specific transfer syntax is required and the existing
	// copy is in a different one (downloaded before the requirement was set),
	// fall through and replace it with the incoming copy, which the conversion
	// step below guarantees is in the required syntax.
	req := s.transferPolicy()
	if _, statErr := os.Stat(dest); statErr == nil {
		if req == "" || fileTransferSyntaxUID(dest) == req {
			os.Remove(tmpPath)
			return dimse.Success
		}
	}

	// Enforce the required transfer syntax BEFORE the file reaches its final
	// destination: a file in the wrong syntax is transcoded in place while
	// still a temp file, so the destination only ever holds conforming files.
	// An object that cannot be converted — typically a screenshot or vendor
	// graphic stored under an image SOP class whose pixel data the built-in
	// decoders cannot handle — is skipped: nothing lands in the download
	// folder, the skip is reported in the Activity Log and counted for the
	// end-of-retrieve summary, and Success is returned so the PACS keeps
	// sending the rest of the retrieve.
	if req != "" && transferSyntaxUID != req {
		changed, convErr := transcodeDICOMFile(tmpPath, req)
		if convErr != nil {
			s.skipped.Add(1)
			logWarn("scp: SKIPPED %s — cannot convert to %s: %v (series %q, SOP class %s); object not saved, retrieve continues",
				sopInstanceUID, transferSyntaxLabel(req), convErr, seriesDesc, sopClassUID)
			os.Remove(tmpPath)
			return dimse.Success
		}
		if changed {
			s.converted.Add(1)
		}
	}

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

	s.callOnFileReceived(dest)
	return dimse.Success
}

// patientFolderName, studyFolderName and seriesFolderName build one path
// component of the organized hierarchy from its source tag values: a
// placeholder when the descriptive part is empty, sanitized, with the second
// part appended in parentheses only when it is present, truncated to 64 runes
// (Phase 3-F). organizeFilePath, resultsModel.localFolderFor and the
// modification engine's export layout (modifyengine.go) all build path
// components through these — one authority for what a folder is called,
// rather than three copies that could drift.
func patientFolderName(name, id string) string {
	if name == "" {
		name = "Unknown Patient"
	}
	f := sanitize(name)
	if id != "" {
		f += " (" + sanitize(id) + ")"
	}
	return safePathComponent(truncateRunes(f, 64))
}

func studyFolderName(desc, date string) string {
	if desc == "" {
		desc = "Unknown Study"
	}
	f := sanitize(desc)
	if date != "" {
		f += " (" + sanitize(date) + ")"
	}
	return safePathComponent(truncateRunes(f, 64))
}

func seriesFolderName(desc, number string) string {
	if desc == "" {
		desc = "Unknown Series"
	}
	f := sanitize(desc)
	if number != "" {
		f += " (" + sanitize(number) + ")"
	}
	return safePathComponent(truncateRunes(f, 64))
}

// safePathComponent defuses a folder name that is nothing but dots. "." and
// ".." are directory references, not names: sanitize passes them through (they
// hold none of the characters it strips), and filepath.Join then Cleans them,
// so a PatientName of ".." used to place the received file one level ABOVE the
// download folder. Every DICOM string that names a folder here comes straight
// out of a file the application did not write.
//
// The whole-string dot test rather than an exact "." / ".." match also covers
// "..." and longer runs, which Windows cannot create as a directory at all. No
// real patient name, study description or series description is all dots, so
// this can never rename a legitimate folder — which matters, because renaming
// one would orphan everything already downloaded under it.
//
// The "_" prefix is the same defusing sanitize already applies to reserved
// device names. It is applied after truncation, since truncating a long value
// could otherwise produce an all-dots component from one that was not.
func safePathComponent(s string) string {
	if s == "" || strings.Trim(s, ".") != "" {
		return s
	}
	return "_" + s
}

// organizeFilePath builds the destination path for a received DICOM file using
// the fixed structure:
//
//	<downloadDir>/<Patient Name> (MRN)/<Study Description> (StudyDate)/<Series Description> (SeriesNumber)/<sopInstanceUID>.dcm
func organizeFilePath(downloadDir, patientName, patientID, studyDesc, studyDate, seriesDesc, seriesNumber, sopInstanceUID string) string {
	patFolder := patientFolderName(patientName, patientID)
	studyFolder := studyFolderName(studyDesc, studyDate)
	seriesFolder := seriesFolderName(seriesDesc, seriesNumber)

	filename := sanitize(sopInstanceUID) + ".dcm"
	if filename == ".dcm" {
		filename = fmt.Sprintf("%d.dcm", time.Now().UnixNano())
	}

	full := filepath.Join(downloadDir, patFolder, studyFolder, seriesFolder, filename)
	// Fall back to flat layout when the full path would exceed 255 characters.
	if len(full) > 255 {
		full = filepath.Join(downloadDir, filename)
	}
	// Second layer under safePathComponent: make containment a property of this
	// function rather than of having got the character rules exactly right. The
	// components are built from tag values in a file the application did not
	// write, and everything downstream — the catalog, the tree, delete, export —
	// assumes the result is inside the download folder.
	//
	// The downloadDir != "" guard keeps an unconfigured folder — where every
	// path is relative and nothing is "inside" anything — from logging this per
	// received file; that case is already degenerate and reported elsewhere.
	if downloadDir != "" && !pathWithinDir(filepath.Dir(full), downloadDir) {
		logWarn("scp: %q would place a file outside the download folder — using the flat layout instead", full)
		full = filepath.Join(downloadDir, filename)
	}
	return full
}

// windowsReserved is the set of device names forbidden as path components on Windows.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// sanitize strips characters that are unsafe in path components and prefixes
// Windows reserved device names with "_" (Phase 3-G).
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	result := b.String()
	if windowsReserved[strings.ToUpper(result)] {
		return "_" + result
	}
	return result
}

// truncateRunes limits a string to maxRunes Unicode code points.
func truncateRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes])
}

// scpParseMetadata extracts the six metadata strings used to build the
// organized folder hierarchy from a received DICOM file. It reads elements
// sequentially via a streaming parser and stops as soon as it passes group
// 0x0020, so complex Content Sequences present in SR and other non-image
// modalities (group 0x0040+) are never visited. Returns empty strings on any
// parse failure, which causes the caller to fall back to a flat layout.
func scpParseMetadata(path string) (patientName, patientID, studyDesc, studyDate, seriesDesc, seriesNumber string) {
	// handleCStore recovers panics for the whole C-STORE handler, but saveGetFile
	// reaches this from the C-GET callback with no such cover, so the boundary
	// belongs here as well. Whatever was read before the panic is returned: a
	// partial folder name is the same outcome a partial parse already gives.
	defer recoverParserPanic(path)
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	p, err := sdicom.NewParser(f, info.Size(), nil, sdicom.SkipPixelData())
	if err != nil {
		return
	}
	for {
		elem, err := p.Next()
		if err != nil {
			break
		}
		if elem.Tag.Group > 0x0020 {
			break
		}
		strs, ok := elem.Value.GetValue().([]string)
		if !ok || len(strs) == 0 {
			continue
		}
		val := strings.TrimSpace(strs[0])
		switch elem.Tag {
		case tag.PatientName:
			patientName = val
		case tag.PatientID:
			patientID = val
		case tag.StudyDescription:
			studyDesc = val
		case tag.StudyDate:
			studyDate = val
		case tag.SeriesDescription:
			seriesDesc = val
		case tag.SeriesNumber:
			seriesNumber = val
		}
	}
	return
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

// saveGetFile writes a C-GET instance payload to the download directory using
// the same organized subfolder hierarchy as the C-STORE SCP. The data argument
// is the raw DICOM dataset bytes as received from the C-GET callback (no Group
// 2 prefix); this function prepends the proper DICOM File Meta Information
// header before writing. Returns the path of the saved file, whether the
// payload was transcoded locally to requiredTS, and whether the object was
// skipped because conversion failed.
// requiredTS, when non-empty, is enforced exactly as in handleCStore: an
// arriving file in a different syntax is converted before it reaches its
// destination (an unconvertible object is skipped — logged, counted by the
// caller, nothing saved — so the retrieve continues), and an existing on-disk
// copy in a different syntax is overwritten instead of skipped.
func saveGetFile(downloadDir, transferSyntaxUID, sopClassUID, sopInstanceUID string, data []byte, requiredTS string) (path string, converted, skipped bool, err error) {
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return "", false, false, fmt.Errorf("cannot create download directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(downloadDir, ".recv_*.tmp")
	if err != nil {
		return "", false, false, err
	}
	tmpPath := tmpFile.Name()

	enc := dicomio.NewEncoderWithTransferSyntax(tmpFile, transferSyntaxUID)
	dicom.WriteFileHeader(enc, []*dicom.Element{
		dicom.MustNewElement(dicomtag.TransferSyntaxUID, transferSyntaxUID),
		dicom.MustNewElement(dicomtag.MediaStorageSOPClassUID, sopClassUID),
		dicom.MustNewElement(dicomtag.MediaStorageSOPInstanceUID, sopInstanceUID),
	})
	if encErr := enc.Error(); encErr != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", false, false, encErr
	}

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", false, false, err
	}
	tmpFile.Close()

	patientName, patientID, studyDesc, studyDate, seriesDesc, seriesNumber := scpParseMetadata(tmpPath)

	dest := organizeFilePath(downloadDir, patientName, patientID, studyDesc, studyDate, seriesDesc, seriesNumber, sopInstanceUID)

	// Skip writing if the file already exists; discard the temp file.
	// The caller does not invoke the status callback for skipped files.
	// Exception: replace an existing copy whose transfer syntax differs from
	// the required one (mirrors handleCStore).
	if _, statErr := os.Stat(dest); statErr == nil {
		if requiredTS == "" || fileTransferSyntaxUID(dest) == requiredTS {
			os.Remove(tmpPath)
			return dest, false, false, nil
		}
	}

	// Enforce the required transfer syntax before the file reaches its final
	// destination (mirrors handleCStore): an unconvertible object is skipped —
	// logged here, counted by the caller, nothing saved — so the retrieve
	// continues instead of aborting.
	if requiredTS != "" && transferSyntaxUID != requiredTS {
		changed, convErr := transcodeDICOMFile(tmpPath, requiredTS)
		if convErr != nil {
			logWarn("c-get: SKIPPED %s — cannot convert to %s: %v (series %q, SOP class %s); object not saved, retrieve continues",
				sopInstanceUID, transferSyntaxLabel(requiredTS), convErr, seriesDesc, sopClassUID)
			os.Remove(tmpPath)
			return "", false, true, nil
		}
		converted = changed
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		os.Remove(tmpPath)
		return "", false, false, err
	}

	if err := os.Rename(tmpPath, dest); err != nil {
		if copyErr := scpCopyFile(tmpPath, dest); copyErr != nil {
			os.Remove(tmpPath)
			return "", false, false, copyErr
		}
		os.Remove(tmpPath)
	}

	return dest, converted, false, nil
}

// dirWritable verifies that dir exists (creating it if necessary) and is
// writable, by creating and removing a probe file. Returns a descriptive error
// so the caller can fail a retrieve up front rather than per received file.
func dirWritable(dir string) error {
	if dir == "" {
		return errors.New("download directory is not configured")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create download directory: %w", err)
	}
	probe, err := os.CreateTemp(dir, ".write_test_*.tmp")
	if err != nil {
		return fmt.Errorf("download directory is not writable: %w", err)
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)
	return nil
}

// cleanupStaleTempFiles removes any .recv_*.tmp or .transcode_*.tmp files left
// in dir by a previous session that was killed mid-transfer or mid-conversion.
func cleanupStaleTempFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		stale := strings.HasPrefix(n, ".recv_") || strings.HasPrefix(n, ".transcode_")
		if !e.IsDir() && stale && strings.HasSuffix(n, ".tmp") {
			os.Remove(filepath.Join(dir, n))
		}
	}
}
