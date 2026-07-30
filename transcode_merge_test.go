package main

import (
	"bytes"
	"testing"

	"github.com/suyashkumar/dicom/pkg/frame"
)

func encFrag(data []byte) *frame.Frame {
	return &frame.Frame{Encapsulated: true, EncapsulatedData: frame.EncapsulatedFrame{Data: data}}
}

// mergeEncapsulatedFragments must concatenate fragments in order, pass a
// single fragment through unchanged, and reject native or nil entries.
func TestMergeEncapsulatedFragments(t *testing.T) {
	merged, err := mergeEncapsulatedFragments([]*frame.Frame{encFrag([]byte{1, 2}), encFrag([]byte{3}), encFrag([]byte{4, 5})})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !merged.IsEncapsulated() || !bytes.Equal(merged.EncapsulatedData.Data, []byte{1, 2, 3, 4, 5}) {
		t.Fatalf("merged = %v, want encapsulated [1 2 3 4 5]", merged.EncapsulatedData.Data)
	}

	single, err := mergeEncapsulatedFragments([]*frame.Frame{encFrag([]byte{9, 8})})
	if err != nil {
		t.Fatalf("single fragment: %v", err)
	}
	if !bytes.Equal(single.EncapsulatedData.Data, []byte{9, 8}) {
		t.Fatalf("single = %v, want [9 8]", single.EncapsulatedData.Data)
	}

	if _, err := mergeEncapsulatedFragments([]*frame.Frame{encFrag([]byte{1}), nil}); err == nil || err.Error() != "mixed native and encapsulated fragments" {
		t.Errorf("nil entry error = %v, want 'mixed native and encapsulated fragments'", err)
	}
	if _, err := mergeEncapsulatedFragments([]*frame.Frame{{Encapsulated: false}}); err == nil || err.Error() != "mixed native and encapsulated fragments" {
		t.Errorf("native entry error = %v, want 'mixed native and encapsulated fragments'", err)
	}
}
