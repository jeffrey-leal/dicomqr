package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	netdicom "github.com/algm/go-netdicom"
	"github.com/algm/go-netdicom/dimse"
	"github.com/algm/go-netdicom/sopclass"
	dicom "github.com/grailbio/go-dicom"
	"github.com/grailbio/go-dicom/dicomio"
	"github.com/grailbio/go-dicom/dicomtag"
)

// proposedTransferSyntaxes returns the transfer syntaxes offered in
// A-ASSOCIATE-RQ for a C-GET association. When the profile requires a specific
// syntax, the proposal is that syntax first plus the syntaxes the receive path
// can convert locally (see acceptedSyntaxesFor) — a transcoding server sends
// the required syntax, a stored-form server sends what it has and the file is
// converted on receipt. A server limited to something outside the list fails
// its sub-operations, which aborts the retrieve. The unrestricted default,
// dicomio.StandardTransferSyntaxes, contains no compressed syntax.
//
// Always a fresh slice: NewServiceUser canonicalises the list it is given in
// place, so handing it the package-level default would have every concurrent
// C-GET of a parallel retrieve writing into one shared slice.
func proposedTransferSyntaxes(p ServerProfile) []string {
	if req := p.requiredTransferSyntax(); req != "" {
		return acceptedSyntaxesFor(req)
	}
	return slices.Clone(dicomio.StandardTransferSyntaxes)
}

// FindResult holds one C-FIND response item. Err is set on error items;
// all other fields are zero when Err != nil.
type FindResult struct {
	Err               error
	Level             string
	PatientName       string
	PatientID         string
	StudyInstanceUID  string
	StudyDate         string
	StudyDescription  string
	AccessionNumber   string
	ModalitiesInStudy string
	SeriesInstanceUID string
	SeriesNumber      string
	SeriesDescription string
	Modality          string
	NumInstances      int
	SOPInstanceUID    string
	InstanceNumber    int
}

// MoveProgress reports sub-operation counts from a C-MOVE-RSP.
type MoveProgress struct {
	Remaining int
	Completed int
	Failed    int
	Warning   int
}

// DicomClient is an SCU that wraps the go-netdicom library.
type DicomClient struct {
	profile      ServerProfile
	localAETitle string
}

// NewDicomClient creates a client configured for the given server profile.
func NewDicomClient(profile ServerProfile, localAETitle string) *DicomClient {
	return &DicomClient{profile: profile, localAETitle: localAETitle}
}

// Echo sends a C-ECHO (Verification SOP Class 1.2.840.10008.1.1) to verify
// DICOM connectivity with the configured server. The association is opened,
// the echo is sent, and the association is released — all within one call.
// Returns nil on a successful Status 0000H response.
func (c *DicomClient) Echo(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
		CalledAETitle:  c.profile.RemoteAETitle,
		CallingAETitle: c.localAETitle,
		SOPClasses:     sopclass.VerificationClasses,
	})
	if err != nil {
		return fmt.Errorf("c-echo: create service user: %w", err)
	}

	type result struct{ err error }
	done := make(chan result, 1)

	go func() {
		defer su.Release()
		su.Connect(fmt.Sprintf("%s:%d", c.profile.Host, c.profile.Port))
		done <- result{su.CEcho()}
	}()

	select {
	case r := <-done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// EchoConcurrent opens n associations to the server at once, sends a C-ECHO
// on each, and releases them only after every one has answered or failed —
// so all n are held open together, which is what a parallel retrieve needs
// the server to allow. errs[i] is association i's outcome; a refusal keeps
// the library's *netdicom.AssociationRejectedError so the caller can name the
// reason. It says nothing about whether a C-MOVE would get faster: a server
// may accept several associations and still send its deliveries one at a time.
func (c *DicomClient) EchoConcurrent(ctx context.Context, n int) []error {
	errs := make([]error, n)
	sus := make([]*netdicom.ServiceUser, n)
	for i := range n {
		su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
			CalledAETitle:  c.profile.RemoteAETitle,
			CallingAETitle: c.localAETitle,
			SOPClasses:     sopclass.VerificationClasses,
		})
		if err != nil {
			errs[i] = fmt.Errorf("c-echo: create service user: %w", err)
			continue
		}
		sus[i] = su
	}

	// Results are written under mu: after a cancel the function may return
	// while a stuck echo is still running, and its late write must not race
	// the copy handed back.
	var mu sync.Mutex
	finished := make([]bool, n)
	addr := fmt.Sprintf("%s:%d", c.profile.Host, c.profile.Port)
	var wg sync.WaitGroup
	for i, su := range sus {
		if su == nil {
			finished[i] = true
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			su.Connect(addr)
			err := su.CEcho()
			mu.Lock()
			errs[i], finished[i] = err, true
			mu.Unlock()
		}()
	}
	all := make(chan struct{})
	go func() { wg.Wait(); close(all) }()

	select {
	case <-all:
		for _, su := range sus {
			if su != nil {
				su.Release()
			}
		}
	case <-ctx.Done():
		for _, su := range sus {
			if su != nil {
				su.Abort()
			}
		}
		select {
		case <-all:
		case <-time.After(5 * time.Second):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	out := slices.Clone(errs)
	for i := range out {
		if !finished[i] {
			out[i] = ctx.Err()
		}
	}
	return out
}

// Find sends a C-FIND (Study Root or Patient Root QR) at the given query level
// and streams results on the returned channel. The channel is closed when the
// query completes or ctx is cancelled. A non-nil error is returned only when the
// ServiceUser cannot be created. Association failures (e.g. the connection
// dropped mid-session) and query rejections are reported in-band as a
// FindResult with Err set, so callers can distinguish a genuinely empty result
// from a failed query.
func (c *DicomClient) Find(ctx context.Context, level string, params map[string]string) (<-chan FindResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
		CalledAETitle:  c.profile.RemoteAETitle,
		CallingAETitle: c.localAETitle,
		SOPClasses:     sopclass.QRFindClasses,
	})
	if err != nil {
		return nil, fmt.Errorf("c-find: create service user: %w", err)
	}

	out := make(chan FindResult, 128)

	go func() {
		defer close(out)
		defer su.Release()
		su.Connect(fmt.Sprintf("%s:%d", c.profile.Host, c.profile.Port))

		for r := range su.CFind(levelToQRLevel(level), buildFindFilter(level, params)) {
			if r.Err != nil {
				select {
				case out <- FindResult{Err: r.Err}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case out <- elementsToFindResult(r.Elements):
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

// Move sends a C-MOVE-RQ (PS3.4 C.4.2) for the given UIDs, directing the PACS
// to push files to destAE. onProgress is called for each C-MOVE-RSP pending
// response. Returns nil when the final response carries StatusSuccess (0000H).
// patientID is included in the filter when non-empty; required by PACS that
// mandate it at STUDY level (Patient Root model per PS3.4 C.4.2.1).
func (c *DicomClient) Move(ctx context.Context, level, patientID, studyUID, seriesUID string, destAE string, onProgress func(MoveProgress)) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
		CalledAETitle:  c.profile.RemoteAETitle,
		CallingAETitle: c.localAETitle,
		SOPClasses:     sopclass.QRMoveClasses,
	})
	if err != nil {
		return fmt.Errorf("c-move: create service user: %w", err)
	}

	type result struct{ err error }
	done := make(chan result, 1)

	go func() {
		var err error
		defer func() { done <- result{err} }()
		defer su.ReleaseAndWait(retrieveReleaseWait)
		su.Connect(fmt.Sprintf("%s:%d", c.profile.Host, c.profile.Port))

		progressFn := func(p netdicom.CMoveProgress) {
			if onProgress != nil {
				onProgress(MoveProgress{
					Remaining: p.Remaining,
					Completed: p.Completed,
					Failed:    p.Failed,
					Warning:   p.Warning,
				})
			}
		}
		err = su.CMove(levelToQRLevel(level), destAE, buildMoveFilter(level, patientID, studyUID, seriesUID), progressFn)
	}()

	select {
	case r := <-done:
		return r.err
	case <-ctx.Done():
		abortAndReap(su, done)
		return ctx.Err()
	}
}

// retrieveReleaseWait bounds how long Move and Get wait, after releasing their
// association, for the connection to close before they return. Returning only
// once it has keeps a retrieve's associations within its parallel limit as
// the server counts them: the next target's association would otherwise open
// while this one is still releasing, and a server allowing exactly that many
// could refuse it. A peer that never answers the release costs at most this.
const retrieveReleaseWait = 3 * time.Second

// abortAndReap force-closes a wedged association with A-ABORT and waits
// (briefly) for the blocked DIMSE goroutine to finish, so a cancelled or
// stalled retrieve does not leak the goroutine and the TCP connection.
func abortAndReap[T any](su *netdicom.ServiceUser, done <-chan T) {
	su.Abort()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// Get sends a C-GET-RQ (PS3.4 C.4.3) for the given UIDs, causing the PACS to
// return DICOM instances over the same association. onStore is called once per
// received instance; returning a non-nil error sends CStoreOutOfResources and
// aborts the retrieve. onProgress (may be nil) is called for each C-GET-RSP
// with sub-operation counts, exactly as in Move. C-GET does not require a
// separate inbound C-STORE SCP.
// Returns nil when the final response carries StatusSuccess (0000H).
func (c *DicomClient) Get(ctx context.Context, level, patientID, studyUID, seriesUID string,
	onStore func(transferSyntaxUID, sopClassUID, sopInstanceUID string, data []byte) error,
	onProgress func(MoveProgress)) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
		CalledAETitle:    c.profile.RemoteAETitle,
		CallingAETitle:   c.localAETitle,
		SOPClasses:       sopclass.QRGetClasses,
		TransferSyntaxes: proposedTransferSyntaxes(c.profile),
	})
	if err != nil {
		return fmt.Errorf("c-get: create service user: %w", err)
	}

	type result struct{ err error }
	done := make(chan result, 1)

	go func() {
		var err error
		defer func() { done <- result{err} }()
		defer su.ReleaseAndWait(retrieveReleaseWait)
		su.Connect(fmt.Sprintf("%s:%d", c.profile.Host, c.profile.Port))

		progressFn := func(p netdicom.CMoveProgress) {
			if onProgress != nil {
				onProgress(MoveProgress{
					Remaining: p.Remaining,
					Completed: p.Completed,
					Failed:    p.Failed,
					Warning:   p.Warning,
				})
			}
		}
		err = su.CGetWithProgress(levelToQRLevel(level), buildMoveFilter(level, patientID, studyUID, seriesUID),
			progressFn,
			func(txUID, scUID, siUID string, data []byte) dimse.Status {
				if storeErr := onStore(txUID, scUID, siUID, data); storeErr != nil {
					return dimse.Status{Status: dimse.CStoreOutOfResources, ErrorComment: storeErr.Error()}
				}
				return dimse.Success
			})
	}()

	select {
	case r := <-done:
		return r.err
	case <-ctx.Done():
		abortAndReap(su, done)
		return ctx.Err()
	}
}

// levelToQRLevel maps the query-level string used by the UI to the go-netdicom
// QRLevel constant. Defaults to Study Root if the level is unrecognised.
func levelToQRLevel(level string) netdicom.QRLevel {
	switch strings.ToUpper(level) {
	case "SERIES":
		return netdicom.QRLevelSeries
	case "PATIENT":
		return netdicom.QRLevelPatient
	case "PATIENT-STUDY-ONLY":
		return netdicom.QRLevelPatientStudyOnly
	default:
		return netdicom.QRLevelStudy
	}
}

// buildFindFilter builds a C-FIND identifier dataset from the UI query params.
// Empty-value elements act as return keys; non-empty values are match criteria.
// At SERIES level only StudyInstanceUID is used as a match key; all other fields
// are return keys. StudyDate uses DICOM range syntax (PS3.4 C.2.2.2.5).
func buildFindFilter(level string, params map[string]string) []*dicom.Element {
	if strings.ToUpper(level) == "SERIES" {
		return []*dicom.Element{
			dicom.MustNewElement(dicomtag.SpecificCharacterSet, "ISO_IR 192"),
			dicom.MustNewElement(dicomtag.StudyInstanceUID, params["StudyInstanceUID"]),
			dicom.MustNewElement(dicomtag.SeriesInstanceUID, ""),
			dicom.MustNewElement(dicomtag.SeriesNumber, ""),
			dicom.MustNewElement(dicomtag.Modality, ""),
			dicom.MustNewElement(dicomtag.SeriesDescription, ""),
			dicom.MustNewElement(dicomtag.NumberOfSeriesRelatedInstances, ""),
		}
	}
	dateRange := buildDateRange(params["StudyDateFrom"], params["StudyDateTo"])
	return []*dicom.Element{
		dicom.MustNewElement(dicomtag.SpecificCharacterSet, "ISO_IR 192"),
		dicom.MustNewElement(dicomtag.PatientName, params["PatientName"]),
		dicom.MustNewElement(dicomtag.PatientID, params["PatientID"]),
		dicom.MustNewElement(dicomtag.AccessionNumber, params["AccessionNumber"]),
		dicom.MustNewElement(dicomtag.StudyDate, dateRange),
		dicom.MustNewElement(dicomtag.StudyInstanceUID, ""),
		dicom.MustNewElement(dicomtag.StudyDescription, ""),
		dicom.MustNewElement(dicomtag.ModalitiesInStudy, params["ModalitiesInStudy"]),
	}
}

// buildMoveFilter builds the C-MOVE identifier dataset for the given UIDs.
// PatientID is included when non-empty — required by PACS using Patient Root
// at STUDY level (PS3.4 C.4.2.1). At SERIES level SeriesInstanceUID is also
// included so the PACS can scope the sub-operations correctly (PS3.4 C.4.2).
func buildMoveFilter(level, patientID, studyUID, seriesUID string) []*dicom.Element {
	var filter []*dicom.Element
	if patientID != "" {
		filter = append(filter, dicom.MustNewElement(dicomtag.PatientID, patientID))
	}
	filter = append(filter, dicom.MustNewElement(dicomtag.StudyInstanceUID, studyUID))
	if level == "SERIES" && seriesUID != "" {
		filter = append(filter, dicom.MustNewElement(dicomtag.SeriesInstanceUID, seriesUID))
	}
	return filter
}

// buildDateRange encodes a DICOM date range string from UI from/to values.
// Returns "" (match-all) when both are empty.
func buildDateRange(from, to string) string {
	switch {
	case from == "" && to == "":
		return ""
	case from == "":
		return "-" + to
	case to == "":
		return from + "-"
	default:
		return from + "-" + to
	}
}

// elementsToFindResult extracts a FindResult from the elements in one C-FIND
// response dataset. Unknown tags are silently ignored.
func elementsToFindResult(elems []*dicom.Element) FindResult {
	var r FindResult
	for _, elem := range elems {
		s, err := elem.GetString()
		if err != nil {
			continue
		}
		switch elem.Tag {
		case dicomtag.PatientName:
			r.PatientName = s
		case dicomtag.PatientID:
			r.PatientID = s
		case dicomtag.StudyInstanceUID:
			r.StudyInstanceUID = s
		case dicomtag.StudyDate:
			r.StudyDate = s
		case dicomtag.StudyDescription:
			r.StudyDescription = s
		case dicomtag.AccessionNumber:
			r.AccessionNumber = s
		case dicomtag.ModalitiesInStudy:
			r.ModalitiesInStudy = s
		case dicomtag.SeriesInstanceUID:
			r.SeriesInstanceUID = s
		case dicomtag.SeriesNumber:
			r.SeriesNumber = s
		case dicomtag.SeriesDescription:
			r.SeriesDescription = s
		case dicomtag.Modality:
			r.Modality = s
		case dicomtag.SOPInstanceUID:
			r.SOPInstanceUID = s
		case dicomtag.InstanceNumber:
			if n, err2 := strconv.Atoi(s); err2 == nil {
				r.InstanceNumber = n
			}
		case dicomtag.NumberOfSeriesRelatedInstances:
			if n, err2 := strconv.Atoi(s); err2 == nil {
				r.NumInstances = n
			}
		}
	}
	return r
}

// StoreProgress reports per-file progress from StoreFiles.
type StoreProgress struct {
	Done  int
	Total int
	Path  string
	Err   error // nil on success
}

// StoreFiles sends each file in paths to the remote PACS via C-STORE SCU.
// onProgress is called after each file attempt. The goroutine checks ctx
// between files so cancellation stops the loop promptly. Returns nil when all
// files have been attempted; ctx.Err() when cancelled.
//
// Files are sent as stored bytes, verbatim (DICOM library policy, Phase 1):
// the association offers each file's own transfer syntax so no re-encode —
// and no data dictionary — is involved in the common case. A file whose
// stored syntax the server did not accept is converted locally to the
// negotiated uncompressed syntax (on a temp copy; sources are never touched)
// and resent; a file that cannot be converted is reported and skipped.
func (c *DicomClient) StoreFiles(ctx context.Context, paths []string, onProgress func(StoreProgress)) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Offer the union of the files' stored transfer syntaxes, then the two
	// uncompressed syntaxes as universal fallbacks (Implicit VR LE is
	// mandatory for every conformant implementation, PS3.5 §10.1).
	seenTS := map[string]bool{}
	var offerTS []string
	addTS := func(ts string) {
		if ts != "" && !seenTS[ts] {
			seenTS[ts] = true
			offerTS = append(offerTS, ts)
		}
	}
	// Each file's identity (SOP UIDs, stored syntax, where its data set
	// starts) is read once, here, on the scan worker pool, and reused by the
	// send loop. The pass used to read every file's syntax one at a time
	// before connecting — about 9 ms a file on a cold spinning disk, so a
	// minute and a half of silence for 10,000 files — and then each file was
	// opened again for its identity and a third time for its bytes.
	idents := readPushIdentities(paths)
	for _, id := range idents {
		if id.err == nil {
			addTS(id.transferSyntaxUID)
		}
	}
	addTS(tsExplicitVRLE)
	addTS(tsImplicitVRLE)

	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
		CalledAETitle:    c.profile.RemoteAETitle,
		CallingAETitle:   c.localAETitle,
		SOPClasses:       sopclass.StorageClasses,
		TransferSyntaxes: offerTS,
	})
	if err != nil {
		// A stored syntax the negotiation layer does not recognize must not
		// sink the whole push — retry with the standard uncompressed set.
		su, err = netdicom.NewServiceUser(netdicom.ServiceUserParams{
			CalledAETitle:  c.profile.RemoteAETitle,
			CallingAETitle: c.localAETitle,
			SOPClasses:     sopclass.StorageClasses,
		})
		if err != nil {
			return fmt.Errorf("c-store: create service user: %w", err)
		}
	}

	type result struct{ err error }
	resultCh := make(chan result, 1)

	go func() {
		defer su.Release()
		su.Connect(fmt.Sprintf("%s:%d", c.profile.Host, c.profile.Port))

		// The next file is read from disk while the current one is on the
		// wire — one file ahead, so memory holds at most the file being sent,
		// the one read ahead and the one being read.
		type loaded struct {
			i   int
			raw []byte
			err error
		}
		next := make(chan loaded, 1)
		stopRead := make(chan struct{})
		defer close(stopRead)
		go func() {
			defer close(next)
			for i, path := range paths {
				l := loaded{i: i}
				if idents[i].err != nil {
					l.err = fmt.Errorf("read file meta: %w", idents[i].err)
				} else {
					l.raw, l.err = os.ReadFile(path)
				}
				select {
				case next <- l:
				case <-stopRead:
					return
				}
			}
		}()

		total := len(paths)
		for l := range next {
			if ctx.Err() != nil {
				resultCh <- result{ctx.Err()}
				return
			}
			path := paths[l.i]
			fileErr := l.err
			if fileErr == nil {
				fileErr = storeFileRaw(su, path, idents[l.i].dicomFileIdentity, l.raw)
			}
			if onProgress != nil {
				onProgress(StoreProgress{Done: l.i + 1, Total: total, Path: path, Err: fileErr})
			}
		}
		resultCh <- result{nil}
	}()

	select {
	case r := <-resultCh:
		return r.err
	case <-ctx.Done():
		abortAndReap(su, resultCh)
		return ctx.Err()
	}
}

// pushIdentity is one file's File Meta identity, or why it could not be read.
type pushIdentity struct {
	dicomFileIdentity
	err error
}

// readPushIdentities reads every file's identity on the scan worker pool —
// each is a few hundred bytes at the start of the file, so the pass is bound by
// disk seeks, which several readers at once overlap — returning them in the
// order given.
func readPushIdentities(paths []string) []pushIdentity {
	out := make([]pushIdentity, len(paths))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range scanWorkers(len(paths)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(paths) {
					return
				}
				id, err := fileMetaIdentity(paths[i])
				out[i] = pushIdentity{dicomFileIdentity: id, err: err}
			}
		}()
	}
	wg.Wait()
	return out
}

// storeFileRaw sends one stored file verbatim over an established push
// association, given its identity and bytes (both already read by StoreFiles).
// On a transfer-syntax mismatch it converts a temp copy to the negotiated
// uncompressed syntax and resends; any other error — including an
// unconvertible mismatch — is returned for per-file reporting.
func storeFileRaw(su *netdicom.ServiceUser, path string, ident dicomFileIdentity, raw []byte) error {
	send := func(raw []byte, offset int64, ts string) error {
		if int64(len(raw)) <= offset {
			return errors.New("file holds no dataset after the meta group")
		}
		return su.CStoreRaw(ident.sopClassUID, ident.sopInstanceUID, ts, raw[offset:])
	}

	err := send(raw, ident.datasetOffset, ident.transferSyntaxUID)
	var mismatch *netdicom.TransferSyntaxMismatchError
	if !errors.As(err, &mismatch) {
		return err
	}
	if !isUncompressedOnDisk(mismatch.Negotiated) {
		return fmt.Errorf("stored as %s but the server negotiated %s — no local conversion available",
			transferSyntaxLabel(ident.transferSyntaxUID), transferSyntaxLabel(mismatch.Negotiated))
	}
	tmpPath, changed, terr := transcodeDICOMFileToTemp(path, mismatch.Negotiated, os.TempDir())
	if terr != nil {
		return fmt.Errorf("cannot convert %s to negotiated %s: %w",
			transferSyntaxLabel(ident.transferSyntaxUID), transferSyntaxLabel(mismatch.Negotiated), terr)
	}
	if !changed {
		return err // defensive: mismatch reported but file already in the negotiated syntax
	}
	defer os.Remove(tmpPath)
	tmpIdent, ierr := fileMetaIdentity(tmpPath)
	if ierr != nil {
		return fmt.Errorf("read converted file meta: %w", ierr)
	}
	logInfo("c-store: %s converted %s → %s for push (server did not accept the stored syntax)",
		filepath.Base(path), transferSyntaxLabel(ident.transferSyntaxUID), transferSyntaxLabel(mismatch.Negotiated))
	converted, rerr := os.ReadFile(tmpPath)
	if rerr != nil {
		return rerr
	}
	return send(converted, tmpIdent.datasetOffset, tmpIdent.transferSyntaxUID)
}

// WorklistResult holds one Modality Worklist C-FIND response item.
// Err is set on error items; all other fields are zero when Err != nil.
// Scheduled attributes are extracted from the ScheduledProcedureStepSequence.
type WorklistResult struct {
	Err                    error
	PatientName            string
	PatientID              string
	AccessionNumber        string
	StudyInstanceUID       string
	RequestedProcedureDesc string
	RequestedProcedureID   string
	ScheduledDate          string
	ScheduledTime          string
	Modality               string
	ScheduledStation       string
	ProcedureStepDesc      string
}

// FindWorklist sends a C-FIND against the Modality Worklist Information Model
// (1.2.840.10008.5.1.4.31, PS3.4 K.4). Results are streamed on the returned
// channel, which is closed when the query completes or ctx is cancelled.
func (c *DicomClient) FindWorklist(ctx context.Context, params map[string]string) (<-chan WorklistResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
		CalledAETitle:  c.profile.RemoteAETitle,
		CallingAETitle: c.localAETitle,
		SOPClasses:     sopclass.QRFindClasses, // includes 1.2.840.10008.5.1.4.31
	})
	if err != nil {
		return nil, fmt.Errorf("worklist c-find: create service user: %w", err)
	}

	out := make(chan WorklistResult, 128)

	go func() {
		defer close(out)
		defer su.Release()
		su.Connect(fmt.Sprintf("%s:%d", c.profile.Host, c.profile.Port))

		for r := range su.CFind(netdicom.QRLevelWorklist, buildWorklistFilter(params)) {
			if r.Err != nil {
				select {
				case out <- WorklistResult{Err: r.Err}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case out <- elementsToWorklistResult(r.Elements):
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

// buildWorklistFilter builds a Modality Worklist C-FIND identifier dataset.
// The ScheduledProcedureStepSequence (SQ 0040,0100) is included as a single
// item containing schedule-level matching and return keys (PS3.4 K.6.1.3).
func buildWorklistFilter(params map[string]string) []*dicom.Element {
	childElems := []*dicom.Element{
		dicom.MustNewElement(dicomtag.Modality, params["Modality"]),
		dicom.MustNewElement(dicomtag.ScheduledProcedureStepStartDate, params["ScheduledDate"]),
		dicom.MustNewElement(dicomtag.ScheduledProcedureStepStartTime, ""),
		dicom.MustNewElement(dicomtag.ScheduledProcedureStepDescription, ""),
		dicom.MustNewElement(dicomtag.ScheduledProcedureStepID, ""),
		dicom.MustNewElement(dicomtag.ScheduledStationAETitle, ""),
		dicom.MustNewElement(dicomtag.ScheduledStationName, ""),
		dicom.MustNewElement(dicomtag.ScheduledPerformingPhysicianName, ""),
	}

	// Wrap children in an Item element (FFFE,E000), then in the SQ element.
	itemArgs := make([]interface{}, len(childElems))
	for i, e := range childElems {
		itemArgs[i] = e
	}
	seqItem := dicom.MustNewElement(dicomtag.Item, itemArgs...)
	seq := dicom.MustNewElement(dicomtag.ScheduledProcedureStepSequence, seqItem)

	return []*dicom.Element{
		dicom.MustNewElement(dicomtag.SpecificCharacterSet, "ISO_IR 192"),
		dicom.MustNewElement(dicomtag.PatientName, params["PatientName"]),
		dicom.MustNewElement(dicomtag.PatientID, params["PatientID"]),
		dicom.MustNewElement(dicomtag.AccessionNumber, params["AccessionNumber"]),
		dicom.MustNewElement(dicomtag.StudyInstanceUID, ""),
		dicom.MustNewElement(dicomtag.RequestedProcedureDescription, ""),
		dicom.MustNewElement(dicomtag.RequestedProcedureID, ""),
		seq,
	}
}

// elementsToWorklistResult extracts a WorklistResult from one C-FIND response.
// Top-level patient/study tags are read directly; schedule attributes are read
// from items inside the ScheduledProcedureStepSequence.
func elementsToWorklistResult(elems []*dicom.Element) WorklistResult {
	var r WorklistResult
	for _, elem := range elems {
		if elem.Tag == dicomtag.ScheduledProcedureStepSequence {
			for _, v := range elem.Value {
				item, ok := v.(*dicom.Element)
				if !ok || item.Tag != dicomtag.Item {
					continue
				}
				for _, child := range item.Value {
					ce, ok := child.(*dicom.Element)
					if !ok {
						continue
					}
					s, err := ce.GetString()
					if err != nil {
						continue
					}
					switch ce.Tag {
					case dicomtag.Modality:
						r.Modality = s
					case dicomtag.ScheduledProcedureStepStartDate:
						r.ScheduledDate = s
					case dicomtag.ScheduledProcedureStepStartTime:
						r.ScheduledTime = s
					case dicomtag.ScheduledProcedureStepDescription:
						r.ProcedureStepDesc = s
					case dicomtag.ScheduledStationName:
						r.ScheduledStation = s
					}
				}
			}
			continue
		}
		s, err := elem.GetString()
		if err != nil {
			continue
		}
		switch elem.Tag {
		case dicomtag.PatientName:
			r.PatientName = s
		case dicomtag.PatientID:
			r.PatientID = s
		case dicomtag.AccessionNumber:
			r.AccessionNumber = s
		case dicomtag.StudyInstanceUID:
			r.StudyInstanceUID = s
		case dicomtag.RequestedProcedureDescription:
			r.RequestedProcedureDesc = s
		case dicomtag.RequestedProcedureID:
			r.RequestedProcedureID = s
		}
	}
	return r
}

// Close is a no-op; associations are opened and closed per-operation.
func (c *DicomClient) Close() {}

// localIP returns the preferred outbound IPv4 address of this machine.
// It tries a UDP connect first (no packets sent); on failure it enumerates
// network interfaces as a fallback for air-gapped environments (Phase 3-H).
func localIP() string {
	if conn, err := net.Dial("udp4", "8.8.8.8:53"); err == nil {
		defer conn.Close()
		return conn.LocalAddr().(*net.UDPAddr).IP.String()
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return "unknown"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() {
				return ip4.String()
			}
		}
	}
	return "unknown"
}
