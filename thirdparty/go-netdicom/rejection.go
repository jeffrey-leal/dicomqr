package netdicom

import (
	"fmt"

	"github.com/algm/go-netdicom/pdu"
)

// AssociationRejectedError is returned when the peer answers an
// A-ASSOCIATE-RQ with A-ASSOCIATE-RJ (dicomqr local patch). Upstream
// reported every failure to establish an association — refused, unreachable,
// timed out — as one fixed string, which left a caller nothing to decide on.
// Use errors.As to recognise it.
type AssociationRejectedError struct {
	Result pdu.RejectResultType
	Source pdu.SourceType
	Reason pdu.RejectReasonType
}

func (e *AssociationRejectedError) Error() string {
	kind := "permanently"
	if e.Transient() {
		kind = "transiently"
	}
	return fmt.Sprintf("dicom.serviceUser: Connection failed: association rejected %s (%s)", kind, e.ReasonText())
}

// ReasonText names the reject reason, read according to its source.
func (e *AssociationRejectedError) ReasonText() string {
	rj := pdu.AAssociateRj{Result: e.Result, Source: e.Source, Reason: e.Reason}
	return rj.ReasonText()
}

// Transient reports whether the peer marked the rejection as temporary.
func (e *AssociationRejectedError) Transient() bool {
	return e.Result == pdu.ResultRejectedTransient
}

// ResourceLimited reports whether the peer refused for lack of capacity —
// temporary congestion or a local limit on associations — rather than over
// the request itself (an unknown AE title, an unsupported context).
func (e *AssociationRejectedError) ResourceLimited() bool {
	return e.Source == pdu.SourceULServiceProviderPresentation && (e.Reason == 1 || e.Reason == 2)
}
