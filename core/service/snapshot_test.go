package service

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/testenv"
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

// TestSnapshotSQLite_RestoreRemovesTheShmInsteadOfWritingAStaleOneBack is
// the review's M7 fix. -shm is a derived index that has to correspond to
// the -wal beside it, and this snapshot reads its three files one after
// another rather than atomically, so a captured -shm cannot be trusted to
// match a restored -wal. SQLite rebuilds -shm from -wal whenever it is
// missing, so removing it is the only restore that is certainly right.
//
// The -wal assertion in the same test is the positive control: it proves
// the restore genuinely ran and put a sidecar back, so "-shm is gone"
// means "restore deliberately removed it", not "restore did nothing".
func TestSnapshotSQLite_RestoreRemovesTheShmInsteadOfWritingAStaleOneBack(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"

	for path, content := range map[string]string{
		dbPath:  "main file content",
		walPath: "wal content with committed data",
		shmPath: "shm index matching the wal above",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}

	snap, err := snapshotSQLite(dbPath)
	if err != nil {
		t.Fatalf("snapshotSQLite: %v", err)
	}

	for path, content := range map[string]string{
		dbPath:  "corrupted main",
		walPath: "corrupted wal",
		shmPath: "shm index for a wal that no longer exists",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile (corrupt) %s: %v", path, err)
		}
	}

	if err := snap.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if _, err := os.Stat(shmPath); !os.IsNotExist(err) {
		t.Errorf("Stat(-shm) after restore = %v, want os.IsNotExist: a captured -shm cannot be known to match the restored -wal, and SQLite rebuilds it from that -wal when it is absent", err)
	}
	gotWAL, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("ReadFile (wal): %v", err)
	}
	if string(gotWAL) != "wal content with committed data" {
		t.Errorf("wal content = %q, want %q — without this the -shm assertion above would pass against a restore that did nothing at all", gotWAL, "wal content with committed data")
	}
	gotMain, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("ReadFile (main): %v", err)
	}
	if string(gotMain) != "main file content" {
		t.Errorf("main file content = %q, want %q", gotMain, "main file content")
	}
}

// TestWriteFileAtomically_PersistsTheRenameByFsyncingTheDirectory pins the
// other half of M7. Fsyncing the temp file only promises its CONTENT
// survives a crash; the directory entry that makes that content reachable
// under the real name is persisted by fsyncing the DIRECTORY, and every
// caller of this function is already on a recovery path where "the restore
// silently did not happen after the reboot" is the worst possible outcome.
//
// A crash cannot be staged in a unit test, so the observable used here is
// the one operation the directory fsync needs and the rest of the function
// does not: opening the directory for reading. A directory with write and
// execute permission but no read permission lets CreateTemp and Rename
// through and refuses exactly that open, so this call must fail — and the
// second half, in an ordinary directory, is the positive control proving
// the failure came from the missing fsync rather than from the write
// itself being broken.
func TestWriteFileAtomically_PersistsTheRenameByFsyncingTheDirectory(t *testing.T) {
	testenv.RequirePermissionBitsApply(t)

	unreadableDir := filepath.Join(t.TempDir(), "no-read")
	if err := os.Mkdir(unreadableDir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(unreadableDir, 0o300); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadableDir, 0o700) })

	if err := writeFileAtomically(filepath.Join(unreadableDir, "state.db"), []byte("x"), 0o600); err == nil {
		t.Error("writeFileAtomically into a directory it cannot open: error = nil, want an error — the rename was never fsynced, so a crash right afterwards would lose it")
	}

	ordinaryDir := t.TempDir()
	if err := writeFileAtomically(filepath.Join(ordinaryDir, "state.db"), []byte("x"), 0o600); err != nil {
		t.Errorf("writeFileAtomically into an ordinary directory: %v, want nil", err)
	}
}
