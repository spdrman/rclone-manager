package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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
// cycle counts as failed (issue #283): a systemic error (reconcile or
// discover exhausting its retry budget, a journal listing failing
// outright, or a shutdown mid-cycle), OR an artifact reconciliation could
// not reach a verdict on, OR any artifact the cycle walked ending in
// FAILED, QUARANTINED or QUARANTINED_LOST, OR a cycle that had work in
// front of it and got none of it through. Before this existed, each
// command checked only the systemic half, so a cycle where every artifact
// discovered fine and then failed verification exited 0 -- the exact bug
// this function exists to make structurally impossible to reintroduce in
// one of the two commands without the other.
//
// The last of those four is issue #361, and it is the one that needs
// stating carefully, because the obvious version of it is wrong. "Nothing
// was transferred" is not a failure: a backup set with nothing new
// waiting on the remote transfers nothing every poll interval for weeks
// at a time, and that is the product working. What is a failure is a
// cycle that had artifacts in front of it and moved none of them, whether
// they were refused at discovery or refused at transfer. app.CycleProgress
// is the count that tells those two apart; see its doc for what it
// deliberately does not count, which is the other half of not turning a
// quiet night into an alarm.
//
// It takes one app.CycleVerdict rather than a list of arguments each
// command assembles for itself. That is not tidiness: #283 introduced
// this function to stop the two commands disagreeing, and #361 found them
// disagreeing anyway, because each was still building its own arguments
// at the call site. The verdict is built in internal/app now, from the
// same fields, for both.
//
// failedArtifacts (internal/app.processArtifacts) already folds in a
// loss this cycle's own reconcile pass discovered on its own -- a
// previously-durable artifact whose local copy turned out corrupted or
// missing, moved to QUARANTINED or QUARANTINED_LOST -- not just a
// this-cycle transfer/verify/commit failure: a successful reconciliation
// pass that finds rot is not a systemic error, but it is a stronger case
// for a non-zero exit than a single artifact this cycle's own pipeline
// quarantined, and this function must not let that distinction matter.
func cycleFailed(v app.CycleVerdict) bool {
	return v.Systemic || v.ReconcileErrors > 0 || v.FailedArtifacts > 0 || v.NothingGotThrough()
}

// cycleExit turns one cycle's per-backup-set verdicts into the exit
// status `run` and `fetch` both return, and prints the reason for any
// non-zero one it can name. Both commands go through this single
// function so neither can grow its own idea of what a failed cycle is.
//
// Callers pass os.Stderr, deliberately. This binary's stdout is FR-23's
// newline-delimited JSON event stream (logger, above, writes there), and
// a sentence in the middle of it would break every consumer that parses
// the stream a line at a time. `fetch` already prints its own human
// summary to stdout, which predates this and is not worth changing, but
// nothing new goes there.
func cycleExit(w io.Writer, verdicts ...app.CycleVerdict) int {
	code := 0
	for _, v := range verdicts {
		if !cycleFailed(v) {
			continue
		}
		code = 1
		if v.NothingGotThrough() {
			// Deliberately unchecked, like every other diagnostic this
			// binary prints: a write to stderr failing cannot change the
			// verdict that is being reported, and swallowing the verdict
			// because the terminal went away would be the worse answer.
			_, _ = fmt.Fprintf(w, "backup-manager: %s backed nothing up this cycle: %d walked, %d got through\n",
				v.Set, v.Progress.Walked, v.Progress.Durable)
		}
	}
	return code
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
