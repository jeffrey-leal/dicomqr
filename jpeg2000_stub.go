//go:build !openjpeg

// Fallback used when the "openjpeg" build tag is not set: JPEG 2000 decoding is
// not compiled in (no OpenJPEG dependency). jpeg2000_openjpeg.go provides the
// real decoder under the openjpeg tag.
package main

import "errors"

func decodeJPEG2000Frame(_ []byte, _, _ float64, _ bool, _, _ float64, _ string) (*decodedFrame, error) {
	return nil, errors.New("JPEG 2000 support is not built into this version of dicomqr\n\nUse Open in Viewer to open this file in an external DICOM viewer.")
}
