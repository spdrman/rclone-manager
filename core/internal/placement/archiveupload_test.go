package placement_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is about what a move to an archive-class medium COSTS when it
// cannot succeed, which is a different question from whether it is safe.
//
// archivedelete_test.go already proves the safe half: the source copy
// survives, every time, on every one of these paths. Nothing here disputes
// that. The question here is the bill. An archive class has a minimum
// billable duration (AWS charges DEEP_ARCHIVE for 180 days whether or not
// the object lives that long), so an upload that is immediately discarded
// is not a wasted request, it is six months of storage for bytes nobody
// will ever read, and a cycle that does it again tomorrow buys another six
// months on top.
//
// So these tests count. They count uploads, they count deletes, and they
// count the GETs spent finding out something that was knowable for free.

// archiveCycles is how many cycles the loop tests run. Four is enough to
// tell a bounded cost from an unbounded one, and it is the number the
// review that found this measured with.
const archiveCycles = 4

// uploadsPerCycle runs the cycle n times against f and reports the
// CUMULATIVE upload count after each one.
//
// It returns the series rather than a total because the series is the
// finding. A total says "it uploaded a lot"; 2, 4, 6, 8 says "it will keep
// doing that for ever", and those are different bugs with different fixes.
func uploadsPerCycle(t *testing.T, f *fixture, n int) []int {
	t.Helper()
	out := make([]int, 0, n)
	for i := 0; i < n; i++ {
		f.runCycle()
		out = append(out, f.medium.uploadCount())
	}
	return out
}

func series(counts []int) string {
	parts := make([]string, 0, len(counts))
	for _, c := range counts {
		parts = append(parts, fmt.Sprint(c))
	}
	return strings.Join(parts, ", ")
}

// TestAMoveToAnArchiveClassCostsNothingToRefuse is the bug in one number.
//
// A retention tier configured onto an archive class cannot take delivery
// of an artifact: the destination is unreadable the instant it lands, so
// the read-back the medium requires cannot run, so the move never reaches
// VERIFIED. That refusal is correct and #428 is about it.
//
// What is NOT correct is paying for it. Nothing about that outcome depends
// on anything this cycle learns from the endpoint. The storage class is in
// the configuration, the required verification class is in the
// configuration, and the two are incompatible before a byte moves. So the
// honest cost of this configuration is zero uploads, zero deletes and zero
// GETs, in the first cycle and in every cycle after it.
func TestAMoveToAnArchiveClassCostsNothingToRefuse(t *testing.T) {
	f := newFixture(t, fixtureOpts{storageClass: config.StorageClassDeepArchive})
	f.medium.archiveRefusesReads = true

	counts := uploadsPerCycle(t, f, archiveCycles)

	// The unbounded half: whatever this configuration costs, it must not
	// cost more tomorrow. A growing series is a bill with no ceiling.
	if counts[len(counts)-1] != counts[0] {
		t.Errorf("cumulative uploads over %d cycles were %s; a refusal that cannot succeed must not buy another copy every cycle",
			archiveCycles, series(counts))
	}
	// The absolute half: the ceiling is zero, because the answer was
	// knowable from the configuration alone.
	if counts[len(counts)-1] != 0 {
		t.Errorf("%s uploads to %s after %d cycles (series %s); every one of them is billed for the class's %s and then thrown away",
			fmt.Sprint(counts[len(counts)-1]), config.StorageClassDeepArchive, archiveCycles, series(counts),
			"180-day minimum")
	}
	if got := f.medium.deleteCount(); got != 0 {
		t.Errorf("the engine deleted %d objects from %s; it should not have created any", got, testMedium)
	}
	// The GET is the "route it through the gate" half. Verify spends a
	// request to be told InvalidObjectState; VerifyWithAccess works the
	// answer out from facts already held and spends nothing.
	if got := f.medium.openCount(); got != 0 {
		t.Errorf("the engine spent %d GETs against archived objects to learn what the storage class already said", got)
	}
}

// TestTheArchiveRefusalReachesTheOperator is the other half of refusing:
// a tier that silently never receives anything is #428's original
// complaint, and a cheap silent refusal would be the same bug with a
// smaller bill.
func TestTheArchiveRefusalReachesTheOperator(t *testing.T) {
	f := newFixture(t, fixtureOpts{storageClass: config.StorageClassDeepArchive})
	f.medium.archiveRefusesReads = true

	report := f.runCycle()
	f.guard.fail()

	if report.Refused != 1 {
		t.Fatalf("the cycle reported %+v; a move that cannot be planned has to be reported as refused", report)
	}
	why := report.Outcomes[0].Refused
	for _, want := range []string{config.StorageClassDeepArchive, string(placement.Content), "restore"} {
		if !strings.Contains(why, want) {
			t.Errorf("the refusal does not mention %q, so an operator cannot tell what to change: %q", want, why)
		}
	}
	if len(f.moves()) != 0 {
		t.Errorf("a move row was written for a move that was refused before it started: %+v", f.moves())
	}
}

// TestAMoveToAClassThatReadsOnDemandStillCompletes is the positive
// control, and it is the one that keeps the fix from being "stop moving
// anything".
//
// It flips exactly one fact, the storage class, and both rows have to
// finish. GLACIER_IR is in the table on purpose: it is cold, it is cheap,
// its name says Glacier, and it serves reads on demand, so a guard that
// keyed off the word instead of off the Archive flag passes the
// DEEP_ARCHIVE test above and fails here.
func TestAMoveToAClassThatReadsOnDemandStillCompletes(t *testing.T) {
	for _, class := range []string{config.StorageClassStandard, config.StorageClassGlacierIR, ""} {
		name := class
		if name == "" {
			name = "unset(defaults to STANDARD)"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{storageClass: class})
			f.medium.archiveRefusesReads = true

			report := f.runCycle()
			f.guard.fail()

			if report.Completed != 1 {
				t.Fatalf("a move to %s did not complete: %+v", name, report.Outcomes)
			}
			if got := f.medium.uploadCount(); got != 1 {
				t.Errorf("a move to %s uploaded %d times, want exactly 1", name, got)
			}
			if f.localExists() {
				t.Errorf("a move to %s reached DONE with the local source still there", name)
			}
		})
	}
}

// plantCopying leaves a move at COPYING with nothing uploaded, which is
// what a crash between the COPYING write and the upload leaves behind, and
// also what a move planned by a build without the plan-time refusal looks
// like to the build that has one.
func plantCopying(t *testing.T, f *fixture) state.Move {
	t.Helper()
	mv, err := f.journal.PlanMove(f.ctx, state.MovePlan{
		Artifact: f.artifact, SourceMedium: state.MediumLocal,
		DestinationMedium: testMedium, DestinationKey: f.key, OccurredAt: f.clock,
	})
	if err != nil {
		t.Fatalf("planting the move: %v", err)
	}
	f.clock = f.clock.Add(time.Second)
	mv, err = f.journal.AdvanceMove(f.ctx, state.MoveAdvance{
		MoveID: mv.ID, From: state.MovePlanned, To: state.MoveCopying, OccurredAt: f.clock,
	})
	if err != nil {
		t.Fatalf("planting PLANNED -> COPYING: %v", err)
	}
	return mv
}

// TestAMoveAlreadyInFlightToAnArchiveClassStopsAtOneUpload covers the move
// the plan-time refusal cannot see: one that was already past PLANNED when
// the process died, or one planned by a build that did not have the
// refusal, or one whose bucket grew a lifecycle rule after it started.
//
// It cannot cost nothing, because the row already exists and the engine
// finds it at COPYING. It can cost ONE upload and then be over, and the
// distinction is the whole issue: recopyOrAbandon's retry is right for a
// flaky endpoint and wrong for a storage class, because the second attempt
// is identical to the first and so is the thousandth.
func TestAMoveAlreadyInFlightToAnArchiveClassStopsAtOneUpload(t *testing.T) {
	f := newFixture(t, fixtureOpts{storageClass: config.StorageClassDeepArchive})
	f.medium.archiveRefusesReads = true

	plantCopying(t, f)

	report, err := f.engine.RunCycle(f.ctx, nil)
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	f.guard.fail()

	mv := f.onlyMove()
	if placement.Phase(mv.Phase) != placement.Abandoned {
		t.Fatalf("the resumed move ended at %s with %q, want ABANDONED; a class that cannot be verified is not a transient failure. Outcomes: %+v",
			mv.Phase, mv.Error, report.Outcomes)
	}
	if got := f.medium.uploadCount(); got > 1 {
		t.Errorf("the resumed move uploaded %d times before giving up; the second attempt could not have gone any differently from the first", got)
	}
	if !f.localExists() {
		t.Fatal("THE SOURCE WAS DELETED against a destination nothing verified")
	}
	if f.medium.has(f.key) {
		t.Error("the abandoned move left its destination object on the medium")
	}

	// And the next cycle does not start it again.
	before := f.medium.uploadCount()
	f.runCycle()
	if got := f.medium.uploadCount(); got != before {
		t.Errorf("the cycle after the abandonment uploaded again (%d -> %d)", before, got)
	}
}

// TestAnExpiredRestoreDuringAMoveDoesNotLoopEither is the same property on
// the one path that can legitimately get an archive-class move as far as
// SOURCE_DELETE_PENDING: a restore was in effect when the destination was
// verified, and it expired before the source delete.
//
// The phase table has no SOURCE_DELETE_PENDING -> ABANDONED edge, on
// purpose (see phases.go: the destination has a placements row by then, so
// abandoning would mean deleting verified data to tidy up). So this one
// goes back to COPYING, which is FR-30's own restart answer, and the
// bounded cost has to come from the VERIFYING refusal that follows.
func TestAnExpiredRestoreDuringAMoveDoesNotLoopEither(t *testing.T) {
	f := newFixture(t, fixtureOpts{storageClass: config.StorageClassDeepArchive})
	f.medium.archiveRefusesReads = true
	expired := testNow2.Add(-time.Hour)
	f.medium.restore = &transport.RestoreState{ExpiresAt: &expired}

	plantSourceDeletePending(t, f, deleteAttempt{})

	if _, err := f.engine.RunCycle(f.ctx, nil); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	f.guard.fail()

	if !f.localExists() {
		t.Fatal("THE SOURCE WAS DELETED against a destination whose restore had expired")
	}
	if got := f.medium.uploadCount(); got > 1 {
		t.Errorf("an expired restore window cost %d uploads; one re-copy is FR-30's restart answer, more than one is a loop", got)
	}
	src, _ := f.placement(state.MediumLocal)
	if src.Status != state.PlacementActive {
		t.Errorf("the source placement is %s, want ACTIVE: the move gave up and the source is the good copy", src.Status)
	}
}
