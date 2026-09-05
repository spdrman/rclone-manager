package health

import (
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file is FR-24's placement half (issue #444): not "are this backup
// set's restore points fresh", which the rest of the package answers, but
// "are they where the operator's retention chain says they belong, and is
// this manager getting them there".
//
// # Why health had to grow a second question at all
//
// EPIC E made the storage medium selectable per retention tier, and #441
// made a move's outcome visible on the exit status, in the activity feed,
// in the FR-23 event stream and on the dashboard's last-run-cycle panel.
// All four of those are statements about ONE PASS. An operator opening a
// status page on a deployment nobody has run `backup-manager run` in
// front of sees none of them, so a deployment whose moves had been
// failing for a week reported itself healthy, on every surface, forever.
//
// The fix is not to carry a cycle count into the health report. A cycle
// count is the wrong kind of fact for a verdict that has to survive the
// process that computed it: it lives in memory and it is gone by the time
// anybody looks. So this reads the durable state instead, which is the
// placements rows joined against the resolved chain (the caller's join,
// see PlacementEvidence) and the placement_moves journal.
//
// # A signal that cannot go red is worse than no signal
//
// That is the whole design constraint here, and it cuts both ways.
//
// It has to be able to fire. FailedMoves is what makes the verdict move,
// and it comes off a fact the move engine already writes durably: a
// failed copy leaves the move at COPYING and records the reason on the
// row (internal/placement's copy() says so in as many words, and does it
// precisely so that "a move stuck for a week against a permanent refusal"
// stops looking identical to one that started ten seconds ago). Every
// subsequent cycle rewrites that reason, so the row's error is always the
// LAST attempt's, and its created_at is still the moment the move was
// planned. Those two together are the week.
//
// It also has to be able to stay green when nothing is wrong, or it gets
// turned off and then it is worth nothing when it fires for real. An
// artifact away from home with a move in flight and nothing recorded
// against it is the ordinary state of a working deployment on every
// cycle, so AwayFromHome on its own changes no verdict. Only a
// relocation that has been TRIED and has not worked does.
//
// # What is deliberately not decided here
//
// Where an artifact belongs is FR-27's home-medium rule, and
// internal/retention.HomeMedium is the ONE derivation of it (issue #239's
// refactor line: a second answer to "where does this artifact live" is a
// second policy, and the two would eventually disagree about a deletion).
// This package does not re-derive it, does not import config, and never
// sees a retention chain. It is handed the answer, the same way it is
// handed the reinstatement history, and for the same reason: the caller
// has already made that read and a second one would drift.

// AwayFromHome names one artifact whose confirmed durable copy is not on
// the medium its backup set's retention chain says it belongs on.
//
// It is retention.HomeMove in this package's own vocabulary rather than
// that type directly, which keeps internal/retention (and through it
// internal/config) out of this package's imports. The mapping at the call
// site is one line and the independence is worth more than the line: this
// package computes a verdict, and it must not acquire a way to reach a
// policy it could then read differently.
type AwayFromHome struct {
	Artifact model.ArtifactID

	// On is the medium the artifact's one ACTIVE placement is on, and
	// Home is the medium its chain names. They are always different: an
	// artifact already at home is not an entry here.
	On   string
	Home string
}

// PlacementEvidence is what the caller has already established about
// where this backup set's artifacts are. Like the reinstatement history,
// it is a positional argument to ComputeBackupSetHealth rather than a
// field on BackupSetInputs, and the split is the same one:
// BackupSetInputs holds readings that can legitimately be unavailable and
// that never reach decideState, while this one is a plain read of durable
// state that the health pass already has the database open for, and it
// DOES reach decideState. There is no honest "unknown" for it.
//
// The zero value is the honest reading for every deployment that declares
// no storage medium: nothing is away from home because every tier's
// medium is local, and no move has ever been planned. That is asserted
// rather than assumed (TestADeploymentWithNoMediumReportsNothingNew).
type PlacementEvidence struct {
	// AwayFromHome is every artifact in this set whose confirmed durable
	// copy is not on the medium its chain names, as
	// retention.PlanHomeMoves worked it out from the same records this
	// health pass is reading.
	AwayFromHome []AwayFromHome

	// Unconfirmed is how many artifacts the planner could not place at
	// all: no ACTIVE placement (nothing durable yet, or nothing
	// recorded), or more than one (a move is mid-flight and "where is
	// this" has two answers).
	//
	// It is carried rather than dropped because "I could not confirm
	// where this is" is a different fact from "this is at home", and
	// collapsing the two is exactly the direction this whole report
	// exists to stop. It changes no verdict: an unconfirmed location is
	// not evidence of a problem, only evidence that this pass could not
	// answer.
	Unconfirmed int

	// Moves is what placement_moves holds for this backup set. The
	// caller normally passes the engine's own resume population, the
	// non-terminal phases, because a move that is over is not an
	// outstanding relocation and the alternative is reading every row a
	// deployment has ever written on every status call.
	//
	// Terminal rows are still tolerated rather than rejected: everything
	// below re-checks Terminal() instead of trusting the filter, so a
	// caller that hands over the whole table gets the same answer and a
	// filter that drifts from the phase machine cannot inflate a count.
	Moves []state.Move
}

// PlacementHealth is the placement half of one backup set's FR-24
// verdict: how far its artifacts are from the mediums its chain names,
// and whether this manager's attempts to close that gap are getting
// anywhere.
//
// Every count here is a real reading, so none of them is ever omitted or
// left to a caller to guess at. The two ages are pointers because there
// genuinely is no age when the count beside them is zero, and a zero
// duration would read as "this happened just now", which is the one thing
// an absent reading must never be mistaken for.
type PlacementHealth struct {
	// AwayFromHome is how many of this set's artifacts have a confirmed
	// durable copy somewhere other than where their chain says.
	//
	// On its own this is not a complaint. A retention pass that has just
	// decided an artifact belongs offsite puts it here, and the move that
	// carries it there runs in the same cycle.
	AwayFromHome int

	// OldestAwayFromHomeAge is how long the oldest of those copies has
	// existed on the medium it is sitting on, from its placements row's
	// created_at.
	//
	// Read it for exactly what it says. It is NOT "how long this artifact
	// has been in the wrong place", because nothing durable records when
	// an artifact's home last changed: an operator who repointed a
	// monthly tier at a bucket this morning made a copy created a year
	// ago away-from-home this morning, and this would report a year. It
	// is an upper bound, and it is the only one available that is not
	// invented. The number that answers "how long has this been failing"
	// is OldestFailedMoveAge below, which is measured from a moment this
	// manager actually wrote down.
	//
	// Nil when AwayFromHome is zero, and also nil in the corner where no
	// away-from-home artifact could be found among the records this pass
	// read, which is a race between two reads rather than a state a
	// deployment sits in.
	OldestAwayFromHomeAge *time.Duration

	// UnconfirmedLocation is PlacementEvidence.Unconfirmed, carried
	// through: artifacts this pass could not place, which are not thereby
	// at home. See that field for what puts an artifact here.
	UnconfirmedLocation int

	// OpenMoves is how many relocations this set has that the move
	// journal has not finished, in any non-terminal phase.
	OpenMoves int

	// OldestOpenMoveAge is how long the oldest of those has been open,
	// from when this manager planned it, whether or not anything has been
	// recorded against it.
	//
	// It exists because FailedMoves cannot see every way a move gets
	// stuck. The engine writes the reason onto the row when a COPY fails,
	// which is the common case and the one this issue was reported for,
	// but its standing refusal immediately before a source delete (a
	// destination that cannot be re-verified at the class the medium
	// requires) deliberately changes nothing at all: it returns, leaves
	// the phase and the placements exactly as they were, and reports the
	// reason to a cycle report that nobody is reading. A move parked
	// there carries no error, so it is open and not failed, and this is
	// the number that still grows.
	//
	// It changes no verdict, because "open for a while" has no honest
	// threshold here: a single copy can legitimately outlast a poll
	// interval, and picking a multiple of one would be inventing a policy
	// rather than reading a fact. It is reported and exported so an
	// operator can alert on the shape their own deployment has. The
	// engine recording that refusal on the row, the way copy() already
	// records its own, would let FailedMoves cover it.
	//
	// Nil exactly when OpenMoves is zero.
	OldestOpenMoveAge *time.Duration

	// FailedMoves is the subset of OpenMoves whose last attempt failed
	// and left the reason on the row. This is the one number here that
	// changes the verdict.
	//
	// The engine rewrites a move's error on every advance, including with
	// the empty string, so that a move which recovers does not keep
	// carrying the sentence explaining a failure it survived. A non-empty
	// error on a row that has not reached DONE or ABANDONED therefore
	// means: this was tried, it did not work, and it is still outstanding.
	FailedMoves int

	// OldestFailedMoveAge is how long the oldest failing relocation has
	// been open, measured from when this manager wrote the move down. It
	// is the difference between a blip and a wedge: a transient upload
	// failure clears on the next cycle, and one that does not turns this
	// into days and then weeks.
	//
	// Nil exactly when FailedMoves is zero.
	OldestFailedMoveAge *time.Duration

	// FailedMoveReason is what the engine last recorded on that oldest
	// failing move. It is carried because the count alone cannot tell an
	// operator whether to look at a bucket policy, a credential or a
	// network, and because the cycle report that used to hold this
	// sentence is in memory and gone by the time anybody reads a status
	// page.
	FailedMoveReason string
}

// buildPlacementHealth turns the caller's evidence into the reported
// numbers and the one fact decideState is allowed to see.
//
// records is this set's journal rows, already loaded, and is read only to
// find the age of an away-from-home copy: the placements are on the
// records, so this needs no second query and cannot disagree with the
// rows the rest of the report was computed from.
func buildPlacementHealth(ev PlacementEvidence, records []state.Record, now time.Time) PlacementHealth {
	out := PlacementHealth{
		AwayFromHome:        len(ev.AwayFromHome),
		UnconfirmedLocation: ev.Unconfirmed,
	}

	if len(ev.AwayFromHome) > 0 {
		byArtifact := make(map[model.ArtifactID]state.Record, len(records))
		for _, r := range records {
			byArtifact[r.Artifact] = r
		}
		var oldest time.Time
		for _, away := range ev.AwayFromHome {
			rec, ok := byArtifact[away.Artifact]
			if !ok {
				// Two reads of a live database, so a row can go between
				// them. Skipped rather than defaulted: the artifact is
				// still counted above, because the planner saw it, and no
				// age is invented for a placement this pass cannot read.
				continue
			}
			for _, p := range rec.Placements {
				if p.Medium != away.On || p.Status != state.PlacementActive {
					continue
				}
				if oldest.IsZero() || p.CreatedAt.Before(oldest) {
					oldest = p.CreatedAt
				}
			}
		}
		if !oldest.IsZero() {
			out.OldestAwayFromHomeAge = ageOf(&oldest, now)
		}
	}

	var oldestOpen, oldestFailed *state.Move
	for i := range ev.Moves {
		mv := ev.Moves[i]
		if mv.Terminal() {
			continue
		}
		out.OpenMoves++
		if oldestOpen == nil || mv.CreatedAt.Before(oldestOpen.CreatedAt) {
			oldestOpen = &ev.Moves[i]
		}
		if mv.Error == "" {
			continue
		}
		out.FailedMoves++
		if oldestFailed == nil || mv.CreatedAt.Before(oldestFailed.CreatedAt) {
			oldestFailed = &ev.Moves[i]
		}
	}
	if oldestOpen != nil {
		planned := oldestOpen.CreatedAt
		out.OldestOpenMoveAge = ageOf(&planned, now)
	}
	if oldestFailed != nil {
		planned := oldestFailed.CreatedAt
		out.OldestFailedMoveAge = ageOf(&planned, now)
		out.FailedMoveReason = oldestFailed.Error
	}

	return out
}

// placementReason is the sentence decideState hands back when a
// relocation has been failing. It names the count and the age, because
// "one move is failing" and "one move has been failing for nine days" ask
// an operator for very different amounts of urgency, and the age is the
// half that a per-cycle report could never have carried.
func placementReason(p PlacementHealth) string {
	age := "an unknown length of time"
	if p.OldestFailedMoveAge != nil {
		age = p.OldestFailedMoveAge.Round(time.Minute).String()
	}
	plural := "s"
	if p.FailedMoves == 1 {
		plural = ""
	}
	return fmt.Sprintf(
		"a known-good backup exists within the stale threshold, but %d relocation%s this backup set's retention chain asked for %s failing, the oldest for %s: %s",
		p.FailedMoves, plural, isAre(p.FailedMoves), age, p.FailedMoveReason)
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
