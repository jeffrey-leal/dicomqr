package netdicom

// Tests for the exact-syntax raw push (dicomqr local patch): presentation
// contexts proposed verbatim, several per SOP class, and CStoreRaw sending a
// data set only on a context whose transfer syntax is exactly its own.

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/algm/go-netdicom/dimse"
	"github.com/algm/go-netdicom/pdu/pdu_item"
	"github.com/stretchr/testify/require"
)

const (
	testCT       = "1.2.840.10008.5.1.4.1.1.2"     // CT Image Storage
	testDoseSR   = "1.2.840.10008.5.1.4.1.1.88.67" // X-Ray Radiation Dose SR Storage
	testJ2K      = "1.2.840.10008.1.2.4.90"        // JPEG 2000 Lossless
	testExplicit = "1.2.840.10008.1.2.1"           // Explicit VR Little Endian
	testImplicit = "1.2.840.10008.1.2"             // Implicit VR Little Endian
	testPrivate  = "1.3.6.1.4.1.99999.1.1"         // a vendor's private class
)

// received is one C-STORE as the test provider saw it.
type received struct {
	sopClass, transferSyntax string
	data                     []byte
}

// startRecordingProvider runs a C-STORE provider on loopback that records what
// it receives. accepted restricts its transfer syntaxes (nil accepts the first
// one offered in each context, as dcm4che-based archives do).
func startRecordingProvider(t *testing.T, accepted []string) (addr string, got func() []received) {
	t.Helper()
	var mu sync.Mutex
	var all []received
	params := ServiceProviderParams{
		AETitle:                  "TESTSCP",
		AcceptedTransferSyntaxes: accepted,
		CStore: func(_ context.Context, _ ConnectionState, transferSyntaxUID, sopClassUID, _ string,
			dataReader io.Reader, _ int64) dimse.Status {
			data, err := io.ReadAll(dataReader)
			if err != nil {
				return dimse.Status{Status: dimse.CStoreCannotUnderstand}
			}
			mu.Lock()
			all = append(all, received{sopClassUID, transferSyntaxUID, data})
			mu.Unlock()
			return dimse.Success
		},
	}
	sp, err := NewServiceProvider(params, "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	go sp.Run(ctx)
	t.Cleanup(func() {
		cancel()
		_ = sp.Close()
	})
	time.Sleep(100 * time.Millisecond)
	return sp.ListenAddr().String(), func() []received {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(all)
	}
}

func connectUser(t *testing.T, addr string, params ServiceUserParams) *ServiceUser {
	t.Helper()
	params.CalledAETitle = "TESTSCP"
	params.CallingAETitle = "TESTSCU"
	su, err := NewServiceUser(params)
	require.NoError(t, err)
	su.Connect(addr)
	t.Cleanup(su.Release)
	return su
}

func TestCStoreRawSendsEachSyntaxOnItsOwnContext(t *testing.T) {
	addr, got := startRecordingProvider(t, nil)
	su := connectUser(t, addr, ServiceUserParams{PresentationContexts: []PresentationContextSpec{
		{AbstractSyntax: testCT, TransferSyntaxes: []string{testJ2K}},
		{AbstractSyntax: testCT, TransferSyntaxes: []string{testExplicit, testImplicit}},
		{AbstractSyntax: testPrivate, TransferSyntaxes: []string{testExplicit, testImplicit}},
	}})

	j2k := []byte{0x08, 0x00, 0x16, 0x00, 'U', 'I', 2, 0, '1', 0}
	explicit := []byte{0x08, 0x00, 0x18, 0x00, 'U', 'I', 2, 0, '2', 0}
	require.NoError(t, su.CStoreRaw(testCT, "1.2.3.1", testJ2K, j2k))
	require.NoError(t, su.CStoreRaw(testCT, "1.2.3.2", testExplicit, explicit))
	require.NoError(t, su.CStoreRaw(testPrivate, "1.2.3.3", testExplicit, explicit))

	r := got()
	require.Len(t, r, 3)
	require.Equal(t, received{testCT, testJ2K, j2k}, r[0], "compressed bytes arrive under their own syntax")
	require.Equal(t, received{testCT, testExplicit, explicit}, r[1])
	require.Equal(t, testPrivate, r[2].sopClass, "a class on no list is sent once it is proposed")
}

// The defect this patch fixes: the old parameters canonicalise every
// compressed syntax to Explicit VR Little Endian, so the server accepted
// "Explicit VR LE" and CStoreRaw sent JPEG 2000 bytes under that label.
func TestCStoreRawNeverSendsCompressedBytesOnAnUncompressedContext(t *testing.T) {
	addr, got := startRecordingProvider(t, nil)
	su := connectUser(t, addr, ServiceUserParams{
		SOPClasses:       []string{testCT},
		TransferSyntaxes: []string{testJ2K, testExplicit, testImplicit},
	})

	err := su.CStoreRaw(testCT, "1.2.3.1", testJ2K, []byte{1, 2, 3, 4})

	var mismatch *TransferSyntaxMismatchError
	require.True(t, errors.As(err, &mismatch), "want a mismatch, got %v", err)
	require.Equal(t, testExplicit, mismatch.Negotiated)
	require.Equal(t, []string{testExplicit}, mismatch.Accepted)
	require.Equal(t, testJ2K, mismatch.File)
	require.Empty(t, got(), "nothing may be sent under the wrong label")
}

func TestCStoreRawRefusedSyntaxNamesTheAcceptedOnes(t *testing.T) {
	addr, got := startRecordingProvider(t, []string{testExplicit, testImplicit})
	su := connectUser(t, addr, ServiceUserParams{PresentationContexts: []PresentationContextSpec{
		{AbstractSyntax: testCT, TransferSyntaxes: []string{testJ2K}},
		{AbstractSyntax: testCT, TransferSyntaxes: []string{testImplicit, testExplicit}},
	}})

	err := su.CStoreRaw(testCT, "1.2.3.1", testJ2K, []byte{1, 2})
	var mismatch *TransferSyntaxMismatchError
	require.True(t, errors.As(err, &mismatch), "want a mismatch, got %v", err)
	// The provider prefers Explicit from its own list; that is the target.
	require.Equal(t, testExplicit, mismatch.Negotiated)
	require.Equal(t, []string{testExplicit}, mismatch.Accepted)

	// The converted data set then goes out on the fallback context.
	require.NoError(t, su.CStoreRaw(testCT, "1.2.3.1", testExplicit, []byte{1, 2}))
	require.Equal(t, []received{{testCT, testExplicit, []byte{1, 2}}}, got())
}

func TestCStoreRawSOPClassNotAccepted(t *testing.T) {
	addr, _ := startRecordingProvider(t, []string{testExplicit})
	su := connectUser(t, addr, ServiceUserParams{PresentationContexts: []PresentationContextSpec{
		{AbstractSyntax: testCT, TransferSyntaxes: []string{testExplicit}},
		{AbstractSyntax: testDoseSR, TransferSyntaxes: []string{testJ2K}}, // refused: no acceptable syntax
	}})

	var notAccepted *SOPClassNotAcceptedError
	err := su.CStoreRaw(testDoseSR, "1.2.3.1", testJ2K, []byte{1, 2})
	require.True(t, errors.As(err, &notAccepted), "got %v", err)
	require.Equal(t, SOPClassNotAcceptedError{SOPClass: testDoseSR, Proposed: true}, *notAccepted)

	err = su.CStoreRaw(testPrivate, "1.2.3.2", testExplicit, []byte{1, 2})
	require.True(t, errors.As(err, &notAccepted), "got %v", err)
	require.False(t, notAccepted.Proposed)
}

func TestGenerateAssociateRequestProposesSyntaxesVerbatim(t *testing.T) {
	m := newContextManager("test", nil)
	items := m.generateAssociateRequest([]PresentationContextSpec{
		{AbstractSyntax: testCT, TransferSyntaxes: []string{testJ2K}},
		{AbstractSyntax: testCT, TransferSyntaxes: []string{testExplicit, testImplicit}},
	})

	var ids []byte
	var syntaxes [][]string
	for _, item := range items {
		pc, ok := item.(*pdu_item.PresentationContextItem)
		if !ok {
			continue
		}
		ids = append(ids, pc.ContextID)
		var ts []string
		for _, sub := range pc.Items {
			if s, ok := sub.(*pdu_item.TransferSyntaxSubItem); ok {
				ts = append(ts, s.Name)
			}
		}
		syntaxes = append(syntaxes, ts)
	}
	require.Equal(t, []byte{1, 3}, ids)
	require.Equal(t, [][]string{{testJ2K}, {testExplicit, testImplicit}}, syntaxes)
}

func TestContextManagerKeepsTheFirstAcceptedContextPerClass(t *testing.T) {
	m := newContextManager("test", nil)
	addContextMapping(m, testCT, testJ2K, 1, pdu_item.PresentationContextProviderRejectionTransferSyntaxNotSupported)
	addContextMapping(m, testCT, testExplicit, 3, pdu_item.PresentationContextAccepted)
	addContextMapping(m, testCT, testImplicit, 5, pdu_item.PresentationContextAccepted)
	addContextMapping(m, testCT, testJ2K, 7, pdu_item.PresentationContextProviderRejectionTransferSyntaxNotSupported)

	byName, err := m.lookupByAbstractSyntaxUID(testCT)
	require.NoError(t, err, "a later refusal must not hide an accepted context")
	require.Equal(t, byte(3), byName.contextID)

	e, ok := m.lookupAccepted(testCT, testImplicit)
	require.True(t, ok)
	require.Equal(t, byte(5), e.contextID)
	_, ok = m.lookupAccepted(testCT, testJ2K)
	require.False(t, ok, "refused contexts never match")

	accepted, proposed := m.acceptedSyntaxesFor(testCT)
	require.True(t, proposed)
	require.Equal(t, []string{testExplicit, testImplicit}, accepted)
	_, proposed = m.acceptedSyntaxesFor(testDoseSR)
	require.False(t, proposed)

	// A message names its context explicitly, or falls back to the by-name one.
	e, err = m.sendContext(testCT, 5)
	require.NoError(t, err)
	require.Equal(t, byte(5), e.contextID)
	e, err = m.sendContext(testCT, 0)
	require.NoError(t, err)
	require.Equal(t, byte(3), e.contextID)
}

func TestValidatePresentationContexts(t *testing.T) {
	params := ServiceUserParams{PresentationContexts: []PresentationContextSpec{
		{AbstractSyntax: testCT, TransferSyntaxes: []string{testJ2K}},
	}}
	require.NoError(t, validateServiceUserParams(&params))
	require.Equal(t, []string{testJ2K}, params.PresentationContexts[0].TransferSyntaxes, "never canonicalised")

	params = ServiceUserParams{PresentationContexts: []PresentationContextSpec{{AbstractSyntax: testCT}}}
	require.Error(t, validateServiceUserParams(&params), "a context needs a transfer syntax")

	params = ServiceUserParams{}
	for range maxPresentationContexts + 1 {
		params.PresentationContexts = append(params.PresentationContexts,
			PresentationContextSpec{AbstractSyntax: testCT, TransferSyntaxes: []string{testExplicit}})
	}
	require.Error(t, validateServiceUserParams(&params), "more than 128 contexts")
}
