package miniointegration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// The fixture helpers this package's cells share: a journal, a hash, and one
// artifact driven to the state the move engine leaves it in.
//
// Seeding through the journal directly is the honest route rather than a
// shortcut. Nothing in this tier runs a move for these cells to piggyback
// on, so the alternative is a test that stages its own placements inline,
// once per file, with each copy free to be subtly different. One helper means
// every cell here starts from the same artifact in the same state, and a
// change to what "on a medium" means is one edit rather than a search.

func sha256HexOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func openJournal(t *testing.T) *state.Journal {
	t.Helper()
	j, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// seedArtifactOnMedium drives an artifact to COMPLETE and then leaves its
// only ACTIVE placement on a storage medium, which is the state an
// artifact is in once the move engine (#238) has finished with it.
//
// It writes the placements through the journal directly, because that is
// the only writer there is: nothing in Phase 1 moves anything, and a test
// that waited for a mover would be a test that never runs.
func seedArtifactOnMedium(t *testing.T, j *state.Journal, artifact model.ArtifactID, key string, content []byte, mediumID string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	size := int64(len(content))
	localPath := "/backups/postgres-primary/" + artifact.Name

	if _, err := j.Discover(ctx, artifact, artifact.String()+":discover", "backups/"+artifact.Name,
		state.RemoteIdentity{Size: &size}, at); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, tr := range []state.Transition{
		{Artifact: artifact, Key: artifact.String() + ":transferring", From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: at, LocalPath: &localPath},
		{Artifact: artifact, Key: artifact.String() + ":transferred", From: "TRANSFERRING", To: "TRANSFERRED", OccurredAt: at, Transfer: &state.TransferResult{BytesTransferred: size}},
		{Artifact: artifact, Key: artifact.String() + ":verifying", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: at},
		{Artifact: artifact, Key: artifact.String() + ":verified", From: "VERIFYING", To: "VERIFIED", OccurredAt: at, Hashes: &state.HashUpdate{Hash: sha256HexOf(content), Alg: "sha256"}},
		{Artifact: artifact, Key: artifact.String() + ":committing", From: "VERIFIED", To: "COMMITTING", OccurredAt: at},
		{Artifact: artifact, Key: artifact.String() + ":committed", From: "COMMITTING", To: "COMMITTED", OccurredAt: at, LocalPath: &localPath,
			Placement: &state.PlacementUpdate{Medium: state.MediumLocal, Location: localPath, Size: &size,
				Hash: sha256HexOf(content), HashAlg: "sha256", VerificationClass: state.VerificationContent, Status: state.PlacementActive}},
		{Artifact: artifact, Key: artifact.String() + ":pending", From: "COMMITTED", To: "REMOTE_DELETE_PENDING", OccurredAt: at},
		{Artifact: artifact, Key: artifact.String() + ":complete", From: "REMOTE_DELETE_PENDING", To: "COMPLETE", OccurredAt: at},
		{Artifact: artifact, Key: artifact.String() + ":gone-local", From: "COMPLETE", To: "COMPLETE", OccurredAt: at,
			Placement: &state.PlacementUpdate{Medium: state.MediumLocal, Location: localPath, Status: state.PlacementGone}},
		{Artifact: artifact, Key: artifact.String() + ":on-medium", From: "COMPLETE", To: "COMPLETE", OccurredAt: at,
			Placement: &state.PlacementUpdate{Medium: mediumID, Location: key, Size: &size,
				Hash: sha256HexOf(content), HashAlg: "sha256", VerificationClass: state.VerificationContent, Status: state.PlacementActive}},
	} {
		if _, err := j.RecordTransition(ctx, tr); err != nil {
			t.Fatalf("-> %s (%s): %v", tr.To, tr.Key, err)
		}
	}
}
