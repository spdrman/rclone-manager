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

	level, message := apiActionLevelAndMessage(action)
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
	if action.BackupSetID != "" {
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

	b.logger.Event(ctx, level, obs.EventAPIAction, message, attrs...)
}

// apiActionLevelAndMessage decides how loudly the line is emitted and what
// it says.
//
// The level is the status, because that is what the emitter knows: a
// refusal an operator caused (a 4xx) is a warning, a failure this process
// caused (a 5xx) is an error, and everything else is a note. Nothing here
// composes the operator-facing sentence beyond a plain summary; what a
// moment is worth calling belongs to whichever client is presenting it,
// which is the same argument liveactivity.go already makes for every other
// event on this feed.
func apiActionLevelAndMessage(action cliecho.APIAction) (obs.Level, string) {
	verb := strings.ToLower(action.Method)
	switch {
	case action.Status >= 500:
		return obs.LevelError, verb + " " + action.Route + " failed: " + statusWords(action)
	case action.Status >= 400:
		return obs.LevelWarn, verb + " " + action.Route + " refused: " + statusWords(action)
	default:
		return obs.LevelInfo, verb + " " + action.Route
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
