// Package sftpfixture stands up a disposable SFTP server in Docker so the
// Phase-1 gate tests can drive the real rclone sftp backend against a real
// server, rather than reasoning about the API from the outside.
//
// It uses atmoz/sftp (OpenSSH's sshd, chrooted, forced into internal-sftp)
// because that gives us a genuine SSH/SFTP endpoint with real host-key
// verification and real chroot/permission semantics, for the cost of a
// disposable container. All key material is generated fresh per test run
// under tests/.run and removed on cleanup; nothing here is a real credential.
//
// What this suite costs, written down here rather than left as folklore for
// the next person to rediscover through a 25-minute hang (issue #161). Every
// fixture is a real sshd in its own container, and one scripts/ci-local.sh
// run starts the suite three times over: once directly, and again inside the
// throwaway worktrees of verify-core-without-apps.sh and
// verify-ugos-removable.sh. The Docker VM on the machine this was written on
// has 4 CPUs and roughly 4 GB, which is comfortable for one gate at a time
// and demonstrably not comfortable for several at once: under concurrent
// gate runs, containers get evicted mid-test. If a machine has to run gates
// in parallel, either that VM needs more than 4 GB, or the two architecture
// checks need to stop re-running a container-backed suite to prove a
// dependency boundary that no container is involved in.
package sftpfixture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/tests/dockerlease"
)

// User is the fixed username created inside the container.
const User = "backupuser"

const containerUID = "1001"

// serverImage is the SFTP server this fixture runs. Every place that names
// the image reads it from here, so the presence check, the pull and the
// `docker run` can never drift apart and start talking about two different
// images.
//
// Deliberately a tag and not a digest, which #243 asked about. Pinning
// atmoz/sftp@sha256:... would make the reference reproducible, and it would
// also mean that every machine already holding the `alpine` tag has to go
// back to Docker Hub once to fetch the pinned one: the registry round trip
// this change exists to remove, arriving everywhere on the same day. What
// it would buy is reproducibility this suite does not lean on. The fixture
// never trusts the image's contents; it verifies the properties it needs
// directly (that both host keys are the ones it mounted, that the chroot
// and key-only login behave, that an unknown host key is refused), so a
// moved tag surfaces as a red test rather than as a silent change of
// behaviour. And the presence check below settles the tag in practice
// anyway: whatever a machine pulled once is what it goes on using, because
// nothing asks the registry again. If a genuinely fixed input is ever
// wanted, the honest way to get it is to mirror the image somewhere we
// control, not to pin a digest against a registry we do not.
const serverImage = "atmoz/sftp:alpine"

// Every subprocess this fixture starts gets a timeout, because the
// deadline-bounded retry loops further down only re-read their deadline
// BETWEEN attempts. One `docker` that never returns outruns all of them,
// and that is the shape of the 25-minute hang in #161: whichever test
// happened to be talking to a wedged daemon is the one that hangs, which
// is why the victim was different on every run.
const (
	// dockerInfoTimeout bounds the capability probe. It is generous
	// because answering it is the daemon's first job after a cold start.
	dockerInfoTimeout = 60 * time.Second
	// dockerPullTimeout is generous for the same reason a cold machine
	// really does have to download the image. It bounds ONE attempt;
	// pullBudget below bounds the retries together.
	dockerPullTimeout = 5 * time.Minute
	// imageInspectTimeout bounds the "is this image already here?" probe.
	// That is a purely local question and the daemon answers it in
	// milliseconds, so the allowance is for a busy daemon, never for a
	// network round trip: nothing in this step is allowed to need one.
	imageInspectTimeout = 30 * time.Second
	// dockerRunTimeout covers creating and starting the container.
	dockerRunTimeout = 90 * time.Second
	// dockerProbeTimeout bounds the small, frequent calls (port, inspect,
	// logs). It is short on purpose: they run inside retry loops whose own
	// deadlines are 15 and 20 seconds.
	dockerProbeTimeout = 10 * time.Second
	// dockerRemoveTimeout bounds teardown, which must not be the reason a
	// suite hangs either.
	dockerRemoveTimeout = 60 * time.Second
	// keygenTimeout bounds one ssh-keygen, keyscanTimeout one ssh-keyscan
	// attempt inside its retry loop.
	keygenTimeout  = 60 * time.Second
	keyscanTimeout = 10 * time.Second
	// sshDialTimeout and sshHandshakeTimeout bound the two halves of one
	// readiness probe separately, because ssh.ClientConfig.Timeout covers
	// only the first of them.
	sshDialTimeout      = 2 * time.Second
	sshHandshakeTimeout = 5 * time.Second
)

// What happens when the image is genuinely missing (#243). The registry
// failures this fixture hit were all transient (a TLS handshake timeout
// twice, an auth-token context deadline once), and all three fail an
// attempt in well under a minute, so a couple of retries turn them into a
// slightly slower run instead of a red gate.
const (
	// pullAttempts is how many times the fixture asks for the image before
	// it gives up. Three, because the point is to ride out a blip and not
	// to sit through a registry outage: a run that is going to fail should
	// fail while somebody is still watching it.
	pullAttempts = 3
	// defaultPullBackoff is the base wait between attempts, multiplied by
	// the attempt number, so the gaps are 3s and then 6s. Long enough for
	// a TLS handshake or a token endpoint to recover, short enough that a
	// doomed pull is done inside ten seconds of waiting.
	defaultPullBackoff = 3 * time.Second
	// pullBackoffEnv overrides defaultPullBackoff, as a Go duration, the
	// same way budgetEnv and graceEnv override the watchdog. The fixture's
	// own tests use it to keep a deliberate retry loop short; nothing else
	// should.
	pullBackoffEnv = "RCLONE_MANAGER_SFTP_PULL_BACKOFF"
	// pullBudget bounds the retries TOGETHER, which is the part retrying a
	// five-minute timeout three times would otherwise get wrong: fifteen
	// minutes of silence is not a fix for a gate that fails on network
	// weather, it is a worse version of it. One legitimately slow first
	// download still gets its full dockerPullTimeout and simply leaves no
	// room for a retry it does not need, while a registry that is timing
	// out burns a few seconds per attempt and fits three of them easily.
	pullBudget = 6 * time.Minute
)

// The mid-test watchdog. Setup was never the gap: the gap is that once
// setup succeeds, nothing notices the server has gone, and the operation
// under test keeps retrying against a corpse until `go test` kills the
// package.
const (
	// defaultTestBudget is how long one test may run against a fixture
	// before the fixture stops it and says why. The whole SFTP suite takes
	// 94 to 98 seconds on a quiet machine, roughly ten seconds a test, so
	// four minutes is more than twenty times the usual and still a small
	// fraction of the package's 25-minute go test timeout. A stuck test
	// should name itself while the run is still worth watching.
	defaultTestBudget = 4 * time.Minute
	// defaultGrace is how long the fixture waits, after cancelling its
	// context, for the test to unwind on its own before stopping the
	// process outright. A test that takes Context() unwinds well inside
	// it; one that ignores it gets stopped anyway.
	defaultGrace = 20 * time.Second
	// budgetEnv and graceEnv override the two above, as Go durations.
	// The fixture's own tests use them to keep a deliberate hang short.
	budgetEnv = "RCLONE_MANAGER_SFTP_TEST_BUDGET"
	graceEnv  = "RCLONE_MANAGER_SFTP_DEATH_GRACE"
	// probeInterval is how often the watchdog asks docker whether the
	// container is still there.
	probeInterval = 500 * time.Millisecond
	// probesBeforeDeclaringDeath is why a single hiccup from the daemon
	// cannot fail a healthy test: the container has to be missing or
	// stopped on two consecutive probes before the fixture says it died.
	probesBeforeDeclaringDeath = 2
)

var (
	// errNoSuchContainer means docker itself no longer knows the
	// container, which is a death, not an inconclusive answer.
	errNoSuchContainer = errors.New("docker no longer knows this container")
	// errDockerTimedOut means the docker CLI did not come back at all.
	// That is a statement about the daemon, never about the container, so
	// the watchdog treats it as no evidence either way.
	errDockerTimedOut = errors.New("docker did not answer in time")
	// errTestFinished is the context cause for the ordinary path: the
	// test that owns the fixture ended.
	errTestFinished = errors.New("the test that owns this fixture has finished")
)

// Fixture is a running SFTP server plus everything a test needs to point the
// real rclone adapter at it.
type Fixture struct {
	Host string
	Port int
	User string

	// KeyFile is the private client key (ed25519, PEM, no passphrase)
	// authorized to log in as User.
	KeyFile string

	// KnownHostsFile pins the container's real host key. Using it with the
	// adapter should succeed.
	KnownHostsFile string

	// BadKnownHostsFile pins a different, unrelated key for the same
	// host:port. Using it with the adapter should fail closed.
	BadKnownHostsFile string

	// UploadDir is the host path bind-mounted onto the chroot's writable
	// upload directory. Tests seed remote files by writing here directly,
	// and observe remote deletes by checking what disappears here.
	UploadDir string

	containerID   string
	containerName string
	runDir        string

	// ctx is what Context() hands out, cancelled with a cause the moment
	// the fixture knows something is wrong.
	ctx    context.Context
	cancel context.CancelCauseFunc

	// done is closed when the owning test finishes, which is the
	// watchdog's signal to stand down. Everything below it is shared with
	// that watchdog goroutine.
	mu          sync.Mutex
	done        chan struct{}
	finished    bool
	expectDeath bool
	// stage is what Start is currently doing. It is the whole reason a
	// hang during setup can name the step it is stuck on.
	stage string

	teardownOnce sync.Once
}

// Start launches a disposable SFTP server for the duration of the calling
// test and registers cleanup. It skips (rather than fails) the test when the
// required external tools are unavailable, since this fixture is evidence
// for the embedding gate, not a requirement on every developer machine.
func Start(t *testing.T) *Fixture {
	t.Helper()

	// The fixture exists, its cleanup is registered and its watchdog is
	// running before anything can block. Every step below shells out to
	// something, and the point of #161 is that none of them may be able to
	// hang the package silently, setup included.
	f := &Fixture{
		Host: "127.0.0.1",
		User: User,
		done: make(chan struct{}),
	}
	f.ctx, f.cancel = context.WithCancelCause(context.Background())
	f.setStage("looking for docker, ssh-keygen and ssh-keyscan")
	t.Cleanup(f.finish)
	f.watch(t)

	for _, tool := range []string{"docker", "ssh-keygen", "ssh-keyscan"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("sftpfixture: SKIPPING (missing capability: %q not found on PATH): %v", tool, err)
		}
	}
	f.setStage("docker info")
	if _, errOut, err := dockerRun(dockerInfoTimeout, "info"); err != nil {
		// A daemon that is absent is a capability this machine does not
		// have, and skipping is honest. A daemon that is present but not
		// answering is not: skipping there would quietly delete the whole
		// SFTP suite from the gate, which is the failure mode #160 is
		// about. So the two get different verdicts.
		if errors.Is(err, errDockerTimedOut) {
			t.Fatalf("sftpfixture: `docker info` did not answer within %s. The daemon is there but wedged, and skipping would silently remove this suite from the gate, so this is a failure: %v\n%s", dockerInfoTimeout, err, errOut)
		}
		t.Skipf("sftpfixture: SKIPPING (missing capability: docker daemon not reachable, Docker itself appears absent or not running here): %v\n%s", err, errOut)
	}

	f.setStage("creating the run directory")
	runDir := filepath.Join(testsRoot(t), ".run", fmt.Sprintf("%s-%d", sanitize(t.Name()), time.Now().UnixNano()))
	must(t, os.MkdirAll(runDir, 0o700), "create run dir")
	f.mu.Lock()
	f.runDir = runDir
	f.mu.Unlock()

	// Two host keys are mounted deliberately, not just one. golang.org/x/crypto/ssh
	// negotiates a host-key algorithm using ITS OWN preference order (RSA-family
	// before ed25519, see ssh.supportedHostKeyAlgos), not whichever type happens to
	// be in known_hosts. atmoz/sftp's sshd_config offers both an ed25519 and an RSA
	// host key, so if we only pinned ed25519 here, rclone's sftp backend would still
	// end up negotiating RSA and fail host-key verification against a real server
	// that was configured exactly the way FR-6 asks for. Pinning both, the way a
	// real known_hosts populated by a plain `ssh-keyscan host` would, is what makes
	// this an honest test of "host-key verification works".
	f.setStage("generating host and client keys")
	hostKeyEd25519 := filepath.Join(runDir, "ssh_host_ed25519_key")
	keygenType(t, hostKeyEd25519, "ed25519", "")
	hostKeyRSA := filepath.Join(runDir, "ssh_host_rsa_key")
	keygenType(t, hostKeyRSA, "rsa", "2048")

	clientKey := filepath.Join(runDir, "id_ed25519")
	keygenType(t, clientKey, "ed25519", "")
	f.KeyFile = clientKey

	authorizedDir := filepath.Join(runDir, "authorized_keys")
	must(t, os.MkdirAll(authorizedDir, 0o755), "create authorized_keys dir")
	copyFile(t, clientKey+".pub", filepath.Join(authorizedDir, "id_ed25519.pub"))

	uploadDir := filepath.Join(runDir, "upload")
	must(t, os.MkdirAll(uploadDir, 0o777), "create upload dir")
	must(t, os.Chmod(uploadDir, 0o777), "chmod upload dir")
	f.UploadDir = uploadDir

	// Reclaim anything a previously KILLED run left behind (#150) before
	// adding one more. Once per test binary, best effort, never fatal.
	f.setStage("dockerlease.Sweep (reclaiming containers a killed run left behind)")
	dockerlease.Sweep()

	// Get the image in hand before "docker run -d", so the run step itself
	// is quiet regardless of whether this machine had it already. Belt and
	// braces alongside dockerCapture's stdout/stderr separation above: a
	// quiet run has nothing to accidentally mix into the container ID even
	// if some future docker version starts writing something else to
	// stdout during "run".
	f.ensureImage(t, serverImage)

	name := fmt.Sprintf("rclone-manager-gate-sftp-%d", time.Now().UnixNano())
	f.mu.Lock()
	f.containerName = name
	f.mu.Unlock()

	args := []string{
		"run", "-d", "--name", name,
		dockerlease.LabelFlag, dockerlease.LabelSpec,
		"-p", "127.0.0.1::22",
		"-v", hostKeyEd25519 + ":/etc/ssh/ssh_host_ed25519_key:ro",
		"-v", hostKeyEd25519 + ".pub:/etc/ssh/ssh_host_ed25519_key.pub:ro",
		"-v", hostKeyRSA + ":/etc/ssh/ssh_host_rsa_key:ro",
		"-v", hostKeyRSA + ".pub:/etc/ssh/ssh_host_rsa_key.pub:ro",
		"-v", authorizedDir + ":/home/" + User + "/.ssh/keys:ro",
		"-v", uploadDir + ":/home/" + User + "/upload",
		serverImage,
		User + "::" + containerUID + ":" + containerUID + ":upload",
	}
	f.setStage("docker run " + serverImage)
	containerID, err := dockerCapture(t, dockerRunTimeout, args...)
	if err != nil {
		t.Fatalf("sftpfixture: docker run: %v", err)
	}
	// Publishing the id is what arms the watchdog: from here on a
	// container that dies is noticed within a second, and the cleanup
	// registered above has something to remove on every exit path.
	f.mu.Lock()
	f.containerID = containerID
	f.mu.Unlock()

	f.setStage("waiting for the container to publish its ssh port")
	f.Port = waitForPublishedPort(t, containerID)

	f.setStage("ssh-keyscan for the container host keys")
	f.KnownHostsFile = filepath.Join(runDir, "known_hosts")
	keyscan(t, f.Port, f.KnownHostsFile)

	decoyKey := filepath.Join(runDir, "decoy_ed25519")
	keygen(t, decoyKey)
	f.BadKnownHostsFile = filepath.Join(runDir, "known_hosts_bad")
	writeSubstituteKnownHosts(t, f.BadKnownHostsFile, f.Port, decoyKey+".pub")

	f.setStage("waiting for sshd to accept a real session")
	waitForSSHReady(t, f)

	f.setStage("running the test body")
	return f
}

// ContainerID is the id of the docker container backing this fixture. Tests
// that need to act on the container itself (the fail-fast tests in #161 kill
// it deliberately) address it by this id, never by a `docker ps` scan: this
// machine runs many worktrees against one docker daemon, so an assertion
// that matched on a name pattern could be answered by somebody else's
// container instead of this fixture's.
func (f *Fixture) ContainerID() string { return f.containerID }

// Context returns the context every operation a test runs against this
// fixture should use, instead of context.Background().
//
// It is the fail-fast channel for #161: when the fixture notices its
// container has died, or that the test has outrun its budget, it cancels
// this context with a cause that says which of the two happened. An
// operation that takes it therefore unwinds in seconds with a legible
// reason, rather than retrying against a corpse until the package's
// 25-minute go test timeout kills everything.
func (f *Fixture) Context() context.Context { return f.ctx }

// ExpectContainerDeath tells the fixture that this test kills the container
// on purpose, so the death is evidence rather than a failure. The context
// is still cancelled with the same cause; the fixture just stops reporting
// the death as a test failure and stops stopping the process over it.
//
// Only the tests that prove the fail-fast mechanism itself should call it.
func (f *Fixture) ExpectContainerDeath() {
	f.mu.Lock()
	f.expectDeath = true
	f.mu.Unlock()
}

// ContainerDiedError is the cause Context() carries once the fixture's
// container has gone. Tests distinguish it from every other failure with
// errors.As, which is the distinction #161 asks for: a dead fixture
// container and a genuine deadlock in the transport used to look identical
// from the outside, and both cost 25 minutes.
type ContainerDiedError struct {
	// Name and ID identify the container that died.
	Name string
	ID   string
	// Removed is true when the container was gone from docker entirely,
	// rather than present but exited.
	Removed bool
	// ExitCode and OOMKilled are docker's account of how it ended, and are
	// only meaningful when Removed is false. OOMKilled is the one worth
	// looking for first: this suite runs a real sshd per fixture inside a
	// Docker VM provisioned at roughly 4 GB.
	ExitCode  int
	OOMKilled bool
	// Status is docker's own word for the state ("exited", "dead").
	Status string
	// Logs is the tail of the container's output, when it could still be
	// read.
	Logs string
}

func (e *ContainerDiedError) Error() string {
	if e.Removed {
		return fmt.Sprintf("the fixture container %s (%s) died mid-test: it was removed from docker out from under the running test", e.Name, e.ID)
	}
	oom := ""
	if e.OOMKilled {
		oom = ", OOM-killed"
	}
	logs := ""
	if e.Logs != "" {
		logs = "\ncontainer logs (tail):\n" + e.Logs
	}
	return fmt.Sprintf("the fixture container %s (%s) died mid-test: docker status %q, exit code %d%s%s", e.Name, e.ID, e.Status, e.ExitCode, oom, logs)
}

// --- the mid-test watchdog ------------------------------------------------

// ensureImage puts ref on the local daemon before anything tries to run a
// container from it, and refuses the run when it cannot.
//
// The shape here is the whole of #243. Until this existed the fixture ran
// `docker pull` unconditionally on every start, so every gate run on every
// branch, on a machine that already held the image, still needed Docker Hub
// to answer right then. Three runs in one campaign died on that: twice a
// TLS handshake timeout, once an auth token that never came. None of them
// had anything to do with the change under test, and each surfaced at the
// verdict line as a repository-structure dependency failure, because the
// deletion proof re-runs core's suite with apps/ removed and this fixture
// is inside it. A gate that fails on network weather teaches its readers to
// re-run rather than read, and that is how a genuine failure eventually
// gets waved through.
//
// So: ask the daemon first, and skip the pull when the image is already
// there. A pinned tag sitting on a local daemon does not need re-fetching
// to start a container from it.
//
// The other half matters more than the first. When the image is genuinely
// missing and cannot be fetched, this FAILS, and it must keep failing. The
// obliging version of this function skips instead, on the reasoning that a
// machine without the image cannot be expected to run the suite, and that
// version is strictly worse than the bug it replaces: it converts a loud
// environmental failure into silently missing coverage, with the gate still
// printing ok. That is the exact hole issue #160 was opened about and the
// reason `docker info` timing out a few lines above is fatal rather than a
// skip. A fixture that cannot stand up refuses.
func (f *Fixture) ensureImage(t *testing.T, ref string) {
	t.Helper()

	f.setStage("docker image inspect " + ref)
	if _, _, err := dockerRun(imageInspectTimeout, "image", "inspect", ref); err == nil {
		f.setStage(ref + " is already on this daemon, so nothing is pulled")
		return
	}

	backoff := durationFromEnv(pullBackoffEnv, defaultPullBackoff)
	deadline := time.Now().Add(pullBudget)
	started := time.Now()

	var lastErr error
	made := 0
	for attempt := 1; attempt <= pullAttempts; attempt++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		timeout := dockerPullTimeout
		if remaining < timeout {
			timeout = remaining
		}

		f.setStage(fmt.Sprintf("docker pull %s (attempt %d of %d)", ref, attempt, pullAttempts))
		made++
		_, stderr, err := dockerRun(timeout, "pull", ref)
		if err == nil {
			return
		}
		lastErr = fmt.Errorf("attempt %d of %d: %w\n%s", attempt, pullAttempts, err, stderr)

		if attempt < pullAttempts {
			time.Sleep(time.Duration(attempt) * backoff)
		}
	}

	t.Fatalf("sftpfixture: %s is not on this daemon and %d pull attempt(s) over %s could not fetch it, so this suite cannot run here.\n"+
		"That is a FAILURE and deliberately not a skip: skipping would take the whole SFTP suite out of the gate while the gate went on reporting ok, which is the silent hole #160 exists to close. Put the image on this daemon, or fix the registry access, and run again.\n"+
		"last error: %v", ref, made, time.Since(started).Round(time.Second), lastErr)
}

func (f *Fixture) setStage(stage string) {
	f.mu.Lock()
	f.stage = stage
	f.mu.Unlock()
}

func (f *Fixture) current() (id, name, stage string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containerID, f.containerName, f.stage
}

// finish is the fixture's single cleanup. It is registered before anything
// can fail, so it covers every exit path Start has, not just a clean
// return.
func (f *Fixture) finish() {
	f.mu.Lock()
	if !f.finished {
		f.finished = true
		close(f.done)
	}
	f.mu.Unlock()
	f.cancel(errTestFinished)
	f.teardown()
}

// teardown removes the container and the run directory, at most once. It is
// called from cleanup on the ordinary paths, and directly from the watchdog
// before it stops the process, because a panic raised in a goroutine other
// than the test's own never runs t.Cleanup at all. That is the leak half of
// #161: orphans found still running after 4 and 11 hours compete with the
// next run for a Docker VM that has roughly 4 GB to give, and each leak
// makes the next collapse likelier.
func (f *Fixture) teardown() {
	f.teardownOnce.Do(func() {
		f.mu.Lock()
		id, dir := f.containerID, f.runDir
		f.mu.Unlock()
		if id != "" {
			_, _, _ = dockerRun(dockerRemoveTimeout, "rm", "-f", id)
		}
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
	})
}

// watch runs for the life of the test and answers the one question the
// suite could not answer before: when this test stops making progress, is
// its fixture container dead, or is the code under test genuinely stuck?
// Those two looked identical from outside and both cost 25 minutes.
func (f *Fixture) watch(t *testing.T) {
	testName := t.Name()
	budget := durationFromEnv(budgetEnv, defaultTestBudget)
	grace := durationFromEnv(graceEnv, defaultGrace)
	deadline := time.Now().Add(budget)

	go func() {
		ticker := time.NewTicker(probeInterval)
		defer ticker.Stop()
		missing := 0
		for {
			select {
			case <-f.done:
				return
			case <-ticker.C:
			}

			if id, _, _ := f.current(); id != "" {
				st, err := inspectContainer(id)
				removed := errors.Is(err, errNoSuchContainer)
				switch {
				case err == nil && st.Running:
					missing = 0
				case err == nil, removed:
					missing++
				default:
					// Docker itself did not answer. That says nothing
					// about the container, and guessing here would fail
					// healthy tests, so it counts for neither side.
				}
				if missing >= probesBeforeDeclaringDeath {
					f.containerDied(t, testName, id, st, removed, grace)
					return
				}
			}

			if !time.Now().Before(deadline) {
				f.budgetExceeded(t, testName, budget, grace)
				return
			}
		}
	}()
}

func (f *Fixture) containerDied(t *testing.T, testName, id string, st containerState, removed bool, grace time.Duration) {
	_, name, _ := f.current()
	cause := &ContainerDiedError{
		Name:      name,
		ID:        id,
		Removed:   removed,
		ExitCode:  st.ExitCode,
		OOMKilled: st.OOMKilled,
		Status:    st.Status,
	}
	if !removed {
		cause.Logs = containerLogTail(id)
	}
	report := fmt.Sprintf("sftpfixture: %s\n\n%s failed because its fixture container died, not because of anything the code under test did. "+
		"The Docker VM on this machine has roughly 4 GB and 4 CPUs, and one ci-local.sh run starts this suite three times over, "+
		"so an eviction or an OOM under concurrent load is the first thing to check.", cause.Error(), testName)

	f.mu.Lock()
	if f.finished {
		f.mu.Unlock()
		return
	}
	expected := f.expectDeath
	if expected {
		t.Logf("sftpfixture: %s (this test killed it on purpose)", cause.Error())
	} else {
		t.Errorf("%s", report)
	}
	f.mu.Unlock()

	// Cancelling with the cause is the graceful half: an operation running
	// on Context() unwinds in about a second, and errors.As tells its
	// caller exactly which of the two possible stories this was.
	f.cancel(cause)
	if expected {
		return
	}

	select {
	case <-f.done:
	case <-time.After(grace):
		f.hardStop(report + fmt.Sprintf("\n\nThe test did not unwind within %s of its container dying, so this process is stopping now "+
			"rather than retrying against a corpse until the package's go test timeout.", grace))
	}
}

func (f *Fixture) budgetExceeded(t *testing.T, testName string, budget, grace time.Duration) {
	id, name, stage := f.current()
	verdict := fmt.Sprintf("It never reached a running container: the fixture was still at %q. That points at docker or at this host, not at the code under test.", stage)
	if id != "" {
		verdict = fmt.Sprintf("Its fixture container %s (%s) is still running, which rules out the container death in #161: "+
			"this is a genuine hang in the code under test, in the transport, the adapter or the lifecycle.", name, id)
	}
	report := fmt.Sprintf("sftpfixture: %s ran past its %s budget. %s\n\nAll goroutine stacks follow; the one worth reading is this test's own.", testName, budget, verdict)

	f.mu.Lock()
	if f.finished {
		f.mu.Unlock()
		return
	}
	t.Errorf("%s", report)
	f.mu.Unlock()

	f.cancel(errors.New(report))
	select {
	case <-f.done:
	case <-time.After(grace):
		f.hardStop(report)
	}
}

// hardStop is the last resort, for a test that ignores Context() entirely.
// It removes the container FIRST, because the panic below is raised on the
// watchdog's goroutine and t.Cleanup will never run.
func (f *Fixture) hardStop(report string) {
	// The grace timer and the test finishing can land together. Stopping a
	// process whose test has already passed would be a worse flake than the
	// one this fixture exists to remove, so the last word is the flag, not
	// the timer.
	f.mu.Lock()
	over := f.finished
	f.mu.Unlock()
	if over {
		return
	}
	f.teardown()
	debug.SetTraceback("all")
	panic(report)
}

func durationFromEnv(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// containerState is as much of docker's account of a container as the
// watchdog needs to say something useful about how it ended.
type containerState struct {
	Running   bool
	ExitCode  int
	OOMKilled bool
	Status    string
}

func inspectContainer(id string) (containerState, error) {
	out, errOut, err := dockerRun(dockerProbeTimeout, "inspect", "--format",
		"{{.State.Running}} {{.State.ExitCode}} {{.State.OOMKilled}} {{.State.Status}}", id)
	if err != nil {
		if strings.Contains(strings.ToLower(errOut), "no such") {
			return containerState{Status: "removed"}, errNoSuchContainer
		}
		return containerState{}, err
	}
	fields := strings.Fields(out)
	if len(fields) < 4 {
		return containerState{}, fmt.Errorf("unparseable docker inspect output: %q", out)
	}
	code, _ := strconv.Atoi(fields[1])
	return containerState{
		Running:   fields[0] == "true",
		ExitCode:  code,
		OOMKilled: fields[2] == "true",
		Status:    fields[3],
	}, nil
}

// containerLogTail is best effort: a container that has already been
// removed has no logs left to read, and that is not worth an error.
func containerLogTail(id string) string {
	out, errOut, err := dockerRun(dockerProbeTimeout, "logs", "--tail", "40", id)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out + "\n" + errOut)
}

// Source builds the transport.Source a real Adapter needs to reach this
// fixture, rooted at root (a path relative to UploadDir, "" for the upload
// directory itself). Every caller building this by hand duplicated the same
// seven fields; centralising it here means a future field added to
// transport.Source (or to how this fixture authenticates) only needs
// updating in one place.
//
// root is joined onto "upload", the fixed writable subdirectory this
// fixture's container exposes UploadDir as (see the atmoz/sftp arguments
// in Start: "...:upload"): the SFTP account's home directory is the
// chroot root itself, and UploadDir is not it, so a caller that passed
// root straight through as Root would have Adapter list, stat, copy and
// delete against the wrong part of the chroot entirely (including, for
// root == "", files this fixture mounts for its own purposes outside
// upload/, such as the authorized_keys directory), not the sandbox this
// method's caller actually seeded through UploadDir.
func (f *Fixture) Source(id, root string) transport.Source {
	return transport.Source{
		ID:         id,
		Type:       "sftp",
		Host:       f.Host,
		Port:       f.Port,
		User:       f.User,
		KeyFile:    f.KeyFile,
		KnownHosts: f.KnownHostsFile,
		Root:       path.Join("upload", root),
	}
}

// Deny arranges for the object at name (already written under UploadDir) to
// exist but be unreadable by the fixture's SFTP user, and returns a cleanup
// func that restores access. It implements the same shape
// internal/transport/contract.Fixtures.Deny documents, so an SFTP
// contract-style test can use it directly.
//
// Unlike a local-filesystem Deny (see transport_test.go's localFixtures),
// this needs no "running as root" guard: chmod 0 denies read access to
// whoever the SFTP session authenticates as inside the container
// (containerUID, a plain, non-root, non-CAP_DAC_OVERRIDE user), regardless
// of which user owns the file or what euid the host test process itself
// happens to run under.
func (f *Fixture) Deny(t *testing.T, name string) (cleanup func()) {
	t.Helper()
	full := filepath.Join(f.UploadDir, filepath.FromSlash(name))
	info, err := os.Stat(full)
	if err != nil {
		t.Fatalf("sftpfixture: Deny: stat %s before chmod: %v", full, err)
	}
	if err := os.Chmod(full, 0o000); err != nil {
		t.Fatalf("sftpfixture: Deny: chmod %s: %v", full, err)
	}
	original := info.Mode().Perm()
	return func() {
		_ = os.Chmod(full, original)
	}
}

// testsRoot finds the tests/ directory regardless of the caller's working
// directory (go test runs with the package under test as cwd, not tests/).
func testsRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("sftpfixture: could not determine source location")
	}
	// this file is tests/sftpfixture/fixture.go
	return filepath.Dir(filepath.Dir(file))
}

func sanitize(name string) string {
	r := strings.NewReplacer("/", "_", " ", "_")
	return r.Replace(name)
}

func must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("sftpfixture: %s: %v", what, err)
	}
}

// dockerCapture runs a docker command and returns its trimmed stdout, with
// stderr captured separately for diagnostics rather than merged in.
//
// This distinction matters specifically for "docker run -d": on a cold
// runner (or any machine without the image already cached), the image pull
// this triggers writes its progress to stderr, not stdout. CombinedOutput
// merges the two, so the "container ID" a caller reads back is actually pull
// progress followed by the ID, which then fails every later docker command
// that tries to address the container by that corrupted string ("Error
// response from daemon: page not found", exactly the failure a cold CI
// runner hits and a machine with the image already pulled never does). Only
// stdout is ever meaningful output here, so only stdout is what gets parsed.
func dockerCapture(t *testing.T, timeout time.Duration, args ...string) (stdout string, err error) {
	t.Helper()
	stdout, _, err = dockerRun(timeout, args...)
	return stdout, err
}

// dockerRun is the only place this package shells out to docker, and every
// call through it is bounded. Before #161 each one was a plain
// exec.Command with no timeout, which is why the retry loops below could
// not do what their deadlines promised: they only re-read the deadline
// between attempts, so a single call that never returned outran all of
// them and took the whole package to its go test timeout.
//
// A timeout and a non-zero exit are told apart, because they mean
// completely different things: one is a statement about the daemon, the
// other about the container.
func dockerRun(timeout time.Duration, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	stdout = strings.TrimSpace(outBuf.String())
	stderr = strings.TrimSpace(errBuf.String())

	switch {
	case ctx.Err() != nil:
		return stdout, stderr, fmt.Errorf("%w: `docker %s` was still running after %s", errDockerTimedOut, args[0], timeout)
	case runErr != nil:
		return stdout, stderr, fmt.Errorf("%w: %s", runErr, stderr)
	}
	return stdout, stderr, nil
}

func keygen(t *testing.T, path string) {
	t.Helper()
	keygenType(t, path, "ed25519", "")
}

func keygenType(t *testing.T, path, keyType, bits string) {
	t.Helper()
	args := []string{"-q", "-t", keyType, "-N", "", "-C", "", "-f", path}
	if bits != "" {
		args = append(args, "-b", bits)
	}
	ctx, cancel := context.WithTimeout(context.Background(), keygenTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh-keygen", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("sftpfixture: ssh-keygen %s (%s): %v\n%s", path, keyType, err, out)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	must(t, err, "read "+src)
	must(t, os.WriteFile(dst, data, 0o644), "write "+dst)
}

// waitForPublishedPort polls docker for the host port mapped to the
// container's SSH port. docker run -d returns before the mapping is
// necessarily queryable, so this retries briefly.
func waitForPublishedPort(t *testing.T, containerID string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		out, err := dockerCapture(t, dockerProbeTimeout, "port", containerID, "22/tcp")
		if err == nil {
			line := strings.TrimSpace(strings.Split(out, "\n")[0])
			idx := strings.LastIndex(line, ":")
			if idx >= 0 {
				if port, convErr := strconv.Atoi(line[idx+1:]); convErr == nil {
					return port
				}
			}
			lastErr = fmt.Errorf("unparseable docker port output: %q", line)
		} else {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	dumpContainerLogs(t, containerID)
	t.Fatalf("sftpfixture: container never published its ssh port: %v", lastErr)
	return 0
}

// keyscan captures the container's real host key in the exact known_hosts
// format the ssh client family (including golang.org/x/crypto/ssh/knownhosts,
// which rclone's sftp backend uses) expects for a non-standard port, instead
// of hand-building the "[host]:port" bracket syntax ourselves.
// keyscan captures every host key type the server actually offers (both the
// ed25519 and RSA keys mounted above), the same way an operator following
// FR-6 would populate known_hosts with a plain `ssh-keyscan host`. Pinning
// only one type is not enough: golang.org/x/crypto/ssh (which rclone's sftp
// backend uses) negotiates a host-key algorithm using its own preference
// order, not whichever type is in known_hosts, so it can end up asking the
// server for a key type that was never captured.
func keyscan(t *testing.T, port int, outPath string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var lastOut []byte
	var lastErr error
	for time.Now().Before(deadline) {
		var buf bytes.Buffer
		attempt, cancelAttempt := context.WithTimeout(context.Background(), keyscanTimeout)
		cmd := exec.CommandContext(attempt, "ssh-keyscan", "-p", strconv.Itoa(port), "-t", "rsa,ed25519", "127.0.0.1")
		cmd.Stdout = &buf
		lastErr = cmd.Run()
		cancelAttempt()
		lastOut = buf.Bytes()
		if lastErr == nil && bytes.Contains(lastOut, []byte("ssh-ed25519")) && bytes.Contains(lastOut, []byte("ssh-rsa")) {
			must(t, os.WriteFile(outPath, lastOut, 0o644), "write known_hosts")
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("sftpfixture: ssh-keyscan on port %d never returned both host key types: %v\n%s", port, lastErr, lastOut)
}

// writeSubstituteKnownHosts writes a known_hosts entry for host:port using a
// key that is NOT the container's real host key, so tests can prove the
// adapter refuses an impostor rather than only proving it accepts the truth.
func writeSubstituteKnownHosts(t *testing.T, outPath string, port int, pubKeyPath string) {
	t.Helper()
	pub, err := os.ReadFile(pubKeyPath)
	must(t, err, "read decoy pub key")
	fields := strings.Fields(string(pub))
	if len(fields) < 2 {
		t.Fatalf("sftpfixture: unexpected pub key format: %q", pub)
	}
	line := fmt.Sprintf("[127.0.0.1]:%d %s %s\n", port, fields[0], fields[1])
	must(t, os.WriteFile(outPath, []byte(line), 0o644), "write bad known_hosts")
}

// waitForSSHReady performs a real SSH handshake and authenticates as User,
// retrying briefly, so the fixture only hands the test a server that is
// actually ready to speak SFTP (a published port can accept TCP connections
// slightly before sshd has finished starting up).
//
// This probe intentionally does not verify the host key: its only job is to
// confirm the server is up. Host-key verification itself is exercised by the
// gate test through the real adapter, using KnownHostsFile / BadKnownHostsFile.
func waitForSSHReady(t *testing.T, f *Fixture) {
	t.Helper()
	key, err := os.ReadFile(f.KeyFile)
	must(t, err, "read client key")
	signer, err := ssh.ParsePrivateKey(key)
	must(t, err, "parse client key")

	cfg := &ssh.ClientConfig{
		User:            f.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         sshDialTimeout,
	}

	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	addr := net.JoinHostPort(f.Host, strconv.Itoa(f.Port))
	for time.Now().Before(deadline) {
		err := trySSHHandshake(addr, cfg)
		if err == nil {
			return
		}
		lastErr = err
		time.Sleep(300 * time.Millisecond)
	}
	dumpContainerLogs(t, f.containerID)
	t.Fatalf("sftpfixture: sftp server never became ready at %s: %v", addr, lastErr)
}

// trySSHHandshake bounds the handshake as well as the dial, which ssh.Dial
// does not. ssh.ClientConfig.Timeout is documented as "the maximum amount of
// time for the TCP connection to establish", and that is all it is used for:
// the version exchange and key exchange that follow in NewClientConn have no
// deadline at all.
//
// That gap is reachable on every fixture start. A published docker port
// accepts TCP the moment the mapping exists, which is before sshd inside the
// container is necessarily answering, so a peer that accepts and then says
// nothing is this fixture's ordinary startup window rather than an exotic
// case, and on a loaded host it stretches. One such attempt outlives the
// 20-second loop above, because that deadline is only re-read between
// attempts. It is the same shape as the unbounded docker calls in #161, in
// the one place left that does not shell out.
func trySSHHandshake(addr string, cfg *ssh.ClientConfig) error {
	conn, err := net.DialTimeout("tcp", addr, sshDialTimeout)
	if err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Now().Add(sshHandshakeTimeout)); err != nil {
		_ = conn.Close()
		return err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return err
	}
	// Clear the deadline before handing the connection on, so the close
	// below is not racing one that has already passed.
	_ = conn.SetDeadline(time.Time{})
	_ = ssh.NewClient(c, chans, reqs).Close()
	return nil
}

func dumpContainerLogs(t *testing.T, containerID string) {
	t.Helper()
	t.Logf("sftpfixture: container logs:\n%s", containerLogTail(containerID))
}
