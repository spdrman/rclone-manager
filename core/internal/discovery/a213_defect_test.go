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

// TestRealPipeline_DeleteRemote_NeverConfirmsIdentityStrongly_KnownDefect is
// the most favourable possible case for a delete to succeed: a real local
// backend (full hash support, no SFTP shell restriction at all), a remote
// object nobody ever touched after discovery, driven through the real
// discovery.Discover -> lifecycle.Transfer -> lifecycle.Verify ->
// lifecycle.Commit -> lifecycle.DeleteRemote pipeline. It proves a second
// real defect this issue's test suites found (see the PR description
// under its own heading): lifecycle.DeleteRemote's FR-16 re-identification
// re-Stats the remote object but never calls Transport.RemoteHash, and
// transport.RemoteArtifact from a real Stat call never carries a hash or a
// backend stable id (see internal/transport/rclone/adapter.go's toArtifact,
// which only ever fills in Path/Size/ModTime). So the "current" side of
// model.CompareIdentity can never carry a hash, regardless of what the
// "discovered" side captured or what the backend is actually capable of,
// and the comparison can only ever reach ConfidenceWeak on a same-second,
// same-size match: never the ConfidenceStrong hash match that would let a
// delete proceed.
//
// internal/lifecycle/remotedelete.go's own package doc attributes this
// weak-confidence outcome to "docs/ssh-setup.md['s] hardened, shell-less
// SFTP account", implying a more capable backend or account would do
// better. This test shows that framing is incomplete: the local backend
// has no shell restriction of any kind and can compute a real sha256
// (this package's own discovery.go proves as much at discovery time), yet
// DeleteRemote still cannot reach a strong-confidence match, because
// DeleteRemote's own re-identification step never asks for one. The
// practical consequence: as this code stands today, a remote object can
// never be positively re-confirmed and deleted through this pipeline
// against ANY backend registered in this binary, not only the
// hardened-SFTP posture the comments call out; every real delete attempt
// refuses. That fails safe (nothing is ever wrongly deleted) but is a real
// "no artifact may be stuck with no way forward" violation once an
// operator actually needs remote storage back. The recommended fix (out
// of this PR's file scope: it touches internal/lifecycle/remotedelete.go,
// production code) is for the real-adapter equivalent of "current
// identity" to attempt Transport.RemoteHash best-effort when the
// discovered side carries one, mirroring exactly the pattern
// captureRemoteIdentity already uses in this package's own discovery.go.
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
		Source: source, Artifact: artifact, AttemptKey: "delete:attempt-1",
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
