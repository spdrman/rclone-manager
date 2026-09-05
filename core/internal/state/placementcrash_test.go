// What migration 7 leaves behind when the machine loses power partway
// through, proven by actually killing a process rather than by arguing from
// the Go code.
//
// The comment above crashEnvVar explains why the polite interruption tests
// in placements_test.go are not enough and how the child process is killed.
// The part worth knowing before reading any of it is why this is a separate
// file at all: the tests here fork the test binary back into itself, so
// they carry an environment variable protocol, a child entry point and a
// set of production pragmas that have to stay in step with Open. None of
// that belongs in the middle of a file about placements.

package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The two interruption tests already in placements_test.go plant a
// statement that cannot succeed, which proves applyMigration's own
// deferred rollback runs. That is a real interruption but it is the polite
// kind: the process is still alive and Go still gets to execute a defer.
//
// This one is the impolite kind, and it is the one an operator actually
// hits: the machine loses power, or systemd kills the unit, while
// migration 7 is halfway through writing a backfill. Nothing in this
// process runs afterwards. What the database looks like next time it is
// opened is then entirely SQLite's business, not the runner's, so it has
// to be proven against SQLite rather than argued from the Go code.
//
// The child opens the journal with the exact pragmas Open uses (WAL and
// synchronous = FULL are the two that decide what survives), begins a
// transaction, runs every statement of migration 7 including the backfill,
// and then dies with os.Exit before COMMIT. os.Exit runs no deferred
// function and closes nothing, which is as close to a kill -9 as a test
// can get from inside the process it is killing.
const crashEnvVar = "ARB_RCM_CRASH_DURING_MIGRATION_DB"

func TestMigration0007SurvivesTheProcessBeingKilledMidBackfill(t *testing.T) {
	if path := os.Getenv(crashEnvVar); path != "" {
		crashChild(path)
		return
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "journal.db")

	// Stand a populated database up at version 6, the way a deployment
	// about to take this upgrade actually looks.
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	for _, pragma := range productionPragmas {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			t.Fatalf("%s: %v", pragma, err)
		}
	}
	migrateUpTo(t, db, placementsMigrationVersion-1)
	seedArtifacts(t, db)
	if err := db.Close(); err != nil {
		t.Fatalf("closing before the crash run: %v", err)
	}

	// Now kill a process partway through migration 7.
	cmd := exec.Command(os.Args[0], "-test.run", "TestMigration0007SurvivesTheProcessBeingKilledMidBackfill")
	cmd.Env = append(os.Environ(), crashEnvVar+"="+path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the child was supposed to die before committing, but it exited cleanly:\n%s", out)
	}
	if !strings.Contains(string(out), "ARB-CHILD-REACHED-THE-BACKFILL") {
		t.Fatalf("the child died before it got as far as the backfill, so this proves nothing about an interrupted one:\n%s", out)
	}

	// Reopen exactly as the next start would, and see what SQLite left.
	reopened, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("reopening after the crash: %v", err)
	}
	defer reopened.Close()
	reopened.SetMaxOpenConns(1)
	for _, pragma := range productionPragmas {
		if _, err := reopened.ExecContext(ctx, pragma); err != nil {
			t.Fatalf("%s: %v", pragma, err)
		}
	}

	applied, err := appliedMigrations(ctx, reopened)
	if err != nil {
		t.Fatalf("appliedMigrations after the crash: %v", err)
	}
	if _, recorded := applied[placementsMigrationVersion]; recorded {
		t.Fatalf("after a process was killed mid-backfill, schema_migrations records version %d; the next start would skip the migration and run against a schema that was never finished",
			placementsMigrationVersion)
	}

	for _, table := range []string{"placements", "placement_moves"} {
		var name string
		err := reopened.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err == nil {
			t.Fatalf("after a process was killed mid-backfill, table %s survived; a half-built schema is exactly what the single transaction is there to prevent", table)
		}
		if err != sql.ErrNoRows {
			t.Fatalf("looking for %s after the crash: %v", table, err)
		}
	}

	// And the whole point: the next start migrates cleanly to current.
	if err := migrate(ctx, reopened); err != nil {
		t.Fatalf("migrating after an interrupted attempt: %v", err)
	}
	if n := countPlacementsDB(t, reopened); n == 0 {
		t.Fatal("the retried migration backfilled nothing")
	}
}

// productionPragmas has to be what Open sets, because the whole question
// here is what SQLite leaves on disk after a kill, and WAL plus
// synchronous=FULL are the two settings that decide it. A child that
// crashed under the defaults would prove something about a configuration
// this product does not ship.
var productionPragmas = []string{
	"PRAGMA journal_mode = WAL",
	"PRAGMA synchronous = FULL",
	"PRAGMA foreign_keys = ON",
	"PRAGMA busy_timeout = 5000",
}

// crashChild is the other half: it runs inside a subprocess, gets migration
// 7 as far as its backfill, and then dies without committing.
func crashChild(path string) {
	ctx := context.Background()
	db, err := sql.Open(driverName, path)
	if err != nil {
		_, _ = os.Stderr.WriteString("child: open: " + err.Error() + "\n")
		os.Exit(3)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range productionPragmas {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_, _ = os.Stderr.WriteString("child: " + pragma + ": " + err.Error() + "\n")
			os.Exit(3)
		}
	}

	known, err := loadMigrations()
	if err != nil {
		_, _ = os.Stderr.WriteString("child: loadMigrations: " + err.Error() + "\n")
		os.Exit(3)
	}
	var m migration
	for _, k := range known {
		if k.version == placementsMigrationVersion {
			m = k
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		_, _ = os.Stderr.WriteString("child: begin: " + err.Error() + "\n")
		os.Exit(3)
	}
	for _, stmt := range splitStatements(m.sql) {
		if strings.Contains(stmt, "INSERT INTO placements") {
			_, _ = os.Stderr.WriteString("ARB-CHILD-REACHED-THE-BACKFILL\n")
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_, _ = os.Stderr.WriteString("child: exec: " + err.Error() + "\n")
			os.Exit(3)
		}
	}
	// Everything migration 7 does has now been done, and none of it is
	// committed. Die here, running no defer and closing no handle.
	os.Exit(9)
}

// countPlacementsDB counts through a raw handle rather than a Journal,
// because after the crash the point is to see what is on disk without Open
// running migrations over it first and changing the answer.
func countPlacementsDB(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM placements`).Scan(&n); err != nil {
		t.Fatalf("counting placements: %v", err)
	}
	return n
}

// migrate turns foreign key enforcement OFF for the duration of a
// migration run, because SQLite's documented recipe for recreating a
// referenced table needs it (0002 and 0006 both DROP artifacts, and the
// implicit DELETE that runs first would otherwise trip every
// state_transitions row pointing at it).
//
// That trade is only safe because applyMigration runs PRAGMA
// foreign_key_check inside each migration's own transaction before
// committing it. If that check did not actually fire, suspending
// enforcement would be a straight loss: a migration could write a dangling
// reference into the journal and it would be committed in silence, and
// nothing downstream would ever look again.
//
// So this is the test for the half of that fix that is easy to leave
// vacuous. It plants a migration that inserts a placement pointing at an
// artifact id that does not exist, which is exactly what a mistake in a
// future migration would look like, and requires that it be refused and
// rolled back.
func TestAMigrationThatWouldLeaveADanglingReferenceIsRefused(t *testing.T) {
	ctx := context.Background()
	db, _ := openLikeProduction(t)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	before := countPlacementsDB(t, db)

	// Reproduce the conditions applyMigration actually runs under: migrate
	// suspends enforcement for the whole run, so this has to as well. With
	// enforcement still on, ordinary FK checking refuses the insert and
	// this test would pass without foreign_key_check existing at all,
	// which is the vacuous version of it.
	restore, err := suspendForeignKeys(ctx, db)
	if err != nil {
		t.Fatalf("suspendForeignKeys: %v", err)
	}
	defer restore()

	bad := migration{
		version:  9999,
		name:     "dangling",
		filename: "9999_dangling.sql",
		sql: `INSERT INTO placements (artifact_id, medium, location, created_at, updated_at)
		      VALUES (424242, 'local', '/nowhere', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');`,
		checksum: "deadbeef",
	}

	err = applyMigration(ctx, db, bad)
	if err == nil {
		t.Fatal("a migration that leaves a placement pointing at a missing artifact was accepted; with enforcement suspended for the run, PRAGMA foreign_key_check is the only thing standing between a bad migration and a silently corrupted journal")
	}
	if !strings.Contains(err.Error(), "dangling reference") {
		t.Fatalf("refused, but not by the foreign key check: %v", err)
	}

	if after := countPlacementsDB(t, db); after != before {
		t.Errorf("placements went from %d to %d; the refused migration was supposed to roll back", before, after)
	}
	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		t.Fatalf("appliedMigrations: %v", err)
	}
	if _, recorded := applied[9999]; recorded {
		t.Error("the refused migration was recorded as applied")
	}
}

// The positive control for the test above: the SAME insert, pointing at an
// artifact that does exist, is accepted. Without this, "refused" could
// just mean the insert was broken for some unrelated reason and the test
// would keep passing if foreign_key_check were deleted tomorrow.
func TestTheSameMigrationIsAcceptedWhenTheReferenceResolves(t *testing.T) {
	ctx := context.Background()
	db, _ := openLikeProduction(t)
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO artifacts (source, backup_set, artifact_name, remote_path, local_path,
			state, discovered_at, updated_at)
		VALUES ('production','pg','real.dump','/remote/real.dump','/local/real.dump',
			'COMMITTED','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatalf("seeding an artifact: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}

	restore, err := suspendForeignKeys(ctx, db)
	if err != nil {
		t.Fatalf("suspendForeignKeys: %v", err)
	}
	defer restore()

	good := migration{
		version:  9998,
		name:     "resolves",
		filename: "9998_resolves.sql",
		sql: fmt.Sprintf(`INSERT INTO placements (artifact_id, medium, location, created_at, updated_at)
		      VALUES (%d, 'local', '/local/real.dump', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');`, id),
		checksum: "cafebabe",
	}
	if err := applyMigration(ctx, db, good); err != nil {
		t.Fatalf("a migration whose reference resolves was refused: %v", err)
	}
}
