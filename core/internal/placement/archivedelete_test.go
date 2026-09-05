package placement_test

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Every test in this file is the data-loss path #241 named, driven through
// the real engine rather than through archive.CheckSourceDelete alone: a
// move whose destination sits on an archive class, where the journal's own
// account of the copy is true and useless, because nothing in it says
// whether anybody can read the bytes today.
//
// # The premise this file used to run on, and why it changed
//
// It used to build a world in which the destination was on DEEP_ARCHIVE
// and the fake served its bytes anyway. That made the guard's eighth
// clause easy to reach, and it made three of the four refusals describe
// something S3 cannot do. A GET of an unrestored archived object answers
// InvalidObjectState (internal/transport/rclone maps it, and its
// medium_test.go pins the mapping), so against a real endpoint
// deleteSource's own re-verification refuses first and the clause never
// speaks. #440 is that finding.
//
// So the fake is archive-honest here now (archiveRefusesReads), and the
// tests are split by WHICH refusal actually fires:
//
//   - an archived copy nobody has restored cannot be re-verified at all.
//     deleteSource's capability branch stands pat and the guard's own
//     refusal is never the sentence the operator reads. That is where
//     nearly all of production lands, and it is the first line of
//     defence;
//   - the eighth clause is what catches the copy that WAS readable when
//     the bytes were read and is not readable any more by the time the
//     guard asks. A restore window lapsing mid-move, or a restore-status
//     call that fails, are the two ways an endpoint does that, and they
//     are the whole reason the clause is answered from facts gathered AT
//     the guard rather than from the read that just happened.
//
// Both keep the source, which is the property that matters and which
// TestNoArchiveWorldEverLeavesOnlyACopyNobodyCanRead asserts over all four
// worlds at once. The tests after it pin which refusal ran, because a
// suite that cannot tell them apart cannot tell whether the clause it
// claims to cover was reached.

// archiveRefusal is the phrase that identifies the guard's eighth clause,
// which is archive.ErrNoRetrievableCopy's own words.
const archiveRefusal = "verified and retrievable right now"

// capabilityRefusal is the phrase that identifies deleteSource's
// capability branch, the refusal in front of the guard.
const capabilityRefusal = "could not be re-verified at"

// archiveWorld is one endpoint that an archive-class destination can
// actually present to this engine at SOURCE_DELETE_PENDING.
type archiveWorld struct {
	name string
	// class is the storage class the destination medium writes with.
	class string
	// setUp arranges what the endpoint will say.
	setUp func(f *fixture)
	// reachesTheGuard is whether the destination's bytes can be read, so
	// that deleteSource gets past its re-verification and the eighth
	// clause is the thing that refuses.
	reachesTheGuard bool
	// access is the access state the guard's refusal has to name, so an
	// operator reads what to do rather than that something went wrong. It
	// is only asserted for a world that reaches the guard: the refusal in
	// front of it is about a read that failed, and it reports the
	// provider's own words for that (InvalidObjectState, and the class),
	// which is a different sentence about a different fact.
	access string
}

// archiveWorlds is the closed list. Between them they cover every access
// state an ACTIVE archive-class destination can hold at this phase:
// requires_restore twice (nobody asked, and a window that lapsed),
// restoring, and unreachable.
var archiveWorlds = []archiveWorld{
	{
		name:   "nobody has asked for a restore",
		class:  config.StorageClassDeepArchive,
		setUp:  func(*fixture) {},
		access: string(archive.RequiresRestore),
	},
	{
		name:  "a restore is running and has not finished",
		class: config.StorageClassGlacier,
		setUp: func(f *fixture) {
			f.medium.restore = &transport.RestoreState{InProgress: true}
		},
		access: string(archive.Restoring),
	},
	{
		name:  "the restore window lapses between the read and the guard",
		class: config.StorageClassDeepArchive,
		setUp: func(f *fixture) {
			until := testNow2.Add(48 * time.Hour)
			f.medium.restore = &transport.RestoreState{ExpiresAt: &until}
			f.medium.afterOpen = func(m *fakeMedium) {
				expired := testNow2.Add(-time.Hour)
				m.restore = &transport.RestoreState{ExpiresAt: &expired}
			}
		},
		reachesTheGuard: true,
		access:          string(archive.RequiresRestore),
	},
	{
		name:  "the restore-status call fails after the bytes were read",
		class: config.StorageClassDeepArchive,
		setUp: func(f *fixture) {
			until := testNow2.Add(48 * time.Hour)
			f.medium.restore = &transport.RestoreState{ExpiresAt: &until}
			f.medium.afterOpen = func(m *fakeMedium) {
				m.restoreErr = errors.New("restore-status: the endpoint reset the connection")
			}
		},
		reachesTheGuard: true,
		access:          string(archive.Unreachable),
	},
}

// newArchiveFixture builds w's world with an archive-honest endpoint and a
// move planted at SOURCE_DELETE_PENDING.
func newArchiveFixture(t *testing.T, w archiveWorld) *fixture {
	t.Helper()
	f := newFixture(t, fixtureOpts{storageClass: w.class})
	f.medium.archiveRefusesReads = true
	w.setUp(f)
	plantSourceDeletePending(t, f, deleteAttempt{})
	return f
}

// TestNoArchiveWorldEverLeavesOnlyACopyNobodyCanRead is the safety claim,
// and it is deliberately indifferent to which refusal produced it.
//
// FR-30's standing invariant is that at no instant may an artifact have no
// confirmed readable copy. The four worlds below are every way an
// archive-class destination can fail to be one at this phase, and in all
// four the local source has to survive, the destination object has to
// survive, and the engine has to say why. Which of the two refusals ran is
// the next two tests' business; this one would still be true if the
// engine's internals were rearranged.
func TestNoArchiveWorldEverLeavesOnlyACopyNobodyCanRead(t *testing.T) {
	for _, w := range archiveWorlds {
		t.Run(w.name, func(t *testing.T) {
			f := newArchiveFixture(t, w)
			refusal := runDeleteAttempt(t, f, deleteAttempt{})

			if !f.medium.has(f.key) {
				t.Error("the destination object was thrown away; a copy the journal believes in is not rubbish because it cannot be read today")
			}
			if got := f.medium.deleteCount(); got != 0 {
				t.Errorf("the engine deleted %d objects over a copy it could not read", got)
			}
			// Whichever refusal ran, it has to name the storage class,
			// because that is the fact an operator acts on: a restore is
			// what makes this move finish and the class is what says how
			// long one takes.
			if !contains(refusal, w.class) {
				t.Errorf("the refusal does not name the storage class that is the reason:\n%s", refusal)
			}
			// The guard's refusal names the access state as well, because
			// "requires_restore" tells an operator to ask for a restore
			// and "unreachable" tells them to look at the endpoint, and
			// those are different afternoons. The capability refusal in
			// front of it reports the endpoint's own answer instead; see
			// the field's comment.
			if w.reachesTheGuard && !contains(refusal, w.access) {
				t.Errorf("the refusal does not name the access state %q, so an operator cannot tell what to do about it:\n%s", w.access, refusal)
			}

			src, _ := f.placement(state.MediumLocal)
			if src.Status != state.PlacementDeletePending {
				t.Errorf("the source placement is %s, want %s: nothing about the world changed, so nothing about the journal should have",
					src.Status, state.PlacementDeletePending)
			}
		})
	}
}

// TestAnArchivedCopyNobodyRestoredIsRefusedBeforeTheGuardEverSpeaks is the
// half of #440 that was being reported as something else.
//
// In the two worlds where nothing has restored the object, the bytes
// cannot be read, so deleteSource's re-verification cannot RUN, and the
// capability branch stands pat. The eighth clause is not what refuses and
// must not be reported as though it were: "no other copy is verified and
// retrievable right now" is a verdict about the copies, and "I could not
// read the destination" is a verdict about the endpoint, and an operator
// who is told the first when the second is true goes looking in the wrong
// place.
//
// The negative assertion is the load-bearing one. Without it this test
// passes against an engine that reaches the clause, which is exactly the
// state the old suite was in.
func TestAnArchivedCopyNobodyRestoredIsRefusedBeforeTheGuardEverSpeaks(t *testing.T) {
	for _, w := range archiveWorlds {
		if w.reachesTheGuard {
			continue
		}
		t.Run(w.name, func(t *testing.T) {
			f := newArchiveFixture(t, w)
			refusal := runDeleteAttempt(t, f, deleteAttempt{})

			if !contains(refusal, capabilityRefusal) {
				t.Errorf("the refusal is not the capability branch's, and an unreadable destination is exactly what that branch is for:\n%s", refusal)
			}
			if contains(refusal, archiveRefusal) {
				t.Errorf("the refusal is the guard's eighth clause; a real endpoint answers InvalidObjectState to this GET, so the clause cannot be reached in this world and a test that sees it is testing a fake that serves archived bytes:\n%s", refusal)
			}
			// One GET, spent being told InvalidObjectState. deleteSource
			// calls Verify rather than VerifyWithAccess on purpose (see
			// verifyCopy), so this request is the price of keeping the
			// eighth clause reachable at all, and it is pinned so that
			// paying it twice would be a decision.
			if got := f.medium.openCount(); got != 1 {
				t.Errorf("the engine spent %d GETs on the destination, want exactly 1", got)
			}
			// The guard is still consulted, and this is what it costs. It
			// is asked because "the source is already gone" is the one
			// answer that changes what happens here and only the guard
			// can give it; see deleteSource's capability branch and
			// TestASourceAlreadyGoneConvergesEvenWhenTheSurvivorIsArchived.
			if got := f.medium.restoreStatusCount(); got != 1 {
				t.Errorf("the medium was asked about a restore %d times, want exactly 1", got)
			}
		})
	}
}

// TestTheEighthClauseIsReachedByAWindowThatLapsesMidMove is the other
// half, and it is the one that keeps the clause a guard rather than dead
// code.
//
// The bytes really were read back and really did re-hash, so every
// preceding refusal in deleteSource is satisfied by facts, not by a fake
// being lenient. Then the guard asks the medium about the restore, and the
// answer no longer shows the copy readable. That gap is the clause's whole
// reason for being asked from freshly gathered facts, and it is the only
// world in which it can speak.
func TestTheEighthClauseIsReachedByAWindowThatLapsesMidMove(t *testing.T) {
	for _, w := range archiveWorlds {
		if !w.reachesTheGuard {
			continue
		}
		t.Run(w.name, func(t *testing.T) {
			f := newArchiveFixture(t, w)
			refusal := runDeleteAttempt(t, f, deleteAttempt{})

			if !contains(refusal, archiveRefusal) {
				t.Errorf("the refusal is not archive's own, so the eighth clause is not what stopped this delete:\n%s", refusal)
			}
			if contains(refusal, capabilityRefusal) {
				t.Errorf("the destination re-verification refused, so the guard was never the thing under test here:\n%s", refusal)
			}
			// The read happened and it succeeded, which is what makes the
			// clause's refusal a second, independent question rather than
			// the same one asked twice.
			if got := f.medium.openCount(); got != 1 {
				t.Errorf("the engine read the destination back %d times, want exactly 1; the clause under test only speaks after a read that worked", got)
			}
			if got := f.medium.restoreStatusCount(); got != 1 {
				t.Errorf("the medium was asked about a restore %d times, want exactly 1; an archive-class survivor has to be asked, and only once", got)
			}
			mv := f.onlyMove()
			if placement.Phase(mv.Phase) != placement.SourceDeletePending {
				t.Errorf("the move moved to %s; a refusal that changes nothing should leave it where it was", mv.Phase)
			}
		})
	}
}

// TestARestoredArchivedCopyLetsTheMoveFinish is the positive control, and
// it changes exactly one fact: the restore stays in effect for the whole
// cycle instead of lapsing partway through it.
//
// It has to finish, or the eighth clause is not a guard, it is a refusal
// to ever move anything to an archive class. And it is what makes the
// restore probe load-bearing: an engine that never asked the medium would
// read the destination as requires_restore in this world too, and refuse.
func TestARestoredArchivedCopyLetsTheMoveFinish(t *testing.T) {
	f := newFixture(t, fixtureOpts{storageClass: config.StorageClassDeepArchive})
	f.medium.archiveRefusesReads = true
	until := testNow2.Add(48 * time.Hour)
	f.medium.restore = &transport.RestoreState{ExpiresAt: &until}

	a := deleteAttempt{}
	plantSourceDeletePending(t, f, a)

	report, err := f.engine.RunCycle(f.ctx, nil)
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	f.guard.fail()

	mv := f.onlyMove()
	if placement.Phase(mv.Phase) != placement.Done {
		t.Fatalf("the move ended at %s with %q; a restored, content-verified copy is exactly what may stand in for the source. Outcomes: %+v", mv.Phase, mv.Error, report.Outcomes)
	}
	if f.localExists() {
		t.Error("the move reached DONE and the local source copy is still there")
	}
	if !f.medium.has(f.key) {
		t.Error("the destination object is gone after a completed move")
	}
	if got := f.medium.restoreStatusCount(); got != 1 {
		t.Errorf("the medium was asked about a restore %d times, want exactly 1", got)
	}
}

// TestACopyOnAnOnDemandClassIsNotAskedAboutARestore pins that the probe is
// spent only where the answer matters. GLACIER_IR is the class that makes
// this worth a test: the name says archive and the class serves on demand.
func TestACopyOnAnOnDemandClassIsNotAskedAboutARestore(t *testing.T) {
	f := newFixture(t, fixtureOpts{storageClass: config.StorageClassGlacierIR})
	f.medium.archiveRefusesReads = true
	report := f.runCycle()
	f.guard.fail()
	if report.Completed != 1 {
		t.Fatalf("a move to an on-demand class did not complete: %+v", report.Outcomes)
	}
	if got := f.medium.restoreStatusCount(); got != 0 {
		t.Errorf("the medium was asked about a restore %d times for a class that reads on demand; the answer could not change anything", got)
	}
}

// TestASourceAlreadyGoneConvergesEvenWhenTheSurvivorIsArchived pins the
// order of the guard's last two clauses, which is an argument made in a
// comment and would otherwise be a comment.
//
// A crash between the source delete landing on disk and the DONE write
// leaves the journal at SOURCE_DELETE_PENDING and a file that no longer
// exists. The medium-specific proof reports that as errSourceAlreadyGone,
// and the answer is to record DONE: the delete happened, and there is
// nothing left to protect. If the readable-survivor clause ran FIRST, an
// unrestored archive-class destination would refuse this move on every
// cycle for ever, over a copy that is already gone, with a source row that
// says DELETE_PENDING about nothing. #372's shape, one machine down.
//
// # Both halves, because only one of them used to be true
//
// The restored half is the world the crash happened in: the source delete
// only ever runs after a re-verification that worked, so a restore was in
// effect at the moment the file went. The unrestored half is that same
// artifact an hour later, after the window expired and before the process
// came back, and it is the one an archive-honest endpoint exposes. With
// the re-verification refusing first, deleteSource used to return before
// the guard could report errSourceAlreadyGone at all, so the move stayed
// at SOURCE_DELETE_PENDING for ever with its source row pointing at a file
// that does not exist. #372's shape again, arrived at from the other side.
func TestASourceAlreadyGoneConvergesEvenWhenTheSurvivorIsArchived(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restore *transport.RestoreState
	}{
		{name: "the restore was still in effect when the process came back", restore: restoreUntil(testNow2.Add(48 * time.Hour))},
		{name: "the restore expired before the process came back", restore: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{storageClass: config.StorageClassDeepArchive})
			f.medium.archiveRefusesReads = true
			f.medium.restore = tc.restore

			a := deleteAttempt{afterPlant: func(t *testing.T, f *fixture) {
				if err := os.Remove(f.localPath()); err != nil {
					t.Fatalf("removing the source to simulate the crash: %v", err)
				}
			}}
			plantSourceDeletePending(t, f, a)

			report, err := f.engine.RunCycle(f.ctx, nil)
			if err != nil {
				t.Fatalf("RunCycle: %v", err)
			}
			mv := f.onlyMove()
			if placement.Phase(mv.Phase) != placement.Done {
				t.Fatalf("a source that was already gone left the move at %s (%q); the delete happened and the journal has to say so. Outcomes: %+v",
					mv.Phase, mv.Error, report.Outcomes)
			}
			if got := f.medium.restoreStatusCount(); got != 0 {
				t.Errorf("the medium was asked about a restore %d times for a delete that had already happened; the medium-specific proof answers before the survivor clause is reached", got)
			}
			if !f.medium.has(f.key) {
				t.Error("the destination object was discarded while it was the artifact's only remaining copy")
			}
			src, _ := f.placement(state.MediumLocal)
			if src.Status != state.PlacementGone {
				t.Errorf("the source placement is %s, want %s: the file is not there and the journal has to stop saying a delete is pending against it",
					src.Status, state.PlacementGone)
			}
		})
	}
}

func restoreUntil(t time.Time) *transport.RestoreState {
	return &transport.RestoreState{ExpiresAt: &t}
}
