package pdu_item

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

// P3.8 9.3.2.3
type UserInformationItem struct {
	Items []SubItem // P3.8, Annex D.
}

func (v *UserInformationItem) Write(e *dicomio.Writer) error {
	var itemBuffer bytes.Buffer
	itemEncoder := dicomio.NewWriter(&itemBuffer, binary.BigEndian, true)
	for _, s := range v.Items {
		if err := s.Write(itemEncoder); err != nil {
			return err
		}
	}
	itemBytes := itemBuffer.Bytes()
	if err := encodeSubItemHeader(e, ItemTypeUserInformation, uint16(len(itemBytes))); err != nil {
		return err
	}
	return e.WriteBytes(itemBytes)
}

func decodeUserInformationItem(d *dicomio.Reader, length uint16) (*UserInformationItem, error) {
	v := &UserInformationItem{}
	if err := d.PushLimit(int64(length)); err != nil {
		return nil, err
	}
	defer d.PopLimit()
	for !d.IsLimitExhausted() {
		item, err := DecodeSubItem(d)
		if err != nil {
			return nil, err
		}
		v.Items = append(v.Items, item)
	}
	return v, nil
}

func (v *UserInformationItem) String() string {
	return fmt.Sprintf("UserInformationItem{items: %s}",
		SubItemListString(v.Items))
}
