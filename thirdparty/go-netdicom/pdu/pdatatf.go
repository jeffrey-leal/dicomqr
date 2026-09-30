package pdu

import (
	"bytes"
	"encoding/binary"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

type PDataTf struct {
	Items []PresentationDataValueItem
}

func (PDataTf) Read(d *dicomio.Reader) (PDU, error) {
	pdu := &PDataTf{}
	for !d.IsLimitExhausted() {
		item, err := ReadPresentationDataValueItem(d)
		if err != nil {
			return nil, err
		}
		pdu.Items = append(pdu.Items, item)
	}
	return pdu, nil
}

// encode is EncodePDU for a P-DATA-TF (dicomqr local patch): the 6-byte PDU
// header and every item written straight into one buffer of exactly the
// encoded size — the same bytes Write plus EncodePDU's header produce, with
// one allocation and no intermediate copy.
func (pdu *PDataTf) encode() []byte {
	size := 0
	for _, item := range pdu.Items {
		size += 6 + len(item.Value) // item length (4), context ID (1), control header (1), value
	}
	out := make([]byte, 6+size)
	out[0] = byte(TypePDataTf)
	binary.BigEndian.PutUint32(out[2:6], uint32(size))
	off := 6
	for _, item := range pdu.Items {
		var header byte
		if item.Command {
			header |= 1
		}
		if item.Last {
			header |= 2
		}
		binary.BigEndian.PutUint32(out[off:], uint32(2+len(item.Value)))
		out[off+4] = item.ContextID
		out[off+5] = header
		off += 6
		off += copy(out[off:], item.Value)
	}
	return out
}

func (pdu *PDataTf) Write() ([]byte, error) {
	var buf bytes.Buffer
	e := dicomio.NewWriter(&buf, binary.BigEndian, false)
	for _, item := range pdu.Items {
		if err := item.Write(e); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func (pdu *PDataTf) String() string {
	buf := bytes.Buffer{}
	buf.WriteString("P_DATA_TF{items: [")
	for i, item := range pdu.Items {
		if i > 0 {
			buf.WriteString("\n")
		}
		buf.WriteString(item.String())
	}
	buf.WriteString("]}")
	return buf.String()
}
