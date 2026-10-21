package main

// Parallel push: StoreFiles (dicomnet.go) splits a push into targets and runs
// them through the retrieve scheduler (retrievesched.go), each target on an
// association of its own, up to the server profile's Parallel transfers at
// once (user decision 2026-10-16 — the one setting covers retrieve and push,
// since it states how many associations the server takes from us). Pushing
// gains from it for the reason retrieving does: one association sends one
// file at a time and waits for the server to store and index it before the
// next, and several associations overlap those waits.
//
// The scheduler's fallback carries over unchanged: an association-level
// failure in an attempt begun while more than one association was allowed
// drops the push to one association and re-runs that target. Two things are
// push-specific. A target re-runs only the files it had not resolved, so
// nothing already stored is sent twice (bar the one file in flight when an
// association died, which the server may or may not have kept). And only a
// failure of the association counts — a server refusing one file's SOP class,
// syntax or contents is that file's failure, never a sign the server cannot
// take parallel associations (pushSendError draws the line).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	netdicom "github.com/algm/go-netdicom"
)

// pushMinTargetFiles is the fewest files a target is cut to when a push is
// split. Every target opens and releases an association of its own, which
// costs about as much as storing a file or two, so splitting a small push
// finer than this would spend more on handshakes than parallelism saves.
const pushMinTargetFiles = 8

// pushTargetsPerAssociation sets the target size relative to the limit: about
// this many targets per association, so one that finishes early picks up
// another rather than idling while the slowest runs on alone.
const pushTargetsPerAssociation = 2

// pushTargets splits paths into the push's targets, as file indices in the
// order given. With one association allowed it is a single target — exactly
// the one-association push this replaced. Otherwise it keeps a series together
// where it can: files are grouped by folder (in the download folder's layout a
// folder is a series), whole groups are packed into targets of up to a
// computed size, and only a group larger than that is split. A study that is
// mostly one large series therefore still spreads across associations.
func pushTargets(paths []string, limit int) [][]int {
	n := len(paths)
	if n == 0 {
		return nil
	}
	if limit <= 1 {
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		return [][]int{all}
	}
	size := max(pushMinTargetFiles, (n+limit*pushTargetsPerAssociation-1)/(limit*pushTargetsPerAssociation))

	var targets [][]int
	var cur []int
	flush := func() {
		if len(cur) > 0 {
			targets = append(targets, cur)
			cur = nil
		}
	}
	for start := 0; start < n; {
		end := start + 1
		for end < n && filepath.Dir(paths[end]) == filepath.Dir(paths[start]) {
			end++
		}
		if len(cur) > 0 && len(cur)+end-start > size {
			flush()
		}
		for ; start < end; start++ {
			cur = append(cur, start)
			if len(cur) == size {
				flush()
			}
		}
	}
	flush()
	return targets
}

// pushRun is the state one StoreFiles call shares across its associations.
type pushRun struct {
	client      *DicomClient
	paths       []string
	idents      []pushIdentity
	idleTimeout time.Duration
	report      func(i int, err error) // serialised by StoreFiles
	budget      *memBudget             // bytes of file data held in read-ahead
	// convertSlots bounds the conversions in progress across the push
	// (pushConversionLimit); each association's reader converts its own files.
	convertSlots chan struct{}
}

// pushConversionLimit is how many files a push converts at once: one per
// association, as far as half the logical processors allow
// (modifyWorkerCount's CPU-heavy share, DICOMQR_MODIFY_WORKERS overriding it).
// Conversion was first held to one at a time on the assumption that a server
// refusing a file's stored syntax would be rare; a server that does not take
// JPEG 2000 refuses every file of a JPEG 2000 study, and one conversion at a
// time then set the pace of the whole push (field-measured 2026-10-16: GARBER,
// 2,943 files, to UTILITY — 82% of a one-association push spent converting,
// and four associations only 1.43× faster because their conversions queued).
func pushConversionLimit(associations int) int {
	return modifyWorkerCount(true, max(1, associations))
}

// pushConversionTarget is the syntax file must be converted to before it can go
// out on su's association, or "" to send it as stored. "" also covers every
// case this cannot settle — the association failed, the class was refused, or
// only compressed syntaxes other than the file's own were accepted — so that
// the send reports it exactly as it always has (storeFileRaw).
func pushConversionTarget(su *netdicom.ServiceUser, ident dicomFileIdentity) string {
	accepted, _, err := su.AcceptedTransferSyntaxes(ident.sopClassUID)
	if err != nil || len(accepted) == 0 || slices.Contains(accepted, ident.transferSyntaxUID) {
		return ""
	}
	if target := netdicom.ConversionTarget(accepted); isUncompressedOnDisk(target) {
		return target
	}
	return ""
}

// convertForPush converts the file at path to target and returns the bytes and
// the identity they carry (pushConvertFn). It holds one of slots for the
// duration, and runs at below-normal CPU priority (lowerWorkerPriority): a
// conversion decodes the whole image, and several at once must use idle cores
// without crowding out the desktop.
func convertForPush(path string, ident dicomFileIdentity, target string, slots chan struct{}) ([]byte, dicomFileIdentity, error) {
	slots <- struct{}{}
	defer func() { <-slots }()
	restore := lowerWorkerPriority()
	defer restore()
	return pushConvertFn(path, ident, target)
}

// pushConvertFn is the conversion itself, convertFileForPush — replaceable by
// tests, which time and count it inside the bound convertForPush applies.
var pushConvertFn = convertFileForPush

// convertFileForPush converts the file at path to target on a temp copy and
// returns the copy's bytes and identity. A file already in target comes back
// as it is.
func convertFileForPush(path string, ident dicomFileIdentity, target string) ([]byte, dicomFileIdentity, error) {
	tmpPath, changed, err := transcodeDICOMFileToTemp(path, target, os.TempDir())
	if err != nil {
		return nil, ident, fmt.Errorf("cannot convert %s to negotiated %s: %w",
			transferSyntaxLabel(ident.transferSyntaxUID), transferSyntaxLabel(target), err)
	}
	if !changed {
		raw, rerr := os.ReadFile(path)
		return raw, ident, rerr
	}
	defer os.Remove(tmpPath)
	converted, err := fileMetaIdentity(tmpPath)
	if err != nil {
		return nil, ident, fmt.Errorf("read converted file meta: %w", err)
	}
	raw, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, ident, err
	}
	logInfo("c-store: %s converted %s → %s for push (server did not accept the stored syntax)",
		filepath.Base(path), transferSyntaxLabel(ident.transferSyntaxUID), transferSyntaxLabel(target))
	return raw, converted, nil
}

// pushAssociationError marks an error that ended the association rather than
// one file: the target stops, and its unsent files wait for a re-run.
type pushAssociationError struct{ err error }

func (e *pushAssociationError) Error() string { return e.err.Error() }
func (e *pushAssociationError) Unwrap() error { return e.err }

// pushSendError classifies an error from one C-STORE. The server refusing the
// object — its SOP class or syntax not accepted at negotiation, or a failure
// status in its response — leaves the association working and stays a
// per-file error. Anything else (the association refused, closed, aborted) is
// wrapped as a pushAssociationError.
func pushSendError(err error) error {
	if err == nil {
		return nil
	}
	var notAccepted *netdicom.SOPClassNotAcceptedError
	var mismatch *netdicom.TransferSyntaxMismatchError
	var status *netdicom.CStoreStatusError
	if errors.As(err, &notAccepted) || errors.As(err, &mismatch) || errors.As(err, &status) {
		return err
	}
	return &pushAssociationError{err: err}
}

// pushAssociation is the rawStorer storeFileRaw sends through: each C-STORE
// is marked as waiting on the server for the idle watchdog, and its error is
// classified (pushSendError).
type pushAssociation struct {
	su    *netdicom.ServiceUser
	watch *pushActivity
}

func (a pushAssociation) CStoreRaw(sopClassUID, sopInstanceUID, transferSyntaxUID string, data []byte) error {
	a.watch.begin()
	err := a.su.CStoreRaw(sopClassUID, sopInstanceUID, transferSyntaxUID, data)
	a.watch.end()
	return pushSendError(err)
}

// pushActivity wraps a push association's connection to see whether bytes are
// still moving. The watchdog it runs counts silence only while a C-STORE is
// waiting on the server (between begin and end) — never while the push reads
// the next file from disk or converts one, which can legitimately take longer
// than any timeout and are not the server's doing. Measuring bytes rather
// than responses is what lets a single large file on a slow link take as long
// as it needs: it is never silent while it is being written.
type pushActivity struct {
	net.Conn
	last atomic.Int64 // UnixNano of the latest byte moved, or of begin
	busy atomic.Bool
}

func (a *pushActivity) Read(p []byte) (int, error) {
	n, err := a.Conn.Read(p)
	if n > 0 {
		a.touch()
	}
	return n, err
}

func (a *pushActivity) Write(p []byte) (int, error) {
	n, err := a.Conn.Write(p)
	if n > 0 {
		a.touch()
	}
	return n, err
}

func (a *pushActivity) touch() { a.last.Store(time.Now().UnixNano()) }
func (a *pushActivity) begin() { a.touch(); a.busy.Store(true) }
func (a *pushActivity) end()   { a.busy.Store(false) }

// watch calls onSilent, once, when a C-STORE has waited timeout with no byte
// moving either way. stop ends the watch; it is safe to call more than once.
func (a *pushActivity) watch(timeout time.Duration, onSilent func()) (stop func()) {
	tick := min(max(timeout/10, 10*time.Millisecond), time.Second)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if a.busy.Load() && time.Since(time.Unix(0, a.last.Load())) > timeout {
					onSilent()
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// sendTarget sends files (indices into r.paths) over one association of its
// own, reporting each file through r.report as it is resolved. It returns the
// files it did not resolve and, when the association itself failed — could
// not be opened, was refused, dropped, went silent for r.idleTimeout, or ctx
// was cancelled — why; the scheduler then re-runs the target or StoreFiles
// fails the rest. A file the server refuses is reported and the target goes on.
func (r *pushRun) sendTarget(ctx context.Context, files []int) (rest []int, err error) {
	// A file whose identity could not be read can be neither proposed nor
	// sent. It is reported in its turn by the send loop, so reports keep the
	// target's order; a target with nothing else needs no association at all.
	var ids []pushIdentity
	for _, i := range files {
		if r.idents[i].err == nil {
			ids = append(ids, r.idents[i])
		}
	}
	if len(ids) == 0 {
		for _, i := range files {
			r.report(i, unreadablePushFile(r.idents[i]))
		}
		return nil, nil
	}

	c := r.client
	addr := fmt.Sprintf("%s:%d", c.profile.Host, c.profile.Port)
	dialer := net.Dialer{Timeout: c.connectTimeout()}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		if ctx.Err() != nil {
			return files, ctx.Err()
		}
		return files, fmt.Errorf("c-store: connect to %s: %w", addr, err)
	}
	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
		CalledAETitle:  c.profile.RemoteAETitle,
		CallingAETitle: c.localAETitle,
		// Only this target's classes and syntaxes: fewer to negotiate, and
		// less of the 128-context limit spent on files sent elsewhere.
		PresentationContexts: pushPresentationContexts(ids),
	})
	if err != nil {
		conn.Close()
		return files, fmt.Errorf("c-store: create service user: %w", err)
	}
	watch := &pushActivity{Conn: conn}
	watch.touch()
	su.SetConn(watch)
	var aborted atomic.Bool
	abort := func() {
		aborted.Store(true)
		su.Abort()
	}
	// Returning only once the association has closed keeps the push within
	// its parallel limit as the server counts associations (see Move). Not
	// after an abort: a server that went silent will not close its end either,
	// so the wait would only add its full bound to every cancel and timeout.
	defer func() {
		if aborted.Load() {
			su.Release()
			return
		}
		su.ReleaseAndWait(retrieveReleaseWait)
	}()

	var silent atomic.Bool
	if r.idleTimeout > 0 {
		stopWatch := watch.watch(r.idleTimeout, func() {
			silent.Store(true)
			abort()
		})
		defer stopWatch()
	}
	stopCancel := context.AfterFunc(ctx, abort)
	defer stopCancel()
	// Why the association ended early, if it did, outranks the "connection
	// closed" the abort itself produces.
	why := func(err error) error {
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case silent.Load():
			return errNoResponse{r.idleTimeout}
		}
		return err
	}

	// Wait for the association before preparing anything: which files need
	// converting depends on what the server accepted. The wait is a wait on the
	// server, so the idle watchdog counts it — a server that never answers the
	// association request is abandoned like one that stops mid-file.
	watch.begin()
	_, _, err = su.AcceptedTransferSyntaxes(ids[0].sopClassUID)
	watch.end()
	if err != nil {
		return files, why(err)
	}

	// The next file is prepared while the current one is on the wire — read
	// from disk, or, when the server did not accept its stored syntax,
	// converted (pushConversionTarget): the conversion is the slow part of a
	// push to such a server, and doing it here overlaps it with the network
	// instead of following it. One file ahead, so this association holds at most
	// the file being sent, the one prepared and the one being prepared, each
	// admitted against the push's shared budget first: by its size on disk, or
	// for a conversion by what its decoded pixels will occupy.
	type loaded struct {
		k       int               // index into files
		ident   dicomFileIdentity // of raw: the converted copy's when converted
		raw     []byte
		err     error
		release func()
	}
	next := make(chan loaded, 1)
	stopRead := make(chan struct{})
	go func() {
		defer close(next)
		for k, i := range files {
			select {
			case <-stopRead:
				return
			default:
			}
			if r.idents[i].err != nil {
				l := loaded{k: k, err: unreadablePushFile(r.idents[i]), release: func() {}}
				select {
				case next <- l:
					continue
				case <-stopRead:
					return
				}
			}
			ident := r.idents[i].dicomFileIdentity
			target := pushConversionTarget(su, ident)
			var weight int64
			if info, serr := os.Stat(r.paths[i]); serr == nil {
				weight = info.Size()
			}
			if target != "" {
				weight = max(weight, fileMemoryWeight(r.paths[i]))
			}
			l := loaded{k: k, ident: ident, release: r.budget.acquire(weight)}
			// acquire can wait on the other associations; the send loop may
			// have ended meanwhile, and then the file is not wanted.
			select {
			case <-stopRead:
				l.release()
				return
			default:
			}
			if target != "" {
				l.raw, l.ident, l.err = convertForPush(r.paths[i], ident, target, r.convertSlots)
			} else {
				l.raw, l.err = os.ReadFile(r.paths[i])
			}
			select {
			case next <- l:
			case <-stopRead:
				l.release()
				return
			}
		}
	}()
	// However the loop ends, stop the reader and give back everything it
	// still holds — the budget is shared with the other associations.
	defer func() {
		close(stopRead)
		for l := range next {
			l.release()
		}
	}()

	for l := range next {
		i := files[l.k]
		if ctx.Err() != nil {
			l.release()
			return files[l.k:], ctx.Err()
		}
		fileErr := l.err
		if fileErr == nil {
			fileErr = storeFileRaw(pushAssociation{su: su, watch: watch}, r.paths[i],
				l.ident, l.raw, r.convertSlots)
		}
		l.release()
		var assocErr *pushAssociationError
		if errors.As(fileErr, &assocErr) {
			return slices.Clone(files[l.k:]), why(assocErr.err)
		}
		r.report(i, fileErr)
	}
	return nil, nil
}
