// This file is E2.2's integration leg (issue #239): preview, confirm and
// apply, with a MOVE and a DELETION in one plan, against a real S3 API.
//
// Everything below runs through the production surfaces. The plan comes
// out of service.BackupService.PreviewRetention, the deletion runs through
// service.BackupService.ApplyRetentionPlan against the plan_id that
// preview issued, and the move runs through app.Service.RunHomeMoves over
// the plans app.HomeMovePlans derives from the same report. Nothing here
// reassembles an engine or a resolver of its own, because a suite that
// reassembles the wiring proves something about the reassembly.
package miniointegration_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/service"
	"github.com/spdrman/rclone-manager/core/tests/machines"
)

const integrationMediumID = "cold_offsite"

// offsiteChain is the deployment story FR-27 exists for: recent backups on
// local disk, older ones offsite, expressed the only way a medium can be
// expressed.
func offsiteChain() config.Retention {
	protect := true
	return config.Retention{
		Timezone: "UTC", WeekStartsOn: "monday",
		Tiers: []config.RetentionTier{
			{Name: "daily", Granularity: config.GranularityDay, Keep: 2},
			{Name: "monthly", Granularity: config.GranularityMonth, Keep: 12, Medium: integrationMediumID},
		},
		ProtectLastKnownGood: &protect,
	}
}

// integrationConfig is a whole, resolved deployment addressed at the
// fixture's bucket.
func integrationConfig(t *testing.T, medium transport.Medium, credentialsFile, localRoot string) (*config.Config, config.BackupSet) {
	t.Helper()
	id, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	bs := config.BackupSet{Name: "postgres-primary", ID: id, LocalPath: localRoot}
	cfg := &config.Config{
		Sources:   []config.Source{{Name: id.Source, BackupSets: []config.BackupSet{bs}}},
		Retention: offsiteChain(),
		StorageMediums: []config.StorageMedium{{
			ID:          integrationMediumID,
			Type:        config.StorageMediumTypeS3,
			Region:      medium.Region,
			Endpoint:    medium.Endpoint,
			Bucket:      medium.Bucket,
			Credentials: config.MediumCredentials{File: credentialsFile},
		}},
	}
	if err := cfg.ResolveBackupSetRetention(); err != nil {
		t.Fatalf("resolving retention: %v", err)
	}
	return cfg, bs
}

// putObject uploads content at key through the same adapter the product
// uses, so the object a test plants is the object the product would have
// made.
func putObject(t *testing.T, medium transport.Medium, key string, content []byte) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "payload")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("writing the payload: %v", err)
	}
	if _, err := rclone.New().UploadFromLocal(context.Background(), medium, path, key, transport.UploadOptions{}); err != nil {
		t.Fatalf("uploading to %q: %v", key, err)
	}
}

// integrationFixture seeds the two artifacts every test below decides
// over, and returns the services that decide.
//
//   - "recent" is today's backup and stays where it is. It is the control,
//     and it is TODAY's rather than a couple of days old on purpose: the
//     daily tier keeps two, so an artifact two days back is already
//     outside it and the monthly tier is the first that selects it, which
//     makes its home offsite and it a move like any other. Which tier is
//     FIRST is the whole of FR-27's rule, and a control has to sit on the
//     side of it the test claims.
//   - "month-old" is 40 days old, so the daily tier no longer selects it
//     and the monthly tier does. Its home is offsite and its bytes are
//     local, which is a MOVE.
//   - "ancient" is 800 days old, so nothing selects it, and its only
//     durable copy is a real object on the bucket, which is a DELETION on
//     a medium.
func integrationFixture(t *testing.T) (*service.BackupService, *app.Service, config.BackupSet, *state.Journal, transport.Medium, string) {
	t.Helper()
	fixture := machines.Start(t).Medium(t)
	medium := fixture.NewBucket(t)
	root := t.TempDir()
	journal := openJournal(t)
	now := time.Now().UTC()

	cfg, bs := integrationConfig(t, medium, fixture.CredentialsFile, root)

	for _, seed := range []struct {
		name string
		age  int
	}{{"recent.dump", 0}, {"month-old.dump", 40}} {
		artifact := model.ArtifactID{Set: bs.ID, Name: seed.name}
		content := []byte("the bytes of " + seed.name)
		path := filepath.Join(root, seed.name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatalf("writing %s: %v", seed.name, err)
		}
		seedArtifactOnLocal(t, journal, artifact, path, content, now.AddDate(0, 0, -seed.age))
	}

	ancient := model.ArtifactID{Set: bs.ID, Name: "ancient.dump"}
	ancientContent := []byte("an expired artifact, living in a real bucket")
	key, err := transport.MediumKey(medium.Prefix, ancient)
	if err != nil {
		t.Fatalf("computing the key: %v", err)
	}
	putObject(t, medium, key, ancientContent)
	seedArtifactOnMedium(t, journal, ancient, key, ancientContent, integrationMediumID, now.AddDate(0, 0, -800))

	return service.New(cfg, journal, rclone.New(), nil), app.New(cfg, journal, rclone.New(), nil), bs, journal, medium, key
}

// TestPreviewConfirmAndApplyOverMinIO is the whole integration line: one
// preview showing both a move and a deletion, one confirmation, and both
// carried out against a real S3 API.
func TestPreviewConfirmAndApplyOverMinIO(t *testing.T) {
	ctx := context.Background()
	svc, appSvc, bs, journal, medium, ancientKey := integrationFixture(t)

	// --- preview: it shows both, and neither has happened ---

	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	if len(plan.Moves) != 1 {
		t.Fatalf("plan.Moves = %+v, want exactly the month-old artifact moving offsite", plan.Moves)
	}
	if got, want := plan.Moves[0], (service.RetentionMove{Artifact: "month-old.dump", FromMedium: "local", ToMedium: integrationMediumID}); got != want {
		t.Errorf("plan.Moves[0] = %+v, want %+v", got, want)
	}

	var deletions int
	for _, v := range plan.Verdicts {
		if v.Action != "DELETE" {
			continue
		}
		deletions++
		if v.Artifact != "ancient.dump" {
			t.Errorf("the plan would delete %s; only the artifact no tier selects should be on that list", v.Artifact)
		}
		if v.Medium != integrationMediumID {
			t.Errorf("the deletion of %s names medium %q, want %q: an operator confirming this is authorising a delete against a bucket", v.Artifact, v.Medium, integrationMediumID)
		}
	}
	if deletions != 1 {
		t.Fatalf("the plan carries %d deletions, want exactly one (verdicts: %+v)", deletions, plan.Verdicts)
	}

	if _, err := rclone.New().StatObject(ctx, medium, ancientKey); err != nil {
		t.Fatalf("the object is already gone at preview time (%v); a preview touches nothing", err)
	}
	if _, err := os.Lstat(filepath.Join(bs.LocalPath, "month-old.dump")); err != nil {
		t.Fatalf("the local copy is already gone at preview time: %v", err)
	}

	// --- confirm and apply: the deletion happens, on the bucket ---

	applied, err := svc.ApplyRetentionPlan(ctx, service.ApplyRetentionRequest{
		PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice",
	})
	if err != nil {
		t.Fatalf("ApplyRetentionPlan: %v", err)
	}
	if applied.DeleteCount != 1 {
		t.Errorf("the apply reports %d deletions, want 1 (%+v)", applied.DeleteCount, applied.Verdicts)
	}
	if _, err := rclone.New().StatObject(ctx, medium, ancientKey); err == nil {
		t.Error("the expired object is still on the bucket after a confirmed apply")
	}
	if _, err := os.Lstat(filepath.Join(bs.LocalPath, "recent.dump")); err != nil {
		t.Errorf("the control artifact's local copy is gone after the apply: %v", err)
	}

	// --- and the move, driven by the same home plan ---

	report, err := appSvc.RetentionPreview(ctx, bs.ID)
	if err != nil {
		t.Fatalf("RetentionPreview: %v", err)
	}
	moves, err := appSvc.RunHomeMoves(ctx, app.HomeMovePlans(report.HomePlan))
	if err != nil {
		t.Fatalf("RunHomeMoves: %v", err)
	}
	if moves.Completed != 1 {
		t.Fatalf("the move cycle completed %d moves, want 1: %+v", moves.Completed, moves.Outcomes)
	}

	moved := model.ArtifactID{Set: bs.ID, Name: "month-old.dump"}
	movedKey, err := transport.MediumKey(medium.Prefix, moved)
	if err != nil {
		t.Fatalf("computing the key: %v", err)
	}
	if _, err := rclone.New().StatObject(ctx, medium, movedKey); err != nil {
		t.Errorf("the moved artifact is not on the bucket at %q: %v", movedKey, err)
	}
	if _, err := os.Lstat(filepath.Join(bs.LocalPath, "month-old.dump")); err == nil {
		t.Error("the local copy survives a completed move; a move is copy, verify, THEN delete the source")
	}
	if _, err := os.Lstat(filepath.Join(bs.LocalPath, "recent.dump")); err != nil {
		t.Errorf("the control artifact was moved too (%v); the daily tier keeps it on local", err)
	}
	assertPlacement(t, journal, moved, integrationMediumID, state.PlacementActive, state.VerificationContent)
}

// TestApplyOverMinIORefusesADeletionWhoseObjectChanged is AC3's planted
// violation, over a real endpoint: the fixture swaps the object behind the
// key between the preview and the apply, and the delete must be refused.
//
// FR-16's re-check is the whole point of the refusal. The plan is not
// stale by any journal or configuration measure, so nothing upstream of
// the delete has any reason to stop: the only thing standing between this
// and removing an object the journal does not describe is the stat and the
// size comparison that placement.Reclaimer runs immediately before it.
//
// This is written before the success path in the sense that matters
// (TDD invariant 9): it is the assertion I would keep if I could only
// keep one, because a build that deletes here has destroyed something a
// deployment cannot get back.
func TestApplyOverMinIORefusesADeletionWhoseObjectChanged(t *testing.T) {
	ctx := context.Background()
	svc, _, bs, _, medium, ancientKey := integrationFixture(t)

	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	// Somebody else's object, at this manager's key. Nothing in the
	// journal changed, so the plan is still perfectly fresh.
	swapped := []byte("a completely different object, of a completely different length, put here by something that is not this manager")
	putObject(t, medium, ancientKey, swapped)

	applied, err := svc.ApplyRetentionPlan(ctx, service.ApplyRetentionRequest{
		PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice",
	})
	if err != nil {
		t.Fatalf("ApplyRetentionPlan: %v", err)
	}

	var refusals int
	for _, v := range applied.Verdicts {
		if v.Artifact != "ancient.dump" {
			continue
		}
		if v.Action != "REFUSE" {
			t.Fatalf("ancient.dump is %s (%s), want REFUSE: the object at its key is not the one this journal recorded", v.Action, v.Reason)
		}
		refusals++
	}
	if refusals != 1 {
		t.Fatalf("no verdict at all for ancient.dump in %+v", applied.Verdicts)
	}
	if applied.DeleteCount != 0 {
		t.Errorf("the apply reports %d deletions, want 0", applied.DeleteCount)
	}

	info, err := rclone.New().StatObject(ctx, medium, ancientKey)
	if err != nil {
		t.Fatalf("the object was deleted despite the refusal: %v", err)
	}
	if info.Size != int64(len(swapped)) {
		t.Errorf("the object at %q is %d bytes, want the %d the fixture put there; something rewrote it", ancientKey, info.Size, len(swapped))
	}
}

// TestApplyOverMinIORefusesWhenTheObjectIsSimplyGone is the other half of
// FR-16's "identity cannot be established". An object that is not at its
// key means the journal and the medium disagree about a backup, and the
// answer is a refusal an operator can reconcile.
//
// It is deliberately NOT convergence. The move engine has a case for a
// source that is already gone, because it holds a durably recorded,
// verified destination copy proving the artifact still exists somewhere.
// Here there is no such copy, and reporting "deleted, fine" would record
// a deletion this manager never performed against bytes it never found.
func TestApplyOverMinIORefusesWhenTheObjectIsSimplyGone(t *testing.T) {
	ctx := context.Background()
	svc, _, bs, _, medium, ancientKey := integrationFixture(t)

	plan, err := svc.PreviewRetention(ctx, bs.ID.Source, bs.ID.Set)
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}
	if err := rclone.New().DeleteObject(ctx, medium, ancientKey); err != nil {
		t.Fatalf("removing the object out from under the plan: %v", err)
	}

	applied, err := svc.ApplyRetentionPlan(ctx, service.ApplyRetentionRequest{
		PlanID: plan.PlanID, Source: bs.ID.Source, Set: bs.ID.Set, Actor: "alice",
	})
	if err != nil {
		t.Fatalf("ApplyRetentionPlan: %v", err)
	}
	for _, v := range applied.Verdicts {
		if v.Artifact == "ancient.dump" && v.Action != "REFUSE" {
			t.Fatalf("ancient.dump is %s (%s), want REFUSE: there is no object at its key to prove anything about", v.Action, v.Reason)
		}
	}
	if applied.DeleteCount != 0 {
		t.Errorf("the apply reports %d deletions, want 0", applied.DeleteCount)
	}
	if _, err := rclone.New().StatObject(ctx, medium, ancientKey); err == nil {
		t.Error("the object came back, which means this test proved nothing")
	}
}
