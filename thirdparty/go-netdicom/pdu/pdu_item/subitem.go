package pdu_item

import (
	"bytes"
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

const SubItemHeaderLength = 4

// SubItem is the interface for DUL items, such as ApplicationContextItem and
// TransferSyntaxSubItem.
type SubItem interface {
	fmt.Stringer

	// Write serializes the item.
	Write(*dicomio.Writer) error
}

// Possible Type field values for SubItem.
const (
	ItemTypeApplicationContext           = 0x10
	ItemTypePresentationContextRequest   = 0x20
	ItemTypePresentationContextResponse  = 0x21
	ItemTypeAbstractSyntax               = 0x30
	ItemTypeTransferSyntax               = 0x40
	ItemTypeUserInformation              = 0x50
	ItemTypeUserInformationMaximumLength = 0x51
	ItemTypeImplementationClassUID       = 0x52
	ItemTypeAsynchronousOperationsWindow = 0x53
	ItemTypeRoleSelection                = 0x54
	ItemTypeImplementationVersionName    = 0x55
)

func DecodeSubItem(d *dicomio.Reader) (SubItem, error) {
	itemType, err := d.ReadUInt8()
	if err != nil {
		return nil, err
	}
	if err := d.Skip(1); err != nil {
		return nil, err
	}
	length, err := d.ReadUInt16()
	if err != nil {
		return nil, err
	}
	switch itemType {
	case ItemTypeApplicationContext:
		return decodeApplicationContextItem(d, length)
	case ItemTypeAbstractSyntax:
		return decodeAbstractSyntaxSubItem(d, length)
	case ItemTypeTransferSyntax:
		return decodeTransferSyntaxSubItem(d, length)
	case ItemTypePresentationContextRequest:
		return decodePresentationContextItem(d, itemType, length)
	case ItemTypePresentationContextResponse:
		return decodePresentationContextItem(d, itemType, length)
	case ItemTypeUserInformation:
		return decodeUserInformationItem(d, length)
	case ItemTypeUserInformationMaximumLength:
		return decodeUserInformationMaximumLengthItem(d, length)
	case ItemTypeImplementationClassUID:
		return decodeImplementationClassUIDSubItem(d, length)
	case ItemTypeAsynchronousOperationsWindow:
		return decodeAsynchronousOperationsWindowSubItem(d, length)
	case ItemTypeRoleSelection:
		return decodeRoleSelectionSubItem(d, length)
	case ItemTypeImplementationVersionName:
		return decodeImplementationVersionNameSubItem(d, length)
	default:
		return nil, fmt.Errorf("unknown item type: 0x%x", itemType)
	}
}

func encodeSubItemHeader(e *dicomio.Writer, itemType byte, length uint16) error {
	if err := e.WriteByte(itemType); err != nil {
		return err
	}
	if err := e.WriteZeros(1); err != nil {
		return err
	}
	return e.WriteUInt16(length)
}

func SubItemListString(items []SubItem) string {
	buf := bytes.Buffer{}
	buf.WriteString("[")
	for i, subitem := range items {
		if i > 0 {
			buf.WriteString("\n")
		}
		buf.WriteString(subitem.String())
	}
	buf.WriteString("]")
	return buf.String()
}

type subItemWithName struct {
	// Type byte
	Name string
}

func encodeSubItemWithName(e *dicomio.Writer, itemType byte, name string) error {
	err := encodeSubItemHeader(e, itemType, uint16(len(name)))
	if err != nil {
		return err
	}
	// TODO: handle unicode properly
	return e.WriteBytes([]byte(name))
}

func DecodeSubItemWithName(d *dicomio.Reader, length uint16) (string, error) {
	return d.ReadString(uint32(length))
}

type SubItemUnsupported struct {
	Type byte
	Data []byte
}

func (item *SubItemUnsupported) Write(e *dicomio.Writer) {
	encodeSubItemHeader(e, item.Type, uint16(len(item.Data)))
	// TODO: handle unicode properly
	e.WriteBytes(item.Data)
}

func (item *SubItemUnsupported) String() string {
	return fmt.Sprintf("SubitemUnsupported{type: 0x%0x data: %dbytes}",
		item.Type, len(item.Data))
}
