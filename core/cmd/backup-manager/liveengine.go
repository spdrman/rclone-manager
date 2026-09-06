package main

import (
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/service"
)

// Issue #538, Phase 1 of #536: the CLI refusing to write a configuration
// a running engine holds, instead of writing it and reporting success.
//
// # What went wrong, and why refusing is the honest answer
//
// #535: `backup-set create` through `docker exec`, against a container
// already running the engine. It wrote config.yaml, adopted the change in
// its own memory, exited 0 and printed the new set. The engine had read
// that file when it started and has no watcher on it, so the Web UI never
// showed the set. `sources` -- another CLI process, reading the file --
// showed it. Two surfaces, two answers, no complaint from either.
//
// Refusing is not the end state. #536 Phase 2 gives this binary a real
// route to the engine, and then a mutation will REACH it rather than be
// turned away. Until that exists, a write that cannot take effect must
// not report that it did, and that is the whole of what this file does.
//
// # Why it is not simply "always check before opening"
//
// The check only means anything from a process that is not the engine.
// core/service answers it from a lock only a SERVING process takes, so an
// engine asking about itself would find itself and refuse its own API
// writes. That is why the check happens here, in the CLI, rather than
// down beside the write in core/service where both routes meet.
//
// It is not a check taken once and then trusted, either. It is taken
// while holding core/service's ConfigWriteGuard, and the guard is kept
// until the command is done, so an engine that was not there when the
// question was asked cannot have finished starting by the time the answer
// is acted on. The version of this that only asked was demonstrably not
// enough: a `backup-set retention --policy-file -` parked on a fifo asked
// before it read stdin, and an operator could take as long as they liked
// to make the answer stale.
//
// # The one shape this cannot see, said out loud
//
// A host serving the FIRST-RUN flow (issue #176: an install with no
// config.yaml serves a setup wizard rather than refusing to start) holds
// no journal and serves no deployment yet. apps/generic announces itself
// only in its Activate callback, after its own POST has written the
// configuration, so until then there is nothing to find and no lock-based
// detector can find it. A CLI `backup-set create` on such a host takes
// createFirstConfig's path, writes the first configuration underneath the
// wizard, and the wizard goes on serving setup until it is restarted.
//
// That is genuinely the wizard's shape, and it is narrow: it is not "any
// host with no config.yaml", because a create against an absent
// configuration path now asks about the journal --state-database names
// before it writes anything (backupset.go's createFirstConfig). A
// mistyped --config against a live deployment, and a config.yaml renamed
// out from under a running engine, both used to land here and are both
// refused now. What remains is a host where nothing has ever been
// configured and something is waiting to be, which is the same family as
// #535 and is not detectable here: there is no journal, no port this
// binary knows about, and no credential it holds. #536 Phase 2 is where
// it closes, by giving this binary a real route to the running process
// instead of a way to notice one.
//
// # And why reads are left alone
//
// A CLI read beside a live engine is ordinary use of this binary and
// always has been: core/service's startup.go takes the journal lock
// SHARED for exactly that reason, so `backup-manager status` next to a
// running `serve` keeps working. Nothing here may narrow that. Only a
// write that lands in config.yaml is at risk of being believed by one
// process and not the other.

// configIntent is what a subcommand is about to do to config.yaml, named
// at every openBackupService call site.
//
// It is a named string rather than a bool deliberately. `validate` once
// passed withTransport=false at the neighbouring call and thereby never
// reached a storage medium at all, which is what a bare boolean at a
// choke point costs: the wrong value is invisible at the call site and
// silently permissive. Here the wrong value is worse than a missing
// feature -- it is #535 coming back -- so the argument says out loud
// which of the two things this command does, and the table in
// liveengine_test.go checks every writing command against a real running
// engine rather than trusting the declaration.
type configIntent string

const (
	// readsConfig: this command loads the configuration and never
	// rewrites it. Safe beside a running engine, and must stay that way.
	readsConfig configIntent = "reads the configuration"

	// writesConfig: this command persists a change into config.yaml, so
	// it is only meaningful when this process is the deployment's only
	// authority.
	writesConfig configIntent = "writes the configuration"
)

// refuseIfAnEngineHoldsTheConfiguration returns the refusal a
// configuration write gets when another process is already running this
// deployment, or nil when this process is the only authority there is.
//
// A detection that could not be PERFORMED is returned as an error too,
// and deliberately so: "I could not tell" and "nothing is running" are
// the same behaviour only if you are willing to write the file anyway,
// which is the defect. core/service.DetectRunningEngine's own doc splits
// that from the case where the configuration cannot be read, which is not
// an error here because the open that follows this call fails on the same
// file with the message the operator actually needs.
func refuseIfAnEngineHoldsTheConfiguration(configPath string) error {
	engine, err := service.DetectRunningEngine(configPath)
	if err != nil {
		return cannotTellError(err)
	}
	if engine == nil {
		return nil
	}
	// Resolved for the message, because --config may name the packaged
	// configuration DIRECTORY (#196) and an operator matching this
	// sentence against their own deployment needs the file, not the
	// directory they typed.
	return engineRefusal(engine, fmt.Sprintf("that process read %s when it started and nothing re-reads that file", config.ResolvePath(configPath)))
}

// refuseIfAnEngineServesThisJournal is the same refusal for a caller with
// no configuration to read the journal out of: a `backup-set create`
// against a path where no config.yaml exists, which writes a whole first
// configuration through core/service.FirstRun rather than through
// BackupService.
//
// It exists because that path was the way around the check. A mistyped
// --config, or a config.yaml renamed out from under a running engine,
// both leave `create` looking at an absent file, and both used to write a
// brand-new configuration beside a live deployment and report success.
// The journal is what still identifies the deployment in that state,
// because --state-database names it and carries the packaged default the
// first-run wizard writes.
func refuseIfAnEngineServesThisJournal(stateDatabase string) error {
	engine, err := service.DetectRunningEngineForJournal(stateDatabase)
	if err != nil {
		return cannotTellError(err)
	}
	if engine == nil {
		return nil
	}
	return engineRefusal(engine, "that process read its configuration when it started and nothing re-reads it")
}

// engineRefusal is the sentence both refusals print, so the two cannot
// drift into telling an operator different things about the same
// situation.
//
// Three things have to be in it, and each is there because leaving it out
// was worse.
//
// What was found, named by the state database rather than by a pid:
// flock(2) offers no portable way to ask which process holds a lock, and
// a message that carries a pid on Linux and not on macOS would be worse
// than one that carries none. The database is also the thing an operator
// can match against a container's own mounts.
//
// That nothing was written. "This was refused" and "this was refused and
// your file is untouched" are different pieces of news, and only the
// second one tells somebody reading a failed script whether they now have
// to go and check the file.
//
// And a remedy that exists. The remedy this used to lead with was "make
// this change through the Web UI, or through the HTTP API that process
// serves", which on a host running `backup-manager daemon` names two
// things that are not there: the daemon serves no HTTP at all. Stopping
// the process is the one answer that is true on every deployment, so it
// is the one stated plainly, and the other is offered as the conditional
// it actually is.
func engineRefusal(engine *service.RunningEngine, because string) error {
	return fmt.Errorf(
		"another process is already serving this deployment (state database %s), so nothing was written: a configuration change made here would never reach it, because %s. Stop that process and run this command again; if it serves this deployment's Web UI or HTTP API, the change can be made there instead",
		engine.StateDatabase, because)
}

// cannotTellError is the refusal for a check that could not be performed,
// and it is deliberately a refusal rather than a shrug.
//
// "I could not tell" and "nothing is running" are the same behaviour only
// if you are willing to write the file anyway, which is the defect. The
// check does fail in production: EACCES on a lock file owned by another
// uid, ENOTSUP where flock is unavailable, EIO on a sick volume, and the
// whole non-unix build, where core/service refuses to guess by design.
func cannotTellError(err error) error {
	return fmt.Errorf("cannot tell whether another process is already serving this deployment, and a configuration change written while one is would never reach it, so nothing was written: %w", err)
}
