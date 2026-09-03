package compat

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/migrations"
)

// captureUpgrade is the cell the rest of this package needed and did not
// have.
//
// The other state cells create a database, apply every migration to it,
// and only then put rows in. That is what a NEW deployment looks like, and
// against it the spec's own planted violation for FR-35 (a backfill that
// rewrites retention_tier) updates zero rows and changes nothing, so the
// gate passed it. scripts/compat/selftest.sh caught that, which is the
// entire argument for running a planted violation against every cell
// before believing any of them.
//
// So this cell builds the thing FR-35 is actually about: a database that
// already has an operator's artifacts in it, at the oldest schema this
// binary knows, upgraded in place by the migration runner. Every migration
// after the first therefore executes against populated tables, including
// the two that recreate the artifacts table wholesale (0002 and 0006) and
// including whichever one lands next, which is where FR-29's placements
// backfill is going to live.
func captureUpgrade(ctx context.Context, workDir string) (Cell, Cell, error) {
	root := filepath.Join(workDir, "upgrade")

	// The source of truth for what an operator's rows look like: seeded
	// through the public journal API, exactly as the other cells are, so
	// the two cannot describe different artifacts.
	bs, _, err := seedDeployment(ctx, root, deploymentConfigYAML(root), theDeployment())
	if err != nil {
		return Cell{}, Cell{}, fmt.Errorf("seeding the upgrade source: %w", err)
	}
	newDB := filepath.Join(root, "state.db")

	oldDB := filepath.Join(root, "old-schema.db")
	oldest, err := applyMigrationsUpTo(ctx, oldDB, 1)
	if err != nil {
		return Cell{}, Cell{}, err
	}

	// Artifact rows only, and NOT state_transitions, which is not an
	// oversight and not a convenience.
	//
	// Copying the transition history too is the more faithful upgrade, and
	// it does not currently work: migrations 0002 and 0006 recreate the
	// artifacts table with DROP TABLE, both carry a comment asserting
	// "foreign keys are not enabled on this connection", and state.Open
	// sets PRAGMA foreign_keys = ON before it runs the migrations at all.
	// state_transitions.artifact_id references artifacts(id), so the drop
	// trips FK enforcement the moment any transition row exists, which for
	// a real deployment is always. Proven against a journal written by an
	// actual pre-0006 build of this product and then opened by this one:
	// "state: apply migration 6 (remote_retained): constraint failed:
	// FOREIGN KEY constraint failed (787)". Filed as #396; it is a
	// defect in the migration runner, not in this gate, and it is why
	// docs/conformance/epic-e-matrix.md records this cell's transition
	// half as BLOCKED rather than quietly leaving it out.
	//
	// What the artifacts-only copy still buys, which is the whole reason
	// this cell exists, is a populated artifacts table under every
	// migration from 0002 onward, including whichever one lands next.
	for _, table := range []string{"artifacts"} {
		copied, err := copyRows(ctx, newDB, oldDB, table)
		if err != nil {
			return Cell{}, Cell{}, fmt.Errorf("populating the old-schema database's %s: %w", table, err)
		}
		if copied == 0 {
			return Cell{}, Cell{}, fmt.Errorf("copied no %s rows into the old-schema database, so the migrations would run against nothing and this cell would certify nothing", table)
		}
	}

	// The upgrade itself, through the one door every deployment uses.
	journal, err := state.Open(ctx, oldDB)
	if err != nil {
		return Cell{}, Cell{}, fmt.Errorf("upgrading a populated schema-%d database: %w", oldest, err)
	}
	records, err := journal.ListByBackupSet(ctx, bs.ID)
	closeErr := journal.Close()
	if err != nil {
		return Cell{}, Cell{}, err
	}
	if closeErr != nil {
		return Cell{}, Cell{}, closeErr
	}
	if len(records) == 0 {
		return Cell{}, Cell{}, fmt.Errorf("the upgraded database read back empty")
	}

	rows, err := captureArtifactRows(ctx, oldDB, root)
	if err != nil {
		return Cell{}, Cell{}, err
	}
	rows.Certifies = "FR-35 and FR-29, on the path FR-35 is actually about: an EXISTING deployment's artifact rows survive every migration this binary carries, column for column, with no column added to the artifact row. This is the cell docs/EPIC-E-alternative-storage.md section 4's planted violation, \"a migration variant that rewrites retention_tier during backfill\", has to make red."

	verdicts, err := captureRetentionVerdicts(ctx, oldDB, bs)
	if err != nil {
		return Cell{}, Cell{}, err
	}
	verdicts.Certifies = "FR-35 clause 2 on the upgrade path: the retention verdicts an existing deployment gets after migrating are the verdicts it got before, decided from records read back out of the upgraded database."

	return rows, verdicts, nil
}

// applyMigrationsUpTo builds a database at exactly schema version upTo,
// the same way internal/state's own runner would have built it on the day
// that version was current: every migration applied in order, and every
// one recorded in schema_migrations with the checksum the runner computes.
//
// The checksum matters more than it looks. Get it wrong and the upgrade
// below fails with ErrSchemaDrift rather than silently doing something
// else, which is the failure mode this whole package wants: loud.
func applyMigrationsUpTo(ctx context.Context, path string, upTo int) (int, error) {
	files, err := embeddedMigrations()
	if err != nil {
		return 0, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return 0, err
	}
	defer db.Close() //nolint:errcheck // reopened by state.Open below

	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TEXT NOT NULL
)`); err != nil {
		return 0, err
	}

	applied := 0
	for _, m := range files {
		if m.version > upTo {
			break
		}
		for _, stmt := range splitMigrationStatements(m.sql) {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return 0, fmt.Errorf("applying migration %04d (%s): %w", m.version, m.name, err)
			}
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			m.version, m.name, m.checksum, fixedNow.UTC().Format(time.RFC3339Nano)); err != nil {
			return 0, err
		}
		applied = m.version
	}
	if applied == 0 {
		return 0, fmt.Errorf("no migration at or below version %d, so no old schema was built", upTo)
	}
	remaining := 0
	for _, m := range files {
		if m.version > upTo {
			remaining++
		}
	}
	if remaining == 0 {
		return 0, fmt.Errorf("every migration this binary carries is at or below version %d, so nothing would run against the populated database and this cell would certify nothing", upTo)
	}
	return applied, nil
}

type embeddedMigration struct {
	version  int
	name     string
	sql      string
	checksum string
}

func embeddedMigrations() ([]embeddedMigration, error) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var out []embeddedMigration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".sql")
		numPart, rest, found := strings.Cut(base, "_")
		if !found || rest == "" {
			return nil, fmt.Errorf("migration file %q is not named NNNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(numPart)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("migration file %q does not start with a positive version", e.Name())
		}
		content, err := migrations.FS.ReadFile(e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(content)
		out = append(out, embeddedMigration{
			version:  v,
			name:     rest,
			sql:      string(content),
			checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// splitMigrationStatements mirrors internal/state's own splitStatements:
// strip line comments first (a "--" comment describing a migration is
// exactly where a stray semicolon hides), then split on the rest.
func splitMigrationStatements(script string) []string {
	var b strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	var out []string
	for _, p := range strings.Split(b.String(), ";") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// copyRows moves one table's rows from src into dst over the columns the
// two schemas share.
//
// The intersection is computed at runtime rather than written down, and
// that is the point: an old database genuinely does not have the columns a
// later migration added, so "the columns both have" is the honest
// definition of what an operator's row looked like back then, and it stays
// honest without anyone maintaining a list.
func copyRows(ctx context.Context, srcPath, dstPath, table string) (int, error) {
	src, err := sql.Open("sqlite", srcPath)
	if err != nil {
		return 0, err
	}
	defer src.Close() //nolint:errcheck // read-only

	dst, err := sql.Open("sqlite", dstPath)
	if err != nil {
		return 0, err
	}
	defer dst.Close() //nolint:errcheck // written to below

	srcCols, err := tableColumns(ctx, src, table)
	if err != nil {
		return 0, err
	}
	dstCols, err := tableColumns(ctx, dst, table)
	if err != nil {
		return 0, err
	}
	inDst := map[string]bool{}
	for _, c := range dstCols {
		inDst[c] = true
	}
	var shared []string
	for _, c := range srcCols {
		if inDst[c] {
			shared = append(shared, c)
		}
	}
	if len(shared) == 0 {
		return 0, fmt.Errorf("%s shares no column between the two schemas", table)
	}

	rows, err := src.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s ORDER BY rowid", strings.Join(shared, ", "), table))
	if err != nil {
		return 0, err
	}
	defer rows.Close() //nolint:errcheck // read-only

	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(shared)), ", ")
	insert := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(shared, ", "), placeholders)

	copied := 0
	for rows.Next() {
		vals := make([]any, len(shared))
		ptrs := make([]any, len(shared))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return 0, err
		}
		if _, err := dst.ExecContext(ctx, insert, vals...); err != nil {
			return 0, fmt.Errorf("%s: %w", insert, err)
		}
		copied++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return copied, nil
}
