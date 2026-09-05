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

	// ReadOnlyRetainedCount is how many artifacts in this set currently
	// hold a remote source this manager will never delete because the
	// set itself is declared read-only (config.BackupSet.ReadOnly, issue
	// #282), not because any one of them was individually reinstated.
	// Unlike ReinstatedRemoteRetainedCount above, this can go back down:
	// turning a set's read-only declaration off does not retroactively
	// authorise deleting what it already retained (see
	// SetBackupSetReadOnly's own doc), but an artifact discovered after
	// that toggle is no longer routed to REMOTE_RETAINED at all, so the
	// count this set reports naturally stops growing and, over the
	// journal's retention window, falls as older REMOTE_RETAINED rows are
	// superseded.
	//
	// There is no bytes figure beside it, for the identical reason
	// ReinstatedRemoteRetainedCount has none: see that field's own doc.
	ReadOnlyRetainedCount int

	// FreeBytes is a live reading of the local destination's free space,
	// and FreeBytesKnown is false when that reading could not be taken
	// (the path does not exist yet, or the platform refused). A zero with
	// no flag beside it would read as "the disk is full", which is the
	// one thing it must never be mistaken for.
	FreeBytes      uint64
	FreeBytesKnown bool

	// HaltReason is why the manager could not connect to this backup set
	// the last time it tried: "HOST_KEY_CHANGED", "AUTHENTICATION_FAILED",
	// "KEY_PERMISSIONS" (issue #293), or empty when no refusal is on
	// record (issue #245).
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

	// Placement is FR-24's medium half (issue #444): how far this set's
	// artifacts are from the storage mediums its retention chain names,
	// and whether the relocations meant to close that gap are getting
	// anywhere.
	//
	// It is the same computation `backup-manager status` prints, reached
	// through the same call, which is the property this whole type exists
	// to hold: a CLI and a Web UI that compute health separately will
	// eventually disagree about whether a deployment is healthy, and the
	// one that disagrees quietly is the one nobody is looking at.
	Placement PlacementHealth

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

// PlacementHealth is where one backup set's artifacts actually are, as
// opposed to how fresh they are (issue #444).
//
// All of it comes from durable state: the placements journal joined
// against the set's resolved retention chain, and the move journal. None
// of it is a statement about the last cycle. That distinction is the
// whole reason this exists: a move's outcome was already visible on the
// exit status, in the activity feed, in the event stream and on the
// dashboard's last-run panel, and every one of those describes ONE PASS,
// so an operator opening a status page on a deployment nobody has run a
// cycle in front of saw none of it.
//
// The two ages here are plain durations rather than optionals, and they
// are meaningful exactly when the count beside them is non-zero:
// OldestAwayFromHomeAge with AwayFromHome, OldestFailedMoveAge with
// FailedMoves. A zero age next to a zero count is not a reading, and
// there is nothing for it to be a reading of.
type PlacementHealth struct {
	// AwayFromHome is how many of this set's artifacts have a durable
	// copy somewhere other than where the chain says. On its own this is
	// not a complaint: a retention pass that has just decided an artifact
	// belongs offsite puts it here, and the move runs in the same cycle.
	AwayFromHome int

	// OldestAwayFromHomeAge is how long the oldest of those copies has
	// existed on the medium it is sitting on. Read it for exactly that:
	// it is an upper bound on how long the artifact has been in the wrong
	// place, because nothing durable records when a chain last changed an
	// artifact's home. OldestFailedMoveAge is the number measured from a
	// moment this manager actually wrote down.
	OldestAwayFromHomeAge time.Duration

	// UnconfirmedLocation is how many artifacts this pass could not place
	// at all: no durable copy recorded yet, or a move mid-flight leaving
	// two answers to "where is this". They are not thereby at home, and
	// reporting them as such is the collapse the away-from-home count
	// exists to end.
	UnconfirmedLocation int

	// OpenMoves is how many relocations the move journal has not
	// finished, in any non-terminal phase.
	OpenMoves int

	// OldestOpenMoveAge is how long the oldest of those has been open,
	// whether or not anything has been recorded against it. It changes no
	// State, because "open for a while" has no honest threshold: a single
	// large copy can legitimately outlast a poll interval. It is here
	// because not every way a move gets stuck leaves a reason on the row,
	// so this is the number that keeps growing when FailedMoves cannot
	// see the problem. See internal/health's own field doc.
	OldestOpenMoveAge time.Duration

	// FailedMoves is the subset of OpenMoves whose last attempt failed.
	// This is the one number here that changes State, and it is what
	// makes a week of failing moves reach a status page at all.
	FailedMoves int

	// OldestFailedMoveAge is how long the oldest failing relocation has
	// been open, from when this manager planned it. It is the difference
	// between a blip and a wedge.
	OldestFailedMoveAge time.Duration

	// FailedMoveReason is what the engine last recorded on that move.
	// Never rendered on its own: a count without a reason sends an
	// operator looking, and the reason is the only account of the failure
	// that outlives the cycle that hit it.
	FailedMoveReason string
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
		ReadOnlyRetainedCount:         bs.ReadOnlyRetainedCount,

		Placement: PlacementHealth{
			AwayFromHome:        bs.Placement.AwayFromHome,
			UnconfirmedLocation: bs.Placement.UnconfirmedLocation,
			OpenMoves:           bs.Placement.OpenMoves,
			FailedMoves:         bs.Placement.FailedMoves,
			FailedMoveReason:    bs.Placement.FailedMoveReason,
		},
	}
	if bs.Placement.OldestOpenMoveAge != nil {
		out.Placement.OldestOpenMoveAge = *bs.Placement.OldestOpenMoveAge
	}
	if bs.Placement.OldestAwayFromHomeAge != nil {
		out.Placement.OldestAwayFromHomeAge = *bs.Placement.OldestAwayFromHomeAge
	}
	if bs.Placement.OldestFailedMoveAge != nil {
		out.Placement.OldestFailedMoveAge = *bs.Placement.OldestFailedMoveAge
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
