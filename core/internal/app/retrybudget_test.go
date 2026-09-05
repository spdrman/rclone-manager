package app

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/internal/transport/retry"
)

// This file pins the one number issue #415 is about: how long a backup set
// pointed at a source that has gone dark holds a cycle before it reports
// FAILED.
//
// It is not a behaviour test, and that is deliberate. The behaviour costs
// exactly the budget to observe (two minutes of real dialling into a
// blackhole, six times over), which is not a price a gate can pay on every
// run, and paying it would prove nothing the arithmetic below does not.
// What CAN go wrong here is quieter than a broken behaviour: the budget is
// the product of two numbers that live in different packages and neither
// author can see the other, so either one can move and leave
// DefaultRetryPolicy's doc describing a bound the code stopped keeping.
// That is exactly what #388 did to it, and #396 and #405 are the same
// defect class one layer down. So the arithmetic is pinned, the model the
// arithmetic assumes is checked against the real retry.Do, and the
// constant carries the sentence it is keeping honest.

// unreachableSourceBudget is the worst case DefaultRetryPolicy's doc claims
// out loud: "a little over two minutes".
//
// It is named for the failure it bounds and not for the policy, because it
// does not bound the policy. It is what a source that NEVER ANSWERS costs,
// where every attempt ends at the connect deadline. A source that answers
// and then stalls partway through a read is bounded by rclone's --timeout
// instead, which this repository deliberately leaves at its five-minute
// default; transport/rclone's TestAStalledSourceIsBoundedByADifferentNumber
// is what keeps those two from collapsing into one, and DefaultRetryPolicy's
// doc says so under "What two minutes is NOT".
//
// Change either input and this test fails, which is the point. What to do
// when it does is NOT to move this constant until the failure message
// agrees with it; it is to decide whether the new number is still one an
// operator should be told to expect, and to rewrite DefaultRetryPolicy's
// doc if it is.
const unreachableSourceBudget = 2*time.Minute + time.Second

// TestUnreachableSourceBudgetIsPinned holds "six attempts, at most
// ConnectTimeout each, plus at most 31s of backoff" to a single number.
func TestUnreachableSourceBudgetIsPinned(t *testing.T) {
	p := DefaultRetryPolicy
	dialling := time.Duration(p.MaxAttempts) * rclone.ConnectTimeout
	backoff := worstCaseBackoff(p)
	got := dialling + backoff

	t.Logf("worst case: %d attempts x %s of dialling (%s) + %s of backoff = %s",
		p.MaxAttempts, rclone.ConnectTimeout, dialling, backoff, got)

	if got != unreachableSourceBudget {
		t.Fatalf("one backup set now holds a cycle for up to %s against a source that blackholes, "+
			"not the %s DefaultRetryPolicy's doc claims.\n"+
			"  %d attempts (DefaultRetryPolicy.MaxAttempts) x %s (transport/rclone.ConnectTimeout) = %s of dialling\n"+
			"  plus %s of backoff (the caps of %s x %v, ceiling %s)\n"+
			"Whichever of those two numbers moved, DefaultRetryPolicy's doc and this constant have to move with it, "+
			"and the question to answer first is whether the new number is one an operator should be told to expect.",
			got, unreachableSourceBudget, p.MaxAttempts, rclone.ConnectTimeout, dialling,
			backoff, p.BaseDelay, p.Multiplier, p.MaxDelay)
	}
}

// TestUnreachableSourceBudgetPinCanStillFail is the companion guard the timing
// bounds elsewhere in this repository keep (see
// transport/rclone's TestKeyCommandTimeoutBudget_CanStillFail and
// lifecycle's hookReturnBudget): a pin that cannot be violated is not a
// pin, it is a comment that runs.
func TestUnreachableSourceBudgetPinCanStillFail(t *testing.T) {
	p := DefaultRetryPolicy

	// The pin has to be sensitive to BOTH inputs, or half of #415 could
	// come back without anything going red. A budget that ignored the
	// attempts would sit green through rclone's own 60s default, which is
	// the six-and-a-half-minute number #415 was filed about.
	if worstCaseBackoff(p)+time.Duration(p.MaxAttempts)*(rclone.ConnectTimeout+time.Second) == unreachableSourceBudget {
		t.Error("the pin does not move when ConnectTimeout does, so it is not pinning the dialling half at all")
	}
	if worstCaseBackoff(p)+time.Duration(p.MaxAttempts+1)*rclone.ConnectTimeout == unreachableSourceBudget {
		t.Error("the pin does not move when MaxAttempts does, so it is not pinning the attempt count at all")
	}

	// And it has to be strictly better than the state #415 describes, or
	// it would have passed before the bound existed. rclone's own default
	// --contimeout is 60s.
	const rcloneDefaultConnectTimeout = 60 * time.Second
	before := worstCaseBackoff(p) + time.Duration(p.MaxAttempts)*rcloneDefaultConnectTimeout
	if unreachableSourceBudget >= before {
		t.Errorf("unreachableSourceBudget is %s, at or past the %s six attempts cost at rclone's own %s default; "+
			"at this value the pin sits green through exactly the defect it is named for",
			unreachableSourceBudget, before, rcloneDefaultConnectTimeout)
	}
	t.Logf("bounded: %s, was %s at rclone's own %s default", unreachableSourceBudget, before, rcloneDefaultConnectTimeout)

	// A budget under the dialling alone could never be met by a correct
	// run, which is the opposite failure and just as useless.
	if unreachableSourceBudget <= time.Duration(p.MaxAttempts)*rclone.ConnectTimeout {
		t.Errorf("unreachableSourceBudget is %s, at or below the %s the attempts alone cost, so no correct run fits inside it",
			unreachableSourceBudget, time.Duration(p.MaxAttempts)*rclone.ConnectTimeout)
	}
}

// TestRetryBudgetArithmeticMatchesRetryDo checks the MODEL, which is the
// part a pure arithmetic pin cannot check on its own: "attempts x
// per-attempt cost, plus the backoff caps" is only the budget if retry.Do
// really calls op MaxAttempts times and really waits no longer than each
// cap between them.
//
// It runs on a scaled-down policy so it costs milliseconds rather than the
// two minutes the real one describes. The shape is what is being checked,
// not the production numbers, which the test above pins instead.
func TestRetryBudgetArithmeticMatchesRetryDo(t *testing.T) {
	const perAttempt = 25 * time.Millisecond
	scaled := retry.Policy{
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    40 * time.Millisecond,
		Multiplier:  2,
		MaxAttempts: 4,
	}

	transient := transport.NewError(transport.Transient, "dial", errors.New("blackholed"))
	attempts := 0
	start := time.Now()
	err := retry.Do(context.Background(), scaled, nil, func(context.Context) error {
		attempts++
		time.Sleep(perAttempt)
		return transient
	})
	elapsed := time.Since(start)

	if !errors.Is(err, transient) {
		t.Fatalf("retry.Do returned %v, want the transient error it was given back", err)
	}
	if attempts != scaled.MaxAttempts {
		t.Fatalf("op was called %d times, want MaxAttempts (%d); the budget's first factor is the attempt count, "+
			"so a loop that calls op a different number of times makes the pinned arithmetic describe something else",
			attempts, scaled.MaxAttempts)
	}

	worst := time.Duration(scaled.MaxAttempts)*perAttempt + worstCaseBackoff(scaled)
	floor := time.Duration(scaled.MaxAttempts) * perAttempt
	t.Logf("%d attempts of %s plus backoff: took %s, model says between %s and %s",
		attempts, perAttempt, elapsed, floor, worst)
	if elapsed < floor {
		t.Fatalf("retry.Do returned in %s, less than the %s the attempts alone cost; "+
			"the model the budget is computed from does not describe this loop", elapsed, floor)
	}
	// The upper half is a bound on the SLEEPING, and a loaded host can add
	// scheduling on top of it, so it is allowed a generous multiple rather
	// than being asserted tight. What it still catches is a loop whose
	// waiting is not bounded by the caps at all.
	if ceiling := 4 * worst; elapsed > ceiling {
		t.Fatalf("retry.Do took %s, past %s (4x the %s the caps allow); the backoff half of the budget "+
			"is not bounded by this policy's caps", elapsed, ceiling, worst)
	}
}

// worstCaseBackoff is the most time retry.Do can spend WAITING under p,
// which is the sum of the caps of the gaps between attempts.
//
// It reads the caps off the policy's exported fields rather than calling
// into retry's own unexported delay(), on purpose: this is a second,
// independent statement of the schedule, and a copy that agreed with the
// original by construction would not be one.
//
// The jitter is why these are caps and not values: retry.Do uses full
// jitter, a uniform draw from [0, cap], so the cap is reached only in the
// limit and never exceeded. A budget has to be built from the ceiling.
func worstCaseBackoff(p retry.Policy) time.Duration {
	var total time.Duration
	for gap := 1; gap < p.MaxAttempts; gap++ {
		cap := float64(p.BaseDelay) * math.Pow(p.Multiplier, float64(gap-1))
		if cap > float64(p.MaxDelay) {
			cap = float64(p.MaxDelay)
		}
		total += time.Duration(cap)
	}
	return total
}
