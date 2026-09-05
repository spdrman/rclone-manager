// Package metrics renders an already-computed internal/health.Report
// (FR-24) as Prometheus text exposition format.
//
// # Why this package exists, and why it stops here
//
// docs/adr/0002-phase-5-scope.md is the full reasoning; the short version:
// internal/health already computes every fact FR-24 asks for (process
// version info, and, per backup set, its health state, freshness, pending
// deletes, failures, quarantine counts, free space and every timestamp it
// tracks), and nothing renders any of it yet. This package is exactly one
// rendering, and only the mechanical, policy-free half of "metrics and
// alerts": turning already-computed values into a text format a scraper
// can read. It makes no decision about what counts as alert-worthy (that
// is health.State's job, already done, see State.OK()), invents no
// threshold, and has no opinion about delivery, a webhook, a pager, a
// dashboard query. Rendering exposition text needed none of those
// decisions to be made first, so this package makes none of them.
//
// # No wiring included on purpose
//
// Render takes a health.Report as a plain value and returns a string.
// Nothing in this package calls internal/health itself, holds a journal,
// or knows how a Report gets built. Nothing outside this package calls
// Render yet either: cmd/backup-manager has no subcommand to serve it from
// (issues #25, #26), the same position internal/health, internal/obs and
// internal/capacity are already in. Wiring this in later, a
// "backup-manager status --prometheus" flag, an HTTP handler, or both, is
// meant to be a few lines calling Render, not a redesign.
//
// # Format
//
// Output follows the Prometheus text exposition format, version 0.0.4
// (see ContentType): a "# HELP" and "# TYPE" line per metric name,
// followed by that metric's samples grouped together, one line each. Every
// metric name is prefixed backup_manager_ so it cannot collide with
// another exporter's metric on the same scrape target. A health.Report
// field the caller never populated (any of BackupSetInputs' three
// pointers) or one internal/health never had evidence for
// (NewestGoodBackupAge) omits that metric's sample for that backup set
// entirely, matching Prometheus's own convention that an unknown value has
// no series, rather than rendering a fabricated zero that would read as a
// real reading of zero.
package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/health"
)

// ContentType is the MIME type a caller should set on an HTTP response
// (or otherwise associate with Render's output), per the Prometheus text
// exposition format's own versioning scheme.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// namePrefix roots every metric name this package emits. It is the binary
// name, backup-manager, with the hyphen replaced by an underscore, since a
// Prometheus metric name may not contain a hyphen.
const namePrefix = "backup_manager_"

// healthStates lists FR-24's four backup-set states in the fixed order
// backup_set_state always renders them in, so sample order never depends
// on map iteration or on the order decideState happens to check them in.
var healthStates = []health.State{health.Healthy, health.Degraded, health.Stale, health.Failing}

// Render renders report as Prometheus text exposition format.
//
// Backup sets are sorted by their model.BackupSetID string form before
// rendering, so two calls against Reports holding the same data in a
// different slice order produce byte-identical output; nothing here
// depends on the order report.BackupSets happened to be built in.
func Render(report health.Report) string {
	sets := append([]health.BackupSetHealth(nil), report.BackupSets...)
	sort.Slice(sets, func(i, j int) bool {
		return sets[i].Set.String() < sets[j].Set.String()
	})

	var b strings.Builder

	writeProcessInfo(&b, report)
	writeGeneratedAt(&b, report)
	writeState(&b, sets)

	writeGauge(&b, sets, "newest_good_backup_age_seconds",
		"Age of the newest known-good (COMMITTED, REMOTE_DELETE_PENDING or COMPLETE) backup, in seconds.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.NewestGoodBackupAge == nil {
				return 0, false
			}
			return s.NewestGoodBackupAge.Seconds(), true
		})

	writeGauge(&b, sets, "stale_threshold_seconds",
		"Configured stale_after threshold for this backup set, in seconds.",
		func(s health.BackupSetHealth) (float64, bool) {
			return s.StaleThreshold.Seconds(), true
		})

	writeGauge(&b, sets, "pending_deletes",
		"Artifacts currently at REMOTE_DELETE_PENDING for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.PendingDeletes), true
		})

	writeGauge(&b, sets, "failures",
		"Artifacts currently FAILED for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.Failures), true
		})

	writeGauge(&b, sets, "quarantined",
		"Artifacts currently quarantined, recoverable or not, for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.QuarantinedCount), true
		})

	writeGauge(&b, sets, "quarantined_lost",
		"Artifacts currently QUARANTINED_LOST (irrecoverable) for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.QuarantinedLostCount), true
		})

	// Issue #227. A reinstated artifact never authorises deleting its
	// remote source again, so this number only ever grows, and a scrape is
	// the surface that answers "is it growing" over the months an operator
	// would actually notice it in. There is deliberately no companion
	// bytes metric: see health.BackupSetHealth's own field doc for why the
	// size of those preserved remote objects is not a fact this manager
	// has. Unlike free_bytes below, a zero here is a real reading rather
	// than a missing one, so the sample always renders.
	writeGauge(&b, sets, "reinstated_remote_retained",
		"Artifacts reinstated out of quarantine that still hold a remote source this manager will never delete. The bytes those remote objects occupy are deliberately not reported: this manager cannot see them.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.ReinstatedRemoteRetainedCount), true
		})

	writeGauge(&b, sets, "current_transfers",
		"Artifacts currently TRANSFERRING for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(len(s.CurrentTransfers)), true
		})

	writeGauge(&b, sets, "free_bytes",
		"Free space on this backup set's local destination filesystem, in bytes.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.FreeBytes == nil {
				return 0, false
			}
			return float64(*s.FreeBytes), true
		})

	writeGauge(&b, sets, "last_successful_poll_timestamp_seconds",
		"Unix time discovery last completed successfully for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.LastSuccessfulPollAt == nil {
				return 0, false
			}
			return float64(s.LastSuccessfulPollAt.Unix()), true
		})

	writeGauge(&b, sets, "last_completed_backup_timestamp_seconds",
		"Unix time of the newest artifact currently COMPLETE for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.LastCompletedBackupAt == nil {
				return 0, false
			}
			return float64(s.LastCompletedBackupAt.Unix()), true
		})

	// Issue #444, FR-24's placement half. These are the metrics that make
	// "the moves have been failing for a week" alertable, which is the
	// whole shape of the defect: the fact was visible for one pass, on a
	// terminal nobody was watching, and then gone.
	//
	// away_from_home is reported unconditionally because zero is the real
	// and common reading (every deployment whose artifacts are where they
	// belong, and every deployment that declares no medium at all). The
	// two ages beside it are not: an age only exists when there is
	// something to be the age of, and a zero would read as "this happened
	// just now", which is the opposite of missing.
	writeGauge(&b, sets, "away_from_home",
		"Artifacts whose durable copy is not on the storage medium this backup set's retention chain says it belongs on.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.Placement.AwayFromHome), true
		})

	writeGauge(&b, sets, "away_from_home_oldest_age_seconds",
		"How long the oldest away-from-home copy has existed on the medium it is sitting on, in seconds. An upper bound on how long it has been in the wrong place: nothing durable records when an artifact's home last changed.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Placement.OldestAwayFromHomeAge == nil {
				return 0, false
			}
			return s.Placement.OldestAwayFromHomeAge.Seconds(), true
		})

	writeGauge(&b, sets, "open_moves",
		"Relocations this backup set has open in the move journal, in any non-terminal phase.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.Placement.OpenMoves), true
		})

	writeGauge(&b, sets, "failed_moves",
		"Open relocations whose last attempt failed. This is the number that turns an otherwise-healthy backup set DEGRADED.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.Placement.FailedMoves), true
		})

	writeGauge(&b, sets, "failed_move_oldest_age_seconds",
		"How long the oldest failing relocation has been open, in seconds, measured from when this manager wrote the move down. This is the difference between a blip and a wedge, and it is the one to alert on.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Placement.OldestFailedMoveAge == nil {
				return 0, false
			}
			return s.Placement.OldestFailedMoveAge.Seconds(), true
		})

	writeGauge(&b, sets, "last_retention_run_timestamp_seconds",
		"Unix time GFS retention last ran for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.LastRetentionRunAt == nil {
				return 0, false
			}
			return float64(s.LastRetentionRunAt.Unix()), true
		})

	return b.String()
}

func writeProcessInfo(b *strings.Builder, report health.Report) {
	name := namePrefix + "process_info"
	writeHelp(b, name, "Build information for the running backup-manager process. Constant 1; the version data is in the labels.")
	writeType(b, name, "gauge")
	fmt.Fprintf(b, "%s{binary_version=%s,rclone_version=%s} 1\n",
		name, quoteLabel(report.Process.BinaryVersion), quoteLabel(report.Process.RcloneVersion))
}

func writeGeneratedAt(b *strings.Builder, report health.Report) {
	name := namePrefix + "report_generated_timestamp_seconds"
	writeHelp(b, name, "Unix time this health report was generated.")
	writeType(b, name, "gauge")
	fmt.Fprintf(b, "%s %d\n", name, report.GeneratedAt.Unix())
}

// writeState renders backup_set_state as the standard Prometheus "one-hot
// enum" pattern: one sample per (backup_set, state) pair, 1 for the state
// BackupSetHealth.State actually holds and 0 for the other three, rather
// than a single sample whose value is some arbitrary state-to-number
// mapping a reader would have to memorize.
func writeState(b *strings.Builder, sets []health.BackupSetHealth) {
	name := namePrefix + "backup_set_state"
	writeHelp(b, name, "Backup set health state (FR-24), as a one-hot indicator per state label: exactly one state label reads 1 for a given backup_set, the rest read 0.")
	writeType(b, name, "gauge")
	for _, s := range sets {
		for _, st := range healthStates {
			v := 0
			if s.State == st {
				v = 1
			}
			fmt.Fprintf(b, "%s{backup_set=%s,state=%s} %d\n",
				name, quoteLabel(s.Set.String()), quoteLabel(strings.ToLower(st.String())), v)
		}
	}
}

// writeGauge writes one metric family, name namePrefix+"backup_set_"+suffix,
// with one sample per backup set in sets for which value reports ok. A
// backup set for which value reports !ok contributes no sample at all: see
// the package doc for why an unknown value must never render as a
// fabricated zero.
func writeGauge(b *strings.Builder, sets []health.BackupSetHealth, suffix, help string, value func(health.BackupSetHealth) (float64, bool)) {
	name := namePrefix + "backup_set_" + suffix
	writeHelp(b, name, help)
	writeType(b, name, "gauge")
	for _, s := range sets {
		v, ok := value(s)
		if !ok {
			continue
		}
		fmt.Fprintf(b, "%s{backup_set=%s} %s\n", name, quoteLabel(s.Set.String()), formatFloat(v))
	}
}

func writeHelp(b *strings.Builder, name, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
}

func writeType(b *strings.Builder, name, typ string) {
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
}

// formatFloat renders v the way Prometheus's own exporters do: the
// shortest decimal representation that round-trips, no exponent, no
// trailing zeros. strconv.FormatFloat with precision -1 already guarantees
// round-tripping; 'f' rather than 'g' keeps it away from scientific
// notation, which the exposition format allows but no widely-used
// Prometheus client library actually emits for a plain gauge value.
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// quoteLabel renders v as a double-quoted Prometheus label value, escaping
// backslash, double quote and newline exactly as the text exposition
// format's own escaping rules require.
//
// Every label value this package actually emits comes from either a
// model.BackupSetID (whose own validation already forbids control
// characters, and "/" needs no escaping in this format) or a build-time
// version string, so none of this is expected to ever fire in practice.
// It is here anyway because Render's contract is "always valid exposition
// text for whatever health.Report it is handed", not "valid for every
// health.Report this repository's own validation happens to allow through
// today".
func quoteLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(v) + `"`
}
