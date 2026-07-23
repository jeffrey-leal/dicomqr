package main

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("invalid handle") }

// A failing sink (the invalid stderr handle of a -H windowsgui launch) must
// never starve the sinks that follow it in an io.MultiWriter — this is the
// bug that left dicom.log and the Activity Log empty in all release builds.
func TestFailsafeWriterSurvivesBrokenSink(t *testing.T) {
	var file, ring bytes.Buffer

	// The fixed wiring: every sink wrapped, file first.
	mw := io.MultiWriter(failsafeWriter{&file}, failsafeWriter{&ring}, failsafeWriter{errorWriter{}})
	msg := []byte("negotiation evidence\n")
	n, err := mw.Write(msg)
	if err != nil || n != len(msg) {
		t.Fatalf("MultiWriter write: n=%d err=%v", n, err)
	}
	if file.String() != string(msg) {
		t.Errorf("file sink got %q", file.String())
	}
	if ring.String() != string(msg) {
		t.Errorf("ring sink got %q", ring.String())
	}

	// Regression demonstration: the old wiring (bare failing sink first)
	// discards everything — this is what must never come back.
	var file2 bytes.Buffer
	old := io.MultiWriter(errorWriter{}, &file2)
	old.Write(msg)
	if file2.Len() != 0 {
		t.Fatalf("expected the old wiring to demonstrate the loss, got %q", file2.String())
	}
}

func TestLogCaptureRing(t *testing.T) {
	lc := newLogCapture(3)
	for _, s := range []string{"a\n", "b\n", "c\n", "d\n"} {
		if _, err := lc.Write([]byte(s)); err != nil {
			t.Fatalf("Write(%q): %v", s, err)
		}
	}
	lines := lc.Lines()
	if len(lines) != 3 || lines[0] != "b" || lines[2] != "d" {
		t.Errorf("ring contents = %v, want [b c d]", lines)
	}
}
