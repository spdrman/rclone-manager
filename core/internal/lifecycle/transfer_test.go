// These cover FR-11 and FR-12's transfer step, and unlike the delete and
// commit suites they run against in-memory fakes rather than a real journal.
//
// That is a deliberate trade. Transfer's interesting behaviour is the state
// sequence and the local filenames, not journal history, so a fake journal
// that enforces the two rules Transfer genuinely depends on, idempotency-key
// replay and refusing a mismatched From, is enough. What it buys is that a
// test can make a write fail on demand, which is how the "refuse loudly even
// when recording the refusal also fails" case gets covered at all.
//
// The .partial name is the thread running through the file. It is not a
// naming convention, it is the mechanism: nothing downstream may treat a
// .partial as a restore point, an orphaned one from a crashed attempt is
// discarded rather than resumed, and the final name is checked for an
// occupant BEFORE any bytes move. Several tests below assert on which of the
// two files exists after a failure, and that is why.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	// applied is the subset of recorded that actually went through, which
	// is what the two log queries below answer from. recorded holds every
	// write that was ASKED for, refused ones included, because several
	// tests assert on whether a write was attempted at all; a refused
	// write never reaches the real state_transitions table, so answering
	// LastTransition from it would invent history.
	applied []state.Transition

	getErr        error
	failRecordFor string // if non-empty, RecordTransition to this To-state fails

	// afterGet, when set, runs immediately after Get has answered. It is
	// the only way to stand in for another attempt moving the row in the
	// window between this step reading where the artifact is and writing
	// where it is going, which is a window every lifecycle step has and
	// which issue #570 is about the far end of.
	afterGet func()
}

// newFakeTransferJournal starts an artifact at DISCOVERED with a recorded
// remote path, which is where the pipeline hands one to Transfer.
//
// The remote path is set here rather than left empty because one test
// asserts Transfer uses the path from the JOURNAL rather than one it
// recomposed from the artifact name, and that assertion needs the two to be
// distinguishable.
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

// Get returns the current row, or the configured failure. The getErr hook is
// what lets a test make the journal unreachable at the moment Transfer looks
// the artifact up, which is a different failure from a write failing and
// leads somewhere different.
func (f *fakeTransferJournal) Get(_ context.Context, _ model.ArtifactID) (state.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return state.Record{}, f.getErr
	}
	if !f.exists {
		return state.Record{}, errors.New("fake journal: artifact not found")
	}
	rec := f.rec
	if hook := f.afterGet; hook != nil {
		f.mu.Unlock()
		hook()
		f.mu.Lock()
	}
	return rec, nil
}

// LastEnteredAt answers from the applied log, the way the real query does:
// the most recent write INTO st from a different state, ignoring same-state
// writes. That exclusion is the whole point of the query and it is
// load-bearing here, because the bound in failCopy dates its own same-state
// charges against the moment the artifact genuinely entered TRANSFERRING.
// A fake that reported "never" (which this used to, when nothing in this
// file asked) would leave that comparison untested and passing.
func (f *fakeTransferJournal) LastEnteredAt(_ context.Context, _ model.ArtifactID, st string) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.applied) - 1; i >= 0; i-- {
		if t := f.applied[i]; t.To == st && t.From != st {
			return t.OccurredAt, true, nil
		}
	}
	return time.Time{}, false, nil
}

// LastTransition answers the narrower question from the same log: when this
// artifact last recorded one exact from -> to edge.
func (f *fakeTransferJournal) LastTransition(_ context.Context, _ model.ArtifactID, from, to string) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.applied) - 1; i >= 0; i-- {
		if t := f.applied[i]; t.From == from && t.To == to {
			return t.OccurredAt, true, nil
		}
	}
	return time.Time{}, false, nil
}

// RecordTransition implements the two journal rules Transfer depends on,
// and nothing else.
//
// Key replay comes first: a repeated key returns the earlier outcome without
// applying anything, which is what makes a retried attempt converge. The
// From check comes second and is what catches a caller acting on a stale
// idea of where the artifact is. failRecordFor is the hook for the tests
// about a refusal that cannot be recorded.
func (f *fakeTransferJournal) RecordTransition(_ context.Context, t state.Transition) (state.Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.recorded = append(f.recorded, t)

	if f.failRecordFor != "" && t.To == f.failRecordFor {
		return state.Outcome{}, fmt.Errorf("fake journal: forced failure recording %s", t.To)
	}
	if _, ok := f.seen[t.Key]; ok {
		// The row as it stands NOW, not as it stood when the key was
		// first seen. That is what the real journal does (it re-reads by
		// row id inside the replay's own transaction) and the difference
		// is the whole of issue #570's silent window: a replay that
		// echoed the old snapshot would tell a caller the artifact is
		// still where this attempt left it no matter where it has got to.
		return state.Outcome{Applied: false, Record: f.rec}, nil
	}
	if f.exists && f.rec.State != t.From {
		// Joined to the real sentinel rather than spelled as a fresh
		// error: production classifies this refusal by identity (issue
		// #570's supersededBy asks errors.Is), so a fake that reports the
		// same situation under a different identity would leave that
		// classification untested here and passing everywhere.
		return state.Outcome{}, fmt.Errorf("%w: fake journal: have %q, want from %q", state.ErrStateMismatch, f.rec.State, t.From)
	}

	f.rec.State = t.To
	if t.LocalPath != nil {
		f.rec.LocalPath = *t.LocalPath
	}
	if t.Transfer != nil {
		f.rec.Transfer = t.Transfer
	}
	// FR-22's counter, which the real journal writes from the same field.
	// The bound in failCopy reads it back on the next attempt, so a fake
	// that dropped it would let a budget appear to work while never
	// counting anything.
	if t.Retry != nil {
		f.rec.RetryCount = t.Retry.Count
		f.rec.LastError = t.Retry.LastError
	}
	f.exists = true
	f.applied = append(f.applied, t)

	out := state.Outcome{Applied: true, Record: f.rec}
	f.seen[t.Key] = out
	return out, nil
}

// forceState moves the row without recording anything, which is how a test
// stands in for a SECOND attempt (in this process or another) that carried
// the same artifact somewhere else while the attempt under test was busy.
// Going through RecordTransition would make it this attempt's own write.
func (f *fakeTransferJournal) forceState(s State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec.State = string(s)
}

// forceRetryCount sets FR-22's counter without recording anything, which is
// how a test stands in for an artifact that has already been round the
// houses (operator releases from quarantine and issue #419's stalled
// verifications both move it, and it is never reset).
func (f *fakeTransferJournal) forceRetryCount(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec.RetryCount = n
}

// currentState reads the row under the lock, for tests asserting where the
// artifact ended up. The lock matters because the cancellation tests have a
// goroutine in flight when this is called.
func (f *fakeTransferJournal) currentState() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rec.State
}

// row copies the record under the lock, for tests asserting on the
// bookkeeping (FR-22's counter and last error) rather than only the state.
func (f *fakeTransferJournal) row() state.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rec
}

// transitionsTo returns every recorded write into one state.
//
// It returns all of them rather than the last, because several tests assert
// there was exactly ONE: a second TRANSFERRED write for the same attempt
// would mean the idempotency key was not doing its job, and only counting
// shows that.
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

// List fails. Transfer copies one named object; it never enumerates.
func (f *fakeTransport) List(context.Context, transport.Source) ([]transport.RemoteArtifact, error) {
	return nil, errors.New("fakeTransport: List not used")
}

// Stat fails. The identity was captured at discovery and re-checked at
// delete time, not here, so a Stat during transfer would be a request nobody
// accounted for.
func (f *fakeTransport) Stat(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
	return transport.RemoteArtifact{}, errors.New("fakeTransport: Stat not used")
}

// CopyToLocal counts its calls, optionally signals that it has started, and
// then does whatever the test asked.
//
// The count is how the retry tests distinguish "retried three times" from
// "failed once"; callStart is how the cancellation tests wait until the copy
// is genuinely in flight before cancelling, rather than racing the goroutine
// with a sleep. The send is non-blocking so a test that does not read the
// channel cannot deadlock the transport.
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

// RemoteHash fails. Hashing belongs to verification, one state later.
func (f *fakeTransport) RemoteHash(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
	return "", errors.New("fakeTransport: RemoteHash not used")
}

// DeleteRemote fails, and this is the one that would matter most: transfer
// is a copy, never a move, and a transport whose delete quietly succeeded
// would let that distinction erode without a test noticing.
func (f *fakeTransport) DeleteRemote(context.Context, transport.Source, string) error {
	return errors.New("fakeTransport: DeleteRemote not used")
}

// callCount reads the counter under the lock, since the retry and
// cancellation tests read it while a copy may still be running.
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
		return transport.TransferResult{BytesTransferred: int64(len(content))}, nil
	}
}

// testArtifact builds the one artifact this file uses, through the real
// constructors so its name is one the pipeline could genuinely produce. The
// name has two extensions on purpose: the .partial suffix is appended to the
// whole basename, not substituted for an extension, and a single-extension
// name would not show the difference.
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

// fastPolicy shrinks the retry schedule to milliseconds.
//
// MaxAttempts is left at 0, meaning the package default, because the retry
// tests are about WHICH failures are retried rather than how many times, and
// pinning a count here would quietly duplicate a policy decision that lives
// in the transport package.
func fastPolicy() retry.Policy {
	return retry.Policy{
		BaseDelay:   5 * time.Millisecond,
		MaxDelay:    20 * time.Millisecond,
		Multiplier:  2,
		MaxAttempts: 0,
	}
}

// --- the nominal path ---

// TestTransferNominalSequenceCopiesToPartialThenRecordsTransferred is the
// happy path, and it asserts the SEQUENCE rather than only the end state.
//
// The order is the safety property: TRANSFERRING is recorded before any
// bytes move, so a crash mid-copy leaves a journal that says a transfer was
// underway and a .partial that a later attempt knows to discard. An
// implementation that copied first and recorded afterwards would reach the
// same final state and leave nothing to explain an orphaned file.
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

// TestTransferPassesTheExactRemotePathRecordedAtDiscovery pins that the
// path comes out of the journal rather than being recomposed.
//
// Recursion made this real: an artifact is named by its basename but lives
// at a full path, so a transfer that rebuilt the path from the identity
// would fetch from the wrong directory whenever a producer nests its output,
// and would do so silently because the bytes it copied would be a perfectly
// valid file.
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

// TestFinalNameCollisionRefusesAndLeavesTheExistingFileUntouched is FR-12's
// collision rule checked at the earliest point it can be.
//
// The check happens before TRANSFERRING is recorded and before any bytes
// move, which is what the assertions are really about: the existing file is
// byte-for-byte unchanged, and no bandwidth was spent on a transfer that
// could only have ended in a refusal. The existing file might be a
// known-good backup, and this package has no way to tell.
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

// TestFinalNameCollisionStillReturnsLoudlyEvenIfRecordingFailedAlsoFails
// covers the compound failure: a collision, and then the journal refusing
// the write that would record it.
//
// The rule is that the caller still hears about the collision. A function
// that returned only the journal error would report a database problem for
// what is actually a file sitting where a backup is about to go, and the
// operator would go and look at the wrong thing.
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

// TestTransferRefusesAnArtifactPastItsOwnStage covers the stale-caller
// case: something asks for a transfer of an artifact that has already moved
// on.
//
// Re-transferring is not harmless. It would overwrite the .partial of an
// artifact that may already be verified, so the refusal is about protecting
// work that has been done rather than about tidiness.
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

// TestOrphanedPartialFromACrashedAttemptIsDiscardedNotResumed pins the
// decision not to resume.
//
// A .partial left by a killed process has unknown contents: it may be
// truncated, or it may be a complete copy of an object that has since
// changed. Appending to it would produce a file that passes a size check and
// contains two halves of different objects, so the only safe reading is that
// it is residue. Discarding costs bandwidth and buys the ability to say what
// the bytes are.
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

// TestOrphanedPartialDoesNotBlockOrSkipAFreshCopy is the other half: having
// discarded the residue, the transfer must actually happen.
//
// Without this, an implementation that treated an existing .partial as a
// reason to refuse, or as evidence the artifact was already transferred,
// would satisfy the test above while leaving the artifact permanently
// stuck.
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

// TestTransferRetriesTransientFailuresThenSucceeds pins that a classified
// transient failure is retried inside one call rather than surfacing as a
// failed attempt.
//
// It matters because the alternative is not merely slower. A dropped
// connection that ended the attempt would count against the artifact's retry
// budget and eventually record FAILED, which is a verdict about the backup
// for something that was only ever a fact about the network.
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

// TestTransferDoesNotRetryAPermanentFailure is the necessary companion. A
// retry loop with no classification would sit re-attempting a
// permission-denied for the whole schedule, on every artifact, turning one
// misconfiguration into a pass that never finishes.
//
// The call count is the assertion: exactly one attempt.
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

// TestCancellationDuringRetryBackoffLeavesJournalAtTransferringNotTransferred
// covers a shutdown landing in the sleep between attempts, which is where a
// retrying transfer spends most of its time.
//
// The artifact has to stay at TRANSFERRING. It is an honest description: a
// transfer was underway and was interrupted, and the next run resumes from
// there. Recording TRANSFERRED would claim bytes arrived that did not, and
// recording FAILED would blame the artifact for the operator stopping the
// process.
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

// TestCancellationBeforeTransferStartsIsRefusedWithoutTouchingTheJournal is
// the same principle one step earlier: a context already cancelled when
// Transfer is called must leave no trace at all.
//
// During a shutdown this is the common case, once per remaining artifact in
// the pass. A journal write per artifact would fill the log with transitions
// describing nothing that happened.
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

// TestTransferRejectsMissingRequiredParams pins the preconditions that
// cannot be defaulted, and LocalDir is the one with teeth: an empty one used
// to join to a path relative to the daemon's working directory, so a backup
// set with no configured local_path would have written artifacts somewhere
// nobody was backing up.
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

// mustFinalPath and mustPartialPath are the test-side spelling of the two
// helpers issue #390's conversion gave an error return. Every call in this
// package's tests supplies a real temp directory and a real artifact id, so
// an error here is a broken test rather than a case worth exercising; the
// case that DOES exercise the refusal is named, and it calls the helpers
// directly.
func mustFinalPath(t *testing.T, dir string, artifact model.ArtifactID) string {
	t.Helper()
	p, err := finalPath(dir, artifact)
	if err != nil {
		t.Fatalf("finalPath(%q, %s): %v", dir, artifact, err)
	}
	return p
}

// mustPartialPath computes the .partial path a test expects, failing the
// test rather than returning an error. It goes through the production helper
// so a test and the code agree on the name; the tests that pin the name
// itself use literals instead, for the reason artifactstore's locator tests
// spell out.
func mustPartialPath(t *testing.T, dir string, artifact model.ArtifactID) string {
	t.Helper()
	p, err := partialPath(dir, artifact)
	if err != nil {
		t.Fatalf("partialPath(%q, %s): %v", dir, artifact, err)
	}
	return p
}

// TestFinalPathRefusesAnUnrootedStore is the behaviour issue #334 deferred
// and issue #390 lands. Before the conversion, finalPath("", artifact)
// returned the artifact's bare name: a path relative to whatever directory
// the daemon started in, which nothing is backing up.
//
// config.Validate refuses an empty local_path, so no configuration that got
// as far as running a cycle could reach this. That is why deferring it was
// safe and why leaving it was not: a backstop is worth having precisely for
// the caller that did not come through Validate.
func TestFinalPathRefusesAnUnrootedStore(t *testing.T) {
	artifact := testArtifact(t)
	if got, err := finalPath("", artifact); err == nil {
		t.Fatalf("finalPath with no local directory returned %q; a store with no root can write an artifact somewhere nobody is backing up", got)
	}
	if got, err := partialPath("", artifact); err == nil {
		t.Fatalf("partialPath with no local directory returned %q", got)
	}
	if got, err := FinalArtifactPath("", artifact); err == nil {
		t.Fatalf("FinalArtifactPath with no local directory returned %q", got)
	}
}

// --- issue #570: an attempt that stops speaking for its artifact ---

// TestTransferRefusesWhenItsTransferringKeyReplaysOverAnArtifactThatMovedOn
// closes the hole the field report came through: a transition key the
// journal has already seen replays without validating anything, because
// there is nothing to validate about a write it is not making, and up to
// #570 this step read that "already applied" as permission to carry on.
//
// The sequence here is the one internal/app's attemptKey makes reachable
// with no crash and no corruption involved. Two attempts on one artifact
// derive the same keys, the first records TRANSFERRING and walks the
// artifact to a durable terminal state, and the second arrives with the
// same key. What it must not do is copy: those bytes would land in the
// directory of an artifact that is already committed and retained, under a
// .partial name a later step could act on.
func TestTransferRefusesWhenItsTransferringKeyReplaysOverAnArtifactThatMovedOn(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	tr := &fakeTransport{copyFunc: writingCopy([]byte("must never be written"))}
	deps := Deps{Journal: j, Transport: tr}
	params := TransferParams{Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-1"}

	// The first attempt records TRANSFERRING under this key ...
	if _, err := Advance(context.Background(), deps, state.Transition{
		Artifact: artifact,
		Key:      params.AttemptKey + keyTransferringSuffix,
		From:     string(Discovered),
		To:       string(Transferring),
	}); err != nil {
		t.Fatalf("seeding the first attempt's TRANSFERRING: %v", err)
	}

	// ... and carries the artifact all the way to a retained terminal state
	// in the window between this attempt reading the row and writing to it.
	// That window is the only way in: Transfer re-reads the artifact itself
	// and Advance checks the table, so an artifact that had ALREADY moved
	// on when this attempt looked is refused by machine.go long before the
	// journal is asked (TestTransferRefusesAnArtifactPastItsOwnStage). What
	// is left is the interleaving, and the replay is what makes it silent:
	// the key has been seen, so the write reports "already applied" and
	// validates nothing, because there is nothing to validate about a write
	// it is not making.
	once := false
	j.afterGet = func() {
		if once {
			return
		}
		once = true
		j.forceState(RemoteRetained)
	}

	_, err := Transfer(context.Background(), deps, params)

	var superseded *TransferSupersededError
	if !errors.As(err, &superseded) {
		t.Fatalf("Transfer error = %v, want a *TransferSupersededError", err)
	}
	if superseded.Current != RemoteRetained {
		t.Errorf("superseded.Current = %q, want %q: the refusal has to name where the artifact actually is", superseded.Current, RemoteRetained)
	}
	if tr.callCount() != 0 {
		t.Fatalf("CopyToLocal was called %d time(s); a superseded attempt must not spend a byte of bandwidth or touch the local directory", tr.callCount())
	}
	if j.currentState() != string(RemoteRetained) {
		t.Fatalf("journal state = %q, want %q: a refusal must leave the artifact exactly as it was", j.currentState(), RemoteRetained)
	}
}

// TestCopyFailureAfterAnotherAttemptFinishedTheArtifactIsReportedNotClaimed
// is issue #570's own reproduction at this level: the copy fails, and by
// the time this attempt goes to write FAILED the artifact is already
// durable and retained.
//
// The two halves of the assertion are the whole point. FAILED must not be
// recorded, because it is not true and machine.go refuses it for exactly
// that reason once an artifact has a committed local copy. And the caller
// must still be told what happened, in a sentence that names what the
// artifact IS, because the alternative it used to get ("recording FAILED
// also failed", carrying the journal's refusal verbatim) reads as a verdict
// while `status` and `artifacts` show a healthy artifact.
func TestCopyFailureAfterAnotherAttemptFinishedTheArtifactIsReportedNotClaimed(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	copyErr := transport.NewError(transport.NotFound, "copy_to_local", errors.New("rename ...partial.ac832174.partial: no such file or directory"))
	tr := &fakeTransport{copyFunc: func(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
		// The other attempt finishes while this copy is in flight, which
		// is the only window the field report leaves: the collision guard
		// has already found nothing at the final name and TRANSFERRING is
		// already recorded.
		j.forceState(RemoteRetained)
		return transport.TransferResult{}, copyErr
	}}

	_, err := Transfer(context.Background(), Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-1", Policy: fastPolicy(),
	})

	var superseded *TransferSupersededError
	if !errors.As(err, &superseded) {
		t.Fatalf("Transfer error = %v, want a *TransferSupersededError", err)
	}
	if superseded.Current != RemoteRetained {
		t.Errorf("superseded.Current = %q, want %q", superseded.Current, RemoteRetained)
	}
	if !errors.Is(err, copyErr) {
		t.Error("the copy failure is no longer reachable through the returned error; it is the only account of what actually went wrong")
	}
	if !strings.Contains(err.Error(), string(RemoteRetained)) {
		t.Errorf("the error never names the state the artifact is in: %v", err)
	}
	if strings.Contains(err.Error(), state.ErrStateMismatch.Error()) {
		t.Errorf("the journal's refusal is being reported as the outcome, which is the #570 sentence: %v", err)
	}
	if j.currentState() != string(RemoteRetained) {
		t.Fatalf("journal state = %q, want %q: a durable, retained artifact must not be stamped FAILED by an attempt that lost", j.currentState(), RemoteRetained)
	}

	// Which refusal fired matters as much as the fact one did. This attempt
	// pinned From to TRANSFERRING, so the transition table let the move
	// through (TRANSFERRING -> FAILED is a legal edge) and the write reached
	// the journal, where the row's own state turned it down. That is the
	// journal's CAS, one of the two independent refusals standing between a
	// losing attempt and an artifact that already has a durable local copy,
	// and a fix that stopped asking would take it out of the path
	// altogether. The collision test above holds the other one.
	if failed := j.transitionsTo(Failed); len(failed) != 1 {
		t.Fatalf("%d FAILED transitions reached the journal, want 1: the CAS has to be the thing that refuses this one", len(failed))
	}
}

// TestCopyFailureDoesNotStampFailedOverAWinnerThatIsStillCopying is the
// ordering the field report actually describes, and the one pinning From to
// TRANSFERRING does not cover on its own.
//
// The loser fails in milliseconds, because the winner renamed its .partial
// out from under it, while the winner is still copying gigabytes. So the row
// is exactly where the loser's FAILED write expects it: TRANSFERRING is a
// legal predecessor of FAILED and the journal's CAS matches, and the verdict
// lands on a winner that is mid-flight. What comes of that is the artifact
// ending FAILED with a complete good copy sitting at .partial for the next
// attempt to delete, which is the sentence #570 was filed about.
//
// The two attempts share a key because internal/app's attemptKey is the
// artifact plus its retry count and nothing in it tells two live attempts
// apart, so the loser's TRANSFERRING write is a replay of the winner's. That
// replay is the whole signal: an attempt that did not put the artifact at
// TRANSFERRING itself has no claim to stamp a verdict from it.
func TestCopyFailureDoesNotStampFailedOverAWinnerThatIsStillCopying(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")

	// The winning attempt recorded TRANSFERRING under the shared key and is
	// still moving bytes, so the row sits there for as long as its copy
	// takes. It goes through Advance rather than forceState because a
	// winner that could not have reached this state legally would prove
	// nothing about a loser arriving behind it.
	if _, err := Advance(context.Background(), Deps{Journal: j}, state.Transition{
		Artifact: artifact,
		Key:      "attempt-1" + keyTransferringSuffix,
		From:     string(Discovered),
		To:       string(Transferring),
	}); err != nil {
		t.Fatalf("seeding the winning attempt's TRANSFERRING: %v", err)
	}

	copyErr := transport.NewError(transport.NotFound, "copy_to_local",
		errors.New("rename backup-2026-08-27.dump.zst.partial.ac832174.partial: no such file or directory"))
	tr := &fakeTransport{copyFunc: func(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
		return transport.TransferResult{}, copyErr
	}}

	_, err := Transfer(context.Background(), Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-1", Policy: fastPolicy(),
	})

	var superseded *TransferSupersededError
	if !errors.As(err, &superseded) {
		t.Fatalf("Transfer error = %v, want a *TransferSupersededError", err)
	}
	if superseded.Current != Transferring {
		t.Errorf("superseded.Current = %q, want %q: the refusal has to name where the artifact actually is", superseded.Current, Transferring)
	}
	if !errors.Is(err, copyErr) {
		t.Error("the copy failure is no longer reachable through the returned error; it is the only account of what this attempt actually saw")
	}

	// The verdict is the assertion. A FAILED here is a verdict about an
	// artifact somebody else is still copying, and it also blocks the
	// winner's own TRANSFERRED, which is how the good copy ends up
	// abandoned at .partial.
	if j.currentState() != string(Transferring) {
		t.Fatalf("journal state = %q, want %q: a losing attempt must not stamp a verdict over a winner that is still copying", j.currentState(), Transferring)
	}
	if failed := j.transitionsTo(Failed); len(failed) != 0 {
		t.Fatalf("recorded %d FAILED transitions, want 0: an attempt with no claim on the artifact must not even ask", len(failed))
	}

	// And the property underneath all of it: the winner can still finish.
	// This is what a stamped FAILED actually costs, since TRANSFERRING is
	// the only state TRANSFERRED is reachable from, so the good copy would
	// be stranded at .partial for the next attempt to delete. It is
	// asserted here rather than left implied because a bound on the
	// claimless path (see the budget test below) is one careless line away
	// from writing a verdict on the first failure again, and this is the
	// assertion that would go red if it did.
	if _, err := Advance(context.Background(), Deps{Journal: j}, state.Transition{
		Artifact: artifact,
		Key:      "attempt-1" + keyTransferredSuffix,
		From:     string(Transferring),
		To:       string(Transferred),
	}); err != nil {
		t.Fatalf("the winner could not record TRANSFERRED after the loser failed, which is the burial this refusal exists to stop: %v", err)
	}
}

// TestFinalNameCollisionOnAnArtifactAnotherAttemptFinishedIsReportedNotClaimed
// covers the likelier half of #570, and the half the collision path was
// still emitting the issue's own sentence on.
//
// A winning attempt renames its .partial to the final name, so an attempt
// arriving after it finished hits the collision guard before it ever reaches
// the TRANSFERRING write. failCollision declares From = whatever the row
// says, and for an artifact already carried to REMOTE_RETAINED the refusal
// therefore comes from machine.go's table rather than from the journal's
// CAS. Both are refusals of the same shape and only one of them used to be
// classified, so what an operator got was "(and recording FAILED also
// failed: ... REMOTE_RETAINED -> FAILED is not a legal transition)" over an
// artifact that is durable and retained.
func TestFinalNameCollisionOnAnArtifactAnotherAttemptFinishedIsReportedNotClaimed(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	// The winning attempt finished: the artifact is durable and retained
	// and its bytes are sitting at the final name.
	j.forceState(RemoteRetained)
	final, err := finalPath(dir, artifact)
	if err != nil {
		t.Fatalf("finalPath: %v", err)
	}
	knownGood := []byte("the winning attempt's copy")
	if err := os.WriteFile(final, knownGood, 0o600); err != nil {
		t.Fatalf("seeding the winner's final-name file: %v", err)
	}

	tr := &fakeTransport{copyFunc: writingCopy([]byte("must never be written"))}
	_, err = Transfer(context.Background(), Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-2",
	})

	var superseded *TransferSupersededError
	if !errors.As(err, &superseded) {
		t.Fatalf("Transfer error = %v, want a *TransferSupersededError", err)
	}
	if superseded.Current != RemoteRetained {
		t.Errorf("superseded.Current = %q, want %q", superseded.Current, RemoteRetained)
	}
	if strings.Contains(err.Error(), "recording FAILED also failed") {
		t.Errorf("the refused write is being reported as the outcome, which is #570's own sentence: %v", err)
	}
	if !strings.Contains(err.Error(), string(RemoteRetained)) {
		t.Errorf("the error never names the state the artifact is in: %v", err)
	}

	// The collision itself stays reachable: it is the only thing that says
	// which file an operator has to go and look at.
	var collision *FinalNameCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("err = %v, want a *FinalNameCollisionError still reachable through it", err)
	}
	if collision.Path != final {
		t.Errorf("collision.Path = %q, want %q", collision.Path, final)
	}

	// FR-30's floor, twice. The artifact has a durable local copy, so
	// nothing may record a failure over it, and nothing may touch the file.
	if j.currentState() != string(RemoteRetained) {
		t.Fatalf("journal state = %q, want %q: a durable, retained artifact must not be stamped FAILED", j.currentState(), RemoteRetained)
	}
	if failed := j.transitionsTo(Failed); len(failed) != 0 {
		t.Fatalf("%d FAILED transitions reached the journal, want 0: the transition table has to refuse this one before the journal is touched at all", len(failed))
	}
	if err := Validate(RemoteRetained, Failed); err == nil {
		t.Fatal("REMOTE_RETAINED -> FAILED is legal again; that refusal is one of the two things standing between a losing attempt and an artifact that is already durable")
	}
	if tr.callCount() != 0 {
		t.Errorf("CopyToLocal was called %d time(s); a collision must be refused before any copy is attempted", tr.callCount())
	}
	got, readErr := os.ReadFile(final)
	if readErr != nil {
		t.Fatalf("reading the final-name file after the refusal: %v", readErr)
	}
	if string(got) != string(knownGood) {
		t.Fatalf("the winner's file was modified: got %q, want %q", got, knownGood)
	}
}

// TestConsecutiveClaimlessCopyFailuresEndInFailedRatherThanRetryingForever
// bounds the cost of the claim check.
//
// An attempt that cannot claim the artifact records no verdict, and on its
// own that leaves one shape unbounded: a copy that fails permanently after
// a mid-copy crash is retried every cycle, never counted by `status`, and
// rendered as work in flight by anything reading the row. That is #570's
// own complaint in different clothes, three surfaces disagreeing about one
// artifact, so the claimless path is charged against the artifact's own
// FR-22 counter the way issue #419 charges a verification that could not be
// completed, and once the budget is spent it records FAILED.
//
// The two halves are the assertion. One claimless failure must NOT produce
// a verdict, because that is exactly the burial the claim check exists to
// stop, and a second one on a later cycle must, because otherwise nothing
// ever tells an operator.
func TestConsecutiveClaimlessCopyFailuresEndInFailedRatherThanRetryingForever(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	deps := Deps{Journal: j}

	// The artifact genuinely entered TRANSFERRING once, under a key this
	// attempt does not own. Every later attempt therefore replays it, which
	// is what a crash mid-copy leaves behind (the key is stable across a
	// resume) and what a second live attempt looks like from here.
	if _, err := Advance(context.Background(), deps, state.Transition{
		Artifact: artifact,
		Key:      "attempt-0" + keyTransferringSuffix,
		From:     string(Discovered),
		To:       string(Transferring),
	}); err != nil {
		t.Fatalf("seeding the TRANSFERRING nobody can claim: %v", err)
	}

	copyErr := transport.NewError(transport.NotFound, "copy_to_local", errors.New("no such object"))
	tr := &fakeTransport{copyFunc: func(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
		return transport.TransferResult{}, copyErr
	}}
	deps.Transport = tr

	// Cycle one. The attempt key is the artifact's retry count, so this is
	// the same key the seeded write used.
	_, err := Transfer(context.Background(), deps, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-0", Policy: fastPolicy(),
	})
	var superseded *TransferSupersededError
	if !errors.As(err, &superseded) {
		t.Fatalf("first claimless failure returned %v, want a *TransferSupersededError", err)
	}
	if j.currentState() != string(Transferring) {
		t.Fatalf("journal state = %q after ONE claimless failure, want %q: a single failure must never be a verdict", j.currentState(), Transferring)
	}
	if failed := j.transitionsTo(Failed); len(failed) != 0 {
		t.Fatalf("%d FAILED transitions after one claimless failure, want 0", len(failed))
	}

	// The attempt is charged, and the charge is what an operator and the
	// next cycle both read: the counter moves, so the next cycle derives a
	// different attempt key, and the reason is on the row.
	charged := j.row()
	if charged.RetryCount != 1 {
		t.Fatalf("RetryCount = %d after one claimless failure, want 1: nothing counted the attempt", charged.RetryCount)
	}
	if charged.LastError == "" {
		t.Fatal("the charged attempt left no reason on the row, so nothing an operator reads says why this artifact is not moving")
	}

	// Cycle two, with the key the moved counter produces.
	_, err = Transfer(context.Background(), deps, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-1", Policy: fastPolicy(),
	})
	if errors.As(err, &superseded) {
		t.Fatalf("the second claimless failure was reported as superseded again, so this artifact retries forever with nothing counting it: %v", err)
	}
	if !errors.Is(err, copyErr) {
		t.Errorf("the copy failure is not reachable through %v", err)
	}
	if j.currentState() != string(Failed) {
		t.Fatalf("journal state = %q after two consecutive claimless failures, want %q", j.currentState(), Failed)
	}
	failed := j.transitionsTo(Failed)
	if len(failed) != 1 {
		t.Fatalf("recorded %d FAILED transitions, want 1", len(failed))
	}
	if failed[0].Detail == "" {
		t.Fatal("the FAILED transition carries no Detail, so `artifacts` shows an operator a verdict with no reason")
	}
	if failed[0].Retry == nil {
		t.Fatal("the verdict did not carry the retry bookkeeping that justifies it")
	}
}

// TestAnAlreadySpentRetryCountStillBuysOneClaimlessFailure covers the half
// of the bound that the budget alone cannot supply.
//
// RetryCount is shared on purpose (quarantine.go's package doc asked for
// exactly one counter), so an artifact an operator has released a few
// times, or one issue #419 has already charged for stalled verifications,
// arrives at a claimless copy failure with the budget ALREADY spent. On the
// budget alone its first failure would write a verdict over an attempt that
// may still be copying, which is the burial the claim check exists to
// prevent, so the verdict also needs a charge recorded during this same
// occupancy of TRANSFERRING.
func TestAnAlreadySpentRetryCountStillBuysOneClaimlessFailure(t *testing.T) {
	artifact := testArtifact(t)
	dir := t.TempDir()

	j := newFakeTransferJournal(artifact, "backups/backup-2026-08-27.dump.zst")
	deps := Deps{Journal: j}
	if _, err := Advance(context.Background(), deps, state.Transition{
		Artifact: artifact,
		Key:      "attempt-9" + keyTransferringSuffix,
		From:     string(Discovered),
		To:       string(Transferring),
	}); err != nil {
		t.Fatalf("seeding the TRANSFERRING nobody can claim: %v", err)
	}
	// Well past any budget this policy could produce, and none of it was
	// spent on this transfer.
	j.forceRetryCount(9)

	copyErr := transport.NewError(transport.NotFound, "copy_to_local", errors.New("no such object"))
	tr := &fakeTransport{copyFunc: func(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
		return transport.TransferResult{}, copyErr
	}}
	deps.Transport = tr

	_, err := Transfer(context.Background(), deps, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-9", Policy: fastPolicy(),
	})
	var superseded *TransferSupersededError
	if !errors.As(err, &superseded) {
		t.Fatalf("first claimless failure returned %v, want a *TransferSupersededError", err)
	}
	if j.currentState() != string(Transferring) {
		t.Fatalf("journal state = %q, want %q: a counter spent somewhere else must not turn one claimless failure into a verdict", j.currentState(), Transferring)
	}
	if failed := j.transitionsTo(Failed); len(failed) != 0 {
		t.Fatalf("%d FAILED transitions on the first claimless failure, want 0", len(failed))
	}

	// The winner this attempt could not claim can still finish, which is
	// the property the whole claim check is for.
	if _, err := Advance(context.Background(), Deps{Journal: j}, state.Transition{
		Artifact: artifact,
		Key:      "attempt-9" + keyTransferredSuffix,
		From:     string(Transferring),
		To:       string(Transferred),
	}); err != nil {
		t.Fatalf("the winner could not record TRANSFERRED: %v", err)
	}
}

// TestTheClaimlessBoundHoldsAgainstARealJournal is the control on the fake.
//
// The bound asks the journal two questions the rest of this file never
// does, and the answers only mean what failUnclaimedCopy needs them to
// mean because of one clause in each query: LastEnteredAt ignores
// same-state writes, so it dates this occupancy of TRANSFERRING rather than
// the charges written inside it, and LastTransition matches the exact
// TRANSFERRING -> TRANSFERRING edge nothing else in this package writes. A
// fake that models both is still a model, so this runs the same two cycles
// against real SQL, and ends where an operator actually reads the verdict:
// the detail on the transition that produced FAILED, which is what
// internal/app's FailureReason returns.
func TestTheClaimlessBoundHoldsAgainstARealJournal(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	artifact := testArtifact(t)
	dir := t.TempDir()

	size := int64(11)
	if _, err := j.Discover(ctx, artifact, "seed:discovered", "backups/backup-2026-08-27.dump.zst",
		state.RemoteIdentity{Size: &size, Hash: "abc", HashAlg: "sha256"}, time.Now().UTC()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	deps := Deps{Journal: j}
	if _, err := Advance(ctx, deps, state.Transition{
		Artifact: artifact,
		Key:      "attempt-0" + keyTransferringSuffix,
		From:     string(Discovered),
		To:       string(Transferring),
	}); err != nil {
		t.Fatalf("seeding the TRANSFERRING nobody can claim: %v", err)
	}

	copyErr := transport.NewError(transport.NotFound, "copy_to_local", errors.New("no such object"))
	deps.Transport = &fakeTransport{copyFunc: func(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
		return transport.TransferResult{}, copyErr
	}}

	_, err := Transfer(ctx, deps, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-0", Policy: fastPolicy(),
	})
	var superseded *TransferSupersededError
	if !errors.As(err, &superseded) {
		t.Fatalf("first claimless failure returned %v, want a *TransferSupersededError", err)
	}
	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(Transferring) || rec.RetryCount != 1 {
		t.Fatalf("after one claimless failure the row is state=%q retries=%d, want %q and 1", rec.State, rec.RetryCount, Transferring)
	}

	_, err = Transfer(ctx, deps, TransferParams{
		Artifact: artifact, LocalDir: dir, AttemptKey: "attempt-1", Policy: fastPolicy(),
	})
	if errors.As(err, &superseded) {
		t.Fatalf("the second claimless failure was reported as superseded again: %v", err)
	}
	rec, err = j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(Failed) {
		t.Fatalf("journal state = %q after two consecutive claimless failures, want %q", rec.State, Failed)
	}
	if rec.LastError == "" {
		t.Error("the verdict left no last error on the row")
	}

	detail, _, found, err := j.LastEnteredDetail(ctx, artifact, string(Failed))
	if err != nil {
		t.Fatalf("LastEnteredDetail: %v", err)
	}
	if !found || detail == "" {
		t.Fatal("the FAILED transition carries no detail, so `artifacts` would show this verdict with no reason at all")
	}
	if !strings.Contains(detail, "consecutive attempts") {
		t.Errorf("the recorded reason does not say why this took several attempts, which is the whole difference between this verdict and an ordinary copy failure: %q", detail)
	}
}
