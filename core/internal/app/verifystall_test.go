package app

// Issue #419's acceptance line asks for a test that drives the whole path
// rather than the classification alone: the timeout, the exhaustion, the
// state it lands in, and the recovery. This is that test, run through
// processArtifact and a real SQLite journal, one call per simulated cycle.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/retry"
)

// unreachableSource is the failure #388 measured, reproduced as a value:
// rclone's own connect timeout, classified Transient, with
// context.DeadlineExceeded still reachable through Unwrap. The second part
// is what makes this the right fixture rather than any transient error:
// a naive errors.Is would read it as the operator having stopped the run.
func unreachableSource() error {
	return transport.NewError(transport.Transient, "remote_hash",
		fmt.Errorf(`source "production": NewFs: couldn't connect SSH: dial tcp 192.0.2.1:22: %w`, context.DeadlineExceeded))
}

// mustGetRow reads the artifact's current journal row, which is what a cycle
// starts from: every assertion here is against what was durably recorded,
// never against what a call happened to return.
func mustGetRow(t *testing.T, j Journal, id model.ArtifactID) state.Record {
	t.Helper()
	rec, err := j.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("journal.Get(%s): %v", id, err)
	}
	return rec
}

// TestVerificationAgainstAnUnreachableSource_StallsThenReachesAnOperator
// walks the whole thing. The source answers the copy and then goes quiet
// for the hash call, which is exactly the window a connect timeout during
// verification opens.
func TestVerificationAgainstAnUnreachableSource_StallsThenReachesAnOperator(t *testing.T) {
	ctx := context.Background()
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.Validation = config.Validation{Hash: "sha256"}
	source := transport.Source{ID: "production"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload bytes", epoch.Unix())

	journal := openJournal(t)
	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)
	// A budget of three, so the whole path fits in a test. It is the same
	// number verifyOne derives in production, read off the same field.
	svc.RetryPolicy = retry.Policy{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 2, MaxAttempts: 3}

	tr.remoteHashErr = unreachableSource()

	// --- cycles 1 and 2: the copy lands, the hash call cannot be made,
	// and the artifact stays exactly where it honestly is.
	for cycle := 1; cycle <= 2; cycle++ {
		got := svc.processArtifact(ctx, source, bs, mustGetRow(t, journal, rec.Artifact))
		if got != lifecycle.Verifying {
			t.Fatalf("cycle %d left the artifact at %s, want %s: an unreachable source is not evidence about the artifact",
				cycle, got, lifecycle.Verifying)
		}
		cur := mustGetRow(t, journal, rec.Artifact)
		if cur.RetryCount != cycle {
			t.Fatalf("cycle %d: RetryCount = %d, want %d: the stall has to be counted or nothing bounds it", cycle, cur.RetryCount, cycle)
		}
		if cur.LocalHash != "" {
			t.Fatalf("cycle %d recorded a local hash on a row nothing compared: %q", cycle, cur.LocalHash)
		}
	}

	// The bytes really did land: this is a verification that could not be
	// completed, not a transfer that never happened.
	if tr.copyToLocalCalls() != 1 {
		t.Fatalf("CopyToLocal called %d times, want 1: the stall must not re-copy an artifact that is already on disk", tr.copyToLocalCalls())
	}

	// --- cycle 3: the budget is spent. The artifact is handed to an
	// operator rather than left in progress forever or stranded in FAILED,
	// which has no route back.
	if got := svc.processArtifact(ctx, source, bs, mustGetRow(t, journal, rec.Artifact)); got != lifecycle.Quarantined {
		t.Fatalf("cycle 3 left the artifact at %s, want %s", got, lifecycle.Quarantined)
	}

	held := mustGetRow(t, journal, rec.Artifact)
	if held.RetryCount != 3 {
		t.Fatalf("RetryCount = %d, want 3", held.RetryCount)
	}
	reason := lifecycle.QuarantineReason(held)
	if !strings.Contains(reason, "192.0.2.1") {
		t.Fatalf("QuarantineReason = %q, want it to name what actually stopped the check", reason)
	}
	for _, forbidden := range []string{"content check", "content found invalid"} {
		if strings.Contains(reason, forbidden) {
			t.Fatalf("QuarantineReason = %q, want it not to report %q, which nobody ran", reason, forbidden)
		}
	}

	// --- the recovery. This is the half FAILED could not offer: the
	// operator action exists, it is refused for nothing here, and it puts
	// the artifact back where the pipeline picks it up.
	if err := svc.RetryQuarantinedIngestion(ctx, rec.Artifact); err != nil {
		t.Fatalf("RetryQuarantinedIngestion: %v", err)
	}
	back := mustGetRow(t, journal, rec.Artifact)
	if back.State != string(lifecycle.Discovered) {
		t.Fatalf("after the retry the artifact is %s, want %s", back.State, lifecycle.Discovered)
	}

	// --- and the outage clears. The next cycle carries the artifact all
	// the way through, which is the proof that nothing along the stall
	// path left the row in a shape the pipeline cannot use.
	tr.remoteHashErr = nil
	if got := svc.processArtifact(ctx, source, bs, back); got != lifecycle.Complete {
		t.Fatalf("after the source came back the cycle left the artifact at %s, want %s", got, lifecycle.Complete)
	}
}

// TestVerificationStall_IsNotCountedAsAFailedArtifact keeps the cycle
// report honest. A stalled artifact has not failed: reporting it as one
// would tell an operator a backup went wrong when what went wrong is the
// network, and #361 is the issue about a cycle whose counts do not match
// what happened.
func TestVerificationStall_IsNotCountedAsAFailedArtifact(t *testing.T) {
	ctx := context.Background()
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.Validation = config.Validation{Hash: "sha256"}
	source := transport.Source{ID: "production"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload bytes", epoch.Unix())
	tr.remoteHashErr = unreachableSource()

	journal := openJournal(t)
	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RetryPolicy = retry.Policy{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 2, MaxAttempts: 4}

	walk := svc.processArtifacts(ctx, source, bs, []state.Record{rec})
	if walk.Failed != 0 {
		t.Fatalf("walk.Failed = %d, want 0: a check that could not be run is not a failed backup", walk.Failed)
	}
	if walk.Progress.Walked != 1 {
		t.Fatalf("walk.Progress.Walked = %d, want 1", walk.Progress.Walked)
	}
	if walk.Progress.Durable != 0 {
		t.Fatalf("walk.Progress.Durable = %d, want 0: nothing became durable", walk.Progress.Durable)
	}
}

// TestVerifyOne_DerivesItsStallBudgetFromTheOperatorsOwnRetryPolicy pins
// the derivation rather than the number. #419 asks for a bound that is
// derived and stated, so what has to hold is that there is exactly one
// place an operator changes it, not that it happens to be six today.
func TestVerifyOne_DerivesItsStallBudgetFromTheOperatorsOwnRetryPolicy(t *testing.T) {
	svc := &Service{}
	if got := svc.retryPolicy().MaxAttempts; got != DefaultRetryPolicy.MaxAttempts {
		t.Fatalf("an unconfigured service's budget = %d, want DefaultRetryPolicy's %d", got, DefaultRetryPolicy.MaxAttempts)
	}

	svc.RetryPolicy = retry.Policy{BaseDelay: time.Second, MaxDelay: time.Second, Multiplier: 2, MaxAttempts: 11}
	if got := svc.retryPolicy().MaxAttempts; got != 11 {
		t.Fatalf("a configured service's budget = %d, want the 11 the operator set", got)
	}
}

// TestVerificationStall_SurvivesACrashWithoutSpendingTwoAttempts is the
// idempotency half. A process that dies between the stall write landing
// and the call returning must not cost the artifact two of its attempts
// for one outage, because the whole point of the budget is that it counts
// outages rather than restarts.
func TestVerificationStall_SurvivesACrashWithoutSpendingTwoAttempts(t *testing.T) {
	ctx := context.Background()
	localDir := t.TempDir()
	bs := testBackupSet(t, localDir)
	bs.Validation = config.Validation{Hash: "sha256"}
	source := transport.Source{ID: "production"}

	tr := newFakeTransport()
	tr.put("backup.dump", "payload bytes", epoch.Unix())

	journal := openJournal(t)
	rec := discoverOneRecord(t, ctx, journal, tr, source, bs)

	svc := New(testConfig(t, testSource("production", bs)), journal, tr, nil)
	svc.Now = fixedNow(epoch)
	svc.RetryPolicy = retry.Policy{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 2, MaxAttempts: 5}
	tr.remoteHashErr = unreachableSource()

	// The first cycle records the stall.
	svc.processArtifact(ctx, source, bs, mustGetRow(t, journal, rec.Artifact))
	after := mustGetRow(t, journal, rec.Artifact)
	if after.RetryCount != 1 {
		t.Fatalf("RetryCount = %d after one cycle, want 1", after.RetryCount)
	}

	// Now replay THAT cycle: the same record snapshot, which is what a
	// process restarted before it could observe its own write is holding.
	out, err := lifecycle.Verify(ctx, svc.lifecycleDeps(), lifecycle.VerifyParams{
		Artifact:    rec.Artifact,
		Source:      source,
		Validation:  bs.Validation,
		AttemptKey:  attemptKey(rec) + ":verify",
		StallBudget: svc.retryPolicy().MaxAttempts,
	})
	var stall *lifecycle.VerificationStalledError
	if err == nil {
		t.Fatalf("the replay reported success: %+v", out)
	} else if !errors.As(err, &stall) {
		t.Fatalf("the replay returned %v, want a stall", err)
	}
	if replayed := mustGetRow(t, journal, rec.Artifact); replayed.RetryCount != 1 {
		t.Fatalf("RetryCount = %d after replaying one attempt, want 1: one outage must not cost two attempts", replayed.RetryCount)
	}
}
