package conformance_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/artifactstore"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// adapter is the real rclone adapter. One function so every caller in
// this package reaches the same code an operator runs.
func adapter() *rclone.Adapter { return rclone.New() }

// pass is one retention-and-placement cycle: what the chain decided, and
// what the engine did about it.
type pass struct {
	now      time.Time
	verdicts []retention.GFSVerdict
	plan     retention.HomePlan
	report   placement.CycleReport
}

// outcomeFor finds one artifact's outcome in a pass, if the engine
// produced one.
func (p pass) outcomeFor(id model.ArtifactID) (placement.Outcome, bool) {
	for _, o := range p.report.Outcomes {
		if o.Artifact == id {
			return o, true
		}
	}
	return placement.Outcome{}, false
}

// runPass is the composed cycle, and the order of the four calls is the
// whole point of this package.
//
//  1. retention.GFSDecide decides what is KEPT, from the journal and the
//     chain, with no medium-supplied value anywhere in scope.
//  2. retention.PlanHomeMoves applies FR-27's home rule to those verdicts,
//     through internal/app.ActiveMediumFromRecords, which is the product's
//     own reading of where an artifact currently is.
//  3. placement.Engine.RunCycle executes what came out, re-deciding for
//     itself whether each move is safe.
//  4. The watcher evaluates FR-30's standing invariant at every event
//     inside step 3.
//
// Steps 1 to 3 are all product code. What this function supplies is the
// call between them, which in the shipped daemon is #239's retention pass.
// That wiring is not in this tree, so the loop is here; every decision
// inside it is the product's.
func runPass(t *testing.T, w *world, now time.Time, wa *watcher) pass {
	t.Helper()
	return runPassThrough(t, w, now, wa, nil)
}

// runPassThrough is runPass with a decorator slipped between the watcher
// and the real journal, which is how a cell plants a breach the engine
// itself would never write.
func runPassThrough(t *testing.T, w *world, now time.Time, wa *watcher, under func(placement.MoveJournal) placement.MoveJournal) pass {
	t.Helper()

	records := w.records()
	verdicts, err := retention.GFSDecide(now, w.cfg.Retention, setID, records)
	if err != nil {
		t.Fatalf("the retention pass failed at %s: %v", now.Format(time.RFC3339), err)
	}

	homePlan, err := retention.PlanHomeMoves(w.chain(), verdicts, app.ActiveMediumFromRecords(records))
	if err != nil {
		t.Fatalf("planning home moves at %s: %v", now.Format(time.RFC3339), err)
	}

	plans := make([]placement.Plan, 0, len(homePlan.Moves))
	for _, m := range homePlan.Moves {
		plans = append(plans, placement.Plan{Artifact: m.Artifact, DestinationMedium: m.To})
	}

	engine := w.engine(wa, verdicts, now, under)
	report, err := engine.RunCycle(context.Background(), plans)
	if err != nil {
		t.Fatalf("the move cycle failed at %s: %v", now.Format(time.RFC3339), err)
	}
	return pass{now: now, verdicts: verdicts, plan: homePlan, report: report}
}

// engine builds the move engine with the watcher wrapped around every
// surface that can change the invariant.
func (w *world) engine(wa *watcher, verdicts []retention.GFSVerdict, now time.Time, under func(placement.MoveJournal) placement.MoveJournal) *placement.Engine {
	w.t.Helper()
	local, err := artifactstore.NewLocal(w.root)
	if err != nil {
		w.t.Fatalf("building the local store: %v", err)
	}
	var journal placement.MoveJournal = w.journal
	if under != nil {
		journal = under(journal)
	}
	return &placement.Engine{
		Journal: &watchedJournal{inner: journal, w: wa},
		Store:   &watchedStore{MediumStore: adapter(), w: wa},
		Local:   &watchedLocal{Local: local, w: wa},
		Mediums: scenarioResolver{w: w},
		Sets:    scenarioSets{set: w.backupSet()},
		Tiers:   newChainTierGuard(w.chain(), verdicts),
		Now:     func() time.Time { return now },

		// Generous enough that no cycle in this scenario is bounded by
		// it, which is what makes "the chain did not finish" a fact
		// about the chain rather than about a budget.
		MaxMovesPerCycle: 16,
	}
}

// --- the seams the daemon will own -------------------------------------

// scenarioResolver answers what the engine asks about a medium: how to
// reach it, and what class a move to it must achieve.
//
// The class is derived from the medium's own upload_verification through
// config.EffectiveUploadVerification, which is the mapping FR-31 defines
// and the one #239's wiring will make. There is no production
// MediumResolver in this tree yet, so this is the stand-in, and it is
// written to be the same derivation rather than a convenient one: an
// unrecognised mode is an error, not a default, because defaulting it
// would pick the verification standard a local copy is deleted against.
type scenarioResolver struct{ w *world }

func (r scenarioResolver) Resolve(id string) (transport.Medium, placement.Class, error) {
	medium, ok := r.w.mediumByID(id)
	if !ok {
		return transport.Medium{}, "", fmt.Errorf("%w: %q", app.ErrMediumNotDeclared, id)
	}
	for _, m := range r.w.cfg.StorageMediums {
		if m.ID != id {
			continue
		}
		switch m.EffectiveUploadVerification() {
		case config.UploadVerificationReadback:
			return medium, placement.Content, nil
		case config.UploadVerificationAttested:
			return medium, placement.Attested, nil
		default:
			return transport.Medium{}, "", fmt.Errorf(
				"medium %q declares upload_verification %q, which this build does not know how to hold a copy to",
				id, m.EffectiveUploadVerification())
		}
	}
	return transport.Medium{}, "", fmt.Errorf("%w: %q", app.ErrMediumNotDeclared, id)
}

type scenarioSets struct{ set config.BackupSet }

func (s scenarioSets) Set(model.BackupSetID) (config.BackupSet, error) { return s.set, nil }

// chainTierGuard answers FR-30's last question before a source delete:
// does any retention tier whose medium is the SOURCE still select this
// artifact?
//
// It is derived from the real chain and the real verdicts the same pass
// produced, which is the arithmetic #239 owns. Two rules matter and both
// point the same way:
//
//   - An artifact with no verdict cannot be shown to be unwanted, so the
//     answer is "still selected" and the source survives. A guard that
//     cannot prove the source is disposable must refuse; that is the
//     engine's own rule for a nil guard and this one holds to it.
//   - FR-19's protection term names no tier and therefore no medium, so it
//     is skipped rather than matched, exactly as retention.HomeMedium
//     skips it.
type chainTierGuard struct {
	byName   map[retention.GFSTier]config.RetentionTier
	verdicts map[model.ArtifactID]retention.GFSVerdict
}

func newChainTierGuard(chain []config.RetentionTier, verdicts []retention.GFSVerdict) *chainTierGuard {
	g := &chainTierGuard{
		byName:   map[retention.GFSTier]config.RetentionTier{},
		verdicts: map[model.ArtifactID]retention.GFSVerdict{},
	}
	for _, t := range chain {
		g.byName[retention.GFSTier(strings.ToUpper(t.Name))] = t
	}
	for _, v := range verdicts {
		g.verdicts[v.Artifact] = v
	}
	return g
}

func (g *chainTierGuard) SourceStillSelected(_ context.Context, rec state.Record, medium string) (bool, string, error) {
	v, ok := g.verdicts[rec.Artifact]
	if !ok {
		return true, fmt.Sprintf(
			"this pass produced no retention verdict for %s, so nothing here can show its copy on %q is unwanted",
			rec.Artifact, medium), nil
	}
	for _, sel := range v.Tiers {
		if sel.By == retention.GFSSelectedByProtection {
			continue
		}
		t, ok := g.byName[sel.Tier]
		if !ok {
			return true, fmt.Sprintf(
				"the verdict for %s names tier %q, which the chain does not contain, so this guard refuses rather than guessing",
				rec.Artifact, sel.Tier), nil
		}
		if t.EffectiveMedium() == medium {
			return true, fmt.Sprintf("tier %s still selects %s and its home is %q", sel.Tier, rec.Artifact, medium), nil
		}
	}
	return false, "", nil
}

// --- the composed scenario ---------------------------------------------

// TestTheThreeTierChainEndToEnd is job one of #242.
//
// It runs the exact chain the phase 2 exit gate names, over MinIO, across
// three points in time, and asserts at every step what the product itself
// decided rather than what this test would like.
//
// Read the assertions in the order they appear. All three rungs take
// delivery here: the daily one keeps its artifact on local disk, the
// monthly one receives one from local, and so does the annual one. That is
// what changed when the annual tier stopped naming an archive class, and
// it is the difference between a chain this suite can demonstrate and one
// it can only describe.
//
// The scenario now comes out clean. The third pass is the leg that used to
// stop it: the hop from the monthly medium to the annual one is medium to
// medium, which the engine refused outright until #429, and which now goes
// through a staging copy on the backup set's own disk. So an artifact
// reaches any rung from local AND walks from one medium rung to the next,
// which is what the exit-gate line asks for. See
// TestTheChainsSecondHopIsMediumToMedium for that hop on its own.
//
// The one thing this suite still cannot demonstrate is the annual rung on
// a COLD class, and that is not a gap in the product: a retention tier on
// an archive class is refused at load (#442), and this MinIO would not
// take the storage class anyway. Both facts are checks rather than prose;
// see TestAnArchiveClassTierIsRefusedAtLoad and archiveboundary_test.go.
func TestTheThreeTierChainEndToEnd(t *testing.T) {
	w := newWorld(t)
	wa := newWatcher(t, w.journal, w.ids())

	// The starting state, observed once, so the trajectory the watcher
	// covers begins where the seeding left off rather than at the first
	// write.
	wa.observe("before the first cycle")

	// --- pass one: the chain as it stands today ------------------------

	p1 := runPass(t, w, scenarioNow, wa)

	wantHomes := map[string]string{
		"2026-09-03T02-00-00Z.dump": state.MediumLocal, // daily
		"2026-07-15T02-00-00Z.dump": mediumOffsite,     // monthly
		"2024-06-15T02-00-00Z.dump": mediumAnnual,      // annual
	}
	assertHomes(t, w, p1, wantHomes, "2026-07-01T02-00-00Z.dump")

	// The monthly hop is the one that works, and it has to actually work:
	// the bytes are on the bucket, the local copy is gone, and the
	// journal says both.
	summer := w.artifactNamed(t, "2026-07-15T02-00-00Z.dump")
	assertCompleted(t, p1, summer.id)
	assertActiveOn(t, w, summer.id, mediumOffsite)
	if w.localExists(summer.id) {
		t.Errorf("%s still has a local copy after a completed move to %q", summer.id.Name, mediumOffsite)
	}
	assertBytesAreReal(t, w, summer)

	// The annual hop works too, and it is the rung this scenario could
	// not reach at all while the annual tier named an archive class. It
	// goes from LOCAL, which is the direction the engine does: the
	// artifact is older than every window but the annual one, so its home
	// is the annual medium and its only copy has been on disk since it
	// was ingested.
	ancient := w.artifactNamed(t, "2024-06-15T02-00-00Z.dump")
	assertCompleted(t, p1, ancient.id)
	assertActiveOn(t, w, ancient.id, mediumAnnual)
	if w.localExists(ancient.id) {
		t.Errorf("%s still has a local copy after a completed move to %q", ancient.id.Name, mediumAnnual)
	}
	assertBytesAreReal(t, w, ancient)

	// And the two mediums really are two places. A chain whose second and
	// third rungs resolved to the same bucket would satisfy every
	// assertion above while proving nothing about a three-tier chain.
	if w.offsite.Bucket == w.annual.Bucket {
		t.Fatalf("the monthly and annual mediums are the same bucket (%q), so this run did not exercise two destinations", w.offsite.Bucket)
	}

	// --- pass two: the clock moves, and that alone plans a move --------

	// Eight days on, the freshest artifact has fallen out of the daily
	// window and is now the monthly tier's representative for its month,
	// so its home changes from local to the offsite medium with nothing
	// else about the world having changed. This is the ageing leg, and it
	// is the one that shows the chain, not the test, deciding.
	aged := scenarioNow.AddDate(0, 0, 8)
	p2 := runPass(t, w, aged, wa)

	fresh := w.artifactNamed(t, "2026-09-03T02-00-00Z.dump")
	if !plannedMove(p2, fresh.id, state.MediumLocal, mediumOffsite) {
		t.Fatalf("eight days on, %s should have been planned local -> %s by the chain alone; the plan was %s",
			fresh.id.Name, mediumOffsite, describePlan(p2.plan))
	}
	assertCompleted(t, p2, fresh.id)
	assertActiveOn(t, w, fresh.id, mediumOffsite)
	assertBytesAreReal(t, w, fresh)

	// --- pass three: the second hop, medium to medium ------------------

	// More than a year on, the monthly window no longer reaches the
	// artifact that pass two put on the offsite medium, and the annual
	// tier claims it. Its copy is on one medium and its home is another,
	// which is the hop the chain's own shape produces and the engine used
	// to refuse. It goes through a staging copy on the backup set's own
	// disk now (#429), so this is the leg that turns "an artifact can
	// reach any rung from local" into "an artifact walks the chain".
	later := time.Date(2027, 10, 1, 9, 0, 0, 0, time.UTC)
	p3 := runPass(t, w, later, wa)

	if !plannedMove(p3, fresh.id, mediumOffsite, mediumAnnual) {
		t.Fatalf("in %s the chain should place %s on %s; the plan was %s",
			later.Format("2006"), fresh.id.Name, mediumAnnual, describePlan(p3.plan))
	}
	assertCompleted(t, p3, fresh.id)
	assertActiveOn(t, w, fresh.id, mediumAnnual)
	assertBytesAreReal(t, w, fresh)
	if w.localExists(fresh.id) {
		t.Errorf("%s has a local copy after a staged hop between two mediums; the staging copy must not land on the artifact's own path", fresh.id.Name)
	}

	// --- what the watcher saw ------------------------------------------

	wa.report()

	// A watcher that observed only the ends would have the same breach
	// count as one that observed everything, so the count of observations
	// and the presence of mid-move events are asserted rather than
	// assumed. Without this, a decorator silently detached by a refactor
	// would read as a clean run.
	if got := wa.observationCount(); got < 20 {
		t.Errorf("the invariant was evaluated %d times across three cycles, which is too few for this to be a continuous watch: %s",
			got, wa.eventSummary())
	}
	for _, mid := range []string{
		"after the copy to",
		"after the journal wrote phase VERIFIED",
		"removing the local copy",
	} {
		if !wa.sawEvent(mid) {
			t.Errorf("the watcher never observed an event matching %q, so it was not awake during the part of a move that matters: %s",
				mid, wa.eventSummary())
		}
	}
}

// --- the two findings, each as its own named check ---------------------

// TestAFailedCopyLeavesItsReasonOnTheMoveRow is the regression test for
// the one defect the composed run turned up in product code.
//
// A failed copy is deliberately not abandoned, because the ordinary reason
// a copy fails is transient and the next cycle should try again. What was
// missing is the account: the move row stayed at COPYING with an EMPTY
// error column, so the only record of what went wrong was the cycle
// report, which lives in memory and is gone by the time anybody looks. A
// move wedged for a week against a permanent refusal read exactly like one
// that started ten seconds ago.
//
// This cell needs a copy that really fails against a real endpoint, which
// is why it lives here and not in internal/placement's own suite: the
// failure it is about is one a double would have been written not to
// produce.
//
// # Why the failing destination changed
//
// It used to be the archive-class medium. The chain planned a move onto
// it, MinIO answered InvalidStorageClass to the PUT, and the reason landed
// on the move row. #437 closed that route and closed it correctly: a move
// to an archive-class destination is now refused at PLAN time, so no move
// row is written at all and there is nothing here to read. Zero move rows
// is the RIGHT answer, and it is worth more than this cell's old premise,
// because abandoning still uploads once per cycle and refusing uploads
// nothing. That claim has its own cell now
// (TestAnArchiveClassTierIsRefusedBeforeItCostsAnything), and this one
// needs a different way to fail.
//
// A bucket that does not exist is the replacement, and it is a better
// premise than the one it replaces. It is the failure an operator actually
// produces (a typo, or a bucket deleted underneath a running deployment),
// it goes through the same upload call, and it rests on nothing this
// product gates and nothing this fixture might change its mind about: no
// endpoint invents a bucket to hold a PUT. It is also classified
// Configuration rather than transient by the adapter's own bucket check
// (tests/miniointegration pins that against this same server), which makes
// it exactly the permanent, retried-for-ever failure this cell's whole
// argument is about.
func TestAFailedCopyLeavesItsReasonOnTheMoveRow(t *testing.T) {
	w := newWorldWithAnnualHome(t, mediumUnreachable)
	wa := newWatcher(t, w.journal, w.ids())
	wa.observe("before the cycle")

	p := runPass(t, w, scenarioNow, wa)
	ancient := w.artifactNamed(t, "2024-06-15T02-00-00Z.dump")

	if !plannedMove(p, ancient.id, state.MediumLocal, mediumUnreachable) {
		t.Fatalf("the chain did not plan %s onto the misconfigured medium at all, so this check inspected nothing: %s",
			ancient.id.Name, describePlan(p.plan))
	}

	// The premise, checked rather than assumed. If this endpoint ever
	// took an upload into a bucket nobody created, the move would
	// complete and every assertion below would be about the wrong move
	// while the cell still looked busy.
	if o, ok := p.outcomeFor(ancient.id); ok && o.Phase == placement.Done {
		t.Fatalf("the move into a bucket that does not exist COMPLETED, so there is no failed copy here to read about: %+v", o)
	}

	moves, err := w.journal.MovesForArtifact(w.ctx, ancient.id)
	if err != nil {
		t.Fatalf("reading the move journal: %v", err)
	}
	if len(moves) != 1 {
		t.Fatalf("expected exactly one move for %s, got %d", ancient.id.Name, len(moves))
	}
	mv := moves[0]
	if got := placement.Phase(mv.Phase); got != placement.Copying {
		t.Fatalf("the move sits at %s; this cell is about a copy that failed and stayed at %s", got, placement.Copying)
	}
	if mv.Error == "" {
		t.Fatal("the move row carries no error at all, so an operator reading the move journal " +
			"has no account of why this move has not progressed")
	}
	for _, want := range []string{"copying", mediumUnreachable} {
		if !strings.Contains(mv.Error, want) {
			t.Errorf("the recorded reason does not mention %q, so it does not say what failed: %q", want, mv.Error)
		}
	}
	t.Logf("the move row records: %s", mv.Error)

	// And the failure changed nothing about where the artifact is.
	if !w.localExists(ancient.id) {
		t.Fatal("THE LOCAL COPY WAS DELETED against a destination that never took the bytes")
	}
	assertActiveOn(t, w, ancient.id, state.MediumLocal)
	assertBytesAreReal(t, w, ancient)
	wa.report()
}

// TestAnArchiveClassTierIsRefusedAtLoad is what the annual rung of this
// chain used to be, stated as its own claim instead of as a hole in the
// middle of the composed scenario.
//
// The chain is the same chain, written to the same config.yaml, with one
// field changed: the annual tier names the medium on GLACIER. That
// configuration can never take delivery of an artifact, for the reason
// #428 sets out and this suite's own archiveboundary_test.go proves from
// the product's exported functions, and #442 is where the refusal ended
// up: at load, before a daemon starts, rather than per cycle for ever.
//
// This cell used to assert the ENGINE's plan-time refusal, which #437
// added and which is still there and still tested (internal/placement's
// archiveupload_test.go). What changed is which refusal an operator meets
// first. A refusal at load costs a message on the way in; the engine's
// costs a cycle that runs, plans, refuses and reports, every cycle, on a
// deployment that started and looked healthy.
//
// Three things have to be true, and they are three separate claims:
//
//  1. The pairing is refused, and the message says which tier, which
//     medium, which class and what to write instead. A refusal an
//     operator cannot act on is a config file they will change at random.
//  2. The SAME file with the annual tier on an ordinary medium loads.
//     Without that control, a build that refused every config would
//     satisfy the first claim.
//  3. The archive-class medium is still DECLARED in the config that
//     loads, and nothing objects. That is the half #442 deliberately
//     leaves legal: an operator with objects already on DEEP_ARCHIVE
//     declares the medium so this product can see them and restore them
//     (#241), and points no tier at it.
func TestAnArchiveClassTierIsRefusedAtLoad(t *testing.T) {
	// The world stands up on the ordinary chain, which is claim 2's
	// control and is also what gives this cell a real config.yaml with
	// real buckets and a real credential file in it.
	w := newWorld(t)

	// Claim 3, from the config that loaded: the GLACIER medium is
	// declared, and no tier delivers to it.
	declared := false
	for _, m := range w.cfg.StorageMediums {
		if m.ID == mediumDeepFreeze {
			declared = true
			if m.EffectiveStorageClass() != config.StorageClassGlacier {
				t.Fatalf("the medium this cell is about writes with %q, not %q, so it is not an archive medium at all",
					m.EffectiveStorageClass(), config.StorageClassGlacier)
			}
		}
	}
	if !declared {
		t.Fatalf("the loaded config declares no %q medium, so nothing here shows a declared archive medium validating", mediumDeepFreeze)
	}
	for i, tier := range w.chain() {
		if tier.EffectiveMedium() == mediumDeepFreeze {
			t.Fatalf("tiers[%d] (%s) delivers to the archive medium in the config that LOADED, which is the pairing this cell says is refused", i, tier.Name)
		}
	}

	// Claim 1: the same file, one field changed.
	path := w.writeConfig(mediumDeepFreeze)
	cfg, err := config.LoadAndValidate(path)
	if err == nil {
		t.Fatalf("the config loaded with the annual tier on a %s medium; that tier can never take delivery of an artifact, "+
			"and a deployment that starts on it moves nothing and says nothing (chain: %+v)", config.StorageClassGlacier, cfg.Retention.Tiers)
	}
	for _, want := range []string{
		tierAnnual,                      // which tier
		mediumDeepFreeze,                // which medium
		config.StorageClassGlacier,      // which class
		"archived the instant it lands", // why
		config.StorageClassStandard,     // what to write instead
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q, so it does not tell an operator what to change:\n%v", want, err)
		}
	}
	t.Logf("the load refused with: %v", err)

	// And nothing was spent finding out. There is no daemon, no journal
	// write and no request, because the configuration never became a
	// running deployment; the bucket is checked rather than assumed
	// because "nothing was uploaded" is the claim #437 measured and it is
	// worth keeping stated even now that it is structural.
	objects, err := adapter().ListObjects(w.ctx, w.deepFreeze, "")
	if err != nil {
		t.Fatalf("listing the archive-class bucket: %v", err)
	}
	if len(objects) != 0 {
		t.Errorf("the archive-class bucket holds %d object(s) after a config that never loaded; an archive class bills "+
			"a minimum duration for every copy, so a refusal that uploads first is not free", len(objects))
	}
}

// TestTheChainsSecondHopIsMediumToMedium is the second place the composed
// chain used to stop, and it is now the place it does not.
//
// internal/placement's own comment used to say medium to medium was
// refused because "FR-27's home rule only ever produces local-to-medium
// and medium-to-local". Over a three-tier chain with two mediums that is
// false, and this is the cheapest possible demonstration: two tiers, both
// naming a medium, and an artifact that ages out of the first into the
// second. #429 is that sentence being wrong; the fix is a staging copy on
// the backup set's own disk, and this cell is the composed proof of it
// against a real endpoint rather than against a double.
//
// What it asserts is arrival rather than absence: the bytes are on the
// annual bucket and they are the artifact's, the object on the monthly
// bucket is gone, the journal says both, and the local staging copy the
// hop went through is not on disk and never became a placement.
func TestTheChainsSecondHopIsMediumToMedium(t *testing.T) {
	w := newWorld(t)
	wa := newWatcher(t, w.journal, w.ids())
	wa.observe("before the cycle")

	// Pass one puts the freshest artifact on the offsite medium by
	// running the clock past its daily window.
	aged := scenarioNow.AddDate(0, 0, 8)
	runPass(t, w, aged, wa)
	fresh := w.artifactNamed(t, "2026-09-03T02-00-00Z.dump")
	assertActiveOn(t, w, fresh.id, mediumOffsite)
	if w.localExists(fresh.id) {
		t.Fatal("the local copy survived the first hop, so the second one is not medium-to-medium")
	}

	// Pass two ages it out of the monthly window and into the annual
	// tier, whose home is the other medium.
	later := time.Date(2027, 10, 1, 9, 0, 0, 0, time.UTC)
	p := runPass(t, w, later, wa)

	home, hasHome, err := retention.HomeMedium(w.chain(), verdictFor(t, p, fresh.id))
	if err != nil {
		t.Fatalf("asking the home rule where %s belongs: %v", fresh.id.Name, err)
	}
	if !hasHome || home != mediumAnnual {
		t.Fatalf("FR-27's home rule puts %s on (%q, hasHome=%t), so this check is not looking at a medium-to-medium hop",
			fresh.id.Name, home, hasHome)
	}
	if !plannedMove(p, fresh.id, mediumOffsite, mediumAnnual) {
		t.Fatalf("the chain did not plan %s from %s to %s, so this cell inspected nothing: %s",
			fresh.id.Name, mediumOffsite, mediumAnnual, describePlan(p.plan))
	}

	assertCompleted(t, p, fresh.id)
	assertActiveOn(t, w, fresh.id, mediumAnnual)
	assertBytesAreReal(t, w, fresh)

	// The source object really left the monthly bucket. A hop that copied
	// and did not delete would satisfy every assertion above.
	if _, err := adapter().StatObject(w.ctx, w.offsite, recordedLocationOn(t, w, fresh.id, mediumOffsite)); err == nil {
		t.Errorf("%s is still on %q after a completed hop to %q; the second copy was made and the first was not removed",
			fresh.id.Name, mediumOffsite, mediumAnnual)
	}

	// The staging copy went through local disk and is not there any more,
	// and it never earned a placements row. Both halves matter: a staging
	// file left behind is an artifact-sized file per hop on the backup
	// set's own disk, and a staging file recorded as a placement is a
	// durable copy the journal claims and the copy phase deletes.
	if w.localExists(fresh.id) {
		t.Errorf("%s has a local copy after a staged hop; the staging copy must not land on the artifact's own path", fresh.id.Name)
	}
	staged := filepath.Join(w.root, placement.StagingDirName, fresh.id.Name)
	if _, err := os.Lstat(staged); err == nil {
		t.Errorf("the staging copy is still at %q after a completed hop", staged)
	}
	rec, err := w.journal.Get(w.ctx, fresh.id)
	if err != nil {
		t.Fatalf("reading %s out of the journal: %v", fresh.id.Name, err)
	}
	if local, ok := placementOn(rec, state.MediumLocal); ok && local.Status == state.PlacementActive {
		t.Errorf("the journal records an ACTIVE local placement after a staged hop (%s); the staging copy became a placement", describe(rec))
	}

	wa.report()
}

// recordedLocationOn is where the journal says an artifact's copy on one
// named medium lives, whatever that placement's status.
//
// prune_test.go's objectKeyOf is the same idea for the ACTIVE copy, and
// this cannot be it: the placement it asks about is the SOURCE of a
// completed move, which is GONE by the time the assertion runs, and that
// is exactly what makes its location worth stating.
func recordedLocationOn(t *testing.T, w *world, id model.ArtifactID, medium string) string {
	t.Helper()
	rec, err := w.journal.Get(w.ctx, id)
	if err != nil {
		t.Fatalf("reading %s out of the journal: %v", id.Name, err)
	}
	p, ok := placementOn(rec, medium)
	if !ok {
		t.Fatalf("%s has no placement on %q at all: %s", id.Name, medium, describe(rec))
	}
	return p.Location
}

// --- assertions ---------------------------------------------------------

// assertHomes checks FR-27's home rule against every artifact in the cast,
// including the one that must have no home at all.
//
// The homeless artifact is asserted explicitly rather than by omission. A
// map comparison that only checked the artifacts it listed would pass just
// as happily if the chain had grown a fourth tier that claimed the one
// this scenario needs to be unclaimed.
func assertHomes(t *testing.T, w *world, p pass, want map[string]string, homeless ...string) {
	t.Helper()
	byName := map[string]struct {
		medium string
		has    bool
	}{}
	for _, v := range p.verdicts {
		home, has, err := retention.HomeMedium(w.chain(), v)
		if err != nil {
			t.Fatalf("the home rule refused %s: %v", v.Artifact.Name, err)
		}
		byName[v.Artifact.Name] = struct {
			medium string
			has    bool
		}{home, has}
	}
	for name, wantMedium := range want {
		got, ok := byName[name]
		if !ok {
			t.Errorf("the retention pass produced no verdict for %s at all", name)
			continue
		}
		if !got.has {
			t.Errorf("%s has no home, want %q", name, wantMedium)
			continue
		}
		if got.medium != wantMedium {
			t.Errorf("%s's home is %q, want %q", name, got.medium, wantMedium)
		}
	}
	for _, name := range homeless {
		got, ok := byName[name]
		if !ok {
			t.Errorf("the retention pass produced no verdict for %s at all", name)
			continue
		}
		if got.has {
			t.Errorf("%s has a home (%q), but this scenario needs it selected by no tier; "+
				"a chain that claims it is not the chain these cells are about", name, got.medium)
		}
	}
}

func assertCompleted(t *testing.T, p pass, id model.ArtifactID) {
	t.Helper()
	o, ok := p.outcomeFor(id)
	if !ok {
		t.Fatalf("the engine reported no outcome for %s", id.Name)
	}
	if o.Phase != placement.Done {
		t.Fatalf("%s's move ended at %s, want DONE (refusal: %q)", id.Name, o.Phase, o.Refused)
	}
}

func assertActiveOn(t *testing.T, w *world, id model.ArtifactID, medium string) {
	t.Helper()
	rec, err := w.journal.Get(w.ctx, id)
	if err != nil {
		t.Fatalf("reading %s: %v", id.Name, err)
	}
	active := activeMediumOf(rec)
	if len(active) != 1 || active[0] != medium {
		t.Errorf("%s's ACTIVE copy should be on %q alone; the journal says %s", id.Name, medium, describe(rec))
		return
	}
	p, _ := placementOn(rec, medium)
	if p.VerificationClass != state.VerificationContent {
		t.Errorf("%s's copy on %q records class %q, want %q", id.Name, medium, p.VerificationClass, state.VerificationContent)
	}
}

func plannedMove(p pass, id model.ArtifactID, from, to string) bool {
	for _, m := range p.plan.Moves {
		if m.Artifact == id && m.From == from && m.To == to {
			return true
		}
	}
	return false
}

func describePlan(p retention.HomePlan) string {
	if len(p.Moves) == 0 && len(p.Unconfirmed) == 0 {
		return "empty"
	}
	parts := make([]string, 0, len(p.Moves)+len(p.Unconfirmed))
	for _, m := range p.Moves {
		parts = append(parts, fmt.Sprintf("%s: %s -> %s", m.Artifact.Name, m.From, m.To))
	}
	for _, u := range p.Unconfirmed {
		parts = append(parts, fmt.Sprintf("%s: unconfirmed", u.Name))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func verdictFor(t *testing.T, p pass, id model.ArtifactID) retention.GFSVerdict {
	t.Helper()
	for _, v := range p.verdicts {
		if v.Artifact == id {
			return v
		}
	}
	t.Fatalf("no verdict for %s in this pass", id.Name)
	return retention.GFSVerdict{}
}

func (w *world) artifactNamed(t *testing.T, name string) seeded {
	t.Helper()
	for _, a := range w.artifacts {
		if a.id.Name == name {
			return a
		}
	}
	t.Fatalf("this scenario seeds no artifact called %q", name)
	return seeded{}
}

// TestAMoveDoesNotChangeARetentionVerdict is the phase 2 exit gate's
// bucketing-invariance line, composed: "moving an artifact does not change
// its retention bucketing: verdicts before and after are bit-identical".
//
// core/tests/compat already holds the mechanism half of this, and holds it
// well: a backfill that re-derives an artifact's discovery timestamp turns
// cell 11 red. What it cannot hold is the ACROSS A MOVE half, because
// until #238 there was no move to run. This is that half, and it is the
// cheapest possible shape of it: the same verdicts, at the same instant,
// with a real move over a real S3 endpoint in between.
//
// The comparison is bit-identical rather than "the same tiers", because
// the interesting failures are the small ones. A move that shifted an
// artifact's bucket by a month would still keep it, still under a tier
// name that looks right, and would quietly change WHICH artifact each
// bucket's representative is, which is a deletion a year later.
func TestAMoveDoesNotChangeARetentionVerdict(t *testing.T) {
	w := newWorld(t)
	wa := newWatcher(t, w.journal, w.ids())
	wa.observe("before the cycle")

	before, err := retention.GFSDecide(scenarioNow, w.cfg.Retention, setID, w.records())
	if err != nil {
		t.Fatalf("the retention pass before the move failed: %v", err)
	}
	whereBefore := placementsByArtifact(w.records())

	runPass(t, w, scenarioNow, wa)

	// The control. A comparison of two identical worlds is not a check of
	// anything, and this cell is worthless unless something actually moved
	// between the two passes.
	whereAfter := placementsByArtifact(w.records())
	var moved int
	for id, was := range whereBefore {
		if whereAfter[id] != was {
			moved++
		}
	}
	if moved == 0 {
		t.Fatal("no artifact changed medium during the cycle, so comparing the verdicts before and after " +
			"compares one world with itself")
	}
	t.Logf("%d artifact(s) changed medium between the two passes", moved)

	// The same clock, deliberately. Re-deciding at a later instant would
	// be measuring the calendar, and the calendar is allowed to change a
	// verdict; the move is not.
	after, err := retention.GFSDecide(scenarioNow, w.cfg.Retention, setID, w.records())
	if err != nil {
		t.Fatalf("the retention pass after the move failed: %v", err)
	}

	if len(before) != len(after) {
		t.Fatalf("the pass produced %d verdicts before the move and %d after", len(before), len(after))
	}
	for i := range before {
		if reflect.DeepEqual(before[i], after[i]) {
			continue
		}
		t.Errorf("moving %s changed its retention verdict.\n  before: %s\n   after: %s",
			before[i].Artifact, renderVerdict(before[i]), renderVerdict(after[i]))
	}
	wa.report()
}

// placementsByArtifact is where each artifact's ACTIVE copy is, as one
// string per artifact, for the control above.
func placementsByArtifact(records []state.Record) map[model.ArtifactID]string {
	out := make(map[model.ArtifactID]string, len(records))
	for _, rec := range records {
		out[rec.Artifact] = strings.Join(activeMediumOf(rec), "+")
	}
	return out
}

// renderVerdict spells a verdict out in full, so a failure above says what
// changed rather than that something did.
func renderVerdict(v retention.GFSVerdict) string {
	return fmt.Sprintf("%+v", v)
}

// TestTheSeededDatesAreTheDatesTheChainWillSee is the fixture's own
// control, and it is here because a scenario whose dates are not what it
// thinks they are is a scenario testing a different chain and passing.
//
// Every home this package asserts is arithmetic over these four dates
// against a seven-day, twelve-month, five-year chain. If the seeding
// silently recorded a different discovery instant, say by rounding to the
// day in the local zone or by taking the moment the row was written, the
// tiers would still select SOMETHING, the homes would still be one of
// three mediums, and roughly half the assertions would still pass. That is
// the shape of a check that has stopped covering what it says it does, and
// this repository has found several.
//
// It also asserts the derived boundaries, not only the raw dates, because
// the raw dates being right is only interesting relative to the windows the
// chain resolves at scenarioNow.
func TestTheSeededDatesAreTheDatesTheChainWillSee(t *testing.T) {
	w := newWorld(t)

	for _, a := range w.artifacts {
		rec, err := w.journal.Get(w.ctx, a.id)
		if err != nil {
			t.Fatalf("reading %s: %v", a.id.Name, err)
		}
		if !rec.DiscoveredAt.Equal(a.discoveredAt) {
			t.Errorf("%s was seeded for %s and the journal records %s; every home this package "+
				"asserts is arithmetic over that date",
				a.id.Name, a.discoveredAt.Format(time.RFC3339), rec.DiscoveredAt.Format(time.RFC3339))
		}
		if rec.State != "COMPLETE" {
			t.Errorf("%s is %s, and only a managed-complete artifact gets a retention verdict at all",
				a.id.Name, rec.State)
		}
	}

	// And the four dates have to sit where the story says relative to the
	// chain's own windows at scenarioNow: one inside the daily window, two
	// inside the monthly one, one outside both.
	daily := scenarioNow.AddDate(0, 0, -6)    // day granularity, keep 7, counting today
	monthly := scenarioNow.AddDate(0, -11, 0) // month granularity, keep 12, counting this month
	for _, tc := range []struct {
		name          string
		insideDaily   bool
		insideMonthly bool
	}{
		{"2026-09-03T02-00-00Z.dump", true, true},
		{"2026-07-15T02-00-00Z.dump", false, true},
		{"2026-07-01T02-00-00Z.dump", false, true},
		{"2024-06-15T02-00-00Z.dump", false, false},
	} {
		a := w.artifactNamed(t, tc.name)
		if got := !a.discoveredAt.Before(daily); got != tc.insideDaily {
			t.Errorf("%s inside the daily window: got %t, want %t (window opens %s)",
				tc.name, got, tc.insideDaily, daily.Format("2006-01-02"))
		}
		if got := !a.discoveredAt.Before(monthly.AddDate(0, 0, -monthly.Day()+1)); got != tc.insideMonthly {
			t.Errorf("%s inside the monthly window: got %t, want %t (window opens %s)",
				tc.name, got, tc.insideMonthly, monthly.Format("2006-01"))
		}
	}
}
