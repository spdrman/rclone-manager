package health

import (
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file is issue #444: FR-24 was medium-blind, so a week of failing
// moves left every surface reporting a healthy deployment.
//
// The rule these tests exist to hold is that the signal can go red. A
// health field that only ever reports the reassuring value is worse than
// no field at all, because it converts "nobody has checked" into a green
// tick, so every test here is a pair: a control that establishes the set
// really is HEALTHY on the evidence, and then the same set with one real
// failure planted in the move journal.

// placedRec is rec() plus the ACTIVE placement an artifact with durable
// bytes really carries, because the away-from-home age is read off that
// row's created_at and a record without one would let the age assertion
// pass against a computation that never looked.
func placedRec(name string, st lifecycle.State, discovered, updated time.Time, medium string, placedAt time.Time) state.Record {
	r := rec(name, st, discovered, updated)
	r.Placements = []state.Placement{{
		Medium: medium, Location: "/local/" + name, Status: state.PlacementActive,
		CreatedAt: placedAt, UpdatedAt: placedAt,
	}}
	return r
}

func freshSet(now time.Time) []state.Record {
	return []state.Record{
		placedRec("nightly.dump", lifecycle.Complete, now.Add(-2*time.Hour), now.Add(-time.Hour), state.MediumLocal, now.AddDate(0, 0, -40)),
	}
}

// openMove is a move row in a non-terminal phase, with whatever the engine
// last recorded on it. A failing copy leaves exactly this: the phase stays
// COPYING and the reason is written onto the row (see internal/placement's
// copy(), which says so in as many words), so the row below is the shape
// production writes rather than one invented here.
func openMove(artifact, destination, phase, why string, planned time.Time) state.Move {
	return state.Move{
		Artifact:          mustArtifact(artifact),
		DestinationMedium: destination,
		DestinationKey:    "rclone-manager/" + artifact,
		Phase:             phase,
		Error:             why,
		CreatedAt:         planned,
		UpdatedAt:         planned,
	}
}

// TestAFailingMoveTurnsAHealthySetDegraded is the whole issue in one test.
// The control comes first and is not decoration: without it a DEGRADED
// verdict below would prove nothing, because it could equally be a set
// that was never HEALTHY in the first place.
func TestAFailingMoveTurnsAHealthySetDegraded(t *testing.T) {
	now := time.Now().UTC()
	records := freshSet(now)

	control := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{}, day, BackupSetInputs{}, now)
	if control.State != Healthy {
		t.Fatalf("control State = %s (%s), want %s; nothing below means anything unless this set really is healthy on its backup evidence alone",
			control.State, control.Reason, Healthy)
	}

	week := now.AddDate(0, 0, -7)
	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{
		AwayFromHome: []AwayFromHome{{Artifact: mustArtifact("nightly.dump"), On: state.MediumLocal, Home: "cold_offsite"}},
		Moves: []state.Move{
			openMove("nightly.dump", "cold_offsite", state.MoveCopying, "uploading to cold_offsite: connection refused", week),
		},
	}, day, BackupSetInputs{}, now)

	if got.State != Degraded {
		t.Fatalf("State = %s (%s), want %s: a relocation this deployment asked for has been failing for a week and the status page still reads green",
			got.State, got.Reason, Degraded)
	}
	if got.Placement.FailedMoves != 1 {
		t.Errorf("Placement.FailedMoves = %d, want 1", got.Placement.FailedMoves)
	}
	if got.Placement.OpenMoves != 1 {
		t.Errorf("Placement.OpenMoves = %d, want 1", got.Placement.OpenMoves)
	}
	if got.Placement.OldestFailedMoveAge == nil || *got.Placement.OldestFailedMoveAge < 7*day {
		t.Errorf("Placement.OldestFailedMoveAge = %v, want at least a week: how long it has been failing is the difference between a blip and a wedge",
			got.Placement.OldestFailedMoveAge)
	}
	if got.Placement.FailedMoveReason == "" {
		t.Error("Placement.FailedMoveReason is empty; the engine recorded why on the row and an operator has to be able to read it")
	}
	if got.Placement.AwayFromHome != 1 {
		t.Errorf("Placement.AwayFromHome = %d, want 1", got.Placement.AwayFromHome)
	}
	if got.Placement.OldestAwayFromHomeAge == nil || *got.Placement.OldestAwayFromHomeAge < 40*day {
		t.Errorf("Placement.OldestAwayFromHomeAge = %v, want the age of the copy it is sitting on (40 days)",
			got.Placement.OldestAwayFromHomeAge)
	}
}

// TestAMoveInFlightWithNothingWrongIsNotDegraded is the other half of "it
// can go red": a signal that goes red at the ordinary state of a working
// deployment gets turned off, and then it is worth nothing when it fires
// for real. A retention pass that has just planned a move has artifacts
// away from home by construction, on every cycle, forever.
func TestAMoveInFlightWithNothingWrongIsNotDegraded(t *testing.T) {
	now := time.Now().UTC()
	records := freshSet(now)

	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{
		AwayFromHome: []AwayFromHome{{Artifact: mustArtifact("nightly.dump"), On: state.MediumLocal, Home: "cold_offsite"}},
		Moves: []state.Move{
			openMove("nightly.dump", "cold_offsite", state.MoveCopying, "", now.Add(-time.Minute)),
		},
	}, day, BackupSetInputs{}, now)

	if got.State != Healthy {
		t.Fatalf("State = %s (%s), want %s: an artifact on its way to the medium its chain names, with nothing recorded against the move, is a deployment working exactly as configured",
			got.State, got.Reason, Healthy)
	}
	if got.Placement.AwayFromHome != 1 || got.Placement.OpenMoves != 1 {
		t.Errorf("Placement = %+v, want the move still reported (it is a fact) even though it changes no verdict", got.Placement)
	}
	if got.Placement.FailedMoves != 0 {
		t.Errorf("Placement.FailedMoves = %d, want 0: nothing has failed yet", got.Placement.FailedMoves)
	}
}

// TestAFinishedMoveIsNotAFailure pins the direction the signal has to be
// able to travel back in. A move that was abandoned or completed is over,
// and an operator who fixed the endpoint must see the deployment go green
// again, otherwise the first thing they learn is that this field is noise.
func TestAFinishedMoveIsNotAFailure(t *testing.T) {
	now := time.Now().UTC()
	records := freshSet(now)

	got := ComputeBackupSetHealth(testSet, records, nil, PlacementEvidence{
		Moves: []state.Move{
			openMove("nightly.dump", "cold_offsite", state.MoveDone, "", now.AddDate(0, 0, -7)),
			openMove("older.dump", "cold_offsite", state.MoveAbandoned, "the destination could not be verified", now.AddDate(0, 0, -7)),
		},
	}, day, BackupSetInputs{}, now)

	if got.State != Healthy {
		t.Fatalf("State = %s (%s), want %s: both moves are terminal, so nothing is outstanding", got.State, got.Reason, Healthy)
	}
	if got.Placement.OpenMoves != 0 || got.Placement.FailedMoves != 0 {
		t.Errorf("Placement = %+v, want no open or failed moves: DONE and ABANDONED are both over", got.Placement)
	}
}

// TestADeploymentWithNoMediumReportsNothingNew is the issue's second
// acceptance line, asserted rather than assumed. Every deployment that
// predates EPIC E passes empty placement evidence, and it must read
// exactly as it did before this field existed.
func TestADeploymentWithNoMediumReportsNothingNew(t *testing.T) {
	now := time.Now().UTC()
	got := ComputeBackupSetHealth(testSet, freshSet(now), nil, PlacementEvidence{}, day, BackupSetInputs{}, now)

	if got.State != Healthy {
		t.Fatalf("State = %s (%s), want %s", got.State, got.Reason, Healthy)
	}
	if got.Placement != (PlacementHealth{}) {
		t.Errorf("Placement = %+v, want the zero value: a deployment that declares no storage medium has nothing new to say", got.Placement)
	}
}

// TestAFailingMoveNeverMasksAWorseVerdict keeps this field in its place.
// FAILING and STALE are statements about the backups themselves; where
// those backups are stored is a lesser problem, and a set holding an
// irrecoverable artifact must not start reading DEGRADED because a move
// is also stuck.
func TestAFailingMoveNeverMasksAWorseVerdict(t *testing.T) {
	now := time.Now().UTC()
	evidence := PlacementEvidence{
		AwayFromHome: []AwayFromHome{{Artifact: mustArtifact("lost.dump"), On: state.MediumLocal, Home: "cold_offsite"}},
		Moves: []state.Move{
			openMove("lost.dump", "cold_offsite", state.MoveCopying, "connection refused", now.AddDate(0, 0, -7)),
		},
	}

	failing := []state.Record{placedRec("lost.dump", lifecycle.QuarantinedLost, now.Add(-2*time.Hour), now.Add(-time.Hour), state.MediumLocal, now.AddDate(0, 0, -40))}
	if got := ComputeBackupSetHealth(testSet, failing, nil, evidence, day, BackupSetInputs{}, now); got.State != Failing {
		t.Errorf("State = %s (%s), want %s: a QUARANTINED_LOST artifact is checked before anything else", got.State, got.Reason, Failing)
	}

	stale := []state.Record{placedRec("lost.dump", lifecycle.Complete, now.AddDate(0, 0, -40), now.AddDate(0, 0, -40), state.MediumLocal, now.AddDate(0, 0, -40))}
	if got := ComputeBackupSetHealth(testSet, stale, nil, evidence, day, BackupSetInputs{}, now); got.State != Stale {
		t.Errorf("State = %s (%s), want %s: the freshness guarantee being broken outranks a copy being in the wrong place", got.State, got.Reason, Stale)
	}
}

// TestUnconfirmedPlacementsAreReportedNotCountedAsHome is the honesty
// half. An artifact whose location this pass could not name is not
// thereby at home, and reporting it as such is the collapse the whole
// away-from-home count exists to end.
func TestUnconfirmedPlacementsAreReportedNotCountedAsHome(t *testing.T) {
	now := time.Now().UTC()
	got := ComputeBackupSetHealth(testSet, freshSet(now), nil, PlacementEvidence{Unconfirmed: 3}, day, BackupSetInputs{}, now)

	if got.Placement.UnconfirmedLocation != 3 {
		t.Errorf("Placement.UnconfirmedLocation = %d, want 3", got.Placement.UnconfirmedLocation)
	}
	if got.Placement.AwayFromHome != 0 {
		t.Errorf("Placement.AwayFromHome = %d, want 0: an unconfirmed location is not an away-from-home artifact either", got.Placement.AwayFromHome)
	}
}
