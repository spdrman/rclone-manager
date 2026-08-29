// Package health computes FR-24's health picture: whether the manager
// process is running, and, entirely separately, whether each configured
// backup set is actually producing fresh, trustworthy restore points.
//
// # Why this is two questions, not one
//
// Failure-safety invariant 14 says process liveness is not evidence of
// backup freshness. A health check that answers both questions with a
// single "is everything OK" bit will report green for as long as the
// process keeps running, even after the one thing this whole project
// exists to guarantee, backups actually landing, has silently stopped. That
// is the exact failure this package is built to make structurally
// impossible to produce by accident:
//
//   - ProcessHealth carries only process/build facts (BinaryVersion,
//     RcloneVersion). It has no field that could hold a journal-derived
//     fact, and NewProcessHealth takes no journal, no records, nothing
//     backup-shaped, as input. There is no way to compute a backup-set
//     verdict through it, because it has nowhere to put one.
//
//   - BackupSetHealth carries only one backup set's evidence and verdict.
//     Its State is computed by decideState in compute.go, a function whose
//     one parameter, evidence, has no field for process liveness,
//     free space, or version strings. Nothing about "is the process alive"
//     can influence State, because decideState is never handed that fact
//     in the first place, not because the code merely declines to look.
//
//   - The two types share no field names (TestProcessAndBackupSetHealthShareNoFields
//     checks this directly), so a caller reading a ProcessHealth can never
//     mistake it for a BackupSetHealth or vice versa by, say, reusing a
//     field name out of habit.
//
// Report exists only to bundle one ProcessHealth with every configured
// backup set's BackupSetHealth so a renderer (the `backup-manager status`
// CLI, or an optional HTTP handler, both separate issues) can print both
// halves from one already-computed value, without recomputing anything and
// without either half leaking into the other's answer.
//
// # The four backup-set states
//
// FR-24 names HEALTHY, DEGRADED, STALE and FAILING. decideState in
// compute.go is the one place that assigns them; see its comments for the
// exact boundaries, but in short:
//
//   - FAILING is for artifacts that need a human right now: any
//     QUARANTINED_LOST artifact (the irrecoverable loss lifecycle defines,
//     see internal/lifecycle's package doc) forces FAILING no matter how
//     fresh other artifacts in the set are, and a FAILED artifact with no
//     retry scheduled does too. QUARANTINED_LOST must never read as merely
//     DEGRADED: it is the one loss in this whole system that cannot be
//     fixed by waiting or retrying, so it gets the most severe state,
//     checked first, before freshness is even considered.
//
//   - STALE means the freshness guarantee is broken: no known-good backup
//     (COMMITTED, REMOTE_DELETE_PENDING or COMPLETE, per FR-19) exists
//     within the configured stale_after window, and nothing has happened
//     recently enough to suggest a first backup is merely still in flight.
//
//   - DEGRADED covers everything that deserves attention but has not
//     broken the freshness guarantee outright: a backup set that has never
//     produced any artifact at all yet (explicitly not STALE: nothing has
//     "stopped" if nothing ever started), a set whose first backup looks to
//     still be in progress, a set with a fresh known-good backup but a
//     quarantined newest arrival (something did arrive, but it is not
//     trustworthy, so this is not HEALTHY either), or a fresh known-good
//     backup alongside a failure that is still being retried.
//
//   - HEALTHY requires positive evidence: a known-good backup inside the
//     stale window, with nothing else needing attention. Silence is never
//     read as HEALTHY.
//
// # Injected inputs
//
// Free space, the last retention run, the last successful poll and the
// embedded rclone/binary versions all come from subsystems that either do
// not exist yet in this repository (internal/capacity, internal/retention)
// or live outside this package's reach (build-time version variables in
// cmd/, and internal/transport/rclone, which does not expose a version
// today). This package never reaches for them: BackupSetInputs and
// ProcessInputs are how a caller supplies them, so this package stays
// buildable and testable independently of whichever issue lands those
// subsystems, and so the value is always explicit at every call site
// instead of quietly defaulting to zero.
package health

import (
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// State is one of the four backup-set health states FR-24 names. See the
// package doc and decideState (compute.go) for what each one means and
// where the boundaries between them sit.
type State string

const (
	// Healthy means a known-good backup exists inside the stale window and
	// nothing else about this set currently needs attention.
	Healthy State = "HEALTHY"

	// Degraded means something is worth a look, but the freshness
	// guarantee has not broken: no history yet, a first backup that looks
	// still in progress, a quarantined newest arrival next to an otherwise
	// fresh known-good backup, or a failure still being retried.
	Degraded State = "DEGRADED"

	// Stale means no known-good backup exists inside the stale window and
	// there is no recent activity to suggest one is still coming.
	Stale State = "STALE"

	// Failing means the pipeline needs a human right now: an irrecoverable
	// QUARANTINED_LOST artifact, or a FAILED artifact with no retry
	// scheduled. This is checked before freshness, so it always wins.
	Failing State = "FAILING"
)

func (s State) String() string { return string(s) }

// OK reports whether s is the one state that requires no attention at all.
func (s State) OK() bool { return s == Healthy }

// ProcessInputs is everything ProcessHealth needs that this package cannot
// learn on its own: BinaryVersion is normally a build-time ldflags value
// living in cmd/, and RcloneVersion belongs to internal/transport/rclone,
// which does not expose one yet.
type ProcessInputs struct {
	BinaryVersion string
	RcloneVersion string
}

// ProcessHealth is the process-liveness half of FR-24. It answers "is the
// running binary the build it claims to be", nothing else, and it is built
// directly from caller-supplied facts: no journal read, no backup-set
// evidence, ever contributes to it. See the package doc for why that
// separation is structural rather than a convention.
type ProcessHealth struct {
	BinaryVersion string
	RcloneVersion string
}

// NewProcessHealth builds the process-liveness half of an FR-24 report.
func NewProcessHealth(in ProcessInputs) ProcessHealth {
	return ProcessHealth{
		BinaryVersion: in.BinaryVersion,
		RcloneVersion: in.RcloneVersion,
	}
}

// BackupSetInputs is what one backup set's BackupSetHealth needs that
// cannot be derived from its journal rows: a connectivity/liveness signal
// for the most recent discovery poll, when retention last ran, and free
// space on the set's destination filesystem. All three come from
// subsystems outside this package's reach today (see the package doc), so
// ComputeBackupSetHealth takes them as explicit input rather than reaching
// for them, and none of the three is ever passed to decideState: they
// describe the set, but they never decide its State.
type BackupSetInputs struct {
	// LastSuccessfulPollAt is when discovery last completed successfully
	// for this set. It is a liveness signal, not evidence of freshness: a
	// set can be polled successfully every cycle while its source has
	// stopped producing artifacts entirely. This is invariant 14's
	// canonical example, which is exactly why it never reaches decideState.
	LastSuccessfulPollAt *time.Time

	// LastRetentionRunAt is when GFS retention (FR-18) last ran against
	// this set.
	LastRetentionRunAt *time.Time

	// FreeBytes is free space on the filesystem backing this set's local
	// destination. Nil means the caller has not wired a free-space source
	// yet, not that free space is zero.
	FreeBytes *uint64
}

// TransferInProgress names one artifact currently in the TRANSFERRING
// state, and since when. A backup set is not guaranteed to process exactly
// one artifact at a time, so BackupSetHealth carries a slice rather than
// assuming at most one.
type TransferInProgress struct {
	Artifact  model.ArtifactID
	StartedAt time.Time
}

// BackupSetHealth is the backup-freshness half of FR-24 for exactly one
// backup set. State is computed from journal evidence and the set's
// configured stale_after threshold alone; the display-only fields sourced
// from BackupSetInputs (LastSuccessfulPollAt, LastRetentionRunAt,
// FreeBytes) are carried here for a renderer to show, but never influence
// State. See decideState in compute.go for the decision itself.
type BackupSetHealth struct {
	Set   model.BackupSetID
	State State

	// Reason is a short, human-readable explanation of why State was
	// chosen. A CLI or HTTP renderer (a later issue) can print it directly
	// instead of re-deriving an explanation from the other fields.
	Reason string

	// LastSuccessfulPollAt is a liveness signal, injected; see
	// BackupSetInputs. It never contributes to State.
	LastSuccessfulPollAt *time.Time

	// LastCompletedBackupAt is the newest artifact currently in state
	// COMPLETE: the pipeline's full happy path, remote deletion included.
	LastCompletedBackupAt *time.Time

	// NewestGoodBackupAt is the newest artifact currently in any
	// known-good state per FR-19 (COMMITTED, REMOTE_DELETE_PENDING or
	// COMPLETE): a valid restore point, whether or not its remote source
	// has been deleted yet. NewestGoodBackupAge is nil exactly when this
	// is nil.
	NewestGoodBackupAt  *time.Time
	NewestGoodBackupAge *time.Duration

	// StaleThreshold is this set's configured stale_after (internal/config
	// guarantees it is set and positive).
	StaleThreshold time.Duration

	CurrentTransfers []TransferInProgress
	PendingDeletes   int
	Failures         int

	// QuarantinedCount counts every artifact currently quarantined,
	// recoverable (QUARANTINED) or not (QUARANTINED_LOST): see machine.go's
	// note that FR-24's quarantined count should count QUARANTINED_LOST
	// too. QuarantinedLostCount is the irrecoverable subset of that same
	// count, broken out separately so it can never be mistaken for the
	// merely-recoverable kind.
	QuarantinedCount     int
	QuarantinedLostCount int

	// LastRetentionRunAt and FreeBytes are injected; see BackupSetInputs.
	LastRetentionRunAt *time.Time
	FreeBytes          *uint64
}

// Report bundles one ProcessHealth with every configured backup set's
// BackupSetHealth, computed once, so a CLI command and an HTTP handler (both
// separate issues) can each render it without recomputing anything and
// without either half of FR-24 leaking into the other.
type Report struct {
	Process     ProcessHealth
	BackupSets  []BackupSetHealth
	GeneratedAt time.Time
}

// NewReport bundles an already-computed ProcessHealth and set of
// BackupSetHealth values into one Report. It performs no computation of its
// own; every BackupSetHealth in sets should already come from
// ComputeBackupSetHealth.
func NewReport(process ProcessHealth, sets []BackupSetHealth, now time.Time) Report {
	return Report{Process: process, BackupSets: sets, GeneratedAt: now}
}
