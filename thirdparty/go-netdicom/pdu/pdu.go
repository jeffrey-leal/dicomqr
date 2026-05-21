package pdu

//go:generate stringer -type Type

// Implements message types defined in P3.8. It sits below the DIMSE layer.
//
// http://dicom.nema.org/medical/dicom/current/output/pdf/part08.pdf
import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

const CurrentProtocolVersion uint16 = 1

// PDU is the interface for DUL messages like A-ASSOCIATE-AC, P-DATA-TF.
type PDU interface {
	fmt.Stringer
	Write() ([]byte, error)
	Read(*dicomio.Reader) (PDU, error)
}

// Type defines type of the PDU packet.
type Type byte

const (
	TypeAAssociateRq Type = 1 // A_ASSOCIATE_RQ
	TypeAAssociateAc Type = 2 // A_ASSOCIATE_AC
	TypeAAssociateRj Type = 3 // A_ASSOCIATE_RJ
	TypePDataTf      Type = 4 // P_DATA_TF
	TypeAReleaseRq   Type = 5 // A_RELEASE_RQ
	TypeAReleaseRp   Type = 6 // A_RELEASE_RP
	TypeAAbort       Type = 7 // A_ABORT
)

// EncodePDU serializes "pdu" into []byte.
func EncodePDU(pdu PDU) ([]byte, error) {
	var pduType Type
	switch pdu.(type) {
	case *AAssociateRQ:
		pduType = TypeAAssociateRq
	case *AAssociateAC:
		pduType = TypeAAssociateAc
	case *AAssociateRj:
		pduType = TypeAAssociateRj
	case *PDataTf:
		pduType = TypePDataTf
	case *AReleaseRq:
		pduType = TypeAReleaseRq
	case *AReleaseRp:
		pduType = TypeAReleaseRp
	case *AAbort:
		pduType = TypeAAbort
	default:
		panic(fmt.Sprintf("Unknown PDU %v", pdu))
	}
	payload, err := pdu.Write()
	if err != nil {
		return nil, err
	}
	// Reserve the header bytes. It will be filled in Finish.
	var header [6]byte // First 6 bytes of buf.
	header[0] = byte(pduType)
	header[1] = 0 // Reserved.
	binary.BigEndian.PutUint32(header[2:6], uint32(len(payload)))
	return append(header[:], payload...), nil
}

// EncodePDU reads a "pdu" from a stream. maxPDUSize defines the maximum
// possible PDU size, in bytes, accepted by the caller.
func ReadPDU(in io.Reader, maxPDUSize int) (PDU, error) {
	var pduType Type
	var skip byte
	var length uint32
	err := binary.Read(in, binary.BigEndian, &pduType)
	if err != nil {
		return nil, err
	}
	err = binary.Read(in, binary.BigEndian, &skip)
	if err != nil {
		return nil, err
	}
	err = binary.Read(in, binary.BigEndian, &length)
	if err != nil {
		return nil, err
	}
	if length >= uint32(maxPDUSize)*2 {
		// Avoid using too much memory. *2 is just an arbitrary slack.
		return nil, fmt.Errorf("Invalid length %d; it's much larger than max PDU size of %d", length, maxPDUSize)
	}
	d := dicomio.NewReader(
		bufio.NewReader(&io.LimitedReader{R: in, N: int64(length)}),
		binary.BigEndian, // PDU is always big endian
		int64(length))
	switch pduType {
	case TypeAAssociateRq:
		return AAssociateRQ{}.Read(d)
	case TypeAAssociateAc:
		return AAssociateAC{}.Read(d)
	case TypeAAssociateRj:
		return AAssociateRj{}.Read(d)
	case TypeAAbort:
		return AAbort{}.Read(d)
	case TypePDataTf:
		return PDataTf{}.Read(d)
	case TypeAReleaseRq:
		return AReleaseRq{}.Read(d)
	case TypeAReleaseRp:
		return AReleaseRp{}.Read(d)
	default:
		return nil, fmt.Errorf("ReadPDU: unknown message type %d", pduType)
	}
}

// fillString pads the string with " " up to the given length.
func fillString(v string) string {
	if len(v) > 16 {
		return v[:16]
	}
	return v + strings.Repeat(" ", 16-len(v))
}
