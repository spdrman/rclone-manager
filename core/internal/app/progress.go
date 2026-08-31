package app

import (
	"context"
	"sync"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

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
	c.clearArtifactLocked()
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
