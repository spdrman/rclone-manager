package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/obs"
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
// process that has the journal open, which is every `backupd
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
// whose API is not up yet and find a stale one that is. A `backupd
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

// DefaultStateDatabase is the SQLite journal a deployment that has not
// been told otherwise names, and it is ONE definition on purpose.
//
// Everything in this file rests on it. A first-run engine announces
// itself about the journal --state-database names (AnnounceServingFirstRun
// below), and a `backup-set create` typed on that host finds the
// announcement by asking about the journal ITS --state-database names, so
// "the CLI and the web host agree about which deployment is live" is
// exactly the statement "those two defaults are the same string". It used
// to be two constants in two modules with nothing comparing them: the
// claim held only while they happened to match, and a change to one of
// them broke #571's fix silently, on the one deployment shape where
// nothing else would notice.
//
// It is the packaged mount from container/compose.yaml (STATE_DIR ->
// /data/state), the path scripts/deploy/deploy_generic.py's
// render_config_yaml has always written, and the value an operator on a
// machine the installer just set up should never have to type.
//
// It is a deployment fact, never something an API caller supplies: see
// FirstRunDefaults' own doc for why that boundary matters.
const DefaultStateDatabase = "/data/state/state.db"

// StateDatabaseEnv is the environment variable that moves the journal, and
// it has to reach BOTH surfaces or it does the opposite of what an
// operator setting it means.
//
// The web host read it and the CLI did not, so a hand-tuned deployment
// that moved the journal moved the engine out of the CLI's sight: the
// engine announced about $STATE_DATABASE and the create asked about
// /data/state/state.db, found nothing, and wrote a first configuration
// behind a running wizard. That is #571 again, reached through the one
// setting that was supposed to be the supported way to move the journal.
const StateDatabaseEnv = "STATE_DATABASE"

// StateDatabaseDefault is the journal a process should assume when its own
// command line does not name one: what $STATE_DATABASE says, or the
// packaged path.
//
// Both commands' --state-database flags take their default from here and
// statedatabase_test.go fails if either one stops doing so, which is what
// makes the paragraphs above a check rather than an intention.
func StateDatabaseDefault() string {
	if v := os.Getenv(StateDatabaseEnv); v != "" {
		return v
	}
	return DefaultStateDatabase
}

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
// A process that goes on serving with no configuration, rather than
// exiting, must not take that no-op: an instance serving the first-run
// setup flow is serving, and #571 is what it costs when nothing can find
// it. AnnounceServingFirstRun below is the entry point for that caller,
// and a provider app that serves setup has to call it instead of this one.
//
// # Where this deployment gets its name
//
// This is also the one place that mints a deployment identity (#555,
// deploymentidentity.go), and it is here because of who calls it: a
// process about to serve, and nothing else. It used to sit in
// runStartupSequence, which every CLI subcommand goes through, so a
// `backupd status` against a deployment whose identity file had
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
	return announceServingJournal(dbPath)
}

// AnnounceServingFirstRun is AnnounceServing for the one process that
// starts serving before it has a configuration to name a journal with: a
// provider app on a fresh install, which serves the setup flow rather than
// exiting (#176) and gains its first configuration through an HTTP POST.
//
// # The gap it closes
//
// Issue #571, found on a real NAS. AnnounceServing above is a no-op on an
// instance with no configuration, because there is no journal to announce
// about, so a `backup-set create` typed at that host found nothing serving,
// took the first-configuration path, wrote config.yaml, exited 0 and
// printed the set. The engine went on serving setup and answering
// 503 not_ready until somebody restarted it, so the CLI showed a backup set
// the Web UI did not have. That is #535 in its original words, arriving on
// the ordinary path an operator installing fresh and configuring from the
// command line walks straight down.
//
// The fix is that a process about to serve knows which journal it will
// serve before it knows anything else: firstRunDatabase is the
// --state-database its own packaging fixes, and it is the same value the
// configuration setup writes will name (FirstRunDefaults.StateDatabase),
// so announcing about it now is announcing about the deployment this
// process is going to be. It is also the same value the CLI's own
// --state-database carries, which is what makes the announcement findable:
// DetectRunningEngineForJournal already asks about exactly this journal for
// exactly this reason.
//
// # What it does not do
//
// It does not invent a journal for a process that has one. A configuration
// that names a journal wins, always, so a configured deployment behaves
// exactly as it did before and firstRunDatabase is never looked at.
//
// And it can only be found by somebody asking about the same journal, so
// both ends read that path out of ONE definition: StateDatabaseDefault
// above, which is what this app's --state-database and the CLI's both
// default to, $STATE_DATABASE included. That last part was the hole PR
// #581's review found. The engine read the variable and the CLI did not,
// so an operator who moved the journal the supported way moved the engine
// out of the CLI's sight, and a `backup-set create` on that host went
// straight back to writing a configuration behind a running wizard. What
// gets past this now is the same thing that has always got past it: a
// --config or a --state-database typed by hand that names a deployment
// other than the one running.
//
// It does not guess for a configuration that EXISTS and cannot be read.
// That is a deployment that is set up wrongly rather than one that is not
// set up (ErrConfigAbsent draws the same line, and OpenConfigAndJournal
// draws it with the same stat on the same resolved path), and a process
// about to exit over a broken config.yaml has no business taking a lock on,
// and minting an identity beside, a journal that may not be the one that
// file names.
//
// # Why the state directory is made here, and what happens when it cannot be
//
// acquireServingLock creates its lock file, and it cannot create it in a
// directory that is not there. A first-ever start against a fresh volume is
// exactly that directory not being there, so this runs the same
// validateStateDir step §46.1's startup sequence runs, which creates a
// missing directory and refuses one that exists and cannot be used.
//
// A refusal there does NOT fail the start. It is carried back on the
// FirstRunServing this returns, and the whole argument for that is on the
// type: an install that cannot announce yet still serves its wizard, says
// why inside it, and refuses to be configured until the announcement can
// really be made.
//
// What DOES fail the start is ErrAlreadyServing, on either path, and any
// failure at all on the configured path. Neither of those is a volume an
// operator can fix from a browser.
func AnnounceServingFirstRun(configPath, firstRunDatabase string) (*FirstRunServing, error) {
	if dbPath, ok := journalNamedBy(configPath); ok {
		release, err := announceServingJournal(dbPath)
		if err != nil {
			return nil, err
		}
		return &FirstRunServing{release: release}, nil
	}
	if firstRunDatabase == "" || !configAbsent(configPath) {
		return &FirstRunServing{}, nil
	}
	s := &FirstRunServing{dbPath: firstRunDatabase}
	if err := s.announce(); err != nil {
		return nil, err
	}
	return s, nil
}

// FirstRunServing is a first-run process's announcement, and the one
// thing that announcement can be missing.
//
// It exists because of what announcing costs on the config-absent path: a
// lock file inside the state directory, which is the one thing a fresh
// install is most likely to have wrong. This type is what lets that be a
// refusal the operator READS rather than a start that fails.
//
// # Why a failed announcement does not fail a first-run start
//
// The first version of #571's fix refused the start, on AnnounceServing's
// own rule: an engine nothing can find is the failure this file exists to
// prevent. That rule is right for a CONFIGURED deployment and wrong here,
// and PR #581's review is where the difference got named.
//
// A configured engine that cannot announce is unsafe to run: a
// `backup-set create` beside it writes a configuration it will never
// read, which is #535 with both halves live, so refusing to start is the
// fail-closed answer and the operator has a CLI, a config.yaml and a
// running deployment to diagnose it with.
//
// A FIRST install has none of that. It has a browser pointed at a setup
// wizard and nothing else, so a read-only bind mount, a volume mounted
// after the service starts or a uid that cannot write /data/state turned
// a recoverable misconfiguration into a container restart loop whose only
// symptom is `docker logs`. Serving the wizard and saying what is wrong
// inside it is strictly more information than exiting, and it costs
// nothing that was there to lose: with no configuration and no announcement
// this deployment has nothing anybody could write behind, and the one
// write that would create something (setup itself) is refused by Blocked
// below until the announcement can actually be made.
//
// Two failures are still failures rather than degradations, and both come
// back from AnnounceServingFirstRun as errors. A configured deployment's
// announcement, for the paragraph above. And ErrAlreadyServing on either
// path, because that is not a broken volume, it is a second engine over
// one journal, and the answer to that has always been to refuse the second
// one (#551 gives it an exit code of its own).
type FirstRunServing struct {
	// dbPath is the journal this process will serve, and is empty for the
	// two shapes that have nothing to retry: a configured deployment
	// (announced once, at the top) and a process with no journal to name.
	dbPath string

	// mu guards the two fields below. Blocked is reachable from an HTTP
	// handler while Release is reachable from the process's own shutdown,
	// so the retry and the giving-back can genuinely race.
	mu       sync.Mutex
	release  func() error
	problem  error
	released bool
}

// Blocked reports what stops this deployment being set up, or nil when
// nothing does.
//
// It RETRIES the announcement rather than reporting a verdict taken at
// startup, and that is the whole point of the type. An operator who
// remounts the volume read-write, or fixes the ownership of /data/state,
// has to be able to finish setup in the wizard already on their screen;
// a verdict cached at boot would tell them to restart a container they
// have never seen a shell for.
//
// It is also what keeps #571 closed while the wizard is up. The moment
// this returns nil the deployment is announced, so the `backup-set
// create` that #571 was reported for finds this process rather than
// writing a first configuration behind it. Setup calls this before it
// writes anything, which is what makes "announced" and "allowed to be
// configured" the same instant rather than two.
func (s *FirstRunServing) Blocked() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.release != nil || s.dbPath == "" || s.released {
		return nil
	}
	if err := s.announceLocked(); err != nil {
		// Marked, unlike the identical error at startup. This is reached
		// from a setup submission rather than from a start, and what a
		// caller there needs is one thing to branch on: every reason this
		// deployment is not announced reaches the operator the same way,
		// through the wizard, in whatever words the reason itself used.
		return notAnnounced{err}
	}
	return s.problem
}

// announce makes the first attempt, and is the only one whose failure can
// fail the process's start.
func (s *FirstRunServing) announce() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.announceLocked()
}

// announceLocked tries to take the serving lock, recording an ordinary
// misconfiguration as a problem to be read and returning only the errors
// that must stop a start.
func (s *FirstRunServing) announceLocked() error {
	s.problem = nil
	// The state directory has to exist before a lock file can be created
	// in it, and a first-ever start against a fresh volume is exactly that
	// directory not being there, so this runs §46.1's own
	// "validate state directory" step: it creates a missing directory and
	// refuses one that exists and cannot be used.
	if err := validateStateDir(s.dbPath); err != nil {
		s.problem = notAnnounced{err}
		return nil
	}
	release, err := announceServingJournal(s.dbPath)
	if err != nil {
		if errors.Is(err, ErrAlreadyServing) {
			// Not a degradation. Two engines over one journal is the one
			// thing this lock exists to make impossible, and a supervisor
			// reading exit code 3 knows to wait and try again.
			return err
		}
		// Everything else is the volume rather than a second engine:
		// EACCES on a lock file owned by another uid, ENOTSUP where flock
		// is unavailable, EIO on a sick disk. A fresh install used to come
		// up on those filesystems and must go on doing so.
		s.problem = notAnnounced{err}
		return nil
	}
	s.release = release
	return nil
}

// Release gives the announcement back. Safe on a nil receiver and on one
// that never managed to announce, so a caller can defer it
// unconditionally.
func (s *FirstRunServing) Release() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Marked released whether or not there was anything to give back, so a
	// late Blocked (a setup submission racing this process's own shutdown)
	// cannot re-announce a deployment nobody is going to serve.
	s.released = true
	if s.release == nil {
		return nil
	}
	release := s.release
	s.release = nil
	return release()
}

// ErrNotAnnounced marks the refusal a first-run process hands to its own
// setup surface: this deployment could not be announced, so nothing may
// write its first configuration yet.
//
// A sentinel rather than a sentence, for the reason the CLI's
// errEngineHoldsDeployment gives: the caller branching on it (the setup
// handler, which turns it into an HTTP refusal) must never have to read
// the prose an operator reads.
var ErrNotAnnounced = errors.New("service: this deployment could not be announced, so it cannot be set up yet")

// notAnnounced marks an error as that refusal without altering a word of
// it, the same trick core/cmd/backupd's engineHeld plays.
//
// The words matter here more than usual: what validateStateDir says
// ("/data/state is not writable", "exists and is not a directory") is the
// entire diagnosis an operator gets, and it reaches them through the
// wizard. Wrapping it in a prefix of this package's own would push the
// useful half of the sentence further from the start of a message shown
// in a browser.
type notAnnounced struct{ err error }

func (e notAnnounced) Error() string   { return e.err.Error() }
func (e notAnnounced) Unwrap() []error { return []error{e.err, ErrNotAnnounced} }

// announceServingJournal is the announcement itself, shared by the two
// entry points above so that "which journal" is the only thing they can
// differ about.
func announceServingJournal(dbPath string) (func() error, error) {
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

// configAbsent reports whether configPath names nothing at all, which is
// the one startup state that means "not set up yet" rather than "set up
// wrongly" (firstrun.go's ErrConfigAbsent).
//
// The stat is on the RESOLVED path for OpenConfigAndJournal's own reason:
// --config may name the packaged configuration DIRECTORY (#196), and
// statting the directory would find it present on a completely empty
// install, so the one shape that most needs this answer is the one shape
// that would never get it.
func configAbsent(configPath string) bool {
	_, err := os.Stat(config.ResolvePath(configPath))
	return errors.Is(err, os.ErrNotExist)
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
// and then reports ErrStartupLocked. For a `backupd status` that
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
