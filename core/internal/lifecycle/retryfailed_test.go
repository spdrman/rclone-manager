package lifecycle

// Issue #419's other half. FAILED declares two exits in the Transitions
// table and nothing in this product has ever taken either, so an artifact
// that reached it stopped being worked on permanently. These tests pin the
// first of the two: the operator-triggered FAILED -> DISCOVERED edge.

import (
	"context"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// failedFixture walks an artifact to FAILED through the real edges, from
// whichever state the caller names, so each test stands its artifact
// somewhere the pipeline can actually put it rather than somewhere only a
// test can.
func failedFixture(t *testing.T, j *state.Journal, artifact model.ArtifactID, from State, detail string) {
	t.Helper()
	ctx := context.Background()
	localPath, _ := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath, state.RemoteIdentity{Size: &size}, localPath,
		&state.TransferResult{BytesTransferred: 10}, nil, from)

	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: artifact.String() + ":fixture-failed",
		From: string(from), To: string(Failed), OccurredAt: time.Now(), Detail: detail,
	}); err != nil {
		t.Fatalf("failedFixture: %s -> FAILED: %v", from, err)
	}
}

// TestRetryFailed_MovesBackToDiscoveredAndCountsTheAttempt is the happy
// path, and the counter is the half that is easy to leave out.
//
// Without it a retried artifact is indistinguishable from one arriving for
// the first time, so an artifact that fails, is retried, and fails again
// looks like two unrelated first attempts. That is precisely the "silently
// retried into oblivion" shape Phase 4 asks this product to make visible,
// and RetryCount on the row is where it becomes visible.
func TestRetryFailed_MovesBackToDiscoveredAndCountsTheAttempt(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	artifact := mustID(t)
	failedFixture(t, j, artifact, Transferring, "lifecycle: transfer: copy failed: transient: connection reset")

	out, err := RetryFailed(ctx, Deps{Journal: j}, RetryFailedParams{
		Artifact:       artifact,
		AttemptKey:     "retry-1",
		RecoveringFrom: "lifecycle: transfer: copy failed: transient: connection reset",
		Note:           "the NAS came back",
	})
	if err != nil {
		t.Fatalf("RetryFailed: %v", err)
	}
	if out.Record.State != string(Discovered) {
		t.Fatalf("state = %q, want %q", out.Record.State, Discovered)
	}
	// The same counter a quarantine release and a stalled verification
	// move, because it answers the same question: how many attempts has
	// this artifact already spent from an exceptional state.
	if out.Record.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1 after the first retry", out.Record.RetryCount)
	}
	if out.Record.LastError == "" {
		t.Fatal("LastError is empty, want the failure this attempt is recovering FROM to be recorded")
	}
}

// TestRetryFailed_IsOfferedFromEveryStateThatCanReachFailed is the
// eligibility answer, and it is deliberately uniform.
//
// The safety argument is structural rather than case by case, and
// machine.go already makes it: FAILED is reachable only before COMMITTED,
// so the remote delete has never been issued and the source is
// presumptively still there. This edge touches no remote and removes no
// durable copy; the only thing the transfer step deletes on the way past
// is a .partial. So there is no lineage this has to refuse, and inventing
// one would be refusing an operator a recovery for a reason nobody can
// state.
func TestRetryFailed_IsOfferedFromEveryStateThatCanReachFailed(t *testing.T) {
	for _, from := range Predecessors(Failed) {
		t.Run(string(from), func(t *testing.T) {
			ctx := context.Background()
			j := openTestJournal(t)
			artifact := mustID(t)
			failedFixture(t, j, artifact, from, "fixture: failed out of "+string(from))

			out, err := RetryFailed(ctx, Deps{Journal: j}, RetryFailedParams{
				Artifact: artifact, AttemptKey: "retry-1",
			})
			if err != nil {
				t.Fatalf("RetryFailed from %s: %v", from, err)
			}
			if out.Record.State != string(Discovered) {
				t.Fatalf("state = %q, want %q", out.Record.State, Discovered)
			}
		})
	}
}

// TestRetryFailed_HasEveryStateFailedCanBeReachedFrom is the control for
// the loop above: driven off the real table rather than a list typed here,
// so a new way into FAILED is covered the moment it is declared, and a
// walk that visited nothing fails instead of passing quietly.
func TestRetryFailed_HasEveryStateFailedCanBeReachedFrom(t *testing.T) {
	predecessors := Predecessors(Failed)
	if len(predecessors) < 5 {
		t.Fatalf("Predecessors(FAILED) = %v, which is too few for the loop above to be checking anything", predecessors)
	}
	for _, from := range predecessors {
		if from == Failed {
			t.Fatalf("Predecessors(FAILED) names FAILED itself, so the fixture above cannot stand an artifact where it says")
		}
	}
}

// TestRetryFailed_RefusesAnArtifactThatIsNotFailed walks every state an
// artifact could be sitting in when somebody clicks retry on a stale screen.
//
// The two quarantine states are the important entries. Both are exceptional
// states with their own operator exits and their own evidence rules, and a
// retry that quietly worked on them would let an operator bypass those rules
// by pressing the wrong button. The states before COMMITTED matter for a
// different reason: an artifact quietly making progress must not be thrown
// back to the start because a page was loaded a minute ago.
func TestRetryFailed_RefusesAnArtifactThatIsNotFailed(t *testing.T) {
	for _, at := range []State{Discovered, Transferring, Transferred, Verifying, Verified, Committing, Committed, Quarantined, QuarantinedLost} {
		t.Run(string(at), func(t *testing.T) {
			ctx := context.Background()
			j := openTestJournal(t)
			artifact := mustID(t)

			switch at {
			case Quarantined:
				quarantineFixture(t, j, artifact, true)
			case QuarantinedLost:
				quarantineLostFixture(t, j, artifact)
			default:
				localPath, _ := writeLocalFile(t, 10)
				size := int64(10)
				discoverAndAdvance(t, j, artifact, testRemotePath, state.RemoteIdentity{Size: &size}, localPath,
					&state.TransferResult{BytesTransferred: 10}, nil, at)
			}

			_, err := RetryFailed(ctx, Deps{Journal: j}, RetryFailedParams{Artifact: artifact, AttemptKey: "retry-1"})
			if err == nil {
				t.Fatalf("RetryFailed succeeded on an artifact at %s", at)
			}
			notFailed, ok := AsNotFailed(err)
			if !ok {
				t.Fatalf("error is not a *NotFailedError: %v", err)
			}
			if notFailed.Current != at {
				t.Fatalf("the refusal says the artifact is at %s, want %s", notFailed.Current, at)
			}

			rec, getErr := j.Get(ctx, artifact)
			if getErr != nil {
				t.Fatalf("Get: %v", getErr)
			}
			if rec.State != string(at) {
				t.Fatalf("journal state changed to %q, want it left at %q", rec.State, at)
			}
		})
	}
}

// TestRetryFailed_ASecondClickIsRefusedRatherThanSpendingASecondAttempt
// pins the ordering, which matters more here than raw idempotency does.
//
// The state guard runs before the write, exactly as ReleaseFromQuarantine's
// does, so a second call against an artifact this one already moved is
// refused BY NAME rather than replayed. That is the honest answer to an
// operator clicking twice, or to a stale screen: the artifact really is
// not FAILED any more. What must not happen either way is a second attempt
// coming off the artifact's budget for one decision, and that is what this
// asserts.
func TestRetryFailed_ASecondClickIsRefusedRatherThanSpendingASecondAttempt(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	artifact := mustID(t)
	failedFixture(t, j, artifact, Transferring, "fixture")

	params := RetryFailedParams{Artifact: artifact, AttemptKey: "retry-1"}
	if _, err := RetryFailed(ctx, Deps{Journal: j}, params); err != nil {
		t.Fatalf("first RetryFailed: %v", err)
	}

	_, err := RetryFailed(ctx, Deps{Journal: j}, params)
	if err == nil {
		t.Fatal("a second retry of an artifact already sent back to DISCOVERED succeeded")
	}
	notFailed, ok := AsNotFailed(err)
	if !ok {
		t.Fatalf("the second call did not refuse by name: %v", err)
	}
	if notFailed.Current != Discovered {
		t.Fatalf("the refusal says the artifact is at %s, want %s", notFailed.Current, Discovered)
	}

	rec, getErr := j.Get(ctx, artifact)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if rec.RetryCount != 1 {
		t.Fatalf("RetryCount = %d after two clicks on one decision, want 1", rec.RetryCount)
	}
}

// TestRetryFailed_RejectsMissingRequiredParams covers the two preconditions
// that cannot be defaulted.
//
// A missing AttemptKey is the one worth having. The key is what makes a
// repeated call converge instead of counting a second attempt, so accepting
// an empty one would mean every double click on the operator's button was
// recorded as another failure of the artifact.
func TestRetryFailed_RejectsMissingRequiredParams(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	artifact := mustID(t)
	failedFixture(t, j, artifact, Transferring, "fixture")

	if _, err := RetryFailed(ctx, Deps{}, RetryFailedParams{Artifact: artifact, AttemptKey: "k"}); err == nil {
		t.Error("RetryFailed with no Journal succeeded")
	}
	if _, err := RetryFailed(ctx, Deps{Journal: j}, RetryFailedParams{Artifact: artifact}); err == nil {
		t.Error("RetryFailed with no AttemptKey succeeded")
	}
}
