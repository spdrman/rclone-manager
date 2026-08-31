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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if !strings.Contains(out, "docker") {
		t.Fatalf("the failure never mentions docker, so it does not name the stuck step; the whole point of #161 is that the message says what happened.\nhelper output:\n%s", out)
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
