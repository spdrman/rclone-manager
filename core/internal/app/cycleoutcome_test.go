package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/retry"
)

// --- the fixture: a transport that can refuse one named remote path at a
// time, so a test can reproduce issue #361's shape (a source that answers
// a listing and then refuses the connections every later operation needs)
// without touching the shared fakeTransport every other test in this
// package leans on.

type refusingTransport struct {
	*fakeTransport

	// copyRefused and statRefused name the remote paths CopyToLocal and
	// Stat refuse. Stat is what discovery's own per-candidate identity
	// capture calls, so refusing it is how a candidate lands in
	// discovery.Result.Errors rather than becoming a journal row at all.
	copyRefused map[string]bool
	statRefused map[string]bool
}

func newRefusingTransport() *refusingTransport {
	return &refusingTransport{
		fakeTransport: newFakeTransport(),
		copyRefused:   map[string]bool{},
		statRefused:   map[string]bool{},
	}
}

// refusal is the error a host that will not accept another connection
// hands back, classified exactly as the rclone adapter classifies it: a
// transient failure, which is what makes issue #361's cycle look clean.
func refusal(op string) error {
	return transport.NewError(transport.Transient, op, errors.New("connect: connection refused"))
}

func (r *refusingTransport) CopyToLocal(ctx context.Context, source transport.Source, remotePath, localPartialPath string) (transport.TransferResult, error) {
	if r.copyRefused[remotePath] {
		return transport.TransferResult{}, refusal("copy_to_local")
	}
	return r.fakeTransport.CopyToLocal(ctx, source, remotePath, localPartialPath)
}

func (r *refusingTransport) Stat(ctx context.Context, source transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	if r.statRefused[remotePath] {
		return transport.RemoteArtifact{}, refusal("stat")
	}
	return r.fakeTransport.Stat(ctx, source, remotePath)
}

var _ transport.Transport = (*refusingTransport)(nil)

// cycleUnder runs one RunCycle over a single backup set against tr and
// returns that set's result. RetryPolicy is pinned to a single attempt so
// a refused copy fails in milliseconds rather than spending the default
// policy's two-minute budget on a host that is never going to answer.
func cycleUnder(t *testing.T, tr transport.Transport) (BackupSetCycleResult, *Service) {
	t.Helper()
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = "" // fakeTransport ignores Source.Root, so this is inert here.

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RetryPolicy = retry.Policy{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 2, MaxAttempts: 1}

	report := svc.RunCycle(context.Background())
	if len(report.Sets) != 1 {
		t.Fatalf("len(report.Sets) = %d, want 1", len(report.Sets))
	}
	return report.Sets[0], svc
}

// TestRunCycle_AFailedTransferIsCountedAsAFailedArtifact is the sharpest
// half of issue #361: the journal and the cycle report disagreed about
// the same artifact.
//
// lifecycle.Transfer records FAILED itself when the copy exhausts its
// retry budget (transfer.go's failCopy) and then returns the copy error.
// processArtifact's error path returns without reassigning rec, so the
// state it reports back is the one the record carried before the step
// ran, not the FAILED the journal now holds. processArtifacts counts the
// reported state, so the artifact was never counted, FailedArtifacts
// stayed 0, and the cycle read clean while the journal said the only
// artifact in it had failed.
func TestRunCycle_AFailedTransferIsCountedAsAFailedArtifact(t *testing.T) {
	tr := newRefusingTransport()
	tr.put("backup.dump", "cycle payload", epoch.Unix())
	tr.copyRefused["backup.dump"] = true

	set, svc := cycleUnder(t, tr)

	if len(set.Discovery.Discovered) != 1 {
		t.Fatalf("precondition: Discovery.Discovered = %+v, want exactly one artifact", set.Discovery.Discovered)
	}
	rec, err := svc.Journal.Get(context.Background(), set.Discovery.Discovered[0].Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if lifecycle.State(rec.State) != lifecycle.Failed {
		t.Fatalf("precondition: journal state = %q, want %q; this test only means anything if the transfer really did record FAILED", rec.State, lifecycle.Failed)
	}

	if set.FailedArtifacts != 1 {
		t.Errorf("BackupSetCycleResult.FailedArtifacts = %d, want 1: the journal says this artifact ended FAILED, so the cycle report must say so too", set.FailedArtifacts)
	}
}

// TestRunCycle_NothingGotThroughWhenEveryArtifactWasRefused is issue
// #361's own reproduction: three artifacts on the remote, discovery
// refused on two of them, and the third's transfer refused. Every
// individual verdict is defensible on its own, and the cycle as a whole
// still did nothing at all.
func TestRunCycle_NothingGotThroughWhenEveryArtifactWasRefused(t *testing.T) {
	tr := newRefusingTransport()
	for _, name := range []string{"one.dump", "two.dump", "three.dump"} {
		tr.put(name, "payload of "+name, epoch.Unix())
	}
	tr.statRefused["one.dump"] = true
	tr.statRefused["two.dump"] = true
	tr.copyRefused["three.dump"] = true

	set, _ := cycleUnder(t, tr)

	if len(set.Discovery.Errors) != 2 {
		t.Fatalf("precondition: Discovery.Errors = %+v, want 2", set.Discovery.Errors)
	}
	if set.Progress.Walked != 3 {
		t.Errorf("Progress.Walked = %d, want 3: two candidates discovery could not take in, plus the one journal row this cycle tried to transfer", set.Progress.Walked)
	}
	if set.Progress.Advanced != 0 {
		t.Errorf("Progress.Advanced = %d, want 0: nothing moved toward a durable backup", set.Progress.Advanced)
	}
	if !set.Progress.NothingGotThrough() {
		t.Errorf("Progress.NothingGotThrough() = false, want true for a cycle that walked %d and advanced %d", set.Progress.Walked, set.Progress.Advanced)
	}
}

// TestRunCycle_APartialCycleStillCounts is the other side of the same
// coin, and the reason the fix cannot simply be "no transfers, no
// success": two artifacts got through and one hit a refusal it will get
// another go at next cycle. That is a pass that did real work.
func TestRunCycle_APartialCycleStillCounts(t *testing.T) {
	tr := newRefusingTransport()
	for _, name := range []string{"one.dump", "two.dump", "three.dump"} {
		tr.put(name, "payload of "+name, epoch.Unix())
	}
	tr.copyRefused["three.dump"] = true

	set, _ := cycleUnder(t, tr)

	if set.Progress.Walked != 3 {
		t.Errorf("Progress.Walked = %d, want 3", set.Progress.Walked)
	}
	if set.Progress.Advanced != 2 {
		t.Errorf("Progress.Advanced = %d, want 2: the two artifacts that transferred", set.Progress.Advanced)
	}
	if set.Progress.NothingGotThrough() {
		t.Errorf("Progress.NothingGotThrough() = true for a cycle that got 2 of 3 artifacts through; that is a healthy pass, not a failure")
	}
}

// TestRunCycle_AnEmptyCycleIsNotABarrenOne is the distinction the whole
// fix turns on. Nothing on the remote and nothing in the journal is not a
// cycle that failed, it is a cycle with nothing to do, and a fix that
// cannot tell those apart turns every quiet night into an alarm.
func TestRunCycle_AnEmptyCycleIsNotABarrenOne(t *testing.T) {
	set, _ := cycleUnder(t, newRefusingTransport())

	if set.Progress.Walked != 0 {
		t.Errorf("Progress.Walked = %d, want 0: there was nothing on the remote and nothing in the journal", set.Progress.Walked)
	}
	if set.Progress.NothingGotThrough() {
		t.Errorf("Progress.NothingGotThrough() = true for a cycle with nothing to do; nothing was waiting, so nothing failed")
	}
}

// TestRunCycle_ASteadyStateCycleIsNotABarrenOne is the same distinction
// against a real journal rather than an empty one: the first cycle takes
// the artifact all the way to COMPLETE, and the second has genuinely
// nothing left to do. The second cycle still walks that journal row, so
// counting rows rather than pending work would fail this.
func TestRunCycle_ASteadyStateCycleIsNotABarrenOne(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newRefusingTransport()
	tr.put("backup.dump", "cycle payload", epoch.Unix())

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	first := svc.RunCycle(context.Background())
	if first.Sets[0].Progress.Advanced != 1 {
		t.Fatalf("precondition: first cycle Progress.Advanced = %d, want 1", first.Sets[0].Progress.Advanced)
	}

	second := svc.RunCycle(context.Background()).Sets[0]
	if second.Progress.Walked != 0 {
		t.Errorf("Progress.Walked = %d, want 0 on a steady-state cycle: the one journal row is COMPLETE, so there is nothing left to move", second.Progress.Walked)
	}
	if second.Progress.NothingGotThrough() {
		t.Errorf("Progress.NothingGotThrough() = true on a steady-state cycle; nothing was waiting, so nothing failed")
	}
}

// TestFetch_ReportsTheSameProgressAsRunCycle is issue #361's fourth
// acceptance criterion at the level it actually has to hold: `fetch` and
// `run` share processArtifacts, so they must produce the same arithmetic
// for the same backup set, not two counts that happen to agree today.
func TestFetch_ReportsTheSameProgressAsRunCycle(t *testing.T) {
	build := func() (*Service, string) {
		localDir := t.TempDir()
		bs := testBackupSet(t, localDir)
		bs.RemotePath = ""
		tr := newRefusingTransport()
		tr.put("one.dump", "payload one", epoch.Unix())
		tr.put("two.dump", "payload two", epoch.Unix())
		tr.statRefused["one.dump"] = true
		tr.copyRefused["two.dump"] = true
		svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
		svc.Now = fixedNow(epoch)
		svc.RetryPolicy = retry.Policy{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 2, MaxAttempts: 1}
		return svc, bs.Name
	}

	runSvc, _ := build()
	fromRun := runSvc.RunCycle(context.Background()).Sets[0].Progress

	fetchSvc, setName := build()
	result, err := fetchSvc.Fetch(context.Background(), "production", setName, false)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if result.Progress != fromRun {
		t.Errorf("fetch Progress = %+v, run Progress = %+v: the two commands walk the same journal through the same code and must report the same arithmetic", result.Progress, fromRun)
	}
	if !result.Progress.NothingGotThrough() {
		t.Errorf("fetch Progress.NothingGotThrough() = false for a cycle where one candidate was refused discovery and the other's transfer was refused: %+v", result.Progress)
	}
}
