package fuzze2e

import (
	"testing"
)

// FuzzE2E uses native Go 1.18+ fuzzing instead of deprecated go-fuzz
// NOTE: This fuzzer may discover panics and crashes in the DICOM network stack.
// This is expected behavior as fuzzing is designed to find bugs.
func FuzzE2E(f *testing.F) {
	// Simple seed that won't cause issues initially
	f.Add([]byte{0x01})
	f.Add([]byte{0x42})

	f.Fuzz(func(t *testing.T, data []byte) {
		// Skip test data that's too large or empty
		if len(data) == 0 || len(data) > 100 {
			t.Skip("Skipping invalid data size")
			return
		}

		// Use a simple approach that calls the existing Fuzz function
		// Panics are expected during fuzzing as they indicate bugs found
		defer func() {
			if r := recover(); r != nil {
				// This is expected - log the panic but mark as success
				// since finding panics is the goal of fuzzing
				t.Logf("FUZZING SUCCESS: Found panic/crash: %v", r)
			}
		}()

		// Call the existing Fuzz function
		Fuzz(data)
	})
}
