package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/retry"
)

// fastRetries keeps a test that deliberately drives a transport.Transient
// failure to its retry ceiling from spending DefaultRetryPolicy's two
// minutes doing it. It changes the count of attempts and the wait between
// them, nothing about which outcome the ceiling produces.
func fastRetries() retry.Policy {
	return retry.Policy{BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, Multiplier: 2, MaxAttempts: 2}
}

func refusedConnection(op string) error {
	return transport.NewError(transport.Transient, op, errors.New("dial tcp 172.20.0.3:22: connect: connection refused"))
}

// TestRunCycle_NothingGotThroughIsNotASuccessfulCycle is issue #361's
// reported shape, reproduced through the real RunCycle: three objects on
// the source, the source refusing the per-object connections behind its
// own listing so two of them cannot be discovered at all, and the one
// that was discovered refusing its transfer too. Nothing lands.
//
// The cycle has to be able to say that: Walked counts every artifact this
// pass had a reason to touch and Durable counts the ones whose bytes are
// actually on local disk, so "three walked, none through" is a fact the
// caller can read rather than something it has to infer from a summary
// line.
func TestRunCycle_NothingGotThroughIsNotASuccessfulCycle(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)

	tr := newFakeTransport()
	tr.put("first.dump", "first payload", epoch.Unix())
	tr.put("second.dump", "second payload", epoch.Unix())
	tr.put("third.dump", "third payload", epoch.Unix())
	tr.statErrFor = map[string]error{
		"second.dump": refusedConnection("stat"),
		"third.dump":  refusedConnection("stat"),
	}
	tr.copyErrFor = map[string]error{"first.dump": refusedConnection("copy_to_local")}

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RetryPolicy = fastRetries()

	report := svc.RunCycle(context.Background())
	if len(report.Sets) != 1 {
		t.Fatalf("len(report.Sets) = %d, want 1", len(report.Sets))
	}
	set := report.Sets[0]

	if set.Err != nil {
		t.Fatalf("BackupSetCycleResult.Err = %v, want nil: neither half of this failure is systemic, which is the whole reason #361 slipped through", set.Err)
	}
	if got := len(set.Discovery.Errors); got != 2 {
		t.Fatalf("discovery errors = %d, want 2 (result=%+v)", got, set.Discovery)
	}
	if set.Walked != 3 {
		t.Errorf("Walked = %d, want 3: two candidates discovery could not even identify, plus the one journal row it could", set.Walked)
	}
	if set.Durable != 0 {
		t.Errorf("Durable = %d, want 0: nothing reached a durable local copy this cycle", set.Durable)
	}

	entries, err := os.ReadDir(localDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadDir(%s): %v", localDir, err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".partial" {
			t.Errorf("local destination holds %q; this cycle backed nothing up and should have left nothing restorable behind", e.Name())
		}
	}
}

// TestRunCycle_ATransferThatExhaustsItsRetriesIsCountedFailedTheSameCycle
// is the mechanical half of #361, and a bug in its own right. When
// lifecycle.Transfer runs out of retries it durably records FAILED and
// then returns the copy error, so processArtifact takes its error exit
// and never sees the record the journal now holds. The cycle therefore
// reported the artifact as merely un-advanced, while `status`, reading
// the same journal a moment later, called the set FAILING.
//
// The journal is the authority on what happened to an artifact. This
// pins the cycle's own count to it.
func TestRunCycle_ATransferThatExhaustsItsRetriesIsCountedFailedTheSameCycle(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())

	tr := newFakeTransport()
	tr.put("backup.dump", "payload nobody can fetch", epoch.Unix())
	tr.copyErrFor = map[string]error{"backup.dump": refusedConnection("copy_to_local")}

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RetryPolicy = fastRetries()

	set := svc.RunCycle(context.Background()).Sets[0]

	rec, err := journal.Get(context.Background(), set.Discovery.Discovered[0].Artifact)
	if err != nil {
		t.Fatalf("Journal.Get: %v", err)
	}
	if lifecycle.State(rec.State) != lifecycle.Failed {
		t.Fatalf("journal state = %s, want FAILED: precondition for this test is that lifecycle.Transfer recorded the failure durably", rec.State)
	}
	if set.FailedArtifacts != 1 {
		t.Errorf("FailedArtifacts = %d, want 1: the journal says this artifact is FAILED, so the cycle that put it there has to say so too", set.FailedArtifacts)
	}
}

// TestRunCycle_AnIdleCycleIsNotAFailedCycle is #361's third
// Given/When/Then. Nothing on the remote, nothing in the journal: there
// was nothing to get through, which is not the same outcome as nothing
// getting through, and the counts have to tell the two apart. This is the
// common case on a poll interval and the one that must never page anyone.
func TestRunCycle_AnIdleCycleIsNotAFailedCycle(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), newFakeTransport(), nil)
	svc.Now = fixedNow(epoch)

	set := svc.RunCycle(context.Background()).Sets[0]

	if set.Err != nil {
		t.Fatalf("BackupSetCycleResult.Err = %v, want nil", set.Err)
	}
	if set.Walked != 0 || set.Durable != 0 || set.FailedArtifacts != 0 {
		t.Errorf("Walked=%d Durable=%d FailedArtifacts=%d, want 0/0/0 for a cycle with nothing to do", set.Walked, set.Durable, set.FailedArtifacts)
	}
}

// TestRunCycle_ASettledBackupSetKeepsCountingAsThroughput is the other
// idle shape, and the one a naive "did anything commit this cycle" rule
// gets wrong. Everything this backup set holds is already COMPLETE and
// the remote has nothing new, so no artifact moves at all; the bytes are
// still on disk, so the cycle has not failed to deliver anything.
func TestRunCycle_ASettledBackupSetKeepsCountingAsThroughput(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())

	tr := newFakeTransport()
	tr.put("backup.dump", "settled payload", epoch.Unix())

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	if first := svc.RunCycle(context.Background()).Sets[0]; first.Durable != 1 {
		t.Fatalf("precondition: first cycle Durable = %d, want 1", first.Durable)
	}

	second := svc.RunCycle(context.Background()).Sets[0]
	if second.Walked != 1 {
		t.Errorf("Walked = %d, want 1: the settled artifact is still a row this cycle walked", second.Walked)
	}
	if second.Durable != 1 {
		t.Errorf("Durable = %d, want 1: a COMPLETE artifact's bytes are on disk, so this cycle delivered a backup even though it moved nothing", second.Durable)
	}
}

// TestRunCycle_APartialCycleStillGetsSomethingThrough is #361's second
// Given/When/Then: one candidate the source would not identify, one that
// went all the way. The pass did real work and the failed candidate is
// genuinely for next time, so this must stay an ordinary cycle.
func TestRunCycle_APartialCycleStillGetsSomethingThrough(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())

	tr := newFakeTransport()
	tr.put("good.dump", "payload that lands", epoch.Unix())
	tr.put("refused.dump", "payload nobody can identify", epoch.Unix())
	tr.statErrFor = map[string]error{"refused.dump": refusedConnection("stat")}

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RetryPolicy = fastRetries()

	set := svc.RunCycle(context.Background()).Sets[0]

	if set.Walked != 2 {
		t.Errorf("Walked = %d, want 2", set.Walked)
	}
	if set.Durable != 1 {
		t.Errorf("Durable = %d, want 1: good.dump reached a durable local copy", set.Durable)
	}
	if set.FailedArtifacts != 0 {
		t.Errorf("FailedArtifacts = %d, want 0: a candidate discovery could not identify is left for the next pass, not failed", set.FailedArtifacts)
	}
}

// TestFetch_ReportsTheSameThroughputCountsAsRunCycle keeps #361's fourth
// acceptance criterion structural rather than incidental: `fetch` walks
// one backup set's share of exactly the same cycle, so it has to report
// the same evidence about it. Two commands that count differently are two
// commands that will eventually disagree about whether a cycle succeeded.
func TestFetch_ReportsTheSameThroughputCountsAsRunCycle(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())

	tr := newFakeTransport()
	tr.put("first.dump", "first payload", epoch.Unix())
	tr.put("second.dump", "second payload", epoch.Unix())
	tr.statErrFor = map[string]error{
		"first.dump":  refusedConnection("stat"),
		"second.dump": refusedConnection("stat"),
	}

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RetryPolicy = fastRetries()

	result, err := svc.Fetch(context.Background(), "production", "postgres-primary", false)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if result.Walked != 2 {
		t.Errorf("Walked = %d, want 2: both candidates were refused before they could become journal rows", result.Walked)
	}
	if result.Durable != 0 {
		t.Errorf("Durable = %d, want 0", result.Durable)
	}
}

// TestRunCycle_ADaemonHearsAboutACycleThatGotNothingThrough pins the
// daemon's half of #361's answer. A daemon cannot exit on a bad cycle
// without turning one outage into two, so the verdict has to reach an
// operator some other way: the same evidence `run` turns into an exit
// status is logged at ERROR in the FR-23 event stream, and then the next
// cycle runs. Without it, a daemon polling a source that has stopped
// answering describes every one of those cycles with the same INFO
// "cycle finished" line as a cycle that worked.
func TestRunCycle_ADaemonHearsAboutACycleThatGotNothingThrough(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())

	tr := newFakeTransport()
	tr.put("backup.dump", "payload nobody can identify", epoch.Unix())
	tr.statErrFor = map[string]error{"backup.dump": refusedConnection("stat")}

	var logged bytes.Buffer
	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, obs.New(&logged, obs.LevelInfo))
	svc.Now = fixedNow(epoch)
	svc.RetryPolicy = fastRetries()

	svc.RunCycle(context.Background())

	if !strings.Contains(logged.String(), "backed nothing up") {
		t.Errorf("the event stream never says this cycle backed nothing up.\nlogged:\n%s", logged.String())
	}
	if !strings.Contains(logged.String(), `"level":"ERROR"`) {
		t.Errorf("the cycle-got-nothing-through event is not at ERROR level.\nlogged:\n%s", logged.String())
	}
}

// TestRunCycle_AnIdleCycleSaysNothingAtAll is the other half of the same
// contract, and the one that decides whether this is a useful signal or
// noise an operator learns to filter out. A poll interval that finds an
// empty remote must produce no error line at all.
func TestRunCycle_AnIdleCycleSaysNothingAtAll(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())

	var logged bytes.Buffer
	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), newFakeTransport(), obs.New(&logged, obs.LevelInfo))
	svc.Now = fixedNow(epoch)

	svc.RunCycle(context.Background())

	if strings.Contains(logged.String(), "backed nothing up") {
		t.Errorf("an idle cycle logged the nothing-got-through error; on a poll interval that is a page every fifteen minutes.\nlogged:\n%s", logged.String())
	}
}

// TestRunCycle_AReadOnlyBackupSetCountsItsRetainedArtifactsAsThroughput
// guards the one state that would have made this whole rule a disaster if
// I had missed it. A backup set declared read-only (issue #282) never
// deletes its remote source, so its artifacts finish at REMOTE_RETAINED
// rather than COMPLETE. Their bytes are on local disk, verified and
// committed, so they are backed up in every sense that matters; a rule
// that only recognised COMPLETE would call every read-only cycle a
// failure, forever, on the exact posture a VPS being backed up actually
// wants. #356's two-machine end-to-end run uses --read-only, so this is
// also what stops that harness reading a healthy run as a failed one.
func TestRunCycle_AReadOnlyBackupSetCountsItsRetainedArtifactsAsThroughput(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	bs.ReadOnly = true

	tr := newFakeTransport()
	tr.put("backup.dump", "read-only payload", epoch.Unix())
	tr.poison = t // DeleteRemote must never be reached for a read-only set.

	svc := New(testConfig(t, testSource("production", bs)), openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)

	set := svc.RunCycle(context.Background()).Sets[0]

	if set.Walked != 1 {
		t.Fatalf("Walked = %d, want 1", set.Walked)
	}
	if set.Durable != 1 {
		t.Errorf("Durable = %d, want 1: a REMOTE_RETAINED artifact's bytes are on local disk, so this cycle delivered a backup", set.Durable)
	}
	if set.Outcome().Failed() {
		t.Errorf("a read-only cycle that backed its artifact up was called failed: %s", set.Outcome().Summary())
	}
}
