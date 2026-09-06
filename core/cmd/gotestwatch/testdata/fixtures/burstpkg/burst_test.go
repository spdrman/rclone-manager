// Package burstpkg is gotestwatch's own test fixture for issue #533: a
// real `go test` package (in its own module, under testdata/ so core's
// build/vet/test/lint never touch it) that reports progress the whole way
// through, gets stalled once by something outside itself, recovers, and
// then keeps going for longer than the overall cap allows.
//
// Nothing in it is stuck. It is the shape a healthy suite takes on a
// machine another project is competing for: a quiet stretch that sets a
// fast pace, a burst that stalls it, and a recovery back to the same pace.
// That last part is what makes the fixture worth having. By the time the
// cap closes, the watchdog's rolling memory of recent gaps has rolled the
// stall out entirely, so the cap it is killed against is the same one a
// perfectly smooth run would have had, and the run's own record that it
// was stalled at all is nowhere in it.
//
// run_test.go runs this for real and asserts the kill still happens and
// the report no longer calls it a livelock.
package burstpkg

import (
	"fmt"
	"testing"
	"time"
)

// The three numbers this fixture is made of, duplicated as constants of
// the same name in run_test.go (this package is a separate module under
// testdata/, so it cannot be imported from there). Keep them in sync by
// hand, the same way slowpkg's own stalls are.
const (
	// burstPace is how often a line is printed while nothing is
	// competing for the machine. It is the pace both derived bounds end
	// up being multiples of.
	burstPace = 20 * time.Millisecond
	// burstQuietLines is how many lines the opening quiet stretch
	// prints. More than the watchdog's rolling memory holds, so by the
	// time the stall arrives the bounds are made entirely of this pace
	// and not of `go test`'s own startup.
	burstQuietLines = 30
	// burstStall is the one stall: another project taking the CPU. Well
	// under the no-progress window the pace above derives, so it is
	// absorbed rather than tripping anything, which is exactly what
	// makes it invisible to the cap later.
	burstStall = 800 * time.Millisecond
)

// TestBurst never returns, and that is the point rather than a hang: it is
// standing in for a suite with more work left than the overall cap allows,
// so the cap is what ends it. Every iteration prints, so no no-progress
// window ever closes on it and the run is genuinely making progress at the
// moment it is killed.
func TestBurst(t *testing.T) {
	for i := 0; i < burstQuietLines; i++ {
		fmt.Printf("burstpkg: quiet line %d\n", i)
		time.Sleep(burstPace)
	}
	fmt.Printf("burstpkg: stalling for %s\n", burstStall)
	time.Sleep(burstStall)
	for i := 0; ; i++ {
		fmt.Printf("burstpkg: recovered line %d\n", i)
		time.Sleep(burstPace)
	}
}
