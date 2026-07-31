package netdicom

import (
	"fmt"
	"strings"

	"github.com/algm/go-netdicom/dimse"
	"github.com/grailbio/go-dicom"
	"github.com/grailbio/go-dicom/dicomio"
	"github.com/grailbio/go-dicom/dicomlog"
	"github.com/grailbio/go-dicom/dicomtag"
	"github.com/grailbio/go-dicom/dicomuid"
)

// Helper function used by C-{STORE,GET,MOVE} to send a dataset using C-STORE
// over an already-established association.
func runCStoreOnAssociation(upcallCh chan upcallEvent, downcallCh chan stateEvent,
	cm *contextManager,
	messageID dimse.MessageID,
	ds *dicom.DataSet) error {
	var getElement = func(tag dicomtag.Tag) (string, error) {
		elem, err := ds.FindElementByTag(tag)
		if err != nil {
			return "", fmt.Errorf("dicom.cstore: data lacks %s: %v", tag.String(), err)
		}
		s, err := elem.GetString()
		if err != nil {
			return "", err
		}
		return s, nil
	}
	sopInstanceUID, err := getElement(dicomtag.MediaStorageSOPInstanceUID)
	if err != nil {
		return fmt.Errorf("dicom.cstore: data lacks SOPInstanceUID: %v", err)
	}
	sopClassUID, err := getElement(dicomtag.MediaStorageSOPClassUID)
	if err != nil {
		return fmt.Errorf("dicom.cstore: data lacks MediaStorageSOPClassUID: %v", err)
	}
	dicomlog.Vprintf(1, "dicom.cstore(%s): DICOM abstractsyntax: %s, sopinstance: %s", cm.label, dicomuid.UIDString(sopClassUID), sopInstanceUID)
	context, err := cm.lookupByAbstractSyntaxUID(sopClassUID)
	if err != nil {
		dicomlog.Vprintf(0, "dicom.cstore(%s): sop class %v not found in context %v", cm.label, sopClassUID, err)
		return err
	}
	dicomlog.Vprintf(1, "dicom.cstore(%s): using transfersyntax %s to send sop class %s, instance %s",
		cm.label,
		dicomuid.UIDString(context.transferSyntaxUID),
		dicomuid.UIDString(sopClassUID),
		sopInstanceUID)
	bodyEncoder := dicomio.NewBytesEncoderWithTransferSyntax(context.transferSyntaxUID)
	for _, elem := range ds.Elements {
		if elem.Tag.Group == dicomtag.MetadataGroup {
			continue
		}
		dicom.WriteElement(bodyEncoder, elem)
	}
	if err := bodyEncoder.Error(); err != nil {
		// Local patch: the encoder error can embed a full element dump —
		// grailbio includes Element.String() of the offending element,
		// sequence items and all, easily dozens of lines per failing file.
		// Log only the first line (which names the element and reason); the
		// caller still receives the complete error.
		msg := err.Error()
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i] + " …"
		}
		dicomlog.Vprintf(0, "dicom.cstore(%s): body encoder failed: %s", cm.label, msg)
		return err
	}
	return runCStoreRawOnAssociation(upcallCh, downcallCh, cm, messageID, sopClassUID, sopInstanceUID, bodyEncoder.Bytes())
}

// TransferSyntaxMismatchError is returned by ServiceUser.CStoreRaw when the
// dataset's transfer syntax differs from the one negotiated for its SOP
// class: pre-encoded bytes can only be sent verbatim. The caller may convert
// the dataset to Negotiated and retry.
type TransferSyntaxMismatchError struct {
	Negotiated string // transfer syntax UID the association negotiated
	File       string // transfer syntax UID the dataset is encoded in
}

func (e *TransferSyntaxMismatchError) Error() string {
	return fmt.Sprintf("dicom.cstore: dataset transfer syntax %s differs from negotiated %s", e.File, e.Negotiated)
}

// runCStoreRawOnAssociation sends an already-encoded dataset (the file's
// bytes after the meta group, in the association-negotiated transfer syntax)
// over an established association and waits for the C-STORE response. Local
// addition: sending stored bytes verbatim avoids the grailbio re-encode and
// its stale data dictionary entirely.
func runCStoreRawOnAssociation(upcallCh chan upcallEvent, downcallCh chan stateEvent,
	cm *contextManager,
	messageID dimse.MessageID,
	sopClassUID, sopInstanceUID string,
	data []byte) error {
	downcallCh <- stateEvent{
		event: evt09,
		dimsePayload: &stateEventDIMSEPayload{
			abstractSyntaxName: sopClassUID,
			command: &dimse.CStoreRq{
				AffectedSOPClassUID:    sopClassUID,
				MessageID:              messageID,
				CommandDataSetType:     dimse.CommandDataSetTypeNonNull,
				AffectedSOPInstanceUID: sopInstanceUID,
			},
			data: data,
		},
	}
	for {
		dicomlog.Vprintf(0, "dicom.cstore(%s): Start reading resp w/ messageID:%v", cm.label, messageID)
		event, ok := <-upcallCh
		if !ok {
			return fmt.Errorf("dicom.cstore(%s): Connection closed while waiting for C-STORE response", cm.label)
		}
		dicomlog.Vprintf(1, "dicom.cstore(%s): resp event: %v", cm.label, event.command)
		doassert(event.eventType == upcallEventData)
		doassert(event.command != nil)
		resp, ok := event.command.(*dimse.CStoreRsp)
		doassert(ok) // TODO(saito)
		if resp.Status.Status != 0 {
			return fmt.Errorf("dicom.cstore(%s): failed: %v", cm.label, resp.String())
		}
		return nil
	}
}
