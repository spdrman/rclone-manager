package conformance_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file is the negative control for the whole package.
//
// conformance_test.go claims the invariant watcher is continuous rather
// than a sampler. That claim is worth exactly nothing until the watcher
// has been watched to catch something a sampler would not, so this plants
// a breach that opens and closes INSIDE one move and judges the same run
// twice: once by the watcher, once the way a before-and-after test in this
// repository would have judged it.
//
// The sampler passes. The watcher fails. Both judgements come from one
// run of one engine over one artifact, so the comparison is not two runs
// that might have differed for some other reason.
//
// # The planted breach
//
// The conformance matrix names the falsification for the phase 2 exit
// gate's first line: "an engine that lets both placements be non-verified
// at the same instant. The harness has to observe it at that instant,
// which is why 'continuously' is in the gate line and why sampling would
// not do."
//
// That is what earlyRelease writes. When the engine records COPIED, which
// is the phase meaning "the destination reported bytes written and nothing
// has looked at them", the decorator also releases the SOURCE placement
// out of ACTIVE. For the three phase writes between COPIED and VERIFIED
// the artifact has no ACTIVE placement at content class at all: the source
// has been given up and the destination has not been earned. Then VERIFIED
// lands, the destination gets its content-class row, and the window shuts.
//
// It is a decorator rather than an edit to internal/placement because this
// control is about the WATCHER. The equivalent mutation to product code is
// planted in scripts/conformance/selftest.sh, which is where the product's
// own cells are falsified.

// earlyRelease is the planted breach: it lets go of the source copy one
// phase too early, and everything tidies itself up a few writes later.
type earlyRelease struct {
	inner  placement.MoveJournal
	read   *state.Journal
	fired  int
	target model.ArtifactID
}

func (j *earlyRelease) Get(ctx context.Context, a model.ArtifactID) (state.Record, error) {
	return j.inner.Get(ctx, a)
}

func (j *earlyRelease) ListMoves(ctx context.Context, phases ...string) ([]state.Move, error) {
	return j.inner.ListMoves(ctx, phases...)
}

func (j *earlyRelease) PlanMove(ctx context.Context, p state.MovePlan) (state.Move, error) {
	return j.inner.PlanMove(ctx, p)
}

func (j *earlyRelease) AdvanceMove(ctx context.Context, a state.MoveAdvance) (state.Move, error) {
	if a.To != state.MoveCopied {
		return j.inner.AdvanceMove(ctx, a)
	}
	rec, err := j.read.Get(ctx, j.target)
	if err != nil {
		return state.Move{}, err
	}
	src, ok := placementOn(rec, state.MediumLocal)
	if !ok || src.Status != state.PlacementActive {
		// Nothing to release. Saying so loudly matters: a control that
		// silently plants nothing is the "green mutation" this
		// repository has been bitten by, and a mutation that did not
		// happen would make the sampler and the watcher agree for the
		// wrong reason.
		return state.Move{}, fmt.Errorf(
			"the planted breach found no ACTIVE local placement on %s to release, so it planted nothing", j.target)
	}
	a.Placements = append(a.Placements, src.Update().WithStatus(state.PlacementDeletePending))
	j.fired++
	return j.inner.AdvanceMove(ctx, a)
}

// sampler is how this invariant would be checked by a test that looked
// before and after, which is the shape the exit-gate line rules out.
//
// It reads the same journal through the same CheckInvariant with the same
// sufficient classes. The ONLY difference from the watcher is when it
// looks, which is what makes the comparison below about sampling and not
// about two different notions of the invariant.
type sampler struct {
	journal    *state.Journal
	ids        []model.ArtifactID
	sufficient []placement.Class
	samples    int
	breaches   []string
}

func newSampler(j *state.Journal, ids []model.ArtifactID) *sampler {
	return &sampler{journal: j, ids: ids, sufficient: sufficientClass}
}

func (s *sampler) sample(t *testing.T, when string) {
	t.Helper()
	s.samples++
	for _, id := range s.ids {
		rec, err := s.journal.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("the sampler could not read %s %s: %v", id.Name, when, err)
		}
		if err := placement.CheckInvariant(rec, s.sufficient...); err != nil {
			s.breaches = append(s.breaches, fmt.Sprintf("%s: %v", when, err))
		}
	}
}

// TestTheWatcherCatchesABreachASamplerWouldMiss is the control that makes
// "continuously" mean something in this package.
func TestTheWatcherCatchesABreachASamplerWouldMiss(t *testing.T) {
	w := newWorld(t)

	// The artifact the monthly tier moves onto the offsite medium. It is
	// the one leg of the chain that completes against this endpoint, so
	// it is the only one that can carry a breach that CLOSES.
	summer := w.artifactNamed(t, "2026-07-15T02-00-00Z.dump")

	wa := newWatcher(t, w.journal, []model.ArtifactID{summer.id})
	// This is the one cell that WANTS a breach, so the watcher collects
	// rather than failing at the instant.
	wa.expectBreaches = true
	sm := newSampler(w.journal, []model.ArtifactID{summer.id})

	sm.sample(t, "before the cycle")
	wa.observe("before the cycle")

	plant := &earlyRelease{read: w.journal, target: summer.id}
	p := runPassThrough(t, w, scenarioNow, wa, func(inner placement.MoveJournal) placement.MoveJournal {
		plant.inner = inner
		return plant
	})

	sm.sample(t, "after the cycle")

	// --- the control's own controls ------------------------------------

	// The mutation has to have happened. A green mutation is a bug in the
	// mutation until proven otherwise, and this repository has the scars.
	if plant.fired == 0 {
		t.Fatal("the planted breach never fired, so neither judgement below is about anything")
	}
	// And the move has to have completed anyway, because a breach that
	// derails the move is a breach a sampler WOULD see in the end state.
	assertCompleted(t, p, summer.id)
	assertActiveOn(t, w, summer.id, mediumOffsite)

	// --- the comparison -------------------------------------------------

	if len(sm.breaches) != 0 {
		t.Fatalf("the sampler saw the breach, so this control is not about sampling at all: %s",
			strings.Join(sm.breaches, "; "))
	}
	if sm.samples != 2 {
		t.Fatalf("the sampler took %d samples, want the two a before-and-after test takes", sm.samples)
	}

	if wa.breachCount() == 0 {
		t.Fatalf("the WATCHER did not see the planted breach either, so this package's continuity claim "+
			"is not backed by anything. It observed: %s", wa.eventSummary())
	}
	for _, b := range wa.breaches {
		t.Logf("the watcher caught: %s", b)
	}

	// The breach has to be caught in the middle, not at an end. A watcher
	// that only noticed once the copy came back would be a sampler with
	// extra steps.
	var midMove bool
	for _, b := range wa.breaches {
		if strings.Contains(b, "phase COPIED") || strings.Contains(b, "phase VERIFYING") {
			midMove = true
		}
	}
	if !midMove {
		t.Errorf("the watcher's breaches are all at the ends of the move, so it has not been shown to see "+
			"the middle: %s", strings.Join(wa.breaches, "; "))
	}
}

// TestNeitherJudgementFiresOnACleanRun is the positive control for the
// control.
//
// The same world, the same artifact, the same two judgements, and no
// planted breach. Both have to come back clean. Without this, a watcher
// that reported a breach at every event would pass the test above and mean
// nothing at all, which is the exact shape of failure the compat gate's
// own self-test exists to rule out.
func TestNeitherJudgementFiresOnACleanRun(t *testing.T) {
	w := newWorld(t)
	summer := w.artifactNamed(t, "2026-07-15T02-00-00Z.dump")

	wa := newWatcher(t, w.journal, []model.ArtifactID{summer.id})
	// This is the one cell that WANTS a breach, so the watcher collects
	// rather than failing at the instant.
	wa.expectBreaches = true
	sm := newSampler(w.journal, []model.ArtifactID{summer.id})

	sm.sample(t, "before the cycle")
	wa.observe("before the cycle")
	p := runPass(t, w, scenarioNow, wa)
	sm.sample(t, "after the cycle")

	assertCompleted(t, p, summer.id)
	if len(sm.breaches) != 0 {
		t.Errorf("the sampler reported a breach on an unmutated run: %s", strings.Join(sm.breaches, "; "))
	}
	if n := wa.breachCount(); n != 0 {
		wa.report()
		t.Errorf("the watcher reported %d breaches on an unmutated run, so its failures mean nothing", n)
	}
	if wa.observationCount() < 8 {
		t.Errorf("the watcher only observed %d times during a complete move, which is too few to have seen "+
			"its phases: %s", wa.observationCount(), wa.eventSummary())
	}
}
