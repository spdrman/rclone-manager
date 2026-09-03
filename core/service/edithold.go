// This file is issue #350's second half: what happens to a backup set's
// own processing while an operator is editing it.
//
// # The problem
//
// A set being edited while a cycle is running against it is two writers
// on one definition. The cycle is mid-transfer against the old remote
// path while the operator is changing it, and whichever finishes last
// wins silently. So entering edit mode takes a HOLD on that one backup
// set, which stops the pass currently running against it and stops the
// scheduler starting another until edit mode is left. Stopping the
// in-flight run while leaving the poll interval free would be the same
// race with extra steps.
//
// # A lease, not a flag
//
// A hold expires. That is the single most important property here,
// because the failure this design must not have is a set left
// permanently paused because somebody closed a tab: a backup silently
// not happening is this product's worst outcome, worse than the race the
// hold exists to prevent. So a hold is a lease with a short lifetime that
// the editing client renews while its form is open, and every route out
// of edit mode releases it explicitly on top of that. The explicit
// release is the fast path; the lease is what covers the routes a client
// cannot report (a closed laptop, a crashed browser, a lost network).
//
// It is deliberately NOT durable. A hold that survived a restart would
// mean a process coming up after a crash refuses to back up a set nobody
// is editing any more, and the only way out would be a second restart.
// The in-memory registry here is on exactly the same footing as
// liveProgress (progress.go) and the retention plan store (retention.go)
// and for the same reason.
//
// # What an authenticated caller can do with this, said out loud
//
// Any authenticated caller can hold any backup set, and can keep renewing
// it, which pauses that set's backups for as long as they keep going.
// That is worth stating rather than leaving to be discovered, and it is
// not an escalation: the same caller can already turn the set off
// outright through POST /backup-sets/{source}/{set}/enabled, which is
// both easier and more durable. The lease is what bounds the accidental
// version of it, which is the one that actually happens.
//
// # No token, and why that is the right trade here
//
// Any authenticated caller can renew or release any hold. A token would
// stop operator B releasing operator A's hold, and would cost the thing
// that matters more: a release path that can fail. With a lease and a
// renewing client, B releasing A's hold costs A one heartbeat, after
// which A's own renewal takes it again; the concurrent-edit case that
// actually corrupts something is caught by the staleness check the edit
// form already runs (ui/shared's isSetEditStale), which compares against
// the value the form opened over rather than against who holds a lease.
package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
)

// editHoldLease is how long one hold lasts without being renewed. Short,
// because the cost of a stale hold is backups not running; long enough
// that an ordinary client heartbeat (a third of this) covers a slow
// network without the hold lapsing under an operator mid-edit.
const editHoldLease = 90 * time.Second

// RunningWork names what a cycle is doing for one backup set at the
// moment it was asked. It is what makes the warning an operator sees
// before entering edit mode a real sentence rather than a bare "are you
// sure": discarding a partial transfer of a named artifact is a very
// different cost from cancelling a scheduler tick that has not started
// work, and only the first one is worth interrupting an operator over.
type RunningWork struct {
	// Artifact is the artifact being worked on, or "" during discovery,
	// when the cycle has reached this set but has not picked one yet.
	Artifact string
	// Stage is one of OperationStages (progress.go).
	Stage string
}

// EditHold is what BeginBackupSetEdit and RenewBackupSetEdit return.
type EditHold struct {
	BackupSetID string
	// ExpiresAt is when this hold lapses if nothing renews it. A client
	// that means to keep editing renews well before it.
	ExpiresAt time.Time
	// Stopped describes the work this hold interrupted, or nil when
	// nothing was running for this set. Reported so a client can say what
	// it actually stopped, rather than claiming it stopped something
	// whenever an operator presses Edit.
	Stopped *RunningWork
}

// BackupSetEditState is the read behind the warning: what is happening to
// this set right now, and whether a hold is already in place.
type BackupSetEditState struct {
	BackupSetID string
	// Held reports whether a hold is currently in force.
	Held bool
	// ExpiresAt is meaningful only when Held.
	ExpiresAt time.Time
	// Running is what a cycle is currently doing for this set, or nil.
	// Nil is what makes "entering edit mode is silent when nothing is
	// running" implementable: no prompt for a risk that does not exist.
	Running *RunningWork
}

// editHolds is the in-memory registry of every backup set currently held
// for editing in this process. It implements internal/app's
// BackupSetHolds, which is how a cycle in flight consults it.
//
// Expiry is lazy, checked on every read rather than swept by a
// goroutine: there is nothing to clean up when a hold lapses (no file, no
// journal row), the only observable effect is that Held stops answering
// true, and a sweeper would be a goroutine whose only job is to make that
// happen a few milliseconds earlier.
type editHolds struct {
	mu sync.Mutex
	// held maps a backup set id to when its hold expires.
	held map[string]time.Time
	// removed names every backup set whose configuration this process has
	// REMOVED (issue #391), and it is the one hold here that does not
	// expire.
	//
	// The lease above exists because an edit hold that outlived its
	// client would leave a set silently not backing up, and nothing worse
	// than that can happen here. A removal is the other way round: the
	// set is gone from the configuration, so a hold on its id stops
	// nothing an operator still wants, and letting it lapse is what would
	// hurt. A cycle can run for hours holding the config snapshot it
	// started with, so a removal whose hold expired after ninety seconds
	// would be a cycle that reaches the removed set forty minutes later,
	// sees Held answer false again, and processes it from the old
	// snapshot: discovering, transferring and, for a set that is not
	// read-only, deleting from the operator's source machine, all after
	// they removed it and watched the dialog close.
	//
	// It is cleared by exactly one thing: a reload of a configuration
	// that names the id again (adoptConfig, which every write path ends
	// in), so a set brought back gets a clean start whether it came back
	// through CreateBackupSet or through a hand edit of config.yaml that
	// the next write picked up. A process restart clears it too, and
	// correctly: by then the configuration on disk no longer names the
	// set, so nothing will reach it anyway.
	removed map[string]bool
	// changed is closed and replaced whenever a hold is PLACED, which is
	// how a cycle already inside that set learns to stop. Releases
	// deliberately do not broadcast; see BackupSetHolds.Changed's own doc.
	changed chan struct{}
	// now is the clock, a seam so a test can prove a lease actually
	// lapses without sleeping for a minute and a half.
	now func() time.Time
}

func newEditHolds() *editHolds {
	return &editHolds{
		held:    map[string]time.Time{},
		removed: map[string]bool{},
		changed: make(chan struct{}),
		now:     func() time.Time { return now() },
	}
}

// Held is internal/app.BackupSetHolds.Held.
func (h *editHolds) Held(setID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.heldLocked(setID)
}

// heldLocked is what a cycle asks: is this set held for ANY reason. A
// removal outranks a lease and is checked first, because it is the answer
// that must not depend on a clock.
func (h *editHolds) heldLocked(setID string) bool {
	if h.removed[setID] {
		return true
	}
	return h.leaseHeldLocked(setID)
}

// leaseHeldLocked is the edit hold on its own, expiry and all. It is
// separate from heldLocked so BackupSetEditState can report on edit mode
// without a removed set's permanent hold showing up there as an edit
// somebody is in the middle of.
func (h *editHolds) leaseHeldLocked(setID string) bool {
	expiry, ok := h.held[setID]
	if !ok {
		return false
	}
	if !h.now().Before(expiry) {
		// Lapsed. Dropped here rather than left to accumulate, so the map
		// holds exactly the sets someone is editing right now.
		delete(h.held, setID)
		return false
	}
	return true
}

// Changed is internal/app.BackupSetHolds.Changed.
func (h *editHolds) Changed() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed
}

// place takes or extends setID's hold and returns its new expiry. It
// broadcasts on every call, including a renewal: a renewal is cheap to
// broadcast and a watcher that wakes for one simply re-checks and goes
// back to waiting, whereas working out which calls "really" changed
// something would be a correctness argument for no benefit.
func (h *editHolds) place(setID string) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	expiry := h.now().Add(editHoldLease)
	h.held[setID] = expiry
	close(h.changed)
	h.changed = make(chan struct{})
	return expiry
}

// release drops setID's hold. Releasing one that is not held is not an
// error: every route out of edit mode calls this, several of them can
// fire for the same edit (a Save-and-exit followed by an unmount), and a
// second release must not be something a client has to avoid.
func (h *editHolds) release(setID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.held, setID)
}

// holdRemoved holds setID permanently, because its configuration is being
// removed. It broadcasts exactly as place does, which is what stops a
// cycle already inside that set: the watcher wakes, re-reads Held, and
// cancels that set's own context.
//
// Called BEFORE the configuration is rewritten and the service swapped,
// never after, and under configMu. The write swaps this service's
// *app.Service, and a cycle already running kept the pointer and the
// config snapshot it started with, so nothing about the write reaches
// it. The hold is the only thing that does. Under the lock, because two
// removals of the same set can overlap, and a hold taken outside it was
// one the losing call could not tell from its own and gave back; see
// RemoveBackupSet's own doc.
func (h *editHolds) holdRemoved(setID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removed[setID] = true
	close(h.changed)
	h.changed = make(chan struct{})
}

// forgetRemoved drops setID's removal hold. Called by RemoveBackupSet
// when it fails after having taken the hold, under the same lock it took
// it under, so the hold it drops is its own. Forgetting one that was
// never taken is not an error, for the same reason release beside it
// says so.
func (h *editHolds) forgetRemoved(setID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.removed, setID)
}

// forgetRemovedNamedIn drops the removal hold of every backup set cfg
// names. adoptConfig calls it on every hot reload, which is what keeps
// "a set the configuration names is never removal-held" true by whatever
// route the set came back: CreateBackupSet, or a hand-restored config.yaml
// that an unrelated write re-read. A set cfg does not name keeps its
// hold, which is the removal's own case.
func (h *editHolds) forgetRemovedNamedIn(cfg *config.Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, src := range cfg.Sources {
		for _, bs := range src.BackupSets {
			delete(h.removed, src.Name+"/"+bs.Name)
		}
	}
}

// state reports whether setID is held for EDITING and until when.
func (h *editHolds) state(setID string) (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.leaseHeldLocked(setID) {
		return time.Time{}, false
	}
	return h.held[setID], true
}

var _ app.BackupSetHolds = (*editHolds)(nil)

// cycleWatch is the ONE live reading of whatever run cycle is executing
// in this process, whichever way it started.
//
// liveProgress (progress.go) beside it is keyed by operation id, which is
// the right shape for polling a submitted operation and the wrong shape
// for this question: a SCHEDULED tick has no operation row and therefore
// no id, so a set being transferred by the scheduler was invisible to
// every reader. That is exactly the case an operator meets, since the
// scheduler is what runs unattended, and a warning that could not name
// what it was about to stop would be the bare "are you sure" the issue
// rules out.
type cycleWatch struct {
	mu      sync.RWMutex
	running bool
	cur     app.Progress
}

func newCycleWatch() *cycleWatch { return &cycleWatch{} }

// begin marks a cycle as executing. end is deferred by both callers, so a
// cycle that panicked stops answering exactly like one that returned.
func (c *cycleWatch) begin() {
	c.mu.Lock()
	c.running = true
	c.cur = app.Progress{}
	c.mu.Unlock()
}

func (c *cycleWatch) end() {
	c.mu.Lock()
	c.running = false
	c.cur = app.Progress{}
	c.mu.Unlock()
}

// ObserveProgress is internal/app.ProgressObserver.
func (c *cycleWatch) ObserveProgress(p app.Progress) {
	c.mu.Lock()
	c.cur = p
	c.mu.Unlock()
}

// workFor reports what the running cycle is doing for setID, or nil when
// no cycle is running or the running one is somewhere else. Nil is the
// answer that means "no prompt", so it has to be nil for the ordinary
// case rather than a zero-valued struct a caller might render.
func (c *cycleWatch) workFor(setID string) *RunningWork {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.running || c.cur.BackupSetID != setID {
		return nil
	}
	return &RunningWork{Artifact: c.cur.Artifact, Stage: c.cur.Stage}
}

// progressFanout hands one cycle's readings to several observers, so an
// API-submitted run can feed both its own operation-scoped reading
// (liveProgress, which a client polls) and the process-wide cycleWatch
// above, which answers "what would entering edit mode stop".
type progressFanout []app.ProgressObserver

func (f progressFanout) ObserveProgress(p app.Progress) {
	for _, o := range f {
		o.ObserveProgress(p)
	}
}

// BackupSetEditState reports what entering edit mode for this set would
// interrupt, and whether a hold is already in force. Read-only: it takes
// no hold, stops nothing, and is safe to poll.
//
// This is the call behind "the warning appears only when something is
// actually running": Running is nil whenever no cycle is currently inside
// this set, and a client that gets nil opens edit mode with no prompt.
func (b *BackupService) BackupSetEditState(_ context.Context, id string) (BackupSetEditState, error) {
	if _, _, ok := splitBackupSetID(id); !ok {
		return BackupSetEditState{}, wrapNotFound(id)
	}
	if err := b.requireBackupSet(id); err != nil {
		return BackupSetEditState{}, err
	}
	expiry, held := b.holds.state(id)
	return BackupSetEditState{
		BackupSetID: id,
		Held:        held,
		ExpiresAt:   expiry,
		Running:     b.cycleWatch.workFor(id),
	}, nil
}

// BeginBackupSetEdit takes (or renews) the hold on one backup set: the
// cycle currently processing it, if any, is stopped at its next safe
// boundary, and no new pass will be started for it until the hold is
// released or lapses.
//
// It returns what it interrupted, so a client can report the truth
// afterwards rather than claiming to have stopped something every time.
// The reading is taken BEFORE the hold is placed, which is the only order
// that can answer the question: once the hold lands the cycle starts
// unwinding, and a reading taken after it would race the very
// cancellation this call caused.
func (b *BackupService) BeginBackupSetEdit(_ context.Context, id string) (EditHold, error) {
	if _, _, ok := splitBackupSetID(id); !ok {
		return EditHold{}, wrapNotFound(id)
	}
	if err := b.requireBackupSet(id); err != nil {
		return EditHold{}, err
	}
	stopped := b.cycleWatch.workFor(id)
	expiry := b.holds.place(id)
	return EditHold{BackupSetID: id, ExpiresAt: expiry, Stopped: stopped}, nil
}

// RenewBackupSetEdit extends an existing hold's lease. A client holding
// an edit form open calls this on a heartbeat; without it the hold lapses
// on its own, which is what stops a closed tab pausing a backup set
// forever.
//
// It is deliberately the same operation as taking one, rather than a
// renew that fails when the hold has already lapsed: a client whose
// heartbeat arrived a second late still has its form open and its
// operator still mid-edit, and refusing would leave the set unheld while
// the form stayed on screen, which is the worst of both.
func (b *BackupService) RenewBackupSetEdit(ctx context.Context, id string) (EditHold, error) {
	return b.BeginBackupSetEdit(ctx, id)
}

// EndBackupSetEdit releases the hold. Every route out of edit mode calls
// it, and calling it for a set that is not held is a success: several
// routes can fire for one edit (a save-and-exit followed by the form
// unmounting), and a duplicate release must not be something a client has
// to take care to avoid.
func (b *BackupService) EndBackupSetEdit(_ context.Context, id string) error {
	if _, _, ok := splitBackupSetID(id); !ok {
		return wrapNotFound(id)
	}
	if err := b.requireBackupSet(id); err != nil {
		return err
	}
	b.holds.release(id)
	return nil
}

// requireBackupSet refuses an id this deployment does not configure, so
// holding, renewing or releasing a set that does not exist is a 404
// rather than a hold on a name nothing will ever consult.
func (b *BackupService) requireBackupSet(id string) error {
	st := b.state.Load()
	for _, src := range st.inner.Config.Sources {
		for _, bs := range src.BackupSets {
			if src.Name+"/"+bs.Name == id {
				return nil
			}
		}
	}
	return wrapNotFound(id)
}

// wrapNotFound is the same %w-wrap every other method in this package
// uses for this condition (GetBackupSet, SetBackupSetEnabled,
// UpdateBackupSet), rather than a bespoke error type: a caller gets
// errors.Is(err, ErrBackupSetNotFound) and a message naming what was
// asked for, and one spelling means one thing to read.
func wrapNotFound(id string) error {
	return fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
}
