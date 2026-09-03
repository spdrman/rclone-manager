// Where a kept artifact currently is, read off the journal.
//
// This adapter lives here rather than in internal/retention, and the
// package boundary is the point. internal/retention decides what is KEPT,
// and FR-32 says nothing a medium reported may reach that decision. The
// way this product holds that is structural rather than careful: there is
// no medium-supplied value in scope in that package at all, so a future
// change cannot reach for one by accident, and
// placement.TestRetentionReadsNoMediumSuppliedValue scans for exactly
// that and fails the build.
//
// PlanHomeMoves is already built for this: it takes the lookup as an
// injected func(ArtifactID) (string, bool) and never sees a placement
// row. Only the adapter that BUILDS that function has to read
// rec.Placements, and the adapter is not a retention decision, it is
// "where is this artifact right now" for a move whose keep/prune verdict
// was already taken. So it belongs next to its caller, which is this
// package, and its tests were already here (homemedium_test.go) before
// the function was.
//
// #387 landed it in internal/retention, where it compiled and behaved
// correctly and quietly cost that package the one property its guard
// exists to assert. Moving it changes no behaviour: same function, same
// semantics, one package up.
package app

import (
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// ActiveMediumFromRecords builds PlanHomeMoves' placement lookup from a
// backup set's journal records (EPIC E FR-29's placement rows, #236).
//
// A placement row means a DURABLE copy, so the reading is:
//
//   - exactly one ACTIVE placement: that is where the artifact is, and
//     the planner may compare it against the home the chain names;
//   - none: this manager cannot confirm where the artifact is. An
//     artifact still transferring deliberately has no row, and a
//     hand-built Record has none either, so absence is never evidence of
//     absence;
//   - more than one: a move is already in flight (FR-30's copy phase
//     leaves the source and the destination both ACTIVE until the source
//     delete lands), and "where is this" has two answers. Planning a
//     second move on top of one already running is exactly the race
//     FR-30's journal exists to make unrepresentable.
//
// A DELETE_PENDING or GONE row is not a location either. It records a
// copy on its way out or already gone, and reading one as a location
// would plan a move FROM somewhere this manager is in the middle of
// emptying.
//
// Two of those readings are "cannot confirm", and both take the same
// branch in the planner: report it, move nothing, leave the artifact
// where it is.
func ActiveMediumFromRecords(records []state.Record) func(model.ArtifactID) (string, bool) {
	byArtifact := make(map[model.ArtifactID][]string, len(records))
	for _, rec := range records {
		var active []string
		for _, p := range rec.Placements {
			if p.Status == state.PlacementActive {
				active = append(active, p.Medium)
			}
		}
		byArtifact[rec.Artifact] = active
	}
	return func(id model.ArtifactID) (string, bool) {
		active, ok := byArtifact[id]
		if !ok || len(active) != 1 {
			return "", false
		}
		return active[0], true
	}
}
