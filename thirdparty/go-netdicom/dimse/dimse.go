package dimse

//go:generate ./generate_dimse_messages.py

// Implements message types defined in P3.7.
//
// http://dicom.nema.org/medical/dicom/current/output/pdf/part07.pdf

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// Encode the given elements. The elements are sorted in ascending tag order.
func EncodeElements(e io.Writer, elems []*dicom.Element) error {
	writer, err := dicom.NewWriter(e)
	if err != nil {
		return fmt.Errorf("EncodeElements: failed to create writer: %w", err)
	}
	writer.SetTransferSyntax(binary.LittleEndian, true)
	sort.Slice(elems, func(i, j int) bool {
		return elems[i].Tag.Compare(elems[j].Tag) < 0
	})
	for _, elem := range elems {
		if err := writer.WriteElement(elem); err != nil {
			return fmt.Errorf("EncodeElements: error writing element %s: %w", elem.Tag.String(), err)
		}

	}
	return nil
}

func NewElement(tag tag.Tag, value any) (*dicom.Element, error) {
	switch v := value.(type) {
	case string:
		return dicom.NewElement(tag, []string{v})
	case []string:
		return dicom.NewElement(tag, v)
	case int:
		return dicom.NewElement(tag, []int{v})
	case int8:
		return dicom.NewElement(tag, []int{int(v)})
	case uint8:
		return dicom.NewElement(tag, []int{int(v)})
	case int16:
		return dicom.NewElement(tag, []int{int(v)})
	case uint16:
		return dicom.NewElement(tag, []int{int(v)})
	case int32:
		return dicom.NewElement(tag, []int{int(v)})
	case uint32:
		return dicom.NewElement(tag, []int{int(v)})
	case int64:
		return dicom.NewElement(tag, []int{int(v)})
	default:
		return nil, fmt.Errorf("NewElement: unsupported type %T for tag %s", value, tag)
	}
}
