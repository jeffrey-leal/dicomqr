package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// Live diagnostics against the local UTILITY archive (127.0.0.1:5678).
// Run manually: DICOMQR_LIVE=1 go test -run TestLiveUtility -v -timeout 300s .
// Requires the dicomqr app to be closed (port 11112 must be free).

const (
	liveStudyUID    = "1.2.840.114350.2.331.2.798268.2.1017386863.1"
	livePatientID   = "E0092066"
	livePRSeries    = "1.2.840.113845.11.2000000002170592405.20250306161036.1105994" // PR annotations
	livePDFSeries   = "1.2.840.113845.11.2000000002170592405.20250306161058.1105995" // Encapsulated PDF
	liveImageSeries = "1.2.840.113619.2.391.85445.1741260319.331.1"                  // US images (control)
)

func liveMoveSeries(t *testing.T, seriesUID, label string) {
	t.Helper()
	dir := t.TempDir()
	scp := NewStorageSCP("DICOMQR", 11112, dir)
	if err := scp.Start(); err != nil {
		t.Fatalf("scp start (close the dicomqr app first?): %v", err)
	}
	defer scp.Stop()

	nRecv := 0
	scp.SetOnFileReceived(func(path string) {
		nRecv++
		t.Logf("received %d: %s", nRecv, path)
	})

	profile := ServerProfile{
		Name: "UTILITY", RemoteAETitle: "UTILITY",
		Host: "127.0.0.1", Port: 5678, InfoModel: "study",
	}
	client := NewDicomClient(profile, "DICOMQR")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	start := time.Now()
	err := client.Move(ctx, "SERIES", livePatientID, liveStudyUID, seriesUID, "DICOMQR",
		func(p MoveProgress) {
			t.Logf("progress: remaining=%d completed=%d failed=%d warning=%d",
				p.Remaining, p.Completed, p.Failed, p.Warning)
		})
	elapsed := time.Since(start)

	if ctx.Err() != nil {
		t.Fatalf("HANG (%s): C-MOVE did not complete within 45s — received %d file(s) before stall", label, nRecv)
	}
	t.Logf("%s: C-MOVE finished in %v, err=%v, files received=%d", label, elapsed, err, nRecv)
	if err != nil {
		t.Fatalf("%s: C-MOVE error: %v", label, err)
	}
}

func TestLiveUtilityMoveImageSeries(t *testing.T) {
	if os.Getenv("DICOMQR_LIVE") == "" {
		t.Skip("set DICOMQR_LIVE=1 to run against the local UTILITY archive")
	}
	liveMoveSeries(t, liveImageSeries, "US image series (control)")
}

func TestLiveUtilityMovePRSeries(t *testing.T) {
	if os.Getenv("DICOMQR_LIVE") == "" {
		t.Skip("set DICOMQR_LIVE=1 to run against the local UTILITY archive")
	}
	liveMoveSeries(t, livePRSeries, "PR annotation series")
}

func TestLiveUtilityMovePDFSeries(t *testing.T) {
	if os.Getenv("DICOMQR_LIVE") == "" {
		t.Skip("set DICOMQR_LIVE=1 to run against the local UTILITY archive")
	}
	liveMoveSeries(t, livePDFSeries, "Encapsulated PDF series")
}
