package obs

import (
	"context"
	"github.com/spdrman/backupd/core/cliecho"
	"log/slog"
	"time"
)

// This file is the FR-23 event catalog: one constant and one method per
// bullet FR-23 lists (docs/EPIC.md), plus a couple of split-outs where a
// single bullet names two distinct moments (cycle start and cycle end are
// one FR-23 line but two log lines).
//
// # Why every event has a named constant
//
// A log line is only structured if the thing reading it can rely on the
// event field's value staying put. If "lifecycle_transition" quietly became
// "lifecycle-transition" or "transition" in a later refactor because nobody
// thought of it as an API, every dashboard, alert rule and jq filter built
// against the old value breaks silently, with no compiler and no test to
// catch it. Declaring each one as an exported constant does two things: it
// gives grep a single definition site to point at, and it lets this
// package's own tests assert the literal string, so a rename shows up as a
// failing test instead of a quiet contract break for whoever is parsing
// these lines downstream. Treat these strings exactly as you would a
// database column name or a wire-protocol field: renaming one is a breaking
// change, not a cleanup.
//
// # Why a method per event instead of one generic call
//
// Event (logger.go) is the general fallback and would technically suffice
// for all of these. The methods below exist anyway because each FR-23 item
// has a specific, known shape (a lifecycle transition always has a from and
// a to, a retry always has an attempt count), and encoding that as a
// typed parameter list catches a caller passing the wrong thing at compile
// time, whereas Event's attrs ...slog.Attr would accept anything and typo
// its way into production silently. The methods are thin on purpose: each
// one is a couple of lines translating typed parameters into the
// corresponding attrs, so the actual logging behaviour (level selection,
// the event constant, nil-safety) still lives in exactly one place, emit.
//
// # Field naming
//
// Fields reuse the vocabulary internal/state and internal/model already
// settled on (artifact, backup_set, bytes_transferred, checksummed, ...)
// rather than inventing parallel names, so a reader who already knows this
// codebase's domain vocabulary does not have to learn a second one just for
// the logs.
//
// # Which of these state an outcome, and which do not
//
// Every event below that reports a COMPLETION states how it went, as an
// Result (action.go) rather than as something a client works out from
// the absence of an error field. That is most of this list: a cycle end,
// a discovery pass, a transfer, a validation, a commit, a remote delete,
// a reconciliation, a retention verdict, a retention hold, a retry, an
// alert delivered, and Error, which is the completion of whatever its op
// names.
//
// Four kinds state nothing, or state only half, and the absence is the
// point rather than an oversight.
//
// Startup and RcloneVersion are announcements. Nothing was attempted, so
// there is no way it went.
//
// Hash is a measurement. A digest is neither good nor bad news; what is
// done with it later is.
//
// StaleBackup and DiskPressure report a CONDITION rather than an
// operation. Nothing ran, so nothing went any way: a filesystem crossing
// a threshold is a fact about the world this process noticed, and calling
// that a warning outcome would stretch the field from "how the thing I
// did turned out" to "how I feel about what I saw", which is the second
// vocabulary this one exists to avoid needing. Both are already emitted
// at a level that says how much attention they want, and that half was
// never the problem.
//
// LifecycleTransition states HALF an outcome, and it is the one worth
// saying out loud, because it is the event a reader most expects to find
// a whole one on.
//
// It states the failure. An artifact that ended an attempt in one of the
// three states this machine calls exceptional produced no backup, which
// is the manager failing at the one thing it is for, and no screen has to
// be consulted to know it. Its caller passes that in as a bool (see the
// method), so this package still does not know a state name. Until issue
// #625 the line arrived at info, which meant `activity --follow
// --severity error` answered "did a backup fail" with silence while the
// browser painted the very same line red off a list of state names it was
// keeping itself.
//
// It states nothing about the rest. An artifact reaching VERIFIED is good
// news, but WHICH resting states in the FR-10 machine deserve to READ as
// good news is a decision about a screen: ActivityStrip's own
// SETTLED_STATES set says so at length, differs from
// internal/retention's managed-complete set at both ends, and is
// registered as differing from it in that package's
// managedcompleteprose_test.go. Deciding that here would move a display
// decision into the engine's vocabulary, which is the thing
// service/activity.go correctly refuses to do. The transition carries
// from and to, which is the fact; what a log makes of them is the
// client's.
const (
	// EventStartup marks the process starting: binary version, commit and
	// the Go toolchain it was built with.
	EventStartup = "startup"

	// EventRcloneVersion records the embedded rclone version the running
	// process was built against.
	EventRcloneVersion = "rclone_version"

	// EventCycleStart marks the beginning of one discovery-through-retention
	// processing cycle (docs/EPIC.md's "run" / "daemon" loop).
	EventCycleStart = "cycle_start"

	// EventCycleEnd marks that same cycle's end, however it ended.
	EventCycleEnd = "cycle_end"

	// EventDiscovery summarizes one discovery pass over a backup set's
	// remote (FR-8): how many candidates landed in each of
	// discovery.Result's buckets.
	EventDiscovery = "discovery"

	// EventLifecycleTransition records one artifact moving from one FR-10
	// state to another.
	EventLifecycleTransition = "lifecycle_transition"

	// EventTransferStats records what a completed transfer step (FR-11)
	// actually moved.
	EventTransferStats = "transfer_stats"

	// EventHash records a computed or compared content hash (FR-13,
	// FR-16). A hash is a fingerprint meant for exactly this kind of audit
	// trail, not sensitive material, so unlike a key path it belongs in
	// the clear here.
	EventHash = "hash"

	// EventValidation records the outcome of FR-13's optional
	// application-level validator.
	EventValidation = "validation"

	// EventCommit records FR-14's durable local commit: the .partial file
	// fsynced, renamed to its final name, and that rename's directory
	// entry fsynced.
	EventCommit = "commit"

	// EventRemoteDelete records FR-15/FR-16's explicit, manager-controlled
	// deletion of the remote source artifact.
	EventRemoteDelete = "remote_delete"

	// EventReconciliation records one FR-17 reconciliation decision:
	// SQLite, local disk and remote state compared, and what the
	// reconciler did about a mismatch.
	EventReconciliation = "reconciliation"

	// EventRetention records one FR-18/FR-19 GFS retention verdict for a
	// single artifact.
	EventRetention = "retention"

	// EventRetentionHold records a whole backup set's retention pass
	// refusing to delete anything, because the restore point FR-19
	// reports as protected has no confirmed readable copy (FR-30, issue
	// #602). It is a condition to be reconciled, not a verdict, which is
	// why it is not an EventRetention line with a different decision on
	// it.
	EventRetentionHold = "retention_hold"

	// EventRetry records one FR-22 bounded-backoff retry attempt.
	EventRetry = "retry"

	// EventStaleBackup records a backup set whose newest known-good
	// restore point has exceeded its configured stale_after threshold
	// (FR-8's Completion, FR-24's STALE backup-health state).
	EventStaleBackup = "stale_backup"

	// EventDiskPressure records the destination filesystem's capacity
	// crossing a configured warning or critical threshold (FR-21).
	EventDiskPressure = "disk_pressure"

	// EventAlert records one proactive alert actually delivered to an
	// operator (docs/EPIC-B-multi-nas.md §71's Work Package 3.5): a
	// stale backup, repeated failure, changed SSH host key or critical
	// storage pressure that internal/alert observed for the first time
	// and pushed through the configured notification mechanism. It is
	// deliberately one line per DELIVERY, not one per evaluation pass,
	// so the log answers "who was told what, and when" rather than
	// re-stating a still-unresolved condition on every poll.
	EventAlert = "alert"

	// EventAPIAction records one action somebody took through the
	// /api/v1 surface: what was asked, by whom, what came back, and the
	// `backupd` command that would have done the same thing
	// (issue #599).
	//
	// It is the one event in this catalog that is not emitted by the
	// cycle, and that is the gap it closes. Every step a cycle takes has
	// been on this list since FR-23, and nothing the Web UI did emitted
	// anything at all: a connection test returned a sentence to one
	// caller and vanished, a settings patch left no trace, a refusal
	// reached a dialog that closed. So an operator watching a button do
	// nothing could not tell "it refused" from "it failed" from "it was
	// never wired up", and none of those three left a line anywhere.
	//
	// One line per action, refusals included, which is the half that
	// matters: a refusal is the case where an operator has nothing else
	// to go on.
	EventAPIAction = "api_action"

	// EventError is the catch-all for an error that does not already have
	// a more specific event above attached to it (for example, a failure
	// reading config, or an unexpected panic recovered at the top of a
	// cycle). Prefer attaching an error to the specific event it belongs
	// to (RemoteDelete, Retry, CycleEnd, ...) when one applies; reach for
	// this only when none does.
	EventError = "error"
)

// Startup logs EventStartup: binaryVersion and commit are normally the
// values cmd/backupd's main.go already sets via -ldflags (default
// "dev" / "none" in a non-release build), and goVersion is typically
// runtime.Version(). None of these are secret; they exist to make "which
// build is this" answerable from a log line alone, without shelling into
// the host to run `backupd version`.
func (l *Logger) Startup(ctx context.Context, binaryVersion, commit, goVersion string) {
	l.emit(ctx, LevelInfo, EventStartup, cliecho.Binary+" starting",
		slog.String("version", binaryVersion),
		slog.String("commit", commit),
		slog.String("go_version", goVersion),
	)
}

// RcloneVersion logs EventRcloneVersion for the embedded rclone build the
// running process is using.
func (l *Logger) RcloneVersion(ctx context.Context, version string) {
	l.emit(ctx, LevelInfo, EventRcloneVersion, "embedded rclone version",
		slog.String("rclone_version", version),
	)
}

// CycleStart logs EventCycleStart. cycleID is the caller's own correlation
// id for this pass (for example a timestamp or a counter); this package
// does not mint one itself, since deciding what identifies a cycle is the
// caller's business, not this package's.
// The cycle id is also the ACTION id both lines carry, which is what
// pairs them by something other than the habit of spelling one event
// cycle_start and the other cycle_end (issue #625). Nothing else about
// either line changes, and no caller has to do anything: the two halves
// were already given the same cycle id, so the pairing was sitting there
// unused. What it buys is that a reader can now notice a cycle that
// announced itself and never reported an outcome, which is the state an
// operator most needs named and the one nothing could see.
func (l *Logger) CycleStart(ctx context.Context, cycleID string) {
	l.emitMarked(ctx, LevelInfo, mark{action: ActionCycle, actionID: cycleID}, EventCycleStart, "cycle starting",
		slog.String("cycle_id", cycleID),
	)
}

// CycleEnd logs EventCycleEnd for the cycle cycleID started. duration is
// the cycle's wall-clock length; err is the cycle's terminal error, if any
// (nil for a clean run). A non-nil err logs at LevelError rather than
// LevelInfo, since a cycle that ended in error is exactly the kind of line
// an operator's alerting should be able to key off of by level alone,
// without also having to know to check for an error field's presence.
func (l *Logger) CycleEnd(ctx context.Context, cycleID string, duration time.Duration, err error) {
	result := ResultSuccess
	msg := "cycle finished"
	attrs := []slog.Attr{
		slog.String("cycle_id", cycleID),
		slog.Duration("duration", duration),
	}
	if err != nil {
		result = ResultError
		msg = "cycle finished with an error"
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	l.emitMarked(ctx, result.Level(), mark{result: result, action: ActionCycle, actionID: cycleID}, EventCycleEnd, msg, attrs...)
}

// Discovery logs EventDiscovery: a summary of one discovery pass over
// backupSet, matching discovery.Result's own partition of what happened to
// each candidate the remote listing produced (discovered, alreadyKnown,
// pending, rejected, conflicts, errored are meant to be len() of that
// Result's Discovered, AlreadyKnown, Pending, Rejected, Conflicts and
// Errors fields respectively).
func (l *Logger) Discovery(ctx context.Context, backupSet string, discovered, alreadyKnown, pending, rejected, conflicts, errored int) {
	result := ResultInfo
	if errored > 0 {
		result = ResultWarn
	}
	l.emitMarked(ctx, result.Level(), mark{result: result}, EventDiscovery, "discovery pass complete",
		slog.String("backup_set", backupSet),
		slog.Int("discovered", discovered),
		slog.Int("already_known", alreadyKnown),
		slog.Int("pending", pending),
		slog.Int("rejected", rejected),
		slog.Int("conflicts", conflicts),
		slog.Int("errored", errored),
	)
}

// LifecycleTransition logs EventLifecycleTransition: artifact identifies
// the artifact that moved (its String() form, e.g. "source/set/name"), and
// from/to are the FR-10 state names it moved between (a State's own
// String() form, e.g. "DISCOVERED"). detail carries an optional
// human-readable note (empty is fine); it is never the place to put an
// error's raw text if that text might embed a path (see Retry and
// RemoteDelete below for how those handle an error argument instead of a
// free-text detail).
// failed says the artifact landed in one of the states the machine calls
// exceptional, which is a backup that did not happen. It is a bool the
// CALLER computes rather than something this package works out from `to`,
// and that is the whole of the split the catalog note above describes:
// which state names mean a failed attempt is internal/lifecycle's to
// answer (IsExceptionalState), and this package declining to import a
// domain vocabulary is what keeps it the standalone sink it is. The shape
// is Validation's, one event over, for the same reason: the caller knows
// how it went and this decides what that costs in severity.
//
// A true logs at LevelError with an error outcome. That is a departure
// from the convention Alert and RetentionHold state, and a deliberate
// one: those two reserve LevelError for the manager failing at something
// rather than for correctly reporting a problem it found, and an artifact
// that ended its attempt in FAILED or QUARANTINED IS the manager failing
// at the one thing it exists to do. `activity --follow --severity error`
// is where an operator goes to ask whether a backup did not happen, and
// before issue #625 the answer was silence.
//
// A false is exactly what it always was, and that is the point. Which of
// the OTHER states read as good news is a decision about a screen (see
// the catalog note above), and stating a success here would take that
// decision away from every client at once.
func (l *Logger) LifecycleTransition(ctx context.Context, artifact, from, to, detail string, failed bool) {
	attrs := []slog.Attr{
		slog.String("artifact", artifact),
		slog.String("from", from),
		slog.String("to", to),
	}
	if detail != "" {
		attrs = append(attrs, slog.String("detail", detail))
	}
	if !failed {
		l.emit(ctx, LevelInfo, EventLifecycleTransition, "lifecycle transition", attrs...)
		return
	}
	l.emitMarked(ctx, ResultError.Level(), mark{result: ResultError}, EventLifecycleTransition, "lifecycle transition", attrs...)
}

// TransferStats logs EventTransferStats for one completed FR-11 transfer:
// bytesTransferred and duration describe what moved and how long it took,
// and checksummed carries state.TransferResult.Checksummed straight
// through.
//
// That field is a journal column nothing writes any more (#492 removed the
// transport-side claim it came from, and its doc says why), so this
// attribute is false on every real transfer. The key stays in the event
// because the shape of a log line is an operator-visible surface under
// FR-35, and it stays honest because false is exactly what "no copy-time
// checksum was recorded" should read as.
func (l *Logger) TransferStats(ctx context.Context, artifact string, bytesTransferred int64, duration time.Duration, checksummed bool) {
	l.emitMarked(ctx, LevelInfo, mark{result: ResultSuccess}, EventTransferStats, "transfer complete",
		slog.String("artifact", artifact),
		slog.Int64("bytes_transferred", bytesTransferred),
		slog.Duration("duration", duration),
		slog.Bool("checksummed", checksummed),
	)
}

// Hash logs EventHash: a content hash computed or compared for artifact.
// alg names the algorithm (e.g. "sha256"); hash is its hex digest. Neither
// value is treated as sensitive (see EventHash's doc above), so both are
// logged as plain strings rather than wrapped in Secret.
func (l *Logger) Hash(ctx context.Context, artifact, alg, hash string) {
	l.emit(ctx, LevelInfo, EventHash, "content hash",
		slog.String("artifact", artifact),
		slog.String("alg", alg),
		slog.String("hash", hash),
	)
}

// Validation logs EventValidation for FR-13's optional external validator
// outcome. A failed validation logs at LevelWarn, since it means an
// artifact is being routed to QUARANTINED or FAILED and is worth standing
// out from routine LevelInfo traffic without rising to LevelError (the
// validator did its job correctly; it found something wrong with the
// content, which is a successful check, not a system failure).
func (l *Logger) Validation(ctx context.Context, artifact string, passed bool, detail string) {
	result := ResultSuccess
	if !passed {
		result = ResultWarn
	}
	attrs := []slog.Attr{
		slog.String("artifact", artifact),
		slog.Bool("passed", passed),
	}
	if detail != "" {
		attrs = append(attrs, slog.String("detail", detail))
	}
	l.emitMarked(ctx, result.Level(), mark{result: result}, EventValidation, "validation result", attrs...)
}

// Commit logs EventCommit for FR-14's durable local commit: localPath is
// the artifact's final (non-.partial) local destination. This is an
// operational path under the backup set's configured local directory, not
// a credential, so it is logged in the clear; it is what durable commit
// actually IS, from an audit-trail standpoint.
func (l *Logger) Commit(ctx context.Context, artifact, localPath string) {
	l.emitMarked(ctx, LevelInfo, mark{result: ResultSuccess}, EventCommit, "durable commit complete",
		slog.String("artifact", artifact),
		slog.String("local_path", localPath),
	)
}

// RemoteDelete logs EventRemoteDelete for FR-15/FR-16's remote-source
// deletion. remotePath is the artifact's path on the remote (not a
// credential); err is the deletion attempt's outcome, nil for success. A
// non-nil err logs at LevelError.
func (l *Logger) RemoteDelete(ctx context.Context, artifact, remotePath string, err error) {
	result := ResultSuccess
	msg := "remote source deleted"
	attrs := []slog.Attr{
		slog.String("artifact", artifact),
		slog.String("remote_path", remotePath),
	}
	if err != nil {
		result = ResultError
		msg = "remote delete failed"
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	l.emitMarked(ctx, result.Level(), mark{result: result}, EventRemoteDelete, msg, attrs...)
}

// Reconciliation logs EventReconciliation for one FR-17 reconciliation
// decision. scenario is a short, stable label for which row of FR-17's
// reconciliation table matched (for example "remote_absent_local_final",
// mirroring docs/EPIC.md's own table); action is what the reconciler did
// about it (for example "advance_to_complete", "quarantine_local",
// "resume_transfer").
func (l *Logger) Reconciliation(ctx context.Context, artifact, scenario, action string) {
	l.emitMarked(ctx, LevelInfo, mark{result: ResultInfo}, EventReconciliation, "reconciliation decision",
		slog.String("artifact", artifact),
		slog.String("scenario", scenario),
		slog.String("action", action),
	)
}

// Retention logs EventRetention for one FR-18/FR-19 GFS retention verdict.
// tier is the bucket the artifact was classified into ("daily", "weekly",
// "monthly", "protected", or "" for none); decision is the resulting policy
// action ("keep" or "delete").
func (l *Logger) Retention(ctx context.Context, artifact, backupSet, tier, decision string) {
	l.emitMarked(ctx, LevelInfo, mark{result: ResultInfo}, EventRetention, "retention decision",
		slog.String("artifact", artifact),
		slog.String("backup_set", backupSet),
		slog.String("tier", tier),
		slog.String("decision", decision),
	)
}

// RetentionHold logs EventRetentionHold: a retention pass over backupSet
// refused every deletion in it, because the restore point FR-19 reports as
// protected has no confirmed readable copy (FR-30, issue #602). reason is
// internal/retention's own sentence, which names the last known good it
// could not confirm and what was wrong with it.
//
// The artifact is carried inside that sentence rather than as a field of
// its own, because this event is about a backup SET: the hold stops every
// deletion in it, an alert on it groups by backup_set, and a second
// artifact-shaped field on the verdicts that carry this reason would be a
// parallel copy of a fact the sentence already states.
//
// LevelWarn, and the level is the point of the event. Every other refusal
// in a retention plan is a routine outcome an operator reads in the plan;
// this one means retention for this backup set has stopped and will stay
// stopped until somebody reconciles its inventory (FR-17), while the only
// other symptom is local copies quietly accumulating until FR-21's
// capacity refusal starts refusing transfers for a reason that names
// neither this set nor this cause. It is deliberately not LevelError, for
// the reason Alert's own doc gives: this package reserves that for the
// manager failing at something, and refusing to delete a backup it cannot
// prove is safe to delete is the manager working exactly as designed.
//
// Emitted from an apply and never from a preview. The condition is
// permanent until it is fixed, so a preview surface that logged it would
// write one of these per dashboard poll for as long as the set stayed
// broken, which is how a real signal gets filtered out.
func (l *Logger) RetentionHold(ctx context.Context, backupSet, reason string) {
	l.emitMarked(ctx, LevelWarn, mark{result: ResultWarn}, EventRetentionHold, "retention held: no confirmed copy of this backup set's last known good",
		slog.String("backup_set", backupSet),
		slog.String("reason", reason),
	)
}

// Retry logs EventRetry for one FR-22 bounded-backoff attempt. op names the
// operation being retried (for example "copy_to_local"); attempt is the
// 1-based attempt number that just failed; category is the
// transport.Category that classified the failure (its String() form, e.g.
// "transient"); err is that attempt's error. Retry always logs at
// LevelWarn: FR-22's whole premise is that a Transient failure is expected
// to happen sometimes and is not itself an incident, but it is still worth
// distinguishing from routine LevelInfo traffic, since a backup set retrying
// constantly is a real signal even before its retry budget is exhausted.
func (l *Logger) Retry(ctx context.Context, op string, attempt int, category string, err error) {
	attrs := []slog.Attr{
		slog.String("op", op),
		slog.Int("attempt", attempt),
		slog.String("category", category),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	l.emitMarked(ctx, LevelWarn, mark{result: ResultWarn}, EventRetry, "retrying after a transient failure", attrs...)
}

// StaleBackup logs EventStaleBackup: backupSet's newest known-good restore
// point is age old against a configured threshold (config.BackupSet's
// stale_after). This always logs at LevelWarn, since by definition it only
// ever fires once age has already exceeded threshold.
func (l *Logger) StaleBackup(ctx context.Context, backupSet string, age, threshold time.Duration) {
	l.emit(ctx, LevelWarn, EventStaleBackup, "backup set is stale",
		slog.String("backup_set", backupSet),
		slog.Duration("age", age),
		slog.Duration("threshold", threshold),
	)
}

// Alert logs EventAlert: one proactive notification that internal/alert
// just delivered. kind is the alert's own typed kind (STALE_BACKUP,
// REPEATED_FAILURE, HOST_KEY_CHANGED, CRITICAL_STORAGE_PRESSURE),
// backupSet is what it was about, and detail is the operator-facing text
// that actually went out.
//
// This always logs at LevelWarn: every condition §71 alerts on is, by
// definition, something already wrong. It is deliberately not LevelError,
// which this package reserves for the manager itself failing at something
// (see Error and CycleEnd) rather than for correctly reporting a problem
// it detected. A delivery that FAILED is logged through Error instead, by
// internal/alert, since that one is the manager failing.
func (l *Logger) Alert(ctx context.Context, kind, backupSet, detail string) {
	l.emitMarked(ctx, LevelWarn, mark{result: ResultWarn}, EventAlert, "proactive alert delivered",
		slog.String("alert_kind", kind),
		slog.String("backup_set", backupSet),
		slog.String("detail", detail),
	)
}

// DiskPressure logs EventDiskPressure for FR-21's destination-capacity
// monitoring. path is the filesystem path checked; freeBytes/totalBytes
// describe its current capacity; threshold is the one crossed ("warning" or
// "critical"; any other value is logged as given, this package does not
// validate it). A "critical" threshold logs at LevelError, anything else at
// LevelWarn, since disk pressure by definition means at least a warning
// threshold has already been crossed.
//
// The field this carries is named threshold_level, not level: this
// package's JSON lines already have a top-level "level" key for the log
// record's own severity (INFO/WARN/ERROR), courtesy of slog. Reusing that
// name for the threshold would silently shadow it in the JSON object
// (encoding/json keeps the last of two duplicate keys, so the severity
// would vanish behind whichever of the two happened to be written second)
// rather than raise any error, which makes it exactly the kind of bug a
// test has to catch rather than a reviewer noticing by eye. See
// TestDiskPressureEventDoesNotShadowSeverityLevel.
func (l *Logger) DiskPressure(ctx context.Context, path string, freeBytes, totalBytes int64, threshold string) {
	sevLevel := LevelWarn
	if threshold == "critical" {
		sevLevel = LevelError
	}
	l.emit(ctx, sevLevel, EventDiskPressure, "disk pressure threshold crossed",
		slog.String("path", path),
		slog.Int64("free_bytes", freeBytes),
		slog.Int64("total_bytes", totalBytes),
		slog.String("threshold_level", threshold),
	)
}

// Error logs EventError: op names what was being attempted; err is what
// went wrong. Use this only when no more specific helper above already
// covers the failure (RemoteDelete, Retry and CycleEnd all accept their own
// error argument and should be preferred when they apply, so the same
// failure does not need two different event names depending on which
// helper happened to be reached for).
//
// Whatever produced err is responsible for not having built it out of a
// Secret's raw value in the first place; wrapping *after* the fact, here,
// is too late; a Secret must be wrapped where it is read (see secret.go),
// not where it is logged.
func (l *Logger) Error(ctx context.Context, op string, err error) {
	l.emitMarked(ctx, LevelError, mark{result: ResultError}, EventError, "error",
		slog.String("op", op),
		slog.String("error", err.Error()),
	)
}
