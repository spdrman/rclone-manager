package state

import (
	"context"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// TestBackupSetAddress_RecordsReplacesAndTellsAbsenceApartFromEmpty is the
// whole contract of the address record in one pass.
//
// The absence half is the half that matters. An id with no row means
// nothing was ever recorded about where it pointed, and a caller has to be
// able to tell that from an address whose fields are genuinely empty:
// collapsing the two would make every create over an unrecorded id look
// like a move away from "".
func TestBackupSetAddress_RecordsReplacesAndTellsAbsenceApartFromEmpty(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	set := haltSetID(t, "production", "postgres-primary")
	other := haltSetID(t, "production", "auth-config")
	at := time.Date(2026, 9, 3, 11, 4, 0, 0, time.UTC)

	if _, found, err := j.BackupSetAddress(ctx, set); err != nil || found {
		t.Fatalf("BackupSetAddress on a fresh journal = found %v, err %v; want not found and no error", found, err)
	}

	first := BackupSetAddress{
		Set: set, Host: "nas-1.internal", RemotePath: "/volume1/pg", LocalPath: "/srv/backups/pg", RecordedAt: at,
	}
	if err := j.RecordBackupSetAddress(ctx, first); err != nil {
		t.Fatalf("RecordBackupSetAddress: %v", err)
	}
	got, found, err := j.BackupSetAddress(ctx, set)
	if err != nil || !found {
		t.Fatalf("BackupSetAddress after recording = found %v, err %v", found, err)
	}
	if got.Host != first.Host || got.RemotePath != first.RemotePath || got.LocalPath != first.LocalPath {
		t.Errorf("read back %+v, want %+v", got, first)
	}
	if !got.RecordedAt.Equal(at) {
		t.Errorf("RecordedAt = %v, want %v", got.RecordedAt, at)
	}

	// One row per id, replaced in place: the question is where the id was
	// pointing LAST, and a second answer supersedes the first.
	second := BackupSetAddress{
		Set: set, Host: "nas-2.internal", RemotePath: "/volume2/pg", LocalPath: "/srv/backups/pg2", RecordedAt: at.Add(time.Hour),
	}
	if err := j.RecordBackupSetAddress(ctx, second); err != nil {
		t.Fatalf("RecordBackupSetAddress (second): %v", err)
	}
	got, _, err = j.BackupSetAddress(ctx, set)
	if err != nil {
		t.Fatalf("BackupSetAddress: %v", err)
	}
	if got.Host != second.Host || got.RemotePath != second.RemotePath || got.LocalPath != second.LocalPath {
		t.Errorf("read back %+v after re-recording, want %+v", got, second)
	}

	// Per set, not per deployment. Without this the test above would pass
	// against an implementation that ignored the id entirely.
	if _, found, err := j.BackupSetAddress(ctx, other); err != nil || found {
		t.Errorf("BackupSetAddress(%s) = found %v, err %v; recording one set's address must not answer for another", other, found, err)
	}

	// An empty host is a real answer (a local-type remote has none), and
	// must read back as a recorded address rather than as absence.
	local := haltSetID(t, "production", "local-set")
	if err := j.RecordBackupSetAddress(ctx, BackupSetAddress{
		Set: local, RemotePath: "/mnt/dumps", LocalPath: "/srv/backups/local", RecordedAt: at,
	}); err != nil {
		t.Fatalf("RecordBackupSetAddress (no host): %v", err)
	}
	gotLocal, found, err := j.BackupSetAddress(ctx, local)
	if err != nil || !found {
		t.Fatalf("BackupSetAddress(%s) = found %v, err %v; an address with no host is still an address", local, found, err)
	}
	if gotLocal.Host != "" {
		t.Errorf("Host = %q, want empty", gotLocal.Host)
	}
}

// TestBackupSetAddress_RefusesAnUnusableRecord: an id-less or time-less
// record would be a row nothing can ever match or reason about, so it is
// refused at the write rather than stored and puzzled over later.
func TestBackupSetAddress_RefusesAnUnusableRecord(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	if err := j.RecordBackupSetAddress(ctx, BackupSetAddress{RemotePath: "/x", LocalPath: "/y", RecordedAt: time.Now().UTC()}); err == nil {
		t.Error("recording an address with no backup set id was accepted")
	}
	if err := j.RecordBackupSetAddress(ctx, BackupSetAddress{
		Set: haltSetID(t, "production", "postgres-primary"), RemotePath: "/x", LocalPath: "/y",
	}); err == nil {
		t.Error("recording an address with no recorded_at was accepted")
	}
	if _, _, err := j.BackupSetAddress(ctx, model.BackupSetID{}); err == nil {
		t.Error("reading the address of a zero backup set id was accepted")
	}
}
