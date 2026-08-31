package retention

import (
	"reflect"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// --- test helpers (prefixed lkg* so these never collide with gfs_test.go's
// gfs*-prefixed helpers in this same package; gfsMustSet, gfsMustArtifact,
// gfsRecSpec and gfsBuildRecords are generic enough to reuse directly) ---

func lkgBoolPtr(b bool) *bool { return &b }

// --- LastKnownGoodDecide: eligibility ---

func TestLastKnownGoodDecideProtectsNewestEligibleArtifact(t *testing.T) {
	set := gfsMustSet(t, "lkg", "basic")
	specs := []gfsRecSpec{
		{"oldest", lifecycle.Complete, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"middle", lifecycle.Committed, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
		{"newest", lifecycle.RemoteDeletePending, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
	}
	records := gfsBuildRecords(t, set, specs)
	cfg := config.Retention{ProtectLastKnownGood: lkgBoolPtr(true)}

	got, err := LastKnownGoodDecide(cfg, set, records)
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	if !got.Enabled {
		t.Fatalf("Enabled = false, want true")
	}
	if !got.Protected {
		t.Fatalf("Protected = false, want true: %+v", got)
	}
	if got.Artifact.Name != "newest" {
		t.Errorf("Artifact = %q, want %q", got.Artifact.Name, "newest")
	}
	if got.Reason == "" {
		t.Errorf("Reason is empty, want an explanation")
	}
}

// TestLastKnownGoodFallsBackPastQuarantinedNewest is the headline trap this
// issue calls out: a set whose only recent artifact is QUARANTINED must
// protect an older genuinely-good artifact instead, never the quarantined
// one and never nothing at all.
func TestLastKnownGoodFallsBackPastQuarantinedNewest(t *testing.T) {
	set := gfsMustSet(t, "lkg", "trap")
	specs := []gfsRecSpec{
		{"old-but-good", lifecycle.Complete, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"newest-but-quarantined", lifecycle.Quarantined, time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)},
	}
	records := gfsBuildRecords(t, set, specs)
	cfg := config.Retention{} // ProtectLastKnownGood left nil: must still default to protecting

	got, err := LastKnownGoodDecide(cfg, set, records)
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	if !got.Protected {
		t.Fatalf("Protected = false, want true: an older good artifact exists to fall back to (%+v)", got)
	}
	if got.Artifact.Name != "old-but-good" {
		t.Errorf("Artifact = %q, want %q (the quarantined artifact must never be selected)", got.Artifact.Name, "old-but-good")
	}
}

// TestLastKnownGoodEligibilityMatchesGFSManagedComplete pins
// LastKnownGoodDecide's eligibility to gfsIsManagedComplete, the exact same
// Committed/RemoteDeletePending/Complete set gfs.go already uses (and, by
// construction, the same set internal/health's decideState calls knownGood
// for FR-24 -- see this file's package doc). Walking every lifecycle state
// one at a time is a complete proof, not a sample.
func TestLastKnownGoodEligibilityMatchesGFSManagedComplete(t *testing.T) {
	for _, st := range lifecycle.AllStates {
		st := st
		t.Run(string(st), func(t *testing.T) {
			set := gfsMustSet(t, "lkg-elig", string(st))
			records := gfsBuildRecords(t, set, []gfsRecSpec{
				{"only", st, time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)},
			})
			cfg := config.Retention{ProtectLastKnownGood: lkgBoolPtr(true)}

			got, err := LastKnownGoodDecide(cfg, set, records)
			if err != nil {
				t.Fatalf("LastKnownGoodDecide: %v", err)
			}
			want := gfsIsManagedComplete(string(st))
			if got.Protected != want {
				t.Errorf("Protected = %v, want %v (gfsIsManagedComplete(%q) = %v)", got.Protected, want, st, want)
			}
		})
	}
}

func TestLastKnownGoodDecideExcludesEveryDisqualifiedStateEvenWhenNewest(t *testing.T) {
	set := gfsMustSet(t, "lkg-disq", "set")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	var specs []gfsRecSpec
	for i, st := range lifecycle.AllStates {
		specs = append(specs, gfsRecSpec{
			name:       "artifact-" + string(st),
			state:      st,
			discovered: base.Add(time.Duration(i) * time.Hour),
		})
	}
	records := gfsBuildRecords(t, set, specs)
	cfg := config.Retention{ProtectLastKnownGood: lkgBoolPtr(true)}

	got, err := LastKnownGoodDecide(cfg, set, records)
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	// lifecycle.AllStates lists Failed, Quarantined, QuarantinedLost last,
	// so each is discovered strictly after Complete (the newest of the
	// three eligible states). If eligibility were bypassed for "the newest
	// arrival regardless of state," one of those three would win instead.
	if !got.Protected {
		t.Fatalf("Protected = false, want true: Committed/RemoteDeletePending/Complete are all present (%+v)", got)
	}
	wantName := "artifact-" + string(lifecycle.Complete)
	if got.Artifact.Name != wantName {
		t.Errorf("Artifact = %q, want %q (the newest *eligible* artifact, not the newest arrival of any kind)", got.Artifact.Name, wantName)
	}
}

func TestLastKnownGoodDecideNothingEligibleMeansNotProtected(t *testing.T) {
	set := gfsMustSet(t, "lkg-none", "set")
	specs := []gfsRecSpec{
		{"failed", lifecycle.Failed, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
		{"quarantined", lifecycle.Quarantined, time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)},
		{"lost", lifecycle.QuarantinedLost, time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)},
		{"partial", lifecycle.Transferring, time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)},
	}
	records := gfsBuildRecords(t, set, specs)
	cfg := config.Retention{ProtectLastKnownGood: lkgBoolPtr(true)}

	got, err := LastKnownGoodDecide(cfg, set, records)
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	if got.Protected {
		t.Fatalf("Protected = true, want false: no artifact here is eligible (%+v)", got)
	}
	if got.Artifact != (model.ArtifactID{}) {
		t.Errorf("Artifact = %+v, want the zero value when nothing is protected", got.Artifact)
	}
	if got.Reason == "" {
		t.Errorf("Reason is empty, want an explanation of why nothing is protected")
	}
}

// --- the config flag ---

func TestLastKnownGoodDecideRespectsExplicitFalse(t *testing.T) {
	set := gfsMustSet(t, "lkg-off", "set")
	records := gfsBuildRecords(t, set, []gfsRecSpec{
		{"good", lifecycle.Complete, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
	})
	cfg := config.Retention{ProtectLastKnownGood: lkgBoolPtr(false)}

	got, err := LastKnownGoodDecide(cfg, set, records)
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	if got.Enabled {
		t.Fatalf("Enabled = true, want false: the operator explicitly disabled protection")
	}
	if got.Protected {
		t.Fatalf("Protected = true, want false: protection is disabled, an eligible artifact must not matter")
	}
	if got.Reason == "" {
		t.Fatalf("Reason is empty: an explicit, more dangerous configuration must be visible, not silent")
	}
}

func TestLastKnownGoodDecideNilPointerDefaultsToEnabled(t *testing.T) {
	set := gfsMustSet(t, "lkg-nil", "set")
	records := gfsBuildRecords(t, set, []gfsRecSpec{
		{"good", lifecycle.Complete, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
	})
	cfg := config.Retention{ProtectLastKnownGood: nil} // caller bypassed config.Validate

	got, err := LastKnownGoodDecide(cfg, set, records)
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	if !got.Enabled {
		t.Fatalf("Enabled = false, want true: an absent flag must default to the safe reading, matching config.Validate's own default")
	}
	if !got.Protected {
		t.Fatalf("Protected = false, want true")
	}
}

func TestLastKnownGoodDecideExplicitTrueIsEnabled(t *testing.T) {
	set := gfsMustSet(t, "lkg-true", "set")
	records := gfsBuildRecords(t, set, []gfsRecSpec{
		{"good", lifecycle.Complete, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
	})
	cfg := config.Retention{ProtectLastKnownGood: lkgBoolPtr(true)}

	got, err := LastKnownGoodDecide(cfg, set, records)
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	if !got.Enabled || !got.Protected {
		t.Fatalf("got %+v, want Enabled and Protected both true", got)
	}
}

// --- structural properties: isolation, determinism ---

func TestLastKnownGoodDecideRejectsRecordFromAnotherBackupSet(t *testing.T) {
	setA := gfsMustSet(t, "lkg-iso", "a")
	setB := gfsMustSet(t, "lkg-iso", "b")
	now := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)

	records := gfsBuildRecords(t, setA, []gfsRecSpec{{"own", lifecycle.Complete, now}})
	foreign := gfsBuildRecords(t, setB, []gfsRecSpec{{"foreign", lifecycle.Complete, now}})
	records = append(records, foreign...)

	cfg := config.Retention{ProtectLastKnownGood: lkgBoolPtr(true)}
	if _, err := LastKnownGoodDecide(cfg, setA, records); err == nil {
		t.Fatal("expected an error for a record belonging to a different backup set, got nil (FR-7 isolation would be silently broken)")
	}
}

func TestLastKnownGoodDecideRejectsZeroBackupSet(t *testing.T) {
	cfg := config.Retention{ProtectLastKnownGood: lkgBoolPtr(true)}
	if _, err := LastKnownGoodDecide(cfg, model.BackupSetID{}, nil); err == nil {
		t.Fatal("expected an error for a zero backup set id, got nil")
	}
}

func TestLastKnownGoodDecideTieBreakIsDeterministic(t *testing.T) {
	set := gfsMustSet(t, "lkg-tie", "set")
	sameInstant := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	specs := []gfsRecSpec{
		{"zzz-later-name", lifecycle.Complete, sameInstant},
		{"aaa-earlier-name", lifecycle.Complete, sameInstant},
	}
	cfg := config.Retention{ProtectLastKnownGood: lkgBoolPtr(true)}

	for _, order := range [][]int{{0, 1}, {1, 0}} {
		ordered := []gfsRecSpec{specs[order[0]], specs[order[1]]}
		records := gfsBuildRecords(t, set, ordered)
		got, err := LastKnownGoodDecide(cfg, set, records)
		if err != nil {
			t.Fatalf("LastKnownGoodDecide: %v", err)
		}
		// Matches gfsIsNewerRepresentative's own tie break: the
		// lexicographically greater name wins, regardless of input order.
		if got.Artifact.Name != "zzz-later-name" {
			t.Errorf("order %v: Artifact = %q, want %q", order, got.Artifact.Name, "zzz-later-name")
		}
	}
}

func TestLastKnownGoodDecideIsOrderIndependent(t *testing.T) {
	set := gfsMustSet(t, "lkg-order", "set")
	specs := []gfsRecSpec{
		{"a", lifecycle.Complete, time.Date(2026, 8, 26, 8, 0, 0, 0, time.UTC)},
		{"b", lifecycle.Complete, time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)},
		{"c", lifecycle.Quarantined, time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)},
		{"d", lifecycle.Committed, time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)},
	}
	cfg := config.Retention{ProtectLastKnownGood: lkgBoolPtr(true)}

	forward := gfsBuildRecords(t, set, specs)
	want, err := LastKnownGoodDecide(cfg, set, forward)
	if err != nil {
		t.Fatalf("LastKnownGoodDecide (forward order): %v", err)
	}

	reversedSpecs := make([]gfsRecSpec, len(specs))
	for i, s := range specs {
		reversedSpecs[len(specs)-1-i] = s
	}
	reversed := gfsBuildRecords(t, set, reversedSpecs)
	got, err := LastKnownGoodDecide(cfg, set, reversed)
	if err != nil {
		t.Fatalf("LastKnownGoodDecide (reversed order): %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("LastKnownGoodDecide is not order-independent:\n forward = %+v\n reversed = %+v", want, got)
	}
}

// --- ApplyLastKnownGood / DecideKeep: composition with GFS ---

func TestApplyLastKnownGoodKeepsArtifactOutsideEveryGFSTier(t *testing.T) {
	set := gfsMustSet(t, "compose", "outside")
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	// Far outside every GFS window below (daily_days: 3), so GFSDecide alone
	// drops it, but it is the only eligible artifact in the set.
	records := gfsBuildRecords(t, set, []gfsRecSpec{
		{"ancient-but-good", lifecycle.Complete, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
	})
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 3, ProtectLastKnownGood: lkgBoolPtr(true)}

	gfsVerdicts, err := GFSDecide(now, cfg, set, records)
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}
	if len(gfsVerdicts) != 1 || gfsVerdicts[0].Keep {
		t.Fatalf("test setup invalid: expected GFSDecide alone to drop the artifact, got %+v", gfsVerdicts)
	}

	lkg, err := LastKnownGoodDecide(cfg, set, records)
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	if !lkg.Protected {
		t.Fatalf("test setup invalid: expected the artifact to be last-known-good, got %+v", lkg)
	}

	composed := ApplyLastKnownGood(gfsVerdicts, lkg)
	if len(composed) != 1 {
		t.Fatalf("composed = %+v, want exactly one verdict", composed)
	}
	v := composed[0]
	if !v.Keep {
		t.Fatalf("Keep = false, want true: an artifact outside every GFS tier but holding last-known-good title must be kept (%+v)", v)
	}
	want := []GFSTier{TierLastKnownGood}
	if !reflect.DeepEqual(v.TierNames(), want) {
		t.Errorf("Tiers = %v, want %v: the kept reason must be visible on the verdict, not merely inferred", v.Tiers, want)
	}

	// The original GFSDecide output must not have been mutated in place.
	if gfsVerdicts[0].Keep {
		t.Errorf("ApplyLastKnownGood mutated its input slice's Keep field in place")
	}
	if len(gfsVerdicts[0].Tiers) != 0 {
		t.Errorf("ApplyLastKnownGood mutated its input slice's Tiers field in place: %v", gfsVerdicts[0].Tiers)
	}
}

func TestApplyLastKnownGoodAddsTierAlongsideExistingGFSTiers(t *testing.T) {
	set := gfsMustSet(t, "compose", "alongside")
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	records := gfsBuildRecords(t, set, []gfsRecSpec{
		{"today", lifecycle.Complete, now},
	})
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 1, WeeklyMonths: 1, MonthlyMonths: 1, ProtectLastKnownGood: lkgBoolPtr(true)}

	gfsVerdicts, err := GFSDecide(now, cfg, set, records)
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}
	lkg, err := LastKnownGoodDecide(cfg, set, records)
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	if !lkg.Protected {
		t.Fatalf("test setup invalid: expected protection, got %+v", lkg)
	}

	composed := ApplyLastKnownGood(gfsVerdicts, lkg)
	if len(composed) != 1 {
		t.Fatalf("composed = %+v, want exactly one verdict", composed)
	}
	v := composed[0]
	if !v.Keep {
		t.Fatalf("Keep = false, want true")
	}
	want := []GFSTier{GFSDaily, GFSWeekly, GFSMonthly, TierLastKnownGood}
	if !reflect.DeepEqual(v.TierNames(), want) {
		t.Errorf("Tiers = %v, want %v (GFS tiers first, TierLastKnownGood appended last, no duplicate)", v.Tiers, want)
	}
}

func TestApplyLastKnownGoodNoOpWhenNotProtected(t *testing.T) {
	set := gfsMustSet(t, "compose", "noop")
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	records := gfsBuildRecords(t, set, []gfsRecSpec{
		{"today", lifecycle.Complete, now},
	})
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 1, ProtectLastKnownGood: lkgBoolPtr(false)}

	gfsVerdicts, err := GFSDecide(now, cfg, set, records)
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}
	lkg, err := LastKnownGoodDecide(cfg, set, records)
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	if lkg.Protected {
		t.Fatalf("test setup invalid: protection is disabled, expected Protected = false, got %+v", lkg)
	}

	composed := ApplyLastKnownGood(gfsVerdicts, lkg)
	if !reflect.DeepEqual(composed, gfsVerdicts) {
		t.Errorf("ApplyLastKnownGood changed the verdicts when nothing is protected:\n got  = %+v\n want = %+v", composed, gfsVerdicts)
	}
}

func TestApplyLastKnownGoodFallsBackToAppendWhenArtifactMissingFromVerdicts(t *testing.T) {
	// Defensive path: verdicts that simply don't contain the protected
	// artifact (mismatched inputs). ApplyLastKnownGood must still surface
	// the protection rather than silently dropping it, and must preserve
	// GFSVerdict's documented by-name sort order.
	set := gfsMustSet(t, "compose", "missing")
	missing := gfsMustArtifact(t, set, "zzz-not-in-verdicts")
	existing := gfsMustArtifact(t, set, "aaa-existing")

	verdicts := []GFSVerdict{{Artifact: existing, Keep: true, Tiers: []GFSTierSelection{{Tier: GFSDaily, By: GFSSelectedByDiscovery}}}}
	lkg := LastKnownGoodResult{Set: set, Enabled: true, Protected: true, Artifact: missing}

	composed := ApplyLastKnownGood(verdicts, lkg)
	if len(composed) != 2 {
		t.Fatalf("composed = %+v, want 2 verdicts", composed)
	}
	if composed[0].Artifact.Name != "aaa-existing" || composed[1].Artifact.Name != "zzz-not-in-verdicts" {
		t.Errorf("composed is not sorted by artifact name: %+v", composed)
	}
	if !composed[1].Keep {
		t.Errorf("the appended defensive verdict must have Keep = true")
	}
}

func TestDecideKeepComposesGFSAndLastKnownGoodEndToEnd(t *testing.T) {
	set := gfsMustSet(t, "decidekeep", "trap")
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	records := gfsBuildRecords(t, set, []gfsRecSpec{
		{"ancient-good", lifecycle.Complete, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"newest-quarantined", lifecycle.Quarantined, now},
	})
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 3, WeeklyMonths: 1, MonthlyMonths: 1, ProtectLastKnownGood: lkgBoolPtr(true)}

	verdicts, lkg, err := DecideKeep(now, cfg, set, records)
	if err != nil {
		t.Fatalf("DecideKeep: %v", err)
	}
	if !lkg.Protected || lkg.Artifact.Name != "ancient-good" {
		t.Fatalf("LastKnownGoodResult = %+v, want Protected with ancient-good", lkg)
	}
	if len(verdicts) != 1 {
		t.Fatalf("verdicts = %+v, want exactly one (the quarantined artifact is not managed-complete and never appears)", verdicts)
	}
	v := verdicts[0]
	if v.Artifact.Name != "ancient-good" {
		t.Fatalf("verdicts[0].Artifact = %q, want %q", v.Artifact.Name, "ancient-good")
	}
	if !v.Keep {
		t.Errorf("Keep = false, want true: last-known-good protection must keep it despite being outside every GFS window")
	}
	if !reflect.DeepEqual(v.TierNames(), []GFSTier{TierLastKnownGood}) {
		t.Errorf("Tiers = %v, want [%v]", v.Tiers, TierLastKnownGood)
	}
}

func TestDecideKeepReflectsDisabledProtectionEndToEnd(t *testing.T) {
	set := gfsMustSet(t, "decidekeep", "disabled")
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	records := gfsBuildRecords(t, set, []gfsRecSpec{
		{"ancient-good", lifecycle.Complete, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
	})
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 3, ProtectLastKnownGood: lkgBoolPtr(false)}

	verdicts, lkg, err := DecideKeep(now, cfg, set, records)
	if err != nil {
		t.Fatalf("DecideKeep: %v", err)
	}
	if lkg.Enabled {
		t.Fatalf("Enabled = true, want false")
	}
	if lkg.Protected {
		t.Fatalf("Protected = true, want false")
	}
	if len(verdicts) != 1 || verdicts[0].Keep {
		t.Errorf("verdicts = %+v, want the sole artifact left un-kept: with protection explicitly off, the operator gets what they asked for", verdicts)
	}
}
