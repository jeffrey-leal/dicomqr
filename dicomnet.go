package main

import (
	"context"
	"errors"
)

// FindResult holds one C-FIND response item.
type FindResult struct {
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
// TODO: replace stub bodies with algm/go-netdicom calls once go mod tidy
// resolves the dependency.
type DicomClient struct {
	profile     ServerProfile
	localAETitle string
}

// NewDicomClient creates a client configured for the given server profile.
func NewDicomClient(profile ServerProfile, localAETitle string) *DicomClient {
	return &DicomClient{profile: profile, localAETitle: localAETitle}
}

// Echo sends a C-ECHO to verify connectivity. Returns nil on success.
func (c *DicomClient) Echo(ctx context.Context) error {
	// TODO: implement using algm/go-netdicom ServiceUser
	return errors.New("C-ECHO: not yet implemented")
}

// Find sends a C-FIND at the given query level and streams results on the
// returned channel. The channel is closed when the query completes or ctx
// is cancelled. A non-nil error is returned if the association fails.
func (c *DicomClient) Find(ctx context.Context, level string, params map[string]string) (<-chan FindResult, error) {
	// TODO: implement using algm/go-netdicom C-FIND
	ch := make(chan FindResult)
	close(ch)
	return ch, errors.New("C-FIND: not yet implemented")
}

// Move sends a C-MOVE-RQ for the given UIDs, directing the PACS to push
// files to destAE. Progress callbacks are fired for each C-MOVE-RSP pending
// response. Returns nil when the operation completes successfully.
func (c *DicomClient) Move(ctx context.Context, level, studyUID, seriesUID string, destAE string, onProgress func(MoveProgress)) error {
	// TODO: implement using algm/go-netdicom C-MOVE
	return errors.New("C-MOVE: not yet implemented")
}

// Close is a no-op; associations are opened and closed per-operation.
func (c *DicomClient) Close() {}
