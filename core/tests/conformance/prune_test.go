package conformance_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file is the prune leg of #242's composed scenario, and it is
// deliberately smaller than the issue asks for, because #239 owns the
// medium-aware prune and its lane is not in this tree.
//
// What can be checked today is the thing that matters most while that is
// true: what prune does when it meets an artifact this build has just
// moved onto a medium. The composed scenario produces one on every run,
// which no unit fixture in internal/retention does, and the answer has to
// be a refusal rather than a delete. There are two wrong answers available
// and both lose data:
//
//   - delete the local path, which is not there any more, and record a
//     DELETE that did nothing while the object on the medium survives
//     unreferenced;
//   - reach for the object and delete THAT, which is the medium-aware
//     prune #239 is writing and which this build must not improvise.
//
// The right answer while neither is built is REFUSE, and this pins it.

// TestPruneRefusesAnArtifactWhoseOnlyCopyIsOnAMedium is the safety property
// that has to hold in the window between the move engine landing and #239's
// medium-aware prune landing.
//
// The artifact it looks at is one the chain itself moved, in this same
// process, over a real S3 endpoint. That is the whole reason this check is
// here rather than in internal/retention: a hand-built record with a medium
// placement is easy to write, and easy to write slightly wrong in a way
// that makes prune take a different branch.
func TestPruneRefusesAnArtifactWhoseOnlyCopyIsOnAMedium(t *testing.T) {
	w := newWorld(t)
	wa := newWatcher(t, w.journal, w.ids())
	wa.observe("before the cycle")

	// Move the monthly artifact onto the offsite medium, for real.
	runPass(t, w, scenarioNow, wa)
	summer := w.artifactNamed(t, "2026-07-15T02-00-00Z.dump")
	assertActiveOn(t, w, summer.id, mediumOffsite)
	if w.localExists(summer.id) {
		t.Fatalf("%s still has a local copy, so this check is not about a medium-resident artifact", summer.id.Name)
	}

	// Now age the clock so that nothing selects it any more, which is what
	// makes it a delete candidate at all. Two years on, its month is long
	// out of the monthly window and its year's representative is a
	// different artifact.
	far := scenarioNow.AddDate(2, 0, 0)
	records := w.records()

	// app.ActiveMediumFromRecords is the locator the product's own prune
	// pass uses (internal/app/prune.go), and it is the one this scenario
	// needs: the artifact under test lives on a medium, and AllLocal would
	// answer the question wrongly for exactly that case.
	verdicts, err := retention.PruneDecide(far, w.cfg.Retention, w.backupSet(), records, app.ActiveMediumFromRecords(records))
	if err != nil {
		t.Fatalf("the prune pass failed: %v", err)
	}

	byArtifact := map[model.ArtifactID]retention.PruneVerdict{}
	for _, v := range verdicts {
		byArtifact[v.Artifact] = v
	}

	got, ok := byArtifact[summer.id]
	if !ok {
		t.Fatalf("the prune pass produced no verdict for %s at all", summer.id.Name)
	}
	if got.Action == retention.PruneKeep {
		t.Fatalf("%s is still kept two years on, so this check never reached the interesting branch: %s",
			summer.id.Name, got.Reason)
	}
	if got.Action != retention.PruneRefuse {
		t.Errorf("prune's verdict for %s is %s, want %s. Its only copy is on %q and this build has no "+
			"medium-aware prune (#239), so the only safe answer is a refusal: %s",
			summer.id.Name, got.Action, retention.PruneRefuse, mediumOffsite, got.Reason)
	}

	// The control. A run in which prune refused EVERYTHING would satisfy
	// the assertion above and prove nothing, so an artifact that is still
	// on local disk and unselected has to come back DELETE.
	stale := w.artifactNamed(t, "2026-07-01T02-00-00Z.dump")
	if !w.localExists(stale.id) {
		t.Fatalf("%s is not on local disk, so it cannot be this check's control", stale.id.Name)
	}
	control, ok := byArtifact[stale.id]
	if !ok {
		t.Fatalf("the prune pass produced no verdict for the control artifact %s", stale.id.Name)
	}
	if control.Action != retention.PruneDelete {
		t.Fatalf("prune would not delete %s either (%s: %s), so its refusal above says nothing about mediums",
			stale.id.Name, control.Action, control.Reason)
	}

	// And applying it has to leave the medium copy alone. A DECIDE that
	// refuses and an APPLY that deletes anyway is exactly the gap between
	// a dry run and the real thing that FR-20 exists to close.
	//
	// The medium pruner this is handed deletes nothing and records that it
	// was asked, which is a stronger check than the object still being
	// there afterwards: an apply that decided to delete and then failed to
	// reach the endpoint would leave the object in place too, and that is
	// not the same outcome at all.
	pruner := &recordingMediumPruner{}
	applied, err := retention.PruneApply(w.ctx, far, w.cfg.Retention, w.backupSet(), records,
		app.ActiveMediumFromRecords(records), pruner)
	if err != nil {
		t.Fatalf("applying the prune pass failed: %v", err)
	}
	for _, v := range applied {
		if v.Artifact == summer.id && v.Action == retention.PruneDelete {
			t.Errorf("PruneApply deleted %s, whose only copy is on %q: %s", v.Artifact.Name, mediumOffsite, v.Reason)
		}
	}
	if asked := pruner.calls(); len(asked) != 0 {
		t.Errorf("PruneApply asked to delete %v from a medium; the only medium-resident artifact in this scenario is the one it must refuse", asked)
	}
	if _, err := adapter().StatObject(w.ctx, w.offsite, objectKeyOf(t, w, summer.id)); err != nil {
		t.Fatalf("THE OBJECT ON THE MEDIUM IS GONE after a prune pass: %v", err)
	}
	if _, err := os.Lstat(w.localPath(stale.id)); err == nil {
		t.Errorf("the control artifact %s is still on disk after PruneApply said DELETE", stale.id.Name)
	}
	wa.report()
}

// objectKeyOf is where the journal says an artifact's copy on the offsite
// medium is. It reads the placement rather than recomputing the key,
// because a check that recomputed it would pass just as happily against a
// journal pointing somewhere else entirely.
func objectKeyOf(t *testing.T, w *world, id model.ArtifactID) string {
	t.Helper()
	rec, err := w.journal.Get(w.ctx, id)
	if err != nil {
		t.Fatalf("reading %s: %v", id.Name, err)
	}
	p, ok := placementOn(rec, mediumOffsite)
	if !ok || p.Status != state.PlacementActive {
		t.Fatalf("%s has no ACTIVE placement on %q: %s", id.Name, mediumOffsite, describe(rec))
	}
	return p.Location
}

// recordingMediumPruner is the medium half of PruneApply, and it deletes
// nothing.
//
// It is not a stub that quietly succeeds. This scenario's whole claim is
// that prune refuses the one artifact whose only copy is on a medium, so
// any call at all is the failure, and recording the calls says which
// artifact it was rather than leaving the reader to work that out from an
// object that vanished.
type recordingMediumPruner struct {
	mu    sync.Mutex
	asked []string
}

func (p *recordingMediumPruner) DeleteFromMedium(_ context.Context, rec state.Record, medium string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, rec.Artifact.Name+" on "+medium)
	return nil
}

func (p *recordingMediumPruner) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.asked...)
}
