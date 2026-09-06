// Issue #537, Phase 1 of #536: telling whether an engine is already
// running against a deployment, from a process that is not it.
//
// This exists because of #535. A `backup-set create` run through
// `docker exec` against a container that was already serving wrote
// config.yaml, adopted the change in its own memory, exited, and reported
// success. The engine had read that file an hour earlier and has no
// watcher on it, so the Web UI went on showing the old world. Two
// processes, two beliefs, and nothing anywhere noticing.
//
// # Nothing new is invented here
//
// startup.go already keeps a `.journal-lock` beside the journal, taken
// SHARED by every process that has the journal open and EXCLUSIVE by one
// that is about to migrate, precisely so that "nobody else has this
// journal open" is (its words) something the kernel answers rather than
// something this package assumes. That is the same question this file
// asks, from the other side: try the exclusive lock, and if it will not
// come, somebody has this journal open.
//
// Which is a stronger answer than the alternatives that were on the
// table. A pid file goes stale the moment a container is killed, and this
// product is deployed under restart policies that kill containers
// routinely. An API probe needs a port, a scheme and a credential to be
// known before the configuration has been read, and answers about a
// listener rather than about the journal, so it would miss an engine
// whose API is not up yet and find a stale one that is. The lock is held
// by the kernel on behalf of a live process and released by the kernel
// when that process dies, however it dies.
//
// # What it deliberately does not report
//
// A pid. flock(2) offers no portable way to ask WHICH process holds a
// lock: Linux would give it up through /proc/locks, macOS has no
// equivalent, and an operator-visible message that carries a pid on one
// platform and not the other is worse than one that carries none. So the
// engine is named by the thing it was actually found holding, which is
// also the thing an operator needs in order to go and find it.
package service

import (
	"github.com/spdrman/rclone-manager/core/internal/config"
)

// RunningEngine describes a process, other than this one, that currently
// has this deployment's journal open. Its presence is the whole answer:
// there is no "maybe", because the field below is a fact the kernel
// reported rather than an inference.
type RunningEngine struct {
	// StateDatabase is the journal that process holds, spelled exactly as
	// the configuration spells it, so an operator reading a refusal can
	// match it against their own config.yaml and their own container.
	StateDatabase string

	// LockPath is the advisory lock file whose contention answered the
	// question. Kept beside the answer so a caller diagnosing a surprising
	// verdict has the evidence rather than only the conclusion.
	LockPath string
}

// DetectRunningEngine reports the process holding the journal that
// configPath names, or nil when nothing holds it.
//
// It is non-destructive by construction (journalHeldByAnotherProcess,
// lock_unix.go, spells out both halves of that): it creates no file to
// answer the question, and it gives back the lock it borrows the instant
// it has an answer, so asking whether an engine is running can never be
// the reason one stops working.
//
// # Errors, and the one shape that is not an error
//
// A probe that cannot be performed is returned as an error, and every
// caller should treat that as a refusal rather than as a "no". "No" is
// the answer that lets #535 happen again, and lock_other.go's own rule --
// a safety condition this codebase cannot honestly assess is reported,
// never quietly skipped -- is exactly about this.
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
	// Resolved, not as supplied, for the reason OpenConfigAndJournal
	// resolves before it stats: --config may name the configuration
	// DIRECTORY the packaging mounts (#196), and this has to reach the
	// same file that call is about to open, or it would answer about a
	// deployment nobody asked about.
	cfg, err := config.Load(config.ResolvePath(configPath))
	if err != nil {
		return nil, nil
	}
	dbPath := cfg.State.Database
	if dbPath == "" {
		return nil, nil
	}

	lockPath := dbPath + journalLockSuffix
	held, err := journalHeldByAnotherProcess(lockPath)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, nil
	}
	return &RunningEngine{StateDatabase: dbPath, LockPath: lockPath}, nil
}
