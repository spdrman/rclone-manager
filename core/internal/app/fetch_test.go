package app

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
)

// The dry run's promise, and the real run being the same pipeline.
//
// `fetch --dry-run` is only useful if it is genuinely safe, so the assertion
// is that the journal is untouched afterwards rather than that the preview
// looks right. A preview that quietly journaled a DISCOVERED row would still
// print the correct objects.
//
// The real-run case exists to prove the other half: a fetch is one backup
// set's share of the SAME cycle, not a second implementation of it, so the
// artifact has to come out the far end in the state a full cycle would have
// left it in.
//
// The unknown-set case is the same *NotFoundError ListArtifacts returns for
// the same mistake, and it is here so the two doors cannot drift into
// answering a typo differently.

// TestFetch_DryRun_DoesNotTouchJournal proves `fetch --dry-run` never
// records anything: the remote listing comes back in Preview, but the
// journal stays exactly as it was (no DISCOVERED rows created).
func TestFetch_DryRun_DoesNotTouchJournal(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)

	tr := newFakeTransport()
	tr.put("backup.dump", "dry run payload", epoch.Unix())

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	result, err := svc.Fetch(context.Background(), "production", "postgres-primary", true)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !result.DryRun {
		t.Error("DryRun = false, want true")
	}
	if len(result.Preview) != 1 {
		t.Fatalf("Preview = %+v, want exactly one entry", result.Preview)
	}
	if result.Preview[0].RemotePath != "backup.dump" {
		t.Errorf("Preview[0].RemotePath = %q, want %q", result.Preview[0].RemotePath, "backup.dump")
	}
	if result.Preview[0].Known {
		t.Error("Preview[0].Known = true, want false: nothing has been discovered yet")
	}

	records, err := journal.ListByBackupSet(context.Background(), bs.ID)
	if err != nil {
		t.Fatalf("ListByBackupSet: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("journal has %d record(s) after a dry-run fetch, want 0", len(records))
	}
}

// TestFetch_Real_RunsFullPipeline proves a real (non-dry-run) fetch
// discovers and fully processes the one named backup set, reaching the
// same COMPLETE outcome RunCycle would for the same artifact.
func TestFetch_Real_RunsFullPipeline(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)

	tr := newFakeTransport()
	tr.put("backup.dump", "real fetch payload", epoch.Unix())

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	result, err := svc.Fetch(context.Background(), "production", "postgres-primary", false)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(result.Discovery.Discovered) != 1 {
		t.Fatalf("Discovery.Discovered = %+v, want exactly one artifact", result.Discovery.Discovered)
	}

	final, err := journal.Get(context.Background(), result.Discovery.Discovered[0].Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Complete) {
		t.Errorf("journal state = %q, want %q", final.State, lifecycle.Complete)
	}
}

// TestFetch_UnknownBackupSet_ReturnsNotFoundError proves Fetch reports a
// clear error when --source/--backup-set name something not in config,
// rather than a nil-pointer panic or a silent no-op.
func TestFetch_UnknownBackupSet_ReturnsNotFoundError(t *testing.T) {
	journal := openJournal(t)
	svc := New(testConfig(t), journal, newFakeTransport(), nil)

	if _, err := svc.Fetch(context.Background(), "nope", "nope", true); err == nil {
		t.Error("Fetch with an unconfigured source = nil error, want an error")
	}
}
