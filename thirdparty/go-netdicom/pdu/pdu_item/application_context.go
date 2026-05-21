package pdu_item

import (
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

type ApplicationContextItem subItemWithName

// The app context for DICOM. The first item in the A-ASSOCIATE-RQ
const DICOMApplicationContextItemName = "1.2.840.10008.3.1.1.1"

func decodeApplicationContextItem(d *dicomio.Reader, length uint16) (*ApplicationContextItem, error) {
	name, err := DecodeSubItemWithName(d, length)
	if err != nil {
		return nil, err
	}
	return &ApplicationContextItem{Name: name}, nil
}

func (v *ApplicationContextItem) Write(e *dicomio.Writer) error {
	return encodeSubItemWithName(e, ItemTypeApplicationContext, v.Name)
}

func (v *ApplicationContextItem) String() string {
	return fmt.Sprintf("ApplicationContext{name: \"%s\"}", v.Name)
}
