package retention

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// --- test helpers (prefixed gfs* so a sibling FR-19/FR-20 test file in
// this same package can declare its own without colliding with these) ---

func gfsMustSet(t *testing.T, source, name string) model.BackupSetID {
	t.Helper()
	id, err := model.NewBackupSetID(source, name)
	if err != nil {
		t.Fatalf("building backup set id: %v", err)
	}
	return id
}

func gfsMustArtifact(t *testing.T, set model.BackupSetID, name string) model.ArtifactID {
	t.Helper()
	a, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("building artifact id %q: %v", name, err)
	}
	return a
}

func gfsMustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("loading location %q: %v", name, err)
	}
	return loc
}

// gfsRecSpec is a compact way to describe one journal row for these tests:
// only the fields GFSDecide actually looks at.
type gfsRecSpec struct {
	name       string
	state      lifecycle.State
	discovered time.Time
}

func gfsBuildRecords(t *testing.T, set model.BackupSetID, specs []gfsRecSpec) []state.Record {
	t.Helper()
	out := make([]state.Record, 0, len(specs))
	for _, s := range specs {
		out = append(out, state.Record{
			Artifact:     gfsMustArtifact(t, set, s.name),
			State:        string(s.state),
			DiscoveredAt: s.discovered,
			UpdatedAt:    s.discovered,
		})
	}
	return out
}

// gfsKeptNames extracts the names GFSDecide marked Keep == true, in the
// order GFSDecide returned them (already sorted by name).
func gfsKeptNames(verdicts []GFSVerdict) []string {
	var out []string
	for _, v := range verdicts {
		if v.Keep {
			out = append(out, v.Artifact.Name)
		}
	}
	return out
}

func gfsAssertKeptNames(t *testing.T, verdicts []GFSVerdict, want []string) {
	t.Helper()
	got := gfsKeptNames(verdicts)
	gotSorted := append([]string(nil), got...)
	wantSorted := append([]string(nil), want...)
	sort.Strings(gotSorted)
	sort.Strings(wantSorted)
	if len(gotSorted) == 0 {
		gotSorted = nil
	}
	if len(wantSorted) == 0 {
		wantSorted = nil
	}
	if !reflect.DeepEqual(gotSorted, wantSorted) {
		t.Errorf("kept = %v, want %v (full verdicts: %+v)", gotSorted, wantSorted, verdicts)
	}
}

// gfsCase is one calendar/DST/leap/year-boundary scenario. protects
// documents, in the test's own words, the retention bug it exists to
// catch: per the issue brief, these are the cases that quietly delete a
// month of backups and never announce themselves when they do.
type gfsCase struct {
	name      string
	protects  string
	timezone  string
	weekStart string
	daily     int
	weekly    int
	monthly   int
	now       time.Time
	records   []gfsRecSpec
	wantKeep  []string
}

func TestGFSDecideCalendarCases(t *testing.T) {
	utc := time.UTC
	van := gfsMustLoc(t, "America/Vancouver")

	cases := []gfsCase{
		{
			name:     "daily tier keeps the newest backup per calendar day, within the window",
			protects: "a day with two backups keeps only the later one; a day just outside the N-day window is dropped entirely, even though it is barely a few hours older than the oldest kept day",
			timezone: "UTC", weekStart: "monday",
			daily: 3,
			now:   time.Date(2026, 8, 28, 18, 0, 0, 0, utc),
			records: []gfsRecSpec{
				{"d-aug26-morning", lifecycle.Complete, time.Date(2026, 8, 26, 8, 0, 0, 0, utc)},
				{"d-aug26-evening", lifecycle.Complete, time.Date(2026, 8, 26, 20, 0, 0, 0, utc)},
				{"d-aug27", lifecycle.Complete, time.Date(2026, 8, 27, 12, 0, 0, 0, utc)},
				{"d-aug28", lifecycle.Complete, time.Date(2026, 8, 28, 9, 0, 0, 0, utc)},
				{"d-aug25-too-old", lifecycle.Complete, time.Date(2026, 8, 25, 23, 59, 0, 0, utc)},
			},
			wantKeep: []string{"d-aug26-evening", "d-aug27", "d-aug28"},
		},
		{
			name:     "monday-start week: Sunday and the following Monday are different week buckets",
			protects: "week_starts_on actually changes which days are grouped together, not just a display label",
			timezone: "UTC", weekStart: "monday",
			weekly: 12, // wide window: this case is about bucketing, not the window edge
			now:    time.Date(2026, 9, 1, 12, 0, 0, 0, utc),
			records: []gfsRecSpec{
				{"w-sun-aug30", lifecycle.Complete, time.Date(2026, 8, 30, 10, 0, 0, 0, utc)},
				{"w-mon-aug31", lifecycle.Complete, time.Date(2026, 8, 31, 10, 0, 0, 0, utc)},
			},
			wantKeep: []string{"w-sun-aug30", "w-mon-aug31"},
		},
		{
			name:     "sunday-start week: the same Sunday and Monday now fall in the same week bucket",
			protects: "changing week_starts_on to sunday merges Aug 30 and Aug 31 into one bucket, so only the newer of the two survives; a hardcoded Monday assumption would keep both here too and never surface the bug",
			timezone: "UTC", weekStart: "sunday",
			weekly: 12,
			now:    time.Date(2026, 9, 1, 12, 0, 0, 0, utc),
			records: []gfsRecSpec{
				{"w2-sun-aug30", lifecycle.Complete, time.Date(2026, 8, 30, 10, 0, 0, 0, utc)},
				{"w2-mon-aug31", lifecycle.Complete, time.Date(2026, 8, 31, 10, 0, 0, 0, utc)},
			},
			wantKeep: []string{"w2-mon-aug31"},
		},
		{
			name:     "monthly tier keeps the newest backup per calendar month, within the window",
			protects: "a month with several backups keeps only the newest; a month one step outside the configured window is dropped in full",
			timezone: "UTC", weekStart: "monday",
			monthly: 3, // this month + 2 previous = Jun, Jul, Aug
			now:     time.Date(2026, 8, 28, 12, 0, 0, 0, utc),
			records: []gfsRecSpec{
				{"m-jun-early", lifecycle.Complete, time.Date(2026, 6, 5, 0, 0, 0, 0, utc)},
				{"m-jun-late", lifecycle.Complete, time.Date(2026, 6, 25, 0, 0, 0, 0, utc)},
				{"m-jul", lifecycle.Complete, time.Date(2026, 7, 10, 0, 0, 0, 0, utc)},
				{"m-aug", lifecycle.Complete, time.Date(2026, 8, 20, 0, 0, 0, 0, utc)},
				{"m-may-too-old", lifecycle.Complete, time.Date(2026, 5, 31, 0, 0, 0, 0, utc)},
			},
			wantKeep: []string{"m-jun-late", "m-jul", "m-aug"},
		},
		{
			// This is the headline regression this issue calls out: "the
			// cases that quietly delete a month of backups." A cutoff
			// computed as today.AddDate(0, -(N-1), 0) applied directly to
			// May 31 lands on the nonexistent "April 31", which Go's
			// calendar normalizes forward to May 1 -- silently excluding
			// the whole month of April from the window. Normalizing to the
			// 1st of the month before subtracting (see
			// gfsCivilDate.firstOfMonth) avoids the overflow entirely.
			name:     "weekly window survives a month-end day-of-month overflow (May 31 minus one month)",
			protects: "a naive today.AddDate(0,-1,0) on May 31 overflows to May 1 instead of April 1, which would wrongly drop every April backup from a 2-month weekly window",
			timezone: "UTC", weekStart: "monday",
			weekly: 2, // this month + 1 previous = April, May
			now:    time.Date(2026, 5, 31, 12, 0, 0, 0, utc),
			records: []gfsRecSpec{
				{"wk-apr15", lifecycle.Complete, time.Date(2026, 4, 15, 9, 0, 0, 0, utc)},
			},
			wantKeep: []string{"wk-apr15"},
		},
		{
			name:     "monthly window survives the same month-end day-of-month overflow",
			protects: "the identical May-31 overflow bug, checked against the monthly tier's own window computation, not just the weekly one",
			timezone: "UTC", weekStart: "monday",
			monthly: 2,
			now:     time.Date(2026, 5, 31, 12, 0, 0, 0, utc),
			records: []gfsRecSpec{
				{"mo-apr20", lifecycle.Complete, time.Date(2026, 4, 20, 9, 0, 0, 0, utc)},
			},
			wantKeep: []string{"mo-apr20"},
		},
		{
			// America/Vancouver's clocks spring forward from 02:00 to
			// 03:00 on 2026-03-08, making that calendar day 23 hours long.
			name:     "daily window boundary lands correctly on a 23-hour DST spring-forward day",
			protects: "a duration-based cutoff (now minus N*24h) is an hour short of local midnight on a day next to a 23-hour DST day, which can shift which side of the boundary a backup near midnight falls on; calendar-day arithmetic does not have that hour to lose",
			timezone: "America/Vancouver", weekStart: "monday",
			daily: 4, // window: Mar 8, 9, 10, 11
			now:   time.Date(2026, 3, 11, 12, 0, 0, 0, van),
			records: []gfsRecSpec{
				{"dst-mar8-in-window", lifecycle.Complete, time.Date(2026, 3, 8, 1, 0, 0, 0, van)},
				{"dst-mar7-just-outside", lifecycle.Complete, time.Date(2026, 3, 7, 23, 30, 0, 0, van)},
			},
			wantKeep: []string{"dst-mar8-in-window"},
		},
		{
			name:     "daily window spans a Dec 31 / Jan 1 year boundary",
			protects: "a 7-day window computed across a year rollover keeps the year field correct on both sides, rather than only comparing month/day and accidentally matching across years",
			timezone: "UTC", weekStart: "monday",
			daily: 7, // window: Dec 28 2026 .. Jan 3 2027
			now:   time.Date(2027, 1, 3, 10, 0, 0, 0, utc),
			records: []gfsRecSpec{
				{"yb-dec28", lifecycle.Complete, time.Date(2026, 12, 28, 8, 0, 0, 0, utc)},
				{"yb-dec27-too-old", lifecycle.Complete, time.Date(2026, 12, 27, 8, 0, 0, 0, utc)},
				{"yb-jan3", lifecycle.Complete, time.Date(2027, 1, 3, 8, 0, 0, 0, utc)},
			},
			wantKeep: []string{"yb-dec28", "yb-jan3"},
		},
		{
			name:     "12-month monthly window from a January 'now' reaches back into February of the previous year",
			protects: "addMonths crossing a year boundary must decrement the year, not just wrap the month number; an off-by-one here would either include 13 months or drop the oldest one",
			timezone: "UTC", weekStart: "monday",
			monthly: 12, // window: Feb 2026 .. Jan 2027
			now:     time.Date(2027, 1, 15, 10, 0, 0, 0, utc),
			records: []gfsRecSpec{
				{"yb-feb2026-edge", lifecycle.Complete, time.Date(2026, 2, 20, 10, 0, 0, 0, utc)},
				{"yb-jan2026-too-old", lifecycle.Complete, time.Date(2026, 1, 20, 10, 0, 0, 0, utc)},
			},
			wantKeep: []string{"yb-feb2026-edge"},
		},
		{
			name:     "Feb 29 in a leap year is a distinct, correctly bucketed calendar day",
			protects: "the leap day itself is neither skipped nor merged with Feb 28 or Mar 1 when stepping calendar days across it",
			timezone: "UTC", weekStart: "monday",
			daily: 5, // window: Feb 27, 28, 29, Mar 1, Mar 2 (2028 is a leap year)
			now:   time.Date(2028, 3, 2, 10, 0, 0, 0, utc),
			records: []gfsRecSpec{
				{"leap-feb29", lifecycle.Complete, time.Date(2028, 2, 29, 10, 0, 0, 0, utc)},
				{"leap-feb26-too-old", lifecycle.Complete, time.Date(2028, 2, 26, 10, 0, 0, 0, utc)},
			},
			wantKeep: []string{"leap-feb29"},
		},
		{
			name:     "leap day as 'now': the monthly bucket is exactly February, no more and no less",
			protects: "running the calculation on Feb 29 itself doesn't widen the current month's window to include Mar 1, and a backup discovered nominally 'in the future' relative to Feb 29 is never a tier representative",
			timezone: "UTC", weekStart: "monday",
			monthly: 1, // this month only: Feb 2028
			now:     time.Date(2028, 2, 29, 12, 0, 0, 0, utc),
			records: []gfsRecSpec{
				{"leapmo-feb29", lifecycle.Complete, time.Date(2028, 2, 29, 8, 0, 0, 0, utc)},
				{"leapmo-feb01-loses-bucket", lifecycle.Complete, time.Date(2028, 2, 1, 8, 0, 0, 0, utc)},
				{"leapmo-mar01-in-the-future", lifecycle.Complete, time.Date(2028, 3, 1, 8, 0, 0, 0, utc)},
				{"leapmo-jan31-too-old", lifecycle.Complete, time.Date(2028, 1, 31, 8, 0, 0, 0, utc)},
			},
			wantKeep: []string{"leapmo-feb29"},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("protects against: %s", tc.protects)
			if tc.protects == "" {
				t.Fatalf("test case %q has no protects note", tc.name)
			}
			set := gfsMustSet(t, "gfscal", fmt.Sprintf("case%d", i))
			records := gfsBuildRecords(t, set, tc.records)
			cfg := config.Retention{
				Timezone:      tc.timezone,
				WeekStartsOn:  tc.weekStart,
				DailyDays:     tc.daily,
				WeeklyMonths:  tc.weekly,
				MonthlyMonths: tc.monthly,
			}
			got, err := GFSDecide(tc.now, cfg, set, records)
			if err != nil {
				t.Fatalf("GFSDecide: %v", err)
			}
			gfsAssertKeptNames(t, got, tc.wantKeep)
		})
	}
}

// --- structural properties: eligibility, isolation, determinism ---

func TestGFSDecideOnlyConsidersManagedCompleteStates(t *testing.T) {
	set := gfsMustSet(t, "elig", "set")
	base := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)

	var specs []gfsRecSpec
	for i, st := range lifecycle.AllStates {
		specs = append(specs, gfsRecSpec{
			name:       strings.ToLower(string(st)),
			state:      st,
			discovered: base.Add(time.Duration(i) * time.Minute),
		})
	}
	records := gfsBuildRecords(t, set, specs)

	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 1}
	got, err := GFSDecide(base.Add(time.Hour), cfg, set, records)
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}

	wantEligible := map[string]bool{
		strings.ToLower(string(lifecycle.Committed)):           true,
		strings.ToLower(string(lifecycle.RemoteDeletePending)): true,
		strings.ToLower(string(lifecycle.Complete)):            true,
	}
	if len(got) != len(wantEligible) {
		t.Fatalf("got %d verdicts, want %d (one per managed-complete state); got=%+v", len(got), len(wantEligible), got)
	}
	for _, v := range got {
		if !wantEligible[v.Artifact.Name] {
			t.Errorf("verdict for ineligible state artifact %q leaked into the output", v.Artifact.Name)
		}
	}

	// Complete is discovered latest of the three (see AllStates order), so
	// within the shared daily bucket it wins; Committed and
	// RemoteDeletePending are eligible (they appear above) but lose that
	// bucket and are correctly not kept.
	completeName := strings.ToLower(string(lifecycle.Complete))
	for _, v := range got {
		want := v.Artifact.Name == completeName
		if v.Keep != want {
			t.Errorf("artifact %q: Keep = %v, want %v", v.Artifact.Name, v.Keep, want)
		}
	}
}

func TestGFSDecideRejectsRecordFromAnotherBackupSet(t *testing.T) {
	setA := gfsMustSet(t, "iso", "a")
	setB := gfsMustSet(t, "iso", "b")
	now := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)

	records := []state.Record{
		{Artifact: gfsMustArtifact(t, setA, "own"), State: string(lifecycle.Complete), DiscoveredAt: now},
		{Artifact: gfsMustArtifact(t, setB, "foreign"), State: string(lifecycle.Complete), DiscoveredAt: now},
	}

	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 1}
	if _, err := GFSDecide(now, cfg, setA, records); err == nil {
		t.Fatal("expected an error for a record belonging to a different backup set, got nil (FR-7 isolation would be silently broken)")
	}
}

func TestGFSDecideRejectsZeroBackupSet(t *testing.T) {
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 1}
	if _, err := GFSDecide(time.Now(), cfg, model.BackupSetID{}, nil); err == nil {
		t.Fatal("expected an error for a zero backup set id, got nil")
	}
}

func TestGFSDecideIsOrderIndependent(t *testing.T) {
	set := gfsMustSet(t, "order", "set")
	specs := []gfsRecSpec{
		{"a", lifecycle.Complete, time.Date(2026, 8, 26, 8, 0, 0, 0, time.UTC)},
		{"b", lifecycle.Complete, time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)},
		{"c", lifecycle.Complete, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{"d", lifecycle.Committed, time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)},
		{"e", lifecycle.RemoteDeletePending, time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)},
	}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 3, WeeklyMonths: 2, MonthlyMonths: 3}

	forward := gfsBuildRecords(t, set, specs)
	want, err := GFSDecide(now, cfg, set, forward)
	if err != nil {
		t.Fatalf("GFSDecide (forward order): %v", err)
	}

	reversedSpecs := make([]gfsRecSpec, len(specs))
	for i, s := range specs {
		reversedSpecs[len(specs)-1-i] = s
	}
	reversed := gfsBuildRecords(t, set, reversedSpecs)
	got, err := GFSDecide(now, cfg, set, reversed)
	if err != nil {
		t.Fatalf("GFSDecide (reversed order): %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("GFSDecide is not order-independent:\n forward = %+v\n reversed = %+v", want, got)
	}
}

func TestGFSDecideTieBreakIsDeterministic(t *testing.T) {
	set := gfsMustSet(t, "tie", "set")
	sameInstant := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	specs := []gfsRecSpec{
		{"zzz-later-name", lifecycle.Complete, sameInstant},
		{"aaa-earlier-name", lifecycle.Complete, sameInstant},
	}
	now := time.Date(2026, 8, 28, 18, 0, 0, 0, time.UTC)
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 1}

	for _, order := range [][]int{{0, 1}, {1, 0}} {
		ordered := []gfsRecSpec{specs[order[0]], specs[order[1]]}
		records := gfsBuildRecords(t, set, ordered)
		got, err := GFSDecide(now, cfg, set, records)
		if err != nil {
			t.Fatalf("GFSDecide: %v", err)
		}
		// "zzz-later-name" sorts after "aaa-earlier-name", and the tie
		// break in gfsIsNewerRepresentative favours the lexicographically
		// greater name, so it must win regardless of input order.
		gfsAssertKeptNames(t, got, []string{"zzz-later-name"})
	}
}

func TestGFSDecideSameCalendarDayDifferentTimeOfDayIsIdentical(t *testing.T) {
	set := gfsMustSet(t, "sameday", "set")
	specs := []gfsRecSpec{
		{"only", lifecycle.Complete, time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)},
	}
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 3}

	early := gfsBuildRecords(t, set, specs)
	gotEarly, err := GFSDecide(time.Date(2026, 8, 28, 0, 0, 1, 0, time.UTC), cfg, set, early)
	if err != nil {
		t.Fatalf("GFSDecide (early instant): %v", err)
	}
	late := gfsBuildRecords(t, set, specs)
	gotLate, err := GFSDecide(time.Date(2026, 8, 28, 23, 59, 59, 0, time.UTC), cfg, set, late)
	if err != nil {
		t.Fatalf("GFSDecide (late instant): %v", err)
	}
	if !reflect.DeepEqual(gotEarly, gotLate) {
		t.Errorf("verdict changed between two instants on the same calendar day:\n 00:00:01 = %+v\n 23:59:59 = %+v", gotEarly, gotLate)
	}
}

func TestGFSDecideFutureDatedRecordIsNeverARepresentative(t *testing.T) {
	set := gfsMustSet(t, "future", "set")
	specs := []gfsRecSpec{
		{"from-the-future", lifecycle.Complete, time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)},
	}
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC) // clock is "before" the record's own timestamp
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 7, WeeklyMonths: 3, MonthlyMonths: 12}

	records := gfsBuildRecords(t, set, specs)
	got, err := GFSDecide(now, cfg, set, records)
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}
	gfsAssertKeptNames(t, got, nil)
	if len(got) != 1 {
		t.Fatalf("expected the record to still appear in the output (it is eligible), got %+v", got)
	}
}

// TestGFSDecideDisabledTiersSelectNothing pins the per-tier reading of a
// non-positive window: the tier is off, it contributes nothing to KEEP,
// and it is not an error.
//
// The fixture leaves one tier live, because the whole chain being off is
// a different question with a different answer (gfsResolveChain refuses
// it: see TestGFSDecideRefusesAChainWithNoEnabledTier). "a" falls in the
// live monthly tier's window and is kept by it alone; if the two zeroed
// tiers were being read as live, "a" would come back badged DAILY and
// WEEKLY as well.
func TestGFSDecideDisabledTiersSelectNothing(t *testing.T) {
	set := gfsMustSet(t, "disabled", "set")
	specs := []gfsRecSpec{
		{"a", lifecycle.Complete, time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)},
		{"b", lifecycle.Committed, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
	}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	// daily and weekly off, monthly live over August alone.
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", MonthlyMonths: 1}

	records := gfsBuildRecords(t, set, specs)
	got, err := GFSDecide(now, cfg, set, records)
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}
	gfsAssertTiers(t, got, "a", []GFSTier{GFSMonthly})
	gfsAssertKeptNames(t, got, []string{"a"})
	if len(got) != 2 {
		t.Fatalf("expected both eligible records to still appear, got %+v", got)
	}
}

func TestGFSVerdictListsTiersInFixedOrder(t *testing.T) {
	set := gfsMustSet(t, "union", "set")
	// A single backup, alone, is simultaneously the daily, weekly and
	// monthly representative for its own day/week/month.
	specs := []gfsRecSpec{
		{"triple-tier", lifecycle.Complete, time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)},
	}
	now := time.Date(2026, 8, 28, 18, 0, 0, 0, time.UTC)
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 1, WeeklyMonths: 1, MonthlyMonths: 1}

	records := gfsBuildRecords(t, set, specs)
	got, err := GFSDecide(now, cfg, set, records)
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one verdict, got %+v", got)
	}
	v := got[0]
	if !v.Keep {
		t.Fatalf("expected Keep == true, got %+v", v)
	}
	want := []GFSTier{GFSDaily, GFSWeekly, GFSMonthly}
	if !reflect.DeepEqual(v.Tiers, want) {
		t.Errorf("Tiers = %v, want %v (fixed Daily, Weekly, Monthly order)", v.Tiers, want)
	}
}

func TestGFSDecideRejectsUnloadableTimezone(t *testing.T) {
	set := gfsMustSet(t, "tz", "set")
	cfg := config.Retention{Timezone: "Mars/Phobos", WeekStartsOn: "monday", DailyDays: 1}
	if _, err := GFSDecide(time.Now(), cfg, set, nil); err == nil {
		t.Fatal("expected an error for an unloadable timezone, got nil")
	}
}

func TestGFSDecideRejectsUnknownWeekday(t *testing.T) {
	set := gfsMustSet(t, "weekday", "set")
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "blursday", DailyDays: 1}
	if _, err := GFSDecide(time.Now(), cfg, set, nil); err == nil {
		t.Fatal("expected an error for an unrecognised week_starts_on, got nil")
	}
}
