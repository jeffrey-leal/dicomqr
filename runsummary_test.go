package main

// The progress windows close themselves only when a run has nothing to say
// beyond "done" (user decision 2026-10-16). These tests pin what counts as
// something to say: closing on a clause the user needed to read would make it
// as good as unreported.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestModificationSummaryReview(t *testing.T) {
	clean := modifyResult{Processed: 10}
	for _, tc := range []struct {
		name   string
		res    modifyResult
		review bool
		clause string // a substring the message must carry
	}{
		{"clean", clean, false, "Done — 10 file(s) written to OUT"},
		// The profile's own filters skipping files is what it was asked to do.
		{"filtered files skipped", modifyResult{Processed: 10, Skipped: 3}, false, "3 skipped"},
		{"recompressed into its own syntax", modifyResult{Processed: 10, MaskRecompressed: 2}, false, "original transfer syntax"},
		{"dicomdir written", modifyResult{Processed: 10, DicomdirWritten: true}, false, "DICOMDIR index written"},

		{"cancelled", modifyResult{Processed: 4, Canceled: true}, true, "Cancelled"},
		{"failed file", modifyResult{Processed: 9, Failed: 1}, true, "1 failed"},
		{"nothing written", modifyResult{Skipped: 10}, true, "0 file(s) written"},
		// The encoding must never change silently.
		{"lossy re-encoded lossless", modifyResult{Processed: 10, MaskRecodedLossless: 1}, true, "re-encoded"},
		{"decompressed for masking", modifyResult{Processed: 10, MaskDecompressed: 1}, true, "decompressed"},
		{"ultrasound fallback", modifyResult{Processed: 10, MaskUSFallback: 1}, true, "worth reviewing"},
		{"nested birth date", modifyResult{Processed: 10, NestedDOBKept: 1}, true, "0400,0561"},
		{"phi finding", modifyResult{Processed: 10, PHIRisks: map[phiRisk]int{phiRiskKinds[0]: 1}}, true, "review before sharing"},
		{"dicomdir failed", modifyResult{Processed: 10, DicomdirError: "disk full"}, true, "could not be written"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, review := modificationSummary(tc.res, "OUT")
			if review != tc.review {
				t.Errorf("review = %v, want %v (msg %q)", review, tc.review, msg)
			}
			if !strings.Contains(msg, tc.clause) {
				t.Errorf("msg %q lacks %q", msg, tc.clause)
			}
		})
	}

	// A PHI map holding only zero counts says nothing, so it holds nothing.
	if _, review := modificationSummary(modifyResult{Processed: 1, PHIRisks: map[phiRisk]int{phiRiskKinds[0]: 0}}, "OUT"); review {
		t.Error("an all-zero PHI map kept the window open")
	}
}

func TestPushSummaryClean(t *testing.T) {
	refused := errors.New("association refused")
	for _, tc := range []struct {
		name         string
		sent, failed int
		degraded     error
		err          error
		clean        bool
		clause       string
	}{
		{"all sent", 12, 0, nil, nil, true, "Done — 12 sent"},
		{"some failed", 10, 2, nil, nil, false, "2 failed"},
		{"every file failed", 0, 12, nil, nil, false, "12 failed"},
		{"nothing sent", 0, 0, nil, nil, false, "0 sent"},
		{"stopped by an error", 0, 0, nil, errors.New("c-store: boom"), false, "stopped: c-store"},
		{"cancelled", 5, 0, nil, context.Canceled, false, "Cancelled — 5 sent, 7 not sent"},
		{"cancelled after failures", 3, 2, nil, context.Canceled, false, "2 failed (see Activity Log), 7 not sent"},
		// Every file arrived, but the clause says to change the server's
		// setting, so it must be read rather than closed on.
		{"fell back to one association", 12, 0, refused, nil, false, "set Parallel transfers to 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, clean := pushSummary(tc.sent, tc.failed, 12, tc.degraded, tc.err)
			if clean != tc.clean {
				t.Errorf("clean = %v, want %v (msg %q)", clean, tc.clean, msg)
			}
			if !strings.Contains(msg, tc.clause) {
				t.Errorf("msg %q lacks %q", msg, tc.clause)
			}
		})
	}
}
