package pdu

import (
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

type AReleaseRp struct {
}

func (AReleaseRp) Read(d *dicomio.Reader) (PDU, error) {
	pdu := &AReleaseRp{}
	return pdu, d.Skip(4)
}

func (pdu *AReleaseRp) Write() ([]byte, error) {
	return []byte{0, 0, 0, 0}, nil
}

func (pdu *AReleaseRp) String() string {
	return fmt.Sprintf("A_RELEASE_RP(%v)", *pdu)
}
