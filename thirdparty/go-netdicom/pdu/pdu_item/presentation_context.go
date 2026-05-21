package pdu_item

//go:generate stringer -type PresentationContextResult

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

// Result of abstractsyntax/transfersyntax handshake during A-ACCEPT.  P3.8,
// 90.3.3.2, table 9-18.
type PresentationContextResult byte

const (
	PresentationContextAccepted                                    PresentationContextResult = 0
	PresentationContextUserRejection                               PresentationContextResult = 1
	PresentationContextProviderRejectionNoReason                   PresentationContextResult = 2
	PresentationContextProviderRejectionAbstractSyntaxNotSupported PresentationContextResult = 3
	PresentationContextProviderRejectionTransferSyntaxNotSupported PresentationContextResult = 4
)

// P3.8 9.3.2.2, 9.3.3.2
type PresentationContextItem struct {
	Type      byte // ItemTypePresentationContext*
	ContextID byte
	// 1 byte reserved

	// Result is meaningful iff Type=0x21, zero else.
	Result PresentationContextResult

	// 1 byte reserved
	Items []SubItem // List of {Abstract,Transfer}SyntaxSubItem
}

func decodePresentationContextItem(d *dicomio.Reader, itemType byte, length uint16) (*PresentationContextItem, error) {
	v := &PresentationContextItem{Type: itemType}
	var err error
	d.PushLimit(int64(length))
	defer d.PopLimit()
	v.ContextID, err = d.ReadUInt8()
	if err != nil {
		return nil, err
	}
	err = d.Skip(1)
	if err != nil {
		return nil, err
	}
	presentationContextResult, err := d.ReadUInt8()
	if err != nil {
		return nil, err
	}
	v.Result = PresentationContextResult(presentationContextResult)
	err = d.Skip(1)
	if err != nil {
		return nil, err
	}
	for !d.IsLimitExhausted() {
		item, err := DecodeSubItem(d)
		if err != nil {
			return nil, err
		}
		v.Items = append(v.Items, item)
	}
	if v.ContextID%2 != 1 {
		return nil, fmt.Errorf("PresentationContextItem ID must be odd, but found %x", v.ContextID)
	}
	return v, nil
}

func (v *PresentationContextItem) Write(e *dicomio.Writer) error {
	if v.Type != ItemTypePresentationContextRequest &&
		v.Type != ItemTypePresentationContextResponse {
		panic(*v)
	}
	var itemBuffer bytes.Buffer
	itemEncoder := dicomio.NewWriter(&itemBuffer, binary.BigEndian, true)
	for _, s := range v.Items {
		if err := s.Write(itemEncoder); err != nil {
			return err
		}
	}
	itemBytes := itemBuffer.Bytes()
	subItemLength := uint16(SubItemHeaderLength + len(itemBytes))
	if err := encodeSubItemHeader(e, v.Type, subItemLength); err != nil {
		return err
	}
	if err := e.WriteByte(v.ContextID); err != nil {
		return err
	}
	if err := e.WriteZeros(3); err != nil {
		return err
	}
	return e.WriteBytes(itemBytes)
}

func (v *PresentationContextItem) String() string {
	itemType := "rq"
	if v.Type == ItemTypePresentationContextResponse {
		itemType = "ac"
	}
	return fmt.Sprintf("PresentationContext%s{id: %d result: %d, items:%s}",
		itemType, v.ContextID, v.Result, SubItemListString(v.Items))
}
