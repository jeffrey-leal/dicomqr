//go:build !openjpeg

// Fallback used when the "openjpeg" build tag is not set: JPEG 2000 decoding is
// not compiled in (no OpenJPEG dependency). jpeg2000_openjpeg.go provides the
// real decoder under the openjpeg tag.
package main

import "errors"

// jpeg2000Available reports at compile time whether the OpenJPEG decoder is
// linked in. The local decompress fallback checks it before attempting to
// transcode a JPEG 2000 file.
const jpeg2000Available = false

func decodeJPEG2000Frame(_ []byte, _, _ float64, _ bool, _, _ float64, _ string) (*decodedFrame, error) {
	return nil, errors.New("JPEG 2000 support is not built into this version of dicomqr\n\nUse Open in Viewer to open this file in an external DICOM viewer.")
}

func decodeJPEG2000(_ []byte) (width, height, numComps, prec int, signed bool, samples []int32, err error) {
	return 0, 0, 0, 0, false, nil, errors.New("JPEG 2000 support is not built into this version of dicomqr")
}
