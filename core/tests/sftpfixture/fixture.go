// Package sftpfixture stands up a disposable SFTP server in Docker so the
// Phase-1 gate tests can drive the real rclone sftp backend against a real
// server, rather than reasoning about the API from the outside.
//
// It uses atmoz/sftp (OpenSSH's sshd, chrooted, forced into internal-sftp)
// because that gives us a genuine SSH/SFTP endpoint with real host-key
// verification and real chroot/permission semantics, for the cost of a
// disposable container. All key material is generated fresh per test run
// under tests/.run and removed on cleanup; nothing here is a real credential.
package sftpfixture

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/tests/dockerlease"
)

// User is the fixed username created inside the container.
const User = "backupuser"

const containerUID = "1001"

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

	containerID string
	runDir      string
}

// Start launches a disposable SFTP server for the duration of the calling
// test and registers cleanup. It skips (rather than fails) the test when the
// required external tools are unavailable, since this fixture is evidence
// for the embedding gate, not a requirement on every developer machine.
func Start(t *testing.T) *Fixture {
	t.Helper()

	for _, tool := range []string{"docker", "ssh-keygen", "ssh-keyscan"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("sftpfixture: SKIPPING (missing capability: %q not found on PATH): %v", tool, err)
		}
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("sftpfixture: SKIPPING (missing capability: docker daemon not reachable, Docker itself appears absent or not running here): %v\n%s", err, out)
	}

	runDir := filepath.Join(testsRoot(t), ".run", fmt.Sprintf("%s-%d", sanitize(t.Name()), time.Now().UnixNano()))
	must(t, os.MkdirAll(runDir, 0o700), "create run dir")

	f := &Fixture{
		Host:   "127.0.0.1",
		User:   User,
		runDir: runDir,
	}

	// Two host keys are mounted deliberately, not just one. golang.org/x/crypto/ssh
	// negotiates a host-key algorithm using ITS OWN preference order (RSA-family
	// before ed25519, see ssh.supportedHostKeyAlgos), not whichever type happens to
	// be in known_hosts. atmoz/sftp's sshd_config offers both an ed25519 and an RSA
	// host key, so if we only pinned ed25519 here, rclone's sftp backend would still
	// end up negotiating RSA and fail host-key verification against a real server
	// that was configured exactly the way FR-6 asks for. Pinning both, the way a
	// real known_hosts populated by a plain `ssh-keyscan host` would, is what makes
	// this an honest test of "host-key verification works".
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

	name := fmt.Sprintf("rclone-manager-gate-sftp-%d", time.Now().UnixNano())

	// Pull explicitly, before "docker run -d", so the run step itself is
	// quiet regardless of whether the image was already cached on this
	// machine. Belt and braces alongside dockerCapture's stdout/stderr
	// separation above: a quiet run has nothing to accidentally mix into the
	// container ID even if some future docker version starts writing
	// something else to stdout during "run".
	// Reclaim anything a previously KILLED run left behind (#150) before
	// adding one more. Once per test binary, best effort, never fatal.
	dockerlease.Sweep()

	if _, err := dockerCapture(t, "pull", "atmoz/sftp:alpine"); err != nil {
		t.Fatalf("sftpfixture: docker pull atmoz/sftp:alpine: %v", err)
	}

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
		"atmoz/sftp:alpine",
		User + "::" + containerUID + ":" + containerUID + ":upload",
	}
	containerID, err := dockerCapture(t, args...)
	if err != nil {
		os.RemoveAll(runDir)
		t.Fatalf("sftpfixture: docker run: %v", err)
	}
	f.containerID = containerID

	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", f.containerID).Run()
		_ = os.RemoveAll(f.runDir)
	})

	f.Port = waitForPublishedPort(t, f.containerID)

	f.KnownHostsFile = filepath.Join(runDir, "known_hosts")
	keyscan(t, f.Port, f.KnownHostsFile)

	decoyKey := filepath.Join(runDir, "decoy_ed25519")
	keygen(t, decoyKey)
	f.BadKnownHostsFile = filepath.Join(runDir, "known_hosts_bad")
	writeSubstituteKnownHosts(t, f.BadKnownHostsFile, f.Port, decoyKey+".pub")

	waitForSSHReady(t, f)

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
func (f *Fixture) Context() context.Context { return context.Background() }

// ExpectContainerDeath tells the fixture that this test kills the container
// on purpose, so the death is evidence rather than a failure. The context
// is still cancelled with the same cause; the fixture just stops reporting
// the death as a test failure and stops stopping the process over it.
//
// Only the tests that prove the fail-fast mechanism itself should call it.
func (f *Fixture) ExpectContainerDeath() {}

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
func dockerCapture(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	cmd := exec.Command("docker", args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	stdout = strings.TrimSpace(outBuf.String())
	if err != nil {
		return stdout, fmt.Errorf("%w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return stdout, nil
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
	out, err := exec.Command("ssh-keygen", args...).CombinedOutput()
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
		out, err := dockerCapture(t, "port", containerID, "22/tcp")
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
		cmd := exec.Command("ssh-keyscan", "-p", strconv.Itoa(port), "-t", "rsa,ed25519", "127.0.0.1")
		cmd.Stdout = &buf
		lastErr = cmd.Run()
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
		Timeout:         2 * time.Second,
	}

	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	addr := net.JoinHostPort(f.Host, strconv.Itoa(f.Port))
	for time.Now().Before(deadline) {
		client, err := ssh.Dial("tcp", addr, cfg)
		if err == nil {
			_ = client.Close()
			return
		}
		lastErr = err
		time.Sleep(300 * time.Millisecond)
	}
	dumpContainerLogs(t, f.containerID)
	t.Fatalf("sftpfixture: sftp server never became ready at %s: %v", addr, lastErr)
}

func dumpContainerLogs(t *testing.T, containerID string) {
	t.Helper()
	out, _ := exec.Command("docker", "logs", containerID).CombinedOutput()
	t.Logf("sftpfixture: container logs:\n%s", out)
}
