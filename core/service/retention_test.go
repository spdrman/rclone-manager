package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	_, err = svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Actor: "alice"})

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
	if _, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Actor: "alice"}); !errors.Is(err, ErrRetentionPlanNotFound) {
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

	result, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Actor: "alice"})
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
	if _, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Actor: "alice"}); !errors.Is(err, ErrRetentionPlanNotFound) {
		t.Errorf("re-applying an already-applied plan_id: error = %v, want errors.Is(err, ErrRetentionPlanNotFound)", err)
	}
}

// TestApplyRetentionPlan_UnknownPlanIDReturnsNotFound is ApplyRetentionPlan's
// negative/refusal case for a plan_id nobody ever issued: distinct from
// ErrRetentionPlanStale (which implies a plan that WAS once valid).
func TestApplyRetentionPlan_UnknownPlanIDReturnsNotFound(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.ApplyRetentionPlan(context.Background(), ApplyRetentionRequest{PlanID: "retplan_does-not-exist", Actor: "alice"})
	if !errors.Is(err, ErrRetentionPlanNotFound) {
		t.Fatalf("ApplyRetentionPlan error = %v, want errors.Is(err, ErrRetentionPlanNotFound)", err)
	}
}

// TestApplyRetentionPlan_MissingPlanIDIsInvalidRequest is the request-
// validation refusal (an empty plan_id is malformed input, not "this
// plan_id is unknown" or "this plan_id is stale").
func TestApplyRetentionPlan_MissingPlanIDIsInvalidRequest(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.ApplyRetentionPlan(context.Background(), ApplyRetentionRequest{Actor: "alice"})
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

	_, err = svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Actor: "alice"})
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
	applied, err := svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{PlanID: plan.PlanID, Actor: "alice"})
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
