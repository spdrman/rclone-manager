package app

import (
	"context"
	"sync"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// The live feed a caller subscribes to while a cycle is running.
//
// cycleoutcome.go is the counterpart and the two are easy to confuse: that
// file counts what a FINISHED cycle achieved, for a command that has to
// produce an exit status. This one reports what is happening right now, for a
// screen.
//
// Two decisions shape everything here. There is no percentage of the cycle,
// because a cycle discovers what it will find set by set as it goes, so the
// denominator for "how much is left" does not exist until the last set has
// been discovered; the counters that ARE known are the ones on Progress.
// And the byte fields are pointers, so that "not being measured" and
// "measured, nothing yet" stay different facts. Collapsing them would make an
// unmeasured stage indistinguishable from a stalled transfer, which is the
// one reading a person watching a progress bar most needs to be able to make.
//
// The observer rides on the context rather than on the Service for the reason
// holds.go gives for the hold registry: a Service is shared and is rebuilt on
// every configuration reload, a cycle is neither, and a field on a shared
// struct is something a second cycle can overwrite. The stage names are a
// published contract, not internal vocabulary: apps/common/webhost serves them
// verbatim and api/v1/openapi.json declares the same set, held together by a
// test in that package.

// The stages a run cycle passes through, in order. They are this package's
// names for the steps pipeline.go actually performs, not a second
// vocabulary invented for a display: "discovering" is FR-17 reconcile plus
// FR-8 discovery, and the four after it are exactly processArtifact's four
// steps.
//
// apps/common/webhost serves these strings verbatim and
// api/v1/openapi.json declares the same set as OperationProgress.stage's
// enum; TestOperationProgress_StagesAreExactlyTheContractsEnum in that
// package holds the two together, so a stage renamed here without the
// contract fails there rather than reaching a client as an unknown value.
const (
	StageDiscovering    = "discovering"
	StageTransferring   = "transferring"
	StageVerifying      = "verifying"
	StageCommitting     = "committing"
	StageCleaningRemote = "cleaning-remote"
)

// Stages lists every stage a run cycle reports, in the order they happen.
var Stages = []string{
	StageDiscovering,
	StageTransferring,
	StageVerifying,
	StageCommitting,
	StageCleaningRemote,
}

// Progress is one live reading of the run cycle in flight.
//
// # What is here, and what deliberately is not
//
// There is no percentage of the cycle, because none can be computed
// honestly. A run cycle is a pass over every enabled backup set, and what
// it will find is discovered set by set as it goes, so at any moment
// before the last set has been discovered the denominator for "how much of
// this cycle is left" does not exist yet. The counters below are what IS
// known: which set out of how many, how many artifacts this cycle has
// finished, and how far the artifact currently being copied has got.
//
// The byte fields are pointers for the same reason. A nil BytesTransferred
// means "not being measured right now" (this is not the transferring
// stage, or the copy has not reported yet); a non-nil zero means "measured,
// and nothing has arrived yet". Those are different facts, and collapsing
// them onto one int64 would make an unmeasured stage indistinguishable
// from a stalled transfer.
type Progress struct {
	// Stage is one of the constants above.
	Stage string

	// BackupSetID is the set being processed, empty before the first one
	// starts.
	BackupSetID string

	// BackupSetsDone is how many enabled backup sets this cycle has
	// finished. BackupSetsTotal is how many it will visit, which IS known
	// up front: it is a count of the enabled sets in the configuration
	// snapshot the cycle started with.
	BackupSetsDone  int
	BackupSetsTotal int

	// Artifact is the artifact currently being worked on, empty during
	// discovery.
	Artifact string

	// ArtifactsDone is how many artifacts this cycle has finished driving
	// forward, across every set so far. There is deliberately no total
	// beside it: see this type's own doc.
	ArtifactsDone int

	// SetArtifactsPlanned and SetArtifactsCompleted are the same question
	// asked of ONE backup set, where it does have an answer (issue #573).
	//
	// The type doc's argument against a cycle-wide total is that a cycle
	// discovers what it will find set by set as it goes. That is true of
	// the cycle and stops being true of a set: by the time a set's pass
	// starts walking rows, it has reconciled and discovered, so the rows
	// it is about to drive forward are countable. SetArtifactsPlanned is
	// that count, and SetArtifactsCompleted is how many of them the pass
	// has finished, which is what lets a per-set view draw a bar with an
	// honest denominator instead of a spinner.
	//
	// Planned is a pointer for the same reason the byte counters are.
	// Before the walk begins there is no total, and a zero would read as
	// "nothing to do" on a set that has plenty to do and has simply not
	// finished discovering it yet. Both reset when the cycle enters a new
	// set, because they describe the set named by BackupSetID.
	//
	// The numerator counts only rows the pass is actually driving
	// forward, which is the same set of rows the denominator counted. A
	// set holding a thousand finished backups walks a thousand rows every
	// cycle and works on none of them; counting those would put a bar at
	// 99% permanently.
	SetArtifactsPlanned   *int
	SetArtifactsCompleted int

	// BytesTransferred, BytesTotal and BytesPerSecond describe the ONE
	// artifact named by Artifact, never the cycle. They are set only
	// while a copy is in flight and reporting.
	BytesTransferred *int64
	BytesTotal       *int64
	BytesPerSecond   *int64
}

// ProgressObserver receives a Progress reading every time the cycle's
// picture of itself changes.
//
// It is called from whatever goroutine reached the change, including the
// transport's own sampling goroutine, so an implementation must be
// concurrency-safe and must not block. An observer may never influence the
// cycle: nothing in this package reads a return value, and there is none.
type ProgressObserver interface {
	ObserveProgress(Progress)
}

type progressObserverKey struct{}

// WithProgressObserver returns a context that asks RunCycle to report live
// progress to obs.
//
// On the context rather than on Service because a Service is shared by
// every caller and a run cycle is not: core/service submits operations one
// at a time today, but that is a policy in that package (its single-flight
// lock), not a structural guarantee this one may rely on. Attaching the
// observer to the call's own context scopes it to exactly the cycle that
// asked for it, with no field on a shared struct for a second cycle to
// overwrite.
//
// A nil obs is the same as no observer.
func WithProgressObserver(ctx context.Context, obs ProgressObserver) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, progressObserverKey{}, obs)
}

// ProgressObserverFrom returns the observer WithProgressObserver put on
// ctx, or nil when there is none. It is exported for the same reason
// transport.ProgressReporterFrom is: the package that installs an observer
// has to be able to prove it actually installed one, and a caller that
// cannot read the value back can only assert that it called the setter.
func ProgressObserverFrom(ctx context.Context) ProgressObserver {
	obs, _ := ctx.Value(progressObserverKey{}).(ProgressObserver)
	return obs
}

type cycleProgressKey struct{}

// cycleProgress holds the running Progress for one RunCycle call and
// publishes it to the observer on every change.
//
// Every method is nil-safe, because a cycle with no observer carries a nil
// *cycleProgress and the call sites in cycle.go and pipeline.go should not
// each have to remember that.
type cycleProgress struct {
	obs ProgressObserver

	mu  sync.Mutex
	cur Progress
}

// beginCycle attaches a tracker to ctx when, and only when, ctx carries an
// observer. It returns the context the rest of the cycle must use.
func beginCycle(ctx context.Context, backupSetsTotal int) context.Context {
	obs := ProgressObserverFrom(ctx)
	if obs == nil {
		return ctx
	}
	c := &cycleProgress{obs: obs}
	c.cur.BackupSetsTotal = backupSetsTotal
	c.cur.Stage = StageDiscovering
	c.publishLocked()
	return context.WithValue(ctx, cycleProgressKey{}, c)
}

// beginOneSetCycle is beginCycle for a pass whose one backup set is known
// before anything starts, which is exactly what Fetch is.
//
// It differs in the one way that matters: the FIRST reading already names
// the set. beginCycle cannot do that, and should not try to: a RunCycle
// does not know which set it will enter until it enters one, so its
// opening reading carries no id and processBackupSet fills it in on the
// next publish.
//
// For a per-set run that gap is not harmless. core/service's live
// activity feed DROPS a reading whose BackupSetID is empty, so an opening
// reading without the id reaches no terminal at all, and "this run
// started" is the line a per-set terminal most needs to show (issue
// #597). The denominator is 1 and is honest, for the reason Progress's
// own doc gives about why a cycle has none.
func beginOneSetCycle(ctx context.Context, setID string) context.Context {
	obs := ProgressObserverFrom(ctx)
	if obs == nil {
		return ctx
	}
	c := &cycleProgress{obs: obs}
	c.cur.BackupSetsTotal = 1
	c.cur.BackupSetID = setID
	c.cur.Stage = StageDiscovering
	// No lock taken, exactly as beginCycle does not: nothing else holds a
	// pointer to c yet, so there is nobody to race with.
	c.publishLocked()
	return context.WithValue(ctx, cycleProgressKey{}, c)
}

func progressFrom(ctx context.Context) *cycleProgress {
	c, _ := ctx.Value(cycleProgressKey{}).(*cycleProgress)
	return c
}

// publishLocked hands the current reading to the observer. The caller
// holds c.mu, and the copy taken here is what makes handing it out safe:
// the observer receives a value, never a window onto this struct.
func (c *cycleProgress) publishLocked() {
	c.obs.ObserveProgress(c.cur)
}

// enterSet records that a backup set's own pass has started. Stage resets
// to discovering because that is genuinely what happens next
// (processBackupSet reconciles and discovers before it touches an
// artifact), and the per-artifact fields clear because they described the
// previous set's artifact.
func (c *cycleProgress) enterSet(setID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur.BackupSetID = setID
	c.cur.Stage = StageDiscovering
	// The per-set counters describe the set named on the reading, so
	// entering a new one clears them rather than carrying the previous
	// set's numbers into a pass that has not started.
	c.cur.SetArtifactsPlanned = nil
	c.cur.SetArtifactsCompleted = 0
	c.clearArtifactLocked()
	c.publishLocked()
}

// planSetArtifacts records how many of this set's journal rows the pass is
// about to drive forward. It is called once per set, after discovery, by
// the walk itself: nothing earlier can know the number, and nothing later
// could put a denominator under the readings the walk is already
// publishing.
func (c *cycleProgress) planSetArtifacts(n int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	planned := n
	c.cur.SetArtifactsPlanned = &planned
	c.publishLocked()
}

// finishPlannedArtifact records that one of the rows planSetArtifacts
// counted has finished. It is deliberately separate from finishArtifact:
// that one counts every row the cycle walked, and this one counts only the
// rows the set's own denominator was built from, so the two halves of a
// per-set bar are always counting the same population.
func (c *cycleProgress) finishPlannedArtifact() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur.SetArtifactsCompleted++
	c.publishLocked()
}

// finishSet records that a backup set's pass has ended, however it ended:
// a set whose reconcile failed is still a set this cycle is done with, and
// reporting it as still in flight would leave the count stuck.
func (c *cycleProgress) finishSet() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur.BackupSetsDone++
	c.clearArtifactLocked()
	c.publishLocked()
}

// enterStage records that the named artifact has reached stage.
func (c *cycleProgress) enterStage(stage, artifact string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur.Stage = stage
	c.cur.Artifact = artifact
	// The byte counters belong to a copy in flight. Leaving the last
	// transfer's numbers on a verifying or committing stage would report
	// a transfer that has already finished as though it were still
	// running.
	c.cur.BytesTransferred = nil
	c.cur.BytesTotal = nil
	c.cur.BytesPerSecond = nil
	c.publishLocked()
}

// finishArtifact records that one artifact's pass through the pipeline has
// ended.
func (c *cycleProgress) finishArtifact() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur.ArtifactsDone++
	c.clearArtifactLocked()
	c.publishLocked()
}

func (c *cycleProgress) clearArtifactLocked() {
	c.cur.Artifact = ""
	c.cur.BytesTransferred = nil
	c.cur.BytesTotal = nil
	c.cur.BytesPerSecond = nil
}

// observeBytes folds one transport sample into the current reading.
func (c *cycleProgress) observeBytes(p transport.ByteProgress) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	done := p.BytesTransferred
	c.cur.BytesTransferred = &done
	// A zero total is the transport saying "the backend could not tell me
	// how big this is", so it is dropped rather than reported as a total
	// of zero, which would read as an empty artifact and make any
	// fraction computed from it nonsense.
	if p.BytesTotal > 0 {
		total := p.BytesTotal
		c.cur.BytesTotal = &total
	} else {
		c.cur.BytesTotal = nil
	}
	// Likewise a zero rate is "too early to measure one", not "stalled".
	if p.BytesPerSecond > 0 {
		rate := p.BytesPerSecond
		c.cur.BytesPerSecond = &rate
	} else {
		c.cur.BytesPerSecond = nil
	}
	c.publishLocked()
}

// reportingCtx returns a context that asks the transport to report copy
// progress into this tracker. It is the one place the transport's
// reporting vocabulary meets this package's.
func (c *cycleProgress) reportingCtx(ctx context.Context) context.Context {
	if c == nil {
		return ctx
	}
	return transport.WithProgressReporter(ctx, transport.ProgressReporterFunc(c.observeBytes))
}
