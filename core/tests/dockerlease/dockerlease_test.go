package dockerlease

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

// The label is the entire safety boundary for Sweep, so these tests drive a
// real docker rather than a fake: the thing worth proving is that a `docker
// rm -f` built from a `--filter label=` never reaches a container somebody
// else owns.
//
// The other half of the file is about what happens when there is no daemon
// at all, and it is here rather than in a package of its own because the two
// halves are the same question asked from opposite sides. A sweep that is
// too broad destroys somebody else's work; a gate that skips itself when
// docker is missing destroys the evidence instead, silently, while the run
// keeps printing ok. Both are checked against a real docker client pointed
// at an endpoint nothing listens on, because the real answer is the one that
// matters and stopping the shared daemon would take every other worktree's
// run down with it.

// requireDocker gates every test here that needs a real daemon, and decides
// whether an absent one is a skip or a failure.
//
// On a machine that simply has no docker, skipping is honest: this package
// is evidence for the gate, not a requirement on every developer's laptop.
// Inside the gate it is the opposite. Docker is a declared prerequisite
// there, so the same condition means the gate's own machine is broken, and
// a skip quietly deletes these tests from the run while the run goes on
// printing ok.
//
// That is #456, and it is not hypothetical: in one stored gate log a Docker
// VM died mid-run and 13 of the 14 conformance mutation cells printed
// `ok ... 0.08s` against a dead daemon, because "not reachable" took a skip
// everywhere it was asked. One cell happened to refuse, which is the only
// reason anybody noticed.
//
// So under the gate this is a failure carrying infraMarker. The one way
// past it is CI_LOCAL_SKIP_DOCKER=1, the gate's own documented opt-out for
// a run with the daemon down, which already ledgers that run as INCOMPLETE.
func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "version").Run(); err != nil {
		dockerUnavailable(t, "`docker version` did not answer, so docker is not available here: %v", err)
	}
}

// infraMarker is the fixed, greppable string every infrastructure refusal
// here carries. It is the same literal in core/tests/machines and in
// distribution/tests/adapterstacks on purpose: a gate log sorts into "the
// machine broke" and "the product broke" with one grep, and a marker that
// varied by package would not.
const infraMarker = "INFRA:"

func dockerUnavailable(t *testing.T, reason string, args ...any) {
	t.Helper()
	detail := fmt.Sprintf(reason, args...)
	if gateRequiresDocker() {
		t.Fatalf("%s dockerlease: %s\nDocker is a declared prerequisite of this gate (CI_LOCAL=1), so this is an INFRASTRUCTURE failure and not a product one: the machine could not offer a docker daemon. Skipping here would take the label-boundary proofs out of the run while the gate still printed ok, which is #456.", infraMarker, detail)
	}
	t.Skipf("dockerlease: SKIPPING (missing capability: %s)", detail)
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

// create makes a container without starting it. Created is all Sweep reads,
// and not starting one keeps the test cheap and side-effect free.
func create(t *testing.T, labelled bool) string {
	t.Helper()
	args := []string{"create"}
	if labelled {
		args = append(args, LabelFlag, LabelSpec)
	}
	args = append(args, "alpine:latest", "true")
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker create: %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	return id
}

func exists(t *testing.T, id string) bool {
	t.Helper()
	return exec.Command("docker", "inspect", id).Run() == nil
}

// Every test here sweeps its OWN ids and nothing else. sweepOlderThan with
// a cutoff in the future is not an option: it lists every labelled container
// on the daemon, and under `go test ./...` those include live SFTP fixtures
// held by tests/sftpintegration, tests/crashmatrix, core/service and
// core/internal/transport/rclone, running in parallel with this package, plus
// every other worktree sharing this machine's daemon. See
// TestNoSweepTestReapsAContainerItDoesNotOwn.

func TestSweepLeavesAContainerYoungerThanTheCutoff(t *testing.T) {
	requireDocker(t)
	id := create(t, true)

	sweepIDs([]string{id}, time.Now().Add(-time.Hour))

	if !exists(t, id) {
		t.Fatal("swept a labelled container created after the cutoff; a threshold that " +
			"reaches live containers would delete one out from under a running test")
	}
}

func TestSweepRemovesALabelledContainerOlderThanTheCutoff(t *testing.T) {
	requireDocker(t)
	id := create(t, true)

	// Cutoff in the future, so a container created a moment ago is stale.
	sweepIDs([]string{id}, time.Now().Add(time.Hour))

	if exists(t, id) {
		t.Fatal("left a labelled container older than the cutoff; this is the leak in #150")
	}
}

// TestSweepNeverTouchesAnUnlabelledContainer asks the question through
// listLabelled rather than by sweeping, because the label filter IS
// listLabelled: sweepIDs removes whatever it is handed. Asserting on the
// candidate list proves the same boundary and, unlike a sweep with a future
// cutoff, cannot take a live container in another package down with it.
func TestSweepNeverTouchesAnUnlabelledContainer(t *testing.T) {
	requireDocker(t)
	mine := create(t, false)
	labelled := create(t, true)

	candidates := listLabelled()

	if !containsID(candidates, labelled) {
		t.Fatal("listLabelled did not offer up a labelled container at all, so the absence of the unlabelled one below would prove nothing")
	}
	if containsID(candidates, mine) {
		t.Fatal("listLabelled offered up a container this repo does not own; the label filter is the only " +
			"thing standing between Sweep and somebody else's work")
	}
}

// containsID compares by prefix because `docker ps -q` answers in short ids
// while `docker create` returns the full one.
func containsID(ids []string, id string) bool {
	for _, got := range ids {
		if got != "" && (strings.HasPrefix(id, got) || strings.HasPrefix(got, id)) {
			return true
		}
	}
	return false
}

// TestSweepStillReapsWhenAListedContainerVanishedFromUnderIt is issue #161's
// sweeper finding. StaleAfter is fifteen minutes, and yet #161 found
// rclone-manager-gate-sftp-* containers still running after 4 and 11 hours.
// Part of that is simply that they predate the label (it landed the same
// morning, in #151), but there is a live defect underneath it: the batch
// `docker inspect` used to date the candidates exits non-zero if ANY of its
// arguments is missing, and that exit status was being read as "nothing can
// be dated". One container removed by another worktree between the listing
// and the inspect therefore turned the entire sweep into a silent no-op.
func TestSweepStillReapsWhenAListedContainerVanishedFromUnderIt(t *testing.T) {
	requireDocker(t)

	// Positive control: the same call, with a batch containing nothing
	// stale-but-missing, does reap. Without it "the live one is gone"
	// below could just as well mean sweepIDs never works at all.
	control := create(t, true)
	sweepIDs([]string{control}, time.Now().Add(time.Hour))
	if exists(t, control) {
		t.Fatal("sweepIDs left a labelled container older than the cutoff even with a clean batch, so it does not reap at all and the assertion below would prove nothing")
	}

	live := create(t, true)
	const vanished = "0000000000000000000000000000000000000000000000000000000000000000"
	sweepIDs([]string{live, vanished}, time.Now().Add(time.Hour))
	if exists(t, live) {
		t.Fatal("one already-vanished id in the batch made the whole sweep a no-op; on a machine where several worktrees share one docker daemon that race is routine, and a sweeper that silently sweeps nothing is exactly the class of bug #161 is about")
	}
}

// --- this package must not reap other packages' live containers -----------

// runSelf re-runs some of this package's own tests in a child process, so
// the parent can watch what they did to a container they never created.
func runSelf(t *testing.T, pattern string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run="+pattern, "-test.v=true", "-test.timeout=2m")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running %s in a child process failed:\n%s", pattern, out)
	}
	return string(out)
}

// TestNoSweepTestReapsAContainerItDoesNotOwn is the #161 root cause, and it
// is this package's own tests doing it.
//
// `go test ./...` runs packages in parallel. While tests/dockerlease is
// running, tests/sftpintegration, tests/crashmatrix, core/service and
// core/internal/transport/rclone are each holding a LIVE, labelled SFTP
// fixture container, and so is every other worktree on this machine running
// its own gate against the same docker daemon. A sweep here with a cutoff in
// the future matches every labelled container in existence, not just this
// test's own, so it kills healthy fixtures belonging to tests that have
// nothing to do with it.
//
// That is exactly the signature in the issue: the container vanishes
// mid-suite, everything after it gets connection refused, and the victim is
// a different test on every run, because the victim is simply whichever
// fixture happened to be alive when this package ran. Captured from the
// daemon's own event stream during a run of this branch: `kill signal=9`,
// `die exitCode=137` and `destroy` on a two-second-old SFTP fixture, in the
// same instant as this package's own throwaway containers were being created
// and destroyed around it.
func TestNoSweepTestReapsAContainerItDoesNotOwn(t *testing.T) {
	requireDocker(t)

	// A labelled container this package does not own, standing in for the
	// live fixture another package is holding at the same moment.
	bystander := create(t, true)
	if !exists(t, bystander) {
		t.Fatal("the bystander is not there before the sweep tests run, so the assertion after them would be about nothing")
	}
	if exists(t, "0000000000000000000000000000000000000000000000000000000000000000") {
		t.Fatal("exists() says yes to an id that cannot be there, so it can never report a reaping")
	}

	runSelf(t, "^TestSweep")

	if !exists(t, bystander) {
		t.Fatalf("this package's own sweep tests destroyed a labelled container (%s) they never created. Under `go test ./...` that container is somebody else's live SFTP fixture, which is the container death in #161", bystander)
	}
}

// --- docker is not available at all (#456) --------------------------------

// requireDocker used to skip whenever `docker version` did not answer, and
// this package is one of the four the gate loses when the Docker VM dies
// mid-run: every test here goes through it, so one skip empties the whole
// package while the gate still prints ok.
//
// The skip is still right on a laptop with no docker, so both directions
// are asserted below. A test that only checked the new branch would let the
// laptop case regress silently.
//
// Nothing stops the real daemon to make any of this true. Several worktrees
// on this machine share one, so stopping it would take everybody else's run
// down with it; DOCKER_HOST is pointed at an endpoint nothing listens on
// and the real docker client gives the real answer.

// helperEnv guards the helper below so it only runs in a child process one
// of these tests started.
const helperEnv = "DOCKERLEASE_HELPER"

// deadDockerHost is an endpoint nothing listens on. `docker version`
// against it fails in milliseconds with the daemon's own message.
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
//
// It is deliberately separate from runSelf above, which fails the parent
// whenever the child does. Here the child failing is the thing being
// measured.
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
		return errors.New("the run SKIPPED, which is the failure mode #456 is about: the package quietly leaves the gate and the gate still says ok")
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

// skipVerdict is refusalVerdict's opposite: the run left the package out,
// out loud, and did not fail.
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

// requireTheDaemonIsUnreachable checks the premise the simulation rests on.
// A docker context can name an endpoint of its own, and if DOCKER_HOST were
// ignored the child would reach the REAL daemon, sail through requireDocker
// and fail on the marker instead, which the assertions below would read as
// a refusal.
func requireTheDaemonIsUnreachable(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "version")
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+deadDockerHost)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("`docker version` SUCCEEDED with DOCKER_HOST=%s, so this machine's docker ignores it and nothing below simulates an unreachable daemon at all:\n%s", deadDockerHost, out)
	}
}

func TestRequireDockerRefusesAnUnreachableDaemonInsideTheGate(t *testing.T) {
	requireDocker(t)
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
		t.Fatalf("requireDocker waved the test through against a daemon it cannot reach.\nhelper output:\n%s", out)
	}
	if err := refusalVerdict(out, code); err != nil {
		t.Fatalf("requireDocker did not refuse an unreachable daemon under CI_LOCAL=1: %v.\nEvery test in this package goes through it, so one skip here empties the whole package while the gate goes on printing ok.\nhelper output:\n%s", err, out)
	}
}

func TestRequireDockerStillSkipsAnUnreachableDaemonOutsideTheGate(t *testing.T) {
	requireDocker(t)
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
		t.Fatalf("requireDocker waved the test through with no docker on PATH at all.\nhelper output:\n%s", out)
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
// INCOMPLETE, so this package honours it rather than overruling it.
func TestTheGatesOwnOptOutStillSkips(t *testing.T) {
	requireDocker(t)
	requireTheDaemonIsUnreachable(t)

	out, code := runHelper(t, "TestHelperRequireDockerAgainstAnUnavailableDocker",
		"CI_LOCAL=1", "CI_LOCAL_SKIP_DOCKER=1", "DOCKER_HOST="+deadDockerHost)
	if err := skipVerdict(out, code); err != nil {
		t.Fatalf("CI_LOCAL_SKIP_DOCKER=1 no longer gets past requireDocker: %v.\nThat flag is documented as proceeding with the daemon down and ending the run INCOMPLETE, so overruling it here would make the documentation a lie.\nhelper output:\n%s", err, out)
	}
}

// TestHelperRequireDockerAgainstAnUnavailableDocker drives the real gate
// every test in this package goes through. Which of the two
// unavailable-docker paths it takes is decided by the environment its
// parent gave it.
func TestHelperRequireDockerAgainstAnUnavailableDocker(t *testing.T) {
	skipUnlessHelper(t)
	requireDocker(t)
	fmt.Println(requireReturnedMarker)
	t.Fatal("requireDocker returned against a docker that is not available, which should be impossible")
}
