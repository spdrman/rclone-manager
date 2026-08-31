package service

import (
	"context"
	"fmt"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/health"
)

// BackupSetHealth is one configured backup set's FR-24 verdict, in plain
// provider-agnostic terms.
//
// It is the same computation `backup-manager status` prints, read through
// core/service instead of a terminal, so the CLI and the Web UI cannot
// disagree about whether a deployment is healthy.
type BackupSetHealth struct {
	BackupSetID string
	SourceName  string
	SetName     string

	// State is "HEALTHY", "DEGRADED", "STALE" or "FAILING".
	State string
	// Reason is one operator-facing sentence explaining State. Never
	// empty: colour alone is not an explanation.
	Reason string

	// NewestGoodBackupAt is the newest known-good restore point, or the
	// zero time when this set has never produced one.
	NewestGoodBackupAt time.Time
	// LastCompletedBackupAt is the newest artifact that reached COMPLETE,
	// whether or not it is still trustworthy.
	LastCompletedBackupAt time.Time
	// StaleAfter is the freshness window this set is judged against.
	StaleAfter time.Duration

	CurrentTransfers     int
	PendingDeletes       int
	Failures             int
	QuarantinedCount     int
	QuarantinedLostCount int

	// ReinstatedRemoteRetainedCount is how many artifacts in this set were
	// returned to service out of quarantine and still hold a remote source
	// this manager has undertaken never to delete (issue #227). It only
	// ever grows: the forfeiture is permanent by design, and releasing
	// those remote copies is an operator decision made outside this
	// manager.
	//
	// There is no bytes figure beside it, and that absence is deliberate
	// rather than unfinished. See internal/health's own field doc: the
	// only size this manager ever recorded for a remote object is what it
	// measured at discovery, the object may since have been removed or
	// replaced by its producer, and re-reading every one of them on every
	// health call would be a network round trip per artifact against a
	// source that may be unreachable. The honest answer is the count, and
	// that the size is not known.
	ReinstatedRemoteRetainedCount int

	// FreeBytes is a live reading of the local destination's free space,
	// and FreeBytesKnown is false when that reading could not be taken
	// (the path does not exist yet, or the platform refused). A zero with
	// no flag beside it would read as "the disk is full", which is the
	// one thing it must never be mistaken for.
	FreeBytes      uint64
	FreeBytesKnown bool

	// HaltReason is why the manager could not connect to this backup set
	// the last time it tried: "HOST_KEY_CHANGED", "AUTHENTICATION_FAILED",
	// or empty when no refusal is on record (issue #245).
	//
	// Empty means "no refusal has been observed", never "this set is
	// reachable". Those are different claims, and only the first one is
	// ever available to make: a set that has simply never been cycled has
	// nothing on record either. That asymmetry is why this is an optional
	// reason and not a halted boolean, which is the shape issue #231
	// removed after every mapper was forced to fill it with a fabricated
	// false.
	//
	// It sits beside State rather than inside it. A set refused on every
	// cycle still gets its verdict from journal evidence alone and usually
	// reads STALE, which is true and only half the story; this is the
	// other half. Nothing here re-trusts a key, retries a connection or
	// resumes a set: §77 invariant 5 makes that an explicit administrator
	// action, and this is a report of a refusal, not a control over it.
	HaltReason string

	// TotalBytes and StorageLevel come from the same FR-21 capacity
	// assessment ListStorageStatus reports, read here so one call can
	// answer "are my backups healthy" completely rather than leaving a
	// caller to join two endpoints to find out whether the disk they land
	// on is nearly full. StorageLevel is "OK", "WARNING" or "CRITICAL",
	// or "" when no reading could be taken, which is a different thing
	// from OK and must not be collapsed into it.
	TotalBytes   uint64
	StorageLevel string
}

// HealthReport is FR-24's backup-freshness half: every configured backup
// set's verdict, computed at GeneratedAt.
//
// It carries no process or build facts on purpose. Failure-safety
// invariant 14 is that process liveness is not evidence of backup
// freshness, and internal/health enforces that by giving its two halves no
// shared field at all. Restating the version here would put a second copy
// of what GET /system/version already reports behind a name that invites
// exactly the conflation the invariant forbids. A caller that wants one
// summary state has to decide, out loud, how it collapses these.
type HealthReport struct {
	GeneratedAt time.Time
	BackupSets  []BackupSetHealth
}

// Health computes the FR-24 health report for every configured backup set.
//
// It is read-only and takes a fresh capacity reading on every call, so two
// consecutive calls can legitimately differ; nothing is cached, because a
// cached health verdict is exactly the thing that reports green after the
// deployment has stopped backing anything up.
func (b *BackupService) Health(ctx context.Context) (HealthReport, error) {
	st := b.state.Load()

	// An empty VersionInfo, deliberately: BuildHealthReport uses it only
	// for the process half of its report, which HealthReport does not
	// carry (see its own doc). Passing this process's real version would
	// compute a value nothing reads.
	report, err := st.inner.BuildHealthReport(ctx, app.VersionInfo{})
	if err != nil {
		return HealthReport{}, fmt.Errorf("service: computing health: %w", err)
	}

	// The FR-21 capacity assessment, taken once and indexed, rather than
	// per set: ListStorageStatus already walks every configured backup
	// set, and calling it inside the loop below would stat the same
	// destination once per set that shares it.
	//
	// A failure here is not a failure of the health report. Capacity is
	// one input to it, and the freshness verdict, which is the thing this
	// report exists to carry, does not depend on it: an unreadable mount
	// leaves StorageLevel empty and FreeBytesKnown false, which is exactly
	// what those two fields are for.
	capacity := map[string]StorageStatus{}
	if statuses, statusErr := b.ListStorageStatus(ctx); statusErr == nil {
		for _, s := range statuses {
			capacity[s.BackupSetID] = s
		}
	}

	out := HealthReport{
		GeneratedAt: report.GeneratedAt,
		BackupSets:  make([]BackupSetHealth, 0, len(report.BackupSets)),
	}
	for _, bs := range report.BackupSets {
		set := toServiceBackupSetHealth(bs)
		if status, ok := capacity[set.BackupSetID]; ok && status.Available {
			set.TotalBytes = status.TotalBytes
			set.StorageLevel = status.Level
		}
		out.BackupSets = append(out.BackupSets, set)
	}
	return out, nil
}

func toServiceBackupSetHealth(bs health.BackupSetHealth) BackupSetHealth {
	out := BackupSetHealth{
		BackupSetID:          bs.Set.String(),
		SourceName:           bs.Set.Source,
		SetName:              bs.Set.Set,
		State:                string(bs.State),
		Reason:               bs.Reason,
		HaltReason:           bs.HaltReason,
		StaleAfter:           bs.StaleThreshold,
		CurrentTransfers:     len(bs.CurrentTransfers),
		PendingDeletes:       bs.PendingDeletes,
		Failures:             bs.Failures,
		QuarantinedCount:     bs.QuarantinedCount,
		QuarantinedLostCount: bs.QuarantinedLostCount,

		ReinstatedRemoteRetainedCount: bs.ReinstatedRemoteRetainedCount,
	}
	if bs.NewestGoodBackupAt != nil {
		out.NewestGoodBackupAt = *bs.NewestGoodBackupAt
	}
	if bs.LastCompletedBackupAt != nil {
		out.LastCompletedBackupAt = *bs.LastCompletedBackupAt
	}
	if bs.FreeBytes != nil {
		out.FreeBytes = *bs.FreeBytes
		out.FreeBytesKnown = true
	}
	return out
}
