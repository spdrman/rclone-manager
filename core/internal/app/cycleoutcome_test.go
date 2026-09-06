package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/obs"
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
	if set.Progress.Durable != 0 {
		t.Errorf("Progress.Durable = %d, want 0: nothing moved toward a durable backup", set.Progress.Durable)
	}
	if !set.Progress.NothingGotThrough() {
		t.Errorf("Progress.NothingGotThrough() = false, want true for a cycle that walked %d and advanced %d", set.Progress.Walked, set.Progress.Durable)
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
	if set.Progress.Durable != 2 {
		t.Errorf("Progress.Durable = %d, want 2: the two artifacts that transferred", set.Progress.Durable)
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
	if first.Sets[0].Progress.Durable != 1 {
		t.Fatalf("precondition: first cycle Progress.Durable = %d, want 1", first.Sets[0].Progress.Durable)
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

// TestRunCycle_ARefusedRemoteCleanupIsNotABarrenCycle is the false alarm
// this count has to not raise. FR-16's identity re-check refusing to
// delete a remote source is the documented, expected steady state against
// a hardened account (see internal/lifecycle/remotedelete.go), so an
// artifact can sit at COMMITTED indefinitely with its bytes already
// durable on local disk. That backup succeeded. A cycle that walks it and
// moves nothing must not read as a cycle that got nothing through, every
// poll interval, forever.
func TestRunCycle_ARefusedRemoteCleanupIsNotABarrenCycle(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newRefusingTransport()
	tr.put("backup.dump", "cycle payload", epoch.Unix())
	tr.deleteErr = refusal("delete_remote")

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RetryPolicy = retry.Policy{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 2, MaxAttempts: 1}

	first := svc.RunCycle(context.Background()).Sets[0]
	artifact := first.Discovery.Discovered[0].Artifact
	rec, err := svc.Journal.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st := lifecycle.State(rec.State); st == lifecycle.Complete {
		t.Fatalf("precondition: journal state = %q; this test needs the remote cleanup to have been refused, leaving the artifact short of COMPLETE", st)
	}
	if _, err := os.Stat(mustFinalArtifactPath(t, localDir, artifact)); err != nil {
		t.Fatalf("precondition: the artifact's durable local copy should exist, since only the remote cleanup was refused: %v", err)
	}

	second := svc.RunCycle(context.Background()).Sets[0]
	if second.Progress.NothingGotThrough() {
		t.Errorf("Progress.NothingGotThrough() = true (%+v) for a cycle whose only artifact is already durable on local disk and is waiting on a remote cleanup its source keeps refusing; that is a healthy set, not a failed backup", second.Progress)
	}
	if second.FailedArtifacts != 0 {
		t.Errorf("FailedArtifacts = %d, want 0: a refused remote cleanup is not a failed artifact", second.FailedArtifacts)
	}
}

// TestRunCycle_HistoryDoesNotHideABarrenCycle is the case that decides
// what Walked is allowed to count, and it is the production shape the
// issue describes: "the operator saw a manager that reported success on
// every scheduled run and had backed up nothing". A real backup set has
// history. If the settled COMPLETE rows from earlier cycles counted
// toward this cycle's throughput, the rule would work exactly once, on a
// fresh journal, and never again.
//
// The storage ceiling is what makes this test say something the failed-
// artifact clause does not already say: internal/capacity refuses each
// transfer before it starts, so no row ends in a failure state and
// FailedArtifacts stays 0. The only thing left that can fail this cycle
// is the count.
func TestRunCycle_HistoryDoesNotHideABarrenCycle(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newRefusingTransport()
	tr.put("old.dump", "already backed up", epoch.Unix())

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RetryPolicy = retry.Policy{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 2, MaxAttempts: 1}

	first := svc.RunCycle(context.Background()).Sets[0]
	if first.Progress.NothingGotThrough() {
		t.Fatalf("precondition: the first cycle was supposed to back up its one artifact: %+v", first.Progress)
	}

	for _, name := range []string{"one.dump", "two.dump", "three.dump"} {
		tr.put(name, "payload of "+name, epoch.Unix())
	}
	svc.Capacity = capacity.Thresholds{CapBytes: 1}

	second := svc.RunCycle(context.Background()).Sets[0]

	if second.FailedArtifacts != 0 {
		t.Fatalf("precondition: FailedArtifacts = %d, want 0; this test only means anything if nothing ended in a failure state, so that the count is the only thing that can fail the cycle", second.FailedArtifacts)
	}
	if second.Progress.Walked != 3 {
		t.Errorf("Progress.Walked = %d, want 3: the three artifacts that arrived this cycle, and not the one that was already finished", second.Progress.Walked)
	}
	if second.Progress.Durable != 0 {
		t.Errorf("Progress.Durable = %d, want 0: none of the three got through", second.Progress.Durable)
	}
	if !second.Verdict().NothingGotThrough() {
		t.Errorf("Verdict().NothingGotThrough() = false (%+v): three artifacts arrived, none got through, and an artifact this set finished weeks ago does not make that a successful cycle", second.Progress)
	}
}

// TestRunCycle_ADiscoveryErrorOnAWalkedRowIsCountedOnce keeps the number
// an operator reads honest. A candidate that errors at identity capture
// can already have a journal row from an earlier cycle, in which case
// discovery and the walk are both looking at the same object. Counting it
// twice changes no verdict, since both readings say the same thing, but
// it puts a count in front of an operator that does not match what is on
// the remote.
func TestRunCycle_ADiscoveryErrorOnAWalkedRowIsCountedOnce(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newRefusingTransport()
	tr.put("backup.dump", "cycle payload", epoch.Unix())

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RetryPolicy = retry.Policy{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 2, MaxAttempts: 1}
	svc.Capacity = capacity.Thresholds{CapBytes: 1}

	// Cycle one journals the artifact and is refused admission, so the row
	// is left at DISCOVERED rather than ending in a failure state.
	first := svc.RunCycle(context.Background()).Sets[0]
	if len(first.Discovery.Discovered) != 1 {
		t.Fatalf("precondition: Discovery.Discovered = %+v, want one artifact", first.Discovery.Discovered)
	}

	// Now the source refuses the identity capture too, so the same object
	// is both a discovery error and a row this cycle walks.
	tr.statRefused["backup.dump"] = true

	second := svc.RunCycle(context.Background()).Sets[0]
	if len(second.Discovery.Errors) != 1 {
		t.Fatalf("precondition: Discovery.Errors = %+v, want 1", second.Discovery.Errors)
	}
	if second.Progress.Walked != 1 {
		t.Errorf("Progress.Walked = %d, want 1: there is one object on the remote, seen twice by two parts of the same pass", second.Progress.Walked)
	}
}

// TestCycleVerdict_ACycleThatStoppedEarlyIsNotCalledBarren pins the one
// case where the arithmetic is true and saying so would be wrong. A cycle
// that never reached its pipeline walked nothing for a reason that is
// already in the log; announcing "nothing got through" would invent a
// second, made-up cause in front of the real one.
func TestCycleVerdict_ACycleThatStoppedEarlyIsNotCalledBarren(t *testing.T) {
	barren := CycleVerdict{Progress: CycleProgress{Walked: 3}}
	if !barren.NothingGotThrough() {
		t.Fatalf("precondition: a verdict with three walked and none through should report it")
	}

	stopped := CycleVerdict{Systemic: true, Progress: CycleProgress{Walked: 3}}
	if stopped.NothingGotThrough() {
		t.Errorf("NothingGotThrough() = true for a cycle that stopped early; its zeros are vacuous, and the failure it actually hit is already reported")
	}
}

// TestRunCycle_SaysSoInTheEventStreamWhenNothingGotThrough is the
// daemon's half of the answer. `daemon` cannot exit on a bad cycle
// without turning one outage into two, so the evidence has to be in the
// FR-23 stream it already writes, not only in an exit status it never
// produces.
func TestRunCycle_SaysSoInTheEventStreamWhenNothingGotThrough(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newRefusingTransport()
	tr.put("backup.dump", "cycle payload", epoch.Unix())

	var stream bytes.Buffer
	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, obs.New(&stream, obs.LevelInfo))
	svc.Now = fixedNow(epoch)
	svc.Capacity = capacity.Thresholds{CapBytes: 1}

	set := svc.RunCycle(context.Background()).Sets[0]
	if !set.Verdict().NothingGotThrough() {
		t.Fatalf("precondition: this cycle was supposed to get nothing through: %+v", set.Progress)
	}

	var found string
	for _, line := range strings.Split(strings.TrimSpace(stream.String()), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("the event stream carries a line that is not JSON: %q", line)
		}
		if event["op"] == "cycle" {
			found, _ = event["error"].(string)
		}
	}
	if found == "" {
		t.Fatalf("nothing in the event stream says this cycle backed nothing up; a daemon has no exit status, so this is the only place it can say it.\nstream:\n%s", stream.String())
	}
	if !strings.Contains(found, "1 walked") || !strings.Contains(found, "0 got through") {
		t.Errorf("the event stream's message = %q, want it to name how many artifacts were walked and how many got through", found)
	}
}

// TestRunCycle_SaysNothingInTheEventStreamOnAQuietCycle is that message's
// own control. A daemon polling an empty remote every fifteen minutes
// must not write an error line every time it does.
func TestRunCycle_SaysNothingInTheEventStreamOnAQuietCycle(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	var stream bytes.Buffer
	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), newRefusingTransport(), obs.New(&stream, obs.LevelInfo))
	svc.Now = fixedNow(epoch)

	svc.RunCycle(context.Background())

	if strings.Contains(stream.String(), "backed nothing up") {
		t.Errorf("a cycle with nothing waiting on the remote wrote a failure into the event stream:\n%s", stream.String())
	}
}

// TestRunCycle_ATransientReadOfAFinishedReadOnlyArtifactIsNotWork is the
// false alarm that decides what a discovery error is allowed to count as.
//
// A read-only backup set never deletes its remote objects, so discovery
// re-reads every one of them on every cycle for the life of the set. One
// transient identity-capture failure against an artifact that was safely
// backed up weeks ago is not work this cycle failed to do, and a rule
// that read it that way would fail a healthy set on a single blip with
// nothing else going on.
func TestRunCycle_ATransientReadOfAFinishedReadOnlyArtifactIsNotWork(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""
	bs.ReadOnly = true

	tr := newRefusingTransport()
	tr.put("backup.dump", "cycle payload", epoch.Unix())
	tr.poison = t // a read-only set must never reach the transport's delete

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RetryPolicy = retry.Policy{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 2, MaxAttempts: 1}

	first := svc.RunCycle(context.Background()).Sets[0]
	artifact := first.Discovery.Discovered[0].Artifact
	rec, err := svc.Journal.Get(context.Background(), artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if acquiring(lifecycle.State(rec.State)) {
		t.Fatalf("precondition: journal state = %q, which is still in flight; this test needs a finished artifact", rec.State)
	}

	// The object is still on the remote, because that is what read-only
	// means, and this cycle cannot read its identity.
	tr.statRefused["backup.dump"] = true

	second := svc.RunCycle(context.Background()).Sets[0]
	if len(second.Discovery.Errors) != 1 {
		t.Fatalf("precondition: Discovery.Errors = %+v, want 1", second.Discovery.Errors)
	}
	if second.Progress.Walked != 0 {
		t.Errorf("Progress.Walked = %d, want 0: the object discovery could not read is one this set finished backing up, not work waiting to be done", second.Progress.Walked)
	}
	if second.Verdict().NothingGotThrough() {
		t.Errorf("Verdict().NothingGotThrough() = true for a read-only set whose only artifact is already safe and whose remote it re-reads every cycle by design")
	}
}

// TestFetch_SaysSoInTheEventStreamWhenNothingGotThrough is the same fact
// through the on-demand command. A manual fetch prints to a terminal too,
// but what a log shipper reads should not depend on which command
// produced the cycle.
func TestFetch_SaysSoInTheEventStreamWhenNothingGotThrough(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newRefusingTransport()
	tr.put("backup.dump", "cycle payload", epoch.Unix())

	var stream bytes.Buffer
	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, obs.New(&stream, obs.LevelInfo))
	svc.Now = fixedNow(epoch)
	svc.Capacity = capacity.Thresholds{CapBytes: 1}

	result, err := svc.Fetch(context.Background(), "production", bs.Name, false)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !result.Verdict().NothingGotThrough() {
		t.Fatalf("precondition: this fetch was supposed to get nothing through: %+v", result.Progress)
	}
	if !strings.Contains(stream.String(), "backed nothing up") {
		t.Errorf("nothing in the event stream says this fetch backed nothing up.\nstream:\n%s", stream.String())
	}
}

// --- issue #412: a cycle whose report covers no backup sets at all ---

// cycleErrors pulls every op="cycle" error message out of an event
// stream, which is the one place reportBarrenSets writes a cycle-level
// verdict (issue #361's per-set one and issue #412's whole-cycle one).
// Reading the stream rather than a return value is the point: `daemon`
// has no exit status, so this is the only channel it can say any of this
// on.
func cycleErrors(t *testing.T, stream string) []string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(strings.TrimSpace(stream), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("the event stream carries a line that is not JSON: %q", line)
		}
		if event["op"] != "cycle" {
			continue
		}
		if msg, _ := event["error"].(string); msg != "" {
			found = append(found, msg)
		}
	}
	return found
}

// TestRunCycle_SaysSoWhenNoBackupSetsAreConfigured is issue #412's first
// half. reportBarrenSets iterates report.Sets, so a cycle that visited no
// backup sets at all has nothing to iterate and says nothing: cycle_start
// and cycle_end are written exactly as they are for a healthy cycle, and
// a deployment with nothing configured backs nothing up every poll
// interval, forever, while reading clean.
//
// Removing backup sets (issue #391) means a deployment can genuinely end
// up here, and a fresh install starts here.
func TestRunCycle_SaysSoWhenNoBackupSetsAreConfigured(t *testing.T) {
	var stream bytes.Buffer
	svc := New(testConfig(t), openJournal(t), newRefusingTransport(), obs.New(&stream, obs.LevelInfo))
	svc.Now = fixedNow(epoch)

	report := svc.RunCycle(context.Background())
	if len(report.Sets) != 0 {
		t.Fatalf("precondition: report.Sets = %+v, want none for a configuration with no backup sets in it", report.Sets)
	}

	msgs := cycleErrors(t, stream.String())
	if len(msgs) != 1 {
		t.Fatalf("op=cycle errors = %v, want exactly one saying this cycle had no backup sets to run at all.\nstream:\n%s", msgs, stream.String())
	}
	if !strings.Contains(msgs[0], "no backup sets are configured") {
		t.Errorf("message = %q, want it to name this reason specifically: there is nothing configured", msgs[0])
	}
	if strings.Contains(msgs[0], "disabled") || strings.Contains(msgs[0], "held") {
		t.Errorf("message = %q, but nothing is configured here; sending an operator to look for switched-off sets that do not exist is a different problem from the one they have", msgs[0])
	}
}

// TestRunCycle_SaysSoWhenEveryConfiguredBackupSetIsDisabled is the second
// half, and it is the route that already existed before backup sets could
// be removed: RunCycle skips a disabled set before it is appended to the
// report, so switching every set off empties report.Sets just as
// thoroughly as configuring none.
//
// It must not be reported in the same words as the empty configuration
// above. An operator acts on "everything is switched off" by turning
// something back on, and on "nothing exists" by creating something, and a
// single message covering both sends half of them to the wrong screen.
func TestRunCycle_SaysSoWhenEveryConfiguredBackupSetIsDisabled(t *testing.T) {
	first := testBackupSet(t, t.TempDir())
	first.RemotePath = ""
	first.Disabled = true
	second := testBackupSet(t, t.TempDir())
	second.Name = "redis"
	second.ID = mustSetID(t, "production", "redis")
	second.RemotePath = ""
	second.Disabled = true

	var stream bytes.Buffer
	svc := New(testConfig(t, testSource("production", first, second)), openJournal(t), newRefusingTransport(), obs.New(&stream, obs.LevelInfo))
	svc.Now = fixedNow(epoch)

	report := svc.RunCycle(context.Background())
	if len(report.Sets) != 0 {
		t.Fatalf("precondition: report.Sets = %+v, want none; every set in this fixture is disabled", report.Sets)
	}

	msgs := cycleErrors(t, stream.String())
	if len(msgs) != 1 {
		t.Fatalf("op=cycle errors = %v, want exactly one saying every configured backup set is switched off.\nstream:\n%s", msgs, stream.String())
	}
	if strings.Contains(msgs[0], "no backup sets are configured") {
		t.Errorf("message = %q: two backup sets ARE configured, they are just disabled, and that is a different thing for an operator to fix", msgs[0])
	}
	if !strings.Contains(msgs[0], "2 configured") || !strings.Contains(msgs[0], "2 disabled") {
		t.Errorf("message = %q, want it to name how many backup sets exist and how many of them are disabled", msgs[0])
	}
}

// TestRunCycle_SaysSoWhenEveryConfiguredBackupSetIsHeldForEditing is the
// third route to an empty report and the one a reader is most likely to
// misdiagnose: an edit hold (issue #350) is skipped in the same place a
// disabled set is, but it is transient and nobody switched anything off.
// A message that only ever says "disabled" would have an operator hunting
// for a setting that is not set.
func TestRunCycle_SaysSoWhenEveryConfiguredBackupSetIsHeldForEditing(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	bs.RemotePath = ""

	var stream bytes.Buffer
	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), newRefusingTransport(), obs.New(&stream, obs.LevelInfo))
	svc.Now = fixedNow(epoch)

	ctx := WithBackupSetHolds(context.Background(), newTestHolds("production/postgres-primary"))
	report := svc.RunCycle(ctx)
	if len(report.Sets) != 0 {
		t.Fatalf("precondition: report.Sets = %+v, want none; this cycle's only backup set is held", report.Sets)
	}

	msgs := cycleErrors(t, stream.String())
	if len(msgs) != 1 {
		t.Fatalf("op=cycle errors = %v, want exactly one saying this cycle ran nothing.\nstream:\n%s", msgs, stream.String())
	}
	if !strings.Contains(msgs[0], "1 configured") || !strings.Contains(msgs[0], "1 held") {
		t.Errorf("message = %q, want it to say the one configured backup set was held rather than leaving a reader to assume somebody disabled it", msgs[0])
	}
	if strings.Contains(msgs[0], "1 disabled") {
		t.Errorf("message = %q: nothing here is disabled, and pointing an operator at a switch nobody flipped is the false alarm issue #350 exists to remove", msgs[0])
	}
}

// TestRunCycle_SaysNothingWhenAConfiguredSetGotSomethingThrough is this
// message's control, and it is the failure mode worse than the bug: a
// rule that cannot tell an empty cycle from a working one turns every
// healthy poll interval into an error line.
func TestRunCycle_SaysNothingWhenAConfiguredSetGotSomethingThrough(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.RemotePath = ""

	tr := newRefusingTransport()
	tr.put("backup.dump", "cycle payload", epoch.Unix())

	var stream bytes.Buffer
	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, obs.New(&stream, obs.LevelInfo))
	svc.Now = fixedNow(epoch)

	set := svc.RunCycle(context.Background()).Sets[0]
	if set.Progress.Durable != 1 {
		t.Fatalf("precondition: Progress = %+v, want one artifact through; this control only means something against a cycle that really worked", set.Progress)
	}

	if msgs := cycleErrors(t, stream.String()); len(msgs) != 0 {
		t.Errorf("a cycle that backed an artifact up wrote %v into the event stream.\nstream:\n%s", msgs, stream.String())
	}
}

// TestRunCycle_DoesNotBlameConfigurationForACycleThatWasCancelled is the
// vacuity guard, the same one CycleVerdict.NothingGotThrough already
// applies to a stopped pass. A cycle whose context is done breaks out of
// its loop before the first set, so report.Sets is empty for a reason
// that has nothing to do with the configuration. Announcing "every
// configured backup set is disabled" there would put an invented cause in
// front of an operator whose real one (the shutdown) is in cycle_end one
// line below.
func TestRunCycle_DoesNotBlameConfigurationForACycleThatWasCancelled(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	bs.RemotePath = ""

	tr := newRefusingTransport()
	tr.put("backup.dump", "cycle payload", epoch.Unix())

	var stream bytes.Buffer
	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, obs.New(&stream, obs.LevelInfo))
	svc.Now = fixedNow(epoch)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report := svc.RunCycle(ctx)
	if len(report.Sets) != 0 {
		t.Fatalf("precondition: report.Sets = %+v, want none; this cycle's context was already done", report.Sets)
	}

	if msgs := cycleErrors(t, stream.String()); len(msgs) != 0 {
		t.Errorf("a cycle cut short by shutdown reported %v; its one backup set is neither missing nor switched off.\nstream:\n%s", msgs, stream.String())
	}
}
