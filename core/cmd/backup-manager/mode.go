package main

import (
	"fmt"
	"io"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/service"
)

// Issue #542, Phase 2 of #536: engine-attached or direct is a decision,
// so it is made once per invocation, carried, and printed, and a mode
// that cannot be carried out is a refusal rather than a downgrade.
//
// # Why this is a decision and not a condition
//
// #538 already asks the kernel whether another process holds this
// deployment's journal, and refuses a configuration write when one does.
// What it does not do is say which world the command believed it was in.
// An operator reading `backup-set create` scroll back sees a set printed
// and has no way to tell whether that reached the running engine, was
// written directly because nothing was running, or was written directly
// because the check was skipped, and those are three very different
// facts about their deployment.
//
// So the answer is given a name, printed, and made once. Once matters on
// its own: a second probe is a second question asked of a world that can
// have changed between the two, and an engine that exits in that gap
// turns a refusal into a direct write. That is #535 again, arriving
// through a race rather than through a missing check, and the only thing
// that structurally prevents it is that nothing downstream re-derives the
// answer. mode_test.go counts the probes rather than reading the code for
// it.
//
// # The failed attach is the whole point
//
// Engine-attached mode cannot presently be carried out: #541 is the HTTP
// client that would reach the running process and #543 is what routes a
// mutation through it, and neither exists yet. So every attach fails
// today, and what this file guarantees is what happens next. It refuses.
// It does not write the file directly and report success, which is
// exactly the shape #535 recorded: a change one process believes in and
// the serving process will never see. A fallback that happens quietly is
// that bug wearing a different hat, so there is no fallback here at all,
// quiet or otherwise.
//
// # And there is no third mode
//
// A probe that cannot be performed does not produce a mode. It produces a
// refusal, for the reason core/service/liveengine.go's own doc gives: "I
// could not tell" and "nothing is running" are the same behaviour only if
// you are willing to write the file anyway, which is the defect.
//
// # What deliberately has no mode yet
//
// Reads. `settings`, `backup-set retention` shown, `restore` and every
// command on openService answer from the configuration file and the
// shared journal whether an engine is up or not, which is ordinary use of
// this binary and which #538 was careful not to narrow. They will get a
// mode when they get a route, which is #544. Saying so is the
// alternative to leaving a gap indistinguishable from one nobody thought
// of.

// executionMode is where one invocation's configuration write actually
// happened: through the process already running this deployment, or
// straight into the file because there is no such process.
type executionMode string

const (
	// directMode: no other process holds this deployment's journal, so
	// this command is the only authority there is and changes the
	// configuration file itself.
	directMode executionMode = "direct"

	// engineAttachedMode: another process is running this deployment, so
	// the change belongs to it. Nothing in this build can hand it over
	// yet (#541, #543), which makes every engine-attached configuration
	// write a refusal today.
	engineAttachedMode executionMode = "engine-attached"
)

// modeLinePrefix is what an operator (or a test) greps for. Both
// announcements start with it and the two mode names are not prefixes of
// each other, so "which mode was this" is answerable from one line of
// scroll back rather than from a paragraph.
const modeLinePrefix = "mode: "

// detectRunningEngine is core/service.DetectRunningEngine behind a
// package variable so mode_test.go can count how many times one
// invocation asks.
//
// A variable rather than an interface, unlike this package's other two
// seams (backupSetRemover, backupSetCreatePrereqs), because the thing
// being observed is not a collaborator a caller could be handed: it is
// how often the whole binary reaches the kernel across a dispatch that
// takes only an argv. Nothing overrides it outside a test, and the one
// test that does restores it through t.Cleanup.
var detectRunningEngine = service.DetectRunningEngine

// modeDecision is the one answer an invocation gets, and the value every
// later step reads instead of asking again.
type modeDecision struct {
	// mode is the decision itself.
	mode executionMode

	// engine is the process that was found holding this deployment, and
	// is nil in direct mode. Kept beside the mode rather than discarded
	// because the refusal has to name what it found: an operator needs
	// the state database to go and identify the container, which is the
	// same reason core/service.RunningEngine carries it.
	engine *service.RunningEngine

	// configFile is the configuration this decision is about, resolved,
	// because --config may name the packaged configuration DIRECTORY
	// (#196) and an operator matching an announcement against their own
	// deployment needs the file they would edit.
	configFile string
}

// heldBy names the state database the engine was found holding, or the
// empty string in direct mode.
//
// It exists so that mode and engine are read together in one place.
// decideConfigWriteMode is the only thing that builds a modeDecision and
// it sets the two from the same answer, so they cannot disagree; this is
// what keeps that from being a fact every later reader has to remember.
// The engine is named by its journal rather than by a pid for the reason
// liveengine.go gives: flock(2) offers no portable way to ask which
// process holds a lock, and a message carrying a pid on Linux and not on
// macOS is worse than one carrying none.
func (d modeDecision) heldBy() string {
	if d.engine == nil {
		return ""
	}
	return d.engine.StateDatabase
}

// decideConfigWriteMode asks, once, whether another process is running
// this deployment, and turns the answer into the mode this invocation is
// in.
//
// The error it returns is a probe that could not be performed, never a
// probe that answered "nobody". Callers must treat it as a refusal; there
// is no mode to fall back to, because falling back is the defect.
func decideConfigWriteMode(configPath string) (modeDecision, error) {
	engine, err := detectRunningEngine(configPath)
	if err != nil {
		// #538's sentence, unchanged: it is already the one an operator
		// needs, and it belongs here now rather than beside the refusal
		// because this is where the question is asked.
		return modeDecision{}, fmt.Errorf("cannot tell whether another process is already running this deployment, and a configuration change written while one is would never reach it: %w", err)
	}
	d := modeDecision{mode: directMode, engine: engine, configFile: config.ResolvePath(configPath)}
	if engine != nil {
		d.mode = engineAttachedMode
	}
	return d, nil
}

// announce writes the one line that says which mode this invocation is
// in.
//
// # Which stream, and why they differ
//
// A direct write announces itself on stdout, beside the command's own
// report of what it did (printBackupSet, printSettings and the removal
// summary are all there), because it qualifies that report: it is the
// difference between "this set exists" and "this set exists in a file a
// running engine has not read". `run` and `fetch` are the commands whose
// stdout is FR-23's JSON event stream and nothing new may go there
// (cycleExit says so in its own doc); neither of them writes a
// configuration, so neither of them reaches this.
//
// An engine-attached announcement goes to stderr, because in this build
// it is always followed by a refusal and a refusal is not a report. That
// asymmetry is deliberate and it is why mode_test.go folds the two
// streams together: what is being promised is that an operator reading
// one terminal can see which mode ran, not that a particular file
// descriptor carries it.
func (d modeDecision) announce(announceTo, refuseTo io.Writer) {
	// Deliberately unchecked, like every other diagnostic this binary
	// prints: a terminal that went away cannot change what mode this
	// invocation is in, and swallowing the command because the
	// announcement could not be delivered would be the worse answer.
	switch d.mode {
	case engineAttachedMode:
		_, _ = fmt.Fprintf(refuseTo, "%s%s. Another process is running this deployment (it holds %s open), and this build has no route to it, so nothing was written: a mode that cannot be carried out is refused here, never downgraded to a direct write.\n",
			modeLinePrefix, d.mode, d.heldBy())
	case directMode:
		_, _ = fmt.Fprintf(announceTo, "%s%s. Nothing was found running this deployment, so this command changes %s itself.\n",
			modeLinePrefix, d.mode, d.configFile)
	default:
		// Not reachable from decideConfigWriteMode, which is the only
		// thing that builds one of these, and it stays that way by being
		// loud rather than by falling through to the direct wording. A
		// mode nobody named is not a mode, and a configuration write that
		// announced the wrong one would be worse than one that announced
		// nothing.
		_, _ = fmt.Fprintf(refuseTo, "%s%q, which this binary does not recognise; this is a bug in %s\n",
			modeLinePrefix, d.mode, d.configFile)
	}
}

// enterConfigWriteMode is the whole of #542 at a call site: decide which
// mode this invocation is in, say so, and return the refusal when it is a
// mode this build cannot carry out.
//
// The three steps are one function on purpose. Split up, a new
// configuration-writing command could decide a mode and forget to print
// it, or print one and forget that engine-attached means it may not
// write, and both of those are silent. There is no way to get half of
// this.
//
// Every route that rewrites config.yaml calls it exactly once:
// openBackupService for the commands that persist through a
// BackupService, and createFirstConfig for the one write that predates
// having a service at all.
func enterConfigWriteMode(configPath string, announceTo, refuseTo io.Writer) error {
	decision, err := decideConfigWriteMode(configPath)
	if err != nil {
		return err
	}
	decision.announce(announceTo, refuseTo)
	return refuseIfAnEngineHoldsTheConfiguration(decision)
}
