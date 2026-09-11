package retention

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/backupd/core/internal/config"
	"github.com/spdrman/backupd/core/internal/lifecycle"
	"github.com/spdrman/backupd/core/internal/model"
	"github.com/spdrman/backupd/core/internal/state"
)

// lastknowngoodpresence_test.go proves FR-30's confirmation exists. This
// file is about WHEN it is taken, which is a different claim and the one
// the deletion actually rests on.
//
// The confirmation asks a question about a file, and a file is something
// another process can take away. So the answer has a shelf life measured
// in whatever happens next, and a pass that removes a thousand artifacts
// spends real time inside itself. Two moments therefore matter and neither
// is the plan's own:
//
//   - between the plan and the first delete. PruneDecide's answer was
//     taken while the plan was being composed, and the deletes rest on an
//     answer taken after it (TestPruneReconfirmsTheLastKnownGoodAfterThePlan
//     BeforeItDeletes). This is the same argument PruneApply's second
//     pruneVerifySafeToDelete call rests on, applied to FR-19's own
//     protection instead of to FR-20's path checks.
//
//   - between one delete and the next. A confirmation taken once for a
//     whole pass says nothing about the pass's second half, and half a
//     pass carried out is the outcome prune.go is arranged to make
//     impossible: the artifacts a partial run removed are the ones that
//     were still readable
//     (TestPruneHoldsTheRestOfThePassWhenTheLastKnownGoodGoesPartwayThrough).
//
// Both tests need the copy to disappear at a controlled instant INSIDE
// PruneApply, because a removal before the call is caught by the plan's
// own confirmation and proves nothing about the apply's. There are exactly
// two seams for that, and both of them are ordinary injected parameters
// this package already takes from its caller: the ArtifactLocator, which
// every confirmation asks first, and the MediumPruner, which the delete
// loop calls and no confirmation does.

// lkgFreshnessRecords is the two-artifact fixture both halves of this file
// start from: a last known good with a real file, and one aged-out
// artifact that nothing keeps.
func lkgFreshnessRecords(t *testing.T, set model.BackupSetID, root string) (model.ArtifactID, string, model.ArtifactID, string, []state.Record) {
	t.Helper()

	newest := gfsMustArtifact(t, set, "newest.zst")
	newestPath := filepath.Join(root, "newest.zst")
	pruneWriteFile(t, newestPath, "the last known good, and it is here while the plan is composed")

	older := gfsMustArtifact(t, set, "older.zst")
	olderPath := filepath.Join(root, "older.zst")
	pruneWriteFile(t, olderPath, "aged out, and the only copy left the moment the newest goes")

	return newest, newestPath, older, olderPath, []state.Record{
		pruneRecord(newest, lifecycle.Complete, pruneNow.Add(-40*24*time.Hour), newestPath),
		pruneRecord(older, lifecycle.Complete, pruneNow.Add(-90*24*time.Hour), olderPath),
	}
}

// TestPruneReconfirmsTheLastKnownGoodAfterThePlanBeforeItDeletes is the
// claim that the evidence under a delete is fresher than the plan above
// it.
//
// PruneApply computes its verdicts and then removes files, and the gap
// between those two things is where another process gets to act. FR-19's
// protection is the one KEEP in this file that authorises deleting
// everything else in the backup set, so an apply that carries the plan's
// own confirmation into the delete loop is deleting on the strength of a
// stat nobody has repeated since the plan was drawn up.
//
// The removal is timed through the locator, which is the only thing the
// apply half asks anything at all between finishing its plan and removing
// its first file. The test measures how many questions the plan itself
// asks rather than hardcoding a count, so it pins the ORDER of events and
// not this package's current call pattern: the answer to the first
// question asked after the plan arrives with the file already gone.
//
// Both assertions below are load-bearing and they fail in different
// worlds. The first fires when the apply asks nothing at all, which is an
// apply resting entirely on the plan's evidence, and it is the one that
// names why the second happened.
func TestPruneReconfirmsTheLastKnownGoodAfterThePlanBeforeItDeletes(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "lkg-freshness", "set")
	newest, newestPath, _, olderPath, records := lkgFreshnessRecords(t, set, root)
	bs := pruneBackupSet(set, root)

	// The calibration run: the plan, with nothing taken away, counting
	// every question it asks about the artifact FR-19 protects.
	asked := 0
	counting := func(a model.ArtifactID) Location {
		if a == newest {
			asked++
		}
		return Location{Medium: config.MediumLocal, Status: LocationConfirmed}
	}
	plan, err := PruneDecide(pruneNow, lkgPresenceChain(true), bs, records, counting)
	if err != nil {
		t.Fatalf("PruneDecide: %v", err)
	}
	if v := pruneFindVerdict(t, plan, "older.zst"); v.Action != PruneDelete {
		t.Fatalf("precondition: older.zst is %s in the plan, want %s. This test is about an apply that carries out a plan, so the plan has to want something deleted (reason: %s)", v.Action, PruneDelete, v.Reason)
	}
	planQuestions := asked
	if planQuestions == 0 {
		t.Fatalf("precondition: the plan asked nothing about %s, so counting questions cannot tell the plan's evidence from the apply's", newest.Name)
	}

	// The real run. Everything up to and including the plan sees the file;
	// the first question asked after it does not, which is another process
	// removing the last readable restore point at the instant this pass
	// starts deleting.
	asked = 0
	var removeErr error
	vanishing := func(a model.ArtifactID) Location {
		if a == newest {
			asked++
			if asked > planQuestions && removeErr == nil {
				if err := os.Remove(newestPath); err != nil && !os.IsNotExist(err) {
					removeErr = err
				}
			}
		}
		return Location{Medium: config.MediumLocal, Status: LocationConfirmed}
	}
	applied, err := PruneApply(context.Background(), pruneNow, lkgPresenceChain(true), bs, records, vanishing, nil)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	if removeErr != nil {
		t.Fatalf("removing %s partway through the apply: %v", newestPath, removeErr)
	}

	if asked <= planQuestions {
		t.Errorf("the apply asked about %s %d times, exactly the %d its own plan asked and not one more: nothing between the plan and the deletes re-established that the copy FR-19 protects is still there, so every deletion in this pass rests on evidence taken before the pass began",
			newest.Name, asked, planQuestions)
	}
	if v := pruneFindVerdict(t, applied, "older.zst"); v.Action != PruneRefuse {
		t.Errorf("older.zst: Action = %s, want %s. The last known good's file went away after the plan was drawn up and before anything was deleted, and older.zst is the only readable copy this backup set has left (reason: %s)", v.Action, PruneRefuse, v.Reason)
	} else if !strings.Contains(v.Reason, "last known good") {
		t.Errorf("older.zst was refused without naming why, so an operator cannot act on it. Reason: %q", v.Reason)
	}
	pruneMustExist(t, olderPath)
}

// hookPruner is a MediumPruner that runs before it answers, which is what
// lets a test act at a moment strictly inside the delete loop. Every
// confirmation in prune.go stats and reads only, so nothing but the loop
// itself reaches this: an implementation that never got as far as a
// deletion never runs the hook, and the test's own preconditions say so
// rather than passing quietly.
type hookPruner struct {
	before func()
	calls  []string
}

func (p *hookPruner) DeleteFromMedium(_ context.Context, rec state.Record, medium string) error {
	p.calls = append(p.calls, rec.Artifact.Name+" on "+medium)
	if p.before != nil {
		p.before()
	}
	return nil
}

// TestPruneHoldsTheRestOfThePassWhenTheLastKnownGoodGoesPartwayThrough is
// the confirmation's other deadline.
//
// A pass removing a thousand artifacts is not an instant. If the copy
// FR-19 protects goes at artifact four hundred, a confirmation taken once
// before the loop has nothing to say about the six hundred deletes that
// follow, and those six hundred are exactly the copies that were still
// readable. pruneHoldEveryDelete's own doc calls half a pass carried out
// the outcome this file is arranged to make impossible, and a per-pass
// confirmation is the shape that permits it.
//
// The pass here is four artifacts in name order, which is the order
// PruneApply works in. The first is an object on a storage medium, so its
// removal goes through the injected MediumPruner, and that is the hook: at
// the moment the pass has genuinely started and genuinely deleted
// something, the last known good's file disappears. Everything after it
// must be refused.
//
// The cost of the fix is stated in pruneLastKnownGoodUnconfirmed's own
// doc: it stats and reads only, so a pass of a thousand deletes pays a
// thousand extra Lstats against a thousand unlinks.
func TestPruneHoldsTheRestOfThePassWhenTheLastKnownGoodGoesPartwayThrough(t *testing.T) {
	root := t.TempDir()
	set := gfsMustSet(t, "lkg-midpass", "set")

	// Named so that name order, which is verdict order, is also the order
	// this test needs things to happen in.
	onMediumArtifact := gfsMustArtifact(t, set, "a-on-a-medium.zst")
	second := gfsMustArtifact(t, set, "b-second.zst")
	third := gfsMustArtifact(t, set, "c-third.zst")
	newest := gfsMustArtifact(t, set, "z-newest.zst")

	secondPath := filepath.Join(root, "b-second.zst")
	thirdPath := filepath.Join(root, "c-third.zst")
	newestPath := filepath.Join(root, "z-newest.zst")
	pruneWriteFile(t, secondPath, "still readable when the pass starts")
	pruneWriteFile(t, thirdPath, "and so is this one")
	pruneWriteFile(t, newestPath, "the last known good, until artifact one is done")

	records := []state.Record{
		pruneRecord(newest, lifecycle.Complete, pruneNow.Add(-40*24*time.Hour), newestPath),
		pruneRecord(onMediumArtifact, lifecycle.Complete, pruneNow.Add(-60*24*time.Hour), filepath.Join(root, "a-on-a-medium.zst")),
		pruneRecord(second, lifecycle.Complete, pruneNow.Add(-90*24*time.Hour), secondPath),
		pruneRecord(third, lifecycle.Complete, pruneNow.Add(-120*24*time.Hour), thirdPath),
	}
	bs := pruneBackupSet(set, root)

	where := func(a model.ArtifactID) Location {
		if a == onMediumArtifact {
			return Location{Medium: "cold", Status: LocationConfirmed}
		}
		return Location{Medium: config.MediumLocal, Status: LocationConfirmed}
	}

	// The precondition, and it is the whole reason the refusals below mean
	// anything: with the last known good where it belongs, this plan wants
	// all three of the others gone.
	plan, err := PruneDecide(pruneNow, lkgPresenceChain(true), bs, records, where)
	if err != nil {
		t.Fatalf("PruneDecide: %v", err)
	}
	for _, name := range []string{"a-on-a-medium.zst", "b-second.zst", "c-third.zst"} {
		if v := pruneFindVerdict(t, plan, name); v.Action != PruneDelete {
			t.Fatalf("precondition: %s is %s in the plan, want %s; this test needs a pass with a delete before the loss and two after it (reason: %s)", name, v.Action, PruneDelete, v.Reason)
		}
	}

	var removeErr error
	pruner := &hookPruner{before: func() {
		if err := os.Remove(newestPath); err != nil && !os.IsNotExist(err) {
			removeErr = err
		}
	}}

	applied, err := PruneApply(context.Background(), pruneNow, lkgPresenceChain(true), bs, records, where, pruner)
	if err != nil {
		t.Fatalf("PruneApply: %v", err)
	}
	if removeErr != nil {
		t.Fatalf("removing %s partway through the apply: %v", newestPath, removeErr)
	}
	if len(pruner.calls) != 1 {
		t.Fatalf("precondition: the medium pruner was called %d times (%v), want exactly once; the last known good is taken away by that call, so without it nothing in this test has happened at all", len(pruner.calls), pruner.calls)
	}

	for _, tc := range []struct {
		name string
		path string
	}{
		{"b-second.zst", secondPath},
		{"c-third.zst", thirdPath},
	} {
		v := pruneFindVerdict(t, applied, tc.name)
		if v.Action != PruneRefuse {
			t.Errorf("%s: Action = %s, want %s. The copy FR-19 protects went away after this pass had already deleted one artifact, and the pass went on deleting the ones that were still readable (reason: %s)", tc.name, v.Action, PruneRefuse, v.Reason)
		}
		if _, err := os.Lstat(tc.path); err != nil {
			t.Errorf("%s was removed while this backup set's last known good had no readable copy, which is FR-30's invariant broken partway through a pass: %v", tc.name, err)
		}
	}
}
