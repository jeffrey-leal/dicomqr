package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// The two uncompressed transfer syntaxes a profile can require on disk
// (PS3.5 §10.1). Strict negotiation offers exactly one of these to the server;
// there is no local transcoding fallback.
const (
	tsImplicitVRLE = "1.2.840.10008.1.2"
	tsExplicitVRLE = "1.2.840.10008.1.2.1"
)

// transferSyntaxLabel returns a short human-readable name for the two
// requirable transfer syntaxes, used in status and error messages. Falls back
// to the raw UID for anything else.
func transferSyntaxLabel(uid string) string {
	switch uid {
	case tsImplicitVRLE:
		return "Implicit VR Little Endian"
	case tsExplicitVRLE:
		return "Explicit VR Little Endian"
	}
	return uid
}

// dicomFileIdentity is the File Meta information the raw C-STORE push needs:
// the three identifying UIDs plus the exact byte offset where the dataset
// (everything after the group 0002 meta elements) begins.
type dicomFileIdentity struct {
	sopClassUID       string
	sopInstanceUID    string
	transferSyntaxUID string
	datasetOffset     int64
}

// fileMetaIdentity scans the File Meta group of a Part-10 file by hand —
// PS3.10 §7.1 fixes it to Explicit VR Little Endian regardless of the dataset
// syntax — because the push needs the exact byte offset where the dataset
// begins, which element-level parsers do not expose. The scan stops at the
// first non-0002 group without touching the dataset.
func fileMetaIdentity(path string) (dicomFileIdentity, error) {
	var id dicomFileIdentity
	f, err := os.Open(path)
	if err != nil {
		return id, err
	}
	defer f.Close()

	header := make([]byte, 132)
	if _, err := io.ReadFull(f, header); err != nil {
		return id, fmt.Errorf("read preamble: %w", err)
	}
	if string(header[128:132]) != "DICM" {
		return id, errors.New("not a DICOM part-10 file (no DICM marker)")
	}

	r := bufio.NewReader(f)
	pos := int64(132)
	readN := func(n int) ([]byte, error) {
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		pos += int64(n)
		return buf, nil
	}

	for {
		peek, perr := r.Peek(2)
		if perr != nil {
			// EOF right after the meta group: a meta-only file. The offset
			// points at the (empty) dataset; the caller decides whether that
			// is an error.
			id.datasetOffset = pos
			break
		}
		if binary.LittleEndian.Uint16(peek) != 0x0002 {
			id.datasetOffset = pos
			break
		}
		hdr, err := readN(8) // group, element, VR, 16-bit length (or reserved)
		if err != nil {
			return id, fmt.Errorf("read meta element header: %w", err)
		}
		elem := binary.LittleEndian.Uint16(hdr[2:4])
		var vlen uint32
		switch string(hdr[4:6]) {
		case "OB", "OW", "OF", "SQ", "UT", "UN":
			lenBuf, lerr := readN(4) // hdr[6:8] were the reserved bytes
			if lerr != nil {
				return id, fmt.Errorf("read meta element length: %w", lerr)
			}
			vlen = binary.LittleEndian.Uint32(lenBuf)
		default:
			vlen = uint32(binary.LittleEndian.Uint16(hdr[6:8]))
		}
		if vlen == 0xFFFFFFFF {
			return id, errors.New("undefined-length element in file meta group")
		}
		if vlen > 1<<20 {
			return id, fmt.Errorf("implausible meta element length %d — corrupt file meta", vlen)
		}
		val, verr := readN(int(vlen))
		if verr != nil {
			return id, fmt.Errorf("read meta element value: %w", verr)
		}
		s := strings.TrimRight(string(val), "\x00 ")
		switch elem {
		case 0x0002:
			id.sopClassUID = s
		case 0x0003:
			id.sopInstanceUID = s
		case 0x0010:
			id.transferSyntaxUID = s
		}
	}
	if id.sopClassUID == "" || id.sopInstanceUID == "" || id.transferSyntaxUID == "" {
		return id, errors.New("file meta group lacks SOP Class, SOP Instance, or Transfer Syntax UID")
	}
	return id, nil
}

// fileTransferSyntaxUID reads the Transfer Syntax UID (0002,0010) from a DICOM
// file. NewParser consumes only the group 0002 meta elements up front, so the
// dataset itself — pixel data, deep SR sequences — is never touched. Returns
// "" when the file cannot be parsed.
func fileTransferSyntaxUID(path string) string {
	// NewParser reads the File Meta group eagerly, so the panic risk is in the
	// constructor rather than in a Next loop. An unreadable file yields "" here
	// exactly as a parse error does.
	defer recoverParserPanic(path)
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	p, err := sdicom.NewParser(f, info.Size(), nil, sdicom.SkipPixelData())
	if err != nil {
		return ""
	}
	meta := p.GetMetadata()
	elem, err := meta.FindElementByTag(tag.TransferSyntaxUID)
	if err != nil {
		return ""
	}
	if strs, ok := elem.Value.GetValue().([]string); ok && len(strs) > 0 {
		return strings.TrimSpace(strs[0])
	}
	return ""
}
