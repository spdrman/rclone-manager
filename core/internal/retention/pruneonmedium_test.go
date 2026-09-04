package retention

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// FR-20's prune, once an artifact's durable copy is not a local file
// (EPIC E FR-30, issue #239).
//
// TDD invariant 9 in this repository, and this issue's own Given/When/Then
// third clause, both say the refusal test precedes the success test, so
// this file is ordered that way and the first delete that actually
// succeeds is most of the way down. The reason is not ceremony: every
// refusal below is the difference between deleting a copy of a backup and
// not, and a suite that establishes the happy path first tends to grow the
// refusals as afterthoughts around it.
//
// The FR-16 re-check itself is not here. It reads a placement row and
// stats an object, and internal/retention may read neither (FR-32, held
// structurally by placement.TestRetentionReadsNoMediumSuppliedValue). What
// is here is the decision, the refusal-first wiring, and the proof that
// nothing is deleted from a medium without something that can prove the
// object's identity first.

const pruneTestMedium = "offsite_s3"

// onMedium builds the locator that says every artifact lives on medium.
func onMedium(medium string) ArtifactLocator {
	return func(model.ArtifactID) (string, bool) { return medium, true }
}

// nowhere is the locator that can never confirm a location, which is what
// two ACTIVE placements (a move in flight) and no placement row at all
// both look like from here.
func nowhere(model.ArtifactID) (string, bool) { return "", false }

// recordingPruner is a MediumPruner that answers however a test tells it
// to and records every call, so a refusal test can assert the strongest
// thing available: not merely that the verdict said REFUSE, but that
// nothing ever reached the delete.
type recordingPruner struct {
	err   error
	calls []string
}

func (p *recordingPruner) DeleteFromMedium(_ context.Context, rec state.Record, medium string) error {
	p.calls = append(p.calls, rec.Artifact.Name+" on "+medium)
	return p.err
}

// pruneMediumFixture is one expired artifact (the today-only chain keeps
// nothing older than pruneNow's own civil day) with a real local file
// beside it. The file exists on purpose: a medium-resident artifact's
// verdict must not be reached through the local filesystem, and leaving a
// deletable file there is what would let a wrong implementation quietly
// pass by deleting the wrong thing.
func pruneMediumFixture(t *testing.T) (config.BackupSet, []state.Record, string) {
	t.Helper()
	root := t.TempDir()
	set := gfsMustSet(t, "production", "postgres-primary")
	artifact := gfsMustArtifact(t, set, "expired.dump")
	path := filepath.Join(root, "expired.dump")
	pruneWriteFile(t, path, "expired")

	rec := pruneRecord(artifact, lifecycle.Complete, pruneNow.AddDate(0, 0, -400), path)
	return pruneBackupSet(set, root), []state.Record{rec}, path
}

// --- the refusals ---

// TestPruneOnMedium_RefusesWhenItCannotConfirmWhereTheArtifactIs is the
// first refusal because it is the one a live deployment hits: FR-30's copy
// phase leaves the source and the destination both ACTIVE, so mid-move
// there are two answers to "where is this", and a prune that picked one
// would delete a copy out from under a move.
func TestPruneOnMedium_RefusesWhenItCannotConfirmWhereTheArtifactIs(t *testing.T) {
	bs, records, path := pruneMediumFixture(t)

	verdicts, err := PruneApply(context.Background(), pruneNow, pruneTodayOnlyChain(), bs, records, nowhere, nil)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	v := pruneFindVerdict(t, verdicts, "expired.dump")
	if v.Action != PruneRefuse {
		t.Fatalf("Action = %s, want %s: nothing confirmed where this artifact's durable copy is", v.Action, PruneRefuse)
	}
	if !strings.Contains(v.Reason, "confirm") {
		t.Errorf("Reason = %q, which does not say the location could not be confirmed", v.Reason)
	}
	pruneMustExist(t, path)
}

// TestPruneOnMedium_RefusesWithNothingThatCanDeleteFromAMedium is the nil
// -pruner refusal, and it is the same shape #238 gave the nil TierGuard: a
// prune with no way to prove an object's identity has no business
// removing it, so absence is a refusal rather than a pass.
func TestPruneOnMedium_RefusesWithNothingThatCanDeleteFromAMedium(t *testing.T) {
	bs, records, _ := pruneMediumFixture(t)

	verdicts, err := PruneApply(context.Background(), pruneNow, pruneTodayOnlyChain(), bs, records, onMedium(pruneTestMedium), nil)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	v := pruneFindVerdict(t, verdicts, "expired.dump")
	if v.Action != PruneRefuse {
		t.Fatalf("Action = %s, want %s", v.Action, PruneRefuse)
	}
	if v.Medium != pruneTestMedium {
		t.Errorf("Medium = %q, want %q: a refusal still has to say where the deletion would have happened", v.Medium, pruneTestMedium)
	}
}

// TestPruneOnMedium_RefusesWhenTheDeleteItselfRefuses is the FR-16
// re-check reaching the verdict. The re-check lives one package over (it
// stats the object and compares it against the placement record); what
// this pins is that its refusal is carried through as a REFUSE naming the
// medium, and never swallowed into a DELETE.
func TestPruneOnMedium_RefusesWhenTheDeleteItselfRefuses(t *testing.T) {
	bs, records, _ := pruneMediumFixture(t)
	pruner := &recordingPruner{err: errors.New("the object at prod/pg/expired.dump is 12 bytes and the placement records 7")}

	verdicts, err := PruneApply(context.Background(), pruneNow, pruneTodayOnlyChain(), bs, records, onMedium(pruneTestMedium), pruner)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	v := pruneFindVerdict(t, verdicts, "expired.dump")
	if v.Action != PruneRefuse {
		t.Fatalf("Action = %s, want %s", v.Action, PruneRefuse)
	}
	if !strings.Contains(v.Reason, "12 bytes") {
		t.Errorf("Reason = %q, which loses what the identity check actually found; an operator reconciling this needs the mismatch, not the fact that there was one", v.Reason)
	}
}

// TestPruneOnMedium_KeepIsNotRefuse is #390's distinction carried onto the
// medium path. KEEP asserts a tier selected the artifact; REFUSE asserts
// it was a delete candidate and a safety check said no. Collapsing them is
// how a prune reports a decision it did not make, and on this path the
// collapse is easy to write by accident, because both leave the object
// exactly where it is.
func TestPruneOnMedium_KeepIsNotRefuse(t *testing.T) {
	bs, records, _ := pruneMediumFixture(t)
	// Same fixture, but the artifact is inside the window, so a tier does
	// select it.
	records[0].DiscoveredAt = pruneNow
	records[0].UpdatedAt = pruneNow
	pruner := &recordingPruner{}

	verdicts, err := PruneApply(context.Background(), pruneNow, pruneTodayOnlyChain(), bs, records, onMedium(pruneTestMedium), pruner)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	v := pruneFindVerdict(t, verdicts, "expired.dump")
	if v.Action != PruneKeep {
		t.Fatalf("Action = %s, want %s: a tier selects this artifact and nothing was refused", v.Action, PruneKeep)
	}
	if len(v.Tiers) == 0 {
		t.Error("a KEEP verdict on a medium names no tier, so it cannot say what kept the artifact")
	}
	if v.Medium != pruneTestMedium {
		t.Errorf("Medium = %q, want %q", v.Medium, pruneTestMedium)
	}
	if len(pruner.calls) != 0 {
		t.Errorf("a kept artifact reached the medium delete: %v", pruner.calls)
	}
}

// TestPruneOnMedium_RefusesAnArtifactThatIsNotAFinalManagedOne re-runs
// FR-20's "never a .partial" guarantee on this path. The local path
// re-derives it from rec.State at the moment of the delete rather than
// trusting DecideKeep's own upstream filtering, and this one has to do the
// same or the guarantee holds on one path and not the other.
func TestPruneOnMedium_RefusesAnArtifactThatIsNotAFinalManagedOne(t *testing.T) {
	bs, records, _ := pruneMediumFixture(t)
	records[0].State = string(lifecycle.Transferring)
	pruner := &recordingPruner{}

	verdicts, err := PruneApply(context.Background(), pruneNow, pruneTodayOnlyChain(), bs, records, onMedium(pruneTestMedium), pruner)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	// An in-flight artifact gets no verdict at all from GFS, so the
	// strongest available assertion is that nothing was deleted for it.
	for _, v := range verdicts {
		if v.Action == PruneDelete {
			t.Errorf("%s was marked for deletion while its journal state is %s", v.Artifact, records[0].State)
		}
	}
	if len(pruner.calls) != 0 {
		t.Errorf("a TRANSFERRING artifact reached the medium delete: %v", pruner.calls)
	}
}

// TestPruneOnMedium_RefusesTheLastKnownGoodArtifact is FR-19 on this path.
// It is checked independently of the Keep flag for the reason the local
// path checks it independently: a caller that passed a verdict computed
// from different records must never be able to talk this function into
// removing the last good copy.
func TestPruneOnMedium_RefusesTheLastKnownGoodArtifact(t *testing.T) {
	bs, records, _ := pruneMediumFixture(t)
	pruner := &recordingPruner{}

	// The today-only chain with FR-19 turned back on. The artifact is
	// older than every window, and it is the only one in the set, so
	// protection is the single thing standing between it and a delete.
	protecting := pruneTodayOnlyChain()
	on := true
	protecting.ProtectLastKnownGood = &on

	verdicts, err := PruneApply(context.Background(), pruneNow, protecting, bs, records, onMedium(pruneTestMedium), pruner)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	v := pruneFindVerdict(t, verdicts, "expired.dump")
	if v.Action == PruneDelete {
		t.Fatalf("the only validated artifact in the set was deleted from %q; FR-19 protects it", v.Medium)
	}
	if len(pruner.calls) != 0 {
		t.Errorf("the last-known-good artifact reached the medium delete: %v", pruner.calls)
	}
}

// TestPruneOnMedium_NeverTouchesTheLocalFile is the containment claim for
// this path, and it is why the fixture leaves a real file on disk. The
// artifact's durable copy is on a medium; the local path is not this
// verdict's subject, and a decision that removed it would be deleting
// something nothing decided about.
func TestPruneOnMedium_NeverTouchesTheLocalFile(t *testing.T) {
	bs, records, path := pruneMediumFixture(t)
	pruner := &recordingPruner{}

	if _, err := PruneApply(context.Background(), pruneNow, pruneTodayOnlyChain(), bs, records, onMedium(pruneTestMedium), pruner); err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	pruneMustExist(t, path)
}

// --- and only now, the delete ---

// TestPruneOnMedium_DeletesAnExpiredArtifactThroughTheMediumPruner is the
// success case. It comes last on purpose.
func TestPruneOnMedium_DeletesAnExpiredArtifactThroughTheMediumPruner(t *testing.T) {
	bs, records, _ := pruneMediumFixture(t)
	pruner := &recordingPruner{}

	verdicts, err := PruneApply(context.Background(), pruneNow, pruneTodayOnlyChain(), bs, records, onMedium(pruneTestMedium), pruner)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	v := pruneFindVerdict(t, verdicts, "expired.dump")
	if v.Action != PruneDelete {
		t.Fatalf("Action = %s (%s), want %s", v.Action, v.Reason, PruneDelete)
	}
	if v.Medium != pruneTestMedium {
		t.Errorf("Medium = %q, want %q", v.Medium, pruneTestMedium)
	}
	if !strings.Contains(v.Reason, pruneTestMedium) {
		t.Errorf("Reason = %q, which does not name where the deletion happened; FR-30 asks the dry-run to explain per-artifact WHERE, not only whether", v.Reason)
	}
	want := []string{"expired.dump on " + pruneTestMedium}
	if len(pruner.calls) != 1 || pruner.calls[0] != want[0] {
		t.Fatalf("medium deletes = %v, want %v", pruner.calls, want)
	}
}

// TestPruneOnMedium_DecideDeletesNothing is PruneDecide's no-mutation
// contract on this path. The dry-run is mandatory, so the function that
// serves it must be provably incapable of reaching a delete.
func TestPruneOnMedium_DecideDeletesNothing(t *testing.T) {
	bs, records, path := pruneMediumFixture(t)

	verdicts, err := PruneDecide(pruneNow, pruneTodayOnlyChain(), bs, records, onMedium(pruneTestMedium))
	if err != nil {
		t.Fatalf("PruneDecide: %v", err)
	}
	v := pruneFindVerdict(t, verdicts, "expired.dump")
	if v.Action != PruneDelete {
		t.Fatalf("Action = %s (%s), want %s: the dry-run has to show what would happen", v.Action, v.Reason, PruneDelete)
	}
	if v.Medium != pruneTestMedium {
		t.Errorf("Medium = %q, want %q: FR-30's dry-run names the medium for every proposed deletion", v.Medium, pruneTestMedium)
	}
	pruneMustExist(t, path)
}

// TestPruneOnMedium_ALocalArtifactStillTakesTheLocalPath is the control
// for every test above. Without it they would all pass against an
// implementation that sent everything down the medium path, including the
// local files FR-20 exists to protect.
func TestPruneOnMedium_ALocalArtifactStillTakesTheLocalPath(t *testing.T) {
	bs, records, path := pruneMediumFixture(t)
	pruner := &recordingPruner{}

	// The local branch returns the canonicalized path, which on macOS
	// resolves the temp root through /private. Taken before the apply,
	// because afterwards there is no file left to canonicalize.
	wantPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	verdicts, err := PruneApply(context.Background(), pruneNow, pruneTodayOnlyChain(), bs, records, onMedium(config.MediumLocal), pruner)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	v := pruneFindVerdict(t, verdicts, "expired.dump")
	if v.Action != PruneDelete {
		t.Fatalf("Action = %s (%s), want %s", v.Action, v.Reason, PruneDelete)
	}
	if v.Medium != config.MediumLocal {
		t.Errorf("Medium = %q, want %q", v.Medium, config.MediumLocal)
	}
	if v.Path != wantPath {
		t.Errorf("Path = %q, want %q: a local deletion still names the file", v.Path, wantPath)
	}
	pruneMustNotExist(t, wantPath)
	if len(pruner.calls) != 0 {
		t.Errorf("a local artifact reached the medium delete: %v", pruner.calls)
	}
}

// TestPruneAllLocal_IsTheMediumFreeDeployment pins the locator every
// pre-EPIC-E caller passes. FR-35's compatibility claim rests on this
// being exactly today's behaviour, so it is asserted rather than assumed.
func TestPruneAllLocal_IsTheMediumFreeDeployment(t *testing.T) {
	id := gfsMustArtifact(t, gfsMustSet(t, "production", "postgres-primary"), "a.dump")
	medium, known := AllLocal(id)
	if !known || medium != config.MediumLocal {
		t.Fatalf("AllLocal = (%q, %v), want (%q, true)", medium, known, config.MediumLocal)
	}
}

// TestPruneRefusesWithoutALocator is the nil-input refusal. A nil locator
// would have to mean something, and every meaning available is a guess
// about where a copy of a backup lives.
func TestPruneRefusesWithoutALocator(t *testing.T) {
	bs, records, _ := pruneMediumFixture(t)

	if _, err := PruneDecide(pruneNow, pruneTodayOnlyChain(), bs, records, nil); err == nil {
		t.Fatal("PruneDecide accepted a nil locator")
	}
}
