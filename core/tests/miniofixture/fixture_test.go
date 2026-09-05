// These tests are issue #456's evidence.
//
// This fixture already drew the right distinction and then fell out of the
// wrong side of it. A daemon that is present but WEDGED was a failure, with
// a comment saying that skipping would silently remove this suite from the
// gate. A daemon that is NOT REACHABLE, three lines below that comment, was
// a skip. A Docker VM that dies in the middle of a gate run is not
// reachable rather than wedged, so it took the skip: in one stored gate log
// 13 of the 14 conformance mutation cells printed `ok ... 0.08s` against a
// dead daemon and the run went on being green.
//
// The skip is still right on a laptop with no docker, so both directions
// are asserted here. Under the gate an unreachable daemon has to be a
// failure carrying the INFRA: marker, and off the gate it still has to be a
// skip. A test that only checked the first would let the second regress
// silently, which is how this fixture got here.
//
// The daemon is never actually stopped to make any of this true. Several
// worktrees on this machine share one docker daemon, so stopping it would
// take everybody else's run down as well; DOCKER_HOST is pointed at an
// endpoint nothing listens on instead, and the real docker client gives the
// real answer. requireTheDaemonIsUnreachable checks that premise rather
// than assuming it, because a DOCKER_HOST that was quietly ignored would
// send the child at the real daemon and every assertion below would pass
// for the wrong reason.
package miniofixture

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

// helperEnv guards the helper tests below so they only ever run in a child
// process this file started, never as part of a normal `go test ./...`.
const helperEnv = "MINIOFIXTURE_HELPER"

// deadDockerHost is an endpoint nothing listens on. `docker info` against
// it fails in milliseconds with the daemon's real unreachable message.
const deadDockerHost = "tcp://127.0.0.1:1"

// startReturnedMarker is printed by a helper if Start ever comes back. It
// cannot, on any path these tests drive, and a fixture that shrugged and
// carried on would otherwise look like an ordinary failure.
const startReturnedMarker = "START_RETURNED"

// infraMarker is asserted as a literal rather than through the constant in
// fixture.go on purpose. The marker is a contract with whoever reads a gate
// log, so the test has to fail if the production side changes it.
const infraMarker = "INFRA:"

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

// refusalVerdict reports whether a child run REFUSED, meaning it failed and
// said so, rather than skipping or quietly passing. It is a value the test
// can check both ways, because a skip exits 0 exactly like a pass and the
// whole point here is telling those two apart.
func refusalVerdict(out string, code int) error {
	if strings.Contains(out, "--- SKIP") {
		return errors.New("the run SKIPPED, which is the failure mode #456 is about: the suite quietly leaves the gate and the gate still says ok")
	}
	if code == 0 {
		return fmt.Errorf("the run exited 0, so it did not refuse")
	}
	if !strings.Contains(out, "--- FAIL") {
		return fmt.Errorf("the run exited %d but never reported a test failure, so what stopped it is not the fixture refusing", code)
	}
	if !strings.Contains(out, infraMarker) {
		return fmt.Errorf("the run failed but never said %q, so a reader sorting a gate log into a broken machine and a broken product cannot tell which this was", infraMarker)
	}
	return nil
}

// skipVerdict is refusalVerdict's opposite: the run left the suite out, out
// loud, and did not fail.
func skipVerdict(out string, code int) error {
	if code != 0 {
		return fmt.Errorf("the run exited %d rather than skipping, so a developer machine with no docker now has a red test", code)
	}
	if !strings.Contains(out, "--- SKIP") {
		return errors.New("the run exited 0 without skipping anything, so the fixture did not report the missing capability at all")
	}
	if strings.Contains(out, infraMarker) {
		return fmt.Errorf("the run skipped but still printed %q, which reads in a log as an infrastructure failure that did not happen", infraMarker)
	}
	return nil
}

// requireDockerBinary guards the two tests that need the real docker client
// in order to be pointed at a dead endpoint. It goes through the fixture's
// own verdict, so inside the gate a missing docker binary is the same
// infrastructure failure here as it is anywhere else in this package.
func requireDockerBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("miniofixture: SKIPPING (missing capability: %q not found on PATH, so there is no client to point at a dead endpoint): %v", "docker", err)
	}
}

// requireTheDaemonIsUnreachable checks the premise the simulation rests on.
// A docker context can name an endpoint of its own, and if DOCKER_HOST were
// ignored the child process would reach the REAL daemon, start a real MinIO
// container and fail for a completely unrelated reason, which every
// assertion below would happily read as a refusal.
func requireTheDaemonIsUnreachable(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "info")
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+deadDockerHost)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("`docker info` SUCCEEDED with DOCKER_HOST=%s, so this machine's docker ignores it and nothing below simulates an unreachable daemon at all:\n%s", deadDockerHost, out)
	}
}

// emptyPATH is a directory with nothing in it, which is how the laptop
// case ("docker is not installed here") is reproduced without uninstalling
// anything.
func emptyPATH(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// --- an unreachable daemon --------------------------------------------------

// TestStartRefusesAnUnreachableDaemonInsideTheGate is #456 itself. The gate
// declares docker a prerequisite, so a daemon it cannot reach means the
// gate's own machine broke, and the only honest answer is a failure that
// says so.
func TestStartRefusesAnUnreachableDaemonInsideTheGate(t *testing.T) {
	requireDockerBinary(t)
	requireTheDaemonIsUnreachable(t)

	// The positive control, on the checker rather than on the subject. A
	// skipping fixture is the exact thing this test exists to reject, so
	// the verdict is first shown rejecting a run that skipped. Without it,
	// a verdict that called everything a refusal would pass below and
	// prove nothing.
	skipOut, skipCode := runHelper(t, "TestHelperSkipsInstead")
	if err := refusalVerdict(skipOut, skipCode); err == nil {
		t.Fatalf("refusalVerdict ACCEPTED a run that skipped in exactly the shape #456 is about, so it cannot tell a refusal from a silent skip and its verdict below would mean nothing.\ncontrol helper output:\n%s", skipOut)
	}

	out, code := runHelper(t, "TestHelperStartAgainstAnUnreachableDaemon",
		"CI_LOCAL=1", "CI_LOCAL_SKIP_DOCKER=", "DOCKER_HOST="+deadDockerHost)

	if strings.Contains(out, startReturnedMarker) {
		t.Fatalf("Start RETURNED against a daemon it cannot reach, so the suite would carry on against nothing.\nhelper output:\n%s", out)
	}
	if err := refusalVerdict(out, code); err != nil {
		t.Fatalf("the fixture did not refuse an unreachable daemon under CI_LOCAL=1: %v.\nThis is #456: a Docker VM that dies mid-run is 'not reachable', the fixture skips, and the gate goes on printing ok with the MinIO suite silently empty.\nhelper output:\n%s", err, out)
	}
}

// TestStartStillSkipsAnUnreachableDaemonOutsideTheGate is the other half,
// and the half a fix for the first one breaks. Off the gate this fixture is
// evidence, not a requirement on every machine, so no docker still means no
// test rather than a red one.
func TestStartStillSkipsAnUnreachableDaemonOutsideTheGate(t *testing.T) {
	requireDockerBinary(t)
	requireTheDaemonIsUnreachable(t)

	out, code := runHelper(t, "TestHelperStartAgainstAnUnreachableDaemon",
		"CI_LOCAL=", "DOCKER_HOST="+deadDockerHost)

	if err := skipVerdict(out, code); err != nil {
		t.Fatalf("with CI_LOCAL unset, an unreachable daemon no longer skips: %v.\nEvery developer without a running docker would now have a red package, which is why the skip is conditional and not deleted.\nhelper output:\n%s", err, out)
	}
}

// TestTheGatesOwnOptOutStillSkips keeps the gate's documented escape hatch
// honest. CI_LOCAL_SKIP_DOCKER=1 is scripts/ci-local.sh's out-loud opt-out
// for a run with the daemon down, and it already ledgers such a run as
// INCOMPLETE, so this fixture honours it rather than overruling it. Without
// this, that flag would silently stop working the moment CI_LOCAL landed.
func TestTheGatesOwnOptOutStillSkips(t *testing.T) {
	requireDockerBinary(t)
	requireTheDaemonIsUnreachable(t)

	out, code := runHelper(t, "TestHelperStartAgainstAnUnreachableDaemon",
		"CI_LOCAL=1", "CI_LOCAL_SKIP_DOCKER=1", "DOCKER_HOST="+deadDockerHost)

	if err := skipVerdict(out, code); err != nil {
		t.Fatalf("CI_LOCAL_SKIP_DOCKER=1 no longer gets past this fixture: %v.\nThat flag is documented as proceeding with the daemon down and ending the run INCOMPLETE, so overruling it here makes the documentation a lie.\nhelper output:\n%s", err, out)
	}
}

// --- no docker at all -------------------------------------------------------

// TestStartRefusesAMissingDockerBinaryInsideTheGate is the same question
// asked of the other path into "docker is not available". A gate machine
// with no docker on PATH is as broken as one whose daemon died, and the two
// have to answer the same way or the hole just moves.
func TestStartRefusesAMissingDockerBinaryInsideTheGate(t *testing.T) {
	out, code := runHelper(t, "TestHelperStartAgainstAnUnreachableDaemon",
		"CI_LOCAL=1", "CI_LOCAL_SKIP_DOCKER=", "PATH="+emptyPATH(t))

	if strings.Contains(out, startReturnedMarker) {
		t.Fatalf("Start RETURNED with no docker on PATH at all.\nhelper output:\n%s", out)
	}
	if err := refusalVerdict(out, code); err != nil {
		t.Fatalf("the fixture did not refuse a missing docker binary under CI_LOCAL=1: %v.\nhelper output:\n%s", err, out)
	}
}

// TestStartStillSkipsAMissingDockerBinaryOutsideTheGate is the laptop case
// in its purest form: docker is genuinely not installed, and that is not
// this repository's problem to be red about.
func TestStartStillSkipsAMissingDockerBinaryOutsideTheGate(t *testing.T) {
	out, code := runHelper(t, "TestHelperStartAgainstAnUnreachableDaemon",
		"CI_LOCAL=", "PATH="+emptyPATH(t))

	if err := skipVerdict(out, code); err != nil {
		t.Fatalf("with CI_LOCAL unset, a machine with no docker installed no longer skips: %v.\nhelper output:\n%s", err, out)
	}
}

// --- helpers, run only in a child process -----------------------------------

// TestHelperStartAgainstAnUnreachableDaemon drives the real entry point.
// Which of the two unavailable-docker paths it takes is decided by the
// environment its parent gave it.
func TestHelperStartAgainstAnUnreachableDaemon(t *testing.T) {
	skipUnlessHelper(t)
	Start(t)
	fmt.Println(startReturnedMarker)
	t.Fatal("Start returned against a docker that is not available, which should be impossible")
}

// TestHelperSkipsInstead is the obliging fixture this repository must not
// ship, written down so the verdict above can be shown rejecting it. It is
// the positive control and nothing in the fixture calls it.
func TestHelperSkipsInstead(t *testing.T) {
	skipUnlessHelper(t)
	t.Skipf("miniofixture: SKIPPING (missing capability: docker daemon not reachable)")
}
