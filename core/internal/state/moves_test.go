package state

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// This file tests the durable half of FR-30's journal directly: the
// claims placement_moves has to hold for the move engine's safety argument
// to mean anything.

var moveNow = time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)

func moveJournal(t *testing.T) (*Journal, model.ArtifactID) {
	t.Helper()
	ctx := context.Background()
	j, err := Open(ctx, filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	set := model.BackupSetID{Source: "production", Set: "pg"}
	artifact := model.ArtifactID{Set: set, Name: "a.dump"}
	size := int64(11)
	path := "/backups/pg/a.dump"

	if _, err := j.Discover(ctx, artifact, "k:discover", "remote/a.dump",
		RemoteIdentity{Size: &size}, moveNow); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, tr := range []Transition{
		{Artifact: artifact, Key: "k:transferring", From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: moveNow},
		{Artifact: artifact, Key: "k:transferred", From: "TRANSFERRING", To: "TRANSFERRED", OccurredAt: moveNow},
		{Artifact: artifact, Key: "k:verifying", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: moveNow},
		{Artifact: artifact, Key: "k:verified", From: "VERIFYING", To: "VERIFIED", OccurredAt: moveNow},
		{Artifact: artifact, Key: "k:committing", From: "VERIFIED", To: "COMMITTING", OccurredAt: moveNow},
		{Artifact: artifact, Key: "k:committed", From: "COMMITTING", To: "COMMITTED", OccurredAt: moveNow,
			Placement: &PlacementUpdate{Medium: MediumLocal, Location: path, Size: &size,
				Hash: strings.Repeat("c", 64), HashAlg: "sha256",
				VerificationClass: VerificationContent, Status: PlacementActive}},
		{Artifact: artifact, Key: "k:pending", From: "COMMITTED", To: "REMOTE_DELETE_PENDING", OccurredAt: moveNow},
		{Artifact: artifact, Key: "k:complete", From: "REMOTE_DELETE_PENDING", To: "COMPLETE", OccurredAt: moveNow},
	} {
		if _, err := j.RecordTransition(ctx, tr); err != nil {
			t.Fatalf("-> %s: %v", tr.To, err)
		}
	}
	return j, artifact
}

func planOne(t *testing.T, j *Journal, artifact model.ArtifactID) Move {
	t.Helper()
	mv, err := j.PlanMove(context.Background(), MovePlan{
		Artifact: artifact, SourceMedium: MediumLocal,
		DestinationMedium: "offsite", DestinationKey: "p/production/pg/a.dump",
		OccurredAt: moveNow,
	})
	if err != nil {
		t.Fatalf("PlanMove: %v", err)
	}
	return mv
}

// TestPlanMoveNamesTheCopyItPreserves is the reason source_placement_id is
// a foreign key rather than a medium string: a move row that cannot say
// which copy it is preserving is worse than no row.
func TestPlanMoveNamesTheCopyItPreserves(t *testing.T) {
	j, artifact := moveJournal(t)
	mv := planOne(t, j, artifact)

	if mv.Phase != MovePlanned {
		t.Errorf("a fresh move is at %s, want %s", mv.Phase, MovePlanned)
	}
	if mv.SourcePlacementID == nil {
		t.Fatal("the move names no source placement")
	}
	if mv.SourceMedium != MediumLocal || mv.SourceLocation != "/backups/pg/a.dump" {
		t.Errorf("the move resolves its source to %q at %q, want the local placement", mv.SourceMedium, mv.SourceLocation)
	}
	if mv.BytesCopied != nil {
		t.Error("a fresh move claims bytes were copied")
	}
	if mv.Terminal() {
		t.Error("a fresh move is terminal")
	}
}

// TestPlanMoveRefusesASourceWithNoActiveCopy is the conservative failure:
// a move off a medium the journal does not record a copy on cannot promise
// to preserve anything.
func TestPlanMoveRefusesASourceWithNoActiveCopy(t *testing.T) {
	j, artifact := moveJournal(t)
	_, err := j.PlanMove(context.Background(), MovePlan{
		Artifact: artifact, SourceMedium: "somewhere_else",
		DestinationMedium: "offsite", DestinationKey: "k", OccurredAt: moveNow,
	})
	if err == nil {
		t.Fatal("a move off a medium with no ACTIVE placement was planned")
	}
	if !strings.Contains(err.Error(), "no ACTIVE placement records a copy there") {
		t.Errorf("refused with %q; it should say the source copy cannot be named", err)
	}
}

// TestAPhaseWriteAndItsPlacementsLandTogetherOrNotAtAll is the claim the
// whole crash story rests on.
//
// Two of the phase writes carry a placement change with them: VERIFIED
// creates the destination's placement, DONE marks the source GONE. If
// those could come apart, a crash between them would leave a phase
// claiming a verified destination with no placement to prove it, or a
// source recorded as gone while the phase still says the delete had not
// been decided.
func TestAPhaseWriteAndItsPlacementsLandTogetherOrNotAtAll(t *testing.T) {
	ctx := context.Background()
	j, artifact := moveJournal(t)
	mv := planOne(t, j, artifact)

	size := int64(11)
	at := moveNow.Add(time.Minute)

	// A placement with no location is refused by upsertPlacement, which is
	// what makes this an honest atomicity test: the phase write in the same
	// call has already run by then, so if the two were not in one
	// transaction the phase would have moved and the placement would not.
	_, err := j.AdvanceMove(ctx, MoveAdvance{
		MoveID: mv.ID, From: MovePlanned, To: MoveCopying, OccurredAt: at,
		Placements: []PlacementUpdate{{Medium: "offsite", Location: "", Size: &size}},
	})
	if err == nil {
		t.Fatal("a placement with no location was accepted")
	}

	after, err := j.GetMove(ctx, mv.ID)
	if err != nil {
		t.Fatalf("GetMove: %v", err)
	}
	if after.Phase != MovePlanned {
		t.Errorf("the phase moved to %s even though the placement write in the same call failed; the two are not in one transaction", after.Phase)
	}
	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, p := range rec.Placements {
		if p.Medium == "offsite" {
			t.Error("a placement landed from a call that failed")
		}
	}
}

// TestAnAdvanceStatesThePhaseItIsLeaving is the wall under
// internal/placement's transition table: a write against a row that moved
// on affects no rows and is reported, rather than silently overwriting a
// phase somebody else set.
func TestAnAdvanceStatesThePhaseItIsLeaving(t *testing.T) {
	ctx := context.Background()
	j, artifact := moveJournal(t)
	mv := planOne(t, j, artifact)

	if _, err := j.AdvanceMove(ctx, MoveAdvance{
		MoveID: mv.ID, From: MovePlanned, To: MoveCopying, OccurredAt: moveNow,
	}); err != nil {
		t.Fatalf("the first advance failed: %v", err)
	}
	_, err := j.AdvanceMove(ctx, MoveAdvance{
		MoveID: mv.ID, From: MovePlanned, To: MoveCopied, OccurredAt: moveNow,
	})
	if !errors.Is(err, ErrMovePhaseConflict) {
		t.Fatalf("a stale advance returned %v, want ErrMovePhaseConflict", err)
	}
	after, err := j.GetMove(ctx, mv.ID)
	if err != nil {
		t.Fatalf("GetMove: %v", err)
	}
	if after.Phase != MoveCopying {
		t.Errorf("the row is at %s; a refused advance must change nothing", after.Phase)
	}
}

// TestTheSchemaRefusesAPhaseItDoesNotKnow proves the CHECK constraint in
// 0007_placements.sql is doing work, so internal/placement's vocabulary
// cannot quietly grow a rung the schema never heard of.
func TestTheSchemaRefusesAPhaseItDoesNotKnow(t *testing.T) {
	ctx := context.Background()
	j, artifact := moveJournal(t)
	mv := planOne(t, j, artifact)

	_, err := j.AdvanceMove(ctx, MoveAdvance{
		MoveID: mv.ID, From: MovePlanned, To: "HALF_DELETED", OccurredAt: moveNow,
	})
	if err == nil {
		t.Fatal("the schema accepted a phase it does not know")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "CONSTRAINT") {
		t.Errorf("refused with %q; the CHECK constraint should be what refused it", err)
	}
}

// TestListMovesFindsExactlyTheNonTerminalOnes is what restart
// reconciliation reads on startup.
func TestListMovesFindsExactlyTheNonTerminalOnes(t *testing.T) {
	ctx := context.Background()
	j, artifact := moveJournal(t)
	mv := planOne(t, j, artifact)

	live, err := j.ListMoves(ctx, MovePlanned, MoveCopying, MoveCopied,
		MoveVerifying, MoveVerified, MoveSourceDeletePending)
	if err != nil {
		t.Fatalf("ListMoves: %v", err)
	}
	if len(live) != 1 || live[0].ID != mv.ID {
		t.Fatalf("the non-terminal listing is %+v, want the one planned move", live)
	}

	if _, err := j.AdvanceMove(ctx, MoveAdvance{
		MoveID: mv.ID, From: MovePlanned, To: MoveAbandoned, OccurredAt: moveNow,
		Error: "nothing came of it",
	}); err != nil {
		t.Fatalf("abandoning: %v", err)
	}
	live, err = j.ListMoves(ctx, MovePlanned, MoveCopying, MoveCopied,
		MoveVerifying, MoveVerified, MoveSourceDeletePending)
	if err != nil {
		t.Fatalf("ListMoves: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("a terminal move is still in the non-terminal listing: %+v", live)
	}

	all, err := j.MovesForArtifact(ctx, artifact)
	if err != nil {
		t.Fatalf("MovesForArtifact: %v", err)
	}
	if len(all) != 1 || all[0].Error != "nothing came of it" {
		t.Errorf("the move's history is %+v; an abandoned move must keep the sentence explaining it", all)
	}
}

// TestAMoveErrorIsRedactedLikeEverythingElseThisJournalStores: the error
// text on a move row is the same kind of string internal/lifecycle's
// FAILED transitions carry, often straight out of a transport failure, so
// it goes through the same filter on the way to disk (issue #295).
func TestAMoveErrorIsRedactedLikeEverythingElseThisJournalStores(t *testing.T) {
	ctx := context.Background()
	j, artifact := moveJournal(t)
	mv := planOne(t, j, artifact)

	j.SetRedactor(obs.NewRedactor(obs.Endpoint{Host: "sftp.internal.example"}))
	if _, err := j.AdvanceMove(ctx, MoveAdvance{
		MoveID: mv.ID, From: MovePlanned, To: MoveAbandoned, OccurredAt: moveNow,
		Error: "the endpoint sftp.internal.example refused",
	}); err != nil {
		t.Fatalf("abandoning: %v", err)
	}
	after, err := j.GetMove(ctx, mv.ID)
	if err != nil {
		t.Fatalf("GetMove: %v", err)
	}
	if strings.Contains(after.Error, "sftp.internal.example") {
		t.Errorf("the stored error is %q; a sensitive endpoint must not reach the journal", after.Error)
	}
}
