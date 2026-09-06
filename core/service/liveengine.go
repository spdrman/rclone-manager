package service

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// Issue #537, Phase 1 of #536: telling whether an engine is already
// running against a deployment, and holding a door open long enough that
// the answer is still true when it is acted on.
//
// This exists because of #535. A `backup-set create` run through
// `docker exec` against a container that was already serving wrote
// config.yaml, adopted the change in its own memory, exited, and reported
// success. The engine had read that file an hour earlier and has no
// watcher on it, so the Web UI went on showing the old world. Two
// processes, two beliefs, and nothing anywhere noticing.
//
// # An engine, not a journal holder
//
// The obvious mechanism is the wrong one, and it was tried first. The
// `.journal-lock` startup.go already keeps is taken SHARED by every
// process that has the journal open, which is every `backup-manager
// status`, every `sources`, and every cron `run` for the length of a
// whole backup cycle. lock_unix.go says so in as many words: a `status`
// alongside a live `serve` is ordinary use of this CLI. Reading that lock
// as "an engine is running" refuses configuration writes on hosts where
// no engine exists at all, and #537 forbids exactly that: reporting an
// engine that is not there strands the CLI on a host with nothing
// running, which is the case the direct path exists for.
//
// So a process that is going to SERVE says so, in a lock of its own
// (AnnounceServing below, `.serving-lock`), before it reads anything.
// Nothing else takes that lock, so a held serving lock means one thing
// only, and a CLI probing it can never find itself. The holder takes it
// EXCLUSIVELY and an asker only ever SHARED, which is also what stops two
// concurrent askers from mistaking each other for an engine.
//
// # Why a lock rather than a pid file or an API probe
//
// A pid file goes stale the moment a container is killed, and this
// product is deployed under restart policies that kill containers
// routinely. An API probe needs a port, a scheme and a credential to be
// known before the configuration has been read, and answers about a
// listener rather than about the deployment, so it would miss an engine
// whose API is not up yet and find a stale one that is. A `backup-manager
// daemon` serves no HTTP at all and would be invisible to it. The lock is
// held by the kernel on behalf of a live process and released by the
// kernel when that process dies, however it dies.
//
// # Asking is not enough: the answer has to still be true
//
// A probe on its own is a sample, and a sample taken before a startup
// sequence, an SSH host-key probe over the network and a key import is a
// sample about a world that has had seconds to change. That gap was
// demonstrated: a `backup-set retention --policy-file -` parked on a fifo
// with nothing running, an engine started while it waited, stdin then fed,
// exit 0, config.yaml rewritten underneath the running process.
//
// BeginConfigWrite is the answer to that. It takes the `.startup-lock`
// exclusively and keeps it for the whole write, and every engine start
// must take that same lock before it can finish starting. So the two are
// mutually exclusive rather than merely ordered: an engine that got there
// first is holding its serving lock by the time this one is granted, and
// is found; an engine that arrives later cannot finish starting until the
// write is done, and reads the new file when its supervisor restarts it.
//
// # What it deliberately does not report
//
// A pid. flock(2) offers no portable way to ask WHICH process holds a
// lock: Linux would give it up through /proc/locks, macOS has no
// equivalent, and an operator-visible message that carries a pid on one
// platform and not the other is worse than one that carries none. So the
// engine is named by the thing it was actually found holding, which is
// also the thing an operator needs in order to go and find it.

// RunningEngine describes a process that has announced itself as serving
// this deployment and is still serving it. Its presence is the whole
// answer: there is no "maybe", because the field below is a fact the
// kernel reported rather than an inference.
//
// # Whether it can be this process
//
// It can, and saying "another process" would be a claim this package
// cannot make. flock(2) attaches to the open file description, so a
// process that took the serving lock itself and then asks about it finds
// itself. That is not a defect and it is not reachable from the CLI,
// which never announces: the only callers that announce are a `daemon` or
// a web host, which are the engine, and neither of them asks. A caller
// that does both has to know that it will find itself, which is why this
// says what is true (somebody serves this deployment) rather than
// something friendlier that is not.
type RunningEngine struct {
	// StateDatabase is the journal that process serves, spelled exactly
	// as the configuration spells it, so an operator reading a refusal
	// can match it against their own config.yaml and their own container.
	StateDatabase string

	// LockPath is the advisory lock file whose contention answered the
	// question. Kept beside the answer so a caller diagnosing a surprising
	// verdict has the evidence rather than only the conclusion.
	LockPath string
}

// DetectRunningEngine reports the engine serving the deployment that
// configPath names, or nil when nothing serves it.
//
// It is non-destructive by construction (servingLockHeld, lock_unix.go,
// spells out both halves of that): it creates no file to
// answer the question, and it gives back the lock it borrows the instant
// it has an answer. A serving process that collides with a probe waits
// the microseconds out rather than failing (flockWithin), so asking
// whether an engine is running can never be the reason one stops working.
//
// # Errors, and the one shape that is not an error
//
// A probe that cannot be performed is returned as an error, and every
// caller should treat that as a refusal rather than as a "no". "No" is
// the answer that lets #535 happen again, and lock_other.go's own rule (a
// safety condition this codebase cannot honestly assess is reported,
// never quietly skipped) is exactly about this. The probe does fail in
// production: EACCES on a lock file owned by another uid, ENOTSUP where
// flock is unavailable, EIO on a sick volume, and the whole non-unix
// build.
//
// A configuration that cannot be READ is different, and comes back as
// (nil, nil). There is no journal path to probe without it, and nothing
// this answer feeds can write without it either: every caller's next step
// is to open that same file, which fails on its own terms with the error
// an operator actually needs (ErrConfigAbsent for a deployment that has
// not been set up, config.Load's own complaint for one that is set up
// wrongly). Returning a locking error here would replace those with a
// complaint about a lock file for a journal nobody has named yet.
func DetectRunningEngine(configPath string) (*RunningEngine, error) {
	dbPath, ok := journalNamedBy(configPath)
	if !ok {
		return nil, nil
	}
	return DetectRunningEngineForJournal(dbPath)
}

// DetectRunningEngineForJournal is DetectRunningEngine for a caller that
// knows the journal but has no configuration to read it out of.
//
// That caller is `backup-set create` on a host with no config.yaml, which
// is the shape #535 gets in through when the file is absent for a reason
// nobody intended: a mistyped --config, or a config.yaml renamed out from
// under a running engine. Both write a whole new first configuration that
// nothing will ever read, and both are invisible to a check that starts
// by loading a configuration that is not there. The journal is the one
// thing still known in that state, because --state-database names it and
// carries the same packaged default the first-run wizard uses.
func DetectRunningEngineForJournal(dbPath string) (*RunningEngine, error) {
	if dbPath == "" {
		return nil, nil
	}
	lockPath := dbPath + servingLockSuffix
	held, err := servingLockHeld(lockPath)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, nil
	}
	return &RunningEngine{StateDatabase: dbPath, LockPath: lockPath}, nil
}

// AnnounceServing records that this process is about to serve the
// deployment configPath names, and returns the func that takes the
// announcement back. Every process that serves must call it: an engine
// nothing can find is an engine a configuration write lands behind.
//
// It is called BEFORE the configuration is opened, deliberately. What
// makes the guard in BeginConfigWrite airtight rather than merely narrow
// is that a serving process holds this lock before it takes the startup
// lock, so a writer that is granted the startup lock knows any engine
// that got there first is already visible to a probe.
//
// A configuration that cannot be read yields a no-op release and no
// error, for DetectRunningEngine's reason: the caller's next step is to
// open that same file, which reports the problem in the words an operator
// needs. A lock that cannot be TAKEN is an error, and fails the start:
// serving invisibly is the failure mode this whole file exists to
// prevent.
//
// # Where this deployment gets its name
//
// This is also the one place that mints a deployment identity (#555,
// deploymentidentity.go), and it is here because of who calls it: a
// process about to serve, and nothing else. It used to sit in
// runStartupSequence, which every CLI subcommand goes through, so a
// `backup-manager status` against a deployment whose identity file had
// gone missing renamed the deployment out from under the engine still
// serving it, and every routed write afterwards refused against that
// deployment's own engine.
//
// It is done after the lock is taken and before anything is opened. After
// the lock, so the read-then-write inside it has one process in it at a
// time. Before the open, because a client asks GET /system/version for
// this and an identity that only appeared once a schema migration had
// gone through would be missing on exactly the starts where the most is
// happening.
//
// Its failure does not fail the start, and that is the rule rather than a
// leniency: a deployment that refused to come up because it could not
// name itself would trade a routed write that gets refused for a whole
// deployment that is down. An engine that cannot name itself serves
// deployment_id "" and every routed write against it refuses, which is
// exactly the state every engine older than this build is in. So it is
// reported and stepped over.
func AnnounceServing(configPath string) (func() error, error) {
	dbPath, ok := journalNamedBy(configPath)
	if !ok {
		return func() error { return nil }, nil
	}
	lock, err := acquireServingLock(dbPath + servingLockSuffix)
	if err != nil {
		return nil, err
	}
	if _, err := ensureDeploymentIdentity(dbPath); err != nil {
		// The same sink and the same op Open logs its own startup
		// failures to, so a container that comes up unable to name itself
		// says why in the log an operator is already reading rather than
		// only through a refusal somebody meets later on another host.
		obs.New(os.Stdout, obs.LevelInfo).Error(context.Background(), "startup",
			fmt.Errorf("this deployment could not be given an identity, so clients cannot confirm which deployment they are writing to and every routed write against it will be refused: %w", err))
	}
	return lock.release, nil
}

// ConfigWriteGuard is a configuration write's exclusive claim on a
// deployment, held from before the engine check until after the bytes are
// on disk.
//
// It is the `.startup-lock` runStartupSequence already takes, borrowed
// from the other side. Holding it is what turns "no engine was running a
// moment ago" into "no engine can finish starting until I am done", which
// is the difference between a sample and mutual exclusion.
//
// It is not free and the cost is worth naming: while a write holds it,
// another process's startup sequence waits (startupLockWait, lock_unix.go)
// and then reports ErrStartupLocked. For a `backup-manager status` that
// wait is longer than the hold and nothing is felt. For a container
// starting at the exact moment of a `create --trust-host-key` that is
// dialling a source host, the start fails and the supervisor restarts it,
// which is a loud, recoverable outcome, and the alternative is the silent
// one: an engine that comes up holding a configuration the write is about
// to replace.
type ConfigWriteGuard struct {
	lock *startupLock
}

// BeginConfigWrite claims the deployment configPath names for a
// configuration write. The caller must Release it once the write is done,
// and must do the engine check (DetectRunningEngine) while holding it.
func BeginConfigWrite(configPath string) (*ConfigWriteGuard, error) {
	dbPath, ok := journalNamedBy(configPath)
	if !ok {
		// Nothing to claim, and nothing to protect: a caller that cannot
		// read the configuration cannot write one through this door
		// either, and the open that follows says so properly.
		return &ConfigWriteGuard{}, nil
	}
	return BeginConfigWriteForJournal(dbPath)
}

// BeginConfigWriteForJournal is BeginConfigWrite for the first-run write,
// which has no configuration to read the journal out of and takes it from
// --state-database instead.
func BeginConfigWriteForJournal(dbPath string) (*ConfigWriteGuard, error) {
	if dbPath == "" {
		return &ConfigWriteGuard{}, nil
	}
	lock, err := acquireStartupLockWithin(dbPath+startupLockSuffix, configWriteLockWait)
	if err == nil {
		return &ConfigWriteGuard{lock: lock}, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		// The directory this journal would live in is not there yet, so
		// there is no lock file to take and, more to the point, nothing
		// can be serving a journal inside a directory that does not
		// exist. That is a genuine first run on a bare host, which is
		// the case this whole path exists for, and refusing it because
		// a lock file could not be created in a directory the startup
		// sequence has not made yet would be the false positive #537
		// spends most of its text warning about.
		return &ConfigWriteGuard{}, nil
	}
	if errors.Is(err, ErrStartupLocked) {
		return nil, fmt.Errorf("another process on this host is starting this deployment or changing its configuration right now, so nothing was written; run this command again in a moment: %w", err)
	}
	return nil, err
}

// Release gives the claim back. Safe on a nil guard and on one that never
// took a lock, so a caller can defer it unconditionally.
func (g *ConfigWriteGuard) Release() error {
	if g == nil {
		return nil
	}
	return g.lock.release()
}

// journalNamedBy resolves configPath and reads the journal path out of
// it, reporting false for any configuration this process cannot get an
// answer out of.
//
// Resolved, not as supplied, for the reason OpenConfigAndJournal resolves
// before it stats: --config may name the configuration DIRECTORY the
// packaging mounts (#196), and this has to reach the same file the call
// that follows is about to open, or it would answer about a deployment
// nobody asked about.
func journalNamedBy(configPath string) (string, bool) {
	cfg, err := config.Load(config.ResolvePath(configPath))
	if err != nil {
		return "", false
	}
	if cfg.State.Database == "" {
		return "", false
	}
	return cfg.State.Database, true
}
