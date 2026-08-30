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
// classification internal/transport/rclone's Classify already assigns to
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
