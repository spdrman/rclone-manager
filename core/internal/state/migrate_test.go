package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func openRaw(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// A brand new database should come up on the latest known schema with no
// error: this is the ordinary path every other test in this package relies
// on.
func TestMigrate_FreshDatabase(t *testing.T) {
	db, _ := openRaw(t)
	ctx := context.Background()

	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	known, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		t.Fatalf("appliedMigrations: %v", err)
	}
	if len(applied) != len(known) {
		t.Fatalf("applied %d migrations, want %d", len(applied), len(known))
	}
	for _, m := range known {
		if applied[m.version] != m.checksum {
			t.Fatalf("version %d: recorded checksum %q, want %q", m.version, applied[m.version], m.checksum)
		}
	}

	// The table FR-9 actually asks for should exist and be queryable.
	if _, err := db.ExecContext(ctx, `SELECT count(*) FROM artifacts`); err != nil {
		t.Fatalf("artifacts table missing after migrate: %v", err)
	}
}

// Running migrate twice against the same database must be a no-op the second
// time: this is what lets Open call migrate unconditionally on every process
// start without re-applying anything.
func TestMigrate_IsIdempotent(t *testing.T) {
	db, _ := openRaw(t)
	ctx := context.Background()

	if err := migrate(ctx, db); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

// A database that records a migration version this binary's embedded set
// does not contain means a newer (or different) build touched it. The
// runner must refuse rather than silently proceed as if that version did
// not matter.
func TestMigrate_RefusesUnknownFutureVersion(t *testing.T) {
	db, _ := openRaw(t)
	ctx := context.Background()

	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (99999, 'from-the-future', 'deadbeef', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert future migration row: %v", err)
	}

	err := migrate(ctx, db)
	if !errors.Is(err, ErrUnknownSchemaVersion) {
		t.Fatalf("migrate() error = %v, want ErrUnknownSchemaVersion", err)
	}
}

// A recorded migration whose checksum no longer matches this binary's copy
// of that migration file means the file changed after it was applied. The
// runner must refuse rather than guess which version is authoritative.
func TestMigrate_RefusesChecksumDrift(t *testing.T) {
	db, _ := openRaw(t)
	ctx := context.Background()

	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'not-the-real-checksum' WHERE version = 1`,
	); err != nil {
		t.Fatalf("corrupt checksum: %v", err)
	}

	err := migrate(ctx, db)
	if !errors.Is(err, ErrSchemaDrift) {
		t.Fatalf("migrate() error = %v, want ErrSchemaDrift", err)
	}
}

func TestParseMigrationFilename(t *testing.T) {
	tests := []struct {
		filename    string
		wantVersion int
		wantName    string
		wantErr     bool
	}{
		{"0001_init.sql", 1, "init", false},
		{"0042_add_retention.sql", 42, "add_retention", false},
		{"init.sql", 0, "", true},
		{"0001.sql", 0, "", true},
		{"abc_init.sql", 0, "", true},
		{"0000_zero.sql", 0, "", true},
	}
	for _, tt := range tests {
		version, name, err := parseMigrationFilename(tt.filename)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseMigrationFilename(%q): want error, got version=%d name=%q", tt.filename, version, name)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMigrationFilename(%q): unexpected error: %v", tt.filename, err)
			continue
		}
		if version != tt.wantVersion || name != tt.wantName {
			t.Errorf("parseMigrationFilename(%q) = (%d, %q), want (%d, %q)", tt.filename, version, name, tt.wantVersion, tt.wantName)
		}
	}
}

// TestPendingMigration_OnlyTrueWhenSomethingWouldActuallyBeApplied is the
// check core/service's startup sequence uses to decide whether to take a
// pre-migration snapshot at all. Getting it wrong in the "true" direction
// costs only a needless copy; getting it wrong in the "false" direction
// would migrate without one, so each case here states which side it pins.
func TestPendingMigration_OnlyTrueWhenSomethingWouldActuallyBeApplied(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	// A database that does not exist yet: Open would create it and apply
	// everything.
	pending, err := PendingMigration(ctx, dbPath)
	if err != nil {
		t.Fatalf("PendingMigration (nonexistent): %v", err)
	}
	if !pending {
		t.Fatal("PendingMigration on a nonexistent database = false, want true")
	}

	journal, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The same database, now fully migrated. This is the case that matters:
	// every start after the first, and every CLI command run against a
	// journal a daemon already migrated.
	pending, err = PendingMigration(ctx, dbPath)
	if err != nil {
		t.Fatalf("PendingMigration (migrated): %v", err)
	}
	if pending {
		t.Fatal("PendingMigration on a fully migrated database = true, want false: an ordinary start must not snapshot or arm a restore")
	}

	// And the positive control for that false: put the database back one
	// migration and it must report true again, so the false above is a
	// real reading rather than a function that always says no.
	db, err := sql.Open(driverName, dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec("DELETE FROM schema_migrations WHERE version = (SELECT MAX(version) FROM schema_migrations)"); err != nil {
		t.Fatalf("delete the newest applied migration: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	pending, err = PendingMigration(ctx, dbPath)
	if err != nil {
		t.Fatalf("PendingMigration (one migration missing): %v", err)
	}
	if !pending {
		t.Fatal("PendingMigration with a migration un-applied = false, want true")
	}
}

// applyUpTo brings db to exactly schema version v, bootstrapping the
// tracking table first, so a test can stand a database up at a version
// some released binary actually shipped.
func applyUpTo(t *testing.T, ctx context.Context, db *sql.DB, v int) {
	t.Helper()
	if _, err := db.ExecContext(ctx, bootstrapSchemaMigrationsSQL); err != nil {
		t.Fatalf("bootstrap schema_migrations: %v", err)
	}
	known, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range known {
		if m.version > v {
			return
		}
		if err := applyMigration(ctx, db, m); err != nil {
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
	}
}

// openLikeProduction opens a raw database with the exact pragma set
// internal/state.Open uses, foreign_keys = ON included. Every migration
// test that means to reproduce what a real deployment does has to open it
// this way: with foreign keys off (the driver default) the recreate-the-
// table migrations below all pass, which is precisely why nothing caught
// the bug this file's TestMigrate_AppliesToAPopulatedDatabaseAtEveryShippedVersion
// pins.
func openLikeProduction(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db, path := openRaw(t)
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			t.Fatalf("%s: %v", pragma, err)
		}
	}
	return db, path
}

// populateLikeADeployment writes the rows every real journal has: one
// artifact and one state_transitions row referencing it. The referencing
// row is the whole point. state_transitions has a foreign key to
// artifacts, and the migrations that widen artifacts.state (0002, 0006,
// and every future one that follows the same recreate-the-table recipe)
// DROP the artifacts table on the way past.
func populateLikeADeployment(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artifacts (id, source, backup_set, artifact_name, remote_path, local_path, state, discovered_at, updated_at, local_hash, local_hash_alg)
		VALUES (1, 'nas', 'daily', 'db.dump', '/remote/db.dump', '/local/daily/db.dump', 'COMPLETE',
		        '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z', 'abc123', 'sha256')`); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO state_transitions (artifact_id, idempotency_key, from_state, to_state, occurred_at)
		VALUES (1, 'k1', '', 'DISCOVERED', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert state transition: %v", err)
	}
}

// A migration has to apply to a database that has rows in it, which is
// every database that matters. Nothing in this package tested that before:
// every migration test here started from an empty database, and an empty
// database survives a DROP TABLE artifacts even with foreign keys on
// because there is no referencing row to violate anything.
//
// A populated one does not. v0.1.0 shipped schema version 4; 0005 and 0006
// landed after it, and 0006 recreates the artifacts table. So every
// deployment that had ever discovered a single artifact could not open its
// journal on the next release: Open enables foreign_keys, the implicit
// DELETE FROM artifacts inside DROP TABLE trips state_transitions' foreign
// key, and the whole migration is refused.
//
// This runs the check at every version a database could plausibly be
// sitting at, not just the released one, so the next migration that
// recreates a referenced table fails here rather than on somebody's NAS.
func TestMigrate_AppliesToAPopulatedDatabaseAtEveryShippedVersion(t *testing.T) {
	ctx := context.Background()
	known, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	latest := known[len(known)-1].version

	for _, from := range known {
		if from.version == latest {
			continue
		}
		t.Run(fmt.Sprintf("from_v%d", from.version), func(t *testing.T) {
			db, _ := openLikeProduction(t)
			applyUpTo(t, ctx, db, from.version)
			populateLikeADeployment(t, ctx, db)

			if err := migrate(ctx, db); err != nil {
				t.Fatalf("migrating a populated v%d database: %v", from.version, err)
			}

			var artifacts, transitions int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM artifacts`).Scan(&artifacts); err != nil {
				t.Fatalf("count artifacts: %v", err)
			}
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM state_transitions`).Scan(&transitions); err != nil {
				t.Fatalf("count state_transitions: %v", err)
			}
			if artifacts != 1 || transitions != 1 {
				t.Fatalf("after migrating: %d artifacts and %d transitions, want 1 and 1", artifacts, transitions)
			}
		})
	}
}

// The positive control for the test above: with foreign keys left at the
// driver's default (off), the very same populated upgrade passes. That is
// the reading that made the bug invisible, and pinning it here is what
// stops someone "fixing" the test by dropping the pragma from
// openLikeProduction.
func TestMigrate_PopulatedUpgradeAlsoPassesWithForeignKeysOff(t *testing.T) {
	ctx := context.Background()
	db, _ := openRaw(t)
	db.SetMaxOpenConns(1)

	applyUpTo(t, ctx, db, 4)
	populateLikeADeployment(t, ctx, db)

	var fk int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if fk != 0 {
		t.Fatalf("PRAGMA foreign_keys = %d on a raw handle, want 0: this control assumes the driver default is off", fk)
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrating a populated v4 database with foreign keys off: %v", err)
	}
}

// Foreign key enforcement is a property of the connection, and migrate
// turns it off for the duration of a migration on purpose (SQLite's own
// documented recipe for recreating a referenced table). It has to put it
// back: everything after Open depends on it, and a journal running with
// foreign keys silently off would accept an orphan placement row.
func TestMigrate_LeavesForeignKeyEnforcementAsItFoundIt(t *testing.T) {
	ctx := context.Background()

	for _, want := range []int{0, 1} {
		db, _ := openRaw(t)
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(fmt.Sprintf("PRAGMA foreign_keys = %d", want)); err != nil {
			t.Fatalf("set foreign_keys = %d: %v", want, err)
		}
		if err := migrate(ctx, db); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		var got int
		if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&got); err != nil {
			t.Fatalf("read foreign_keys: %v", err)
		}
		if got != want {
			t.Fatalf("after migrate, PRAGMA foreign_keys = %d, want %d", got, want)
		}
	}
}
