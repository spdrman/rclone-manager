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
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
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

// Mediums resolves a placement's medium id into the descriptor a
// MediumStore needs, out of whatever configuration the caller holds.
//
// It is an interface rather than a map because this package must not
// import internal/config: config is where medium truth lives, and a
// package that read it directly would be a second place deciding what a
// medium is. A caller that has no mediums configured supplies nothing,
// and this package then has nothing to check a medium placement with,
// which it reports honestly rather than papering over.
type Mediums interface {
	MediumFor(id string) (transport.Medium, bool)
}

// Deps is what Run is handed.
//
// There is still no Transport here, and the reason has not changed:
// revalidation never re-checks a remote SOURCE, which for a COMPLETE
// artifact is already confirmed gone. What EPIC E adds is the other
// direction, a DESTINATION an artifact's durable copy may now live on, and
// that arrives as a MediumStore plus a way to resolve a medium id.
//
// Both are optional, and both being absent is the ordinary case for every
// deployment that configures no medium: with no store, a medium placement
// is reported as not checked rather than as passed, which is exactly the
// checked-versus-passed distinction this package already draws.
type Deps struct {
	// Journal is both the source of what to consider and the destination
	// for what is found. It is the wider interface rather than
	// lifecycle.Journal because this package has to enumerate a backup set
	// before it can decide anything, which lifecycle never needs to do.
	Journal Journal

	// Store reaches storage mediums. Nil means this deployment cannot
	// reach one, which is true of every deployment that configures none.
	Store placement.Store

	// Mediums resolves a placement's medium id. Nil has the same effect
	// as a nil Store.
	Mediums Mediums

	// Now is injectable so a test can control both what a recorded
	// transition's OccurredAt is stamped with and, transitively through
	// UpdatedAt, what a later SelectDue call sees. Nil means time.Now.
	Now func() time.Time
}

// now resolves the clock once per pass, in UTC whichever branch it takes.
//
// Run calls this once and hands the instant to SelectDue, rather than
// letting each artifact ask again. That matters more here than it looks:
// due-ness is a comparison against UpdatedAt, so a clock read per artifact
// would make the interval boundary land in a different place for the first
// record in a batch than for the last, and an artifact sitting exactly on
// the boundary would be selected or not depending on how long the pass had
// been running.
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
	// Artifact is which one was examined.
	Artifact model.ArtifactID

	// From and To are the states either side of what this pass recorded.
	// They are equal for a pass and for a not-checked artifact alike, so
	// they are not on their own a way to tell those two apart: Checked is.
	// To is read back out of the journal's own answer rather than assumed
	// from the requested edge, so a Finding reports where the artifact
	// actually ended up.
	From lifecycle.State
	To   lifecycle.State

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

	// Class is the verification class this pass actually ACHIEVED, never
	// the strongest one configured (EPIC E, FR-31). It is empty when
	// Checked is false.
	//
	// It exists because "revalidated" stopped being one thing. An
	// artifact whose durable copy is a local file is re-read and
	// re-hashed, which is placement.Content. An artifact whose durable
	// copy is on a storage medium is HEADed, which is
	// placement.Existence, proves nothing about the bytes, and must never
	// be reported to an operator as the artifact having been revalidated
	// in the sense the local check means. Carrying the class is what lets
	// every surface downstream say which one happened.
	Class placement.Class
}

// ArtifactError is a per-artifact problem that stopped Run from reaching a
// verdict for exactly one artifact, without aborting the rest of the pass:
// the same convention internal/reconcile.ArtifactError already
// established.
type ArtifactError struct {
	// Artifact names which one could not be given a verdict. Without it a
	// batch of these is unactionable.
	Artifact model.ArtifactID

	// Err is the infrastructure problem, never a verdict. A check that ran
	// and said "this artifact is corrupt" is a Finding with Passed false;
	// only a check that could not run at all lands here.
	Err error
}

// Error renders as "artifact: reason".
func (e ArtifactError) Error() string { return fmt.Sprintf("%s: %v", e.Artifact, e.Err) }

// Unwrap keeps the cause reachable through errors.Is. Run uses that itself,
// through isCancelled, to tell a pass that was stopped from a pass where one
// artifact went wrong, and a caller reading a Report needs the same ability
// for the same reason.
func (e ArtifactError) Unwrap() error { return e.Err }

// Report is everything one Run call found and did.
type Report struct {
	// Findings is one entry per artifact that reached a conclusion,
	// including the not-checked ones. A not-checked artifact belongs here
	// rather than in Errors because nothing went wrong: the configuration
	// simply enabled nothing that could produce a verdict for it, and
	// reporting that as an error every cycle would train an operator to
	// ignore the list.
	Findings []Finding

	// Errors is one entry per artifact that could not be given a verdict
	// at all. An empty Errors with a short Findings list is the shape of a
	// pass bounded by MaxPerCycle, not the shape of a pass that failed.
	Errors []ArtifactError
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

	checked, passed, class, reason, err := runChecks(ctx, deps, cfg, rec)
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
			// The class is named in the audit trail, not just in the
			// returned Finding, because the journal is what an operator
			// reads six months later when they want to know when this
			// artifact was last actually looked at. "Revalidation passed"
			// on its own would let an existence check masquerade there as
			// the content check the same words used to mean.
			Detail: fmt.Sprintf("Phase 4: scheduled revalidation passed at %s class: %s", class, reason),
		}); err != nil {
			return Finding{}, fmt.Errorf("recording a passed revalidation for %s: %w", rec.Artifact, err)
		}
		return Finding{Artifact: rec.Artifact, From: cur, To: cur, Checked: true, Passed: true, Reason: reason, Class: class}, nil
	}

	// A failed recheck: route through the exact same edges reconcile.go
	// already established for "the durable local copy was found invalid
	// after the fact" (see machine.go and this package's own doc). Only
	// COMPLETE routes to QUARANTINED_LOST; every other eligible state,
	// REMOTE_RETAINED (issue #315) included, routes to the ordinary,
	// recoverable QUARANTINED: unlike COMPLETE, none of them has confirmed
	// the remote object gone, so the remote is presumptively still there
	// to recover from.
	to := lifecycle.Quarantined
	if cur == lifecycle.Complete {
		to = lifecycle.QuarantinedLost
	}

	out, err := lifecycle.Advance(ctx, deps.lifecycleDeps(), state.Transition{
		Artifact: rec.Artifact,
		Key:      revalidateKey(rec.Artifact, "fail", cur, to, rec.UpdatedAt),
		From:     string(cur),
		To:       string(to),
		Detail:   fmt.Sprintf("Phase 4: scheduled revalidation at %s class found the artifact's durable copy invalid: %s", class, reason),
	})
	if err != nil {
		return Finding{}, fmt.Errorf("quarantining %s after a failed revalidation: %w", rec.Artifact, err)
	}
	return Finding{Artifact: rec.Artifact, From: cur, To: lifecycle.State(out.Record.State), Checked: true, Passed: false, Reason: reason, Class: class}, nil
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
//
// It mirrors that function's ordering too, and for the reason spelled out
// there: a transport.Error keeps its cause reachable through Unwrap, so an
// error already classified as anything other than Cancelled can still
// answer errors.Is(err, context.DeadlineExceeded), and a connect timeout
// rclone imposed on itself is that exact shape (issue #388). Nothing on
// this package's current call graph hands it one, because runChecks
// reaches the local filesystem and the restore-test hook rather than a
// Transport, but the two predicates disagreeing is not a difference worth
// leaving here for the first remote check to discover.
func isCancelled(err error) bool {
	if err == nil {
		return false
	}
	if category, ok := transport.CategoryOf(err); ok {
		return category == transport.Cancelled
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
