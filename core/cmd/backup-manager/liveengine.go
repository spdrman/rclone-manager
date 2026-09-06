package main

import (
	"errors"
	"fmt"

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
// binary knows about, and no credential it holds.
//
// Phase 2 did not close it, and this is the place to say so rather than
// leave the old sentence promising that it would. A route only helps a
// command that takes one, and the first-configuration write deliberately
// takes none (mode.go's enterFirstConfigWriteMode): finding a process
// serving the journal `--state-database` names says the deployment is
// already configured, and POST /system/first-run is not an operation to
// send an already configured engine. On a wizard host there is nothing to
// find in the first place, because `AnnounceServing` on an instance with
// no configuration is a no-op and the announcement only happens in
// `Activate`, once setup has written one. So the shape survives EPIC #536
// intact, and closing it needs something this file cannot offer: an
// address for a process that has not yet decided what it serves.
//
// # And why reads are left alone
//
// A CLI read beside a live engine is ordinary use of this binary and
// always has been: core/service's startup.go takes the journal lock
// SHARED for exactly that reason, so `backup-manager status` next to a
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
// differ only in whether they can name the file the engine read.
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
//
// #543 added a third, and it is last for the same reason the second one is
// conditional. Four configuration writes can now be handed to a serving
// process that speaks HTTP, and the way to say where that process is is
// $BACKUP_MANAGER_API_URL (route.go). It is offered rather than instructed
// because it is not true everywhere: a `daemon` has no listener to point
// at, and `backup-set retention` and the first-configuration write have no
// route even when one does. The four are named rather than summarised, so
// an operator who reaches this refusal from one of the others is not sent
// to set a variable that will not help them.
func engineRefusal(engine *service.RunningEngine, because string) error {
	return engineHeld{fmt.Errorf(
		"another process is already serving this deployment (state database %s), so nothing was written: a configuration change made here would never reach it, because %s. Stop that process and run this command again; if it serves this deployment's Web UI or HTTP API, the change can be made there instead, and `backup-set create`, `backup-set patch`, `backup-set remove` and `settings patch` can be handed to it directly by setting $%s (with $%s and $%s) to the address it serves",
		engine.StateDatabase, because, apiURLEnv, apiUsernameEnv, apiPasswordEnv)}
}

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
