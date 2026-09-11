package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/obs"
	"github.com/backupdproject/backupd/core/internal/retention"
)

// FR-30's hold, seen from outside internal/retention (issue #602).
//
// The hold itself is proven over there: when the copy FR-19 reports as
// protected is not on disk, every delete in the pass becomes a REFUSE
// carrying a sentence that says so. What this file is about is that the
// sentence reaches somebody.
//
// It is the one refusal in prune.go that means "reconciliation is needed"
// rather than "retention decided not to delete this". Every other refusal
// there is a routine outcome of a plan, and the plan is where an operator
// reads it. This one says the backup set's own inventory disagrees with
// its journal, retention for that set is stopped until somebody fixes it,
// and nothing about the deployment looks wrong in the meantime: local
// copies simply accumulate, silently, until FR-21's capacity refusal
// starts refusing transfers for a reason that has nothing to do with the
// cause. So it is emitted as a warning when an apply hits it, and it is a
// standing condition on the set's own health for as long as it lasts.

// heldRetentionFixture is one backup set holding two managed-complete
// artifacts, both outside every tier, with the newest one's file removed
// out of band. That is exactly the shape issue #602 is about: FR-19 goes
// on protecting the newest ROW, and the only readable copy left in the
// set is the older artifact that nothing keeps.
//
// The removal is the fixture's whole content, so the control that leaves
// the file where it is takes the same path with one boolean flipped.
func heldRetentionFixture(t *testing.T, stream *bytes.Buffer, removeNewest bool) (*Service, model.BackupSetID, string) {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	bs := testBackupSet(t, dir)
	cfg := testConfig(t, testSource("production", bs))
	cfg.Retention = pruneDailyOnlyRetention()
	resolveTestRetention(cfg)

	journal := openJournal(t)
	var logger *obs.Logger
	if stream != nil {
		logger = obs.New(stream, obs.LevelInfo)
	}
	svc := New(cfg, journal, newFakeTransport(), logger)
	svc.Now = fixedNow(retentionTestNow)

	seedMovableArtifact(t, ctx, journal, bs, "newest.dump", retentionTestNow.AddDate(0, 0, -40))
	seedMovableArtifact(t, ctx, journal, bs, "older.dump", retentionTestNow.AddDate(0, 0, -90))

	if removeNewest {
		newestPath := filepath.Join(dir, "newest.dump")
		if err := os.Remove(newestPath); err != nil {
			t.Fatalf("removing %s out of band: %v", newestPath, err)
		}
	}
	return svc, configuredSet(t, cfg), filepath.Join(dir, "older.dump")
}

// configuredSet is the one backup set this fixture declares, read back out
// of the resolved configuration rather than rebuilt by hand, so a test can
// never ask about a set the service was not given.
func configuredSet(t *testing.T, cfg *config.Config) model.BackupSetID {
	t.Helper()
	for _, src := range cfg.Sources {
		for _, bs := range src.BackupSets {
			return bs.ID
		}
	}
	t.Fatal("no backup set in this fixture's configuration")
	return model.BackupSetID{}
}

// TestPruneApply_AHeldPassIsLoggedAtWarn is the observability half.
//
// A wedged retention pass currently says nothing anywhere: the refusal
// exists only inside a verdict list somebody has to go and ask for. That
// is fine for a REFUSE about one artifact and wrong for this one, which
// stops retention for the whole backup set until a human acts.
func TestPruneApply_AHeldPassIsLoggedAtWarn(t *testing.T) {
	ctx := context.Background()
	var stream bytes.Buffer
	svc, set, olderPath := heldRetentionFixture(t, &stream, true)

	// The preview first, and it must stay quiet. An operator refreshing a
	// dashboard is not an event, and a set that is genuinely broken would
	// otherwise write one warning per poll for as long as it stayed
	// broken, which is how a real signal gets filtered out.
	if _, err := svc.PrunePreview(ctx, set); err != nil {
		t.Fatalf("PrunePreview: %v", err)
	}
	if got := stream.String(); strings.Contains(got, obs.EventRetentionHold) {
		t.Errorf("a preview emitted %s. Previewing is reading, and a broken backup set would emit one of these per dashboard poll:\n%s", obs.EventRetentionHold, got)
	}

	plan, err := svc.PruneApply(ctx, set)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}

	// The precondition: the apply really did hold, which is what there is
	// to be told about.
	held := 0
	for _, v := range plan.Verdicts {
		if v.Action == retention.PruneRefuse && v.HoldReason != "" {
			held++
		}
	}
	if held == 0 {
		t.Fatalf("precondition: no verdict in this pass carries a hold, so there is nothing for a warning to be about: %+v", plan.Verdicts)
	}
	if _, err := os.Lstat(olderPath); err != nil {
		t.Fatalf("precondition: %s was deleted, so this fixture is not the one this test is about: %v", olderPath, err)
	}

	logged := stream.String()
	if !strings.Contains(logged, obs.EventRetentionHold) {
		t.Errorf("an apply held every delete in %s and logged no %s event. The only symptom left is local copies accumulating until FR-21 starts refusing transfers, which names neither this backup set nor this cause. Log:\n%s",
			set, obs.EventRetentionHold, logged)
	}
	if !strings.Contains(logged, `"level":"WARN"`) {
		t.Errorf("the hold was not logged at WARN. An alert rule keyed on level is the thing this event exists for. Log:\n%s", logged)
	}
	if !strings.Contains(logged, "newest.dump") {
		t.Errorf("the logged hold does not name the artifact whose copy could not be confirmed, so nobody reading it knows what to reconcile. Log:\n%s", logged)
	}
	if n := strings.Count(logged, obs.EventRetentionHold); n != 1 {
		t.Errorf("one apply wrote %d hold events, want exactly 1: this is a per-pass condition about a backup set, not a per-artifact one, and a pass over a thousand artifacts must not write a thousand warnings", n)
	}
}

// TestPruneApply_AnOrdinaryApplyLogsNoHold is the control, and without it
// the assertion above is satisfied by a build that warns on every apply.
func TestPruneApply_AnOrdinaryApplyLogsNoHold(t *testing.T) {
	ctx := context.Background()
	var stream bytes.Buffer
	svc, set, olderPath := heldRetentionFixture(t, &stream, false)

	plan, err := svc.PruneApply(ctx, set)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	deletes := 0
	for _, v := range plan.Verdicts {
		if v.Action == retention.PruneDelete {
			deletes++
		}
	}
	if deletes == 0 {
		t.Fatalf("precondition: this apply deleted nothing, so a silent log proves nothing about a working deployment: %+v", plan.Verdicts)
	}
	if _, err := os.Lstat(olderPath); !os.IsNotExist(err) {
		t.Fatalf("precondition: %s survived an apply that marked it DELETE (err=%v)", olderPath, err)
	}
	if got := stream.String(); strings.Contains(got, obs.EventRetentionHold) {
		t.Errorf("a healthy apply logged %s, which makes the event worthless as a signal:\n%s", obs.EventRetentionHold, got)
	}
}

// TestBuildHealthReport_AHeldRetentionPassIsAStandingCondition is the
// other half, and the more important one: the log line is a moment, and
// this condition lasts until somebody reconciles the set.
//
// `status` is where an operator asks "is anything wrong here". A backup
// set whose retention has been refusing to run for a fortnight is exactly
// the thing that answer should mention, and nothing in FR-24's report
// mentions it today: BuildHealthReport computes DecideKeep and
// PlanHomeMoves and never asks FR-30's question at all.
func TestBuildHealthReport_AHeldRetentionPassIsAStandingCondition(t *testing.T) {
	ctx := context.Background()
	svc, set, _ := heldRetentionFixture(t, nil, true)

	got := healthFor(t, ctx, svc, set)
	if got.RetentionHoldReason == "" {
		t.Fatalf("%s reports nothing about its retention while every delete in the set is being refused. State = %s (%s)", set, got.State, got.Reason)
	}
	if !strings.Contains(got.RetentionHoldReason, "newest.dump") {
		t.Errorf("RetentionHoldReason = %q, which does not name the artifact whose copy is missing; an operator cannot act on it", got.RetentionHoldReason)
	}
}

// TestBuildHealthReport_AWorkingSetReportsNoRetentionHold is that
// condition's control. A field that is always set is not a condition.
func TestBuildHealthReport_AWorkingSetReportsNoRetentionHold(t *testing.T) {
	ctx := context.Background()
	svc, set, _ := heldRetentionFixture(t, nil, false)

	got := healthFor(t, ctx, svc, set)
	if got.RetentionHoldReason != "" {
		t.Errorf("RetentionHoldReason = %q for a backup set whose last known good is exactly where it belongs", got.RetentionHoldReason)
	}
}

// TestBuildHealthReport_AnEmptyBackupSetReportsNoRetentionHold pins the
// corner every deployment passes through on its first day.
//
// FR-19 protects nothing in a set with no eligible artifact, so there is
// no copy to confirm and nothing to report. A condition that fired here
// would put a permanent warning on every set that has not backed anything
// up yet, which is the STALE verdict's job and not this one's.
func TestBuildHealthReport_AnEmptyBackupSetReportsNoRetentionHold(t *testing.T) {
	ctx := context.Background()

	dir := t.TempDir()
	bs := testBackupSet(t, dir)
	cfg := testConfig(t, testSource("production", bs))
	cfg.Retention = pruneDailyOnlyRetention()
	resolveTestRetention(cfg)
	svc := New(cfg, openJournal(t), newFakeTransport(), nil)
	svc.Now = fixedNow(retentionTestNow)

	got := healthFor(t, ctx, svc, bs.ID)
	if got.RetentionHoldReason != "" {
		t.Errorf("RetentionHoldReason = %q for a backup set that has never completed a backup", got.RetentionHoldReason)
	}
}
