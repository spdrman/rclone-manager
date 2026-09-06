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
// #538 already asks whether another process is serving this deployment,
// and refuses a configuration write when one is. What it does not do is
// say which world the command believed it was in. An operator reading
// `backup-set create` scroll back sees a set printed and has no way to
// tell whether that reached the running engine, was written directly
// because nothing was serving, or was written directly because the check
// was skipped, and those are three very different facts about their
// deployment.
//
// So the answer is given a name, printed, and made once. Once matters on
// its own: a second probe is a second question asked of a world that can
// have changed between the two, and an engine that starts in that gap
// turns a refusal into a direct write. That is #535 again, arriving
// through a race rather than through a missing check, and the only thing
// that structurally prevents it is that nothing downstream re-derives the
// answer. mode_test.go counts the probes rather than reading the code for
// it.
//
// # The decision and the claim are one act
//
// This is the part that changed when core/service stopped answering
// "who holds the journal" and started answering "who announced that they
// serve this deployment". The new mechanism does not only give a better
// answer; it gives an answer that STAYS true, and it does that through
// service.BeginConfigWrite, which takes the same `.startup-lock` every
// engine start has to take and holds it until the write is done.
//
// Deciding a mode outside that claim would put this file back where the
// review found #538: a sample about a world that has had seconds to
// change, with a startup sequence, an SSH host-key probe and, on one
// route, a blocking read of standard input in between. A `backup-set
// retention --policy-file -` parked on a fifo made that gap as wide as an
// operator liked, and the write landed behind an engine that had started
// in the meantime.
//
// So there is no way here to decide a mode without claiming the
// deployment, and no way to claim it without deciding a mode.
// enterConfigWriteMode does both or neither, and hands back the claim its
// caller has to hold for as long as the write lasts. "Decided once" is
// then not a property of how many times this file calls the kernel, it is
// a property of the world: nothing else can finish starting between the
// question and the write.
//
// # The attach that fails is still the whole point
//
// Engine-attached mode can now be carried out for the three mutating
// backup-set verbs and for `settings patch` (#543, route.go), and only
// when this command has been told where the engine is. Everything else
// still refuses, and so does a route that was named and cannot be built.
//
// What this file guarantees is what happens on that refusal. It refuses.
// It does not write the file directly and report success, which is
// exactly the shape #535 recorded: a change one process believes in and
// the serving process will never see. A fallback that happens quietly is
// that bug wearing a different hat, so there is no fallback here at all,
// quiet or otherwise, and there is no path through settleConfigWriteMode
// on which an engine-attached command with no route returns a usable
// claim.
//
// # And there is no third mode
//
// A probe that cannot be performed does not produce a mode. It produces a
// refusal, for the reason cannotTellError gives: "I could not tell" and
// "nothing is running" are the same behaviour only if you are willing to
// write the file anyway, which is the defect.
//
// # Reads have their own, next door
//
// They used to have none, and #544 gave them one: readmode.go decides the
// same question the same way, once, and announces it, for `sources`,
// `status`, `artifacts` and the retention preview. It does NOT take the
// claim this file takes, because a read changes nothing and holding the
// startup lock for the length of a `status` would stop containers
// starting for no benefit, and it has a third mode this file does not,
// because a read that cannot reach the engine still has to answer.
//
// What still has no mode is the rest of the reads: `settings`,
// `backup-set retention` shown, `restore`, and every other command on
// openService. Saying so is the alternative to leaving a gap
// indistinguishable from one nobody thought of. Whoever picks those up
// should not read "read" as "no side effects": `restore` is declared
// readsConfig and writes an operation row into the journal, which is true
// of the configuration and not of the deployment.

// executionMode is where one invocation's configuration write actually
// happened: through the process already serving this deployment, or
// straight into the file because no process is.
type executionMode string

const (
	// directMode: no process has announced itself as serving this
	// deployment, so this command is the only authority there is and
	// changes the configuration file itself.
	directMode executionMode = "direct"

	// engineAttachedMode: another process is serving this deployment, so
	// the change belongs to it. Whether this build can hand it over
	// depends on the verb and on whether an address was given: the three
	// mutating backup-set verbs and `settings patch` route (#543) when
	// $BACKUP_MANAGER_API_URL names an engine, and everything else is
	// still a refusal, including a `backup-set retention` that sets or
	// clears a policy, a first configuration, and any write at all beside
	// a `daemon`, which serves no HTTP for a route to reach.
	engineAttachedMode executionMode = "engine-attached"
)

// modeLinePrefix is what an operator (or a test) greps for. Both
// announcements start with it and the two mode names are not prefixes of
// each other, so "which mode was this" is answerable from one line of
// scroll back rather than from a paragraph.
const modeLinePrefix = "mode: "

// detectRunningEngine and detectRunningEngineForJournal are core/service's
// two detection entry points behind package variables, so mode_test.go can
// count how many times one invocation asks.
//
// Both, not one, and that is not symmetry for its own sake. They are the
// same question asked by callers that differ only in whether there is a
// configuration file to read the journal path out of, and a counter that
// watched one of them would report "asked once" for an invocation that
// went down the other path and asked twice.
//
// Variables rather than an interface, unlike this package's other two
// seams (backupSetRemover, backupSetCreatePrereqs), because the thing
// being observed is not a collaborator a caller could be handed: it is
// how often the whole binary reaches the kernel across a dispatch that
// takes only an argv. Nothing overrides them outside a test, and the one
// test that does restores them through t.Cleanup.
var (
	detectRunningEngine           = service.DetectRunningEngine
	detectRunningEngineForJournal = service.DetectRunningEngineForJournal
)

// modeDecision is the one answer an invocation gets, and the value every
// later step reads instead of asking again.
type modeDecision struct {
	// mode is the decision itself.
	mode executionMode

	// engine is the process that announced itself as serving this
	// deployment, and is nil in direct mode. Kept beside the mode rather
	// than discarded because the refusal has to name what it found: an
	// operator needs the state database to go and identify the container,
	// which is the same reason core/service.RunningEngine carries it.
	engine *service.RunningEngine

	// configFile is the configuration this decision is about, resolved,
	// because --config may name the packaged configuration DIRECTORY
	// (#196) and an operator matching an announcement against their own
	// deployment needs the file they would edit.
	configFile string

	// because is the clause engineRefusal appends to say why a change
	// made here would never reach that process. It differs between the
	// two callers only because one of them can name the file the engine
	// read and the other, by construction, cannot.
	because string

	// route is how this invocation hands the change to that process, and
	// is nil whenever it cannot: in direct mode, on a write no verb
	// routes, and beside a serving process this command was told nothing
	// about. Nil is what refusal() reads, so "engine-attached" and
	// "engine-attached and carryable" can never be confused for each
	// other by a later reader.
	route configWriteRoute

	// routeAddress is where that route goes, already redacted, for the
	// announcement. #542's rule is that the mode is reported rather than
	// inferred, and "through the engine" without saying which engine is
	// half a report.
	routeAddress string
}

// heldBy names the state database the serving process announced, or the
// empty string in direct mode.
//
// It exists so that mode and engine are read together in one place. The
// two enter functions below are the only things that build a
// modeDecision and they set the two from the same answer, so they cannot
// disagree; this is what keeps that from being a fact every later reader
// has to remember. The engine is named by its journal rather than by a
// pid for the reason engineRefusal gives: flock(2) offers no portable way
// to ask which process holds a lock, and a message carrying a pid on
// Linux and not on macOS is worse than one carrying none.
func (d modeDecision) heldBy() string {
	if d.engine == nil {
		return ""
	}
	return d.engine.StateDatabase
}

// announce writes the one line that says which mode this invocation is
// in.
//
// # What it may claim, and what it may not
//
// It says what is SERVING, which is a claim this binary can now actually
// make. It used to say what was found holding the journal open, and that
// was the wrong sentence about the wrong fact: every `status`, every
// `sources` and every cron `run` holds the journal, so an operator with a
// backup cycle in flight was told an engine was attached. The mechanism
// underneath is now an announcement a serving process makes about itself,
// so the line can name serving without over-claiming.
//
// The direct line says "no process has announced itself" rather than
// "nothing is serving this deployment", and the difference is the one gap
// liveengine.go names out loud: a host still on the first-run wizard
// serves something and has not announced anything, because it has no
// journal to announce about yet. The weaker sentence is the true one.
//
// No address appears here. A mode that named one would print
// apiclient.BaseURL(), which renders userinfo credentials in cleartext
// (#546). The state database is what an operator needs to find the
// process anyway, and it is not a secret.
//
// # Which stream, and why they differ
//
// A direct write announces itself on stdout, beside the command's own
// report of what it did (printBackupSet, printSettings and the removal
// summary are all there), because it qualifies that report: it is the
// difference between "this set exists" and "this set exists in a file a
// serving engine has not read". `run` and `fetch` are the commands whose
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
	switch {
	case d.mode == engineAttachedMode && d.route != nil:
		_, _ = fmt.Fprintf(announceTo, "%s%s. Another process is serving this deployment (state database %s), so this command hands the change to it at %s rather than writing %s itself.\n",
			modeLinePrefix, d.mode, d.heldBy(), d.routeAddress, d.configFile)
	case d.mode == engineAttachedMode:
		_, _ = fmt.Fprintf(refuseTo, "%s%s. Another process is serving this deployment (state database %s) and this build has no route to it, so the change is refused here rather than downgraded to a direct write it would never see.\n",
			modeLinePrefix, d.mode, d.heldBy())
	case d.mode == directMode:
		_, _ = fmt.Fprintf(announceTo, "%s%s. No process has announced itself as serving this deployment, so this command changes %s itself.\n",
			modeLinePrefix, d.mode, d.configFile)
	default:
		// Not reachable from the two enter functions below, which are the
		// only things that build one of these, and it stays that way by
		// being loud rather than by falling through to the direct
		// wording. A mode nobody named is not a mode, and a configuration
		// write that announced the wrong one would be worse than one that
		// announced nothing.
		_, _ = fmt.Fprintf(refuseTo, "%s%q, which this binary does not recognise; this is a bug in %s\n",
			modeLinePrefix, d.mode, d.configFile)
	}
}

// refusal is what engine-attached mode means when nothing can hand the
// change over: the command stops. Direct mode returns nil because this
// process is the only authority there is, and engine-attached mode with a
// route returns nil because the change is about to go to the process that
// IS the authority.
//
// The condition is the route rather than the mode, which is the whole of
// what #543 changed here. A reader adding a fourth case should keep it that
// way: "an engine is serving this" and "and this command can reach it" are
// two facts, and the refusal is about the second one.
func (d modeDecision) refusal() error {
	if d.mode != engineAttachedMode || d.route != nil {
		return nil
	}
	return engineRefusal(d.engine, d.because)
}

// enterConfigWriteMode claims the deployment configPath names for a
// configuration write, decides this invocation's mode from inside that
// claim, says which one it is, and refuses when it is a mode this build
// cannot carry out.
//
// The four steps are one function on purpose, and the claim is one of
// them for the reason this file's doc gives: a mode decided outside the
// claim is a sample, and #538's review demonstrated a write landing
// behind an engine that started after the sample was taken. Split up, a
// new configuration-writing command could also decide a mode and forget
// to print it, or print one and forget that engine-attached means it may
// not write. There is no way to get part of this.
//
// The caller owns the returned claim and must Release it once the write
// is done. A refusal releases it here, so a caller that got an error has
// nothing to give back.
//
// The order is BeginConfigWrite and then the probe, never the other way
// round. A serving process announces itself before it takes the startup
// lock (core/service's AnnounceServing), so by the time this claim is
// granted, an engine that got there first is already visible to the
// question below, and one that arrives later cannot finish starting until
// the claim is released.
func enterConfigWriteMode(configPath string, attach attachFunc, announceTo, refuseTo io.Writer) (*configWrite, error) {
	guard, err := service.BeginConfigWrite(configPath)
	if err != nil {
		return nil, err
	}
	engine, err := detectRunningEngine(configPath)
	if err != nil {
		_ = guard.Release()
		return nil, cannotTellError(err)
	}
	// Resolved for the announcement and for the refusal, because --config
	// may name the packaged configuration DIRECTORY (#196) and an
	// operator matching either sentence against their own deployment
	// needs the file, not the directory they typed.
	resolved := config.ResolvePath(configPath)
	return settleConfigWriteMode(guard, modeDecision{
		engine:     engine,
		configFile: resolved,
		because:    fmt.Sprintf("that process read %s when it started and nothing re-reads that file", resolved),
	}, attach, announceTo, refuseTo)
}

// enterFirstConfigWriteMode is enterConfigWriteMode for the one
// configuration write that has no configuration to read a journal path
// out of: `backup-set create` against a path where no config.yaml exists,
// which writes a whole first configuration through core/service.FirstRun.
//
// It exists because that path was the way around the check, and it is
// also the one write in this binary that would otherwise announce no mode
// at all. Two ordinary mistakes land here against a LIVE deployment and
// both used to exit 0 after writing a configuration nothing would ever
// read: a mistyped --config, and a config.yaml renamed out from under a
// running engine. --state-database is what still identifies the
// deployment in both, because it carries the same packaged default the
// first-run wizard writes.
//
// configFile is only ever announced, never probed. That split is the
// point: the decision is about the deployment, which is the journal, and
// the announcement is about the file this command is going to write.
func enterFirstConfigWriteMode(configFile, stateDatabase string, announceTo, refuseTo io.Writer) (*service.ConfigWriteGuard, error) {
	guard, err := service.BeginConfigWriteForJournal(stateDatabase)
	if err != nil {
		return nil, err
	}
	engine, err := detectRunningEngineForJournal(stateDatabase)
	if err != nil {
		_ = guard.Release()
		return nil, cannotTellError(err)
	}
	// No attach, and that is a decision rather than an omission. This
	// path writes a whole FIRST configuration, and finding a process
	// serving the journal it names means that deployment is already
	// configured; POST /system/first-run is the wrong operation to send an
	// already-configured engine, and there is no other. So engine-attached
	// here is still exactly what it was: a refusal.
	write, err := settleConfigWriteMode(guard, modeDecision{
		engine:     engine,
		configFile: config.ResolvePath(configFile),
		because:    "that process read its configuration when it started and nothing re-reads it",
	}, nil, announceTo, refuseTo)
	if err != nil {
		return nil, err
	}
	return write.guard, nil
}

// settleConfigWriteMode is the tail both enter functions share: name the
// mode from the one answer, settle how (or whether) it can be carried out,
// say it out loud, and either hand the claim on or give it back with the
// refusal.
//
// The mode is derived here and nowhere else, from the engine field alone,
// so the two can never be set from different answers. A caller that built
// a modeDecision and set the mode itself would be a second place that
// could decide, which is what this issue exists to remove. The route is
// settled in the same place and for the same reason: a command that asked
// separately could ask after the announcement, and print one thing while
// doing another.
//
// A route is looked for only in engine-attached mode. Building one in
// direct mode would be reaching for an engine this command has just
// established is not there, and would turn a stray environment variable
// into a change aimed at somebody else's deployment.
func settleConfigWriteMode(guard *service.ConfigWriteGuard, d modeDecision, attach attachFunc, announceTo, refuseTo io.Writer) (*configWrite, error) {
	d.mode = directMode
	if d.engine != nil {
		d.mode = engineAttachedMode
	}

	var attachErr error
	if d.mode == engineAttachedMode && attach != nil {
		d.route, d.routeAddress, attachErr = attach()
	}

	d.announce(announceTo, refuseTo)

	// The named-and-unusable case, announced first so the mode is on the
	// terminal before the complaint about it, and refused rather than
	// downgraded: an operator who set an address meant it.
	if attachErr != nil {
		_ = guard.Release()
		return nil, attachErr
	}
	if err := d.refusal(); err != nil {
		_ = guard.Release()
		return nil, err
	}
	return &configWrite{guard: guard, route: d.route}, nil
}

// attachFunc is how a caller says whether the write it is about to make
// can be routed, and builds the route when it can.
//
// A nil attachFunc is "this write has no route", which is the honest
// answer for every configuration write except the four #543 covers, the
// three mutating backup-set verbs and `settings patch`. Saying it by
// passing nil rather than by omitting a step is what makes the unrouted
// writes visible: they are call sites that deliberately hand over
// nothing, not call sites that forgot.
type attachFunc func() (configWriteRoute, string, error)

// configWrite is one settled configuration write: the claim that keeps the
// decision true, and the route it decided on, which is nil for a write
// this process performs itself.
type configWrite struct {
	guard *service.ConfigWriteGuard
	route configWriteRoute
}
