package main

// The parse-panic boundary.
//
// suyashkumar/dicom panics rather than returning an error on some malformed
// datasets — a truncated sequence, an implausible value length, a corrupt
// character set. That is survivable on the main goroutine (main.go's handler
// logs it and shuts down cleanly) but fatal everywhere else: Go cannot recover
// a panic raised on another goroutine, and this application parses files on
// worker pools all over — the modification engine, the mask review scan, the
// series scanners, the receive path.
//
// The download folder is not a trusted input either. It holds whatever a PACS
// sent, whatever the Import tab copied off an outside CD, and whatever a user
// dropped in by hand.
//
// Every parse in the application therefore goes through one of the three
// helpers below, so a bad file is a reported failure rather than a dead
// process. They are used uniformly rather than only on the background paths:
// "wrap only the parses that run off the UI goroutine" is an invariant that
// would rot the first time one of these helpers gained a second caller.
//
// A recovered panic is never silent — each helper logs the file that caused it,
// because the Activity Log is the only place that names the file afterwards.

import (
	"fmt"
	"io"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
)

// safeParseFile is sdicom.ParseFile with a parser panic converted into an
// ordinary error. frameCh may be nil; when it is not, note that the library
// closes it only on a successful parse, so a caller draining it must still
// perform its own close on the error path (see parseDicomFile in viewer.go).
func safeParseFile(path string, frameCh chan *frame.Frame, opts ...sdicom.ParseOption) (ds sdicom.Dataset, err error) {
	defer func() {
		if r := recover(); r != nil {
			logWarn("dicom: parser panic reading %s: %v", path, r)
			err = fmt.Errorf("parser panic: %v", r)
		}
	}()
	return sdicom.ParseFile(path, frameCh, opts...)
}

// safeParse is sdicom.Parse with a parser panic converted into an ordinary
// error. what names the source for the log line — a file path where the caller
// has one, since a reader has no name of its own.
func safeParse(r io.Reader, size int64, what string, frameCh chan *frame.Frame, opts ...sdicom.ParseOption) (ds sdicom.Dataset, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			logWarn("dicom: parser panic reading %s: %v", what, rec)
			err = fmt.Errorf("parser panic: %v", rec)
		}
	}()
	return sdicom.Parse(r, size, frameCh, opts...)
}

// recoverParserPanic is the boundary for helpers built on sdicom.NewParser,
// whose contract is already "return zero values when the file cannot be read":
// a panic simply ends the scan early and the caller sees the same absence a
// parse error would produce. Used as
//
//	defer recoverParserPanic(path)
//
// which works because a deferred function value calls recover() directly —
// wrapping it one level deeper would not.
func recoverParserPanic(what string) {
	if r := recover(); r != nil {
		logWarn("dicom: parser panic reading %s: %v", what, r)
	}
}
