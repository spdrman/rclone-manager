package app

import (
	"context"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Issue #284: the literal sentence the journal recorded, not a reconstruction
// of it.
//
// internal/lifecycle already has QuarantineReason, which rebuilds a plausible
// explanation from whatever else the record carries. GetArtifactDetail reads
// state_transitions.detail, which is the text the step that failed actually
// wrote and which nothing else in this codebase reads back. These tests are
// what keep those two from being confused: each drives a real failure through
// the real pipeline and then asserts the exact text came back, so a
// reconstruction slipped in behind the same field would not satisfy them.
//
// FAILED gets its own case beside the two quarantine states because FR-10
// gives FAILED no reason field anywhere else at all, so this read is the only
// one there is.
//
// The healthy case is the negative control. Without it, an implementation
// that returned the last transition's detail for every artifact would pass
// everything above.

// TestGetArtifactDetail_FailedArtifactCarriesTheJournalsOwnReason is issue
// #284's core claim at the use-case layer: an artifact that reached FAILED
// carries the diagnostic sentence internal/lifecycle recorded on the
// transition that put it there, read back from the one place that text
// actually lives (state_transitions.detail), not reconstructed from
// whatever else the record happens to carry.
func TestGetArtifactDetail_FailedArtifactCarriesTheJournalsOwnReason(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	tr := newFakeTransport()
	tr.put("backup.dump", "payload", epoch.Unix())

	journal := openJournal(t)
	ctx := context.Background()
	rec := discoverOneRecord(t, ctx, journal, tr, transport.Source{}, bs)

	wantDetail := `hash verification required (sha256) but the backend could not supply a comparable remote hash: unsupported_capability: backend "fake" cannot compute sha256`
	failedAt := epoch.Add(time.Hour)
	for _, transition := range []state.Transition{
		{Artifact: rec.Artifact, Key: "t1", From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: epoch},
		{Artifact: rec.Artifact, Key: "t2", From: "TRANSFERRING", To: "TRANSFERRED", OccurredAt: epoch},
		{Artifact: rec.Artifact, Key: "t3", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: epoch},
		{Artifact: rec.Artifact, Key: "t4", From: "VERIFYING", To: "FAILED", OccurredAt: failedAt, Detail: wantDetail},
	} {
		if _, err := journal.RecordTransition(ctx, transition); err != nil {
			t.Fatalf("-> %s: %v", transition.To, err)
		}
	}

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	detail, err := svc.GetArtifactDetail(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("GetArtifactDetail: %v", err)
	}
	if detail.State != "FAILED" {
		t.Fatalf("State = %q, want FAILED", detail.State)
	}
	if detail.FailureReason != wantDetail {
		t.Errorf("FailureReason = %q, want %q", detail.FailureReason, wantDetail)
	}
	if !detail.FailureReasonAt.Equal(failedAt) {
		t.Errorf("FailureReasonAt = %s, want %s", detail.FailureReasonAt, failedAt)
	}
}

// TestGetArtifactDetail_QuarantinedArtifactCarriesTheJournalsOwnReason
// covers the other of the two exceptional states the issue names: a hash
// MISMATCH (a positive finding about content, distinct from FAILED's
// "could not check at all") routes to QUARANTINED, and this must carry
// its own detail text the same way.
func TestGetArtifactDetail_QuarantinedArtifactCarriesTheJournalsOwnReason(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	tr := newFakeTransport()
	tr.put("backup.dump", "payload", epoch.Unix())

	journal := openJournal(t)
	ctx := context.Background()
	rec := discoverOneRecord(t, ctx, journal, tr, transport.Source{}, bs)

	wantDetail := "sha256 mismatch: local file hashes to aaaa, remote reports bbbb"
	quarantinedAt := epoch.Add(time.Hour)
	for _, transition := range []state.Transition{
		{Artifact: rec.Artifact, Key: "t1", From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: epoch},
		{Artifact: rec.Artifact, Key: "t2", From: "TRANSFERRING", To: "TRANSFERRED", OccurredAt: epoch},
		{Artifact: rec.Artifact, Key: "t3", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: epoch},
		{Artifact: rec.Artifact, Key: "t4", From: "VERIFYING", To: "QUARANTINED", OccurredAt: quarantinedAt, Detail: wantDetail},
	} {
		if _, err := journal.RecordTransition(ctx, transition); err != nil {
			t.Fatalf("-> %s: %v", transition.To, err)
		}
	}

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	detail, err := svc.GetArtifactDetail(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("GetArtifactDetail: %v", err)
	}
	if detail.FailureReason != wantDetail {
		t.Errorf("FailureReason = %q, want %q", detail.FailureReason, wantDetail)
	}
}

// TestGetArtifactDetail_HealthyArtifactHasNoFailureReason is the negative
// control: an artifact that never failed must not manufacture a reason
// out of whatever it last recorded.
func TestGetArtifactDetail_HealthyArtifactHasNoFailureReason(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	tr := newFakeTransport()
	tr.put("backup.dump", "payload", epoch.Unix())

	journal := openJournal(t)
	ctx := context.Background()
	rec := discoverOneRecord(t, ctx, journal, tr, transport.Source{}, bs)

	for _, transition := range []state.Transition{
		{Artifact: rec.Artifact, Key: "t1", From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: epoch},
		{Artifact: rec.Artifact, Key: "t2", From: "TRANSFERRING", To: "TRANSFERRED", OccurredAt: epoch},
		{Artifact: rec.Artifact, Key: "t3", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: epoch},
		{Artifact: rec.Artifact, Key: "t4", From: "VERIFYING", To: "VERIFIED", OccurredAt: epoch, Detail: "transfer and configured checks passed"},
	} {
		if _, err := journal.RecordTransition(ctx, transition); err != nil {
			t.Fatalf("-> %s: %v", transition.To, err)
		}
	}

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	detail, err := svc.GetArtifactDetail(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("GetArtifactDetail: %v", err)
	}
	if detail.FailureReason != "" {
		t.Errorf("FailureReason = %q for a VERIFIED artifact, want empty: a healthy artifact must not surface an unrelated detail as if it explained a failure", detail.FailureReason)
	}
}
