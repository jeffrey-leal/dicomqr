package pdu_item

import (
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

type AbstractSyntaxSubItem subItemWithName

func decodeAbstractSyntaxSubItem(d *dicomio.Reader, length uint16) (*AbstractSyntaxSubItem, error) {
	name, err := DecodeSubItemWithName(d, length)
	if err != nil {
		return nil, err
	}
	return &AbstractSyntaxSubItem{Name: name}, nil
}

func (v *AbstractSyntaxSubItem) Write(e *dicomio.Writer) error {
	return encodeSubItemWithName(e, ItemTypeAbstractSyntax, v.Name)
}

func (v *AbstractSyntaxSubItem) String() string {
	return fmt.Sprintf("AbstractSyntax{name: \"%s\"}", v.Name)
}
