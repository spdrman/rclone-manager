package placement_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	return mv
}

// runDeleteAttempt runs the engine over the planted move and asserts the
// source survived and the refusal said why.
func runDeleteAttempt(t *testing.T, f *fixture, a deleteAttempt) {
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
