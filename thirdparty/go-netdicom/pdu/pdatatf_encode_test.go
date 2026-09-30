package pdu

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The direct P-DATA-TF encoder (dicomqr local patch) must produce exactly the
// bytes of the generic path — Write, with EncodePDU's six-byte header put in
// front — and read back through ReadPDU to the same items.
func TestPDataTfEncodeMatchesGenericPath(t *testing.T) {
	pdu := &PDataTf{Items: []PresentationDataValueItem{
		{ContextID: 1, Command: true, Last: true, Value: []byte("command bytes")},
		{ContextID: 3, Command: false, Last: false, Value: bytes.Repeat([]byte{0xAB}, 70000)},
		{ContextID: 3, Command: false, Last: true, Value: nil},
	}}
	payload, err := pdu.Write()
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 6, 6+len(payload))
	want[0] = byte(TypePDataTf)
	binary.BigEndian.PutUint32(want[2:6], uint32(len(payload)))
	want = append(want, payload...)

	got, err := EncodePDU(pdu)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("direct encoding differs from the generic path (%d vs %d bytes)", len(got), len(want))
	}

	back, err := ReadPDU(bytes.NewReader(got), 1<<20)
	if err != nil {
		t.Fatalf("ReadPDU: %v", err)
	}
	items := back.(*PDataTf).Items
	if len(items) != len(pdu.Items) {
		t.Fatalf("read back %d items, want %d", len(items), len(pdu.Items))
	}
	for i := range items {
		a, b := items[i], pdu.Items[i]
		if a.ContextID != b.ContextID || a.Command != b.Command || a.Last != b.Last || !bytes.Equal(a.Value, b.Value) {
			t.Errorf("item %d read back as %v, want %v", i, a.String(), b.String())
		}
	}
}
