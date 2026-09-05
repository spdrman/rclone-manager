// These cover QUARANTINED's operator exits and the reason string an
// operator is shown when they get there.
//
// The file is organised around one distinction: a quarantine is either
// recoverable or it is not, and that depends entirely on whether a source
// still exists to re-fetch from. QUARANTINED means one does, so its exit
// throws the local copy away and starts again. QUARANTINED_LOST means the
// remote was already confirmed gone before the corruption was found, so the
// same exit would ask the pipeline to re-fetch from nothing, fail, land in
// FAILED, and be sent round again: a livelock that also mislabels an
// irrecoverable loss as an ordinary retry. Every refusal test here is
// defending one side of that line.
//
// QuarantineReason gets its own tests because it is a message rather than a
// mechanism, and it is the message an operator reads before deciding which
// exit to take. It has to say what was actually measured, which is why the
// fixture stages the two shapes, a validator's rejection and a content
// check, separately.
package lifecycle

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// quarantineFixture drives artifact through the real nominal sequence up to
// VERIFYING (discoverAndAdvance, remotedelete_test.go's own helper), then
// records one more transition into QUARANTINED carrying either a
// ValidationUpdate (the application-validator-rejection shape verify.go's
// decide() produces) or a HashUpdate (everything else this pipeline can
// quarantine for today: a hash mismatch, or FR-17/Phase-4 finding the
// durable copy invalid after the fact). This is what QuarantineReason has
// to tell apart.
func quarantineFixture(t *testing.T, j *state.Journal, artifact model.ArtifactID, withValidation bool) {
	t.Helper()
	ctx := context.Background()
	localPath, _ := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath, state.RemoteIdentity{Size: &size}, localPath,
		&state.TransferResult{BytesTransferred: 10}, nil, Verifying)

	tr := state.Transition{
		Artifact: artifact, Key: artifact.String() + ":fixture-quarantine",
		From: string(Verifying), To: string(Quarantined),
		OccurredAt: time.Now(), Detail: "fixture: quarantined for testing",
	}
	if withValidation {
		tr.Validation = &state.ValidationUpdate{Passed: false, Detail: "pg_restore --list: archive header checksum mismatch"}
	} else {
		tr.Hashes = &state.HashUpdate{Hash: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", Alg: "sha256"}
	}
	if _, err := j.RecordTransition(ctx, tr); err != nil {
		t.Fatalf("quarantineFixture: -> Quarantined: %v", err)
	}
}

// quarantineLostFixture drives artifact all the way to COMPLETE
// (discoverAndAdvance again, extended by hand for the two edges it does not
// cover: RemoteDeletePending -> Complete -> QuarantinedLost), the only path
// machine.go admits into QUARANTINED_LOST.
func quarantineLostFixture(t *testing.T, j *state.Journal, artifact model.ArtifactID) {
	t.Helper()
	ctx := context.Background()
	localPath, _ := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath, state.RemoteIdentity{Size: &size}, localPath,
		&state.TransferResult{BytesTransferred: 10}, nil, RemoteDeletePending)

	now := time.Now()
	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: artifact.String() + ":fixture-complete",
		From: string(RemoteDeletePending), To: string(Complete),
		OccurredAt: now, Deletion: &state.DeletionUpdate{DeletedAt: &now},
	}); err != nil {
		t.Fatalf("quarantineLostFixture: -> Complete: %v", err)
	}
	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: artifact.String() + ":fixture-lost",
		From: string(Complete), To: string(QuarantinedLost),
		OccurredAt: now, Detail: "fixture: the only copy was corrupt and the remote source was already confirmed gone",
	}); err != nil {
		t.Fatalf("quarantineLostFixture: -> QuarantinedLost: %v", err)
	}
}

// --- ReleaseFromQuarantine ---

// TestReleaseFromQuarantine_MovesBackToDiscovered checks three things, and
// only the state change is obvious.
//
// RetryCount is Phase 4's repeat-visibility requirement: an artifact that
// has been quarantined, released and quarantined again has to be
// distinguishable from a first-time failure, or it can be silently cycled
// for ever. LastError carries the reason it was quarantined for, forward
// onto the row it is being released into, so the next failure of the same
// artifact arrives with the context of what was already tried.
func TestReleaseFromQuarantine_MovesBackToDiscovered(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	artifact := mustID(t)
	quarantineFixture(t, j, artifact, true)

	out, err := ReleaseFromQuarantine(ctx, Deps{Journal: j}, QuarantineReleaseParams{
		Artifact: artifact, AttemptKey: "release-1", Note: "confirmed a false positive",
	})
	if err != nil {
		t.Fatalf("ReleaseFromQuarantine: %v", err)
	}
	if out.Record.State != string(Discovered) {
		t.Fatalf("state = %q, want %q", out.Record.State, Discovered)
	}
	if out.Record.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1 after the first release", out.Record.RetryCount)
	}
	if out.Record.LastError == "" {
		t.Fatal("LastError is empty, want the derived quarantine reason to have been recorded")
	}
}

// TestReleaseFromQuarantine_RefusesQuarantinedLost pins both halves of the
// refusal: the typed error, which callers route on to show this differently
// from an ordinary quarantine, and the journal being left exactly where it
// was.
//
// The second half is what makes the refusal safe to retry. An operator who
// clicks release on a lost artifact should be told no and find everything
// unchanged, not discover that the attempt itself moved something.
func TestReleaseFromQuarantine_RefusesQuarantinedLost(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	artifact := mustID(t)
	quarantineLostFixture(t, j, artifact)

	_, err := ReleaseFromQuarantine(ctx, Deps{Journal: j}, QuarantineReleaseParams{
		Artifact: artifact, AttemptKey: "release-1",
	})
	if err == nil {
		t.Fatal("ReleaseFromQuarantine succeeded on a QUARANTINED_LOST artifact, want a refusal")
	}
	if _, ok := AsQuarantinedLostIsTerminal(err); !ok {
		t.Fatalf("error is not a *QuarantinedLostIsTerminalError: %v", err)
	}
	rec, getErr := j.Get(ctx, artifact)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if rec.State != string(QuarantinedLost) {
		t.Fatalf("journal state changed to %q, want it left at %q", rec.State, QuarantinedLost)
	}
}

// TestReleaseFromQuarantine_RefusesWhenNotQuarantined walks every state
// that is not a quarantine, which is the stale-screen case: an operator
// looking at a page rendered before the artifact moved on.
//
// Including the terminal states matters as much as the in-flight ones.
// Releasing a COMPLETE artifact back to DISCOVERED would re-fetch from a
// remote this manager has already deleted, which is the exact livelock
// QUARANTINED_LOST exists to avoid, reached from a different direction.
func TestReleaseFromQuarantine_RefusesWhenNotQuarantined(t *testing.T) {
	for _, from := range []State{Discovered, Transferring, Transferred, Verifying, Verified, Committing, Committed, RemoteDeletePending, Complete, Failed} {
		t.Run(string(from), func(t *testing.T) {
			ctx := context.Background()
			j := openTestJournal(t)
			artifact := mustID(t)
			localPath, _ := writeLocalFile(t, 10)
			size := int64(10)

			switch from {
			case Failed:
				discoverAndAdvance(t, j, artifact, testRemotePath, state.RemoteIdentity{Size: &size}, localPath,
					&state.TransferResult{BytesTransferred: 10}, nil, Discovered)
				if _, err := j.RecordTransition(ctx, state.Transition{
					Artifact: artifact, Key: "fixture:failed", From: string(Discovered), To: string(Failed),
					OccurredAt: time.Now(), Detail: "fixture: permanent failure",
				}); err != nil {
					t.Fatalf("-> Failed: %v", err)
				}
			case Complete:
				discoverAndAdvance(t, j, artifact, testRemotePath, state.RemoteIdentity{Size: &size}, localPath,
					&state.TransferResult{BytesTransferred: 10}, nil, RemoteDeletePending)
				now := time.Now()
				if _, err := j.RecordTransition(ctx, state.Transition{
					Artifact: artifact, Key: "fixture:complete", From: string(RemoteDeletePending), To: string(Complete),
					OccurredAt: now, Deletion: &state.DeletionUpdate{DeletedAt: &now},
				}); err != nil {
					t.Fatalf("-> Complete: %v", err)
				}
			default:
				discoverAndAdvance(t, j, artifact, testRemotePath, state.RemoteIdentity{Size: &size}, localPath,
					&state.TransferResult{BytesTransferred: 10}, nil, from)
			}

			_, err := ReleaseFromQuarantine(ctx, Deps{Journal: j}, QuarantineReleaseParams{
				Artifact: artifact, AttemptKey: "release-1",
			})
			if err == nil {
				t.Fatalf("ReleaseFromQuarantine succeeded from %s, want a refusal", from)
			}
			refusal, ok := AsNotQuarantined(err)
			if !ok {
				t.Fatalf("error is not a *NotQuarantinedError: %v", err)
			}
			if refusal.Current != from {
				t.Fatalf("refusal.Current = %s, want %s", refusal.Current, from)
			}
		})
	}
}

// TestReleaseFromQuarantine_RepeatedQuarantineIsVisible is Phase 4's own
// requirement in test form: "an artifact must not be silently retried into
// oblivion, so repeated quarantine of the same artifact should be visible
// rather than looking like fresh failures each time." This drives one
// artifact through quarantine, release, and back into quarantine three
// times over, and asserts RetryCount climbs by exactly one on every
// release, never resets, and never gets confused with a fresh artifact's
// RetryCount of 0.
func TestReleaseFromQuarantine_RepeatedQuarantineIsVisible(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	artifact := mustID(t)
	quarantineFixture(t, j, artifact, false)

	for cycle := 1; cycle <= 3; cycle++ {
		out, err := ReleaseFromQuarantine(ctx, Deps{Journal: j}, QuarantineReleaseParams{
			Artifact: artifact, AttemptKey: fmt.Sprintf("release-cycle-%d", cycle), Note: "retrying",
		})
		if err != nil {
			t.Fatalf("cycle %d: ReleaseFromQuarantine: %v", cycle, err)
		}
		if out.Record.RetryCount != cycle {
			t.Fatalf("cycle %d: RetryCount = %d, want %d", cycle, out.Record.RetryCount, cycle)
		}

		if cycle == 3 {
			break
		}

		// Simulate a fresh attempt that fails validation again: straight
		// back to VERIFYING then QUARANTINED, exactly like verify.go's
		// decide() would drive it a second time.
		if _, err := j.RecordTransition(ctx, state.Transition{
			Artifact: artifact, Key: fmt.Sprintf("cycle-%d:transferring", cycle), From: string(Discovered), To: string(Transferring),
			OccurredAt: time.Now(),
		}); err != nil {
			t.Fatalf("cycle %d: -> Transferring: %v", cycle, err)
		}
		if _, err := j.RecordTransition(ctx, state.Transition{
			Artifact: artifact, Key: fmt.Sprintf("cycle-%d:transferred", cycle), From: string(Transferring), To: string(Transferred),
			OccurredAt: time.Now(),
		}); err != nil {
			t.Fatalf("cycle %d: -> Transferred: %v", cycle, err)
		}
		if _, err := j.RecordTransition(ctx, state.Transition{
			Artifact: artifact, Key: fmt.Sprintf("cycle-%d:verifying", cycle), From: string(Transferred), To: string(Verifying),
			OccurredAt: time.Now(),
		}); err != nil {
			t.Fatalf("cycle %d: -> Verifying: %v", cycle, err)
		}
		if _, err := j.RecordTransition(ctx, state.Transition{
			Artifact: artifact, Key: fmt.Sprintf("cycle-%d:quarantined", cycle), From: string(Verifying), To: string(Quarantined),
			OccurredAt: time.Now(), Detail: "fixture: quarantined again",
			Hashes: &state.HashUpdate{Hash: "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe", Alg: "sha256"},
		}); err != nil {
			t.Fatalf("cycle %d: -> Quarantined: %v", cycle, err)
		}

		rec, err := j.Get(ctx, artifact)
		if err != nil {
			t.Fatalf("cycle %d: Get: %v", cycle, err)
		}
		if rec.RetryCount != cycle {
			t.Fatalf("cycle %d: RetryCount changed to %d across a plain re-quarantine, want it to stay at %d until the next release", cycle, rec.RetryCount, cycle)
		}
	}
}

// --- QuarantineReason ---

// TestQuarantineReason_ValidatorRejection asserts that the validator's own
// words survive into the reason.
//
// The detail is the whole value here. "The validator rejected it" tells an
// operator nothing they cannot infer from the state; "archive header
// checksum mismatch" tells them whether to re-fetch or to go and look at the
// producer. The assertion is on substrings rather than the full sentence so
// the wording can improve without the test having to be rewritten.
func TestQuarantineReason_ValidatorRejection(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	artifact := mustID(t)
	quarantineFixture(t, j, artifact, true)

	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	reason := QuarantineReason(rec)
	if !containsAll(reason, "application validator rejected", "archive header checksum mismatch") {
		t.Fatalf("QuarantineReason = %q, want it to mention the validator rejection and its detail", reason)
	}
}

// TestQuarantineReason_HashShape is the other shape the same function has
// to distinguish, and having both is what makes either meaningful: a
// reason builder that ignored its input and returned one fixed sentence
// would pass whichever of these two tests happened to match it.
//
// The recorded hash is asserted to appear because it is the evidence. An
// operator deciding whether to trust a copy again wants to see the digest
// that was compared, not just that a comparison failed.
func TestQuarantineReason_HashShape(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	artifact := mustID(t)
	quarantineFixture(t, j, artifact, false)

	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	reason := QuarantineReason(rec)
	if !containsAll(reason, "failed a content check", "deadbeef") {
		t.Fatalf("QuarantineReason = %q, want it to mention the content check and the recorded hash", reason)
	}
}

// containsAll is how the reason tests assert on a message without pinning
// it.
//
// The reasons are prose an operator reads, so they are expected to be
// reworded; what must not change is which FACTS they carry. Checking for
// several substrings says exactly that, and it fails usefully, because the
// test prints the whole reason when any one is missing.
func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// --- proof: quarantine forecloses remote deletion, via DeleteRemote itself ---

// TestDeleteRemote_RefusesFromQuarantine is Phase 4's other requirement in
// test form: "quarantine must never permit the remote source to be
// deleted." remotedelete_test.go's own TestDeleteRemote_RefusesWhenJournalStateIsWrong
// already proves this for every state on the nominal pre-COMMITTED path
// (Discovered through Committing) but does not include either quarantine
// state; this fills that gap by calling DeleteRemote directly against an
// artifact sitting in QUARANTINED and in QUARANTINED_LOST, and proves it
// refuses via the exact same revalidation #1 check (remotedelete.go), not
// a new one, and never reaches transport.Transport.DeleteRemote at all.
func TestDeleteRemote_RefusesFromQuarantine(t *testing.T) {
	ctx := context.Background()

	t.Run("QUARANTINED", func(t *testing.T) {
		j := openTestJournal(t)
		artifact := mustID(t)
		quarantineFixture(t, j, artifact, false)

		tp := &deleteTransport{}
		_, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
			CompletionStrategy: "rename",
			Artifact:           artifact, AttemptKey: "attempt-1",
		})
		_ = requireRefusal(t, err, "journal state")
		if tp.deleteCalls != 0 {
			t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
		}
		rec, getErr := j.Get(ctx, artifact)
		if getErr != nil {
			t.Fatalf("Get: %v", getErr)
		}
		if rec.State != string(Quarantined) {
			t.Fatalf("journal state changed to %q, want it left at %q", rec.State, Quarantined)
		}
	})

	t.Run("QUARANTINED_LOST", func(t *testing.T) {
		j := openTestJournal(t)
		artifact := mustID(t)
		quarantineLostFixture(t, j, artifact)

		tp := &deleteTransport{}
		_, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
			CompletionStrategy: "rename",
			Artifact:           artifact, AttemptKey: "attempt-1",
		})
		_ = requireRefusal(t, err, "journal state")
		if tp.deleteCalls != 0 {
			t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
		}
		rec, getErr := j.Get(ctx, artifact)
		if getErr != nil {
			t.Fatalf("Get: %v", getErr)
		}
		if rec.State != string(QuarantinedLost) {
			t.Fatalf("journal state changed to %q, want it left at %q", rec.State, QuarantinedLost)
		}
	})
}
