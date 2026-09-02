package state

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// schemaVersionBeforePlacements is the version a deployment is at just
// before 0007 runs: everything this product shipped before EPIC E.
const schemaVersionBeforePlacements = 6

// artifactFixture is one pre-existing artifact row, written the way the
// journal itself writes it, so the backfill is exercised against the shape
// a real deployment has rather than a convenient one.
type artifactFixture struct {
	name       string
	state      string
	localPath  string
	localHash  string
	hashAlg    string
	transfer   *int64
	verifiedAt string // empty means this artifact never entered VERIFIED
	tier       string
}

func int64p(v int64) *int64 { return &v }

// everyLifecycleState is the fixture the issue asks for: artifact rows in
// every lifecycle state the schema admits, not just the happy one. A
// deployment upgrading mid-cycle has rows scattered across all of them.
func everyLifecycleState() []artifactFixture {
	return []artifactFixture{
		// Nothing has landed yet: the journal has no local path to record.
		{name: "a-discovered.dump", state: "DISCOVERED"},
		// Mid-transfer: local_path is the .partial destination.
		{name: "b-transferring.dump", state: "TRANSFERRING", localPath: "/backups/daily/b-transferring.dump.partial"},
		{name: "c-transferred.dump", state: "TRANSFERRED", localPath: "/backups/daily/c-transferred.dump.partial", transfer: int64p(4096)},
		{name: "d-verifying.dump", state: "VERIFYING", localPath: "/backups/daily/d-verifying.dump.partial", transfer: int64p(4096)},
		{name: "e-verified.dump", state: "VERIFIED", localPath: "/backups/daily/e-verified.dump.partial",
			localHash: "e5e5", hashAlg: "sha256", transfer: int64p(4096), verifiedAt: "2026-02-01T10:00:00Z"},
		{name: "f-committing.dump", state: "COMMITTING", localPath: "/backups/daily/f-committing.dump.partial",
			localHash: "f6f6", hashAlg: "sha256", transfer: int64p(4096), verifiedAt: "2026-02-01T11:00:00Z"},
		// Committed onward: local_path is the final, durable path.
		{name: "g-committed.dump", state: "COMMITTED", localPath: "/backups/daily/g-committed.dump",
			localHash: "0707", hashAlg: "sha256", transfer: int64p(8192), verifiedAt: "2026-02-01T12:00:00Z"},
		{name: "h-deletepending.dump", state: "REMOTE_DELETE_PENDING", localPath: "/backups/daily/h-deletepending.dump",
			localHash: "0808", hashAlg: "sha256", transfer: int64p(8192), verifiedAt: "2026-02-01T13:00:00Z"},
		{name: "i-complete.dump", state: "COMPLETE", localPath: "/backups/daily/i-complete.dump",
			localHash: "0909", hashAlg: "sha256", transfer: int64p(8192), verifiedAt: "2026-02-01T14:00:00Z", tier: "daily"},
		{name: "j-retained.dump", state: "REMOTE_RETAINED", localPath: "/backups/daily/j-retained.dump",
			localHash: "1010", hashAlg: "sha256", transfer: int64p(8192), verifiedAt: "2026-02-01T15:00:00Z", tier: "monthly"},
		{name: "k-failed.dump", state: "FAILED", localPath: "/backups/daily/k-failed.dump.partial"},
		{name: "l-quarantined.dump", state: "QUARANTINED", localPath: "/backups/daily/l-quarantined.dump",
			localHash: "1212", hashAlg: "sha256", transfer: int64p(8192), verifiedAt: "2026-02-01T16:00:00Z"},
		{name: "m-quarantinedlost.dump", state: "QUARANTINED_LOST", localPath: "/backups/daily/m-quarantinedlost.dump",
			localHash: "1313", hashAlg: "sha256", transfer: int64p(8192), verifiedAt: "2026-02-01T17:00:00Z"},
		// A row whose hash arrived without the artifact ever entering
		// VERIFIED, which is exactly what catalog rebuild produces: the
		// hash is real evidence, the moment it was checked is not
		// recoverable, and the backfill must not invent one.
		{name: "n-rebuilt.dump", state: "REMOTE_DELETE_PENDING", localPath: "/backups/daily/n-rebuilt.dump",
			localHash: "1414", hashAlg: "sha256", transfer: int64p(8192)},
	}
}

// seedPreEpicEJournal writes fixtures into a database standing at the
// schema version that shipped before placements existed.
func seedPreEpicEJournal(t *testing.T, ctx context.Context, db *sql.DB, fixtures []artifactFixture) {
	t.Helper()
	for i, f := range fixtures {
		id := int64(i + 1)
		var transfer any
		if f.transfer != nil {
			transfer = *f.transfer
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO artifacts (id, source, backup_set, artifact_name, remote_path, local_path, state,
			                       discovered_at, updated_at, transfer_bytes, local_hash, local_hash_alg, retention_tier)
			VALUES (?, 'nas', 'daily', ?, ?, ?, ?, '2026-01-01T00:00:00Z', '2026-02-02T00:00:00Z', ?, ?, ?, ?)`,
			id, f.name, "/remote/daily/"+f.name, f.localPath, f.state, transfer, f.localHash, f.hashAlg, f.tier,
		); err != nil {
			t.Fatalf("seed %s: %v", f.name, err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO state_transitions (artifact_id, idempotency_key, from_state, to_state, occurred_at)
			VALUES (?, ?, '', 'DISCOVERED', '2026-01-01T00:00:00Z')`,
			id, fmt.Sprintf("seed-discover-%d", id),
		); err != nil {
			t.Fatalf("seed discover transition for %s: %v", f.name, err)
		}
		if f.verifiedAt != "" {
			if _, err := db.ExecContext(ctx, `
				INSERT INTO state_transitions (artifact_id, idempotency_key, from_state, to_state, occurred_at)
				VALUES (?, ?, 'VERIFYING', 'VERIFIED', ?)`,
				id, fmt.Sprintf("seed-verify-%d", id), f.verifiedAt,
			); err != nil {
				t.Fatalf("seed verify transition for %s: %v", f.name, err)
			}
		}
	}
}

type placementRow struct {
	medium     string
	location   string
	sizeBytes  sql.NullInt64
	hash       string
	hashAlg    string
	class      string
	verifiedAt sql.NullString
	status     string
	createdAt  string
	updatedAt  string
}

func readPlacements(t *testing.T, ctx context.Context, db *sql.DB) map[string]placementRow {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT a.artifact_name, p.medium, p.location, p.size_bytes, p.hash, p.hash_alg,
		       p.verification_class, p.verified_at, p.status, p.created_at, p.updated_at
		  FROM placements p JOIN artifacts a ON a.id = p.artifact_id
		 ORDER BY p.id`)
	if err != nil {
		t.Fatalf("read placements: %v", err)
	}
	defer rows.Close()

	out := make(map[string]placementRow)
	for rows.Next() {
		var name string
		var p placementRow
		if err := rows.Scan(&name, &p.medium, &p.location, &p.sizeBytes, &p.hash, &p.hashAlg,
			&p.class, &p.verifiedAt, &p.status, &p.createdAt, &p.updatedAt); err != nil {
			t.Fatalf("scan placement: %v", err)
		}
		if _, dup := out[name]; dup {
			t.Fatalf("%s has more than one placement after the backfill", name)
		}
		out[name] = p
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read placements: %v", err)
	}
	return out
}

// The forward-migration test the issue's TDD plan asks for: a populated
// pre-EPIC-E database in, one ACTIVE local placement per artifact out,
// carrying what the journal already recorded and nothing it did not.
func TestMigrate0007_BackfillsOneLocalPlacementPerExistingArtifact(t *testing.T) {
	ctx := context.Background()
	db, _ := openLikeProduction(t)
	applyUpTo(t, ctx, db, schemaVersionBeforePlacements)

	fixtures := everyLifecycleState()
	seedPreEpicEJournal(t, ctx, db, fixtures)

	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got := readPlacements(t, ctx, db)
	if len(got) != len(fixtures) {
		t.Fatalf("backfilled %d placements, want %d (one per artifact)", len(got), len(fixtures))
	}

	for _, f := range fixtures {
		p, ok := got[f.name]
		if !ok {
			t.Errorf("%s (%s): no placement backfilled", f.name, f.state)
			continue
		}
		if p.medium != "local" {
			t.Errorf("%s: medium %q, want \"local\"", f.name, p.medium)
		}
		if p.status != "ACTIVE" {
			t.Errorf("%s: status %q, want ACTIVE", f.name, p.status)
		}
		// The location is the journal's own local_path, verbatim. Not a
		// path recomputed from a configured backup root: the migration has
		// no config to read and must not invent one.
		if p.location != f.localPath {
			t.Errorf("%s: location %q, want the journal's own local_path %q", f.name, p.location, f.localPath)
		}
		if p.hash != f.localHash || p.hashAlg != f.hashAlg {
			t.Errorf("%s: hash %q/%q, want the journal's own %q/%q", f.name, p.hash, p.hashAlg, f.localHash, f.hashAlg)
		}
		switch {
		case f.transfer == nil && p.sizeBytes.Valid:
			t.Errorf("%s: size_bytes = %d, want NULL: the journal recorded no transferred size", f.name, p.sizeBytes.Int64)
		case f.transfer != nil && p.sizeBytes.Int64 != *f.transfer:
			t.Errorf("%s: size_bytes = %v, want the journal's own transfer_bytes %d", f.name, p.sizeBytes, *f.transfer)
		}
		// The verification class is derived only from evidence already in
		// the journal: a recorded local hash AND a recorded VERIFIED
		// entry. The migration does no I/O, so it cannot achieve a class
		// of its own and must not claim one.
		wantClass, wantVerified := "", ""
		if f.localHash != "" && f.verifiedAt != "" {
			wantClass, wantVerified = "content", f.verifiedAt
		}
		if p.class != wantClass {
			t.Errorf("%s: verification_class %q, want %q", f.name, p.class, wantClass)
		}
		if p.verifiedAt.String != wantVerified || p.verifiedAt.Valid != (wantVerified != "") {
			t.Errorf("%s: verified_at %v, want %q", f.name, p.verifiedAt, wantVerified)
		}
	}
}

// The migration must not touch the artifact rows themselves. This is the
// behaviour-neutrality claim stated as a check rather than as a sentence:
// the entire artifacts table, column for column, has to come out of the
// migration byte-identical to what went in.
func TestMigrate0007_LeavesEveryArtifactRowUntouched(t *testing.T) {
	ctx := context.Background()
	db, _ := openLikeProduction(t)
	applyUpTo(t, ctx, db, schemaVersionBeforePlacements)
	seedPreEpicEJournal(t, ctx, db, everyLifecycleState())

	before := dumpArtifacts(t, ctx, db)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	after := dumpArtifacts(t, ctx, db)

	if len(before) != len(after) {
		t.Fatalf("artifacts row count changed from %d to %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("artifacts row %d changed across the migration:\n before: %s\n  after: %s", i, before[i], after[i])
		}
	}
}

// dumpArtifacts renders every artifact row as one string per row, so a
// comparison covers every column without naming them one at a time and
// still fails loudly on a column added later.
func dumpArtifacts(t *testing.T, ctx context.Context, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT * FROM artifacts ORDER BY id`)
	if err != nil {
		t.Fatalf("dump artifacts: %v", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("dump artifacts: %v", err)
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("dump artifacts: %v", err)
		}
		line := ""
		for i, c := range cols {
			line += fmt.Sprintf("%s=%v;", c, cells[i])
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dump artifacts: %v", err)
	}
	return out
}

// A migration that fails partway through must leave the schema version
// exactly where it was, with nothing it created still standing. The
// runner's transaction is what makes that true, and this proves the
// backfill is inside it rather than beside it: a database that already has
// a table named placements makes 0007's CREATE fail, and the version must
// not advance.
func TestMigrate0007_InterruptedBackfillLeavesTheSchemaVersionUnadvanced(t *testing.T) {
	ctx := context.Background()
	db, _ := openLikeProduction(t)
	applyUpTo(t, ctx, db, schemaVersionBeforePlacements)
	seedPreEpicEJournal(t, ctx, db, everyLifecycleState())

	// Stand something in the migration's way, inside the database, so the
	// failure happens where a real one would: mid-statement.
	if _, err := db.ExecContext(ctx, `CREATE TABLE placements (blocker INTEGER)`); err != nil {
		t.Fatalf("plant blocking table: %v", err)
	}

	if err := migrate(ctx, db); err == nil {
		t.Fatal("migrate succeeded against a database with a conflicting table, want an error")
	}

	var version int
	if err := db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != schemaVersionBeforePlacements {
		t.Fatalf("schema version advanced to %d after a failed migration, want %d", version, schemaVersionBeforePlacements)
	}

	// And the half-built work is gone: the blocker is still exactly the
	// one-column table the test planted, not something the migration
	// partially rewrote, and placement_moves was never left behind.
	var cols int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('placements')`).Scan(&cols); err != nil {
		t.Fatalf("inspect placements: %v", err)
	}
	if cols != 1 {
		t.Fatalf("placements has %d columns after the failed migration, want the planted 1", cols)
	}
	var moves int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='placement_moves'`).Scan(&moves); err != nil {
		t.Fatalf("inspect placement_moves: %v", err)
	}
	if moves != 0 {
		t.Fatal("placement_moves survived a failed migration")
	}
}

// One local placement per artifact is a schema guarantee, not a convention
// the writers are trusted to keep. A second one has to be refused by the
// database itself.
func TestPlacements_RefuseASecondLocalPlacementForOneArtifact(t *testing.T) {
	ctx := context.Background()
	db, _ := openLikeProduction(t)
	applyUpTo(t, ctx, db, schemaVersionBeforePlacements)
	seedPreEpicEJournal(t, ctx, db, everyLifecycleState()[:1])
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO placements (artifact_id, medium, location, status, created_at, updated_at)
		VALUES (1, 'local', '/somewhere/else.dump', 'ACTIVE', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z')`)
	if err == nil {
		t.Fatal("a second local placement for one artifact was accepted, want a unique-constraint refusal")
	}

	// The positive control: the same insert against a different medium is
	// fine, because an artifact genuinely can live in two places at once
	// during a move.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO placements (artifact_id, medium, location, status, created_at, updated_at)
		VALUES (1, 'cold_s3', 'nas/daily/a-discovered.dump', 'ACTIVE', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("a placement on a second medium was refused: %v", err)
	}
}

// A placement row cannot outlive the artifact it describes, and the
// vocabularies the schema pins are pinned by the database, not by comment.
func TestPlacements_RefuseAnOrphanAndAnUnknownStatus(t *testing.T) {
	ctx := context.Background()
	db, _ := openLikeProduction(t)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO placements (artifact_id, medium, location, status, created_at, updated_at)
		VALUES (4242, 'local', '/x', 'ACTIVE', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z')`,
	); err == nil {
		t.Fatal("a placement referencing no artifact was accepted")
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO artifacts (id, source, backup_set, artifact_name, remote_path, discovered_at, updated_at)
		VALUES (1, 'nas', 'daily', 'x.dump', '/remote/x.dump', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
	for _, bad := range []string{
		`INSERT INTO placements (artifact_id, medium, location, status, created_at, updated_at) VALUES (1, 'local', '/x', 'PROBABLY_THERE', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z')`,
		`INSERT INTO placements (artifact_id, medium, location, status, verification_class, created_at, updated_at) VALUES (1, 'local', '/x', 'ACTIVE', 'vibes', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, bad); err == nil {
			t.Fatalf("accepted a value outside the schema's vocabulary: %s", bad)
		}
	}
}

// placement_moves is the FR-30 journal. Nothing writes it yet, so what is
// pinned here is that it exists with the phases FR-30 names and refuses
// anything else, which is what stops the move engine inventing a phase.
func TestPlacementMoves_PinTheFR30Phases(t *testing.T) {
	ctx := context.Background()
	db, _ := openLikeProduction(t)
	applyUpTo(t, ctx, db, schemaVersionBeforePlacements)
	seedPreEpicEJournal(t, ctx, db, everyLifecycleState()[:1])
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, phase := range []string{"PLANNED", "COPYING", "COPIED", "VERIFYING", "VERIFIED", "SOURCE_DELETE_PENDING", "DONE", "ABANDONED"} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO placement_moves (artifact_id, source_placement_id, destination_medium, destination_location, phase, started_at, updated_at)
			VALUES (1, 1, 'cold_s3', 'nas/daily/a-discovered.dump', ?, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z')`, phase,
		); err != nil {
			t.Errorf("phase %s was refused: %v", phase, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO placement_moves (artifact_id, source_placement_id, destination_medium, destination_location, phase, started_at, updated_at)
		VALUES (1, 1, 'cold_s3', 'k', 'MOVING_PROBABLY', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z')`,
	); err == nil {
		t.Fatal("an unknown move phase was accepted")
	}
}
