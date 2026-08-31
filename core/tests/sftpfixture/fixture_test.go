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

func requireDocker(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"docker"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("SKIPPING (missing capability: %q not on PATH)", tool)
		}
	}
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skip("SKIPPING (missing capability: docker daemon not reachable)")
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
		t.Skipf("SKIPPING (cannot create the control container this assertion needs): %v", err)
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
		t.Skipf("SKIPPING (missing capability: %q not on PATH): %v", "docker", err)
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
