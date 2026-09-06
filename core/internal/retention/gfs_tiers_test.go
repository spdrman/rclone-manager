package retention

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// Tests for issue #156 (B3.8): FR-18's retention policy is an ordered
// chain of any number of named tiers, not a fixed daily/weekly/monthly
// triple.
//
// Every "this artifact is a delete candidate" assertion in this file is
// paired with a positive control: the same artifact, the same records and
// the same instant, run through a chain that does keep it. A gap test that
// can only ever report "not kept" proves nothing about the gap, only that
// the fixture is unreachable, so each one here is required to flip.

// gfsChainRecords is the shared fixture builder for these tests: the same
// gfsRecSpec shape gfs_test.go already uses, all rows Complete.
func gfsChainRecords(t *testing.T, set model.BackupSetID, rows map[string]time.Time) []state.Record {
	t.Helper()
	specs := make([]gfsRecSpec, 0, len(rows))
	for name, at := range rows {
		specs = append(specs, gfsRecSpec{name: name, state: lifecycle.Complete, discovered: at})
	}
	return gfsBuildRecords(t, set, specs)
}

// gfsTierChainCfg builds a policy that uses only the explicit chain: the
// three legacy scalars stay zero, which config.Validate requires whenever
// tiers is set (see config.Retention.Tiers' doc).
func gfsTierChainCfg(tz, weekStart string, tiers ...config.RetentionTier) config.Retention {
	return config.Retention{Timezone: tz, WeekStartsOn: weekStart, Tiers: tiers}
}

// gfsAssertTiers checks the exact tier list recorded against one artifact,
// including its order: FR-18 fixes the order at the order the operator
// wrote the chain in, so a reordering is a contract break, not cosmetic.
func gfsAssertTiers(t *testing.T, verdicts []GFSVerdict, name string, want []GFSTier) {
	t.Helper()
	for _, v := range verdicts {
		if v.Artifact.Name != name {
			continue
		}
		wantKeep := len(want) > 0
		if v.Keep != wantKeep {
			t.Errorf("%s: Keep = %v, want %v (tiers %v)", name, v.Keep, wantKeep, v.Tiers)
		}
		if !reflect.DeepEqual(v.tierNames(), want) {
			t.Errorf("%s: Tiers = %v, want %v", name, v.Tiers, want)
		}
		return
	}
	t.Errorf("no verdict returned for %q (verdicts: %+v)", name, verdicts)
}

// --- RED: a five-tier chain unions all five ---

// TestGFSDecideUnionsFivePlusChainedTiers is the issue's headline case: a
// daily/weekly/monthly/semi-annual/annual chain, with one artifact claimed
// by all five at once and one artifact claimed by each tier alone, so the
// union is proven per tier rather than only in aggregate.
func TestGFSDecideUnionsFivePlusChainedTiers(t *testing.T) {
	set := gfsMustSet(t, "chain", "five")
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC) // a Saturday

	cfg := gfsTierChainCfg("UTC", "monday",
		config.RetentionTier{Name: "daily", Granularity: config.GranularityDay, Keep: 7},
		config.RetentionTier{Name: "weekly", Granularity: config.GranularityWeek, Keep: 3, WindowUnit: config.GranularityMonth},
		config.RetentionTier{Name: "monthly", Granularity: config.GranularityMonth, Keep: 12},
		config.RetentionTier{Name: "semi_annual", Granularity: config.GranularityHalfYear, Keep: 6},
		config.RetentionTier{Name: "annual", Granularity: config.GranularityYear, Keep: 10},
	)

	// Windows for now = 2026-08-29:
	//   daily       2026-08-23 .. 2026-08-29
	//   weekly      2026-06-01 .. 2026-08-29 (3 calendar months, week buckets)
	//   monthly     2025-09-01 .. 2026-08-29
	//   semi_annual 2024-01-01 .. 2026-08-29 (6 calendar half-years)
	//   annual      2017-01-01 .. 2026-08-29
	records := gfsChainRecords(t, set, map[string]time.Time{
		"newest":       time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC),
		"daily-only":   time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC),
		"weekly-only":  time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC),
		"monthly-only": time.Date(2026, 4, 15, 9, 0, 0, 0, time.UTC),
		"may-and-h1":   time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC),
		"semi-only":    time.Date(2025, 3, 10, 9, 0, 0, 0, time.UTC),
		"nov-and-2025": time.Date(2025, 11, 5, 9, 0, 0, 0, time.UTC),
		"annual-only":  time.Date(2023, 6, 15, 9, 0, 0, 0, time.UTC),
		"outside-all":  time.Date(2016, 12, 31, 9, 0, 0, 0, time.UTC),
	})

	got, err := GFSDecide(now, cfg, set, records)
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}

	const (
		daily  GFSTier = "DAILY"
		weekly GFSTier = "WEEKLY"
		month  GFSTier = "MONTHLY"
		semi   GFSTier = "SEMI_ANNUAL"
		annual GFSTier = "ANNUAL"
	)

	// newest wins its day, its week, its month, H2 2026 and 2026: the
	// five-way union on a single artifact.
	gfsAssertTiers(t, got, "newest", []GFSTier{daily, weekly, month, semi, annual})
	gfsAssertTiers(t, got, "daily-only", []GFSTier{daily})
	gfsAssertTiers(t, got, "weekly-only", []GFSTier{weekly})
	gfsAssertTiers(t, got, "monthly-only", []GFSTier{month})
	gfsAssertTiers(t, got, "may-and-h1", []GFSTier{month, semi})
	gfsAssertTiers(t, got, "semi-only", []GFSTier{semi})
	gfsAssertTiers(t, got, "nov-and-2025", []GFSTier{month, semi, annual})
	gfsAssertTiers(t, got, "annual-only", []GFSTier{annual})
	// Older than the longest window: nothing in the chain reaches it.
	gfsAssertTiers(t, got, "outside-all", nil)

	gfsAssertKeptNames(t, got, []string{
		"newest", "daily-only", "weekly-only", "monthly-only",
		"may-and-h1", "semi-only", "nov-and-2025", "annual-only",
	})
}

// --- RED: a deliberately non-contiguous chain deletes what falls in the gap ---

// TestGFSDecideNonContiguousChainDeletesTheGap is Rom's own "everything
// in-between and outside that policy would be deleted" case, exercised
// with daily + annual and nothing between them, so the delete verdicts
// cannot come from tiers happening to blanket every day.
//
// Each of the three sub-cases is a control for the one above it: the same
// records and the same instant, with a chain that reaches the artifact,
// flipping it from delete candidate to kept. Without that pairing a "not
// kept" assertion would also pass on a GFSDecide that kept nothing at all.
func TestGFSDecideNonContiguousChainDeletesTheGap(t *testing.T) {
	set := gfsMustSet(t, "chain", "gap")
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	rows := map[string]time.Time{
		"newest":         time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC),
		"gap-weekly":     time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC),
		"gap-monthly":    time.Date(2025, 10, 8, 9, 0, 0, 0, time.UTC),
		"annual-2025":    time.Date(2025, 12, 20, 9, 0, 0, 0, time.UTC),
		"outside-oldest": time.Date(2020, 1, 1, 9, 0, 0, 0, time.UTC),
	}
	records := gfsChainRecords(t, set, rows)

	daily3 := config.RetentionTier{Name: "daily", Granularity: config.GranularityDay, Keep: 3}
	annual5 := config.RetentionTier{Name: "annual", Granularity: config.GranularityYear, Keep: 5}

	t.Run("gap chain deletes what neither tier reaches", func(t *testing.T) {
		// daily 2026-08-27..29; annual 2022-01-01..2026-08-29.
		// Nothing covers mid-2026 or most of 2025.
		cfg := gfsTierChainCfg("UTC", "monday", daily3, annual5)
		got, err := GFSDecide(now, cfg, set, records)
		if err != nil {
			t.Fatalf("GFSDecide: %v", err)
		}
		gfsAssertKeptNames(t, got, []string{"newest", "annual-2025"})
		gfsAssertTiers(t, got, "gap-weekly", nil)
		gfsAssertTiers(t, got, "gap-monthly", nil)
		gfsAssertTiers(t, got, "outside-oldest", nil)
	})

	t.Run("control: filling the gap with weekly and monthly rescues both", func(t *testing.T) {
		cfg := gfsTierChainCfg("UTC", "monday",
			daily3,
			config.RetentionTier{Name: "weekly", Granularity: config.GranularityWeek, Keep: 3, WindowUnit: config.GranularityMonth},
			config.RetentionTier{Name: "monthly", Granularity: config.GranularityMonth, Keep: 12},
			annual5,
		)
		got, err := GFSDecide(now, cfg, set, records)
		if err != nil {
			t.Fatalf("GFSDecide: %v", err)
		}
		if names := gfsKeptNames(got); len(names) != 4 {
			t.Fatalf("control kept %v, want the four in-window artifacts", names)
		}
		gfsAssertTiers(t, got, "gap-weekly", []GFSTier{"WEEKLY", "MONTHLY"})
		gfsAssertTiers(t, got, "gap-monthly", []GFSTier{"MONTHLY"})
		// Still outside every window, even the filled chain's.
		gfsAssertTiers(t, got, "outside-oldest", nil)
	})

	t.Run("control: widening the longest tier rescues the oldest artifact", func(t *testing.T) {
		cfg := gfsTierChainCfg("UTC", "monday",
			daily3,
			config.RetentionTier{Name: "annual", Granularity: config.GranularityYear, Keep: 10},
		)
		got, err := GFSDecide(now, cfg, set, records)
		if err != nil {
			t.Fatalf("GFSDecide: %v", err)
		}
		gfsAssertTiers(t, got, "outside-oldest", []GFSTier{"ANNUAL"})
	})
}

// --- RED: semi-annual, annual and quarter bucket/window math ---

type gfsChainCase struct {
	name      string
	protects  string
	timezone  string
	weekStart string
	tiers     []config.RetentionTier
	now       time.Time
	records   map[string]time.Time
	wantKeep  []string
}

// TestGFSDecideLongPeriodCalendarCases holds the semi-annual, annual and
// quarter granularities to the same standard FR-18 already demands of
// daily/weekly/monthly: calendar boundaries, DST, leap years and year
// boundaries. Cases are paired so that each one's expectation is
// discriminating: the "half-year splits June from July" case is followed
// by the same records under an annual chain, where they collapse into one
// bucket, so neither expectation can be met by a bucket function that
// simply ignores the granularity.
func TestGFSDecideLongPeriodCalendarCases(t *testing.T) {
	semi := func(keep int) config.RetentionTier {
		return config.RetentionTier{Name: "semi_annual", Granularity: config.GranularityHalfYear, Keep: keep}
	}
	annual := func(keep int) config.RetentionTier {
		return config.RetentionTier{Name: "annual", Granularity: config.GranularityYear, Keep: keep}
	}

	cases := []gfsChainCase{
		{
			name:     "half-year bucket splits June 30 from July 1",
			protects: "a half-year bucket implemented as a year bucket, which would silently drop one of the two backups an operator expects a semi-annual tier to hold",
			timezone: "UTC", weekStart: "monday",
			tiers: []config.RetentionTier{semi(2)},
			now:   time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC),
			records: map[string]time.Time{
				"jun-30": time.Date(2026, 6, 30, 23, 0, 0, 0, time.UTC),
				"jul-01": time.Date(2026, 7, 1, 1, 0, 0, 0, time.UTC),
			},
			wantKeep: []string{"jun-30", "jul-01"},
		},
		{
			name:     "control: the same two dates share one annual bucket",
			protects: "an over-eager half-year split leaking into the annual tier; if this case kept both, the case above would prove nothing about half-years",
			timezone: "UTC", weekStart: "monday",
			tiers: []config.RetentionTier{annual(2)},
			now:   time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC),
			records: map[string]time.Time{
				"jun-30": time.Date(2026, 6, 30, 23, 0, 0, 0, time.UTC),
				"jul-01": time.Date(2026, 7, 1, 1, 0, 0, 0, time.UTC),
			},
			wantKeep: []string{"jul-01"},
		},
		{
			name:     "annual bucket splits Dec 31 from Jan 1 and groups the whole year",
			protects: "a year bucket anchored on a rolling 365 days rather than on January 1, which would let a late-December and an early-January backup share a bucket and delete one of them",
			timezone: "UTC", weekStart: "monday",
			tiers: []config.RetentionTier{annual(3)},
			now:   time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC),
			records: map[string]time.Time{
				"y2025-jan-02": time.Date(2025, 1, 2, 9, 0, 0, 0, time.UTC),
				"y2025-dec-31": time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC),
				"y2026-jan-01": time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC),
			},
			wantKeep: []string{"y2025-dec-31", "y2026-jan-01"},
		},
		{
			name:     "leap day falls in the first half-year and does not move the anchor",
			protects: "half-year arithmetic that steps by 182 or 183 days instead of six calendar months, which drifts by a day every leap year and eventually moves a window edge past a real backup",
			timezone: "UTC", weekStart: "monday",
			tiers: []config.RetentionTier{semi(1)},
			now:   time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC),
			records: map[string]time.Time{
				"dec-2023": time.Date(2023, 12, 31, 9, 0, 0, 0, time.UTC),
				"jan-2024": time.Date(2024, 1, 15, 9, 0, 0, 0, time.UTC),
				"leap-day": time.Date(2024, 2, 29, 9, 0, 0, 0, time.UTC),
			},
			wantKeep: []string{"leap-day"},
		},
		{
			name:     "annual window computed on a leap day includes its exact lower edge",
			protects: "an off-by-one or duration-based year step that would place the window edge on December 31 or January 2 and either keep a backup the policy excludes or delete one it covers",
			timezone: "UTC", weekStart: "monday",
			tiers: []config.RetentionTier{annual(5)},
			now:   time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC),
			records: map[string]time.Time{
				"y2019-dec-31": time.Date(2019, 12, 31, 9, 0, 0, 0, time.UTC),
				"y2020-jan-01": time.Date(2020, 1, 1, 9, 0, 0, 0, time.UTC),
			},
			wantKeep: []string{"y2020-jan-01"},
		},
		{
			name:     "annual bucket uses the configured timezone's calendar year, not UTC's",
			protects: "reading a New Year instant in UTC instead of the retention timezone, which moves a backup into the wrong year bucket and hands that year's slot to a different artifact",
			timezone: "America/Vancouver", weekStart: "monday",
			tiers: []config.RetentionTier{annual(2)},
			now:   time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
			records: map[string]time.Time{
				// 05:00Z on Jan 1 is 21:00 on Dec 31 in Vancouver: this is
				// the 2025 bucket's newest artifact there, and would be the
				// 2026 bucket's loser if the zone were ignored.
				"nye-2026-utc": time.Date(2026, 1, 1, 5, 0, 0, 0, time.UTC),
				"early-2025":   time.Date(2025, 3, 5, 12, 0, 0, 0, time.UTC),
				"mid-2026":     time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
			},
			wantKeep: []string{"mid-2026", "nye-2026-utc"},
		},
		{
			name:     "half-year window's lower edge holds on a DST transition day",
			protects: "a window computed with 24-hour durations across a spring-forward day, which shifts the edge by an hour and pulls in a backup that belongs to the previous half-year",
			timezone: "America/Vancouver", weekStart: "monday",
			tiers: []config.RetentionTier{semi(1)},
			// 20:00Z on the 2026 spring-forward day is 13:00 PDT that same day.
			now: time.Date(2026, 3, 8, 20, 0, 0, 0, time.UTC),
			records: map[string]time.Time{
				// 21:00 Dec 31 Vancouver: before the 2026-01-01 window edge.
				"just-before-window": time.Date(2026, 1, 1, 5, 0, 0, 0, time.UTC),
				// 03:30 PDT on the transition day itself.
				"on-dst-day": time.Date(2026, 3, 8, 10, 30, 0, 0, time.UTC),
			},
			wantKeep: []string{"on-dst-day"},
		},
		{
			name:     "quarter buckets follow the calendar quarters",
			protects: "a quarter implemented as a rolling 90 days, which would let two backups from different calendar quarters collide in one bucket",
			timezone: "UTC", weekStart: "monday",
			tiers: []config.RetentionTier{{Name: "quarterly", Granularity: config.GranularityQuarter, Keep: 4}},
			now:   time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC),
			records: map[string]time.Time{
				"q3-2025": time.Date(2025, 9, 30, 9, 0, 0, 0, time.UTC),
				"q4-2025": time.Date(2025, 11, 10, 9, 0, 0, 0, time.UTC),
				"q1-2026": time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC),
				"q2-2026": time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC),
				"q3-2026": time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC),
			},
			wantKeep: []string{"q4-2025", "q1-2026", "q2-2026", "q3-2026"},
		},
		{
			name:     "a month-end 'today' does not overflow the half-year window",
			protects: "the classic day-of-month overflow (August 31 minus six months resolving into March), which narrows a semi-annual window by a whole period",
			timezone: "UTC", weekStart: "monday",
			tiers: []config.RetentionTier{semi(3)},
			now:   time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC),
			records: map[string]time.Time{
				"just-out": time.Date(2025, 6, 30, 9, 0, 0, 0, time.UTC),
				"on-edge":  time.Date(2025, 7, 1, 9, 0, 0, 0, time.UTC),
			},
			wantKeep: []string{"on-edge"},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.protects == "" {
				t.Fatalf("test case %q has no protects note", tc.name)
			}
			t.Logf("protects against: %s", tc.protects)
			set := gfsMustSet(t, "chaincal", fmt.Sprintf("case%d", i))
			records := gfsChainRecords(t, set, tc.records)
			cfg := gfsTierChainCfg(tc.timezone, tc.weekStart, tc.tiers...)
			got, err := GFSDecide(tc.now, cfg, set, records)
			if err != nil {
				t.Fatalf("GFSDecide: %v", err)
			}
			gfsAssertKeptNames(t, got, tc.wantKeep)
		})
	}
}

// --- RED: the custom "every N days" granularity ---

// TestGFSDecideCustomPeriodTier covers FR-18's escape hatch for periods
// the named granularities do not cover. Its buckets are anchored to a
// fixed epoch rather than to today, so the window does not slide a day at
// a time as the calculation is re-run: the second sub-case is the control
// that proves it, and the third proves the assertions still move when the
// instant crosses a real bucket boundary.
func TestGFSDecideCustomPeriodTier(t *testing.T) {
	set := gfsMustSet(t, "chain", "custom")
	cfg := gfsTierChainCfg("UTC", "monday",
		config.RetentionTier{Name: "fortnightly", Granularity: config.GranularityDays, PeriodDays: 14, Keep: 3},
	)

	// 14-day buckets anchored on 1970-01-01 put these boundaries in play:
	//   2026-07-30 .. 2026-08-12
	//   2026-08-13 .. 2026-08-26
	//   2026-08-27 .. 2026-09-09
	records := gfsChainRecords(t, set, map[string]time.Time{
		"aug-29": time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC),
		"aug-28": time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC),
		"aug-20": time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC),
		"aug-15": time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC),
		"aug-01": time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC),
		"jul-30": time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC),
		"jul-29": time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC),
	})

	t.Run("newest per 14-day bucket, three buckets back", func(t *testing.T) {
		got, err := GFSDecide(time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC), cfg, set, records)
		if err != nil {
			t.Fatalf("GFSDecide: %v", err)
		}
		gfsAssertKeptNames(t, got, []string{"aug-29", "aug-20", "aug-01"})
		gfsAssertTiers(t, got, "aug-28", nil) // same bucket as aug-29, older
		gfsAssertTiers(t, got, "jul-30", nil) // same bucket as aug-01, older
		gfsAssertTiers(t, got, "jul-29", nil) // one bucket too far back
	})

	t.Run("control: a later instant inside the same bucket decides identically", func(t *testing.T) {
		got, err := GFSDecide(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC), cfg, set, records)
		if err != nil {
			t.Fatalf("GFSDecide: %v", err)
		}
		gfsAssertKeptNames(t, got, []string{"aug-29", "aug-20", "aug-01"})
	})

	t.Run("control: crossing a bucket boundary does move the window", func(t *testing.T) {
		got, err := GFSDecide(time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC), cfg, set, records)
		if err != nil {
			t.Fatalf("GFSDecide: %v", err)
		}
		gfsAssertKeptNames(t, got, []string{"aug-29", "aug-20"})
	})
}

// --- REGRESSION: the three legacy scalars decide exactly as they always have ---

// TestGFSDecideLegacyScalarsMatchTheirExplicitChain pins issue #156's
// backward-compatibility promise from both ends at once. It asserts that
//
//  1. the three legacy scalars and the explicit chain they are sugar for
//     produce identical verdicts, tier for tier and in the same order, and
//  2. that shared result is the specific set of decisions the scalars
//     produced before the chain existed,
//
// so the test still fails if both spellings drift together. The final
// sub-case is the control: a chain that differs from the scalars in one
// field flips a verdict, proving the equality above is discriminating and
// that window_unit: month is load-bearing rather than decoration.
func TestGFSDecideLegacyScalarsMatchTheirExplicitChain(t *testing.T) {
	set := gfsMustSet(t, "chain", "compat")
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	records := gfsChainRecords(t, set, map[string]time.Time{
		"too-old":      time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC),
		"monthly-only": time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC),
		"mid-june":     time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC),
		"late-june":    time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC),
		"week-old":     time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC),
		"recent-daily": time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC),
	})

	legacy := config.Retention{
		Timezone: "UTC", WeekStartsOn: "monday",
		DailyDays: 7, WeeklyMonths: 3, MonthlyMonths: 12,
	}
	explicit := gfsTierChainCfg("UTC", "monday",
		config.RetentionTier{Name: "daily", Granularity: config.GranularityDay, Keep: 7},
		config.RetentionTier{Name: "weekly", Granularity: config.GranularityWeek, Keep: 3, WindowUnit: config.GranularityMonth},
		config.RetentionTier{Name: "monthly", Granularity: config.GranularityMonth, Keep: 12},
	)

	fromLegacy, err := GFSDecide(now, legacy, set, records)
	if err != nil {
		t.Fatalf("GFSDecide (legacy scalars): %v", err)
	}
	fromExplicit, err := GFSDecide(now, explicit, set, records)
	if err != nil {
		t.Fatalf("GFSDecide (explicit chain): %v", err)
	}

	if !reflect.DeepEqual(fromLegacy, fromExplicit) {
		t.Errorf("legacy scalars and their explicit chain disagree:\n legacy   = %+v\n explicit = %+v", fromLegacy, fromExplicit)
	}

	// The absolute baseline, so a drift that moved both spellings the same
	// way is still caught.
	for _, got := range [][]GFSVerdict{fromLegacy, fromExplicit} {
		gfsAssertTiers(t, got, "too-old", nil)
		gfsAssertTiers(t, got, "monthly-only", []GFSTier{GFSMonthly})
		gfsAssertTiers(t, got, "mid-june", []GFSTier{GFSWeekly})
		gfsAssertTiers(t, got, "late-june", []GFSTier{GFSWeekly, GFSMonthly})
		gfsAssertTiers(t, got, "week-old", []GFSTier{GFSWeekly})
		gfsAssertTiers(t, got, "recent-daily", []GFSTier{GFSDaily, GFSWeekly, GFSMonthly})
	}

	t.Run("control: dropping window_unit changes the decisions", func(t *testing.T) {
		// weekly's look-back becomes 3 weeks instead of 3 calendar months.
		differs := gfsTierChainCfg("UTC", "monday",
			config.RetentionTier{Name: "daily", Granularity: config.GranularityDay, Keep: 7},
			config.RetentionTier{Name: "weekly", Granularity: config.GranularityWeek, Keep: 3},
			config.RetentionTier{Name: "monthly", Granularity: config.GranularityMonth, Keep: 12},
		)
		got, err := GFSDecide(now, differs, set, records)
		if err != nil {
			t.Fatalf("GFSDecide (differing chain): %v", err)
		}
		if reflect.DeepEqual(fromLegacy, got) {
			t.Fatal("a chain with weekly measured in weeks decided identically to one measured in calendar months; the comparison above cannot detect a real regression")
		}
		gfsAssertTiers(t, got, "mid-june", nil)
		gfsAssertTiers(t, got, "late-june", []GFSTier{GFSMonthly})
	})
}

// TestGFSDecideLegacyTierNamesStayOnTheWire pins the wire contract
// apps/common/webhost sends to the client: a tier named daily, weekly or
// monthly resolves to exactly DAILY, WEEKLY or MONTHLY, whether it came
// from the legacy scalars or from an explicit chain. Renaming any of the
// three is a breaking API change, not an implementation detail.
func TestGFSDecideLegacyTierNamesStayOnTheWire(t *testing.T) {
	set := gfsMustSet(t, "chain", "wire")
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	records := gfsChainRecords(t, set, map[string]time.Time{
		"only": time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC),
	})

	cfg := gfsTierChainCfg("UTC", "monday",
		config.RetentionTier{Name: "daily", Granularity: config.GranularityDay, Keep: 7},
		config.RetentionTier{Name: "weekly", Granularity: config.GranularityWeek, Keep: 3, WindowUnit: config.GranularityMonth},
		config.RetentionTier{Name: "monthly", Granularity: config.GranularityMonth, Keep: 12},
		config.RetentionTier{Name: "semi_annual", Granularity: config.GranularityHalfYear, Keep: 6},
	)
	got, err := GFSDecide(now, cfg, set, records)
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}
	gfsAssertTiers(t, got, "only", []GFSTier{"DAILY", "WEEKLY", "MONTHLY", "SEMI_ANNUAL"})
	if GFSDaily != "DAILY" || GFSWeekly != "WEEKLY" || GFSMonthly != "MONTHLY" {
		t.Fatalf("the three legacy tier constants must stay DAILY/WEEKLY/MONTHLY, got %q/%q/%q", GFSDaily, GFSWeekly, GFSMonthly)
	}
}

// TestGFSDecideRejectsAnUnusableTier keeps a malformed chain from being
// read as "this tier selects nothing", which would quietly widen DELETE.
// A non-positive window stays the documented "disabled" spelling (see
// GFSDecide's doc), and is checked here alongside so the two are not
// confused.
func TestGFSDecideRejectsAnUnusableTier(t *testing.T) {
	set := gfsMustSet(t, "chain", "invalid")
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	records := gfsChainRecords(t, set, map[string]time.Time{
		"only": time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC),
	})

	bad := []struct {
		name string
		tier config.RetentionTier
	}{
		{"unknown granularity", config.RetentionTier{Name: "weird", Granularity: "fortnight", Keep: 3}},
		{"unknown window unit", config.RetentionTier{Name: "weird", Granularity: config.GranularityDay, Keep: 3, WindowUnit: "fortnight"}},
		{"custom period with no length", config.RetentionTier{Name: "weird", Granularity: config.GranularityDays, Keep: 3}},
		{"custom period with a negative length", config.RetentionTier{Name: "weird", Granularity: config.GranularityDays, PeriodDays: -1, Keep: 3}},
		{"empty name", config.RetentionTier{Granularity: config.GranularityDay, Keep: 3}},
	}
	for _, b := range bad {
		t.Run(b.name, func(t *testing.T) {
			cfg := gfsTierChainCfg("UTC", "monday", b.tier)
			if _, err := GFSDecide(now, cfg, set, records); err == nil {
				t.Fatal("GFSDecide accepted an unusable tier; a chain it cannot evaluate must be an error, never a tier that selects nothing")
			}
		})
	}

	t.Run("control: a well-formed tier is accepted", func(t *testing.T) {
		cfg := gfsTierChainCfg("UTC", "monday", config.RetentionTier{Name: "daily", Granularity: config.GranularityDay, Keep: 3})
		got, err := GFSDecide(now, cfg, set, records)
		if err != nil {
			t.Fatalf("GFSDecide refused a well-formed tier: %v", err)
		}
		gfsAssertTiers(t, got, "only", []GFSTier{"DAILY"})
	})

	t.Run("the reserved last-known-good name", func(t *testing.T) {
		cfg := gfsTierChainCfg("UTC", "monday", config.RetentionTier{Name: config.TierLastKnownGoodName, Granularity: config.GranularityDay, Keep: 3})
		if _, err := GFSDecide(now, cfg, set, records); err == nil {
			t.Fatal("GFSDecide accepted a tier named last_known_good; a GFS selection reported under FR-19's own wire name is indistinguishable from a protected artifact in a verdict's tier list")
		}
	})

	// The disabled reading survives, but only per tier: the chain as a
	// whole still has to have something enabled in it (see
	// TestGFSDecideRefusesAChainWithNoEnabledTier), so this fixture pairs
	// the zeroed tier with a live one.
	t.Run("a non-positive window is a disabled tier, not an error", func(t *testing.T) {
		cfg := gfsTierChainCfg("UTC", "monday",
			config.RetentionTier{Name: "daily", Granularity: config.GranularityDay, Keep: 0},
			config.RetentionTier{Name: "annual", Granularity: config.GranularityYear, Keep: 1},
		)
		got, err := GFSDecide(now, cfg, set, records)
		if err != nil {
			t.Fatalf("GFSDecide: %v", err)
		}
		gfsAssertTiers(t, got, "only", []GFSTier{"ANNUAL"})
	})
}

// TestGFSDecideRefusesAChainWithNoEnabledTier is the chain-level half of
// the rule TestGFSDecideRejectsAnUnusableTier pins per tier: a chain this
// package cannot get a single selection out of is an error, never a KEEP
// set of nothing.
//
// The per-tier "a non-positive keep means this tier is off" reading is
// what makes the hole: applied to every tier at once it resolves cleanly
// to KEEP = {}, and DELETE is everything KEEP did not claim, so FR-20 is
// handed every managed-complete artifact in the set. The three legacy
// scalars left at zero are the shape that actually arrives, because
// config.DefaultTierChain expands them straight into three tiers with
// Keep: 0 for any caller that skipped config.Validate.
func TestGFSDecideRefusesAChainWithNoEnabledTier(t *testing.T) {
	set := gfsMustSet(t, "chain", "all-off")
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	records := gfsChainRecords(t, set, map[string]time.Time{
		"a": time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC),
		"b": time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC),
		"c": time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
	})

	// Not config.Retention{}: the zero value fails on week_starts_on
	// first, so a test written that way would pass for the wrong reason
	// and keep passing with the chain check removed.
	t.Run("the three legacy scalars all left at zero", func(t *testing.T) {
		cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday"}
		got, err := GFSDecide(now, cfg, set, records)
		if err == nil {
			t.Fatalf("GFSDecide accepted a policy whose every tier is disabled and kept %d of %d artifacts; an empty KEEP is a proposal to delete the whole set", gfsKeptCount(got), len(records))
		}
	})

	t.Run("an explicit chain whose every tier is disabled", func(t *testing.T) {
		cfg := gfsTierChainCfg("UTC", "monday",
			config.RetentionTier{Name: "daily", Granularity: config.GranularityDay, Keep: 0},
			config.RetentionTier{Name: "annual", Granularity: config.GranularityYear, Keep: -3},
		)
		if _, err := GFSDecide(now, cfg, set, records); err == nil {
			t.Fatal("GFSDecide accepted an explicit chain in which no tier is enabled")
		}
	})

	// The control: the refusal is about the whole chain being off, not
	// about any single zeroed tier, so one live tier is enough.
	t.Run("control: one enabled tier alongside a disabled one is accepted", func(t *testing.T) {
		cfg := gfsTierChainCfg("UTC", "monday",
			config.RetentionTier{Name: "daily", Granularity: config.GranularityDay, Keep: 0},
			config.RetentionTier{Name: "annual", Granularity: config.GranularityYear, Keep: 1},
		)
		got, err := GFSDecide(now, cfg, set, records)
		if err != nil {
			t.Fatalf("GFSDecide refused a chain with a live tier in it: %v", err)
		}
		gfsAssertKeptNames(t, got, []string{"a"})
	})

	// The other control: the legacy scalars, resolved the way
	// config.Validate resolves them, still decide exactly as they always
	// have.
	t.Run("control: the resolved 7/3/12 scalars are accepted", func(t *testing.T) {
		cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 7, WeeklyMonths: 3, MonthlyMonths: 12}
		got, err := GFSDecide(now, cfg, set, records)
		if err != nil {
			t.Fatalf("GFSDecide refused the default chain: %v", err)
		}
		gfsAssertKeptNames(t, got, []string{"a", "b", "c"})
	})
}

func gfsKeptCount(verdicts []GFSVerdict) int {
	n := 0
	for _, v := range verdicts {
		if v.Keep {
			n++
		}
	}
	return n
}
