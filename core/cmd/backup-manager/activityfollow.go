package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/apicontract"
)

// `rbm activity --follow`: the live feed, in a terminal.
//
// # Two feeds, and why this is not a mode of the other one
//
// `activity` without --follow reads the durable transition log: what
// HAPPENED, queryable after the fact, surviving every restart. This reads
// the serving process's own bounded in-memory event stream: what is
// happening NOW, at the level the engine logs it, and it exists only for
// as long as that process does.
//
// They answer different questions, and the difference is why --follow
// refuses when there is no route to the engine instead of quietly reading
// the journal. A fallback would put the wrong feed on screen under the
// flag that asked for the other one, which is this repository's own
// recurring defect: two surfaces telling an operator different things
// while both look right.
//
// # The cursor, and the one thing that invalidates it
//
// Each reading carries an epoch naming the process that answered. When it
// changes, the process restarted, and both the cursor and everything
// already printed belong to something that is gone: the sequence numbers
// start again, so a cursor carried across would silently skip every line
// the new process has emitted so far. So the epoch is checked before the
// events are read, the cursor is dropped, and it is said out loud on
// stderr rather than leaving a gap nobody can see. Issue #573 shipped
// without this once, and what it looked like was a terminal showing a
// dead process's log for ever.
//
// # Polling, not streaming
//
// The contract's own decision, and this follows it rather than reaching
// for a held-open connection: a NAS sits behind whatever reverse proxy the
// operator already had, and behind this product's own UI container proxy,
// so a long-lived response is at the mercy of every buffering and
// idle-timeout default in that path. The engine says how long to wait
// before asking again and this honours it.

// followOptions is what the command line asked for, resolved.
type followOptions struct {
	backupSetID string
	// scope narrows the reading to the deployment's own log, and is the
	// one reading a backup set id cannot ask for: naming no set already
	// means "every set" (#593). It carries
	// service.LiveActivityScopeDeployment rather than a spelling of its
	// own, because the value goes on the wire and the route compares it
	// against that constant. Empty means the whole feed.
	scope       string
	minSeverity int
	limit       int
	asJSON      bool
}

// defaultFollowInterval is the wait between readings when the engine did
// not say. It never should not say, but a zero here would spin.
const defaultFollowInterval = time.Second

// followActivity streams the live feed to out until ctx is cancelled.
//
// It returns nil on cancellation, because a follow that an operator ended
// with Ctrl-C did what it was asked. Every other return is a real failure
// and the caller turns it into a non-zero exit.
//
// Taking a context and two writers rather than reading os.Stdout is what
// makes this testable at all: a loop that only ends on a signal cannot be
// driven from a test, and a test that could not drive it would be a test
// of the flag parsing in front of it.
func followActivity(ctx context.Context, mode readDecision, opts followOptions, out, errOut io.Writer) error {
	if !mode.attached() {
		return fmt.Errorf("--follow needs a route to the process serving this deployment and this command has none, so there is nothing to follow: the live feed is that process's own in-memory event stream and no file holds it. `activity` without --follow reads the durable transition log instead, which answers from this host's own journal and says so")
	}

	var (
		since int64
		epoch string
	)
	for {
		reading, err := mode.client.LiveActivity(ctx, opts.backupSetID, opts.scope, since, opts.limit)
		if err != nil {
			// A cancelled context surfaces here as a failed request, and
			// it is not one: the operator ended the follow.
			if ctx.Err() != nil {
				return nil
			}
			return mode.unanswered("what is happening right now", err)
		}

		switch {
		case epoch == "":
			epoch = reading.Epoch
		case reading.Epoch != epoch:
			// Said on stderr, so it never lands in a piped feed, and said
			// before anything from the new process is printed.
			_, _ = fmt.Fprintf(errOut, "%sthe process serving this deployment restarted, so this feed starts again: the lines above came from a process that is gone, and its sequence numbers mean nothing to the one answering now\n", modeLinePrefix)
			epoch = reading.Epoch
			since = 0
			// The reading in hand was fetched with the OLD process's
			// cursor, which the new one cannot honour, so it is dropped
			// rather than printed.
			continue
		}

		printed := collectFollowEvents(reading, opts.minSeverity)
		for _, e := range printed {
			if opts.asJSON {
				if err := writeFollowJSON(out, e); err != nil {
					return err
				}
				continue
			}
			_, _ = fmt.Fprintln(out, formatFollowEvent(e))
		}
		if high := highestSequence(reading); high > since {
			since = high
		}

		wait := time.Duration(reading.PollAfterMs) * time.Millisecond
		if wait <= 0 {
			wait = defaultFollowInterval
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// collectFollowEvents flattens one reading into the lines to print, in the
// order the engine emitted them.
//
// A reading has two kinds of bucket and a follow prints both. Every
// configured set carries its own lines, and the deployment's bucket
// carries the ones that belong to no single set: a cycle starting, a
// capacity check, and every action somebody took through the API on a
// route that names no backup set (#593). Walking reading.Sets alone
// printed none of those, so a settings patch, a medium created, a
// credential imported and a gate refusal all reached the browser's
// terminal and never the CLI's. On a deployment with nothing configured
// it printed nothing at all, for ever, which is precisely the case the
// deployment bucket was added for: a new operator pressing buttons in a
// wizard with no set anywhere to read.
//
// The sort is what makes the two buckets one feed. The sequence is a
// per-process counter shared by every bucket, so ordering by it puts the
// lines back into the order they actually happened rather than printing
// one bucket and then the other, which would move the line before an
// error somewhere else on the screen.
//
// The dedup by sequence stays even though the engine no longer copies one
// event into more than one bucket. It costs nothing on a correct reading
// and it is what keeps an overlapping or repeated page harmless, which
// matters because asking again from where a truncated bucket stopped
// re-sends whatever a quieter bucket had already handed over.
func collectFollowEvents(reading apicontract.LiveActivityResponse, minSeverity int) []apicontract.LiveActivityEvent {
	seen := make(map[int64]bool)
	out := make([]apicontract.LiveActivityEvent, 0, 16)
	take := func(events []apicontract.LiveActivityEvent) {
		for _, e := range events {
			if seen[e.Sequence] {
				continue
			}
			seen[e.Sequence] = true
			if followSeverityRank(e.Level) < minSeverity {
				continue
			}
			out = append(out, e)
		}
	}
	for _, s := range reading.Sets {
		take(s.Events)
	}
	if reading.Deployment != nil {
		take(reading.Deployment.Events)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out
}

// highestSequence is the cursor for the next reading, taken from what the
// engine says it HOLDS rather than from what was printed.
//
// Those differ whenever a filter is on, and taking the printed one would
// re-request every filtered-out line for ever: `--severity error` on a
// healthy deployment would ask for the same window on every tick and
// never advance.
//
// The deployment's bucket counts here for the same reason it is printed:
// on a deployment with no configured sets it is the only bucket there is,
// and a cursor built from reading.Sets alone would sit at zero for ever
// while every poll re-fetched the same window.
//
// A truncated bucket holds the cursor back. The engine answers a cursor
// with the OLDEST slice above it and says when the limit cut that slice
// short, which is an invitation to ask again from where it ended. A
// cursor that jumped to the highest sequence anywhere in the reading
// would step clean over the rest of that bucket: those lines are still
// held, still inside the engine's buffer, and would simply never be
// requested again, with nothing in any later response saying they were
// skipped. One busy set beside one quiet one is all it takes. So the
// cursor is capped at the last line each truncated bucket actually handed
// over, and the next poll picks the rest up.
func highestSequence(reading apicontract.LiveActivityResponse) int64 {
	var high int64
	// The lowest page boundary among the buckets that said they have more
	// to hand over, or 0 when none did.
	var ceiling int64
	consider := func(events []apicontract.LiveActivityEvent, latest int64, truncated bool) {
		if latest > high {
			high = latest
		}
		for _, e := range events {
			if e.Sequence > high {
				high = e.Sequence
			}
		}
		// A bucket that says it was cut short and then hands over nothing
		// names no page boundary, so there is nothing to hold back to.
		// The engine cannot produce that reading (it only truncates a
		// slice it has filled), and treating it as a boundary would stall
		// the cursor on the same window for ever.
		if !truncated || len(events) == 0 {
			return
		}
		// Oldest first on the wire, so the last one handed over is the
		// page boundary.
		if last := events[len(events)-1].Sequence; ceiling == 0 || last < ceiling {
			ceiling = last
		}
	}
	for _, s := range reading.Sets {
		consider(s.Events, s.LatestSequence, s.Truncated)
	}
	if reading.Deployment != nil {
		consider(reading.Deployment.Events, reading.Deployment.LatestSequence, reading.Deployment.Truncated)
	}
	if ceiling > 0 && ceiling < high {
		return ceiling
	}
	return high
}

// followSeverityRank maps the engine's own log level onto the same three
// ranks --severity uses for the durable feed, so one flag means one thing
// on both halves of this verb. Anything below a warning is a note.
func followSeverityRank(level string) int {
	switch strings.ToLower(level) {
	case "error":
		return severityErr
	case "warn", "warning":
		return severityWarn
	}
	return severityInfo
}

// formatFollowEvent is one line: when, how loudly, how it went, what
// happened, and the fields the emitter attached, in the order it attached
// them.
//
// The fields are printed rather than summarised because they are the whole
// content of most events (which artifact, which backup set, how many
// bytes), and they are already redacted on the way to the wire, so nothing
// here has to decide what is safe to show.
//
// The result gets a column of its own rather than being left among the
// fields, because it is the answer to the question somebody following a
// terminal is actually asking (issue #625), and because the two surfaces
// have to tell the same story: the browser's dock colours a line by this
// value, and a command line that buried it would be a second, worse
// account of the same moment. A line stating none gets spaces, so the
// event names still line up in their column and a completion stands out
// from the notes around it by having something in that space at all.
func formatFollowEvent(e apicontract.LiveActivityEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %-5s %-7s %-28s %s", followClock(e.At), strings.ToUpper(e.Level), e.Result, e.Event, e.Message)
	for _, f := range e.Fields {
		fmt.Fprintf(&b, " %s=%s", f.Key, f.Value)
	}
	return b.String()
}

// followClock renders the moment as an operator reads a clock, in this
// host's own zone, to the second. --json keeps the wire's own RFC 3339.
func followClock(at string) string {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return at
	}
	return t.Local().Format("15:04:05")
}

// writeFollowJSON emits one wire event per line, so a script tails this
// with a line reader rather than an incremental JSON parser. The object is
// apicontract.LiveActivityEvent unchanged: a wrapper naming the set would
// be a shape this contract does not declare, and the set-scoped events
// already carry their backup set as one of their own fields.
func writeFollowJSON(out io.Writer, e apicontract.LiveActivityEvent) error {
	encoded, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("writing the live feed: %w", err)
	}
	_, err = fmt.Fprintln(out, string(encoded))
	return err
}
