package netdicom

import (
	"os"
	"sync"
	"testing"

	"github.com/algm/go-netdicom/dimse"
)

// Dummy callback to capture invocation parameters
func TestServiceDispatcher_HandleEvent(t *testing.T) {
	disp := newServiceDispatcher("test")

	cm := newContextManager("cm")
	// Prepare context mapping for contextID 1
	entry := &contextManagerEntry{
		contextID:         1,
		abstractSyntaxUID: "1.2.3",
		transferSyntaxUID: "1.2.840.10008.1.2",
	}
	cm.contextIDToAbstractSyntaxNameMap[1] = entry
	cm.abstractSyntaxNameToContextIDMap[entry.abstractSyntaxUID] = entry

	// Build a simple command (CEchoRq has no data)
	cmd := &dimse.CEchoRq{
		MessageID:          5,
		CommandDataSetType: dimse.CommandDataSetTypeNull,
	}

	var (
		wg            sync.WaitGroup
		capturedMsg   dimse.Message
		capturedData  *dimse.DimseCommand
		capturedState *serviceCommandState
	)
	wg.Add(1)

	disp.registerCallback(cmd.CommandField(), func(msg dimse.Message, data *dimse.DimseCommand, cs *serviceCommandState) {
		capturedMsg = msg
		capturedData = data
		capturedState = cs
		wg.Done()
	})

	// Send upcall event
	tmpFile, _ := os.CreateTemp("", "testpayload*")
	tmpFile.Write([]byte("payload"))
	tmpFile.Close()
	dcPayload := dimse.NewDimseCommand(tmpFile.Name())

	evt := upcallEvent{
		eventType: upcallEventData,
		cm:        cm,
		contextID: 1,
		command:   cmd,
		data:      dcPayload,
	}
	disp.handleEvent(evt)

	wg.Wait()

	if capturedMsg == nil {
		t.Fatal("callback not invoked")
	}
	if capturedMsg.GetMessageID() != cmd.GetMessageID() {
		t.Errorf("expected messageID %d got %d", cmd.GetMessageID(), capturedMsg.GetMessageID())
	}
	if capturedData == nil {
		t.Errorf("expected non-nil DimseCommand")
	}
	if capturedState == nil {
		t.Error("expected serviceCommandState non-nil")
	}
}

func TestServiceDispatcher_NewCommandAndFind(t *testing.T) {
	disp := newServiceDispatcher("test2")
	cm := newContextManager("cm2")
	entry := contextManagerEntry{contextID: 3, abstractSyntaxUID: "1", transferSyntaxUID: "ts"}
	cm.contextIDToAbstractSyntaxNameMap[3] = &entry
	cm.abstractSyntaxNameToContextIDMap["1"] = &entry

	cs1, err := disp.newCommand(cm, entry)
	if err != nil {
		t.Fatalf("newCommand failed: %v", err)
	}
	if cs1.messageID == 0 {
		t.Error("expected non-zero messageID")
	}

	cs2, found := disp.findOrCreateCommand(cs1.messageID, cm, entry)
	if !found {
		t.Error("expected to find existing command")
	}
	if cs2 != cs1 {
		t.Error("expected same commandState returned")
	}
}
