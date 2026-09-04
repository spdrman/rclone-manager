package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// EPIC E FR-27/FR-30 (issue #239) at the preview/apply boundary: a
// preview has to show every MOVE and every DELETION with the medium it
// happens on, before anything runs.
//
// internal/retention decides where an artifact belongs, and internal/app
// already carries the answer on its own report. These tests pin the half
// that was missing: nothing outside core/ could see a move at all, so an
// operator confirming a plan was confirming a plan whose moves they were
// never shown.

// offsiteMonthlyChain is chainWithOffsiteMonthly's core/service twin (see
// internal/app/homemedium_test.go): a daily tier on the implicit local
// medium and a monthly tier that is not, which is the deployment story
// FR-27 exists for. Both are spelled out as tiers because a medium is
// only expressible in that spelling.
func offsiteMonthlyChain() config.Retention {
	off := false
	return config.Retention{
		Timezone: "UTC", WeekStartsOn: "monday",
		Tiers: []config.RetentionTier{
			{Name: "daily", Granularity: config.GranularityDay, Keep: 2},
			{Name: "monthly", Granularity: config.GranularityMonth, Keep: 12, Medium: "cold_offsite"},
		},
		ProtectLastKnownGood: &off,
	}
}

// seedPlacedArtifact is seedCompleteArtifact plus the FR-29 placement row
// that says where the durable copy actually is.
//
// The placement is written on the COMPLETE transition rather than patched
// in afterwards, because that is the only way a real ingestion writes one
// (internal/state.Transition.Placement), and a test that reached around
// it would be proving something about a row shape nothing produces.
func seedPlacedArtifact(t *testing.T, ctx context.Context, journal *state.Journal, bs config.BackupSet, name string, discoveredAt time.Time, content, medium string) model.ArtifactID {
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
	size := int64(len(content))
	location := localPath
	if medium != config.MediumLocal {
		location = "artifacts/" + name
	}
	if _, err := journal.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        "complete-" + name,
		From:       string(lifecycle.Discovered),
		To:         string(lifecycle.Complete),
		OccurredAt: discoveredAt,
		LocalPath:  &lp,
		Transfer:   &state.TransferResult{BytesTransferred: size, Checksummed: true},
		Placement: &state.PlacementUpdate{
			Medium: medium, Location: location, Size: &size,
			Hash: "abc", HashAlg: "sha256", VerificationClass: "content",
			Status: state.PlacementActive,
		},
	}); err != nil {
		t.Fatalf("RecordTransition(complete %s): %v", name, err)
	}
	return artifact
}

// addPlacement writes a SECOND placement row for an artifact that already
// has one, which is what an in-flight move looks like on the journal:
// FR-30's copy phase leaves the source and the destination both ACTIVE
// until the source delete lands.
func addPlacement(t *testing.T, ctx context.Context, journal *state.Journal, artifact model.ArtifactID, medium string, at time.Time) {
	t.Helper()
	size := int64(1)
	if _, err := journal.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        "place-" + medium + "-" + artifact.Name,
		From:       string(lifecycle.Complete),
		To:         string(lifecycle.Complete),
		OccurredAt: at,
		Placement: &state.PlacementUpdate{
			Medium: medium, Location: "artifacts/" + artifact.Name, Size: &size,
			Hash: "abc", HashAlg: "sha256", VerificationClass: "content",
			Status: state.PlacementActive,
		},
	}); err != nil {
		t.Fatalf("RecordTransition(place %s on %s): %v", artifact.Name, medium, err)
	}
}

// findVerdict is a small reader so each assertion below can name the
// artifact it is about rather than an index.
func findVerdict(t *testing.T, plan RetentionPlan, name string) RetentionArtifactVerdict {
	t.Helper()
	for _, v := range plan.Verdicts {
		if v.Artifact == name {
			return v
		}
	}
	t.Fatalf("no verdict for %q in a plan of %d verdicts", name, len(plan.Verdicts))
	return RetentionArtifactVerdict{}
}

// TestPreviewRetention_ShowsEveryMoveWithItsMedium is issue #239's AC2,
// first half: a preview names every artifact that is not where the chain
// says it belongs, and both mediums, before anything runs.
//
// The artifact is a month old, so the daily tier (keep 2) does not select
// it and the monthly tier does. Its home is therefore the monthly tier's
// medium, its ACTIVE placement is local, and that difference is a move.
func TestPreviewRetention_ShowsEveryMoveWithItsMedium(t *testing.T) {
	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()

	old := now().AddDate(0, -1, 0)
	seedPlacedArtifact(t, ctx, journal, bs, "month-old.dump", old, "twenty bytes!!!!!!!!", config.MediumLocal)

	svc := New(retentionTestConfig(bs, offsiteMonthlyChain()), journal, nil, nil)
	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	if len(plan.Moves) != 1 {
		t.Fatalf("plan.Moves = %+v, want exactly one move: the monthly tier is the first tier that selects a month-old artifact, and its medium is not where the artifact is", plan.Moves)
	}
	got := plan.Moves[0]
	want := RetentionMove{Artifact: "month-old.dump", FromMedium: config.MediumLocal, ToMedium: "cold_offsite"}
	if got != want {
		t.Errorf("plan.Moves[0] = %+v, want %+v", got, want)
	}
	if len(plan.UnconfirmedPlacements) != 0 {
		t.Errorf("plan.UnconfirmedPlacements = %v, want none: this artifact has exactly one ACTIVE placement", plan.UnconfirmedPlacements)
	}
}

// TestPreviewRetention_ShowsTheMediumEveryDeletionHappensOn is AC2's
// second half, and FR-30's own sentence: "the mandatory dry-run explains
// per-artifact WHERE the deletion would happen, not only whether".
//
// Both artifacts are old enough that nothing selects them, so both are
// DELETE. One is a local file and the other is an object in somebody
// else's bucket, and a preview that renders those two identically is
// asking an operator to authorise two very different acts with one word.
func TestPreviewRetention_ShowsTheMediumEveryDeletionHappensOn(t *testing.T) {
	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()

	old := now().AddDate(-2, 0, 0)
	seedPlacedArtifact(t, ctx, journal, bs, "here.dump", old, "twenty bytes!!!!!!!!", config.MediumLocal)
	seedPlacedArtifact(t, ctx, journal, bs, "offsite.dump", old.Add(time.Hour), "twenty bytes!!!!!!!!", "cold_offsite")

	svc := New(retentionTestConfig(bs, retentionTodayOnlyChain()), journal, nil, nil)
	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	local := findVerdict(t, plan, "here.dump")
	if local.Action != "DELETE" {
		t.Fatalf("here.dump is %s (%s), want DELETE so this test is about a deletion at all", local.Action, local.Reason)
	}
	if local.Medium != config.MediumLocal {
		t.Errorf("here.dump's verdict names medium %q, want %q", local.Medium, config.MediumLocal)
	}

	offsite := findVerdict(t, plan, "offsite.dump")
	if offsite.Medium != "cold_offsite" {
		t.Errorf("offsite.dump's verdict names medium %q, want %q: an operator confirming this plan is authorising a delete against a bucket, and the plan has to say so", offsite.Medium, "cold_offsite")
	}
}

// TestPreviewRetention_ReportsAPlacementItCouldNotConfirm pins the
// difference between "this is already at home" and "I could not confirm
// where this is". They are different claims, and only one of them is a
// reason to do nothing quietly.
//
// Two ACTIVE placements is a move already in flight, so there are two
// answers to "where is this" and the planner must take neither.
func TestPreviewRetention_ReportsAPlacementItCouldNotConfirm(t *testing.T) {
	bs := retentionTestBackupSet(t, t.TempDir())
	journal := openTestJournal(t)
	ctx := context.Background()

	old := now().AddDate(0, -1, 0)
	artifact := seedPlacedArtifact(t, ctx, journal, bs, "midmove.dump", old, "twenty bytes!!!!!!!!", config.MediumLocal)
	addPlacement(t, ctx, journal, artifact, "cold_offsite", old.Add(time.Hour))

	svc := New(retentionTestConfig(bs, offsiteMonthlyChain()), journal, nil, nil)
	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	if len(plan.Moves) != 0 {
		t.Errorf("plan.Moves = %+v, want none: an artifact whose location cannot be confirmed is never moved", plan.Moves)
	}
	if len(plan.UnconfirmedPlacements) != 1 || plan.UnconfirmedPlacements[0] != "midmove.dump" {
		t.Errorf("plan.UnconfirmedPlacements = %v, want exactly [midmove.dump]: silently skipping it would render identically to it already being at home", plan.UnconfirmedPlacements)
	}
}

// TestRetentionPlanRevision_CoversTheMovesSection is the RED plan's
// stale-plan line, asserted at the fingerprint rather than argued from
// the inputs that feed it.
//
// This is the same shape as this file's own mandatory finding M1: the
// guarantee "what would run is still what was reviewed" is an assertion
// about the reviewed OUTPUT, and the moves section is now part of that
// output. Two plans whose verdicts are byte-identical and whose moves are
// not are two different plans, and the fingerprint has to say so, whether
// or not some other revision happens to catch the same change today.
func TestRetentionPlanRevision_CoversTheMovesSection(t *testing.T) {
	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, "a.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	verdicts := []retention.PruneVerdict{{
		Artifact: artifact, Action: retention.PruneKeep,
		Medium: config.MediumLocal, Reason: "the monthly tier selects it",
	}}

	staying := app.PrunePlan{Verdicts: verdicts}
	moving := app.PrunePlan{Verdicts: verdicts, HomePlan: retention.HomePlan{
		Moves: []retention.HomeMove{{Artifact: artifact, From: config.MediumLocal, To: "cold_offsite"}},
	}}
	unconfirmed := app.PrunePlan{Verdicts: verdicts, HomePlan: retention.HomePlan{
		Unconfirmed: []model.ArtifactID{artifact},
	}}

	stayingRev := computeReviewedRevision(staying)
	if got := computeReviewedRevision(moving); got == stayingRev {
		t.Errorf("a plan that moves an artifact to cold_offsite fingerprints as %q, the same as a plan that moves nothing; a moves section nothing fingerprints is a section an apply can silently change", got)
	}
	if got := computeReviewedRevision(unconfirmed); got == stayingRev {
		t.Errorf("a plan reporting an artifact whose placement could not be confirmed fingerprints as %q, the same as a plan reporting none", got)
	}
	if computeReviewedRevision(moving) == computeReviewedRevision(unconfirmed) {
		t.Error("a planned move and an unconfirmed placement fingerprint identically; they are different claims about the same artifact")
	}
}

// TestApplyRetentionPlan_AMoveThatAppearedAfterThePreviewIsStale is the
// same guarantee end to end, over the surface an operator actually uses.
//
// The plan is previewed while the artifact's placement is ambiguous (no
// move can be planned), and the ambiguity is resolved before the apply,
// which makes a move appear that nobody confirmed. Zero files are
// deleted and the plan is refused.
func TestApplyRetentionPlan_AMoveThatAppearedAfterThePreviewIsStale(t *testing.T) {
	dir := t.TempDir()
	bs := retentionTestBackupSet(t, dir)
	journal := openTestJournal(t)
	ctx := context.Background()

	old := now().AddDate(0, -1, 0)
	artifact := seedPlacedArtifact(t, ctx, journal, bs, "midmove.dump", old, "twenty bytes!!!!!!!!", config.MediumLocal)
	addPlacement(t, ctx, journal, artifact, "cold_offsite", old.Add(time.Hour))

	svc := New(retentionTestConfig(bs, offsiteMonthlyChain()), journal, nil, nil)
	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}
	if len(plan.Moves) != 0 {
		t.Fatalf("the preview already plans %+v; this test needs a preview with no moves in it", plan.Moves)
	}

	// The in-flight move lands: the local copy is gone, so there is one
	// ACTIVE placement again and the planner can suddenly see a move.
	if _, err := journal.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        "retire-local-" + artifact.Name,
		From:       string(lifecycle.Complete),
		To:         string(lifecycle.Complete),
		OccurredAt: now(),
		Placement: &state.PlacementUpdate{
			Medium: config.MediumLocal, Location: filepath.Join(dir, "midmove.dump"),
			Status: state.PlacementGone,
		},
	}); err != nil {
		t.Fatalf("retiring the local placement: %v", err)
	}

	_, err = svc.ApplyRetentionPlan(ctx, ApplyRetentionRequest{
		PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice",
	})
	if !errors.Is(err, ErrRetentionPlanStale) {
		t.Fatalf("ApplyRetentionPlan = %v, want ErrRetentionPlanStale", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "midmove.dump")); statErr != nil {
		t.Errorf("the local file is gone after a refused apply (%v); a stale plan deletes nothing", statErr)
	}
}
