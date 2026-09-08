package service

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/spdrman/rclone-manager/core/cliecho"
	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// What somebody did in the Web UI, recorded where an operator can read it
// (issue #599).
//
// # The gap this closes
//
// `grep RecordEvent apps/common/webhost` used to find nothing. Every step
// of a cycle has emitted a structured event since FR-23, and nothing the
// Web UI did emitted anything: a connection test returned a sentence to
// one caller and vanished, a settings patch left no trace, a refusal
// reached a dialog that closed. So a set's strip and the Activity page
// could tell an operator everything about work the engine started for
// itself, and nothing at all about work they had just asked for.
//
// # Why it goes through obs rather than being composed in the browser
//
// Three things follow from it, and none of them would if the browser
// wrote the line.
//
// It reaches the live feed, because liveactivity.go is wired in as this
// logger's obs.Sink, so an action lands in the same bounded tail as the
// cycle's own events, with a sequence number from the same counter. That
// is what lets a terminal interleave "somebody patched this set" with
// "and then the cycle failed" in the order they happened.
//
// It reaches the log, so an action survives the process that served it,
// and a support conversation about yesterday has something to read.
//
// And it goes through the redaction that already runs on this path
// (obs/redact.go, obs/secret.go), which a string composed in a browser
// would not. This panel is exportable and copy-to-clipboard by design, so
// that is not a nicety.
//
// # Why the actor is a field rather than a prefix
//
// Two people administering one NAS have to be able to tell each other
// apart: one of them restarting a cycle is something the other needs to
// see as somebody else's doing. A line that said "you" to whoever was
// reading it would tell them the opposite. So the actor is on the event,
// and the client decides how to render it (its own session's name as
// "you", anybody else's as the account name).

// RecordAPIAction records one action taken through the /api/v1 surface.
//
// It satisfies apps/common/webhost.ActionRecorder. Nothing here can fail
// and nothing here returns an error: a line that could refuse to be
// written would be a reason a request failed, and describing an action
// must never be able to break the action.
func (b *BackupService) RecordAPIAction(ctx context.Context, action cliecho.APIAction) {
	if b == nil {
		return
	}

	outcome, message := apiActionOutcomeAndMessage(action)
	attrs := []slog.Attr{
		slog.String("actor", action.Actor),
		slog.String("route", action.Method+" /api/v1"+action.Route),
		slog.Int("status", action.Status),
	}
	// The set the action was about, under the field name attributeRecord
	// reads, so an action on one backup set lands on THAT set's feed and
	// an action on none lands on the deployment's. Absent rather than
	// empty: an empty backup_set is what attributeRecord already treats
	// as no set, and an empty field is one more thing for a terminal to
	// render and skip.
	//
	// Only when the running configuration actually names the set, and
	// that is not a formality. The id on this action comes from the
	// route's own {source} and {set} parameters (apps/common/webhost's
	// backupSetOfRoute), so it is whatever the request put in the URL,
	// checked by nothing: a PATCH at backup-sets/ghost-src/ghost-set
	// records an action naming a set that does not exist, and it does so
	// for a 404 and for a 403 refused before any handler ran. An action
	// on a set this deployment does not have is a deployment-scoped
	// line, which is where an operator watching somebody probe this API
	// wants to see it anyway, and it is also what stops those ids
	// reaching the live feed's bucket map (see liveactivity.go's
	// setLocked for what they did there).
	if action.BackupSetID != "" && b.configuresBackupSetID(action.BackupSetID) {
		attrs = append(attrs, slog.String("backup_set", action.BackupSetID))
	}
	if action.ErrorCode != "" {
		attrs = append(attrs, slog.String("error_code", action.ErrorCode))
	}
	if action.Message != "" {
		attrs = append(attrs, slog.String("detail", action.Message))
	}
	// Exactly one of these two, always. A terminal prints the command
	// when there is one and the marked gap line when there is not, and
	// never nothing: silence is the thing this feature exists to abolish.
	//
	// Both halves go out as data and neither as a rendered line. `command`
	// is the shell-quoted invocation with no "$ " in front of it, and the
	// gap is its wording plus its detail with no "# " in front of either;
	// the route is already on the line. A terminal draws those prefixes,
	// the same way it draws the timestamp and the [actor], so the dock and
	// `activity --follow` render the same fields the same way and a script
	// reading `activity --json` gets a command it can run rather than a
	// screen it has to parse. Whether that command is runnable as printed
	// is data as well, on command_runnable, rather than a sentence inside
	// the command.
	switch {
	case action.Command != "":
		attrs = append(attrs, slog.String("command", action.Command))
		if action.Placeholder {
			// The command carries a <value in angle brackets>. Saying so
			// as a field rather than only inside the rendered text means
			// a client can mark the line rather than parse it.
			attrs = append(attrs, slog.String("command_runnable", "false"))
		}
	default:
		attrs = append(attrs, slog.String("command_gap", action.Gap))
		if action.GapDetail != "" {
			attrs = append(attrs, slog.String("command_gap_detail", action.GapDetail))
		}
	}

	// Completed rather than Event, because this line IS a completion: the
	// request has been served by the time this recorder is reached, and
	// the status code is how it went (issue #625). It is deliberately
	// the unpaired half of obs's start-and-completion shape, since a
	// start line emitted here would be announcing something that had
	// already happened. Work an action KICKS OFF that takes real time,
	// a cycle most of all, is bracketed where that work runs.
	b.logger.Completed(ctx, outcome, obs.EventAPIAction, message, attrs...)
}

// apiActionOutcomeAndMessage decides how the action went and what the line
// says.
//
// The outcome is the status, because that is what the emitter knows: a
// refusal an operator caused (a 4xx) is a warning, a failure this process
// caused (a 5xx) is an error, and anything that was served is a success.
// That last one is the value there was no way to say before: every action
// somebody took in the Web UI and that WORKED arrived on the feed at info
// and read as a neutral note, indistinguishable from a line about nothing
// in particular, so the surface that exists to tell an operator their
// button did something told them only that something had been mentioned.
//
// The level follows from the outcome (obs.Outcome.Level) rather than being
// picked separately here, so the two can never tell different stories
// about one request. Nothing here composes the operator-facing sentence
// beyond a plain summary; what a moment is worth CALLING belongs to
// whichever client is presenting it, which is the same argument
// liveactivity.go already makes for every other event on this feed.
func apiActionOutcomeAndMessage(action cliecho.APIAction) (obs.Outcome, string) {
	verb := strings.ToLower(action.Method)
	switch {
	case action.Status >= 500:
		return obs.OutcomeError, verb + " " + action.Route + " failed: " + statusWords(action)
	case action.Status >= 400:
		return obs.OutcomeWarning, verb + " " + action.Route + " refused: " + statusWords(action)
	default:
		return obs.OutcomeSuccess, verb + " " + action.Route
	}
}

// statusWords is the refusal in as few words as are true: its code, or the
// status itself when whatever refused did not name one.
func statusWords(action cliecho.APIAction) string {
	if action.ErrorCode != "" {
		return action.ErrorCode
	}
	return strconv.Itoa(action.Status)
}
