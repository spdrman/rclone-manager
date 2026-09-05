package placement_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file is issue #439: a move at content class read the whole
// artifact back TWICE, once to reach VERIFIED and once again in
// deleteSource immediately before the source delete, so moving a 100 GB
// artifact cost 200 GB of egress and the operator-facing cost table said
// "a full download", singular.
//
// The second read was never redundant, and the tests here are arranged so
// that a change which merely deletes it fails. Two claims have to hold at
// once:
//
//   - the count drops, which is what the issue measured
//     (TestACompletedMoveReadsTheObjectBackOnce); and
//   - a destination that goes bad between the read and the delete still
//     keeps the source, which is what the second read was FOR
//     (TestADestinationThatGoesBadBetweenTheReadAndTheDeleteKeepsTheSource).
//
// A change that halves the cost and drops the protection passes the first
// and fails the second, which is the whole reason the second one is here.

// errTheEndpointWentAway is a medium that answered a moment ago and does
// not answer now. It is deliberately not classified as NotFound: "the
// object is not there" and "I could not ask" are different facts, and the
// pre-delete proof treats both as "no identity", which is the only safe
// reading of either.
var errTheEndpointWentAway = errors.New("stat: the endpoint reset the connection")

// --- the cost -----------------------------------------------------------

// TestACompletedMoveReadsTheObjectBackOnce is the measurement from #439,
// inverted.
//
// It used to be TestACompletedMoveReadsTheObjectBackTwice and it pinned
// the cost rather than arguing with it. What changed is not that the
// re-verification went away: deleteSource still asks for a proof about the
// destination immediately before the delete, unconditionally, and takes
// the same branch on the answer. What changed is where the proof can come
// from. The read this process performed a moment ago, about an object
// whose size, mod time and storage class the medium still reports
// unchanged, is that proof. Anything else is a full read.
//
// docs/storage-mediums.md quotes this number to operators budgeting
// egress, so it has to change with this test.
func TestACompletedMoveReadsTheObjectBackOnce(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	report := f.runCycle()
	f.guard.fail()
	if report.Completed != 1 {
		t.Fatalf("the move did not complete, so these counts are of something else: %+v", report.Outcomes)
	}

	if got := f.medium.uploadCount(); got != 1 {
		t.Errorf("the move uploaded %d times, want 1", got)
	}
	if got := f.medium.openCount(); got != 1 {
		t.Errorf("the move read the object back %d times, want 1 (verifyDestination, and deleteSource standing on that read). "+
			"If this is 2, the pre-delete proof is being rejected and every move is paying an extra artifact's worth of egress; "+
			"if it is 0, nothing read the destination at all and VERIFIED means nothing. "+
			"docs/storage-mediums.md quotes this number to operators budgeting egress and has to change with it", got)
	}
	if f.localExists() {
		t.Error("the local source copy is still there after a completed move")
	}
}

// TestTheSecondReadWasReplacedByTwoHeads is the other half of the cost
// claim, and it is here so the saving is stated rather than assumed.
//
// A full read became two metadata calls: one immediately before the read,
// which is what the proof is ABOUT, and one immediately before the delete,
// which is what says the object has not changed since. Both are HEADs.
// Neither moves a byte of the artifact. Pinning the number means a third
// one has to be argued for, the same way the second read did.
func TestTheSecondReadWasReplacedByTwoHeads(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	report := f.runCycle()
	f.guard.fail()
	if report.Completed != 1 {
		t.Fatalf("the move did not complete: %+v", report.Outcomes)
	}
	if got := f.medium.statCount(); got != 2 {
		t.Errorf("the move spent %d metadata calls on the destination, want 2: one before the read that produces the proof, "+
			"one before the delete that says the object has not changed since. A third is a request nobody has written down", got)
	}
}

// --- the protection -----------------------------------------------------

// TestADestinationThatGoesBadBetweenTheReadAndTheDeleteKeepsTheSource is
// the trap this whole change had to walk past.
//
// deleteSource's read exists so that the delete of the only other copy is
// authorised by a fact about NOW rather than by a fact from earlier that
// may since have stopped being true. Reusing the earlier read is only
// honest if something still establishes that "since" part, and the thing
// that does it here is the medium's own account of the object: its size,
// its mod time and its storage class, taken before the read and again
// immediately before the delete.
//
// So each row breaks the destination after its bytes have been served and
// before the delete is issued, which is exactly the window the second read
// covered. The engine has to notice, fall back to the full read, and keep
// the source. A cache that simply trusted the VERIFIED write would delete
// the source in every one of these worlds and would still pass the
// open-count test above.
func TestADestinationThatGoesBadBetweenTheReadAndTheDeleteKeepsTheSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		poison func(f *fixture) func(*fakeMedium)
	}{
		{
			// The sharp one. Same length, so a check that only compared
			// sizes would wave it through, and the mod time is the only
			// thing that says a write happened.
			name: "the bytes are replaced by something else of exactly the same length",
			poison: func(f *fixture) func(*fakeMedium) {
				junk := bytes.Repeat([]byte("x"), len(f.content))
				key := f.key
				return func(m *fakeMedium) { m.putLocked(key, junk) }
			},
		},
		{
			name: "the bytes are replaced by something shorter",
			poison: func(f *fixture) func(*fakeMedium) {
				key := f.key
				return func(m *fakeMedium) { m.putLocked(key, []byte("truncated")) }
			},
		},
		{
			// An endpoint that overwrites without moving the mod time. It
			// is not what S3 does, and it is exactly what makes size a
			// signal of its own rather than one mod time already covers:
			// this Store is an interface and the refusal has to hold for
			// whatever is behind it.
			name: "the bytes are replaced and the mod time does not move",
			poison: func(f *fixture) func(*fakeMedium) {
				key := f.key
				return func(m *fakeMedium) { m.objects[key] = []byte("silently shorter") }
			},
		},
		{
			name: "the object is gone by the time the delete is issued",
			poison: func(f *fixture) func(*fakeMedium) {
				key := f.key
				return func(m *fakeMedium) {
					delete(m.objects, key)
					delete(m.modTimes, key)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			f.medium.afterOpen = tc.poison(f)

			report, err := f.engine.RunCycle(f.ctx, []placement.Plan{{Artifact: f.artifact, DestinationMedium: testMedium}})
			if err != nil {
				t.Fatalf("RunCycle: %v", err)
			}
			f.guard.fail()

			if !f.localExists() {
				t.Fatalf("THE SOURCE COPY WAS DELETED against a destination that changed after it was read. The engine reported: %+v", report.Outcomes)
			}
			mv := f.onlyMove()
			if placement.Phase(mv.Phase) == placement.Done {
				t.Fatalf("the move reached DONE, so it believes it deleted the source against a destination it never proved: %+v", report.Outcomes)
			}
			if mv.Error == "" {
				t.Error("the move stopped and recorded no reason, so nothing an operator reads says what happened")
			}
			src, _ := f.placement(state.MediumLocal)
			if src.Status == state.PlacementGone {
				t.Errorf("the source placement is %s; the copy is on disk and the journal has to say so", src.Status)
			}
		})
	}
}

// TestEveryFactThePreDeleteProofRestsOnIsLoadBearing is the other
// direction, and it is what stops the continuity check being decoration.
//
// Each row changes exactly one of the things the proof rests on, while
// leaving the artifact itself perfectly good. The move still has to
// finish, because none of these is a reason to refuse anything; what it
// must NOT do is finish on the strength of the earlier read. So the
// assertion is that the full read came back: two opens, and a completed
// move.
//
// Without this, a proof that was never usable in the first place would
// satisfy every other test in this file, and the saving would be
// imaginary.
func TestEveryFactThePreDeleteProofRestsOnIsLoadBearing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(f *fixture)
	}{
		{
			// Something wrote to the key and put back bytes that happen to
			// be right. The engine cannot know that without reading, and
			// this is the point at which it has to.
			name: "the mod time moves although the bytes do not",
			breakIt: func(f *fixture) {
				key := f.key
				f.medium.afterOpen = func(m *fakeMedium) { m.touchLocked(key) }
			},
		},
		{
			// A bucket lifecycle rule transitioning the object mid-move is
			// the case that made #241 exist. The class the endpoint
			// reports is part of the object's identity for exactly this
			// reason.
			name: "a lifecycle rule transitions the object under the move",
			breakIt: func(f *fixture) {
				f.medium.afterOpen = func(m *fakeMedium) { m.statClass = config.StorageClassGlacierIR }
			},
		},
		{
			// A backend with no mod time gives the continuity check
			// nothing to work with, so there is no proof to hold and every
			// move pays for the second read. That is the fail-safe
			// direction and it has to be the one that is taken.
			name:    "the medium reports no mod time at all",
			breakIt: func(f *fixture) { f.medium.noModTime = true },
		},
		{
			// The medium answered once and then stopped answering. An
			// identity that could not be taken is not an identity that
			// matched.
			name: "the metadata call before the delete fails",
			breakIt: func(f *fixture) {
				f.medium.afterOpen = func(m *fakeMedium) { m.statErr = errTheEndpointWentAway }
			},
		},
		{
			// The proof has an age and the age has a bound. This clock
			// jumps five minutes per reading, so by the time the delete is
			// issued the read is well outside it.
			name:    "the proof is older than the bound",
			breakIt: func(f *fixture) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := fixtureOpts{}
			if tc.name == "the proof is older than the bound" {
				opts.clockStep = 5 * time.Minute
			}
			f := newFixture(t, opts)
			tc.breakIt(f)

			report := f.runCycle()
			f.guard.fail()
			if report.Completed != 1 {
				t.Fatalf("the move did not complete, and nothing in this world is a reason to refuse it: %+v", report.Outcomes)
			}
			if got := f.medium.openCount(); got != 2 {
				t.Errorf("the move read the object back %d times, want 2: this world breaks one of the facts the pre-delete proof "+
					"rests on, so the proof must not stand and the full read has to happen", got)
			}
		})
	}
}

// TestAProofCannotCrossACycleBoundary is the resume argument, made as a
// measurement rather than as a comment.
//
// The whole reason deleteSource re-reads is that a move it finds at
// SOURCE_DELETE_PENDING may have been left there by a process that is
// gone, hours or weeks ago, and VERIFIED is then a fact about a world
// nobody has looked at since. That case must keep paying for the read, or
// the change has bought a bill by giving up the thing the read is for.
//
// The first cycle here is refused by the tier guard, so the move stops at
// SOURCE_DELETE_PENDING with a destination that is genuinely fine. The
// second cycle picks it up the way a restart would. It has to read again.
//
// What makes that true is the proof's SCOPE, not any check: the second
// cycle's advance loop declares its own, so there is nothing to find. This
// test is the behaviour that scope buys, and
// TestNothingHoldsAPreDeleteProofBeyondOneWalkOfOneMove is what pins the
// scope itself. Neither is enough alone: hoisting the proof onto Engine
// leaves this one green, because the first cycle spends the proof on its
// way to the guard's refusal, and it takes the structural test to say so.
func TestAProofCannotCrossACycleBoundary(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.tiers.selected = true
	f.tiers.why = "the weekly tier still selects it on local"

	report := f.runCycle()
	f.guard.fail()
	if report.Refused != 1 {
		t.Fatalf("the first cycle was not refused by the tier guard, so the move is not parked where this test needs it: %+v", report.Outcomes)
	}
	if got := f.medium.openCount(); got != 1 {
		t.Fatalf("the first cycle read the object back %d times, want 1", got)
	}
	if !f.localExists() {
		t.Fatal("the source was deleted while a tier still selected it")
	}

	f.tiers.selected = false
	report = f.runCycle()
	f.guard.fail()
	if report.Completed != 1 {
		t.Fatalf("the second cycle did not finish the move: %+v", report.Outcomes)
	}
	if got := f.medium.openCount(); got != 2 {
		t.Errorf("the object was read back %d times over two cycles, want 2: the proof the first cycle made cannot authorise a delete "+
			"in the second, because that is exactly the restart case the pre-delete read exists for", got)
	}
}

// TestOnlyTheClassThatCostsEgressIsEverProved pins the scope of the whole
// mechanism.
//
// A proof with an age is a weaker thing than a check run on the spot, and
// it is worth having only where running the check on the spot costs a full
// download. Attested is one metadata call and existence is one HEAD;
// re-running either immediately before the delete costs nothing worth
// saving, so they keep the unconditional fresh check and there is nothing
// to argue about.
func TestOnlyTheClassThatCostsEgressIsEverProved(t *testing.T) {
	f := newFixture(t, fixtureOpts{class: placement.Attested, attests: true})
	report := f.runCycle()
	f.guard.fail()
	if report.Completed != 1 {
		t.Fatalf("the attested move did not complete: %+v", report.Outcomes)
	}
	if got := f.medium.checksumCount(); got != 2 {
		t.Errorf("the move asked the endpoint for an attestation %d times, want 2: an attestation costs one metadata call, "+
			"so the check immediately before the delete stays a check", got)
	}
}

// TestAMoveHomeReadsTheLocalCopyOnce is the same claim for the other
// direction, and it is here because the local end is a different branch of
// the same mechanism rather than an afterthought.
//
// A move back to local has its content check on a file rather than on an
// object: verifyLocalCopy opens it and hashes it, and deleteSource used to
// open it and hash it again. No egress, so no bill, but on a large
// artifact it is the whole thing off the disk twice for a question that
// was answered a moment ago. The proof's subject on this end is the file's
// size and its modification time at nanosecond resolution, which is a
// better signal than anything S3 will give.
func TestAMoveHomeReadsTheLocalCopyOnce(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	// Out first, the ordinary way, so there is something to bring back.
	f.runCycle()
	f.guard.fail()
	if f.localExists() {
		t.Fatal("the outward move did not remove the local copy, so the reverse move has nothing to prove")
	}
	before := f.local.openCount()

	report, err := f.engine.RunCycle(f.ctx, []placement.Plan{{Artifact: f.artifact, DestinationMedium: state.MediumLocal}})
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	f.guard.fail()
	if report.Completed != 1 {
		t.Fatalf("the reverse move did not complete: %+v", report.Outcomes)
	}
	if got := f.local.openCount() - before; got != 1 {
		t.Errorf("the reverse move read the local copy back %d times, want 1: verifyLocalCopy hashes it, and the delete stands on "+
			"that read plus the file's size and mod time, which is what artifactstore.Stat reports without opening anything", got)
	}
}
