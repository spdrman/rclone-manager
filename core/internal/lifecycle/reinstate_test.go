package lifecycle

import (
	"context"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// --- observation channel ---
//
// Every "nothing was written" assertion in this file counts rows in the
// append-only state_transitions log rather than comparing the artifacts
// row's UpdatedAt. UpdatedAt is written by every transition there is,
// including a same-state one, and it is stamped with the caller's own
// OccurredAt: with a frozen or coarse clock two consecutive writes leave
// it byte-for-byte identical, so "UpdatedAt did not move" is compatible
// with a write having happened. Counting log rows is not.

func transitionCount(t *testing.T, j *state.Journal, artifact model.ArtifactID) int {
	t.Helper()
	events, err := j.RecentActivity(context.Background(), 1000)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.Artifact == artifact {
			n++
		}
	}
	return n
}

// quarantinedFixture builds a journal row that really walked the pipeline
// to COMMITTED and was then quarantined by a later content check, which is
// the shape issue #220 is about: a durable local copy that the manager
// stopped trusting after the fact.
func quarantinedFixture(t *testing.T) (*state.Journal, model.ArtifactID) {
	t.Helper()
	j := openTestJournal(t)
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: localSum, HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10},
		&state.HashUpdate{Alg: "sha256", Hash: localSum},
		Committed)

	if _, err := Advance(context.Background(), Deps{Journal: j}, state.Transition{
		Artifact: artifact, Key: "fixture-quarantine",
		From: string(Committed), To: string(Quarantined),
		Detail: "reconciliation found the durable local copy invalid",
	}); err != nil {
		t.Fatalf("fixture: -> QUARANTINED: %v", err)
	}
	return j, artifact
}

func conclusiveEvidence() ReinstatementEvidence {
	return ReinstatementEvidence{
		HashMatched: true,
		Summary:     "recomputed hash still matches the hash recorded at verification",
	}
}

// The whole point of the issue: an operator who can prove the durable local
// copy is intact gets the artifact back as a restore point, without a
// re-fetch from a remote that may no longer exist.
func TestReinstateFromQuarantineReturnsTheArtifactToCommitted(t *testing.T) {
	ctx := context.Background()
	j, artifact := quarantinedFixture(t)

	out, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "reinstate-1",
		Evidence:   conclusiveEvidence(),
		Note:       "replaced the failing validator binary",
	})
	if err != nil {
		t.Fatalf("ReinstateFromQuarantine: %v", err)
	}
	if !out.Applied {
		t.Fatal("Applied = false on a fresh reinstatement")
	}
	if out.Record.State != string(Committed) {
		t.Fatalf("state after reinstatement = %q, want COMMITTED", out.Record.State)
	}

	// The audit requirement: the append-only log, not the artifacts row,
	// is what tells a later reader this artifact was re-trusted rather
	// than never distrusted.
	at, ok, err := j.LastTransition(ctx, artifact, string(Quarantined), string(Committed))
	if err != nil || !ok {
		t.Fatalf("LastTransition(QUARANTINED -> COMMITTED) = (ok %v, err %v), want (true, nil)", ok, err)
	}
	if at.IsZero() {
		t.Fatal("the reinstatement was recorded with a zero timestamp")
	}

	events, err := j.RecentActivity(ctx, 1000)
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	var detail string
	for _, e := range events {
		if e.Artifact == artifact && e.From == string(Quarantined) && e.To == string(Committed) {
			detail = e.Detail
		}
	}
	if !strings.Contains(detail, "recomputed hash") {
		t.Errorf("recorded detail = %q, want it to name the evidence that carried the reinstatement", detail)
	}
	if !strings.Contains(detail, "replaced the failing validator binary") {
		t.Errorf("recorded detail = %q, want it to carry the operator's note", detail)
	}
}

// A pass that could not actually have failed is not evidence. The check
// runs unconditionally against the local file, so an artifact with no
// recorded hash baseline and no configured validator "passes" on nothing
// more than the file still existing, and that must not re-trust anything.
//
// The positive control is the same call with the same fixture and
// conclusive evidence: it proves the refusal below is about the evidence
// and not about the fixture being unreinstatable for some other reason.
func TestReinstateRefusesEvidenceThatCouldNotHaveFailed(t *testing.T) {
	ctx := context.Background()
	j, artifact := quarantinedFixture(t)

	before := transitionCount(t, j, artifact)

	_, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "reinstate-inconclusive",
		Evidence: ReinstatementEvidence{
			Summary: "local final file present, 10 bytes (no recorded hash to compare against)",
		},
	})
	if err == nil {
		t.Fatal("ReinstateFromQuarantine accepted evidence that proves only that the file exists")
	}
	if _, ok := AsInsufficientEvidence(err); !ok {
		t.Fatalf("err = %v, want an *InsufficientEvidenceError", err)
	}

	if after := transitionCount(t, j, artifact); after != before {
		t.Fatalf("state_transitions grew from %d to %d rows on a refused reinstatement; a refusal must write nothing", before, after)
	}
	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(Quarantined) {
		t.Fatalf("state = %q, want it left at QUARANTINED", rec.State)
	}

	// Positive control.
	if _, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "reinstate-conclusive",
		Evidence:   conclusiveEvidence(),
	}); err != nil {
		t.Fatalf("positive control: conclusive evidence was refused on the same fixture: %v", err)
	}
}

// An artifact quarantined out of VERIFYING never had a durable local copy
// at all: its recorded local path is still a .partial. Reinstating it to
// COMMITTED would declare a half-written file a restore point, so the log
// has to be consulted for whether the artifact ever actually held the
// state it is being returned to.
func TestReinstateRefusesAnArtifactThatNeverReachedTheTargetState(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: localSum, HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10}, nil, Verifying)

	if _, err := Advance(ctx, Deps{Journal: j}, state.Transition{
		Artifact: artifact, Key: "verify-reject",
		From: string(Verifying), To: string(Quarantined),
		Detail: "validator rejected the transferred content",
	}); err != nil {
		t.Fatalf("-> QUARANTINED: %v", err)
	}

	before := transitionCount(t, j, artifact)

	_, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "reinstate-never-committed",
		Evidence:   ReinstatementEvidence{ValidatorPassed: true, Summary: "restore-test hook passed"},
	})
	if err == nil {
		t.Fatal("ReinstateFromQuarantine promoted an artifact that never committed straight to COMMITTED")
	}
	if _, ok := AsNeverHeldTargetState(err); !ok {
		t.Fatalf("err = %v, want a *NeverHeldTargetStateError", err)
	}
	if after := transitionCount(t, j, artifact); after != before {
		t.Fatalf("state_transitions grew from %d to %d rows on a refused reinstatement", before, after)
	}

	// Positive control: the identical call, with the identical evidence,
	// against an artifact whose log does record a COMMITTED entry.
	committed, committedArtifact := quarantinedFixture(t)
	if _, err := ReinstateFromQuarantine(ctx, Deps{Journal: committed}, QuarantineReinstateParams{
		Artifact:   committedArtifact,
		AttemptKey: "reinstate-never-committed",
		Evidence:   ReinstatementEvidence{ValidatorPassed: true, Summary: "restore-test hook passed"},
	}); err != nil {
		t.Fatalf("positive control: an artifact that did reach COMMITTED was refused: %v", err)
	}
}

// The evidence has to answer the reason for the distrust. An artifact the
// application validator itself rejected is not re-trusted by proving its
// bytes are unchanged: those are the very bytes the validator refused.
func TestReinstateRefusesHashEvidenceAloneWhenTheValidatorRejectedTheArtifact(t *testing.T) {
	ctx := context.Background()
	j, artifact := quarantinedFixture(t)

	// Record the validator's own verdict on the row, the way verify.go
	// does when config.Validation.Command rejects an artifact.
	if _, err := Advance(ctx, Deps{Journal: j}, state.Transition{
		Artifact: artifact, Key: "record-validator-rejection",
		From: string(Quarantined), To: string(Quarantined),
		Validation: &state.ValidationUpdate{Passed: false, Detail: "restore-test hook exited 1"},
	}); err != nil {
		t.Fatalf("recording the validator verdict: %v", err)
	}

	before := transitionCount(t, j, artifact)

	_, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "reinstate-hash-only",
		Evidence:   conclusiveEvidence(),
	})
	if err == nil {
		t.Fatal("ReinstateFromQuarantine re-trusted a validator-rejected artifact on hash evidence alone")
	}
	if _, ok := AsInsufficientEvidence(err); !ok {
		t.Fatalf("err = %v, want an *InsufficientEvidenceError", err)
	}
	if after := transitionCount(t, j, artifact); after != before {
		t.Fatalf("state_transitions grew from %d to %d rows on a refused reinstatement", before, after)
	}

	// Positive control: the same fixture, the same rejected validation
	// verdict on the row, but this time the validator itself ran and
	// passed.
	out, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "reinstate-validator-passed",
		Evidence: ReinstatementEvidence{
			HashMatched:     true,
			ValidatorPassed: true,
			Summary:         "recomputed hash still matches; restore-test hook passed",
		},
	})
	if err != nil {
		t.Fatalf("positive control: a re-run validator that passed was still refused: %v", err)
	}
	if out.Record.State != string(Committed) {
		t.Fatalf("state = %q, want COMMITTED", out.Record.State)
	}
	// A validator that ran and passed replaces the stale rejection on the
	// row; leaving validation_passed = false on an artifact this manager
	// now presents as a restore point is exactly the kind of drift the
	// audit requirement exists to prevent.
	if out.Record.ValidationPassed == nil || !*out.Record.ValidationPassed {
		t.Errorf("validation_passed = %v, want true once the validator itself re-passed", out.Record.ValidationPassed)
	}
}

// QUARANTINED_LOST is reached only from COMPLETE, where the remote source
// is already confirmed gone. A local copy found invalid there used to be
// an unconditional, permanent write-off, including when the finding was an
// unmounted volume rather than a bad file. Reinstatement returns it to
// COMPLETE, which is the state it actually held, and never to anything
// that could re-enter the pipeline.
func TestReinstateReturnsAQuarantinedLostArtifactToComplete(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: localSum, HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10},
		&state.HashUpdate{Alg: "sha256", Hash: localSum},
		RemoteDeletePending)

	for _, tr := range []state.Transition{
		{Artifact: artifact, Key: "to-complete", From: string(RemoteDeletePending), To: string(Complete)},
		{Artifact: artifact, Key: "to-lost", From: string(Complete), To: string(QuarantinedLost),
			Detail: "reconciliation found the durable local copy invalid"},
	} {
		if _, err := Advance(ctx, Deps{Journal: j}, tr); err != nil {
			t.Fatalf("fixture -> %s: %v", tr.To, err)
		}
	}

	out, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "reinstate-lost",
		Evidence:   conclusiveEvidence(),
		Note:       "the backup volume was not mounted when the check ran",
	})
	if err != nil {
		t.Fatalf("ReinstateFromQuarantine: %v", err)
	}
	if out.Record.State != string(Complete) {
		t.Fatalf("state = %q, want COMPLETE: a QUARANTINED_LOST artifact is returned to the state it held, never to one that re-enters the pipeline", out.Record.State)
	}
	if _, ok, err := j.LastTransition(ctx, artifact, string(QuarantinedLost), string(Complete)); err != nil || !ok {
		t.Fatalf("LastTransition(QUARANTINED_LOST -> COMPLETE) = (ok %v, err %v), want (true, nil)", ok, err)
	}
}

// Anything that is not quarantined is refused by name, exactly the way
// ReleaseFromQuarantine already refuses one, so a caller on a stale screen
// gets a typed answer rather than a mutation.
func TestReinstateRefusesAnArtifactThatIsNotQuarantined(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: localSum, HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10},
		&state.HashUpdate{Alg: "sha256", Hash: localSum},
		Committed)

	before := transitionCount(t, j, artifact)

	_, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "reinstate-healthy",
		Evidence:   conclusiveEvidence(),
	})
	if err == nil {
		t.Fatal("ReinstateFromQuarantine accepted an artifact that is not quarantined")
	}
	if _, ok := AsNotQuarantined(err); !ok {
		t.Fatalf("err = %v, want a *NotQuarantinedError", err)
	}
	if after := transitionCount(t, j, artifact); after != before {
		t.Fatalf("state_transitions grew from %d to %d rows on a refused reinstatement", before, after)
	}
}

// A reinstatement is a one-shot operator decision, not a step the pipeline
// retries, so it deliberately does not converge on a replay the way Commit
// does: a second attempt finds an artifact that is no longer quarantined
// and is refused by name, exactly like a second Retry-ingestion click on a
// stale screen. What matters for safety is that the refusal writes
// nothing, so a double click cannot record two reinstatements.
func TestASecondReinstatementIsRefusedAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	j, artifact := quarantinedFixture(t)

	p := QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "reinstate-once",
		Evidence:   conclusiveEvidence(),
	}
	first, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, p)
	if err != nil {
		t.Fatalf("first reinstatement: %v", err)
	}
	if !first.Applied {
		t.Fatal("first reinstatement: Applied = false")
	}
	after := transitionCount(t, j, artifact)

	if _, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, p); err == nil {
		t.Fatal("a second reinstatement of an already-reinstated artifact succeeded")
	} else if _, ok := AsNotQuarantined(err); !ok {
		t.Fatalf("err = %v, want a *NotQuarantinedError", err)
	}
	if again := transitionCount(t, j, artifact); again != after {
		t.Fatalf("state_transitions grew from %d to %d rows on a second attempt", after, again)
	}
}
