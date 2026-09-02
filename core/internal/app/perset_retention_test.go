package app

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// Issue #333's fourth acceptance criterion, at the app layer: a preview
// has to say which policy decided it. The verdicts alone cannot, because
// an override and a global policy that agree produce identical output, and
// "go and edit the set" is different advice from "go and edit the
// deployment".

func TestRetentionPreview_ReportsWhetherTheSetOverridesThePolicy(t *testing.T) {
	ctx := context.Background()

	t.Run("inheriting set reports no override", func(t *testing.T) {
		dir := t.TempDir()
		journal := openJournal(t)
		bs := testBackupSet(t, dir)
		cfg := testConfig(t, testSource("production", bs))
		svc := New(cfg, journal, nil, nil)

		got, err := svc.RetentionPreview(ctx, bs.ID)
		if err != nil {
			t.Fatalf("RetentionPreview: %v", err)
		}
		if got.RetentionIsOverride {
			t.Error("a set with no retention block reported an override")
		}
	})

	t.Run("overriding set reports an override", func(t *testing.T) {
		dir := t.TempDir()
		journal := openJournal(t)
		bs := testBackupSet(t, dir)
		own := pruneDailyOnlyRetention()
		bs.RetentionConfig = &own
		cfg := testConfig(t, testSource("production", bs))
		svc := New(cfg, journal, nil, nil)

		got, err := svc.RetentionPreview(ctx, bs.ID)
		if err != nil {
			t.Fatalf("RetentionPreview: %v", err)
		}
		if !got.RetentionIsOverride {
			t.Error("a set with its own retention block did not report an override")
		}
	})
}

func TestPrunePreview_ReportsWhetherTheSetOverridesThePolicy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	journal := openJournal(t)
	bs := testBackupSet(t, dir)
	own := pruneDailyOnlyRetention()
	bs.RetentionConfig = &own
	cfg := testConfig(t, testSource("production", bs))
	svc := New(cfg, journal, nil, nil)

	plan, err := svc.PrunePreview(ctx, bs.ID)
	if err != nil {
		t.Fatalf("PrunePreview: %v", err)
	}
	if !plan.RetentionIsOverride {
		t.Error("PrunePreview did not report the set's override")
	}
}

// TestRetentionPreview_UsesTheSetsOwnChainNotTheGlobalOne is the one that
// would still pass if the attribution flag were wired up but the policy
// itself were not: it checks the decision, not the label.
func TestRetentionPreview_UsesTheSetsOwnChainNotTheGlobalOne(t *testing.T) {
	dir := t.TempDir()
	bs := testBackupSet(t, dir)
	own := config.Retention{
		Timezone: "UTC", WeekStartsOn: "monday",
		Tiers: []config.RetentionTier{
			{Name: "only_yearly", Granularity: config.GranularityYear, Keep: 1},
		},
	}
	bs.RetentionConfig = &own
	cfg := testConfig(t, testSource("production", bs))

	resolved := cfg.Sources[0].BackupSets[0].Retention
	tiers := resolved.EffectiveTiers()
	if len(tiers) != 1 || tiers[0].Name != "only_yearly" {
		t.Fatalf("the set resolved to %+v, want its own single only_yearly tier rather than the global chain", tiers)
	}
	if len(cfg.Retention.EffectiveTiers()) == 1 && cfg.Retention.EffectiveTiers()[0].Name == "only_yearly" {
		t.Fatal("the global policy was overwritten by the set's override")
	}
}
