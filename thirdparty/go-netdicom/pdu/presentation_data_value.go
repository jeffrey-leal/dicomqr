package pdu

import (
	"encoding/binary"
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

// P3.8 9.3.2.2.1 & 9.3.2.2.2
type PresentationDataValueItem struct {
	// Length: 2 + len(Value)
	ContextID byte

	// P3.8, E.2: the following two fields encode a single byte.
	Command bool // Bit 7 (LSB): 1 means command 0 means data
	Last    bool // Bit 6: 1 means last fragment. 0 means not last fragment.

	// Payload, either command or data
	Value []byte
}

func ReadPresentationDataValueItem(d *dicomio.Reader) (PresentationDataValueItem, error) {
	item := PresentationDataValueItem{}
	length, err := d.ReadUInt32()
	if err != nil {
		return PresentationDataValueItem{}, err
	}
	item.ContextID, err = d.ReadUInt8()
	if err != nil {
		return PresentationDataValueItem{}, err
	}
	header, err := d.ReadUInt8()
	if err != nil {
		return PresentationDataValueItem{}, err
	}
	item.Command = (header&1 != 0)
	item.Last = (header&2 != 0)
	item.Value = make([]byte, length-2)
	err = binary.Read(d, binary.BigEndian, &item.Value)
	if err != nil {
		return PresentationDataValueItem{}, err
	}
	return item, nil
}

func (v *PresentationDataValueItem) Write(e *dicomio.Writer) error {
	var header byte
	if v.Command {
		header |= 1
	}
	if v.Last {
		header |= 2
	}
	if err := e.WriteUInt32(uint32(2 + len(v.Value))); err != nil {
		return err
	}
	if err := e.WriteByte(v.ContextID); err != nil {
		return err
	}
	if err := e.WriteByte(header); err != nil {
		return err
	}
	return e.WriteBytes(v.Value)
}

func (v *PresentationDataValueItem) String() string {
	return fmt.Sprintf("PresentationDataValue{context: %d, cmd:%v last:%v value: %d bytes}", v.ContextID, v.Command, v.Last, len(v.Value))
}
