package state

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// placementsMigrationVersion is the version this file's subject is. It is
// named once so a future renumbering breaks in one place.
const placementsMigrationVersion = 7

// migrateUpTo applies every embedded migration with a version <= upTo, in
// order, recording each in schema_migrations exactly as the real runner
// does.
//
// It exists because the contract this file has to check is about a
// database that already holds artifacts, written by a build that had never
// heard of placements. There is no honest way to construct that state by
// running the whole migration set and then deleting rows: the whole
// question is what the backfill does to data that was already there.
func migrateUpTo(t *testing.T, db *sql.DB, upTo int) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, bootstrapSchemaMigrationsSQL); err != nil {
		t.Fatalf("bootstrap schema_migrations: %v", err)
	}
	known, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	applied := 0
	for _, m := range known {
		if m.version > upTo {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			t.Fatalf("applying migration %d: %v", m.version, err)
		}
		applied++
	}
	if applied != upTo {
		t.Fatalf("applied %d migrations up to version %d; the embedded set is not contiguous, so this fixture is not the schema it claims to be", applied, upTo)
	}
}

// migrationSQL returns one embedded migration's own text, so a test can
// mutate it and apply the mutant.
func migrationSQL(t *testing.T, version int) migration {
	t.Helper()
	known, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range known {
		if m.version == version {
			return m
		}
	}
	t.Fatalf("no migration with version %d is embedded in this binary", version)
	return migration{}
}

// artifactFixture is one pre-placements artifact row, written straight into
// the schema-6 table.
type artifactFixture struct {
	name          string
	state         string
	localPath     string
	localHash     string
	transferBytes *int64
	retentionTier string
}

// seedArtifacts writes fixture rows into a database at schema version 6,
// covering every lifecycle state the artifacts CHECK admits, so the
// backfill is exercised against the whole vocabulary rather than against
// the happy path.
func seedArtifacts(t *testing.T, db *sql.DB) []artifactFixture {
	t.Helper()
	bytes := func(n int64) *int64 { return &n }

	fixtures := []artifactFixture{
		{"discovered.dump", "DISCOVERED", "", "", nil, ""},
		{"transferring.dump", "TRANSFERRING", "/backups/pg/transferring.dump.partial", "", nil, ""},
		{"transferred.dump", "TRANSFERRED", "/backups/pg/transferred.dump.partial", "", bytes(11), ""},
		{"verifying.dump", "VERIFYING", "/backups/pg/verifying.dump.partial", "", bytes(12), ""},
		{"verified.dump", "VERIFIED", "/backups/pg/verified.dump.partial", "aaaa", bytes(13), ""},
		{"committing.dump", "COMMITTING", "/backups/pg/committing.dump.partial", "bbbb", bytes(14), ""},
		{"committed.dump", "COMMITTED", "/backups/pg/committed.dump", "cccc", bytes(15), "daily"},
		{"pending.dump", "REMOTE_DELETE_PENDING", "/backups/pg/pending.dump", "dddd", bytes(16), "weekly"},
		{"complete.dump", "COMPLETE", "/backups/pg/complete.dump", "eeee", bytes(17), "monthly"},
		{"retained.dump", "REMOTE_RETAINED", "/backups/pg/retained.dump", "ffff", bytes(18), "daily"},
		{"failed.dump", "FAILED", "", "", nil, ""},
		{"quarantined.dump", "QUARANTINED", "/backups/pg/quarantined.dump", "9999", bytes(19), "daily"},
		{"lost.dump", "QUARANTINED_LOST", "/backups/pg/lost.dump", "8888", bytes(20), "monthly"},
		// A committed artifact with no recorded hash: verified without
		// hash: sha256, which is a legal configuration and the case where
		// a backfilled placement must not claim a content verification.
		{"nohash.dump", "COMPLETE", "/backups/pg/nohash.dump", "", bytes(21), "daily"},
	}

	now := formatTime(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC))
	for i, f := range fixtures {
		res, err := db.Exec(`
			INSERT INTO artifacts (
				source, backup_set, artifact_name, remote_path, local_path, state,
				discovered_at, updated_at, transfer_bytes, local_hash, local_hash_alg,
				retention_tier
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"production", "postgres-primary", f.name, "/remote/"+f.name, f.localPath, f.state,
			now, now, optionalInt64(f.transferBytes), f.localHash, hashAlgFor(f.localHash),
			f.retentionTier,
		)
		if err != nil {
			t.Fatalf("seeding %s: %v", f.name, err)
		}
		rowID, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("seeding %s: %v", f.name, err)
		}
		// A COMMITTED transition for the rows that reached one, so the
		// backfill's created_at has something honest to read.
		if f.localHash != "" {
			if _, err := db.Exec(`
				INSERT INTO state_transitions (artifact_id, idempotency_key, from_state, to_state, occurred_at)
				VALUES (?, ?, ?, ?, ?)`,
				rowID, fmt.Sprintf("seed-%d-committed", i), "COMMITTING", "COMMITTED",
				formatTime(time.Date(2026, 3, 4, 4, 0, 0, 0, time.UTC)),
			); err != nil {
				t.Fatalf("seeding a transition for %s: %v", f.name, err)
			}
		}
	}
	return fixtures
}

func hashAlgFor(hash string) string {
	if hash == "" {
		return ""
	}
	return "sha256"
}

// durableStates are the states in which local_path names a durable copy
// rather than a partial in flight. It mirrors the migration's own WHERE
// clause deliberately: if the two ever disagree, this test is where that
// shows up.
var durableStates = map[string]bool{
	"COMMITTED":             true,
	"REMOTE_DELETE_PENDING": true,
	"COMPLETE":              true,
	"REMOTE_RETAINED":       true,
	"QUARANTINED":           true,
	"QUARANTINED_LOST":      true,
}

// TestMigration0007BackfillsEveryDurableArtifactAndNothingElse is TDD
// invariant 6's forward-migration test for this migration: a database with
// artifacts in every lifecycle state goes in, and the placements that come
// out are checked in both directions.
//
// Both directions is the point. Asserting only that durable artifacts got
// a placement would pass just as happily if the backfill gave one to
// everything, including a half-written .partial, which is the specific
// mistake that would put a row in this table claiming a committed copy
// exists where none does.
func TestMigration0007BackfillsEveryDurableArtifactAndNothingElse(t *testing.T) {
	db, _ := openRaw(t)
	migrateUpTo(t, db, placementsMigrationVersion-1)
	fixtures := seedArtifacts(t, db)

	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			rows, err := db.Query(`
				SELECT p.medium, p.location, p.size_bytes, p.hash, p.hash_alg,
				       p.verification_class, p.verified_at, p.status, p.created_at
				  FROM placements p
				  JOIN artifacts a ON a.id = p.artifact_id
				 WHERE a.artifact_name = ?`, f.name)
			if err != nil {
				t.Fatalf("querying placements: %v", err)
			}
			defer rows.Close()

			type placementRow struct {
				medium, location, hash, hashAlg, class, status, createdAt string
				size                                                      sql.NullInt64
				verifiedAt                                                sql.NullString
			}
			var got []placementRow
			for rows.Next() {
				var p placementRow
				if err := rows.Scan(&p.medium, &p.location, &p.size, &p.hash, &p.hashAlg,
					&p.class, &p.verifiedAt, &p.status, &p.createdAt); err != nil {
					t.Fatalf("scanning: %v", err)
				}
				got = append(got, p)
			}

			wantPlacement := durableStates[f.state] && f.localPath != ""
			if !wantPlacement {
				if len(got) != 0 {
					t.Fatalf("a %s artifact with local_path %q got %d placements (%+v); before its transfer an artifact has zero copies, and a .partial in flight is not a durable one",
						f.state, f.localPath, len(got), got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("a %s artifact got %d placements, want exactly 1", f.state, len(got))
			}

			p := got[0]
			if p.medium != "local" {
				t.Errorf("medium = %q, want %q", p.medium, "local")
			}
			if p.location != f.localPath {
				t.Errorf("location = %q, want the artifact's own local_path %q", p.location, f.localPath)
			}
			if p.status != "ACTIVE" {
				t.Errorf("status = %q, want ACTIVE", p.status)
			}
			if p.hash != f.localHash {
				t.Errorf("hash = %q, want the artifact's recorded local_hash %q", p.hash, f.localHash)
			}
			if f.transferBytes != nil {
				if !p.size.Valid || p.size.Int64 != *f.transferBytes {
					t.Errorf("size_bytes = %v, want the locally transferred %d", p.size, *f.transferBytes)
				}
			}
			if p.verifiedAt.Valid {
				t.Errorf("verified_at = %q; the journal never recorded when a local hash was checked, and inventing one would make every backfilled placement claim a verification time it does not have", p.verifiedAt.String)
			}

			wantClass := ""
			if f.localHash != "" && f.state != "QUARANTINED" && f.state != "QUARANTINED_LOST" {
				wantClass = "content"
			}
			if p.class != wantClass {
				t.Errorf("verification_class = %q, want %q", p.class, wantClass)
			}
		})
	}
}

// TestMigration0007IsIdempotent proves running the whole set twice over an
// already-migrated database changes nothing: the runner skips an applied
// version, so the backfill must not run a second time and double every
// placement.
func TestMigration0007IsIdempotent(t *testing.T) {
	db, _ := openRaw(t)
	migrateUpTo(t, db, placementsMigrationVersion-1)
	seedArtifacts(t, db)

	ctx := context.Background()
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	before := countPlacements(t, db)
	if before == 0 {
		t.Fatal("the first migration backfilled nothing, so running it twice proves nothing")
	}

	if err := migrate(ctx, db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if after := countPlacements(t, db); after != before {
		t.Errorf("placements went from %d to %d across a second migrate; the backfill ran twice", before, after)
	}
}

// TestMigration0007InterruptedLeavesTheSchemaVersionUnadvanced is TDD
// invariant 6's failure test, and it is the one that matters most for a
// migration that writes DATA: a half-applied backfill must be impossible,
// not merely unlikely.
//
// The interruption is planted by applying a variant of the real migration
// whose backfill cannot succeed, which is what an interrupted one looks
// like from the database's point of view: statements ran, and then one did
// not. Everything before it, the two CREATE TABLEs included, has to be
// gone afterwards, and the version has to still be 6.
func TestMigration0007InterruptedLeavesTheSchemaVersionUnadvanced(t *testing.T) {
	db, _ := openRaw(t)
	migrateUpTo(t, db, placementsMigrationVersion-1)
	seedArtifacts(t, db)
	ctx := context.Background()

	real := migrationSQL(t, placementsMigrationVersion)
	interrupted := real
	interrupted.sql = real.sql + "\nINSERT INTO placements (artifact_id, medium, location, created_at, updated_at) SELECT id, 'local', local_path, updated_at, updated_at FROM a_table_that_does_not_exist;\n"

	if err := applyMigration(ctx, db, interrupted); err == nil {
		t.Fatal("an interrupted migration reported success")
	}

	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		t.Fatalf("appliedMigrations: %v", err)
	}
	if _, recorded := applied[placementsMigrationVersion]; recorded {
		t.Errorf("schema_migrations records version %d after an interrupted run; a later start would then skip the migration entirely and run against a schema that was never finished", placementsMigrationVersion)
	}

	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('placements', 'placement_moves')`).Scan(&tables); err != nil {
		t.Fatalf("counting tables: %v", err)
	}
	if tables != 0 {
		t.Errorf("%d of the new tables survived an interrupted migration; the statements before the failure were not rolled back", tables)
	}

	// And the database is still usable: the real migration applies
	// cleanly afterwards, which is what a restart after a crash does.
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrating after an interrupted attempt: %v", err)
	}
	if countPlacements(t, db) == 0 {
		t.Error("the retry backfilled nothing")
	}
}

// retentionSnapshot is every column FR-18 reads to decide whether an
// artifact is kept, plus the identity it is keyed by.
type retentionSnapshot map[string]string

func snapshotRetention(t *testing.T, db *sql.DB) retentionSnapshot {
	t.Helper()
	rows, err := db.Query(`
		SELECT artifact_name, state, discovered_at, retention_tier,
		       COALESCE(retention_expires_at, ''), COALESCE(remote_mtime, ''),
		       local_path, local_hash, COALESCE(validation_passed, -1)
		  FROM artifacts ORDER BY artifact_name`)
	if err != nil {
		t.Fatalf("snapshotting: %v", err)
	}
	defer rows.Close()

	out := retentionSnapshot{}
	for rows.Next() {
		var name, st, discovered, tier, expires, mtime, localPath, localHash string
		var validation int64
		if err := rows.Scan(&name, &st, &discovered, &tier, &expires, &mtime, &localPath, &localHash, &validation); err != nil {
			t.Fatalf("scanning snapshot: %v", err)
		}
		out[name] = strings.Join([]string{st, discovered, tier, expires, mtime, localPath, localHash, fmt.Sprint(validation)}, "|")
	}
	if len(out) == 0 {
		t.Fatal("the snapshot is empty, so comparing it proves nothing")
	}
	return out
}

// TestMigration0007ChangesNothingRetentionReads is FR-35's compatibility
// claim held as a test: a deployment that upgrades and configures no
// medium must reach identical retention verdicts, and the only way this
// migration could change one is by touching a column retention reads.
//
// It compares every such column, artifact by artifact, across the
// migration. The planted violation the spec names for this guard is "a
// migration variant that rewrites retention_tier during backfill", and
// TestTheRetentionCompatibilityGuardCatchesARewrite below plants exactly
// that and requires this comparison to catch it.
func TestMigration0007ChangesNothingRetentionReads(t *testing.T) {
	db, _ := openRaw(t)
	migrateUpTo(t, db, placementsMigrationVersion-1)
	seedArtifacts(t, db)

	before := snapshotRetention(t, db)
	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	after := snapshotRetention(t, db)

	for name, want := range before {
		got, ok := after[name]
		if !ok {
			t.Errorf("artifact %q disappeared across the migration", name)
			continue
		}
		if got != want {
			t.Errorf("artifact %q changed across the migration:\n  before: %s\n  after:  %s", name, want, got)
		}
	}
	if len(after) != len(before) {
		t.Errorf("the migration changed the artifact count from %d to %d", len(before), len(after))
	}
}

// TestTheRetentionCompatibilityGuardCatchesARewrite is the planted
// violation, and it is what makes the test above worth having. A guard
// that cannot be shown to fire is not a guard.
func TestTheRetentionCompatibilityGuardCatchesARewrite(t *testing.T) {
	db, _ := openRaw(t)
	migrateUpTo(t, db, placementsMigrationVersion-1)
	seedArtifacts(t, db)
	ctx := context.Background()

	before := snapshotRetention(t, db)

	real := migrationSQL(t, placementsMigrationVersion)
	mutant := real
	// THE PLANTED VIOLATION: a backfill that also rewrites the retention
	// tier, the exact shape the spec names for this guard.
	mutant.sql = real.sql + "\nUPDATE artifacts SET retention_tier = 'daily' WHERE local_path <> '';\n"
	if err := applyMigration(ctx, db, mutant); err != nil {
		t.Fatalf("applying the mutant migration: %v", err)
	}

	after := snapshotRetention(t, db)

	caught := false
	for name, want := range before {
		if after[name] != want {
			caught = true
			break
		}
	}
	if !caught {
		t.Fatal("the retention snapshot comparison did not notice a backfill that rewrote retention_tier, so TestMigration0007ChangesNothingRetentionReads is not proven to be able to fail")
	}
}

func countPlacements(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM placements`).Scan(&n); err != nil {
		t.Fatalf("counting placements: %v", err)
	}
	return n
}

// --- the Go surface ---

// TestRecordCarriesItsPlacements is the read side: a migrated database's
// backfilled rows have to arrive on state.Record, or every caller that is
// supposed to ask the placements has nothing to ask.
func TestRecordCarriesItsPlacements(t *testing.T) {
	db, path := openRaw(t)
	migrateUpTo(t, db, placementsMigrationVersion-1)
	seedArtifacts(t, db)
	db.Close()

	ctx := context.Background()
	j, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, "complete.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}

	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.Placements) != 1 {
		t.Fatalf("Get returned %d placements, want 1", len(rec.Placements))
	}
	p := rec.Placements[0]
	if p.Medium != MediumLocal {
		t.Errorf("Medium = %q, want %q", p.Medium, MediumLocal)
	}
	if p.Location != "/backups/pg/complete.dump" {
		t.Errorf("Location = %q, want the artifact's local path", p.Location)
	}
	if p.Status != PlacementActive {
		t.Errorf("Status = %q, want %q", p.Status, PlacementActive)
	}

	// And through the list path, which loads many records at once and is
	// where a per-record query would otherwise be quietly forgotten.
	records, err := j.ListByBackupSet(ctx, set)
	if err != nil {
		t.Fatalf("ListByBackupSet: %v", err)
	}
	seen := 0
	for _, r := range records {
		if len(r.Placements) > 0 {
			seen++
		}
	}
	if seen == 0 {
		t.Error("ListByBackupSet returned no record carrying a placement, so the list path never loads them")
	}
}

// TestReadableLocalPathPrefersThePlacement is the sweep's own contract:
// the callers that ask "can I read this artifact locally" go through this
// accessor, so this is where the answer changes when a placement stops
// being local (#239) rather than in four packages at once.
func TestReadableLocalPathPrefersThePlacement(t *testing.T) {
	t.Run("an active local placement is the answer", func(t *testing.T) {
		rec := Record{
			LocalPath: "/backups/pg/stale.dump",
			Placements: []Placement{
				{Medium: MediumLocal, Location: "/backups/pg/actual.dump", Status: PlacementActive},
			},
		}
		got, ok := rec.ReadableLocalPath()
		if !ok || got != "/backups/pg/actual.dump" {
			t.Errorf("ReadableLocalPath() = %q, %v; want the placement's own location", got, ok)
		}
	})

	t.Run("a placement that is not active is not readable", func(t *testing.T) {
		rec := Record{
			Placements: []Placement{
				{Medium: MediumLocal, Location: "/backups/pg/gone.dump", Status: PlacementGone},
			},
		}
		if got, ok := rec.ReadableLocalPath(); ok {
			t.Errorf("ReadableLocalPath() = %q, true for a GONE placement", got)
		}
	})

	t.Run("a placement on a medium is not a local path", func(t *testing.T) {
		rec := Record{
			Placements: []Placement{
				{Medium: "offsite_s3", Location: "prefix/production/pg/a.dump", Status: PlacementActive},
			},
		}
		if got, ok := rec.ReadableLocalPath(); ok {
			t.Errorf("ReadableLocalPath() = %q, true for an object key on a storage medium", got)
		}
	})

	t.Run("no placements at all falls back to LocalPath", func(t *testing.T) {
		// The fallback is what makes this sweep provably behaviour-neutral
		// in Phase 1, and it is also what every hand-built Record in this
		// repository's tests relies on. #239 is where it goes away, once a
		// placement can legitimately be somewhere other than local.
		rec := Record{LocalPath: "/backups/pg/a.dump"}
		got, ok := rec.ReadableLocalPath()
		if !ok || got != "/backups/pg/a.dump" {
			t.Errorf("ReadableLocalPath() = %q, %v; want the record's own LocalPath", got, ok)
		}
	})

	t.Run("an empty LocalPath and no placements is not readable", func(t *testing.T) {
		if got, ok := (Record{}).ReadableLocalPath(); ok {
			t.Errorf("ReadableLocalPath() = %q, true on an empty record", got)
		}
	})
}

// TestRecordTransitionWritesThePlacementItIsGiven is the write side. state
// stores what it is told, exactly as it does for every other vocabulary it
// does not own, so the caller that creates the durable copy is the caller
// that names the placement.
func TestRecordTransitionWritesThePlacementItIsGiven(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.db")
	j, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, "a.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}

	if _, err := j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "k1", From: "", To: "DISCOVERED",
		RemotePath: "/remote/a.dump", OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("discover: %v", err)
	}

	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.Placements) != 0 {
		t.Fatalf("a freshly discovered artifact has %d placements; it has no copies yet", len(rec.Placements))
	}

	size := int64(42)
	final := "/backups/pg/a.dump"
	if _, err := j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "k2", From: "DISCOVERED", To: "COMMITTED",
		OccurredAt: time.Now().UTC(),
		LocalPath:  &final,
		Placement: &PlacementUpdate{
			Medium: MediumLocal, Location: final, Size: &size,
			Hash: "abc", HashAlg: "sha256", VerificationClass: "content",
			Status: PlacementActive,
		},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rec, err = j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.Placements) != 1 {
		t.Fatalf("after a commit the artifact has %d placements, want 1", len(rec.Placements))
	}
	if got, ok := rec.ReadableLocalPath(); !ok || got != final {
		t.Errorf("ReadableLocalPath() = %q, %v; want %q", got, ok, final)
	}

	// Recording the same placement again must update the one row rather
	// than add a second: FR-28's key layout is deterministic, so one
	// artifact has exactly one location per medium, and an interrupted
	// upload that resumes writes the same placement again.
	if _, err := j.RecordTransition(ctx, Transition{
		Artifact: artifact, Key: "k3", From: "COMMITTED", To: "COMPLETE",
		OccurredAt: time.Now().UTC(),
		Placement: &PlacementUpdate{
			Medium: MediumLocal, Location: final, Size: &size,
			Hash: "abc", HashAlg: "sha256", VerificationClass: "content",
			Status: PlacementActive,
		},
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	rec, err = j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.Placements) != 1 {
		t.Errorf("recording the same placement twice produced %d rows, want 1", len(rec.Placements))
	}
}

// driverParameterCeiling is the number of bound parameters this project's
// SQLite driver accepts in one statement.
//
// I measured it rather than quoting the familiar 999, which is a
// build-time default this driver does not use: on modernc.org/sqlite
// v1.57.0 a statement with 32,766 placeholders runs and one with 32,767
// fails with "too many SQL variables". TestTheDriverParameterCeilingIsWhereIThinkItIs
// below pins that, so a driver upgrade that moves the ceiling is caught
// here rather than in production.
const driverParameterCeiling = 32766

// TestTheDriverParameterCeilingIsWhereIThinkItIs is the measurement the
// read path's design rests on, kept as a test so it stays true.
//
// loadPlacementsFor re-runs the caller's predicate instead of listing the
// artifact ids it already holds, and the whole argument for paying a second
// predicate evaluation is that the id-listing shape has a ceiling. A
// measurement that stops being true silently would turn that argument into
// a story.
func TestTheDriverParameterCeilingIsWhereIThinkItIs(t *testing.T) {
	db, _ := openRaw(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE ceiling_probe (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("creating the probe table: %v", err)
	}

	ask := func(n int) error {
		ph := make([]string, n)
		args := make([]any, n)
		for i := range ph {
			ph[i] = "?"
			args[i] = int64(i)
		}
		rows, err := db.QueryContext(ctx,
			`SELECT id FROM ceiling_probe WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return err
		}
		return rows.Close()
	}

	if err := ask(driverParameterCeiling); err != nil {
		t.Fatalf("%d placeholders should be accepted by this driver, got: %v", driverParameterCeiling, err)
	}
	if err := ask(driverParameterCeiling + 1); err == nil {
		t.Fatalf("%d placeholders were accepted; the ceiling this read path is designed around has moved, so re-derive it and revisit loadPlacementsFor's doc", driverParameterCeiling+1)
	}
}

// TestListingABackupSetPastTheDriverParameterCeiling is the behavioural
// proof that the placement read has no per-artifact parameter limit.
//
// The shape this replaced built one placeholder per artifact and batched to
// stay under the ceiling above. This one re-runs the caller's predicate, so
// the statement binds two parameters whatever the set holds, and the way to
// prove that is to list a set larger than any placeholder-per-artifact read
// could have served in one statement. Every record still has to come back
// with its own placement and not somebody else's, which is the thing a read
// that lost or misattributed a chunk would fail.
//
// It seeds rows directly rather than through RecordTransition because the
// subject here is the read, and 32,767 separate write transactions would
// cost the gate minutes to prove nothing about it.
func TestListingABackupSetPastTheDriverParameterCeiling(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.db")
	j, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	count := driverParameterCeiling + 1
	now := formatTime(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC))

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("artifact-%05d.dump", i)
		final := "/backups/pg/" + name
		res, err := tx.ExecContext(ctx, `
			INSERT INTO artifacts (
				source, backup_set, artifact_name, remote_path, local_path, state,
				discovered_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			set.Source, set.Set, name, "/remote/"+name, final, "COMPLETE", now, now)
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("seeding artifact %d: %v", i, err)
		}
		rowID, err := res.LastInsertId()
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("seeding artifact %d: %v", i, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO placements (artifact_id, medium, location, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			rowID, MediumLocal, final, PlacementActive, now, now); err != nil {
			_ = tx.Rollback()
			t.Fatalf("seeding the placement for artifact %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	records, err := j.ListByBackupSet(ctx, set)
	if err != nil {
		t.Fatalf("ListByBackupSet over %d artifacts: %v; a read that binds one parameter per artifact fails here and that is the point", count, err)
	}
	if len(records) != count {
		t.Fatalf("ListByBackupSet returned %d records, want %d", len(records), count)
	}
	for _, rec := range records {
		if len(rec.Placements) != 1 {
			t.Fatalf("artifact %s came back with %d placements, want 1; a read that dropped or duplicated a chunk looks exactly like this", rec.Artifact, len(rec.Placements))
		}
		want := "/backups/pg/" + rec.Artifact.Name
		if got, ok := rec.ReadableLocalPath(); !ok || got != want {
			t.Fatalf("artifact %s resolved to %q (ok=%v), want %q; a read that misattributed a placement looks exactly like this", rec.Artifact, got, ok, want)
		}
	}
}

// journalShape renders every artifact row's retention- and
// lifecycle-relevant content, plus its placements, as comparable text.
func journalShape(t *testing.T, j *Journal, set model.BackupSetID) map[string]string {
	t.Helper()
	records, err := j.ListByBackupSet(context.Background(), set)
	if err != nil {
		t.Fatalf("ListByBackupSet: %v", err)
	}
	out := map[string]string{}
	for _, rec := range records {
		var placements []string
		for _, p := range rec.Placements {
			size := "nil"
			if p.Size != nil {
				size = fmt.Sprint(*p.Size)
			}
			placements = append(placements, strings.Join([]string{p.Medium, p.Location, size, p.Hash, p.HashAlg, p.VerificationClass, p.Status}, ","))
		}
		out[rec.Artifact.Name] = strings.Join([]string{
			rec.State, rec.RemotePath, rec.LocalPath, rec.LocalHash, rec.LocalHashAlg,
			rec.RetentionTier, rec.DiscoveredAt.UTC().Format(time.RFC3339Nano),
			"[" + strings.Join(placements, " | ") + "]",
		}, "|")
	}
	return out
}

// driveOneArtifact runs the nominal lifecycle sequence for one artifact,
// discover through complete, recording the durable local placement at
// COMMITTED exactly as internal/lifecycle's Commit does.
func driveOneArtifact(t *testing.T, j *Journal, set model.BackupSetID, name string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	artifact, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	size := int64(4096)
	final := "/backups/postgres-primary/" + name

	if _, err := j.Discover(ctx, artifact, name+":discover", "incoming/"+name, RemoteIdentity{Size: &size}, at); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	partial := final + ".partial"
	for _, tr := range []Transition{
		{Artifact: artifact, Key: name + ":transferring", From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: at, LocalPath: &partial},
		{Artifact: artifact, Key: name + ":transferred", From: "TRANSFERRING", To: "TRANSFERRED", OccurredAt: at, Transfer: &TransferResult{BytesTransferred: size}},
		{Artifact: artifact, Key: name + ":verifying", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: at},
		{Artifact: artifact, Key: name + ":verified", From: "VERIFYING", To: "VERIFIED", OccurredAt: at, Hashes: &HashUpdate{Hash: "abc123", Alg: "sha256"}},
		{Artifact: artifact, Key: name + ":committing", From: "VERIFIED", To: "COMMITTING", OccurredAt: at},
		{Artifact: artifact, Key: name + ":committed", From: "COMMITTING", To: "COMMITTED", OccurredAt: at, LocalPath: &final,
			Placement: &PlacementUpdate{Medium: MediumLocal, Location: final, Size: &size, Hash: "abc123", HashAlg: "sha256",
				VerificationClass: VerificationContent, Status: PlacementActive}},
		{Artifact: artifact, Key: name + ":pending", From: "COMMITTED", To: "REMOTE_DELETE_PENDING", OccurredAt: at},
		{Artifact: artifact, Key: name + ":complete", From: "REMOTE_DELETE_PENDING", To: "COMPLETE", OccurredAt: at,
			Retention: &RetentionUpdate{Tier: "daily"}},
	} {
		if _, err := j.RecordTransition(ctx, tr); err != nil {
			t.Fatalf("-> %s: %v", tr.To, err)
		}
	}
}

// TestAFullRunOnAMigratedDatabaseMatchesAFullRunOnAFreshOne is #236's
// INTEGRATION step: a pipeline run over a database that was migrated from
// the pre-placements schema, with rows already in it, has to produce
// exactly the journal rows a run on a brand new database produces, plus
// the placement rows.
//
// The pre-existing rows are checked too, and that is the half that would
// catch the failure worth catching: a migration that quietly altered
// artifacts it backfilled would show up here as a difference in rows the
// new run never touched.
func TestAFullRunOnAMigratedDatabaseMatchesAFullRunOnAFreshOne(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	// A database that came up on the current schema from nothing.
	freshPath := filepath.Join(t.TempDir(), "fresh.db")
	fresh, err := Open(ctx, freshPath)
	if err != nil {
		t.Fatalf("Open fresh: %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	driveOneArtifact(t, fresh, set, "new-artifact.dump", at)
	freshShape := journalShape(t, fresh, set)

	// A database that existed before placements did, with artifacts in it,
	// migrated in place.
	migratedPath := filepath.Join(t.TempDir(), "migrated.db")
	raw, err := sql.Open(driverName, migratedPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	migrateUpTo(t, raw, placementsMigrationVersion-1)
	seedArtifacts(t, raw)
	beforeMigration := snapshotRetention(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatalf("closing the raw handle: %v", err)
	}

	migrated, err := Open(ctx, migratedPath)
	if err != nil {
		t.Fatalf("Open migrated: %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })
	driveOneArtifact(t, migrated, set, "new-artifact.dump", at)

	migratedShape := journalShape(t, migrated, set)

	// The new artifact must look identical on both.
	got, ok := migratedShape["new-artifact.dump"]
	if !ok {
		t.Fatal("the run on the migrated database produced no row for the new artifact")
	}
	if want := freshShape["new-artifact.dump"]; got != want {
		t.Errorf("a full run produced a different row on a migrated database:\n  migrated: %s\n  fresh:    %s", got, want)
	}
	if !strings.Contains(got, "local,/backups/postgres-primary/new-artifact.dump,4096,abc123,sha256,content,ACTIVE") {
		t.Errorf("the new artifact carries no ACTIVE local placement: %s", got)
	}

	// And the rows that were already there are exactly as they were.
	raw2, err := sql.Open(driverName, migratedPath)
	if err != nil {
		t.Fatalf("sql.Open again: %v", err)
	}
	t.Cleanup(func() { _ = raw2.Close() })
	afterRun := snapshotRetention(t, raw2)
	for name, want := range beforeMigration {
		if got := afterRun[name]; got != want {
			t.Errorf("pre-existing artifact %q changed across the migration and a full run:\n  before: %s\n  after:  %s", name, want, got)
		}
	}
}
