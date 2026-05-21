// This file defines ServiceProvider (i.e., a DICOM server).

package netdicom

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"

	"github.com/algm/go-netdicom/commandset"
	"github.com/algm/go-netdicom/dimse"
	"github.com/algm/go-netdicom/sopclass"
	dicom "github.com/grailbio/go-dicom"
	"github.com/grailbio/go-dicom/dicomio"
	"github.com/grailbio/go-dicom/dicomlog"
)

// CMoveResult is an object streamed by CMove implementation.
type CMoveResult struct {
	Remaining int // Number of files remaining to be sent. Set -1 if unknown.
	Err       error
	Path      string         // Path name of the DICOM file being copied. Used only for reporting errors.
	DataSet   *dicom.DataSet // Contents of the file.
}

func handleCStore(
	ctx context.Context,
	params ServiceProviderParams,
	connState ConnectionState,
	c *dimse.CStoreRq, data *dimse.DimseCommand,
	cs *serviceCommandState) {
	status := dimse.Status{Status: dimse.StatusUnrecognizedOperation}

	if params.CStore != nil {
		// Determine data reader and size directly from DimseCommand
		var (
			dataReader io.Reader
			dataSize   int64
		)

		if data != nil {
			dataReader = data
			dataSize = data.Size()
		}

		status = params.CStore(
			ctx,
			connState,
			cs.context.transferSyntaxUID,
			c.AffectedSOPClassUID,
			c.AffectedSOPInstanceUID,
			dataReader,
			dataSize)
	}

	resp := &dimse.CStoreRsp{
		AffectedSOPClassUID:       c.AffectedSOPClassUID,
		MessageIDBeingRespondedTo: c.MessageID,
		CommandDataSetType:        dimse.CommandDataSetTypeNull,
		AffectedSOPInstanceUID:    c.AffectedSOPInstanceUID,
		Status:                    status,
	}
	cs.sendMessage(resp, nil)

	// Clean up temporary file associated with DimseCommand
	if data != nil {
		_ = data.Ack()
	}
}

func handleCFind(
	params ServiceProviderParams,
	connState ConnectionState,
	c *dimse.CFindRq, data *dimse.DimseCommand,
	cs *serviceCommandState) {
	if params.CFind == nil {
		cs.sendMessage(&dimse.CFindRsp{
			AffectedSOPClassUID:       c.AffectedSOPClassUID,
			MessageIDBeingRespondedTo: c.MessageID,
			CommandDataSetType:        dimse.CommandDataSetTypeNull,
			Status:                    dimse.Status{Status: dimse.StatusUnrecognizedOperation, ErrorComment: "No callback found for C-FIND"},
		}, nil)
		return
	}
	var payload []byte
	if data != nil {
		payload, _ = io.ReadAll(data)
	}
	elems, err := readElementsInBytes(payload, cs.context.transferSyntaxUID)
	if err != nil {
		cs.sendMessage(&dimse.CFindRsp{
			AffectedSOPClassUID:       c.AffectedSOPClassUID,
			MessageIDBeingRespondedTo: c.MessageID,
			CommandDataSetType:        dimse.CommandDataSetTypeNull,
			Status:                    dimse.Status{Status: dimse.StatusUnrecognizedOperation, ErrorComment: err.Error()},
		}, nil)
		return
	}
	dicomlog.Vprintf(1, "dicom.serviceProvider: C-FIND-RQ payload: %s", elementsString(elems))

	status := dimse.Status{Status: dimse.StatusSuccess}
	responseCh := make(chan CFindResult, 128)
	go func() {
		params.CFind(connState, cs.context.transferSyntaxUID, c.AffectedSOPClassUID, elems, responseCh)
	}()
	for resp := range responseCh {
		if resp.Err != nil {
			status = dimse.Status{
				Status:       dimse.CFindUnableToProcess,
				ErrorComment: resp.Err.Error(),
			}
			break
		}
		dicomlog.Vprintf(1, "dicom.serviceProvider: C-FIND-RSP: %s", elementsString(resp.Elements))
		payload, err := writeElementsToBytes(resp.Elements, cs.context.transferSyntaxUID)
		if err != nil {
			dicomlog.Vprintf(0, "dicom.serviceProvider: C-FIND: encode error %v", err)
			status = dimse.Status{
				Status:       dimse.CFindUnableToProcess,
				ErrorComment: err.Error(),
			}
			break
		}
		cs.sendMessage(&dimse.CFindRsp{
			AffectedSOPClassUID:       c.AffectedSOPClassUID,
			MessageIDBeingRespondedTo: c.MessageID,
			CommandDataSetType:        dimse.CommandDataSetTypeNonNull,
			Status:                    dimse.Status{Status: dimse.StatusPending},
		}, payload)
	}
	cs.sendMessage(&dimse.CFindRsp{
		AffectedSOPClassUID:       c.AffectedSOPClassUID,
		MessageIDBeingRespondedTo: c.MessageID,
		CommandDataSetType:        dimse.CommandDataSetTypeNull,
		Status:                    status}, nil)
	// Drain the responses in case of errors
	for range responseCh {
	}

	if data != nil {
		_ = data.Ack()
	}
}

func handleCMove(
	params ServiceProviderParams,
	connState ConnectionState,
	c *dimse.CMoveRq, data *dimse.DimseCommand,
	cs *serviceCommandState) {
	sendError := func(err error) {
		cs.sendMessage(&dimse.CMoveRsp{
			AffectedSOPClassUID:       c.AffectedSOPClassUID,
			MessageIDBeingRespondedTo: c.MessageID,
			CommandDataSetType:        dimse.CommandDataSetTypeNull,
			Status:                    dimse.Status{Status: dimse.StatusUnrecognizedOperation, ErrorComment: err.Error()},
		}, nil)
	}
	if params.CMove == nil {
		cs.sendMessage(&dimse.CMoveRsp{
			AffectedSOPClassUID:       c.AffectedSOPClassUID,
			MessageIDBeingRespondedTo: c.MessageID,
			CommandDataSetType:        dimse.CommandDataSetTypeNull,
			Status:                    dimse.Status{Status: dimse.StatusUnrecognizedOperation, ErrorComment: "No callback found for C-MOVE"},
		}, nil)
		return
	}
	remoteHostPort, ok := params.RemoteAEs[c.MoveDestination]
	if !ok {
		sendError(fmt.Errorf("C-MOVE destination '%v' not registered in the server", c.MoveDestination))
		return
	}
	var payload []byte
	if data != nil {
		payload, _ = io.ReadAll(data)
	}
	elems, err := readElementsInBytes(payload, cs.context.transferSyntaxUID)
	if err != nil {
		sendError(err)
		return
	}
	dicomlog.Vprintf(1, "dicom.serviceProvider: C-MOVE-RQ payload: %s", elementsString(elems))
	responseCh := make(chan CMoveResult, 128)
	go func() {
		params.CMove(connState, cs.context.transferSyntaxUID, c.AffectedSOPClassUID, elems, responseCh)
	}()
	// responseCh :=
	status := dimse.Status{Status: dimse.StatusSuccess}
	var numSuccesses, numFailures uint16
	for resp := range responseCh {
		if resp.Err != nil {
			status = dimse.Status{
				Status:       dimse.CFindUnableToProcess,
				ErrorComment: resp.Err.Error(),
			}
			break
		}
		dicomlog.Vprintf(0, "dicom.serviceProvider: C-MOVE: Sending %v to %v(%s)", resp.Path, c.MoveDestination, remoteHostPort)
		err := runCStoreOnNewAssociation(params.AETitle, c.MoveDestination, remoteHostPort, resp.DataSet)
		if err != nil {
			dicomlog.Vprintf(0, "dicom.serviceProvider: C-MOVE: C-store of %v to %v(%v) failed: %v", resp.Path, c.MoveDestination, remoteHostPort, err)
			numFailures++
		} else {
			numSuccesses++
		}
		cs.sendMessage(&dimse.CMoveRsp{
			AffectedSOPClassUID:            c.AffectedSOPClassUID,
			MessageIDBeingRespondedTo:      c.MessageID,
			CommandDataSetType:             dimse.CommandDataSetTypeNull,
			NumberOfRemainingSuboperations: uint16(resp.Remaining),
			NumberOfCompletedSuboperations: numSuccesses,
			NumberOfFailedSuboperations:    numFailures,
			Status:                         dimse.Status{Status: dimse.StatusPending},
		}, nil)
	}
	cs.sendMessage(&dimse.CMoveRsp{
		AffectedSOPClassUID:            c.AffectedSOPClassUID,
		MessageIDBeingRespondedTo:      c.MessageID,
		CommandDataSetType:             dimse.CommandDataSetTypeNull,
		NumberOfCompletedSuboperations: numSuccesses,
		NumberOfFailedSuboperations:    numFailures,
		Status:                         status}, nil)
	// Drain the responses in case of errors
	for range responseCh {
	}

	if data != nil {
		_ = data.Ack()
	}
}

func handleCGet(
	params ServiceProviderParams,
	connState ConnectionState,
	c *dimse.CGetRq, data *dimse.DimseCommand, cs *serviceCommandState) {
	sendError := func(err error) {
		cs.sendMessage(&dimse.CGetRsp{
			AffectedSOPClassUID:       c.AffectedSOPClassUID,
			MessageIDBeingRespondedTo: c.MessageID,
			CommandDataSetType:        dimse.CommandDataSetTypeNull,
			Status:                    dimse.Status{Status: dimse.StatusUnrecognizedOperation, ErrorComment: err.Error()},
		}, nil)
	}
	if params.CGet == nil {
		cs.sendMessage(&dimse.CGetRsp{
			AffectedSOPClassUID:       c.AffectedSOPClassUID,
			MessageIDBeingRespondedTo: c.MessageID,
			CommandDataSetType:        dimse.CommandDataSetTypeNull,
			Status:                    dimse.Status{Status: dimse.StatusUnrecognizedOperation, ErrorComment: "No callback found for C-GET"},
		}, nil)
		return
	}
	var payload []byte
	if data != nil {
		payload, _ = io.ReadAll(data)
	}
	elems, err := readElementsInBytes(payload, cs.context.transferSyntaxUID)
	if err != nil {
		sendError(err)
		return
	}
	dicomlog.Vprintf(1, "dicom.serviceProvider: C-GET-RQ payload: %s", elementsString(elems))
	responseCh := make(chan CMoveResult, 128)
	go func() {
		params.CGet(connState, cs.context.transferSyntaxUID, c.AffectedSOPClassUID, elems, responseCh)
	}()
	status := dimse.Status{Status: dimse.StatusSuccess}
	var numSuccesses, numFailures uint16
	for resp := range responseCh {
		if resp.Err != nil {
			status = dimse.Status{
				Status:       dimse.CFindUnableToProcess,
				ErrorComment: resp.Err.Error(),
			}
			break
		}
		subCs, err := cs.disp.newCommand(cs.cm, cs.context /*not used*/)
		if err != nil {
			status = dimse.Status{
				Status:       dimse.CFindUnableToProcess,
				ErrorComment: err.Error(),
			}
			break
		}
		err = runCStoreOnAssociation(subCs.upcallCh, subCs.disp.downcallCh, subCs.cm, subCs.messageID, resp.DataSet)
		if err != nil {
			dicomlog.Vprintf(0, "dicom.serviceProvider: C-GET: C-store of %v failed: %v", resp.Path, err)
			numFailures++
		} else {
			dicomlog.Vprintf(0, "dicom.serviceProvider: C-GET: Sent %v", resp.Path)
			numSuccesses++
		}
		cs.sendMessage(&dimse.CGetRsp{
			AffectedSOPClassUID:            c.AffectedSOPClassUID,
			MessageIDBeingRespondedTo:      c.MessageID,
			CommandDataSetType:             dimse.CommandDataSetTypeNull,
			NumberOfRemainingSuboperations: uint16(resp.Remaining),
			NumberOfCompletedSuboperations: numSuccesses,
			NumberOfFailedSuboperations:    numFailures,
			Status:                         dimse.Status{Status: dimse.StatusPending},
		}, nil)
		cs.disp.deleteCommand(subCs)
	}
	cs.sendMessage(&dimse.CGetRsp{
		AffectedSOPClassUID:            c.AffectedSOPClassUID,
		MessageIDBeingRespondedTo:      c.MessageID,
		CommandDataSetType:             dimse.CommandDataSetTypeNull,
		NumberOfCompletedSuboperations: numSuccesses,
		NumberOfFailedSuboperations:    numFailures,
		Status:                         status}, nil)
	// Drain the responses in case of errors
	for range responseCh {
	}

	if data != nil {
		_ = data.Ack()
	}
}

func handleCEcho(
	params ServiceProviderParams,
	connState ConnectionState,
	c *dimse.CEchoRq, data *dimse.DimseCommand,
	cs *serviceCommandState) {
	status := dimse.Status{Status: dimse.StatusUnrecognizedOperation}
	if params.CEcho != nil {
		status = params.CEcho(connState)
	}
	dicomlog.Vprintf(0, "dicom.serviceProvider: Received E-ECHO: context: %+v, status: %+v", cs.context, status)
	resp := &dimse.CEchoRsp{
		AffectedSOPClassUID:       c.AffectedSOPClassUID,
		MessageIDBeingRespondedTo: c.MessageID,
		CommandDataSetType:        dimse.CommandDataSetTypeNull,
		Status:                    status,
	}
	cs.sendMessage(resp, nil)

	if data != nil {
		_ = data.Ack()
	}
}

// ServiceProviderParams defines parameters for ServiceProvider.
type ServiceProviderParams struct {
	// The application-entity title of the server. Must be nonempty
	AETitle string

	// Names of remote AEs and their host:ports. Used only by C-MOVE. This
	// map should be nonempty iff the server supports CMove.
	RemoteAEs map[string]string

	// Called on C_ECHO request. If nil, a C-ECHO call will produce an error response.
	//
	// TODO(saito) Support a default C-ECHO callback?
	CEcho CEchoCallback

	// Called on C_FIND request.
	// If CFindCallback=nil, a C-FIND call will produce an error response.
	CFind CFindCallback

	// CMove is called on C_MOVE request.
	CMove CMoveCallback

	// CGet is called on C_GET request. The only difference between cmove
	// and cget is that cget uses the same connection to send images back to
	// the requester. Generally you shuold set the same function to CMove
	// and CGet.
	CGet CMoveCallback

	// If CStoreCallback=nil, a C-STORE call will produce an error response.
	// The callback always receives data as io.Reader for memory efficiency.
	CStore CStoreCallback

	// StreamingThreshold specifies the size (in bytes) above which true streaming mode
	// is enabled. Files smaller than this threshold will be buffered in memory for
	// better performance. Files larger will stream directly from network. Default: 100MB.
	StreamingThreshold int64

	// TLSConfig, if non-nil, enables TLS on the connection. See
	// https://gist.github.com/michaljemala/d6f4e01c4834bf47a9c4 for an
	// example for creating a TLS config from x509 cert files.
	TLSConfig *tls.Config

	Verbose bool
}

// DefaultMaxPDUSize is the the PDU size advertized by go-netdicom.
const DefaultMaxPDUSize = 4 << 20

// CStoreCallback is called C-STORE request.  sopInstanceUID is the UID of the
// data.  sopClassUID is the data type requested
// (e.g.,"1.2.840.10008.5.1.4.1.1.1.2"), and transferSyntaxUID is the encoding
// of the data (e.g., "1.2.840.10008.1.2.1").  These args are extracted from the
// request packet.
//
// "dataReader" provides access to the payload as a stream, i.e., a sequence of
// serialized dicom.DataElement objects in transferSyntaxUID. "dataSize" indicates
// the total size of the data stream in bytes. The data does not contain metadata
// elements (elements whose Tag.Group=2 -- e.g., TransferSyntaxUID and
// MediaStorageSOPClassUID), since they are stripped by the requester (two key
// metadata are passed as sop{Class,Instance)UID).
//
// The function should store encode the sop{Class,InstanceUID} as the DICOM
// header, followed by data. It should return either dimse.Success0 on success,
// or one of CStoreStatus* error codes on errors.
// CStoreCallback is called on C-STORE request. All data is provided as a stream
// via io.Reader for memory efficiency. For small files, the reader will be backed
// by a bytes.Reader. For large files, it streams directly from network PDUs.
type CStoreCallback func(
	ctx context.Context,
	conn ConnectionState,
	transferSyntaxUID string,
	sopClassUID string,
	sopInstanceUID string,
	dataReader io.Reader,
	dataSize int64) dimse.Status

// CFindCallback implements a C-FIND handler.  sopClassUID is the data type
// requested (e.g.,"1.2.840.10008.5.1.4.1.1.1.2"), and transferSyntaxUID is the
// data encoding requested (e.g., "1.2.840.10008.1.2.1").  These args are
// extracted from the request packet.
//
// This function should stream CFindResult objects through "ch". The function
// may block.  To report a matched DICOM dataset, the function should send one
// CFindResult with a nonempty Element field. To report multiple DICOM-dataset
// matches, the callback should send multiple CFindResult objects, one for each
// dataset.  The callback must close the channel after it produces all the
// responses.
type CFindCallback func(
	conn ConnectionState,
	transferSyntaxUID string,
	sopClassUID string,
	filters []*dicom.Element,
	ch chan CFindResult)

// CMoveCallback implements C-MOVE or C-GET handler.  sopClassUID is the data
// type requested (e.g.,"1.2.840.10008.5.1.4.1.1.1.2"), and transferSyntaxUID is
// the data encoding requested (e.g., "1.2.840.10008.1.2.1").  These args are
// extracted from the request packet.
//
// The callback must stream datasets or error to "ch". The callback may
// block. The callback must close the channel after it produces all the
// datasets.
type CMoveCallback func(
	conn ConnectionState,
	transferSyntaxUID string,
	sopClassUID string,
	filters []*dicom.Element,
	ch chan CMoveResult)

// ConnectionState informs session state to callbacks.
type ConnectionState struct {
	// TLS connection state. It is nonempty only when the connection is set up
	// over TLS.
	TLS tls.ConnectionState
}

// CEchoCallback implements C-ECHO callback. It typically just returns
// dimse.Success.
type CEchoCallback func(conn ConnectionState) dimse.Status

// ServiceProvider encapsulates the state for DICOM server (provider).
type ServiceProvider struct {
	params   ServiceProviderParams
	listener net.Listener
	// Label is a unique string used in log messages to identify this provider.
	label string
}

func writeElementsToBytes(elems []*dicom.Element, transferSyntaxUID string) ([]byte, error) {
	dataEncoder := dicomio.NewBytesEncoderWithTransferSyntax(transferSyntaxUID)
	for _, elem := range elems {
		dicom.WriteElement(dataEncoder, elem)
	}
	if err := dataEncoder.Error(); err != nil {
		return nil, err
	}
	return dataEncoder.Bytes(), nil
}

func readElementsInBytes(data []byte, transferSyntaxUID string) ([]*dicom.Element, error) {
	decoder := dicomio.NewBytesDecoderWithTransferSyntax(data, transferSyntaxUID)
	var elems []*dicom.Element
	for !decoder.EOF() {
		elem := dicom.ReadElement(decoder, dicom.ReadOptions{})
		dicomlog.Vprintf(1, "dicom.serviceProvider: C-FIND: Read elem: %v, err %v", elem, decoder.Error())
		if decoder.Error() != nil {
			break
		}
		elems = append(elems, elem)
	}
	if decoder.Error() != nil {
		return nil, decoder.Error()
	}
	return elems, nil
}

func elementsString(elems []*dicom.Element) string {
	s := "["
	for i, elem := range elems {
		if i > 0 {
			s += ", "
		}
		s += elem.String()
	}
	return s + "]"
}

// Send "ds" to remoteHostPort using C-STORE. Called as part of C-MOVE.
func runCStoreOnNewAssociation(myAETitle, remoteAETitle, remoteHostPort string, ds *dicom.DataSet) error {
	su, err := NewServiceUser(ServiceUserParams{
		CalledAETitle:  remoteAETitle,
		CallingAETitle: myAETitle,
		SOPClasses:     sopclass.StorageClasses})
	if err != nil {
		return err
	}
	defer su.Release()
	su.Connect(remoteHostPort)
	err = su.CStore(ds)
	dicomlog.Vprintf(1, "dicom.serviceProvider: C-STORE subop done: %v", err)
	return err
}

// NewServiceProvider creates a new DICOM server object.  "listenAddr" is the
// TCP address to listen to. E.g., ":1234" will listen to port 1234 at all the
// IP address that this machine can bind to.  Run() will actually start running
// the service.
func NewServiceProvider(params ServiceProviderParams, port string) (*ServiceProvider, error) {
	dicomlog.SetLevel(-1)

	if params.Verbose {
		dicomlog.SetLevel(0)
	}

	sp := &ServiceProvider{
		params: params,
		label:  newUID("sp"),
	}
	var err error
	if params.TLSConfig != nil {
		sp.listener, err = tls.Listen("tcp", port, params.TLSConfig)
	} else {
		sp.listener, err = net.Listen("tcp", port)
	}
	if err != nil {
		return nil, err
	}
	return sp, nil
}

func getConnState(conn net.Conn) (cs ConnectionState) {
	tlsConn, ok := conn.(*tls.Conn)
	if ok {
		cs.TLS = tlsConn.ConnectionState()
	}
	return
}

// RunProviderForConn starts threads for running a DICOM server on "conn". This
// function returns immediately; "conn" will be cleaned up in the background.
func RunProviderForConn(ctx context.Context, conn net.Conn, params ServiceProviderParams) {
	upcallCh := make(chan upcallEvent, 128)
	label := newUID("sc")
	disp := newServiceDispatcher(label)
	disp.registerCallback(dimse.CommandFieldCStoreRq,
		func(msg dimse.Message, data *dimse.DimseCommand, cs *serviceCommandState) {
			handleCStore(ctx, params, getConnState(conn), msg.(*dimse.CStoreRq), data, cs)
		})
	disp.registerCallback(dimse.CommandFieldCFindRq,
		func(msg dimse.Message, data *dimse.DimseCommand, cs *serviceCommandState) {
			handleCFind(params, getConnState(conn), msg.(*dimse.CFindRq), data, cs)
		})
	disp.registerCallback(dimse.CommandFieldCMoveRq,
		func(msg dimse.Message, data *dimse.DimseCommand, cs *serviceCommandState) {
			handleCMove(params, getConnState(conn), msg.(*dimse.CMoveRq), data, cs)
		})
	disp.registerCallback(dimse.CommandFieldCGetRq,
		func(msg dimse.Message, data *dimse.DimseCommand, cs *serviceCommandState) {
			handleCGet(params, getConnState(conn), msg.(*dimse.CGetRq), data, cs)
		})
	disp.registerCallback(dimse.CommandFieldCEchoRq,
		func(msg dimse.Message, data *dimse.DimseCommand, cs *serviceCommandState) {
			handleCEcho(params, getConnState(conn), msg.(*dimse.CEchoRq), data, cs)
		})
	go runStateMachineForServiceProvider(conn, upcallCh, disp.downcallCh, label)
	for event := range upcallCh {
		disp.handleEvent(event)
	}
	dicomlog.Vprintf(0, "dicom.serviceProvider(%s): Finished connection %p (remote: %+v)", label, conn, conn.RemoteAddr())
	disp.close()
}

// Run listens to incoming connections, accepts them, and runs the DICOM
// protocol. This function blocks until the context is cancelled.
func (sp *ServiceProvider) Run(ctx context.Context) {
	commandset.Init()

	// Create a channel to handle graceful shutdown
	connCh := make(chan net.Conn)
	errCh := make(chan error)

	// Start accepting connections in a goroutine
	go func() {
		for {
			conn, err := sp.listener.Accept()
			if err != nil {
				errCh <- err
				return
			}
			connCh <- conn
		}
	}()

	for {
		select {
		case <-ctx.Done():
			dicomlog.Vprintf(0, "dicom.serviceProvider(%s): Context cancelled, stopping server", sp.label)
			sp.listener.Close() // Close listener to stop accepting new connections
			return
		case conn := <-connCh:
			dicomlog.Vprintf(0, "dicom.serviceProvider(%s): Accepted connection %p (remote: %+v)", sp.label, conn, conn.RemoteAddr())
			go func() { RunProviderForConn(ctx, conn, sp.params) }()
		case err := <-errCh:
			// Check if the error is due to listener being closed (during shutdown)
			if ctx.Err() != nil {
				dicomlog.Vprintf(0, "dicom.serviceProvider(%s): Accept terminated due to context cancellation", sp.label)
				return
			}
			dicomlog.Vprintf(0, "dicom.serviceProvider(%s): Accept error: %v", sp.label, err)
			// Don't return here, continue listening for connections
		}
	}
}

// RunForever listens to incoming connections, accepts them, and runs the DICOM
// protocol. This function never returns and is provided for backward compatibility.
// For new code, prefer using Run() with a context.Context for graceful shutdown.
func (sp *ServiceProvider) RunForever() {
	sp.Run(context.Background())
}

// Close closes the underlying listener, causing the server to stop accepting new connections.
// Existing connections will continue to be served.
func (sp *ServiceProvider) Close() error {
	return sp.listener.Close()
}

// ListenAddr returns the TCP address that the server is listening on. It is the
// address passed to the NewServiceProvider(), except that if value was of form
// <name>:0, the ":0" part is replaced by the actual port numwber.
func (sp *ServiceProvider) ListenAddr() net.Addr {
	return sp.listener.Addr()
}
