// This file is the end-to-end evidence for issue #256, the same way
// tests/crashmatrix/watchdog_test.go is for #247 and #248: the arithmetic
// in tracker_test.go is proved against a synthetic clock so it costs the
// gate milliseconds, and this file proves that arithmetic is really wired
// to a real `go test` subprocess, under deliberately small bounds so it
// stays fast. Neither replaces the other: #247's own comment on this is
// "a fixed condition being made harder to reach is exactly the shape of
// change that can quietly stop detecting the thing it is about", so the
// property is demonstrated here, not just asserted by the unit tests.
//
// testdata/fixtures/hangpkg and testdata/fixtures/slowpkg are real,
// separate Go modules (go tool ignores anything under testdata/, so
// core's own build/vet/test/lint never see them) that this file compiles
// and runs for real via Run.
package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tightBounds keeps the tests below to seconds. Every ratio matches
// defaultBounds; only the floors are shrunk, because what these tests
// prove is that Run is really connected to a real go test subprocess, not
// what the production floors happen to be.
func tightBounds(floor time.Duration, factor float64) bounds {
	return bounds{
		stepFloor:     floor,
		stepFactor:    factor,
		overallFloor:  30 * time.Second,
		overallFactor: defaultBounds.overallFactor,
	}
}

func TestRun_CatchesAGenuineHang(t *testing.T) {
	const floor = 2 * time.Second
	dir := filepath.Join("testdata", "fixtures", "hangpkg")

	var stdout, stderr bytes.Buffer
	start := time.Now()
	res, err := Run(Options{
		Dir:    dir,
		Args:   []string{"-count=1", "./..."},
		Bounds: tightBounds(floor, defaultBounds.stepFactor),
		Poll:   50 * time.Millisecond,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run returned an error instead of a trip: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if res.Trip == nil {
		t.Fatalf("a package with a genuinely hung test was never caught\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	if res.Trip.kind != "no-progress" {
		t.Fatalf("trip.kind = %q, want no-progress: %v", res.Trip.kind, res.Trip)
	}
	if !strings.Contains(stdout.String(), "=== RUN   TestOne") || !strings.Contains(stdout.String(), "--- PASS: TestOne") {
		t.Fatalf("reconstructed output does not show TestOne actually having run and passed first, so the run's own pace was never measured:\n%s", stdout.String())
	}
	if len(res.Trip.running) != 1 || res.Trip.running[0] != "TestHang" {
		t.Fatalf("trip.running = %v, want exactly [TestHang]: the whole point of issue #256's legibility ask is naming which test was mid-flight", res.Trip.running)
	}
	if !strings.Contains(res.Trip.String(), "TestHang") {
		t.Fatalf("the trip's own failure text does not name TestHang:\n%v", res.Trip)
	}
	// Promptly, not eventually: this asserts on the watchdog's own
	// reaction time (sinceLast, close to the floor), not on wall-clock
	// time, so this test does not reintroduce the exact defect issue #256
	// is about by failing on a machine that is merely busy while it runs.
	// The multiplier is generous (not 2x or 3x) for the same reason: this
	// test's own poll goroutine can itself be descheduled under real host
	// contention (observed: 6.091s against a tighter 3x/6s bound on a
	// heavily loaded machine, a false failure from exactly the class of
	// defect this file exists to prove gotestwatch no longer has), and a
	// prompt-catch assertion that itself flakes under load is not a
	// prompt-catch assertion.
	if res.Trip.sinceLast > 10*floor {
		t.Fatalf("the watchdog took %s to notice a %s window had closed; that is not a prompt catch", res.Trip.sinceLast.Round(time.Millisecond), floor)
	}
	t.Logf("planted hang caught %s after Run started: %v", elapsed.Round(time.Millisecond), res.Trip)
}

func TestRun_DoesNotFailASlowButProgressingRun(t *testing.T) {
	// Mirrors testdata/fixtures/slowpkg/slow_test.go's own constants;
	// see that file's doc comment for why they cannot be imported here.
	const (
		floor       = 3 * time.Second
		firstStall  = 1 * time.Second
		secondStall = 6 * time.Second
	)
	dir := filepath.Join("testdata", "fixtures", "slowpkg")

	t.Run("derived window absorbs it", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		start := time.Now()
		res, err := Run(Options{
			Dir:    dir,
			Args:   []string{"-v", "-count=1", "./..."},
			Bounds: tightBounds(floor, defaultBounds.stepFactor),
			Poll:   50 * time.Millisecond,
			Stdout: &stdout,
			Stderr: &stderr,
		})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("Run returned an error: %v\nstderr=%s", err, stderr.String())
		}
		if res.Trip != nil {
			t.Fatalf("a slow but genuinely progressing run was failed as a hang: %v\nstdout=%s", res.Trip, stdout.String())
		}
		if res.ExitCode != 0 {
			t.Fatalf("go test exited %d, want 0 (both tests should pass)\nstdout=%s", res.ExitCode, stdout.String())
		}
		if !strings.Contains(stdout.String(), "--- PASS: TestA") || !strings.Contains(stdout.String(), "--- PASS: TestB") {
			t.Fatalf("reconstructed output does not show both tests passing:\n%s", stdout.String())
		}
		if elapsed < firstStall+secondStall {
			t.Fatalf("Run returned in %s, faster than the two real sleeps (%s) sum to; the fixture did not really run", elapsed, firstStall+secondStall)
		}
		t.Logf("a run whose slowest gap was %s (TestB, twice the %s floor) finished in %s and was not failed", secondStall, floor, elapsed.Round(time.Millisecond))
	})

	t.Run("without the derivation the same run fails", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		// stepFactor 1 is the bound with its derivation switched off: the
		// window can never grow past the slowest gap measured so far, so
		// it behaves like a fixed deadline again, which is the shape of
		// go test's own -timeout that issue #256 is about.
		res, err := Run(Options{
			Dir:    dir,
			Args:   []string{"-v", "-count=1", "./..."},
			Bounds: tightBounds(floor, 1),
			Poll:   50 * time.Millisecond,
			Stdout: &stdout,
			Stderr: &stderr,
		})
		if err != nil {
			t.Fatalf("Run returned an error: %v\nstderr=%s", err, stderr.String())
		}
		if res.Trip == nil {
			t.Fatalf("with the derivation off, the %s stall in TestB was still absorbed, so the passing subtest above proves nothing about the derivation itself\nstdout=%s", secondStall, stdout.String())
		}
		if len(res.Trip.running) != 1 || res.Trip.running[0] != "TestB" {
			t.Fatalf("trip.running = %v, want exactly [TestB]: %v", res.Trip.running, res.Trip)
		}
		t.Logf("with the derivation off, the identical run failed: %v", res.Trip)
	})
}

func TestRun_RefusesATimeoutOrJsonFlagFromTheCaller(t *testing.T) {
	for _, arg := range []string{"-timeout=30s", "-timeout", "-json", "--timeout=1m"} {
		t.Run(arg, func(t *testing.T) {
			_, err := Run(Options{Args: []string{arg, "./..."}, Bounds: defaultBounds})
			if err == nil {
				t.Fatalf("Run accepted %q, which gotestwatch owns itself; a caller-supplied -timeout would silently win the last-flag-wins race against gotestwatch's own -timeout=0 and reintroduce issue #256", arg)
			}
		})
	}
}
