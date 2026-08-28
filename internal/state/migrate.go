package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spdrman/rclone-manager/migrations"
)

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

	for _, m := range known {
		if _, ok := applied[m.version]; ok {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}

	return nil
}

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
