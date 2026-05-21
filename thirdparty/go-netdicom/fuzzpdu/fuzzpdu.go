package fuzzpdu

import (
	"bytes"

	"github.com/algm/go-netdicom/dimse"
	"github.com/algm/go-netdicom/pdu"
	"github.com/suyashkumar/dicom"
)

func Fuzz(data []byte) int {
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
	return 0
}
