package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/service"
)

// defaultConfigPath matches container/compose.yaml's mount point and
// docs/deployment.md's documented layout. The packaged mount is the
// DIRECTORY /etc/backup-manager/config (issue #196) and config.yaml lives
// inside it; --config also accepts that directory, which
// config.ResolvePath turns into this same file.
const defaultConfigPath = "/etc/backup-manager/config/config.yaml"

// newFlagSet builds a flag.FlagSet every subcommand but `version` shares:
// a name (for its own usage/error output) and the one flag they all take,
// --config. flag.ContinueOnError, not the package default, is deliberate:
// it lets this command return its own exit code and still run every
// deferred cleanup between here and main, rather than flag.Parse calling
// os.Exit out from under a subcommand that has already opened a journal.
func newFlagSet(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to the manager's YAML config file, or to the directory holding it")
	return fs, cfgPath
}

// parseFlagsAroundOperands parses args into fs and returns the operands
// that were not flags, in the order they appeared, accepting flags on
// either side of them.
//
// flag.Parse on its own stops at the first argument that is not a flag,
// which makes `validate <artifact-id> --config <path>` two extra operands
// rather than one operand and one flag. Both of those forms are what this
// binary's own usage text describes (one line gives validate an operand,
// another says every command except version accepts --config, and neither
// puts them in an order), and command-then-subject-then-options is the
// order most CLIs take, so the parse accommodates the operator here
// instead of the message explaining the parser to them (issue #188).
//
// This is the ordinary repeated-Parse loop: parse, take the operand that
// stopped the parse, parse what is left, until nothing is left. An
// explicit "--" keeps its usual meaning, ending flag parsing for good, so
// an operand that looks like a flag can still be written after one.
func parseFlagsAroundOperands(fs *flag.FlagSet, args []string) ([]string, error) {
	var operands []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return operands, nil
		}
		// Whatever Parse consumed out of rest, which ends in "--" when it
		// stopped at an explicit terminator rather than at an operand.
		consumed := rest[:len(rest)-fs.NArg()]
		if len(consumed) > 0 && consumed[len(consumed)-1] == "--" {
			return append(operands, fs.Args()...), nil
		}
		operands = append(operands, fs.Arg(0))
		rest = fs.Args()[1:]
	}
}

// openService loads and validates configPath, opens its state journal
// (both via core/service.OpenConfigAndJournal — see that function's own
// doc for why this no longer reimplements that sequence itself), and
// builds an internal/app.Service ready for whichever use case the calling
// subcommand needs. withTransport controls whether the service is given a
// real transport.Transport (internal/transport/rclone.Adapter): every
// subcommand that can reach a remote (run, daemon, fetch, reconcile) needs
// one; every purely local one (check, status, sources, artifacts,
// retention, validate) does not, and leaves Service.Transport nil rather
// than pay for constructing an adapter it will never call.
//
// The returned cleanup func closes the journal; callers should always
// `defer cleanup()` immediately.
func openService(ctx context.Context, configPath string, withTransport bool) (*app.Service, *config.Config, func(), error) {
	cfg, journal, releaseJournal, err := service.OpenConfigAndJournal(ctx, configPath)
	if err != nil {
		return nil, nil, func() {}, err
	}
	cleanup := func() {
		if err := journal.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "backup-manager: closing state database: %v\n", err)
		}
		// Only after the journal handle is closed: the shared journal lock
		// is what keeps another process from migrating this journal while
		// this command still has it open (see core/service's startup.go).
		if err := releaseJournal(); err != nil {
			fmt.Fprintf(os.Stderr, "backup-manager: releasing the state database lock: %v\n", err)
		}
	}

	var tr transport.Transport
	if withTransport {
		tr = rclone.New()
	}

	svc := app.New(cfg, journal, tr, logger())
	return svc, cfg, cleanup, nil
}

// openBackupService is openService's counterpart for the handful of
// subcommands (today, just `settings`) whose use case lives on
// core/service.BackupService rather than internal/app.Service: anything
// that needs a file-backed configPath to persist a change to (see
// BackupService.configPath's own doc), which internal/app.Service, built
// directly from an already-loaded *config.Config, has no notion of at
// all. service.Open is the identical production constructor
// apps/common/webhost's Open uses, so a CLI-driven settings write goes
// through the exact same persist-then-hot-reload sequence
// (BackupService.UpdateSettings's own doc) an HTTP PATCH would.
//
// The returned cleanup func closes the journal (via BackupService.Close);
// callers should always `defer cleanup()` immediately.
func openBackupService(ctx context.Context, configPath string) (*service.BackupService, func(), error) {
	svc, closeFn, err := service.Open(ctx, configPath)
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() {
		if err := closeFn(); err != nil {
			fmt.Fprintf(os.Stderr, "backup-manager: closing state database: %v\n", err)
		}
	}
	return svc, cleanup, nil
}

// logger builds the FR-23 structured-observability sink every Service
// this binary constructs shares: newline-delimited JSON on stdout, so the
// process's own supervisor (systemd, a container runtime) owns rotation
// and shipping, exactly as internal/obs's package doc describes.
func logger() *obs.Logger {
	return obs.New(os.Stdout, obs.LevelInfo)
}

// logStartup emits FR-23's two mandatory startup log lines (binary
// version/commit/Go version, and the embedded rclone version) once, right
// after a Service exists. Every subcommand that can mutate anything
// (run, daemon, fetch, reconcile, validate) calls this; the purely
// read-only ones (status, sources, artifacts, retention, check) do not,
// since FR-23's "startup" event is about a processing run starting, not
// about a query being answered.
func logStartup(ctx context.Context, l *obs.Logger, info app.VersionInfo) {
	l.Startup(ctx, info.BinaryVersion, info.Commit, info.GoVersion)
	l.RcloneVersion(ctx, info.RcloneVersion)
}

// cycleFailed is the one place `run` and `fetch` both decide whether a
// cycle counts as failed (issue #283, extended by issue #361). It takes an
// internal/app.CycleOutcome, which both commands build from the same
// method on their own result type, so neither can decide from evidence the
// other does not have.
//
// The decision itself lives on CycleOutcome.Failed, next to the fields it
// weighs and next to the full reasoning for each of its three clauses: a
// systemic failure, an artifact left in a failure state, or a cycle that
// had artifacts to move and moved none of them to safety. Read that doc
// before changing anything here; the third clause in particular is
// carefully bounded so an idle poll cycle stays a success.
//
// What stays here is the shape of the seam, which is the part issue #283
// asked for and issue #361's fourth acceptance criterion asks for again:
// one function, called by both commands, that neither may work around.
func cycleFailed(outcome app.CycleOutcome) bool {
	return outcome.Failed()
}

// reportCycleFailure prints why a cycle is being called failed, so the
// reason is not encoded in the exit status alone (issue #361's fifth
// acceptance criterion). It names the backup set, how many artifacts the
// cycle walked and how many got through, from the same numbers the exit
// status was computed from.
//
// It writes to stderr. This binary's stdout is the FR-23 newline-delimited
// JSON event stream (see logger above), and a sentence in the middle of it
// would break every consumer that parses it.
func reportCycleFailure(outcome app.CycleOutcome) {
	if outcome.NothingGotThrough() {
		fmt.Fprintln(os.Stderr, "backup-manager: this cycle backed nothing up:", outcome.Summary())
		return
	}
	fmt.Fprintln(os.Stderr, "backup-manager: this cycle did not complete cleanly:", outcome.Summary())
}

// fail prints err to stderr in a consistent shape and returns the exit
// code every subcommand's own failure path returns.
func fail(err error) int {
	fmt.Fprintln(os.Stderr, "backup-manager:", err)
	return 1
}

// usageError prints a usage complaint straight to stderr (flag.Parse
// already prints its own error under flag.ContinueOnError, so this is for
// the errors this package's own subcommands catch afterward, like a
// missing required flag or a wrong argument count) and returns the
// argument-error exit code.
func usageError(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "backup-manager: "+format+"\n", args...)
	return 2
}
