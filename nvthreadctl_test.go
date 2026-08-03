package main

// Self-test for the NVAPI driver-profile guard. It writes to the system's
// NVIDIA driver settings store (creating and then deleting a throwaway
// profile), so it only runs when explicitly requested:
//
//	DICOMQR_NVAPI_TEST=1 go test -run TestNvapiThreadControlRoundTrip -v .
//
// On a machine without an NVIDIA driver it skips.

import (
	"errors"
	"os"
	"testing"
)

func TestNvapiThreadControlRoundTrip(t *testing.T) {
	if os.Getenv("DICOMQR_NVAPI_TEST") == "" {
		t.Skip("set DICOMQR_NVAPI_TEST=1 to run (writes to the NVIDIA driver profile store)")
	}
	const testApp = "dicomqr-nvapi-selftest.exe"
	const testProfile = "dicomqr NVAPI self-test"

	// Always try to clean up, whatever happens mid-test.
	defer func() {
		s, err := nvapiOpen()
		if err != nil {
			return
		}
		defer s.close()
		if err := s.deleteProfileByName(testProfile); err != nil {
			t.Logf("cleanup: %v (a leftover %q profile is harmless; delete via NVIDIA Control Panel)", err, testProfile)
		}
	}()

	changed, err := nvapiEnsureThreadControlOff(testApp, testProfile)
	if errors.Is(err, errNoNVAPI) {
		t.Skip("no NVIDIA driver on this machine")
	}
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if !changed {
		t.Fatal("first ensure reported nothing written for a fresh profile")
	}

	// Read back through a fresh session: the setting must have persisted.
	s, err := nvapiOpen()
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	hProfile, err := s.profileForApp(testApp, testProfile)
	if err != nil {
		s.close()
		t.Fatalf("find profile after write: %v", err)
	}
	v, ok := s.threadControl(hProfile)
	s.close()
	if !ok {
		t.Fatal("OGL_THREAD_CONTROL not present after write")
	}
	if v != nvOGLThreadControlDisable {
		t.Fatalf("OGL_THREAD_CONTROL = %d, want %d (off)", v, nvOGLThreadControlDisable)
	}

	// Second ensure must be a no-op.
	changed, err = nvapiEnsureThreadControlOff(testApp, testProfile)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if changed {
		t.Error("second ensure rewrote an already-configured profile")
	}
}
