package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// readDicomHeader returns every element of a file that precedes its pixel data —
// the File Meta group included, exactly as a full parse would return them —
// without reading the pixel data at all.
//
// A full parse cannot do that even with SkipPixelData: the library's Reader.Skip
// is io.CopyN(io.Discard, …), not a seek, so every pixel byte still comes off the
// disk to be thrown away. That is what made the series scans (scanChapters,
// scanMaskSeries), the modification engine's memory weighing and the Import
// tab's naming read scale with study size rather than file count — the same
// defect the Local Browse scan fixed with scanLocalFileMeta.
//
// That scan stops once the stream passes the highest group it needs, which works
// because group 0028 always follows the group 0020 it stops after. These callers
// need tags up to group 0054 and beyond, and the element after those is usually
// the pixel data itself — stopping "once past" would read it first. So instead
// this stops *before* it: the parser is handed a *bufio.Reader of our own, which
// bufio.NewReaderSize returns unchanged rather than wrapping (it does so for any
// Reader already at least as large as requested), and the library's dicomio
// reader keeps no read-ahead of its own. Between two Next calls the next bytes
// in that buffer are therefore the next element's tag, and a Peek sees the pixel
// data group before its value is touched.
//
// Stopping at pixel data rather than at a chosen group also means there is no
// cut-off for a later tag to fall beyond: whatever a caller looks up, if it
// precedes the pixel data it is here. The only elements missing compared with a
// SkipPixelData parse are the PixelData element itself and anything after it
// (trailing padding, digital signatures), which no caller reads.
//
// Where the peek cannot be trusted the read simply continues to the end, which
// is the previous behaviour: a Deflated transfer syntax (the library swaps in a
// decompressing reader, so the buffer holds compressed bytes) and a file with no
// Transfer Syntax UID (the library infers the byte order, which the peek would
// have to guess).
func readDicomHeader(path string) (sdicom.Dataset, error) {
	f, err := os.Open(path)
	if err != nil {
		return sdicom.Dataset{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return sdicom.Dataset{}, err
	}
	return readDicomHeaderFrom(f, info.Size(), path)
}

// headerReadBufferSize is the parser's read size. Large enough that a typical
// header — a few KB, tens with vendor private groups — arrives in one read, so
// on a cold disk it costs one seek; small enough that the read-ahead past the
// header into the pixel data stays negligible. Must be at least bufio's default
// (4096) for the pass-through the peek depends on.
const headerReadBufferSize = 32 << 10

// readDicomHeaderFrom is readDicomHeader over any reader — the seam the tests use
// to count how many bytes a header read actually pulls. what names the source
// for the log line, as in safeParse.
func readDicomHeaderFrom(r io.Reader, size int64, what string) (ds sdicom.Dataset, err error) {
	// NewParser rather than safeParse, so this carries the parse-panic boundary
	// itself (see dicomsafe.go).
	defer func() {
		if rec := recover(); rec != nil {
			logWarn("dicom: parser panic reading %s: %v", what, rec)
			ds, err = sdicom.Dataset{}, fmt.Errorf("parser panic: %v", rec)
		}
	}()

	br := bufio.NewReaderSize(r, headerReadBufferSize)
	p, err := sdicom.NewParser(br, size, nil, sdicom.SkipPixelData())
	if err != nil {
		return sdicom.Dataset{}, err
	}
	meta := p.GetMetadata()
	bo, peekable := headerPeekByteOrder(&meta)

	elems := append([]*sdicom.Element(nil), meta.Elements...)
	for {
		if peekable {
			if b, perr := br.Peek(2); perr == nil && bo.Uint16(b) >= tag.PixelData.Group {
				break
			}
		}
		elem, nerr := p.Next()
		if nerr != nil {
			if errors.Is(nerr, sdicom.ErrorEndOfDICOM) {
				break
			}
			return sdicom.Dataset{}, nerr
		}
		elems = append(elems, elem)
	}
	return sdicom.Dataset{Elements: elems}, nil
}

// headerPeekByteOrder reports the byte order the dataset's tags are written in,
// and whether a peek at the raw stream can be trusted to read them — see
// readDicomHeader for the two cases where it cannot.
func headerPeekByteOrder(meta *sdicom.Dataset) (binary.ByteOrder, bool) {
	ts := datasetTransferSyntaxUID(meta)
	if ts == "" || ts == uid.DeflatedExplicitVRLittleEndian {
		return nil, false
	}
	bo, _, err := uid.ParseTransferSyntaxUID(ts)
	if err != nil {
		return nil, false
	}
	return bo, true
}
