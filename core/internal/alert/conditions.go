package alert

import (
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/health"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is the whole of this package's translation layer: three pure
// functions turning a verdict some other package already reached into the
// Condition vocabulary Dispatcher de-duplicates on. None of them decides
// anything. If one of them ever needs an `if` about thresholds, freshness
// or trust, that logic belongs in the package that owns the fact (see
// this package's doc).
//
// # Why reinstated-remote retention is NOT a fifth condition (issue #227)
//
// health.BackupSetHealth now carries ReinstatedRemoteRetainedCount: how
// many artifacts were re-trusted out of quarantine and therefore hold a
// remote source internal/lifecycle's FR-15 gate will refuse to delete for
// as long as the deployment lives. It has the shape of something alertable
// (it is storage pressure, and CriticalStoragePressure already exists),
// and it is deliberately not one. Four reasons, in the order they decide
// it:
//
//  1. An alert asks a question this manager cannot answer. The pressure is
//     on the SOURCE machine's disk, and nothing here measures that:
//     internal/capacity assesses the backup set's local DESTINATION,
//     against thresholds an operator set for that filesystem. Any
//     threshold on the count alone would be a number of artifacts standing
//     in for an amount of storage nobody has measured, on a volume of
//     unknown size. That is a threshold with no reasoning behind it, which
//     is worse than no alert.
//
//  2. It can never resolve, so this package's model does not fit it.
//     Dispatcher's whole contract is that a condition fires once, stays
//     silent while it remains true, and is forgotten when it stops being
//     observed so a genuine recurrence alerts again. The forfeiture is
//     permanent by design and the count is monotone, so such a condition
//     would fire exactly once, per backup set, for the life of the
//     process, and then be silent forever. That is no better than the
//     sentence the operator already reads at the moment they reinstate,
//     which is precisely the gap issue #227 opened against.
//
//  3. It fires at the wrong moment. The operator learns nothing at
//     reinstatement time that the reinstatement response did not already
//     tell them; what they lack is the answer a month later, and an
//     interruption is not how you serve a question asked later. FR-24's
//     report, the `status` command and the Prometheus gauge all are: a
//     scrape in particular is the one surface that shows the SLOPE, which
//     is the actual signal here ("this population is growing and nobody is
//     watching") rather than "it is non-zero".
//
//  4. §71 says do not add a broad notification framework in v1, and
//     mechanism_test.go pins the vocabulary to exactly four conditions so
//     a fifth takes a deliberate edit. This is the kind of signal that
//     edit exists to stop: real, worth surfacing, and not an incident.
//
// If a later change gives this manager a genuine reading of source-side
// capacity, the threshold argument in (1) changes and this is worth
// revisiting. Until then the count belongs in the report, not in a
// notification.

// BackupSetConditions returns the conditions h implies. h comes straight
// from internal/health.ComputeBackupSetHealth, so the four FR-24 states
// are decided there, by decideState, and only read here.
//
// Two of §71's conditions come from this one report:
//
//   - StaleBackup, exactly when State is health.Stale. Degraded is
//     deliberately quiet: FR-24 uses it for a set that has never produced
//     an artifact yet, a first backup still in flight, or a fresh
//     known-good backup next to something that needs a look. None of
//     those has broken the freshness guarantee, and alerting on all of
//     them would make the stale alert meaningless.
//
//   - RepeatedFailure, when the set has accumulated at least
//     repeatedFailureThreshold artifacts currently in FAILED, or when
//     State is health.Failing. The second arm matters: Failing is FR-24's
//     "a human is needed right now" state (an irrecoverable
//     QUARANTINED_LOST artifact, or a FAILED artifact with no retry
//     scheduled), and gating it behind a count would mean the single most
//     severe failure state is the one that never alerts. Both arms are
//     the same condition about the same backup set, so Dispatcher's
//     de-duplication still yields exactly one alert either way.
//
// A repeatedFailureThreshold of zero or less disables the count arm
// entirely rather than firing on the first failure. That is the fail-safe
// direction for a value nobody configured: internal/config always
// resolves this to a positive number (see its Alerts block), so a
// non-positive value here means a caller built a Service by hand and
// never set it, and turning that into an alert on every single failed
// artifact would be worse than staying quiet. The Failing arm is
// unaffected, so the state that genuinely needs a human still alerts.
func BackupSetConditions(h health.BackupSetHealth, repeatedFailureThreshold int) []Condition {
	scope := h.Set.String()
	var out []Condition

	if h.State == health.Stale {
		out = append(out, Condition{
			Kind:  StaleBackup,
			Scope: scope,
			Detail: fmt.Sprintf("Backup set %s is STALE: %s (stale threshold %s).",
				scope, h.Reason, h.StaleThreshold),
		})
	}

	switch {
	case h.State == health.Failing:
		out = append(out, Condition{
			Kind:  RepeatedFailure,
			Scope: scope,
			Detail: fmt.Sprintf("Backup set %s is FAILING and needs attention: %s.",
				scope, h.Reason),
		})
	case repeatedFailureThreshold > 0 && h.Failures >= repeatedFailureThreshold:
		out = append(out, Condition{
			Kind:  RepeatedFailure,
			Scope: scope,
			Detail: fmt.Sprintf("Backup set %s has %d failed artifacts, at or above the alert threshold of %d.",
				scope, h.Failures, repeatedFailureThreshold),
		})
	}

	return out
}

// StorageConditions returns §71's critical-storage-pressure condition for
// scope, exactly when a.Level is internal/capacity's Critical. The level
// is decided by internal/capacity.Assess against the operator's own
// thresholds; nothing here re-derives it, and Warning is not an alert
// (see the CriticalStoragePressure constant's own doc).
//
// This is a notification and nothing more. §71 is explicit that a
// critical-storage alert must never become an automatic call into B3.1's
// retention-apply path, and internal/capacity's own package doc makes the
// same point from the other side: a full filesystem is something to
// report loudly, never a licence to go looking for something to delete.
func StorageConditions(scope string, a capacity.Assessment) []Condition {
	if a.Level != capacity.Critical {
		return nil
	}
	return []Condition{{
		Kind:  CriticalStoragePressure,
		Scope: scope,
		Detail: fmt.Sprintf("Storage for backup set %s is at its CRITICAL level: %d bytes available against a critical floor of %d. No data has been deleted; freeing space is an administrator decision.",
			scope, a.Stat.AvailableBytes, a.Thresholds.CriticalFreeBytes),
	}}
}

// HostKeyConditions returns §71's changed-host-key condition for scope,
// exactly when category is internal/transport's HostVerification: the
// classification internal/transport/rclone's ClassifyCtx already assigns to
// a golang.org/x/crypto/ssh/knownhosts key mismatch, which is the same
// failure that made the connection refuse in the first place.
//
// The alert is additive. §77 invariant #5 requires a changed SSH host key
// to stay an explicit administrator action, so this never re-trusts a
// key, never retries the connection, and never suppresses the refusal it
// is reporting on. The message says so, because an operator reading a
// notification needs to know the manager already stopped rather than
// assume it worked around the problem.
func HostKeyConditions(scope string, category transport.Category) []Condition {
	if category != transport.HostVerification {
		return nil
	}
	return []Condition{{
		Kind:  HostKeyChanged,
		Scope: scope,
		Detail: fmt.Sprintf("The SSH host key for backup set %s no longer matches its known_hosts entry. The connection was refused and no backup ran. Verify the new key out of band and update known_hosts yourself; the manager will not trust it on its own.",
			scope),
	}}
}
