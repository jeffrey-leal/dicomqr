package main

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	// NumStudySeries and NumStudyInstances are the study's size from a
	// STUDY-level query; 0 when the server did not report them.
	NumStudySeries    int
	NumStudyInstances int
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
// query completes, fails, times out or ctx is cancelled (cFindStream). A
// non-nil error is returned only for a ctx already cancelled. Every failure —
// connecting, a refused association, a server that stops responding, a
// rejected query — is reported in-band as a FindResult with Err set, so callers
// can distinguish a genuinely empty result from a failed query.
func (c *DicomClient) Find(ctx context.Context, level string, params map[string]string) (<-chan FindResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	out := make(chan FindResult, 128)

	go func() {
		defer close(out)
		err := c.cFindStream(ctx, "c-find", levelToQRLevel(level), buildFindFilter(level, params),
			func(elems []*dicom.Element) bool {
				select {
				case out <- elementsToFindResult(elems):
					return true
				case <-ctx.Done():
					return false
				}
			})
		if err != nil {
			select {
			case out <- FindResult{Err: err}:
			case <-ctx.Done():
			}
		}
	}()

	return out, nil
}

// queryIdleTimeout is how long a query waits for the server's next response —
// the association's acceptance, the first match, or the one after — before
// giving up. Any response resets it, so a large result that keeps arriving is
// never cut off; a server that has stopped answering is.
var queryIdleTimeout = 60 * time.Second

// errNoResponse reports an association abandoned because the server went
// silent: a query under queryIdleTimeout, a push under the stall timeout.
type errNoResponse struct{ after time.Duration }

func (e errNoResponse) Error() string {
	return fmt.Sprintf("the server stopped responding (nothing for %.0f s)", e.after.Seconds())
}

// connectTimeout is the profile's Connect timeout, or the 10 s default.
func (c *DicomClient) connectTimeout() time.Duration {
	if c.profile.ConnectTimeout > 0 {
		return time.Duration(c.profile.ConnectTimeout) * time.Second
	}
	return 10 * time.Second
}

// cFindStream runs one C-FIND on an association of its own and hands each
// matching identifier to emit, which returns false to stop early. It cannot
// hang: the TCP connect is bounded by the profile's Connect timeout, and once
// connected the association is aborted if the server sends nothing for
// queryIdleTimeout or when ctx is cancelled. Aborting is what makes either
// stick — CFind waits for the handshake and for each response with no deadline
// of its own, so a query merely abandoned used to leave its goroutine and the
// connection behind, and the worklist's Query button greyed out for good.
func (c *DicomClient) cFindStream(ctx context.Context, op string, level netdicom.QRLevel,
	filter []*dicom.Element, emit func([]*dicom.Element) bool) error {

	addr := fmt.Sprintf("%s:%d", c.profile.Host, c.profile.Port)
	dialer := net.Dialer{Timeout: c.connectTimeout()}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s: connect to %s: %w", op, addr, err)
	}
	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
		CalledAETitle:  c.profile.RemoteAETitle,
		CallingAETitle: c.localAETitle,
		SOPClasses:     sopclass.QRFindClasses, // includes Modality Worklist
	})
	if err != nil {
		conn.Close()
		return fmt.Errorf("%s: create service user: %w", op, err)
	}
	su.SetConn(conn)
	defer su.Release()

	var timedOut atomic.Bool
	idle := time.AfterFunc(queryIdleTimeout, func() {
		timedOut.Store(true)
		su.Abort()
	})
	defer idle.Stop()
	stop := context.AfterFunc(ctx, su.Abort)
	defer stop()

	// The reason the association ended early, if it did, outranks the
	// "connection closed" error the abort itself produces.
	why := func(err error) error {
		switch {
		case timedOut.Load():
			return fmt.Errorf("%s: %w", op, errNoResponse{queryIdleTimeout})
		case ctx.Err() != nil:
			return ctx.Err()
		}
		return err
	}
	for r := range su.CFind(level, filter) {
		idle.Reset(queryIdleTimeout)
		if r.Err != nil {
			return why(r.Err)
		}
		if !emit(r.Elements) {
			return ctx.Err()
		}
	}
	return why(nil)
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
		// Optional return keys (PS3.4 C.6.1.1.3): the study's size, shown on
		// its row before anything is retrieved. A server that does not
		// support them returns them empty or leaves them out.
		dicom.MustNewElement(dicomtag.NumberOfStudyRelatedSeries, ""),
		dicom.MustNewElement(dicomtag.NumberOfStudyRelatedInstances, ""),
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
		case dicomtag.NumberOfStudyRelatedSeries:
			if n, err2 := strconv.Atoi(strings.TrimSpace(s)); err2 == nil {
				r.NumStudySeries = n
			}
		case dicomtag.NumberOfStudyRelatedInstances:
			if n, err2 := strconv.Atoi(strings.TrimSpace(s)); err2 == nil {
				r.NumStudyInstances = n
			}
		}
	}
	return r
}

// StoreProgress reports per-file progress from StoreFiles.
type StoreProgress struct {
	Done  int // files resolved so far, counting this one
	Total int
	Path  string
	Err   error // nil on success
}

// pushReport is what a finished push says beyond its per-file reports.
type pushReport struct {
	// limit is the number of associations the push was allowed at once — the
	// server profile's Parallel transfers, capped by the number of targets.
	limit int
	// degradedBy is why the push dropped to one association, nil if it never
	// did (describeParallelDegrade words it).
	degradedBy error
}

// StoreFiles sends each file in paths to the remote PACS via C-STORE SCU,
// over as many associations at once as the server profile's Parallel
// transfers allows (pushrun.go). onProgress is called once per file as it is
// resolved — sent, refused, or failed — one call at a time, from whichever
// association finished it; Done counts up across them. A file whose
// association is cancelled is not reported.
//
// idleTimeout aborts an association that has a send in flight and moves no
// byte either way for that long — a server that stopped answering, which used
// to leave the push waiting forever. Zero or negative disables it.
//
// The returned error is ctx.Err() when the push was cancelled and nil
// otherwise: every other failure — a refused association, a dropped one, a
// silent server, a refused file — is reported per file.
//
// Files are sent as stored bytes, verbatim (DICOM library policy, Phase 1):
// each association proposes exactly the SOP classes and transfer syntaxes its
// files are stored in (pushPresentationContexts), so a server that accepts a
// file's syntax receives its bytes unchanged — no re-encode, no data
// dictionary. A file whose stored syntax the server did not accept is
// converted locally to an accepted uncompressed syntax (on a temp copy;
// sources are never touched) and resent; a file that cannot be converted, or
// whose SOP class the server refused, is reported and skipped.
func (c *DicomClient) StoreFiles(ctx context.Context, paths []string, idleTimeout time.Duration,
	onProgress func(StoreProgress)) (pushReport, error) {
	if err := ctx.Err(); err != nil {
		return pushReport{}, err
	}

	// Each file's identity (SOP UIDs, stored syntax, where its data set
	// starts) is read once, here, on the scan worker pool, and reused both to
	// build each association's proposal and by the send loop. The pass used to
	// read every file's syntax one at a time before connecting — about 9 ms a
	// file on a cold spinning disk, so a minute and a half of silence for
	// 10,000 files — and then each file was opened again for its identity and
	// a third time for its bytes.
	idents := readPushIdentities(paths)
	limit := c.profile.parallelTransfers()
	targets := pushTargets(paths, limit)
	rep := pushReport{limit: max(1, min(limit, len(targets)))}

	// Reports are serialised and numbered here, so onProgress sees one call
	// at a time whichever association finished the file.
	var mu sync.Mutex
	done := 0
	report := func(i int, err error) {
		mu.Lock()
		defer mu.Unlock()
		done++
		if onProgress != nil {
			onProgress(StoreProgress{Done: done, Total: len(paths), Path: paths[i], Err: err})
		}
	}

	push := &pushRun{
		client:      c,
		paths:       paths,
		idents:      idents,
		idleTimeout: idleTimeout,
		report:      report,
		// Read-ahead is bounded by bytes as well as by count: each association
		// holds up to three files, and several associations of large
		// multi-frame files would otherwise hold gigabytes.
		budget:       newMemBudget(modifyMemoryBudget),
		convertSlots: make(chan struct{}, pushConversionLimit(rep.limit)),
	}
	// pending[t] is target t's files not yet resolved. A target whose
	// association fails part-way keeps the rest for its re-run, so nothing
	// already sent goes again. Written by the worker running the target and
	// read by the next to run it, which the scheduler's lock orders.
	pending := slices.Clone(targets)
	sched := newRetrieveRun(len(targets), limit)
	sched.onDegrade = func(err error) {
		logWarn("push: dropping to one association at a time: %v", err)
	}
	logInfo("push: %d file(s) to %s (%s) in %d part(s), up to %d association(s) at once",
		len(paths), c.profile.Name, c.profile.RemoteAETitle, len(targets), rep.limit)
	sched.run(ctx, func(tctx context.Context, t int) error {
		rest, err := push.sendTarget(tctx, pending[t])
		pending[t] = rest
		return err
	})
	// A target that failed for good fails its unsent files with the reason —
	// except one that could never have been sent, which keeps its own.
	for t, res := range sched.outcome() {
		if res.state != targetFailed {
			continue
		}
		for _, i := range pending[t] {
			if idents[i].err != nil {
				report(i, unreadablePushFile(idents[i]))
			} else {
				report(i, res.err)
			}
		}
	}
	rep.degradedBy = sched.degradedBy()
	return rep, ctx.Err()
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

// unreadablePushFile is the failure reported for a file whose identity could
// not be read before connecting.
func unreadablePushFile(id pushIdentity) error {
	return fmt.Errorf("read file meta: %w", id.err)
}

// maxPresentationContexts is the A-ASSOCIATE-RQ limit: context IDs are the
// odd numbers 1-255 (PS3.8 9.3.2.2).
const maxPresentationContexts = 128

// pushPresentationContexts is what a push proposes, built from the files
// actually being sent: for each SOP class, one presentation context per
// stored transfer syntax. A compressed syntax is offered alone, so a server
// that takes it receives the file exactly as stored (a server picks one
// syntax per context, and offered several it typically takes the first). An
// uncompressed one is offered first, then the two uncompressed little-endian
// syntaxes. A class whose files are all compressed also gets an
// uncompressed-only context, the fallback storeFileRaw converts into when a
// server refuses the compressed syntax.
//
// The syntaxes are proposed exactly as stored. The push used to offer one
// list per class through the netdicom layer's canonicalisation, which turns
// every compressed syntax into Explicit VR Little Endian, so a server
// accepted "Explicit VR LE" and received JPEG 2000 bytes under that label.
// Proposing only the classes present also lets any SOP class be pushed, not
// just those on the library's storage list.
//
// If the per-syntax contexts exceed the association's 128, each class gets
// just the uncompressed context instead (everything still sendable, at the
// cost of a conversion); classes beyond 128 are left out, and their files
// fail as not proposed.
func pushPresentationContexts(idents []pushIdentity) []netdicom.PresentationContextSpec {
	var classes []string
	syntaxesByClass := map[string][]string{}
	for _, id := range idents {
		if id.err != nil {
			continue
		}
		syntaxes, seen := syntaxesByClass[id.sopClassUID]
		if !seen {
			classes = append(classes, id.sopClassUID)
		}
		if !slices.Contains(syntaxes, id.transferSyntaxUID) {
			syntaxesByClass[id.sopClassUID] = append(syntaxes, id.transferSyntaxUID)
		}
	}
	contexts := buildPushContexts(classes, syntaxesByClass, false)
	if len(contexts) > maxPresentationContexts {
		contexts = buildPushContexts(classes, syntaxesByClass, true)
	}
	if len(contexts) > maxPresentationContexts {
		contexts = contexts[:maxPresentationContexts]
	}
	return contexts
}

func buildPushContexts(classes []string, syntaxesByClass map[string][]string,
	uncompressedOnly bool) []netdicom.PresentationContextSpec {
	uncompressed := []string{tsExplicitVRLE, tsImplicitVRLE}
	var contexts []netdicom.PresentationContextSpec
	for _, class := range classes {
		hasUncompressed := false
		if !uncompressedOnly {
			for _, ts := range syntaxesByClass[class] {
				offered := []string{ts}
				if isUncompressedOnDisk(ts) {
					for _, u := range uncompressed {
						if u != ts {
							offered = append(offered, u)
						}
					}
					hasUncompressed = true
				}
				contexts = append(contexts, netdicom.PresentationContextSpec{AbstractSyntax: class, TransferSyntaxes: offered})
			}
		}
		if !hasUncompressed {
			contexts = append(contexts, netdicom.PresentationContextSpec{AbstractSyntax: class, TransferSyntaxes: uncompressed})
		}
	}
	return contexts
}

// rawStorer is the one association operation storeFileRaw needs —
// *netdicom.ServiceUser, or the push's watched wrapper (pushAssociation).
type rawStorer interface {
	CStoreRaw(sopClassUID, sopInstanceUID, transferSyntaxUID string, data []byte) error
}

// storeFileRaw sends one file's bytes verbatim over an established push
// association, given the identity they carry. A file the server would not take
// in its stored syntax has normally been converted already, by the push's
// reader while the previous file was on the wire (pushConversionTarget,
// convertForPush), so this sends what it is handed. Should the server still
// refuse the syntax, it converts here — through the same bounded conversion,
// slots — and resends; any other error, a refused SOP class or an unconvertible
// syntax, is returned for per-file reporting. path is the source file, read
// again only for that conversion.
func storeFileRaw(su rawStorer, path string, ident dicomFileIdentity, raw []byte, slots chan struct{}) error {
	send := func(raw []byte, offset int64, ts string) error {
		if int64(len(raw)) <= offset {
			return errors.New("file holds no dataset after the meta group")
		}
		return su.CStoreRaw(ident.sopClassUID, ident.sopInstanceUID, ts, raw[offset:])
	}

	err := send(raw, ident.datasetOffset, ident.transferSyntaxUID)
	var notAccepted *netdicom.SOPClassNotAcceptedError
	if errors.As(err, &notAccepted) {
		if notAccepted.Proposed {
			return fmt.Errorf("the server does not accept SOP class %s", describeSOPClass(ident.sopClassUID))
		}
		return fmt.Errorf("SOP class %s was not offered to the server (more than %d SOP classes in one push)",
			describeSOPClass(ident.sopClassUID), maxPresentationContexts)
	}
	var mismatch *netdicom.TransferSyntaxMismatchError
	if !errors.As(err, &mismatch) {
		return err
	}
	if !isUncompressedOnDisk(mismatch.Negotiated) {
		accepted := make([]string, len(mismatch.Accepted))
		for i, ts := range mismatch.Accepted {
			accepted[i] = transferSyntaxLabel(ts)
		}
		return fmt.Errorf("stored as %s, which the server does not accept for %s; it accepts only %s, and a file can only be converted to an uncompressed syntax",
			transferSyntaxLabel(ident.transferSyntaxUID), describeSOPClass(ident.sopClassUID), strings.Join(accepted, ", "))
	}
	converted, convIdent, cerr := convertForPush(path, ident, mismatch.Negotiated, slots)
	if cerr != nil {
		return cerr
	}
	if convIdent.transferSyntaxUID == ident.transferSyntaxUID {
		return err // defensive: mismatch reported but the file is already in the negotiated syntax
	}
	return send(converted, convIdent.datasetOffset, convIdent.transferSyntaxUID)
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
// channel, which is closed when the query completes, fails, times out or ctx
// is cancelled; failures arrive in-band, as for Find.
func (c *DicomClient) FindWorklist(ctx context.Context, params map[string]string) (<-chan WorklistResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	out := make(chan WorklistResult, 128)

	go func() {
		defer close(out)
		err := c.cFindStream(ctx, "worklist c-find", netdicom.QRLevelWorklist, buildWorklistFilter(params),
			func(elems []*dicom.Element) bool {
				select {
				case out <- elementsToWorklistResult(elems):
					return true
				case <-ctx.Done():
					return false
				}
			})
		if err != nil {
			select {
			case out <- WorklistResult{Err: err}:
			case <-ctx.Done():
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
