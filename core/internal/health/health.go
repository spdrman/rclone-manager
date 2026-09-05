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
//     trustworthy, so this is not HEALTHY either), a fresh known-good
//     backup alongside a failure that is still being retried, or a fresh
//     known-good backup that is not where the operator's retention chain
//     says it belongs because the relocation meant to move it there keeps
//     failing (issue #444, see mediums.go).
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
	return ProcessHealth(in)
}

// BackupSetInputs is what one backup set's BackupSetHealth needs that
// cannot be derived from its journal rows: a connectivity/liveness signal
// for the most recent discovery poll, when retention last ran, and free
// space on the set's destination filesystem. All three come from
// subsystems outside this package's reach today (see the package doc), so
// ComputeBackupSetHealth takes them as explicit input rather than reaching
// for them, and none of the three is ever passed to decideState: they
// describe the set, but they never decide its State.
//
// PlacementEvidence (mediums.go) is deliberately NOT one of these and is
// a separate argument. Everything here is a reading that can legitimately
// be unavailable, where nil means "unknown" and the report says so, and
// nothing here is allowed to reach a verdict. That is the opposite of
// what the placement evidence is for on both counts.
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

	// HaltReason is why the manager could not connect to this backup set
	// the last time it tried, or empty when nothing of the kind has been
	// observed (issue #245). It comes from internal/state's durable
	// backup_set_halts record, which is written by the cycle that hit the
	// refusal and removed by a later cycle that connected.
	//
	// Empty is "no refusal is on record", never "this set is reachable".
	// The two are different claims and only the first one is ever
	// available to make here, which is exactly why this is a reason and
	// not a boolean: see issue #231 for what a fabricated definite value
	// cost the last time this concept had one.
	//
	// Like the three fields above it, this is carried for a renderer and
	// never reaches decideState. That is not a convention: decideState's
	// evidence parameter has no field it could arrive through.
	HaltReason string
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

	// ReinstatedRemoteRetainedCount is how many artifacts in this set were
	// reinstated out of quarantine and still have a remote source this
	// manager has undertaken never to delete (issue #227).
	//
	// # Why this is a standing condition and not a failure
	//
	// Reinstating an artifact permanently forfeits its remote delete:
	// internal/lifecycle's FR-15 gate refuses one outright, before it
	// records intent and before it touches the transport, reading the fact
	// out of the append-only transition log (see ADR 0004). That is the
	// price that makes the reinstatement edges safe, and the direction it
	// fails in is deliberate: a reinstatement that should not have
	// happened costs disk on the source, while a delete that should not
	// have happened costs the backup.
	//
	// But it is permanent by design, so this number only ever grows, and
	// an operator is told about it exactly once, at the moment they
	// reinstate. remotedelete.go's own package doc already names what that
	// costs for the other reason a delete gets refused routinely: an
	// archive that never prunes its remote side fills the source disk
	// eventually, and that has to be loud rather than discovered when the
	// volume is full. This is the count that makes it askable a month
	// later.
	//
	// It never reaches decideState, which has no field it could arrive
	// through (see compute.go's evidence type): the backups themselves are
	// fine, and a set holding reinstated artifacts is not thereby
	// DEGRADED. What is accumulating is storage on a machine this manager
	// does not measure.
	//
	// # Which artifacts it counts, and why not the others
	//
	// Membership comes from the append-only transition log, via
	// lifecycle.ReinstatedArtifacts, which derives its edge set from the
	// same lifecycle.ReinstatementEdges() the delete gate's own refusal
	// reads. That is the whole reason it is not a counter maintained
	// alongside the writes: two independently-kept answers to the same
	// question drift, and the one that drifts silently is this one.
	//
	// An artifact whose remote source this manager has already released
	// (state.Record.RemoteDeletedAt is set) is excluded, because there is
	// nothing left for it to be holding. That is not a corner case: the
	// QUARANTINED_LOST -> COMPLETE edge is reachable only from COMPLETE,
	// which is precisely the state that says the remote object is gone, so
	// every reinstatement of that kind lands here and counting it would
	// send an operator looking for storage that does not exist.
	//
	// # There is deliberately no byte figure beside it
	//
	// How much those preserved remote objects actually occupy is a fact
	// this manager does not have and must not invent. The only size it
	// ever recorded is what the remote object measured at discovery, and
	// FR-8's rule that remote metadata is untrusted applies with full
	// force here: the object may since have been removed by its producer,
	// replaced, or grown, and the reason the delete gate refuses in the
	// first place is that the manager usually cannot re-establish the
	// remote's identity with confidence. Summing discovery-time sizes
	// would be a confident number about bytes nobody has looked at, and
	// re-Stat-ing every artifact on every health pass would put a network
	// call per artifact behind every status invocation and dashboard load,
	// against a source that may be unreachable. So this is a count, and
	// the size is not known: see issue #211 for what this repository does
	// with a field it cannot compute honestly.
	ReinstatedRemoteRetainedCount int

	// ReadOnlyRetainedCount is how many artifacts in this set currently
	// sit at REMOTE_RETAINED: this backup set is declared read-only
	// (config.BackupSet.ReadOnly, issue #282), and this manager has never
	// called, and structurally cannot call, transport.Transport.DeleteRemote
	// for them.
	//
	// Unlike ReinstatedRemoteRetainedCount, this is a direct count of the
	// artifacts' own CURRENT journal state, not a join against transition
	// history: REMOTE_RETAINED is unambiguous the moment it is read, so
	// there is no "was this artifact ever retained" question that only the
	// append-only log could answer. It shares that field's doc's other
	// reasoning, though: no bytes figure is reported beside it, for the
	// same reason (this manager's only size reading for these objects is
	// the one taken at discovery, and it may be stale by an unknown
	// amount), and it is never omitted, because zero is a real, common
	// reading here (every backup set with no read_only flag set).
	ReadOnlyRetainedCount int

	// Placement is FR-24's medium half (issue #444): how many of this
	// set's artifacts are not on the medium its retention chain says they
	// belong on, and whether the relocations meant to fix that are
	// getting anywhere. See mediums.go, which holds both the type and the
	// argument for why health had to grow a second question.
	//
	// Unlike the four injected display-only facts below it, one number in
	// here DOES reach decideState: a relocation that has been tried and
	// has not worked turns an otherwise-HEALTHY set DEGRADED. That is the
	// whole point of the field. It is durable journal evidence, the same
	// kind as everything else State is decided from, and a week of
	// failing moves that could not change any verdict was the defect this
	// field exists to end.
	Placement PlacementHealth

	// LastRetentionRunAt, FreeBytes and HaltReason are injected; see
	// BackupSetInputs.
	LastRetentionRunAt *time.Time
	FreeBytes          *uint64

	// HaltReason is why this set could not be connected to, empty when no
	// refusal is on record (issue #245). It sits beside the verdict and
	// never inside it: a set refused on every cycle still gets its State
	// from journal evidence alone, and that State is usually STALE, which
	// is true and incomplete. This is the missing half of the sentence.
	HaltReason string
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
