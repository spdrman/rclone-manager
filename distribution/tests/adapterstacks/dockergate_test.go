// This file is issue #456 in the distribution module: the verdict on a
// docker that is not available, and the tests that pin it in both
// directions.
//
// The four packages under core/tests had the same hole and now share this
// exact shape. A daemon that is present but wedged was a failure there,
// with a comment saying that skipping would silently remove the suite from
// the gate, and a daemon that was not reachable was a skip three lines
// later. A Docker VM that dies in the middle of a gate run is not reachable
// rather than wedged, so it took the skip: in one stored gate log 13 of the
// 14 conformance mutation cells printed `ok ... 0.08s` against a dead
// daemon and the run stayed green. This suite's own `requireDocker` said it
// more briefly and did the same thing, and it is a fifth Docker-backed
// suite the gate can lose without noticing.
//
// The skip is still right on a laptop with no docker, so both directions
// are asserted below. A test that only checked the new branch would let the
// laptop case regress silently.
//
// Nothing stops the real daemon to make any of this true. Several worktrees
// on this machine share one, so stopping it would take everybody else's run
// down; DOCKER_HOST is pointed at an endpoint nothing listens on and the
// real docker client gives the real answer.
package adapterstacks_test

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

// infraMarker is the fixed, greppable string every infrastructure refusal
// carries. It is the same literal in core's sftpfixture, miniofixture,
// dockerlease and machines on purpose: a gate log sorts into "the machine
// broke" and "the product broke" with one grep, and a marker that varied by
// package would not.
//
// This is a local copy rather than an import. distribution is a separate
// module from core, and #81's dependency rule is that core must build and
// pass with the distribution tree deleted, so a shared constant would have
// to live in core and be imported the wrong way down the layers for a test
// helper's convenience. Ten lines of duplication is the cheaper of the two.
const infraMarker = "INFRA:"

// dockerUnavailable ends the calling test for a docker that is not there to
// be used, and decides whether that is a skip or a failure.
//
// On a machine that simply has no docker, skipping is honest: this suite is
// evidence for the gate, not a requirement on every developer's laptop.
// Inside the gate it is the opposite. Docker is a declared prerequisite
// there, so the same condition means the gate's own machine is broken, and
// a skip quietly deletes this suite from the run while the run goes on
// printing ok.
//
// So under the gate this is a failure carrying infraMarker. The one way
// past it is CI_LOCAL_SKIP_DOCKER=1, the gate's own documented opt-out for
// a run with the daemon down, which already ledgers that run as INCOMPLETE.
func dockerUnavailable(t *testing.T, reason string, args ...any) {
	t.Helper()
	detail := fmt.Sprintf(reason, args...)
	if gateRequiresDocker() {
		t.Fatalf("%s adapterstacks: %s\nDocker is a declared prerequisite of this gate (CI_LOCAL=1), so this is an INFRASTRUCTURE failure and not a product one: the machine could not offer a docker daemon. Skipping here would take the adapter stack suite out of the run while the gate still printed ok, which is #456.", infraMarker, detail)
	}
	t.Skipf("adapterstacks: SKIPPING (missing capability: %s)", detail)
}

// gateRequiresDocker reports whether this process is inside the local gate,
// which declares docker a prerequisite. scripts/ci-local.sh exports
// CI_LOCAL=1. CI_LOCAL_SKIP_DOCKER=1 is that same gate's documented opt-out
// for a run with the daemon down, and it already ends the run INCOMPLETE,
// so it is honoured here rather than overruled: refusing anyway would make
// that flag a lie.
func gateRequiresDocker() bool {
	return os.Getenv("CI_LOCAL") == "1" && os.Getenv("CI_LOCAL_SKIP_DOCKER") != "1"
}

// --- the tests ------------------------------------------------------------

// helperEnv guards the helper below so it only ever runs in a child process
// one of these tests started, never as part of a normal `go test ./...`.
const helperEnv = "ADAPTERSTACKS_HELPER"

// deadDockerHost is an endpoint nothing listens on. `docker info` against
// it fails in milliseconds with the daemon's own unreachable message.
const deadDockerHost = "tcp://127.0.0.1:1"

// requireReturnedMarker is printed if requireDocker ever waves a child
// through on a path where it cannot.
const requireReturnedMarker = "REQUIRE_RETURNED"

// wantInfraMarker is the marker a refusal has to carry, written out as its
// own literal rather than read from the constant above on purpose. The
// marker is a contract with whoever reads a gate log, so renaming it has to
// turn this red rather than follow along quietly.
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
		return errors.New("the run SKIPPED, which is the failure mode #456 is about: the suite quietly leaves the gate and the gate still says ok")
	}
	if code == 0 {
		return errors.New("the run exited 0, so it did not refuse")
	}
	if !strings.Contains(out, "--- FAIL") {
		return fmt.Errorf("the run exited %d but never reported a test failure, so what stopped it is not requireDocker refusing", code)
	}
	if !strings.Contains(out, wantInfraMarker) {
		return fmt.Errorf("the run failed but never said %q, so nothing in the log says this was the machine rather than the product", wantInfraMarker)
	}
	return nil
}

// skipVerdict is refusalVerdict's opposite: the run left the suite out, out
// loud, and did not fail.
func skipVerdict(out string, code int) error {
	if code != 0 {
		return fmt.Errorf("the run exited %d rather than skipping, so a developer machine with no docker now has a red package", code)
	}
	if !strings.Contains(out, "--- SKIP") {
		return errors.New("the run exited 0 without skipping anything, so requireDocker never reported the missing capability at all")
	}
	if strings.Contains(out, wantInfraMarker) {
		return fmt.Errorf("the run skipped but still printed %q, which reads in a log as an infrastructure failure that did not happen", wantInfraMarker)
	}
	return nil
}

// requireDockerBinary guards the tests that need the real docker client in
// order to point it at a dead endpoint. It goes through this suite's own
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
// ignored the child would reach the REAL daemon, sail through requireDocker
// and go on to build a container image, which the assertions below would
// read as something else entirely.
func requireTheDaemonIsUnreachable(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "info")
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+deadDockerHost)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("`docker info` SUCCEEDED with DOCKER_HOST=%s, so this machine's docker ignores it and nothing below simulates an unreachable daemon at all:\n%s", deadDockerHost, out)
	}
}

func TestRequireDockerRefusesAnUnreachableDaemonInsideTheGate(t *testing.T) {
	requireDockerBinary(t)
	requireTheDaemonIsUnreachable(t)

	// The positive control, and it is the same helper against the same
	// dead endpoint with only CI_LOCAL removed. That controls two things
	// at once: the verdict really can tell a skip from a refusal, and the
	// environment is the only thing deciding which one happens.
	skipOut, skipCode := runHelper(t, "TestHelperRequireDockerAgainstAnUnavailableDocker",
		"CI_LOCAL=", "DOCKER_HOST="+deadDockerHost)
	if err := refusalVerdict(skipOut, skipCode); err == nil {
		t.Fatalf("refusalVerdict ACCEPTED a run that skipped in exactly the shape #456 is about, so it cannot tell a refusal from a silent skip and its verdict below would mean nothing.\ncontrol helper output:\n%s", skipOut)
	}

	out, code := runHelper(t, "TestHelperRequireDockerAgainstAnUnavailableDocker",
		"CI_LOCAL=1", "CI_LOCAL_SKIP_DOCKER=", "DOCKER_HOST="+deadDockerHost)
	if strings.Contains(out, requireReturnedMarker) {
		t.Fatalf("requireDocker waved the suite through against a daemon it cannot reach.\nhelper output:\n%s", out)
	}
	if err := refusalVerdict(out, code); err != nil {
		t.Fatalf("requireDocker did not refuse an unreachable daemon under CI_LOCAL=1: %v.\nThis is #456: a Docker VM that dies mid-run is 'not reachable', the suite skips, and the gate goes on printing ok with the adapter stack proof silently empty.\nhelper output:\n%s", err, out)
	}
}

func TestRequireDockerStillSkipsAnUnreachableDaemonOutsideTheGate(t *testing.T) {
	requireDockerBinary(t)
	requireTheDaemonIsUnreachable(t)

	out, code := runHelper(t, "TestHelperRequireDockerAgainstAnUnavailableDocker",
		"CI_LOCAL=", "DOCKER_HOST="+deadDockerHost)
	if err := skipVerdict(out, code); err != nil {
		t.Fatalf("with CI_LOCAL unset, an unreachable daemon no longer skips: %v.\nEvery developer without a running docker would now have a red package, which is why the skip is conditional and not gone.\nhelper output:\n%s", err, out)
	}
}

func TestRequireDockerRefusesAMissingDockerBinaryInsideTheGate(t *testing.T) {
	out, code := runHelper(t, "TestHelperRequireDockerAgainstAnUnavailableDocker",
		"CI_LOCAL=1", "CI_LOCAL_SKIP_DOCKER=", "PATH="+t.TempDir())
	if strings.Contains(out, requireReturnedMarker) {
		t.Fatalf("requireDocker waved the suite through with no docker on PATH at all.\nhelper output:\n%s", out)
	}
	if err := refusalVerdict(out, code); err != nil {
		t.Fatalf("requireDocker did not refuse a missing docker binary under CI_LOCAL=1: %v.\nhelper output:\n%s", err, out)
	}
}

func TestRequireDockerStillSkipsAMissingDockerBinaryOutsideTheGate(t *testing.T) {
	out, code := runHelper(t, "TestHelperRequireDockerAgainstAnUnavailableDocker",
		"CI_LOCAL=", "PATH="+t.TempDir())
	if err := skipVerdict(out, code); err != nil {
		t.Fatalf("with CI_LOCAL unset, a machine with no docker installed no longer skips: %v.\nhelper output:\n%s", err, out)
	}
}

// TestTheGatesOwnOptOutStillSkips keeps the gate's documented escape hatch
// honest. CI_LOCAL_SKIP_DOCKER=1 is scripts/ci-local.sh's out-loud opt-out
// for a run with the daemon down and it already ledgers that run as
// INCOMPLETE, so this suite honours it rather than overruling it. Without
// this, the flag would quietly stop working the moment CI_LOCAL landed.
func TestTheGatesOwnOptOutStillSkips(t *testing.T) {
	requireDockerBinary(t)
	requireTheDaemonIsUnreachable(t)

	out, code := runHelper(t, "TestHelperRequireDockerAgainstAnUnavailableDocker",
		"CI_LOCAL=1", "CI_LOCAL_SKIP_DOCKER=1", "DOCKER_HOST="+deadDockerHost)
	if err := skipVerdict(out, code); err != nil {
		t.Fatalf("CI_LOCAL_SKIP_DOCKER=1 no longer gets past this suite: %v.\nThat flag is documented as proceeding with the daemon down and ending the run INCOMPLETE, so overruling it here would make the documentation a lie.\nhelper output:\n%s", err, out)
	}
}

// TestHelperRequireDockerAgainstAnUnavailableDocker drives the real gate
// this suite goes through. It calls requireDocker directly rather than
// buildImage, so no child ever starts a container image build it cannot
// finish. Which of the two unavailable-docker paths it takes is decided by
// the environment its parent gave it.
func TestHelperRequireDockerAgainstAnUnavailableDocker(t *testing.T) {
	skipUnlessHelper(t)
	requireDocker(t)
	fmt.Println(requireReturnedMarker)
	t.Fatal("requireDocker returned against a docker that is not available, which should be impossible")
}
