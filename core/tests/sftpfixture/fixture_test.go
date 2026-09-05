// These tests are issue #161's evidence: a fixture container that dies (or
// a docker daemon that stops answering) partway through a test must cost
// seconds and name itself, not 25 minutes of a package hanging to its go
// test timeout, and nothing may be left running behind it.
//
// Three of them drive the fixture from a CHILD test process, re-executing
// this same test binary with a guard variable set. That is not ceremony:
// the behaviour under test is "the test process fails, fast, saying why",
// and a test cannot assert its own failure from the inside. Watching a
// child gives the exit code, the elapsed time and the message all three,
// and lets the leak assertions run after the fixture's owner is genuinely
// gone.
//
// Every container assertion here addresses a container by the exact id the
// fixture created, never by a `docker ps` scan or a count. This machine
// runs many worktrees against one docker daemon, so a scan-shaped
// assertion could be answered by another agent's container and would prove
// nothing about this one.
package sftpfixture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/spdrman/rclone-manager/core/tests/dockerlease"
)

// helperEnv guards the helper tests below so they only run in a child
// process this file started, never as part of a normal `go test ./...`.
const helperEnv = "SFTPFIXTURE_HELPER"

// containerMarker is how a helper hands its container id back to the parent.
const containerMarker = "FIXTURE_CONTAINER="

func skipUnlessHelper(t *testing.T) {
	t.Helper()
	if os.Getenv(helperEnv) == "" {
		t.Skip("helper process only; driven by the fail-fast tests in this file")
	}
}

// requireDocker gates the tests here that need a real daemon. Both arms are
// docker-availability checks, so both go through the fixture's own verdict
// (#456): a skip on a laptop with no docker, and an INFRA: failure inside
// the gate, where docker is a declared prerequisite and a skip would take
// these assertions out of the run while the run still said ok.
func requireDocker(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"docker"} {
		if _, err := exec.LookPath(tool); err != nil {
			dockerUnavailable(t, "%q not on PATH", tool)
		}
	}
	if err := exec.Command("docker", "version").Run(); err != nil {
		dockerUnavailable(t, "docker daemon not reachable: %v", err)
	}
}

// runHelper re-executes this test binary for one helper test and reports
// what happened within window. exited is false when the child was still
// running at the end of the window, which is the shape of the #161 hang.
func runHelper(t *testing.T, name string, window time.Duration, extraEnv ...string) (out string, exited bool, code int) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.v=true", "-test.timeout="+(4*window).String())
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.Env = append(cmd.Env, extraEnv...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper %s: %v", name, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		code = 0
		if err != nil {
			code = cmd.ProcessState.ExitCode()
			if code == 0 {
				code = -1
			}
		}
		return buf.String(), true, code
	case <-time.After(window):
		_ = cmd.Process.Kill()
		<-done
		return buf.String(), false, 0
	}
}

// containerIDFrom pulls the id a helper printed. Reading the id the helper
// itself reported is what keeps these assertions scoped to this test's own
// container rather than to whatever else is on the daemon.
func containerIDFrom(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, containerMarker); i >= 0 {
			id := strings.TrimSpace(line[i+len(containerMarker):])
			if id != "" {
				return id
			}
		}
	}
	t.Fatalf("the helper never reported its container id, so there is nothing to assert about:\n%s", out)
	return ""
}

// removeAfterwards guarantees this test does not itself leak the helper's
// container while it is red. The assertions above it have already run by
// the time it fires, so it can only tidy up, never mask a leak.
func removeAfterwards(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
}

func containerExists(t *testing.T, id string) bool {
	t.Helper()
	return exec.Command("docker", "inspect", "--type=container", id).Run() == nil
}

// liveControlContainer creates a container this test owns and removes it
// again, purely so containerExists is shown answering "yes" for something
// live. Without it, "the fixture container is gone" would also be true if
// containerExists could never see anything at all.
func liveControlContainer(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "create", dockerlease.LabelFlag, dockerlease.LabelSpec, "alpine:latest", "true").Output()
	if err != nil {
		// A daemon that will not create this is a daemon that is not
		// available for the purpose, so it takes the same verdict as any
		// other one (#456). Without the control the assertion it feeds
		// proves nothing, which is exactly what must not go quiet under
		// the gate.
		dockerUnavailable(t, "the control container this assertion needs could not be created: %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	return id
}

// hangingDocker writes a `docker` that answers nothing, and returns the
// directory to put in front of PATH. It reproduces the real failure mode
// without needing to wedge the actual daemon: on this machine the Docker VM
// is provisioned at roughly 4 GB and 4 CPUs, and under several concurrent
// gate runs the daemon stops answering, which is what turns a fixture into
// a 25-minute hang.
func hangingDocker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 900\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the hanging docker shim: %v", err)
	}
	return dir
}

// --- the docker daemon stops answering ------------------------------------

// TestFixtureFailsFastWhenDockerStopsAnswering is the setup half of #161.
// Every wait in Start looks bounded (15s for the published port, 15s for
// the host-key scan, 20s for readiness) but each of those deadlines is only
// re-read BETWEEN attempts, and every attempt shells out to docker with no
// timeout at all. One `docker` that never returns therefore outruns all
// three, and the package hangs to its go test timeout with nothing said
// about which step is stuck.
func TestFixtureFailsFastWhenDockerStopsAnswering(t *testing.T) {
	// Positive control, on the checker rather than the subject: prove
	// runHelper reports a child that outlives its window as "did not
	// exit". If it reported an exit for that, "the subject exited in
	// time" below would be true no matter how badly the fixture hung.
	if _, exited, _ := runHelper(t, "TestHelperSleepsPastItsWindow", 2*time.Second); exited {
		t.Fatal("runHelper claimed a helper that sleeps far past its window had exited; it cannot tell a fail-fast from a hang, so nothing below would prove anything")
	}

	const window = 45 * time.Second
	shim := hangingDocker(t)
	out, exited, code := runHelper(t, "TestHelperFixtureAgainstAHangingDocker", window,
		"RCLONE_MANAGER_SFTP_TEST_BUDGET=5s",
		"RCLONE_MANAGER_SFTP_DEATH_GRACE=2s",
		"PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))

	if !exited {
		t.Fatalf("sftpfixture.Start never came back from a docker that answers nothing, %s in: this is #161, where the whole package hangs to its 25-minute go test timeout instead of naming the stuck step.\nhelper output:\n%s", window, out)
	}
	if code == 0 {
		t.Fatalf("the helper exited SUCCESSFULLY against a docker that answers nothing; a fixture that cannot reach docker must fail or skip loudly, never pass.\nhelper output:\n%s", out)
	}
	// Naming the step is the point, so the assertion is on the step, not
	// merely on the word "docker" appearing somewhere.
	if !strings.Contains(out, "docker info") {
		t.Fatalf("the failure never says it was stuck in `docker info`, so it does not name the step; a message that only says something timed out leaves the reader exactly where #161 left them.\nhelper output:\n%s", out)
	}
}

func TestHelperSleepsPastItsWindow(t *testing.T) {
	skipUnlessHelper(t)
	time.Sleep(60 * time.Second)
}

func TestHelperFixtureAgainstAHangingDocker(t *testing.T) {
	skipUnlessHelper(t)
	f := Start(t)
	fmt.Println(containerMarker + f.ContainerID())
	t.Fatal("Start returned against a docker that answers nothing, which should be impossible")
}

// --- a hang while the container is healthy --------------------------------

// TestFixtureNamesAGenuineHangAndLeavesNoContainer is the diagnostic half of
// #161: a container death and a genuine deadlock in the transport used to be
// indistinguishable from outside, and both cost 25 minutes. A test that
// outruns its budget while its container is demonstrably still running must
// say so, so the next person does not go looking for a Docker problem that
// is not there. It must also not leak the container on the way out, which is
// the harder half: the stop comes from a watchdog goroutine, and a panic
// there never runs t.Cleanup.
func TestFixtureNamesAGenuineHangAndLeavesNoContainer(t *testing.T) {
	requireDocker(t)

	const window = 90 * time.Second
	out, exited, code := runHelper(t, "TestHelperFixtureHangsWithAHealthyContainer", window,
		"RCLONE_MANAGER_SFTP_TEST_BUDGET=5s", "RCLONE_MANAGER_SFTP_DEATH_GRACE=2s")

	if !exited {
		t.Fatalf("a test that hangs with a perfectly healthy fixture container was still running %s later; it has to name itself long before the package's 25-minute timeout.\nhelper output:\n%s", window, out)
	}
	if code == 0 {
		t.Fatalf("the hanging helper exited successfully.\nhelper output:\n%s", out)
	}
	if !strings.Contains(out, "still running") {
		t.Fatalf("the failure does not say the fixture container was still running, so it does not distinguish a genuine deadlock from the container death in #161.\nhelper output:\n%s", out)
	}

	id := containerIDFrom(t, out)
	removeAfterwards(t, id)
	control := liveControlContainer(t)
	if !containerExists(t, control) {
		t.Fatal("containerExists says no to a container that was just created, so it cannot answer yes to anything; the leak assertion below would pass whatever happened")
	}
	if containerExists(t, id) {
		t.Fatalf("the fixture container %s outlived the test that was stopped for hanging; a fixture whose cleanup misses a hard exit path leaves containers competing with the next run, which is the self-worsening loop in #161", id)
	}
}

func TestHelperFixtureHangsWithAHealthyContainer(t *testing.T) {
	skipUnlessHelper(t)
	f := Start(t)
	fmt.Println(containerMarker + f.ContainerID())
	select {}
}

// --- a hard failure must not leak the container ---------------------------

// TestFixtureRemovesItsContainerWhenTheTestPanics covers the other hard exit
// path. #161 found orphaned rclone-manager-gate-sftp-* containers running
// for 4 and 11 hours from crashed earlier runs; each one competes with the
// next run for a Docker VM that has roughly 4 GB to give.
func TestFixtureRemovesItsContainerWhenTheTestPanics(t *testing.T) {
	requireDocker(t)

	const window = 90 * time.Second
	out, exited, code := runHelper(t, "TestHelperFixturePanicsMidTest", window)
	if !exited {
		t.Fatalf("a panicking test never finished within %s.\nhelper output:\n%s", window, out)
	}
	if code == 0 {
		t.Fatalf("a panicking test reported success.\nhelper output:\n%s", out)
	}

	id := containerIDFrom(t, out)
	removeAfterwards(t, id)
	control := liveControlContainer(t)
	if !containerExists(t, control) {
		t.Fatal("containerExists says no to a container that was just created, so it cannot answer yes to anything; the leak assertion below would pass whatever happened")
	}
	if containerExists(t, id) {
		t.Fatalf("the fixture container %s outlived a panicking test", id)
	}
}

func TestHelperFixturePanicsMidTest(t *testing.T) {
	skipUnlessHelper(t)
	f := Start(t)
	fmt.Println(containerMarker + f.ContainerID())
	panic("deliberate panic, standing in for any hard failure mid-test")
}

// --- a peer that accepts TCP and then says nothing ------------------------

// TestSSHHandshakeIsBoundedAgainstASilentPeer covers the one unbounded wait
// left in this fixture that does not shell out to anything.
// ssh.ClientConfig.Timeout is documented as bounding "the TCP connection to
// establish", and that is all ssh.Dial uses it for; the version and key
// exchange after it have no deadline. Measured against the code this
// replaced: ssh.Dial with a 2s Timeout was still waiting 20 seconds later.
//
// The gap is reachable on every fixture start. A published docker port
// accepts TCP as soon as the mapping exists, before sshd inside is
// necessarily answering, so a peer that accepts and then goes quiet is this
// fixture's ordinary startup window. One of those outlives waitForSSHReady's
// 20-second loop, because the loop only re-reads its deadline between
// attempts.
//
// No container is involved, which is the point: a bare listener that accepts
// and never speaks reproduces it identically on every machine, every time.
func TestSSHHandshakeIsBoundedAgainstASilentPeer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	accepted := make(chan net.Conn, 8)
	go func() {
		defer close(accepted)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Never written to and never closed: this peer completes the
			// TCP handshake and then says nothing at all.
			accepted <- c
		}
	}()
	// One cleanup, in this order on purpose. t.Cleanup runs LIFO, so two
	// separate ones would drain the channel before the listener was closed,
	// and the drain would then wait forever for a close that cannot happen
	// until the listener goes.
	t.Cleanup(func() {
		_ = ln.Close()
		for c := range accepted {
			_ = c.Close()
		}
	})

	cfg := &ssh.ClientConfig{
		User:            User,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         sshDialTimeout,
	}

	const window = 15 * time.Second
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- trySSHHandshake(ln.Addr().String(), cfg) }()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("the probe reported success against a peer that never sent a byte")
		}
		// Elapsed time is the positive control on the mechanism. A probe
		// that never got past the dial would come back in microseconds and
		// would satisfy "returned an error quickly" while proving nothing
		// about the handshake. Only one that connected and then waited out
		// the handshake deadline can land in this window.
		if elapsed < sshHandshakeTimeout-time.Second {
			t.Fatalf("the probe gave up after only %s, well short of the %s handshake deadline, so it cannot have got past the dial and says nothing about the handshake being bounded", elapsed, sshHandshakeTimeout)
		}
		t.Logf("bounded at %s: %v", elapsed, err)
	case <-time.After(window):
		t.Fatalf("the probe against a peer that accepts TCP and then says nothing was still waiting %s later; ssh.ClientConfig.Timeout bounds only the dial, so this is an unbounded wait inside a loop whose 20-second deadline it outruns, which is #161's shape in the one place that does not shell out", window)
	}
}

// --- the registry is not a dependency of a run that needs nothing from it --

// serverImageOnDaemon makes the premise of the image tests below true:
// they are about what the fixture does when the image is ALREADY on the
// local daemon, so there has to be one. It inspects first and pulls only
// when the image is genuinely missing, so on an ordinary machine it costs
// no network at all, and on a cold one it does the download a first run
// cannot avoid anyway.
//
// A failure here is fatal rather than a skip on purpose. A machine with
// neither the image nor a reachable registry cannot run the SFTP suite at
// all, and saying that once, out loud, beats skipping quietly and leaving
// the reader believing these ran.
func serverImageOnDaemon(t *testing.T) {
	t.Helper()

	inspect, cancel := context.WithTimeout(context.Background(), dockerProbeTimeout)
	defer cancel()
	if exec.CommandContext(inspect, "docker", "image", "inspect", serverImage).Run() == nil {
		return
	}

	pull, cancelPull := context.WithTimeout(context.Background(), dockerPullTimeout)
	defer cancelPull()
	out, err := exec.CommandContext(pull, "docker", "pull", serverImage).CombinedOutput()
	if err != nil {
		t.Fatalf("this machine has neither %s on its daemon nor a registry that will hand it over, so the SFTP suite cannot run here at all and there is no premise for these tests to stand on: %v\n%s", serverImage, err, out)
	}
}

// recordingDocker writes a `docker` that appends its arguments to a log and
// then execs the real one, and returns the directory to put in front of
// PATH plus the path of that log.
//
// Nothing is faked: every command still reaches the real daemon and comes
// back with the real answer, so the log is not a record of what a stand-in
// was asked, it is a record of what the fixture actually ran. That is the
// observable this file needs. "Did it pull?" is a question about the
// processes the fixture started, and reading them directly beats asserting
// on an internal flag that a future edit could set without the behaviour
// following.
func recordingDocker(t *testing.T) (dir, logPath string) {
	t.Helper()

	realDocker, err := exec.LookPath("docker")
	if err != nil {
		dockerUnavailable(t, "%q not on PATH, so there is no real client for the shim to hand its arguments to: %v", "docker", err)
	}
	dir = t.TempDir()
	logPath = filepath.Join(dir, "invocations.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(logPath) + "\nexec " + shellQuote(realDocker) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the recording docker shim: %v", err)
	}
	return dir, logPath
}

// shellQuote wraps s for /bin/sh. The paths here come from t.TempDir() and
// exec.LookPath and contain nothing exotic, but a shim that mangles its own
// log path would produce an empty log, which reads exactly like "the
// fixture ran no docker commands" and would make an assertion about a
// missing pull pass for the wrong reason.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// dockerInvocations reads back what the recording shim saw, one command per
// entry, already split into arguments.
func dockerInvocations(t *testing.T, logPath string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading the recorded docker invocations at %s: %v", logPath, err)
	}
	var invocations [][]string
	for _, line := range strings.Split(string(raw), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			invocations = append(invocations, fields)
		}
	}
	return invocations
}

// countInvocations counts the recorded commands starting with prefix.
func countInvocations(invocations [][]string, prefix ...string) int {
	n := 0
	for _, args := range invocations {
		if len(args) < len(prefix) {
			continue
		}
		match := true
		for i, want := range prefix {
			if args[i] != want {
				match = false
				break
			}
		}
		if match {
			n++
		}
	}
	return n
}

// TestFixtureDoesNotPullWhenTheImageIsAlreadyOnTheDaemon is issue #243's
// evidence. The fixture pulled atmoz/sftp:alpine unconditionally on every
// single start, so every gate run on every branch was a hard dependency on
// Docker Hub answering at that moment. Three separate gate runs died that
// way during one campaign, each with the image already sitting on the local
// daemon, and each reported at the verdict line as
// "FAILED (repository-structure dependency rules (§7.1), by actual
// deletion)" because the deletion proof re-runs core's suite. Reading that
// as an architecture violation and then finding a TLS handshake timeout a
// hundred lines up is how a gate stops being read.
//
// A pinned tag that is already on the local daemon does not need
// re-fetching to start a container, so the assertion is simply that no
// pull happened. It is made on the processes the fixture ran, not on
// anything the fixture reports about itself.
func TestFixtureDoesNotPullWhenTheImageIsAlreadyOnTheDaemon(t *testing.T) {
	requireDocker(t)
	serverImageOnDaemon(t)

	shim, logPath := recordingDocker(t)

	const window = 150 * time.Second
	out, exited, code := runHelper(t, "TestHelperStartsAFixtureAndReturns", window,
		"PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	if !exited {
		t.Fatalf("the helper that just starts a fixture was still running %s later.\nhelper output:\n%s", window, out)
	}
	if code != 0 {
		t.Fatalf("starting a fixture with %s already on this daemon failed (exit %d); nothing below can say anything about pulls until an ordinary start works.\nhelper output:\n%s", serverImage, code, out)
	}

	invocations := dockerInvocations(t, logPath)

	// Two positive controls on the recorder, before the assertion that
	// matters. Without them "no pull was recorded" is also what an empty
	// log says, and an empty log is what a shim that never got onto the
	// child's PATH produces.
	if len(invocations) == 0 {
		t.Fatalf("the recording shim logged nothing at all, so it was never the `docker` the fixture ran; an assertion about a missing pull would pass here no matter what the fixture did.\nhelper output:\n%s", out)
	}
	if countInvocations(invocations, "run", "-d") == 0 {
		t.Fatalf("the recorder never saw the `docker run -d` that starts the container, so it did not observe the fixture's own docker calls; recorded instead:\n%s", strings.Join(flatten(invocations), "\n"))
	}

	if got := countInvocations(invocations, "image", "inspect", serverImage); got == 0 {
		t.Fatalf("the fixture never asked whether %s was already on this daemon, so it cannot be deciding anything from the answer; recorded:\n%s", serverImage, strings.Join(flatten(invocations), "\n"))
	}
	if got := countInvocations(invocations, "pull"); got != 0 {
		t.Fatalf("the fixture ran `docker pull` %d time(s) for an image that was already on this daemon, so every start of it still depends on the registry answering: that is #243, and it has already failed three gate runs that had nothing wrong with them.\nrecorded:\n%s", got, strings.Join(flatten(invocations), "\n"))
	}
}

// flatten renders recorded invocations for a failure message.
func flatten(invocations [][]string) []string {
	lines := make([]string, 0, len(invocations))
	for _, args := range invocations {
		lines = append(lines, "  docker "+strings.Join(args, " "))
	}
	return lines
}

// TestHelperStartsAFixtureAndReturns is an ordinary, successful fixture
// start. It exists as a child process so the parent can read back every
// docker command the start ran, from a log the child's own PATH shim wrote,
// after the child and its cleanup are both finished.
func TestHelperStartsAFixtureAndReturns(t *testing.T) {
	skipUnlessHelper(t)
	f := Start(t)
	fmt.Println(containerMarker + f.ContainerID())
}

// --- a missing image is fetched, and an unfetchable one refuses ----------

// helperImageEnv carries the image reference a helper process should try to
// obtain, so the parent can hand its child a name that exists nowhere.
const helperImageEnv = "SFTPFIXTURE_HELPER_IMAGE"

// ensureReturnedMarker is how a helper reports that ensureImage came back
// normally. It has to be distinguishable from a refusal AND from a skip: a
// version of the fixture that shrugged and carried on would otherwise look
// like a pass.
const ensureReturnedMarker = "ENSURE_IMAGE_RETURNED"

// registryDocker writes a `docker` that stands in for the registry and for
// nothing else: `pull` is answered here, every other command is handed
// straight to the real daemon.
//
// failures is how many pull attempts fail before one succeeds, with the
// stderr of the real thing (the TLS handshake timeout that took two of
// #243's three gate runs). source is a local image to tag as the pulled
// reference when an attempt succeeds, which is what makes the success a
// genuine one: the reference really is absent beforehand and really is on
// the daemon afterwards, with no registry anywhere in it. An empty source
// means a pull that can never succeed.
func registryDocker(t *testing.T, failures int, source string) (dir, logPath string) {
	t.Helper()

	realDocker, err := exec.LookPath("docker")
	if err != nil {
		dockerUnavailable(t, "%q not on PATH, so there is no real client for the shim to hand its arguments to: %v", "docker", err)
	}
	dir = t.TempDir()
	logPath = filepath.Join(dir, "invocations.log")
	counter := filepath.Join(dir, "attempts")

	success := "echo 'the stand-in registry was asked to succeed but has nothing to hand over' >&2\n  exit 1"
	if source != "" {
		success = "exec " + shellQuote(realDocker) + " tag " + shellQuote(source) + ` "$2"`
	}

	script := "#!/bin/sh\n" +
		`printf '%s\n' "$*" >> ` + shellQuote(logPath) + "\n" +
		`if [ "$1" = "pull" ]; then` + "\n" +
		"  attempt=$(cat " + shellQuote(counter) + " 2>/dev/null || echo 0)\n" +
		"  attempt=$((attempt + 1))\n" +
		`  printf '%s' "$attempt" > ` + shellQuote(counter) + "\n" +
		fmt.Sprintf("  if [ \"$attempt\" -le %d ]; then\n", failures) +
		`    echo 'Error response from daemon: Head "https://registry-1.docker.io/v2/'"$2"'/manifests/x": net/http: TLS handshake timeout' >&2` + "\n" +
		"    exit 1\n" +
		"  fi\n" +
		"  " + success + "\n" +
		"fi\n" +
		"exec " + shellQuote(realDocker) + ` "$@"` + "\n"

	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the stand-in registry docker shim: %v", err)
	}
	return dir, logPath
}

// TestEnsureImageFetchesAMissingImageAndRidesOutATransientFailure is the
// other half of #243, and the reason the presence check above cannot be
// the whole fix. An image that is genuinely not here still has to be
// fetched, and the registry failures that started this were transient by
// nature, so one blip must not end the run.
//
// The absence is real. The reference is a tag this machine has never had
// and no registry has ever heard of, and the stand-in registry makes it
// appear by tagging a local image, so "it was missing, then it was here"
// is checked against the daemon both times rather than inferred. Nothing
// touches atmoz/sftp:alpine itself, so a failure here cannot leave the
// machine without the image every other test needs.
func TestEnsureImageFetchesAMissingImageAndRidesOutATransientFailure(t *testing.T) {
	requireDocker(t)
	serverImageOnDaemon(t)

	realDocker, err := exec.LookPath("docker")
	if err != nil {
		dockerUnavailable(t, "%q not on PATH, so there is no real client for the shim to hand its arguments to: %v", "docker", err)
	}

	runID := time.Now().UnixNano()
	source := fmt.Sprintf("rclone-manager-gate-243-source:%d", runID)
	absent := fmt.Sprintf("rclone-manager-gate-243-absent:%d", runID)

	// Both of these are tags this test creates and this test removes, and
	// `docker rmi` on a tag that shares an image with another one only
	// drops the tag. atmoz/sftp:alpine keeps every layer it had.
	if out, err := exec.Command(realDocker, "tag", serverImage, source).CombinedOutput(); err != nil {
		t.Fatalf("tagging %s as the stand-in registry's copy %s: %v\n%s", serverImage, source, err, out)
	}
	t.Cleanup(func() { _ = exec.Command(realDocker, "rmi", source).Run() })
	t.Cleanup(func() { _ = exec.Command(realDocker, "rmi", absent).Run() })

	// The premise, checked rather than assumed: "it pulled a missing
	// image" proves nothing if the image was never missing.
	if exec.Command(realDocker, "image", "inspect", absent).Run() == nil {
		t.Fatalf("%s is somehow already on this daemon, so this test cannot say anything about an image that is not", absent)
	}

	const failures = 2
	shim, logPath := registryDocker(t, failures, source)
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(pullBackoffEnv, "200ms")

	f := &Fixture{}
	started := time.Now()
	f.ensureImage(t, absent)
	elapsed := time.Since(started)

	if err := exec.Command(realDocker, "image", "inspect", absent).Run(); err != nil {
		t.Fatalf("ensureImage returned as though it had the image, but the daemon still does not know %s: a fixture that believes it has an image it has not got fails later, at `docker run`, with a message about the wrong thing: %v", absent, err)
	}

	invocations := dockerInvocations(t, logPath)
	if got := countInvocations(invocations, "pull", absent); got != failures+1 {
		t.Fatalf("the pull was attempted %d time(s) against a registry that failed the first %d, want %d: without the retry a single TLS handshake timeout is still a red gate, which is what #243 is.\nrecorded:\n%s", got, failures, failures+1, strings.Join(flatten(invocations), "\n"))
	}

	// A positive control on the backoff itself. Three attempts with a
	// 200ms base wait cannot be over in less than 200ms + 400ms, so an
	// elapsed time below that would mean the retries were immediate,
	// which against a registry that needs a moment to recover is barely a
	// retry at all.
	if floor := 600 * time.Millisecond; elapsed < floor {
		t.Fatalf("three pull attempts were over in %s, under the %s that a %s base backoff makes unavoidable, so nothing waited between them", elapsed, floor, "200ms")
	}
}

// refusalVerdict reports whether a child run REFUSED, meaning it failed and
// said so, rather than skipping or quietly passing. It is deliberately a
// value the test can check both ways, because "this fixture refuses" is
// only worth asserting if the check would notice a skip, and a skip exits 0
// exactly like a pass.
func refusalVerdict(out string, code int) error {
	if strings.Contains(out, "--- SKIP") {
		return errors.New("the run SKIPPED, which is the failure mode this is here to catch: the suite silently leaves the gate and the gate still says ok")
	}
	if code == 0 {
		return fmt.Errorf("the run exited 0, so it did not refuse")
	}
	if !strings.Contains(out, "--- FAIL") {
		return fmt.Errorf("the run exited %d but never reported a test failure, so what stopped it is not the fixture refusing", code)
	}
	return nil
}

// TestEnsureImageRefusesRatherThanSkippingWhenTheImageCannotBeObtained is
// the assertion that matters most in #243, and the one a well meaning fix
// breaks. Making the fixture skip when the image cannot be fetched looks
// like kindness to a machine that is offline. It is not: it deletes the
// whole SFTP suite from the gate and lets the gate go on printing ok, which
// is strictly worse than the loud failure it replaced and is the same hole
// #160 was opened about. So an image that genuinely cannot be obtained has
// to be fatal, and it has to stay fatal.
func TestEnsureImageRefusesRatherThanSkippingWhenTheImageCannotBeObtained(t *testing.T) {
	requireDocker(t)

	absent := fmt.Sprintf("rclone-manager-gate-243-unobtainable:%d", time.Now().UnixNano())

	// The positive control, run first and on the checker rather than on
	// the subject. A skipping fixture is the exact thing this test exists
	// to reject, so a helper that skips in that shape is put through the
	// same verdict, and the verdict has to reject it. Without this, a
	// check that called everything a refusal would pass below and prove
	// nothing at all.
	skipOut, skipExited, skipCode := runHelper(t, "TestHelperEnsureImageSkipsInstead", 45*time.Second,
		helperImageEnv+"="+absent)
	if !skipExited {
		t.Fatalf("the skipping control helper never finished, so the control says nothing.\nhelper output:\n%s", skipOut)
	}
	if err := refusalVerdict(skipOut, skipCode); err == nil {
		t.Fatalf("refusalVerdict ACCEPTED a run that skipped in exactly the shape this test forbids, so it cannot tell a refusal from a silent skip and its verdict on the real fixture below would mean nothing.\ncontrol helper output:\n%s", skipOut)
	}

	shim, logPath := registryDocker(t, 1000, "")
	out, exited, code := runHelper(t, "TestHelperEnsureImageAgainstAnUnobtainableImage", 60*time.Second,
		helperImageEnv+"="+absent,
		pullBackoffEnv+"=200ms",
		"PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))

	if !exited {
		t.Fatalf("ensureImage never came back for an image no registry will hand over.\nhelper output:\n%s", out)
	}
	if strings.Contains(out, ensureReturnedMarker) {
		t.Fatalf("ensureImage RETURNED for an image it never obtained, so the fixture would carry on to `docker run` with nothing to run.\nhelper output:\n%s", out)
	}
	if err := refusalVerdict(out, code); err != nil {
		t.Fatalf("the fixture did not refuse an image it cannot obtain: %v.\nSkipping here takes the SFTP suite out of the gate while the gate goes on saying ok, which is #160's hole and is worse than the network failure it would be papering over.\nhelper output:\n%s", err, out)
	}
	if !strings.Contains(out, absent) {
		t.Fatalf("the refusal never names %s, so the reader is told something failed without being told which image is missing.\nhelper output:\n%s", absent, out)
	}

	invocations := dockerInvocations(t, logPath)
	if got := countInvocations(invocations, "pull", absent); got != pullAttempts {
		t.Fatalf("a doomed pull was attempted %d time(s), want exactly %d: fewer means the retry does not apply on the path that matters, more means the fixture keeps a dead gate waiting.\nrecorded:\n%s", got, pullAttempts, strings.Join(flatten(invocations), "\n"))
	}
}

// TestHelperEnsureImageAgainstAnUnobtainableImage asks for an image that is
// not on this daemon and that the stand-in registry in front of it will
// never hand over.
func TestHelperEnsureImageAgainstAnUnobtainableImage(t *testing.T) {
	skipUnlessHelper(t)
	ref := os.Getenv(helperImageEnv)
	if ref == "" {
		t.Fatalf("%s was not set, so this helper has no image to ask for", helperImageEnv)
	}
	f := &Fixture{}
	f.ensureImage(t, ref)
	fmt.Println(ensureReturnedMarker)
	t.Fatal("ensureImage returned for an image that cannot be obtained")
}

// TestHelperEnsureImageSkipsInstead is the obliging fixture this repository
// must not ship, written down so the test above can be shown rejecting it.
// It exists only as the positive control: nothing here is called by the
// fixture.
func TestHelperEnsureImageSkipsInstead(t *testing.T) {
	skipUnlessHelper(t)
	t.Skipf("sftpfixture: SKIPPING (missing capability: %s could not be pulled)", os.Getenv(helperImageEnv))
}

// --- docker is not available at all ---------------------------------------

// These are issue #456. This fixture already failed a WEDGED daemon, with a
// comment saying that skipping would silently remove the SFTP suite from
// the gate, and then skipped an UNREACHABLE one three lines below that
// comment. A Docker VM that dies in the middle of a gate run is unreachable
// rather than wedged, so it took the skip. In one stored gate log it did:
// 13 of the 14 conformance mutation cells printed `ok ... 0.08s` against a
// dead daemon and the run stayed green.
//
// The skip is still right on a laptop with no docker, so both directions
// are asserted. A test that only checked the new branch would let the
// laptop case regress silently, which is the mirror image of how this got
// here in the first place.
//
// Nothing stops the real daemon to make this true. Several worktrees on
// this machine share one, so stopping it would take everybody else's run
// down; DOCKER_HOST is pointed at an endpoint nothing listens on and the
// real docker client gives the real answer.

// deadDockerHost is an endpoint nothing listens on. `docker info` against
// it fails in milliseconds with the daemon's own unreachable message.
const deadDockerHost = "tcp://127.0.0.1:1"

// startReturnedMarker is printed if Start ever comes back on a path where
// it cannot. A fixture that shrugged and carried on would otherwise look
// like an ordinary failure further down.
const startReturnedMarker = "START_RETURNED"

// wantInfraMarker is the marker a refusal has to carry, written out as its
// own literal rather than read from fixture.go's constant on purpose. The
// marker is a contract with whoever reads a gate log, so renaming it on the
// production side has to turn this red rather than follow along quietly.
const wantInfraMarker = "INFRA:"

// gateRefusalVerdict is refusalVerdict plus the marker. A refusal that does
// not say INFRA leaves a reader unable to sort a gate log into a machine
// that broke and a product that broke, which is half of what #456 asked
// for.
func gateRefusalVerdict(out string, code int) error {
	if err := refusalVerdict(out, code); err != nil {
		return err
	}
	if !strings.Contains(out, wantInfraMarker) {
		return fmt.Errorf("the run failed but never said %q, so nothing in the log says this was the machine rather than the product", wantInfraMarker)
	}
	return nil
}

// laptopSkipVerdict is the opposite: the run left the suite out, out loud,
// and did not fail.
func laptopSkipVerdict(out string, code int) error {
	if code != 0 {
		return fmt.Errorf("the run exited %d rather than skipping, so a developer machine with no docker now has a red package", code)
	}
	if !strings.Contains(out, "--- SKIP") {
		return errors.New("the run exited 0 without skipping anything, so the fixture never reported the missing capability at all")
	}
	if strings.Contains(out, wantInfraMarker) {
		return fmt.Errorf("the run skipped but still printed %q, which reads in a log as an infrastructure failure that did not happen", wantInfraMarker)
	}
	return nil
}

// requireTheDaemonIsUnreachable checks the premise the simulation rests on.
// A docker context can name an endpoint of its own, and if DOCKER_HOST were
// ignored the child would reach the REAL daemon, start a real SFTP server
// and fail somewhere else entirely, which the assertions below would
// happily read as a refusal.
func requireTheDaemonIsUnreachable(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "info")
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+deadDockerHost)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("`docker info` SUCCEEDED with DOCKER_HOST=%s, so this machine's docker ignores it and nothing below simulates an unreachable daemon at all:\n%s", deadDockerHost, out)
	}
}

// TestStartRefusesAnUnreachableDaemonInsideTheGate is #456 itself.
func TestStartRefusesAnUnreachableDaemonInsideTheGate(t *testing.T) {
	requireDocker(t)
	requireTheDaemonIsUnreachable(t)

	const window = 60 * time.Second

	// The positive control, and it is the same helper against the same
	// dead endpoint with only CI_LOCAL removed. That makes it a control on
	// two things at once: the verdict really can tell a skip from a
	// refusal, and the environment is the only thing deciding which one
	// happens.
	skipOut, skipExited, skipCode := runHelper(t, "TestHelperStartAgainstAnUnavailableDocker", window,
		"CI_LOCAL=", "DOCKER_HOST="+deadDockerHost)
	if !skipExited {
		t.Fatalf("the control helper never finished, so the control says nothing.\nhelper output:\n%s", skipOut)
	}
	if err := gateRefusalVerdict(skipOut, skipCode); err == nil {
		t.Fatalf("gateRefusalVerdict ACCEPTED a run that skipped in exactly the shape #456 is about, so it cannot tell a refusal from a silent skip and its verdict below would mean nothing.\ncontrol helper output:\n%s", skipOut)
	}

	out, exited, code := runHelper(t, "TestHelperStartAgainstAnUnavailableDocker", window,
		"CI_LOCAL=1", "CI_LOCAL_SKIP_DOCKER=", "DOCKER_HOST="+deadDockerHost)
	if !exited {
		t.Fatalf("Start never came back from a daemon it cannot reach.\nhelper output:\n%s", out)
	}
	if strings.Contains(out, startReturnedMarker) {
		t.Fatalf("Start RETURNED against a daemon it cannot reach, so the suite would carry on against nothing.\nhelper output:\n%s", out)
	}
	if err := gateRefusalVerdict(out, code); err != nil {
		t.Fatalf("the fixture did not refuse an unreachable daemon under CI_LOCAL=1: %v.\nThis is #456: a Docker VM that dies mid-run is 'not reachable', the fixture skips, and the gate goes on printing ok with the SFTP suite silently empty.\nhelper output:\n%s", err, out)
	}
}

// TestStartStillSkipsAnUnreachableDaemonOutsideTheGate is the half that a
// fix for the one above breaks if the skip is deleted rather than made
// conditional.
func TestStartStillSkipsAnUnreachableDaemonOutsideTheGate(t *testing.T) {
	requireDocker(t)
	requireTheDaemonIsUnreachable(t)

	out, exited, code := runHelper(t, "TestHelperStartAgainstAnUnavailableDocker", 60*time.Second,
		"CI_LOCAL=", "DOCKER_HOST="+deadDockerHost)
	if !exited {
		t.Fatalf("the helper never finished.\nhelper output:\n%s", out)
	}
	if err := laptopSkipVerdict(out, code); err != nil {
		t.Fatalf("with CI_LOCAL unset, an unreachable daemon no longer skips: %v.\nEvery developer without a running docker would now have a red package, which is why the skip is conditional and not gone.\nhelper output:\n%s", err, out)
	}
}

// TestStartRefusesAMissingDockerBinaryInsideTheGate asks the same question
// of the other path into "docker is not available". A gate machine with no
// docker on PATH is as broken as one whose daemon died, and if the two
// answered differently the hole would just move.
func TestStartRefusesAMissingDockerBinaryInsideTheGate(t *testing.T) {
	out, exited, code := runHelper(t, "TestHelperStartAgainstAnUnavailableDocker", 60*time.Second,
		"CI_LOCAL=1", "CI_LOCAL_SKIP_DOCKER=", "PATH="+t.TempDir())
	if !exited {
		t.Fatalf("the helper never finished.\nhelper output:\n%s", out)
	}
	if strings.Contains(out, startReturnedMarker) {
		t.Fatalf("Start RETURNED with no docker on PATH at all.\nhelper output:\n%s", out)
	}
	if err := gateRefusalVerdict(out, code); err != nil {
		t.Fatalf("the fixture did not refuse a missing docker binary under CI_LOCAL=1: %v.\nhelper output:\n%s", err, out)
	}
}

// TestStartStillSkipsAMissingDockerBinaryOutsideTheGate is the laptop case
// in its purest form: docker is genuinely not installed, and that is not
// this repository's problem to be red about.
func TestStartStillSkipsAMissingDockerBinaryOutsideTheGate(t *testing.T) {
	out, exited, code := runHelper(t, "TestHelperStartAgainstAnUnavailableDocker", 60*time.Second,
		"CI_LOCAL=", "PATH="+t.TempDir())
	if !exited {
		t.Fatalf("the helper never finished.\nhelper output:\n%s", out)
	}
	if err := laptopSkipVerdict(out, code); err != nil {
		t.Fatalf("with CI_LOCAL unset, a machine with no docker installed no longer skips: %v.\nhelper output:\n%s", err, out)
	}
}

// TestTheGatesOwnOptOutStillSkips keeps the gate's documented escape hatch
// honest. CI_LOCAL_SKIP_DOCKER=1 is scripts/ci-local.sh's out-loud opt-out
// for a run with the daemon down and it already ledgers that run as
// INCOMPLETE, so this fixture honours it rather than overruling it. Without
// this, the flag would quietly stop working the moment CI_LOCAL landed.
func TestTheGatesOwnOptOutStillSkips(t *testing.T) {
	requireDocker(t)
	requireTheDaemonIsUnreachable(t)

	out, exited, code := runHelper(t, "TestHelperStartAgainstAnUnavailableDocker", 60*time.Second,
		"CI_LOCAL=1", "CI_LOCAL_SKIP_DOCKER=1", "DOCKER_HOST="+deadDockerHost)
	if !exited {
		t.Fatalf("the helper never finished.\nhelper output:\n%s", out)
	}
	if err := laptopSkipVerdict(out, code); err != nil {
		t.Fatalf("CI_LOCAL_SKIP_DOCKER=1 no longer gets past this fixture: %v.\nThat flag is documented as proceeding with the daemon down and ending the run INCOMPLETE, so overruling it here would make the documentation a lie.\nhelper output:\n%s", err, out)
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
