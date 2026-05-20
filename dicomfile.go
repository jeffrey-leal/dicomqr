package main

import (
	"io"
	"os"
)

const dicomMagicOffset = 128

var dicomMagic = []byte{'D', 'I', 'C', 'M'}

// isDICOMFile reports whether path is a valid DICOM file by checking for the
// "DICM" signature at byte offset 128.
func isDICOMFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, dicomMagicOffset+len(dicomMagic))
	n, err := io.ReadFull(f, buf)
	if err != nil || n < len(buf) {
		return false
	}

	for i, b := range dicomMagic {
		if buf[dicomMagicOffset+i] != b {
			return false
		}
	}
	return true
}
