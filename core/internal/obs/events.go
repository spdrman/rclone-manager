package obs

import (
	"context"
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

	// EventError is the catch-all for an error that does not already have
	// a more specific event above attached to it (for example, a failure
	// reading config, or an unexpected panic recovered at the top of a
	// cycle). Prefer attaching an error to the specific event it belongs
	// to (RemoteDelete, Retry, CycleEnd, ...) when one applies; reach for
	// this only when none does.
	EventError = "error"
)

// Startup logs EventStartup: binaryVersion and commit are normally the
// values cmd/backup-manager's main.go already sets via -ldflags (default
// "dev" / "none" in a non-release build), and goVersion is typically
// runtime.Version(). None of these are secret; they exist to make "which
// build is this" answerable from a log line alone, without shelling into
// the host to run `backup-manager version`.
func (l *Logger) Startup(ctx context.Context, binaryVersion, commit, goVersion string) {
	l.emit(ctx, LevelInfo, EventStartup, "backup-manager starting",
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
func (l *Logger) CycleStart(ctx context.Context, cycleID string) {
	l.emit(ctx, LevelInfo, EventCycleStart, "cycle starting",
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
	level := LevelInfo
	msg := "cycle finished"
	attrs := []slog.Attr{
		slog.String("cycle_id", cycleID),
		slog.Duration("duration", duration),
	}
	if err != nil {
		level = LevelError
		msg = "cycle finished with an error"
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	l.emit(ctx, level, EventCycleEnd, msg, attrs...)
}

// Discovery logs EventDiscovery: a summary of one discovery pass over
// backupSet, matching discovery.Result's own partition of what happened to
// each candidate the remote listing produced (discovered, alreadyKnown,
// pending, rejected, conflicts, errored are meant to be len() of that
// Result's Discovered, AlreadyKnown, Pending, Rejected, Conflicts and
// Errors fields respectively).
func (l *Logger) Discovery(ctx context.Context, backupSet string, discovered, alreadyKnown, pending, rejected, conflicts, errored int) {
	l.emit(ctx, LevelInfo, EventDiscovery, "discovery pass complete",
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
func (l *Logger) LifecycleTransition(ctx context.Context, artifact, from, to, detail string) {
	attrs := []slog.Attr{
		slog.String("artifact", artifact),
		slog.String("from", from),
		slog.String("to", to),
	}
	if detail != "" {
		attrs = append(attrs, slog.String("detail", detail))
	}
	l.emit(ctx, LevelInfo, EventLifecycleTransition, "lifecycle transition", attrs...)
}

// TransferStats logs EventTransferStats for one completed FR-11 transfer:
// bytesTransferred and duration describe what moved and how long it took,
// and checksummed reports whether the transport backend verified the copy
// against a checksum itself (transport.TransferResult.Checksummed).
func (l *Logger) TransferStats(ctx context.Context, artifact string, bytesTransferred int64, duration time.Duration, checksummed bool) {
	l.emit(ctx, LevelInfo, EventTransferStats, "transfer complete",
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
	level := LevelInfo
	if !passed {
		level = LevelWarn
	}
	attrs := []slog.Attr{
		slog.String("artifact", artifact),
		slog.Bool("passed", passed),
	}
	if detail != "" {
		attrs = append(attrs, slog.String("detail", detail))
	}
	l.emit(ctx, level, EventValidation, "validation result", attrs...)
}

// Commit logs EventCommit for FR-14's durable local commit: localPath is
// the artifact's final (non-.partial) local destination. This is an
// operational path under the backup set's configured local directory, not
// a credential, so it is logged in the clear; it is what durable commit
// actually IS, from an audit-trail standpoint.
func (l *Logger) Commit(ctx context.Context, artifact, localPath string) {
	l.emit(ctx, LevelInfo, EventCommit, "durable commit complete",
		slog.String("artifact", artifact),
		slog.String("local_path", localPath),
	)
}

// RemoteDelete logs EventRemoteDelete for FR-15/FR-16's remote-source
// deletion. remotePath is the artifact's path on the remote (not a
// credential); err is the deletion attempt's outcome, nil for success. A
// non-nil err logs at LevelError.
func (l *Logger) RemoteDelete(ctx context.Context, artifact, remotePath string, err error) {
	level := LevelInfo
	msg := "remote source deleted"
	attrs := []slog.Attr{
		slog.String("artifact", artifact),
		slog.String("remote_path", remotePath),
	}
	if err != nil {
		level = LevelError
		msg = "remote delete failed"
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	l.emit(ctx, level, EventRemoteDelete, msg, attrs...)
}

// Reconciliation logs EventReconciliation for one FR-17 reconciliation
// decision. scenario is a short, stable label for which row of FR-17's
// reconciliation table matched (for example "remote_absent_local_final",
// mirroring docs/EPIC.md's own table); action is what the reconciler did
// about it (for example "advance_to_complete", "quarantine_local",
// "resume_transfer").
func (l *Logger) Reconciliation(ctx context.Context, artifact, scenario, action string) {
	l.emit(ctx, LevelInfo, EventReconciliation, "reconciliation decision",
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
	l.emit(ctx, LevelInfo, EventRetention, "retention decision",
		slog.String("artifact", artifact),
		slog.String("backup_set", backupSet),
		slog.String("tier", tier),
		slog.String("decision", decision),
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
	l.emit(ctx, LevelWarn, EventRetry, "retrying after a transient failure", attrs...)
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
	l.emit(ctx, LevelWarn, EventAlert, "proactive alert delivered",
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
	l.emit(ctx, LevelError, EventError, "error",
		slog.String("op", op),
		slog.String("error", err.Error()),
	)
}
