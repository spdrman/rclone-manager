package retention

import (
	"reflect"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// bkAttachMediumValues gives every record a full set of medium-supplied
// values: a placement on a storage medium carrying its own size, its own
// hash, a verification class, and timestamps of its own, all of them
// disagreeing with the artifact's journal truth as loudly as they can.
//
// The hostility is the point. FR-32 says nothing read from a medium may
// move an artifact out of KEEP, widen DELETE, or displace a KEEP
// selection, and the way to test a rule about untrusted input is to make
// the input as untrustworthy as the schema allows.
func bkAttachMediumValues(records []state.Record) []state.Record {
	out := make([]state.Record, len(records))
	copy(out, records)

	// Deliberately wrong in every direction at once: far in the past, far
	// in the future, and a size and hash that belong to nothing.
	longAgo := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	farFuture := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := range out {
		size := int64(1)
		verifiedAt := longAgo
		if i%2 == 0 {
			verifiedAt = farFuture
		}
		out[i].Placements = []state.Placement{
			{
				Medium:            "offsite_s3",
				Location:          "rclone-manager/production/pg/" + out[i].Artifact.Name,
				Size:              &size,
				Hash:              "0000000000000000000000000000000000000000000000000000000000000000",
				HashAlg:           "sha256",
				VerificationClass: state.VerificationExistence,
				VerifiedAt:        &verifiedAt,
				Status:            state.PlacementActive,
				CreatedAt:         longAgo,
				UpdatedAt:         farFuture,
			},
		}
	}
	return out
}

func bkStripPlacements(records []state.Record) []state.Record {
	out := make([]state.Record, len(records))
	copy(out, records)
	for i := range out {
		out[i].Placements = nil
	}
	return out
}

// TestMediumSuppliedValuesNeverShrinkKeep is #215's union invariant
// extended over medium-supplied inputs, which is what EPIC E's FR-32 asks
// for: "stripping every medium-supplied value from the inputs never
// shrinks KEEP".
//
// It asserts something stronger than the issue's own wording, because
// something stronger is true and is worth pinning: the verdicts are
// IDENTICAL, tier for tier, with and without every medium-supplied value.
// Retention reads journal truth and nothing else, so a medium can neither
// take a tier away (which would be a safety failure) nor add one (which
// would make an artifact's retention depend on where its bytes happen to
// be sitting, and would break the bit-identical-verdict-under-movement
// property FR-32 also demands).
func TestMediumSuppliedValuesNeverShrinkKeep(t *testing.T) {
	now := bkAt(t, "2026-08-31T12:00:00Z")
	cfg := bkDefaultChain()
	set := gfsMustSet(t, "medium", "supplied")

	specs := []bkRecSpec{
		{"today.dump", lifecycle.Complete, bkAt(t, "2026-08-31T02:00:00Z"), nil},
		{"yesterday.dump", lifecycle.Complete, bkAt(t, "2026-08-30T02:00:00Z"), bkPtr(bkAt(t, "2026-08-29T23:00:00Z"))},
		{"last-week.dump", lifecycle.Complete, bkAt(t, "2026-08-20T02:00:00Z"), nil},
		{"last-month.dump", lifecycle.Complete, bkAt(t, "2026-07-02T02:00:00Z"), bkPtr(bkAt(t, "2026-07-01T22:00:00Z"))},
		{"ancient.dump", lifecycle.Complete, bkAt(t, "2024-01-02T02:00:00Z"), nil},
	}
	records := bkBuildRecords(t, set, specs)

	withMedium := bkAttachMediumValues(records)
	withoutMedium := bkStripPlacements(withMedium)

	// The positive control this comparison needs: the two input sets must
	// genuinely differ, or an equality assertion over them proves only
	// that reflect.DeepEqual works.
	if reflect.DeepEqual(withMedium, withoutMedium) {
		t.Fatal("attaching medium-supplied values changed nothing about the input records, so comparing the two verdict sets proves nothing")
	}
	if len(withMedium[0].Placements) == 0 {
		t.Fatal("the fixture attached no placements at all")
	}

	verdictsWith, err := GFSDecide(now, cfg, set, withMedium)
	if err != nil {
		t.Fatalf("GFSDecide with medium-supplied values: %v", err)
	}
	verdictsWithout, err := GFSDecide(now, cfg, set, withoutMedium)
	if err != nil {
		t.Fatalf("GFSDecide without them: %v", err)
	}

	with, without := bkTierMap(verdictsWith), bkTierMap(verdictsWithout)

	// The safety direction first, on its own, because it is the one that
	// loses a backup: nothing kept without medium data may be unkept with
	// it, and no tier may be taken away.
	for name, tiers := range without {
		gotTiers, present := with[name]
		if !present {
			t.Errorf("artifact %q has a verdict without medium-supplied values and none with them", name)
			continue
		}
		for _, tier := range tiers {
			found := false
			for _, got := range gotTiers {
				if got == tier {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("artifact %q is kept by %s without medium-supplied values but not with them; nothing read from a medium may move an artifact out of KEEP or displace a KEEP selection", name, tier)
			}
		}
	}

	// Then the stronger equality, which is what makes an artifact's
	// bucketing invariant under movement.
	if !reflect.DeepEqual(with, without) {
		t.Errorf("medium-supplied values changed the retention verdicts:\n with:    %v\n without: %v", with, without)
	}
}

// TestVerdictsAreIdenticalAfterAnArtifactMoves is FR-32's
// bit-identical-under-movement claim in the shape an operator would
// recognise: the same artifact, before and after its bytes move to a
// medium, gets the same verdict.
//
// The spec's planted violation for this guard is "a mutation that rewrites
// the journal's discovery timestamp from the destination object during a
// move". The mutation is planted below by doing exactly that to the input,
// and the test requires the comparison to catch it.
func TestVerdictsAreIdenticalAfterAnArtifactMoves(t *testing.T) {
	now := bkAt(t, "2026-08-31T12:00:00Z")
	cfg := bkDefaultChain()
	set := gfsMustSet(t, "moved", "artifact")

	specs := []bkRecSpec{
		{"a.dump", lifecycle.Complete, bkAt(t, "2026-08-30T02:00:00Z"), nil},
		{"b.dump", lifecycle.Complete, bkAt(t, "2026-07-02T02:00:00Z"), nil},
		{"c.dump", lifecycle.Complete, bkAt(t, "2024-01-02T02:00:00Z"), nil},
	}
	before := bkBuildRecords(t, set, specs)
	after := bkAttachMediumValues(before)

	verdictsBefore, err := GFSDecide(now, cfg, set, before)
	if err != nil {
		t.Fatalf("GFSDecide before the move: %v", err)
	}
	verdictsAfter, err := GFSDecide(now, cfg, set, after)
	if err != nil {
		t.Fatalf("GFSDecide after the move: %v", err)
	}
	if !reflect.DeepEqual(bkTierMap(verdictsBefore), bkTierMap(verdictsAfter)) {
		t.Errorf("moving an artifact changed its retention verdict:\n before: %v\n after:  %v",
			bkTierMap(verdictsBefore), bkTierMap(verdictsAfter))
	}

	// THE PLANTED VIOLATION: a move that re-derives the journal's
	// discovery timestamp from the destination object. Nothing in the
	// product does this; the point is that the comparison above would
	// notice if something started to.
	mutated := bkAttachMediumValues(before)
	for i := range mutated {
		mutated[i].DiscoveredAt = *mutated[i].Placements[0].VerifiedAt
	}
	verdictsMutated, err := GFSDecide(now, cfg, set, mutated)
	if err != nil {
		t.Fatalf("GFSDecide over the mutated records: %v", err)
	}
	if reflect.DeepEqual(bkTierMap(verdictsBefore), bkTierMap(verdictsMutated)) {
		t.Fatal("rewriting the journal's discovery timestamp from a medium-supplied value changed no verdict, so the comparison above is not proven to be able to fail")
	}
}
