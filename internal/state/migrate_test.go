package state

import (
	"context"
	"database/sql"
	"errors"
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
