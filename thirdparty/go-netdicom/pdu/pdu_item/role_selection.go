package pdu_item

import (
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
)

// PS3.7 Annex D.3.3.4
type RoleSelectionSubItem struct {
	SOPClassUID string
	SCURole     byte
	SCPRole     byte
}

func decodeRoleSelectionSubItem(d *dicomio.Reader, length uint16) (*RoleSelectionSubItem, error) {
	uidLen, err := d.ReadUInt16()
	if err != nil {
		return nil, err
	}
	sopclassuid, err := d.ReadString(uint32(uidLen))
	if err != nil {
		return nil, err
	}
	scuRole, err := d.ReadUInt8()
	if err != nil {
		return nil, err
	}
	scpRole, err := d.ReadUInt8()
	if err != nil {
		return nil, err
	}
	return &RoleSelectionSubItem{
		SOPClassUID: sopclassuid,
		SCURole:     scuRole,
		SCPRole:     scpRole,
	}, nil
}

func (v *RoleSelectionSubItem) Write(e *dicomio.Writer) error {
	if err := encodeSubItemHeader(e, ItemTypeRoleSelection, uint16(2+len(v.SOPClassUID)+1*2)); err != nil {
		return err
	}
	if err := e.WriteUInt16(uint16(len(v.SOPClassUID))); err != nil {
		return err
	}
	if err := e.WriteString(v.SOPClassUID); err != nil {
		return err
	}
	if err := e.WriteByte(v.SCURole); err != nil {
		return err
	}
	return e.WriteByte(v.SCPRole)
}

func (v *RoleSelectionSubItem) String() string {
	return fmt.Sprintf("RoleSelection{sopclassuid: %v, scu: %v, scp: %v}", v.SOPClassUID, v.SCURole, v.SCPRole)
}
