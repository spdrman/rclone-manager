// Package hangpkg is gotestwatch's own test fixture: a real `go test`
// package (in its own module, under testdata/ so core's build/vet/test/
// lint never touch it) containing one genuinely hung test. run_test.go
// runs `go test` against this package for real, under gotestwatch's own
// watchdog, and asserts the hang is actually caught rather than assuming
// the tracker arithmetic proved in tracker_test.go is wired up correctly.
package hangpkg

import (
	"testing"
	"time"
)

// TestOne passes almost immediately, so the run has at least one measured
// gap (and therefore a derived, non-floor window) before TestHang starts,
// exactly like a real suite's early fast tests.
func TestOne(t *testing.T) {}

// TestHang blocks for an hour: a stand-in for a stuck network call or a
// goroutine leak, the two hangs #247's own harnessTimeout comment named.
// Nothing in this package, and nothing `go test` does with -timeout=0,
// ever ends it before that; gotestwatch's own no-progress watchdog is
// meant to be the only thing that does, well before the hour is up.
//
// This is deliberately time.Sleep and not `select {}`: with only one
// goroutine running, `select {}` is a Go runtime deadlock the runtime
// itself detects and crashes on immediately (this file tried that first,
// and TestRun_CatchesAGenuineHang caught its own fixture being wrong
// rather than gotestwatch being right, exactly per issue #256's "must be
// shown, not asserted"). A goroutine that is genuinely sleeping is not
// considered deadlocked by the runtime, which is a fair proxy for the
// process actually being stuck in a real blocking call.
func TestHang(t *testing.T) {
	time.Sleep(time.Hour)
}
