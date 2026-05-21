package pdu_item

import (
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

// P3.8 D.1
type UserInformationMaximumLengthItem struct {
	MaximumLengthReceived uint32
}

func (v *UserInformationMaximumLengthItem) Write(e *dicomio.Writer) error {
	if err := encodeSubItemHeader(e, ItemTypeUserInformationMaximumLength, 4); err != nil {
		return err
	}
	return e.WriteUInt32(v.MaximumLengthReceived)
}

func decodeUserInformationMaximumLengthItem(d *dicomio.Reader, length uint16) (*UserInformationMaximumLengthItem, error) {
	if length != 4 {
		return nil, fmt.Errorf("UserInformationMaximumLengthItem must be 4 bytes, but found %dB", length)
	}
	maximumLengthReceived, err := d.ReadUInt32()
	if err != nil {
		return nil, err
	}
	return &UserInformationMaximumLengthItem{MaximumLengthReceived: maximumLengthReceived}, nil
}

func (v *UserInformationMaximumLengthItem) String() string {
	return fmt.Sprintf("UserInformationMaximumlengthItem{%d}",
		v.MaximumLengthReceived)
}
