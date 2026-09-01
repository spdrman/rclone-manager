// Package slowpkg is gotestwatch's own test fixture for the "slow but
// genuinely progressing" side of the proof (run_test.go's
// TestRun_DoesNotFailASlowButProgressingRun): a real `go test` package (in
// its own module, under testdata/ so core's build/vet/test/lint never
// touch it) with two tests run in order. TestA is what the run measures
// its own pace by; TestB is slower than a naive fixed bound but well
// inside the window TestA's own pace derives, so it must survive only
// because of the derivation, not because either number was picked large
// enough to never matter.
package slowpkg

import (
	"testing"
	"time"
)

// firstStall and secondStall are duplicated as constants of the same name
// in run_test.go (this package's own separate module, under testdata/,
// cannot be imported from there) — keep the two in sync by hand.
const (
	firstStall  = 1 * time.Second
	secondStall = 6 * time.Second
)

func TestA(t *testing.T) { time.Sleep(firstStall) }
func TestB(t *testing.T) { time.Sleep(secondStall) }
