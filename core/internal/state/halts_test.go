package state

import (
	"context"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// The halt table is the durable half of issue #245: a per-backup-set fact
// saying the manager could not connect to this set the last time it tried,
// and why. It is deliberately NOT part of the append-only artifact
// transition log, because a refused connection produces no artifact to
// transition, and it is not history: the only question it answers is "is
// this set refused right now", which the most recent observation answers
// on its own.

func haltSetID(t *testing.T, source, set string) model.BackupSetID {
	t.Helper()
	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		t.Fatalf("NewBackupSetID(%q, %q): %v", source, set, err)
	}
	return id
}

func haltsBySet(t *testing.T, j *Journal) map[string]BackupSetHalt {
	t.Helper()
	halts, err := j.ListBackupSetHalts(context.Background())
	if err != nil {
		t.Fatalf("ListBackupSetHalts: %v", err)
	}
	out := map[string]BackupSetHalt{}
	for _, h := range halts {
		if _, dup := out[h.Set.String()]; dup {
			t.Fatalf("ListBackupSetHalts returned %s twice: one backup set can only be refused for one reason at a time", h.Set)
		}
		out[h.Set.String()] = h
	}
	return out
}

// TestBackupSetHalt_RecordsListsAndClearsPerSet is the whole contract in
// one pass, and the clear half is the half that matters. A halt banner
// left standing on a set that has since connected is worse than no banner:
// it is confidently false.
func TestBackupSetHalt_RecordsListsAndClearsPerSet(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	refused := haltSetID(t, "production", "postgres-primary")
	other := haltSetID(t, "production", "auth-config")
	at := time.Date(2026, 8, 29, 4, 12, 8, 0, time.UTC)

	// Nothing has been observed yet, so nothing is claimed.
	if got := haltsBySet(t, j); len(got) != 0 {
		t.Fatalf("ListBackupSetHalts on a fresh journal = %v, want empty", got)
	}

	if err := j.RecordBackupSetHalt(ctx, refused, HaltHostKeyChanged, at); err != nil {
		t.Fatalf("RecordBackupSetHalt: %v", err)
	}
	if err := j.RecordBackupSetHalt(ctx, other, HaltAuthenticationFailed, at.Add(time.Minute)); err != nil {
		t.Fatalf("RecordBackupSetHalt(other): %v", err)
	}

	halts := haltsBySet(t, j)
	if len(halts) != 2 {
		t.Fatalf("ListBackupSetHalts = %v, want both sets", halts)
	}
	if got := halts[refused.String()]; got.Reason != HaltHostKeyChanged || !got.ObservedAt.Equal(at) {
		t.Errorf("halt for %s = %+v, want reason %q observed at %s", refused, got, HaltHostKeyChanged, at)
	}
	if got := halts[other.String()].Reason; got != HaltAuthenticationFailed {
		t.Errorf("halt reason for %s = %q, want %q: the two sets must not share one row", other, got, HaltAuthenticationFailed)
	}

	// A later cycle connects to the first set. Its halt goes; the other
	// set's stays, which is the positive control proving the clear is
	// scoped to one backup set rather than emptying the table.
	if err := j.ClearBackupSetHalt(ctx, refused); err != nil {
		t.Fatalf("ClearBackupSetHalt: %v", err)
	}
	cleared := haltsBySet(t, j)
	if _, still := cleared[refused.String()]; still {
		t.Errorf("halt for %s survived a clear: %v", refused, cleared)
	}
	if _, kept := cleared[other.String()]; !kept {
		t.Errorf("clearing %s also cleared %s: %v", refused, other, cleared)
	}
}

// TestBackupSetHalt_ReplacesTheEarlierReason proves one set has one halt.
// A set refused for a changed host key and later refused for a failed
// authentication is refused for the newer reason, not both.
func TestBackupSetHalt_ReplacesTheEarlierReason(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := haltSetID(t, "production", "postgres-primary")

	first := time.Date(2026, 8, 29, 4, 0, 0, 0, time.UTC)
	second := first.Add(30 * time.Minute)
	if err := j.RecordBackupSetHalt(ctx, set, HaltHostKeyChanged, first); err != nil {
		t.Fatalf("RecordBackupSetHalt: %v", err)
	}
	if err := j.RecordBackupSetHalt(ctx, set, HaltAuthenticationFailed, second); err != nil {
		t.Fatalf("RecordBackupSetHalt (second): %v", err)
	}

	halts := haltsBySet(t, j)
	if len(halts) != 1 {
		t.Fatalf("ListBackupSetHalts = %v, want exactly one row for one backup set", halts)
	}
	got := halts[set.String()]
	if got.Reason != HaltAuthenticationFailed {
		t.Errorf("reason = %q, want the newer %q", got.Reason, HaltAuthenticationFailed)
	}
	if !got.ObservedAt.Equal(second) {
		t.Errorf("observed_at = %s, want the newer observation at %s", got.ObservedAt, second)
	}
}

// TestBackupSetHalt_ClearingASetWithNoHaltIsNotAnError: a cycle that
// connects reports so for every set it reached, including the sets that
// were never refused, so the common case has to be a cheap no-op.
func TestBackupSetHalt_ClearingASetWithNoHaltIsNotAnError(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	never := haltSetID(t, "media", "weekly-archive")

	if err := j.ClearBackupSetHalt(ctx, never); err != nil {
		t.Fatalf("ClearBackupSetHalt on a set with no halt = %v, want nil", err)
	}

	// Positive control for that nil: the same call on a set that DOES
	// have a halt really does remove something, so the nil above is a
	// no-op rather than a clear that silently does nothing at all.
	refused := haltSetID(t, "production", "postgres-primary")
	if err := j.RecordBackupSetHalt(ctx, refused, HaltHostKeyChanged, time.Now().UTC()); err != nil {
		t.Fatalf("RecordBackupSetHalt: %v", err)
	}
	if got := haltsBySet(t, j); len(got) != 1 {
		t.Fatalf("ListBackupSetHalts = %v, want the one recorded halt", got)
	}
	if err := j.ClearBackupSetHalt(ctx, refused); err != nil {
		t.Fatalf("ClearBackupSetHalt: %v", err)
	}
	if got := haltsBySet(t, j); len(got) != 0 {
		t.Fatalf("ListBackupSetHalts = %v after clearing the only halt, want empty", got)
	}
}

// TestBackupSetHalt_RefusesAReasonTheSchemaDoesNotDeclare keeps the
// vocabulary pinned in the schema, the way operations.status already is.
// A reason nothing can render is worse than no reason, so an unknown one
// is refused at the write rather than served to an operator later.
func TestBackupSetHalt_RefusesAReasonTheSchemaDoesNotDeclare(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := haltSetID(t, "production", "postgres-primary")

	if err := j.RecordBackupSetHalt(ctx, set, "DISK_ON_FIRE", time.Now().UTC()); err == nil {
		t.Fatal("RecordBackupSetHalt accepted a reason the schema does not declare, want an error")
	}
	if got := haltsBySet(t, j); len(got) != 0 {
		t.Fatalf("a refused write still left %v behind", got)
	}

	// Positive control: the same call with a declared reason lands, so
	// the refusal above is the CHECK constraint doing its job rather than
	// every write failing.
	if err := j.RecordBackupSetHalt(ctx, set, HaltHostKeyChanged, time.Now().UTC()); err != nil {
		t.Fatalf("RecordBackupSetHalt with a declared reason: %v", err)
	}
	if got := haltsBySet(t, j); len(got) != 1 {
		t.Fatalf("ListBackupSetHalts = %v, want the one declared halt", got)
	}
}
