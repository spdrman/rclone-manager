// Package retry is the FR-22 bounded-backoff half of error classification:
// "Transient errors SHALL use bounded exponential backoff with jitter" and
// "Cancellation SHALL propagate through Go contexts".
//
// It knows nothing about rclone. It only knows transport.Category, so it can
// retry any transport.Transient-classified failure, whoever produced it.
// Splitting it out of transport/rclone (rather than making it one more
// method on the adapter) is deliberate: the backoff math and the
// cancellation behaviour are worth testing on their own, with a Policy small
// enough to run in milliseconds, instead of only ever being exercised
// indirectly through a real rclone call.
//
// This package does not make a retried operation idempotent. Failure-safety
// invariant 9 ("Retries must be idempotent") is the caller's obligation: Do
// will call op again exactly as asked, as many times as the policy and the
// context allow, and has no way to know or enforce that doing so is safe.
package retry

import (
	"context"
	"math"
	"math/rand"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Policy bounds one Transient retry loop.
type Policy struct {
	// BaseDelay is the (unjittered) delay before the second attempt.
	// Defaults to 500ms if zero or negative.
	BaseDelay time.Duration
	// MaxDelay is the ceiling no computed delay may exceed, before jitter is
	// applied. Defaults to 30s if zero or negative. This is what makes the
	// backoff bounded rather than merely exponential: attempt count alone
	// would otherwise grow the delay without limit.
	MaxDelay time.Duration
	// Multiplier is the growth factor applied per attempt. Defaults to 2 if
	// less than or equal to 1 (a multiplier that doesn't grow the delay
	// isn't "exponential backoff", it's a fixed retry interval).
	Multiplier float64
	// MaxAttempts is the total number of attempts, including the first.
	// Zero means unbounded: Do keeps retrying Transient failures until ctx
	// is done. A negative value behaves like 1 (no retries).
	MaxAttempts int
}

// DefaultPolicy is a reasonable bounded-backoff schedule for a manual
// backup/restore transfer: quick enough that a blip on the second attempt
// resolves in under a second, capped low enough that an operator watching a
// stuck job never wonders whether it is hung versus waiting out a step of
// the schedule.
func DefaultPolicy() Policy {
	return Policy{
		BaseDelay:   500 * time.Millisecond,
		MaxDelay:    30 * time.Second,
		Multiplier:  2,
		MaxAttempts: 0,
	}
}

// withDefaults fills in whatever a caller left at zero, and returns a COPY
// rather than mutating the receiver, so a Policy value a caller keeps and
// reuses never quietly acquires this function's opinions.
//
// It exists because a zero Policy is a perfectly reasonable thing to write
// (retry.Do(ctx, retry.Policy{}, nil, op) reads as "retry this the usual
// way"), and every field's zero value is a value that would be wrong to
// take literally: a zero BaseDelay is a busy loop, a zero MaxDelay is
// unbounded growth with a ceiling of nothing, and a Multiplier of 0 or 1
// is not backoff at all. MaxAttempts is the one field deliberately NOT
// defaulted, because zero there has a real meaning of its own, "keep
// going until ctx says stop", which DefaultPolicy chooses on purpose.
func (p Policy) withDefaults() Policy {
	if p.BaseDelay <= 0 {
		p.BaseDelay = 500 * time.Millisecond
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = 30 * time.Second
	}
	if p.Multiplier <= 1 {
		p.Multiplier = 2
	}
	return p
}

// delay returns the bounded, jittered wait before the attempt numbered
// attempt+1 (attempt is 1 for the wait before the second overall attempt).
// It uses "full jitter" (a uniformly random duration in [0, cap]), the
// scheme AWS's backoff-and-jitter writeup recommends specifically to avoid a
// thundering herd of retries all waking on the same clock tick: unlike
// "add a little jitter to the exponential value", full jitter means the
// bound this function promises (the result never exceeds MaxDelay) holds
// exactly, and rnd is accepted as a parameter so a test can assert that
// bound over many samples without sleeping through the real schedule.
func (p Policy) delay(attempt int, rnd *rand.Rand) time.Duration {
	p = p.withDefaults()
	if attempt < 1 {
		attempt = 1
	}
	exp := float64(p.BaseDelay) * math.Pow(p.Multiplier, float64(attempt-1))
	capped := math.Min(exp, float64(p.MaxDelay))
	if capped <= 0 {
		return 0
	}
	// int64(capped) is safe: capped is bounded above by p.MaxDelay, itself a
	// time.Duration (an int64) that already survived withDefaults.
	return time.Duration(rnd.Int63n(int64(capped) + 1))
}

// IsTransient reports whether err is worth retrying at all.
//
// It is a parameter rather than a fixed rule so a caller can narrow the
// set, never widen it: a test that wants exactly one retry, or a call site
// that knows its own operation is not safe to repeat under some particular
// Transient failure, can say so without this package growing an opinion
// about it. Nothing in this repository passes anything but nil (which
// means DefaultIsTransient) outside tests, and that is the intended
// shape: the classification decision belongs to the adapter that produced
// the error, and this is just the reader.
type IsTransient func(err error) bool

// DefaultIsTransient treats err as retryable exactly when it carries
// transport.Transient, by way of transport.CategoryOf. A caller retrying
// calls through transport/rclone needs no classifier of its own: WrapCtx
// already attached the category, and this just reads it back off.
func DefaultIsTransient(err error) bool {
	category, _ := transport.CategoryOf(err)
	return category == transport.Transient
}

// Do calls op, retrying it under policy for as long as the error it returns
// satisfies isTransient (DefaultIsTransient if isTransient is nil), up to
// whichever comes first of: op succeeding, op returning a non-transient
// error, policy's MaxAttempts being reached, or ctx being done.
//
// The context.Context ctx is FR-22's cancellation path. Do checks it before
// every attempt and, while waiting out a backoff, races the wait against
// ctx.Done() instead of sleeping the wait unconditionally: a caller that
// cancels ctx gets control back as soon as Go's scheduler runs the select,
// not after however much of the current backoff step was left. On that path
// Do returns a *transport.Error carrying transport.Cancelled and ctx.Err()
// as its cause (still reachable via errors.Is(err, context.Canceled) or
// errors.Is(err, context.DeadlineExceeded) through Unwrap), rather than the
// last Transient error op happened to return, since the reason the loop
// stopped was the cancellation, not that failure.
//
// op is called exactly as given, as many times as this decides to call it:
// making that safe to repeat is the caller's job (failure-safety invariant
// 9), not this function's.
func Do(ctx context.Context, policy Policy, isTransient IsTransient, op func(ctx context.Context) error) error {
	if isTransient == nil {
		isTransient = DefaultIsTransient
	}
	policy = policy.withDefaults()
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))

	var lastErr error
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return transport.NewError(transport.Cancelled, "retry", err)
		}

		lastErr = op(ctx)
		if lastErr == nil {
			return nil
		}
		if !isTransient(lastErr) {
			return lastErr
		}
		if policy.MaxAttempts > 0 && attempt >= policy.MaxAttempts {
			return lastErr
		}

		timer := time.NewTimer(policy.delay(attempt, rnd))
		select {
		case <-ctx.Done():
			timer.Stop()
			return transport.NewError(transport.Cancelled, "retry", ctx.Err())
		case <-timer.C:
		}
	}
}
