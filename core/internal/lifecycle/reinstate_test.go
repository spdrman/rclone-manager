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

// remoteDeletePendingQuarantinedFixture builds a journal row quarantined
// out of REMOTE_DELETE_PENDING rather than COMMITTED, the other origin
// quarantineOrigins conservatively reinstates one step further back (see
// machine.go's "Reinstatement" section for why there is deliberately no
// QUARANTINED -> REMOTE_DELETE_PENDING edge for it to return to instead).
func remoteDeletePendingQuarantinedFixture(t *testing.T) (*state.Journal, model.ArtifactID) {
	t.Helper()
	j := openTestJournal(t)
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: localSum, HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10},
		&state.HashUpdate{Alg: "sha256", Hash: localSum},
		RemoteDeletePending)

	ctx := context.Background()
	if _, err := Advance(ctx, Deps{Journal: j}, state.Transition{
		Artifact: artifact, Key: "fixture-quarantine",
		From: string(RemoteDeletePending), To: string(Quarantined),
		Detail: "reconciliation found the durable local copy invalid while the remote object was still present",
	}); err != nil {
		t.Fatalf("fixture: -> QUARANTINED: %v", err)
	}
	return j, artifact
}

// remoteRetainedQuarantinedFixture builds a journal row that walked the
// pipeline to COMMITTED, was retained under issue #282's read-only path
// (COMMITTED -> REMOTE_RETAINED), and was then quarantined by a later
// content check finding the durable local copy invalid: issue #315's own
// entry point, REMOTE_RETAINED -> QUARANTINED.
func remoteRetainedQuarantinedFixture(t *testing.T) (*state.Journal, model.ArtifactID) {
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

	ctx := context.Background()
	if _, err := Advance(ctx, Deps{Journal: j}, state.Transition{
		Artifact: artifact, Key: "fixture-retain",
		From: string(Committed), To: string(RemoteRetained),
		Detail: "issue #282: this backup set is declared read-only",
	}); err != nil {
		t.Fatalf("fixture: -> REMOTE_RETAINED: %v", err)
	}
	if _, err := Advance(ctx, Deps{Journal: j}, state.Transition{
		Artifact: artifact, Key: "fixture-quarantine",
		From: string(RemoteRetained), To: string(Quarantined),
		Detail: "issue #315: reconciliation found the durable local copy of a retained artifact invalid",
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

	// Only a validator that ran and passed may write a validator verdict.
	// This reinstatement was carried by a hash comparison, which says
	// nothing about whether the artifact restores, so the record's
	// validation column has to be left exactly as it was rather than
	// quietly promoted to "passed".
	if out.Record.ValidationPassed != nil {
		t.Errorf("validation_passed = %v after a hash-carried reinstatement, want it left unset: a hash match is not a validator verdict", *out.Record.ValidationPassed)
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

// A mixed verdict is a failing verdict. This is the shape that would slip
// through a rule written only in positives: the backup set has a validator,
// it ran and passed, and the recorded hash no longer matches. The hook then
// exercised a file that is demonstrably not the file this manager verified,
// and "the validator passed" must not carry the reinstatement on its own.
func TestReinstateRefusesWhenAnythingThatRanFailed(t *testing.T) {
	ctx := context.Background()
	j, artifact := quarantinedFixture(t)

	before := transitionCount(t, j, artifact)

	_, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "reinstate-mixed",
		Evidence: ReinstatementEvidence{
			ValidatorPassed: true,
			AnyCheckFailed:  true,
			Summary:         "local final file now hashes to abc, but the sha256 hash recorded at verification was def; restore-test hook passed",
		},
	})
	if err == nil {
		t.Fatal("ReinstateFromQuarantine accepted a verdict in which a check that ran had failed")
	}
	if _, ok := AsInsufficientEvidence(err); !ok {
		t.Fatalf("err = %v, want an *InsufficientEvidenceError", err)
	}
	if after := transitionCount(t, j, artifact); after != before {
		t.Fatalf("state_transitions grew from %d to %d rows on a refused reinstatement", before, after)
	}

	// Positive control: the identical evidence with nothing failing.
	if _, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "reinstate-clean",
		Evidence: ReinstatementEvidence{
			ValidatorPassed: true,
			Summary:         "restore-test hook passed",
		},
	}); err != nil {
		t.Fatalf("positive control: the same evidence without a failure was refused: %v", err)
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

// --- issue #315: origin-aware reinstatement ---

// The decisive proof for issue #315. Before this, QUARANTINED reinstated
// to exactly one fixed state, COMMITTED, no matter which state an artifact
// was quarantined FROM. That collides with REMOTE_RETAINED: an artifact
// quarantined out of it must come back to REMOTE_RETAINED, or it silently
// re-enters the ordinary FR-15 delete-eligible pipeline the moment its
// backup set's ReadOnly flag is ever unset, defeating the entire point of
// issue #282's read-only guarantee. This is exactly the collision issue
// #315's "Why it wasn't just fixed in #314" section names.
func TestReinstateFromQuarantineReturnsARemoteRetainedOriginArtifactToRemoteRetained(t *testing.T) {
	ctx := context.Background()
	j, artifact := remoteRetainedQuarantinedFixture(t)

	out, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "reinstate-retained",
		Evidence:   conclusiveEvidence(),
		Note:       "confirmed the local copy is intact",
	})
	if err != nil {
		t.Fatalf("ReinstateFromQuarantine: %v", err)
	}
	if out.Record.State != string(RemoteRetained) {
		t.Fatalf("state after reinstatement = %q, want %q: a REMOTE_RETAINED-origin quarantine must never resolve to COMMITTED", out.Record.State, RemoteRetained)
	}

	at, ok, err := j.LastTransition(ctx, artifact, string(Quarantined), string(RemoteRetained))
	if err != nil || !ok {
		t.Fatalf("LastTransition(QUARANTINED -> REMOTE_RETAINED) = (ok %v, err %v), want (true, nil)", ok, err)
	}
	if at.IsZero() {
		t.Fatal("the reinstatement was recorded with a zero timestamp")
	}

	// FR-24's reporting has to see this lineage too, the same guarantee
	// health.go's package doc already makes for the pre-existing
	// QUARANTINED -> COMMITTED edge: both readers derive from the same
	// reinstatementEdges, so a new exit is covered by construction rather
	// than by remembering to update a second list.
	reinstated, err := ReinstatedArtifacts(ctx, j, artifact.Set)
	if err != nil {
		t.Fatalf("ReinstatedArtifacts: %v", err)
	}
	found := false
	for _, id := range reinstated {
		if id == artifact {
			found = true
		}
	}
	if !found {
		t.Errorf("ReinstatedArtifacts(%s) = %v, want it to include %s", artifact.Set, reinstated, artifact)
	}
}

// Positive control / regression for the pre-#315 lineage: an ordinary
// COMMITTED-origin quarantine must still land back at COMMITTED, exactly
// as it always has, unaffected by REMOTE_RETAINED gaining its own
// reinstatement exit.
func TestReinstateFromQuarantineCommittedOriginStillReturnsCommitted(t *testing.T) {
	ctx := context.Background()
	j, artifact := quarantinedFixture(t)

	out, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "reinstate-committed",
		Evidence:   conclusiveEvidence(),
	})
	if err != nil {
		t.Fatalf("ReinstateFromQuarantine: %v", err)
	}
	if out.Record.State != string(Committed) {
		t.Fatalf("state after reinstatement = %q, want %q", out.Record.State, Committed)
	}
}

// REMOTE_DELETE_PENDING's origin reinstates one step further back, to
// COMMITTED, not to itself: machine.go deliberately declares no
// QUARANTINED -> REMOTE_DELETE_PENDING edge (COMMITTED must remain the
// only predecessor of REMOTE_DELETE_PENDING). This proves
// reinstatementTargetForArtifact's per-origin table still gets that right
// now that it also has to disambiguate REMOTE_RETAINED.
func TestReinstateFromQuarantineRemoteDeletePendingOriginReturnsToCommitted(t *testing.T) {
	ctx := context.Background()
	j, artifact := remoteDeletePendingQuarantinedFixture(t)

	out, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "reinstate-rdp-origin",
		Evidence:   conclusiveEvidence(),
	})
	if err != nil {
		t.Fatalf("ReinstateFromQuarantine: %v", err)
	}
	if out.Record.State != string(Committed) {
		t.Fatalf("state after reinstatement = %q, want %q: a REMOTE_DELETE_PENDING-origin quarantine reinstates one step further back, never to itself", out.Record.State, Committed)
	}
}

// quarantineOrigins (quarantine.go) and machine.go's Transitions table are
// two independently-maintained lists, exactly the shape of drift this
// project's own convention (see reinstatementEdges' doc) always pins with
// a test rather than trusting by inspection. Every predecessor of
// QUARANTINED that is a durable restore point must have an entry here, and
// every entry here must name an edge machine.go actually declares.
func TestQuarantineOriginsCoverEveryDurablePredecessorOfQuarantined(t *testing.T) {
	origins := make(map[State]State, len(quarantineOrigins))
	for _, o := range quarantineOrigins {
		if _, dup := origins[o.From]; dup {
			t.Errorf("quarantineOrigins names %s more than once", o.From)
		}
		origins[o.From] = o.Target
	}

	for _, from := range Predecessors(Quarantined) {
		if !IsDurableRestorePoint(from) {
			continue // Verifying, Failed: never held a durable local copy.
		}
		if _, ok := origins[from]; !ok {
			t.Errorf("%s precedes QUARANTINED and is a durable restore point, but quarantineOrigins has no entry for it", from)
		}
	}

	for from := range origins {
		declared := false
		for _, p := range Predecessors(Quarantined) {
			if p == from {
				declared = true
				break
			}
		}
		if !declared {
			t.Errorf("quarantineOrigins names %s as an origin of QUARANTINED, but machine.go's Transitions table declares no such edge", from)
		}
	}
}
