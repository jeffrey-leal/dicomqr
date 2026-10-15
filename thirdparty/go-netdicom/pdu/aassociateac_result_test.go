package pdu

import (
	"bytes"
	"testing"

	"github.com/algm/go-netdicom/pdu/pdu_item"
)

// An A-ASSOCIATE-AC must carry each presentation context's result (dicomqr
// local patch): the encoder used to write it as zero, so a requestor read every
// context — rejected ones included — as accepted.
func TestAAssociateACCarriesPresentationContextResults(t *testing.T) {
	ac := &AAssociateAC{
		ProtocolVersion: CurrentProtocolVersion,
		CalledAETitle:   "SCP",
		CallingAETitle:  "SCU",
		Items: []pdu_item.SubItem{
			&pdu_item.ApplicationContextItem{Name: pdu_item.DICOMApplicationContextItemName},
			&pdu_item.PresentationContextItem{
				Type: pdu_item.ItemTypePresentationContextResponse, ContextID: 1,
				Result: pdu_item.PresentationContextProviderRejectionTransferSyntaxNotSupported,
				Items:  []pdu_item.SubItem{&pdu_item.TransferSyntaxSubItem{Name: "1.2.840.10008.1.2.4.90"}},
			},
			&pdu_item.PresentationContextItem{
				Type: pdu_item.ItemTypePresentationContextResponse, ContextID: 3,
				Result: pdu_item.PresentationContextAccepted,
				Items:  []pdu_item.SubItem{&pdu_item.TransferSyntaxSubItem{Name: "1.2.840.10008.1.2.1"}},
			},
		},
	}
	encoded, err := EncodePDU(ac)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReadPDU(bytes.NewReader(encoded), 1<<20)
	if err != nil {
		t.Fatalf("ReadPDU: %v", err)
	}
	var results []pdu_item.PresentationContextResult
	for _, item := range back.(*AAssociateAC).Items {
		if pc, ok := item.(*pdu_item.PresentationContextItem); ok {
			results = append(results, pc.Result)
		}
	}
	want := []pdu_item.PresentationContextResult{
		pdu_item.PresentationContextProviderRejectionTransferSyntaxNotSupported,
		pdu_item.PresentationContextAccepted,
	}
	if len(results) != len(want) || results[0] != want[0] || results[1] != want[1] {
		t.Fatalf("results read back as %v, want %v", results, want)
	}
}
