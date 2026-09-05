package placement_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Every test in this file is the same experiment with one thing broken:
// put a move in the one phase from which a source delete is reachable,
// break something the guard is supposed to notice, run the real engine,
// and check the source copy is still there.
//
// The world is otherwise correct in each case, including the destination
// object itself, so the fresh re-verification deleteSource runs before the
// guard passes and the guard is genuinely the thing that refuses. A test
// where the re-verification would have caught it anyway proves nothing
// about the guard.

// deleteAttempt is one such experiment.
type deleteAttempt struct {
	// destination is the placement the VERIFIED write is pretended to have
	// recorded. A nil pointer records none at all.
	destination *state.PlacementUpdate
	// sourceStatus is what the source placement is left at. Empty means
	// DELETE_PENDING, which is what the real VERIFIED -> SOURCE_DELETE_PENDING
	// write leaves behind.
	sourceStatus string
	// afterPlant runs once the move is planted, to break the world.
	afterPlant func(t *testing.T, f *fixture)
	// wantRefusal is a phrase the recorded refusal must contain.
	wantRefusal string
}

// plantSourceDeletePending puts a move at SOURCE_DELETE_PENDING with the
// destination object really on the medium, so only the journal's own
// account of it is under the test's control.
func plantSourceDeletePending(t *testing.T, f *fixture, a deleteAttempt) state.Move {
	t.Helper()
	ctx := context.Background()

	mv, err := f.journal.PlanMove(ctx, state.MovePlan{
		Artifact: f.artifact, SourceMedium: state.MediumLocal,
		DestinationMedium: testMedium, DestinationKey: f.key, OccurredAt: f.clock,
	})
	if err != nil {
		t.Fatalf("planting the move: %v", err)
	}

	if _, err := f.medium.UploadFromLocal(ctx, transport.Medium{ID: testMedium}, f.localPath(), f.key, transport.UploadOptions{}); err != nil {
		t.Fatalf("planting the destination object: %v", err)
	}

	step := func(from, to string, placements ...state.PlacementUpdate) {
		t.Helper()
		f.clock = f.clock.Add(time.Second)
		mv, err = f.journal.AdvanceMove(ctx, state.MoveAdvance{
			MoveID: mv.ID, From: from, To: to, OccurredAt: f.clock, Placements: placements,
		})
		if err != nil {
			t.Fatalf("planting %s -> %s: %v", from, to, err)
		}
	}

	size := int64(len(f.content))
	step(state.MovePlanned, state.MoveCopying)
	f.clock = f.clock.Add(time.Second)
	mv, err = f.journal.AdvanceMove(ctx, state.MoveAdvance{
		MoveID: mv.ID, From: state.MoveCopying, To: state.MoveCopied, OccurredAt: f.clock, BytesCopied: &size,
	})
	if err != nil {
		t.Fatalf("planting COPYING -> COPIED: %v", err)
	}
	step(state.MoveCopied, state.MoveVerifying)

	verified := f.clock
	dst := state.PlacementUpdate{
		Medium: testMedium, Location: f.key, Size: &size, Hash: f.hash, HashAlg: "sha256",
		VerificationClass: state.VerificationContent, VerifiedAt: &verified, Status: state.PlacementActive,
	}
	if a.destination != nil {
		dst = *a.destination
		if dst.Medium == "" {
			dst.Medium = testMedium
		}
	}
	if a.destination == nil || dst.Location != "" {
		step(state.MoveVerifying, state.MoveVerified, dst)
	} else {
		// No destination placement at all: the VERIFIED write happens
		// without one, which is what a mutation that forgot to record it
		// would produce.
		step(state.MoveVerifying, state.MoveVerified)
	}

	src, ok := f.placement(state.MediumLocal)
	if !ok {
		t.Fatal("the seeded artifact has no local placement")
	}
	status := a.sourceStatus
	if status == "" {
		status = state.PlacementDeletePending
	}
	step(state.MoveVerified, state.MoveSourceDeletePending, src.Update().WithStatus(status))

	if a.afterPlant != nil {
		a.afterPlant(t, f)
	}

	// Some of these worlds break FR-30's invariant the instant they are
	// planted, and that is the premise rather than a defect: a
	// destination the journal cannot rely on, with the source already at
	// DELETE_PENDING, is precisely the state guardSourceDelete has to
	// refuse a delete in. Declared from the journal rather than from a
	// per-cell flag, so a cell cannot claim a breach it does not have and
	// a cell that stops having one stops forgiving anything.
	if placement.CheckInvariant(f.record()) != nil {
		f.guard.tolerateExistingBreach(t)
	}
	return mv
}

// runDeleteAttempt runs the engine over the planted move and asserts the
// source survived and the refusal said why. It returns everything the
// engine said about why, so a test can assert more about the wording than
// the one phrase deleteAttempt carries.
func runDeleteAttempt(t *testing.T, f *fixture, a deleteAttempt) string {
	t.Helper()

	before, statErr := os.Lstat(f.localPath())
	if statErr != nil {
		t.Fatalf("the fixture has no local source to protect: %v", statErr)
	}

	report, err := f.engine.RunCycle(f.ctx, nil)
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	f.guard.fail()

	after, statErr := os.Lstat(f.localPath())
	if statErr != nil {
		t.Fatalf("THE SOURCE COPY WAS DELETED. The engine reported: %+v", report.Outcomes)
	}
	if before.Size() != after.Size() || before.Mode() != after.Mode() {
		t.Errorf("the source copy changed under a refused delete: %v -> %v", before.Mode(), after.Mode())
	}

	mv := f.onlyMove()
	if placement.Phase(mv.Phase) == placement.Done {
		t.Fatal("the move reached DONE, so it believes it deleted the source")
	}

	var refusals []string
	for _, o := range report.Outcomes {
		if o.Refused != "" {
			refusals = append(refusals, o.Refused)
		}
	}
	joined := strings.Join(refusals, "\n") + "\n" + mv.Error
	if a.wantRefusal != "" && !strings.Contains(joined, a.wantRefusal) {
		t.Errorf("the engine refused with:\n%s\nwant a refusal containing %q", joined, a.wantRefusal)
	}
	if len(refusals) == 0 && mv.Error == "" {
		t.Error("the engine declined to delete and said nothing about why")
	}

	// And the reason is on the DURABLE row, not only in the cycle report.
	//
	// This used to be an OR with the line above, which meant a refusal
	// that only ever reached the in-memory report satisfied it. The
	// report is gone by the time anybody looks, and FR-24's health reads
	// the move row to tell a move that is stuck from a move that is
	// young, so a refusal that lives only in the report is one nothing
	// can see. Every path out of deleteSource that stops rather than
	// finishes goes through noteOnRow now, so every cell that lands here
	// has a row to read: the capability branch AND the guard's own
	// clauses.
	if mv.Error == "" {
		t.Errorf("the move row carries no reason, so an operator reading the move journal has no account of why this move "+
			"has not progressed, and a health surface reading the same column has none either. The cycle report said: %s", strings.Join(refusals, "\n"))
	}
	return joined
}

func TestTheGuardRefusesADestinationTheJournalNeverRecorded(t *testing.T) {
	// A mutation that reaches SOURCE_DELETE_PENDING without ever writing
	// the destination's placement. The object is really there and really
	// re-verifies; the journal just never said so, and durable evidence is
	// what authorises a delete.
	f := newFixture(t, fixtureOpts{})
	a := deleteAttempt{
		destination: &state.PlacementUpdate{},
		wantRefusal: "records no placement on the destination",
	}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

func TestTheGuardRefusesADestinationRecordedAtAWeakerClass(t *testing.T) {
	// FR-31: existence is never sufficient to delete a source. The object
	// exists and would pass a content read-back, but what the journal
	// records is an existence check, and that is what the guard reads.
	f := newFixture(t, fixtureOpts{})
	size := int64(len(f.content))
	at := testNow2
	a := deleteAttempt{
		destination: &state.PlacementUpdate{
			Medium: testMedium, Location: f.key, Size: &size, Hash: f.hash, HashAlg: "sha256",
			VerificationClass: state.VerificationExistence, VerifiedAt: &at, Status: state.PlacementActive,
		},
		wantRefusal: `records verification class "existence"`,
	}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

func TestTheGuardRefusesADestinationRecordedAsUnverified(t *testing.T) {
	// The same clause as the test above, at the other end of the ladder:
	// the row records no class at all. It gets its own case because an
	// empty class is what a placement carries before anything has looked
	// at it, so it is the value a half-written journal actually produces,
	// where "existence" has to be written deliberately.
	f := newFixture(t, fixtureOpts{})
	size := int64(len(f.content))
	a := deleteAttempt{
		destination: &state.PlacementUpdate{
			Medium: testMedium, Location: f.key, Size: &size, Hash: f.hash, HashAlg: "sha256",
			Status: state.PlacementActive,
		},
		wantRefusal: `records verification class "unverified"`,
	}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

func TestTheGuardRefusesADestinationPlacementThatIsNotActive(t *testing.T) {
	// DELETE_PENDING on the destination means the journal has already
	// decided to get rid of it. A copy the journal is in the middle of
	// disposing of cannot be the reason another copy is disposable, and
	// the recorded content verification does not change that.
	f := newFixture(t, fixtureOpts{})
	size := int64(len(f.content))
	at := testNow2
	a := deleteAttempt{
		destination: &state.PlacementUpdate{
			Medium: testMedium, Location: f.key, Size: &size, Hash: f.hash, HashAlg: "sha256",
			VerificationClass: state.VerificationContent, VerifiedAt: &at, Status: state.PlacementDeletePending,
		},
		wantRefusal: "destination placement on",
	}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

func TestTheGuardRefusesADestinationRecordedAtADifferentKey(t *testing.T) {
	// The placement and the move disagree about which object was verified.
	// One of them is wrong and nothing here can say which, so the answer
	// is to refuse rather than pick.
	f := newFixture(t, fixtureOpts{})
	size := int64(len(f.content))
	at := testNow2
	a := deleteAttempt{
		destination: &state.PlacementUpdate{
			Medium: testMedium, Location: f.key + ".other", Size: &size, Hash: f.hash, HashAlg: "sha256",
			VerificationClass: state.VerificationContent, VerifiedAt: &at, Status: state.PlacementActive,
		},
		wantRefusal: "refusing to guess which is the copy",
	}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

func TestTheGuardRefusesWhenTheTwoCopiesRecordDifferentHashes(t *testing.T) {
	// Both rows are otherwise impeccable: ACTIVE, content class, the right
	// key, verified, recently. They simply describe different bytes, and
	// no verification class can rescue that, because a class says how hard
	// somebody looked rather than what they were comparing against. The
	// two rows come from different writes, so they can genuinely disagree.
	f := newFixture(t, fixtureOpts{})
	size := int64(len(f.content))
	at := testNow2
	a := deleteAttempt{
		destination: &state.PlacementUpdate{
			Medium: testMedium, Location: f.key, Size: &size,
			Hash: strings.Repeat("a", 64), HashAlg: "sha256",
			VerificationClass: state.VerificationContent, VerifiedAt: &at, Status: state.PlacementActive,
		},
		wantRefusal: "so they are not the same bytes",
	}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

func TestTheGuardRefusesASourceThatWasNeverMarkedDeletePending(t *testing.T) {
	// The intent-before-delete rule. A source still ACTIVE means the write
	// that decided this delete is not the write this row carries.
	f := newFixture(t, fixtureOpts{})
	a := deleteAttempt{
		sourceStatus: state.PlacementActive,
		wantRefusal:  "a delete is only ever issued against DELETE_PENDING",
	}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

func TestTheGuardRefusesWithoutARetentionTierGuard(t *testing.T) {
	// A nil guard is a refusal, not a pass. An engine that cannot ask
	// whether a tier still wants a copy here cannot prove the source is
	// unwanted, and uncertainty preserves the source.
	f := newFixture(t, fixtureOpts{})
	a := deleteAttempt{
		afterPlant:  func(_ *testing.T, f *fixture) { f.engine.Tiers = nil },
		wantRefusal: "no retention-tier guard is configured",
	}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

func TestTheGuardRefusesWhenATierStillSelectsTheSourceMedium(t *testing.T) {
	// FR-30's last question, and the one refusal here that is not about
	// safety: the destination is fine and the source is still wanted where
	// it is. The count at the end is what makes this evidence about the
	// clause rather than about the fixture, since a green run with the
	// tier guard never consulted would mean the plumbing was wrong.
	f := newFixture(t, fixtureOpts{})
	a := deleteAttempt{
		afterPlant: func(_ *testing.T, f *fixture) {
			f.tiers.selected = true
			f.tiers.why = "the daily tier is still local and still selects it"
		},
		wantRefusal: "a retention tier still selects it",
	}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
	if f.tiers.asked == 0 {
		t.Error("the guard never asked the tier guard, so this test proved nothing")
	}
}

func TestTheGuardRefusesWhenTheTierGuardCannotAnswer(t *testing.T) {
	// The other half of the same clause. A guard that returns an error is
	// a refusal and never a pass: an engine that could not find out
	// whether a tier still wants this copy has not proved the source is
	// unwanted, and uncertainty preserves the source.
	f := newFixture(t, fixtureOpts{})
	a := deleteAttempt{
		afterPlant:  func(_ *testing.T, f *fixture) { f.tiers.err = fmt.Errorf("the retention chain could not be evaluated") },
		wantRefusal: "asking whether a tier still selects it",
	}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

func TestTheGuardRefusesAnArtifactThatLeftCOMPLETE(t *testing.T) {
	// The eligibility rule, re-derived at the dangerous action rather than
	// trusted from plan time. A resumed move was planned by a process that
	// is gone, and the artifact can have moved on since.
	f := newFixture(t, fixtureOpts{})
	a := deleteAttempt{
		afterPlant: func(t *testing.T, f *fixture) {
			if _, err := f.journal.RecordTransition(f.ctx, state.Transition{
				Artifact: f.artifact, Key: "quarantine-it", From: "COMPLETE", To: "QUARANTINED_LOST",
				OccurredAt: testNow2,
			}); err != nil {
				t.Fatalf("quarantining the artifact: %v", err)
			}
		},
		wantRefusal: "only COMPLETE artifacts may move",
	}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

func TestTheGuardRefusesASymlinkAtTheSourcePath(t *testing.T) {
	// FR-20's symlink refusal. Commit only ever produces a final name by
	// hard-linking a .partial, so a symlink there is outside every
	// invariant the pipeline maintains. Resolving it and deleting the
	// target, even a target inside the root, would delete a file this
	// check never identified as the artifact.
	f := newFixture(t, fixtureOpts{})
	decoy := filepath.Join(f.root, "decoy.dump")

	a := deleteAttempt{
		afterPlant: func(t *testing.T, f *fixture) {
			if err := os.WriteFile(decoy, f.content, 0o600); err != nil {
				t.Fatalf("writing the decoy: %v", err)
			}
			if err := os.Remove(f.localPath()); err != nil {
				t.Fatalf("removing the real file: %v", err)
			}
			if err := os.Symlink(decoy, f.localPath()); err != nil {
				t.Fatalf("planting the symlink: %v", err)
			}
		},
		wantRefusal: "is a symlink",
	}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)

	if _, err := os.Stat(decoy); err != nil {
		t.Fatalf("the symlink's TARGET was deleted: %v", err)
	}
}

func TestTheGuardRefusesASourceWhoseSizeChangedOnDisk(t *testing.T) {
	// FR-16's identity idea applied to the local end: a file that changed
	// size under a move is not the copy the destination was verified
	// against, whatever the path says.
	f := newFixture(t, fixtureOpts{})
	a := deleteAttempt{
		afterPlant: func(t *testing.T, f *fixture) {
			if err := os.WriteFile(f.localPath(), append(f.content, " and more"...), 0o600); err != nil {
				t.Fatalf("growing the local file: %v", err)
			}
		},
		wantRefusal: "the placement records",
	}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

func TestTheGuardRefusesWhenTheJournalsPathIsNotTheComputedOne(t *testing.T) {
	// "Positively identified database-managed files" in FR-20's own words:
	// the file about to be deleted must be the one the journal knows this
	// artifact as, and a disagreement is refused rather than guessed at.
	f := newFixture(t, fixtureOpts{})
	a := deleteAttempt{
		afterPlant: func(t *testing.T, f *fixture) {
			f.engine.Sets = fixedSets{set: config.BackupSet{
				Name: testSet, ID: f.artifact.Set, LocalPath: filepath.Join(f.root, "elsewhere"),
			}}
			if err := os.MkdirAll(filepath.Join(f.root, "elsewhere"), 0o750); err != nil {
				t.Fatalf("creating the other root: %v", err)
			}
		},
		wantRefusal: "refusing to guess which is correct",
	}
	plantSourceDeletePending(t, f, a)
	runDeleteAttempt(t, f, a)
}

// TestAStandingRefusalBeforeTheSourceDeleteIsRecordedOnTheMoveRow is the
// visibility half of a refusal that is deliberately invisible in every
// other respect.
//
// deleteSource's standing refusals change nothing on purpose: same phase,
// same placements, every copy preserved, and the move re-driven and
// re-refused on the next cycle until somebody acts on it. That is right,
// and it left the move row reading SOURCE_DELETE_PENDING with an EMPTY
// error column, so the only account of what the engine had decided lived
// in the cycle report, which is in memory and gone by the time an operator
// looks.
//
// FR-24's health surface is what makes that matter rather than merely
// untidy. It reads the error on the move row to tell a move that is
// failing from a move that is young, so a move parked here read as
// open-and-fine and could sit there indefinitely with nothing saying so.
//
// The three claims are separate on purpose: the reason is THERE, the move
// did NOT move, and no copy changed. A build that recorded the reason by
// advancing the move, or by touching a placement, would satisfy the first
// and break the thing the refusal exists to protect.
func TestAStandingRefusalBeforeTheSourceDeleteIsRecordedOnTheMoveRow(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	size := int64(len(f.content))
	at := testNow2
	a := deleteAttempt{
		destination: &state.PlacementUpdate{
			Medium: testMedium, Location: f.key, Size: &size, Hash: f.hash, HashAlg: "sha256",
			VerificationClass: state.VerificationContent, VerifiedAt: &at, Status: state.PlacementActive,
		},
	}
	plantSourceDeletePending(t, f, a)

	// The control, and it is the one this cell would be worthless
	// without: the row carries no reason before the cycle runs, so a
	// reason afterwards can only have been written by the refusal. A
	// planted world that already had one would make this pass no matter
	// what the engine did.
	if before := f.onlyMove(); before.Error != "" {
		t.Fatalf("the planted move row already carries a reason (%q), so this cell cannot tell who wrote one", before.Error)
	}
	placementsBefore := describePlacementsForTest(f.record())

	// A destination that cannot be READ at the moment of the re-check.
	// This is the capability refusal, not a failed verification: the
	// object is there, the journal's account of it is perfect, and the
	// endpoint will not serve it right now. deleteSource must change
	// nothing at all in that situation, because the destination is the
	// copy FR-30's invariant is currently resting on.
	f.medium.openErr = &transport.Error{
		Category: transport.UnsupportedCapability, Op: "open",
		Cause: errors.New("InvalidObjectState: the operation is not valid for the object's storage class"),
	}

	// TWO cycles, because "the reason is reported every cycle until
	// somebody acts on it" is the claim, and one cycle cannot tell that
	// from a reason written once and then lost. The second cycle also
	// re-drives a move that is deliberately going nowhere, which is the
	// state a health surface is looking at when it reads the row.
	var report placement.CycleReport
	for cycle := 1; cycle <= 2; cycle++ {
		var err error
		report, err = f.engine.RunCycle(f.ctx, nil)
		if err != nil {
			t.Fatalf("cycle %d: RunCycle: %v", cycle, err)
		}
		f.guard.fail()
		if len(report.Outcomes) != 1 {
			t.Fatalf("cycle %d: expected one outcome, got %+v", cycle, report.Outcomes)
		}
		if report.Outcomes[0].Refused == "" {
			t.Errorf("cycle %d: the cycle report says nothing about a move it refused", cycle)
		}
		if got := f.onlyMove(); got.Error == "" {
			t.Fatalf("cycle %d: the move row carries no reason; a refusal reported once and then cleared is a refusal a health surface sees only if it was watching", cycle)
		}
	}

	mv := f.onlyMove()

	// 1. The reason is on the row, and it is one an operator can act on:
	// which artifact, which medium, what could not be done, and the fact
	// that nothing was changed.
	if mv.Error == "" {
		t.Fatal("the move row carries no reason after a standing refusal, so an operator reading the move journal " +
			"has no account of why this move has not progressed, and a health surface reading the same column has none either")
	}
	for _, want := range []string{
		f.artifact.String(), // which artifact
		testMedium,          // which medium
		"could not be re-verified",
		"nothing has been changed", // that this is a refusal and not a half-done delete
	} {
		if !strings.Contains(mv.Error, want) {
			t.Errorf("the recorded reason does not carry %q, so it does not say what an operator has to act on: %q", want, mv.Error)
		}
	}
	t.Logf("the move row records: %s", mv.Error)

	// 2. It did not move. The refusal is not a fault and not a give-up:
	// the phase is where it was, and it is not terminal, so the next
	// cycle drives it again.
	if got := placement.Phase(mv.Phase); got != placement.SourceDeletePending {
		t.Errorf("the move is at %s after a standing refusal, want %s; recording a reason must not advance anything",
			got, placement.SourceDeletePending)
	}
	if placement.IsTerminal(placement.Phase(mv.Phase)) {
		t.Error("the move reached a terminal phase; a standing refusal is a move waiting, not a move finished")
	}

	// 3. No copy changed, and the source is still on disk.
	if got := describePlacementsForTest(f.record()); got != placementsBefore {
		t.Errorf("the placements changed under a refusal that must change nothing:\n before: %s\n after:  %s", placementsBefore, got)
	}
	if !f.localExists() {
		t.Fatal("THE SOURCE COPY WAS DELETED against a destination nothing could read")
	}

	// And the caller can still classify it. The error the row renders is
	// the caller's own, so an errors.Is that worked before still works.
	o := report.Outcomes[0]
	if o.Err == nil || !errors.Is(o.Err, placement.ErrClassUnavailable) {
		t.Errorf("the outcome no longer carries a capability refusal a caller can ask about: %v", o.Err)
	}
}

// describePlacementsForTest renders every placement, so a cell can compare
// the whole set before and after and not only the one it was thinking of.
func describePlacementsForTest(rec state.Record) string {
	parts := make([]string, 0, len(rec.Placements))
	for _, p := range rec.Placements {
		class := p.VerificationClass
		if class == "" {
			class = "unverified"
		}
		parts = append(parts, fmt.Sprintf("%s=%s/%s@%s", p.Medium, p.Status, class, p.Location))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
