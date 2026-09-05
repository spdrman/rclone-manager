package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/discovery"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// The per-artifact walk, and the one boundary the whole shutdown-safety
// argument rests on.
//
// The case to read first is the shutdown after commit. A commit makes the
// local copy durable and the very next step deletes the source, so a
// cancellation between those two must stop, and it must stop without having
// issued the delete. The assertion is that DeleteRemote was never called at
// all, counted on the fake, because an assertion about the artifact's final
// state would pass against an implementation that deleted the source and
// then failed to record it. Its neighbour, the same walk with no shutdown,
// is the control that proves the delete does happen when nothing interrupts
// it, so the first test cannot be satisfied by a pipeline that has stopped
// deleting anything.
//
// The capacity case is the other refusal that has to be counted rather than
// inferred: FR-21 says a transfer known not to fit must not BEGIN, which is
// a claim about a call that was never made.
//
// The two regression cases are here because both bugs were invisible in a
// happy path. A missing local directory failed only on a first run into a
// fresh path, and a delete left half-done by a previous cycle needed a
// second cycle to resume it, which no single-pass test would ever reach.

// testBackupSet is the backup set almost every test in this package starts
// from: one include pattern, the rename completion strategy, and a caller-
// supplied local directory.
//
// The local directory is a parameter rather than a t.TempDir() call inside,
// because a test with two backup sets needs two roots and sharing one is how
// a fixture quietly proves that two sets can write over each other. Callers
// that need a different shape mutate the returned value rather than growing
// this signature, so the default stays readable.
func testBackupSet(t *testing.T, localDir string) config.BackupSet {
	t.Helper()
	return config.BackupSet{
		Name:       "postgres-primary",
		ID:         mustSetID(t, "production", "postgres-primary"),
		Include:    []string{"*.dump"},
		Completion: config.Completion{Strategy: "rename"},
		LocalPath:  localDir,
		RemotePath: "/backups",
	}
}

// discoverOneRecord runs the real discovery pass and returns the single
// record it produced, failing the test if it produced any other number.
//
// Going through internal/discovery rather than writing a DISCOVERED row by
// hand is what makes the tests built on it mean anything: the row carries
// whatever discovery actually records today, so a change to that shape shows
// up as a pipeline test failing rather than as a fixture that has quietly
// stopped resembling production.
//
// Insisting on exactly one is the guard that keeps the return value honest.
// A fixture that discovered two artifacts would otherwise hand back whichever
// came first out of a map.
func discoverOneRecord(t *testing.T, ctx context.Context, journal Journal, tr transport.Transport, source transport.Source, bs config.BackupSet) state.Record {
	t.Helper()
	res, err := discovery.Discover(ctx, discovery.Deps{Transport: tr, Journal: journal, Now: fixedNow(epoch)}, source, bs)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Discovered) != 1 {
		t.Fatalf("Discover: got %d discovered artifacts, want 1 (result=%+v)", len(res.Discovered), res)
	}
	return res.Discovered[0]
}

// epoch is the instant almost every test in this package pins its clock to,
// through fixedNow. A fixed instant rather than time.Now is what makes
// retention verdicts reproducible: GFS tiers are anchored on the civil date
// an instant falls in, so a suite reading the real clock would decide
// differently either side of midnight and either side of a DST change.
//
// It is deliberately in UTC and deliberately midday, so no arithmetic a test
// does to it lands on a day boundary by accident.
var epoch = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

// TestProcessArtifact_ShutdownAfterCommit_NeverCallsDeleteRemote is this
// PR's proof for FR-1's "shut down without initiating unsafe source
// deletion": a shutdown signal (ctx cancellation) that lands exactly at
// the boundary between Commit returning and DeleteRemote being called
// must never let DeleteRemote run.
//
// The hookJournal below cancels ctx itself, from inside RecordTransition,
// the instant the COMMITTING -> COMMITTED write lands, simulating a
// SIGTERM arriving in exactly that window. This is deliberately the worst
// case for this property: cancellation could not be timed any closer to
// the destructive step without landing inside Commit itself, which
// commit.go's own crash-safety proof already covers for a real process
// kill (see pipeline.go's processArtifact doc for why a plain
// ctx-cancellation observed cleanly between two function calls is a
// strictly easier case than that).
func TestProcessArtifact_ShutdownAfterCommit_NeverCallsDeleteRemote(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	source := transport.Source{ID: "shutdown-test"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload bytes", epoch.Unix())

	baseJournal := openJournal(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	journal := &hookJournal{Journal: baseJournal, onRecordTransition: func(t state.Transition, out state.Outcome) {
		if out.Record.State == string(lifecycle.Committed) {
			cancel()
		}
	}}

	rec := discoverOneRecord(t, context.Background(), journal, tr, source, bs)

	svc := New(&config.Config{}, journal, tr, nil)
	svc.processArtifact(ctx, source, bs, rec)

	// Assertions read against a fresh, uncancelled context: ctx above is
	// now deliberately Done(), and database/sql calls made with a done
	// context fail outright, which would make every read below fail for
	// the wrong reason.
	verifyCtx := context.Background()

	final, err := baseJournal.Get(verifyCtx, rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Committed) {
		t.Errorf("journal state = %q, want %q: a shutdown after Commit must leave the artifact exactly there, never advance it toward a remote delete", final.State, lifecycle.Committed)
	}

	if got := tr.deleteCallCount(); got != 0 {
		t.Errorf("DeleteRemote was called %d time(s), want 0: a shutdown observed before that call must prevent it from ever running", got)
	}

	if _, ok := tr.objects["backup.dump"]; !ok {
		t.Error("the remote object was removed, but a shutdown before DeleteRemote must leave it untouched")
	}

	localFinal := filepath.Join(localDir, "backup.dump")
	if _, err := os.Stat(localFinal); err != nil {
		t.Errorf("local final file %s: %v (a committed local copy must exist durably before any remote delete is even considered)", localFinal, err)
	}
}

// TestProcessArtifact_NoShutdown_CompletesAndDeletesRemote is
// TestProcessArtifact_ShutdownAfterCommit_NeverCallsDeleteRemote's control
// case: run the identical pipeline, with an identical fake transport and
// an uncancelled context, and confirm DeleteRemote *is* reached and the
// artifact *does* reach COMPLETE. Without this, a passing shutdown test
// would be equally consistent with "DeleteRemote is never called at all",
// which would prove nothing.
func TestProcessArtifact_NoShutdown_CompletesAndDeletesRemote(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	source := transport.Source{ID: "control-test"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload bytes", epoch.Unix())

	journal := openJournal(t)
	ctx := context.Background()

	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(&config.Config{}, journal, tr, nil)
	svc.processArtifact(ctx, source, bs, rec)

	final, err := journal.Get(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Complete) {
		t.Fatalf("journal state = %q, want %q (final record: %+v)", final.State, lifecycle.Complete, final)
	}

	if got := tr.deleteCallCount(); got < 1 {
		t.Errorf("DeleteRemote was called %d time(s), want at least 1", got)
	}
	if _, stillThere := tr.objects["backup.dump"]; stillThere {
		t.Error("the remote object should have been deleted once the artifact reached COMPLETE")
	}

	localFinal := filepath.Join(localDir, "backup.dump")
	if _, err := os.Stat(localFinal); err != nil {
		t.Errorf("local final file %s: %v (COMPLETE must retain the durable local copy)", localFinal, err)
	}
}

// TestProcessArtifact_CapacityRefusal_NeverStartsTransfer proves FR-21's
// "do not begin a transfer known not to fit safely" holds at this
// package's own call site: with a Thresholds that cannot possibly be met,
// admitCapacity must refuse before lifecycle.Transfer, and therefore
// Transport.CopyToLocal, is ever called.
func TestProcessArtifact_CapacityRefusal_NeverStartsTransfer(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	source := transport.Source{ID: "capacity-test"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload bytes", epoch.Unix())

	journal := openJournal(t)
	ctx := context.Background()

	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(&config.Config{}, journal, tr, nil)
	// No real filesystem has this much free space; CheckBeforeTransfer must
	// refuse regardless of what StatPath(localDir) actually reports.
	svc.Capacity.CriticalFreeBytes = 1 << 62
	svc.Capacity.WarningFreeBytes = 1 << 62

	svc.processArtifact(ctx, source, bs, rec)

	final, err := journal.Get(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Discovered) {
		t.Errorf("journal state = %q, want %q: a capacity refusal must leave the artifact untouched for a later retry", final.State, lifecycle.Discovered)
	}

	if got := tr.copyToLocalCalls(); got != 0 {
		t.Errorf("CopyToLocal was called %d time(s), want 0: a transfer known not to fit must never begin", got)
	}
}

// TestProcessArtifact_CreatesLocalDirectoryIfMissing is a regression test:
// capacity.StatPath needs an existing directory to statfs, and nothing
// upstream of admitCapacity (config.Validate only checks local_path is an
// absolute, traversal-free string, never that it exists on disk yet)
// creates a fresh backup set's local destination directory. Without this,
// a brand new deployment's very first cycle would fail every transfer
// with a capacity error before Transfer ever got a chance to create the
// directory itself as a side effect of the copy.
func TestProcessArtifact_CreatesLocalDirectoryIfMissing(t *testing.T) {
	// A subdirectory that does not exist yet, unlike every other test in
	// this file which passes t.TempDir() (already created) as LocalPath.
	localDir := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")
	bs := testBackupSet(t, localDir)
	source := transport.Source{ID: "mkdir-test"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload bytes", epoch.Unix())

	journal := openJournal(t)
	ctx := context.Background()
	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(&config.Config{}, journal, tr, nil)
	svc.processArtifact(ctx, source, bs, rec)

	final, err := journal.Get(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Complete) {
		t.Errorf("journal state = %q, want %q: a missing local directory must be created, not treated as a capacity refusal (final record: %+v)", final.State, lifecycle.Complete, final)
	}
}

// TestProcessArtifact_ResumesDeleteFromAPreviousCycle is a regression test:
// an artifact that already reached COMMITTED or REMOTE_DELETE_PENDING in
// an earlier call (for example because that cycle's own delete attempt
// hit a transient transport error, or because the process restarted
// between recording intent and actually deleting) must still be retried
// on a later call, not silently left stuck forever. An earlier version of
// processArtifact only ever reached the delete step for an artifact it
// had *just* committed in the same call, which meant an artifact
// resumed from a prior cycle's own COMMITTED/REMOTE_DELETE_PENDING row
// (exactly the case internal/reconcile's FR-17 table and this package's
// own RunCycle doc both assume gets retried) was silently never looked at
// again.
func TestProcessArtifact_ResumesDeleteFromAPreviousCycle(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	source := transport.Source{ID: "resume-delete-test"}

	tr := newFakeTransport()
	tr.put("backup.dump", "resume payload", epoch.Unix())
	// Simulate the first cycle's delete attempt failing for an
	// operational reason (a transient network error, in spirit) after
	// intent was already durably recorded: DeleteRemote's own contract is
	// that COMMITTED -> REMOTE_DELETE_PENDING lands before the delete call
	// is even attempted, so this leaves the journal at
	// REMOTE_DELETE_PENDING with the remote object still present.
	tr.deleteErr = errors.New("simulated transient delete failure")

	journal := openJournal(t)
	ctx := context.Background()
	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(&config.Config{}, journal, tr, nil)
	svc.processArtifact(ctx, source, bs, rec)

	afterFirstCall, err := journal.Get(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if afterFirstCall.State != string(lifecycle.RemoteDeletePending) {
		t.Fatalf("after the first call: journal state = %q, want %q (precondition for this test)", afterFirstCall.State, lifecycle.RemoteDeletePending)
	}
	if _, stillThere := tr.objects["backup.dump"]; !stillThere {
		t.Fatalf("the remote object should not have been removed yet (precondition for this test)")
	}

	// The transient condition has cleared; a later cycle calls
	// processArtifact again with the freshly re-read record.
	tr.deleteErr = nil
	svc.processArtifact(ctx, source, bs, afterFirstCall)

	final, err := journal.Get(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Complete) {
		t.Errorf("after the second call: journal state = %q, want %q: an artifact already at REMOTE_DELETE_PENDING must be retried, not left stuck", final.State, lifecycle.Complete)
	}
	if _, stillThere := tr.objects["backup.dump"]; stillThere {
		t.Error("the remote object was never deleted on the retried attempt")
	}
}

// stableBackupSet is testBackupSet's WP3.2 twin: a "stable"-strategy
// backup set instead of "rename", with both the discovery-time stability
// window (StableFor) and WP3.2's own additional deletion-safety delay
// (DeleteSafetyDelay) configured.
func stableBackupSet(t *testing.T, localDir string, safetyDelay time.Duration) config.BackupSet {
	t.Helper()
	bs := testBackupSet(t, localDir)
	bs.Completion = config.Completion{
		Strategy:          "stable",
		StableFor:         config.Duration(5 * time.Minute),
		DeleteSafetyDelay: config.Duration(safetyDelay),
	}
	return bs
}

// TestProcessArtifact_StableStrategy_DeleteGateWaitsForSafetyDelay is
// WP3.2's own boundary/INTEGRATION proof (docs/EPIC-B-multi-nas.md §71
// Work Package 3.2): a full transfer -> verify -> commit -> (WP3.2's
// stable-mode gate) -> delete-gate pass for a "stable"-strategy backup
// set, run twice against the identical fake transport and journal like
// TestProcessArtifact_TransientDeleteFailure_RetriedOnNextCycle above.
//
// The first call reaches COMMITTED and then finds the configured
// delete_safety_delay has not elapsed yet: remote deletion must not fire
// early, so the remote object must still be present and the journal must
// still read COMMITTED afterward. The second call, with svc.Now moved far
// enough past that same COMMITTED write, finds the delay satisfied and
// completes the delete exactly as a "rename"/"marker" backup set already
// does in TestProcessArtifact_NoShutdown_CompletesAndDeletesRemote.
func TestProcessArtifact_StableStrategy_DeleteGateWaitsForSafetyDelay(t *testing.T) {
	localDir := t.TempDir()
	safetyDelay := 10 * time.Minute
	bs := stableBackupSet(t, localDir, safetyDelay)
	source := transport.Source{ID: "stable-gate-test"}

	tr := newFakeTransport()
	// Well before epoch - StableFor, so discovery's own "stable"
	// completion check (internal/discovery/complete.go) already treats
	// this candidate as complete as of epoch, exactly like every other
	// fixture in this file that discovers at a fixed `epoch`.
	tr.put("backup.dump", "payload bytes", epoch.Add(-time.Hour).Unix())

	journal := openJournal(t)
	rec := discoverOneRecord(t, context.Background(), journal, tr, source, bs)

	svc := New(&config.Config{}, journal, tr, nil)
	svc.Now = fixedNow(epoch)
	svc.processArtifact(context.Background(), source, bs, rec)

	afterFirstCall, err := journal.Get(context.Background(), rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if afterFirstCall.State != string(lifecycle.Committed) {
		t.Fatalf("after the first call: journal state = %q, want %q: the stable-mode safety delay has not elapsed, so the pipeline must stop at COMMITTED, not advance toward a delete", afterFirstCall.State, lifecycle.Committed)
	}
	if got := tr.deleteCallCount(); got != 0 {
		t.Fatalf("after the first call: DeleteRemote was called %d time(s), want 0: remote deletion must not fire before the safety delay elapses", got)
	}
	if _, stillThere := tr.objects["backup.dump"]; !stillThere {
		t.Fatal("after the first call: the remote object should still be present, remote deletion must not fire early")
	}

	// A later cycle, run far enough past the first COMMITTED write that
	// the configured delete_safety_delay has genuinely elapsed.
	svc.Now = fixedNow(epoch.Add(safetyDelay + time.Minute))
	svc.processArtifact(context.Background(), source, bs, afterFirstCall)

	final, err := journal.Get(context.Background(), rec.Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Complete) {
		t.Fatalf("after the second call: journal state = %q, want %q: once the safety delay has elapsed, a stable-strategy artifact must be able to complete exactly like a rename/marker one", final.State, lifecycle.Complete)
	}
	if got := tr.deleteCallCount(); got != 1 {
		t.Fatalf("after the second call: DeleteRemote was called %d time(s), want exactly 1", got)
	}
	if _, stillThere := tr.objects["backup.dump"]; stillThere {
		t.Error("after the second call: the remote object was never deleted")
	}
}
