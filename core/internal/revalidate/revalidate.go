// Package revalidate implements Phase 4's scheduled revalidation
// (docs/EPIC.md): re-checking artifacts that have already reached a
// durable, "already passed" state (COMMITTED, REMOTE_DELETE_PENDING or
// COMPLETE), on a cadence and at a scope an operator configures
// (config.Revalidation) rather than on every cycle. Bit rot does not
// announce itself: a backup that verified six months ago is not
// guaranteed to still verify today, and the only way to find out before a
// restore is actually needed is to ask again.
//
// # Why this is not FR-17 reconciliation running more often
//
// internal/reconcile's checkLocalFinal already re-hashes a durable local
// copy against the checksum FR-13 recorded for it, but only once, at
// startup, for every artifact in the backup set, unconditionally. That is
// the right shape for FR-17's job (bringing the journal back in line with
// reality after a crash) and the wrong shape for this one: a long-running
// process does not restart on any predictable cadence, so FR-17's pass
// might not run again for weeks, and even where it does, re-reading a
// NAS's worth of already-verified data on every invocation has a real I/O
// cost. config.Revalidation.Interval and MaxPerCycle exist to bound
// exactly that cost: only artifacts overdue by Interval are eligible at
// all (SelectDue), and at most MaxPerCycle of those are actually checked
// in one Run call, so a backlog of simultaneously-due artifacts (for
// example right after a large initial backfill) spreads out over several
// calls instead of turning into one unbounded sweep.
//
// This package does not invent new lifecycle edges for what it finds, and
// it does not reimplement reconcile.go's routing logic: a failed recheck
// is routed through the exact same lifecycle.Advance calls reconcile.go
// already makes for "the durable local copy was found invalid after the
// fact" (COMMITTED or REMOTE_DELETE_PENDING -> QUARANTINED, COMPLETE ->
// QUARANTINED_LOST; see machine.go's package doc for why those are the
// only two shapes and why COMPLETE's case is irrecoverable). A successful
// recheck records a same-state pass (Committed -> Committed, and so on),
// which both leaves an audit trail and resets SelectDue's due-ness clock
// for that artifact (see SelectDue's doc for why UpdatedAt is safe to use
// that way here).
//
// # The restore-test hook
//
// config.Revalidation.Command, run through lifecycle.RunRestoreCheck, is
// the stronger form of the same idea Phase 4 asks for: proving an artifact
// still actually restores, not merely that its bytes are unchanged. It is
// independent of the hash tier (config.Revalidation.Hash); a backup set
// can enable either, both, or (once Interval/MaxPerCycle are unset)
// neither.
package revalidate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// Journal is the slice of internal/state that revalidation needs: the two
// methods every lifecycle.Advance call already requires, plus
// ListByBackupSet to enumerate what to consider in the first place. This
// mirrors internal/reconcile.Journal exactly, for the same reason: a test
// can substitute a fake without standing up SQLite, and this package
// cannot reach past this surface into migrations or schema concerns it
// does not own.
type Journal interface {
	lifecycle.Journal
	ListByBackupSet(ctx context.Context, set model.BackupSetID) ([]state.Record, error)
}

// Deps is what Run is handed. Unlike internal/reconcile.Deps and
// lifecycle.Deps, there is deliberately no Transport field here:
// revalidation only ever re-checks the durable local copy already on
// disk, never the remote (which, for a COMPLETE artifact, is already
// confirmed gone; see the package doc), so nothing in this package ever
// needs one.
type Deps struct {
	Journal Journal

	// Now is injectable so a test can control both what a recorded
	// transition's OccurredAt is stamped with and, transitively through
	// UpdatedAt, what a later SelectDue call sees. Nil means time.Now.
	Now func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now().UTC()
}

// lifecycleDeps adapts Deps into the lifecycle.Deps shape Advance needs.
func (d Deps) lifecycleDeps() lifecycle.Deps {
	return lifecycle.Deps{Journal: d.Journal, Now: d.Now}
}

// Finding is one artifact Run examined.
type Finding struct {
	Artifact model.ArtifactID
	From     lifecycle.State
	To       lifecycle.State

	// Checked is false when nothing cfg enables could actually produce a
	// verdict for this specific artifact (see runChecks's doc). When
	// false, Passed is meaningless and no journal write happened at all:
	// From always equals To, and the artifact's due-ness clock (UpdatedAt)
	// is deliberately left untouched, so it stays selectable next cycle
	// rather than looking freshly checked.
	Checked bool
	// Passed is this artifact's combined verdict across every tier cfg
	// enabled. Meaningless when Checked is false.
	Passed bool
	// Reason is a short, human-readable explanation, suitable for a log
	// line or an audit trail.
	Reason string
}

// ArtifactError is a per-artifact problem that stopped Run from reaching a
// verdict for exactly one artifact, without aborting the rest of the pass:
// the same convention internal/reconcile.ArtifactError already
// established.
type ArtifactError struct {
	Artifact model.ArtifactID
	Err      error
}

func (e ArtifactError) Error() string { return fmt.Sprintf("%s: %v", e.Artifact, e.Err) }
func (e ArtifactError) Unwrap() error { return e.Err }

// Report is everything one Run call found and did.
type Report struct {
	Findings []Finding
	Errors   []ArtifactError
}

// Run performs one scheduled-revalidation pass over set: it loads set's
// journal records, selects which are due for a fresh check (SelectDue, at
// most cfg.MaxPerCycle of them), and re-checks each one against cfg's
// configured tiers.
//
// cfg.MaxPerCycle <= 0 means revalidation is disabled for this backup set
// (config.Validate's own rule is that this is the only way MaxPerCycle can
// be non-positive in a validated config; see validateRevalidation): Run
// returns an empty Report and does not even list the journal, so calling
// it unconditionally every cycle for every backup set is always safe and
// cheap when a set has not opted in.
//
// A non-nil error means an infrastructure problem (a missing Journal, a
// bad backup set id, a listing failure, or the context being cancelled
// partway through): a business outcome ("this artifact failed
// revalidation") is reported through the returned Report, never through
// error. A per-artifact problem that is not itself a verdict (see
// checkArtifact) lands in Report.Errors instead of aborting the rest of
// the pass, the same convention internal/reconcile.Reconcile already
// uses.
func Run(ctx context.Context, deps Deps, set model.BackupSetID, cfg config.Revalidation) (Report, error) {
	if deps.Journal == nil {
		return Report{}, fmt.Errorf("revalidate: Deps needs a Journal")
	}
	if set.IsZero() {
		return Report{}, fmt.Errorf("revalidate: needs a backup set id")
	}
	if cfg.MaxPerCycle <= 0 {
		return Report{}, nil
	}

	records, err := deps.Journal.ListByBackupSet(ctx, set)
	if err != nil {
		return Report{}, fmt.Errorf("revalidate: listing %s: %w", set, err)
	}

	due := SelectDue(records, cfg, deps.now())

	var report Report
	for _, rec := range due {
		if err := ctx.Err(); err != nil {
			return report, fmt.Errorf("revalidate: cancelled: %w", err)
		}

		finding, err := checkArtifact(ctx, deps, cfg, rec)
		if err != nil {
			if isCancelled(err) {
				return report, fmt.Errorf("revalidate: cancelled: %w", err)
			}
			report.Errors = append(report.Errors, ArtifactError{Artifact: rec.Artifact, Err: err})
			continue
		}
		report.Findings = append(report.Findings, finding)
	}
	return report, nil
}

// checkArtifact runs runChecks against rec and, only when it actually
// produced a verdict (checked == true), records exactly one journal
// transition: a same-state pass, or the same COMMITTED/REMOTE_DELETE_PENDING
// -> QUARANTINED / COMPLETE -> QUARANTINED_LOST routing reconcile.go
// already uses for "found invalid after the fact".
//
// A non-nil error here means runChecks hit an infrastructure problem for
// this one artifact (see that function's doc): the journal is never
// touched, and the caller (Run) is responsible for deciding whether that
// is a per-artifact error to record and move on from, or a cancellation to
// stop the whole pass for.
func checkArtifact(ctx context.Context, deps Deps, cfg config.Revalidation, rec state.Record) (Finding, error) {
	cur := lifecycle.State(rec.State)

	checked, passed, reason, err := runChecks(ctx, cfg, rec)
	if err != nil {
		return Finding{}, err
	}
	if !checked {
		return Finding{Artifact: rec.Artifact, From: cur, To: cur, Checked: false, Reason: reason}, nil
	}

	if passed {
		if _, err := lifecycle.Advance(ctx, deps.lifecycleDeps(), state.Transition{
			Artifact: rec.Artifact,
			Key:      revalidateKey(rec.Artifact, "pass", cur, cur, rec.UpdatedAt),
			From:     string(cur),
			To:       string(cur),
			Detail:   "Phase 4: scheduled revalidation passed: " + reason,
		}); err != nil {
			return Finding{}, fmt.Errorf("recording a passed revalidation for %s: %w", rec.Artifact, err)
		}
		return Finding{Artifact: rec.Artifact, From: cur, To: cur, Checked: true, Passed: true, Reason: reason}, nil
	}

	// A failed recheck: route through the exact same edges reconcile.go
	// already established for "the durable local copy was found invalid
	// after the fact" (see machine.go and this package's own doc). Only
	// COMPLETE routes to QUARANTINED_LOST; both other eligible states
	// route to the ordinary, recoverable QUARANTINED.
	to := lifecycle.Quarantined
	if cur == lifecycle.Complete {
		to = lifecycle.QuarantinedLost
	}

	out, err := lifecycle.Advance(ctx, deps.lifecycleDeps(), state.Transition{
		Artifact: rec.Artifact,
		Key:      revalidateKey(rec.Artifact, "fail", cur, to, rec.UpdatedAt),
		From:     string(cur),
		To:       string(to),
		Detail:   "Phase 4: scheduled revalidation found the durable local copy invalid: " + reason,
	})
	if err != nil {
		return Finding{}, fmt.Errorf("quarantining %s after a failed revalidation: %w", rec.Artifact, err)
	}
	return Finding{Artifact: rec.Artifact, From: cur, To: lifecycle.State(out.Record.State), Checked: true, Passed: false, Reason: reason}, nil
}

// revalidateKey mirrors internal/reconcile's own reconcileKey exactly: an
// idempotency key derived from the artifact, a tag distinguishing which of
// this package's writes it is for, the edge, and the journal snapshot's
// own UpdatedAt. Deriving it from UpdatedAt, rather than from wall-clock
// time or a counter, is what makes two calls against the same stale
// snapshot (the shape of a crash between reading the record and writing
// the result) idempotent, while a later call against a snapshot whose
// UpdatedAt has actually moved on gets a fresh key, exactly like
// reconcile.go's own reasoning.
func revalidateKey(artifact model.ArtifactID, tag string, from, to lifecycle.State, updatedAt time.Time) string {
	return fmt.Sprintf("revalidate:%s:%s:%s->%s@%s", artifact, tag, from, to, updatedAt.UTC().Format(time.RFC3339Nano))
}

// isCancelled reports whether err represents this call being stopped
// externally rather than a check failing on its own terms. Mirrors
// verify.go's isCancellation for the same reason: retry.Do and this
// package's own context handling both need a way to tell "the caller
// asked us to stop" apart from every other kind of failure, which must
// still fall through to a real business verdict.
func isCancelled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
