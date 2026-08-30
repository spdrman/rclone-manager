package state

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

func openJournal(t *testing.T) (*Journal, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	j, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j, path
}

func testArtifact(t *testing.T) model.ArtifactID {
	t.Helper()
	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, "backup-2026-08-27.dump.zst")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	return artifact
}

// Open enables WAL journaling. FR-14's durability argument, and the
// idempotent-crash-retry tests below, both depend on this actually being
// true rather than assumed.
func TestOpen_EnablesWAL(t *testing.T) {
	j, _ := openJournal(t)
	var mode string
	if err := j.db.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestDiscover_PersistsRemoteIdentity(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	artifact := testArtifact(t)

	size := int64(4096)
	mtime := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	remote := RemoteIdentity{
		Size:      &size,
		ModTime:   &mtime,
		Hash:      "abc123",
		HashAlg:   "sha256",
		BackendID: "sftp-inode-42",
	}

	outcome, err := j.Discover(ctx, artifact, "discover-1", "/incoming/backup-2026-08-27.dump.zst", remote, time.Now())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !outcome.Applied {
		t.Fatalf("Discover: Applied = false on first call")
	}
	if outcome.Record.State != "DISCOVERED" {
		t.Fatalf("State = %q, want DISCOVERED", outcome.Record.State)
	}
	if outcome.Record.RemotePath != "/incoming/backup-2026-08-27.dump.zst" {
		t.Fatalf("RemotePath = %q", outcome.Record.RemotePath)
	}

	got := outcome.Record.Remote
	if got.Size == nil || *got.Size != size {
		t.Fatalf("Remote.Size = %v, want %d", got.Size, size)
	}
	if got.ModTime == nil || !got.ModTime.Equal(mtime) {
		t.Fatalf("Remote.ModTime = %v, want %v", got.ModTime, mtime)
	}
	if got.Hash != "abc123" || got.HashAlg != "sha256" {
		t.Fatalf("Remote hash = %q/%q", got.Hash, got.HashAlg)
	}
	if got.BackendID != "sftp-inode-42" {
		t.Fatalf("Remote.BackendID = %q", got.BackendID)
	}

	// Get() must agree with what Discover returned.
	fetched, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fetched.State != "DISCOVERED" || fetched.RemotePath != outcome.Record.RemotePath {
		t.Fatalf("Get() = %+v, want it to match Discover's record", fetched)
	}
}

// FR-16 explicitly requires room for the case where a backend supplies none
// of the remote identity attributes. Discovering with an empty
// RemoteIdentity must succeed, not error, and must come back with every
// attribute reporting "unknown", not a zero value that looks like real data.
func TestDiscover_ZeroRemoteIdentityIsUnknownNotZero(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	artifact := testArtifact(t)

	outcome, err := j.Discover(ctx, artifact, "discover-1", "/incoming/backup.dump", RemoteIdentity{}, time.Now())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := outcome.Record.Remote
	if got.Size != nil {
		t.Fatalf("Remote.Size = %v, want nil", got.Size)
	}
	if got.ModTime != nil {
		t.Fatalf("Remote.ModTime = %v, want nil", got.ModTime)
	}
	if got.Hash != "" || got.HashAlg != "" || got.BackendID != "" {
		t.Fatalf("Remote = %+v, want every attribute empty", got)
	}
}

// This is the core crash-safety property the FR-9 issue asks for: a
// transition committed by one process, then replayed with the same
// idempotency key (as a retrying caller would after not observing the
// first call's result), must be recognised as already applied rather than
// applied a second time.
func TestRecordTransition_SameKeyReplayIsNotReapplied(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	artifact := testArtifact(t)

	if _, err := j.Discover(ctx, artifact, "discover-1", "/incoming/backup.dump", RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	transition := Transition{
		Artifact:   artifact,
		Key:        "transfer-attempt-1",
		From:       "DISCOVERED",
		To:         "TRANSFERRING",
		OccurredAt: time.Now(),
		Retry:      &RetryUpdate{Count: 1, LastError: ""},
	}

	first, err := j.RecordTransition(ctx, transition)
	if err != nil {
		t.Fatalf("first RecordTransition: %v", err)
	}
	if !first.Applied {
		t.Fatalf("first call: Applied = false, want true")
	}
	if first.Record.RetryCount != 1 {
		t.Fatalf("first call: RetryCount = %d, want 1", first.Record.RetryCount)
	}

	// Replay with the exact same Key and payload, as a crashed-and-retried
	// caller would.
	second, err := j.RecordTransition(ctx, transition)
	if err != nil {
		t.Fatalf("replayed RecordTransition: %v", err)
	}
	if second.Applied {
		t.Fatalf("replayed call: Applied = true, want false (already applied)")
	}
	if second.Record.RetryCount != 1 {
		t.Fatalf("replayed call: RetryCount = %d, want 1 (must not double-increment)", second.Record.RetryCount)
	}
	if second.Record.State != "TRANSFERRING" {
		t.Fatalf("replayed call: State = %q, want TRANSFERRING", second.Record.State)
	}
}

// The same property as above, but across an actual process boundary: close
// the journal (as a crash would leave the process gone, minus the graceful
// shutdown) and reopen it from the same file, then replay the same key. If
// WAL + synchronous=FULL is actually durable, the reopened journal must
// already have the transition and must refuse to double-apply it.
func TestRecordTransition_SurvivesCloseAndReopen(t *testing.T) {
	ctx := context.Background()
	artifact := testArtifact(t)
	path := filepath.Join(t.TempDir(), "journal.db")

	j1, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := j1.Discover(ctx, artifact, "discover-1", "/incoming/backup.dump", RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	transition := Transition{
		Artifact:   artifact,
		Key:        "transfer-attempt-1",
		From:       "DISCOVERED",
		To:         "TRANSFERRING",
		OccurredAt: time.Now(),
	}
	if _, err := j1.RecordTransition(ctx, transition); err != nil {
		t.Fatalf("RecordTransition: %v", err)
	}
	if err := j1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	j2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = j2.Close() }()

	rec, err := j2.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if rec.State != "TRANSFERRING" {
		t.Fatalf("State after reopen = %q, want TRANSFERRING", rec.State)
	}

	replay, err := j2.RecordTransition(ctx, transition)
	if err != nil {
		t.Fatalf("replayed RecordTransition after reopen: %v", err)
	}
	if replay.Applied {
		t.Fatalf("replayed call after reopen: Applied = true, want false")
	}
}

func TestRecordTransition_RejectsUnexpectedCurrentState(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	artifact := testArtifact(t)

	if _, err := j.Discover(ctx, artifact, "discover-1", "/incoming/backup.dump", RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// The artifact is DISCOVERED, not TRANSFERRED, so this must be refused.
	_, err := j.RecordTransition(ctx, Transition{
		Artifact:   artifact,
		Key:        "verify-attempt-1",
		From:       "TRANSFERRED",
		To:         "VERIFYING",
		OccurredAt: time.Now(),
	})
	if !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("RecordTransition error = %v, want ErrStateMismatch", err)
	}
}

func TestRecordTransition_UnknownArtifactIsRefused(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	artifact := testArtifact(t)

	_, err := j.RecordTransition(ctx, Transition{
		Artifact:   artifact,
		Key:        "transfer-attempt-1",
		From:       "DISCOVERED",
		To:         "TRANSFERRING",
		OccurredAt: time.Now(),
	})
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("RecordTransition error = %v, want ErrArtifactNotFound", err)
	}
}

func TestDiscover_DuplicateIdentityDifferentKeyIsRefused(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	artifact := testArtifact(t)

	if _, err := j.Discover(ctx, artifact, "discover-1", "/incoming/backup.dump", RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("first Discover: %v", err)
	}

	_, err := j.Discover(ctx, artifact, "discover-2", "/incoming/backup.dump", RemoteIdentity{}, time.Now())
	if !errors.Is(err, ErrAlreadyDiscovered) {
		t.Fatalf("second Discover error = %v, want ErrAlreadyDiscovered", err)
	}
}

func TestRecordTransition_ReusedKeyForDifferentTransitionIsRefused(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	artifact := testArtifact(t)

	if _, err := j.Discover(ctx, artifact, "shared-key", "/incoming/backup.dump", RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// Reusing "shared-key" for a transition to a different state than the
	// one it was first recorded against must be refused: that is not a
	// legitimate replay, it is a caller bug (or two unrelated attempts
	// colliding on the same key).
	_, err := j.RecordTransition(ctx, Transition{
		Artifact:   artifact,
		Key:        "shared-key",
		From:       "DISCOVERED",
		To:         "TRANSFERRING",
		OccurredAt: time.Now(),
	})
	if !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("RecordTransition error = %v, want ErrIdempotencyKeyReused", err)
	}
}

// The state column's CHECK constraint is the defense-in-depth this package
// promises: even though it deliberately does not own the FR-10 state enum in
// Go, an invalid state string must still be rejected by the schema rather
// than silently stored.
func TestRecordTransition_RejectsStateOutsideFR10Enum(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	artifact := testArtifact(t)

	_, err := j.RecordTransition(ctx, Transition{
		Artifact:   artifact,
		Key:        "discover-1",
		From:       "",
		To:         "NOT_A_REAL_STATE",
		OccurredAt: time.Now(),
		RemotePath: "/incoming/backup.dump",
	})
	if err == nil {
		t.Fatalf("RecordTransition with an invalid state: want an error, got none")
	}
}

// Full lifecycle walk exercising every payload kind FR-9 lists: transfer
// results, hashes, validation results, retry information, remote deletion
// status and retention classification should all round-trip through
// Record.
func TestRecordTransition_FullLifecycleRoundTrips(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	artifact := testArtifact(t)
	now := time.Now()

	must := func(o Outcome, err error) Outcome {
		t.Helper()
		if err != nil {
			t.Fatalf("RecordTransition: %v", err)
		}
		if !o.Applied {
			t.Fatalf("RecordTransition: Applied = false, want true")
		}
		return o
	}

	must(j.Discover(ctx, artifact, "k-discover", "/incoming/backup.dump", RemoteIdentity{}, now))

	partial := "/data/.partial/backup.dump.partial"
	must(j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "k-transferring", From: "DISCOVERED", To: "TRANSFERRING",
		OccurredAt: now, LocalPath: &partial,
	}))
	must(j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "k-transferred", From: "TRANSFERRING", To: "TRANSFERRED",
		OccurredAt: now, Transfer: &TransferResult{BytesTransferred: 12345, Checksummed: true},
	}))
	must(j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "k-verifying", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: now,
	}))
	verified := must(j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "k-verified", From: "VERIFYING", To: "VERIFIED", OccurredAt: now,
		Hashes:     &HashUpdate{Hash: "deadbeef", Alg: "sha256"},
		Validation: &ValidationUpdate{Passed: true, Detail: "pg_verifybackup: OK"},
	}))
	if verified.Record.LocalHash != "deadbeef" || verified.Record.LocalHashAlg != "sha256" {
		t.Fatalf("hashes did not round trip: %+v", verified.Record)
	}
	if verified.Record.ValidationPassed == nil || !*verified.Record.ValidationPassed {
		t.Fatalf("ValidationPassed = %v, want true", verified.Record.ValidationPassed)
	}
	if verified.Record.ValidationDetail != "pg_verifybackup: OK" {
		t.Fatalf("ValidationDetail = %q", verified.Record.ValidationDetail)
	}

	must(j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "k-committing", From: "VERIFIED", To: "COMMITTING", OccurredAt: now,
	}))
	final := "/data/postgres-primary/backup.dump"
	must(j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "k-committed", From: "COMMITTING", To: "COMMITTED",
		OccurredAt: now, LocalPath: &final,
		Retention: &RetentionUpdate{Tier: "daily", ExpiresAt: ptrTime(now.AddDate(0, 0, 7))},
	}))
	must(j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "k-delete-pending", From: "COMMITTED", To: "REMOTE_DELETE_PENDING", OccurredAt: now,
	}))
	complete := must(j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "k-complete", From: "REMOTE_DELETE_PENDING", To: "COMPLETE",
		OccurredAt: now, Deletion: &DeletionUpdate{DeletedAt: ptrTime(now)},
	}))

	rec := complete.Record
	if rec.State != "COMPLETE" {
		t.Fatalf("final State = %q, want COMPLETE", rec.State)
	}
	if rec.LocalPath != final {
		t.Fatalf("LocalPath = %q, want %q", rec.LocalPath, final)
	}
	if rec.Transfer == nil || rec.Transfer.BytesTransferred != 12345 || !rec.Transfer.Checksummed {
		t.Fatalf("Transfer = %+v", rec.Transfer)
	}
	if rec.RetentionTier != "daily" || rec.RetentionExpiresAt == nil {
		t.Fatalf("Retention = %q / %v", rec.RetentionTier, rec.RetentionExpiresAt)
	}
	if rec.RemoteDeletedAt == nil {
		t.Fatalf("RemoteDeletedAt = nil, want set")
	}
}

func TestListByState_ScopesToOneState(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	names := []string{"a.dump", "b.dump", "c.dump"}
	for i, name := range names {
		artifact, err := model.NewArtifactID(set, name)
		if err != nil {
			t.Fatalf("NewArtifactID: %v", err)
		}
		if _, err := j.Discover(ctx, artifact, "discover-"+name, "/incoming/"+name, RemoteIdentity{}, time.Now()); err != nil {
			t.Fatalf("Discover(%s): %v", name, err)
		}
		if i == 0 {
			if _, err := j.RecordTransition(ctx, Transition{
				Artifact: artifact, Key: "transfer-" + name, From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: time.Now(),
			}); err != nil {
				t.Fatalf("RecordTransition(%s): %v", name, err)
			}
		}
	}

	discovered, err := j.ListByState(ctx, "DISCOVERED")
	if err != nil {
		t.Fatalf("ListByState(DISCOVERED): %v", err)
	}
	if len(discovered) != 2 {
		t.Fatalf("len(discovered) = %d, want 2", len(discovered))
	}

	transferring, err := j.ListByState(ctx, "TRANSFERRING")
	if err != nil {
		t.Fatalf("ListByState(TRANSFERRING): %v", err)
	}
	if len(transferring) != 1 {
		t.Fatalf("len(transferring) = %d, want 1", len(transferring))
	}
}

// FR-7: retention and lifecycle calculations must operate independently per
// backup set. ListByBackupSet must not leak artifacts from a different set.
func TestListByBackupSet_DoesNotCrossSets(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	setA, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	setB, err := model.NewBackupSetID("staging", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	artifactA, err := model.NewArtifactID(setA, "a.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	artifactB, err := model.NewArtifactID(setB, "b.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}

	if _, err := j.Discover(ctx, artifactA, "discover-a", "/incoming/a.dump", RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("Discover A: %v", err)
	}
	if _, err := j.Discover(ctx, artifactB, "discover-b", "/incoming/b.dump", RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("Discover B: %v", err)
	}

	recs, err := j.ListByBackupSet(ctx, setA)
	if err != nil {
		t.Fatalf("ListByBackupSet: %v", err)
	}
	if len(recs) != 1 || recs[0].Artifact != artifactA {
		t.Fatalf("ListByBackupSet(setA) = %+v, want only artifactA", recs)
	}
}

// Two goroutines racing to record the exact same transition (the shape a
// retrying caller and, say, a supervisor-restarted duplicate process would
// produce) must still apply it exactly once: one call actually mutates the
// row, and the other is recognised as a replay, never a state mismatch and
// never a doubled RetryCount.
func TestRecordTransition_ConcurrentSameKeyAppliesExactlyOnce(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	artifact := testArtifact(t)

	if _, err := j.Discover(ctx, artifact, "discover-1", "/incoming/backup.dump", RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	transition := Transition{
		Artifact:   artifact,
		Key:        "transfer-attempt-1",
		From:       "DISCOVERED",
		To:         "TRANSFERRING",
		OccurredAt: time.Now(),
		Retry:      &RetryUpdate{Count: 1},
	}

	const goroutines = 8
	var applied int64
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			outcome, err := j.RecordTransition(ctx, transition)
			errs[i] = err
			if err == nil && outcome.Applied {
				atomic.AddInt64(&applied, 1)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: RecordTransition: %v", i, err)
		}
	}
	if applied != 1 {
		t.Fatalf("applied = %d, want exactly 1", applied)
	}

	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1 (must not be incremented once per goroutine)", rec.RetryCount)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
