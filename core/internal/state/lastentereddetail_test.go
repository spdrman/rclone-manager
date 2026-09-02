package state

import (
	"context"
	"testing"
	"time"
)

// LastEnteredDetail is issue #284's read path: the only durable place a
// lifecycle step's diagnostic sentence lands is state_transitions.detail,
// and until this existed nothing in the codebase read it back for a
// specific artifact and a specific state. This mirrors
// TestLastTransitionReportsOnlyTheExactEdge's shape (lasttransition_test.go)
// but asserts the free-text column that query deliberately does not
// return.
func TestLastEnteredDetailReportsTheTextOfTheEnteringTransition(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	artifact := testArtifact(t)

	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if _, err := j.Discover(ctx, artifact, "discover-1", "/incoming/backup.dump", RemoteIdentity{}, t0); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// Nothing has entered FAILED yet: "never", not a zero-value detail
	// mistaken for a real (empty) answer.
	if detail, _, found, err := j.LastEnteredDetail(ctx, artifact, "FAILED"); err != nil || found || detail != "" {
		t.Fatalf("LastEnteredDetail(FAILED) before anything = (detail %q, found %v, err %v), want (\"\", false, nil)", detail, found, err)
	}

	for _, tr := range []Transition{
		{Artifact: artifact, Key: "t1", From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: t0},
		{Artifact: artifact, Key: "t2", From: "TRANSFERRING", To: "TRANSFERRED", OccurredAt: t0},
		{Artifact: artifact, Key: "t3", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: t0},
	} {
		if _, err := j.RecordTransition(ctx, tr); err != nil {
			t.Fatalf("-> %s: %v", tr.To, err)
		}
	}

	failedAt := t0.Add(time.Hour)
	wantDetail := `hash verification required (sha256) but the backend could not supply a comparable remote hash: unsupported_capability: backend "sftp" cannot compute sha256`
	if _, err := j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "verify-fail-1",
		From: "VERIFYING", To: "FAILED", OccurredAt: failedAt,
		Detail: wantDetail,
	}); err != nil {
		t.Fatalf("-> FAILED: %v", err)
	}

	detail, at, found, err := j.LastEnteredDetail(ctx, artifact, "FAILED")
	if err != nil || !found {
		t.Fatalf("LastEnteredDetail(FAILED) after the transition = (found %v, err %v), want (true, nil)", found, err)
	}
	if detail != wantDetail {
		t.Errorf("detail = %q, want %q", detail, wantDetail)
	}
	if !at.Equal(failedAt) {
		t.Errorf("occurredAt = %s, want %s", at, failedAt)
	}

	// A same-state write (an internal/revalidate-style re-check) must not
	// be mistaken for a fresh entry into FAILED, and must not overwrite
	// the answer with its own, unrelated detail text: this mirrors
	// LastEnteredAt's own "entered" semantics exactly.
	if _, err := j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "verify-fail-2",
		From: "FAILED", To: "FAILED", OccurredAt: t0.Add(2 * time.Hour),
		Detail: "an unrelated same-state write",
	}); err != nil {
		t.Fatalf("-> FAILED (same-state): %v", err)
	}
	detail, at, found, err = j.LastEnteredDetail(ctx, artifact, "FAILED")
	if err != nil || !found {
		t.Fatalf("LastEnteredDetail(FAILED) after a same-state write = (found %v, err %v), want (true, nil)", found, err)
	}
	if detail != wantDetail || !at.Equal(failedAt) {
		t.Errorf("a same-state write moved the answer: detail=%q at=%s, want the original entering transition's own %q at %s", detail, at, wantDetail, failedAt)
	}
}

// RecordTransition's Outcome now echoes back the Detail the caller
// supplied, on both the freshly-applied path and the idempotent-replay
// path, so a caller holding only an Outcome (internal/app's pipeline,
// writing an FR-23 log line right after the call) can report it without a
// second query.
func TestRecordTransitionOutcomeEchoesDetail(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	artifact := testArtifact(t)

	if _, err := j.Discover(ctx, artifact, "discover-1", "/incoming/backup.dump", RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, setup := range []Transition{
		{Artifact: artifact, Key: "t1", From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: time.Now()},
		{Artifact: artifact, Key: "t2", From: "TRANSFERRING", To: "TRANSFERRED", OccurredAt: time.Now()},
		{Artifact: artifact, Key: "t3", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: time.Now()},
	} {
		if _, err := j.RecordTransition(ctx, setup); err != nil {
			t.Fatalf("-> %s: %v", setup.To, err)
		}
	}

	tr := Transition{
		Artifact: artifact, Key: "verify-fail-1",
		From: "VERIFYING", To: "FAILED", OccurredAt: time.Now(),
		Detail: "transfer verification: expected 4096 bytes, local file has 2048",
	}

	out, err := j.RecordTransition(ctx, tr)
	if err != nil {
		t.Fatalf("RecordTransition: %v", err)
	}
	if !out.Applied {
		t.Fatalf("Applied = false on the first call, want true")
	}
	if out.Detail != tr.Detail {
		t.Errorf("Outcome.Detail = %q, want %q", out.Detail, tr.Detail)
	}

	// A replay with the identical Key: Applied must flip to false, and
	// Detail must still come back, echoing this call's own Transition
	// rather than silently going empty just because nothing was written.
	replay, err := j.RecordTransition(ctx, tr)
	if err != nil {
		t.Fatalf("RecordTransition (replay): %v", err)
	}
	if replay.Applied {
		t.Fatalf("Applied = true on a replayed key, want false")
	}
	if replay.Detail != tr.Detail {
		t.Errorf("replayed Outcome.Detail = %q, want %q", replay.Detail, tr.Detail)
	}
}
