// This file is the prune leg of #242's composed scenario, and it used to
// say something else.
//
// It was written while #239's medium-aware prune was unbuilt, and it
// pinned the only safe answer available then: an artifact whose only copy
// is on a medium gets a REFUSE, because the two other answers on offer
// both lose data (delete a local path that is not there any more and
// record a DELETE that did nothing, or improvise a reach for the object).
// The assertion said so in its own message, "this build has no
// medium-aware prune (#239)", and that stopped being true. #239 is in
// this tree. FR-30's prune half exists, and it deletes the object.
//
// So the promise moved. What is worth pinning composed, over a real
// endpoint, on an artifact this same process really moved, is the shape of
// that delete rather than its absence:
//
//   - the decision and the evidence are in different packages, on purpose.
//     internal/retention decides DELETE without reading the medium at all
//     (FR-32), and internal/placement's Reclaimer re-proves the object's
//     identity at the moment of the delete (FR-16). A composed run is the
//     only place both halves are the real ones.
//   - three answers, not two. A medium-resident artifact a tier still
//     wants is a KEEP, one nothing wants is a DELETE, and one whose object
//     cannot be re-proved is a REFUSE. A pass that collapsed any two of
//     those would satisfy a weaker suite.
//   - the refusal is the one FR-16 names, and it is this matrix's V6 line:
//     something else is at the key now, and the delete does not happen.
//
// The FR-16 re-check has unit coverage against a double in
// internal/placement/reclaim_test.go, including the same-length swap this
// endpoint cannot show (rclone's s3 backend attests no checksum, so size
// is the whole proof here). What that suite cannot do is run it against an
// object a real move really put on a real bucket, which is the one thing
// this file adds.
package conformance_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// TestPruneRemovesAnArtifactsOnlyCopyFromAMedium is FR-30's prune half,
// composed.
//
// The artifact it deletes is one the chain itself moved, in this same
// process, over a real S3 endpoint. That is the whole reason this check is
// here rather than in internal/retention: a hand-built record with a
// medium placement is easy to write, and easy to write slightly wrong in a
// way that makes prune take a different branch.
func TestPruneRemovesAnArtifactsOnlyCopyFromAMedium(t *testing.T) {
	w := newWorld(t)
	wa := newWatcher(t, w.journal, w.ids())
	wa.observe("before the cycle")

	// Move the monthly and annual artifacts onto their mediums, for real.
	runPass(t, w, scenarioNow, wa)

	summer := w.artifactNamed(t, "2026-07-15T02-00-00Z.dump")
	assertActiveOn(t, w, summer.id, mediumOffsite)
	if w.localExists(summer.id) {
		t.Fatalf("%s still has a local copy, so this check is not about a medium-resident artifact", summer.id.Name)
	}
	ancient := w.artifactNamed(t, "2024-06-15T02-00-00Z.dump")
	assertActiveOn(t, w, ancient.id, mediumAnnual)

	// Two years on, nothing selects the July artifacts any more: the
	// monthly window has long since passed them, and their year's annual
	// representative is the newest artifact of 2026, which is neither of
	// them. The oldest artifact still owns its own year, so it stays.
	far := scenarioNow.AddDate(2, 0, 0)
	records := w.records()

	// app.ActiveMediumFromRecords is the locator the product's own prune
	// pass uses (internal/app/prune.go), and it is the one this scenario
	// needs: two of these artifacts live on mediums, and AllLocal would
	// answer the question wrongly for exactly that case.
	verdicts, err := retention.PruneDecide(far, w.cfg.Retention, w.backupSet(), records, app.ActiveMediumFromRecords(records))
	if err != nil {
		t.Fatalf("the prune pass failed: %v", err)
	}
	byArtifact := verdictsByArtifact(verdicts)

	// Three different answers in one pass, over four artifacts on three
	// different mediums. This is what makes each of them mean something:
	// a pass that answered the same thing everywhere would satisfy any
	// one of these on its own.
	stale := w.artifactNamed(t, "2026-07-01T02-00-00Z.dump")
	fresh := w.artifactNamed(t, "2026-09-03T02-00-00Z.dump")
	assertPruneVerdict(t, byArtifact, summer.id, retention.PruneDelete, mediumOffsite)
	assertPruneVerdict(t, byArtifact, stale.id, retention.PruneDelete, config.MediumLocal)
	assertPruneVerdict(t, byArtifact, ancient.id, retention.PruneKeep, mediumAnnual)
	assertPruneVerdict(t, byArtifact, fresh.id, retention.PruneKeep, config.MediumLocal)

	// FR-30's dry-run line: "the mandatory dry-run names the medium for
	// every proposed deletion". PruneDecide is exactly what
	// `retention --dry-run` renders, so the sentence it hands over has to
	// carry the medium and say that the object's identity is re-checked
	// before anything is removed. An operator confirming a plan is
	// confirming this text.
	deletion := byArtifact[summer.id]
	for _, want := range []string{mediumOffsite, "re-check"} {
		if !strings.Contains(deletion.Reason, want) {
			t.Errorf("the proposed deletion of %s does not mention %q, so the dry-run does not say where it happens "+
				"or what is checked first: %s", summer.id.Name, want, deletion.Reason)
		}
	}

	// The object is there before the apply, so its absence afterwards is
	// this pass removing it rather than it never having been written.
	key := objectKeyOf(t, w, summer.id)
	if _, err := adapter().StatObject(w.ctx, w.offsite, key); err != nil {
		t.Fatalf("%s's object is not on %q before the prune, so this check cannot show a prune removed it: %v",
			summer.id.Name, mediumOffsite, err)
	}
	ancientKey := objectKeyOf(t, w, ancient.id)

	// The real Reclaimer, wrapped only to count. Handing PruneApply a
	// stub that returns nil would prove the wiring and nothing about
	// FR-16, and the whole point of running this composed is that both
	// halves are the product's.
	pruner := &countingPruner{inner: w.reclaimer()}
	applied, err := retention.PruneApply(w.ctx, far, w.cfg.Retention, w.backupSet(), records,
		app.ActiveMediumFromRecords(records), pruner)
	if err != nil {
		t.Fatalf("applying the prune pass failed: %v", err)
	}
	appliedBy := verdictsByArtifact(applied)

	if got := appliedBy[summer.id]; got.Action != retention.PruneDelete {
		t.Errorf("PruneApply downgraded the deletion of %s to %s: %s", summer.id.Name, got.Action, got.Reason)
	}
	if asked := pruner.calls(); len(asked) != 1 || asked[0] != summer.id.Name+" on "+mediumOffsite {
		t.Errorf("PruneApply asked the medium pruner for %v; the one medium-resident artifact nothing selects is %s on %q",
			asked, summer.id.Name, mediumOffsite)
	}
	if _, err := adapter().StatObject(w.ctx, w.offsite, key); err == nil {
		t.Errorf("%s's object is still on %q after a prune that said DELETE, so nothing was actually removed", summer.id.Name, mediumOffsite)
	}

	// The controls, in both directions. The local delete really happened,
	// so the medium branch is not the only one running; and the KEEP on
	// the other medium is untouched, so this pass did not simply empty
	// every bucket it could reach.
	if _, err := os.Lstat(w.localPath(stale.id)); err == nil {
		t.Errorf("the local artifact %s is still on disk after PruneApply said DELETE", stale.id.Name)
	}
	if _, err := adapter().StatObject(w.ctx, w.annual, ancientKey); err != nil {
		t.Errorf("%s's object on %q is gone, and no verdict said to delete it: %v", ancient.id.Name, mediumAnnual, err)
	}
	if !w.localExists(fresh.id) {
		t.Errorf("%s is gone from local disk, and its verdict was KEEP", fresh.id.Name)
	}
	wa.report()
}

// TestPruneRefusesAnObjectThatIsNoLongerTheOneTheJournalRecorded is the
// conformance matrix's V6 line run rather than described: "a fixture that
// swaps the object behind a key before prune".
//
// It is the same world and the same expired artifact as the cell above.
// The only difference is that something else is at the key by the time the
// apply runs, and that single difference has to turn a DELETE into a
// REFUSE with the object left alone.
//
// Two things are worth reading carefully in what it asserts.
//
// First, PruneDecide is unchanged by the swap. It still says DELETE,
// because retention decides from the journal and the chain and may not
// read a medium at all (FR-32). The refusal arrives at APPLY time, from
// the Reclaimer, immediately before the delete. That split is the design
// rather than an accident, and a build that moved the check into the
// decision would answer this test correctly while making retention
// verdicts depend on what a bucket happened to say.
//
// Second, the proof here is the recorded SIZE. FR-16 asks for the
// strongest practical attributes and the Reclaimer asks for the checksum
// too, but rclone's s3 backend produces no full-object SHA-256, so against
// this endpoint size is the whole of it. The same-length swap, which size
// alone cannot see, is covered against an attesting double in
// internal/placement/reclaim_test.go. Saying which half runs where is the
// difference between a suite that knows what it proved and one that
// assumes.
func TestPruneRefusesAnObjectThatIsNoLongerTheOneTheJournalRecorded(t *testing.T) {
	w := newWorld(t)
	wa := newWatcher(t, w.journal, w.ids())
	wa.observe("before the cycle")

	runPass(t, w, scenarioNow, wa)
	summer := w.artifactNamed(t, "2026-07-15T02-00-00Z.dump")
	assertActiveOn(t, w, summer.id, mediumOffsite)

	// The premise. A placement that recorded no size would be refused by
	// FR-16's closing line instead ("neither a size nor a checksum this
	// endpoint can attest"), which is also a refusal and would make this
	// cell pass while proving nothing about the swap.
	rec, err := w.journal.Get(w.ctx, summer.id)
	if err != nil {
		t.Fatalf("reading %s: %v", summer.id.Name, err)
	}
	p, ok := placementOn(rec, mediumOffsite)
	if !ok || p.Size == nil {
		t.Fatalf("%s's placement on %q records no size, so a size mismatch is not what would refuse this delete: %s",
			summer.id.Name, mediumOffsite, describe(rec))
	}

	// The swap: same key, different bytes, different length.
	key := objectKeyOf(t, w, summer.id)
	impostor := []byte("something else entirely, and a length nothing in this journal ever recorded for this artifact")
	if int64(len(impostor)) == *p.Size {
		t.Fatalf("the replacement is exactly the recorded %d bytes, so the size check cannot see it and this cell "+
			"would pass for the wrong reason", *p.Size)
	}
	putObject(t, w, w.offsite, key, impostor)

	far := scenarioNow.AddDate(2, 0, 0)
	records := w.records()

	decided, err := retention.PruneDecide(far, w.cfg.Retention, w.backupSet(), records, app.ActiveMediumFromRecords(records))
	if err != nil {
		t.Fatalf("the prune pass failed: %v", err)
	}
	if got := verdictsByArtifact(decided)[summer.id]; got.Action != retention.PruneDelete {
		t.Fatalf("the decision for %s is %s and this cell is about an apply-time refusal of a DELETE: %s",
			summer.id.Name, got.Action, got.Reason)
	}

	pruner := &countingPruner{inner: w.reclaimer()}
	applied, err := retention.PruneApply(w.ctx, far, w.cfg.Retention, w.backupSet(), records,
		app.ActiveMediumFromRecords(records), pruner)
	if err != nil {
		t.Fatalf("applying the prune pass failed: %v", err)
	}
	got := verdictsByArtifact(applied)[summer.id]
	if got.Action != retention.PruneRefuse {
		t.Fatalf("the object behind %s's key was replaced and prune's verdict is %s, not %s. FR-16 exists to stop "+
			"exactly this: %s", summer.id.Name, got.Action, retention.PruneRefuse, got.Reason)
	}
	if got.Medium != mediumOffsite {
		t.Errorf("the refusal names medium %q, want %q: a refusal still has to say where the deletion would have happened",
			got.Medium, mediumOffsite)
	}
	if !strings.Contains(got.Reason, "bytes") {
		t.Errorf("the refusal does not say what disagreed, so nobody can reconcile it: %s", got.Reason)
	}
	t.Logf("prune refused with: %s", got.Reason)

	// The object survives, and it is still the impostor. Asserting the
	// bytes rather than only its presence matters: a delete followed by a
	// failed re-upload would also leave "an object" there.
	body := readObject(t, w.ctx, w.offsite, key)
	if string(body) != string(impostor) {
		t.Errorf("the object at %q is not what this cell put there, so something acted on it: %d bytes", key, len(body))
	}
	if asked := pruner.calls(); len(asked) != 1 {
		t.Errorf("the medium pruner was asked %v; it has to be reached exactly once, or the refusal above came from "+
			"somewhere other than the re-check", asked)
	}

	// The control. A pass that refused EVERYTHING would satisfy every
	// assertion above, so the local artifact nothing selects has to have
	// been deleted in this same call.
	stale := w.artifactNamed(t, "2026-07-01T02-00-00Z.dump")
	if v := verdictsByArtifact(applied)[stale.id]; v.Action != retention.PruneDelete {
		t.Fatalf("prune would not delete %s either (%s: %s), so its refusal above says nothing about the swap",
			stale.id.Name, v.Action, v.Reason)
	}
	if _, err := os.Lstat(w.localPath(stale.id)); err == nil {
		t.Errorf("the control artifact %s is still on disk after PruneApply said DELETE", stale.id.Name)
	}
	wa.report()
}

// reclaimer is the product's own MediumPruner, built the way
// internal/app.Service.mediumPruner builds it: the real store, the real
// resolver, and no shortcut around the FR-16 re-check.
func (w *world) reclaimer() retention.MediumPruner {
	return &placement.Reclaimer{
		Store:   adapter(),
		Mediums: scenarioResolver{w: w},
	}
}

// countingPruner wraps a real MediumPruner and records who it was asked
// about.
//
// It answers nothing itself. A stub that returned nil would make every
// assertion about a deleted object true without anything having proved the
// object's identity first, which is the whole thing FR-16 is.
type countingPruner struct {
	inner retention.MediumPruner

	mu    sync.Mutex
	asked []string
}

func (p *countingPruner) DeleteFromMedium(ctx context.Context, rec state.Record, medium string) error {
	p.mu.Lock()
	p.asked = append(p.asked, rec.Artifact.Name+" on "+medium)
	p.mu.Unlock()
	return p.inner.DeleteFromMedium(ctx, rec, medium)
}

func (p *countingPruner) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.asked...)
}

// verdictsByArtifact indexes a prune pass's output.
func verdictsByArtifact(verdicts []retention.PruneVerdict) map[model.ArtifactID]retention.PruneVerdict {
	out := make(map[model.ArtifactID]retention.PruneVerdict, len(verdicts))
	for _, v := range verdicts {
		out[v.Artifact] = v
	}
	return out
}

// assertPruneVerdict checks one artifact's action AND the medium the
// verdict names, because "DELETE" alone does not say what would be
// deleted, and a verdict that named the wrong place would read as correct.
func assertPruneVerdict(t *testing.T, byArtifact map[model.ArtifactID]retention.PruneVerdict,
	id model.ArtifactID, want retention.PruneAction, medium string) {
	t.Helper()
	got, ok := byArtifact[id]
	if !ok {
		t.Fatalf("the prune pass produced no verdict for %s at all", id.Name)
	}
	if got.Action != want {
		t.Errorf("prune's verdict for %s is %s, want %s: %s", id.Name, got.Action, want, got.Reason)
	}
	if got.Medium != medium {
		t.Errorf("prune's verdict for %s names medium %q, want %q: %s", id.Name, got.Medium, medium, got.Reason)
	}
}

// objectKeyOf is where the journal says an artifact's copy on a medium is.
// It reads the placement rather than recomputing the key, because a check
// that recomputed it would pass just as happily against a journal pointing
// somewhere else entirely.
func objectKeyOf(t *testing.T, w *world, id model.ArtifactID) string {
	t.Helper()
	rec, err := w.journal.Get(w.ctx, id)
	if err != nil {
		t.Fatalf("reading %s: %v", id.Name, err)
	}
	for _, p := range rec.Placements {
		if p.Status == state.PlacementActive && p.Medium != state.MediumLocal {
			return p.Location
		}
	}
	t.Fatalf("%s has no ACTIVE placement on a medium: %s", id.Name, describe(rec))
	return ""
}

// putObject writes body at key on medium, through the same adapter the
// product uploads with.
func putObject(t *testing.T, w *world, medium transport.Medium, key string, body []byte) {
	t.Helper()
	local := filepath.Join(t.TempDir(), "replacement")
	if err := writeFile(local, body); err != nil {
		t.Fatalf("writing the replacement object: %v", err)
	}
	if _, err := adapter().UploadFromLocal(w.ctx, medium, local, key, transport.UploadOptions{}); err != nil {
		t.Fatalf("putting %d bytes at %q on %q: %v", len(body), key, medium.ID, err)
	}
}
