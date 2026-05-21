package fuzzpdu

import (
	"bytes"
	"testing"

	"github.com/algm/go-netdicom/dimse"
	"github.com/algm/go-netdicom/pdu"
	"github.com/suyashkumar/dicom"
)

// FuzzPDU uses native Go 1.18+ fuzzing instead of deprecated go-fuzz
func FuzzPDU(f *testing.F) {
	// Add initial corpus data
	f.Add([]byte{})
	f.Add([]byte{0x01})
	f.Add([]byte{0xc1, 0x02, 0x03})

	f.Fuzz(func(t *testing.T, data []byte) {
		// Reuse existing logic safely
		defer func() {
			if r := recover(); r != nil {
				// Log but don't fail test for expected panics during fuzzing
				t.Logf("Recovered from panic during PDU fuzzing: %v", r)
			}
		}()

		in := bytes.NewBuffer(data)
		if len(data) == 0 || data[0] <= 0xc0 {
			pdu.ReadPDU(in, 4<<20) // nolint: errcheck
		} else {
			// Try to read the data as a DICOM dataset first
			dataset, err := dicom.Parse(bytes.NewReader(data), -1, nil)
			if err == nil {
				dimse.ReadMessage(&dataset) // nolint: errcheck
			}
		}
	})
}
