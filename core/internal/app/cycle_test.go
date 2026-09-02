package app

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
)

func testConfig(t *testing.T, sources ...config.Source) *config.Config {
	t.Helper()
	return &config.Config{Sources: sources, Retention: testRetention()}
}

// testRetention mirrors the defaults config.Validate fills in for a config
// that leaves the retention block entirely unset (see
// internal/config/validate.go's validateRetention), since these tests
// construct a config.Config by hand rather than through
// config.LoadAndValidate.
func testRetention() config.Retention {
	protect := true
	return config.Retention{
		Timezone:             "UTC",
		WeekStartsOn:         "monday",
		DailyDays:            7,
		WeeklyMonths:         3,
		MonthlyMonths:        12,
		ProtectLastKnownGood: &protect,
	}
}

func testSource(name string, backupSets ...config.BackupSet) config.Source {
	return config.Source{Name: name, BackupSets: backupSets}
}

// TestRunCycle_ProcessesArtifactThroughToComplete is a smoke test that
// RunCycle actually drives the whole reconcile -> discover -> transfer ->
// verify -> commit -> delete -> retention-preview sequence for a
// single, freshly-discovered artifact in one call, matching FR-1's "run
// performs one processing cycle" (a fresh artifact should not need
// multiple `run` invocations to reach a durable, retired state).
func TestRunCycle_ProcessesArtifactThroughToComplete(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = "" // fakeTransport ignores Source.Root, so this is inert here.

	tr := newFakeTransport()
	tr.put("backup.dump", "cycle payload", epoch.Unix())

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	report := svc.RunCycle(context.Background())

	if len(report.Sets) != 1 {
		t.Fatalf("len(report.Sets) = %d, want 1", len(report.Sets))
	}
	set := report.Sets[0]
	if set.Err != nil {
		t.Fatalf("BackupSetCycleResult.Err = %v, want nil", set.Err)
	}
	if len(set.Discovery.Discovered) != 1 {
		t.Fatalf("Discovery.Discovered = %+v, want exactly one artifact", set.Discovery.Discovered)
	}

	artifact := set.Discovery.Discovered[0].Artifact
	final, err := journal.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Complete) {
		t.Fatalf("journal state = %q, want %q", final.State, lifecycle.Complete)
	}

	if len(set.Retention.Verdicts) != 1 {
		t.Fatalf("Retention.Verdicts = %+v, want exactly one verdict", set.Retention.Verdicts)
	}
	if !set.Retention.Verdicts[0].Keep {
		t.Errorf("Retention.Verdicts[0].Keep = false, want true (the only backup in a set is always the newest and should be kept)")
	}
}

// TestRunCycle_ReconciliationLossCountsAsAFailedCycle is the adversarial
// review's High finding on PR #303: processBackupSet's own reconcile pass
// (FR-17, run before discovery and before this cycle's own forward
// pipeline) can, on its own, discover that a previously-durable
// artifact's local final copy has gone missing after its remote source
// was already cleaned up -- COMPLETE -> QUARANTINED_LOST, total,
// permanent loss of that restore point. Reconcile itself returns err ==
// nil for this: finding and recording the loss is reconciliation doing
// its job correctly, not a systemic failure. Before this fix, nothing
// folded recRep.Findings into result.FailedArtifacts, so a cycle that
// discovered this exact loss still reported success.
//
// The repro drives one artifact all the way to COMPLETE in a first
// RunCycle (mirroring TestRunCycle_ProcessesArtifactThroughToComplete),
// deletes its durable local copy out from under the journal, then runs a
// second RunCycle and confirms the loss reconciliation just found is
// counted toward FailedArtifacts, not silently logged and ignored.
func TestRunCycle_ReconciliationLossCountsAsAFailedCycle(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newFakeTransport()
	tr.put("backup.dump", "irrecoverable loss payload", epoch.Unix())

	journal := openJournal(t)
	ctx := context.Background()
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	first := svc.RunCycle(ctx)
	if len(first.Sets) != 1 || first.Sets[0].Err != nil {
		t.Fatalf("precondition: first RunCycle = %+v, want one clean set", first.Sets)
	}
	artifact := first.Sets[0].Discovery.Discovered[0].Artifact
	rec, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(lifecycle.Complete) {
		t.Fatalf("precondition: journal state = %q, want %q", rec.State, lifecycle.Complete)
	}

	if err := os.Remove(rec.LocalPath); err != nil {
		t.Fatalf("corrupting the durable local copy: %v", err)
	}

	second := svc.RunCycle(ctx)
	if len(second.Sets) != 1 {
		t.Fatalf("len(second.Sets) = %d, want 1", len(second.Sets))
	}
	set := second.Sets[0]
	if set.Err != nil {
		t.Fatalf("BackupSetCycleResult.Err = %v, want nil: reconciliation discovering a loss is not itself a systemic error", set.Err)
	}

	after, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != string(lifecycle.QuarantinedLost) {
		t.Fatalf("precondition: journal state after reconciliation = %q, want %q", after.State, lifecycle.QuarantinedLost)
	}

	if set.FailedArtifacts != 1 {
		t.Errorf("BackupSetCycleResult.FailedArtifacts = %d, want 1: reconciliation just moved a previously-durable artifact to QUARANTINED_LOST, an irrecoverable loss, and that must count toward this cycle's failure verdict exactly like a this-cycle FAILED/QUARANTINED outcome does", set.FailedArtifacts)
	}
}

// TestRunCycle_ReconciliationQuarantineCountsAsAFailedCycle is the
// QUARANTINED sibling of TestRunCycle_ReconciliationLossCountsAsAFailedCycle:
// an artifact stuck at REMOTE_DELETE_PENDING (the remote delete call
// itself failed, so the remote copy is still present) whose local final
// copy is found corrupted by reconciliation moves to QUARANTINED, not
// QUARANTINED_LOST, and that must count toward FailedArtifacts too.
func TestRunCycle_ReconciliationQuarantineCountsAsAFailedCycle(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newFakeTransport()
	tr.put("backup.dump", "quarantine payload", epoch.Unix())
	tr.deleteErr = errors.New("boom: remote delete refused")

	journal := openJournal(t)
	ctx := context.Background()
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	first := svc.RunCycle(ctx)
	if len(first.Sets) != 1 || first.Sets[0].Err != nil {
		t.Fatalf("precondition: first RunCycle = %+v, want one clean set", first.Sets)
	}
	artifact := first.Sets[0].Discovery.Discovered[0].Artifact
	rec, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(lifecycle.RemoteDeletePending) {
		t.Fatalf("precondition: journal state = %q, want %q (remote delete was made to fail so it stops here)", rec.State, lifecycle.RemoteDeletePending)
	}
	if first.Sets[0].FailedArtifacts != 0 {
		t.Fatalf("precondition: first RunCycle FailedArtifacts = %d, want 0", first.Sets[0].FailedArtifacts)
	}

	if err := os.Remove(rec.LocalPath); err != nil {
		t.Fatalf("corrupting the durable local copy: %v", err)
	}

	second := svc.RunCycle(ctx)
	set := second.Sets[0]
	if set.Err != nil {
		t.Fatalf("BackupSetCycleResult.Err = %v, want nil", set.Err)
	}

	after, err := journal.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != string(lifecycle.Quarantined) {
		t.Fatalf("precondition: journal state after reconciliation = %q, want %q", after.State, lifecycle.Quarantined)
	}

	if set.FailedArtifacts != 1 {
		t.Errorf("BackupSetCycleResult.FailedArtifacts = %d, want 1: reconciliation just quarantined a previously-committed artifact", set.FailedArtifacts)
	}
}

// TestRunCycle_SkipsADisabledBackupSet is issue #146 (B2.7)'s "Save
// disabled" wizard tier made concrete at the actual cycle level: a
// backup set with config.BackupSet.Disabled set is never reconciled,
// discovered or processed at all — it does not even appear in
// CycleReport.Sets — while a sibling, enabled set in the same cycle is
// processed normally. Disabled is checked in RunCycle's own loop
// (cycle.go), before processBackupSet is ever called for that set.
// fakeTransport's List (helpers_test.go) is not source-scoped — every
// configured backup set sees the same fake remote objects — so a single
// seeded artifact is enough here: what this test isolates is COUNT and
// IDENTITY of CycleReport.Sets, not which artifacts each set happens to
// discover.
func TestRunCycle_SkipsADisabledBackupSet(t *testing.T) {
	enabledDir := t.TempDir()
	enabledBS := testBackupSet(t, enabledDir)

	disabledDir := t.TempDir()
	disabledBS := testBackupSet(t, disabledDir)
	disabledBS.Name = "disabled-set"
	disabledBS.ID = mustSetID(t, "production", "disabled-set")
	disabledBS.Disabled = true

	tr := newFakeTransport()
	tr.put("backup.dump", "shared fake remote payload", epoch.Unix())

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", enabledBS, disabledBS)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	report := svc.RunCycle(context.Background())

	if len(report.Sets) != 1 {
		t.Fatalf("len(report.Sets) = %d, want exactly 1 (the disabled set must not appear at all): %+v", len(report.Sets), report.Sets)
	}
	if report.Sets[0].Set.String() != enabledBS.ID.String() {
		t.Fatalf("the one processed set = %q, want the enabled one %q", report.Sets[0].Set, enabledBS.ID)
	}
}

// TestRunCycle_ContinuesAfterOneBackupSetFails proves FR-1's "continue
// processing unrelated sources after one source fails": a systemic
// discovery failure for one backup set (its remote is entirely
// unreachable) must not stop RunCycle from still processing the next
// configured backup set.
func TestRunCycle_ContinuesAfterOneBackupSetFails(t *testing.T) {
	brokenDir := t.TempDir()
	brokenBS := testBackupSet(t, brokenDir)
	brokenBS.Name = "broken"
	brokenBS.ID = mustSetID(t, "production", "broken")

	healthyDir := t.TempDir()
	healthyBS := testBackupSet(t, healthyDir)
	healthyBS.Name = "healthy"
	healthyBS.ID = mustSetID(t, "production", "healthy")

	tr := newFakeTransport()
	tr.put("backup.dump", "healthy payload", epoch.Unix())
	// A plain, non-transport.Error is classified Unclassified by
	// transport.CategoryOf, so retry.Do's DefaultIsTransient treats it as
	// non-retryable and discoverOne fails on the first attempt, with no
	// backoff wait, keeping this test fast regardless of Service.RetryPolicy.
	tr.failErr = errors.New("boom: this source's remote is entirely unreachable")
	tr.failForSourceID = brokenBS.ID.String()

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", brokenBS, healthyBS)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	report := svc.RunCycle(context.Background())

	if len(report.Sets) != 2 {
		t.Fatalf("len(report.Sets) = %d, want 2 (one broken, one healthy)", len(report.Sets))
	}

	brokenResult := report.Sets[0]
	if brokenResult.Err == nil {
		t.Error("report.Sets[0].Err = nil, want the broken backup set's systemic discovery failure")
	}

	healthyResult := report.Sets[1]
	if healthyResult.Set != healthyBS.ID {
		t.Fatalf("report.Sets[1].Set = %s, want %s", healthyResult.Set, healthyBS.ID)
	}
	if healthyResult.Err != nil {
		t.Errorf("report.Sets[1].Err = %v, want nil: the healthy backup set must not be affected by the broken one", healthyResult.Err)
	}
	if len(healthyResult.Discovery.Discovered) != 1 {
		t.Errorf("the healthy backup set was not processed after the broken one: Discovery=%+v", healthyResult.Discovery)
	}
}
