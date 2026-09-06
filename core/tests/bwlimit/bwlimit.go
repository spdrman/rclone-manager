// Package bwlimit sets and clears rclone's process-global bandwidth limit
// for the duration of a test.
//
// It exists because that limit is a single package-level token bucket
// shared by every transfer in the binary, several tests in this repository
// turn it down so a transfer lasts long enough to observe, and both of the
// obvious ways to do that are wrong. Issue #414 is what they cost.
//
// # The limit does not say what it looks like it says
//
// rclone's bandwidth strings default to KiB, so a bare 65536 is 64Mi and
// not the 64Ki it reads as. That is not a rounding error, it is a factor of
// 1024, and it is silent: the throttle simply never engages and whatever
// the test was timing turns out to have been timing the network. Two tests
// were built with fmt.Sprintf("%d", ...) and neither had ever throttled
// anything. Throttle refuses a limit with no unit for that reason, and
// TestABareNumberIsKibibytes pins the factor rather than describing it.
//
// # Setting a limit works, clearing one does not
//
// Every caller here used to put the limit back by calling StartTokenBucket
// again with an unlimited config, which cannot clear a limit. Its whole
// body, in rclone v1.75.0, is
//
//	tb.currLimit = ci.BwLimit.LimitAt(time.Now())
//	if tb.currLimit.Bandwidth.IsSet() {
//	    tb.curr = newTokenBucket(tb.currLimit.Bandwidth)
//	    ...
//	}
//
// so an unlimited config takes the `if` and leaves tb.curr exactly as it
// was. SetBwLimit is the one that clears, because it has the else branch
// StartTokenBucket does not.
//
// A leaked limit is not a quiet failure either. It surfaces as the next
// test in file order taking minutes for no visible reason, which reads as a
// hang: a 16MiB payload under a leaked 64KiB/s limit is four minutes, and
// what reports it is the fixture watchdog killing the run.
package bwlimit

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
)

// Throttle turns rclone's process-global bandwidth limit down to limit for
// the duration of t, and clears it again afterwards.
//
// limit is an rclone bandwidth string and it must carry a unit: see
// CheckUnit. The returned context is parent with the matching ConfigInfo
// added, which callers that want their own operations to run under the same
// config can build on; parent is taken rather than assumed because a caller
// with a fixture context has a deadline and a cleanup on it that a
// background context would silently drop.
func Throttle(t testing.TB, parent context.Context, limit string) context.Context {
	t.Helper()
	if err := CheckUnit(limit); err != nil {
		t.Fatalf("bwlimit.Throttle: %v", err)
	}
	ctx, ci := fs.AddConfig(parent)
	if err := (&ci.BwLimit).Set(limit); err != nil {
		t.Fatalf("bwlimit.Throttle: setting --bwlimit to %q: %v", limit, err)
	}
	accounting.TokenBucket.StartTokenBucket(ctx)
	t.Cleanup(Clear)
	return ctx
}

// Clear removes any process-global bandwidth limit.
//
// Safe to call when none is set, and safe to call twice, which matters
// because a test that lifts the limit part way through still wants the
// Cleanup that guarantees it.
func Clear() {
	accounting.TokenBucket.SetBwLimit(fs.BwPair{})
}

// CheckUnit refuses a bandwidth string that carries no unit.
//
// It is a separate, exported function rather than an inline check inside
// Throttle so that it can be tested directly: a guard whose only caller
// fails the test process cannot be shown to fire.
//
// The rule is deliberately narrow. It rejects exactly the shape the defect
// had, an unsuffixed decimal number, and leaves every other string to
// rclone's own parser, which has the timetable syntax and the error
// messages for it. "off" is the one unsuffixed spelling that is not a
// quantity, so it passes through.
func CheckUnit(limit string) error {
	s := strings.TrimSpace(limit)
	if s == "" || strings.EqualFold(s, "off") {
		return nil
	}
	if strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return nil
	}
	return fmt.Errorf(
		"bandwidth limit %q has no unit, and rclone reads a bare number as KiB, so this asks for %s and not the %s bytes per second it reads as. "+
			"Write the unit you mean: %sKi for kibibytes per second, %sB for bytes per second",
		limit, kibibytes(s), s, s, s)
}

// kibibytes renders what rclone will actually do with a bare number, so the
// message above states the mistake in rclone's own spelling rather than
// leaving the reader to multiply.
func kibibytes(digits string) string {
	var parsed fs.SizeSuffix
	if err := parsed.Set(digits); err != nil {
		return digits + "Ki"
	}
	return parsed.String()
}
