package retention

import (
	"reflect"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// goldenRecords is the golden fixture's record set, extracted so a second
// test can run the IDENTICAL records rather than a copy of them that
// could drift. issue #239's medium-invariance test is that second reader:
// "identical inputs" has to mean the same inputs, not the same intent.
//
// It exercises all three GFS tiers, the "not kept by anything" case,
// last-known-good landing on an artifact a tier already kept, and the two
// states that must never appear in a verdict at all.
func goldenRecords(t *testing.T, set model.BackupSetID) []state.Record {
	t.Helper()
	return gfsBuildRecords(t, set, []gfsRecSpec{
		// Outside every tier's window (before the 12-month monthly cutoff
		// of 2025-09-01): a genuine "not kept by anything" case.
		{"too-old-everything", lifecycle.Complete, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		// Inside the monthly window, outside the weekly window (before
		// its 2026-06-01 cutoff), alone in its calendar month: monthly
		// tier only.
		{"monthly-only", lifecycle.Committed, time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)},
		// Inside the weekly window, outside the daily window, alone in
		// its Monday-start week bucket (2026-08-10..16): weekly tier
		// only (August's monthly bucket is won by the newer
		// "recent-daily" record below).
		{"week-old-in-weekly", lifecycle.RemoteDeletePending, time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)},
		// Inside the daily window (2026-08-23..29), and also the newest
		// eligible record in both its week bucket (2026-08-24..30) and
		// its calendar month (August 2026): daily + weekly + monthly, and
		// (being the newest eligible record overall) last-known-good on
		// top of that, so this one artifact proves LKG composes an extra
		// tier onto an already-kept verdict rather than only ever adding
		// a fresh one.
		{"recent-daily", lifecycle.Complete, time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)},
		// The "quarantined-newest" trap (see lastknowngood.go's package
		// doc): the newest arrival by far, but QUARANTINED, so it must
		// never appear in the output at all, must never be a GFS tier
		// representative, and must never be mistaken for the
		// last-known-good artifact.
		{"quarantined-newest", lifecycle.Quarantined, time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)},
		// FAILED: also never a completed backup, also absent entirely.
		{"failed-record", lifecycle.Failed, time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)},
	})
}

// TestDecideKeepGoldenBaselineForOmittedRetentionBlock is issue #111's
// (B3.6) mandatory regression-safety test: "an operator who upgrades and
// changes nothing (no new CLI flag, no UI edit) has to get byte-identical
// GFSDecide/DecideKeep verdicts to what they get today."
//
// The fixture below stands in for "a config file with the retention block
// fully omitted": a zero-valued config.Retention run through the exact
// same config.ValidateRetention path config.Validate itself uses, so the
// resolved policy this test feeds to DecideKeep (UTC, monday, 7/3/12,
// protect=true) is derived from the real defaulting logic, not
// hand-duplicated here as a second, potentially stale, copy of it. If a
// later change to either this package or internal/config's defaults ever
// shifts what an omitted retention block resolves to, or how DecideKeep
// classifies a fixed, representative set of records under it, this test
// fails: that is its entire job.
//
// The fixture is deliberately built to exercise all three GFS tiers, the
// "not kept by anything" case, and last-known-good protection landing on
// an artifact a GFS tier already kept (composing an extra tier onto an
// existing verdict, not just adding a fresh one) in one pass, so this one
// test stands in for "the whole DecideKeep pipeline as an operator
// actually experiences it today," not just one isolated rule.
func TestDecideKeepGoldenBaselineForOmittedRetentionBlock(t *testing.T) {
	var resolved config.Retention // zero value: as if the retention: block were absent from the YAML entirely
	if err := config.ValidateRetention(&resolved); err != nil {
		t.Fatalf("config.ValidateRetention on an omitted retention block: %v", err)
	}

	// Pin the resolved defaults themselves as part of the baseline: if
	// these ever drift, the "today's behavior stays the default" promise
	// is already broken before DecideKeep is even reached.
	wantResolved := config.Retention{
		Timezone:             "UTC",
		WeekStartsOn:         "monday",
		DailyDays:            7,
		WeeklyMonths:         3,
		MonthlyMonths:        12,
		ProtectLastKnownGood: lkgBoolPtr(true),
	}
	if resolved.Timezone != wantResolved.Timezone ||
		resolved.WeekStartsOn != wantResolved.WeekStartsOn ||
		resolved.DailyDays != wantResolved.DailyDays ||
		resolved.WeeklyMonths != wantResolved.WeeklyMonths ||
		resolved.MonthlyMonths != wantResolved.MonthlyMonths ||
		resolved.ProtectLastKnownGood == nil || *resolved.ProtectLastKnownGood != true {
		t.Fatalf("an omitted retention block resolved to %+v (ProtectLastKnownGood=%v), want 7/3/12 UTC/monday/protect=true",
			resolved, resolved.ProtectLastKnownGood)
	}

	set := gfsMustSet(t, "golden", "baseline")
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC) // fixed instant; see package doc's Determinism section

	records := goldenRecords(t, set)

	verdicts, lkg, err := DecideKeep(now, resolved, set, records)
	if err != nil {
		t.Fatalf("DecideKeep: %v", err)
	}

	type wantVerdict struct {
		keep  bool
		tiers []GFSTier
	}
	want := map[string]wantVerdict{
		"too-old-everything": {keep: false, tiers: nil},
		"monthly-only":       {keep: true, tiers: []GFSTier{GFSMonthly}},
		"week-old-in-weekly": {keep: true, tiers: []GFSTier{GFSWeekly}},
		"recent-daily":       {keep: true, tiers: []GFSTier{GFSDaily, GFSWeekly, GFSMonthly, TierLastKnownGood}},
	}

	if len(verdicts) != len(want) {
		t.Fatalf("DecideKeep returned %d verdict(s), want %d (quarantined-newest and failed-record must never appear): %+v", len(verdicts), len(want), verdicts)
	}
	for _, v := range verdicts {
		w, ok := want[v.Artifact.Name]
		if !ok {
			t.Fatalf("unexpected artifact %q in verdicts: %+v", v.Artifact.Name, verdicts)
		}
		if v.Keep != w.keep {
			t.Errorf("%s: Keep = %v, want %v", v.Artifact.Name, v.Keep, w.keep)
		}
		if !reflect.DeepEqual(v.tierNames(), w.tiers) {
			t.Errorf("%s: Tiers = %v, want %v", v.Artifact.Name, v.Tiers, w.tiers)
		}
	}

	if !lkg.Enabled {
		t.Error("LastKnownGoodResult.Enabled = false, want true (protect_last_known_good defaults to true)")
	}
	if !lkg.Protected {
		t.Fatalf("LastKnownGoodResult.Protected = false, want true: %+v", lkg)
	}
	if lkg.Artifact.Name != "recent-daily" {
		t.Errorf("LastKnownGoodResult.Artifact = %q, want %q", lkg.Artifact.Name, "recent-daily")
	}
}
