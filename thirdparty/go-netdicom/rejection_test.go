package netdicom

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/algm/go-netdicom/pdu"
	"github.com/algm/go-netdicom/sopclass"
)

// rejectingPeer accepts one connection, reads the A-ASSOCIATE-RQ and answers
// it with an A-ASSOCIATE-RJ carrying the given result/source/reason.
func rejectingPeer(t *testing.T, result, source, reason byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var hdr [6]byte
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(hdr[2:]))
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		// PS3.8 9.3.4: type 03, reserved, length 4, then reserved/result/source/reason.
		conn.Write([]byte{0x03, 0x00, 0, 0, 0, 4, 0x00, result, source, reason})
		// Hold the connection until the requestor closes it.
		io.Copy(io.Discard, conn)
	}()
	return ln.Addr().String()
}

func TestRejectedAssociationIsTyped(t *testing.T) {
	addr := rejectingPeer(t, 2, 3, 2) // transient, presentation provider, local limit exceeded
	su, err := NewServiceUser(ServiceUserParams{SOPClasses: sopclass.VerificationClasses})
	if err != nil {
		t.Fatal(err)
	}
	defer su.Release()
	su.Connect(addr)

	done := make(chan error, 1)
	go func() { done <- su.CEcho() }()
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("CEcho did not return after the peer rejected the association")
	}

	var rj *AssociationRejectedError
	if !errors.As(err, &rj) {
		t.Fatalf("error %v (%T) is not an *AssociationRejectedError", err, err)
	}
	if !rj.Transient() || !rj.ResourceLimited() {
		t.Errorf("Transient=%v ResourceLimited=%v, want both true", rj.Transient(), rj.ResourceLimited())
	}
	if !strings.Contains(err.Error(), "local limit exceeded") {
		t.Errorf("error text %q does not name the reason", err)
	}
	// The established prefix is kept, so existing log searches still match.
	if !strings.HasPrefix(err.Error(), "dicom.serviceUser: Connection failed") {
		t.Errorf("error text %q lost the established prefix", err)
	}
}

func TestRejectReasonTextDependsOnSource(t *testing.T) {
	cases := []struct {
		source pdu.SourceType
		reason pdu.RejectReasonType
		want   string
	}{
		{pdu.SourceULServiceUser, 2, "application context name not supported"},
		{pdu.SourceULServiceUser, 7, "called AE title not recognized"},
		{pdu.SourceULServiceProviderACSE, 2, "protocol version not supported"},
		{pdu.SourceULServiceProviderPresentation, 1, "temporary congestion"},
		{pdu.SourceULServiceProviderPresentation, 2, "local limit exceeded"},
		{pdu.SourceULServiceProviderPresentation, 5, "reason 5"},
	}
	for _, c := range cases {
		rj := pdu.AAssociateRj{Result: pdu.ResultRejectedPermanent, Source: c.source, Reason: c.reason}
		if got := rj.ReasonText(); got != c.want {
			t.Errorf("source %d reason %d: got %q, want %q", c.source, c.reason, got, c.want)
		}
	}
}

func TestOtherConnectionFailuresAreNotRejections(t *testing.T) {
	// A peer that accepts and closes without a word is a failure, not a reject.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if conn, err := ln.Accept(); err == nil {
			conn.Close()
		}
	}()
	su, err := NewServiceUser(ServiceUserParams{SOPClasses: sopclass.VerificationClasses})
	if err != nil {
		t.Fatal(err)
	}
	defer su.Release()
	su.Connect(ln.Addr().String())
	err = su.CEcho()
	if err == nil {
		t.Fatal("CEcho succeeded against a peer that closed the connection")
	}
	var rj *AssociationRejectedError
	if errors.As(err, &rj) {
		t.Fatalf("closed connection reported as a rejection: %v", err)
	}
}
