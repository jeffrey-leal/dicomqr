package pdu

import (
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

type AReleaseRq struct {
}

func (AReleaseRq) Read(d *dicomio.Reader) (PDU, error) {
	pdu := &AReleaseRq{}
	return pdu, d.Skip(4)
}

func (pdu *AReleaseRq) Write() ([]byte, error) {
	return []byte{0, 0, 0, 0}, nil
}

func (pdu *AReleaseRq) String() string {
	return fmt.Sprintf("A_RELEASE_RQ(%v)", *pdu)
}
