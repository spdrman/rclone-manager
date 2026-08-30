// This file is issue #104 (B3.4)'s INTEGRATION checklist item: "a
// boundary test for the full startup sequence against a real SQLite
// file, including a forced mid-migration crash and restart."
package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// TestStartupSequence_ForcedMidMigrationCrash_RestoresSnapshotAndRestartRecoversData
// exercises docs/EPIC-B-multi-nas.md §46.1's whole sequence against a
// real, previously-used SQLite file with real committed content, forces
// a crash that leaves the on-disk file genuinely unreadable, restores
// from the pre-crash snapshot exactly as OpenConfigAndJournal's own
// failure path does, and proves a real restart (a fresh
// OpenConfigAndJournal call) recovers cleanly with the original data
// intact.
//
// # Why the crash is injected by hand, between real snapshot and real
// restore calls, rather than by making one real OpenConfigAndJournal
// call fail mid-flight
//
// internal/state's migrate() is deliberately built so that every
// individual migration is atomic (one SQL transaction each) and the
// whole batch is safely resumable (a later attempt just continues from
// whatever was already committed) — see migrate.go's own doc. Its only
// two failure modes (ErrUnknownSchemaVersion, ErrSchemaDrift) are both
// detected BEFORE any new SQL runs in that attempt, so neither one
// actually leaves fresh, torn writes on disk for a snapshot to need to
// undo. The only way this database's live file becomes genuinely
// unreadable garbage — the actual "left anything partially applied" case
// the pre-migration snapshot exists to guard against — is an external
// event (a real power-loss crash, filesystem corruption, bit rot) that
// no amount of internal/state's own transactional care can prevent, and
// that this test cannot honestly reproduce by making a single Go
// function call fail in the middle. So this test drives
// snapshotSQLite/restore exactly as OpenConfigAndJournal itself does, in
// the same order, injecting that external event by hand at the one point
// nothing internal/state does defends against it — directly proving the
// mechanism this issue adds, without pretending to reproduce a real
// crash's timing inside a single process.
func TestStartupSequence_ForcedMidMigrationCrash_RestoresSnapshotAndRestartRecoversData(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	// Build a real journal with real committed content, exactly what a
	// production journal would have before an upgrade runs.
	journal, err := state.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("state.Open (seed): %v", err)
	}
	setID, err := model.NewBackupSetID("alpha", "one")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(setID, "backup.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	if _, err := journal.Discover(
		context.Background(), artifact, "seed-discover", "/remote/backup.dump",
		state.RemoteIdentity{}, time.Now().UTC(),
	); err != nil {
		t.Fatalf("Discover (seed): %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close seed journal: %v", err)
	}

	// Section 46.1's "backup/prepare SQLite as required" step, exactly as
	// OpenConfigAndJournal itself performs it.
	snap, err := snapshotSQLite(dbPath)
	if err != nil {
		t.Fatalf("snapshotSQLite: %v", err)
	}

	// The forced crash: truncate the live file to a handful of bytes,
	// simulating an interrupted write that left neither a valid header
	// nor readable page data — a real SQLite driver refuses to open this,
	// exactly like a real torn write from a power loss would be refused.
	if err := os.Truncate(dbPath, 16); err != nil {
		t.Fatalf("Truncate (simulated crash): %v", err)
	}

	// The failed "run transactional migrations" step: a real state.Open
	// call against the now-corrupted file must fail.
	_, openErr := state.Open(context.Background(), dbPath)
	if openErr == nil {
		t.Fatal("state.Open against a truncated database file: error = nil, want an error")
	}

	// The failure-path restore, exactly as OpenConfigAndJournal performs
	// it when state.Open fails.
	if err := snap.restore(); err != nil {
		t.Fatalf("restore after simulated crash: %v", err)
	}

	// The restart: a fresh OpenConfigAndJournal call (the real, complete
	// production entry point, lock and all) against the now-restored file
	// must succeed and see the original data, proving the restored bytes
	// are not merely byte-identical but a genuinely valid, usable SQLite
	// database.
	_, restarted, err := OpenConfigAndJournal(context.Background(), writeConfigFileFor(t, dir, dbPath))
	if err != nil {
		t.Fatalf("OpenConfigAndJournal after restore (the restart): %v", err)
	}
	defer func() { _ = restarted.Close() }()

	rec, err := restarted.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if rec.State != "DISCOVERED" {
		t.Errorf("recovered artifact state = %q, want %q", rec.State, "DISCOVERED")
	}
}
