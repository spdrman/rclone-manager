// What the artifact detail prints for a backup whose local copy is gone
// because a move took it away.
//
// The fixture walks the move journal's real phases in the real order,
// writing the placement facts the engine writes at each one, rather than
// poking two rows into the placements table. It has to build the state by
// hand for one reason only: a real move needs a reachable medium and no CLI
// subcommand has one. That the engine really does leave this shape behind is
// proved separately, end to end, one layer down.
//
// The claim is a negative, which is why it is worth a file. An artifact
// whose bytes have moved offsite must not still be described as having a
// local copy, and the way that regresses is not an error, it is a stale line
// on a screen that reads exactly like a healthy one.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

var movedFixtureEpoch = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// stageMovedArtifact drives one artifact from ingestion through a
// completed move, against the config's own state database, and returns
// its id.
//
// It walks the move journal's real phases in the real order, with the
// placement facts the engine writes at each one (internal/placement/
// engine.go: the destination becomes ACTIVE at VERIFIED, the source
// becomes DELETE_PENDING at SOURCE_DELETE_PENDING and GONE at DONE),
// rather than poking two rows into the placements table. That the ENGINE
// leaves exactly this shape behind is proved separately and end to end by
// TestACompletedMoveLeavesNoLocalCopyOnTheDetailSurface in
// core/internal/app; this fixture exists because the move that produces
// it needs a reachable medium, and no CLI subcommand has one.
func stageMovedArtifact(t *testing.T, configPath, name string) model.ArtifactID {
	t.Helper()
	dir := filepath.Dir(configPath)
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	ctx := context.Background()
	j, err := state.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = j.Close() }()

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}

	payload := "payload for " + name
	local := filepath.Join(localDir, name)
	sum := sha256.Sum256([]byte(payload))
	hash := hex.EncodeToString(sum[:])
	size := int64(len(payload))

	if _, err := j.Discover(ctx, artifact, name+"-discover", "/backups/"+name, state.RemoteIdentity{}, movedFixtureEpoch); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: name + "-complete",
		From: string(lifecycle.Discovered), To: string(lifecycle.Complete),
		OccurredAt: movedFixtureEpoch.Add(time.Minute), LocalPath: &local,
		Transfer: &state.TransferResult{BytesTransferred: size, Checksummed: true},
		Placement: &state.PlacementUpdate{
			Medium: state.MediumLocal, Location: local, Size: &size,
			Hash: hash, HashAlg: "sha256",
			VerificationClass: state.VerificationContent, Status: state.PlacementActive,
		},
	}); err != nil {
		t.Fatalf("RecordTransition(complete): %v", err)
	}

	key := "rclone-manager/production/postgres-primary/" + name
	mv, err := j.PlanMove(ctx, state.MovePlan{
		Artifact: artifact, SourceMedium: state.MediumLocal,
		DestinationMedium: "cold_offsite", DestinationKey: key,
		OccurredAt: movedFixtureEpoch.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("PlanMove: %v", err)
	}

	verifiedAt := movedFixtureEpoch.Add(5 * time.Minute)
	steps := []state.MoveAdvance{
		{From: state.MovePlanned, To: state.MoveCopying},
		{From: state.MoveCopying, To: state.MoveCopied, BytesCopied: &size},
		{From: state.MoveCopied, To: state.MoveVerifying},
		{From: state.MoveVerifying, To: state.MoveVerified, Placements: []state.PlacementUpdate{{
			Medium: "cold_offsite", Location: key, Size: &size, Hash: hash, HashAlg: "sha256",
			VerificationClass: state.VerificationContent, VerifiedAt: &verifiedAt, Status: state.PlacementActive,
		}}},
		{From: state.MoveVerified, To: state.MoveSourceDeletePending, Placements: []state.PlacementUpdate{{
			Medium: state.MediumLocal, Location: local, Size: &size, Hash: hash, HashAlg: "sha256",
			VerificationClass: state.VerificationContent, Status: state.PlacementDeletePending,
		}}},
		{From: state.MoveSourceDeletePending, To: state.MoveDone, Placements: []state.PlacementUpdate{{
			Medium: state.MediumLocal, Location: local, Size: &size, Hash: hash, HashAlg: "sha256",
			VerificationClass: state.VerificationContent, Status: state.PlacementGone,
		}}},
	}
	for i, step := range steps {
		step.MoveID = mv.ID
		step.OccurredAt = movedFixtureEpoch.Add(time.Duration(3+i) * time.Minute)
		if _, err := j.AdvanceMove(ctx, step); err != nil {
			t.Fatalf("AdvanceMove %s -> %s: %v", step.From, step.To, err)
		}
	}

	// The source file really is gone, because the real move really does
	// delete it. A fixture that left it there would let a later change
	// pass this test by reading the disk instead of the journal.
	if err := os.Remove(local); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing the moved local copy: %v", err)
	}
	return artifact
}

// TestRun_ArtifactsPrintsNoLocalCopyBlockForACopyThatIsGone is FR-34 on
// the surface a terminal operator reads.
//
// After a completed move the local placement is GONE, and the copy block
// is a layout for a copy: location, access, storage class, what verified
// it, what could still be checked, whether reading it is billed. Every
// one of those is computed as though the file were there, because the
// journal still carries the row's hash and class. So one line says GONE
// and five contradict it.
//
// core/service already refuses to serve such a row to the API, and says
// why in the one sentence this test is named after: a row for a copy that
// is not there reads as a copy in every layout anyone would write for
// one. The CLI prints that layout, and FR-34's own rule is that the two
// surfaces read the same truth about the same artifact.
func TestRun_ArtifactsPrintsNoLocalCopyBlockForACopyThatIsGone(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)
	artifact := stageMovedArtifact(t, configPath, "moved.dump")

	out := captureStdout(t, func() {
		if got := run([]string{"artifacts", "--config", configPath, artifact.String()}); got != 0 {
			t.Fatalf("artifacts %s: %d, want 0", artifact, got)
		}
	})

	if strings.Contains(out, "copy:                local") {
		t.Errorf("the detail prints a copy block for the local copy the move deleted:\n%s", out)
	}
	if strings.Contains(out, state.PlacementGone) {
		t.Errorf("the detail carries the word %s, which only a copy row could have put there:\n%s", state.PlacementGone, out)
	}

	// The control, and it is the half that makes the assertions above
	// mean something: the offsite copy that DOES exist is printed, in
	// full. Without it a command that had stopped printing copies
	// altogether would pass.
	for _, want := range []string{
		"copy:                cold_offsite",
		"location:          rclone-manager/production/postgres-primary/moved.dump",
		"status:            " + state.PlacementActive,
		"access:            immediate",
		"verified_as:       " + state.VerificationContent,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the detail does not contain %q, so the copy that IS there is not being reported:\n%s", want, out)
		}
	}
}
