package classifytransport

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/discovery"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// TestWithStatHash_LetsAGenuinelySafeDeleteProceed is the positive control
// for this file's whole reason for existing: the exact real-pipeline
// scenario internal/discovery/a213_defect_test.go proves gets stuck at
// REMOTE_DELETE_PENDING against the raw real adapter reaches COMPLETE once
// Stat is decorated with WithStatHash.
func TestWithStatHash_LetsAGenuinelySafeDeleteProceed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	localDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "backup.dump"), []byte("untouched content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	source := transport.Source{ID: "with-stat-hash", Type: "local", Root: root}
	tr := WithStatHash(rclone.New())

	journalPath := filepath.Join(t.TempDir(), "journal.db")
	journal, err := state.Open(ctx, journalPath)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer journal.Close()

	set, err := model.NewBackupSetID("with-stat-hash-source", "set")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	bs := config.BackupSet{Name: "set", ID: set, Completion: config.Completion{Strategy: "rename"}}

	res, err := discovery.Discover(ctx, discovery.Deps{Transport: tr, Journal: journal}, source, bs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Discovered) != 1 {
		t.Fatalf("Discovered = %+v, want exactly one", res.Discovered)
	}
	artifact := res.Discovered[0].Artifact

	deps := lifecycle.Deps{Journal: journal, Transport: tr}
	if _, err := lifecycle.Transfer(ctx, deps, lifecycle.TransferParams{
		Artifact: artifact, Source: source, LocalDir: localDir, AttemptKey: "attempt-1",
	}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	rec, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("journal.Get: %v", err)
	}
	if _, err := lifecycle.Advance(ctx, deps, state.Transition{
		Artifact: artifact, Key: "attempt-1:begin-verifying", From: rec.State, To: string(lifecycle.Verifying),
	}); err != nil {
		t.Fatalf("begin VERIFYING: %v", err)
	}
	if _, err := lifecycle.Verify(ctx, deps, lifecycle.VerifyParams{
		Artifact: artifact, Source: source, Validation: config.Validation{Hash: string(transport.SHA256)}, AttemptKey: "attempt-1",
	}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if _, err := lifecycle.Commit(ctx, deps, lifecycle.CommitInput{
		Artifact: artifact, LocalDir: localDir, CommittingKey: "commit:committing", CommittedKey: "commit:committed",
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if _, err := lifecycle.DeleteRemote(ctx, deps, lifecycle.DeleteRemoteRequest{
		Source: source, Artifact: artifact, AttemptKey: "delete:attempt-1",
	}); err != nil {
		t.Fatalf("DeleteRemote with WithStatHash still refused: %v", err)
	}

	final, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("journal.Get: %v", err)
	}
	if final.State != string(lifecycle.Complete) {
		t.Fatalf("final state = %q, want COMPLETE", final.State)
	}
	if _, err := os.Stat(filepath.Join(root, "backup.dump")); !os.IsNotExist(err) {
		t.Fatalf("remote object still present after a successful DeleteRemote (stat err: %v)", err)
	}
}
