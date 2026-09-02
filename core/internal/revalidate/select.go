package revalidate

import (
	"sort"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// eligibleStates are the lifecycle states worth re-checking: a durable
// local final file has to actually exist for there to be anything to
// re-read. This is the same four-state set FR-19's last-known-good
// protection and internal/health's FR-24 computation already call known
// good. This package does not import either of those (health owns FR-24,
// retention owns FR-19, and this package's selection policy is allowed to
// agree with them by definition, both being grounded in the same "durable
// local copy exists" fact, rather than by a shared dependency neither of
// them exports for this purpose).
//
// RemoteRetained (issue #282) was missing here until issue #315: a
// read-only backup set's artifacts have exactly as durable a local final
// copy as a COMMITTED one, and Phase 4's whole reason to exist, bit rot
// does not announce itself, applies to them no less. Before this fix
// Phase 4 simply never looked at them again once retained, which is the
// gap issue #315 closes: a corrupted local copy for a read-only source is
// often undetectable any other way, since this manager never re-examines
// the remote either.
var eligibleStates = map[lifecycle.State]bool{
	lifecycle.Committed:           true,
	lifecycle.RemoteDeletePending: true,
	lifecycle.RemoteRetained:      true,
	lifecycle.Complete:            true,
}

// SelectDue is the pure half of scheduling: given records, cfg and now, it
// returns which artifacts are due for a fresh check, in the order they
// should be checked in, bounded to at most cfg.MaxPerCycle. It does no I/O
// and reaches for no clock of its own, the same discipline
// retention.GFSDecide already follows for the same reason: the same
// inputs must always produce the same answer.
//
// An artifact is due when its current state is one of eligibleStates and
// now.Sub(rec.UpdatedAt) >= cfg.Interval.Duration(). UpdatedAt stands in
// for "when this artifact was last checked" because, once a record is
// sitting in one of these three states, nothing touches its journal row
// afterward except this package's own same-state pass/fail writes (Run)
// and FR-17 reconciliation at startup, both of which are legitimate
// "this was looked at" events: a pass leaves UpdatedAt where a fresh check
// just confirmed things were fine, a fail moves the record out of
// eligibleStates entirely, and reconciliation either does the same or
// finds nothing wrong and leaves UpdatedAt unchanged from whenever the
// artifact last had a legitimate reason to update.
//
// cfg.MaxPerCycle <= 0 always returns nil, regardless of Interval or how
// many records are otherwise due: see config.Validate's own rule that this
// is the only way MaxPerCycle can be non-positive in a validated config,
// which is this package's way of spelling "revalidation is disabled for
// this backup set".
//
// The returned slice is sorted oldest-UpdatedAt-first, so the most
// overdue artifacts are the ones actually spent within a bounded
// MaxPerCycle, tie-broken by the artifact's own string form so two calls
// over identical input always pick the same subset in the same order
// (see PR#60's own reasoning for why this project treats that kind of
// determinism as load-bearing, not cosmetic).
func SelectDue(records []state.Record, cfg config.Revalidation, now time.Time) []state.Record {
	if cfg.MaxPerCycle <= 0 {
		return nil
	}
	interval := cfg.Interval.Duration()

	var due []state.Record
	for _, rec := range records {
		if !eligibleStates[lifecycle.State(rec.State)] {
			continue
		}
		if now.Sub(rec.UpdatedAt) < interval {
			continue
		}
		due = append(due, rec)
	}

	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].UpdatedAt.Equal(due[j].UpdatedAt) {
			return due[i].UpdatedAt.Before(due[j].UpdatedAt)
		}
		return due[i].Artifact.String() < due[j].Artifact.String()
	})

	if len(due) > cfg.MaxPerCycle {
		due = due[:cfg.MaxPerCycle]
	}
	return due
}
