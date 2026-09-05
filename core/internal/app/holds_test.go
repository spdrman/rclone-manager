package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// Issue #350's two halves, plus the race the watcher is written to lose.
//
// The scheduler half is that a held set is not started. The interrupt half is
// that a hold landing mid-pass stops the set where it stands. Both need a set
// that is genuinely doing work, or a pass that skipped for some unrelated
// reason would look identical to one that yielded.
//
// TestWatchForHold_CatchesAHoldPlacedInsideTheCheckWindow is the one that is
// easy to delete by accident. The watcher reads Changed() before Held() on
// every iteration, and the whole correctness of that ordering is about a hold
// placed in the gap between the two: read the other way round, the hold
// closes a channel nobody is listening on yet and the cancellation is lost
// until something unrelated happens to wake the goroutine. The fixture plants
// a hold inside exactly that window, which is why it is a purpose-built
// double rather than the simple registry the other cases use.
//
// The last two cases are about honesty rather than mechanism. A pass an
// operator stopped must not be reported as a failure, and must not be
// reported as a set that backed nothing up either, because on the arithmetic
// alone a stopped pass looks exactly like a barren one. Both were real false
// alarms before the distinction existed.
//
// The no-registry case is the control that a cycle nobody is holding behaves
// exactly as it did before any of this.

// testHolds is a hand-rolled BackupSetHolds for these tests. core/service
// has the real one; this package must not import it (the dependency runs
// the other way), and a double here also lets a test place a hold at an
// exact moment inside a cycle, which the real one's HTTP-driven lifetime
// could not.
type testHolds struct {
	mu      sync.Mutex
	held    map[string]bool
	changed chan struct{}
}

func newTestHolds(ids ...string) *testHolds {
	h := &testHolds{held: map[string]bool{}, changed: make(chan struct{})}
	for _, id := range ids {
		h.held[id] = true
	}
	return h
}

func (h *testHolds) Held(setID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held[setID]
}

func (h *testHolds) Changed() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed
}

func (h *testHolds) hold(setID string) {
	h.mu.Lock()
	h.held[setID] = true
	close(h.changed)
	h.changed = make(chan struct{})
	h.mu.Unlock()
}

// TestRunCycle_SkipsABackupSetHeldForEditing is the scheduler half of the
// issue's hold: a set being edited must not have a new pass started
// against it, because "stop the in-flight run but leave the poll interval
// free" is the same two-writers race with extra steps.
//
// The unheld set beside it is the control: without it, an implementation
// that skipped every set would pass.
func TestRunCycle_SkipsABackupSetHeldForEditing(t *testing.T) {
	heldLocal, freeLocal := t.TempDir(), t.TempDir()
	held := testBackupSet(t, heldLocal)
	held.Name = "held"
	held.ID = mustSetID(t, "production", "held")
	free := testBackupSet(t, freeLocal)
	free.Name = "free"
	free.ID = mustSetID(t, "production", "free")

	tr := newFakeTransport()
	tr.put("backup.dump", "hold test payload", epoch.Unix())

	svc := New(testConfig(t, testSource("production", held, free)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	ctx := WithBackupSetHolds(context.Background(), newTestHolds("production/held"))
	report := svc.RunCycle(ctx)

	if len(report.Sets) != 1 {
		t.Fatalf("this cycle visited %d backup sets, want exactly 1 (the unheld one); sets=%+v", len(report.Sets), report.Sets)
	}
	if got := report.Sets[0].Set.String(); got != "production/free" {
		t.Errorf("the cycle visited %q, want %q; a held set must not be processed at all", got, "production/free")
	}
}

// TestRunCycle_AHoldLandingMidCycleStopsThatSetsRemainingWork is the
// other half: a hold placed while the cycle is already inside that set
// has to stop it, not wait for the pass to finish. The hold lands from
// inside the transport's own copy, which is the moment the issue names
// (an operator pressing Edit while a transfer is in flight).
//
// It also pins the property the issue is most emphatic about: an
// interrupted transfer must never present as a completed artifact. The
// artifact this cycle was copying must be left in a pre-durable state,
// never COMMITTED or REMOTE_DELETED.
func TestRunCycle_AHoldLandingMidCycleStopsThatSetsRemainingWork(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)

	holds := newTestHolds()
	tr := newFakeTransport()
	tr.put("first.dump", "the artifact being copied when Edit was pressed", epoch.Unix())
	tr.put("second.dump", "an artifact this cycle must never start", epoch.Unix())
	// The hold lands the instant the first copy begins, which is exactly
	// "Edit pressed while a transfer is in flight".
	tr.beforeCopy = func() { holds.hold(bs.ID.String()) }

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	ctx := WithBackupSetHolds(context.Background(), holds)
	svc.RunCycle(ctx)

	records, err := journal.ListByBackupSet(context.Background(), bs.ID)
	if err != nil {
		t.Fatalf("ListByBackupSet: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("the cycle journaled nothing at all, so this test would prove nothing about where it stopped")
	}
	for _, rec := range records {
		switch lifecycle.State(rec.State) {
		case lifecycle.Committed, lifecycle.RemoteDeletePending, lifecycle.Complete, lifecycle.RemoteRetained:
			t.Errorf("artifact %s reached %s after its transfer was interrupted by an edit hold; an interrupted transfer must never present as a completed artifact",
				rec.Artifact, rec.State)
		}
	}
	if got := tr.copyToLocalCalls(); got != 1 {
		t.Errorf("the transport copied %d artifacts, want 1: once the hold landed, no further artifact in that set may be started", got)
	}
}

// TestRunCycle_WithoutAnyHoldsRegistryEverySetRuns is the control that
// stops the two tests above passing vacuously. Without it, a RunCycle
// that skipped every set, or one that cancelled every set's context,
// would satisfy both.
func TestRunCycle_WithoutAnyHoldsRegistryEverySetRuns(t *testing.T) {
	aLocal, bLocal := t.TempDir(), t.TempDir()
	setA := testBackupSet(t, aLocal)
	setA.Name = "held"
	setA.ID = mustSetID(t, "production", "held")
	setB := testBackupSet(t, bLocal)
	setB.Name = "free"
	setB.ID = mustSetID(t, "production", "free")

	tr := newFakeTransport()
	tr.put("backup.dump", "no holds payload", epoch.Unix())

	svc := New(testConfig(t, testSource("production", setA, setB)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	report := svc.RunCycle(context.Background())
	if len(report.Sets) != 2 {
		t.Fatalf("a cycle with no holds registry visited %d sets, want 2", len(report.Sets))
	}
}

// TestRunCycle_AHoldOnOneSetLeavesAnotherAlone: the hold is per backup
// set, so editing one must not stop the deployment's other backups. The
// held set's own work stops; the other's completes.
func TestRunCycle_AHoldOnOneSetLeavesAnotherAlone(t *testing.T) {
	heldLocal, freeLocal := t.TempDir(), t.TempDir()
	held := testBackupSet(t, heldLocal)
	held.Name = "held"
	held.ID = mustSetID(t, "production", "held")
	free := testBackupSet(t, freeLocal)
	free.Name = "free"
	free.ID = mustSetID(t, "production", "free")

	tr := newFakeTransport()
	tr.put("backup.dump", "per-set hold payload", epoch.Unix())

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", held, free)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	ctx := WithBackupSetHolds(context.Background(), newTestHolds("production/held"))
	svc.RunCycle(ctx)

	freeRecords, err := journal.ListByBackupSet(context.Background(), free.ID)
	if err != nil {
		t.Fatalf("ListByBackupSet(free): %v", err)
	}
	if len(freeRecords) == 0 {
		t.Fatal("the unheld backup set journaled nothing; holding one set must not stop another")
	}
	reachedDurable := false
	for _, rec := range freeRecords {
		if isDurable(rec) {
			reachedDurable = true
		}
	}
	if !reachedDurable {
		t.Errorf("the unheld backup set's artifact never reached a durable state: %+v", freeRecords)
	}

	heldRecords, err := journal.ListByBackupSet(context.Background(), held.ID)
	if err != nil {
		t.Fatalf("ListByBackupSet(held): %v", err)
	}
	if len(heldRecords) != 0 {
		t.Errorf("the held backup set journaled %d record(s); a held set must not even be discovered: %+v", len(heldRecords), heldRecords)
	}
}

func isDurable(rec state.Record) bool {
	switch lifecycle.State(rec.State) {
	case lifecycle.Committed, lifecycle.RemoteDeletePending, lifecycle.Complete, lifecycle.RemoteRetained:
		return true
	}
	return false
}

// TestBackupSetHoldsFrom_ReportsWhatWasInstalled: a package that installs
// a value on a context has to be able to prove it installed one, the same
// reason ProgressObserverFrom is exported.
func TestBackupSetHoldsFrom_ReportsWhatWasInstalled(t *testing.T) {
	if got := BackupSetHoldsFrom(context.Background()); got != nil {
		t.Errorf("BackupSetHoldsFrom(a bare context) = %v, want nil", got)
	}
	h := newTestHolds("production/x")
	if got := BackupSetHoldsFrom(WithBackupSetHolds(context.Background(), h)); got != h {
		t.Errorf("BackupSetHoldsFrom did not return the registry WithBackupSetHolds installed")
	}
	if got := BackupSetHoldsFrom(WithBackupSetHolds(context.Background(), nil)); got != nil {
		t.Errorf("WithBackupSetHolds(ctx, nil) installed something; a nil registry must mean no registry")
	}
	_ = time.Second
}

var _ config.BackupSet // keeps the config import honest if the fixtures move

// windowHolds places its hold in the exact window between a watcher's
// Held() answer and its next Changed() read: the first time Held is
// asked and answers false, it plants the hold (and fires the broadcast)
// on the way out.
//
// It exists to test one ordering claim in watchForHold's own doc, which
// no test driving a whole RunCycle can reach: the window it describes is
// a few instructions wide, so a real cycle hits it essentially never, and
// a mutation that reverses the order therefore passes every test in this
// file. That is the shape of bug this double is for.
type windowHolds struct {
	mu      sync.Mutex
	held    bool
	planted bool
	changed chan struct{}
}

func newWindowHolds() *windowHolds { return &windowHolds{changed: make(chan struct{})} }

func (h *windowHolds) Held(string) bool {
	h.mu.Lock()
	answer := h.held
	plant := !answer && !h.planted
	if plant {
		h.planted = true
	}
	h.mu.Unlock()

	// Outside the lock, because plantHold takes it too.
	if plant {
		h.plantHold()
	}
	return answer
}

func (h *windowHolds) plantHold() {
	h.mu.Lock()
	h.held = true
	close(h.changed)
	h.changed = make(chan struct{})
	h.mu.Unlock()
}

func (h *windowHolds) Changed() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed
}

// TestWatchForHold_CatchesAHoldPlacedInsideTheCheckWindow pins the
// ordering watchForHold documents: read Changed() BEFORE Held(), so a
// hold placed between the two closes a channel the watcher is already
// holding rather than one it has not read yet.
//
// Reversed, this deadlocks: the watcher answers "not held", the hold
// lands and closes the channel nobody is listening on, and the watcher
// then subscribes to the NEXT channel, which nothing will ever close. The
// set's pass would run to completion against a definition an operator is
// editing, which is the whole race the hold exists to stop.
func TestWatchForHold_CatchesAHoldPlacedInsideTheCheckWindow(t *testing.T) {
	holds := newWindowHolds()
	setCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var fired atomic.Bool
	go watchForHold(setCtx, holds, "production/postgres-primary", &fired, cancel)

	select {
	case <-setCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("watchForHold never cancelled: a hold placed between its Held() answer and its next Changed() read was lost")
	}
	if !fired.Load() {
		t.Error("the cancellation flag was not set by the time the context was done; a reader that sees the cancellation has to be able to see the reason for it too")
	}
}

// TestRunCycle_AStoppedSetIsNotReportedAsAFailure is the honesty half of
// the hold. Stopping a set because an operator pressed Edit is something
// this manager was asked to do, so it must not come back looking like a
// backup that broke: a cycle report carrying context.Canceled here is
// read as a failure by every consumer downstream of it (core/service
// fails the operation an operator submitted, `backup-manager run` exits
// 1, and the health surface counts the set as unevaluated), which turns
// an ordinary edit into a false alarm in the one product where a false
// alarm about a backup is expensive.
//
// The interrupted artifact is checked too: a transfer stopped this way is
// not a failed transfer, so it must not be journaled as FAILED or
// QUARANTINED either, or the repeated-failure alert (#105) fires for an
// edit.
func TestRunCycle_AStoppedSetIsNotReportedAsAFailure(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)

	holds := newTestHolds()
	tr := newFakeTransport()
	tr.put("first.dump", "the artifact being copied when Edit was pressed", epoch.Unix())
	tr.beforeCopy = func() { holds.hold(bs.ID.String()) }

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	report := svc.RunCycle(WithBackupSetHolds(context.Background(), holds))

	if len(report.Sets) != 1 {
		t.Fatalf("this cycle reported %d backup sets, want exactly 1; sets=%+v", len(report.Sets), report.Sets)
	}
	set := report.Sets[0]
	if set.Err == nil {
		t.Fatal("the stopped set reported no error at all; it has to say WHY its pass did not finish, or a caller cannot tell a stopped set from a completed one")
	}
	if !errors.Is(set.Err, ErrBackupSetHeldForEditing) {
		t.Errorf("Err = %v, want one that errors.Is ErrBackupSetHeldForEditing; a bare context error is indistinguishable from a real failure to every consumer of this report", set.Err)
	}
	if set.FailedArtifacts != 0 {
		t.Errorf("FailedArtifacts = %d, want 0: an artifact whose transfer an operator stopped has not failed", set.FailedArtifacts)
	}

	records, err := journal.ListByBackupSet(context.Background(), bs.ID)
	if err != nil {
		t.Fatalf("ListByBackupSet: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("the cycle journaled nothing at all, so this test would prove nothing about how the interrupted artifact was recorded")
	}
	for _, rec := range records {
		switch lifecycle.State(rec.State) {
		case lifecycle.Failed, lifecycle.Quarantined, lifecycle.QuarantinedLost:
			t.Errorf("artifact %s was journaled %s after an operator stopped its transfer by entering edit mode; a stopped transfer is not a failed one",
				rec.Artifact, rec.State)
		}
	}
}

// TestRunCycle_AStoppedSetIsNotReportedAsBarrenEither is the same false
// alarm arriving by the other road, and it is the one composition
// actually opened up.
//
// Issue #361 gave a cycle report a second way to fail: a set that had
// artifacts in front of it and got none of them through. A pass an
// operator stopped mid-transfer has exactly that arithmetic, because the
// row it was copying is counted and did not land, so on the numbers
// alone a set somebody pressed Edit on a second ago reads as one that
// backed nothing up. Both halves are individually right and together
// they put "backed nothing up this cycle" in front of an operator, and
// exit 1 behind it, for an ordinary edit.
//
// SystemicFailure alone does not cover it. Excluding a stopped pass from
// the failure test is what makes it eligible for the barren test, so the
// two guards have to be separate, which is what CycleVerdict.Stopped is
// for.
//
// The preconditions are Fatal on purpose: without a walked row and no
// durable one, the arithmetic never reaches the guard and the test would
// pass against an implementation that has none.
func TestRunCycle_AStoppedSetIsNotReportedAsBarrenEither(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)

	holds := newTestHolds()
	tr := newFakeTransport()
	tr.put("first.dump", "the artifact being copied when Edit was pressed", epoch.Unix())
	tr.beforeCopy = func() { holds.hold(bs.ID.String()) }

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	report := svc.RunCycle(WithBackupSetHolds(context.Background(), holds))
	if len(report.Sets) != 1 {
		t.Fatalf("this cycle reported %d backup sets, want exactly 1", len(report.Sets))
	}
	set := report.Sets[0]

	if !errors.Is(set.Err, ErrBackupSetHeldForEditing) {
		t.Fatalf("precondition: Err = %v, want ErrBackupSetHeldForEditing; the rest of this test is about what a STOPPED pass reports", set.Err)
	}
	if set.Progress.Walked == 0 {
		t.Fatalf("precondition: Progress.Walked = 0, so the barren arithmetic never applies here and this test would prove nothing")
	}
	if set.Progress.Durable != 0 {
		t.Fatalf("precondition: Progress.Durable = %d, want 0; the hold lands before the copy, so nothing can have become durable", set.Progress.Durable)
	}
	if !set.Progress.NothingGotThrough() {
		t.Fatalf("precondition: the raw arithmetic says something got through (%d walked, %d durable), so the guard under test is never reached", set.Progress.Walked, set.Progress.Durable)
	}

	v := set.Verdict()
	if v.Systemic {
		t.Errorf("Verdict().Systemic = true for a pass an operator stopped; `backup-manager run` exits 1 on that and the activity feed calls an ordinary edit a backup that failed")
	}
	if !v.Stopped {
		t.Errorf("Verdict().Stopped = false; a consumer that cannot see the pass was stopped on purpose has to guess from an absent error, which is also what a pass that simply finished looks like")
	}
	if v.NothingGotThrough() {
		t.Errorf("Verdict().NothingGotThrough() = true for a set stopped mid-transfer by an edit hold; it walked %d and got %d through because it was told to stop, and reporting that as a backup that did not happen is the false alarm this whole path exists to remove", v.Progress.Walked, v.Progress.Durable)
	}
}
