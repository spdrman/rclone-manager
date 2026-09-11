package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spdrman/backupd/core/apicontract"
	"github.com/spdrman/backupd/core/cliecho"
	"github.com/spdrman/backupd/core/internal/config"
	"github.com/spdrman/backupd/core/internal/state"
	"github.com/spdrman/backupd/core/service"
)

// cmdActivity is `backupd activity`: the durable lifecycle feed,
// newest first, which is the same append-only transition log the Web UI's
// Activity page draws (issue #598).
//
// # Why this verb did not exist, and why that mattered
//
// Nothing here read the feed at all. `status` is the closest thing an
// operator had and it reports per-set health, which says nothing about
// what happened. So the one screen that tells somebody what the manager
// has been DOING could not be scripted, could not be pasted into a support
// conversation, and could not be checked by a cron job, and when that
// screen failed on a real NAS there was no second way to look at the same
// rows. EPIC G's parity rule says a capability reachable only from the
// browser is a capability nobody can automate; this is the instance of
// that rule the epic was named after.
//
// # Where the answer comes from
//
// Beside a live engine, from the engine: this is the one read here that
// RENDERS the wire rather than comparing against it. readmode.go's doc
// gives the reason the other four do not, and it is a gap this feed does
// not have. `sources` prints a remote type the schema has no field for,
// `artifacts` prints the quarantine sentence, `status` prints ages rather
// than timestamps and `retention` prints sibling collisions; every column
// below is a field of apicontract.ActivityEvent, so re-rendering from the
// wire loses nothing.
//
// A set comparison would also be actively wrong here, which is worth
// saying because every other read in this package does one. The log grows
// while the two reads happen, so an engine's newest N and this process's
// newest N legitimately differ at the top on any deployment that is
// working, and a check built on that would refuse healthy deployments.
//
// With nothing serving, from this host's own journal, through the same
// service.ListActivity the route is built on, so the clamping rules and
// the projection are one implementation rather than two.
func cmdActivity(args []string) int {
	fs, cfgPath := newFlagSet("activity")
	setFlag := fs.String("backup-set", "", "only events for this backup set, named <source/backup-set>")
	severityFlag := fs.String("severity", "", "only events at this severity or above: warn or error")
	limitFlag := fs.Int("limit", 0, "print at most this many matching events (default 200, maximum 1000)")
	jsonFlag := fs.Bool("json", false, "emit the wire objects unchanged, so a script parses the contract rather than this table")
	followFlag := fs.Bool("follow", false, "stream the LIVE feed instead of the durable log, until interrupted; needs a route to the serving process")
	scopeFlag := fs.String("scope", "", "with --follow, narrow the live feed to the deployment's own log: the lines that name no backup set. Takes deployment, or is left out for everything")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Both of these are wrong on every deployment rather than on this one,
	// so they are settled on the command line before anything is opened,
	// which is what earns them a 2 rather than a 1 (the exit-code table in
	// usage()). Whether the named set EXISTS is a question about this
	// deployment and is deliberately not asked: a filter that matches
	// nothing prints nothing, the same as the page's own set filter.
	minSeverity, ok := severityRank(*severityFlag)
	if !ok {
		return usageError("activity: --severity %q is not a severity; it takes warn or error, or is left out for everything", *severityFlag)
	}
	if *setFlag != "" {
		if _, _, isID := splitBackupSetID(*setFlag); !isID {
			return usageError("activity: --backup-set %q is not a backup set id; a backup set id is exactly source/name", *setFlag)
		}
	}
	if *limitFlag < 0 {
		return usageError("activity: --limit %d is not a count", *limitFlag)
	}
	// --scope is three ways wrong on the command line and none of them
	// needs this deployment read to be seen, so all three are settled
	// here, next to --severity, for the reason above.
	//
	// The pair of --backup-set and --scope deployment is the one that
	// could have been let through: the engine takes both and answers
	// about the set, because a named set is the narrower question. A
	// command line is where an operator can be told that instead of
	// being quietly handed the other half of what they typed, and this
	// feed's whole argument is that a surface must not be silent about
	// what it left out.
	if *scopeFlag != "" {
		if *scopeFlag != service.LiveActivityScopeDeployment {
			return usageError("activity: --scope %q is not a scope; it takes %s, which is the log that names no backup set, or is left out for everything", *scopeFlag, service.LiveActivityScopeDeployment)
		}
		if !*followFlag {
			return usageError("activity: --scope %s is a live-feed flag and this read has no --follow; the durable log is one table of transitions and has no deployment bucket to narrow to", service.LiveActivityScopeDeployment)
		}
		if *setFlag != "" {
			return usageError("activity: --scope %s and --backup-set %s ask two different questions, so pick one; the deployment's log is the lines that name no backup set, and %s's feed is that set's own", service.LiveActivityScopeDeployment, *setFlag, *setFlag)
		}
	}

	ctx := context.Background()
	cfg, journal, releaseJournal, err := service.OpenConfigAndJournal(ctx, *cfgPath)
	if err != nil {
		return fail(err)
	}
	defer func() {
		if err := journal.Close(); err != nil {
			fmt.Fprintf(os.Stderr, cliecho.Binary+": closing state database: %v\n", err)
		}
		// Only after the journal handle is closed, for the reason
		// openService's own cleanup gives.
		if err := releaseJournal(); err != nil {
			fmt.Fprintf(os.Stderr, cliecho.Binary+": releasing the state database lock: %v\n", err)
		}
	}()

	mode, err := enterReadMode(ctx, *cfgPath, cfg, os.Stderr)
	if err != nil {
		return fail(err)
	}

	if *followFlag {
		// Its own context, and its own signal handling: this is the one
		// invocation of this verb that does not end on its own. Ctrl-C or
		// a SIGTERM is how an operator ends it and is not a failure, which
		// is why followActivity returns nil on cancellation. The same
		// shape `run` and `daemon` already use.
		followCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := followActivity(followCtx, mode, followOptions{
			backupSetID: *setFlag,
			scope:       *scopeFlag,
			minSeverity: minSeverity,
			limit:       *limitFlag,
			asJSON:      *jsonFlag,
		}, os.Stdout, os.Stderr); err != nil {
			return fail(err)
		}
		return 0
	}

	events, err := readActivity(ctx, mode, cfg, journal, activityWindow(*limitFlag, *setFlag != "" || minSeverity > 0))
	if err != nil {
		return fail(err)
	}

	events = filterActivity(events, *setFlag, minSeverity)
	if *limitFlag > 0 && len(events) > *limitFlag {
		events = events[:*limitFlag]
	}

	if *jsonFlag {
		return printActivityJSON(events)
	}
	printActivity(events)
	return 0
}

// activityWindow is how many rows to READ, which is not the same number as
// how many to print.
//
// --limit is a count of MATCHING events, because that is what an operator
// asking for "the last ten things that went wrong on this set" means. If
// the two were the same number, `--backup-set X --limit 10` would print
// nothing on any busy deployment whose newest ten rows happen to belong to
// another set, which is a filter that looks broken and is not.
//
// So a filtered read takes the widest window the service allows and
// narrows it here; an unfiltered one reads exactly what it will print.
func activityWindow(limit int, filtered bool) int {
	if filtered {
		return service.MaxActivityLimit
	}
	return limit
}

// readActivity puts the question to whichever world this invocation is
// reporting on. See cmdActivity's own doc for why the engine-attached half
// renders rather than compares.
func readActivity(ctx context.Context, mode readDecision, cfg *config.Config, journal *state.Journal, limit int) ([]apicontract.ActivityEvent, error) {
	if mode.attached() {
		answer, err := mode.client.ListActivity(ctx, limit)
		if err != nil {
			// The mode has already been announced as engine-attached, so
			// answering from the journal underneath that announcement
			// would make the announcement a lie. Same rule the four
			// comparisons in readagreement.go follow.
			return nil, mode.unanswered("what has happened on this deployment", err)
		}
		return answer.Events, nil
	}

	// No cursor: this command prints one window and exits, so there is no
	// second page for a cursor to name. `--limit` is the whole of what an
	// operator here asks for.
	events, _, err := service.New(cfg, journal, nil, nil).ListActivity(ctx, limit, "")
	if err != nil {
		return nil, err
	}
	out := make([]apicontract.ActivityEvent, 0, len(events))
	for _, e := range events {
		out = append(out, apicontract.ActivityEvent{
			ArtifactID:   e.ArtifactID,
			ArtifactName: e.ArtifactName,
			BackupSetID:  e.BackupSetID,
			SourceName:   e.SourceName,
			SetName:      e.SetName,
			From:         e.From,
			To:           e.To,
			OccurredAt:   formatActivityTime(e.OccurredAt),
			Detail:       e.Detail,
		})
	}
	return out, nil
}

// formatActivityTime is apps/common/webhost's own formatTime, so the two
// worlds put the same string in the same field: RFC3339Nano, and empty for
// the zero time rather than a year nobody recorded.
func formatActivityTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

// The severity a transition carries, which is presentation and not a
// property of the record.
//
// It is derived here rather than read off the wire for the reason
// apps/common/webhost's activityEventResponse gives at its own doc: which
// moves deserve attention, and what to call them, belongs to whoever is
// presenting, and baking it into the contract would freeze one client's
// editorial judgement for every other one. This table is deliberately the
// same one ui/shared/src/api/client.ts derives, so the terminal and the
// page agree about which rows an operator gets when they ask for errors.
//
// Ranks rather than names, and info and ok share a rank, because they are
// the same thing to somebody filtering: neither is a problem.
const (
	severityInfo = 0
	severityWarn = 1
	severityErr  = 2
)

var activitySeverityByState = map[string]int{
	"DISCOVERED":       severityInfo,
	"TRANSFERRING":     severityInfo,
	"TRANSFERRED":      severityInfo,
	"VERIFIED":         severityInfo,
	"COMMITTED":        severityInfo,
	"COMPLETE":         severityInfo,
	"FAILED":           severityWarn,
	"QUARANTINED":      severityErr,
	"QUARANTINED_LOST": severityErr,
}

// activitySeverityByEdge is issue #625's other half of #663: two rows a
// successful in-place recovery writes are keyed on their destination alone
// by the table above, which captions them as if that destination were a
// verdict rather than a waypoint. completeIngestionInPlace's own re-stamp
// is a FAILED -> FAILED transition (an amber "attempt failed" under the
// state-only table, for an attempt that just succeeded), and
// ReinstateQuarantined's mandatory hold is FAILED -> QUARANTINED (a red
// "quarantined for review", for a hold nobody reviews because the
// judgement was already made a moment later in the same call). Both are
// waypoints on the way back to a durable state, not destinations, so both
// rank no higher than severityInfo here — the same rank ui/shared/src/api/
// client.ts's mirror of this table gives them, for the same reason: #625
// was filed the last time this file and that one drifted, and the two
// entries below are the two this fix and its TS counterpart agreed on
// before either was written (issue #663).
//
// Checked before activitySeverityByState: an edge named here overrides the
// destination-only guess, and every edge not named here falls through to
// it unchanged. lifecycle.Transition's own doc says the ORIGIN decides
// what a QUARANTINED destination means; this map is the presentation
// layer's application of that same rule. COMMITTED -> QUARANTINED and
// REMOTE_RETAINED -> QUARANTINED are deliberately absent: that is the
// `validate` origin, a true statement about a broken record, and it keeps
// the red badge the state-only table already gives it.
var activitySeverityByEdge = map[[2]string]int{
	{"FAILED", "FAILED"}:      severityInfo, // durable copy matched the remote object
	{"FAILED", "QUARANTINED"}: severityInfo, // held for the reinstatement judgement
}

// activitySeverity is the severity filterActivity and the page both key
// on: the edge if activitySeverityByEdge names it, else the destination
// alone, else severityInfo for a transition this build has never heard of
// (filterActivity's own doc gives the reason for that last fallback).
func activitySeverity(e apicontract.ActivityEvent) int {
	if v, ok := activitySeverityByEdge[[2]string{e.From, e.To}]; ok {
		return v
	}
	return activitySeverityByState[e.To]
}

// severityRank reads the --severity flag. An unrecognised value is
// refused rather than treated as "everything": a filter that silently did
// nothing would print a full feed under a flag that says it is narrowed,
// which is the worst of the three possible answers.
func severityRank(flag string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(flag)) {
	case "":
		return 0, true
	case "warn", "warning":
		return severityWarn, true
	case "error", "err":
		return severityErr, true
	}
	return 0, false
}

// filterActivity applies the two filters the page offers and no others.
// A state this build has never heard of ranks as info, which keeps it in
// an unfiltered feed and out of a narrowed one: an unexplained gap in an
// audit trail is worse than a row nobody has a caption for.
func filterActivity(events []apicontract.ActivityEvent, setID string, minSeverity int) []apicontract.ActivityEvent {
	if setID == "" && minSeverity == 0 {
		return events
	}
	out := make([]apicontract.ActivityEvent, 0, len(events))
	for _, e := range events {
		if setID != "" && e.BackupSetID != setID {
			continue
		}
		if activitySeverity(e) < minSeverity {
			continue
		}
		out = append(out, e)
	}
	return out
}

// printActivity is the plain form: one line per event, the same three
// columns the page shows, plus what the writer recorded about the move.
func printActivity(events []apicontract.ActivityEvent) {
	for _, e := range events {
		// 21 is REMOTE_DELETE_PENDING, the longest state
		// internal/lifecycle has. A narrower column does not truncate, it
		// pushes every later column right on that one row, which is worse
		// than a wide column on every other row.
		fmt.Printf("%s  %-21s %-28s %-28s %s\n",
			activityClock(e.OccurredAt), e.To, e.BackupSetID, e.ArtifactName, activityDetail(e))
	}
}

// activityClock renders occurred_at as an operator reads a clock, in this
// host's own zone. The wire keeps RFC3339Nano and --json emits it
// untouched; this column is for a human scanning a terminal, and a
// nanosecond offset is not.
func activityClock(occurredAt string) string {
	t, err := time.Parse(time.RFC3339Nano, occurredAt)
	if err != nil {
		// A stored timestamp this build cannot parse is still a row that
		// happened, and dropping the row would be an unexplained gap.
		return occurredAt
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// activityDetail is what the writer recorded about this move, falling back
// to the move itself. The same fallback ui/shared derives, so the terminal
// and the page caption a plain pipeline step the same way.
func activityDetail(e apicontract.ActivityEvent) string {
	if e.Detail != "" {
		return e.Detail
	}
	if e.From != "" {
		return e.From + " to " + e.To
	}
	return e.To
}

// printActivityJSON emits the wire objects unchanged, under the same
// `events` key the route uses, so a script parses the contract rather than
// this file's column layout.
func printActivityJSON(events []apicontract.ActivityEvent) int {
	// Never null: a caller iterating the array should not have to guard
	// for absence, which is the same rule the route's own handler follows.
	if events == nil {
		events = []apicontract.ActivityEvent{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(apicontract.ListActivityResponse{Events: events}); err != nil {
		return fail(fmt.Errorf("writing the activity feed: %w", err))
	}
	return 0
}
