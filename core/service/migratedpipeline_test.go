package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/migrations"
)

// schemaBeforePlacements is the version a deployment sits at just before
// the FR-29 placements migration.
const schemaBeforePlacements = 6

// standUpPreEpicEJournal builds a journal at dbPath standing at the last
// pre-EPIC-E schema version, with one artifact already in it, by executing
// the very migration files the runner would. A fresh database created by
// state.Open would be at the current version and would prove nothing about
// an upgrade.
func standUpPreEpicEJournal(t *testing.T, dbPath, source, set, name, localPath, hash string, size int64) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("bootstrap schema_migrations: %v", err)
	}

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for v := 1; v <= schemaBeforePlacements; v++ {
		prefix := fmt.Sprintf("%04d_", v)
		var file string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), prefix) {
				file = e.Name()
			}
		}
		if file == "" {
			t.Fatalf("no migration file for version %d", v)
		}
		content, err := migrations.FS.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		// The runner's own splitter: strip line comments, split on ";".
		var stripped strings.Builder
		for _, line := range strings.Split(string(content), "\n") {
			if i := strings.Index(line, "--"); i >= 0 {
				line = line[:i]
			}
			stripped.WriteString(line)
			stripped.WriteByte('\n')
		}
		for _, stmt := range strings.Split(stripped.String(), ";") {
			if s := strings.TrimSpace(stmt); s != "" {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("migration %d: %v\n%s", v, err, s)
				}
			}
		}
		sum := sha256.Sum256(content)
		if _, err := db.Exec(
			`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			v, strings.TrimSuffix(strings.TrimPrefix(file, prefix), ".sql"), hex.EncodeToString(sum[:]), "2026-09-02T00:00:00Z",
		); err != nil {
			t.Fatalf("record migration %d: %v", v, err)
		}
	}

	if _, err := db.Exec(`
		INSERT INTO artifacts (id, source, backup_set, artifact_name, remote_path, local_path, state,
		                       discovered_at, updated_at, transfer_bytes, local_hash, local_hash_alg, retention_tier)
		VALUES (1, ?, ?, ?, ?, ?, 'COMPLETE', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z', ?, ?, 'sha256', 'daily')`,
		source, set, name, "/remote/"+name, localPath, size, hash); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO state_transitions (artifact_id, idempotency_key, from_state, to_state, occurred_at)
		VALUES (1, 'seed', 'VERIFYING', 'VERIFIED', '2026-01-01T12:00:00Z')`); err != nil {
		t.Fatalf("seed transition: %v", err)
	}
}

// The integration case #236's TDD plan asks for: a real deployment's
// journal, upgraded in place, then a full pipeline run over it.
//
// It is not the same thing as running the pipeline against a fresh
// database, which every other test in this package does. The artifact that
// was already there got its placement from the migration's backfill; the
// artifact the cycle discovers gets its placement from the journal's own
// writer. Both have to come out right, and the pre-existing row has to
// come out of the upgrade unchanged.
func TestFullCycleOnAJournalUpgradedInPlace(t *testing.T) {
	ctx := context.Background()
	configPath := writeTestConfigFile(t)
	dir := filepath.Dir(configPath)
	dbPath := filepath.Join(dir, "state.db")
	preExistingLocal := filepath.Join(dir, "local", "already-here.dump")

	// A real file with a real hash, because reconciliation checks. An
	// artifact whose local copy is missing is correctly quarantined on the
	// first cycle, which would make this test about that instead.
	payload := []byte("a backup this deployment took before it had ever heard of placements")
	if err := os.MkdirAll(filepath.Dir(preExistingLocal), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(preExistingLocal, payload, 0o644); err != nil {
		t.Fatalf("write the pre-existing artifact: %v", err)
	}
	sum := sha256.Sum256(payload)
	preExistingHash := hex.EncodeToString(sum[:])

	standUpPreEpicEJournal(t, dbPath, "production", "postgres-primary", "already-here.dump",
		preExistingLocal, preExistingHash, int64(len(payload)))

	svc, cleanup, err := Open(ctx, configPath)
	if err != nil {
		t.Fatalf("Open on an upgraded journal: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	runOneCycle(t, svc)

	if err := cleanup(); err != nil {
		t.Fatalf("closing the service: %v", err)
	}

	journal, err := state.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen journal: %v", err)
	}
	defer func() { _ = journal.Close() }()

	setID, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	records, err := journal.ListByBackupSet(ctx, setID)
	if err != nil {
		t.Fatalf("ListByBackupSet: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("the backup set holds %d artifacts, want the pre-existing one plus the one this cycle ingested: %+v", len(records), records)
	}

	for _, rec := range records {
		if len(rec.Placements) != 1 {
			t.Errorf("%s has %d placements, want exactly 1", rec.Artifact.Name, len(rec.Placements))
			continue
		}
		p := rec.Placements[0]
		if p.Medium != state.MediumLocal || !p.IsActive() {
			t.Errorf("%s: placement is %s/%s, want an active local one", rec.Artifact.Name, p.Medium, p.Status)
		}
		if p.Location != rec.LocalPath {
			t.Errorf("%s: placement location %q does not match the journal's local path %q", rec.Artifact.Name, p.Location, rec.LocalPath)
		}

		switch rec.Artifact.Name {
		case "already-here.dump":
			// Untouched by the upgrade and untouched by the cycle: the
			// backfill restated what was there and added nothing.
			if rec.State != "COMPLETE" || rec.LocalHash != preExistingHash || rec.LocalPath != preExistingLocal {
				t.Errorf("the pre-existing artifact changed across the upgrade: %+v", rec)
			}
			if p.Hash != preExistingHash || p.HashAlg != "sha256" {
				t.Errorf("the backfilled placement carries hash %q/%q, want the journal's own %s/sha256", p.Hash, p.HashAlg, preExistingHash)
			}
			if p.VerificationClass != state.VerificationContent {
				t.Errorf("the backfilled placement's class is %q, want %q: the journal holds both a hash and a VERIFIED entry",
					p.VerificationClass, state.VerificationContent)
			}
		case "backup.dump":
			// The one this cycle actually ingested, end to end on the
			// migrated schema.
			if rec.State != "COMPLETE" {
				t.Errorf("the newly ingested artifact is %s, want COMPLETE", rec.State)
			}
			if p.Hash == "" || p.HashAlg == "" {
				t.Errorf("the new artifact's placement carries no hash: %+v", p)
			}
			if p.VerificationClass != state.VerificationContent {
				t.Errorf("the new artifact's placement class is %q, want %q after the pipeline's own read-back",
					p.VerificationClass, state.VerificationContent)
			}
			if p.SizeBytes == nil {
				t.Error("the new artifact's placement records no size, though the transfer measured one")
			}
		default:
			t.Errorf("unexpected artifact %q", rec.Artifact.Name)
		}
	}
}
