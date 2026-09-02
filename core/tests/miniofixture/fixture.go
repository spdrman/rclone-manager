// Package miniofixture stands up a disposable MinIO server in a container,
// so the MediumStore contract suite can be run against something that
// speaks the real S3 API rather than against a hand-written double.
//
// It exists for the same reason sftpfixture does, and it is the same
// argument: a double answers the way its author expected, and every
// interesting thing this adapter had to get right was something a double
// would have got wrong. Writing this fixture is what turned up that a bare
// configmap gives an s3 Fs a zero chunk_size and it refuses to build at
// all, and that a wrong credential on a HEAD comes back as the code
// "Forbidden" rather than "AccessDenied", because a HEAD has no body for a
// real S3 code to arrive in. Both of those were shipped-and-wrong in a
// version of this adapter written from the documentation.
//
// Every container assertion here addresses the container by the exact id
// this fixture created, never by a `docker ps` scan: this machine runs many
// worktrees against one docker daemon, so a scan-shaped assertion could be
// answered by another agent's container and would prove nothing.
package miniofixture

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/tests/dockerlease"
)

// serverImage is pinned by name rather than by digest, matching
// sftpfixture's own choice: this is a test fixture, not a shipped artifact,
// and the supply-chain rules that govern the product's own images
// (distribution/packaging/canonical.json) are about what an operator runs.
const serverImage = "minio/minio:latest"

const (
	dockerInfoTimeout = 60 * time.Second
	dockerRunTimeout  = 120 * time.Second
	dockerExecTimeout = 30 * time.Second
	pullTimeout       = 10 * time.Minute
	readyTimeout      = 60 * time.Second
)

// Fixture is one running MinIO server and the one bucket it holds.
type Fixture struct {
	// Endpoint is what a Medium's Endpoint field is set to.
	Endpoint string
	// Bucket exists on the server before Start returns.
	Bucket string
	// Region is what the server is addressed with. MinIO does not care,
	// but rclone's s3 backend wants one.
	Region string

	// AccessKeyID and SecretAccessKey are generated fresh for every run,
	// so nothing constant and credential-shaped is ever written to this
	// repository or left on a disk between runs.
	AccessKeyID     string
	SecretAccessKey string

	// CredentialsFile is a 0600 shared-credentials file under the test's
	// own temp directory, holding the two values above. It is what the
	// `file` credential source is exercised with, which is the source
	// this product prefers and the only one whose secret never enters the
	// manager's own memory.
	CredentialsFile string

	containerID string
}

// Start brings up a MinIO server with one empty bucket and returns once it
// answers its own liveness endpoint.
//
// It SKIPS when docker is genuinely absent, and FAILS when the daemon is
// present but not answering, which is sftpfixture's distinction and it is
// the one that matters: skipping a wedged daemon would quietly delete this
// suite from the gate, which is the exact failure #160 exists to stop.
func Start(t *testing.T) *Fixture {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("miniofixture: SKIPPING (missing capability: %q not found on PATH): %v", "docker", err)
	}
	if _, errOut, err := dockerRun(dockerInfoTimeout, "info"); err != nil {
		if strings.Contains(err.Error(), "did not answer") {
			t.Fatalf("miniofixture: `docker info` did not answer within %s. The daemon is there but wedged, and skipping would silently remove this suite from the gate, so this is a failure: %v\n%s", dockerInfoTimeout, err, errOut)
		}
		t.Skipf("miniofixture: SKIPPING (missing capability: docker daemon not reachable): %v\n%s", err, errOut)
	}

	// Reclaim anything a previously KILLED run left behind (#150) before
	// adding one more.
	dockerlease.Sweep()

	if _, errOut, err := dockerRun(pullTimeout, "pull", "--quiet", serverImage); err != nil {
		t.Fatalf("miniofixture: docker pull %s: %v\n%s", serverImage, err, errOut)
	}

	f := &Fixture{
		Bucket:          "nas-backups",
		Region:          "us-east-1",
		AccessKeyID:     "canary" + randomHex(t, 8),
		SecretAccessKey: randomHex(t, 24),
	}

	name := fmt.Sprintf("rclone-manager-gate-minio-%d", time.Now().UnixNano())
	stdout, errOut, err := dockerRun(dockerRunTimeout,
		"run", "-d", "--name", name,
		dockerlease.LabelFlag, dockerlease.LabelSpec,
		"-p", "127.0.0.1::9000",
		"-e", "MINIO_ROOT_USER="+f.AccessKeyID,
		"-e", "MINIO_ROOT_PASSWORD="+f.SecretAccessKey,
		serverImage, "server", "/data",
	)
	if err != nil {
		t.Fatalf("miniofixture: docker run: %v\n%s", err, errOut)
	}
	f.containerID = strings.TrimSpace(stdout)
	t.Cleanup(func() {
		_, _, _ = dockerRun(dockerExecTimeout, "rm", "-f", f.containerID)
	})

	port, _, err := dockerRun(dockerExecTimeout, "port", f.containerID, "9000/tcp")
	if err != nil {
		t.Fatalf("miniofixture: docker port: %v", err)
	}
	hostPort := strings.TrimSpace(port)
	if i := strings.LastIndex(hostPort, ":"); i >= 0 {
		hostPort = hostPort[i+1:]
	}
	if idx := strings.IndexAny(hostPort, " \n\r"); idx >= 0 {
		hostPort = hostPort[:idx]
	}
	f.Endpoint = "http://127.0.0.1:" + hostPort

	waitUntilLive(t, f)

	// MinIO in single-drive mode reads its buckets off the drive, so a
	// directory under /data IS a bucket. Creating it this way rather than
	// through an S3 call keeps the fixture from depending on the very
	// client it exists to test, and it keeps the adapter's own
	// no_check_bucket refusal honest: nothing in the manager ever creates
	// a bucket, so something else has to.
	if _, errOut, err := dockerRun(dockerExecTimeout, "exec", f.containerID, "mkdir", "-p", "/data/"+f.Bucket); err != nil {
		t.Fatalf("miniofixture: creating the bucket: %v\n%s", err, errOut)
	}

	f.CredentialsFile = filepath.Join(t.TempDir(), "minio.creds")
	contents := "[default]\naws_access_key_id = " + f.AccessKeyID + "\naws_secret_access_key = " + f.SecretAccessKey + "\n"
	if err := os.WriteFile(f.CredentialsFile, []byte(contents), 0o600); err != nil {
		t.Fatalf("miniofixture: writing the credentials file: %v", err)
	}

	return f
}

// Medium returns a transport.Medium pointing at this fixture's own bucket,
// authenticated through the file source.
func (f *Fixture) Medium() transport.Medium {
	return f.MediumForBucket(f.Bucket)
}

// MediumForBucket is Medium against some other bucket on the same server.
func (f *Fixture) MediumForBucket(bucket string) transport.Medium {
	return transport.Medium{
		ID:          "fixture_minio",
		Type:        transport.MediumTypeS3,
		Region:      f.Region,
		Endpoint:    f.Endpoint,
		Bucket:      bucket,
		Credentials: transport.MediumCredentials{File: f.CredentialsFile},
	}
}

// NewBucket creates one more empty bucket on this server and returns a
// Medium pointing at it.
//
// It exists because the contract suite composes whole keys itself and
// reuses the same ones between cases, so its isolation requirement
// ("nothing written under a previous NewMedium may be visible through this
// one") can only be met by a fresh namespace at the bucket level. A prefix
// would not do it, because the suite never sees the prefix.
//
// The bucket is created by making a directory on the drive, not through an
// S3 call, for Start's reason: the fixture must not depend on the client it
// exists to test, and the adapter's own refusal to ever create a bucket
// means something else has to.
func (f *Fixture) NewBucket(t *testing.T) transport.Medium {
	t.Helper()
	bucket := "case-" + randomHex(t, 8)
	if _, errOut, err := dockerRun(dockerExecTimeout, "exec", f.containerID, "mkdir", "-p", "/data/"+bucket); err != nil {
		t.Fatalf("miniofixture: creating bucket %q: %v\n%s", bucket, err, errOut)
	}
	return f.MediumForBucket(bucket)
}

// ContainerID is the exact id this fixture created, for a test that needs
// to address the container itself.
func (f *Fixture) ContainerID() string { return f.containerID }

func waitUntilLive(t *testing.T, f *Fixture) {
	t.Helper()
	deadline := time.Now().Add(readyTimeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		resp, err := client.Get(f.Endpoint + "/minio/health/live")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			logs, _, _ := dockerRun(dockerExecTimeout, "logs", f.containerID)
			t.Fatalf("miniofixture: %s never answered its liveness endpoint within %s (last error: %v). Container logs:\n%s",
				f.Endpoint, readyTimeout, err, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// dockerRun runs one docker command under a hard timeout. A test helper
// must never be the reason a suite hangs, which is #161's whole lesson.
func dockerRun(timeout time.Duration, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command("docker", args...)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("starting `docker %s`: %w", args[0], err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return out.String(), errBuf.String(), err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return out.String(), errBuf.String(), fmt.Errorf("`docker %s` did not answer within %s", args[0], timeout)
	}
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("miniofixture: generating a fixture credential: %v", err)
	}
	return hex.EncodeToString(buf)
}
