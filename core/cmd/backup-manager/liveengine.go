package main

import "fmt"

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
// The lock this detection reads is held by every process with the journal
// open, the engine's own included, so an engine asking this question
// about itself would find itself and refuse its own API writes. That is
// why the probe happens here, in the CLI, before anything opens anything,
// rather than down beside the write in core/service where both routes
// meet.
//
// # The one shape this cannot see, said out loud
//
// A host serving the FIRST-RUN flow (issue #176: an install with no
// config.yaml serves a setup wizard rather than refusing to start) holds
// no journal. apps/generic opens one only in its Activate callback, after
// its own POST has written the configuration, so until then there is no
// lock to find and no lock-based detector can find one. A CLI
// `backup-set create` on such a host takes createFirstConfig's path,
// writes the first configuration underneath the wizard, and the wizard
// goes on serving setup until it is restarted.
//
// That is the same family as #535 and it is not fixed here, because it is
// not detectable here: there is no journal, no port this binary knows
// about, and no credential it holds. #536 Phase 2 is where it closes, by
// giving this binary a real route to the running process instead of a way
// to notice one. Naming it is the alternative to leaving a gap
// indistinguishable from one nobody thought of.
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
// configuration write gets when the mode decided for this invocation is
// engine-attached, or nil when it is direct and this process is the only
// authority there is.
//
// It takes the decision rather than a path, and therefore no longer asks
// the kernel anything itself: #542 made the mode one answer per
// invocation, so the probe happens once, in decideConfigWriteMode, and
// everything downstream reads the value it produced. A refusal that
// re-probed would be a second question about a world that can have
// changed since the first, and an engine exiting in that gap would turn
// this refusal into a direct write.
//
// A detection that could not be PERFORMED is still a refusal rather than
// a "no", for the same reason it always was ("I could not tell" and
// "nothing is running" are the same behaviour only if you are willing to
// write the file anyway, which is the defect); that case never reaches
// here, because decideConfigWriteMode returns it as an error and there is
// no decision to act on.
func refuseIfAnEngineHoldsTheConfiguration(d modeDecision) error {
	held := d.heldBy()
	if held == "" {
		// Direct mode: nothing was found running this deployment, so this
		// process is the only authority there is and the write goes
		// ahead. heldBy rather than the mode field because the two are
		// set from the same answer and reading them together is what
		// stops them being read apart (modeDecision.heldBy says so).
		return nil
	}
	// modeDecision.configFile is already resolved, because --config may
	// name the packaged configuration DIRECTORY (#196) and an operator
	// matching this sentence against their own deployment needs the file,
	// not the directory they typed.
	//
	// The engine is named by the state database it was found holding
	// rather than by a pid: flock(2) offers no portable way to ask which
	// process holds a lock, and a message that carries a pid on Linux and
	// not on macOS would be worse than one that carries none. The
	// database is also the thing an operator can match against a
	// container's own mounts.
	return fmt.Errorf(
		"another process is already running this deployment and holds its state database open (%s), so a configuration change made here would never reach it: that process read %s when it started and nothing re-reads that file. Make this change through the Web UI, or through the HTTP API that process serves, or stop that process and run this command again",
		held, d.configFile)
}
