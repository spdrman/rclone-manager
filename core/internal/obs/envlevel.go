package obs

import (
	"os"
	"strings"
)

// LevelFromEnv is how a deployment turns this sink up without a flag, a
// config key or a restart argument nobody can remember over a phone call:
// LOG_LEVEL=debug, or RM_DEBUG=1 as the shortcut.
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
// same two variables with the same precedence and the same fallback,
// each covered by its own package's test, and that this comment and
// webhost's own say so.
//
// RM_DEBUG wins over LOG_LEVEL because it is the shortcut an operator is
// told to set, and an unparseable LOG_LEVEL falls back to LevelInfo
// rather than refusing to start: a typo in a diagnostic knob must never
// take a backup host down.
func LevelFromEnv() Level {
	if os.Getenv("RM_DEBUG") == "1" {
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
