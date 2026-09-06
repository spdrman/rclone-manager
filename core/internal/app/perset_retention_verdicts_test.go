package app

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is issue #333 asked at the only layer where the answer is
// worth anything: the verdicts. Every other test of this feature checks
// that the right chain was RESOLVED for a set. These check what that
// chain then DELETES.
//
// Two properties, and the second one is the reason this file exists.
//
// A per-set override has to be able to move a decision in both
// directions. One that keeps less than the deployment's chain deletes
// artifacts the deployment would have kept, which is the dangerous
// direction and the one an operator most needs the preview to be honest
// about; one that keeps more retains artifacts the deployment would have
// deleted. A feature that only ever widened, or only ever narrowed, would
// pass a resolution test and still be broken.
//
// And a backup set that never asked for any of this has to go on being
// retained exactly as it was. That is the upgrade case: every backup set
// in every deployment written before this schema existed has no retention
// block, and the control set below is one of them. It is asserted against
// the SAME baseline verdicts in every scenario, including the two where
// the other set's policy changed underneath it, because "we did not touch
// the sets that did not ask" is the whole safety claim of this change.

// retentionNow is the instant every scenario in this file decides at.
// Fixed, because every verdict here is a statement about calendar
// distance from it.
var retentionNow = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

// The three ages, chosen so that a 30-day window separates them: two
// inside it and one well outside.
var (
	ageRecent = retentionNow.AddDate(0, 0, -1)
	ageMiddle = retentionNow.AddDate(0, 0, -10)
	ageOld    = retentionNow.AddDate(0, 0, -40)
)

// dayTierChain is a whole, self-contained policy naming a single
// day-granularity tier. Whole matters: config refuses a per-set override
// that names two thirds of a chain, because the missing third would
// resolve to the product default rather than to the deployment's policy.
func dayTierChain(name string, keep int) config.Retention {
	return config.Retention{
		Tiers: []config.RetentionTier{
			{Name: name, Granularity: config.GranularityDay, Keep: keep},
		},
	}
}

// seedComplete drives one artifact through the real pipeline to COMPLETE
// with a chosen discovery date, which is what GFS buckets on.
func seedComplete(t *testing.T, ctx context.Context, svc *Service, journal Journal, tr *fakeTransport, source transport.Source, bs config.BackupSet, name string, when time.Time) state.Record {
	t.Helper()
	tr.put(name, "payload for "+name, when.Unix())
	rec := discoverAt(t, ctx, journal, tr, source, bs, when)
	svc.Now = fixedNow(when)
	svc.processArtifact(ctx, source, bs, rec)

	final, err := journal.Get(ctx, rec.Artifact)
	if err != nil {
		t.Fatalf("Get(%s): %v", name, err)
	}
	if final.State != string(lifecycle.Complete) {
		t.Fatalf("precondition failed: %s reached state %q, want %q", name, final.State, lifecycle.Complete)
	}
	return rec
}

// verdictLine renders a prune plan as one comparable string, sorted by
// artifact name so it does not depend on map or query order.
func verdictLine(t *testing.T, plan PrunePlan) string {
	t.Helper()
	parts := make([]string, 0, len(plan.Verdicts))
	for _, v := range plan.Verdicts {
		parts = append(parts, fmt.Sprintf("%s=%s", v.Artifact.Name, v.Action))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// twoSetJournal builds a deployment with two backup sets under one
// source, seeds each of them with the same three artifact ages, and hands
// back everything a scenario needs to re-decide under a different policy.
//
// The two sets take disjoint include patterns because they share one fake
// transport, and the control set gets its own local directory so an apply
// against one can be shown not to have touched the other.
func twoSetJournal(t *testing.T) (cfg *config.Config, journal Journal, tr *fakeTransport, source transport.Source, target, control config.BackupSet) {
	t.Helper()
	ctx := context.Background()

	target = testBackupSet(t, t.TempDir())
	target.Include = []string{"pg-*.dump"}
	target.RemotePath = ""

	control = testBackupSet(t, t.TempDir())
	control.Name = "media-share"
	control.ID = mustSetID(t, "production", "media-share")
	control.Include = []string{"media-*.dump"}
	control.RemotePath = ""

	tr = newFakeTransport()
	journal = openJournal(t)
	source = transport.Source{ID: "perset-retention"}

	// The deployment's own chain: 30 days, which is deliberately neither
	// the product default nor either override below, so a decision that
	// reached for the wrong one of the three is visible.
	cfg = testConfig(t, testSource("production", target, control))
	cfg.Retention = config.Retention{
		Timezone: "UTC", WeekStartsOn: "monday",
		Tiers:                dayTierChain("deployment_daily", 30).Tiers,
		ProtectLastKnownGood: cfg.Retention.ProtectLastKnownGood,
	}
	resolveTestRetention(cfg)

	svc := New(cfg, journal, tr, nil)
	for _, bs := range []config.BackupSet{target, control} {
		prefix := "pg-"
		if bs.Name == control.Name {
			prefix = "media-"
		}
		seedComplete(t, ctx, svc, journal, tr, source, bs, prefix+"old.dump", ageOld)
		seedComplete(t, ctx, svc, journal, tr, source, bs, prefix+"middle.dump", ageMiddle)
		seedComplete(t, ctx, svc, journal, tr, source, bs, prefix+"recent.dump", ageRecent)
	}
	return cfg, journal, tr, source, target, control
}

// TestPerSetRetention_AnOverrideMovesVerdictsInBothDirections is the
// issue's whole promise, checked on the deletions rather than on the
// chain.
//
// The control set is asserted in every case, against the same expected
// line every time. That is the point of it: it never declares a policy,
// so nothing any other set does may ever change what it deletes.
func TestPerSetRetention_AnOverrideMovesVerdictsInBothDirections(t *testing.T) {
	const (
		// Under the deployment's 30-day chain: the 40-day-old artifact is
		// outside the window, the other two are inside it.
		deploymentVerdicts = "old.dump=DELETE middle.dump=KEEP recent.dump=KEEP"
	)
	// Written unsorted above for readability; verdictLine sorts, so the
	// expectations are built the same way.
	expect := func(prefix string, old, middle, recent string) string {
		parts := []string{
			prefix + "old.dump=" + old,
			prefix + "middle.dump=" + middle,
			prefix + "recent.dump=" + recent,
		}
		sort.Strings(parts)
		return strings.Join(parts, " ")
	}
	_ = deploymentVerdicts

	cases := []struct {
		name string
		// override is the policy the target set declares, or nil for the
		// pre-#333 case where it declares none at all.
		override *config.Retention
		// what the target set's three artifacts should decide to.
		targetOld, targetMiddle, targetRecent string
	}{
		{
			// The control case, and the upgrade case: a set with no
			// retention block of its own decides exactly as the
			// deployment's chain says, which is what every backup set in
			// every existing deployment does today.
			name:      "no override at all: the deployment's chain decides",
			override:  nil,
			targetOld: "DELETE", targetMiddle: "KEEP", targetRecent: "KEEP",
		},
		{
			// The dangerous direction. The set's own chain reaches back
			// five days, so an artifact the deployment's policy keeps is
			// now a delete candidate. This is the case where a resolution
			// bug destroys backups, and it is why the override has to be
			// visible in a preview.
			name:      "an override that keeps less deletes what the deployment kept",
			override:  ptrRetention(dayTierChain("set_daily", 5)),
			targetOld: "DELETE", targetMiddle: "DELETE", targetRecent: "KEEP",
		},
		{
			// The other direction, which a feature that only ever
			// narrowed would still pass every resolution test with.
			name:      "an override that keeps more retains what the deployment deleted",
			override:  ptrRetention(dayTierChain("set_daily", 365)),
			targetOld: "KEEP", targetMiddle: "KEEP", targetRecent: "KEEP",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, journal, tr, _, target, control := twoSetJournal(t)
			cfg.Sources[0].BackupSets[0].RetentionConfig = tc.override
			resolveTestRetention(cfg)

			svc := New(cfg, journal, tr, nil)
			svc.Now = fixedNow(retentionNow)
			ctx := context.Background()

			targetPlan, err := svc.PrunePreview(ctx, target.ID)
			if err != nil {
				t.Fatalf("PrunePreview(target): %v", err)
			}
			want := expect("pg-", tc.targetOld, tc.targetMiddle, tc.targetRecent)
			if got := verdictLine(t, targetPlan); got != want {
				t.Errorf("the overriding set decided\n  %s\nwant\n  %s", got, want)
			}
			if targetPlan.RetentionIsOverride != (tc.override != nil) {
				t.Errorf("RetentionIsOverride = %v, want %v", targetPlan.RetentionIsOverride, tc.override != nil)
			}

			// The set that asked for nothing, in every scenario.
			controlPlan, err := svc.PrunePreview(ctx, control.ID)
			if err != nil {
				t.Fatalf("PrunePreview(control): %v", err)
			}
			wantControl := expect("media-", "DELETE", "KEEP", "KEEP")
			if got := verdictLine(t, controlPlan); got != wantControl {
				t.Errorf("the set with no retention block of its own decided\n  %s\nwant the deployment's\n  %s", got, wantControl)
			}
			if controlPlan.RetentionIsOverride {
				t.Error("a set with no retention block reported an override")
			}
		})
	}
}

func ptrRetention(r config.Retention) *config.Retention { return &r }

// TestPerSetRetention_ANarrowerOverrideActuallyDeletesTheFile is the same
// dangerous direction taken past the preview to the one irreversible
// step. A preview that says DELETE and an apply that removes a file are
// different claims, and this feature is only as safe as the second one.
//
// The control set's artifact of the same age is checked afterwards and
// has to still be on disk, because the two sets differ only in that one
// of them declared a policy.
func TestPerSetRetention_ANarrowerOverrideActuallyDeletesTheFile(t *testing.T) {
	cfg, journal, tr, _, target, control := twoSetJournal(t)
	override := dayTierChain("set_daily", 5)
	cfg.Sources[0].BackupSets[0].RetentionConfig = &override
	resolveTestRetention(cfg)

	svc := New(cfg, journal, tr, nil)
	svc.Now = fixedNow(retentionNow)
	ctx := context.Background()

	targetMiddle := localPathOf(t, ctx, journal, target.ID, "pg-middle.dump")
	controlMiddle := localPathOf(t, ctx, journal, control.ID, "media-middle.dump")
	for _, p := range []string{targetMiddle, controlMiddle} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("precondition failed: %s is not on disk: %v", p, err)
		}
	}

	if _, err := svc.PruneApply(ctx, target.ID); err != nil {
		t.Fatalf("PruneApply(target): %v", err)
	}

	if _, err := os.Stat(targetMiddle); !os.IsNotExist(err) {
		t.Errorf("the overriding set's 10-day-old artifact is still on disk (stat err = %v); its own chain reaches back five days", err)
	}
	if _, err := os.Stat(controlMiddle); err != nil {
		t.Errorf("applying one set's own retention policy deleted an artifact belonging to a set that declares no policy: %v", err)
	}
}

// localPathOf finds the on-disk path the journal recorded for one
// artifact of one backup set, by name.
func localPathOf(t *testing.T, ctx context.Context, journal Journal, set model.BackupSetID, name string) string {
	t.Helper()
	records, err := journal.ListByBackupSet(ctx, set)
	if err != nil {
		t.Fatalf("ListByBackupSet: %v", err)
	}
	for _, r := range records {
		if r.Artifact.Name == name {
			if r.LocalPath == "" {
				t.Fatalf("record for %s has no local path", name)
			}
			return r.LocalPath
		}
	}
	t.Fatalf("no journal record named %q in %s", name, set)
	return ""
}

// TestPruneRefusesAChainThatKeepsNothing is the invariant that has to
// hold for this whole feature to be safe to ship, stated on the path that
// actually removes files.
//
// config.Retention's zero value is not "no policy configured". It expands
// to the default three-tier chain with every keep at zero, which selects
// nothing, which puts every managed backup in the set on the delete side.
// Before #333 there was one place that value could arrive from; now there
// is one per backup set, and the one that matters most is the set that
// declares NO override, because that is every set in every deployment
// that existed before this schema did. So there must be no path on which
// an unset override reads as "retain nothing".
//
// internal/app's own TestRetentionPreview_RefusesAnUnresolvedPolicy pins
// this for the read-only preview. This pins it for FR-20's prune, both
// halves, and checks the files are still there afterwards: a refusal that
// happened after the delete would be no refusal at all.
func TestPruneRefusesAChainThatKeepsNothing(t *testing.T) {
	cases := []struct {
		name string
		ret  config.Retention
	}{
		{"nothing resolved at all", config.Retention{}},
		{
			// The likelier bug of the two: something inherits the
			// calendar and forgets the chain. It reads as a configured
			// policy at a glance and it keeps nothing.
			"a calendar with no chain behind it",
			config.Retention{Timezone: "UTC", WeekStartsOn: "monday"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, journal, tr, _, target, _ := twoSetJournal(t)
			// Reach past resolution on purpose: this is what a caller
			// that built a Config by hand, or a resolution step that
			// dropped the chain, hands the decision layer.
			cfg.Sources[0].BackupSets[0].Retention = tc.ret

			svc := New(cfg, journal, tr, nil)
			svc.Now = fixedNow(retentionNow)
			ctx := context.Background()

			oldest := localPathOf(t, ctx, journal, target.ID, "pg-old.dump")
			if _, err := os.Stat(oldest); err != nil {
				t.Fatalf("precondition failed: %s is not on disk: %v", oldest, err)
			}

			if _, err := svc.PrunePreview(ctx, target.ID); err == nil {
				t.Error("PrunePreview reported on a chain that keeps nothing instead of refusing it")
			}
			if _, err := svc.PruneApply(ctx, target.ID); err == nil {
				t.Error("PruneApply ran against a chain that keeps nothing instead of refusing it")
			}
			if _, err := os.Stat(oldest); err != nil {
				t.Errorf("a refused prune deleted a file anyway: %v", err)
			}
		})
	}
}
