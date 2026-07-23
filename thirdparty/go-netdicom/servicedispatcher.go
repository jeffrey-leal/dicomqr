package netdicom

import (
	"fmt"
	"sync"

	"github.com/algm/go-netdicom/dimse"
	"github.com/grailbio/go-dicom/dicomlog"
)

// serviceDispatcher multiplexes statemachine upcall events to DIMSE commands.
type serviceDispatcher struct {
	label      string          // for logging.
	downcallCh chan stateEvent // for sending PDUs to the statemachine.

	// closeOnce makes close() idempotent so that Release and Abort may both
	// run (in either order) without double-closing per-command channels
	// (dicomqr local patch).
	closeOnce sync.Once

	mu sync.Mutex

	// Set of locally-initiated DIMSE commands running (created by
	// newCommand). Keys are our message IDs; responses from the peer are
	// routed here by MessageIDBeingRespondedTo.
	activeCommands map[dimse.MessageID]*serviceCommandState // guarded by mu

	// Set of peer-initiated commands running (incoming requests, e.g.
	// C-STORE sub-operations arriving on a C-GET association). Keys are the
	// PEER's message IDs. Kept separate from activeCommands (dicomqr local
	// patch): PS3.7 scopes the Message ID to the initiating AE, so a peer
	// request may legally carry the same ID as one of our outstanding
	// commands. With a single shared map, the 124th C-STORE sub-operation of
	// a C-GET (peer IDs counting up from 1) collided with our C-GET command
	// (IDs counting up from 124) and was mis-forwarded to the C-GET response
	// loop instead of the C-STORE callback, wedging large retrieves.
	peerCommands map[dimse.MessageID]*serviceCommandState // guarded by mu

	// A callback to be called when a dimse request message arrives. Keys
	// are DIMSE CommandField. The callback typically creates a new command
	// by calling findOrCreateCommand.
	callbacks map[uint16]serviceCallback // guarded by mu

	// The last message ID used in newCommand(). Used to avoid creating duplicate
	// IDs.
	lastMessageID dimse.MessageID
}

type serviceCallback func(msg dimse.Message, data *dimse.DimseCommand, cs *serviceCommandState)

// Per-DIMSE-command state.
type serviceCommandState struct {
	disp      *serviceDispatcher  // Parent.
	messageID dimse.MessageID     // Command's MessageID.
	context   contextManagerEntry // Transfersyntax/sopclass for this command.
	cm        *contextManager     // For looking up context -> transfersyntax/sopclass mappings

	// upcallCh streams command+data for this messageID.
	upcallCh chan upcallEvent

	// streamingReader holds the DimseCommand when server decides to stream large datasets.
	streamingReader *dimse.DimseCommand

	// peer reports which map owns this command: peerCommands (true) or
	// activeCommands (false). Set once at creation.
	peer bool
}

// Send a command+data combo to the remote peer. data may be nil.
func (cs *serviceCommandState) sendMessage(cmd dimse.Message, data []byte) {
	if s := cmd.GetStatus(); s != nil && s.Status != dimse.StatusSuccess && s.Status != dimse.StatusPending {
		dicomlog.Vprintf(0, "dicom.serviceDispatcher(%s): Sending DIMSE error: %v %v", cs.disp.label, cmd, cs.disp)
	} else {
		dicomlog.Vprintf(1, "dicom.serviceDispatcher(%s): Sending DIMSE message: %v %v", cs.disp.label, cmd, cs.disp)
	}
	payload := &stateEventDIMSEPayload{
		abstractSyntaxName: cs.context.abstractSyntaxUID,
		command:            cmd,
		data:               data,
	}
	cs.disp.downcallCh <- stateEvent{
		event:        evt09,
		pdu:          nil,
		conn:         nil,
		dimsePayload: payload,
	}
}

// findOrCreatePeerCommand returns the running peer-initiated command with the
// given peer message ID, or creates one. Peer commands live in their own map
// so they can never collide with locally-initiated commands that happen to
// share a message ID (dicomqr local patch).
func (disp *serviceDispatcher) findOrCreatePeerCommand(
	msgID dimse.MessageID,
	cm *contextManager,
	context contextManagerEntry) (*serviceCommandState, bool) {
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if cs, ok := disp.peerCommands[msgID]; ok {
		return cs, true
	}
	cs := &serviceCommandState{
		disp:      disp,
		messageID: msgID,
		cm:        cm,
		context:   context,
		upcallCh:  make(chan upcallEvent, 128),
		peer:      true,
	}
	disp.peerCommands[msgID] = cs
	dicomlog.Vprintf(1, "dicom.serviceDispatcher(%s): Start peer command %+v", disp.label, cs)
	return cs, false
}

// Create a new serviceCommandState with an unused message ID.  Returns an error
// if it fails to allocate a message ID.
func (disp *serviceDispatcher) newCommand(
	cm *contextManager, context contextManagerEntry) (*serviceCommandState, error) {
	disp.mu.Lock()
	defer disp.mu.Unlock()

	for msgID := disp.lastMessageID + 1; msgID != disp.lastMessageID; msgID++ {
		if _, ok := disp.activeCommands[msgID]; ok {
			continue
		}

		cs := &serviceCommandState{
			disp:      disp,
			messageID: msgID,
			cm:        cm,
			context:   context,
			upcallCh:  make(chan upcallEvent, 128),
		}
		disp.activeCommands[msgID] = cs
		disp.lastMessageID = msgID
		dicomlog.Vprintf(1, "dicom.serviceDispatcher: Start new command %+v", cs)
		return cs, nil
	}
	return nil, fmt.Errorf("Failed to allocate a message ID (too many outstading?)")
}

func (disp *serviceDispatcher) deleteCommand(cs *serviceCommandState) {
	disp.mu.Lock()
	dicomlog.Vprintf(1, "dicom.serviceDispatcher(%s): Finish command %v (peer=%v)", disp.label, cs.messageID, cs.peer)
	commands := disp.activeCommands
	if cs.peer {
		commands = disp.peerCommands
	}
	if _, ok := commands[cs.messageID]; !ok {
		panic(fmt.Sprintf("cs %+v", cs))
	}
	delete(commands, cs.messageID)
	disp.mu.Unlock()
	if cs.streamingReader != nil {
		cs.streamingReader.Ack()
	}
}

func (disp *serviceDispatcher) registerCallback(commandField uint16, cb serviceCallback) {
	disp.mu.Lock()
	disp.callbacks[commandField] = cb
	disp.mu.Unlock()
}

func (disp *serviceDispatcher) unregisterCallback(commandField uint16) {
	disp.mu.Lock()
	delete(disp.callbacks, commandField)
	disp.mu.Unlock()
}

func (disp *serviceDispatcher) handleEvent(event upcallEvent) {
	if event.eventType == upcallEventHandshakeCompleted {
		return
	}
	doassert(event.eventType == upcallEventData)
	doassert(event.command != nil)
	context, err := event.cm.lookupByContextID(event.contextID)
	if err != nil {
		dicomlog.Vprintf(0, "dicom.serviceDispatcher(%s): Invalid context ID %d: %v", disp.label, event.contextID, err)
		disp.downcallCh <- stateEvent{event: evt19, pdu: nil, err: err}
		return
	}
	messageID := event.command.GetMessageID()

	// Responses (command field bit 0x8000, PS3.7 E.1) answer one of OUR
	// commands: route by MessageIDBeingRespondedTo into activeCommands only.
	// Peer-initiated requests are routed into the separate peerCommands map —
	// the two message-ID spaces are independent (dicomqr local patch).
	if event.command.CommandField()&0x8000 != 0 {
		disp.mu.Lock()
		dc, ok := disp.activeCommands[messageID]
		disp.mu.Unlock()
		if !ok {
			dicomlog.Vprintf(0, "dicom.serviceDispatcher(%s): response for unknown command %v; dropping: %+v", disp.label, messageID, event.command)
			if event.data != nil {
				_ = event.data.Ack()
			}
			return
		}
		dicomlog.Vprintf(1, "dicom.serviceDispatcher(%s): Forwarding command to existing command: %+v %+v", disp.label, event.command, dc)
		// A watchdog-triggered Abort may close upcallCh concurrently with this
		// send (dicomqr local patch). Treat that as connection shutdown and
		// drop the event instead of crashing the process.
		func() {
			defer func() {
				if recover() != nil {
					dicomlog.Vprintf(0, "dicom.serviceDispatcher(%s): dropped event for aborted command %v", disp.label, messageID)
				}
			}()
			dc.upcallCh <- event
		}()
		dicomlog.Vprintf(1, "dicom.serviceDispatcher(%s): Done forwarding command to existing command: %+v %+v", disp.label, event.command, dc)
		return
	}

	dc, found := disp.findOrCreatePeerCommand(messageID, event.cm, context)
	if found {
		// Continuation of a peer command already being handled (should not
		// happen for the request types we support, but preserve the old
		// forward-to-existing behaviour).
		dicomlog.Vprintf(1, "dicom.serviceDispatcher(%s): Forwarding request to existing peer command: %+v %+v", disp.label, event.command, dc)
		func() {
			defer func() {
				if recover() != nil {
					dicomlog.Vprintf(0, "dicom.serviceDispatcher(%s): dropped event for aborted command %v", disp.label, messageID)
				}
			}()
			dc.upcallCh <- event
		}()
		return
	}
	disp.mu.Lock()
	cb := disp.callbacks[event.command.CommandField()]
	disp.mu.Unlock()
	if cb == nil {
		// No handler registered for this request type. Previously this called
		// a nil function in a fresh goroutine and crashed the process.
		dicomlog.Vprintf(0, "dicom.serviceDispatcher(%s): no callback for command field 0x%04x; dropping: %+v", disp.label, event.command.CommandField(), event.command)
		disp.deleteCommand(dc)
		if event.data != nil {
			_ = event.data.Ack()
		}
		return
	}
	go func() {
		// Attach streaming reader to command state for handlers needing io.Reader
		dc.streamingReader = event.data
		cb(event.command, event.data, dc)
		disp.deleteCommand(dc)
	}()
}

// Shuts down the dispatcher, closing every active command's upcall channel so
// blocked DIMSE calls return. Idempotent (dicomqr local patch): Release and a
// watchdog-triggered Abort may race without double-closing channels.
func (disp *serviceDispatcher) close() {
	disp.closeOnce.Do(func() {
		disp.mu.Lock()
		for _, cs := range disp.activeCommands {
			close(cs.upcallCh)
		}
		for _, cs := range disp.peerCommands {
			close(cs.upcallCh)
		}
		disp.mu.Unlock()
	})
	// TODO(saito): prevent new command from launching.
}

func newServiceDispatcher(label string) *serviceDispatcher {
	return &serviceDispatcher{
		label:          label,
		downcallCh:     make(chan stateEvent, 128),
		activeCommands: make(map[dimse.MessageID]*serviceCommandState),
		peerCommands:   make(map[dimse.MessageID]*serviceCommandState),
		callbacks:      make(map[uint16]serviceCallback),
		lastMessageID:  123,
	}
}
