// Package state is the FR-9 lifecycle journal: a mandatory, authoritative
// SQLite database that records every artifact the manager has ever seen and
// every state transition it has ever made. The filesystem is never treated
// as the sole transactional record; if it isn't in this journal, later
// phases (verification, durable commit, remote delete, reconciliation,
// retention) are not allowed to assume it happened.
//
// This package deliberately does not decide backup policy. It doesn't know
// what a valid transition sequence looks like (that's the FR-10 state
// machine, owned elsewhere), it doesn't classify errors or decide retry
// timing (FR-22), and it doesn't compute GFS retention (FR-18). It only
// knows how to persist whatever those components decide, durably, and how
// to make a write survive a crash between "the transaction committed" and
// "the caller found out" without being applied twice. See RecordTransition
// for how that idempotency guarantee actually works.
//
// The one piece of vocabulary this package does own is the schema itself:
// the set of columns backing FR-9's list (artifact identity, backup set,
// remote path, local path, remote metadata, lifecycle state, timestamps,
// transfer results, hashes, validation results, retry information, remote
// deletion status, retention classification), and the migration runner in
// migrate.go that brings a database to that schema, or refuses to touch it,
// version by version.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	sqlitedriver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// driverName is modernc.org/sqlite's registered database/sql driver name.
//
// modernc.org/sqlite is a pure-Go SQLite implementation: it has no cgo
// dependency. That matters here specifically because the UGREEN deployment
// cross-compiles this binary with CGO_ENABLED=0 for both linux/amd64 and
// linux/arm64, and CI enforces that on every push. A cgo driver such as
// github.com/mattn/go-sqlite3 would build fine on the development machine
// and then fail the cross-compile, so it was never on the table.
const driverName = "sqlite"

// Journal is the FR-9 lifecycle journal. A Journal owns exactly one
// *sql.DB, and that *sql.DB is pinned to a single open connection (see
// Open): the journal is a single logical writer per process, so
// serializing every access in the Go driver, rather than merely reducing
// contention with WAL and a busy timeout, removes SQLITE_BUSY from the
// space of things this package has to handle at all.
type Journal struct {
	db *sql.DB

	// redact, when set, is the filter RecordTransition runs a Transition's
	// error/detail-shaped string fields through before they are written
	// down (issue #295). It is an atomic.Pointer, not a plain field,
	// because this same *Journal is reused, in place, across every
	// internal/app.New call a config hot-reload makes (see
	// service/backupsets.go, settings.go, backupsetenabled.go, all of
	// which call app.New again against the one long-lived Journal a
	// process opened at startup): a RunCycle already in flight in another
	// goroutine may be reading this concurrently with a reload calling
	// SetRedactor, and the *sql.DB single-connection serialization
	// (SetMaxOpenConns(1)) this type's own doc above describes says
	// nothing about ordinary Go field access, which happens before any
	// SQL is even built.
	redact atomic.Pointer[obs.Redactor]
}

// SetRedactor installs r as the filter RecordTransition (journal.go) runs
// a Transition's Detail, and any Deletion.Error it carries, through before
// either is written to the database, or turns filtering back off when r is
// nil. See obs.Redactor's own doc: the SAME value is normally handed to
// the process's obs.Logger too (see internal/app.New), so the log and the
// journal agree, byte for byte, about what a sensitive endpoint's rendered
// text is — issue #295's requirement that redacting one and not the other
// does not count as a fix.
//
// SetRedactor is not part of Open's constructor signature on purpose:
// this package does not import internal/config and has no notion of what
// config.Remote.Sensitive means, so wiring the two together stays a
// post-construction step performed by the one place that already holds
// both a *Journal and a *config.Config (internal/app.New), rather than a
// dependency this package would otherwise need to grow.
func (j *Journal) SetRedactor(r *obs.Redactor) {
	j.redact.Store(r)
}

// Open opens (creating if necessary) the SQLite database at path, brings its
// schema up to date through the embedded migrations in package migrations,
// and returns a ready Journal.
//
// Open refuses to proceed, rather than guess, when the database's recorded
// schema history is not one this binary understands: see
// ErrUnknownSchemaVersion and ErrSchemaDrift.
//
// Durability: Open enables WAL journaling and synchronous=FULL. WAL alone,
// at the more common synchronous=NORMAL setting, is durable across an
// application crash but not across an OS crash or power loss: in that mode
// SQLite only fsyncs the WAL file at checkpoint time, so a transaction can
// be reported as committed before the bytes recording it are actually on
// disk. FR-14's entire argument for when a remote delete is safe rests on
// "the journal says COMMITTED" meaning "this survives a crash right now",
// so this journal pays the fsync-per-commit cost of synchronous=FULL
// instead of the weaker default.
func Open(ctx context.Context, path string) (*Journal, error) {
	db, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("state: open %s: %w", path, err)
	}

	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("state: %s: %w", pragma, err)
		}
	}

	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}

	return &Journal{db: db}, nil
}

// Close releases the underlying database handle.
func (j *Journal) Close() error {
	return j.db.Close()
}

// now is a seam over time.Now so a future test can freeze the clock the
// migration runner uses for applied_at without this package needing to
// change; nothing currently overrides it.
var now = func() time.Time { return time.Now().UTC() }

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint
// failure, as opposed to any other kind of failure (I/O error, a different
// constraint, a closed database, ...). Callers use this to turn "the
// natural key already exists" into a specific, documented error
// (ErrAlreadyDiscovered) instead of a generic SQL failure.
func isUniqueViolation(err error) bool {
	var sqliteErr *sqlitedriver.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
