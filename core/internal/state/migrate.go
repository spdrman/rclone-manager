package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spdrman/rclone-manager/core/migrations"
)

// The migration runner: how a database reaches the schema this binary
// expects, and the two situations where it refuses to touch one at all.
//
// Schema changes are numbered files under core/migrations, embedded into
// the binary, applied in order, each inside its own transaction, each
// recorded alongside a sha256 of its own text. That checksum is not
// bookkeeping, it is the mechanism behind the rule that makes the whole
// scheme safe, and the rule is worth stating where somebody will hit it:
//
//	a migration file that has shipped can never be edited again,
//	comments and whitespace included.
//
// The sum covers the whole file, so correcting a stale comment moves it,
// and every deployment that already applied that version then refuses to
// open with ErrSchemaDrift. Not warns. Refuses, because a journal whose
// history this binary cannot account for is one it must not write to. Two
// files in core/migrations say something about foreign keys that stopped
// being true when suspendForeignKeys landed, and they are staying wrong
// for exactly this reason: the correction goes in this file, or into a new
// migration.
//
// TestShippedMigrationsAreImmutable pins the sum of every landed migration
// and fails loudly if one moves. It deliberately does not print the sum it
// computed, so the only way past it is to put the file back rather than to
// paste the new number over the old one.
//
// The runner never reconciles, downgrades or reapplies anything. A
// recorded version this binary does not carry means a different build has
// been here (ErrUnknownSchemaVersion); a recorded version whose text no
// longer matches means the rule above was broken (ErrSchemaDrift). Both
// stop Open before it returns a Journal to anyone.

// migration is one version-controlled schema change, loaded from a single
// migrations/NNNN_name.sql file.
type migration struct {
	version  int
	name     string
	filename string
	sql      string
	checksum string
}

// loadMigrations reads every embedded *.sql file, parses its leading version
// number, and returns them sorted by version. It fails closed on anything
// that looks like a mistake rather than silently picking an order: a
// malformed filename, or two files claiming the same version, would make
// "which migration is version 3" ambiguous, and this journal does not guess.
func loadMigrations() ([]migration, error) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("state: read embedded migrations: %w", err)
	}

	out := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		version, name, err := parseMigrationFilename(entry.Name())
		if err != nil {
			return nil, err
		}

		content, err := migrations.FS.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("state: read migration %s: %w", entry.Name(), err)
		}

		sum := sha256.Sum256(content)
		out = append(out, migration{
			version:  version,
			name:     name,
			filename: entry.Name(),
			sql:      string(content),
			checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })

	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("state: migrations %s and %s both claim version %d",
				out[i-1].filename, out[i].filename, out[i].version)
		}
	}

	return out, nil
}

// parseMigrationFilename splits "0001_init.sql" into version 1 and name
// "init". The version must be a positive integer so migrations sort and
// compare unambiguously; anything else is a naming mistake the runner
// refuses to interpret.
func parseMigrationFilename(filename string) (version int, name string, err error) {
	base := strings.TrimSuffix(filename, ".sql")
	numPart, rest, found := strings.Cut(base, "_")
	if !found || rest == "" {
		return 0, "", fmt.Errorf("state: migration file %q must be named NNNN_name.sql", filename)
	}

	v, convErr := strconv.Atoi(numPart)
	if convErr != nil || v <= 0 {
		return 0, "", fmt.Errorf("state: migration file %q must start with a positive integer version", filename)
	}

	return v, rest, nil
}

// bootstrapSchemaMigrationsSQL creates the migration-tracking table itself.
// It is not a numbered migration because it has to exist before any numbered
// migration can be recorded against it, so the runner creates it directly
// rather than through the same versioned mechanism it protects.
const bootstrapSchemaMigrationsSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TEXT NOT NULL
)`

// migrate brings db's schema up to date using the embedded migrations, or
// returns an error without changing anything.
//
// It refuses to proceed, rather than guess, in two cases: a recorded
// migration this binary's embedded set does not contain at all
// (ErrUnknownSchemaVersion, meaning a newer or different build touched this
// database), and a recorded migration whose checksum no longer matches the
// migration file compiled into this binary (ErrSchemaDrift, meaning the
// migration file's content changed after it was applied here). Both are
// refusals: this package does not attempt to reconcile, downgrade, or
// reapply anything in either case.
//
// The consequence worth stating out loud, because it is not obvious and it
// has already been proposed once as a harmless tidy-up: a migration file that
// has shipped can never be edited again, comments included. The checksum is
// taken over the whole file, so correcting a stale comment moves it, and
// every deployment that already applied that version then refuses to open
// with ErrSchemaDrift. Two of the files in this directory say something about
// foreign keys that stopped being true when suspendForeignKeys landed, and
// they are staying exactly as they are for that reason. If a landed migration
// needs a correction, the correction goes here or into a new migration.
// TestShippedMigrationsAreImmutable enforces this.
func migrate(ctx context.Context, db *sql.DB) error {
	known, err := loadMigrations()
	if err != nil {
		return err
	}

	byVersion := make(map[int]migration, len(known))
	for _, m := range known {
		byVersion[m.version] = m
	}

	if _, err := db.ExecContext(ctx, bootstrapSchemaMigrationsSQL); err != nil {
		return fmt.Errorf("state: bootstrap schema_migrations: %w", err)
	}

	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return err
	}

	for version, checksum := range applied {
		m, ok := byVersion[version]
		if !ok {
			return fmt.Errorf("%w: version %d is recorded in the database but not compiled into this binary",
				ErrUnknownSchemaVersion, version)
		}
		if m.checksum != checksum {
			return fmt.Errorf("%w: version %d (%s)", ErrSchemaDrift, version, m.name)
		}
	}

	pending := make([]migration, 0, len(known))
	for _, m := range known {
		if _, ok := applied[m.version]; ok {
			continue
		}
		pending = append(pending, m)
	}
	if len(pending) == 0 {
		return nil
	}

	restoreForeignKeys, err := suspendForeignKeys(ctx, db)
	if err != nil {
		return err
	}
	defer restoreForeignKeys()

	for _, m := range pending {
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}

	return nil
}

// suspendForeignKeys turns foreign key enforcement off for the duration of
// a migration run and returns the function that puts it back exactly as it
// was, whatever that was.
//
// This is SQLite's own documented procedure for "making other kinds of
// table schema changes" (sqlite.org/lang_altertable.html), and this schema
// needs it because it cannot alter a CHECK constraint in place: widening
// artifacts.state means creating a new table, copying the rows across,
// dropping the old one and renaming, which 0002 and 0006 both do. DROP
// TABLE runs an implicit DELETE FROM first, and with foreign keys on that
// delete trips every row in state_transitions pointing at the table being
// replaced.
//
// That is not hypothetical. Open (state.go) enables foreign_keys, v0.1.0
// shipped schema version 4, and 0006 landed after it, so every deployment
// that had ever discovered one artifact refused to migrate to the next
// release with "FOREIGN KEY constraint failed" and no journal at all. An
// empty database has no referencing rows and sails through, which is why
// this package's whole migration suite was green while the upgrade was
// broken; see TestMigrate_AppliesToAPopulatedDatabaseAtEveryShippedVersion.
//
// Correctness is not given up in exchange. applyMigration runs
// PRAGMA foreign_key_check inside each migration's own transaction before
// committing it, so a migration that really does leave a dangling
// reference is refused and rolled back rather than quietly written down.
// The pragma has to be toggled out here rather than inside that
// transaction because SQLite makes it a no-op while one is open.
func suspendForeignKeys(ctx context.Context, db *sql.DB) (restore func(), err error) {
	var was int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&was); err != nil {
		return nil, fmt.Errorf("state: reading foreign_keys pragma: %w", err)
	}
	if was == 0 {
		return func() {}, nil
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return nil, fmt.Errorf("state: suspending foreign key enforcement for migration: %w", err)
	}
	return func() {
		// Best effort by necessity: there is nothing useful a caller
		// could do with a failure here that it is not already doing with
		// the migration error it is on its way to returning, and the
		// handle is closed on that path anyway (see Open).
		_, _ = db.ExecContext(ctx, "PRAGMA foreign_keys = ON")
	}, nil
}

// appliedMigrations reads schema_migrations into a version-to-checksum
// map.
//
// Both halves of that pair are needed together, which is why this returns
// the checksums rather than a set of version numbers: the runner has two
// different questions to ask of the same row, whether this binary knows
// the version at all and whether the file it knows still hashes to what
// was recorded, and they produce two different refusals
// (ErrUnknownSchemaVersion and ErrSchemaDrift).
//
// It is also called by PendingMigration, which never applies anything, so
// it takes a *sql.DB rather than running inside a migration's transaction.
func appliedMigrations(ctx context.Context, db *sql.DB) (map[int]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("state: read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]string)
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("state: scan schema_migrations: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: read schema_migrations: %w", err)
	}

	return applied, nil
}

// applyMigration runs one migration's statements and records it as applied
// in a single transaction, so a crash or error partway through leaves the
// database exactly as it was before this migration was attempted, never
// half-applied.
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin migration %d (%s): %w", m.version, m.name, err)
	}
	defer tx.Rollback()

	for _, stmt := range splitStatements(m.sql) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("state: apply migration %d (%s): %w", m.version, m.name, err)
		}
	}

	if err := checkForeignKeys(ctx, tx); err != nil {
		return fmt.Errorf("state: apply migration %d (%s): %w", m.version, m.name, err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		m.version, m.name, m.checksum, formatTime(now()),
	); err != nil {
		return fmt.Errorf("state: record migration %d (%s): %w", m.version, m.name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit migration %d (%s): %w", m.version, m.name, err)
	}

	return nil
}

// splitStatements splits a migration file into individual statements on
// top-level semicolons.
//
// database/sql's Exec is only specified to run one statement per call, and
// driver behavior for a multi-statement string is not something this
// package wants to depend on, so migration files are split here rather than
// executed as one opaque blob. This is a plain split, not a SQL parser: line
// comments are stripped first (a "--" comment describing a migration is
// exactly the place a stray semicolon like "writes to; it is what makes"
// would otherwise get parsed as a statement boundary), and beyond that this
// is correct for the DDL these migration files contain (no string literals
// or identifiers with an embedded semicolon), which is a property this
// package's own migration files control and CI's build/vet/test on every
// push would catch if a future migration violated it.
func splitStatements(script string) []string {
	var withoutComments strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		withoutComments.WriteString(line)
		withoutComments.WriteByte('\n')
	}

	parts := strings.Split(withoutComments.String(), ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// PendingMigration reports whether Open would actually apply a migration to
// the database at path, without opening, migrating or otherwise changing
// anything itself.
//
// It exists so core/service's startup sequence can skip its pre-migration
// snapshot (and the restore that goes with it) on the overwhelmingly common
// start where the schema is already current: taking a copy of a journal
// nothing is about to change, and keeping a restore path armed against it,
// is pure risk with no upside. See core/service/startup.go for what that
// buys.
//
// A path that does not exist yet is reported as pending: Open would create
// the database and apply every migration to it, which is exactly the case a
// snapshot ("nothing existed") is still meaningful for.
//
// The two refusals migrate() makes (ErrUnknownSchemaVersion, ErrSchemaDrift)
// are deliberately NOT reported as pending. Neither one applies anything, so
// there is nothing for a snapshot to protect, and Open reports them itself
// with the same words it always has. This function's job is only "would a
// migration run", never "would Open succeed".
func PendingMigration(ctx context.Context, path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, fmt.Errorf("state: checking for pending migrations at %s: %w", path, err)
	}

	known, err := loadMigrations()
	if err != nil {
		return false, err
	}

	db, err := sql.Open(driverName, path)
	if err != nil {
		return false, fmt.Errorf("state: checking for pending migrations at %s: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		return false, fmt.Errorf("state: checking for pending migrations at %s: %w", path, err)
	}

	var tracked string
	err = db.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'").Scan(&tracked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No tracking table: Open bootstraps it and applies everything.
		return true, nil
	case err != nil:
		return false, fmt.Errorf("state: checking for pending migrations at %s: %w", path, err)
	}

	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return false, err
	}

	for _, m := range known {
		if _, ok := applied[m.version]; !ok {
			return true, nil
		}
	}
	return false, nil
}

// checkForeignKeys is the other half of suspendForeignKeys: enforcement is
// off while a migration runs, so this asks SQLite to verify, inside the
// migration's own transaction and before it commits, that the schema the
// migration just built has no dangling references left in it.
//
// PRAGMA foreign_key_check returns one row per violation and no rows when
// everything resolves, so an error here is "this migration would have left
// the journal referentially broken" and the caller's rollback is the right
// answer to it. Only the first violation is reported: a migration is
// wrong or it is not, and a list of every orphan in a large journal is not
// more actionable than the first one plus the table it is in.
func checkForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	defer rows.Close()

	if rows.Next() {
		// The pragma's columns are (table, rowid, parent, fkid). rowid is
		// NULL for a WITHOUT ROWID table, so it is scanned as a nullable.
		var (
			table  string
			rowID  sql.NullInt64
			parent string
			fkID   int
		)
		if err := rows.Scan(&table, &rowID, &parent, &fkID); err != nil {
			return fmt.Errorf("foreign key check: %w", err)
		}
		return fmt.Errorf("would leave a dangling reference: a row in %s points at a missing row in %s", table, parent)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	return nil
}
