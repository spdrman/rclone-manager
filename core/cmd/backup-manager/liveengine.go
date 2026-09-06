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
// The lock this detection reads is held by every process with the journal
// open, the engine's own included, so an engine asking this question
// about itself would find itself and refuse its own API writes. That is
// why the probe happens here, in the CLI, before anything opens anything,
// rather than down beside the write in core/service where both routes
// meet.
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
		return fmt.Errorf("cannot tell whether another process is already running this deployment, and a configuration change written while one is would never reach it: %w", err)
	}
	if engine == nil {
		return nil
	}
	// Resolved for the message, because --config may name the packaged
	// configuration DIRECTORY (#196) and an operator matching this
	// sentence against their own deployment needs the file, not the
	// directory they typed.
	//
	// The engine is named by the state database it was found holding
	// rather than by a pid: flock(2) offers no portable way to ask which
	// process holds a lock, and a message that carries a pid on Linux and
	// not on macOS would be worse than one that carries none. The
	// database is also the thing an operator can match against a
	// container's own mounts.
	return fmt.Errorf(
		"another process is already running this deployment and holds its state database open (%s), so a configuration change made here would never reach it: that process read %s when it started and nothing re-reads that file. Make this change through the Web UI, or through the HTTP API that process serves, or stop that process and run this command again",
		engine.StateDatabase, config.ResolvePath(configPath))
}
