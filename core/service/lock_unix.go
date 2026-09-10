//go:build unix

package service

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// This file is the three advisory locks this package runs on, and the
// reason there are three of them rather than one.
//
// They answer different questions and are held for wildly different
// lengths of time. The startup lock serialises processes that are both
// inside runStartupSequence, and is dropped the moment that sequence
// ends. The journal lock is held for as long as a process has the journal
// open at all, shared by everyone who is merely reading it and taken
// exclusively only by a process about to change the schema. Folding those
// two into one lock would force a choice between two wrong answers: hold
// it for the process lifetime and a `rbm status` alongside a
// running `serve` becomes an error, or hold it for the startup sequence
// only and a migration can run underneath a process that finished
// starting hours ago.
//
// The serving lock is the third, and it exists because neither of the
// other two answers issue #537's question. "Somebody has this journal
// open" is not "an engine is running here": every `status`, every cron
// `run`, every `sources` holds the journal lock too, and the doc above
// says as much. So a process that is going to SERVE this deployment (the
// daemon, and the web host once it has a backend) says so in a lock of
// its own, and everything else can ask about that lock without asking
// about mere readers. See liveengine.go for what asks and why.
//
// flock(2) rather than a PID file, because the failure mode being
// designed for is the process not getting to run its cleanup. A container
// is expected to be killed (a restart policy, a rolling update, an
// operator losing patience), and a lock the kernel drops when the holder
// dies cannot leave a stale file that blocks every future start.
//
// # Waiting a little, rather than not at all
//
// Every acquisition here was once outright non-blocking, so that a
// startup queueing behind another process's lock was a container that
// said what was in its way rather than one that hung. That is still the
// intent, and the bound below is what keeps it: an acquisition retries
// for a few seconds and then reports exactly the error it would have
// reported immediately. What the bound buys is that a hold measured in
// milliseconds stops being a failure. A configuration write now holds the
// startup lock for its duration (liveengine.go's ConfigWriteGuard, which
// is how a write and an engine start are made mutually exclusive rather
// than merely sampled), and without a wait a `rbm status`
// that happened to land inside one would fail for no reason an operator
// could act on.

// ErrStartupLocked is returned by acquireStartupLock when another process
// already holds the section-46.1 startup lock for the same state
// directory — section 46.1's "lock service initialization" step refusing
// to let two processes snapshot and migrate the same journal
// concurrently. It is scoped to the startup sequence only (see
// acquireStartupLock's own doc): a second process is refused only while
// the first is actually inside that sequence, never for the lifetime of
// a running daemon, so an operator can still run `rbm status`
// (or any other read-only CLI command) against a journal a `serve`
// process already has open.
var ErrStartupLocked = errors.New("service: another process is already running this journal's startup sequence")

// startupLock is an OS-level advisory lock, held only for the duration of
// one startup sequence (runStartupSequence, startup.go, which is where
// exactly when it is acquired and released is spelled out).
type startupLock struct {
	f *os.File
}

// acquireStartupLock takes an exclusive, non-blocking flock(2) on lockPath
// (creating it if necessary). Unlike a PID-file-based lock, this is
// automatically released by the kernel if the holding process dies
// without calling release — including a hard kill — so a crashed or
// killed startup attempt can never leave a stale lock blocking every
// future restart forever, which matters for a container that is expected
// to be restarted routinely (Docker/UGOS restart policies, a rolling
// update) rather than always shut down cleanly.
func acquireStartupLock(lockPath string) (*startupLock, error) {
	return acquireStartupLockWithin(lockPath, startupLockWait)
}

// acquireStartupLockWithin is acquireStartupLock with the wait spelled
// out, for the one caller that needs a different one: a configuration
// write queueing behind another configuration write is ordinary (two
// operators, or an operator and a script), and failing it after the
// couple of seconds a starting container is worth waiting would turn an
// ordinary queue into a refusal.
func acquireStartupLockWithin(lockPath string, wait time.Duration) (*startupLock, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("service: open startup lock %s: %w", lockPath, err)
	}

	if err := flockWithin(int(f.Fd()), unix.LOCK_EX, wait); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrStartupLocked, lockPath)
		}
		return nil, fmt.Errorf("service: lock %s: %w", lockPath, err)
	}

	return &startupLock{f: f}, nil
}

// startupLockWait is how long an ordinary startup sequence retries for
// the startup lock before reporting ErrStartupLocked. It is deliberately
// short: the holds it exists to ride out are a configuration write's (a
// few milliseconds, or as long as a `create --trust-host-key` spends
// dialling a source host), and anything longer than this is a genuine
// snapshot-and-migrate that a second process should be told about rather
// than kept waiting behind.
const startupLockWait = 2 * time.Second

// configWriteLockWait is the same bound for a configuration write, which
// is allowed to queue for longer because what it is usually queueing
// behind is another configuration write rather than a migration, and
// because the alternative to waiting is refusing a write an operator
// typed.
const configWriteLockWait = 20 * time.Second

// flockRetryInterval is how often flockWithin re-asks. Small enough that
// the millisecond-scale holds it exists for are barely felt, large enough
// that a long wait is not a spin.
const flockRetryInterval = 20 * time.Millisecond

// flockWithin takes `how` on fd, retrying for as long as the lock is held
// by somebody else and wait has not run out, and returns exactly the
// error a single non-blocking attempt would have returned.
//
// It is a retry loop rather than a blocking flock(2) for one reason: a
// blocking acquisition cannot be bounded, and an unbounded one is the
// container that hangs instead of saying what is in its way. Every caller
// here still gets EWOULDBLOCK in the end, so every message this file
// produces is the same message it produced when nothing waited at all.
func flockWithin(fd int, how int, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := unix.Flock(fd, how|unix.LOCK_NB)
		if !errors.Is(err, unix.EWOULDBLOCK) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(flockRetryInterval)
	}
}

// release drops the lock and closes the underlying file handle. Safe to
// call on a nil *startupLock (a no-op), so a caller can defer it
// unconditionally even on a path that never successfully acquired one.
func (l *startupLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	// Best-effort explicit unlock before Close: Close alone would also
	// release the flock (it is associated with the open file description),
	// but being explicit here means a future refactor that keeps the file
	// open longer for an unrelated reason cannot silently turn into "the
	// lock is held longer than intended" without this line being touched.
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	return l.f.Close()
}

// ErrJournalInUse is returned by acquireExclusiveJournalLock when another
// process still holds this journal open while this one needs to migrate it.
//
// This is the other half of the section-46.1 locking story, and the half
// the startup lock above deliberately does not cover. The startup lock
// serialises two processes that are both inside runStartupSequence; it says
// nothing about a process that finished its own startup sequence long ago
// and is now running with the journal open. Migrating (and, worse,
// restoring a pre-migration snapshot over the top) underneath such a
// process would rename a new inode into place while that process still
// holds the old one open, so its subsequent writes would go to an unlinked
// file and vanish. Refusing to migrate is the fail-closed answer: an
// operator stops the running process and retries, and no data is at risk in
// the meantime.
var ErrJournalInUse = errors.New("service: another process still has this journal open, so it cannot be migrated right now")

// journalLock is an OS-level advisory lock on a journal's own lock file,
// taken SHARED by every process that opens the journal and held for as long
// as that journal stays open, and taken EXCLUSIVE only by a process that is
// about to snapshot and migrate.
//
// Shared-versus-exclusive is what lets both of the things this codebase
// wants be true at once: any number of processes can have the journal open
// together (an operator's `rbm status` alongside a live `serve`
// is ordinary use of this CLI), while a process that needs to CHANGE the
// schema can prove, with the kernel rather than with a convention, that it
// is the only one there.
type journalLock struct {
	f *os.File
}

// acquireSharedJournalLock takes a shared flock(2) on lockPath, which
// succeeds against any number of other shared holders and fails only while
// a migrating process holds the exclusive lock.
func acquireSharedJournalLock(lockPath string) (*journalLock, error) {
	return acquireJournalLock(lockPath, unix.LOCK_SH)
}

// acquireExclusiveJournalLock takes an exclusive flock(2) on lockPath. It
// fails with ErrJournalInUse if any other process currently has the journal
// open (holding the shared lock), which is precisely the condition under
// which migrating would be unsafe.
func acquireExclusiveJournalLock(lockPath string) (*journalLock, error) {
	return acquireJournalLock(lockPath, unix.LOCK_EX)
}

func acquireJournalLock(lockPath string, how int) (*journalLock, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("service: open journal lock %s: %w", lockPath, err)
	}

	if err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrJournalInUse, lockPath)
		}
		return nil, fmt.Errorf("service: lock %s: %w", lockPath, err)
	}

	return &journalLock{f: f}, nil
}

// downgradeToShared converts an exclusive journal lock into a shared one on
// the same open file description, so the process that just migrated keeps
// its journal open under the same shared lock every other process uses,
// instead of holding everyone else out for its whole lifetime.
//
// flock(2) does not promise the conversion is atomic, and it does not need
// to be here: the only thing that could slip into the gap is another
// process taking the exclusive lock, and to want it that process would have
// to find a migration pending, which the migration this call is the tail of
// has just applied.
func (l *journalLock) downgradeToShared() error {
	if l == nil || l.f == nil {
		return nil
	}
	if err := unix.Flock(int(l.f.Fd()), unix.LOCK_SH); err != nil {
		return fmt.Errorf("service: downgrading journal lock to shared: %w", err)
	}
	return nil
}

// release drops the lock and closes the underlying file handle. Safe to
// call on a nil *journalLock (a no-op), so a caller can defer it
// unconditionally.
func (l *journalLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	return l.f.Close()
}

// ErrAlreadyServing is returned by acquireServingLock when another
// process is already serving this deployment.
//
// Two engines over one journal is not a shape this product has: the
// packaged deployment runs one engine (`serve`) alongside a UI proxy that
// opens no journal at all, and the headless alternative is one `daemon`.
// Two of them would run two schedulers over one set of artifacts and hold
// two independent in-memory copies of one configuration, which is issue
// #535 with both halves live. Refusing the second is the fail-closed
// answer, and it is also the only way the lock below can be exclusive,
// which is what makes asking about it collision-free.
var ErrAlreadyServing = errors.New("service: another process is already serving this deployment")

// servingLock is the third lock, and the only one that means "an engine
// is running here". It is taken EXCLUSIVELY by a process that is about to
// serve this deployment (see AnnounceServing, liveengine.go) and held for
// as long as that process serves; nothing else ever takes it, and a
// prober only ever asks for it SHARED.
//
// Exclusive-for-the-holder and shared-for-the-asker is the inversion of
// the journal lock above, and it is deliberate. It is what makes two
// concurrent probes unable to see each other: two shared requests are
// compatible, so neither prober is ever mistaken for an engine. The
// arrangement it replaced (shared holder, exclusive prober) had exactly
// that defect, measured at 788 false positives in 40000 concurrent
// probes, each one a configuration write refused against a deployment
// nothing was serving.
//
// It also keeps the promise that asking can never stop an engine
// starting. A prober holds its shared lock for the microseconds the
// question takes, and a serving process that collides with one waits
// those microseconds out rather than failing (flockWithin, above).
type servingLock struct {
	f *os.File
}

// acquireServingLock announces that this process serves the deployment
// whose serving-lock file is lockPath, and returns the handle that gives
// that announcement back.
//
// The file is created if it is not there, which is what makes its ABSENCE
// a conclusive "nothing serves this deployment" for the probe below.
func acquireServingLock(lockPath string) (*servingLock, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("service: open serving lock %s: %w", lockPath, err)
	}
	if err := flockWithin(int(f.Fd()), unix.LOCK_EX, startupLockWait); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrAlreadyServing, lockPath)
		}
		return nil, fmt.Errorf("service: lock %s: %w", lockPath, err)
	}
	return &servingLock{f: f}, nil
}

// release drops the lock and closes the underlying file handle. Safe to
// call on a nil *servingLock (a no-op).
func (l *servingLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	return l.f.Close()
}

// servingLockHeld reports whether a process currently serves the
// deployment whose serving-lock file is lockPath.
//
// It is the read-only half of acquireServingLock: the arrangement was
// built so that "an engine is running here" is something the kernel
// answers rather than something this package assumes, and this asks the
// kernel that question without keeping the answer. If the SHARED lock
// comes, no exclusive holder was there and it is dropped again
// immediately; if it does not, one is.
//
// Shared rather than exclusive, and that is the whole trick. Two callers
// asking at once are compatible with each other, so a prober can never be
// mistaken for an engine by another prober, which the exclusive version
// of this got wrong 788 times in 40000 concurrent probes.
//
// # Why it must not create the lock file
//
// The open is O_RDONLY with no O_CREATE, unlike the acquires above. That
// is what makes the probe non-destructive rather than merely brief: a
// deployment that has never started has no lock file, and a probe that
// created one to find out would leave a file behind on every bare host it
// was asked about. An absent file is also a CONCLUSIVE answer rather than
// a guess, which is what makes leaving it alone free: every process that
// serves creates that file on the way in, so nothing can be serving a
// deployment whose serving-lock file does not exist.
//
// flock(2) does not care what mode the descriptor was opened in (unlike
// fcntl(2) record locks, which need write access for a write lock), so a
// read-only descriptor takes a perfectly real lock here.
//
// # The window this opens, said out loud
//
// Between the Flock below and its release, a process trying to ANNOUNCE
// itself would find its exclusive request refused. That window is a few
// microseconds wide, and acquireServingLock rides it out rather than
// failing on it (flockWithin), which is the difference between this probe
// being free and this probe being able to refuse the very thing it is
// asking about.
func servingLockHeld(lockPath string) (bool, error) {
	f, err := os.OpenFile(lockPath, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("service: open serving lock %s: %w", lockPath, err)
	}
	defer f.Close()

	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return true, nil
		}
		return false, fmt.Errorf("service: probing serving lock %s: %w", lockPath, err)
	}
	// Given back at once. Holding it for any longer than the question
	// takes would make asking whether an engine is running a reason for
	// one not to be able to start.
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false, nil
}
