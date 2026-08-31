package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/retry"
)

// --- fakes ---
//
// These fakes are deliberately small re-implementations rather than the
// real *state.Journal/rclone adapter: transfer_test.go's own file scope
// stops at this package, and a real *state.Journal would drag in SQLite for
// tests that only care about the state-machine and filesystem behaviour
// this file owns.

// fakeTransferJournal is a minimal in-memory stand-in for internal/state's
// real journal, faithful to the two rules Transfer actually depends on:
// idempotency-key replay, and refusing an update whose From doesn't match
// the row's current state.
type fakeTransferJournal struct {
	mu       sync.Mutex
	exists   bool
	rec      state.Record
	seen     map[string]state.Outcome
	recorded []state.Transition

	getErr        error
	failRecordFor string // if non-empty, RecordTransition to this To-state fails
}

func newFakeTransferJournal(artifact model.ArtifactID, remotePath string) *fakeTransferJournal {
	return &fakeTransferJournal{
		exists: true,
		rec: state.Record{
			Artifact:   artifact,
			RemotePath: remotePath,
			State:      string(Discovered),
		},
		seen: make(map[string]state.Outcome),
	}
}

func (f *fakeTransferJournal) Get(_ context.Context, _ model.ArtifactID) (state.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return state.Record{}, f.getErr
	}
	if !f.exists {
		return state.Record{}, errors.New("fake journal: artifact not found")
	}
	return f.rec, nil
}

// LastEnteredAt reports "never entered". This fake is only ever used by
// Transfer's own tests, which never reach remotedelete.go's WP3.2 gate, and
// "no evidence" is the only honest answer a fake with no transition log can
// give a check that decides whether a remote copy may be destroyed.
func (f *fakeTransferJournal) LastEnteredAt(context.Context, model.ArtifactID, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

// LastTransition is unused by the step under test, and the safety property
// it exists for (issue #220's reinstatement forfeiture) is proved against a
// real journal in remotedelete_reinstate_test.go, not here. Reporting "no
// such edge" is the honest answer for a fake that records no log at all.
func (f *fakeTransferJournal) LastTransition(context.Context, model.ArtifactID, string, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func (f *fakeTransferJournal) RecordTransition(_ context.Context, t state.Transition) (state.Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.recorded = append(f.recorded, t)

	if f.failRecordFor != "" && t.To == f.failRecordFor {
		return state.Outcome{}, fmt.Errorf("fake journal: forced failure recording %s", t.To)
	}
	if out, ok := f.seen[t.Key]; ok {
		return state.Outcome{Applied: false, Record: out.Record}, nil
	}
	if f.exists && f.rec.State != t.From {
		return state.Outcome{}, fmt.Errorf("fake journal: state mismatch: have %q, want from %q", f.rec.State, t.From)
	}

	f.rec.State = t.To
	if t.LocalPath != nil {
		f.rec.LocalPath = *t.LocalPath
	}
	if t.Transfer != nil {
		f.rec.Transfer = t.Transfer
	}
	f.exists = true

	out := state.Outcome{Applied: true, Record: f.rec}
	f.seen[t.Key] = out
	return out, nil
}

func (f *fakeTransferJournal) currentState() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rec.State
}

func (f *fakeTransferJournal) transitionsTo(to State) []state.Transition {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []state.Transition
	for _, t := range f.recorded {
		if t.To == string(to) {
			out = append(out, t)
		}
	}
	return out
}

// fakeTransport is a minimal transport.Transport. Only CopyToLocal is ever
// exercised by Transfer; the rest exist solely to satisfy the interface.
type fakeTransport struct {
	mu        sync.Mutex
	calls     int
	copyFunc  func(ctx context.Context, source transport.Source, remotePath, localPartialPath string) (transport.TransferResult, error)
	callStart chan struct{} // if non-nil, sent to (non-blocking best-effort) on every call
}

func (f *fakeTransport) List(context.Context, transport.Source) ([]transport.RemoteArtifact, error) {
	return nil, errors.New("fakeTransport: List not used")
}

func (f *fakeTransport) Stat(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
	return transport.RemoteArtifact{}, errors.New("fakeTransport: Stat not used")
}

func (f *fakeTransport) CopyToLocal(ctx context.Context, source transport.Source, remotePath, localPartialPath string) (transport.TransferResult, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.callStart != nil {
		select {
		case f.callStart <- struct{}{}:
		default:
		}
	}
	return f.copyFunc(ctx, source, remotePath, localPartialPath)
}

func (f *fakeTransport) RemoteHash(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
	return "", errors.New("fakeTransport: RemoteHash not used")
}

func (f *fakeTransport) DeleteRemote(context.Context, transport.Source, string) error {
	return errors.New("fakeTransport: DeleteRemote not used")
}

func (f *fakeTransport) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// writingCopy returns a copyFunc that writes content to localPartialPath,
// simulating a real (successful) rclone copy.
func writingCopy(content []byte) func(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
	return func(_ context.Context, _ transport.Source, _ string, localPartialPath string) (transport.TransferResult, error) {
		if err := os.WriteFile(localPartialPath, content, 0o600); err != nil {
			return transport.TransferResult{}, err
		}
		return transport.TransferResult{BytesTransferred: int64(len(content)), Checksummed: false}, nil
	}
}

func testArtifact(t *testing.T) model.ArtifactID {
	t.Helper()
	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	id, err := model.NewArtifactID(set, "backup-2026-08-27.dump.zst")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	return id
}

func fastPolicy() retry.Policy {
	return retry.Policy{
		BaseDelay:   5 * time.Millisecond,
		MaxDelay:    20 * time.Millisecond,
		Multiplier:  2,
		MaxAttempts: 0,
	}
}

// --- the nominal path ---

func TestTransferNominalSequenceCopiesToPartialThenRecordsTransferred(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()
	content := []byte("dump-bytes")

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	tr := &fakeTransport{copyFunc: writingCopy(content)}

	out, err := Transfer(context.Background(), Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact:   artifact,
		LocalDir:   dir,
		AttemptKey: "attempt-1",
	})
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if out.Record.State != string(Transferred) {
		t.Fatalf("final state = %q, want %q", out.Record.State, Transferred)
	}
	if tr.callCount() != 1 {
		t.Fatalf("CopyToLocal called %d times, want 1", tr.callCount())
	}

	wantPartial := filepath.Join(dir, "backup-2026-08-27.dump.zst.partial")
	got, err := os.ReadFile(wantPartial)
	if err != nil {
		t.Fatalf("reading .partial file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf(".partial content = %q, want %q", got, content)
	}

	// FR-12: the final (non-.partial) name must never come into existence
	// as a side effect of this step. Renaming to it is a different step's
	// job (durable commit, FR-14), entirely out of Transfer's business.
	finalName := filepath.Join(dir, "backup-2026-08-27.dump.zst")
	if _, err := os.Stat(finalName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final-name file exists after Transfer (err=%v); Transfer must never create it", err)
	}

	transferring := j.transitionsTo(Transferring)
	if len(transferring) != 1 {
		t.Fatalf("recorded %d TRANSFERRING transitions, want 1", len(transferring))
	}
	if transferring[0].LocalPath == nil || *transferring[0].LocalPath != wantPartial {
		t.Fatalf("TRANSFERRING LocalPath = %v, want %q", transferring[0].LocalPath, wantPartial)
	}

	transferred := j.transitionsTo(Transferred)
	if len(transferred) != 1 {
		t.Fatalf("recorded %d TRANSFERRED transitions, want 1", len(transferred))
	}
	if transferred[0].Transfer == nil || transferred[0].Transfer.BytesTransferred != int64(len(content)) {
		t.Fatalf("TRANSFERRED Transfer result = %+v, want BytesTransferred=%d", transferred[0].Transfer, len(content))
	}
}

func TestTransferPassesTheExactRemotePathRecordedAtDiscovery(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()
	const remote = "backups/nested/backup-2026-08-27.dump.zst"

	j := newFakeTransferJournal(artifact, remote)
	var gotRemote string
	tr := &fakeTransport{copyFunc: func(_ context.Context, _ transport.Source, remotePath, localPartialPath string) (transport.TransferResult, error) {
		gotRemote = remotePath
		return transport.TransferResult{}, os.WriteFile(localPartialPath, nil, 0o600)
	}}

	if _, err := Transfer(context.Background(), Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-1",
	}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if gotRemote != remote {
		t.Fatalf("CopyToLocal remotePath = %q, want %q (the path recorded at discovery)", gotRemote, remote)
	}
}

// --- FR-12's hardest case: a final-name collision must never be clobbered ---

func TestFinalNameCollisionRefusesAndLeavesTheExistingFileUntouched(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()

	finalName := filepath.Join(dir, "backup-2026-08-27.dump.zst")
	knownGood := []byte("PRE-EXISTING-KNOWN-GOOD-BACKUP")
	if err := os.WriteFile(finalName, knownGood, 0o600); err != nil {
		t.Fatalf("seeding existing final file: %v", err)
	}

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	tr := &fakeTransport{copyFunc: writingCopy([]byte("this must never be written"))}

	_, err := Transfer(context.Background(), Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-1",
	})

	// Loud: a well-typed, discoverable error, not a generic one.
	var collision *FinalNameCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("err = %v, want a *FinalNameCollisionError", err)
	}
	if collision.Path != finalName {
		t.Fatalf("collision.Path = %q, want %q", collision.Path, finalName)
	}

	// The copy must never even be attempted.
	if tr.callCount() != 0 {
		t.Fatalf("CopyToLocal was called %d times; a collision must be refused before any copy is attempted", tr.callCount())
	}

	// The existing file is untouched, byte for byte.
	got, readErr := os.ReadFile(finalName)
	if readErr != nil {
		t.Fatalf("reading final file after refusal: %v", readErr)
	}
	if string(got) != string(knownGood) {
		t.Fatalf("existing final file was modified: got %q, want %q (it must never be clobbered)", got, knownGood)
	}

	// No .partial file should have been created either: the refusal
	// happens before TRANSFERRING (and therefore before any partial-path
	// cleanup or copy) is even recorded.
	if _, statErr := os.Stat(finalName + partialSuffix); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf(".partial file exists after a collision refusal (err=%v); none should have been created", statErr)
	}

	// Loud: durably recorded, not a silent skip an operator would never see.
	if j.currentState() != string(Failed) {
		t.Fatalf("journal state = %q, want %q after a collision refusal", j.currentState(), Failed)
	}
	failed := j.transitionsTo(Failed)
	if len(failed) != 1 {
		t.Fatalf("recorded %d FAILED transitions, want 1", len(failed))
	}
	if failed[0].Detail == "" {
		t.Fatal("FAILED transition recorded with no Detail; the collision must be explained in the journal")
	}
}

func TestFinalNameCollisionStillReturnsLoudlyEvenIfRecordingFailedAlsoFails(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()
	finalName := filepath.Join(dir, "backup-2026-08-27.dump.zst")
	if err := os.WriteFile(finalName, []byte("known-good"), 0o600); err != nil {
		t.Fatalf("seeding existing final file: %v", err)
	}

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	j.failRecordFor = string(Failed)
	tr := &fakeTransport{copyFunc: writingCopy([]byte("must never be written"))}

	_, err := Transfer(context.Background(), Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-1",
	})
	if err == nil {
		t.Fatal("Transfer returned nil error when both the collision and recording FAILED failed")
	}
	var collision *FinalNameCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("err = %v, want it to still carry a *FinalNameCollisionError even when recording FAILED failed too", err)
	}
	if tr.callCount() != 0 {
		t.Fatalf("CopyToLocal was called %d times, want 0", tr.callCount())
	}
}

// --- the state machine guardrail: Transfer must go through Advance ---

func TestTransferRefusesAnArtifactPastItsOwnStage(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	j.rec.State = string(Committed) // already far past TRANSFERRING
	tr := &fakeTransport{copyFunc: writingCopy([]byte("must never be written"))}

	_, err := Transfer(context.Background(), Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-1",
	})
	if err == nil {
		t.Fatal("Transfer allowed COMMITTED -> TRANSFERRING")
	}
	if tr.callCount() != 0 {
		t.Fatalf("CopyToLocal was called %d times, want 0", tr.callCount())
	}
	if j.currentState() != string(Committed) {
		t.Fatalf("journal state changed to %q; an illegal move must leave it exactly as it was", j.currentState())
	}
}

// --- orphaned .partial on restart ---

func TestOrphanedPartialFromACrashedAttemptIsDiscardedNotResumed(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()

	// Simulate a prior attempt that crashed mid-copy: the journal already
	// recorded TRANSFERRING (with the .partial LocalPath), but the .partial
	// file on disk holds only truncated garbage from the interrupted copy.
	partial := filepath.Join(dir, "backup-2026-08-27.dump.zst.partial")
	if err := os.WriteFile(partial, []byte("TRUNCATED-GARBAGE-FROM-A-CRASH"), 0o600); err != nil {
		t.Fatalf("seeding orphaned .partial: %v", err)
	}

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	j.rec.State = string(Transferring)
	j.rec.LocalPath = partial

	goodContent := []byte("this-run-actually-succeeds")
	tr := &fakeTransport{copyFunc: writingCopy(goodContent)}

	// The caller resumes with the SAME AttemptKey it used before the crash,
	// which is the whole point of AttemptKey: a resumed call reproduces the
	// same transition keys.
	out, err := Transfer(context.Background(), Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-1",
	})
	if err != nil {
		t.Fatalf("Transfer did not resume past an orphaned .partial: %v", err)
	}
	if out.Record.State != string(Transferred) {
		t.Fatalf("final state = %q, want %q", out.Record.State, Transferred)
	}
	if tr.callCount() != 1 {
		t.Fatalf("CopyToLocal called %d times, want 1", tr.callCount())
	}

	got, err := os.ReadFile(partial)
	if err != nil {
		t.Fatalf("reading .partial after resume: %v", err)
	}
	if string(got) != string(goodContent) {
		t.Fatalf(".partial content = %q, want %q; the stale garbage must be discarded, not left in place or blended", got, goodContent)
	}
}

func TestOrphanedPartialDoesNotBlockOrSkipAFreshCopy(t *testing.T) {
	// The specific "quiet outage" the task calls out: an orphaned .partial
	// existing on disk must never be mistaken for "this artifact is already
	// done" and cause Transfer to short-circuit without actually calling
	// the transport.
	artifact := testArtifact(t)
	dir := t.TempDir()
	partial := filepath.Join(dir, "backup-2026-08-27.dump.zst.partial")
	if err := os.WriteFile(partial, []byte("leftover"), 0o600); err != nil {
		t.Fatalf("seeding orphaned .partial: %v", err)
	}

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	tr := &fakeTransport{copyFunc: writingCopy([]byte("fresh"))}

	if _, err := Transfer(context.Background(), Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-1",
	}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if tr.callCount() != 1 {
		t.Fatalf("CopyToLocal called %d times, want 1; a pre-existing .partial must never be treated as already-done", tr.callCount())
	}
}

// --- retry policy wiring for Transient failures ---

func TestTransferRetriesTransientFailuresThenSucceeds(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	const failuresBeforeSuccess = 2
	attempt := 0
	tr := &fakeTransport{copyFunc: func(_ context.Context, _ transport.Source, _ string, localPartialPath string) (transport.TransferResult, error) {
		attempt++
		if attempt <= failuresBeforeSuccess {
			return transport.TransferResult{}, transport.NewError(transport.Transient, "copy_to_local", errors.New("connection reset"))
		}
		return transport.TransferResult{BytesTransferred: 5}, os.WriteFile(localPartialPath, []byte("hello"), 0o600)
	}}

	out, err := Transfer(context.Background(), Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-1", Policy: fastPolicy(),
	})
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if out.Record.State != string(Transferred) {
		t.Fatalf("final state = %q, want %q", out.Record.State, Transferred)
	}
	if tr.callCount() != failuresBeforeSuccess+1 {
		t.Fatalf("CopyToLocal called %d times, want %d", tr.callCount(), failuresBeforeSuccess+1)
	}
}

func TestTransferDoesNotRetryAPermanentFailure(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	tr := &fakeTransport{copyFunc: func(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
		return transport.TransferResult{}, transport.NewError(transport.PermissionDenied, "copy_to_local", errors.New("permission denied"))
	}}

	_, err := Transfer(context.Background(), Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-1", Policy: fastPolicy(),
	})
	if err == nil {
		t.Fatal("Transfer succeeded despite a permanent copy failure")
	}
	if tr.callCount() != 1 {
		t.Fatalf("CopyToLocal called %d times, want 1 (a non-Transient failure must not be retried)", tr.callCount())
	}
	if j.currentState() != string(Failed) {
		t.Fatalf("journal state = %q, want %q", j.currentState(), Failed)
	}
	category, ok := transport.CategoryOf(err)
	if !ok || category != transport.PermissionDenied {
		t.Fatalf("CategoryOf(err) = (%v, %v), want (PermissionDenied, true)", category, ok)
	}
}

// --- cancellation must never claim TRANSFERRED ---

func TestCancellationDuringRetryBackoffLeavesJournalAtTransferringNotTransferred(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	started := make(chan struct{}, 1)
	tr := &fakeTransport{
		callStart: started,
		copyFunc: func(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
			return transport.TransferResult{}, transport.NewError(transport.Transient, "copy_to_local", errors.New("connection reset"))
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started // wait for the first attempt to actually happen
		cancel()  // then cancel while retry.Do is waiting out the backoff
	}()

	_, err := Transfer(ctx, Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact:   artifact,
		LocalDir:   dir,
		AttemptKey: "attempt-1",
		Policy: retry.Policy{
			BaseDelay:  200 * time.Millisecond, // long enough for cancel() to win the race
			MaxDelay:   200 * time.Millisecond,
			Multiplier: 2,
		},
	})
	if err == nil {
		t.Fatal("Transfer succeeded despite context cancellation")
	}
	category, ok := transport.CategoryOf(err)
	if !ok || category != transport.Cancelled {
		t.Fatalf("CategoryOf(err) = (%v, %v), want (Cancelled, true)", category, ok)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false, want true: %v", err)
	}

	// The hard requirement: never TRANSFERRED.
	if j.currentState() == string(Transferred) {
		t.Fatal("journal claims TRANSFERRED after a cancelled transfer")
	}
	if len(j.transitionsTo(Transferred)) != 0 {
		t.Fatal("a TRANSFERRED transition was recorded despite cancellation")
	}
	// And deliberately not FAILED either: cancellation is a stop request,
	// not a verdict that the artifact is broken. It stays TRANSFERRING so a
	// later attempt resumes cleanly.
	if j.currentState() != string(Transferring) {
		t.Fatalf("journal state = %q, want %q (left in place for a later retry)", j.currentState(), Transferring)
	}
}

func TestCancellationBeforeTransferStartsIsRefusedWithoutTouchingTheJournal(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	tr := &fakeTransport{copyFunc: writingCopy([]byte("must never run"))}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Transfer(ctx, Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-1",
	})
	if err == nil {
		t.Fatal("Transfer proceeded despite an already-cancelled context")
	}
	if tr.callCount() != 0 {
		t.Fatalf("CopyToLocal was called %d times, want 0", tr.callCount())
	}
	if len(j.recorded) != 0 {
		t.Fatalf("the journal was written to despite an already-cancelled context: %+v", j.recorded)
	}
}

// --- input validation ---

func TestTransferRejectsMissingRequiredParams(t *testing.T) {
	artifact := testArtifact(t)
	j := newFakeTransferJournal(artifact, "backups/x")
	tr := &fakeTransport{copyFunc: writingCopy(nil)}

	cases := []struct {
		name string
		deps Deps
		p    TransferParams
	}{
		{"no journal", Deps{Transport: tr}, TransferParams{Artifact: artifact, LocalDir: t.TempDir(), AttemptKey: "a"}},
		{"no transport", Deps{Journal: j}, TransferParams{Artifact: artifact, LocalDir: t.TempDir(), AttemptKey: "a"}},
		{"no local dir", Deps{Journal: j, Transport: tr}, TransferParams{Artifact: artifact, AttemptKey: "a"}},
		{"no attempt key", Deps{Journal: j, Transport: tr}, TransferParams{Artifact: artifact, LocalDir: t.TempDir()}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Transfer(context.Background(), c.deps, c.p); err == nil {
				t.Fatalf("Transfer accepted %s", c.name)
			}
		})
	}
}
