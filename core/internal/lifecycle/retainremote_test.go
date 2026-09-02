package lifecycle

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/state"
)

// --- RetainRemote ---

// TestRetainRemote_NeverReferencesTransport is issue #282's structural
// proof at this package's own level: Deps.Transport is left nil entirely,
// so any expression in RetainRemote's call graph that dereferenced it would
// panic. This is the strongest form of "the transport is never touched"
// this package can express directly: not a double that fails if called
// (that is core/internal/app's proof, driven through a real cycle), but a
// caller that supplies nothing at all for RetainRemote to call, in a state
// (COMMITTED) DeleteRemote would otherwise happily act on.
func TestRetainRemote_NeverReferencesTransport(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: localSum, HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10},
		&state.HashUpdate{Alg: "sha256", Hash: localSum},
		Committed)

	outcome, err := RetainRemote(ctx, Deps{Journal: j}, RetainRemoteRequest{
		Artifact:   artifact,
		AttemptKey: "retain-attempt-1",
	})
	if err != nil {
		t.Fatalf("RetainRemote: %v", err)
	}
	if outcome.Record.State != string(RemoteRetained) {
		t.Fatalf("final state = %q, want %q", outcome.Record.State, RemoteRetained)
	}
}

// TestRetainRemote_MovesCommittedToRemoteRetained is the same construction
// as the structural test above, but checked from a caller's perspective:
// the journal record, not just the returned Outcome, ends up at
// REMOTE_RETAINED with a Detail an operator reading the journal directly
// can make sense of.
func TestRetainRemote_MovesCommittedToRemoteRetained(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: localSum, HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10},
		&state.HashUpdate{Alg: "sha256", Hash: localSum},
		Committed)

	if _, err := RetainRemote(ctx, Deps{Journal: j}, RetainRemoteRequest{
		Artifact:   artifact,
		AttemptKey: "retain-attempt-1",
	}); err != nil {
		t.Fatalf("RetainRemote: %v", err)
	}

	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(RemoteRetained) {
		t.Fatalf("journal state = %q, want %q", rec.State, RemoteRetained)
	}
	if rec.RemoteDeletedAt != nil {
		t.Error("RemoteDeletedAt was recorded, but this artifact's remote was never touched")
	}
}

// TestRetainRemote_MovesRemoteDeletePendingToRemoteRetained proves the
// second declared edge: an artifact whose set was flipped to read-only
// after an earlier cycle already recorded delete INTENT (REMOTE_DELETE_
// PENDING), before the flag existed or before it was set, still has a way
// to reach REMOTE_RETAINED rather than being stuck offering itself to
// DeleteRemote forever.
func TestRetainRemote_MovesRemoteDeletePendingToRemoteRetained(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: localSum, HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10},
		&state.HashUpdate{Alg: "sha256", Hash: localSum},
		RemoteDeletePending)

	if _, err := RetainRemote(ctx, Deps{Journal: j}, RetainRemoteRequest{
		Artifact:   artifact,
		AttemptKey: "retain-attempt-1",
	}); err != nil {
		t.Fatalf("RetainRemote: %v", err)
	}

	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(RemoteRetained) {
		t.Fatalf("journal state = %q, want %q", rec.State, RemoteRetained)
	}
}

// TestRetainRemote_IdempotentOnAlreadyRetained proves calling RetainRemote
// again for an artifact already at REMOTE_RETAINED (a later cycle, the
// same read-only set) is a harmless no-op rather than a refusal: Validate's
// generic current==target rule covers it, and this pins that RetainRemote
// actually reaches that path rather than refusing on a stricter check of
// its own.
func TestRetainRemote_IdempotentOnAlreadyRetained(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: localSum, HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10},
		&state.HashUpdate{Alg: "sha256", Hash: localSum},
		Committed)

	deps := Deps{Journal: j}
	if _, err := RetainRemote(ctx, deps, RetainRemoteRequest{Artifact: artifact, AttemptKey: "retain-1"}); err != nil {
		t.Fatalf("first RetainRemote: %v", err)
	}
	if _, err := RetainRemote(ctx, deps, RetainRemoteRequest{Artifact: artifact, AttemptKey: "retain-2"}); err != nil {
		t.Fatalf("second RetainRemote (already retained): %v", err)
	}

	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(RemoteRetained) {
		t.Fatalf("journal state = %q, want %q", rec.State, RemoteRetained)
	}
}

// TestRetainRemote_RefusesWhenJournalStateIsWrong mirrors
// TestDeleteRemote_RefusesWhenJournalStateIsWrong: RetainRemote may only
// ever be reached from COMMITTED, REMOTE_DELETE_PENDING or (idempotently)
// REMOTE_RETAINED, and every other state is refused before anything is
// written.
func TestRetainRemote_RefusesWhenJournalStateIsWrong(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: localSum, HashAlg: "sha256"},
		localPath, nil, nil, Verifying)

	_, err := RetainRemote(ctx, Deps{Journal: j}, RetainRemoteRequest{
		Artifact:   artifact,
		AttemptKey: "retain-attempt-1",
	})
	if err == nil {
		t.Fatal("RetainRemote succeeded from VERIFYING, want a refusal")
	}

	rec, getErr := j.Get(ctx, artifact)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if rec.State != string(Verifying) {
		t.Fatalf("journal state = %q, want unchanged %q", rec.State, Verifying)
	}
}

func TestRetainRemote_RequiresAnAttemptKey(t *testing.T) {
	j := openTestJournal(t)
	_, err := RetainRemote(context.Background(), Deps{Journal: j}, RetainRemoteRequest{Artifact: mustID(t)})
	if err == nil {
		t.Fatal("RetainRemote succeeded with an empty AttemptKey")
	}
}

func TestRetainRemote_RequiresAJournal(t *testing.T) {
	_, err := RetainRemote(context.Background(), Deps{}, RetainRemoteRequest{Artifact: mustID(t), AttemptKey: "x"})
	if err == nil {
		t.Fatal("RetainRemote succeeded with no Journal")
	}
}

// --- ReleaseFromRetention ---

// TestReleaseFromRetention_MovesRemoteRetainedBackToCommitted proves
// REMOTE_RETAINED's one declared exit: an operator-triggered release
// returns the artifact to COMMITTED, ready to be revalidated from scratch
// by DeleteRemote on the next cycle exactly like any other COMMITTED
// artifact.
func TestReleaseFromRetention_MovesRemoteRetainedBackToCommitted(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: localSum, HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10},
		&state.HashUpdate{Alg: "sha256", Hash: localSum},
		Committed)

	deps := Deps{Journal: j}
	if _, err := RetainRemote(ctx, deps, RetainRemoteRequest{Artifact: artifact, AttemptKey: "retain-1"}); err != nil {
		t.Fatalf("RetainRemote: %v", err)
	}

	if _, err := ReleaseFromRetention(ctx, deps, ReleaseFromRetentionRequest{
		Artifact:   artifact,
		AttemptKey: "release-1",
		Note:       "operator confirmed this source is no longer read-only",
	}); err != nil {
		t.Fatalf("ReleaseFromRetention: %v", err)
	}

	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(Committed) {
		t.Fatalf("journal state = %q, want %q", rec.State, Committed)
	}
}

// TestReleaseFromRetention_RefusesWhenNotRemoteRetained proves the guard:
// only an artifact actually sitting at REMOTE_RETAINED can be released,
// and the error is the typed *NotRemoteRetainedError a caller can match on.
func TestReleaseFromRetention_RefusesWhenNotRemoteRetained(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: localSum, HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10},
		&state.HashUpdate{Alg: "sha256", Hash: localSum},
		Committed)

	_, err := ReleaseFromRetention(ctx, Deps{Journal: j}, ReleaseFromRetentionRequest{
		Artifact:   artifact,
		AttemptKey: "release-1",
	})
	if err == nil {
		t.Fatal("ReleaseFromRetention succeeded from COMMITTED, want a refusal")
	}
	if _, ok := AsNotRemoteRetained(err); !ok {
		t.Fatalf("error is not a *NotRemoteRetainedError: %v", err)
	}
}
