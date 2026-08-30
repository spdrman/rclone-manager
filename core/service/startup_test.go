// This file covers docs/EPIC-B-multi-nas.md §46.1's startup sequence at
// OpenConfigAndJournal/Open's own level: issue #104 (B3.4)'s RED
// checklist items about a migration failure leaving readiness false and
// starting no BackupService, and an unsupported downgrade failing closed
// without OpenConfigAndJournal bypassing internal/state's own refusal.
package service

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/spdrman/rclone-manager/core/internal/state"
)

// TestOpenConfigAndJournal_UnreadableDatabaseFile_NeverReturnsAJournal is
// this issue's stand-in for "a migration failure leaves readiness false
// and starts no BackupService": a database file this process cannot
// write to makes internal/state.Open fail during its own PRAGMA/migrate
// setup, and OpenConfigAndJournal must propagate that as a fatal error
// with a nil *state.Journal — the one thing Open (and every
// cmd/backup-manager subcommand via openService) already treats as "do
// not construct a BackupService", which is exactly "BackupService...
// never start" from the issue's own Given/When/Then.
func TestOpenConfigAndJournal_UnreadableDatabaseFile_NeverReturnsAJournal(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: permission bits do not block root, this check is meaningless here")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	if err := os.WriteFile(dbPath, []byte{}, 0o400); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, journal, err := OpenConfigAndJournal(context.Background(), writeConfigFileFor(t, dir, dbPath))
	if err == nil {
		if journal != nil {
			_ = journal.Close()
		}
		t.Fatal("OpenConfigAndJournal against an unwritable database file: error = nil, want an error")
	}
	if journal != nil {
		t.Fatal("OpenConfigAndJournal returned a non-nil *state.Journal alongside an error — a failed startup sequence must never hand back a usable journal")
	}
}

// TestOpenConfigAndJournal_NewerSchema_FailsClosedWithoutBypassingTheExistingGuard
// is the RED checklist's "a downgrade against a schema newer than the
// running binary's migrations fails closed": internal/state already
// refuses this (ErrUnknownSchemaVersion); this proves OpenConfigAndJournal
// propagates that refusal rather than working around it — e.g. by
// discarding and recreating the database, which would silently destroy
// whatever the newer version had written.
func TestOpenConfigAndJournal_NewerSchema_FailsClosedWithoutBypassingTheExistingGuard(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	// Build a real, fully migrated journal first, then seed a bogus row
	// claiming a migration version no binary in this test run has
	// compiled in — simulating "a newer build already migrated this
	// database further than this one understands".
	journal, err := state.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("state.Open (seed): %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close seed journal: %v", err)
	}
	seedFutureSchemaVersion(t, dbPath)

	_, journal2, err := OpenConfigAndJournal(context.Background(), writeConfigFileFor(t, dir, dbPath))
	if !errors.Is(err, state.ErrUnknownSchemaVersion) {
		if journal2 != nil {
			_ = journal2.Close()
		}
		t.Fatalf("OpenConfigAndJournal error = %v, want errors.Is(_, state.ErrUnknownSchemaVersion)", err)
	}
	if journal2 != nil {
		t.Fatal("OpenConfigAndJournal returned a non-nil *state.Journal for an unknown-schema-version database")
	}
}

// TestOpenConfigAndJournal_ConcurrentStartup_SecondCallRefused is the RED
// checklist's "lock service initialization" step exercised through the
// real entry point: while one caller's startup lock is held, a second
// OpenConfigAndJournal against the SAME state directory must be refused
// rather than race the first into snapshotting/migrating concurrently.
func TestOpenConfigAndJournal_ConcurrentStartup_SecondCallRefused(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	// Stand in for "another process's OpenConfigAndJournal call is
	// currently inside its own startup sequence" by holding the exact
	// lock file OpenConfigAndJournal itself acquires.
	held, err := acquireStartupLock(dbPath + startupLockSuffix)
	if err != nil {
		t.Fatalf("acquireStartupLock: %v", err)
	}
	defer func() { _ = held.release() }()

	_, journal, err := OpenConfigAndJournal(context.Background(), writeConfigFileFor(t, dir, dbPath))
	if !errors.Is(err, ErrStartupLocked) {
		if journal != nil {
			_ = journal.Close()
		}
		t.Fatalf("OpenConfigAndJournal error = %v, want errors.Is(_, ErrStartupLocked)", err)
	}
	if journal != nil {
		t.Fatal("OpenConfigAndJournal returned a non-nil *state.Journal while the startup lock was held by someone else")
	}
}

// TestOpenConfigAndJournal_SucceedsAndReleasesTheLockForALaterCaller
// proves the lock is scoped to one OpenConfigAndJournal call, not held
// for a caller's whole process lifetime: a normal successful call must
// leave the lock free for whatever calls OpenConfigAndJournal next (a
// second CLI invocation run alongside an already-started daemon, the
// ordinary case this codebase's CLI + long-running `serve` process are
// both meant to support at the same time).
func TestOpenConfigAndJournal_SucceedsAndReleasesTheLockForALaterCaller(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := writeConfigFileFor(t, dir, dbPath)

	_, journal, err := OpenConfigAndJournal(context.Background(), configPath)
	if err != nil {
		t.Fatalf("first OpenConfigAndJournal: %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close first journal: %v", err)
	}

	_, journal2, err := OpenConfigAndJournal(context.Background(), configPath)
	if err != nil {
		t.Fatalf("second OpenConfigAndJournal (must not still be locked by the first): %v", err)
	}
	if err := journal2.Close(); err != nil {
		t.Fatalf("close second journal: %v", err)
	}
}

// writeConfigFileFor writes a minimal, valid config.yaml pointing at
// dbPath, reusing writeTestConfigFile's own local/remote fixture shape
// (open_test.go) but with a caller-chosen dbPath and directory, since
// this file's tests need to control exactly which database file gets
// opened (an already-seeded one, an unwritable one, ...), unlike
// writeTestConfigFile's always-fresh one.
func writeConfigFileFor(t *testing.T, dir, dbPath string) string {
	t.Helper()
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + dbPath + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + localDir + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// seedFutureSchemaVersion inserts a schema_migrations row for a version
// number no binary in this test run has an embedded migration file for,
// simulating a database a newer build already migrated further than this
// one understands. internal/state.Journal exposes no generic Exec (by
// design — see that package's own doc), so this opens dbPath directly
// with the same driver Journal itself uses, exactly like a real "a
// different, newer binary already touched this database" scenario would
// have happened outside this process entirely.
func seedFutureSchemaVersion(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open (seed future version): %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(
		`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		999999, "from-the-future", "deadbeef", "2099-01-01T00:00:00Z",
	); err != nil {
		t.Fatalf("seeding future schema_migrations row: %v", err)
	}
}

// TestOpen_FailedStartupSequence_ConstructsNoBackupService is this
// issue's REGRESSION claim at the level the Given/When/Then actually
// states it: not merely "OpenConfigAndJournal returns an error", but
// "BackupService and the daemon/API never start". Open is the one
// production constructor a web host has (apps/generic/cmd/
// backup-manager-web/main.go calls it, and returns a non-zero exit
// without ever reaching serve.RunEngine if it fails), so a nil
// *BackupService out of Open is exactly "no scheduler tick, no cycle, no
// transfer, no delete" — there is no object left for any of those to be
// called on.
//
// Both §46.1 failure modes are covered: a state directory this process
// cannot use at all, and an on-disk schema newer than this binary's
// migrations (internal/state's own ErrUnknownSchemaVersion, which this
// package propagates rather than working around).
func TestOpen_FailedStartupSequence_ConstructsNoBackupService(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: permission bits do not block root, the unwritable-directory case is meaningless here")
	}

	tests := []struct {
		name string
		// setup returns the config path to open, having arranged for the
		// startup sequence to fail in this case's own way.
		setup func(t *testing.T) string
	}{
		{
			name: "state directory is a plain file, not a directory",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				notADir := filepath.Join(dir, "not-a-dir")
				if err := os.WriteFile(notADir, []byte("this is a file"), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return writeConfigFileFor(t, dir, filepath.Join(notADir, "state.db"))
			},
		},
		{
			name: "on-disk schema is newer than this binary's migrations",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				dbPath := filepath.Join(dir, "state.db")
				journal, err := state.Open(context.Background(), dbPath)
				if err != nil {
					t.Fatalf("state.Open (seed): %v", err)
				}
				if err := journal.Close(); err != nil {
					t.Fatalf("close seed journal: %v", err)
				}
				seedFutureSchemaVersion(t, dbPath)
				return writeConfigFileFor(t, dir, dbPath)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, cleanup, err := Open(context.Background(), tc.setup(t))
			if err == nil {
				if cleanup != nil {
					_ = cleanup()
				}
				t.Fatal("Open against a failed startup sequence: error = nil, want an error")
			}
			if svc != nil {
				t.Error("Open returned a non-nil *BackupService alongside an error — a failed startup sequence must never hand back something a scheduler, cycle, transfer or delete could be driven from")
			}
			if cleanup != nil {
				t.Error("Open returned a non-nil cleanup func alongside an error — there is nothing to clean up, and a caller deferring it would be closing a journal that was never opened")
			}
		})
	}
}
