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

// The plumbing every command in this binary shares: the flag set, the
// operand-tolerant parse, the two ways a service gets opened, the log sink,
// and the two functions that decide what a cycle's exit status is.
//
// It is one file because the point of each of these is that there is exactly
// one of it. Every command answering an unknown flag the same way, resolving
// --config the same way, and closing its journal and releasing its lock in
// the same order are all properties that only hold while nobody writes a
// second version, and the two exit-status functions are here specifically
// because `run` and `fetch` disagreed twice about what a failed cycle is,
// each time because each was still deciding for itself.
//
// The refusals and diagnostics printed from here are operator-visible and
// pinned by core/tests/compat, so their wording is part of the contract
// rather than part of the implementation.

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
// (both via core/service.OpenConfigAndJournal, see that function's own
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

// openBackupService is openService's counterpart for the subcommands
// whose use case lives on core/service.BackupService rather than
// internal/app.Service: anything that needs a file-backed configPath to
// persist a change to (see BackupService.configPath's own doc), which
// internal/app.Service, built directly from an already-loaded
// *config.Config, has no notion of at all. That is `settings`, every
// `backup-set` verb, and `restore`.
//
// It is not the only door any more, and which one a command comes through
// is the whole of what it can do beside a running engine. #543 gave four
// configuration writes a route to a serving process, and they come through
// openConfigWriteRoute below: `backup-set create`, `patch` and `remove`,
// and `settings patch`. What is left here as a WRITE is `backup-set
// retention` setting or clearing a policy, which has no route. Not for
// want of endpoints: the client carries setBackupSetRetention,
// clearBackupSetRetention and getBackupSetRetention. #543 would not route
// the write while the report beside it went on reading this host's own
// file, and #544 did not take that read half, so both halves of the verb
// stand where #538 left them. Everything else here reads:
// `settings` on its own, `backup-set retention` reporting the policy in
// force, and `restore`.
//
// So a write through THIS door still has exactly two outcomes. With
// nothing serving, it happens here, and an engine started afterwards
// reads the new file when it starts, because there is no config watcher
// and no SIGHUP reload in this build. With something serving, it is
// refused: nothing lands in the file for a restart to pick up, config.yaml
// is byte for byte what it was, and the operator is told what was found
// and where the change can be made instead. The reading forms are not
// touched either way, which is what the intent argument below is for.
// Issue #539 took the API-equivalence claim out, and #538 and #542 made
// this refuse rather than write: a help text that told an operator to
// restart into a command-line change was describing a binary that no
// longer makes one.
//
// service.Open is the identical production constructor
// apps/common/webhost's Open uses, so a CLI-driven write goes through the
// same service layer, and the same persist-then-hot-reload sequence
// (BackupService.UpdateSettings's own doc), that an HTTP PATCH does. A
// write through this door does not go through it BY CALLING that route,
// and the difference is the whole of issue #535: the hot reload is this
// process's own view of the file, and this process then exits.
//
// intent is issue #538, and it is why this function has an argument
// openService does not need. Every route that rewrites an EXISTING
// config.yaml is a *BackupService method, so a write reaches the file
// through this function or through openConfigWriteRoute and through
// nothing else, and a write aimed at a configuration a running engine
// holds is refused here rather than performed and reported as a success
// (#535). See liveengine.go for why the check cannot live further in,
// beside the write itself, and why a read beside a live engine must keep
// working.
//
// "Existing" is load bearing and it used not to be said. There is exactly
// one configuration write in this binary that comes through neither this
// function nor openConfigWriteRoute: `backup-set create` against a path
// where no config.yaml exists writes a FIRST configuration through
// core/service.FirstRun, whose writeConfigExclusively says in as many
// words that it is deliberately not writeConfigBytesAtomically. That route
// asks the same question about the journal --state-database names, in
// backupset.go's createFirstConfig, because the claim this doc used to
// make ("every route that rewrites config.yaml is a *BackupService method,
// so this is the one door") was phrased over exactly the predicate that
// excluded the escape.
//
// openService has no such argument because it structurally cannot write a
// configuration: it hands back an internal/app.Service built from an
// already-loaded *config.Config, which carries no path to persist to, and
// every writeConfigBytesAtomically in core/service hangs off
// *BackupService. That is a property to re-check rather than assume if
// internal/app.Service ever grows a configuration path of its own.
//
// # Why the claim is taken AFTER the open, and held
//
// The check used to run first and alone, and that made it a sample rather
// than an exclusion: after it came the startup sequence, an SSH host-key
// probe over the network, a key import and, on one route, a blocking read
// of standard input, all with nothing re-checked and no lock held. An
// engine that started during any of that was written straight over.
//
// So the order here is service.Open, then claim, then ask. The claim is
// core/service's ConfigWriteGuard, which is the same `.startup-lock`
// every engine start has to take, and it is held until cleanup, which
// callers defer, so it spans the write itself. An engine that got there
// first is already announced by the time the claim is granted and is
// found by the question; an engine that arrives later cannot finish
// starting until this command is done, and reads the new file when its
// supervisor brings it back. What that costs, and why it is worth it, is
// spelled out on ConfigWriteGuard itself.
//
// # And the answer to the question is this invocation's mode
//
// Claiming and asking are one call rather than two (#542), because the
// answer is not only a refusal: it is which world this command ran in,
// and an operator has to be able to see that in the output. mode.go
// decides it here, once, prints it, and refuses an engine-attached write
// with no route to carry it out rather than downgrading it to a direct
// one, which is every engine-attached write through this door. Nothing
// downstream asks again, so nothing downstream can get a different
// answer.
//
// The one configuration write that does not come through here,
// createFirstConfig, announces its own mode for the same reason: a
// configuration write in this binary that says nothing about which world
// it believed it was in is the gap this issue exists to close.
//
// The returned cleanup func closes the journal (via BackupService.Close)
// and gives the claim back; callers should always `defer cleanup()`
// immediately.
func openBackupService(ctx context.Context, configPath string, intent configIntent) (*service.BackupService, func(), error) {
	switch intent {
	case readsConfig, writesConfig:
	default:
		// Not reachable from any call site in this package, and it stays
		// that way by being loud rather than by being permissive: an
		// unrecognised intent defaulting to "go ahead" is how a new
		// command would silently reacquire #535.
		return nil, func() {}, fmt.Errorf("internal: openBackupService was given no intent for %s; a command has to say whether it writes the configuration", configPath)
	}

	svc, closeFn, err := service.Open(ctx, configPath)
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() {
		if err := closeFn(); err != nil {
			fmt.Fprintf(os.Stderr, "backup-manager: closing state database: %v\n", err)
		}
	}
	if intent == readsConfig {
		return svc, cleanup, nil
	}

	// One call, and it is #538's claim and #542's mode decision together,
	// because they are the same act: the claim is what makes the decision
	// still true when the write happens, and the decision is what the
	// claim is for. mode.go holds the reasoning; enterConfigWriteMode
	// claims, asks, announces and refuses, and gives the claim back
	// itself when it refuses.
	// nil rather than attachToEngine, deliberately. `backup-set
	// retention` is the write that comes through here, and it is not
	// routed. setBackupSetRetention and clearBackupSetRetention are on the
	// client, so this is a decision rather than a missing call: routing
	// the write while the report beside it still read this host's own file
	// would leave one verb answering out of two worlds, and #544 did not
	// take that read half. openConfigWriteRoute below is the
	// door that can hand a change over, and the two are separate
	// functions precisely so that "this write can be routed" is a
	// property of the call site rather than of a flag somebody might get
	// wrong.
	write, err := enterConfigWriteMode(ctx, configPath, nil, os.Stdout, os.Stderr)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	guard := write.guard
	return svc, func() {
		cleanup()
		// After the journal, so the claim outlives everything this
		// command did with it. Its failure is reported and not fatal:
		// the write has already happened by the time this runs, and
		// turning a successful change into a non-zero exit over a lock
		// file that could not be closed would be the wrong trade.
		if err := guard.Release(); err != nil {
			fmt.Fprintf(os.Stderr, "backup-manager: releasing this deployment's configuration-write claim: %v\n", err)
		}
	}, nil
}

// openConfigWriteRoute is openBackupService for the configuration writes
// that can now be handed to a running engine (#543): it answers "where
// does this change go", not "which service do I open".
//
// # Why it is a separate function rather than an argument
//
// The difference between the two doors is which writes may be routed, and
// that is a fact about the command rather than a value it passes. An
// argument would be one more thing a new subcommand could get wrong in the
// permissive direction, which is the failure mode configIntent's own doc
// spends its length on: `validate` once passed withTransport=false at the
// neighbouring call and thereby never reached a storage medium. Here the
// wrong value would be a settings patch quietly aimed at an engine through
// a route that does not carry it.
//
// # The order, and why the local service is opened either way
//
// service.Open, then claim, then ask, exactly as openBackupService does,
// and for a reason that is not a preference: service.Open's own startup
// sequence takes the same `.startup-lock` the claim is, so claiming first
// and opening second would wait on this process's own lock. What that
// costs in engine-attached mode is one journal open that turns out not to
// be needed, which is what this binary already did before it could route
// at all, and what every CLI read beside a live engine still does.
//
// It buys something too. The configuration has to be readable for the mode
// to be decided from it at all (that is where the journal path comes from),
// and a deployment whose config.yaml does not load is told so in the words
// core/service uses for it rather than through a failure against an engine.
//
// In engine-attached mode the local service is closed before the route is
// handed back, so the change and the process making it never touch this
// deployment's own files. The claim outlives it, for the same reason it
// does on the direct path: nothing else may finish starting while this
// command is deciding and acting on one answer.
//
// The returned cleanup func closes whatever was opened and gives the claim
// back; callers should always `defer cleanup()` immediately.
func openConfigWriteRoute(ctx context.Context, configPath string) (configWriteRoute, func(), error) {
	svc, closeFn, err := service.Open(ctx, configPath)
	if err != nil {
		return nil, func() {}, err
	}
	closeLocal := func() {
		if err := closeFn(); err != nil {
			fmt.Fprintf(os.Stderr, "backup-manager: closing state database: %v\n", err)
		}
	}

	write, err := enterConfigWriteMode(ctx, configPath, attachToEngine, os.Stdout, os.Stderr)
	if err != nil {
		closeLocal()
		return nil, func() {}, err
	}
	releaseClaim := func() {
		// Reported and not fatal, exactly as on the direct path: the
		// change has already happened by the time this runs.
		if err := write.guard.Release(); err != nil {
			fmt.Fprintf(os.Stderr, "backup-manager: releasing this deployment's configuration-write claim: %v\n", err)
		}
	}

	if write.route != nil {
		// The engine is the authority for this change, so this process's
		// own service has no part in it and is given back before the first
		// request goes out. Holding it open would be holding a second
		// view of a deployment this command has just agreed it does not
		// own, which is the belief #535 was made of.
		closeLocal()
		return write.route, releaseClaim, nil
	}
	return svc, func() {
		closeLocal()
		releaseClaim()
	}, nil
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

// moveExit is cycleExit's FR-30 half: the exit status a cycle's move pass
// contributes, with the reason for a non-zero one printed beside it.
//
// # Why a failed move pass fails the cycle
//
// An operator who writes `medium: cold_offsite` against a tier has said
// where those backups belong. A cycle in which every move was refused has
// not put them there, and will not on the next cycle either, because the
// reasons a move is refused are configuration reasons: a credential that
// is not set, a bucket that is not there, a storage class an artifact
// cannot be delivered to. Left at exit 0 that is issue #361's defect one
// layer up, a cycle that did nothing reporting success, and it stays
// invisible for exactly as long as nobody reads the logs.
//
// It is deliberately narrow. One refused move among several that landed
// does not fail anything, because the pass is working and one artifact
// hit something transient. It takes the whole pass getting nothing
// through, which is the same line CycleVerdict.NothingGotThrough draws.
//
// A deployment that declares no storage medium attempts no moves and
// therefore can never reach a non-zero code here. That is FR-35's
// compatibility promise for this exit status, which is pinned by a
// black-box contract suite in another repository, held by arithmetic
// rather than by a guard: the denominator is zero.
//
// # Two shapes, and MovesErr is the other one
//
// A pass that could not RUN at all (no way to reach a medium, a journal
// that cannot record a move) reports no outcomes, so the arithmetic above
// is silent about it. It is still a deployment that declared a medium and
// moved nothing, so it fails here too, on its own line, naming its own
// cause.
//
// Callers pass os.Stderr for cycleExit's reason: this binary's stdout is
// FR-23's JSON event stream and a sentence in the middle of it breaks
// every consumer that parses it a line at a time.
func moveExit(w io.Writer, report app.CycleReport) int {
	code := 0
	if report.MovesErr != nil {
		code = 1
		_, _ = fmt.Fprintf(w, "backup-manager: this deployment declares a storage medium and could not run its move pass at all: %v\n", report.MovesErr)
	}
	if p := report.MoveProgress(); p.NothingMoved() {
		code = 1
		if p.Reason == "" {
			_, _ = fmt.Fprintf(w, "backup-manager: this cycle moved nothing: %d artifact(s) were due to move to the medium their retention tier names and none arrived\n", p.Attempted)
		} else {
			_, _ = fmt.Fprintf(w, "backup-manager: this cycle moved nothing: %d artifact(s) were due to move to the medium their retention tier names and none arrived; the first refusal was: %s\n",
				p.Attempted, p.Reason)
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
