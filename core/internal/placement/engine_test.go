package placement_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// TestANominalMoveCopiesVerifiesAndOnlyThenDeletesTheSource is the whole
// contract in one run: the bytes reach the medium, they are read back and
// hashed against the journal's recorded SHA-256, the destination gets an
// ACTIVE placement at content class, and only after all of that does the
// local copy stop existing.
//
// The ordering claim is not asserted by looking at the end state, which
// cannot tell a correct order from a lucky one. It is asserted by the
// guard on the local delete: that call re-reads the durable journal and
// refuses unless another ACTIVE content-verified copy is already recorded
// there. A delete issued one phase early never reaches the filesystem.
func TestANominalMoveCopiesVerifiesAndOnlyThenDeletesTheSource(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	report := f.runCycle()
	f.guard.fail()

	if report.Planned != 1 || report.Completed != 1 {
		t.Fatalf("expected one planned, completed move; got %+v", report)
	}
	mv := f.onlyMove()
	if placement.Phase(mv.Phase) != placement.Done {
		t.Fatalf("the move ended at %s, want DONE (error: %q)", mv.Phase, mv.Error)
	}

	if f.localExists() {
		t.Error("the local source copy is still on disk after a completed move")
	}
	if !f.medium.has(f.key) {
		t.Fatalf("the destination object is not on the medium at %q", f.key)
	}
	if got := f.medium.bytesAt(f.key); string(got) != string(f.content) {
		t.Error("the destination object does not hold the artifact's bytes")
	}

	dst, ok := f.placement(testMedium)
	if !ok {
		t.Fatal("no placement records the destination copy")
	}
	if dst.Status != state.PlacementActive {
		t.Errorf("the destination placement is %s, want ACTIVE", dst.Status)
	}
	if dst.VerificationClass != state.VerificationContent {
		t.Errorf("the destination placement records class %q, want %q", dst.VerificationClass, state.VerificationContent)
	}
	if dst.VerifiedAt == nil {
		t.Error("the destination placement records no verification time")
	}

	src, ok := f.placement(state.MediumLocal)
	if !ok {
		t.Fatal("the local placement row disappeared; a deleted copy is recorded as GONE, never removed")
	}
	if src.Status != state.PlacementGone {
		t.Errorf("the local placement is %s after the delete, want GONE", src.Status)
	}

	// The artifact's own lifecycle state is untouched. FR-30 is explicit:
	// a move adds no edge to the artifact machine.
	if got := f.record().State; got != "COMPLETE" {
		t.Errorf("the artifact is %s after a move; a move must not change the lifecycle state", got)
	}

	// The phases actually walked, in order. A move that reached DONE by a
	// shorter route would be a different engine.
	want := []string{
		"PLANNED->COPYING", "COPYING->COPIED", "COPIED->VERIFYING",
		"VERIFYING->VERIFIED", "VERIFIED->SOURCE_DELETE_PENDING", "SOURCE_DELETE_PENDING->DONE",
	}
	if got := f.guarded.phaseWrites(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the move walked %v\nwant %v", got, want)
	}

	// And exactly one delete happened, of the local copy.
	deletes := f.guard.deleteLog()
	if len(deletes) != 1 || !strings.HasPrefix(deletes[0], "removing the local copy") {
		t.Errorf("the deletes this move issued were %v; a nominal move deletes the source and nothing else", deletes)
	}
}

// TestTheSourceSurvivesADestinationThatStoredTheWrongBytes is the test the
// issue asks for by name: try to make the engine delete a source against
// an unverified destination, and watch it refuse.
//
// The endpoint here accepts the upload and stores something else, which is
// the hostile-endpoint case FR-31's own trust discussion is about. Nothing
// downstream can tell that from a successful upload except the read-back,
// which is the whole reason the read-back is mandatory.
func TestTheSourceSurvivesADestinationThatStoredTheWrongBytes(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.medium.corrupt = []byte("not the artifact at all")

	report := f.runCycle()
	f.guard.fail()

	if !f.localExists() {
		t.Fatal("THE SOURCE WAS DELETED against a destination that never verified")
	}
	if got, err := os.ReadFile(f.localPath()); err != nil || string(got) != string(f.content) {
		t.Fatalf("the local copy is not intact: %v", err)
	}

	mv := f.onlyMove()
	if placement.Phase(mv.Phase) != placement.Abandoned {
		t.Fatalf("the move ended at %s, want ABANDONED", mv.Phase)
	}
	if !strings.Contains(mv.Error, "failed content verification") {
		t.Errorf("the move's recorded error is %q; it should say the content check failed", mv.Error)
	}
	if f.medium.has(f.key) {
		t.Error("the bad destination object was left on the medium")
	}
	if _, ok := f.placement(testMedium); ok {
		t.Error("a placement was written for a destination that never verified")
	}
	src, _ := f.placement(state.MediumLocal)
	if src.Status != state.PlacementActive {
		t.Errorf("the source placement is %s after an abandoned move, want ACTIVE", src.Status)
	}
	if report.Abandoned != 1 {
		t.Errorf("the cycle reported %+v, want one abandoned move", report)
	}

	// It tried more than once before giving up, and every attempt targeted
	// the same key, so a hostile endpoint cannot make the engine litter.
	if f.medium.uploads < 2 {
		t.Errorf("the engine uploaded %d times; it should retry the copy before abandoning", f.medium.uploads)
	}
	for _, k := range f.medium.uploadedKeys {
		if k != f.key {
			t.Errorf("an upload targeted %q, not the deterministic key %q", k, f.key)
		}
	}
}

// TestATruncatedDestinationNeverAuthorisesASourceDelete is the same
// refusal against the failure a flaky upload actually produces, rather
// than against a deliberately malicious one.
func TestATruncatedDestinationNeverAuthorisesASourceDelete(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.medium.truncate = 10

	f.runCycle()
	f.guard.fail()

	if !f.localExists() {
		t.Fatal("THE SOURCE WAS DELETED against a truncated destination")
	}
	if placement.Phase(f.onlyMove().Phase) != placement.Abandoned {
		t.Errorf("the move ended at %s, want ABANDONED", f.onlyMove().Phase)
	}
}

// TestOnlyCompleteArtifactsMayMove is FR-30's eligibility rule. COMMITTED
// and REMOTE_DELETE_PENDING still owe FR-15 its pre-delete local-file
// checks, and a move racing those checks is the bug this rule makes
// unrepresentable.
func TestOnlyCompleteArtifactsMayMove(t *testing.T) {
	for _, st := range []string{"COMMITTED", "REMOTE_DELETE_PENDING", "DISCOVERED"} {
		t.Run(st, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{artifactState: st})

			report := f.runCycle()
			f.guard.fail()

			if report.Planned != 0 {
				t.Fatalf("a %s artifact was planned for a move", st)
			}
			if report.Refused != 1 {
				t.Fatalf("expected one refusal, got %+v", report)
			}
			if len(f.moves()) != 0 {
				t.Error("a move row was written for an artifact that may not move")
			}
			if !f.localExists() {
				t.Error("the local copy was touched")
			}
			refusal := report.Outcomes[0].Refused
			if !strings.Contains(refusal, "only COMPLETE") {
				t.Errorf("the refusal is %q; it should say only COMPLETE artifacts may move", refusal)
			}
		})
	}
}

// TestAnArtifactWithNoPlacementIsRefusedRatherThanInferred is the
// conservative failure the whole design turns on.
//
// A placement row means a durable copy, and a still-transferring
// artifact's .partial deliberately does not get one. This engine reads
// that row to decide whether a source may be deleted, so a missing row
// must make it decline. It must NOT fall back to LocalPath, which would
// reintroduce exactly the ambiguity the separate table removed.
func TestAnArtifactWithNoPlacementIsRefusedRatherThanInferred(t *testing.T) {
	f := newFixture(t, fixtureOpts{noPlacement: true})

	if _, ok := f.placement(state.MediumLocal); ok {
		t.Fatal("this fixture was supposed to seed an artifact with no placement")
	}
	if f.record().LocalPath == "" {
		t.Fatal("the artifact has no LocalPath either, so this test could pass for the wrong reason")
	}

	report := f.runCycle()
	f.guard.fail()

	if report.Refused != 1 || report.Planned != 0 {
		t.Fatalf("expected one refusal and no move, got %+v", report)
	}
	refusal := report.Outcomes[0].Refused
	if !strings.Contains(refusal, "no ACTIVE placement") || !strings.Contains(refusal, "refusing to infer") {
		t.Errorf("the refusal is %q; it should say the journal cannot locate the bytes and it will not infer them", refusal)
	}
	if !f.localExists() {
		t.Error("the local file was touched for an artifact with no placement")
	}
}

// TestAResumedCopyTargetsTheSameKeyAndLeavesOneObject is the
// idempotent-upload rule. FR-28's key layout carries no timestamp and no
// random component precisely so an interrupted upload converges rather
// than leaving a second object nobody knows about.
func TestAResumedCopyTargetsTheSameKeyAndLeavesOneObject(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	// A move found at COPYING with a half-written object at the key is
	// what a crash inside an upload leaves behind.
	plantMoveAt(t, f, placement.Copying)
	if _, err := f.medium.UploadFromLocal(context.Background(), transport.Medium{ID: testMedium}, f.localPath(), f.key, transport.UploadOptions{}); err != nil {
		t.Fatalf("planting a half-written destination: %v", err)
	}
	f.medium.mu.Lock()
	f.medium.objects[f.key] = []byte("half")
	f.medium.mu.Unlock()

	report, err := f.engine.RunCycle(f.ctx, nil)
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	f.guard.fail()

	if report.Resumed != 1 || report.Completed != 1 {
		t.Fatalf("expected the interrupted move to resume and complete, got %+v", report)
	}
	if f.medium.keyCount() != 1 {
		t.Fatalf("the medium holds %d objects; a resumed upload must converge on one key", f.medium.keyCount())
	}
	if got := f.medium.bytesAt(f.key); string(got) != string(f.content) {
		t.Error("the resumed upload did not overwrite the half-written object")
	}
	for _, k := range f.medium.uploadedKeys {
		if k != f.key {
			t.Errorf("an upload targeted %q, not %q", k, f.key)
		}
	}
	if f.localExists() {
		t.Error("the source is still on disk after a completed move")
	}
}

// TestAttestedFailsLoudlyRatherThanVerifyingLess is FR-13's rule restated
// by FR-31, and it is settled fact against this build: rclone v1.75.0's s3
// backend exposes MD5 from the ETag and refuses every other algorithm, so
// no S3 endpoint reachable through this binary can attest a full-object
// SHA-256. A medium configured for attested must therefore fail, loudly,
// and never quietly verify something weaker.
func TestAttestedFailsLoudlyRatherThanVerifyingLess(t *testing.T) {
	f := newFixture(t, fixtureOpts{class: placement.Attested, attests: false})

	f.runCycle()
	f.guard.fail()

	if !f.localExists() {
		t.Fatal("THE SOURCE WAS DELETED for a medium that could not attest anything")
	}
	mv := f.onlyMove()
	if placement.Phase(mv.Phase) != placement.Abandoned {
		t.Fatalf("the move ended at %s, want ABANDONED", mv.Phase)
	}
	if !strings.Contains(mv.Error, "attested") || !strings.Contains(mv.Error, "cannot attest") {
		t.Errorf("the recorded error is %q; it should name the class that was asked for and the capability that refused it", mv.Error)
	}
	if _, ok := f.placement(testMedium); ok {
		t.Error("a placement was written for a destination nothing could verify")
	}

	// The one thing it must never have done: fall back to a check that
	// costs nothing and proves less. Verify has no fallback, so a content
	// read-back here would mean the engine chose one.
	if f.medium.opens != 0 {
		t.Errorf("the engine read the object back %d times while configured for attested; asking for a different class than the one configured is exactly the silent degradation FR-31 forbids", f.medium.opens)
	}
}

// TestAttestedSucceedsWhereTheEndpointGenuinelyCanAttest is the other half
// of the same rule, and it is what keeps the test above honest: the
// refusal has to come from the endpoint's real capability, not from the
// engine being unable to reach attested at all.
func TestAttestedSucceedsWhereTheEndpointGenuinelyCanAttest(t *testing.T) {
	f := newFixture(t, fixtureOpts{class: placement.Attested, attests: true})

	f.runCycle()
	f.guard.fail()

	mv := f.onlyMove()
	if placement.Phase(mv.Phase) != placement.Done {
		t.Fatalf("the move ended at %s, want DONE (error: %q)", mv.Phase, mv.Error)
	}
	dst, ok := f.placement(testMedium)
	if !ok {
		t.Fatal("no placement records the destination copy")
	}
	if dst.VerificationClass != state.VerificationAttested {
		t.Errorf("the destination records class %q, want %q: a surface must report the class that ran", dst.VerificationClass, state.VerificationAttested)
	}
	if f.medium.opens != 0 {
		t.Errorf("attested cost %d egress reads; the whole point of the class is that it costs one metadata call", f.medium.opens)
	}
}

// TestMaxMovesPerCycleBoundsTheCycle is FR-30's per-cycle guard, the same
// shape revalidation's max_per_cycle has, including the fail-safe reading
// of a non-positive value.
func TestMaxMovesPerCycleBoundsTheCycle(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.engine.MaxMovesPerCycle = 0

	report, err := f.engine.RunCycle(f.ctx, []placement.Plan{{Artifact: f.artifact, DestinationMedium: testMedium}})
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if report.Planned != 0 || len(f.moves()) != 0 {
		t.Fatalf("a cycle bounded at zero moved something: %+v", report)
	}
	if !f.localExists() {
		t.Error("the local copy was touched by a cycle that was allowed no moves")
	}
}

// TestASecondLiveMoveForOneArtifactIsRefused: two live moves are two
// independent opinions about which copy is disposable, and the way that
// ends is each of them deleting the copy the other was relying on.
func TestASecondLiveMoveForOneArtifactIsRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	plantMoveAt(t, f, placement.Copying)

	_, err := f.journal.PlanMove(f.ctx, state.MovePlan{
		Artifact: f.artifact, SourceMedium: state.MediumLocal,
		DestinationMedium: "somewhere_else", DestinationKey: "k", OccurredAt: f.clock,
	})
	if err == nil {
		t.Fatal("a second live move was accepted")
	}
	if !strings.Contains(err.Error(), "already in flight") {
		t.Errorf("the refusal is %q; it should say a move is already in flight", err)
	}
}

// TestAnAdvanceAgainstAMovedRowIsRefused is the wall under the phase
// table: a write whose From no longer matches affects no rows and is
// reported, rather than silently overwriting a phase somebody else set.
func TestAnAdvanceAgainstAMovedRowIsRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mv := plantMoveAt(t, f, placement.Copying)

	_, err := f.journal.AdvanceMove(f.ctx, state.MoveAdvance{
		MoveID: mv.ID, From: state.MovePlanned, To: state.MoveCopied, OccurredAt: f.clock,
	})
	if !errors.Is(err, state.ErrMovePhaseConflict) {
		t.Fatalf("advancing from a stale phase returned %v, want ErrMovePhaseConflict", err)
	}
	if got := f.onlyMove().Phase; got != state.MoveCopying {
		t.Errorf("the row is at %s; a refused advance must change nothing", got)
	}
}

// TestAMoveBackToLocalWorksTheSameWay proves the engine is
// direction-agnostic at the unit level: the phases, the ordering and the
// guard are the same walking the other way, with the medium copy as the
// source and the backup set's own root as the destination.
func TestAMoveBackToLocalWorksTheSameWay(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	// Get the artifact onto the medium first, the ordinary way.
	f.runCycle()
	f.guard.fail()
	if f.localExists() {
		t.Fatal("the outward move did not remove the local copy, so the reverse move has nothing to prove")
	}

	report, err := f.engine.RunCycle(f.ctx, []placement.Plan{{Artifact: f.artifact, DestinationMedium: state.MediumLocal}})
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	f.guard.fail()

	if report.Planned != 1 || report.Completed != 1 {
		t.Fatalf("the reverse move did not complete: %+v", report)
	}
	if !f.localExists() {
		t.Fatal("the artifact did not come back to local disk")
	}
	got, err := os.ReadFile(f.localPath())
	if err != nil || string(got) != string(f.content) {
		t.Fatalf("the local copy is not the artifact's bytes: %v", err)
	}
	if f.medium.has(f.key) {
		t.Error("the medium still holds the object after a completed move off it")
	}

	local, ok := f.placement(state.MediumLocal)
	if !ok || local.Status != state.PlacementActive || local.VerificationClass != state.VerificationContent {
		t.Errorf("the local placement is %+v; it should be ACTIVE at content class", local)
	}
	medium, ok := f.placement(testMedium)
	if !ok || medium.Status != state.PlacementGone {
		t.Errorf("the medium placement is %+v; it should be GONE", medium)
	}
}
