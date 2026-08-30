package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// --- test fixtures ---

func openTestJournal(t *testing.T) *state.Journal {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	j, err := state.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// writeLocalFile writes size deterministic bytes to a fresh temp file and
// returns its path and hex sha256.
func writeLocalFile(t *testing.T, size int64) (path string, sum string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "backup.final")
	data := bytes.Repeat([]byte{0xAB}, int(size))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	h := sha256.Sum256(data)
	return path, hex.EncodeToString(h[:])
}

// discoverAndAdvance drives artifact through the nominal FR-11 sequence up
// to (and including) stopAt, one legal Advance-shaped RecordTransition at a
// time, so every fixture this test file builds is a journal row that could
// really have been produced by the pipeline, not a hand-poked row that
// happens to have the right State string.
func discoverAndAdvance(
	t *testing.T,
	j *state.Journal,
	artifact model.ArtifactID,
	remotePath string,
	remote state.RemoteIdentity,
	localFinalPath string,
	transfer *state.TransferResult,
	hashes *state.HashUpdate,
	stopAt State,
) {
	t.Helper()
	ctx := context.Background()
	seq := 0
	nextKey := func() string {
		seq++
		return fmt.Sprintf("%s#%d", artifact, seq)
	}
	now := func() time.Time { return time.Now().UTC() }

	if _, err := j.Discover(ctx, artifact, nextKey(), remotePath, remote, now()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if stopAt == Discovered {
		return
	}

	partial := localFinalPath + ".partial"
	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: nextKey(), From: string(Discovered), To: string(Transferring),
		OccurredAt: now(), LocalPath: &partial,
	}); err != nil {
		t.Fatalf("-> TRANSFERRING: %v", err)
	}
	if stopAt == Transferring {
		return
	}

	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: nextKey(), From: string(Transferring), To: string(Transferred),
		OccurredAt: now(), Transfer: transfer,
	}); err != nil {
		t.Fatalf("-> TRANSFERRED: %v", err)
	}
	if stopAt == Transferred {
		return
	}

	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: nextKey(), From: string(Transferred), To: string(Verifying),
		OccurredAt: now(),
	}); err != nil {
		t.Fatalf("-> VERIFYING: %v", err)
	}
	if stopAt == Verifying {
		return
	}

	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: nextKey(), From: string(Verifying), To: string(Verified),
		OccurredAt: now(), Hashes: hashes,
	}); err != nil {
		t.Fatalf("-> VERIFIED: %v", err)
	}
	if stopAt == Verified {
		return
	}

	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: nextKey(), From: string(Verified), To: string(Committing),
		OccurredAt: now(),
	}); err != nil {
		t.Fatalf("-> COMMITTING: %v", err)
	}
	if stopAt == Committing {
		return
	}

	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: nextKey(), From: string(Committing), To: string(Committed),
		OccurredAt: now(), LocalPath: &localFinalPath,
	}); err != nil {
		t.Fatalf("-> COMMITTED: %v", err)
	}
	if stopAt == Committed {
		return
	}

	if stopAt == RemoteDeletePending {
		if _, err := j.RecordTransition(ctx, state.Transition{
			Artifact: artifact, Key: nextKey(), From: string(Committed), To: string(RemoteDeletePending),
			OccurredAt: now(),
		}); err != nil {
			t.Fatalf("-> REMOTE_DELETE_PENDING: %v", err)
		}
		return
	}

	t.Fatalf("discoverAndAdvance: unsupported stopAt %s", stopAt)
}

// deleteTransport is a minimal, fully controllable transport.Transport for
// exercising DeleteRemote's revalidation without a real backend. Every
// method not needed by a given test panics loudly via "not used", the same
// convention fakeJournal (engine_test.go) already uses in this package, so
// a test that unexpectedly calls further than it meant to fails immediately
// rather than returning a quiet zero value.
type deleteTransport struct {
	statFn func(ctx context.Context, source transport.Source, remotePath string) (transport.RemoteArtifact, error)

	deleteCalls int
	deleteErr   error
}

var _ transport.Transport = (*deleteTransport)(nil)

func (f *deleteTransport) List(context.Context, transport.Source) ([]transport.RemoteArtifact, error) {
	return nil, errors.New("deleteTransport: List not used")
}

func (f *deleteTransport) Stat(ctx context.Context, source transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	if f.statFn == nil {
		return transport.RemoteArtifact{}, errors.New("deleteTransport: Stat not configured")
	}
	return f.statFn(ctx, source, remotePath)
}

func (f *deleteTransport) CopyToLocal(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
	return transport.TransferResult{}, errors.New("deleteTransport: CopyToLocal not used")
}

func (f *deleteTransport) RemoteHash(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
	return "", errors.New("deleteTransport: RemoteHash not used")
}

func (f *deleteTransport) DeleteRemote(ctx context.Context, source transport.Source, remotePath string) error {
	f.deleteCalls++
	return f.deleteErr
}

const testRemotePath = "backups/backup.dump.zst"

// requireRefusal asserts err is a *RemoteDeleteRefusalError with the given
// Check, and returns it for further assertions.
func requireRefusal(t *testing.T, err error, wantCheck string) *RemoteDeleteRefusalError {
	t.Helper()
	if err == nil {
		t.Fatal("DeleteRemote succeeded, want a refusal")
	}
	refusal, ok := AsRemoteDeleteRefusal(err)
	if !ok {
		t.Fatalf("error is not a *RemoteDeleteRefusalError: %v", err)
	}
	if refusal.Check != wantCheck {
		t.Errorf("refusal.Check = %q, want %q (reason: %s)", refusal.Check, wantCheck, refusal.Reason)
	}
	return refusal
}

// --- adversarial cases: every way FR-15 must refuse ---

// The journal artifact must be COMMITTED or REMOTE_DELETE_PENDING. Every
// other state, including every state earlier in the nominal pipeline, must
// be refused without ever reaching the transport.
func TestDeleteRemote_RefusesWhenJournalStateIsWrong(t *testing.T) {
	for _, from := range []State{Discovered, Transferring, Transferred, Verifying, Verified, Committing} {
		t.Run(string(from), func(t *testing.T) {
			j := openTestJournal(t)
			ctx := context.Background()
			artifact := mustID(t)
			localPath, _ := writeLocalFile(t, 10)
			size := int64(10)

			discoverAndAdvance(t, j, artifact, testRemotePath, state.RemoteIdentity{Size: &size}, localPath,
				&state.TransferResult{BytesTransferred: 10}, nil, from)

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
			if rec.State != string(from) {
				t.Errorf("journal state changed to %q, want it left at %q", rec.State, from)
			}
		})
	}
}

// The expected local final file must actually exist. This is what a lost or
// never-written final copy looks like: refuse, and never call the
// transport.
func TestDeleteRemote_RefusesWhenLocalFileIsMissing(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	missingPath := filepath.Join(t.TempDir(), "backup.final")
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath, state.RemoteIdentity{Size: &size}, missingPath,
		&state.TransferResult{BytesTransferred: 10}, nil, Committed)

	tp := &deleteTransport{}
	_, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Artifact:           artifact, AttemptKey: "attempt-1",
	})

	_ = requireRefusal(t, err, "local file")
	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
	}
	rec, getErr := j.Get(ctx, artifact)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if rec.State != string(Committed) {
		t.Errorf("journal state changed to %q, want it left at COMMITTED", rec.State)
	}
}

// The local final file's size must match what was recorded. A short (or
// long) file is exactly the "something touched this after COMMITTED"
// signal FR-15 exists to catch.
func TestDeleteRemote_RefusesWhenLocalFileIsWrongSize(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	// Recorded remote/transfer size is 10 bytes; the file on disk is 5.
	localPath, _ := writeLocalFile(t, 5)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath, state.RemoteIdentity{Size: &size}, localPath,
		&state.TransferResult{BytesTransferred: 10}, nil, Committed)

	tp := &deleteTransport{}
	_, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Artifact:           artifact, AttemptKey: "attempt-1",
	})

	_ = requireRefusal(t, err, "local file")
	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
	}
}

// When a local hash was recorded at VERIFIED, DeleteRemote recomputes it
// and refuses on a mismatch, even though the size is fine, catching content
// corruption or replacement a size check alone would miss.
func TestDeleteRemote_RefusesWhenLocalHashDoesNotMatch(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, _ := writeLocalFile(t, 10) // real hash of this file is not "deadbeef..."
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath, state.RemoteIdentity{Size: &size}, localPath,
		&state.TransferResult{BytesTransferred: 10},
		&state.HashUpdate{Alg: "sha256", Hash: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
		Committed)

	tp := &deleteTransport{}
	_, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Artifact:           artifact, AttemptKey: "attempt-1",
	})

	_ = requireRefusal(t, err, "local file")
	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
	}
}

// A recorded remote size that disagrees with the recorded transfer size is
// an internal inconsistency in the journal itself; refuse rather than
// silently pick one.
func TestDeleteRemote_RefusesWhenRecordedSizesDisagreeWithEachOther(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, _ := writeLocalFile(t, 10)
	remoteSize := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath, state.RemoteIdentity{Size: &remoteSize}, localPath,
		&state.TransferResult{BytesTransferred: 999}, nil, Committed)

	tp := &deleteTransport{}
	_, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Artifact:           artifact, AttemptKey: "attempt-1",
	})

	_ = requireRefusal(t, err, "local file")
	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
	}
}

// The core FR-16 case: the remote object at this path is confirmed, with
// strong confidence, to no longer be the object discovered. This is what
// stops the manager deleting a freshly written file that reused an old
// pathname, and it must be refused, loudly, and durably recorded, never
// silently.
func TestDeleteRemote_RefusesWhenRemoteIdentityConfirmedChanged(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, _ := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: "aaaa", HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10}, nil, Committed)

	tp := &deleteTransport{
		statFn: func(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
			return transport.RemoteArtifact{
				Path: testRemotePath, Size: 10, Hash: "bbbb", HashAlg: transport.SHA256,
			}, nil
		},
	}

	_, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Artifact:           artifact, AttemptKey: "attempt-1",
	})

	refusal := requireRefusal(t, err, "remote identity")
	if !refusal.HasComparison {
		t.Fatal("refusal has no identity comparison attached")
	}
	if refusal.Verdict != model.VerdictChanged {
		t.Errorf("Verdict = %s, want %s", refusal.Verdict, model.VerdictChanged)
	}
	if refusal.Confidence != model.ConfidenceStrong {
		t.Errorf("Confidence = %s, want %s", refusal.Confidence, model.ConfidenceStrong)
	}
	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
	}

	// Intent was already durably recorded before the identity check ran
	// (crash_safety.go's ordering), so the refusal must be visible directly
	// in the journal too, not only in the returned error.
	rec, getErr := j.Get(ctx, artifact)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if rec.State != string(RemoteDeletePending) {
		t.Errorf("journal state = %q, want REMOTE_DELETE_PENDING", rec.State)
	}
	if rec.RemoteDeleteError == "" {
		t.Error("the refusal was not persisted into remote_delete_error; it is only visible to whatever caught the returned error")
	}
}

// The hardened-SFTP case (docs/ssh-setup.md): no hash is available on
// either side, so the strongest evidence CompareIdentity can reach is a
// matching size and modification time, ConfidenceWeak on an Unconfirmed
// verdict. FR-16 requires this to refuse exactly the same as a confirmed
// change, and this is the outcome production deployments follow that
// guidance should expect routinely, not exceptionally.
func TestDeleteRemote_RefusesWhenRemoteIdentityCannotBeConfirmed(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, _ := writeLocalFile(t, 10)
	size := int64(10)
	mtime := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	// No hash captured at discovery: this is what a shell-less SFTP account
	// produces, since remote hashing needs a shell command rclone cannot
	// run against it.
	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, ModTime: &mtime},
		localPath, &state.TransferResult{BytesTransferred: 10}, nil, Committed)

	tp := &deleteTransport{
		statFn: func(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
			return transport.RemoteArtifact{
				Path: testRemotePath, Size: 10, ModTime: mtime.Unix(),
				// no Hash/HashAlg: same as discovery, same limitation.
			}, nil
		},
	}

	_, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Artifact:           artifact, AttemptKey: "attempt-1",
	})

	refusal := requireRefusal(t, err, "remote identity")
	if !refusal.HasComparison {
		t.Fatal("refusal has no identity comparison attached")
	}
	if refusal.Verdict != model.VerdictUnconfirmed {
		t.Errorf("Verdict = %s, want %s", refusal.Verdict, model.VerdictUnconfirmed)
	}
	if refusal.Confidence != model.ConfidenceWeak {
		t.Errorf("Confidence = %s, want %s", refusal.Confidence, model.ConfidenceWeak)
	}
	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
	}

	rec, getErr := j.Get(ctx, artifact)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if rec.RemoteDeleteError == "" {
		t.Error("the routine hardened-SFTP refusal must still be durably visible in the journal, not silent")
	}
}

// A Stat failure (the object is unreachable, or outright gone) must also
// refuse rather than assume anything: FR-17's reconciliation (issue #18),
// not this step, owns deciding what an absent remote object means.
func TestDeleteRemote_RefusesWhenRemoteCannotBeStatted(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, _ := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath, state.RemoteIdentity{Size: &size}, localPath,
		&state.TransferResult{BytesTransferred: 10}, nil, Committed)

	tp := &deleteTransport{
		statFn: func(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
			return transport.RemoteArtifact{}, transport.NewError(transport.NotFound, "stat", errors.New("no such object"))
		},
	}

	_, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Artifact:           artifact, AttemptKey: "attempt-1",
	})

	_ = requireRefusal(t, err, "remote identity")
	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
	}
}

// Revalidation must run in full again even when a previous attempt already
// recorded REMOTE_DELETE_PENDING (crash_safety.go's restart case): it must
// not treat "already pending" as license to skip straight to the delete.
func TestDeleteRemote_RefusesFromRemoteDeletePendingWhenIdentityChanged(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, _ := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: "aaaa", HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10}, nil, RemoteDeletePending)

	tp := &deleteTransport{
		statFn: func(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
			return transport.RemoteArtifact{Path: testRemotePath, Size: 10, Hash: "bbbb", HashAlg: transport.SHA256}, nil
		},
	}

	_, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Artifact:           artifact, AttemptKey: "restart-attempt",
	})

	_ = requireRefusal(t, err, "remote identity")
	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
	}
}

// DeleteRemote refuses malformed requests outright rather than let a
// missing idempotency key slip through and defeat the crash-retry contract
// RecordTransition depends on.
func TestDeleteRemote_RequiresAnAttemptKey(t *testing.T) {
	j := openTestJournal(t)
	tp := &deleteTransport{}
	_, err := DeleteRemote(context.Background(), Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Artifact:           mustID(t),
	})
	if err == nil {
		t.Fatal("DeleteRemote accepted an empty AttemptKey")
	}
	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
	}
}

// --- WP3.2: stable-strategy artifacts need an additional deletion-safety
// delay before this gate treats them as equivalent to rename/marker ---

// stableFixture is a freshly-committed artifact whose remote object still
// matches what discovery captured, so every FR-15 check other than WP3.2's
// own safety delay clears. Everything below varies one thing against it.
func stableFixture(t *testing.T) (*state.Journal, model.ArtifactID, *deleteTransport) {
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

	tp := &deleteTransport{
		statFn: func(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
			return transport.RemoteArtifact{Path: testRemotePath, Size: 10, Hash: localSum, HashAlg: transport.SHA256}, nil
		},
	}
	return j, artifact, tp
}

// committedAt reads back the moment an artifact entered COMMITTED, so a
// test can place Deps.Now an exact distance from the one timestamp WP3.2's
// gate actually measures against.
func committedAt(t *testing.T, j *state.Journal, artifact model.ArtifactID) time.Time {
	t.Helper()
	at, ok, err := j.LastEnteredAt(context.Background(), artifact, string(Committed))
	if err != nil {
		t.Fatalf("LastEnteredAt: %v", err)
	}
	if !ok {
		t.Fatal("the fixture has no recorded COMMITTED transition")
	}
	return at
}

// TestDeleteRemote_StableStrategyRequiresSafetyDelay is WP3.2's own RED
// proof (docs/EPIC-B-multi-nas.md §71 Work Package 3.2, §26 Step 3): a
// "stable"-strategy artifact that has not yet satisfied its configured
// delete_safety_delay must be refused here, at the exact same fresh-commit
// journal state a "rename" or "marker" artifact sails through unrefused.
// Every subtest below shares one fixture and only varies
// CompletionStrategy, so a passing rename/marker subtest next to a refused
// stable one proves this is specifically about the "stable" heuristic, not
// some other difference between the fixtures.
func TestDeleteRemote_StableStrategyRequiresSafetyDelay(t *testing.T) {
	delay := 10 * time.Minute

	t.Run("stable is refused before the safety delay elapses", func(t *testing.T) {
		j, artifact, tp := stableFixture(t)

		_, err := DeleteRemote(context.Background(), Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
			Artifact:           artifact,
			AttemptKey:         "attempt-1",
			CompletionStrategy: "stable",
			DeleteSafetyDelay:  delay,
		})

		_ = requireRefusal(t, err, "stable completion safety delay")
		if tp.deleteCalls != 0 {
			t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
		}
		rec, getErr := j.Get(context.Background(), artifact)
		if getErr != nil {
			t.Fatalf("Get: %v", getErr)
		}
		if rec.State != string(Committed) {
			t.Errorf("journal state = %q, want it left at COMMITTED: the remote source must be preserved, not deleted early", rec.State)
		}
	})

	for _, strategy := range []string{"rename", "marker"} {
		t.Run(strategy+" is not gated by the safety delay", func(t *testing.T) {
			j, artifact, tp := stableFixture(t)

			outcome, err := DeleteRemote(context.Background(), Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
				Artifact:           artifact,
				AttemptKey:         "attempt-1",
				CompletionStrategy: strategy,
			})
			if err != nil {
				t.Fatalf("strategy %s was refused in the exact same journal state a fresh commit ordinarily deletes from: %v", strategy, err)
			}
			if outcome.Record.State != string(Complete) {
				t.Fatalf("final state = %q, want COMPLETE", outcome.Record.State)
			}
			if tp.deleteCalls != 1 {
				t.Fatalf("transport.DeleteRemote called %d times, want exactly 1", tp.deleteCalls)
			}
		})
	}
}

// TestDeleteRemote_StableStrategyProceedsOnceSafetyDelayElapses is the
// above test's positive complement: the identical stable-strategy fixture
// and configured delay, but Deps.Now moved far enough past the COMMITTED
// transition that the delay has genuinely elapsed. This is what proves the
// gate above is a delay, not a disguised permanent refusal of every
// "stable" artifact.
func TestDeleteRemote_StableStrategyProceedsOnceSafetyDelayElapses(t *testing.T) {
	j, artifact, tp := stableFixture(t)

	future := func() time.Time { return time.Now().UTC().Add(time.Hour) }
	outcome, err := DeleteRemote(context.Background(), Deps{Journal: j, Transport: tp, Now: future}, DeleteRemoteRequest{
		Artifact:           artifact,
		AttemptKey:         "attempt-1",
		CompletionStrategy: "stable",
		DeleteSafetyDelay:  10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("a stable-strategy delete whose safety delay has genuinely elapsed was refused: %v", err)
	}
	if outcome.Record.State != string(Complete) {
		t.Fatalf("final state = %q, want COMPLETE", outcome.Record.State)
	}
	if tp.deleteCalls != 1 {
		t.Fatalf("transport.DeleteRemote called %d times, want exactly 1", tp.deleteCalls)
	}
}

// TestDeleteRemote_StableSafetyDelayBoundary pins which side of
// "elapsed < delay" is inclusive.
//
// The two tests above cover elapsed near zero and elapsed an hour past a
// ten minute delay, which leaves the boundary itself inferred from the
// operator rather than stated. On a comparison that authorises destroying
// the only other copy of an artifact that is not good enough, and the
// sibling capacity package uses "at or below" for its own thresholds, so
// the two would otherwise read inconsistently with nothing pinning either.
//
// Exactly at the delay admits: "wait at least this long" is satisfied the
// instant it has been served. One nanosecond short refuses.
func TestDeleteRemote_StableSafetyDelayBoundary(t *testing.T) {
	delay := 10 * time.Minute

	t.Run("exactly at the delay is admitted", func(t *testing.T) {
		j, artifact, tp := stableFixture(t)
		at := committedAt(t, j, artifact).Add(delay)

		outcome, err := DeleteRemote(context.Background(),
			Deps{Journal: j, Transport: tp, Now: func() time.Time { return at }},
			DeleteRemoteRequest{
				Artifact:           artifact,
				AttemptKey:         "attempt-1",
				CompletionStrategy: "stable",
				DeleteSafetyDelay:  delay,
			})
		if err != nil {
			t.Fatalf("elapsed == delay was refused: %v", err)
		}
		if outcome.Record.State != string(Complete) {
			t.Fatalf("final state = %q, want COMPLETE", outcome.Record.State)
		}
		if tp.deleteCalls != 1 {
			t.Fatalf("transport.DeleteRemote called %d times, want exactly 1", tp.deleteCalls)
		}
	})

	t.Run("one nanosecond short of the delay is refused", func(t *testing.T) {
		j, artifact, tp := stableFixture(t)
		at := committedAt(t, j, artifact).Add(delay - time.Nanosecond)

		_, err := DeleteRemote(context.Background(),
			Deps{Journal: j, Transport: tp, Now: func() time.Time { return at }},
			DeleteRemoteRequest{
				Artifact:           artifact,
				AttemptKey:         "attempt-1",
				CompletionStrategy: "stable",
				DeleteSafetyDelay:  delay,
			})
		_ = requireRefusal(t, err, "stable completion safety delay")
		if tp.deleteCalls != 0 {
			t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
		}
	})
}

// TestDeleteRemote_RefusesAnUnknownCompletionStrategy is the gate's default
// position.
//
// The first version of this field embedded the whole config.Completion and
// documented the zero value as equivalent to "rename": no extra delay
// required. That made "no gate" the default for any caller that did not
// fill the struct in, and three of the four call sites in this repository
// did not, the crash matrix among them, which is the suite most likely to
// catch a regression in exactly this code. The argument for it was that
// config.Validate never lets Strategy be empty in a validated config, which
// is a property of a different package this function cannot check and which
// says nothing about a caller building a config.BackupSet in Go.
//
// So an empty or unrecognised strategy is now a refusal. Nothing is
// deleted, nothing is written, and the caller is told which value it sent.
func TestDeleteRemote_RefusesAnUnknownCompletionStrategy(t *testing.T) {
	for _, strategy := range []string{"", "STABLE", "whatever"} {
		name := strategy
		if name == "" {
			name = "the zero value"
		}
		t.Run(name, func(t *testing.T) {
			j, artifact, tp := stableFixture(t)

			_, err := DeleteRemote(context.Background(), Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
				Artifact:           artifact,
				AttemptKey:         "attempt-1",
				CompletionStrategy: strategy,
			})

			_ = requireRefusal(t, err, "unknown completion strategy")
			if tp.deleteCalls != 0 {
				t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
			}
			rec, getErr := j.Get(context.Background(), artifact)
			if getErr != nil {
				t.Fatalf("Get: %v", getErr)
			}
			if rec.State != string(Committed) {
				t.Errorf("journal state = %q, want it left untouched at COMMITTED", rec.State)
			}
		})
	}

	// The positive control. Every subtest above asserts a refusal, and a
	// gate wired to refuse everything would pass all of them. This is the
	// same fixture, the same elapsed time, the same everything, with a
	// strategy the gate does recognise, and it deletes.
	t.Run("a recognised strategy on the same fixture deletes", func(t *testing.T) {
		j, artifact, tp := stableFixture(t)

		outcome, err := DeleteRemote(context.Background(), Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
			Artifact:           artifact,
			AttemptKey:         "attempt-1",
			CompletionStrategy: "rename",
		})
		if err != nil {
			t.Fatalf("strategy \"rename\" was refused on the fixture every subtest above uses: %v", err)
		}
		if outcome.Record.State != string(Complete) {
			t.Fatalf("final state = %q, want COMPLETE", outcome.Record.State)
		}
		if tp.deleteCalls != 1 {
			t.Fatalf("transport.DeleteRemote called %d times, want exactly 1", tp.deleteCalls)
		}
	})
}

// TestDeleteRemote_StableStrategyRefusesWithoutASafetyDelay covers the
// other half of the same default-position argument. A request that says
// "stable" but carries no delay has not said how long to wait, and the
// answer to that is not "no time at all".
func TestDeleteRemote_StableStrategyRefusesWithoutASafetyDelay(t *testing.T) {
	j, artifact, tp := stableFixture(t)

	_, err := DeleteRemote(context.Background(), Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		Artifact:           artifact,
		AttemptKey:         "attempt-1",
		CompletionStrategy: "stable",
	})

	_ = requireRefusal(t, err, "stable completion safety delay")
	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote called %d times, want 0", tp.deleteCalls)
	}
}

// TestDeleteRemote_StableSafetyClockSurvivesItsOwnIntentWrite is the retry
// half of the same livelock internal/revalidate's own WP3.2 test covers
// from the scheduled-re-check side.
//
// The gate's success path is followed immediately by the COMMITTED ->
// REMOTE_DELETE_PENDING intent write, and refuseRemoteIdentity records a
// same-state REMOTE_DELETE_PENDING pass on every routine identity refusal,
// which this package's own doc describes as the expected outcome against a
// hardened SFTP account rather than a rare one. Both of those advance the
// artifacts row's updated_at. If the safety clock were that field, a
// transport failure after the intent write, or one ordinary identity
// refusal, would buy the next attempt another full delay, forever.
//
// Here the transport fails the delete once, then the same artifact is
// retried at the same instant. Measured from the COMMITTED transition the
// delay is still satisfied, so the retry must get through the gate and
// reach the transport a second time.
func TestDeleteRemote_StableSafetyClockSurvivesItsOwnIntentWrite(t *testing.T) {
	delay := 10 * time.Minute
	j, artifact, tp := stableFixture(t)
	at := committedAt(t, j, artifact).Add(delay + time.Minute)
	now := func() time.Time { return at }

	tp.deleteErr = errors.New("transport: connection reset")
	req := DeleteRemoteRequest{
		Artifact:           artifact,
		AttemptKey:         "attempt-1",
		CompletionStrategy: "stable",
		DeleteSafetyDelay:  delay,
	}

	if _, err := DeleteRemote(context.Background(), Deps{Journal: j, Transport: tp, Now: now}, req); err == nil {
		t.Fatal("a failing transport delete returned no error")
	}
	rec, err := j.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(RemoteDeletePending) {
		t.Fatalf("state after the failed delete = %q, want REMOTE_DELETE_PENDING", rec.State)
	}

	// The positive control for the retry below: the intent write really did
	// move the shared timestamp forward, past the point where a clock built
	// on it would refuse. Without this, the retry succeeding would not
	// distinguish the two clocks.
	if !rec.UpdatedAt.After(at.Add(-delay)) {
		t.Fatalf("UpdatedAt = %s, want it advanced to within %s of %s by the intent write; if it is not, this test cannot tell the shared timestamp apart from the COMMITTED transition", rec.UpdatedAt, delay, at)
	}

	tp.deleteErr = nil
	outcome, err := DeleteRemote(context.Background(), Deps{Journal: j, Transport: tp, Now: now}, req)
	if err != nil {
		t.Fatalf("the retry was refused after the first attempt recorded intent: %v", err)
	}
	if outcome.Record.State != string(Complete) {
		t.Fatalf("final state = %q, want COMPLETE", outcome.Record.State)
	}
	if tp.deleteCalls != 2 {
		t.Fatalf("transport.DeleteRemote called %d times, want 2 (the failure and the retry)", tp.deleteCalls)
	}
}

// --- the positive control ---
//
// A function that refuses unconditionally would pass every test above. This
// proves the opposite failure mode does not exist: when every single FR-15
// check genuinely clears, cleanly matching state, present and correctly
// sized/hashed local file, remote identity confirmed unchanged with strong
// confidence, the delete actually proceeds and the artifact reaches
// COMPLETE.
func TestDeleteRemote_PositiveControl_SafeDeleteProceeds(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)
	// A faithful copy has the same content, and so the same hash, on both
	// sides; reuse the one real hash of the bytes actually on disk for
	// both the discovered/current remote identity and the recorded local
	// hash, exactly as a genuine backup would.
	remoteHash := localSum

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: remoteHash, HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10},
		&state.HashUpdate{Alg: "sha256", Hash: localSum},
		Committed)

	tp := &deleteTransport{
		statFn: func(_ context.Context, _ transport.Source, remotePath string) (transport.RemoteArtifact, error) {
			if remotePath != testRemotePath {
				t.Errorf("Stat called with remotePath = %q, want %q", remotePath, testRemotePath)
			}
			return transport.RemoteArtifact{
				Path: testRemotePath, Size: 10, Hash: remoteHash, HashAlg: transport.SHA256,
			}, nil
		},
	}

	outcome, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Source:             transport.Source{ID: "prod-nas"},
		Artifact:           artifact,
		AttemptKey:         "attempt-1",
	})
	if err != nil {
		t.Fatalf("a genuinely safe delete was refused: %v", err)
	}
	if !outcome.Applied {
		t.Fatal("DeleteRemote reported success but Outcome.Applied is false")
	}
	if outcome.Record.State != string(Complete) {
		t.Fatalf("final state = %q, want COMPLETE", outcome.Record.State)
	}
	if tp.deleteCalls != 1 {
		t.Fatalf("transport.DeleteRemote called %d times, want exactly 1", tp.deleteCalls)
	}

	rec, getErr := j.Get(ctx, artifact)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if rec.State != string(Complete) {
		t.Fatalf("journal state = %q, want COMPLETE", rec.State)
	}
	if rec.RemoteDeletedAt == nil {
		t.Error("RemoteDeletedAt was not recorded")
	}
}

// The positive control also has to hold starting from REMOTE_DELETE_PENDING
// (the restart case): a safe delete must still complete, proving the
// "revalidate every single time" rule is not itself what blocks a
// legitimate retry.
func TestDeleteRemote_PositiveControl_ProceedsFromRemoteDeletePending(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)
	remoteHash := localSum

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: remoteHash, HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10},
		&state.HashUpdate{Alg: "sha256", Hash: localSum},
		RemoteDeletePending)

	tp := &deleteTransport{
		statFn: func(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
			return transport.RemoteArtifact{Path: testRemotePath, Size: 10, Hash: remoteHash, HashAlg: transport.SHA256}, nil
		},
	}

	outcome, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Artifact:           artifact, AttemptKey: "restart-attempt",
	})
	if err != nil {
		t.Fatalf("a genuinely safe retry from REMOTE_DELETE_PENDING was refused: %v", err)
	}
	if outcome.Record.State != string(Complete) {
		t.Fatalf("final state = %q, want COMPLETE", outcome.Record.State)
	}
	if tp.deleteCalls != 1 {
		t.Fatalf("transport.DeleteRemote called %d times, want exactly 1", tp.deleteCalls)
	}
}

// If the transport delete call itself fails after every revalidation
// cleared, that failure must be durably visible too, not just returned.
func TestDeleteRemote_RecordsTransportDeleteFailure(t *testing.T) {
	j := openTestJournal(t)
	ctx := context.Background()
	artifact := mustID(t)
	localPath, localSum := writeLocalFile(t, 10)
	size := int64(10)

	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size, Hash: localSum, HashAlg: "sha256"},
		localPath, &state.TransferResult{BytesTransferred: 10}, nil, Committed)

	tp := &deleteTransport{
		statFn: func(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
			return transport.RemoteArtifact{Path: testRemotePath, Size: 10, Hash: localSum, HashAlg: transport.SHA256}, nil
		},
		deleteErr: errors.New("permission denied"),
	}

	_, err := DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Artifact:           artifact, AttemptKey: "attempt-1",
	})
	if err == nil {
		t.Fatal("DeleteRemote succeeded despite the transport delete call failing")
	}
	if tp.deleteCalls != 1 {
		t.Fatalf("transport.DeleteRemote called %d times, want exactly 1", tp.deleteCalls)
	}

	rec, getErr := j.Get(ctx, artifact)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if rec.State != string(RemoteDeletePending) {
		t.Errorf("journal state = %q, want it left at REMOTE_DELETE_PENDING for a future retry", rec.State)
	}
	if rec.RemoteDeleteError == "" {
		t.Error("the transport failure was not persisted into remote_delete_error")
	}
}
