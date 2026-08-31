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

// This file covers issue #192: which timestamp puts an artifact in a
// retention bucket.
//
// FR-18 now answers that with two of them. Every tier is evaluated twice
// over the same artifacts, once placing each artifact by the timestamp
// this manager received it and once by the producer's own timestamp
// (state.Record's Remote.ModTime) where the backend reported one and it
// is not after the received timestamp, and KEEP is the union. The tests
// below hold both halves of that to their contract: the producer term
// makes an ingested backlog keep its real multi-tier shape, and it can
// only ever move an artifact from DELETE to KEEP, never the reverse,
// which is what makes FR-8's untrusted-metadata rule survive contact with
// a wrong or hostile clock.

// --- helpers (prefixed bk* so they cannot collide with gfs_test.go's) ---

type bkRecSpec struct {
	name     string
	state    lifecycle.State
	received time.Time
	// producer is the remote object's own modification time as captured
	// at discovery, or nil when the backend never reported one.
	producer *time.Time
}

func bkBuildRecords(t *testing.T, set model.BackupSetID, specs []bkRecSpec) []state.Record {
	t.Helper()
	out := make([]state.Record, 0, len(specs))
	for _, s := range specs {
		rec := state.Record{
			Artifact:     gfsMustArtifact(t, set, s.name),
			State:        string(s.state),
			DiscoveredAt: s.received,
			UpdatedAt:    s.received,
		}
		if s.producer != nil {
			p := *s.producer
			rec.Remote.ModTime = &p
		}
		out = append(out, rec)
	}
	return out
}

// bkStripProducer returns records with every producer timestamp removed,
// which is exactly the input the received-only calculation sees.
func bkStripProducer(records []state.Record) []state.Record {
	out := make([]state.Record, len(records))
	copy(out, records)
	for i := range out {
		out[i].Remote.ModTime = nil
	}
	return out
}

func bkAt(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return ts
}

func bkPtr(ts time.Time) *time.Time { return &ts }

// bkTierMap renders verdicts as name -> tiers, with a nil entry for an
// artifact that nothing kept, so a test can assert the whole shape as one
// typed value instead of counting.
func bkTierMap(verdicts []GFSVerdict) map[string][]GFSTier {
	out := map[string][]GFSTier{}
	for _, v := range verdicts {
		if !v.Keep {
			out[v.Artifact.Name] = nil
			continue
		}
		out[v.Artifact.Name] = v.Tiers
	}
	return out
}

func bkKeptNames(verdicts []GFSVerdict) []string {
	var out []string
	for _, v := range verdicts {
		if v.Keep {
			out = append(out, v.Artifact.Name)
		}
	}
	sort.Strings(out)
	return out
}

// bkDefaultChain is FR-18's default daily/weekly/monthly policy, spelled
// as the three legacy scalars so these tests exercise the same resolution
// path an untouched config file takes.
func bkDefaultChain() config.Retention {
	return config.Retention{
		Timezone:      "UTC",
		WeekStartsOn:  "monday",
		DailyDays:     7,
		WeeklyMonths:  3,
		MonthlyMonths: 12,
	}
}

// --- the decisive case: a backlog ingested in one cycle ---

// TestGFSDecideKeepsTheMultiTierShapeOfABacklogIngestedInOneCycle is issue
// #192's reported case, asserted as typed values rather than counts.
//
// Six artifacts whose backup dates span 500 days are all received in the
// same cycle, milliseconds apart. Under a received-only bucket key they
// collapse into one daily, one weekly and one monthly bucket, each tier
// selects a single representative, and five of the six become delete
// candidates on the first retention pass. Under FR-18's two-key union
// each artifact is also offered to the buckets its own backup date falls
// in, so the chain keeps the shape it exists to keep.
//
// Every expected tier list below is the union of the two passes:
//
//	received pass (all six placed on 2026-08-31, so one champion per
//	bucket, resolved on the artifact-name tie-break): pg-2026-08-31.dump
//	producer pass: DAILY  buckets 08-31, 08-28
//	               WEEKLY buckets 2026-08-31, 2026-08-24, 2026-08-10, 2026-07-06
//	               MONTHLY buckets 2026-08-01, 2026-07-01, 2026-02-01
//
// pg-2025-04-18.dump is kept by neither: 500 days puts it outside the
// longest configured window, which FR-18 has always said is a delete
// candidate. FR-19 protection lands on pg-2026-08-31.dump, the newest
// restore point by its own date, not the newest by arrival.
func TestGFSDecideKeepsTheMultiTierShapeOfABacklogIngestedInOneCycle(t *testing.T) {
	set := gfsMustSet(t, "production", "postgres-primary")
	now := bkAt(t, "2026-08-31T12:00:00Z")
	received := bkAt(t, "2026-08-31T09:00:00Z")

	records := bkBuildRecords(t, set, []bkRecSpec{
		{"pg-2026-08-31.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2026-08-31T09:00:00Z"))},
		{"pg-2026-08-28.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2026-08-28T09:00:00Z"))},
		{"pg-2026-08-11.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2026-08-11T09:00:00Z"))},
		{"pg-2026-07-12.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2026-07-12T09:00:00Z"))},
		{"pg-2026-02-12.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2026-02-12T09:00:00Z"))},
		{"pg-2025-04-18.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2025-04-18T09:00:00Z"))},
	})

	verdicts, lkg, err := DecideKeep(now, bkDefaultChain(), set, records)
	if err != nil {
		t.Fatalf("DecideKeep: %v", err)
	}

	want := map[string][]GFSTier{
		"pg-2025-04-18.dump": nil,
		"pg-2026-02-12.dump": {GFSMonthly},
		"pg-2026-07-12.dump": {GFSWeekly, GFSMonthly},
		"pg-2026-08-11.dump": {GFSWeekly},
		"pg-2026-08-28.dump": {GFSDaily, GFSWeekly},
		"pg-2026-08-31.dump": {GFSDaily, GFSWeekly, GFSMonthly, TierLastKnownGood},
	}
	if got := bkTierMap(verdicts); !reflect.DeepEqual(got, want) {
		t.Errorf("DecideKeep verdicts:\n got  %v\n want %v", got, want)
	}

	if !lkg.Protected || lkg.Artifact.Name != "pg-2026-08-31.dump" {
		t.Errorf("last-known-good = %q (protected=%v), want %q: FR-19 protects the newest restore point by its own date",
			lkg.Artifact.Name, lkg.Protected, "pg-2026-08-31.dump")
	}
}

// TestDecideKeepOnTheReportedBacklogNamesTheRealNewestRestorePoint uses
// issue #192's own fixture names, where the artifact with the
// lexicographically largest name is also the oldest backup in the set.
// That combination is what produced the report's headline symptom: the
// received-only key put every artifact in one bucket, the name tie-break
// picked the 500-day-old file as every bucket's representative, and
// FR-19 then described that same file as "the newest eligible restore
// point".
//
// Two things have to be true now. Protection lands on the newest backup
// rather than the largest name, and nothing in the set is proposed for
// deletion, because every one of these six is the newest artifact in some
// bucket of some tier under one of the two placements.
func TestDecideKeepOnTheReportedBacklogNamesTheRealNewestRestorePoint(t *testing.T) {
	set := gfsMustSet(t, "production", "postgres-primary")
	now := bkAt(t, "2026-08-31T12:00:00Z")
	received := bkAt(t, "2026-08-31T09:00:00Z")

	records := bkBuildRecords(t, set, []bkRecSpec{
		{"pg-age-000d.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2026-08-31T09:00:00Z"))},
		{"pg-age-003d.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2026-08-28T09:00:00Z"))},
		{"pg-age-020d.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2026-08-11T09:00:00Z"))},
		{"pg-age-050d.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2026-07-12T09:00:00Z"))},
		{"pg-age-200d.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2026-02-12T09:00:00Z"))},
		{"pg-age-500d.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2025-04-18T09:00:00Z"))},
	})

	verdicts, lkg, err := DecideKeep(now, bkDefaultChain(), set, records)
	if err != nil {
		t.Fatalf("DecideKeep: %v", err)
	}

	if !lkg.Protected {
		t.Fatalf("nothing is protected, want the newest restore point protected")
	}
	if lkg.Artifact.Name != "pg-age-000d.dump" {
		t.Errorf("last-known-good = %q, want %q: the newest restore point is the newest backup, not the largest name",
			lkg.Artifact.Name, "pg-age-000d.dump")
	}

	wantKept := []string{
		"pg-age-000d.dump", "pg-age-003d.dump", "pg-age-020d.dump",
		"pg-age-050d.dump", "pg-age-200d.dump", "pg-age-500d.dump",
	}
	if got := bkKeptNames(verdicts); !reflect.DeepEqual(got, wantKept) {
		t.Errorf("kept = %v, want %v: an ingested backlog must not turn into five delete candidates", got, wantKept)
	}
}

// --- the FR-8 invariant: untrusted input may only add ---

// TestGFSDecideProducerTimestampOnlyEverAddsToKeep is the safety property
// the whole design rests on, and it is asserted per artifact and per tier
// rather than only over the set of names: for any records, every (artifact,
// tier) pair the received-only calculation produces is still produced when
// producer timestamps are read. No producer timestamp, absent or wrong or
// hostile, can take a tier away from an artifact, which is what lets FR-18
// read an untrusted value at all.
//
// Name-level monotonicity alone would be too weak to see the failure that
// matters: a merged or replacing implementation can leave an artifact in
// KEEP while silently moving it out of the tier that was actually holding
// it, and the next config change to that tier then deletes it.
//
// The last assertion is the positive control this test needs to be worth
// running. Without it a calculation that ignored producer timestamps
// entirely would satisfy every superset check trivially, so at least one
// case has to keep strictly more, proving the producer term is observable
// through this channel at all.
func TestGFSDecideProducerTimestampOnlyEverAddsToKeep(t *testing.T) {
	now := bkAt(t, "2026-08-31T12:00:00Z")
	cfg := bkDefaultChain()

	// A deterministic spread of received/producer offsets, built to cover
	// same-day, cross-day, cross-week, cross-month and far-outside-every-
	// window disagreements between the two timestamps in one table.
	receivedOffsets := []int{0, 0, 0, 1, 2, 9, 40, 40, 120, 400}
	producerLag := []int{0, 3, 500, 0, 20, 1, 200, 0, 7, 95}

	pairs := func(verdicts []GFSVerdict) map[string]map[GFSTier]bool {
		out := map[string]map[GFSTier]bool{}
		for _, v := range verdicts {
			set := map[GFSTier]bool{}
			for _, tier := range v.Tiers {
				set[tier] = true
			}
			out[v.Artifact.Name] = set
		}
		return out
	}
	count := func(m map[string]map[GFSTier]bool) int {
		n := 0
		for _, tiers := range m {
			n += len(tiers)
		}
		return n
	}

	strictlyLarger := 0
	check := func(caseName string, specs []bkRecSpec) {
		t.Helper()
		set := gfsMustSet(t, "monotone", "case")
		records := bkBuildRecords(t, set, specs)

		withProducer, err := GFSDecide(now, cfg, set, records)
		if err != nil {
			t.Fatalf("%s: GFSDecide with producer timestamps: %v", caseName, err)
		}
		withoutProducer, err := GFSDecide(now, cfg, set, bkStripProducer(records))
		if err != nil {
			t.Fatalf("%s: GFSDecide without producer timestamps: %v", caseName, err)
		}

		with, without := pairs(withProducer), pairs(withoutProducer)
		for name, tiers := range without {
			for tier := range tiers {
				if !with[name][tier] {
					t.Errorf("%s: artifact %q is kept by %s without producer timestamps but not with them; an untrusted producer timestamp must never take a tier away from an artifact",
						caseName, name, tier)
				}
			}
		}
		if count(with) > count(without) {
			strictlyLarger++
		}
	}

	for caseIdx, batch := range [][]int{{0, 1, 2, 3, 4}, {5, 6, 7, 8, 9}, {0, 2, 4, 6, 8}, {1, 3, 5, 7, 9}, {0, 1, 2, 3, 4, 5, 6, 7, 8, 9}} {
		var specs []bkRecSpec
		for _, i := range batch {
			received := now.AddDate(0, 0, -receivedOffsets[i]).Add(-time.Duration(i) * time.Hour)
			producer := received.AddDate(0, 0, -producerLag[i])
			specs = append(specs, bkRecSpec{
				name:     "artifact-" + string(rune('a'+i)) + ".dump",
				state:    lifecycle.Complete,
				received: received,
				producer: bkPtr(producer),
			})
		}
		check(fmt.Sprintf("table case %d", caseIdx), specs)
	}

	// The table above cannot generate the one shape that separates a
	// union from a single merged champion map: an artifact whose producer
	// placement lands late in a day another artifact was received into
	// early. Written out by hand so this property covers it too.
	check("late producer, early arrival, same bucket", []bkRecSpec{
		{"q-early.dump", lifecycle.Complete, bkAt(t, "2026-08-30T09:00:00Z"), nil},
		{"p-late.dump", lifecycle.Complete, bkAt(t, "2026-08-31T11:00:00Z"), bkPtr(bkAt(t, "2026-08-30T23:00:00Z"))},
		{"r-newest.dump", lifecycle.Complete, bkAt(t, "2026-08-31T11:30:00Z"), nil},
	})

	if strictlyLarger == 0 {
		t.Error("no case kept more with producer timestamps than without: the producer term is not observable through this channel at all, so the superset assertions above prove nothing")
	}
}

// TestGFSDecideProducerPlacementNeverDisplacesAReceivedRepresentative is
// the structural reason the two passes are kept separate instead of being
// folded into one champion map per bucket.
//
// "p-late.dump" was produced at 23:00 on the 30th and received at 12:00 on
// the 31st, so its producer placement lands in the 30th's bucket, the same
// bucket "q-early.dump" was received into at 09:00. In a single merged
// map p's producer placement is the later instant and takes that bucket,
// and q, which the received-only calculation kept, silently stops being
// kept. Two passes unioned afterwards make that impossible.
//
// "r-newest.dump" is here so that p is kept by nothing but its own
// producer placement, which makes this test RED against a calculation
// that ignores producer timestamps as well as against a merged one.
func TestGFSDecideProducerPlacementNeverDisplacesAReceivedRepresentative(t *testing.T) {
	set := gfsMustSet(t, "displace", "bucket")
	now := bkAt(t, "2026-08-31T13:00:00Z")
	cfg := bkDefaultChain()

	records := bkBuildRecords(t, set, []bkRecSpec{
		{"q-early.dump", lifecycle.Complete, bkAt(t, "2026-08-30T09:00:00Z"), nil},
		{"p-late.dump", lifecycle.Complete, bkAt(t, "2026-08-31T12:00:00Z"), bkPtr(bkAt(t, "2026-08-30T23:00:00Z"))},
		{"r-newest.dump", lifecycle.Complete, bkAt(t, "2026-08-31T12:30:00Z"), nil},
	})

	verdicts, err := GFSDecide(now, cfg, set, records)
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}
	want := map[string][]GFSTier{
		"q-early.dump":  {GFSDaily, GFSWeekly},
		"p-late.dump":   {GFSDaily, GFSWeekly, GFSMonthly},
		"r-newest.dump": {GFSDaily, GFSWeekly, GFSMonthly},
	}
	if got := bkTierMap(verdicts); !reflect.DeepEqual(got, want) {
		t.Errorf("GFSDecide verdicts:\n got  %v\n want %v", got, want)
	}
}

// TestGFSDecideBackDatedProducerTimestampsChangeNothing is the hostile
// case FR-8 exists for: a producer whose clock says 1990 stamps every
// artifact in the set. Every producer placement then falls outside every
// tier window and contributes nothing, so the verdicts have to be
// identical to the ones the received timestamps alone produce, not
// emptier.
//
// The positive control is the second half: the same records with
// plausible producer timestamps (inside the windows) do change the
// verdicts, which proves this test is looking at a channel that can move.
func TestGFSDecideBackDatedProducerTimestampsChangeNothing(t *testing.T) {
	set := gfsMustSet(t, "hostile", "clock")
	now := bkAt(t, "2026-08-31T12:00:00Z")
	cfg := bkDefaultChain()

	base := []bkRecSpec{
		{"a.dump", lifecycle.Complete, bkAt(t, "2026-08-31T01:00:00Z"), nil},
		{"b.dump", lifecycle.Complete, bkAt(t, "2026-08-31T02:00:00Z"), nil},
		{"c.dump", lifecycle.Complete, bkAt(t, "2026-08-30T02:00:00Z"), nil},
	}
	receivedOnly, err := GFSDecide(now, cfg, set, bkBuildRecords(t, set, base))
	if err != nil {
		t.Fatalf("GFSDecide on received timestamps alone: %v", err)
	}

	hostile := make([]bkRecSpec, len(base))
	copy(hostile, base)
	for i := range hostile {
		hostile[i].producer = bkPtr(bkAt(t, "1990-01-0"+string(rune('1'+i))+"T00:00:00Z"))
	}
	got, err := GFSDecide(now, cfg, set, bkBuildRecords(t, set, hostile))
	if err != nil {
		t.Fatalf("GFSDecide with back-dated producer timestamps: %v", err)
	}
	if !reflect.DeepEqual(bkTierMap(got), bkTierMap(receivedOnly)) {
		t.Errorf("back-dated producer timestamps changed the verdicts:\n got  %v\n want %v",
			bkTierMap(got), bkTierMap(receivedOnly))
	}

	// Positive control: a producer timestamp inside the windows is
	// observable here, so the equality above is a real assertion about
	// back-dating and not about the whole channel being inert.
	plausible := make([]bkRecSpec, len(base))
	copy(plausible, base)
	plausible[0].producer = bkPtr(bkAt(t, "2026-08-27T01:00:00Z"))
	moved, err := GFSDecide(now, cfg, set, bkBuildRecords(t, set, plausible))
	if err != nil {
		t.Fatalf("GFSDecide with a plausible producer timestamp: %v", err)
	}
	if reflect.DeepEqual(bkTierMap(moved), bkTierMap(receivedOnly)) {
		t.Errorf("a producer timestamp inside the daily window changed nothing (%v): the observation channel does not work, so the back-dating assertion above proves nothing",
			bkTierMap(moved))
	}
}

// TestGFSDecideRefusesAProducerTimestampAfterTheReceivedTimestamp pins the
// admissibility gate. A completed artifact cannot have been produced
// after the moment this manager first observed it, so a producer
// timestamp in that range is a wrong or forged clock and is refused
// outright rather than clamped: clamping would manufacture a date this
// manager has no evidence for.
//
// Both artifacts arrive in the same cycle, and "z-arrival.dump" carries
// the larger name, so the received pass keeps it and only it. Whether
// "m-backdated.dump" survives at all is therefore decided entirely by
// whether its producer timestamp was admitted, which is what makes this
// fixture able to see the gate.
//
// The second half is the positive control. The refused timestamp moved to
// before the received timestamp is admitted, and "m-backdated.dump"
// appears in KEEP, so the refusal above is a refusal and not a channel
// that never worked.
func TestGFSDecideRefusesAProducerTimestampAfterTheReceivedTimestamp(t *testing.T) {
	set := gfsMustSet(t, "future", "clock")
	now := bkAt(t, "2026-08-31T12:00:00Z")
	cfg := bkDefaultChain()
	received := bkAt(t, "2026-08-31T09:00:00Z")

	specs := func(producer *time.Time) []bkRecSpec {
		return []bkRecSpec{
			{"m-backdated.dump", lifecycle.Complete, received, producer},
			{"z-arrival.dump", lifecycle.Complete, received, nil},
		}
	}

	gotNone, err := GFSDecide(now, cfg, set, bkBuildRecords(t, set, specs(nil)))
	if err != nil {
		t.Fatalf("GFSDecide with no producer timestamps: %v", err)
	}
	wantNone := map[string][]GFSTier{
		"m-backdated.dump": nil,
		"z-arrival.dump":   {GFSDaily, GFSWeekly, GFSMonthly},
	}
	if got := bkTierMap(gotNone); !reflect.DeepEqual(got, wantNone) {
		t.Fatalf("baseline without producer timestamps:\n got  %v\n want %v", got, wantNone)
	}

	// One second after its own received timestamp: refused, so the
	// verdicts have to be the baseline's exactly.
	gotRefused, err := GFSDecide(now, cfg, set, bkBuildRecords(t, set, specs(bkPtr(received.Add(time.Second)))))
	if err != nil {
		t.Fatalf("GFSDecide with a future-dated producer timestamp: %v", err)
	}
	if got := bkTierMap(gotRefused); !reflect.DeepEqual(got, wantNone) {
		t.Errorf("a producer timestamp after the received timestamp was not refused:\n got  %v\n want %v", got, wantNone)
	}

	// Positive control: the previous day is admissible, and places
	// "m-backdated.dump" in its own daily, weekly and monthly buckets.
	gotAdmitted, err := GFSDecide(now, cfg, set, bkBuildRecords(t, set, specs(bkPtr(bkAt(t, "2026-08-30T09:00:00Z")))))
	if err != nil {
		t.Fatalf("GFSDecide with an admissible producer timestamp: %v", err)
	}
	wantAdmitted := map[string][]GFSTier{
		"m-backdated.dump": {GFSDaily, GFSWeekly, GFSMonthly},
		"z-arrival.dump":   {GFSDaily, GFSWeekly, GFSMonthly},
	}
	if got := bkTierMap(gotAdmitted); !reflect.DeepEqual(got, wantAdmitted) {
		t.Errorf("an admissible producer timestamp did not place the artifact:\n got  %v\n want %v", got, wantAdmitted)
	}
}

// TestGFSDecideIgnoresAZeroProducerTimestamp: a journal row whose stored
// remote mtime is the zero time is not a date, it is a missing value that
// happens to be non-nil. It must be refused the same way an absent one
// is, so FR-19 can never describe an artifact as "dated 0001-01-01".
//
// a.dump is deliberately the artifact the received pass does not keep (it
// arrived an hour before b.dump, into the same buckets), so a producer
// placement for it is observable in the verdicts. The positive control at
// the end proves exactly that: swap the zero time for a real one and
// a.dump appears in KEEP, so the equality above is a statement about the
// zero time and not about a channel that never carried anything.
func TestGFSDecideIgnoresAZeroProducerTimestamp(t *testing.T) {
	set := gfsMustSet(t, "zero", "mtime")
	now := bkAt(t, "2026-08-31T12:00:00Z")
	cfg := bkDefaultChain()
	early := bkAt(t, "2026-08-31T08:00:00Z")
	late := bkAt(t, "2026-08-31T09:00:00Z")

	specs := func(producer *time.Time) []bkRecSpec {
		return []bkRecSpec{
			{"a.dump", lifecycle.Complete, early, producer},
			{"b.dump", lifecycle.Complete, late, nil},
		}
	}

	withNone, err := GFSDecide(now, cfg, set, bkBuildRecords(t, set, specs(nil)))
	if err != nil {
		t.Fatalf("GFSDecide with no producer timestamps: %v", err)
	}
	zero := time.Time{}
	withZero, err := GFSDecide(now, cfg, set, bkBuildRecords(t, set, specs(&zero)))
	if err != nil {
		t.Fatalf("GFSDecide with a zero producer timestamp: %v", err)
	}
	if !reflect.DeepEqual(bkTierMap(withZero), bkTierMap(withNone)) {
		t.Errorf("a zero producer timestamp was treated as a date:\n got  %v\n want %v",
			bkTierMap(withZero), bkTierMap(withNone))
	}

	// Positive control.
	real := bkAt(t, "2026-08-29T08:00:00Z")
	withReal, err := GFSDecide(now, cfg, set, bkBuildRecords(t, set, specs(&real)))
	if err != nil {
		t.Fatalf("GFSDecide with a real producer timestamp: %v", err)
	}
	if reflect.DeepEqual(bkTierMap(withReal), bkTierMap(withNone)) {
		t.Errorf("a real producer timestamp on the same artifact changed nothing (%v): the observation channel does not work, so the zero-time assertion above proves nothing",
			bkTierMap(withReal))
	}

	// FR-19 is where a zero producer timestamp does real damage if it is
	// admitted: it resolves as the artifact's retention date, sorts as
	// the oldest thing in the universe, and prints as "dated
	// 0001-01-01T00:00:00Z by the producer's own timestamp".
	lkg, err := LastKnownGoodDecide(config.Retention{}, set, bkBuildRecords(t, set, []bkRecSpec{
		{"a.dump", lifecycle.Complete, early, &zero},
	}))
	if err != nil {
		t.Fatalf("LastKnownGoodDecide with a zero producer timestamp: %v", err)
	}
	if !strings.Contains(lkg.Reason, early.UTC().Format(time.RFC3339)) || !strings.Contains(lkg.Reason, "received") {
		t.Errorf("Reason %q: a zero producer timestamp must fall back to the received timestamp, not be reported as a date", lkg.Reason)
	}
}

// TestGFSDecideAlwaysKeepsTheMostRecentlyReceivedArtifact is the property
// that makes the FR-19 ordering change safe. Whatever any producer claims,
// the artifact this manager received most recently is placed by the
// received pass on today's date, and today falls inside every enabled
// tier's window by construction, so it is always some bucket's newest
// representative and always in KEEP.
//
// The positive control is the strict-superset check at the end: the
// hostile producer timestamps used here do change the verdict set, so the
// invariant above is being asserted against a calculation that is
// genuinely reading them.
func TestGFSDecideAlwaysKeepsTheMostRecentlyReceivedArtifact(t *testing.T) {
	set := gfsMustSet(t, "newest", "arrival")
	now := bkAt(t, "2026-08-31T12:00:00Z")
	cfg := bkDefaultChain()

	// "newest.dump" is received last, and its producer back-dates it into
	// the previous year to try to push it out of every window.
	records := bkBuildRecords(t, set, []bkRecSpec{
		{"older-1.dump", lifecycle.Complete, bkAt(t, "2026-08-29T01:00:00Z"), bkPtr(bkAt(t, "2026-08-29T00:00:00Z"))},
		{"older-2.dump", lifecycle.Complete, bkAt(t, "2026-08-30T01:00:00Z"), bkPtr(bkAt(t, "2026-08-20T00:00:00Z"))},
		{"newest.dump", lifecycle.Complete, bkAt(t, "2026-08-31T09:00:00Z"), bkPtr(bkAt(t, "2025-01-02T00:00:00Z"))},
	})

	verdicts, err := GFSDecide(now, cfg, set, records)
	if err != nil {
		t.Fatalf("GFSDecide: %v", err)
	}
	tiers := bkTierMap(verdicts)
	if len(tiers["newest.dump"]) == 0 {
		t.Fatalf("the most recently received artifact is not kept (%v); a back-dated producer timestamp must never be able to push it out of KEEP", tiers)
	}

	stripped, err := GFSDecide(now, cfg, set, bkStripProducer(records))
	if err != nil {
		t.Fatalf("GFSDecide without producer timestamps: %v", err)
	}
	if reflect.DeepEqual(bkTierMap(stripped), tiers) {
		t.Errorf("stripping every producer timestamp changed nothing (%v): this test is not observing the producer term at all", tiers)
	}
}

// --- FR-19 ---

// TestLastKnownGoodProtectsTheNewestBackupNotTheNewestArrival holds FR-19
// to the same reading FR-18 now uses. "The newest known-good restore
// point" is the newest by the artifact's own retention date, so a backlog
// ingested in one cycle protects the most recent backup in it rather than
// whichever artifact happened to win a name tie-break.
func TestLastKnownGoodProtectsTheNewestBackupNotTheNewestArrival(t *testing.T) {
	set := gfsMustSet(t, "lkg", "backlog")
	received := bkAt(t, "2026-08-31T09:00:00Z")

	records := bkBuildRecords(t, set, []bkRecSpec{
		// zzz sorts last, so under a received-only ordering with a name
		// tie-break this oldest backup would be "the newest".
		{"zzz-oldest.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2025-04-18T09:00:00Z"))},
		{"aaa-newest.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2026-08-31T08:00:00Z"))},
	})

	got, err := LastKnownGoodDecide(config.Retention{}, set, records)
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	if !got.Protected {
		t.Fatalf("Protected = false, want true")
	}
	if got.Artifact.Name != "aaa-newest.dump" {
		t.Errorf("Artifact = %q, want %q", got.Artifact.Name, "aaa-newest.dump")
	}
}

// TestLastKnownGoodReasonNamesTheDateAndWhereItCameFrom: an operator
// reading the retention preview has to be able to tell which of the two
// timestamps decided, because that is precisely the ambiguity #192 was
// filed about. The reason line carries the resolved date and its
// provenance in both directions.
func TestLastKnownGoodReasonNamesTheDateAndWhereItCameFrom(t *testing.T) {
	set := gfsMustSet(t, "lkg", "reason")
	received := bkAt(t, "2026-08-31T09:00:00Z")

	fromProducer, err := LastKnownGoodDecide(config.Retention{}, set, bkBuildRecords(t, set, []bkRecSpec{
		{"a.dump", lifecycle.Complete, received, bkPtr(bkAt(t, "2026-08-20T04:05:06Z"))},
	}))
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	if !strings.Contains(fromProducer.Reason, "2026-08-20T04:05:06Z") {
		t.Errorf("Reason %q does not carry the resolved retention date", fromProducer.Reason)
	}
	if !strings.Contains(fromProducer.Reason, "producer") {
		t.Errorf("Reason %q does not say the date came from the producer's own timestamp", fromProducer.Reason)
	}

	fromReceived, err := LastKnownGoodDecide(config.Retention{}, set, bkBuildRecords(t, set, []bkRecSpec{
		{"a.dump", lifecycle.Complete, received, nil},
	}))
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	if !strings.Contains(fromReceived.Reason, "2026-08-31T09:00:00Z") {
		t.Errorf("Reason %q does not carry the resolved retention date", fromReceived.Reason)
	}
	if !strings.Contains(fromReceived.Reason, "received") {
		t.Errorf("Reason %q does not say the date is the moment this manager received the artifact", fromReceived.Reason)
	}
}

// TestLastKnownGoodIgnoresAProducerTimestampAfterTheReceivedTimestamp:
// FR-19 is the last line of defence, so the admissibility gate has to
// apply here too. Otherwise a producer could claim tomorrow's date and
// move protection onto an artifact of its choosing.
//
// The positive control is the second half: the same artifact with an
// admissible, genuinely newer producer timestamp does take protection.
func TestLastKnownGoodIgnoresAProducerTimestampAfterTheReceivedTimestamp(t *testing.T) {
	set := gfsMustSet(t, "lkg", "future")

	forged, err := LastKnownGoodDecide(config.Retention{}, set, bkBuildRecords(t, set, []bkRecSpec{
		{"genuine.dump", lifecycle.Complete, bkAt(t, "2026-08-31T09:00:00Z"), nil},
		{"forged.dump", lifecycle.Complete, bkAt(t, "2026-08-20T09:00:00Z"), bkPtr(bkAt(t, "2027-01-01T00:00:00Z"))},
	}))
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	if forged.Artifact.Name != "genuine.dump" {
		t.Errorf("Artifact = %q, want %q: a producer timestamp after the received timestamp must never win FR-19 protection",
			forged.Artifact.Name, "genuine.dump")
	}

	// The received order and the producer order deliberately disagree
	// here: "genuine.dump" arrived a day later, so a received-only
	// ordering picks it, and only a calculation that actually reads the
	// producer timestamps picks "newer.dump".
	admissible, err := LastKnownGoodDecide(config.Retention{}, set, bkBuildRecords(t, set, []bkRecSpec{
		{"genuine.dump", lifecycle.Complete, bkAt(t, "2026-08-21T09:00:00Z"), bkPtr(bkAt(t, "2026-08-01T09:00:00Z"))},
		{"newer.dump", lifecycle.Complete, bkAt(t, "2026-08-20T09:00:00Z"), bkPtr(bkAt(t, "2026-08-19T09:00:00Z"))},
	}))
	if err != nil {
		t.Fatalf("LastKnownGoodDecide: %v", err)
	}
	if admissible.Artifact.Name != "newer.dump" {
		t.Errorf("Artifact = %q, want %q: an admissible producer timestamp must be able to decide FR-19, or the refusal above proves nothing",
			admissible.Artifact.Name, "newer.dump")
	}
}
