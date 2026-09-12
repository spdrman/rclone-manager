package obs

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// Issue #730. The engine built its sink at LevelInfo whatever the
// environment said, so the one deployment that could not be reproduced
// was also the one that could not be turned up: an operator setting
// LOG_LEVEL=debug on both containers got the UI host's proxy trace and
// nothing from the engine to match it against.
//
// The cases below are the precedence and the fallback, because those are
// the two things a second reader of the same variables can get subtly
// wrong (apps/common/webhost has one, and cannot import this one - see
// LevelFromEnv's own doc), plus the deprecated RM_DEBUG alias (issue
// #794): the rename must not turn a diagnosing operator's logs back off
// on upgrade.

func TestLevelFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name         string
		backupdDebug string
		rmDebug      string
		logLevel     string
		want         Level
	}{
		{name: "nothing set is unchanged INFO", want: LevelInfo},
		{name: "LOG_LEVEL selects a level", logLevel: "debug", want: LevelDebug},
		{name: "LOG_LEVEL is case and space insensitive", logLevel: " WARN ", want: LevelWarn},
		{name: "LOG_LEVEL error", logLevel: "error", want: LevelError},
		// The shortcut an operator is told to set wins, so a deployment
		// that already had LOG_LEVEL=warn in its compose file still gets
		// diagnostics from one extra variable.
		{name: "BACKUPD_DEBUG raises the level to debug", backupdDebug: "1", want: LevelDebug},
		{name: "BACKUPD_DEBUG wins over LOG_LEVEL", backupdDebug: "1", logLevel: "warn", want: LevelDebug},
		// A typo in a diagnostic knob must never change how loud a backup
		// host is in some unpredictable direction, and must never stop it.
		{name: "an unparseable LOG_LEVEL falls back to INFO", logLevel: "verbose", want: LevelInfo},
		{name: "BACKUPD_DEBUG only counts as the documented 1", backupdDebug: "true", want: LevelInfo},
		// Issue #794 renamed the knob. A deployment upgraded without its
		// compose file being touched still has the old spelling, and an
		// operator mid-diagnosis must not have their logs silently go
		// quiet under them.
		{name: "the deprecated RM_DEBUG alias still works", rmDebug: "1", want: LevelDebug},
		{name: "the deprecated RM_DEBUG alias still wins over LOG_LEVEL", rmDebug: "1", logLevel: "warn", want: LevelDebug},
		{name: "the deprecated alias only counts as the documented 1", rmDebug: "true", want: LevelInfo},
		// One container upgraded before the other, or a half-edited
		// compose file: either spelling alone is enough, so the two
		// containers of one deployment cannot disagree about how loud
		// they are (the #730 failure this knob exists for).
		{name: "either spelling alone is enough", backupdDebug: "true", rmDebug: "1", want: LevelDebug},
		{name: "both spellings set is still debug", backupdDebug: "1", rmDebug: "1", want: LevelDebug},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BACKUPD_DEBUG", tc.backupdDebug)
			t.Setenv("RM_DEBUG", tc.rmDebug)
			t.Setenv("LOG_LEVEL", tc.logLevel)

			if got := LevelFromEnv(); got != tc.want {
				t.Errorf("LevelFromEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLevelFromEnvActuallyReachesTheSink is the half that matters to a
// deployment: a level nothing is built with is a level nobody gets. It
// goes through New rather than asserting on the constant, so a Logger
// wired at this level really does write the debug line an operator turned
// it on for.
func TestLevelFromEnvActuallyReachesTheSink(t *testing.T) {
	t.Setenv("BACKUPD_DEBUG", "1")
	t.Setenv("RM_DEBUG", "")

	var buf bytes.Buffer
	New(&buf, LevelFromEnv()).emit(context.Background(), slog.LevelDebug, "diagnostic", "a debug line")

	if !strings.Contains(buf.String(), "a debug line") {
		t.Errorf("a sink built at the environment's level dropped its debug line; wrote %q", buf.String())
	}
}
