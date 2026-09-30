//go:build !jpeglossless

package main

import (
	"errors"
	"image"
)

// decodeJPEGForDisplay needs libjpeg-turbo (the jpeglossless build tag); without
// it every call fails and the viewer uses Go's JPEG decoder, as it always did.
func decodeJPEGForDisplay(_ []byte, _ int) (image.Image, error) {
	return nil, errors.New("libjpeg-turbo not built in")
}
