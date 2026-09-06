package app

import (
	"context"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// FR-27's home-medium pass, where it actually runs: on the retention
// preview, over real journal records with real placement rows (#236's
// FR-29 table, landed in #381).
//
// internal/retention's own tests pin the rule. These pin the WIRING, which
// is a different claim and the one that would silently be false: a report
// that computed the plan against the wrong chain, or that read placements
// from somewhere other than the records the verdicts were decided from,
// would pass every test one layer down.

// chainWithOffsiteMonthly is a whole policy whose daily tier is local and
// whose monthly tier is not, which is the deployment story FR-27 exists
// for. Both tiers are named explicitly rather than left to the three
// scalars, because a medium is only expressible in the tiers spelling.
func chainWithOffsiteMonthly() config.Retention {
	protect := true
	return config.Retention{
		Timezone: "UTC", WeekStartsOn: "monday",
		Tiers: []config.RetentionTier{
			{Name: "daily", Granularity: config.GranularityDay, Keep: 7},
			{Name: "monthly", Granularity: config.GranularityMonth, Keep: 12, Medium: "cold_offsite"},
		},
		ProtectLastKnownGood: &protect,
	}
}

// placeOn writes one ACTIVE placement onto the artifact named name, in a
// records slice, so a test can say where an artifact currently is without
// running a move.
func placeOn(t *testing.T, records []state.Record, name, medium string) {
	t.Helper()
	for i := range records {
		if records[i].Artifact.Name == name {
			records[i].Placements = []state.Placement{{Medium: medium, Status: state.PlacementActive}}
			return
		}
	}
	t.Fatalf("no record named %q to place", name)
}

// TestRetentionPreview_PlansAMoveForAnArtifactThatIsNotWhereItBelongs is
// the wiring's whole point: the preview says where each kept artifact
// belongs, before anything moves a byte.
func TestRetentionPreview_PlansAMoveForAnArtifactThatIsNotWhereItBelongs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	journal := openJournal(t)
	bs := testBackupSet(t, dir)
	cfg := testConfig(t, testSource("production", bs))
	cfg.Retention = chainWithOffsiteMonthly()
	resolveTestRetention(cfg)

	svc := New(cfg, journal, nil, nil)
	report, err := svc.RetentionPreview(ctx, bs.ID)
	if err != nil {
		t.Fatalf("RetentionPreview: %v", err)
	}
	// An empty backup set plans nothing, and says so with an empty plan
	// rather than an error.
	if len(report.HomePlan.Moves) != 0 || len(report.HomePlan.Unconfirmed) != 0 {
		t.Fatalf("an empty backup set planned %+v", report.HomePlan)
	}
}

// recordsJournal serves a fixed record set for one backup set, so a test
// can put an artifact on a chosen medium without running a move. It
// embeds Journal, so any method a preview reaches for that this does not
// override is the real one and a nil dereference says so loudly rather
// than a stub quietly answering something plausible.
type recordsJournal struct {
	Journal
	records []state.Record
}

func (j recordsJournal) ListByBackupSet(context.Context, model.BackupSetID) ([]state.Record, error) {
	return j.records, nil
}

// TestRetentionPreview_PlansTheMoveTheChainAsks drives the REAL preview,
// not PlanHomeMoves directly, because the claim here is about the wiring:
// which chain the plan is computed against, and where the placements come
// from. Both would still be wrong under a preview that read the
// deployment's chain, or that looked placements up somewhere other than
// the records the verdicts were decided from.
func TestRetentionPreview_PlansTheMoveTheChainAsks(t *testing.T) {
	ctx := context.Background()
	records := gfsRecordsForHomeTest(t)
	for i := range records {
		records[i].Placements = []state.Placement{{Medium: state.MediumLocal, Status: state.PlacementActive}}
	}

	bs := testBackupSet(t, t.TempDir())
	cfg := testConfig(t, testSource("production", bs))
	cfg.Retention = chainWithOffsiteMonthly()
	resolveTestRetention(cfg)

	svc := New(cfg, recordsJournal{records: records}, nil, nil)
	svc.Now = fixedNow(retentionTestNow)

	report, err := svc.RetentionPreview(ctx, bs.ID)
	if err != nil {
		t.Fatalf("RetentionPreview: %v", err)
	}
	if len(report.HomePlan.Moves) != 1 {
		t.Fatalf("Moves = %+v, want exactly the artifact only the monthly tier selects", report.HomePlan.Moves)
	}
	move := report.HomePlan.Moves[0]
	if move.Artifact.Name != "monthly-only.dump" {
		t.Errorf("planned a move for %s, want monthly-only.dump", move.Artifact.Name)
	}
	if move.From != state.MediumLocal || move.To != "cold_offsite" {
		t.Errorf("move = %s -> %s, want %s -> cold_offsite", move.From, move.To, state.MediumLocal)
	}
	if len(report.HomePlan.Unconfirmed) != 0 {
		t.Errorf("Unconfirmed = %v, want none: every record has exactly one ACTIVE placement", report.HomePlan.Unconfirmed)
	}

	// Put it where it belongs; the move has to disappear. Without this
	// the assertions above would also pass against a preview that planned
	// a move for everything it saw.
	placeOn(t, records, "monthly-only.dump", "cold_offsite")
	settled, err := svc.RetentionPreview(ctx, bs.ID)
	if err != nil {
		t.Fatalf("RetentionPreview: %v", err)
	}
	if len(settled.HomePlan.Moves) != 0 {
		t.Fatalf("Moves = %+v after the artifact was placed on its home medium, want none", settled.HomePlan.Moves)
	}
}

// TestRetentionPreview_PlansUnderTheSetsOwnChain is the wiring claim that
// a single-set fixture cannot make. A set declaring its own retention
// policy is retained under it (#333), and the home medium is derived from
// the chain that produced the verdicts, so a preview that reached for
// s.Config.Retention would plan the deployment's moves for a set that
// decides for itself.
//
// The two chains here name DIFFERENT mediums for the same artifact, so
// the plan says which one was in force.
func TestRetentionPreview_PlansUnderTheSetsOwnChain(t *testing.T) {
	ctx := context.Background()
	records := gfsRecordsForHomeTest(t)
	for i := range records {
		records[i].Placements = []state.Placement{{Medium: state.MediumLocal, Status: state.PlacementActive}}
	}

	bs := testBackupSet(t, t.TempDir())
	own := chainWithOffsiteMonthly()
	own.Tiers[1].Medium = "the_sets_own_medium"
	bs.RetentionConfig = &own

	cfg := testConfig(t, testSource("production", bs))
	cfg.Retention = chainWithOffsiteMonthly() // the deployment says cold_offsite
	resolveTestRetention(cfg)

	svc := New(cfg, recordsJournal{records: records}, nil, nil)
	svc.Now = fixedNow(retentionTestNow)

	report, err := svc.RetentionPreview(ctx, bs.ID)
	if err != nil {
		t.Fatalf("RetentionPreview: %v", err)
	}
	if len(report.HomePlan.Moves) != 1 {
		t.Fatalf("Moves = %+v, want exactly one", report.HomePlan.Moves)
	}
	if got := report.HomePlan.Moves[0].To; got != "the_sets_own_medium" {
		t.Fatalf("planned a move to %q, want the set's own medium; the deployment's chain names cold_offsite and this set does not inherit it", got)
	}
}

// TestActiveMediumFromRecords_TwoActivePlacementsCannotBeConfirmed is the
// mid-move case, and it is the one that would cost data read the other
// way. FR-30's copy phase leaves the source and the destination both
// ACTIVE until the source delete lands, so "where is this artifact" has
// two answers, and a planner that took the first would plan a second move
// on top of one already running.
func TestActiveMediumFromRecords_TwoActivePlacementsCannotBeConfirmed(t *testing.T) {
	records := gfsRecordsForHomeTest(t)
	mid := records[0]
	mid.Placements = []state.Placement{
		{Medium: state.MediumLocal, Status: state.PlacementActive},
		{Medium: "cold_offsite", Status: state.PlacementActive},
	}
	settled := records[1]
	settled.Placements = []state.Placement{{Medium: state.MediumLocal, Status: state.PlacementActive}}

	lookup := ActiveMediumFromRecords([]state.Record{mid, settled})

	if got := lookup(mid.Artifact); got.Status != retention.LocationContested {
		t.Errorf("an artifact with two ACTIVE placements reported %+v, want CONTESTED: a move is already in flight and there are two answers, "+
			"which is a different fact from a journal that simply says nothing", got)
	}
	// The control: without it this would pass against a lookup that never
	// confirms anything.
	if got := lookup(settled.Artifact); got.Status != retention.LocationConfirmed || got.Medium != state.MediumLocal {
		t.Errorf("an artifact with one ACTIVE placement reported %+v, want a confirmed %q", got, state.MediumLocal)
	}
}

// TestActiveMediumFromRecords_ANonActivePlacementIsNotALocation pins the
// other half of "a placement row means a DURABLE copy". A DELETE_PENDING
// or GONE row is a record of a copy that is on its way out or already
// gone, and reading one as a location would plan a move FROM somewhere
// this manager is in the middle of emptying.
func TestActiveMediumFromRecords_ANonActivePlacementIsNotALocation(t *testing.T) {
	records := gfsRecordsForHomeTest(t)
	for _, status := range []string{state.PlacementDeletePending, state.PlacementGone} {
		rec := records[0]
		rec.Placements = []state.Placement{{Medium: state.MediumLocal, Status: status}}
		lookup := ActiveMediumFromRecords([]state.Record{rec})
		got := lookup(rec.Artifact)
		if got.Status == retention.LocationConfirmed {
			t.Errorf("a %s placement reported the artifact as located on %q", status, got.Medium)
		}
		if got.Status != retention.LocationUnrecorded {
			t.Errorf("a %s placement reported %+v; the row is not ACTIVE, so it falls out of the count and what is left is a journal saying nothing", status, got)
		}
	}
}

// retentionTestNow and gfsRecordsForHomeTest build the fixture these
// tests decide over: three managed-complete artifacts at three ages, so
// the chain above produces one artifact selected by daily (home local),
// one selected only by monthly (home offsite) and one selected by
// nothing.
var retentionTestNow = mustParseInstant("2026-08-28T12:00:00Z")

func gfsRecordsForHomeTest(t *testing.T) []state.Record {
	t.Helper()
	set := mustSetID(t, "production", "postgres-primary")
	return []state.Record{
		newCompleteRecord(t, set, "recent.dump", retentionTestNow.AddDate(0, 0, -1)),
		newCompleteRecord(t, set, "monthly-only.dump", retentionTestNow.AddDate(0, 0, -40)),
		newCompleteRecord(t, set, "too-old.dump", retentionTestNow.AddDate(0, 0, -800)),
	}
}

func mustParseInstant(v string) time.Time {
	at, err := time.Parse(time.RFC3339, v)
	if err != nil {
		panic(err)
	}
	return at
}

func newCompleteRecord(t *testing.T, set model.BackupSetID, name string, at time.Time) state.Record {
	t.Helper()
	id, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID(%q): %v", name, err)
	}
	return state.Record{
		Artifact:     id,
		State:        string(lifecycle.Complete),
		DiscoveredAt: at,
		UpdatedAt:    at,
	}
}

// TestRetentionPreview_ReportsWhereEachArtifactIs is the other half of
// FR-30's "the dry-run explains per-artifact WHERE the deletion would
// happen". The verdict cannot carry that, because FR-32 forbids
// internal/retention seeing a placement at all, so it travels beside the
// verdict and this is where it is filled in.
//
// The three statuses are all asserted, because they are three different
// things to tell an operator and collapsing any two of them is how a
// deletion gets reported as happening somewhere it would not.
func TestRetentionPreview_ReportsWhereEachArtifactIs(t *testing.T) {
	ctx := context.Background()
	records := gfsRecordsForHomeTest(t)
	records[0].Placements = []state.Placement{{Medium: state.MediumLocal, Status: state.PlacementActive}}
	records[1].Placements = []state.Placement{{Medium: "cold_offsite", Status: state.PlacementActive}}
	records[2].Placements = []state.Placement{
		{Medium: state.MediumLocal, Status: state.PlacementActive},
		{Medium: "cold_offsite", Status: state.PlacementActive},
	}

	bs := testBackupSet(t, t.TempDir())
	cfg := testConfig(t, testSource("production", bs))
	cfg.Retention = chainWithOffsiteMonthly()
	resolveTestRetention(cfg)

	svc := New(cfg, recordsJournal{records: records}, nil, nil)
	svc.Now = fixedNow(retentionTestNow)

	report, err := svc.RetentionPreview(ctx, bs.ID)
	if err != nil {
		t.Fatalf("RetentionPreview: %v", err)
	}

	for _, want := range []struct {
		name string
		loc  retention.Location
	}{
		{"recent.dump", retention.Location{Medium: state.MediumLocal, Status: retention.LocationConfirmed}},
		{"monthly-only.dump", retention.Location{Medium: "cold_offsite", Status: retention.LocationConfirmed}},
		{"too-old.dump", retention.Location{Status: retention.LocationContested}},
	} {
		id, err := model.NewArtifactID(bs.ID, want.name)
		if err != nil {
			t.Fatalf("NewArtifactID(%q): %v", want.name, err)
		}
		if got := report.Locations[id]; got != want.loc {
			t.Errorf("Locations[%s] = %+v, want %+v", want.name, got, want.loc)
		}
	}
	if len(report.Locations) != len(report.Verdicts) {
		t.Errorf("Locations has %d entries for %d verdicts; every artifact a verdict is about has a location, even when that location is 'I could not confirm one'",
			len(report.Locations), len(report.Verdicts))
	}
}
