package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
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
	entries := lc.Entries()
	if len(entries) != 3 || entries[0].text != "b" || entries[2].text != "d" {
		t.Errorf("ring contents = %v, want [b c d]", entries)
	}
}

func TestClassifyLogLine(t *testing.T) {
	cases := []struct {
		line string
		want logLevel
	}{
		// Tagged app lines: marker sits after the log package's timestamp.
		{"12:00:00.000000 [E] retrieve: study 3/5 error (continuing): boom", logLevelError},
		{"12:00:00.000000 [W] scp: SKIPPED 1.2.3 — cannot convert", logLevelWarn},
		{"12:00:00.000000 [I] scp: association from 10.0.0.1:104", logLevelInfo},
		// Direct ring writes carry the marker at the start of the line.
		{"[E] dicom.log unwritable: access denied", logLevelError},
		// Untagged lines are protocol chatter.
		{"12:00:00.000000 dicom.serviceDispatcher(1): dispatch C-STORE-RQ", logLevelProto},
		{"plain library output with no marker", logLevelProto},
		// Backstop: exceptional lines can never sink below the errors view.
		{"12:00:00.000000 FATAL: panic on main goroutine: nil deref", logLevelError},
		{"12:00:00.000000 scp: PANIC receiving 1.2.3: boom", logLevelError},
		// A message merely mentioning a count of failures is not an error line.
		{"12:00:00.000000 [I] modify: \"p\" finished — 5 written, 0 skipped, 0 failed", logLevelInfo},
	}
	for _, tc := range cases {
		if got := classifyLogLine(tc.line); got != tc.want {
			t.Errorf("classifyLogLine(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestLogCaptureMultiLineInheritsLevel(t *testing.T) {
	lc := newLogCapture(100)
	// One Write call = one log message: a panic with its stack trace. Every
	// line must inherit the first line's severity so the stack stays visible
	// at the errors-only view.
	lc.Write([]byte("12:00:00.000000 [E] FATAL: panic on main goroutine: boom\ngoroutine 1 [running]:\nmain.crash()\n"))
	entries := lc.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	for i, e := range entries {
		if e.level != logLevelError {
			t.Errorf("entry %d level = %v, want error", i, e.level)
		}
	}
}

func TestRenderLogFiltering(t *testing.T) {
	entries := []logEntry{
		{logLevelError, "E1 retrieve failed"},
		{logLevelWarn, "W1 skipped object"},
		{logLevelInfo, "I1 association from pacs"},
		{logLevelProto, "P1 dimse chatter"},
	}

	if got := renderLog(entries, logLevelError, ""); got != "E1 retrieve failed" {
		t.Errorf("errors-only view = %q", got)
	}
	if got := renderLog(entries, logLevelWarn, ""); got != "E1 retrieve failed\nW1 skipped object" {
		t.Errorf("warnings view = %q", got)
	}
	if got := renderLog(entries, logLevelProto, ""); strings.Count(got, "\n") != 3 {
		t.Errorf("everything view should hold all 4 lines, got %q", got)
	}
	// Substring filter is case-insensitive and composes with the level.
	if got := renderLog(entries, logLevelProto, "PACS"); got != "I1 association from pacs" {
		t.Errorf("substring view = %q", got)
	}
	if got := renderLog(entries, logLevelWarn, "chatter"); got != "" {
		t.Errorf("substring above level should be empty, got %q", got)
	}
}

func TestLogViewIndexDefaults(t *testing.T) {
	if i := logViewIndex(""); logViewOptions[i].key != "activity" {
		t.Errorf("empty setting resolves to %q, want activity", logViewOptions[i].key)
	}
	if i := logViewIndex("bogus"); logViewOptions[i].key != "activity" {
		t.Errorf("unknown setting resolves to %q, want activity", logViewOptions[i].key)
	}
	if i := logViewIndex("errors"); logViewOptions[i].max != logLevelError {
		t.Errorf("errors setting resolves to max %v", logViewOptions[i].max)
	}
}
