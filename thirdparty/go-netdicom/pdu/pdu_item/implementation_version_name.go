package pdu_item

import (
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

// PS3.7 Annex D.3.3.2.3
type ImplementationVersionNameSubItem subItemWithName

func decodeImplementationVersionNameSubItem(d *dicomio.Reader, length uint16) (*ImplementationVersionNameSubItem, error) {
	name, err := DecodeSubItemWithName(d, length)
	if err != nil {
		return nil, err
	}
	return &ImplementationVersionNameSubItem{Name: name}, nil
}

func (v *ImplementationVersionNameSubItem) Write(e *dicomio.Writer) error {
	return encodeSubItemWithName(e, ItemTypeImplementationVersionName, v.Name)
}

func (v *ImplementationVersionNameSubItem) String() string {
	return fmt.Sprintf("ImplementationVersionName{name: \"%s\"}", v.Name)
}
