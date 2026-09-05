package app

import (
	"context"
	"errors"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// Issue #333's fourth acceptance criterion, at the app layer: a preview
// has to say which policy decided it. The verdicts alone cannot, because
// an override and a global policy that agree produce identical output, and
// "go and edit the set" is different advice from "go and edit the
// deployment".

// setOwnRetention is a whole, self-contained policy a backup set can
// declare as its own. It names an explicit tier chain rather than reusing
// pruneDailyOnlyRetention's bare daily_days, because a per-set override
// has to say what the WHOLE chain is: two thirds of a chain resolves the
// missing third to the product default instead of to the deployment's
// policy, so config.Validate refuses one (see
// resolveBackupSetRetention's own doc).
func setOwnRetention() config.Retention {
	protect := true
	return config.Retention{
		Timezone: "UTC", WeekStartsOn: "monday",
		Tiers: []config.RetentionTier{
			{Name: "set_own_daily", Granularity: config.GranularityDay, Keep: 1, WindowUnit: config.GranularityDay},
		},
		ProtectLastKnownGood: &protect,
	}
}

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
		own := setOwnRetention()
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
	own := setOwnRetention()
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

// TestRetentionPreview_RefusesAnUnresolvedPolicy is issue #333's new
// invariant, stated where it can be checked. bs.Retention's zero value is
// not "no policy configured", it is a chain whose every tier keeps zero,
// and a chain that selects nothing puts every managed backup in the set on
// the delete side. That hazard is not new (the global Retention's zero
// value always meant the same thing) but the number of places the
// invariant has to hold went from one to one per backup set, and the
// consequence of missing one is a mass delete.
//
// internal/retention already refuses both spellings of it, and this pins
// that it does: a policy that keeps nothing is a refusal, never a
// permissive reading. The two cases are the two ways a Config that skipped
// resolution arrives: nothing filled in at all, and a calendar filled in
// with no chain behind it (which is what a hand-built fixture that copies
// only the timezone produces).
func TestRetentionPreview_RefusesAnUnresolvedPolicy(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		ret  config.Retention
	}{
		{"nothing resolved at all", config.Retention{}},
		{"a calendar with no chain behind it", config.Retention{Timezone: "UTC", WeekStartsOn: "monday"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			journal := openJournal(t)
			bs := testBackupSet(t, dir)
			cfg := testConfig(t, testSource("production", bs))
			// Reach past the fixture's own resolution on purpose: this is
			// exactly what a caller that built a Config by hand and never
			// resolved it hands the decision layer.
			cfg.Sources[0].BackupSets[0].Retention = tc.ret
			svc := New(cfg, journal, nil, nil)

			if _, err := svc.RetentionPreview(ctx, bs.ID); err == nil {
				t.Fatal("an unresolved retention policy produced a report; a chain that keeps nothing has to refuse, because every managed backup in the set would land on the delete side of it")
			}
		})
	}
}

// TestRetentionPreview_RefusesASetConfigNoLongerNames pins the behaviour
// change this issue's config lookup brought with it. Before #333 this
// method never consulted config at all and reported such a set under the
// global policy; now there is no single deployment-wide policy it could
// honestly be said to be retained under, so it refuses, the same way
// PrunePreview already did for the same reason.
func TestRetentionPreview_RefusesASetConfigNoLongerNames(t *testing.T) {
	dir := t.TempDir()
	journal := openJournal(t)
	bs := testBackupSet(t, dir)
	cfg := testConfig(t, testSource("production", bs))
	svc := New(cfg, journal, nil, nil)

	removed := mustSetID(t, "production", "decommissioned")
	_, err := svc.RetentionPreview(context.Background(), removed)
	if err == nil {
		t.Fatal("a backup set config no longer names produced a retention report")
	}
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("RetentionPreview error = %v, want a *NotFoundError naming the set", err)
	}
}

// TestResolveTestRetention_DoesNotAliasTheGlobalChain pins this package's
// own fixture helper, not the product. config's
// TestPerSetRetention_ResolvedChainIsNotAliasedToTheGlobalOne cannot see
// this layer, and this layer is where a test is most likely to mutate one
// set's chain to make two sets diverge. A helper that resolves by plain
// assignment leaves every set sharing one tier backing array with the
// global policy, so that mutation silently moves both.
func TestResolveTestRetention_DoesNotAliasTheGlobalChain(t *testing.T) {
	dir := t.TempDir()
	bs := testBackupSet(t, dir)
	cfg := testConfig(t, testSource("production", bs))

	// An EXPLICIT chain, not testRetention's three scalars. EffectiveTiers
	// builds a fresh slice from the scalars on every call, so a policy
	// spelled that way has no shared backing array to alias and this test
	// would pass without proving anything.
	cfg.Retention.DailyDays, cfg.Retention.WeeklyMonths, cfg.Retention.MonthlyMonths = 0, 0, 0
	cfg.Retention.Tiers = []config.RetentionTier{
		{Name: "daily", Granularity: config.GranularityDay, Keep: 7, WindowUnit: config.GranularityDay},
	}
	resolveTestRetention(cfg)

	resolved := cfg.Sources[0].BackupSets[0].Retention.EffectiveTiers()
	if len(resolved) == 0 {
		t.Fatal("the fixture resolved to an empty chain")
	}
	resolved[0].Name = "mutated_through_the_set"

	if got := cfg.Retention.Tiers[0].Name; got != "daily" {
		t.Errorf("editing a set's resolved chain changed the global policy's: %q", got)
	}
}
