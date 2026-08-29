package health

import (
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// knownGood is FR-19's definition of a valid restore point: currently
// COMMITTED, REMOTE_DELETE_PENDING or COMPLETE. FAILED, QUARANTINED,
// QUARANTINED_LOST and every .partial (pre-COMMITTED) state are excluded,
// exactly as FR-19 requires. An artifact that was once COMPLETE and has
// since moved to QUARANTINED_LOST is excluded too, because this checks the
// artifact's *current* state, not its history: its one copy is gone, so it
// cannot be a restore point any more no matter what it used to be.
var knownGood = map[lifecycle.State]bool{
	lifecycle.Committed:           true,
	lifecycle.RemoteDeletePending: true,
	lifecycle.Complete:            true,
}

// evidence is the only input decideState accepts. It is deliberately
// narrow: every field here comes from journal rows and the set's
// stale_after threshold, and nothing here can hold a process-liveness fact,
// a free-space reading, or a version string. That is what makes the
// separation the package doc describes structural: decideState has no
// parameter through which such a fact could arrive, so it cannot influence
// State even by a future bug that forgets to keep them apart.
type evidence struct {
	now            time.Time
	staleThreshold time.Duration

	hasHistory       bool
	newestActivityAt time.Time // max UpdatedAt across every record, any state

	newestGoodAt *time.Time // max UpdatedAt among knownGood records, if any

	// newestArrivalState is the current state of the record with the
	// latest DiscoveredAt: the most recently noticed artifact, regardless
	// of what has happened to it since.
	newestArrivalState lifecycle.State

	hasQuarantinedLost bool // any artifact currently QUARANTINED_LOST
	hasStuckFailure    bool // any FAILED artifact with no retry scheduled
	hasRetryingFailure bool // any FAILED artifact with a retry still scheduled
}

// decideState assigns one of the four FR-24 states from evidence alone.
// Checked in this order:
//
//  1. FAILING: an irrecoverable QUARANTINED_LOST artifact, or a FAILED
//     artifact with no retry scheduled. Checked first and unconditionally,
//     so neither can ever be masked by an otherwise-fresh backup and read
//     as merely DEGRADED.
//  2. If no known-good backup exists inside staleThreshold: DEGRADED when
//     there is no history yet, or when the newest activity of any kind is
//     still recent enough to look like a first backup in progress; STALE
//     otherwise, because the freshness guarantee has broken with nothing
//     to suggest it is about to recover on its own.
//  3. Otherwise a known-good backup exists inside the window: DEGRADED if
//     the newest arrival is quarantined or a failure is still being
//     retried (something arrived, or something is wrong, even though an
//     older restore point is still fine), HEALTHY otherwise.
func decideState(e evidence) (State, string) {
	if e.hasQuarantinedLost {
		return Failing, "an irrecoverable QUARANTINED_LOST artifact exists in this backup set"
	}
	if e.hasStuckFailure {
		return Failing, "a FAILED artifact has no retry scheduled and needs intervention"
	}

	freshnessOK := e.newestGoodAt != nil && e.now.Sub(*e.newestGoodAt) <= e.staleThreshold

	if !freshnessOK {
		switch {
		case !e.hasHistory:
			return Degraded, "no artifact has ever been discovered for this backup set yet"
		case e.now.Sub(e.newestActivityAt) <= e.staleThreshold:
			return Degraded, "no known-good backup yet, but recent activity is still within the stale threshold"
		default:
			return Stale, "no known-good backup within the stale threshold, and no recent activity either"
		}
	}

	if e.newestArrivalState == lifecycle.Quarantined {
		return Degraded, "a known-good backup exists within the stale threshold, but the newest artifact is quarantined"
	}
	if e.hasRetryingFailure {
		return Degraded, "a known-good backup exists within the stale threshold, but a failed attempt is still being retried"
	}
	return Healthy, "a known-good backup exists within the stale threshold and nothing needs attention"
}

// aggregate is everything ComputeBackupSetHealth derives from records
// alone: the evidence decideState needs, plus the display fields (counts,
// timestamps, in-progress transfers) that are shown but never fed back into
// the state decision.
type aggregate struct {
	evidence

	lastCompletedBackupAt *time.Time
	currentTransfers      []TransferInProgress
	pendingDeletes        int
	failures              int
	quarantinedCount      int
	quarantinedLostCount  int
}

// parseRecordState converts a journal row's plain state string to a
// lifecycle.State, per state.Record's contract that lifecycle owns that
// vocabulary. A string the lifecycle package does not recognize (schema
// drift, a hand-edited row) is treated as unknown rather than crashing: it
// still counts toward hasHistory and newestActivityAt, but matches none of
// the specific-state checks below, so it can never be miscounted as
// known-good, quarantined or failed.
func parseRecordState(raw string) lifecycle.State {
	s := lifecycle.State(raw)
	if !s.Valid() {
		return ""
	}
	return s
}

func buildAggregate(records []state.Record, staleThreshold time.Duration, now time.Time) aggregate {
	agg := aggregate{evidence: evidence{now: now, staleThreshold: staleThreshold}}
	if len(records) == 0 {
		return agg
	}
	agg.hasHistory = true

	var newestArrival state.Record
	haveArrival := false

	for _, r := range records {
		if r.UpdatedAt.After(agg.newestActivityAt) {
			agg.newestActivityAt = r.UpdatedAt
		}
		if !haveArrival || r.DiscoveredAt.After(newestArrival.DiscoveredAt) {
			newestArrival = r
			haveArrival = true
		}

		st := parseRecordState(r.State)

		if knownGood[st] {
			updated := r.UpdatedAt
			if agg.newestGoodAt == nil || updated.After(*agg.newestGoodAt) {
				agg.newestGoodAt = &updated
			}
			if st == lifecycle.Complete {
				completed := r.UpdatedAt
				if agg.lastCompletedBackupAt == nil || completed.After(*agg.lastCompletedBackupAt) {
					agg.lastCompletedBackupAt = &completed
				}
			}
		}

		switch st {
		case lifecycle.RemoteDeletePending:
			agg.pendingDeletes++
		case lifecycle.Transferring:
			started := r.UpdatedAt
			agg.currentTransfers = append(agg.currentTransfers, TransferInProgress{
				Artifact:  r.Artifact,
				StartedAt: started,
			})
		case lifecycle.Failed:
			agg.failures++
			if r.NextRetryAt == nil {
				agg.hasStuckFailure = true
			} else {
				agg.hasRetryingFailure = true
			}
		case lifecycle.Quarantined:
			agg.quarantinedCount++
		case lifecycle.QuarantinedLost:
			agg.quarantinedCount++
			agg.quarantinedLostCount++
			agg.hasQuarantinedLost = true
		}
	}

	agg.newestArrivalState = parseRecordState(newestArrival.State)

	return agg
}

func ageOf(at *time.Time, now time.Time) *time.Duration {
	if at == nil {
		return nil
	}
	d := now.Sub(*at)
	return &d
}

// ComputeBackupSetHealth computes the backup-freshness half of FR-24 for
// one backup set from its already-loaded journal rows (typically
// state.Journal.ListByBackupSet), the set's configured stale_after
// threshold, and the injected facts this package cannot derive on its own
// (see BackupSetInputs). now is the caller's clock reading; pass the same
// clock the journal timestamps were written against.
func ComputeBackupSetHealth(set model.BackupSetID, records []state.Record, staleThreshold time.Duration, in BackupSetInputs, now time.Time) BackupSetHealth {
	agg := buildAggregate(records, staleThreshold, now)
	st, reason := decideState(agg.evidence)

	return BackupSetHealth{
		Set:    set,
		State:  st,
		Reason: reason,

		LastSuccessfulPollAt: in.LastSuccessfulPollAt,

		LastCompletedBackupAt: agg.lastCompletedBackupAt,
		NewestGoodBackupAt:    agg.newestGoodAt,
		NewestGoodBackupAge:   ageOf(agg.newestGoodAt, now),
		StaleThreshold:        staleThreshold,

		CurrentTransfers: agg.currentTransfers,
		PendingDeletes:   agg.pendingDeletes,
		Failures:         agg.failures,

		QuarantinedCount:     agg.quarantinedCount,
		QuarantinedLostCount: agg.quarantinedLostCount,

		LastRetentionRunAt: in.LastRetentionRunAt,
		FreeBytes:          in.FreeBytes,
	}
}
