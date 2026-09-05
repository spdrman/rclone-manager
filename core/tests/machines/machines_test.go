// These tests are issue #456's evidence for the machine tier.
//
// The three fixtures underneath this package all failed a WEDGED daemon,
// with a comment saying that skipping would silently remove the suite from
// the gate, and then skipped an UNREACHABLE one three lines below that
// comment. A Docker VM that dies in the middle of a gate run is unreachable
// rather than wedged, so it took the skip: in one stored gate log 13 of the
// 14 conformance mutation cells printed `ok ... 0.08s` against a dead
// daemon and the run stayed green.
//
// Start had the same hole, copied along with the comment, and it is the one
// that mattered longest: this package is the single entry point every new
// machine-tier test is supposed to use, so fixing only the fixtures would
// have left the hole open on the path everything is moving towards. With
// the daemon unreachable it took TestReadOnlyBackupSet_RealSFTPFixture out
// of tests/sftpintegration and the package still reported ok.
//
// The skip is still right on a laptop with no docker, so both directions
// are asserted. A test that only checked the new branch would let the
// laptop case regress silently, which is the mirror image of how this got
// here in the first place.
//
// Nothing stops the real daemon to make any of this true. Several worktrees
// on this machine share one, so stopping it would take everybody else's run
// down; DOCKER_HOST is pointed at an endpoint nothing listens on and the
// real docker client gives the real answer.
package machines

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// helperEnv guards the helper below so it only ever runs in a child process
// one of these tests started, never as part of a normal `go test ./...`.
const helperEnv = "MACHINES_HELPER"

// deadDockerHost is an endpoint nothing listens on. `docker info` against
// it fails in milliseconds with the daemon's own unreachable message.
const deadDockerHost = "tcp://127.0.0.1:1"

// startReturnedMarker is printed if Start ever comes back on a path where
// it cannot. A harness that shrugged and carried on would otherwise look
// like an ordinary failure somewhere further down.
const startReturnedMarker = "START_RETURNED"

// wantInfraMarker is the marker a refusal has to carry, written out as its
// own literal rather than read from machines.go's constant on purpose. The
// marker is a contract with whoever reads a gate log, so renaming it on the
// production side has to turn this red rather than follow along quietly.
const wantInfraMarker = "INFRA:"

func skipUnlessHelper(t *testing.T) {
	t.Helper()
	if os.Getenv(helperEnv) == "" {
		t.Skip("helper process only; driven by the tests in this file")
	}
}

// runHelper re-executes this test binary for one helper test with env laid
// over this process's own, and reports what the child printed and how it
// exited. Go's exec keeps the LAST of duplicated keys, so a caller can both
// set and clear a variable the parent already has.
func runHelper(t *testing.T, name string, env ...string) (out string, code int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+name+"$", "-test.v=true", "-test.timeout=90s")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.Env = append(cmd.Env, env...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	out = buf.String()
	if ctx.Err() != nil {
		t.Fatalf("the helper %s never finished within the window, so it neither refused nor skipped and there is nothing to read:\n%s", name, out)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out, exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("running the helper %s: %v\n%s", name, err, out)
	}
	return out, 0
}

// refusalVerdict reports whether a child run REFUSED, meaning it failed,
// said so and named itself infrastructure, rather than skipping or quietly
// passing. It is a value the tests can check both ways, because a skip
// exits 0 exactly like a pass and telling those two apart is the whole
// point.
func refusalVerdict(out string, code int) error {
	if strings.Contains(out, "--- SKIP") {
		return errors.New("the run SKIPPED, which is the failure mode #456 is about: the machine tier quietly leaves the gate and the gate still says ok")
	}
	if code == 0 {
		return errors.New("the run exited 0, so it did not refuse")
	}
	if !strings.Contains(out, "--- FAIL") {
		return fmt.Errorf("the run exited %d but never reported a test failure, so what stopped it is not Start refusing", code)
	}
	if !strings.Contains(out, wantInfraMarker) {
		return fmt.Errorf("the run failed but never said %q, so nothing in the log says this was the machine rather than the product", wantInfraMarker)
	}
	return nil
}

// skipVerdict is refusalVerdict's opposite: the run left the tier out, out
// loud, and did not fail.
func skipVerdict(out string, code int) error {
	if code != 0 {
		return fmt.Errorf("the run exited %d rather than skipping, so a developer machine with no docker now has a red package", code)
	}
	if !strings.Contains(out, "--- SKIP") {
		return errors.New("the run exited 0 without skipping anything, so Start never reported the missing capability at all")
	}
	if strings.Contains(out, wantInfraMarker) {
		return fmt.Errorf("the run skipped but still printed %q, which reads in a log as an infrastructure failure that did not happen", wantInfraMarker)
	}
	return nil
}

// requireDockerBinary guards the tests that need the real docker client in
// order to point it at a dead endpoint. It goes through this package's own
// verdict, so inside the gate a missing docker binary is the same
// infrastructure failure here as it is anywhere else.
func requireDockerBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		dockerUnavailable(t, "%q not found on PATH, so there is no client to point at a dead endpoint: %v", "docker", err)
	}
}

// requireTheDaemonIsUnreachable checks the premise the simulation rests on.
// A docker context can name an endpoint of its own, and if DOCKER_HOST were
// ignored the child would reach the REAL daemon, build the source image,
// create a network and fail somewhere else entirely, which the assertions
// below would happily read as a refusal.
func requireTheDaemonIsUnreachable(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "info")
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+deadDockerHost)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("`docker info` SUCCEEDED with DOCKER_HOST=%s, so this machine's docker ignores it and nothing below simulates an unreachable daemon at all:\n%s", deadDockerHost, out)
	}
}

// TestStartRefusesAnUnreachableDaemonInsideTheGate is #456 on the path
// everything is moving towards. Every new machine-tier test comes through
// Start, so one skip here empties the tier while the gate prints ok.
func TestStartRefusesAnUnreachableDaemonInsideTheGate(t *testing.T) {
	requireDockerBinary(t)
	requireTheDaemonIsUnreachable(t)

	// The positive control, and it is the same helper against the same
	// dead endpoint with only CI_LOCAL removed. That controls two things
	// at once: the verdict really can tell a skip from a refusal, and the
	// environment is the only thing deciding which one happens.
	skipOut, skipCode := runHelper(t, "TestHelperStartAgainstAnUnavailableDocker",
		"CI_LOCAL=", "DOCKER_HOST="+deadDockerHost)
	if err := refusalVerdict(skipOut, skipCode); err == nil {
		t.Fatalf("refusalVerdict ACCEPTED a run that skipped in exactly the shape #456 is about, so it cannot tell a refusal from a silent skip and its verdict below would mean nothing.\ncontrol helper output:\n%s", skipOut)
	}

	out, code := runHelper(t, "TestHelperStartAgainstAnUnavailableDocker",
		"CI_LOCAL=1", "CI_LOCAL_SKIP_DOCKER=", "DOCKER_HOST="+deadDockerHost)
	if strings.Contains(out, startReturnedMarker) {
		t.Fatalf("Start RETURNED against a daemon it cannot reach, so the test would carry on against no machines at all.\nhelper output:\n%s", out)
	}
	if err := refusalVerdict(out, code); err != nil {
		t.Fatalf("Start did not refuse an unreachable daemon under CI_LOCAL=1: %v.\nThis is #456: a Docker VM that dies mid-run is 'not reachable', Start skips, and the gate goes on printing ok with the machine tier silently empty.\nhelper output:\n%s", err, out)
	}
}

// TestStartStillSkipsAnUnreachableDaemonOutsideTheGate is the half a fix
// for the one above breaks if the skip is deleted rather than made
// conditional.
func TestStartStillSkipsAnUnreachableDaemonOutsideTheGate(t *testing.T) {
	requireDockerBinary(t)
	requireTheDaemonIsUnreachable(t)

	out, code := runHelper(t, "TestHelperStartAgainstAnUnavailableDocker",
		"CI_LOCAL=", "DOCKER_HOST="+deadDockerHost)
	if err := skipVerdict(out, code); err != nil {
		t.Fatalf("with CI_LOCAL unset, an unreachable daemon no longer skips: %v.\nEvery developer without a running docker would now have a red package, which is why the skip is conditional and not gone.\nhelper output:\n%s", err, out)
	}
}

// TestStartRefusesAMissingDockerBinaryInsideTheGate asks the same question
// of the other path into "docker is not available". A gate machine with no
// docker on PATH is as broken as one whose daemon died, and if the two
// answered differently the hole would just move.
func TestStartRefusesAMissingDockerBinaryInsideTheGate(t *testing.T) {
	out, code := runHelper(t, "TestHelperStartAgainstAnUnavailableDocker",
		"CI_LOCAL=1", "CI_LOCAL_SKIP_DOCKER=", "PATH="+t.TempDir())
	if strings.Contains(out, startReturnedMarker) {
		t.Fatalf("Start RETURNED with no docker on PATH at all.\nhelper output:\n%s", out)
	}
	if err := refusalVerdict(out, code); err != nil {
		t.Fatalf("Start did not refuse a missing docker binary under CI_LOCAL=1: %v.\nhelper output:\n%s", err, out)
	}
}

// TestStartStillSkipsAMissingDockerBinaryOutsideTheGate is the laptop case
// in its purest form: docker is genuinely not installed, and that is not
// this repository's problem to be red about.
func TestStartStillSkipsAMissingDockerBinaryOutsideTheGate(t *testing.T) {
	out, code := runHelper(t, "TestHelperStartAgainstAnUnavailableDocker",
		"CI_LOCAL=", "PATH="+t.TempDir())
	if err := skipVerdict(out, code); err != nil {
		t.Fatalf("with CI_LOCAL unset, a machine with no docker installed no longer skips: %v.\nhelper output:\n%s", err, out)
	}
}

// TestTheGatesOwnOptOutStillSkips keeps the gate's documented escape hatch
// honest. CI_LOCAL_SKIP_DOCKER=1 is scripts/ci-local.sh's out-loud opt-out
// for a run with the daemon down and it already ledgers that run as
// INCOMPLETE, so this package honours it rather than overruling it. Without
// this, the flag would quietly stop working the moment CI_LOCAL landed.
func TestTheGatesOwnOptOutStillSkips(t *testing.T) {
	requireDockerBinary(t)
	requireTheDaemonIsUnreachable(t)

	out, code := runHelper(t, "TestHelperStartAgainstAnUnavailableDocker",
		"CI_LOCAL=1", "CI_LOCAL_SKIP_DOCKER=1", "DOCKER_HOST="+deadDockerHost)
	if err := skipVerdict(out, code); err != nil {
		t.Fatalf("CI_LOCAL_SKIP_DOCKER=1 no longer gets past Start: %v.\nThat flag is documented as proceeding with the daemon down and ending the run INCOMPLETE, so overruling it here would make the documentation a lie.\nhelper output:\n%s", err, out)
	}
}

// TestHelperStartAgainstAnUnavailableDocker drives the real entry point.
// Which of the two unavailable-docker paths it takes is decided by the
// environment its parent gave it.
func TestHelperStartAgainstAnUnavailableDocker(t *testing.T) {
	skipUnlessHelper(t)
	Start(t)
	fmt.Println(startReturnedMarker)
	t.Fatal("Start returned against a docker that is not available, which should be impossible")
}
