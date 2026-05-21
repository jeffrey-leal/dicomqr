package pdu_item

import (
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

// PS3.7 Annex D.3.3.2.1
type ImplementationClassUIDSubItem subItemWithName

func decodeImplementationClassUIDSubItem(d *dicomio.Reader, length uint16) (*ImplementationClassUIDSubItem, error) {
	name, err := DecodeSubItemWithName(d, length)
	if err != nil {
		return nil, err
	}
	return &ImplementationClassUIDSubItem{Name: name}, nil
}

func (v *ImplementationClassUIDSubItem) Write(e *dicomio.Writer) error {
	return encodeSubItemWithName(e, ItemTypeImplementationClassUID, v.Name)
}

func (v *ImplementationClassUIDSubItem) String() string {
	return fmt.Sprintf("ImplementationClassUID{name: \"%s\"}", v.Name)
}
