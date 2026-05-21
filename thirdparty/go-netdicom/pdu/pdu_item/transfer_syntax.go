package pdu_item

import (
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

type TransferSyntaxSubItem subItemWithName

func decodeTransferSyntaxSubItem(d *dicomio.Reader, length uint16) (*TransferSyntaxSubItem, error) {
	name, err := DecodeSubItemWithName(d, length)
	if err != nil {
		return nil, err
	}
	return &TransferSyntaxSubItem{Name: name}, err
}

func (v *TransferSyntaxSubItem) Write(e *dicomio.Writer) error {
	return encodeSubItemWithName(e, ItemTypeTransferSyntax, v.Name)
}

func (v *TransferSyntaxSubItem) String() string {
	return fmt.Sprintf("TransferSyntax{name: \"%s\"}", v.Name)
}
