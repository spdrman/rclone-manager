package obs

import (
	"os"
	"strings"
)

// LevelFromEnv is how a deployment turns this sink up without a flag, a
// config key or a restart argument nobody can remember over a phone call:
// LOG_LEVEL=debug, or BACKUPD_DEBUG=1 as the shortcut.
//
// It exists here, rather than at the one or two places that build a
// Logger, because issue #730's whole difficulty was that the two
// containers of one deployment answered "how loud am I" differently. The
// UI host read the environment (apps/common/webhost's own envLogLevel)
// and the engine hard-coded LevelInfo, so an operator who set LOG_LEVEL
// on both got the proxy's trace and none of the engine events the trace
// was supposed to be joined to.
//
// The two readers are deliberately not one shared helper: apps/ may
// import core/, never the reverse, and core/internal is unreachable from
// apps/ by construction. What keeps them honest is that they read the
// same variables with the same precedence and the same fallback,
// each covered by its own package's test, and that this comment and
// webhost's own say so.
//
// BACKUPD_DEBUG wins over LOG_LEVEL because it is the shortcut an
// operator is told to set, and an unparseable LOG_LEVEL falls back to
// LevelInfo rather than refusing to start: a typo in a diagnostic knob
// must never take a backup host down.
//
// RM_DEBUG is the same shortcut under this project's old name
// (rclone-manager, issue #794) and is DEPRECATED: it is still honoured
// so an upgrade does not silently turn a diagnosing operator's logs
// back off, and it will be dropped a release after BACKUPD_DEBUG. The
// two are OR'd rather than ranked because neither has ever had an "off"
// value - only the documented 1 means anything, so a deployment that
// sets both, or that sets the old one on one container and the new one
// on the other, gets debug either way. BACKUPD_DEBUG is the spelling
// docs/deployment.md and the compose files name.
func LevelFromEnv() Level {
	if debugShortcut() {
		return LevelDebug
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		return LevelDebug
	case "warn":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// debugShortcut reports whether the one-variable debug shortcut is set,
// under its own name or under the deprecated RM_DEBUG alias. Only the
// documented "1" counts, under either name: a knob whose typos mean
// something is a knob that surprises the operator reading it back.
func debugShortcut() bool {
	return os.Getenv("BACKUPD_DEBUG") == "1" || os.Getenv("RM_DEBUG") == "1"
}
