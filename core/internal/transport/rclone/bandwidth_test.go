package rclone

import (
	"context"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
)

// rclone's --bwlimit is process-global: one token bucket, one package-level
// accounting.TokenBucket, shared by every transfer in the binary. Three
// tests in this package and one in tests/sftpintegration turn it down so a
// transfer lasts long enough to observe, so every one of them has to put it
// back, or the next test in the file order runs at whatever limit the last
// one left behind.
//
// # None of them did
//
// Every one of them "restored" it by calling StartTokenBucket again with an
// unlimited config, and StartTokenBucket cannot clear a limit. Its whole
// body, in rclone v1.75.0, is
//
//	tb.currLimit = ci.BwLimit.LimitAt(time.Now())
//	if tb.currLimit.Bandwidth.IsSet() {
//	    tb.curr = newTokenBucket(tb.currLimit.Bandwidth)
//	    ...
//	}
//
// so an unlimited config takes the `if` and leaves tb.curr exactly as it
// was. Setting a limit works; unsetting one is a no-op.
//
// Found while rewriting gate_test.go's MidTransferCancellation (#414). That
// row used to set a limit of its own, which DID replace the bucket, so it
// had been quietly undoing errors_test.go's leak for everything that ran
// after it. Taking the limit out of that row left errors_test.go's 64KiB/s
// standing over TestPhase1Gate's own copies, and a 16MiB payload at
// 64KiB/s is four minutes, which is how this surfaced: as the fixture
// watchdog killing the gate for a hang that was really a leaked throttle.
//
// SetBwLimit is the one that clears, because it has the else branch
// StartTokenBucket does not.

// throttleBandwidth turns rclone's process-global bandwidth limit down to
// limit for the duration of t, and clears it again afterwards. limit is an
// rclone bandwidth string, and it wants its unit: rclone reads a bare
// "65536" as 64Mi rather than 64Ki.
func throttleBandwidth(t *testing.T, limit string) context.Context {
	t.Helper()
	ctx, ci := fs.AddConfig(context.Background())
	if err := (&ci.BwLimit).Set(limit); err != nil {
		t.Fatalf("setting --bwlimit to %q: %v", limit, err)
	}
	accounting.TokenBucket.StartTokenBucket(ctx)
	t.Cleanup(clearBandwidthLimit)
	return ctx
}

// clearBandwidthLimit removes any process-global bandwidth limit.
func clearBandwidthLimit() {
	accounting.TokenBucket.SetBwLimit(fs.BwPair{})
}

// The numbers TestClearingTheBandwidthLimitReallyClearsIt measures against.
//
// rclone's newEmptyTokenBucket drains the bucket the moment it makes it, so
// the first WaitN after a limit is set waits for the tokens to be minted:
// exactly grant/limit, decided by a rate rather than by the scheduler. That
// is what makes a timing assertion here a measurement rather than a bet.
const (
	// bwProofLimit and bwProofGrant are one second of waiting apart.
	bwProofLimit = "1Mi"
	bwProofGrant = 1024 * 1024

	// bwProofThrottled is the floor a throttled wait has to clear. Well
	// under the second the arithmetic says, because a machine cannot make
	// this wait SHORTER, only longer.
	bwProofThrottled = 500 * time.Millisecond

	// bwProofCleared is the ceiling a cleared one has to stay under. With
	// no bucket at all LimitBandwidth returns without waiting for anything,
	// so this is entirely an allowance for a busy scheduler, and it is a
	// quarter of the floor above so the two can never meet.
	bwProofCleared = 125 * time.Millisecond
)

// TestClearingTheBandwidthLimitReallyClearsIt is the evidence for the
// paragraph above, and it is deliberately three assertions rather than one:
// that the limit really throttles (or the rest proves nothing), that
// clearing it really clears it, and that the idiom this package used before
// really did not.
func TestClearingTheBandwidthLimitReallyClearsIt(t *testing.T) {
	t.Cleanup(clearBandwidthLimit)

	throttled, ci := fs.AddConfig(context.Background())
	if err := (&ci.BwLimit).Set(bwProofLimit); err != nil {
		t.Fatalf("setting --bwlimit: %v", err)
	}
	accounting.TokenBucket.StartTokenBucket(throttled)

	// Positive control.
	if waited := timeOneGrant(); waited < bwProofThrottled {
		t.Fatalf("a %d-byte grant under a %s/s limit came back in %s, under the %s floor; "+
			"the limit is not throttling at all, so nothing below is evidence about clearing one",
			int64(bwProofGrant), bwProofLimit, waited, bwProofThrottled)
	}

	// The idiom this package used to use, which is the defect.
	unlimited, _ := fs.AddConfig(context.Background())
	accounting.TokenBucket.StartTokenBucket(unlimited)
	if waited := timeOneGrant(); waited < bwProofThrottled {
		t.Errorf("StartTokenBucket with an unlimited config cleared the limit (a grant came back in %s). "+
			"rclone v1.75.0 could not do that, which is the whole reason throttleBandwidth exists; "+
			"if this is a newer rclone that fixed it, clearBandwidthLimit can go and so can this row", waited)
	}

	// And the fix.
	clearBandwidthLimit()
	if waited := timeOneGrant(); waited > bwProofCleared {
		t.Fatalf("a %d-byte grant still took %s after the limit was cleared, past the %s ceiling; "+
			"the limit outlives the test that set it, and every test after it runs throttled",
			int64(bwProofGrant), waited, bwProofCleared)
	}
}

// TestBandwidthProofBoundsCanStillFail guards the two bounds above, since
// bounds that overlap would make the row green whatever the limiter did.
func TestBandwidthProofBoundsCanStillFail(t *testing.T) {
	if bwProofCleared >= bwProofThrottled {
		t.Errorf("the cleared ceiling (%s) is at or above the throttled floor (%s), so one wait satisfies both "+
			"and the row cannot tell a cleared limiter from a throttling one", bwProofCleared, bwProofThrottled)
	}
	// The floor has to be reachable: a grant that the limit would satisfy
	// instantly could never clear it.
	var limit fs.SizeSuffix
	if err := limit.Set(bwProofLimit); err != nil {
		t.Fatalf("parsing %q: %v", bwProofLimit, err)
	}
	implied := time.Duration(float64(bwProofGrant) / float64(limit) * float64(time.Second))
	if implied <= bwProofThrottled {
		t.Errorf("a %d-byte grant at %s/s implies a %s wait, at or under the %s floor; the positive control "+
			"would fail on a correct limiter", int64(bwProofGrant), bwProofLimit, implied, bwProofThrottled)
	}
}

func timeOneGrant() time.Duration {
	start := time.Now()
	accounting.TokenBucket.LimitBandwidth(accounting.TokenBucketSlotAccounting, bwProofGrant)
	return time.Since(start)
}
