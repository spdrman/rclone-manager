package retention_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/migrations"
)

// The version a deployment sits at immediately before the placements
// migration, which is every deployment that has not upgraded into EPIC E.
const versionBeforePlacements = 6

// goldenRecSpec is one seeded artifact, chosen to land on a different part
// of the retention calendar from every other one, so a verdict that moves
// is visible rather than absorbed.
type goldenRecSpec struct {
	name       string
	state      lifecycle.State
	discovered time.Time
}

func goldenFixture() []goldenRecSpec {
	return []goldenRecSpec{
		{"too-old-everything", lifecycle.Complete, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"monthly-only", lifecycle.Committed, time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)},
		{"week-old-in-weekly", lifecycle.RemoteDeletePending, time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)},
		{"recent-daily", lifecycle.Complete, time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)},
		{"quarantined-newest", lifecycle.Quarantined, time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)},
		{"failed-record", lifecycle.Failed, time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)},
	}
}

// splitMigrationStatements mirrors internal/state/migrate.go's own
// splitter, because a test that stands a database up at an old version has
// to execute the very same migration files the runner would, statement for
// statement, or it is testing a schema nothing ever produced.
func splitMigrationStatements(script string) []string {
	var withoutComments strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		withoutComments.WriteString(line)
		withoutComments.WriteByte('\n')
	}
	var out []string
	for _, p := range strings.Split(withoutComments.String(), ";") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func migrationFile(t *testing.T, version int) (name string, content string) {
	t.Helper()
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	prefix := fmt.Sprintf("%04d_", version)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			b, err := migrations.FS.ReadFile(e.Name())
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			return strings.TrimSuffix(strings.TrimPrefix(e.Name(), prefix), ".sql"), string(b)
		}
	}
	t.Fatalf("no migration file for version %d", version)
	return "", ""
}

func applyOneMigration(t *testing.T, ctx context.Context, db *sql.DB, version int, sqlText, recordedName, recordedChecksum string) {
	t.Helper()
	for _, stmt := range splitMigrationStatements(sqlText) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("migration %d statement failed: %v\n%s", version, err, stmt)
		}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		version, recordedName, recordedChecksum, "2026-09-02T00:00:00Z"); err != nil {
		t.Fatalf("record migration %d: %v", version, err)
	}
}

// seedPreEpicEDatabase stands a database up at the last pre-EPIC-E schema
// version and fills it with the fixture, the way a real deployment about
// to upgrade looks.
func seedPreEpicEDatabase(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("bootstrap schema_migrations: %v", err)
	}
	for v := 1; v <= versionBeforePlacements; v++ {
		name, content := migrationFile(t, v)
		sum := sha256.Sum256([]byte(content))
		applyOneMigration(t, ctx, db, v, content, name, hex.EncodeToString(sum[:]))
	}

	for i, s := range goldenFixture() {
		occurred := s.discovered.UTC().Format(time.RFC3339Nano)
		if _, err := db.ExecContext(ctx, `
			INSERT INTO artifacts (id, source, backup_set, artifact_name, remote_path, local_path, state,
			                       discovered_at, updated_at, transfer_bytes, local_hash, local_hash_alg, retention_tier)
			VALUES (?, 'golden', 'baseline', ?, ?, ?, ?, ?, ?, 8192, 'cafe', 'sha256', '')`,
			i+1, s.name, "/remote/"+s.name, "/backups/baseline/"+s.name, string(s.state), occurred, occurred,
		); err != nil {
			t.Fatalf("seed %s: %v", s.name, err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO state_transitions (artifact_id, idempotency_key, from_state, to_state, occurred_at)
			VALUES (?, ?, '', ?, ?)`, i+1, "seed-"+s.name, string(s.state), occurred); err != nil {
			t.Fatalf("seed transition for %s: %v", s.name, err)
		}
	}
}

// verdictsAfterOpening opens the journal at path (which applies whatever
// migrations are outstanding, the placements one included), reads the
// backup set back, and renders the retention decision as a comparable
// string per artifact.
func verdictsAfterOpening(t *testing.T, ctx context.Context, path string) []string {
	t.Helper()
	j, err := state.Open(ctx, path)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer j.Close()

	set, err := model.NewBackupSetID("golden", "baseline")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	records, err := j.ListByBackupSet(ctx, set)
	if err != nil {
		t.Fatalf("ListByBackupSet: %v", err)
	}
	if len(records) != len(goldenFixture()) {
		t.Fatalf("read back %d records, want %d", len(records), len(goldenFixture()))
	}
	for _, rec := range records {
		if len(rec.Placements) != 1 {
			t.Fatalf("%s came back with %d placements, want the backfilled 1", rec.Artifact.Name, len(rec.Placements))
		}
	}

	var resolved config.Retention
	if err := config.ValidateRetention(&resolved); err != nil {
		t.Fatalf("ValidateRetention: %v", err)
	}
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	verdicts, _, err := retention.DecideKeep(now, resolved, set, records)
	if err != nil {
		t.Fatalf("DecideKeep: %v", err)
	}
	out := make([]string, 0, len(verdicts))
	for _, v := range verdicts {
		tiers := make([]string, 0, len(v.Tiers))
		for _, tier := range v.Tiers {
			tiers = append(tiers, tier.String())
		}
		out = append(out, fmt.Sprintf("%s keep=%v tiers=%s", v.Artifact.Name, v.Keep, strings.Join(tiers, "+")))
	}
	sort.Strings(out)
	return out
}

// The integration half of #236: a real pre-EPIC-E journal, migrated, read
// back, and put through the retention decision. The verdicts have to be
// the ones the golden baseline pins, which is the whole compatibility
// promise (FR-35) stated as an observation rather than as a sentence.
func TestRetentionVerdictsSurviveThePlacementsMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	seedPreEpicEDatabase(t, ctx, path)

	got := verdictsAfterOpening(t, ctx, path)
	want := []string{
		"monthly-only keep=true tiers=MONTHLY(discovery)",
		"recent-daily keep=true tiers=DAILY(discovery)+WEEKLY(discovery)+MONTHLY(discovery)+LAST_KNOWN_GOOD",
		"too-old-everything keep=false tiers=",
		"week-old-in-weekly keep=true tiers=WEEKLY(discovery)",
	}
	if len(got) != len(want) {
		t.Fatalf("after migrating, DecideKeep returned %d verdicts, want %d:\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("verdict %d after migrating:\n got: %s\nwant: %s", i, got[i], want[i])
		}
	}
}

// The planted violation the issue's TDD plan asks for, built as a real
// alternative build rather than as an argument: a 0007 whose backfill also
// writes to the artifact row, recorded in schema_migrations under the
// genuine file's checksum so nothing downstream can tell it apart from the
// real one. If this product ever shipped that migration, this is what an
// operator's retention calendar would do, and the check below is what
// refuses to let it through.
//
// The column it rewrites is discovered_at rather than retention_tier,
// which is what FR-29's own TDD note names. retention_tier is a recorded
// verdict that nothing in the decision path reads back, so a backfill that
// corrupted it would sail past a retention test and be caught only by the
// byte-for-byte artifact-row comparison in internal/state's own migration
// suite. discovered_at is the column the calendar is actually computed
// from, so a violation there is the one that silently deletes backups, and
// it is the one worth planting.
func TestAPlantedBackfillThatTouchesTheArtifactRowFailsTheGoldenVerdicts(t *testing.T) {
	ctx := context.Background()
	honest := filepath.Join(t.TempDir(), "honest.db")
	planted := filepath.Join(t.TempDir(), "planted.db")
	seedPreEpicEDatabase(t, ctx, honest)
	seedPreEpicEDatabase(t, ctx, planted)

	// The honest build's answer, read through the same path.
	honestVerdicts := verdictsAfterOpening(t, ctx, honest)

	// The planted build: 0007 verbatim, plus one extra statement in the
	// same transaction, stamped with the real 0007's checksum.
	_, content := migrationFile(t, 7)
	sum := sha256.Sum256([]byte(content))
	tampered := content + `
;
UPDATE artifacts SET discovered_at = updated_at, updated_at = updated_at
 WHERE local_path <> ''`
	// Nudge the calendar by rewriting discovered_at from a column a real
	// backfill might plausibly reach for. Give the rows a value that lands
	// them somewhere else entirely.
	tampered = strings.Replace(tampered,
		"UPDATE artifacts SET discovered_at = updated_at, updated_at = updated_at",
		"UPDATE artifacts SET discovered_at = '2026-08-28T09:00:00Z'", 1)

	db, err := sql.Open("sqlite", planted)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	applyOneMigration(t, ctx, db, 7, tampered, "placements", hex.EncodeToString(sum[:]))
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	plantedVerdicts := verdictsAfterOpening(t, ctx, planted)

	if len(honestVerdicts) == len(plantedVerdicts) {
		same := true
		for i := range honestVerdicts {
			if honestVerdicts[i] != plantedVerdicts[i] {
				same = false
				break
			}
		}
		if same {
			t.Fatalf("a backfill that rewrote every artifact's discovered_at produced identical retention verdicts, so the golden comparison in this file proves nothing:\n%v", honestVerdicts)
		}
	}
	t.Logf("planted build's verdicts differ, as they must:\n honest: %v\nplanted: %v", honestVerdicts, plantedVerdicts)
}
