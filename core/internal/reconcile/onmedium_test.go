package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file is issue #434's pin for this package: an artifact whose
// durable copy is on a storage medium is not an artifact whose local copy
// is invalid, and reconciliation must not read the one as the other.
//
// Every fixture in reconcile_test.go answers ReadableLocalPath out of the
// LocalPath fallback, because driveTo writes no placement. That is a real
// Phase 1 shape and worth keeping, but it means nothing in this package
// ever handed checkLocalFinal a placement before this file, which is how
// the sweep's "must not read as a missing local file" comment sat above
// four lines that did exactly that.

const onMediumTestMedium = "cold_offsite"

// moveToMedium turns an artifact that already has a durable copy into the
// shape a completed move leaves (#238): its local placement GONE, and its
// only ACTIVE placement a content-verified copy on a medium. The local
// file is not on disk either, because a completed move deleted it.
//
// It writes the placements straight through the journal, as
// internal/revalidate/medium_test.go's helper of the same name does: the
// move engine is the only production writer and a reconcile test that
// waited for one would be testing the engine.
func moveToMedium(t *testing.T, j *state.Journal, artifact model.ArtifactID, st lifecycle.State, localPath string, size int64) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC()
	sum := sha256.Sum256([]byte("the bytes reconcile never reads"))
	hash := hex.EncodeToString(sum[:])

	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: artifact.String() + ":gone-local", From: string(st), To: string(st), OccurredAt: at,
		Placement: &state.PlacementUpdate{Medium: state.MediumLocal, Location: localPath, Status: state.PlacementGone},
	}); err != nil {
		t.Fatalf("retiring the local placement: %v", err)
	}
	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: artifact.String() + ":on-medium", From: string(st), To: string(st), OccurredAt: at,
		Placement: &state.PlacementUpdate{
			Medium: onMediumTestMedium, Location: "rclone-manager/production/postgres-primary/" + artifact.Name,
			Size: &size, Hash: hash, HashAlg: "sha256",
			VerificationClass: state.VerificationContent, Status: state.PlacementActive,
		},
	}); err != nil {
		t.Fatalf("recording the medium placement: %v", err)
	}
}

// TestReconcile_ArtifactOnAMedium_IsLeftAloneInEveryState is the contract
// itself, across every state that asks checkLocalFinal.
//
// COMPLETE is the one that matters in production, since FR-30 lets only
// COMPLETE artifacts move, and it is the one the second-cycle test in
// internal/app drives end to end. The other three are here because they
// share the check, and a handler that shares a check and not its meaning
// is how the next instance of this bug gets written.
//
// The fake transport has no Stat configured at all, so a handler that
// consults the remote before it has decided the copy is elsewhere turns
// into an ArtifactError here rather than a quiet pass.
func TestReconcile_ArtifactOnAMedium_IsLeftAloneInEveryState(t *testing.T) {
	for _, st := range []lifecycle.State{
		lifecycle.Complete, lifecycle.Committed, lifecycle.RemoteDeletePending, lifecycle.RemoteRetained,
	} {
		t.Run(string(st), func(t *testing.T) {
			j := openTestJournal(t)
			artifact := testArtifact(t, "moved.dump")
			size := int64(32)
			localPath := filepath.Join(t.TempDir(), "moved.final") // never written: the move deleted it
			driveTo(t, j, driveParams{
				artifact: artifact, remote: state.RemoteIdentity{Size: &size}, localPath: localPath,
				transfer: &state.TransferResult{BytesTransferred: size}, stopAt: st,
			})
			moveToMedium(t, j, artifact, st, localPath, size)

			tp := &fakeTransport{}
			report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			requireNoErrors(t, report)
			f := requireOneFinding(t, report)
			if f.Changed() {
				t.Fatalf("Finding %s -> %s (%s); an artifact whose durable copy is on %q is not a lost artifact",
					f.From, f.To, f.Reason, onMediumTestMedium)
			}
			if !strings.Contains(f.Reason, onMediumTestMedium) {
				t.Errorf("Reason = %q, want it to name the medium the copy is on", f.Reason)
			}
			if strings.Contains(f.Reason, "no local final path") {
				t.Errorf("Reason = %q reads the moved artifact as a missing local file", f.Reason)
			}
			if tp.statCalls != 0 {
				t.Errorf("statCalls = %d, want 0: nothing about the remote changes where the durable copy is", tp.statCalls)
			}
			assertJournalState(t, j, artifact, st)
		})
	}
}

// TestReconcile_Complete_EveryCopyGone_QuarantinesAsLost is the control
// that keeps the test above honest. The fix is not "an artifact with
// placements is left alone": an artifact whose every placement is GONE has
// no copy anywhere, which is exactly what #238 says COMPLETE ->
// QUARANTINED_LOST is for.
func TestReconcile_Complete_EveryCopyGone_QuarantinesAsLost(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "lost.dump")
	size := int64(32)
	localPath := filepath.Join(t.TempDir(), "lost.final")
	driveTo(t, j, driveParams{
		artifact: artifact, remote: state.RemoteIdentity{Size: &size}, localPath: localPath,
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.Complete,
	})
	moveToMedium(t, j, artifact, lifecycle.Complete, localPath, size)
	if _, err := j.RecordTransition(context.Background(), state.Transition{
		Artifact: artifact, Key: artifact.String() + ":gone-medium", From: "COMPLETE", To: "COMPLETE", OccurredAt: time.Now().UTC(),
		Placement: &state.PlacementUpdate{
			Medium: onMediumTestMedium, Location: "rclone-manager/production/postgres-primary/" + artifact.Name,
			Status: state.PlacementGone,
		},
	}); err != nil {
		t.Fatalf("retiring the medium placement: %v", err)
	}

	tp := &fakeTransport{}
	report, err := Reconcile(context.Background(), Deps{Journal: j, Transport: tp}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireNoErrors(t, report)
	f := requireOneFinding(t, report)
	if f.To != lifecycle.QuarantinedLost {
		t.Fatalf("To = %s (%s), want QUARANTINED_LOST: every recorded copy of this artifact is GONE", f.To, f.Reason)
	}
	if !strings.Contains(f.Reason, "no ACTIVE copy") {
		t.Errorf("Reason = %q, want it to say no ACTIVE copy is recorded anywhere rather than describe a missing local path", f.Reason)
	}
	assertJournalState(t, j, artifact, lifecycle.QuarantinedLost)
}

// TestReconcile_ArtifactMidMove_IsStillCheckedLocally pins the ordering:
// while the local placement is ACTIVE the local copy is what gets checked,
// exactly as before, even though a medium copy exists too. The moved
// shape is "no ACTIVE local", not "any medium placement".
func TestReconcile_ArtifactMidMove_IsStillCheckedLocally(t *testing.T) {
	j := openTestJournal(t)
	artifact := testArtifact(t, "mid-move.dump")
	size := int64(32)
	localPath := writeLocalFile(t, size)
	driveTo(t, j, driveParams{
		artifact: artifact, remote: state.RemoteIdentity{Size: &size}, localPath: localPath,
		transfer: &state.TransferResult{BytesTransferred: size}, stopAt: lifecycle.Complete,
	})
	ctx := context.Background()
	at := time.Now().UTC()
	for _, p := range []state.PlacementUpdate{
		{Medium: state.MediumLocal, Location: localPath, Size: &size, Status: state.PlacementActive},
		{Medium: onMediumTestMedium, Location: "rclone-manager/production/postgres-primary/" + artifact.Name, Size: &size,
			VerificationClass: state.VerificationContent, Status: state.PlacementActive},
	} {
		p := p
		if _, err := j.RecordTransition(ctx, state.Transition{
			Artifact: artifact, Key: artifact.String() + ":placement:" + p.Medium, From: "COMPLETE", To: "COMPLETE", OccurredAt: at,
			Placement: &p,
		}); err != nil {
			t.Fatalf("recording the %s placement: %v", p.Medium, err)
		}
	}

	report, err := Reconcile(ctx, Deps{Journal: j, Transport: &fakeTransport{}}, testSource, testSet(t))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireNoErrors(t, report)
	f := requireOneFinding(t, report)
	if f.Changed() {
		t.Fatalf("Finding %s -> %s (%s) for an artifact whose local copy is ACTIVE and valid", f.From, f.To, f.Reason)
	}
	if !strings.Contains(f.Reason, "local copy verified valid") {
		t.Errorf("Reason = %q, want the local copy to have been the one checked while its placement is ACTIVE", f.Reason)
	}
}
