package retry

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file proves FR-22's bounded backoff without ever waiting out a real
// one, which is the whole reason the retry loop was split out of the
// adapter in the first place.
//
// The backoff math is tested by calling delay directly with a seeded
// *rand.Rand, so the "never exceeds MaxDelay" claim is checked over many
// samples in microseconds rather than inferred from a handful of real
// sleeps. That is why delay takes the source as a parameter at all: it is
// the one piece of this package whose correctness is statistical, and a
// statistical property asserted from three observations is not asserted.
//
// The loop itself is tested with a policy whose delays are sub-millisecond
// (fastPolicy), so a case about how many attempts happen is not also a
// case about how long they take. The two cancellation cases are the
// exception and they are timed on purpose: FR-22 says a caller who cancels
// gets control back promptly, and "promptly" is only a claim about the
// clock.
//
// Errors here are built with transport.NewError rather than mocked,
// because the classification they carry is exactly what
// DefaultIsTransient reads, and a fake that carried something else would
// be testing this file's idea of the vocabulary instead of the one in
// errors.go.

// transientErr builds an error DefaultIsTransient (and so Do, by default)
// treats as retryable.
func transientErr(msg string) error {
	return transport.NewError(transport.Transient, "test-op", errors.New(msg))
}

// permanentErr builds an error DefaultIsTransient treats as not retryable.
func permanentErr(msg string) error {
	return transport.NewError(transport.Permanent, "test-op", errors.New(msg))
}

// ---------------------------------------------------------------------------
// delay(): the backoff math itself, proved without sleeping through it.
// ---------------------------------------------------------------------------

// TestPolicyDelay_NeverExceedsMaxDelay is the "actually bounded" proof FR-22
// asks for: across many attempts and many random draws, the jittered delay
// must never exceed MaxDelay, even though the underlying exponential curve
// (BaseDelay * Multiplier^attempt) blows straight past it after a handful of
// attempts.
func TestPolicyDelay_NeverExceedsMaxDelay(t *testing.T) {
	policy := Policy{
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   200 * time.Millisecond,
		Multiplier: 2,
	}
	rnd := rand.New(rand.NewSource(1))

	for attempt := 1; attempt <= 40; attempt++ {
		for sample := 0; sample < 50; sample++ {
			d := policy.delay(attempt, rnd)
			if d < 0 {
				t.Fatalf("delay(%d) = %v, want >= 0", attempt, d)
			}
			if d > policy.MaxDelay {
				t.Fatalf("delay(%d) = %v, want <= MaxDelay %v", attempt, d, policy.MaxDelay)
			}
		}
	}
}

// TestPolicyDelay_GrowsWithAttemptBeforeHittingTheCap proves the "exponential"
// half separately from the "bounded" half: early attempts, where the
// exponential curve is still well under MaxDelay, should produce
// meaningfully larger delays as attempt increases. It compares the maximum
// observed delay over many samples (jitter makes any single sample noisy) at
// a low attempt count against a high one.
func TestPolicyDelay_GrowsWithAttemptBeforeHittingTheCap(t *testing.T) {
	policy := Policy{
		BaseDelay:  1 * time.Millisecond,
		MaxDelay:   time.Hour, // effectively no cap for this test
		Multiplier: 2,
	}
	rnd := rand.New(rand.NewSource(2))

	maxAt := func(attempt int) time.Duration {
		var max time.Duration
		for i := 0; i < 30; i++ {
			if d := policy.delay(attempt, rnd); d > max {
				max = d
			}
		}
		return max
	}

	early := maxAt(1)
	later := maxAt(8)
	if later <= early {
		t.Fatalf("max delay at attempt 8 (%v) should exceed max delay at attempt 1 (%v)", later, early)
	}
}

// TestPolicyDelay_ZeroAttemptTreatedAsFirst guards against a caller passing
// an off-by-one attempt number and silently getting an unbounded or negative
// delay out of it.
func TestPolicyDelay_ZeroAttemptTreatedAsFirst(t *testing.T) {
	policy := Policy{BaseDelay: 10 * time.Millisecond, MaxDelay: time.Second, Multiplier: 2}
	rnd := rand.New(rand.NewSource(3))
	d0 := policy.delay(0, rnd)
	d1 := policy.delay(1, rnd)
	if d0 > policy.BaseDelay || d1 > policy.BaseDelay {
		t.Fatalf("delay(0)=%v delay(1)=%v, want both <= BaseDelay %v (first-attempt delay is unjittered upper bound BaseDelay)", d0, d1, policy.BaseDelay)
	}
}

// ---------------------------------------------------------------------------
// Do(): the retry loop, including cancellation.
// ---------------------------------------------------------------------------

// fastPolicy makes the loop's behaviour observable without its timing
// getting in the way: the delays are sub-millisecond, so a case about how
// many attempts happen finishes immediately and is not also a case about
// how long they take. TestDo_CancelledContextStopsPromptly deliberately
// does not use it and builds a policy with a full second of backoff
// instead, because what that case asserts IS the clock: returning fast
// under fastPolicy would prove nothing about waking on ctx.Done().
func fastPolicy() Policy {
	return Policy{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, Multiplier: 2}
}

func TestDo_SucceedsWithoutRetryingOnFirstSuccess(t *testing.T) {
	calls := 0
	err := Do(context.Background(), fastPolicy(), nil, func(context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("Do returned %v, want nil", err)
	}
	if calls != 1 {
		t.Fatalf("op called %d times, want 1", calls)
	}
}

// TestDo_RetriesTransientUntilSuccess is the FR-22 happy path: a Transient
// failure gets retried, and a subsequent success is returned as success, not
// buried under the earlier failures.
func TestDo_RetriesTransientUntilSuccess(t *testing.T) {
	calls := 0
	err := Do(context.Background(), fastPolicy(), nil, func(context.Context) error {
		calls++
		if calls < 3 {
			return transientErr("not yet")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do returned %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want 3", calls)
	}
}

// TestDo_StopsImmediatelyOnNonTransient proves a non-Transient category is
// never retried at all, regardless of the policy's attempt budget: retrying
// a NotFound or PermissionDenied is not "trying harder", it's the same wrong
// answer arriving slower.
func TestDo_StopsImmediatelyOnNonTransient(t *testing.T) {
	calls := 0
	want := permanentErr("nope")
	err := Do(context.Background(), fastPolicy(), nil, func(context.Context) error {
		calls++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Do returned %v, want %v", err, want)
	}
	if calls != 1 {
		t.Fatalf("op called %d times, want 1 (a non-transient error must not be retried)", calls)
	}
}

// TestDo_RespectsMaxAttempts proves the attempt budget is an actual ceiling:
// a permanently-Transient-looking failure (say, a flaky remote that never
// recovers within this job) must not retry forever.
func TestDo_RespectsMaxAttempts(t *testing.T) {
	calls := 0
	policy := fastPolicy()
	policy.MaxAttempts = 3
	err := Do(context.Background(), policy, nil, func(context.Context) error {
		calls++
		return transientErr("still failing")
	})
	if err == nil {
		t.Fatal("Do returned nil, want the last Transient error after exhausting MaxAttempts")
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want exactly MaxAttempts (3)", calls)
	}
	if !DefaultIsTransient(err) {
		t.Errorf("Do's returned error after budget exhaustion should still classify as Transient (the cause didn't change, only the decision to keep waiting did), got %v", err)
	}
}

// TestDo_AlreadyCancelledContext proves ctx is checked before op is ever
// called at all: a caller that cancelled before starting should not pay for
// even one attempt.
func TestDo_AlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	err := Do(ctx, fastPolicy(), nil, func(context.Context) error {
		calls++
		return nil
	})
	if calls != 0 {
		t.Fatalf("op called %d times against an already-cancelled context, want 0", calls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if cat, ok := transport.CategoryOf(err); !ok || cat != transport.Cancelled {
		t.Fatalf("transport.CategoryOf(err) = (%v, %v), want (Cancelled, true)", cat, ok)
	}
}

// TestDo_CancelledContextStopsPromptly is the other half of FR-22's
// cancellation requirement: "a cancelled context stops the retry loop
// promptly rather than sleeping out the full schedule". The policy here
// backs off for a full second between attempts; the test cancels shortly
// after the first failure and asserts Do returns in a small fraction of that
// second, proving it woke up on ctx.Done() rather than on the backoff timer.
//
// It deliberately does not assert an exact call count. Full jitter draws
// uniformly from [0, cap], so it can legitimately draw a near-zero delay on
// the very first backoff and let a second (or, vanishingly rarely, a third)
// attempt start before the 20ms cancellation fires; that is jitter working
// as designed, not the loop failing to stop promptly, and asserting calls==1
// here made the test flake on a slower CI runner. The elapsed-time bound
// below is the real proof either way: however many attempts land in that
// window, none of them waited out the 1s backoff step.
func TestDo_CancelledContextStopsPromptly(t *testing.T) {
	const wouldHaveSlept = 1 * time.Second
	policy := Policy{BaseDelay: wouldHaveSlept, MaxDelay: wouldHaveSlept, Multiplier: 2}

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	start := time.Now()
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	err := Do(ctx, policy, nil, func(context.Context) error {
		calls++
		return transientErr("keeps failing, would normally wait ~1s before trying again")
	})
	elapsed := time.Since(start)

	if elapsed >= wouldHaveSlept/2 {
		t.Fatalf("Do took %v to return after cancellation, want well under the %v backoff step; it slept out the schedule instead of stopping promptly", elapsed, wouldHaveSlept)
	}
	if calls < 1 {
		t.Fatalf("op called %d times, want at least 1", calls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if cat, ok := transport.CategoryOf(err); !ok || cat != transport.Cancelled {
		t.Fatalf("transport.CategoryOf(err) = (%v, %v), want (Cancelled, true)", cat, ok)
	}
}

// TestDo_CustomIsTransient proves a caller may supply its own retry
// predicate instead of transport.Category, for callers that are not
// classifying rclone errors at all.
func TestDo_CustomIsTransient(t *testing.T) {
	sentinel := errors.New("custom retryable condition")
	calls := 0
	err := Do(context.Background(), fastPolicy(), func(err error) bool {
		return errors.Is(err, sentinel)
	}, func(context.Context) error {
		calls++
		if calls < 2 {
			return sentinel
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do returned %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("op called %d times, want 2", calls)
	}
}
