package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Issue #282: a backup set whose source this manager may never delete from.
//
// Both cases are about a call that must not happen, which is why both count
// DeleteRemote on the fake rather than inspecting the artifact's final state.
// A read-only set's artifact still has to reach a durable, finished state; it
// just gets there without the source ever being touched.
//
// The resume case is the one that is easy to miss. An artifact already sitting
// at REMOTE_DELETE_PENDING was put there by a cycle that ran before the set
// was declared read-only, or by a config change since, and a pipeline that
// only checked the flag on the way IN would happily finish that pending delete
// on the next pass. The flag has to be honoured where the delete is issued,
// not where it is planned.

// TestProcessArtifact_ReadOnlyBackupSet_NeverCallsDeleteRemote is issue
// #282's critical acceptance criterion, proven the way the issue insists
// on: "not by asserting a refusal". tr is poisoned so
// fakeTransport.DeleteRemote fails this test the instant it is invoked,
// and every other condition is set up exactly like
// TestProcessArtifact_NoShutdown_CompletesAndDeletesRemote (this test's own
// control, below): an uncancelled context, a matching remote object
// (FR-16's identity check would reach ConfidenceStrong/Unchanged, so
// Preserve() would be false and a genuinely unprotected delete WOULD
// proceed), "rename" completion (no stable-safety-delay wait to sit
// through). The only thing different is bs.ReadOnly, and that alone has to
// be what keeps this artifact away from the transport, all the way through
// a real processArtifact call against a real SQLite journal.
func TestProcessArtifact_ReadOnlyBackupSet_NeverCallsDeleteRemote(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.ReadOnly = true
	source := transport.Source{ID: "readonly-test"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload bytes", epoch.Unix())
	tr.poison = t

	journal := openJournal(t)
	ctx := context.Background()

	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(&config.Config{}, journal, tr, nil)
	svc.processArtifact(ctx, source, bs, rec)

	final, err := journal.Get(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.RemoteRetained) {
		t.Fatalf("journal state = %q, want %q: a read-only backup set must reach the retained terminal state, not COMPLETE and not stuck at COMMITTED", final.State, lifecycle.RemoteRetained)
	}

	if got := tr.deleteCallCount(); got != 0 {
		t.Errorf("DeleteRemote was called %d time(s), want 0", got)
	}
	if _, stillThere := tr.objects["backup.dump"]; !stillThere {
		t.Error("the remote object was removed, but a read-only backup set must never delete its remote source")
	}

	localFinal := filepath.Join(localDir, "backup.dump")
	if _, err := os.Stat(localFinal); err != nil {
		t.Errorf("local final file %s: %v (a retained artifact must keep its durable local copy)", localFinal, err)
	}
}

// TestProcessArtifact_ReadOnlyBackupSet_ResumesFromRemoteDeletePending
// proves the second edge RetainRemote takes: an artifact that already
// recorded delete INTENT (REMOTE_DELETE_PENDING) on an earlier cycle,
// before its backup set was flipped to read-only, still reaches
// REMOTE_RETAINED and still never calls DeleteRemote on this cycle -- it
// is not stuck re-offering itself forever, and it is not "already too far
// gone to protect".
func TestProcessArtifact_ReadOnlyBackupSet_ResumesFromRemoteDeletePending(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	source := transport.Source{ID: "readonly-resume-test"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload bytes", epoch.Unix())
	// Simulate the first cycle's delete attempt failing for an operational
	// reason (a transient network error, in spirit) after intent was
	// already durably recorded, exactly as
	// TestProcessArtifact_ResumesDeleteFromAPreviousCycle does: this
	// leaves the journal at REMOTE_DELETE_PENDING with the remote object
	// still present, before the backup set was ever declared read-only.
	tr.deleteErr = errors.New("simulated transient delete failure")

	journal := openJournal(t)
	ctx := context.Background()

	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(&config.Config{}, journal, tr, nil)
	svc.processArtifact(ctx, source, bs, rec)
	tr.deleteErr = nil

	mid, err := journal.Get(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if mid.State != string(lifecycle.RemoteDeletePending) {
		t.Fatalf("setup: journal state = %q, want %q before the read-only run", mid.State, lifecycle.RemoteDeletePending)
	}

	// Now the operator has declared this set read-only, and the next
	// cycle picks the same artifact back up from REMOTE_DELETE_PENDING.
	// The counter is reset first: the failed attempt above legitimately
	// called DeleteRemote once, before the set was ever read-only, and
	// this test's claim is about the SECOND call only.
	tr.deleteRemoteCalls = 0
	bs.ReadOnly = true
	tr.poison = t
	svc.processArtifact(ctx, source, bs, mid)

	final, err := journal.Get(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.RemoteRetained) {
		t.Fatalf("journal state = %q, want %q", final.State, lifecycle.RemoteRetained)
	}
	if got := tr.deleteCallCount(); got != 0 {
		t.Errorf("DeleteRemote was called %d time(s), want 0", got)
	}
}
