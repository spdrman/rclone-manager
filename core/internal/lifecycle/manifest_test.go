// These cover the recovery sidecar Commit writes beside a durable artifact,
// which is the read side of internal/recovery's whole reason to exist.
//
// The fixtures are heavier than the other commit tests' and that is the
// substance of the file. A manifest's job is to carry enough evidence to
// rebuild a journal row after the journal is gone, so a test that committed
// an artifact with no remote identity, no transfer result and no recorded
// hash would produce a manifest with every interesting field empty and would
// pass every round-trip assertion there is. walkToVerifiedWithEvidence
// exists so the artifact reaching Commit looks like one a real FR-11 and
// FR-13 run produced, and the assertions can then be about whether that
// evidence survived rather than about the file's shape.
package lifecycle

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/recovery"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// walkToVerifiedWithEvidence is walkToVerified plus the Remote/Transfer/
// Hash evidence a real FR-11/FR-13 run would already have recorded by the
// time Commit is called, so this file's tests can prove the sidecar
// manifest Commit writes actually carries that evidence through.
func walkToVerifiedWithEvidence(
	t *testing.T, ctx context.Context, d Deps, artifact model.ArtifactID, partial string,
	remote state.RemoteIdentity, transfer *state.TransferResult, hash *state.HashUpdate,
) {
	t.Helper()

	if _, err := Advance(ctx, d, state.Transition{
		Artifact: artifact, Key: "setup-0-DISCOVERED", From: "", To: string(Discovered),
		RemotePath: "/incoming/" + artifact.Name, Remote: &remote,
	}); err != nil {
		t.Fatalf("walkToVerifiedWithEvidence: -> DISCOVERED: %v", err)
	}
	if _, err := Advance(ctx, d, state.Transition{
		Artifact: artifact, Key: "setup-1-TRANSFERRING", From: string(Discovered), To: string(Transferring),
		LocalPath: &partial,
	}); err != nil {
		t.Fatalf("walkToVerifiedWithEvidence: -> TRANSFERRING: %v", err)
	}
	if _, err := Advance(ctx, d, state.Transition{
		Artifact: artifact, Key: "setup-2-TRANSFERRED", From: string(Transferring), To: string(Transferred),
		Transfer: transfer,
	}); err != nil {
		t.Fatalf("walkToVerifiedWithEvidence: -> TRANSFERRED: %v", err)
	}
	if _, err := Advance(ctx, d, state.Transition{
		Artifact: artifact, Key: "setup-3-VERIFYING", From: string(Transferred), To: string(Verifying),
	}); err != nil {
		t.Fatalf("walkToVerifiedWithEvidence: -> VERIFYING: %v", err)
	}
	if _, err := Advance(ctx, d, state.Transition{
		Artifact: artifact, Key: "setup-4-VERIFIED", From: string(Verifying), To: string(Verified),
		Hashes:     hash,
		Validation: &state.ValidationUpdate{Passed: true, Detail: "sha256 verified"},
	}); err != nil {
		t.Fatalf("walkToVerifiedWithEvidence: -> VERIFIED: %v", err)
	}
}

// TestCommit_WritesRecoveryManifestMatchingJournalRecord is issue #102's
// RED/GREEN test for GREEN item 1: the sidecar manifest Commit writes must
// carry the artifact's identity and the fields section 19.3 lists,
// matching exactly what the journal itself recorded.
func TestCommit_WritesRecoveryManifestMatchingJournalRecord(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	d := Deps{Journal: j}
	artifact := mustID(t)

	dir := t.TempDir()
	partial := mustPartialPath(t, dir, artifact)
	content := []byte("durable backup content")
	if err := os.WriteFile(partial, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	size := int64(len(content))
	modTime := time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)
	remote := state.RemoteIdentity{Size: &size, ModTime: &modTime}
	transfer := &state.TransferResult{BytesTransferred: size, Checksummed: true}
	hash := &state.HashUpdate{Hash: "deadbeef", Alg: "sha256"}

	walkToVerifiedWithEvidence(t, ctx, d, artifact, partial, remote, transfer, hash)

	out, err := Commit(ctx, d, CommitInput{
		Artifact:      artifact,
		LocalDir:      dir,
		CommittingKey: "manifest-commit-committing",
		CommittedKey:  "manifest-commit-committed",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	m, err := recovery.ReadManifest(recovery.ManifestPath(dir, artifact.Name))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}

	if m.Source != artifact.Set.Source || m.BackupSet != artifact.Set.Set || m.ArtifactName != artifact.Name {
		t.Errorf("manifest identity = %s/%s/%s, want %s", m.Source, m.BackupSet, m.ArtifactName, artifact)
	}
	if m.RemotePath != out.Record.RemotePath {
		t.Errorf("manifest RemotePath = %q, want %q", m.RemotePath, out.Record.RemotePath)
	}
	if !m.RetentionTimestamp.Equal(out.Record.DiscoveredAt) {
		t.Errorf("manifest RetentionTimestamp = %v, want journal DiscoveredAt %v", m.RetentionTimestamp, out.Record.DiscoveredAt)
	}
	if m.SizeBytes != size {
		t.Errorf("manifest SizeBytes = %d, want %d", m.SizeBytes, size)
	}
	if m.Checksum != "deadbeef" || m.ChecksumAlgorithm != "sha256" {
		t.Errorf("manifest checksum = %s/%s, want deadbeef/sha256", m.Checksum, m.ChecksumAlgorithm)
	}
	if m.ValidationPassed == nil || !*m.ValidationPassed {
		t.Errorf("manifest ValidationPassed = %v, want true", m.ValidationPassed)
	}
	if m.ProducerTimestamp == nil || !m.ProducerTimestamp.Equal(modTime) {
		t.Errorf("manifest ProducerTimestamp = %v, want %v", m.ProducerTimestamp, modTime)
	}
}

// TestCommit_ManifestFileNeverContainsForbiddenMarkers is issue #102's RED
// test asserting a sidecar manifest never contains an SSH private key, an
// auth token, a remote password or a secret env value, for every field
// this issue writes: it inspects the raw bytes actually written to disk,
// not just the Manifest struct's shape.
func TestCommit_ManifestFileNeverContainsForbiddenMarkers(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	d := Deps{Journal: j}
	artifact := mustID(t)

	dir := t.TempDir()
	partial := mustPartialPath(t, dir, artifact)
	content := []byte("payload unrelated to any credential")
	if err := os.WriteFile(partial, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	size := int64(len(content))
	walkToVerifiedWithEvidence(t, ctx, d, artifact, partial,
		state.RemoteIdentity{Size: &size},
		&state.TransferResult{BytesTransferred: size},
		&state.HashUpdate{Hash: "abc123", Alg: "sha256"},
	)

	if _, err := Commit(ctx, d, CommitInput{
		Artifact:      artifact,
		LocalDir:      dir,
		CommittingKey: "secret-check-committing",
		CommittedKey:  "secret-check-committed",
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	raw, err := os.ReadFile(recovery.ManifestPath(dir, artifact.Name))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, marker := range []string{
		"PRIVATE KEY", "BEGIN OPENSSH", "ssh-rsa", "ssh-ed25519",
		"token", "password", "Authorization:",
	} {
		if strings.Contains(string(raw), marker) {
			t.Errorf("recovery manifest content contains %q; sidecar manifests must never carry credential material (EPIC-B section 19.3)", marker)
		}
	}
}

// TestCommit_RetryAfterManifestLoss_RewritesManifestOnConvergedRetry
// proves writeRecoveryManifest's converged-branch call actually matters:
// simulate a manifest that went missing after a Commit call already fully
// succeeded (standing in for a crash between the COMMITTED journal write
// and the manifest write), and confirm a retry with the same keys
// converges *and* rewrites the manifest, rather than leaving it missing
// forever.
func TestCommit_RetryAfterManifestLoss_RewritesManifestOnConvergedRetry(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	d := Deps{Journal: j}
	artifact := mustID(t)

	dir := t.TempDir()
	partial := mustPartialPath(t, dir, artifact)
	content := []byte("evidence of a prior successful commit")
	if err := os.WriteFile(partial, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	size := int64(len(content))
	walkToVerifiedWithEvidence(t, ctx, d, artifact, partial,
		state.RemoteIdentity{Size: &size},
		&state.TransferResult{BytesTransferred: size},
		&state.HashUpdate{Hash: "abc123", Alg: "sha256"},
	)

	in := CommitInput{Artifact: artifact, LocalDir: dir, CommittingKey: "retry-committing", CommittedKey: "retry-committed"}
	if _, err := Commit(ctx, d, in); err != nil {
		t.Fatalf("first Commit: %v", err)
	}

	manifestPath := recovery.ManifestPath(dir, artifact.Name)
	if err := os.Remove(manifestPath); err != nil {
		t.Fatalf("simulating a crash before the manifest survived: %v", err)
	}

	if _, err := Commit(ctx, d, in); err != nil {
		t.Fatalf("second Commit (converged retry): %v", err)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Errorf("manifest was not rewritten on the converged retry: %v", err)
	}
}
