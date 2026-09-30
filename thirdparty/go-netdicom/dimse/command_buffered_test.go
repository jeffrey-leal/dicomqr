package dimse

import (
	"io"
	"os"
	"strings"
	"testing"
)

// A buffered command holds a small data set in memory — no temp file at all —
// and reads it back the same way the file-backed form does (dicomqr local
// patch).
func TestBufferedDimseCommandStaysInMemory(t *testing.T) {
	dc := NewBufferedDimseCommand()
	for _, part := range []string{"alpha ", "beta ", "gamma"} {
		if err := dc.AppendData([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if !dc.InMemory() {
		t.Fatal("a 16-byte data set spilled to disk")
	}
	if got := dc.Size(); got != 16 {
		t.Errorf("Size = %d, want 16", got)
	}
	b, _ := dc.Bytes()
	if string(b) != "alpha beta gamma" {
		t.Errorf("Bytes = %q", b)
	}
	first, _ := io.ReadAll(dc.ReadData())
	second, _ := io.ReadAll(dc.ReadData()) // ReadData rewinds
	if string(first) != "alpha beta gamma" || string(second) != string(first) {
		t.Errorf("reads = %q, %q", first, second)
	}
	var sb strings.Builder
	dc.ReadData()
	if n, err := io.Copy(&sb, dc); err != nil || n != 16 || sb.String() != "alpha beta gamma" {
		t.Errorf("io.Copy = %d, %v, %q", n, err, sb.String())
	}
	if err := dc.Ack(); err != nil {
		t.Errorf("Ack: %v", err)
	}
	if err := dc.Ack(); err != nil {
		t.Errorf("second Ack: %v", err)
	}
}

// Past the memory limit the data set moves to a temp file — everything that
// had arrived so far included — and Ack removes that file.
func TestBufferedDimseCommandSpillsPastLimit(t *testing.T) {
	saved := memoryLimit
	memoryLimit = 10
	t.Cleanup(func() { memoryLimit = saved })

	dc := NewBufferedDimseCommand()
	dc.AppendData([]byte("0123456"))
	if !dc.InMemory() {
		t.Fatal("spilled before reaching the limit")
	}
	dc.AppendData([]byte("789abc")) // 13 bytes: over the limit
	dc.AppendData([]byte("def"))
	if dc.InMemory() {
		t.Fatal("did not spill past the limit")
	}
	path := dc.fpath
	b, err := dc.Bytes()
	if err != nil || string(b) != "0123456789abcdef" {
		t.Fatalf("Bytes after spill = %q, %v", b, err)
	}
	if got := dc.Size(); got != 16 {
		t.Errorf("Size after spill = %d, want 16", got)
	}
	var sb strings.Builder
	dc.ReadData()
	if _, err := io.Copy(&sb, dc); err != nil || sb.String() != "0123456789abcdef" {
		t.Errorf("io.Copy after spill = %q, %v", sb.String(), err)
	}
	if err := dc.Ack(); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("spill file %s survived Ack", path)
	}
}
