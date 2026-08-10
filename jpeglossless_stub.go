//go:build !jpeglossless

// Fallback used when the "jpeglossless" build tag is not set: JPEG Lossless
// decoding is not compiled in (no libjpeg-turbo dependency).
// jpeglossless_turbo.go provides the real decoder under the jpeglossless tag.
package main

import "errors"

// jpegLosslessAvailable reports at compile time whether the libjpeg-turbo
// lossless decoder is linked in. The viewer checks it before letting a
// JPEG Lossless file past the unsupported-syntax veto.
const jpegLosslessAvailable = false

func decodeJPEGLosslessFrame(_ []byte, _, _ float64, _ bool, _, _ float64, _ string, _ bool) (*decodedFrame, error) {
	return nil, errors.New("JPEG Lossless support is not built into this version of dicomqr\n\nUse Open in Viewer to open this file in an external DICOM viewer.")
}

func decodeJPEGLossless(_ []byte) (width, height, numComps, prec int, signed bool, samples []int32, err error) {
	return 0, 0, 0, 0, false, nil, errors.New("JPEG Lossless support is not built into this version of dicomqr")
}

func encodeJPEGLossless(_ []int32, _, _, _, _ int) ([]byte, error) {
	return nil, errors.New("JPEG Lossless support is not built into this version of dicomqr")
}
