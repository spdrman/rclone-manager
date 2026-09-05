// Package machines stands up a disposable MinIO server in a container,
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
package machines

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/tests/dockerlease"
)

// minioImage is pinned by name rather than by digest, matching
// sftpfixture's own choice: this is a test fixture, not a shipped artifact,
// and the supply-chain rules that govern the product's own images
// (distribution/packaging/canonical.json) are about what an operator runs.
const minioImage = "minio/minio:latest"

const (
	minioRunTimeout   = 120 * time.Second
	minioPullTimeout  = 10 * time.Minute
	minioReadyTimeout = 60 * time.Second
)

// Medium is one running MinIO server and the one bucket it holds.
type Medium struct {
	// Endpoint is what a Medium's Endpoint field is set to.
	Endpoint string
	// Bucket exists on the server before Start returns.
	Bucket string
	// Region is what the server is addressed with. MinIO does not care,
	// but rclone's s3 backend wants one.
	Region string

	// Alias is the name other containers on the network reach this medium
	// by.
	Alias string

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

// mediumOptions is how core/tests/machines places this fixture on a network
// (issue #447). The zero value is what Start has always done: no network,
// a port published on 127.0.0.1.
type mediumOptions struct {
	// Network, when set, is a docker network the container joins.
	Network string
	// Alias is the name the container answers to on Network.
	Alias string
	// InNetwork says the test process is itself a container on Network,
	// so the server is reached by Alias on port 9000 and no host port is
	// published.
	InNetwork bool
}

// startMedium is Start with a placement. New tests should reach a medium
// through core/tests/machines rather than calling this directly.
func startMedium(t *testing.T, opts mediumOptions) *Medium {
	t.Helper()
	if opts.InNetwork && (opts.Network == "" || opts.Alias == "") {
		t.Fatalf("machines: mediumOptions.InNetwork needs both Network and Alias, because the server is reached by its alias on that network")
	}

	// No docker probe and no lease sweep here: Start did both before this
	// package would hand anybody a Machines to ask for a medium, and
	// repeating them would cost a `docker info` per medium for an answer
	// that cannot have changed.

	// Ask the daemon first and pull only when the image is missing, which
	// is sftpfixture.ensureImage's shape (#243): a gate that reaches a
	// registry on every run fails on network weather that has nothing to
	// do with the change under test. A missing image that cannot be
	// pulled stays a failure, never a skip.
	if _, _, err := dockerRun(dockerExecTimeout, "image", "inspect", minioImage); err != nil {
		if _, errOut, err := dockerRun(minioPullTimeout, "pull", "--quiet", minioImage); err != nil {
			t.Fatalf("machines: %s is not on this daemon and could not be pulled, so this suite cannot run here. That is a FAILURE and deliberately not a skip (#160): %v\n%s", minioImage, err, errOut)
		}
	}

	f := &Medium{
		Bucket:          "nas-backups",
		Region:          "us-east-1",
		AccessKeyID:     "canary" + randomHex(t, 8),
		SecretAccessKey: randomHex(t, 24),
	}

	name := fmt.Sprintf("rclone-manager-gate-minio-%d", time.Now().UnixNano())
	args := []string{
		"run", "-d", "--name", name,
		dockerlease.LabelFlag, dockerlease.LabelSpec,
	}
	if opts.Network != "" {
		args = append(args, "--network", opts.Network)
		if opts.Alias != "" {
			args = append(args, "--network-alias", opts.Alias)
		}
	}
	if !opts.InNetwork {
		args = append(args, "-p", "127.0.0.1::9000")
	}
	args = append(args,
		"-e", "MINIO_ROOT_USER="+f.AccessKeyID,
		"-e", "MINIO_ROOT_PASSWORD="+f.SecretAccessKey,
		minioImage, "server", "/data",
	)
	stdout, errOut, err := dockerRun(minioRunTimeout, args...)
	if err != nil {
		t.Fatalf("machines: docker run: %v\n%s", err, errOut)
	}
	f.containerID = strings.TrimSpace(stdout)
	t.Cleanup(func() {
		_, _, _ = dockerRun(dockerExecTimeout, "rm", "-f", f.containerID)
	})

	if opts.InNetwork {
		f.Endpoint = "http://" + opts.Alias + ":9000"
	} else {
		port, _, err := dockerRun(dockerExecTimeout, "port", f.containerID, "9000/tcp")
		if err != nil {
			t.Fatalf("machines: docker port: %v", err)
		}
		hostPort := strings.TrimSpace(port)
		if i := strings.LastIndex(hostPort, ":"); i >= 0 {
			hostPort = hostPort[i+1:]
		}
		if idx := strings.IndexAny(hostPort, " \n\r"); idx >= 0 {
			hostPort = hostPort[:idx]
		}
		f.Endpoint = "http://127.0.0.1:" + hostPort
	}

	waitUntilLive(t, f)

	// MinIO in single-drive mode reads its buckets off the drive, so a
	// directory under /data IS a bucket. Creating it this way rather than
	// through an S3 call keeps the fixture from depending on the very
	// client it exists to test, and it keeps the adapter's own
	// no_check_bucket refusal honest: nothing in the manager ever creates
	// a bucket, so something else has to.
	if _, errOut, err := dockerRun(dockerExecTimeout, "exec", f.containerID, "mkdir", "-p", "/data/"+f.Bucket); err != nil {
		t.Fatalf("machines: creating the bucket: %v\n%s", err, errOut)
	}

	// The `file` source reaches rclone through the AWS credential CHAIN,
	// and an ambient AWS_* variable on the machine running the gate would
	// outrank the file this fixture is about to write. The adapter now
	// refuses that outright (see mediumcreds.go's
	// refuseAmbientAWSCredentialEnvironment), which is the right
	// behaviour and would also make this suite's result depend on whoever
	// happens to have AWS_PROFILE exported. So the fixture clears them
	// for the duration of the test, which is what makes the file source
	// testable here at all.
	for _, name := range ambientAWSEnvVars {
		t.Setenv(name, "")
	}

	f.CredentialsFile = filepath.Join(t.TempDir(), "minio.creds")
	contents := "[default]\naws_access_key_id = " + f.AccessKeyID + "\naws_secret_access_key = " + f.SecretAccessKey + "\n"
	if err := os.WriteFile(f.CredentialsFile, []byte(contents), 0o600); err != nil {
		t.Fatalf("machines: writing the credentials file: %v", err)
	}

	return f
}

// Medium returns a transport.Medium pointing at this fixture's own bucket,
// authenticated through the file source.
func (f *Medium) Medium() transport.Medium {
	return f.MediumForBucket(f.Bucket)
}

// MediumForBucket is Medium against some other bucket on the same server.
func (f *Medium) MediumForBucket(bucket string) transport.Medium {
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
func (f *Medium) NewBucket(t *testing.T) transport.Medium {
	t.Helper()
	bucket := "case-" + randomHex(t, 8)
	if _, errOut, err := dockerRun(dockerExecTimeout, "exec", f.containerID, "mkdir", "-p", "/data/"+bucket); err != nil {
		t.Fatalf("machines: creating bucket %q: %v\n%s", bucket, err, errOut)
	}
	return f.MediumForBucket(bucket)
}

// ContainerID is the exact id this fixture created, for a test that needs
// to address the container itself.
func (f *Medium) ContainerID() string { return f.containerID }

func waitUntilLive(t *testing.T, f *Medium) {
	t.Helper()
	deadline := time.Now().Add(minioReadyTimeout)
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
			t.Fatalf("machines: %s never answered its liveness endpoint within %s (last error: %v). Container logs:\n%s",
				f.Endpoint, minioReadyTimeout, err, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("machines: generating a fixture credential: %v", err)
	}
	return hex.EncodeToString(buf)
}

// ambientAWSEnvVars mirrors internal/transport/rclone's own list. It is
// duplicated rather than exported from there because exporting it would
// put a refusal's implementation detail on a production package's surface
// for a test fixture's convenience, and because the two lists serving the
// same purpose from opposite sides is exactly what
// TestTheFileSourceRefusesAnAmbientAWSEnvironment would notice if they
// drifted: a variable added there and missed here makes this suite refuse
// on a machine that has it set, loudly, which is the failure that gets
// fixed rather than the one that gets ignored.
var ambientAWSEnvVars = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_ACCESS_KEY",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SECRET_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_PROFILE",
	"AWS_DEFAULT_PROFILE",
	"AWS_SHARED_CREDENTIALS_FILE",
	"AWS_CONFIG_FILE",
	"AWS_WEB_IDENTITY_TOKEN_FILE",
	"AWS_ROLE_ARN",
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	"AWS_CONTAINER_CREDENTIALS_FULL_URI",
}
