package service

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestSnapshotSQLite_RestoreRecoversExactPreSnapshotBytes is the RED
// checklist's "a pre-migration SQLite snapshot exists and is restorable
// after a simulated mid-migration failure", at the snapshot mechanism's
// own level: simulate a migration attempt that corrupted the live file by
// mutating it directly after the snapshot was taken, and prove restore
// puts back exactly what was there before, byte for byte.
func TestSnapshotSQLite_RestoreRecoversExactPreSnapshotBytes(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	original := []byte("SQLite format 3\x00pretend this is real committed journal content")
	if err := os.WriteFile(dbPath, original, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	snap, err := snapshotSQLite(dbPath)
	if err != nil {
		t.Fatalf("snapshotSQLite: %v", err)
	}

	// Simulate a migration attempt that got partway through writing before
	// failing, leaving garbage in place of the pre-migration content.
	if err := os.WriteFile(dbPath, []byte("garbage: torn write from a simulated crash"), 0o600); err != nil {
		t.Fatalf("WriteFile (simulated corruption): %v", err)
	}

	if err := snap.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	got, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("ReadFile after restore: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("restored content = %q, want the original %q", got, original)
	}
}

// TestSnapshotSQLite_RestoreCapturesWALAndSHMSidecars proves the snapshot
// does not just cover the main file: a WAL-mode SQLite database can have
// committed data sitting in -wal, not yet checkpointed into the main
// file, and a snapshot that dropped it would silently lose that data on
// restore.
func TestSnapshotSQLite_RestoreCapturesWALAndSHMSidecars(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	walPath := dbPath + "-wal"

	if err := os.WriteFile(dbPath, []byte("main file content"), 0o600); err != nil {
		t.Fatalf("WriteFile (main): %v", err)
	}
	if err := os.WriteFile(walPath, []byte("wal content with committed data"), 0o600); err != nil {
		t.Fatalf("WriteFile (wal): %v", err)
	}

	snap, err := snapshotSQLite(dbPath)
	if err != nil {
		t.Fatalf("snapshotSQLite: %v", err)
	}

	if err := os.WriteFile(dbPath, []byte("corrupted main"), 0o600); err != nil {
		t.Fatalf("WriteFile (corrupt main): %v", err)
	}
	if err := os.WriteFile(walPath, []byte("corrupted wal"), 0o600); err != nil {
		t.Fatalf("WriteFile (corrupt wal): %v", err)
	}

	if err := snap.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	gotMain, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("ReadFile (main): %v", err)
	}
	if string(gotMain) != "main file content" {
		t.Errorf("main file content = %q, want %q", gotMain, "main file content")
	}
	gotWAL, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("ReadFile (wal): %v", err)
	}
	if string(gotWAL) != "wal content with committed data" {
		t.Errorf("wal content = %q, want %q", gotWAL, "wal content with committed data")
	}
}

// TestSnapshotSQLite_FreshDeployment_RestoreRemovesWhatMigrationCreated
// covers the very first startup, where dbPath does not exist yet: the
// snapshot must still be valid, and restoring it after a simulated
// failure must remove the file migration created rather than error out
// or leave it behind — "preserve the previous (nonexistent) data
// unchanged" for a fresh deployment.
func TestSnapshotSQLite_FreshDeployment_RestoreRemovesWhatMigrationCreated(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	snap, err := snapshotSQLite(dbPath)
	if err != nil {
		t.Fatalf("snapshotSQLite (nonexistent file): %v", err)
	}

	// Simulate migration creating the file before failing partway.
	if err := os.WriteFile(dbPath, []byte("partially migrated"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(dbPath+"-wal", []byte("wal from failed migration"), 0o600); err != nil {
		t.Fatalf("WriteFile (wal): %v", err)
	}

	if err := snap.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("Stat(dbPath) after restore = %v, want os.IsNotExist", err)
	}
	if _, err := os.Stat(dbPath + "-wal"); !os.IsNotExist(err) {
		t.Errorf("Stat(dbPath-wal) after restore = %v, want os.IsNotExist", err)
	}
}
