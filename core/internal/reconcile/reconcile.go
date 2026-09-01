package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Reconcile runs FR-17's startup reconciliation pass for one backup set:
// it lists every artifact deps.Journal already holds for set, compares
// each one's journal state against local files and remote state, and
// brings the journal back in line wherever the two disagree. See the
// package doc for the full table this implements and how it handles the
// gap the table left in it.
//
// I took source and set as explicit parameters, mirroring
// internal/discovery.Discover's own (source, set) shape, rather than
// folding them into Deps: a future caller that reconciles several backup
// sets is expected to loop over them and call this once per set, exactly
// as it already will for Discover.
//
// A non-nil error here means something systemic: Deps is incomplete, set
// is the zero value, or listing the journal itself failed. A problem with
// one specific artifact goes into the returned Report's Errors instead
// (ArtifactError), so one bad artifact never hides every other one's
// result, the same convention Discover's own Result already uses.
func Reconcile(ctx context.Context, deps Deps, source transport.Source, set model.BackupSetID) (Report, error) {
	if deps.Journal == nil {
		return Report{}, fmt.Errorf("reconcile: Deps needs a Journal")
	}
	if deps.Transport == nil {
		return Report{}, fmt.Errorf("reconcile: Deps needs a Transport")
	}
	if set.IsZero() {
		return Report{}, fmt.Errorf("reconcile: needs a backup set id")
	}

	records, err := deps.Journal.ListByBackupSet(ctx, set)
	if err != nil {
		return Report{}, fmt.Errorf("reconcile: listing %s: %w", set, err)
	}

	var report Report
	for _, rec := range records {
		finding, err := reconcileOne(ctx, deps, source, rec)
		if err != nil {
			report.Errors = append(report.Errors, ArtifactError{Artifact: rec.Artifact, Err: err})
			continue
		}
		report.Findings = append(report.Findings, finding)
	}
	return report, nil
}

// reconcileOne dispatches one journal row to the row of the FR-17 table
// its current state belongs to.
func reconcileOne(ctx context.Context, deps Deps, source transport.Source, rec state.Record) (Finding, error) {
	st, err := lifecycle.ParseState(rec.State)
	if err != nil {
		return Finding{}, fmt.Errorf("reconcile: %w", err)
	}

	switch st {
	case lifecycle.Discovered:
		// Row: exists, absent, DISCOVERED -> transfer. DISCOVERED with no
		// local copy yet is already exactly what the FR-11 Transfer step
		// expects to find; there is nothing durable for reconciliation to
		// fix, so normal processing (which FR-17 explicitly runs after
		// this pass, not before) drives it forward on its own schedule.
		return noAction(rec.Artifact, st, "discovered with no local copy yet; ready for transfer"), nil

	case lifecycle.Transferring, lifecycle.Transferred, lifecycle.Verifying, lifecycle.Verified, lifecycle.Committing:
		// Row: exists, partial, TRANSFERRING -> safe retry/restart,
		// generalised to every state before COMMITTED: all five still
		// point at a .partial file, never a final one, and FR-12 already
		// treats a .partial as disposable (transfer.go clears any stale
		// one before starting a fresh copy). None of them has a final
		// local copy for this package to validate or quarantine, so I
		// take no action and let normal processing resume from wherever
		// the journal says.
		return noAction(rec.Artifact, st, fmt.Sprintf("%s: no durable local final copy exists yet; safe to retry or restart from here", st)), nil

	case lifecycle.Committed:
		return reconcileCommitted(ctx, deps, rec)

	case lifecycle.RemoteDeletePending:
		return reconcileDeletePending(ctx, deps, source, rec)

	case lifecycle.Complete:
		return reconcileComplete(ctx, deps, rec)

	case lifecycle.RemoteRetained:
		return reconcileRemoteRetained(ctx, deps, rec)

	case lifecycle.Failed, lifecycle.Quarantined, lifecycle.QuarantinedLost:
		// These are already exceptional or terminal outcomes that some
		// other, deliberate mechanism owns: FR-22's retry policy for
		// FAILED, an operator's own decision for QUARANTINED and
		// QUARANTINED_LOST alike. Reconciliation reports them, it does not
		// second-guess them.
		return noAction(rec.Artifact, st, fmt.Sprintf("%s: exceptional state outside reconciliation's scope", st)), nil

	default:
		return Finding{}, fmt.Errorf("reconcile: state %s has no reconciliation handling", st)
	}
}

// noAction builds the Finding for a row where I decided nothing needs to
// change.
func noAction(artifact model.ArtifactID, st lifecycle.State, reason string) Finding {
	return Finding{Artifact: artifact, From: st, To: st, Reason: reason}
}

// reconcileRemoteRetained handles the REMOTE_RETAINED row (issue #315):
// re-check the durable local copy the same way reconcileCommitted does for
// COMMITTED, and quarantine it if it has gone bad.
//
// I never call Stat here either, for the same reason reconcileCommitted
// doesn't: this manager was never going to touch this artifact's remote
// object either way (that is the entire point of issue #282's read-only
// path), so no answer a remote check could give changes anything about how
// a corrupted local copy is handled. machine.go's REMOTE_RETAINED ->
// QUARANTINED edge routes to the recoverable QUARANTINED, never to
// QUARANTINED_LOST: unlike COMPLETE, REMOTE_RETAINED never confirmed the
// remote object gone, so the remote is presumptively still there for an
// operator to recover from (ReleaseFromQuarantine's ordinary re-fetch) or
// to reinstate past (ReinstateFromQuarantine, which now resolves back to
// REMOTE_RETAINED specifically for this lineage, never to COMMITTED; see
// quarantine.go's quarantineOrigins).
func reconcileRemoteRetained(ctx context.Context, deps Deps, rec state.Record) (Finding, error) {
	local := checkLocalFinal(rec)
	if local.Valid {
		return noAction(rec.Artifact, lifecycle.RemoteRetained,
			"local final copy verified valid; the remote source remains retained by policy and was never examined"), nil
	}

	out, err := lifecycle.Advance(ctx, deps.lifecycleDeps(), state.Transition{
		Artifact: rec.Artifact,
		Key:      reconcileKey(rec.Artifact, lifecycle.RemoteRetained, lifecycle.Quarantined, rec.UpdatedAt),
		From:     string(lifecycle.RemoteRetained),
		To:       string(lifecycle.Quarantined),
		Detail:   "FR-17 / issue #315: reconciliation found the durable local copy of a retained (read-only-source) artifact invalid: " + local.Reason,
	})
	if err != nil {
		return Finding{}, fmt.Errorf("quarantining %s: %w", rec.Artifact, err)
	}
	return Finding{
		Artifact: rec.Artifact,
		From:     lifecycle.RemoteRetained,
		To:       lifecycle.State(out.Record.State),
		Reason:   "local final copy is invalid (" + local.Reason + "); the remote copy is retained by policy and was never examined, quarantining the local copy so an operator can decide whether to re-fetch or reinstate it",
	}, nil
}

// reconcileCommitted handles the COMMITTED row of the table (verify and
// proceed toward delete) and COMMITTED's half of the invalid-local row.
//
// I never call Stat here. machine.go has no COMMITTED -> QUARANTINED_LOST
// edge, only COMMITTED -> QUARANTINED, so no answer a remote check could
// give changes which state I am allowed to move an invalid COMMITTED
// artifact to; and when the local copy is valid, row 3 ("verify and
// proceed toward delete") only asks me to confirm that, not to touch the
// remote, which stays whatever normal processing's own DeleteRemote step
// finds when it eventually runs.
func reconcileCommitted(ctx context.Context, deps Deps, rec state.Record) (Finding, error) {
	local := checkLocalFinal(rec)
	if local.Valid {
		return noAction(rec.Artifact, lifecycle.Committed,
			"local final copy verified valid; remote still untouched, proceeding toward eventual delete"), nil
	}

	out, err := lifecycle.Advance(ctx, deps.lifecycleDeps(), state.Transition{
		Artifact: rec.Artifact,
		Key:      reconcileKey(rec.Artifact, lifecycle.Committed, lifecycle.Quarantined, rec.UpdatedAt),
		From:     string(lifecycle.Committed),
		To:       string(lifecycle.Quarantined),
		Detail:   "FR-17: reconciliation found the durable local copy invalid: " + local.Reason,
	})
	if err != nil {
		return Finding{}, fmt.Errorf("quarantining %s: %w", rec.Artifact, err)
	}
	return Finding{
		Artifact: rec.Artifact,
		From:     lifecycle.Committed,
		To:       lifecycle.State(out.Record.State),
		Reason:   "local final copy is invalid (" + local.Reason + "); preserving the remote and quarantining the local copy for a fresh attempt",
	}, nil
}

// reconcileDeletePending handles every row that starts from
// REMOTE_DELETE_PENDING: the plain "absent, final -> reconcile COMPLETE"
// row, REMOTE_DELETE_PENDING's half of the invalid-local row, the gap row
// this package adds ("absent, invalid final"), and the "changed identity"
// row.
func reconcileDeletePending(ctx context.Context, deps Deps, source transport.Source, rec state.Record) (Finding, error) {
	local := checkLocalFinal(rec)

	art, remoteExists, statErr := statRemote(ctx, deps.Transport, source, rec.RemotePath)
	if statErr != nil {
		return Finding{}, fmt.Errorf("checking the remote object for %s: %w", rec.Artifact, statErr)
	}

	if !local.Valid {
		if !remoteExists {
			return reconcileToLost(ctx, deps, rec, local.Reason)
		}

		out, err := lifecycle.Advance(ctx, deps.lifecycleDeps(), state.Transition{
			Artifact: rec.Artifact,
			Key:      reconcileKey(rec.Artifact, lifecycle.RemoteDeletePending, lifecycle.Quarantined, rec.UpdatedAt),
			From:     string(lifecycle.RemoteDeletePending),
			To:       string(lifecycle.Quarantined),
			Detail:   "FR-17: reconciliation found the durable local copy invalid while the remote object is still present: " + local.Reason,
		})
		if err != nil {
			return Finding{}, fmt.Errorf("quarantining %s: %w", rec.Artifact, err)
		}
		return Finding{
			Artifact: rec.Artifact,
			From:     lifecycle.RemoteDeletePending,
			To:       lifecycle.State(out.Record.State),
			Reason:   "local final copy is invalid (" + local.Reason + "); the remote object is preserved, quarantining the local copy for a fresh attempt",
		}, nil
	}

	// The local copy is valid from here on.
	if !remoteExists {
		completed, err := reconcileToComplete(ctx, deps, rec)
		if err != nil {
			return Finding{}, fmt.Errorf("reconciling %s to COMPLETE: %w", rec.Artifact, err)
		}
		return Finding{
			Artifact: rec.Artifact,
			From:     lifecycle.RemoteDeletePending,
			To:       lifecycle.State(completed.State),
			Reason:   "remote object confirmed already absent; reconciled to COMPLETE without re-attempting the delete",
		}, nil
	}

	discovered, err := discoveredIdentity(rec)
	if err != nil {
		return Finding{}, fmt.Errorf("comparing remote identity for %s: %w", rec.Artifact, err)
	}
	comparison := model.CompareIdentity(discovered, currentIdentity(art))
	if comparison.Verdict == model.VerdictChanged {
		return Finding{
			Artifact:           rec.Artifact,
			From:               lifecycle.RemoteDeletePending,
			To:                 lifecycle.RemoteDeletePending,
			NeedsInvestigation: true,
			Reason:             "remote object identity changed since discovery (" + comparison.Reason + "); refusing to let the pending delete proceed, this needs investigation",
		}, nil
	}

	return noAction(rec.Artifact, lifecycle.RemoteDeletePending,
		"remote object still present ("+comparison.Reason+"); leaving the pending delete for normal processing to retry"), nil
}

// reconcileComplete handles the COMPLETE row (no-op) and COMPLETE's half
// of the gap row this package adds (irrecoverable loss).
//
// I never call Stat here either. COMPLETE's own contract is that the
// remote is already confirmed gone, and machine.go's only outgoing edge
// from COMPLETE, to QUARANTINED_LOST, depends solely on the local copy's
// validity.
func reconcileComplete(ctx context.Context, deps Deps, rec state.Record) (Finding, error) {
	local := checkLocalFinal(rec)
	if local.Valid {
		return noAction(rec.Artifact, lifecycle.Complete,
			"remote already confirmed gone and the local copy verified valid; nothing to reconcile"), nil
	}

	out, err := lifecycle.Advance(ctx, deps.lifecycleDeps(), state.Transition{
		Artifact: rec.Artifact,
		Key:      reconcileKey(rec.Artifact, lifecycle.Complete, lifecycle.QuarantinedLost, rec.UpdatedAt),
		From:     string(lifecycle.Complete),
		To:       string(lifecycle.QuarantinedLost),
		Detail:   "FR-17: the remote source is already confirmed gone and the durable local copy is now invalid: " + local.Reason,
	})
	if err != nil {
		return Finding{}, fmt.Errorf("quarantining the lost copy of %s: %w", rec.Artifact, err)
	}
	return Finding{
		Artifact: rec.Artifact,
		From:     lifecycle.Complete,
		To:       lifecycle.State(out.Record.State),
		Reason:   "durable local copy found invalid (" + local.Reason + ") after the remote source was already confirmed gone; no copy remains anywhere, recorded as an irrecoverable loss",
	}, nil
}

// reconcileToComplete records REMOTE_DELETE_PENDING -> COMPLETE for rec,
// the same write the plain "absent, final" row makes on its own. Both
// reconcileDeletePending call sites that need this (the plain row and the
// gap row's first half) share it so the Detail and Deletion bookkeeping
// can never drift between them.
func reconcileToComplete(ctx context.Context, deps Deps, rec state.Record) (state.Record, error) {
	now := deps.now()
	out, err := lifecycle.Advance(ctx, deps.lifecycleDeps(), state.Transition{
		Artifact: rec.Artifact,
		Key:      reconcileKey(rec.Artifact, lifecycle.RemoteDeletePending, lifecycle.Complete, rec.UpdatedAt),
		From:     string(lifecycle.RemoteDeletePending),
		To:       string(lifecycle.Complete),
		Detail:   "FR-17: reconciliation confirmed the remote object was already absent",
		Deletion: &state.DeletionUpdate{DeletedAt: &now},
	})
	if err != nil {
		return state.Record{}, err
	}
	return out.Record, nil
}

// reconcileToLost is the gap row's REMOTE_DELETE_PENDING path: reconcile to
// COMPLETE first (reconcileToComplete), then straight on to
// QUARANTINED_LOST, the only edge machine.go admits out of COMPLETE. Doing
// this as two separate lifecycle.Advance calls, rather than trying to
// invent a direct edge, means Validate itself would refuse the shortcut if
// I ever got the order wrong: there is no REMOTE_DELETE_PENDING ->
// QUARANTINED_LOST edge in the table, on purpose.
func reconcileToLost(ctx context.Context, deps Deps, rec state.Record, reason string) (Finding, error) {
	completed, err := reconcileToComplete(ctx, deps, rec)
	if err != nil {
		return Finding{}, fmt.Errorf("completing %s before quarantining its lost copy: %w", rec.Artifact, err)
	}

	out, err := lifecycle.Advance(ctx, deps.lifecycleDeps(), state.Transition{
		Artifact: rec.Artifact,
		Key:      reconcileKey(rec.Artifact, lifecycle.Complete, lifecycle.QuarantinedLost, completed.UpdatedAt),
		From:     string(lifecycle.Complete),
		To:       string(lifecycle.QuarantinedLost),
		Detail:   "FR-17: the remote source is confirmed gone and the durable local copy is invalid: " + reason,
	})
	if err != nil {
		return Finding{}, fmt.Errorf("quarantining the lost copy of %s: %w", rec.Artifact, err)
	}
	return Finding{
		Artifact: rec.Artifact,
		From:     lifecycle.RemoteDeletePending,
		To:       lifecycle.State(out.Record.State),
		Reason:   "remote source confirmed absent and the local copy is invalid (" + reason + "); no copy remains anywhere, recorded as an irrecoverable loss",
	}, nil
}

// reconcileKey derives a deterministic idempotency key for one
// reconciliation-driven transition. I folded in rec's own UpdatedAt
// (already the journal's own timestamp, RFC3339Nano) rather than a counter
// this package invents: UpdatedAt strictly advances every time
// RecordTransition writes this row, so a Reconcile call that lands before
// anything else has touched the row reproduces the exact same key, and the
// journal's own idempotency-key replay (internal/state/journal.go)
// recognises it and changes nothing, while a genuinely later occurrence of
// the same edge for the same artifact, after a full cycle back around the
// lifecycle, computes a fresh key, because the row was necessarily
// rewritten, and UpdatedAt necessarily advanced, at every step in between.
func reconcileKey(artifact model.ArtifactID, from, to lifecycle.State, updatedAt time.Time) string {
	return fmt.Sprintf("reconcile:%s:%s->%s@%s", artifact, from, to, updatedAt.UTC().Format(time.RFC3339Nano))
}
