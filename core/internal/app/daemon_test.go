package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// The two properties of the loop's shape, proven without sending a signal.
//
// Daemon's guarantees are structural: passes never overlap, and cancellation
// stops it promptly. Both are claims about the loop rather than about any
// step inside it, which is exactly why the loop lives in this package instead
// of in the CLI, where proving either would mean driving a real process
// through a real SIGTERM.
//
// The overlap test works by making overlap inevitable if it is possible at
// all: a 10ms poll interval against a journal deliberately slowed to about
// 40ms per call, run for 220ms. So the interesting number is not how many
// cycles happened, it is the high-water mark of concurrent journal calls,
// which slowJournal counts as they enter and leave. A test that only counted
// cycles at the end would pass against an implementation that ran two at once
// and finished both.
//
// The interval and cancellation cases are the ordinary halves, and the
// non-positive interval case is argument validation that costs nothing and
// catches a caller passing a zero duration through from an unset field.

// slowJournal wraps a real *state.Journal and, on every ListByBackupSet
// call (which every RunCycle call makes at least twice per backup set:
// once to list in-flight artifacts, once inside RetentionPreview), tracks
// how many calls are concurrently in flight and sleeps for delay before
// returning. It exists so Daemon's "no two cycles ever overlap" property
// (documented on both Daemon and RunCycle) can be tested directly, by
// giving each simulated cycle enough wall-clock duration that a second,
// overlapping one would be observable if the implementation ever allowed
// it, rather than only inferred from the code never spawning a goroutine.
type slowJournal struct {
	*state.Journal
	delay time.Duration

	inFlight    int32
	maxInFlight int32
}

func (j *slowJournal) ListByBackupSet(ctx context.Context, set model.BackupSetID) ([]state.Record, error) {
	cur := atomic.AddInt32(&j.inFlight, 1)
	for {
		max := atomic.LoadInt32(&j.maxInFlight)
		if cur <= max || atomic.CompareAndSwapInt32(&j.maxInFlight, max, cur) {
			break
		}
	}
	time.Sleep(j.delay)
	defer atomic.AddInt32(&j.inFlight, -1)
	return j.Journal.ListByBackupSet(ctx, set)
}

var _ Journal = (*slowJournal)(nil)

// TestDaemon_NeverOverlapsCycles proves the "no overlapping processing for
// the same backup set" half of FR-1 for the daemon loop specifically: with
// a poll interval far shorter than one cycle's own duration, Daemon must
// never let a second RunCycle start before the first one's own
// ListByBackupSet calls have all returned.
func TestDaemon_NeverOverlapsCycles(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)

	tr := newFakeTransport()
	tr.put("backup.dump", "daemon payload", epoch.Unix())

	base := openJournal(t)
	journal := &slowJournal{Journal: base, delay: 40 * time.Millisecond}

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	ctx, cancel := context.WithTimeout(context.Background(), 220*time.Millisecond)
	defer cancel()

	// A 10ms poll_interval against ~40ms-per-ListByBackupSet-call cycles:
	// if Daemon ever let cycles overlap, maxInFlight would climb well past
	// 1 over a 220ms run.
	if err := svc.Daemon(ctx, 10*time.Millisecond); err != nil {
		t.Fatalf("Daemon: %v", err)
	}

	if max := atomic.LoadInt32(&journal.maxInFlight); max > 1 {
		t.Errorf("max concurrent ListByBackupSet calls = %d, want 1: cycles overlapped", max)
	}
}

// TestDaemon_RepeatsAtPollInterval proves the other half of FR-1's
// "daemon repeatedly invokes the same processing cycle at a configured
// interval": letting Daemon run for a few multiples of a short interval
// must run RunCycle more than once.
func TestDaemon_RepeatsAtPollInterval(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)

	tr := newFakeTransport()
	tr.put("backup.dump", "daemon payload", epoch.Unix())

	var cycles int32
	countJournal := &countingCyclesJournal{Journal: openJournal(t), cycles: &cycles}

	svc := New(testConfig(t, testSource("production", bs)), countJournal, tr, nil)
	svc.Now = fixedNow(epoch)

	ctx, cancel := context.WithTimeout(context.Background(), 130*time.Millisecond)
	defer cancel()

	if err := svc.Daemon(ctx, 20*time.Millisecond); err != nil {
		t.Fatalf("Daemon: %v", err)
	}

	if got := atomic.LoadInt32(&cycles); got < 2 {
		t.Errorf("RunCycle ran %d time(s) over 130ms at a 20ms interval, want at least 2", got)
	}
}

// countingCyclesJournal counts ListByBackupSet calls as a proxy for "how
// many times has RunCycle run", since every cycle calls it at least once
// per configured backup set.
type countingCyclesJournal struct {
	*state.Journal
	cycles *int32
}

func (j *countingCyclesJournal) ListByBackupSet(ctx context.Context, set model.BackupSetID) ([]state.Record, error) {
	atomic.AddInt32(j.cycles, 1)
	return j.Journal.ListByBackupSet(ctx, set)
}

var _ Journal = (*countingCyclesJournal)(nil)

// TestDaemon_StopsOnCancellation proves Daemon returns promptly and
// without error once ctx is cancelled, whether or not a cycle happened to
// be in flight at the time (FR-1: "shut down" is the ordinary path, not an
// error condition).
func TestDaemon_StopsOnCancellation(t *testing.T) {
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)

	tr := newFakeTransport()
	tr.put("backup.dump", "daemon payload", epoch.Unix())

	journal := openJournal(t)
	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Daemon is ever called

	done := make(chan error, 1)
	go func() { done <- svc.Daemon(ctx, time.Hour) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Daemon returned %v, want nil on an ordinary shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Daemon did not return after ctx was already cancelled")
	}
}

// TestDaemon_RejectsNonPositiveInterval is a small argument-validation
// check: interval <= 0 has no sensible meaning for "repeat this cycle
// every interval" and must be refused before the loop ever starts.
func TestDaemon_RejectsNonPositiveInterval(t *testing.T) {
	svc := New(&config.Config{}, openJournal(t), nil, nil)
	if err := svc.Daemon(context.Background(), 0); err == nil {
		t.Error("Daemon(ctx, 0) = nil error, want an error")
	}
}
