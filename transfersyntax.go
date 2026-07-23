package main

import (
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

// fileTransferSyntaxUID reads the Transfer Syntax UID (0002,0010) from a DICOM
// file. NewParser consumes only the group 0002 meta elements up front, so the
// dataset itself — pixel data, deep SR sequences — is never touched. Returns
// "" when the file cannot be parsed.
func fileTransferSyntaxUID(path string) string {
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
