// Package miniofixture stands up a disposable MinIO server in Docker so
// the MediumStore contract suite can drive the real rclone s3 backend
// against a real S3 endpoint, rather than reasoning about the API from the
// outside.
//
// It is tests/sftpfixture's sibling and follows its discipline deliberately:
// every subprocess bounded by a timeout, docker absent means SKIP but docker
// wedged means FAIL, containers labelled and swept on the way IN so a killed
// run cleans up after itself, and a watchdog that notices a container dying
// mid-test instead of letting the operation under test retry against a
// corpse until `go test` kills the package. Those rules were all paid for
// by issues #150, #160 and #161 and none of them is re-litigated here.
//
// # Why a real endpoint at all
//
// Because everything this fixture found could only be found this way. The
// adapter it tests was written against rclone's source and it was still
// wrong in four places, each of which a mock would have cheerfully agreed
// with: a delete against a mistyped bucket reported SUCCESS, a listing
// against one reported an EMPTY MEDIUM, a stat reported the artifact
// missing, and every 403 classified as Permanent because a HEAD carries no
// body for the SDK to read a real error code out of. See mediumstore.go's
// confirmBucket and errors.go's Forbidden entry.
//
// # The credentials here are not credentials
//
// MinIO's root user and password are generated fresh per run, belong to a
// container that is destroyed when the test ends, and reach nothing outside
// this machine. The shared-credentials FILE this fixture writes exists
// because the file source is a code path with a security property worth
// testing, and the only way to test it is to have a file. It lives under
// the test's own temp directory, at 0600 inside a 0700 directory, and goes
// away with the test. Same posture tests/sftpfixture takes with the SSH key
// material it generates.
package miniofixture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/tests/dockerlease"
)

// serverImage is the endpoint this fixture runs.
//
// A tag rather than a digest, for the reason tests/sftpfixture's serverImage
// gives at length: pinning a digest buys reproducibility this suite does not
// lean on, and costs every machine that already holds the tag a trip back to
// the registry. The fixture never trusts the image's contents; it verifies
// the properties it needs by using them.
const serverImage = "minio/minio:latest"

// The credentials MinIO is started with. The user is fixed because it is
// not a secret in any sense; the password is generated per run.
const rootUser = "rclonemanagertest"

// Every subprocess this fixture starts gets a timeout, because the
// deadline-bounded loops below only re-read their deadline BETWEEN
// attempts. One `docker` that never returns outruns all of them, and that
// is the shape of the 25-minute hang in #161.
const (
	dockerInfoTimeout   = 60 * time.Second
	dockerPullTimeout   = 5 * time.Minute
	imageInspectTimeout = 30 * time.Second
	dockerRunTimeout    = 90 * time.Second
	dockerProbeTimeout  = 10 * time.Second
	dockerRemoveTimeout = 60 * time.Second

	// portDeadline and readyDeadline bound the two waits after `docker
	// run` returns. MinIO answers its own health endpoint in well under a
	// second on a quiet machine; these are sized for a loaded one.
	portDeadline  = 20 * time.Second
	readyDeadline = 60 * time.Second
	pollInterval  = 200 * time.Millisecond

	// healthTimeout bounds one health probe inside the loop above.
	healthTimeout = 3 * time.Second
)

// Pull retry, matching sftpfixture's shape: ride out a registry blip
// without sitting through a registry outage.
const (
	pullAttempts       = 3
	defaultPullBackoff = 3 * time.Second
	pullBudget         = 6 * time.Minute
	pullBackoffEnv     = "RCLONE_MANAGER_MINIO_PULL_BACKOFF"
)

// The mid-test watchdog. Setup was never the gap: the gap is that once
// setup succeeds, nothing notices the server has gone.
const (
	probeInterval              = 500 * time.Millisecond
	probesBeforeDeclaringDeath = 2
)

var (
	errNoSuchContainer = errors.New("docker no longer knows this container")
	errDockerTimedOut  = errors.New("docker did not answer in time")
	errTestFinished    = errors.New("the test that owns this fixture has finished")
)

// Fixture is a running MinIO server plus everything a test needs to point
// the real rclone s3 adapter at it.
type Fixture struct {
	// Endpoint is the http://host:port this medium's Endpoint field
	// should carry.
	Endpoint string

	// Bucket is a bucket that exists on this server, pre-created before
	// the server started.
	Bucket string

	// AccessKeyID and SecretAccessKey are the throwaway root credentials.
	// They are exported so a test can build an env or command resolver's
	// payload; see CredentialsINI.
	AccessKeyID     string
	SecretAccessKey string

	// CredentialsFile is a shared-credentials file holding the above, at
	// 0600 inside a 0700 directory, for exercising the file source.
	CredentialsFile string

	containerID   string
	containerName string
	dataDir       string

	ctx    context.Context
	cancel context.CancelCauseFunc

	mu       sync.Mutex
	done     chan struct{}
	finished bool
	stage    string

	teardownOnce sync.Once
}

// Start launches a disposable MinIO server for the duration of the calling
// test and registers cleanup.
//
// It SKIPS rather than fails when docker is genuinely absent, because this
// fixture is evidence for a gate rather than a requirement on every
// developer machine, and it FAILS when docker is present but not answering,
// because skipping there would quietly delete this suite from the gate,
// which is the failure #160 is about.
func Start(t *testing.T) *Fixture {
	t.Helper()

	f := &Fixture{done: make(chan struct{})}
	f.ctx, f.cancel = context.WithCancelCause(context.Background())
	f.setStage("looking for docker")
	t.Cleanup(f.finish)
	f.watch(t)

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("miniofixture: SKIPPING (missing capability: docker not found on PATH): %v", err)
	}
	f.setStage("docker info")
	if _, errOut, err := dockerRun(dockerInfoTimeout, "info"); err != nil {
		if errors.Is(err, errDockerTimedOut) {
			t.Fatalf("miniofixture: `docker info` did not answer within %s. The daemon is there but wedged, and skipping would silently remove this suite from the gate, so this is a failure: %v\n%s", dockerInfoTimeout, err, errOut)
		}
		t.Skipf("miniofixture: SKIPPING (missing capability: docker daemon not reachable): %v\n%s", err, errOut)
	}

	f.AccessKeyID = rootUser
	f.SecretAccessKey = randomSecret(t)
	f.Bucket = "rclone-manager-test"

	// The bucket is created as a DIRECTORY under the data dir before the
	// server starts. MinIO's single-drive backend treats a top-level
	// directory as a bucket, which is what makes this fixture need no
	// second container, no mc client and no signed HTTP request just to
	// have somewhere to write. It is verified rather than assumed: the
	// contract suite's very first upload fails if the bucket is not really
	// there, and no_check_bucket means nothing will quietly create one.
	f.setStage("creating the data directory and the bucket")
	runDir := t.TempDir()
	f.dataDir = filepath.Join(runDir, "data")
	must(t, os.MkdirAll(filepath.Join(f.dataDir, f.Bucket), 0o777), "create the bucket directory")

	f.setStage("writing the shared-credentials file")
	credDir := filepath.Join(runDir, "private")
	must(t, os.MkdirAll(credDir, 0o700), "create the credentials directory")
	f.CredentialsFile = filepath.Join(credDir, "s3.credentials")
	must(t, os.WriteFile(f.CredentialsFile, []byte(f.CredentialsINI()), 0o600), "write the credentials file")

	// Reclaim anything a previously KILLED run left behind (#150) before
	// adding one more.
	f.setStage("dockerlease.Sweep (reclaiming containers a killed run left behind)")
	dockerlease.Sweep()

	f.ensureImage(t, serverImage)

	name := fmt.Sprintf("rclone-manager-gate-minio-%d", time.Now().UnixNano())
	f.mu.Lock()
	f.containerName = name
	f.mu.Unlock()

	args := []string{
		"run", "-d", "--name", name,
		dockerlease.LabelFlag, dockerlease.LabelSpec,
		"-p", "127.0.0.1::9000",
		"-e", "MINIO_ROOT_USER=" + f.AccessKeyID,
		"-e", "MINIO_ROOT_PASSWORD=" + f.SecretAccessKey,
		"-v", f.dataDir + ":/data",
		serverImage,
		"server", "/data", "--address", ":9000",
	}
	f.setStage("docker run " + serverImage)
	containerID, err := dockerCapture(t, dockerRunTimeout, args...)
	if err != nil {
		t.Fatalf("miniofixture: docker run: %v", err)
	}
	// Publishing the id is what arms the watchdog.
	f.mu.Lock()
	f.containerID = containerID
	f.mu.Unlock()

	f.setStage("waiting for the container to publish its api port")
	port := waitForPublishedPort(t, containerID)
	f.Endpoint = fmt.Sprintf("http://127.0.0.1:%d", port)

	f.setStage("waiting for MinIO to answer its health endpoint")
	f.waitForReady(t)

	f.setStage("running the test body")
	return f
}

// CredentialsINI renders this fixture's credentials in the AWS
// shared-credentials format, which is the one format all three sources
// carry.
func (f *Fixture) CredentialsINI() string {
	return fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n", f.AccessKeyID, f.SecretAccessKey)
}

// Context returns the context every operation a test runs against this
// fixture should use, instead of context.Background(). It is cancelled with
// a stated cause the moment the fixture notices its container has died, so
// an operation unwinds in seconds with a legible reason rather than
// retrying against a corpse.
func (f *Fixture) Context() context.Context { return f.ctx }

// ContainerID is the id of the container backing this fixture, for a test
// that needs to act on the container itself. Addressed by id and never by a
// `docker ps` scan, because this machine runs many worktrees against one
// daemon.
func (f *Fixture) ContainerID() string { return f.containerID }

// --- internals ---

func (f *Fixture) setStage(stage string) {
	f.mu.Lock()
	f.stage = stage
	f.mu.Unlock()
}

func (f *Fixture) currentStage() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stage
}

// watch is the mid-test watchdog. It requires two consecutive negative
// probes before declaring a death, so a single hiccup from a busy daemon
// cannot fail a healthy test, and it treats a docker call that TIMED OUT as
// no evidence either way, because that is a statement about the daemon and
// never about the container.
func (f *Fixture) watch(t *testing.T) {
	go func() {
		misses := 0
		ticker := time.NewTicker(probeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-f.done:
				return
			case <-ticker.C:
			}

			f.mu.Lock()
			id, finished := f.containerID, f.finished
			f.mu.Unlock()
			if finished {
				return
			}
			if id == "" {
				continue // not armed yet
			}

			switch err := containerAlive(id); {
			case err == nil:
				misses = 0
			case errors.Is(err, errDockerTimedOut):
				// No evidence. Do not count it.
			default:
				misses++
				if misses >= probesBeforeDeclaringDeath {
					f.cancel(fmt.Errorf("the MinIO container died while the fixture was %q: %w", f.currentStage(), err))
					return
				}
			}
		}
	}()
}

func (f *Fixture) finish() {
	f.teardownOnce.Do(func() {
		f.mu.Lock()
		f.finished = true
		id := f.containerID
		f.mu.Unlock()
		close(f.done)
		f.cancel(errTestFinished)
		if id != "" {
			_, _, _ = dockerRun(dockerRemoveTimeout, "rm", "-f", id)
		}
	})
}

func (f *Fixture) waitForReady(t *testing.T) {
	t.Helper()
	client := &http.Client{Timeout: healthTimeout}
	deadline := time.Now().Add(readyDeadline)
	var last error
	for time.Now().Before(deadline) {
		if cause := context.Cause(f.ctx); cause != nil && !errors.Is(cause, errTestFinished) {
			t.Fatalf("miniofixture: %v", cause)
		}
		req, err := http.NewRequest(http.MethodGet, f.Endpoint+"/minio/health/live", nil)
		if err != nil {
			t.Fatalf("miniofixture: building the health request: %v", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			last = fmt.Errorf("health endpoint answered %s", resp.Status)
		} else {
			last = err
		}
		time.Sleep(pollInterval)
	}
	logs, _, _ := dockerRun(dockerProbeTimeout, "logs", f.containerID)
	t.Fatalf("miniofixture: MinIO did not become ready within %s (last: %v)\ncontainer logs:\n%s", readyDeadline, last, logs)
}

func (f *Fixture) ensureImage(t *testing.T, image string) {
	t.Helper()
	f.setStage("checking for the " + image + " image")
	if _, _, err := dockerRun(imageInspectTimeout, "image", "inspect", image); err == nil {
		return
	}

	backoff := defaultPullBackoff
	if raw := os.Getenv(pullBackoffEnv); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			backoff = d
		}
	}
	budget := time.Now().Add(pullBudget)
	var last string
	for attempt := 1; attempt <= pullAttempts; attempt++ {
		if time.Now().After(budget) {
			break
		}
		f.setStage(fmt.Sprintf("docker pull %s (attempt %d/%d)", image, attempt, pullAttempts))
		_, errOut, err := dockerRun(dockerPullTimeout, "pull", image)
		if err == nil {
			return
		}
		last = fmt.Sprintf("%v\n%s", err, errOut)
		if attempt < pullAttempts {
			time.Sleep(time.Duration(attempt) * backoff)
		}
	}
	t.Skipf("miniofixture: SKIPPING (missing capability: could not obtain image %s): %s", image, last)
}

func waitForPublishedPort(t *testing.T, containerID string) int {
	t.Helper()
	deadline := time.Now().Add(portDeadline)
	var last string
	for time.Now().Before(deadline) {
		out, errOut, err := dockerRun(dockerProbeTimeout, "port", containerID, "9000/tcp")
		if err == nil {
			if port, ok := parsePublishedPort(out); ok {
				return port
			}
			last = "no parsable mapping in: " + out
		} else {
			last = fmt.Sprintf("%v\n%s", err, errOut)
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("miniofixture: container never published its api port within %s (last: %s)", portDeadline, last)
	return 0
}

// parsePublishedPort reads the port out of `docker port`'s output, which
// can carry several lines (IPv4 and IPv6) and is taken from the first one
// that parses.
func parsePublishedPort(out string) (int, bool) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		idx := strings.LastIndex(line, ":")
		if idx < 0 {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(line[idx+1:]))
		if err == nil && port > 0 {
			return port, true
		}
	}
	return 0, false
}

// containerAlive reports nil when docker still knows the container and it
// is running.
func containerAlive(id string) error {
	out, _, err := dockerRun(dockerProbeTimeout, "inspect", "--format", "{{.State.Running}}", id)
	if err != nil {
		if errors.Is(err, errDockerTimedOut) {
			return err
		}
		return errNoSuchContainer
	}
	if strings.TrimSpace(out) != "true" {
		return errNoSuchContainer
	}
	return nil
}

// dockerRun runs one docker command with a timeout, returning stdout and
// stderr separately so a caller never has to disentangle them.
func dockerRun(timeout time.Duration, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	if ctx.Err() != nil {
		return out.String(), errOut.String(), fmt.Errorf("%w: docker %s", errDockerTimedOut, strings.Join(args, " "))
	}
	return out.String(), errOut.String(), err
}

func dockerCapture(t *testing.T, timeout time.Duration, args ...string) (string, error) {
	t.Helper()
	out, errOut, err := dockerRun(timeout, args...)
	if err != nil {
		return "", fmt.Errorf("%v\n%s", err, errOut)
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return "", fmt.Errorf("docker %s produced no container id", strings.Join(args, " "))
	}
	return id, nil
}

// randomSecret generates the per-run MinIO root password. MinIO requires at
// least eight characters; this is 32 hex characters from crypto/rand, which
// is not a meaningful secret because it protects a container that lives for
// one test, but is generated rather than fixed so two concurrent runs on
// one machine never share one.
func randomSecret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("miniofixture: generating the root password: %v", err)
	}
	return hex.EncodeToString(b)
}

func must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("miniofixture: %s: %v", what, err)
	}
}
