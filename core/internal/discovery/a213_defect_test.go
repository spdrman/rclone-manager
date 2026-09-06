package discovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// TestRealPipeline_DeleteRemote_ConfirmsIdentityAndProceeds drives the
// whole real pipeline, Discover then Transfer then Verify then Commit then
// DeleteRemote, against the real rclone local adapter, and proves the FR-16
// re-identification can actually reach ConfidenceStrong and let a delete
// happen.
//
// It was written to prove the opposite, and the history is worth keeping
// because it explains both the fixture and the file it lives in.
//
// The A213 sweep set up the most favourable case a delete could possibly
// get: a local backend with full hash support and no shell restriction at
// all, and a remote object nobody touched between discovery and delete. The
// delete was refused anyway. DeleteRemote's re-identification re-Statted the
// object but never called Transport.RemoteHash, and the adapter's Stat only
// ever filled in path, size and modification time, so the "current" side of
// model.CompareIdentity could never carry a hash no matter what the
// discovered side had captured or what the backend was capable of. The best
// available outcome was a weak same-size, same-second match, and DeleteRemote
// requires a strong one.
//
// That mattered because remotedelete.go's own doc blamed the weak outcome on
// docs/ssh-setup.md's hardened, shell-less SFTP account, which implied a more
// capable backend would do better. It would not have: no backend in this
// binary could reach a strong match, so no remote object could ever be
// deleted through this pipeline. It failed safe, in that nothing was ever
// wrongly deleted, and it was still a real "no artifact may be stuck with no
// way forward" violation for any operator who needed the remote space back.
//
// Stat now asks the backend for a hash and a stable id, so the delete
// proceeds. The fixture stayed rather than being deleted along with the
// defect: driving all five stages end to end against a real backend is
// exactly the regression the fix needs, and nothing else in the tree covers
// that path.
//
// The last assertion is the one that keeps this honest. Reporting success
// without deleting anything would satisfy every check above it, so the test
// finishes by looking for the object on disk.
func TestRealPipeline_DeleteRemote_ConfirmsIdentityAndProceeds(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	localDir := t.TempDir()

	content := []byte("untouched, unmodified, byte-for-byte identical remote content")
	if err := os.WriteFile(filepath.Join(root, "backup.dump"), content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	source := transport.Source{ID: "known-defect-2", Type: "local", Root: root}
	set := backupSet(t, config.Completion{Strategy: "rename"}, nil)
	journal := openJournal(t)
	adapter := rclone.New()
	deps := Deps{Transport: adapter, Journal: journal, Now: fixedNow(epoch)}

	res, err := Discover(ctx, deps, source, set)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Discovered) != 1 {
		t.Fatalf("Discovered = %+v, want exactly one", res.Discovered)
	}
	artifact := res.Discovered[0].Artifact

	lifeDeps := lifecycle.Deps{Journal: journal, Transport: adapter}
	if _, err := lifecycle.Transfer(ctx, lifeDeps, lifecycle.TransferParams{
		Artifact: artifact, Source: source, LocalDir: localDir, AttemptKey: "attempt-1",
	}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	// Verify needs its own TRANSFERRED -> VERIFYING entry transition; see
	// tests/crashmatrix/harness/main.go's identical comment for why this
	// orchestration step exists nowhere else in this repository yet.
	rec, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("journal.Get: %v", err)
	}
	if _, err := lifecycle.Advance(ctx, lifeDeps, state.Transition{
		Artifact: artifact, Key: "attempt-1:begin-verifying",
		From: rec.State, To: string(lifecycle.Verifying),
	}); err != nil {
		t.Fatalf("begin VERIFYING: %v", err)
	}
	if _, err := lifecycle.Verify(ctx, lifeDeps, lifecycle.VerifyParams{
		Artifact: artifact, Source: source, Validation: config.Validation{Hash: string(transport.SHA256)}, AttemptKey: "attempt-1",
	}); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if _, err := lifecycle.Commit(ctx, lifeDeps, lifecycle.CommitInput{
		Artifact: artifact, LocalDir: localDir, CommittingKey: "commit:committing", CommittedKey: "commit:committed",
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	_, deleteErr := lifecycle.DeleteRemote(ctx, lifeDeps, lifecycle.DeleteRemoteRequest{
		CompletionStrategy: "rename",
		Source:             source, Artifact: artifact, AttemptKey: "delete:attempt-1",
	})

	// This used to assert the opposite. The adapter's Stat never carried a
	// hash, so model.CompareIdentity could not reach ConfidenceStrong and
	// DeleteRemote refused every delete against every backend, even a local
	// one that hashes perfectly well. Stat now asks the backend for a hash
	// and a stable id, so an untouched, byte-identical object is positively
	// re-confirmed and the delete proceeds.
	//
	// I kept the fixture rather than deleting the test with the defect,
	// because driving the real Discover, Transfer, Verify, Commit,
	// DeleteRemote pipeline end to end against a real backend is exactly the
	// regression this fix needs, and nothing else covers that path.
	if refusal, ok := lifecycle.AsRemoteDeleteRefusal(deleteErr); ok {
		t.Fatalf("DeleteRemote refused an untouched, byte-identical object: check=%q confidence=%v reason=%s; "+
			"Stat should now supply a hash, so this is the FR-16 re-check failing to reach ConfidenceStrong again",
			refusal.Check, refusal.Confidence, refusal.Reason)
	}
	if deleteErr != nil {
		t.Fatalf("DeleteRemote against an untouched real object = %v (%T)", deleteErr, deleteErr)
	}

	// And prove it actually deleted, rather than reporting success and doing
	// nothing, which would pass the check above while losing the point.
	if _, statErr := os.Stat(filepath.Join(root, "backup.dump")); !os.IsNotExist(statErr) {
		t.Fatalf("DeleteRemote reported success but the remote object is still there (stat err = %v)", statErr)
	}
}
