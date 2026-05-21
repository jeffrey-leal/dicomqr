package pdu

//go:generate stringer -type RejectReasonType
//go:generate stringer -type RejectResultType
//go:generate stringer -type SourceType
import (
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

// P3.8 9.3.4
type AAssociateRj struct {
	Result RejectResultType
	Source SourceType
	Reason RejectReasonType
}

// Possible values for AAssociateRj.Result
type RejectResultType byte

const (
	ResultRejectedPermanent RejectResultType = 1
	ResultRejectedTransient RejectResultType = 2
)

// Possible values for AAssociateRj.Reason
type RejectReasonType byte

const (
	RejectReasonNone                               RejectReasonType = 1
	RejectReasonApplicationContextNameNotSupported RejectReasonType = 2
	RejectReasonCallingAETitleNotRecognized        RejectReasonType = 3
	RejectReasonCalledAETitleNotRecognized         RejectReasonType = 7
)

// Possible values for AAssociateRj.Source
type SourceType byte

const (
	SourceULServiceUser                 SourceType = 1
	SourceULServiceProviderACSE         SourceType = 2
	SourceULServiceProviderPresentation SourceType = 3
)

func (AAssociateRj) Read(d *dicomio.Reader) (PDU, error) {
	pdu := &AAssociateRj{}
	if err := d.Skip(1); err != nil {
		return nil, err
	}
	rejectResult, err := d.ReadUInt8()
	if err != nil {
		return nil, err
	}
	pdu.Result = RejectResultType(rejectResult)
	source, err := d.ReadUInt8()
	if err != nil {
		return nil, err
	}
	pdu.Source = SourceType(source)
	reason, err := d.ReadUInt8()
	if err != nil {
		return nil, err
	}
	pdu.Reason = RejectReasonType(reason)
	return pdu, nil
}

func (pdu *AAssociateRj) Write() ([]byte, error) {
	data := []byte{
		0,
		byte(pdu.Result),
		byte(pdu.Source),
		byte(pdu.Reason),
	}
	return data, nil
}

func (pdu *AAssociateRj) String() string {
	return fmt.Sprintf("A_ASSOCIATE_RJ{result: %v, source: %v, reason: %v}", pdu.Result, pdu.Source, pdu.Reason)
}
