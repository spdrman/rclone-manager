package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// retentionTestBackupSet is testBackupSet's core/service-level equivalent
// (core/internal/app/pipeline_test.go): just enough config.BackupSet for
// internal/retention's own checks (identity, local root).
func retentionTestBackupSet(t *testing.T, localDir string) config.BackupSet {
	t.Helper()
	id, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	return config.BackupSet{Name: "postgres-primary", ID: id, LocalPath: localDir}
}

// retentionTestConfig builds a *config.Config directly (rather than
// through this file's own testConfig, which hardcodes a full daily/weekly/
// monthly policy) so each test controls exactly which GFS tiers are live.
func retentionTestConfig(bs config.BackupSet, ret config.Retention) *config.Config {
	return &config.Config{
		Sources:   []config.Source{{Name: bs.ID.Source, BackupSets: []config.BackupSet{bs}}},
		Retention: ret,
	}
}

// retentionAllTiersDisabled mirrors internal/retention's own
// pruneAllTiersDisabled test helper: every GFS tier off and last-known-good
// protection off, so a managed-complete artifact's Keep flag depends on
// nothing this file did not put there itself. A backup set with exactly
// one such artifact is therefore a guaranteed PruneDelete candidate,
// without this file needing a second, differently-dated artifact just to
// prove a plan actually selects something for deletion.
func retentionAllTiersDisabled() config.Retention {
	off := false
	return config.Retention{Timezone: "UTC", WeekStartsOn: "monday", ProtectLastKnownGood: &off}
}

// seedCompleteArtifact writes a real local file for bs/name and records it
// straight through to lifecycle.Complete, in exactly two journal writes
// (DISCOVERED, then COMPLETE): internal/state.Journal itself does not
// validate FR-10 transition legality (see that package's own doc, "it
// doesn't know what a valid transition sequence looks like"), only that
// the current recorded state matches a transition's own From, so this
// shortcut is a legitimate use of the public Journal API, not a hack
// around it — internal/app's own tests instead drive the real pipeline
// step by step (pipeline_test.go's discoverOneRecord + processArtifact)
// because that package IS the pipeline; this package only needs a
// COMPLETE, FR-20-safe-to-delete artifact to already exist.
func seedCompleteArtifact(t *testing.T, ctx context.Context, journal *state.Journal, bs config.BackupSet, name string, discoveredAt time.Time, content string) model.ArtifactID {
	t.Helper()

	artifact, err := model.NewArtifactID(bs.ID, name)
	if err != nil {
		t.Fatalf("NewArtifactID(%q): %v", name, err)
	}

	localPath := filepath.Join(bs.LocalPath, name)
	if err := os.WriteFile(localPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", localPath, err)
	}

	if _, err := journal.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        "discover-" + name,
		From:       "",
		To:         string(lifecycle.Discovered),
		OccurredAt: discoveredAt,
		RemotePath: "/backups/" + name,
	}); err != nil {
		t.Fatalf("RecordTransition(discover %s): %v", name, err)
	}

	lp := localPath
	if _, err := journal.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        "complete-" + name,
		From:       string(lifecycle.Discovered),
		To:         string(lifecycle.Complete),
		OccurredAt: discoveredAt,
		LocalPath:  &lp,
		Transfer:   &state.TransferResult{BytesTransferred: int64(len(content)), Checksummed: true},
	}); err != nil {
		t.Fatalf("RecordTransition(complete %s): %v", name, err)
	}

	return artifact
}

// TestPreviewRetention_ReturnsSpecSchemaFields is the RED plan's second
// required test: docs/EPIC-B-multi-nas.md §15.6 requires a preview
// response to carry plan_id, inventory_revision, config_revision,
// expires_at, keep_count, delete_count and reclaim_bytes. This checks
// PreviewRetention's own RetentionPlan carries every one of them,
// correctly populated, not just present.
func TestPreviewRetention_ReturnsSpecSchemaFields(t *testing.T) {
	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()

	discoveredAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedCompleteArtifact(t, ctx, journal, bs, "backup.dump", discoveredAt, "twenty bytes!!!!!!!!")

	svc := New(retentionTestConfig(bs, retentionAllTiersDisabled()), journal, nil, nil)

	before := now()
	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	if !strings.HasPrefix(plan.PlanID, "retplan_") {
		t.Errorf("PlanID = %q, want a retplan_ prefix (docs/EPIC-B-multi-nas.md §15.6's own example)", plan.PlanID)
	}
	if plan.BackupSetID != bs.ID.String() {
		t.Errorf("BackupSetID = %q, want %q", plan.BackupSetID, bs.ID.String())
	}
	if plan.InventoryRevision == "" {
		t.Error("InventoryRevision is empty")
	}
	if plan.ConfigRevision != svc.ConfigRevision() {
		t.Errorf("ConfigRevision = %q, want %q", plan.ConfigRevision, svc.ConfigRevision())
	}
	if !plan.ExpiresAt.After(before) {
		t.Errorf("ExpiresAt = %v, want a time after %v", plan.ExpiresAt, before)
	}
	if plan.KeepCount != 0 {
		t.Errorf("KeepCount = %d, want 0 (every GFS tier and last-known-good are disabled)", plan.KeepCount)
	}
	if plan.DeleteCount != 1 {
		t.Errorf("DeleteCount = %d, want 1", plan.DeleteCount)
	}
	if plan.ReclaimBytes != 20 {
		t.Errorf("ReclaimBytes = %d, want 20 (len of the seeded artifact's content)", plan.ReclaimBytes)
	}
	if plan.OperationID != "" {
		t.Errorf("OperationID = %q, want empty: a preview creates no durable operation", plan.OperationID)
	}
}

// TestApplyRetentionPlan_StalePlanRejectedWithZeroDeletions is this
// issue's own primary acceptance test, reproduced at the core/service
// boundary almost verbatim from its Given/When/Then (docs/EPIC-B-multi-
// nas.md §71 WP 3.1, §15.6):
//
//	GIVEN plan P selects A for deletion
//	AND the backup set's inventory changes before apply
//	WHEN P is applied
//	THEN zero files are deleted
//	AND RETENTION_PLAN_STALE (ErrRetentionPlanStale) is returned
func TestApplyRetentionPlan_StalePlanRejectedWithZeroDeletions(t *testing.T) {
	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()

	discoveredAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedCompleteArtifact(t, ctx, journal, bs, "a.dump", discoveredAt, "payload-a")
	aPath := filepath.Join(bs.LocalPath, "a.dump")

	svc := New(retentionTestConfig(bs, retentionAllTiersDisabled()), journal, nil, nil)

	// GIVEN plan P selects A for deletion.
	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}
	if plan.DeleteCount != 1 {
		t.Fatalf("precondition failed: plan.DeleteCount = %d, want 1 (plan=%+v)", plan.DeleteCount, plan)
	}

	// AND the backup set's inventory changes before apply: a second
	// artifact is discovered and completed, which changes what
	// ListByBackupSet returns for this set and therefore its
	// inventory_revision, without touching A at all.
	seedCompleteArtifact(t, ctx, journal, bs, "b.dump", discoveredAt.Add(time.Hour), "payload-b")

	// WHEN P is applied.
	_, err = svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice"})

	// THEN ... RETENTION_PLAN_STALE (ErrRetentionPlanStale) is returned.
	if !errors.Is(err, ErrRetentionPlanStale) {
		t.Fatalf("ApplyRetentionPlan error = %v, want errors.Is(err, ErrRetentionPlanStale)", err)
	}

	// AND zero files are deleted: A must still be exactly where it was,
	// and internal/retention.PruneApply's own os.Remove must never have
	// been reached for it (or anything else).
	if _, statErr := os.Lstat(aPath); statErr != nil {
		t.Errorf("A was deleted (or moved) despite the stale plan being rejected: Lstat(%s): %v", aPath, statErr)
	}

	// The rejected plan is consumed: re-submitting the same plan_id must
	// not be treated as "still pending", see ApplyRetentionPlan's own
	// "Single-use" doc.
	if _, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice"}); !errors.Is(err, ErrRetentionPlanNotFound) {
		t.Errorf("re-applying the same (already-resolved) plan_id: error = %v, want errors.Is(err, ErrRetentionPlanNotFound)", err)
	}
}

// TestApplyRetentionPlan_ValidPlanAppliesExactly is the positive control
// for the stale-rejection test above: with nothing mutated between preview
// and apply, the exact plan previewed is the one that runs, recorded as a
// durable operation.
func TestApplyRetentionPlan_ValidPlanAppliesExactly(t *testing.T) {
	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()

	discoveredAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedCompleteArtifact(t, ctx, journal, bs, "a.dump", discoveredAt, "payload-a")
	aPath := filepath.Join(bs.LocalPath, "a.dump")

	svc := New(retentionTestConfig(bs, retentionAllTiersDisabled()), journal, nil, nil)

	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	result, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice"})
	if err != nil {
		t.Fatalf("ApplyRetentionPlan: %v", err)
	}

	if result.PlanID != plan.PlanID {
		t.Errorf("result.PlanID = %q, want %q", result.PlanID, plan.PlanID)
	}
	if result.DeleteCount != 1 || result.ReclaimBytes != int64(len("payload-a")) {
		t.Errorf("result = %+v, want DeleteCount=1, ReclaimBytes=%d", result, len("payload-a"))
	}
	if _, statErr := os.Lstat(aPath); !os.IsNotExist(statErr) {
		t.Errorf("Lstat(%s) after apply: err=%v, want a not-exist error (the file must actually be gone)", aPath, statErr)
	}

	if result.OperationID == "" {
		t.Fatal("result.OperationID is empty, want a durable operation id")
	}
	op, err := svc.GetOperation(ctx, result.OperationID)
	if err != nil {
		t.Fatalf("GetOperation(%s): %v", result.OperationID, err)
	}
	if op.Status != "completed" {
		t.Errorf("operation status = %q, want %q (Error=%q)", op.Status, "completed", op.Error)
	}
	if op.Action != ActionRetentionApply {
		t.Errorf("operation Action = %q, want %q", op.Action, ActionRetentionApply)
	}

	// Single-use: re-applying the same plan_id after a successful apply
	// must not re-run anything.
	if _, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice"}); !errors.Is(err, ErrRetentionPlanNotFound) {
		t.Errorf("re-applying an already-applied plan_id: error = %v, want errors.Is(err, ErrRetentionPlanNotFound)", err)
	}
}

// TestApplyRetentionPlan_UnknownPlanIDReturnsNotFound is ApplyRetentionPlan's
// negative/refusal case for a plan_id nobody ever issued: distinct from
// ErrRetentionPlanStale (which implies a plan that WAS once valid).
func TestApplyRetentionPlan_UnknownPlanIDReturnsNotFound(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.ApplyRetentionPlan(context.Background(), ApplyRetentionRequest{PlanID: "retplan_does-not-exist", Source: "production", Set: "postgres-primary", Actor: "alice"})
	if !errors.Is(err, ErrRetentionPlanNotFound) {
		t.Fatalf("ApplyRetentionPlan error = %v, want errors.Is(err, ErrRetentionPlanNotFound)", err)
	}
}

// TestApplyRetentionPlan_MissingPlanIDIsInvalidRequest is the request-
// validation refusal (an empty plan_id is malformed input, not "this
// plan_id is unknown" or "this plan_id is stale").
func TestApplyRetentionPlan_MissingPlanIDIsInvalidRequest(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.ApplyRetentionPlan(context.Background(), ApplyRetentionRequest{Source: "production", Set: "postgres-primary", Actor: "alice"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ApplyRetentionPlan error = %v, want errors.Is(err, ErrInvalidRequest)", err)
	}
}

// TestApplyRetentionPlan_ExpiredPlanIsStaleWithZeroDeletions proves
// expires_at (docs/EPIC-B-multi-nas.md §15.6, §29.3 step 6) is enforced
// even when nothing about the backup set's inventory or configuration
// changed at all: a plan that simply sat unconfirmed too long is refused
// the same way a genuinely stale one is.
func TestApplyRetentionPlan_ExpiredPlanIsStaleWithZeroDeletions(t *testing.T) {
	old := retentionPlanTTL
	retentionPlanTTL = time.Millisecond
	t.Cleanup(func() { retentionPlanTTL = old })

	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()

	discoveredAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedCompleteArtifact(t, ctx, journal, bs, "a.dump", discoveredAt, "payload-a")
	aPath := filepath.Join(bs.LocalPath, "a.dump")

	svc := New(retentionTestConfig(bs, retentionAllTiersDisabled()), journal, nil, nil)

	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	time.Sleep(5 * time.Millisecond)

	_, err = svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice"})
	if !errors.Is(err, ErrRetentionPlanStale) {
		t.Fatalf("ApplyRetentionPlan error = %v, want errors.Is(err, ErrRetentionPlanStale)", err)
	}
	if _, statErr := os.Lstat(aPath); statErr != nil {
		t.Errorf("A was deleted despite the plan being expired: Lstat(%s): %v", aPath, statErr)
	}
}

// TestPreviewRetention_UnknownBackupSetReturnsErrBackupSetNotFound proves
// PreviewRetention refuses a source/set naming no configured backup set,
// rather than computing a plan against zero records (which would look
// indistinguishable from "this backup set genuinely has nothing").
func TestPreviewRetention_UnknownBackupSetReturnsErrBackupSetNotFound(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.PreviewRetention(context.Background(), "production", "does-not-exist")
	if !errors.Is(err, ErrBackupSetNotFound) {
		t.Fatalf("PreviewRetention error = %v, want errors.Is(err, ErrBackupSetNotFound)", err)
	}
}

// retentionOn is retentionAllTiersDisabled's opposite: every GFS tier and
// last-known-good protection live, for the one test below that actually
// needs them.
func retentionOn(dailyDays, weeklyMonths, monthlyMonths int) config.Retention {
	on := true
	return config.Retention{
		Timezone: "UTC", WeekStartsOn: "monday",
		DailyDays: dailyDays, WeeklyMonths: weeklyMonths, MonthlyMonths: monthlyMonths,
		ProtectLastKnownGood: &on,
	}
}

// hasTier reports whether tiers contains want, tolerating internal/
// retention.GFSTier's own string type without this file importing that
// package just to spell the comparison.
func hasTier(tiers []string, want string) bool {
	for _, t := range tiers {
		if t == want {
			return true
		}
	}
	return false
}

// TestPreviewThenApply_MixedGFSTiersAndLastKnownGood is this issue's own
// INTEGRATION checklist item: a boundary test through the real internal/
// retention decision path (not a mock), against a backup set with daily/
// weekly/monthly and last-known-good artifacts mixed together — every
// other test in this file either disables every tier
// (retentionAllTiersDisabled) or exercises exactly one artifact, neither of
// which proves the daily/weekly/monthly/last-known-good union (FR-18's own
// formula) actually composes correctly all the way from PreviewRetention
// through ApplyRetentionPlan.
//
// GFSDecide's own window arithmetic (gfs.go) anchors every tier's start at
// the first of a calendar month, N-1 months back — see that file's
// DailyDays/WeeklyMonths/MonthlyMonths handling — so this test places
// artifacts relative to the real wall clock (internal/app.Service.now,
// which this boundary does not let a caller override) rather than fixed
// calendar dates, choosing offsets with a full calendar month of slack on
// either side of each window edge so the result cannot depend on which day
// of the month the suite happens to run on.
func TestPreviewThenApply_MixedGFSTiersAndLastKnownGood(t *testing.T) {
	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()

	base := time.Now().UTC()
	monthStart := time.Date(base.Year(), base.Month(), 1, 0, 0, 0, 0, time.UTC)

	// DailyDays=3, WeeklyMonths=2, MonthlyMonths=3: daily's window is the
	// last 3 calendar days; weekly's window starts at the first of LAST
	// month; monthly's window starts at the first of the month two months
	// back. Every artifact below sits well inside or outside the window it
	// is meant to test, never near a boundary.
	svc := New(retentionTestConfig(bs, retentionOn(3, 2, 3)), journal, nil, nil)

	dailyToday := seedCompleteArtifact(t, ctx, journal, bs, "daily-today.dump", base, "daily-today")
	seedCompleteArtifact(t, ctx, journal, bs, "daily-yesterday.dump", base.AddDate(0, 0, -1), "daily-yesterday")
	// Well inside last month (weekly's window), well outside the daily window.
	seedCompleteArtifact(t, ctx, journal, bs, "weekly.dump", monthStart.AddDate(0, -1, 14), "weekly-artifact")
	// Well inside the month two months back (monthly's window only), well
	// outside weekly's one-month reach.
	seedCompleteArtifact(t, ctx, journal, bs, "monthly.dump", monthStart.AddDate(0, -2, 14), "monthly-artifact")
	// Four months back: outside every tier's window and not the newest
	// artifact, so last-known-good does not save it either — the one
	// artifact this plan actually deletes.
	deletePath := filepath.Join(bs.LocalPath, "too-old.dump")
	seedCompleteArtifact(t, ctx, journal, bs, "too-old.dump", monthStart.AddDate(0, -4, 14), "too-old-payload")

	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	if plan.KeepCount != 4 {
		t.Errorf("KeepCount = %d, want 4 (plan=%+v)", plan.KeepCount, plan)
	}
	if plan.DeleteCount != 1 {
		t.Errorf("DeleteCount = %d, want 1 (plan=%+v)", plan.DeleteCount, plan)
	}
	if plan.ReclaimBytes != int64(len("too-old-payload")) {
		t.Errorf("ReclaimBytes = %d, want %d", plan.ReclaimBytes, len("too-old-payload"))
	}

	var sawDaily, sawWeekly, sawMonthly, sawLastKnownGood bool
	verdictByArtifact := make(map[string]string, len(plan.Verdicts))
	for _, v := range plan.Verdicts {
		verdictByArtifact[v.Artifact] = v.Action
		if hasTier(v.Tiers, "DAILY") {
			sawDaily = true
		}
		if hasTier(v.Tiers, "WEEKLY") {
			sawWeekly = true
		}
		if hasTier(v.Tiers, "MONTHLY") {
			sawMonthly = true
		}
		if hasTier(v.Tiers, "LAST_KNOWN_GOOD") {
			sawLastKnownGood = true
			// The newest eligible artifact is the one last-known-good
			// protection actually names (lastknowngood.go's own doc).
			if v.Artifact != dailyToday.Name {
				t.Errorf("LAST_KNOWN_GOOD protected %q, want the newest artifact %q", v.Artifact, dailyToday.Name)
			}
		}
	}
	if !sawDaily || !sawWeekly || !sawMonthly || !sawLastKnownGood {
		t.Errorf("plan did not mix every tier: daily=%v weekly=%v monthly=%v lastKnownGood=%v (verdicts=%+v)",
			sawDaily, sawWeekly, sawMonthly, sawLastKnownGood, plan.Verdicts)
	}
	if verdictByArtifact["too-old.dump"] != "DELETE" {
		t.Errorf("too-old.dump verdict = %q, want DELETE", verdictByArtifact["too-old.dump"])
	}
	for _, name := range []string{"daily-today.dump", "daily-yesterday.dump", "weekly.dump", "monthly.dump"} {
		if verdictByArtifact[name] != "KEEP" {
			t.Errorf("%s verdict = %q, want KEEP", name, verdictByArtifact[name])
		}
	}

	// WHEN this exact plan is applied (nothing has changed since preview).
	applied, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice"})
	if err != nil {
		t.Fatalf("ApplyRetentionPlan: %v", err)
	}
	if applied.DeleteCount != 1 || applied.KeepCount != 4 {
		t.Errorf("applied = %+v, want DeleteCount=1, KeepCount=4", applied)
	}

	// THEN the one DELETE verdict's file is actually gone, and every KEEP
	// verdict's file is still exactly where it was — the real
	// internal/retention.PruneApply path, not a mock.
	if _, statErr := os.Lstat(deletePath); !os.IsNotExist(statErr) {
		t.Errorf("Lstat(%s) after apply: err=%v, want a not-exist error", deletePath, statErr)
	}
	for _, name := range []string{"daily-today.dump", "daily-yesterday.dump", "weekly.dump", "monthly.dump"} {
		if _, statErr := os.Lstat(filepath.Join(bs.LocalPath, name)); statErr != nil {
			t.Errorf("Lstat(%s) after apply: %v, want the kept artifact still present", name, statErr)
		}
	}
}

// pinClock replaces this package's own clock (service.go's `now`) with one
// this test moves by hand, and restores it afterward. Everything a
// retention plan's freshness depends on reads through it: the preview's
// own decision instant, expires_at, and the instant ApplyRetentionPlan
// re-derives the verdicts at.
func pinClock(t *testing.T, at time.Time) func(time.Time) {
	t.Helper()
	old := now
	current := at
	var mu sync.Mutex
	now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	t.Cleanup(func() { now = old })
	return func(to time.Time) {
		mu.Lock()
		defer mu.Unlock()
		current = to
	}
}

// retentionDailyOnly is a one-day GFS daily tier and nothing else: an
// artifact is kept only while the civil day it was discovered on is still
// "today", which is what makes a civil-day boundary flip its verdict from
// KEEP to DELETE with no other input changing at all.
func retentionDailyOnly() config.Retention {
	off := false
	return config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 1, ProtectLastKnownGood: &off}
}

// TestApplyRetentionPlan_CivilDayBoundaryBetweenPreviewAndApplyIsStale is
// the sharpest facet of this issue's own review (mandatory finding M1),
// and it needs no concurrency: a retention verdict is a function of the
// clock as well as of the journal and the configuration, so a plan
// previewed at 23:58 and applied at 00:01 has an identical inventory
// revision and an identical config revision while describing a genuinely
// different deletion set.
//
// GIVEN plan P shows artifact A as KEEP
// AND the civil day rolls over before apply
// WHEN P is applied
// THEN zero files are deleted AND ErrRetentionPlanStale is returned.
func TestApplyRetentionPlan_CivilDayBoundaryBetweenPreviewAndApplyIsStale(t *testing.T) {
	setClock := pinClock(t, time.Date(2026, 8, 1, 23, 58, 0, 0, time.UTC))

	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()

	seedCompleteArtifact(t, ctx, journal, bs, "a.dump", time.Date(2026, 8, 1, 20, 0, 0, 0, time.UTC), "payload-a")
	aPath := filepath.Join(bs.LocalPath, "a.dump")

	svc := New(retentionTestConfig(bs, retentionDailyOnly()), journal, nil, nil)

	// GIVEN the operator reviews a plan that deletes nothing: A is today's
	// daily backup, held by the daily tier.
	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}
	if plan.DeleteCount != 0 || plan.KeepCount != 1 {
		t.Fatalf("precondition failed: plan = %+v, want KeepCount=1, DeleteCount=0 (the operator must be reviewing a KEEP)", plan)
	}

	// AND the civil day rolls over while the confirmation is on screen:
	// nothing about the journal or the configuration moves, only the date
	// the GFS daily span is anchored on.
	setClock(time.Date(2026, 8, 2, 0, 1, 0, 0, time.UTC))

	// WHEN P is applied.
	_, err = svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice"})

	// THEN it is refused, and A, which the operator saw as KEEP, is still
	// exactly where it was.
	if !errors.Is(err, ErrRetentionPlanStale) {
		t.Fatalf("ApplyRetentionPlan error = %v, want errors.Is(err, ErrRetentionPlanStale)", err)
	}
	if _, statErr := os.Lstat(aPath); statErr != nil {
		t.Fatalf("A was deleted across a civil-day boundary despite being reviewed as KEEP: Lstat(%s): %v", aPath, statErr)
	}

	// Positive control for the refusal above: at this later instant the
	// verdict really has flipped to DELETE and the deletion path really
	// does run, so the surviving file proves the reviewed-plan comparison
	// refused it, not that nothing would have happened anyway.
	fresh, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention (control): %v", err)
	}
	if fresh.DeleteCount != 1 {
		t.Fatalf("control failed: a plan previewed after midnight has DeleteCount = %d, want 1 (the verdict must genuinely have flipped)", fresh.DeleteCount)
	}
	if _, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: fresh.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice"}); err != nil {
		t.Fatalf("ApplyRetentionPlan (control): %v", err)
	}
	if _, statErr := os.Lstat(aPath); !os.IsNotExist(statErr) {
		t.Errorf("control failed: Lstat(%s) after applying the freshly reviewed plan: err=%v, want a not-exist error", aPath, statErr)
	}
}

// TestApplyRetentionPlan_RefusedWhileACycleIsRunningAndConsumesNothing is
// the other half of mandatory finding M1: a cycle writes the very journal
// rows the staleness comparison is computed over, and ApplyRetentionPlan
// used to take no lock against one at all. runOnce is held here exactly as
// scheduler_test.go holds it, which is what an in-flight RunCycle (a
// scheduled tick or a submitted operation) does for its whole duration.
func TestApplyRetentionPlan_RefusedWhileACycleIsRunningAndConsumesNothing(t *testing.T) {
	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()

	seedCompleteArtifact(t, ctx, journal, bs, "a.dump", time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), "payload-a")
	aPath := filepath.Join(bs.LocalPath, "a.dump")

	svc := New(retentionTestConfig(bs, retentionAllTiersDisabled()), journal, nil, nil)

	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	if !svc.runOnce.TryLock() {
		t.Fatal("runOnce.TryLock() failed on a fresh BackupService")
	}

	_, err = svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice"})
	if !errors.Is(err, ErrRetentionApplyBusy) {
		svc.runOnce.Unlock()
		t.Fatalf("ApplyRetentionPlan during a cycle: error = %v, want errors.Is(err, ErrRetentionApplyBusy)", err)
	}
	if _, statErr := os.Lstat(aPath); statErr != nil {
		svc.runOnce.Unlock()
		t.Fatalf("A was deleted by an apply that ran concurrently with a cycle: Lstat(%s): %v", aPath, statErr)
	}

	svc.runOnce.Unlock()

	// Positive control, and the reason busy is its own sentinel: the plan
	// was not consumed by the refusal, so the identical request succeeds
	// the moment the cycle finishes. A refusal that had burnt the plan
	// would fail here with ErrRetentionPlanNotFound.
	applied, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice"})
	if err != nil {
		t.Fatalf("ApplyRetentionPlan after the cycle finished: %v", err)
	}
	if applied.DeleteCount != 1 {
		t.Errorf("applied = %+v, want DeleteCount=1", applied)
	}
	if _, statErr := os.Lstat(aPath); !os.IsNotExist(statErr) {
		t.Errorf("Lstat(%s) after the apply that did run: err=%v, want a not-exist error", aPath, statErr)
	}
}

// TestClaimRetentionPlan_ConcurrentClaimsOfOnePlanIDOnlyOneWins is
// mandatory finding M4 at the exact line it lives on: the lookup and the
// removal happen in one critical section, so of N concurrent callers
// naming one plan_id exactly one is handed the record and can go on to
// delete anything. This drives the claim directly rather than through
// ApplyRetentionPlan, because the runOnce serialisation added for M1 would
// otherwise mask which guard is doing the work.
func TestClaimRetentionPlan_ConcurrentClaimsOfOnePlanIDOnlyOneWins(t *testing.T) {
	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()

	seedCompleteArtifact(t, ctx, journal, bs, "a.dump", time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), "payload-a")
	svc := New(retentionTestConfig(bs, retentionAllTiersDisabled()), journal, nil, nil)

	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	const claimers = 16
	var wg sync.WaitGroup
	results := make([]error, claimers)
	start := make(chan struct{})
	for i := range claimers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.claimRetentionPlan(plan.PlanID, bs.ID)
			results[i] = err
		}()
	}
	close(start)
	wg.Wait()

	won := 0
	for i, err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrRetentionPlanNotFound):
		default:
			t.Errorf("claimer %d: error = %v, want nil or ErrRetentionPlanNotFound", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d of %d concurrent claimers were handed the plan, want exactly 1", won, claimers)
	}
}

// TestApplyRetentionPlan_ConcurrentAppliesDeleteExactlyOnce is the same
// guarantee at the public boundary: whichever refusal each loser gets
// (busy, because one applier holds runOnce, or not-found, because one
// applier claimed the plan), exactly one apply may report success and the
// artifact may only be deleted once.
func TestApplyRetentionPlan_ConcurrentAppliesDeleteExactlyOnce(t *testing.T) {
	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()

	seedCompleteArtifact(t, ctx, journal, bs, "a.dump", time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), "payload-a")
	aPath := filepath.Join(bs.LocalPath, "a.dump")
	svc := New(retentionTestConfig(bs, retentionAllTiersDisabled()), journal, nil, nil)

	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	const appliers = 8
	var wg sync.WaitGroup
	deleted := make([]int, appliers)
	errs := make([]error, appliers)
	start := make(chan struct{})
	for i := range appliers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice"})
			errs[i], deleted[i] = err, result.DeleteCount
		}()
	}
	close(start)
	wg.Wait()

	succeeded, totalDeleted := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
			totalDeleted += deleted[i]
		case errors.Is(err, ErrRetentionPlanNotFound), errors.Is(err, ErrRetentionApplyBusy):
		default:
			t.Errorf("applier %d: error = %v, want nil, ErrRetentionPlanNotFound or ErrRetentionApplyBusy", i, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d of %d concurrent applies succeeded, want exactly 1", succeeded, appliers)
	}
	if totalDeleted != 1 {
		t.Errorf("successful applies reported %d deletions in total, want 1", totalDeleted)
	}
	if _, statErr := os.Lstat(aPath); !os.IsNotExist(statErr) {
		t.Errorf("Lstat(%s) after the one successful apply: err=%v, want a not-exist error", aPath, statErr)
	}
}

// TestApplyRetentionPlan_PlanIDSubmittedForAnotherBackupSetIsRefused is
// mandatory finding M5 (and the server-side half of M3): the {source}/
// {set} the request names is cross-checked against the backup set the plan
// was actually issued for, so a client bug or stale component state
// submitting the right-looking plan id for the wrong set is refused rather
// than silently deleting from a backup set the operator was not looking
// at.
func TestApplyRetentionPlan_PlanIDSubmittedForAnotherBackupSetIsRefused(t *testing.T) {
	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()

	seedCompleteArtifact(t, ctx, journal, bs, "a.dump", time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), "payload-a")
	aPath := filepath.Join(bs.LocalPath, "a.dump")
	svc := New(retentionTestConfig(bs, retentionAllTiersDisabled()), journal, nil, nil)

	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	_, err = svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Source: bs.ID.Source, Set: "some-other-set", Actor: "alice"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ApplyRetentionPlan with a mismatched set: error = %v, want errors.Is(err, ErrInvalidRequest)", err)
	}
	if _, statErr := os.Lstat(aPath); statErr != nil {
		t.Errorf("A was deleted by an apply routed at a different backup set: Lstat(%s): %v", aPath, statErr)
	}

	// Positive control: the refusal is the cross-check firing, not the
	// plan being unusable, and it consumed nothing — the same plan applied
	// at its own backup set still works.
	if _, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice"}); err != nil {
		t.Fatalf("ApplyRetentionPlan at the plan's own backup set: %v", err)
	}
	if _, statErr := os.Lstat(aPath); !os.IsNotExist(statErr) {
		t.Errorf("Lstat(%s) after the correctly routed apply: err=%v, want a not-exist error", aPath, statErr)
	}
}

// TestApplyRetentionPlan_SuccessInvalidatesThisSetsOtherPlans is mandatory
// finding M6's stopgap. A successful apply writes nothing to the journal
// about the files it deleted, so an older plan's inventory fingerprint
// still matches afterward and it would stay applyable against a backup set
// it no longer describes. Single-use has to be per effect, not only per
// plan_id.
func TestApplyRetentionPlan_SuccessInvalidatesThisSetsOtherPlans(t *testing.T) {
	first := retentionTestBackupSet(t, t.TempDir())
	second, err := model.NewBackupSetID("production", "billing")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	otherSet := config.BackupSet{Name: "billing", ID: second, LocalPath: t.TempDir()}

	journal := openTestJournal(t)
	ctx := context.Background()
	discoveredAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedCompleteArtifact(t, ctx, journal, first, "a.dump", discoveredAt, "payload-a")
	seedCompleteArtifact(t, ctx, journal, otherSet, "b.dump", discoveredAt, "payload-b")

	cfg := &config.Config{
		Sources:   []config.Source{{Name: "production", BackupSets: []config.BackupSet{first, otherSet}}},
		Retention: retentionAllTiersDisabled(),
	}
	svc := New(cfg, journal, nil, nil)

	superseded, err := svc.PreviewRetention(ctx, first.ID.Source, first.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention (superseded): %v", err)
	}
	untouched, err := svc.PreviewRetention(ctx, second.Source, second.Set)
	if err != nil {
		t.Fatalf("PreviewRetention (other set): %v", err)
	}
	confirmed, err := svc.PreviewRetention(ctx, first.ID.Source, first.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention (confirmed): %v", err)
	}

	if _, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: confirmed.PlanID, Source: first.ID.Source, Set: first.ID.Set, Actor: "alice"}); err != nil {
		t.Fatalf("ApplyRetentionPlan: %v", err)
	}

	if _, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: superseded.PlanID, Source: first.ID.Source, Set: first.ID.Set, Actor: "alice"}); !errors.Is(err, ErrRetentionPlanNotFound) {
		t.Errorf("applying a plan superseded by a successful apply: error = %v, want errors.Is(err, ErrRetentionPlanNotFound)", err)
	}

	// Positive control: the invalidation is scoped to the backup set that
	// actually changed. Another set's outstanding plan is untouched, which
	// also proves the assertion above is not just "every plan is gone".
	if _, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: untouched.PlanID, Source: second.Source, Set: second.Set, Actor: "alice"}); err != nil {
		t.Errorf("applying another backup set's outstanding plan: error = %v, want nil (it must not have been invalidated)", err)
	}
}

// TestPreviewRetention_SweepsExpiredPlansAndCapsTheStore is mandatory
// finding M8: previews that are never applied are the normal case, and
// only an apply ever removed a record, so the store grew for the life of
// the process inside the daemon that also runs the backups.
func TestPreviewRetention_SweepsExpiredPlansAndCapsTheStore(t *testing.T) {
	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()
	seedCompleteArtifact(t, ctx, journal, bs, "a.dump", time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), "payload-a")
	svc := New(retentionTestConfig(bs, retentionAllTiersDisabled()), journal, nil, nil)

	setClock := pinClock(t, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC))

	if _, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set); err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}
	svc.retentionMu.Lock()
	held := len(svc.retentionPlans)
	svc.retentionMu.Unlock()
	if held != 1 {
		t.Fatalf("after one preview the store holds %d plans, want 1", held)
	}

	// Past that plan's expiry, the next preview sweeps it: an expired plan
	// is unapplyable, so keeping it is pure growth.
	setClock(time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC).Add(retentionPlanTTL).Add(time.Minute))
	if _, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set); err != nil {
		t.Fatalf("PreviewRetention (after expiry): %v", err)
	}
	svc.retentionMu.Lock()
	held = len(svc.retentionPlans)
	svc.retentionMu.Unlock()
	if held != 1 {
		t.Fatalf("after the expired plan should have been swept the store holds %d plans, want 1", held)
	}

	// And the cap holds even when nothing has expired at all: every plan
	// below is live, and the store still never exceeds its ceiling.
	for i := range maxRetentionPlans + 8 {
		if _, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set); err != nil {
			t.Fatalf("PreviewRetention (#%d): %v", i, err)
		}
	}
	svc.retentionMu.Lock()
	held = len(svc.retentionPlans)
	svc.retentionMu.Unlock()
	if held > maxRetentionPlans {
		t.Errorf("the store holds %d plans after %d live previews, want at most %d", held, maxRetentionPlans+8, maxRetentionPlans)
	}
	if held == 0 {
		t.Error("the store holds no plans at all: eviction must make room, not empty the store")
	}
}

// writeRetentionConfigFile is writeTestConfigFile (open_test.go) with an
// explicit retention policy that makes a completed artifact a genuine
// delete candidate, so a file-backed service (the only kind that can
// hot-reload its configuration) has something real at stake in the test
// below.
func writeRetentionConfigFile(t *testing.T) (configPath, localDir string) {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	localDir = filepath.Join(dir, "local")
	for _, d := range []string{remoteDir, localDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d, err)
		}
	}

	configPath = filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + localDir + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n" +
		"  protect_last_known_good: false\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath, localDir
}

// TestApplyRetentionPlan_ConfigurationChangedBetweenPreviewAndApplyIsStale
// is mandatory finding M10: the staleness gate has two arms, and until now
// only the inventory one had ever been observed returning true. The
// configuration arm guards the case where the applied deletion set can
// diverge from the reviewed one most sharply (a tier disabled between
// preview and apply turns KEEP verdicts into DELETE verdicts), so it is
// driven here through the real hot-reload path — CreateBackupSet writing
// the config file and swapping this service's configState — rather than by
// reaching into b.state.
func TestApplyRetentionPlan_ConfigurationChangedBetweenPreviewAndApplyIsStale(t *testing.T) {
	// A file-backed configuration cannot disable the GFS tiers (0 means
	// "use the default" — config/validate.go), so the artifact below is
	// dated far outside every tier's span instead, which is what makes it
	// a genuine delete candidate under the real, defaulted policy.
	pinClock(t, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC))

	configPath, localDir := writeRetentionConfigFile(t)
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	ctx := context.Background()
	setID, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	bs := config.BackupSet{Name: "postgres-primary", ID: setID, LocalPath: localDir}
	seedCompleteArtifact(t, ctx, svc.journal, bs, "a.dump", time.Date(2020, 1, 15, 12, 0, 0, 0, time.UTC), "payload-a")
	aPath := filepath.Join(localDir, "a.dump")

	plan, err := svc.PreviewRetention(ctx, setID.Source, setID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}
	if plan.DeleteCount != 1 {
		t.Fatalf("precondition failed: plan.DeleteCount = %d, want 1 (verdicts=%+v)", plan.DeleteCount, plan.Verdicts)
	}

	// The configuration changes between preview and apply, through the one
	// path that actually recomputes this service's config revision.
	revisionBefore := svc.ConfigRevision()
	if _, err := svc.CreateBackupSet(ctx, validCreateReq(t, svc, "added-mid-review")); err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}
	if svc.ConfigRevision() == revisionBefore {
		t.Fatal("precondition failed: ConfigRevision did not move, so this test would prove nothing")
	}

	_, err = svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Source: setID.Source, Set: setID.Set, Actor: "alice"})
	if !errors.Is(err, ErrRetentionPlanStale) {
		t.Fatalf("ApplyRetentionPlan after a configuration change: error = %v, want errors.Is(err, ErrRetentionPlanStale)", err)
	}
	if _, statErr := os.Lstat(aPath); statErr != nil {
		t.Fatalf("A was deleted despite the configuration having changed since the plan was reviewed: Lstat(%s): %v", aPath, statErr)
	}

	// Positive control: re-previewing under the new configuration and
	// applying that plan does delete the file, so the refusal above was
	// the configuration arm firing and not an inert deletion path.
	fresh, err := svc.PreviewRetention(ctx, setID.Source, setID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention (control): %v", err)
	}
	if _, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: fresh.PlanID, Source: setID.Source, Set: setID.Set, Actor: "alice"}); err != nil {
		t.Fatalf("ApplyRetentionPlan (control): %v", err)
	}
	if _, statErr := os.Lstat(aPath); !os.IsNotExist(statErr) {
		t.Errorf("control failed: Lstat(%s) after applying the re-previewed plan: err=%v, want a not-exist error", aPath, statErr)
	}
}

// retentionChain is retentionOn's generalized counterpart (issue #156,
// B3.8): an explicit FR-18 tier chain of any length, with last-known-good
// protection live. The three legacy scalars stay zero, which
// config.ValidateRetention requires alongside an explicit chain.
func retentionChain(tiers ...config.RetentionTier) config.Retention {
	on := true
	return config.Retention{
		Timezone: "UTC", WeekStartsOn: "monday",
		Tiers:                tiers,
		ProtectLastKnownGood: &on,
	}
}

// TestPreviewThenApply_NonContiguousChainWithSemiAnnualAndAnnual is issue
// #156's INTEGRATION checklist item: a real multi-tier chain reaching past
// monthly, driven all the way through PreviewRetention and
// ApplyRetentionPlan against the genuine internal/retention decision path,
// confirming the webhost API's verdicts[].tiers names every tier that
// selected each artifact.
//
// The chain is deliberately non-contiguous (daily, then semi-annual, then
// annual, with nothing covering the months between them), because a
// contiguous chain would keep every artifact that has a neighbour and
// prove nothing about Rom's "everything in-between and outside that policy
// would be deleted". The two artifacts sharing one year bucket are the
// gap: the older one is a delete candidate that no tier reaches, and the
// sub-test below is its positive control, showing the identical fixture
// keeps it once a monthly tier is chained in.
//
// Like the mixed-tier test above, this places artifacts relative to the
// real wall clock (internal/app.Service.now, which this boundary does not
// let a caller override) rather than to fixed calendar dates, anchoring on
// the calendar year and half-year starts the tiers themselves bucket by
// and leaving whole months of slack around every window edge.
func TestPreviewThenApply_NonContiguousChainWithSemiAnnualAndAnnual(t *testing.T) {
	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()

	base := time.Now().UTC()
	yearStart := time.Date(base.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	halfStart := yearStart
	if base.Month() >= time.July {
		halfStart = time.Date(base.Year(), 7, 1, 0, 0, 0, 0, time.UTC)
	}

	// daily reaches back 3 days; semi_annual reaches back 4 calendar
	// half-years (18 months before the current half's start); annual
	// reaches back 10 calendar years.
	daily := config.RetentionTier{Name: "daily", Granularity: config.GranularityDay, Keep: 3}
	semiAnnual := config.RetentionTier{Name: "semi_annual", Granularity: config.GranularityHalfYear, Keep: 4}
	annual := config.RetentionTier{Name: "annual", Granularity: config.GranularityYear, Keep: 10}

	newest := seedCompleteArtifact(t, ctx, journal, bs, "newest.dump", base, "newest")
	// 45 days before the current half-year began: firmly inside the
	// previous half-year (six months long), firmly inside semi_annual's
	// 18-month reach, and far outside daily's three days.
	seedCompleteArtifact(t, ctx, journal, bs, "prev-half.dump", halfStart.AddDate(0, 0, -45), "prev-half")
	// Two artifacts in the calendar year three years back, days 100 and
	// 300 of it, so both are unambiguously inside that year and inside the
	// annual window while being far outside semi_annual's. Only the newer
	// one wins the year's bucket.
	gapYearStart := yearStart.AddDate(-3, 0, 0)
	seedCompleteArtifact(t, ctx, journal, bs, "gap-loser.dump", gapYearStart.AddDate(0, 0, 100), "gap-loser")
	seedCompleteArtifact(t, ctx, journal, bs, "gap-winner.dump", gapYearStart.AddDate(0, 0, 300), "gap-winner")
	// Twelve years back: past the end of the longest tier in the chain.
	seedCompleteArtifact(t, ctx, journal, bs, "ancient.dump", yearStart.AddDate(-12, 0, 0), "ancient")

	svc := New(retentionTestConfig(bs, retentionChain(daily, semiAnnual, annual)), journal, nil, nil)

	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	tiersByArtifact := map[string][]string{}
	actionByArtifact := map[string]string{}
	for _, v := range plan.Verdicts {
		tiersByArtifact[v.Artifact] = v.Tiers
		actionByArtifact[v.Artifact] = v.Action
	}

	// The newest artifact wins its day, its half-year and its year, and
	// carries FR-19's protection on top: a four-way union reported through
	// the same []string the webhost handler sends as verdicts[].tiers.
	for _, want := range []string{"DAILY", "SEMI_ANNUAL", "ANNUAL", "LAST_KNOWN_GOOD"} {
		if !hasTier(tiersByArtifact[newest.Name], want) {
			t.Errorf("%s tiers = %v, want it to include %s", newest.Name, tiersByArtifact[newest.Name], want)
		}
	}
	if !hasTier(tiersByArtifact["prev-half.dump"], "SEMI_ANNUAL") {
		t.Errorf("prev-half.dump tiers = %v, want SEMI_ANNUAL", tiersByArtifact["prev-half.dump"])
	}
	if hasTier(tiersByArtifact["prev-half.dump"], "DAILY") {
		t.Errorf("prev-half.dump tiers = %v, want no DAILY: it is far outside the three-day window", tiersByArtifact["prev-half.dump"])
	}
	if got := tiersByArtifact["gap-winner.dump"]; len(got) != 1 || got[0] != "ANNUAL" {
		t.Errorf("gap-winner.dump tiers = %v, want exactly [ANNUAL]", got)
	}

	// The two artifacts nothing in the chain reaches.
	for _, name := range []string{"gap-loser.dump", "ancient.dump"} {
		if actionByArtifact[name] != "DELETE" {
			t.Errorf("%s action = %q with tiers %v, want DELETE: no configured tier covers it", name, actionByArtifact[name], tiersByArtifact[name])
		}
	}
	for _, name := range []string{"newest.dump", "prev-half.dump", "gap-winner.dump"} {
		if actionByArtifact[name] != "KEEP" {
			t.Errorf("%s action = %q, want KEEP", name, actionByArtifact[name])
		}
	}
	if plan.KeepCount != 3 || plan.DeleteCount != 2 {
		t.Errorf("KeepCount/DeleteCount = %d/%d, want 3/2 (plan=%+v)", plan.KeepCount, plan.DeleteCount, plan)
	}

	t.Run("control: chaining a monthly tier in rescues the gap artifact", func(t *testing.T) {
		// Same fixture, same instant, one more link: a monthly tier
		// reaching back far enough to cover the gap year. If gap-loser
		// stayed a delete candidate here, the DELETE assertions above
		// would prove nothing about the gap, only that the fixture is out
		// of reach of everything.
		monthly := config.RetentionTier{Name: "monthly", Granularity: config.GranularityMonth, Keep: 60}
		control := New(retentionTestConfig(bs, retentionChain(daily, monthly, semiAnnual, annual)), journal, nil, nil)
		got, err := control.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
		if err != nil {
			t.Fatalf("PreviewRetention (control): %v", err)
		}
		for _, v := range got.Verdicts {
			if v.Artifact != "gap-loser.dump" {
				continue
			}
			if v.Action != "KEEP" || !hasTier(v.Tiers, "MONTHLY") {
				t.Fatalf("control: gap-loser.dump = %s %v, want KEEP by MONTHLY", v.Action, v.Tiers)
			}
			return
		}
		t.Fatal("control: no verdict for gap-loser.dump")
	})

	// Applying the non-contiguous plan removes exactly the two gap and
	// out-of-range artifacts, and nothing else.
	applied, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice"})
	if err != nil {
		t.Fatalf("ApplyRetentionPlan: %v", err)
	}
	if applied.DeleteCount != 2 || applied.KeepCount != 3 {
		t.Errorf("applied = %+v, want DeleteCount=2, KeepCount=3", applied)
	}
	for _, name := range []string{"gap-loser.dump", "ancient.dump"} {
		if _, statErr := os.Lstat(filepath.Join(bs.LocalPath, name)); !os.IsNotExist(statErr) {
			t.Errorf("%s survived the apply: Lstat err = %v, want IsNotExist", name, statErr)
		}
	}
	for _, name := range []string{"newest.dump", "prev-half.dump", "gap-winner.dump"} {
		if _, statErr := os.Lstat(filepath.Join(bs.LocalPath, name)); statErr != nil {
			t.Errorf("%s was deleted despite a KEEP verdict: %v", name, statErr)
		}
	}
}
