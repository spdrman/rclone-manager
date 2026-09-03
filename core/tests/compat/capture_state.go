package compat

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	// The same pure-Go SQLite driver internal/state itself opens the
	// journal with. This package reads the migrated database directly, on
	// purpose: the point of the row and schema cells is to see what the
	// migrations actually left on disk, which is precisely what a typed
	// read surface would hide.
	_ "modernc.org/sqlite"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// captureArtifactRows dumps every column of every artifact row, in id
// order, exactly as the migrations left it.
//
// This is the cell the spec's section 4 planted violation is aimed at: "a
// migration variant that rewrites retention_tier during backfill". It
// reads * rather than a chosen column list so that a migration which adds
// a column to this table is also a red cell, which is FR-29's own decision
// enforced from the outside: placements go in a new table "rather than new
// columns on the artifact row".
func captureArtifactRows(ctx context.Context, dbPath, root string) (Cell, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return Cell{}, err
	}
	defer db.Close() //nolint:errcheck // read-only handle

	cols, err := tableColumns(ctx, db, "artifacts")
	if err != nil {
		return Cell{}, err
	}
	if len(cols) == 0 {
		return Cell{}, fmt.Errorf("the artifacts table has no columns, so this cell would certify nothing")
	}

	rows, err := db.QueryContext(ctx, "SELECT * FROM artifacts ORDER BY id")
	if err != nil {
		return Cell{}, err
	}
	defer rows.Close() //nolint:errcheck // read-only

	var lines []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return Cell{}, err
		}
		parts := make([]string, 0, len(cols))
		for i, c := range cols {
			parts = append(parts, fmt.Sprintf("%s=%s", c, renderSQLValue(vals[i], root)))
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	if err := rows.Err(); err != nil {
		return Cell{}, err
	}
	if len(lines) == 0 {
		return Cell{}, fmt.Errorf("no artifact rows survived migration, so this cell would certify nothing")
	}

	return Cell{
		Certifies: "FR-35 and FR-29: migrating an existing deployment leaves every artifact row exactly as it was, column for column, and adds no column to this table. This is the cell the spec's planted backfill violation (a migration that rewrites retention_tier) has to make red.",
		Rule:      RuleIdentical,
		Lines:     lines,
	}, nil
}

// captureSchema dumps the migrated database's own DDL and the list of
// migrations that produced it.
//
// Additive-only rather than identical, because FR-29 explicitly adds
// tables and this gate would otherwise be a rule against EPIC E existing.
// What additive-only still refuses is the thing that matters: an existing
// table, index or trigger whose definition changed or disappeared. Two of
// this repository's migrations already recreate the artifacts table
// wholesale (0002 and 0006), so "the placements migration recreates it
// slightly differently" is a real, reachable mistake, and this is what
// catches it.
func captureSchema(ctx context.Context, dbPath string) (Cell, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return Cell{}, err
	}
	defer db.Close() //nolint:errcheck // read-only handle

	rows, err := db.QueryContext(ctx,
		"SELECT type, name, COALESCE(sql, '') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name")
	if err != nil {
		return Cell{}, err
	}
	defer rows.Close() //nolint:errcheck // read-only

	var lines []string
	for rows.Next() {
		var kind, name, ddl string
		if err := rows.Scan(&kind, &name, &ddl); err != nil {
			return Cell{}, err
		}
		lines = append(lines, fmt.Sprintf("%s %s :: %s", kind, name, collapse(ddl)))
	}
	if err := rows.Err(); err != nil {
		return Cell{}, err
	}

	applied, err := db.QueryContext(ctx, "SELECT version, name FROM schema_migrations ORDER BY version")
	if err != nil {
		return Cell{}, err
	}
	defer applied.Close() //nolint:errcheck // read-only
	for applied.Next() {
		var version int
		var name string
		if err := applied.Scan(&version, &name); err != nil {
			return Cell{}, err
		}
		lines = append(lines, fmt.Sprintf("migration %04d %s", version, name))
	}
	if err := applied.Err(); err != nil {
		return Cell{}, err
	}
	if len(lines) == 0 {
		return Cell{}, fmt.Errorf("the migrated database reported no schema at all")
	}

	return Cell{
		Certifies: "FR-35: an upgrade may add tables and migrations, and may not change or drop a single definition an existing deployment already has.",
		Rule:      RuleAdditiveOnly,
		Lines:     lines,
	}, nil
}

// captureRetentionVerdicts decides KEEP/DELETE at the fixed instant, from
// records read back out of the migrated database.
//
// Read back out, not held over from seeding: a verdict computed from the
// structs that went in would be identical whatever the migrations did to
// the rows, which is the difference between this cell and a cell that
// cannot fail.
func captureRetentionVerdicts(ctx context.Context, dbPath string, bs config.BackupSet) (Cell, error) {
	records, err := readRecords(ctx, dbPath, bs)
	if err != nil {
		return Cell{}, err
	}

	verdicts, lkg, err := retention.DecideKeep(fixedNow, bs.Retention, bs.ID, records)
	if err != nil {
		return Cell{}, err
	}

	lines := []string{fmt.Sprintf("records considered: %d", len(records))}
	for _, v := range verdicts {
		decision := "DELETE"
		if v.Keep {
			decision = "KEEP"
		}
		lines = append(lines, fmt.Sprintf("%-6s %-28s tiers=%v", decision, v.Artifact.Name, v.Tiers))
		for _, l := range v.SiblingCollisionLines() {
			lines = append(lines, "  ! "+l)
		}
	}
	lines = append(lines, "last-known-good: "+lkg.Reason)

	return Cell{
		Certifies: "FR-35 clause 2: retention verdicts for a medium-free deployment are identical, tier attribution and sibling collisions included, decided from records that came back out of the migrated schema.",
		Rule:      RuleIdentical,
		Lines:     lines,
	}, nil
}

// capturePruneVerdicts is FR-20's mandatory dry-run, at the fixed instant.
//
// It is a separate cell from the verdicts above because it answers a
// different question: not "does policy keep this" but "would this file be
// removed, and if not, exactly which safety check said no". FR-30 extends
// this surface with mediums, and this pins what it says with none.
func capturePruneVerdicts(ctx context.Context, dbPath, root string, bs config.BackupSet) (Cell, error) {
	records, err := readRecords(ctx, dbPath, bs)
	if err != nil {
		return Cell{}, err
	}

	verdicts, err := retention.PruneDecide(fixedNow, bs.Retention, bs, records)
	if err != nil {
		return Cell{}, err
	}

	lines := make([]string, 0, len(verdicts))
	for _, v := range verdicts {
		// The reason gets normalized too, not only the path. A REFUSE
		// reason quotes the path it could not stat, so leaving it raw
		// pinned a per-run temp directory into a checked-in file, which
		// makes the cell red on every machine but the one that captured
		// it. That is not a hypothetical: this is the fix for it.
		lines = append(lines, fmt.Sprintf("%-6s %-28s path=%s reason=%s",
			v.Action, v.Artifact.Name, normalizeRoot(v.Path, root), normalizeRoot(collapse(v.Reason), root)))
	}
	if len(lines) == 0 {
		return Cell{}, fmt.Errorf("prune produced no verdicts at all")
	}

	return Cell{
		Certifies: "FR-20 and FR-35: the mandatory dry-run names the same action, the same path and the same reason for every artifact, and still refuses rather than deletes when a safety check does not pass.",
		Rule:      RuleIdentical,
		Lines:     lines,
	}, nil
}

// readRecords opens the journal the way any other reader does and lists
// the backup set.
func readRecords(ctx context.Context, dbPath string, bs config.BackupSet) ([]state.Record, error) {
	journal, err := state.Open(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	defer journal.Close() //nolint:errcheck // read-only handle

	records, err := journal.ListByBackupSet(ctx, bs.ID)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("the seeded backup set read back empty")
	}
	return records, nil
}

func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only

	var cols []string
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultVal, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}

// renderSQLValue prints a scanned column deterministically, with the
// throwaway root folded away so the corpus does not pin a temp directory.
func renderSQLValue(v any, root string) string {
	switch t := v.(type) {
	case nil:
		return "<null>"
	case []byte:
		return normalizeRoot(string(t), root)
	case string:
		return normalizeRoot(t, root)
	default:
		return normalizeRoot(fmt.Sprintf("%v", t), root)
	}
}

// normalizeRoot is the only normalization this package does, and it is
// deliberately the narrowest one that works: the absolute path of the
// throwaway directory becomes <ROOT>, and nothing else is touched.
//
// Normalization is where a golden gate goes to die. Every rule that
// rewrites output before comparing it is a rule that can hide a real
// change, so there is exactly one here, it is an exact string replacement
// rather than a pattern, and it exists only because the alternative is
// pinning /var/folders/.../T/TestX123 into a checked-in file.
func normalizeRoot(s, root string) string {
	if root == "" {
		return s
	}
	// The resolved form FIRST. FR-20 canonicalizes a path before refusing
	// or accepting it, so a prune verdict carries /private/var/... where
	// everything else carries /var/..., and replacing the short form first
	// leaves "/private<ROOT>" in the corpus: a real, and initially
	// checked-in, artefact of getting this order wrong.
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		s = strings.ReplaceAll(s, resolved, "<ROOT>")
	}
	return strings.ReplaceAll(s, root, "<ROOT>")
}

// collapse folds whitespace so a multi-line value stays one corpus line.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
