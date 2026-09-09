// Package cliname holds the one spelling of the command an operator types.
//
// # Why a package for two strings
//
// The name was a string literal in about fifty places: every diagnostic
// that prefixes itself with it, the usage block's first line, every
// sentence that tells an operator which command to run next, every command
// core/cliecho echoes into the Web UI's terminal panel, and the filename
// core/tests/compat builds the binary under before it pins what that
// binary prints. None of those knew about each other.
//
// That is survivable while the name never moves, and it stops being
// survivable the moment it does. Renaming the command then means reading
// fifty string literals and deciding, one at a time, whether each is the
// CLI or something that merely looks like it. Three shapes in this tree
// spell "backup-manager" and are NOT this constant, and every one of them
// would be swept up by a careless search-and-replace:
//
//   - filesystem paths (/etc/backup-manager/config, /var/lib/backup-manager)
//     which packaging mounts and an operator's existing deployment already
//     has on disk;
//   - the project, the image and the compose service (ghcr.io/spdrman/
//     backup-manager, the "backup-manager" service in container/
//     compose.yaml, "Backup Manager" as the product's name);
//   - wire identity that a log or an audit trail may already be matched
//     on, which is the User-Agent core/internal/apiclient sends.
//
// So this is not a tidy-up. It is the line between "the command" and
// "everything else called that", drawn once, in a place a reader can see
// it, so a rename is this file and nothing else.
//
// # Why a constant and not something read at runtime
//
// What this binary calls itself is a fact about the build, not about how
// somebody reached it. main() hands os.Args[1:] to run() and dispatches on
// the first word of that, so the filename an operator happened to type
// never enters a decision, and taking the printed name from argv[0]
// instead would make two shells on the same host print two different
// spellings of the same command.
package cliname

const (
	// Binary is the command an operator types, and the name this build
	// prints when it names itself: the prefix on a diagnostic, the
	// "usage:" line, and every sentence that says which command to run
	// next.
	Binary = "backup-manager"

	// WebBinary is the other command in the image, the one that serves the
	// Web UI. It is derived rather than spelled so that the two names
	// cannot drift apart, which is the whole reason this file exists.
	//
	// The package directory is core/cmd/backup-manager and the web one is
	// apps/generic/cmd/backup-manager-web. A Go package path is not
	// operator-visible, so neither follows this constant.
	WebBinary = Binary + "-web"
)
