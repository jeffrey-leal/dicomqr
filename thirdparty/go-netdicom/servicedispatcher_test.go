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

	cm := newContextManager("cm", nil)
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
	cm := newContextManager("cm2", nil)
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

	// A peer request carrying the same message ID as our outstanding local
	// command must NOT collide with it — the two ID spaces are independent
	// (PS3.7: Message ID is scoped to the initiating AE).
	cs2, found := disp.findOrCreatePeerCommand(cs1.messageID, cm, entry)
	if found {
		t.Error("peer command with a colliding message ID must be created fresh, not resolve to the local command")
	}
	if cs2 == cs1 {
		t.Error("peer command must be a distinct commandState")
	}
	if !cs2.peer {
		t.Error("peer command must be flagged peer=true")
	}

	// A second event for the same peer message ID finds the running command.
	cs3, found := disp.findOrCreatePeerCommand(cs1.messageID, cm, entry)
	if !found || cs3 != cs2 {
		t.Error("expected to find the existing peer command")
	}

	// Deleting each removes it from its own map only.
	disp.deleteCommand(cs2)
	disp.mu.Lock()
	_, localAlive := disp.activeCommands[cs1.messageID]
	_, peerAlive := disp.peerCommands[cs1.messageID]
	disp.mu.Unlock()
	if !localAlive {
		t.Error("deleting the peer command must not remove the local command")
	}
	if peerAlive {
		t.Error("peer command should be gone after deleteCommand")
	}
}

// Regression test for the C-GET wedge on large retrieves: with our C-GET
// command outstanding as message ID N, an incoming C-STORE-RQ sub-operation
// whose (peer) message ID is also N must be dispatched to the registered
// C-STORE callback — not "forwarded" to the C-GET command's response channel,
// which is what the shared-keyspace dispatcher did once a study's
// sub-operation count reached our ID range (IDs start at 124).
func TestServiceDispatcher_PeerRequestMessageIDCollision(t *testing.T) {
	disp := newServiceDispatcher("test3")
	cm := newContextManager("cm3", nil)
	qrEntry := &contextManagerEntry{contextID: 1, abstractSyntaxUID: "1.2.840.10008.5.1.4.1.2.2.3", transferSyntaxUID: "1.2.840.10008.1.2.1"}
	storeEntry := &contextManagerEntry{contextID: 3, abstractSyntaxUID: "1.2.840.10008.5.1.4.1.1.4", transferSyntaxUID: "1.2.840.10008.1.2.4.90"}
	cm.contextIDToAbstractSyntaxNameMap[1] = qrEntry
	cm.contextIDToAbstractSyntaxNameMap[3] = storeEntry
	cm.abstractSyntaxNameToContextIDMap[qrEntry.abstractSyntaxUID] = qrEntry
	cm.abstractSyntaxNameToContextIDMap[storeEntry.abstractSyntaxUID] = storeEntry

	// Our local command — plays the role of the outstanding C-GET.
	getCmd, err := disp.newCommand(cm, *qrEntry)
	if err != nil {
		t.Fatalf("newCommand failed: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var gotTS string
	storeRq := &dimse.CStoreRq{
		AffectedSOPClassUID:    storeEntry.abstractSyntaxUID,
		MessageID:              getCmd.messageID, // the collision
		AffectedSOPInstanceUID: "1.2.3.4",
		CommandDataSetType:     dimse.CommandDataSetTypeNull,
	}
	disp.registerCallback(storeRq.CommandField(), func(msg dimse.Message, data *dimse.DimseCommand, cs *serviceCommandState) {
		gotTS = cs.context.transferSyntaxUID
		wg.Done()
	})

	disp.handleEvent(upcallEvent{
		eventType: upcallEventData,
		cm:        cm,
		contextID: storeEntry.contextID,
		command:   storeRq,
	})
	wg.Wait()

	// The C-STORE callback ran, its command state carries the STORAGE
	// context (so the payload's transfer syntax is labelled correctly), and
	// nothing was forwarded to the C-GET command's channel.
	if gotTS != storeEntry.transferSyntaxUID {
		t.Errorf("callback context transfer syntax = %q, want %q", gotTS, storeEntry.transferSyntaxUID)
	}
	select {
	case ev := <-getCmd.upcallCh:
		t.Errorf("C-STORE request was mis-forwarded to the local command: %+v", ev.command)
	default:
	}

	// A response with the same message ID still reaches the local command.
	rsp := &dimse.CGetRsp{
		MessageIDBeingRespondedTo: getCmd.messageID,
		CommandDataSetType:        dimse.CommandDataSetTypeNull,
		Status:                    dimse.Status{Status: dimse.StatusSuccess},
	}
	disp.handleEvent(upcallEvent{
		eventType: upcallEventData,
		cm:        cm,
		contextID: qrEntry.contextID,
		command:   rsp,
	})
	select {
	case ev := <-getCmd.upcallCh:
		if _, ok := ev.command.(*dimse.CGetRsp); !ok {
			t.Errorf("expected CGetRsp on local command channel, got %+v", ev.command)
		}
	default:
		t.Error("response was not forwarded to the local command")
	}
}
