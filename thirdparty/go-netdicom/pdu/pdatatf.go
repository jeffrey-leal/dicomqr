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
