// This file is issue #31's destructive-safety suite for package lifecycle:
// adversarial attempts to make DeleteRemote destroy something it should
// not, and Commit clobber something it should not, followed by the test
// that shows each attempt was refused. Helpers added here are prefixed
// a213 to stay clear of the package's existing fakeTransport,
// fakeTransferJournal, fakeJournal, mustID, openTestJournal,
// deleteTransport, verifyJournal, verifyTransport and testArtifact.
package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

func a213Sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- adversarial: swap a COMMITTED file for a symlink to a same-size victim ---

// TestDeleteRemote_SymlinkSwapOfCommittedFile_RefusedViaHashMismatch is an
// attack on FR-15's second/third revalidation ("the expected local final
// file exists and its size/identity is consistent with what the journal
// recorded"): after COMMITTED, replace the real committed file with a
// symlink to a different, pre-existing "victim" file engineered to have
// the EXACT SAME SIZE as the real one, so a size-only check would see
// nothing wrong. It must still be refused, and the reason matters: every
// artifact that reaches VERIFIED gets its local content hashed
// unconditionally (verify.go's own doc: "always computed as a side effect
// of the mandatory full read... regardless of cfg.Hash"), so
// verifyLocalFinal always has a real hash to check against even when the
// operator never configured remote hash verification. This proves that
// unconditional hash actually earns its keep here: a same-size swap is
// exactly the case a size-only check cannot see.
func TestDeleteRemote_SymlinkSwapOfCommittedFile_RefusedViaHashMismatch(t *testing.T) {
	j := openTestJournal(t)
	artifact := mustID(t)

	dir := t.TempDir()
	realContent := bytes.Repeat([]byte{0xAB}, 4096)
	realPath := filepath.Join(dir, "real.final")
	if err := os.WriteFile(realPath, realContent, 0o600); err != nil {
		t.Fatalf("WriteFile real: %v", err)
	}
	realHash := a213Sha256Hex(realContent)

	// The victim: same length, different content, so the swap is invisible
	// to a size check and only a hash check can catch it.
	victimContent := bytes.Repeat([]byte{0xCD}, len(realContent))
	victimPath := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victimPath, victimContent, 0o600); err != nil {
		t.Fatalf("WriteFile victim: %v", err)
	}
	if len(victimContent) != len(realContent) {
		t.Fatalf("test bug: victim and real content are not the same length")
	}

	size := int64(len(realContent))
	discoverAndAdvance(t, j, artifact, testRemotePath,
		state.RemoteIdentity{Size: &size},
		realPath, &state.TransferResult{BytesTransferred: size},
		&state.HashUpdate{Alg: "sha256", Hash: realHash},
		Committed)

	// The attack: swap the committed final path for a symlink to the
	// victim. os.Symlink itself never touches realPath's own content;
	// this only changes what the journal's recorded LocalPath now
	// resolves to.
	if err := os.Remove(realPath); err != nil {
		t.Fatalf("Remove real (staging the swap): %v", err)
	}
	if err := os.Symlink(victimPath, realPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	tp := &deleteTransport{
		statFn: func(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
			t.Fatal("Stat (remote re-identification) must never be reached: the local-file check should refuse first")
			return transport.RemoteArtifact{}, nil
		},
	}
	_, err := DeleteRemote(context.Background(), Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Source:             transport.Source{ID: "prod-nas"}, Artifact: artifact, AttemptKey: "attempt-1",
	})
	refusal := requireRefusal(t, err, "local file")
	t.Logf("refused as expected: %s", refusal.Reason)

	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote was called %d times, want 0", tp.deleteCalls)
	}

	// Neither file was touched by the refused attempt.
	gotVictim, err := os.ReadFile(victimPath)
	if err != nil {
		t.Fatalf("victim file is gone: %v", err)
	}
	if !bytes.Equal(gotVictim, victimContent) {
		t.Fatal("victim file content was altered by the refused delete attempt")
	}
	if _, err := os.Lstat(realPath); err != nil {
		t.Fatalf("the symlink at the committed path is gone: %v", err)
	}

	rec, getErr := j.Get(context.Background(), artifact)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if rec.State != string(Committed) {
		t.Fatalf("journal state = %q, want COMMITTED unchanged (a refusal caught before any journal write)", rec.State)
	}
}

// --- adversarial: a dangling symlink squatting on the final name ---

// TestCommit_DanglingSymlinkAtFinalName_NeverFollowedNeverClobbered attacks
// FR-12's collision guard from the other direction: plant a dangling
// symlink (pointing nowhere) at the exact final path an artifact is about
// to be committed to, before Transfer or Commit ever runs. os.Stat, which
// Transfer's own collision guard uses, follows symlinks and reports
// ErrNotExist for a dangling one exactly as it would for "nothing here at
// all", so Transfer's own pre-check cannot see this occupant and does not
// refuse up front (see the PR description's smaller-observations section:
// this is a real, narrow gap in that specific pre-check, not in the
// system's overall safety). What this test actually proves is that the
// system stays safe anyway: Commit's own rename step, os.Link, does not
// dereference the destination name to decide whether it is "empty" the
// way Stat does; link(2) looks at the directory entry itself, so a
// dangling symlink there still makes os.Link fail with EEXIST, and
// commitFile's recovery logic (this is not literally the same file,
// refuse) takes over from there. Nothing is ever written through the
// symlink to wherever it points, and the attacker-controlled path a
// dangling symlink names is never even opened.
func TestCommit_DanglingSymlinkAtFinalName_NeverFollowedNeverClobbered(t *testing.T) {
	j := openTestJournal(t)
	artifact := mustID(t)
	localDir := t.TempDir()

	final := mustFinalPath(t, localDir, artifact)
	partial := mustPartialPath(t, localDir, artifact)

	danglingTarget := filepath.Join(localDir, "this-path-must-never-be-created")
	if err := os.Symlink(danglingTarget, final); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	content := bytes.Repeat([]byte{0x11}, 2048)
	if err := os.WriteFile(partial, content, 0o600); err != nil {
		t.Fatalf("WriteFile partial: %v", err)
	}

	// Walk the journal to VERIFIED by hand (RecordTransition directly,
	// exactly like discoverAndAdvance does), since this test's whole point
	// is what Commit does with a pre-existing final-path occupant, not the
	// steps before it.
	ctx := context.Background()
	size := int64(len(content))
	if _, err := j.Discover(ctx, artifact, "k1", testRemotePath, state.RemoteIdentity{Size: &size}, time.Now().UTC()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: "k2", From: string(Discovered), To: string(Transferring),
		OccurredAt: time.Now().UTC(), LocalPath: &partial,
	}); err != nil {
		t.Fatalf("-> TRANSFERRING: %v", err)
	}
	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: "k3", From: string(Transferring), To: string(Transferred),
		OccurredAt: time.Now().UTC(), Transfer: &state.TransferResult{BytesTransferred: size},
	}); err != nil {
		t.Fatalf("-> TRANSFERRED: %v", err)
	}
	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: "k4", From: string(Transferred), To: string(Verifying), OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("-> VERIFYING: %v", err)
	}
	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: "k5", From: string(Verifying), To: string(Verified), OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("-> VERIFIED: %v", err)
	}

	_, err := Commit(ctx, Deps{Journal: j}, CommitInput{
		Artifact: artifact, LocalDir: localDir, CommittingKey: "commit", CommittedKey: "committed",
	})
	var collision *FinalPathCollisionError
	if !isFinalPathCollision(err, &collision) {
		t.Fatalf("Commit against a dangling symlink squatting on the final name = %v, want a *FinalPathCollisionError", err)
	}
	t.Logf("refused as expected: %v", collision)

	if _, err := os.Stat(danglingTarget); !os.IsNotExist(err) {
		t.Fatalf("the dangling symlink's target was created by Commit; nothing should ever write through it (stat err: %v)", err)
	}

	link, err := os.Readlink(final)
	if err != nil {
		t.Fatalf("the symlink at the final path is gone: %v", err)
	}
	if link != danglingTarget {
		t.Fatalf("the symlink at the final path was altered: now points to %q, want %q", link, danglingTarget)
	}

	got, err := os.ReadFile(partial)
	if err != nil {
		t.Fatalf("reading .partial after the refused commit: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal(".partial content was altered by the refused commit attempt")
	}

	// COMMITTING, not VERIFIED: commit.go deliberately records COMMITTING
	// durably before ever touching a file (see its own package doc), so a
	// failure inside commitFile leaves the journal at COMMITTING, not
	// rolled back. That is by design (COMMITTING is a safe, resumable "in
	// progress" marker, not a stuck state, see machine.go's crash-safety
	// walkthrough), not this attack getting anywhere: the file-level
	// assertions above already proved neither file was touched.
	rec, getErr := j.Get(ctx, artifact)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if rec.State != string(Committing) {
		t.Fatalf("journal state = %q, want COMMITTING", rec.State)
	}

	// Confirm this is genuinely recoverable, not a second, quieter way to
	// get stuck: clear the attacker's symlink (an operator investigating
	// the FinalPathCollisionError would do exactly this) and retry with
	// the same keys. Commit must converge cleanly.
	if err := os.Remove(final); err != nil {
		t.Fatalf("Remove (clearing the symlink for the retry): %v", err)
	}
	if _, err := Commit(ctx, Deps{Journal: j}, CommitInput{
		Artifact: artifact, LocalDir: localDir, CommittingKey: "commit", CommittedKey: "committed",
	}); err != nil {
		t.Fatalf("Commit retry after clearing the symlink: %v", err)
	}
	rec, getErr = j.Get(ctx, artifact)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if rec.State != string(Committed) {
		t.Fatalf("journal state after retry = %q, want COMMITTED", rec.State)
	}
	got, err = os.ReadFile(final)
	if err != nil {
		t.Fatalf("reading final committed file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("final committed file does not match the original content")
	}
}

func isFinalPathCollision(err error, target **FinalPathCollisionError) bool {
	for e := err; e != nil; {
		if c, ok := e.(*FinalPathCollisionError); ok {
			*target = c
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// --- adversarial: a stale/corrupted journal row that skipped COMMITTED ---

// TestDeleteRemote_StaleJournalRowThatNeverWentThroughCommitted_Refused
// represents a hand-edited row, a schema drift, or a caller bug that wrote
// directly to the FR-9 journal instead of going through lifecycle.Advance
// (state.go's own UnknownStateError doc names exactly these causes). The
// state.Journal.RecordTransition primitive itself only enforces that
// t.From matches the row's actual current state (state/journal.go's
// updateArtifact); it has no opinion on whether the overall sequence is a
// legal walk of the FR-10 graph, that is Advance's job, and this test
// deliberately calls RecordTransition directly to bypass it, producing a
// row Advance itself would have refused to ever create: REMOTE_DELETE_PENDING
// with a LocalPath that was never actually committed.
//
// The point is defense in depth: even if something upstream of this
// package's own machine.go ever got the state graph wrong (or a row was
// corrupted after the fact), DeleteRemote's independent, from-scratch
// filesystem revalidation (verifyLocalFinal) still has the final say and
// still refuses, rather than trusting the journal's State column at face
// value.
func TestDeleteRemote_StaleJournalRowThatNeverWentThroughCommitted_Refused(t *testing.T) {
	j := openTestJournal(t)
	artifact := mustID(t)
	ctx := context.Background()

	size := int64(4096)
	if _, err := j.Discover(ctx, artifact, "k1", testRemotePath, state.RemoteIdentity{Size: &size}, time.Now().UTC()); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// The corruption: jump straight to REMOTE_DELETE_PENDING from
	// DISCOVERED, something lifecycle.Advance would refuse outright
	// (machine.go: RemoteDeletePending's only legal predecessor is
	// Committed), naming a local path that was never written by any real
	// Transfer/Commit.
	neverCommitted := filepath.Join(t.TempDir(), "never-actually-committed.final")
	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: "corrupt-1", From: string(Discovered), To: string(RemoteDeletePending),
		OccurredAt: time.Now().UTC(), LocalPath: &neverCommitted,
	}); err != nil {
		t.Fatalf("RecordTransition (staging the corruption): %v", err)
	}
	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(RemoteDeletePending) {
		t.Fatalf("test bug: staged state = %q, want REMOTE_DELETE_PENDING", rec.State)
	}

	tp := &deleteTransport{
		statFn: func(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
			t.Fatal("Stat must never be reached: the local-file check should refuse first, before ever touching the remote")
			return transport.RemoteArtifact{}, nil
		},
	}
	_, err = DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Source:             transport.Source{ID: "prod-nas"}, Artifact: artifact, AttemptKey: "attempt-1",
	})
	refusal := requireRefusal(t, err, "local file")
	t.Logf("refused as expected: %s", refusal.Reason)

	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote was called %d times, want 0", tp.deleteCalls)
	}
	if _, statErr := os.Stat(neverCommitted); !os.IsNotExist(statErr) {
		t.Fatalf("a file now exists at the never-committed path; something wrote there (stat err: %v)", statErr)
	}
}

// --- adversarial: malformed configuration that bypassed config.Validate ---

// TestVerify_MalformedHashPolicyThatBypassedConfigValidate_FailsExplicitly
// simulates a caller that skipped, or was somehow able to bypass,
// config.Validate: a config.Validation.Hash value that is neither "" nor
// "sha256" (config.Validate's validateValidation refuses everything else)
// reaching decide() directly. verify.go's own switch statement documents
// the intended defence ("config.Validate should have rejected this");
// this proves that defence actually fires, rather than decide() guessing,
// silently treating the unknown policy as "no hash required", or panicking
// on unrecognised input from what is supposed to be a closed, validated
// enum.
func TestVerify_MalformedHashPolicyThatBypassedConfigValidate_FailsExplicitly(t *testing.T) {
	content := []byte("some transferred content")
	localPath := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, localPath, int64(len(content)))
	journal := newVerifyJournal(rec)

	out, err := Verify(context.Background(), Deps{Journal: journal, Transport: &verifyTransport{}}, VerifyParams{
		Artifact:   rec.Artifact,
		Source:     transport.Source{ID: "prod-nas"},
		Validation: config.Validation{Hash: "md5-typo-that-config-validate-would-have-caught"},
		AttemptKey: "attempt-1",
	})
	if err != nil {
		t.Fatalf("Verify returned an infrastructure error rather than a business outcome: %v", err)
	}
	if out.Record.State != string(Failed) {
		t.Fatalf("state = %q, want FAILED for an unrecognised hash policy (never VERIFIED, never a panic)", out.Record.State)
	}
}
