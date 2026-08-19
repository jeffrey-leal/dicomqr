package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// panicReader panics on the first read. The DICOM parser reads the File Meta
// group eagerly, so handing this to safeParse drives a real panic out of the
// real library — the failure mode these helpers exist for — rather than
// simulating one at the boundary.
type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("synthetic parser panic") }

// TestSafeParseRecoversPanic is the load-bearing test for dicomsafe.go: a panic
// raised inside the library must come back as an ordinary error. If the recover
// is ever removed this test does not fail politely — it takes the test binary
// down, which is exactly what it is guarding against in the application.
func TestSafeParseRecoversPanic(t *testing.T) {
	ds, err := safeParse(panicReader{}, 1024, "synthetic.dcm", nil)
	if err == nil {
		t.Fatal("safeParse returned no error for a panicking reader")
	}
	if !strings.Contains(err.Error(), "parser panic") {
		t.Errorf("error = %q, want it to name the panic", err)
	}
	if len(ds.Elements) != 0 {
		t.Errorf("dataset carries %d elements after a panic, want none", len(ds.Elements))
	}
}

// TestRecoverParserPanic covers the variant used by the NewParser helpers,
// whose contract is to return zero values rather than an error.
func TestRecoverParserPanic(t *testing.T) {
	returned := false
	func() {
		defer recoverParserPanic("synthetic.dcm")
		panic("synthetic parser panic")
	}()
	returned = true
	if !returned {
		t.Fatal("unreachable")
	}
}

// TestSafeParseFileMalformed feeds safeParseFile a file with a valid preamble
// and DICM magic followed by garbage. Whether the library errors or panics on
// any particular corruption is not something this test can pin down — the point
// is only that neither outcome escapes as a panic.
func TestSafeParseFileMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.dcm")
	data := make([]byte, 128)
	data = append(data, 'D', 'I', 'C', 'M')
	for i := range 512 {
		data = append(data, byte(i*7+3))
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := safeParseFile(path, nil); err == nil {
		t.Log("malformed file parsed without error — acceptable; the test asserts only that nothing panicked")
	}
}

// TestSafeParseFileRoundTrip confirms the wrapper is transparent for a file
// that parses normally — the recover must not change the success path.
func TestSafeParseFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.dcm")
	writeModifyTestDICOM(t, path)

	ds, err := safeParseFile(path, nil)
	if err != nil {
		t.Fatalf("safeParseFile on a valid file: %v", err)
	}
	if len(ds.Elements) == 0 {
		t.Fatal("safeParseFile returned an empty dataset for a valid file")
	}
}
