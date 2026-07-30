//go:build !jpeglossless

package main

import (
	"strings"
	"testing"
)

// Without the jpeglossless tag the availability constant must be false and
// both stubs must fail with the standard "not built into" message.
func TestJPEGLosslessStub(t *testing.T) {
	if jpegLosslessAvailable {
		t.Fatal("jpegLosslessAvailable must be false without the jpeglossless tag")
	}
	if _, _, _, _, _, _, err := decodeJPEGLossless([]byte{0xFF, 0xD8}); err == nil || !strings.Contains(err.Error(), "not built into") {
		t.Errorf("decodeJPEGLossless stub error = %v, want 'not built into'", err)
	}
	if _, err := decodeJPEGLosslessFrame(nil, 1, 0, false, 0, 0, "RGB", false); err == nil || !strings.Contains(err.Error(), "not built into") {
		t.Errorf("decodeJPEGLosslessFrame stub error = %v, want 'not built into'", err)
	}
}
