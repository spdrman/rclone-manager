package retention

import (
	"fmt"
	"path/filepath"
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

// This file covers issue #218: a KEEP verdict says which tiers kept an
// artifact, but not which of FR-18's two placements put it in each of
// those tiers' buckets.
//
// #215 made every artifact land twice, once by the discovery timestamp
// and once by the producer's own, and made KEEP the union. Those two
// placements have different trust properties (FR-8 distrusts the second),
// so "DAILY kept this" is only half a fact until it says which pass
// selected it. The attribution therefore belongs to a (verdict, tier)
// pair and not to a verdict: one artifact really can be selected by
// DAILY through one placement and by MONTHLY through the other, and the
// fixture below is built so that happens.

// --- helpers (prefixed ta* so they cannot collide with the other files') ---

func taAt(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return ts
}

// taSelections renders verdicts as artifact name -> "TIER=PLACEMENT"
// strings, so a whole expected shape can be written as one typed literal
// rather than walked field by field in the assertion.
func taSelections(verdicts []GFSVerdict) map[string][]string {
	out := map[string][]string{}
	for _, v := range verdicts {
		if !v.Keep {
			out[v.Artifact.Name] = nil
			continue
		}
		rendered := make([]string, 0, len(v.Tiers))
		for _, sel := range v.Tiers {
			rendered = append(rendered, fmt.Sprintf("%s=%s", sel.Tier, sel.By))
		}
		out[v.Artifact.Name] = rendered
	}
	return out
}

// taDecisiveFixture is the whole point of this file.
//
// "now" is Monday 2026-08-31T12:00:00Z, and the chain is FR-18's default
// (daily over 7 days, weekly over 3 calendar months, monthly over 12).
// The four artifacts are chosen so that the placements disagree per tier,
// hand-derived from the chain's own arithmetic:
//
//	daily window   2026-08-25 .. 2026-08-31
//	weekly window  2026-06-01 .. 2026-08-31, bucketed by Monday-start week
//	monthly window 2025-09-01 .. 2026-08-31, bucketed by calendar month
//
//	fresh      discovered 08-31 09:00, produced 08-31 08:00
//	           both placements land on today, and it is the newest under
//	           each, so it wins every tier under BOTH passes.
//	backlogged discovered 08-31 08:30, produced 08-29 00:00
//	           its discovery placement loses today's daily bucket and this
//	           week's weekly bucket to fresh; its producer placement owns
//	           08-29 (its own daily bucket) and the week of 08-24, so it
//	           holds DAILY and WEEKLY by the PRODUCER pass alone. Its
//	           producer month is August, which fresh already owns, so it
//	           takes no MONTHLY.
//	backdated  discovered 08-28 09:00, produced 2026-02-12
//	           its discovery placement owns 08-28's daily bucket outright,
//	           so DAILY is DISCOVERY. Its producer placement is 200 days
//	           back: outside the daily and weekly windows entirely, but
//	           inside the monthly one, where it owns February and fresh
//	           owns August. So MONTHLY is PRODUCER while DAILY is
//	           DISCOVERY, on one artifact, which no per-verdict field can
//	           express.
//	ancient    discovered and produced in 2023: outside every window, and
//	           the control that keeps "the ladder" from being satisfied by
//	           a chain that stopped checking windows.
func taDecisiveFixture(t *testing.T) (time.Time, config.Retention, model.BackupSetID, []recSpecWithProducer) {
	t.Helper()
	set := gfsMustSet(t, "production", "postgres-primary")
	now := taAt(t, "2026-08-31T12:00:00Z")
	cfg := config.Retention{
		Timezone:      "UTC",
		WeekStartsOn:  "monday",
		DailyDays:     7,
		WeeklyMonths:  3,
		MonthlyMonths: 12,
	}
	specs := []recSpecWithProducer{
		{name: "fresh.dump", discovered: taAt(t, "2026-08-31T09:00:00Z"), producer: taAt(t, "2026-08-31T08:00:00Z")},
		{name: "backlogged.dump", discovered: taAt(t, "2026-08-31T08:30:00Z"), producer: taAt(t, "2026-08-29T00:00:00Z")},
		{name: "backdated.dump", discovered: taAt(t, "2026-08-28T09:00:00Z"), producer: taAt(t, "2026-02-12T00:00:00Z")},
		{name: "ancient.dump", discovered: taAt(t, "2023-01-05T09:00:00Z"), producer: taAt(t, "2023-01-05T08:00:00Z")},
	}
	return now, cfg, set, specs
}

type recSpecWithProducer struct {
	name       string
	discovered time.Time
	producer   time.Time
}

// taRecords builds the journal rows for those specs. localRoot is the
// backup set's local directory when the caller is about to run FR-20's
// prune over them, and empty when only the classification matters.
func taRecords(t *testing.T, set model.BackupSetID, specs []recSpecWithProducer, localRoot string) []state.Record {
	t.Helper()
	out := make([]state.Record, 0, len(specs))
	for _, s := range specs {
		p := s.producer
		rec := state.Record{
			Artifact:     gfsMustArtifact(t, set, s.name),
			State:        string(lifecycle.Complete),
			DiscoveredAt: s.discovered,
			UpdatedAt:    s.discovered,
		}
		if localRoot != "" {
			rec.LocalPath = filepath.Join(localRoot, s.name)
		}
		rec.Remote.ModTime = &p
		out = append(out, rec)
	}
	return out
}

// TestGFSDecideAttributesEachTierToThePlacementThatSelectedIt is issue
// #218's decisive case. The expected table is hand-derived from
// taDecisiveFixture's own doc, not recorded from a run.
func TestGFSDecideAttributesEachTierToThePlacementThatSelectedIt(t *testing.T) {
	now, cfg, set, specs := taDecisiveFixture(t)
	verdicts, err := GFSDecide(now, cfg, set, taRecords(t, set, specs, ""))
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}

	want := map[string][]string{
		"fresh.dump":      {"DAILY=BOTH", "WEEKLY=BOTH", "MONTHLY=BOTH"},
		"backlogged.dump": {"DAILY=PRODUCER", "WEEKLY=PRODUCER"},
		"backdated.dump":  {"DAILY=DISCOVERY", "WEEKLY=DISCOVERY", "MONTHLY=PRODUCER"},
		"ancient.dump":    nil,
	}
	got := taSelections(verdicts)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("per-tier placement attribution:\n got %v\nwant %v", got, want)
	}
}

// TestGFSTierAttributionCannotBeCollapsedToOnePerVerdict is the positive
// control on the fixture above. If every artifact's tiers happened to
// share a single placement, a per-verdict field would satisfy the test
// and the test would be proving nothing about the thing #218 is about.
// This asserts the fixture is actually decisive.
func TestGFSTierAttributionCannotBeCollapsedToOnePerVerdict(t *testing.T) {
	now, cfg, set, specs := taDecisiveFixture(t)
	verdicts, err := GFSDecide(now, cfg, set, taRecords(t, set, specs, ""))
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}

	var mixed []string
	for _, v := range verdicts {
		seen := map[GFSSelectedBy]bool{}
		for _, sel := range v.Tiers {
			seen[sel.By] = true
		}
		if len(seen) > 1 {
			mixed = append(mixed, v.Artifact.Name)
		}
	}
	sort.Strings(mixed)
	if want := []string{"backdated.dump"}; !reflect.DeepEqual(mixed, want) {
		t.Errorf("artifacts whose tiers were selected by DIFFERENT placements = %v, want %v; without one of those this fixture cannot tell a per-tier field from a per-verdict one", mixed, want)
	}
}

// TestDecideKeepMarksLastKnownGoodAsProtectionNotAPlacement holds FR-19's
// term to being visibly a different kind of thing. Protection is not a
// bucket selection, so attributing it to the discovery or the producer
// placement would be a lie about where it came from.
func TestDecideKeepMarksLastKnownGoodAsProtectionNotAPlacement(t *testing.T) {
	now, cfg, set, specs := taDecisiveFixture(t)
	verdicts, lkg, err := DecideKeep(now, cfg, set, taRecords(t, set, specs, ""))
	if err != nil {
		t.Fatalf("DecideKeep: %v", err)
	}
	if !lkg.Protected {
		t.Fatalf("the fixture is meant to have a protected artifact: %+v", lkg)
	}

	var protectedVerdict *GFSVerdict
	for i := range verdicts {
		if verdicts[i].Artifact == lkg.Artifact {
			protectedVerdict = &verdicts[i]
		}
	}
	if protectedVerdict == nil {
		t.Fatalf("no verdict for the protected artifact %s", lkg.Artifact)
	}

	last := protectedVerdict.Tiers[len(protectedVerdict.Tiers)-1]
	if last.Tier != TierLastKnownGood || last.By != GFSSelectedByProtection {
		t.Errorf("the last-known-good entry = %+v, want {Tier:%s By:%s}", last, TierLastKnownGood, GFSSelectedByProtection)
	}
	for _, sel := range protectedVerdict.Tiers[:len(protectedVerdict.Tiers)-1] {
		if sel.By == GFSSelectedByProtection {
			t.Errorf("GFS tier %s is attributed to FR-19 protection, which is not a placement and never selects a bucket", sel.Tier)
		}
	}
}

// TestGFSTierSelectionRendersTheTierAndItsPlacement pins the one string
// the CLI's per-artifact line is built out of. A GFS tier always carries
// its placement; FR-19's protected term renders bare, because there is no
// placement to name and a placement-shaped suffix on it would read as one.
func TestGFSTierSelectionRendersTheTierAndItsPlacement(t *testing.T) {
	for _, tc := range []struct {
		sel  GFSTierSelection
		want string
	}{
		{GFSTierSelection{Tier: GFSDaily, By: GFSSelectedByDiscovery}, "DAILY(discovery)"},
		{GFSTierSelection{Tier: GFSWeekly, By: GFSSelectedByProducer}, "WEEKLY(producer)"},
		{GFSTierSelection{Tier: GFSMonthly, By: GFSSelectedByBoth}, "MONTHLY(both)"},
		{GFSTierSelection{Tier: TierLastKnownGood, By: GFSSelectedByProtection}, "LAST_KNOWN_GOOD"},
	} {
		if got := tc.sel.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.sel, got, tc.want)
		}
	}
}

// TestPruneVerdictCarriesTheTierAttributionItWasGiven proves FR-20's
// fully-explained answer did not drop the attribution on the way through,
// which is where an operator actually reads it.
func TestPruneVerdictCarriesTheTierAttributionItWasGiven(t *testing.T) {
	now, cfg, set, specs := taDecisiveFixture(t)
	dir := t.TempDir()
	records := taRecords(t, set, specs, dir)
	bs := pruneBackupSet(set, dir)
	for _, rec := range records {
		pruneWriteFile(t, rec.LocalPath, "payload")
	}

	verdicts, err := PruneDecide(now, cfg, bs, records, AllLocal)
	if err != nil {
		t.Fatalf("PruneDecide: %v", err)
	}
	for _, v := range verdicts {
		if v.Artifact.Name != "backdated.dump" {
			continue
		}
		var rendered []string
		for _, sel := range v.Tiers {
			rendered = append(rendered, sel.String())
		}
		want := []string{"DAILY(discovery)", "WEEKLY(discovery)", "MONTHLY(producer)"}
		if !reflect.DeepEqual(rendered, want) {
			t.Errorf("PruneVerdict tiers for backdated.dump = %v, want %v", rendered, want)
		}
		if !strings.Contains(v.Reason, "MONTHLY") {
			t.Errorf("the KEEP reason does not name the tiers that kept it: %q", v.Reason)
		}
		return
	}
	t.Fatal("PruneDecide returned no verdict for backdated.dump")
}
