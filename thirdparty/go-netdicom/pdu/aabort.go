package pdu

//go:generate stringer -type AbortReasonType
import (
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

type AbortReasonType byte

const (
	AbortReasonNotSpecified             AbortReasonType = 0
	AbortReasonUnexpectedPDU            AbortReasonType = 2
	AbortReasonUnrecognizedPDUParameter AbortReasonType = 3
	AbortReasonUnexpectedPDUParameter   AbortReasonType = 4
	AbortReasonInvalidPDUParameterValue AbortReasonType = 5
)

type AAbort struct {
	Source SourceType
	Reason AbortReasonType
}

func (AAbort) Read(d *dicomio.Reader) (PDU, error) {
	pdu := &AAbort{}
	if err := d.Skip(2); err != nil {
		return nil, err
	}
	sourceType, err := d.ReadUInt8()
	if err != nil {
		return nil, err
	}
	pdu.Source = SourceType(sourceType)
	reasonType, err := d.ReadUInt8()
	if err != nil {
		return nil, err
	}
	pdu.Reason = AbortReasonType(reasonType)
	return pdu, nil
}

func (pdu *AAbort) Write() ([]byte, error) {
	return []byte{
		0,
		0,
		byte(pdu.Source),
		byte(pdu.Reason),
	}, nil
}

func (pdu *AAbort) String() string {
	return fmt.Sprintf("A_ABORT{source:%v reason:%v}", pdu.Source, pdu.Reason)
}
