package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	netdicom "github.com/algm/go-netdicom"
	"github.com/algm/go-netdicom/dimse"
	"github.com/algm/go-netdicom/sopclass"
	dicom "github.com/grailbio/go-dicom"
	"github.com/grailbio/go-dicom/dicomtag"
)

// FindResult holds one C-FIND response item. Err is set on error items;
// all other fields are zero when Err != nil.
type FindResult struct {
	Err               error
	Level             string
	PatientName       string
	PatientID         string
	StudyInstanceUID  string
	StudyDate         string
	StudyDescription  string
	AccessionNumber   string
	ModalitiesInStudy string
	SeriesInstanceUID string
	SeriesNumber      string
	SeriesDescription string
	Modality          string
	NumInstances      int
	SOPInstanceUID    string
	InstanceNumber    int
}

// MoveProgress reports sub-operation counts from a C-MOVE-RSP.
type MoveProgress struct {
	Remaining int
	Completed int
	Failed    int
	Warning   int
}

// DicomClient is an SCU that wraps the go-netdicom library.
type DicomClient struct {
	profile      ServerProfile
	localAETitle string
}

// NewDicomClient creates a client configured for the given server profile.
func NewDicomClient(profile ServerProfile, localAETitle string) *DicomClient {
	return &DicomClient{profile: profile, localAETitle: localAETitle}
}

// Echo sends a C-ECHO (Verification SOP Class 1.2.840.10008.1.1) to verify
// DICOM connectivity with the configured server. The association is opened,
// the echo is sent, and the association is released — all within one call.
// Returns nil on a successful Status 0000H response.
func (c *DicomClient) Echo(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
		CalledAETitle:  c.profile.RemoteAETitle,
		CallingAETitle: c.localAETitle,
		SOPClasses:     sopclass.VerificationClasses,
	})
	if err != nil {
		return fmt.Errorf("c-echo: create service user: %w", err)
	}

	type result struct{ err error }
	done := make(chan result, 1)

	go func() {
		defer su.Release()
		su.Connect(fmt.Sprintf("%s:%d", c.profile.Host, c.profile.Port))
		done <- result{su.CEcho()}
	}()

	select {
	case r := <-done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Find sends a C-FIND (Study Root or Patient Root QR) at the given query level
// and streams results on the returned channel. The channel is closed when the
// query completes or ctx is cancelled. A non-nil error is returned only when the
// ServiceUser cannot be created. Association failures (e.g. the connection
// dropped mid-session) and query rejections are reported in-band as a
// FindResult with Err set, so callers can distinguish a genuinely empty result
// from a failed query.
func (c *DicomClient) Find(ctx context.Context, level string, params map[string]string) (<-chan FindResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
		CalledAETitle:  c.profile.RemoteAETitle,
		CallingAETitle: c.localAETitle,
		SOPClasses:     sopclass.QRFindClasses,
	})
	if err != nil {
		return nil, fmt.Errorf("c-find: create service user: %w", err)
	}

	out := make(chan FindResult, 128)

	go func() {
		defer close(out)
		defer su.Release()
		su.Connect(fmt.Sprintf("%s:%d", c.profile.Host, c.profile.Port))

		for r := range su.CFind(levelToQRLevel(level), buildFindFilter(level, params)) {
			if r.Err != nil {
				select {
				case out <- FindResult{Err: r.Err}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case out <- elementsToFindResult(r.Elements):
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

// Move sends a C-MOVE-RQ (PS3.4 C.4.2) for the given UIDs, directing the PACS
// to push files to destAE. onProgress is called for each C-MOVE-RSP pending
// response. Returns nil when the final response carries StatusSuccess (0000H).
// patientID is included in the filter when non-empty; required by PACS that
// mandate it at STUDY level (Patient Root model per PS3.4 C.4.2.1).
func (c *DicomClient) Move(ctx context.Context, level, patientID, studyUID, seriesUID string, destAE string, onProgress func(MoveProgress)) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
		CalledAETitle:  c.profile.RemoteAETitle,
		CallingAETitle: c.localAETitle,
		SOPClasses:     sopclass.QRMoveClasses,
	})
	if err != nil {
		return fmt.Errorf("c-move: create service user: %w", err)
	}

	type result struct{ err error }
	done := make(chan result, 1)

	go func() {
		defer su.Release()
		su.Connect(fmt.Sprintf("%s:%d", c.profile.Host, c.profile.Port))

		progressFn := func(p netdicom.CMoveProgress) {
			if onProgress != nil {
				onProgress(MoveProgress{
					Remaining: p.Remaining,
					Completed: p.Completed,
					Failed:    p.Failed,
					Warning:   p.Warning,
				})
			}
		}
		done <- result{su.CMove(levelToQRLevel(level), destAE, buildMoveFilter(level, patientID, studyUID, seriesUID), progressFn)}
	}()

	select {
	case r := <-done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Get sends a C-GET-RQ (PS3.4 C.4.3) for the given UIDs, causing the PACS to
// return DICOM instances over the same association. onStore is called once per
// received instance; returning a non-nil error sends CStoreOutOfResources and
// aborts the retrieve. C-GET does not require a separate inbound C-STORE SCP.
// Returns nil when the final response carries StatusSuccess (0000H).
func (c *DicomClient) Get(ctx context.Context, level, patientID, studyUID, seriesUID string,
	onStore func(transferSyntaxUID, sopClassUID, sopInstanceUID string, data []byte) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	su, err := netdicom.NewServiceUser(netdicom.ServiceUserParams{
		CalledAETitle:  c.profile.RemoteAETitle,
		CallingAETitle: c.localAETitle,
		SOPClasses:     sopclass.QRGetClasses,
	})
	if err != nil {
		return fmt.Errorf("c-get: create service user: %w", err)
	}

	type result struct{ err error }
	done := make(chan result, 1)

	go func() {
		defer su.Release()
		su.Connect(fmt.Sprintf("%s:%d", c.profile.Host, c.profile.Port))
		done <- result{su.CGet(levelToQRLevel(level), buildMoveFilter(level, patientID, studyUID, seriesUID),
			func(txUID, scUID, siUID string, data []byte) dimse.Status {
				if storeErr := onStore(txUID, scUID, siUID, data); storeErr != nil {
					return dimse.Status{Status: dimse.CStoreOutOfResources, ErrorComment: storeErr.Error()}
				}
				return dimse.Success
			})}
	}()

	select {
	case r := <-done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// levelToQRLevel maps the query-level string used by the UI to the go-netdicom
// QRLevel constant. Defaults to Study Root if the level is unrecognised.
func levelToQRLevel(level string) netdicom.QRLevel {
	switch strings.ToUpper(level) {
	case "SERIES":
		return netdicom.QRLevelSeries
	case "PATIENT":
		return netdicom.QRLevelPatient
	default:
		return netdicom.QRLevelStudy
	}
}

// buildFindFilter builds a C-FIND identifier dataset from the UI query params.
// Empty-value elements act as return keys; non-empty values are match criteria.
// At SERIES level only StudyInstanceUID is used as a match key; all other fields
// are return keys. StudyDate uses DICOM range syntax (PS3.4 C.2.2.2.5).
func buildFindFilter(level string, params map[string]string) []*dicom.Element {
	if strings.ToUpper(level) == "SERIES" {
		return []*dicom.Element{
			dicom.MustNewElement(dicomtag.SpecificCharacterSet, "ISO_IR 192"),
			dicom.MustNewElement(dicomtag.StudyInstanceUID, params["StudyInstanceUID"]),
			dicom.MustNewElement(dicomtag.SeriesInstanceUID, ""),
			dicom.MustNewElement(dicomtag.SeriesNumber, ""),
			dicom.MustNewElement(dicomtag.Modality, ""),
			dicom.MustNewElement(dicomtag.SeriesDescription, ""),
			dicom.MustNewElement(dicomtag.NumberOfSeriesRelatedInstances, ""),
		}
	}
	dateRange := buildDateRange(params["StudyDateFrom"], params["StudyDateTo"])
	return []*dicom.Element{
		dicom.MustNewElement(dicomtag.SpecificCharacterSet, "ISO_IR 192"),
		dicom.MustNewElement(dicomtag.PatientName, params["PatientName"]),
		dicom.MustNewElement(dicomtag.PatientID, params["PatientID"]),
		dicom.MustNewElement(dicomtag.AccessionNumber, params["AccessionNumber"]),
		dicom.MustNewElement(dicomtag.StudyDate, dateRange),
		dicom.MustNewElement(dicomtag.StudyInstanceUID, ""),
		dicom.MustNewElement(dicomtag.StudyDescription, ""),
		dicom.MustNewElement(dicomtag.ModalitiesInStudy, params["ModalitiesInStudy"]),
	}
}

// buildMoveFilter builds the C-MOVE identifier dataset for the given UIDs.
// PatientID is included when non-empty — required by PACS using Patient Root
// at STUDY level (PS3.4 C.4.2.1). At SERIES level SeriesInstanceUID is also
// included so the PACS can scope the sub-operations correctly (PS3.4 C.4.2).
func buildMoveFilter(level, patientID, studyUID, seriesUID string) []*dicom.Element {
	var filter []*dicom.Element
	if patientID != "" {
		filter = append(filter, dicom.MustNewElement(dicomtag.PatientID, patientID))
	}
	filter = append(filter, dicom.MustNewElement(dicomtag.StudyInstanceUID, studyUID))
	if level == "SERIES" && seriesUID != "" {
		filter = append(filter, dicom.MustNewElement(dicomtag.SeriesInstanceUID, seriesUID))
	}
	return filter
}

// buildDateRange encodes a DICOM date range string from UI from/to values.
// Returns "" (match-all) when both are empty.
func buildDateRange(from, to string) string {
	switch {
	case from == "" && to == "":
		return ""
	case from == "":
		return "-" + to
	case to == "":
		return from + "-"
	default:
		return from + "-" + to
	}
}

// elementsToFindResult extracts a FindResult from the elements in one C-FIND
// response dataset. Unknown tags are silently ignored.
func elementsToFindResult(elems []*dicom.Element) FindResult {
	var r FindResult
	for _, elem := range elems {
		s, err := elem.GetString()
		if err != nil {
			continue
		}
		switch elem.Tag {
		case dicomtag.PatientName:
			r.PatientName = s
		case dicomtag.PatientID:
			r.PatientID = s
		case dicomtag.StudyInstanceUID:
			r.StudyInstanceUID = s
		case dicomtag.StudyDate:
			r.StudyDate = s
		case dicomtag.StudyDescription:
			r.StudyDescription = s
		case dicomtag.AccessionNumber:
			r.AccessionNumber = s
		case dicomtag.ModalitiesInStudy:
			r.ModalitiesInStudy = s
		case dicomtag.SeriesInstanceUID:
			r.SeriesInstanceUID = s
		case dicomtag.SeriesNumber:
			r.SeriesNumber = s
		case dicomtag.SeriesDescription:
			r.SeriesDescription = s
		case dicomtag.Modality:
			r.Modality = s
		case dicomtag.SOPInstanceUID:
			r.SOPInstanceUID = s
		case dicomtag.InstanceNumber:
			if n, err2 := strconv.Atoi(s); err2 == nil {
				r.InstanceNumber = n
			}
		case dicomtag.NumberOfSeriesRelatedInstances:
			if n, err2 := strconv.Atoi(s); err2 == nil {
				r.NumInstances = n
			}
		}
	}
	return r
}

// Close is a no-op; associations are opened and closed per-operation.
func (c *DicomClient) Close() {}

// localIP returns the preferred outbound IPv4 address of this machine.
// It tries a UDP connect first (no packets sent); on failure it enumerates
// network interfaces as a fallback for air-gapped environments (Phase 3-H).
func localIP() string {
	if conn, err := net.Dial("udp4", "8.8.8.8:53"); err == nil {
		defer conn.Close()
		return conn.LocalAddr().(*net.UDPAddr).IP.String()
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return "unknown"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() {
				return ip4.String()
			}
		}
	}
	return "unknown"
}
