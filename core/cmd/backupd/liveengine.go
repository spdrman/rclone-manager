package main

import (
	"errors"
	"fmt"

	"github.com/backupdproject/backupd/core/service"
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
// showed the set. `sources`, which is another CLI process reading that
// same file, showed it. Two surfaces, two answers, no complaint from
// either.
//
// Refusing is not the whole story any more. #536 Phase 2 gave this binary
// a real route to the engine, so `backup-set create`, `patch` and
// `remove` and `settings patch` REACH a serving process when this host
// has been told where it is (#543, route.go). What is left to this file
// is every other case: a write no verb routes, a serving process nobody
// named an address for, and a `daemon`, which serves no HTTP to route to.
// In all of those a write that cannot take effect must not report that it
// did, and saying so is the whole of what this file does.
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
// The asking itself lives next door, in mode.go, because the answer is
// also #542's decision: taking the claim, asking, naming the mode and
// saying it out loud are one act there, and this file is left with the
// two sentences that act prints. Splitting them that way is what stops a
// caller acquiring the claim without deciding a mode, or deciding one
// without holding the claim.
//
// # The shape that used to get through, and what closed it
//
// A host serving the FIRST-RUN flow (issue #176: an install with no
// config.yaml serves a setup wizard rather than refusing to start) holds
// no configuration, so it has nothing to read a journal path out of.
// apps/generic used to announce itself only in its Activate callback,
// after its own POST had written that configuration, so for the whole of
// the setup flow there was nothing to find and no lock-based detector
// could find it. A CLI `backup-set create` on such a host took
// createFirstConfig's path, wrote the first configuration underneath the
// wizard, exited 0 and printed the set, and the wizard went on serving
// setup and answering 503 until it was restarted. That is #571, found on
// a real NAS on the ordinary path an operator installing fresh and
// configuring from the command line walks down.
//
// It is closed from the other end, which is where the missing fact was. A
// process about to serve does not need a configuration to know which
// deployment it is going to serve: --state-database names the journal, it
// is the same value the configuration setup writes will carry, and it is
// the same packaged default this binary's own --state-database has. So
// apps/generic announces about that journal BEFORE it serves the setup
// flow (core/service's AnnounceServingFirstRun), and the question
// createFirstConfig already asked, about the journal rather than about a
// configuration that is not there, now has something to find.
//
// The refusal is the answer here rather than a route, and that is
// deliberate rather than a limitation left standing. A route only helps a
// command that takes one, and the first-configuration write takes none
// (mode.go's enterFirstConfigWriteMode): the request it would carry is
// POST /system/first-run, which two of the three shapes that reach here
// are already past, and the third is a setup flow the operator is
// standing in front of. Refusing leaves config.yaml untouched and leaves
// that flow up, which is a deployment somebody can still finish.
//
// # And why reads are left alone
//
// A CLI read beside a live engine is ordinary use of this binary and
// always has been: core/service's startup.go takes the journal lock
// SHARED for exactly that reason, so `backupd status` next to a
// running `serve` keeps working. Nothing here may narrow that. Only a
// write that lands in config.yaml is at risk of being believed by one
// process and not the other.
//
// #544 qualified that in exactly one place, and the qualification is
// worth stating beside the claim rather than only next door. A read still
// always answers when the two processes hold the same configuration, and
// still answers, saying so, when it cannot reach the engine at all. What
// it will not do is print an answer while the serving process is holding
// a DIFFERENT configuration, because that answer describes a deployment
// nobody is running, which is #535 seen from the surface an operator
// actually reads.

// configIntent is what a subcommand is about to do to config.yaml, named
// at every openBackupService call site.
//
// It is a named string rather than a bool deliberately. `validate` once
// passed withTransport=false at the neighbouring call and thereby never
// reached a storage medium at all, which is what a bare boolean at a
// choke point costs: the wrong value is invisible at the call site and
// silently permissive. Here the wrong value is worse than a missing
// feature, because it is #535 coming back, so the argument says out loud
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

// engineRefusal is the sentence both configuration-write routes print
// when they find a serving process, so the two cannot drift into telling
// an operator different things about the same situation. Its two callers
// are mode.go's enterConfigWriteMode and enterFirstConfigWriteMode, which
// differ in whether they can name the file the engine read and in which
// remedy is true for them.
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
// serves", which on a host running `backupd daemon` names two
// things that are not there: the daemon serves no HTTP at all. Stopping
// the process is the one answer that is true on every deployment, so it
// is the one every remedy below starts with, and the rest is offered as
// the conditional it actually is.
//
// The remedy is the caller's rather than this function's, and #571 is why.
// It used to be one sentence for both, ending in $BACKUP_MANAGER_API_URL
// and the four verbs an address can carry, and that reads as an
// instruction to somebody who has just been refused. On a first
// configuration it is an instruction into a loop: no address carries a
// first configuration, so an operator who set all three variables and ran
// the command again got this identical refusal. That path was rare before
// #571 and is now the one every fresh install takes, so the two remedies
// are separated and each is true where it is printed. What stays shared is
// the head, which is the part that must not drift: what was found, and that
// nothing was written.
func engineRefusal(engine *service.RunningEngine, because, remedy string) error {
	return engineHeld{fmt.Errorf(
		"another process is already serving this deployment (state database %s), so nothing was written: a configuration change made here would never reach it, because %s. %s",
		engine.StateDatabase, because, remedy)}
}

// routableRemedy is what an operator can do about a write an address could
// have carried: the three mutating backup-set verbs and `settings patch`
// (#543, route.go).
//
// The four are named rather than summarised, so an operator who reaches
// this refusal from one of the others is not sent to set a variable that
// will not help them. It is offered rather than instructed because it is
// not true everywhere either: a `daemon` has no listener to point at.
var routableRemedy = fmt.Sprintf(
	"Stop that process and run this command again; if it serves this deployment's Web UI or HTTP API, the change can be made there instead, and `backup-set create`, `backup-set patch`, `backup-set remove` and `settings patch` can be handed to it directly by setting $%s (with $%s and $%s) to the address it serves",
	apiURLEnv, apiUsernameEnv, apiPasswordEnv)

// firstConfigRemedy is what an operator can do about the one configuration
// write no address can carry.
//
// It says so out loud rather than leaving the variable unmentioned. An
// operator who has met the routable remedy once, or read it in the usage
// block, will reach for $BACKUP_MANAGER_API_URL here, and being told
// plainly that it is not the answer for this one write is shorter than
// finding out by setting it.
var firstConfigRemedy = fmt.Sprintf(
	"Stop that process and run this command again; if it is a fresh install still serving its setup flow, that flow writes this deployment's first configuration and is the place to do it. $%s cannot carry this one: a first configuration is the one write with no route, so setting it and running this again gets this same refusal",
	apiURLEnv)

// errEngineHoldsDeployment is the fact exit code 3 reports, carried on the
// error so `fail` can recognise it (issue #551).
//
// A sentinel rather than a string match on the sentence above. Matching
// the sentence would work, because core/tests/compat pins it byte for
// byte, and it would be wrong for exactly that reason: the whole point of
// giving this refusal a code of its own is that a script branching on it
// never has to read the prose, and a binary that read its own prose to
// decide the code would be teaching the habit it exists to remove.
//
// It is deliberately not IN the sentence either. Wrapping with %w would
// append a clause an operator reads, which is a compatibility break under
// FR-35 clause 4 for a change nobody asked for, so engineHeld below
// attaches it without printing it.
var errEngineHoldsDeployment = errors.New("another process is serving this deployment")

// engineHeld marks an error as this refusal without altering a word of it.
//
// Unwrap returns both the error it was given and the sentinel, so the
// error keeps whatever chain it arrived with (which matters for the
// `daemon` path, where service.ErrAlreadyServing is underneath) and gains
// the one the exit code is read from.
type engineHeld struct{ err error }

func (e engineHeld) Error() string   { return e.err.Error() }
func (e engineHeld) Unwrap() []error { return []error{e.err, errEngineHoldsDeployment} }

// asEngineHeld marks the refusal a process gets when something else is
// already serving the deployment it was about to serve, and leaves every
// other error alone.
//
// It exists because `daemon` meets this fact from the other end. The
// configuration writes above find a serving process by probing for it;
// `daemon` finds one by trying to become it and being told no
// (service.AnnounceServing, which returns ErrAlreadyServing). That is the
// same news to a script, and the more retryable half of it: a supervisor
// replacing a container meets it whenever the outgoing process has not let
// go of the lock yet, and waiting is the right answer. The usage block
// already tells an operator a `daemon` is refused rather than started
// beside a serving process, so leaving it on the ordinary failure code
// while a configuration write refused for the identical reason exits 3
// would make that table wrong about the one command it names.
//
// Marked at the call site rather than inside `fail`, so `fail` keeps one
// rule (does this error carry the sentinel) rather than growing a list of
// other packages' sentinels that nobody would think to update.
func asEngineHeld(err error) error {
	if errors.Is(err, service.ErrAlreadyServing) {
		return engineHeld{err}
	}
	return err
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
