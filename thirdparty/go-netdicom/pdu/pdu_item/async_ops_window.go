package pdu_item

import (
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

// PS3.7 Annex D.3.3.3.1
type AsynchronousOperationsWindowSubItem struct {
	MaxOpsInvoked   uint16
	MaxOpsPerformed uint16
}

func decodeAsynchronousOperationsWindowSubItem(d *dicomio.Reader, length uint16) (*AsynchronousOperationsWindowSubItem, error) {
	maxOpsInvoked, err := d.ReadUInt16()
	if err != nil {
		return nil, err
	}
	maxOpsPerformed, err := d.ReadUInt16()
	if err != nil {
		return nil, err
	}
	return &AsynchronousOperationsWindowSubItem{
		MaxOpsInvoked:   maxOpsInvoked,
		MaxOpsPerformed: maxOpsPerformed,
	}, nil
}

func (v *AsynchronousOperationsWindowSubItem) Write(e *dicomio.Writer) error {
	if err := encodeSubItemHeader(e, ItemTypeAsynchronousOperationsWindow, 4); err != nil {
		return err
	}
	if err := e.WriteUInt16(v.MaxOpsInvoked); err != nil {
		return err
	}
	return e.WriteUInt16(v.MaxOpsPerformed)
}

func (v *AsynchronousOperationsWindowSubItem) String() string {
	return fmt.Sprintf("AsynchronousOpsWindow{invoked: %d performed: %d}",
		v.MaxOpsInvoked, v.MaxOpsPerformed)
}
