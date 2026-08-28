package rclone

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/spdrman/rclone-manager/internal/transport"
)

// ---------------------------------------------------------------------------
// Unit tests: the sftpConfig validation and allowlist, no Docker required.
// ---------------------------------------------------------------------------

// touchFile creates an empty file at dir/name and returns its path. sftpConfig
// only ever checks that key_file and known_hosts exist and are readable, it
// never looks at their contents, so an empty placeholder is enough for these
// tests.
func touchFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("placeholder"), 0o600); err != nil {
		t.Fatalf("touchFile(%s): %v", p, err)
	}
	return p
}

func validSource(t *testing.T, dir string) transport.Source {
	t.Helper()
	return transport.Source{
		ID:         "test-source",
		Type:       "sftp",
		Host:       "production.example.internal",
		Port:       22,
		User:       "backup",
		KeyFile:    touchFile(t, dir, "id_ed25519"),
		KnownHosts: touchFile(t, dir, "known_hosts"),
		Root:       "/backups",
	}
}

func TestSftpConfig_RequiresHost(t *testing.T) {
	src := validSource(t, t.TempDir())
	src.Host = ""
	if _, err := sftpConfig(src); err == nil {
		t.Fatal("expected an error for a missing host, got nil")
	}
}

func TestSftpConfig_RequiresUser(t *testing.T) {
	src := validSource(t, t.TempDir())
	src.User = ""
	if _, err := sftpConfig(src); err == nil {
		t.Fatal("expected an error for a missing user, got nil")
	}
}

// TestSftpConfig_RequiresKeyFile is the FR-6 "SSH key authentication by
// default" test. An empty key_file is not a valid "use whatever auth is
// available" configuration: rclone's sftp backend would fall back to an
// ssh-agent in that case, which is a real authentication path this adapter
// must not open by omission.
func TestSftpConfig_RequiresKeyFile(t *testing.T) {
	src := validSource(t, t.TempDir())
	src.KeyFile = ""
	_, err := sftpConfig(src)
	if err == nil {
		t.Fatal("expected an error for a missing key_file, got nil")
	}
	if !strings.Contains(err.Error(), "key_file") {
		t.Errorf("error should mention key_file, got: %v", err)
	}
}

func TestSftpConfig_KeyFileMustExist(t *testing.T) {
	dir := t.TempDir()
	src := validSource(t, dir)
	src.KeyFile = filepath.Join(dir, "does-not-exist")
	if _, err := sftpConfig(src); err == nil {
		t.Fatal("expected an error for a key_file that does not exist, got nil")
	}
}

// TestSftpConfig_RequiresKnownHosts is the core FR-6 test: rclone's own
// default, reached whenever known_hosts_file is left unset, is
// ssh.InsecureIgnoreHostKey(), which accepts any host key at all. This
// adapter must never build a config that reaches that default.
func TestSftpConfig_RequiresKnownHosts(t *testing.T) {
	src := validSource(t, t.TempDir())
	src.KnownHosts = ""
	_, err := sftpConfig(src)
	if err == nil {
		t.Fatal("expected an error for a missing known_hosts, got nil")
	}
	if !strings.Contains(err.Error(), "known_hosts") {
		t.Errorf("error should mention known_hosts, got: %v", err)
	}
}

// TestSftpConfig_RejectsNoneKnownHosts covers the other way to reach
// rclone's insecure default: the sftp backend treats the literal string
// "none" as an explicit request to disable host-key checking (it still calls
// ssh.InsecureIgnoreHostKey(), it just stops logging about it). If this
// adapter forwarded that value unchanged, a typo'd or copy-pasted "none" in
// configuration would silently switch verification off.
func TestSftpConfig_RejectsNoneKnownHosts(t *testing.T) {
	for _, value := range []string{"none", "None", "NONE", " none ", "NoNe"} {
		t.Run(value, func(t *testing.T) {
			src := validSource(t, t.TempDir())
			src.KnownHosts = value
			if _, err := sftpConfig(src); err == nil {
				t.Fatalf("expected known_hosts=%q to be refused, it was accepted", value)
			}
		})
	}
}

func TestSftpConfig_KnownHostsMustExist(t *testing.T) {
	dir := t.TempDir()
	src := validSource(t, dir)
	src.KnownHosts = filepath.Join(dir, "does-not-exist")
	if _, err := sftpConfig(src); err == nil {
		t.Fatal("expected an error for a known_hosts file that does not exist, got nil")
	}
}

func TestSftpConfig_KnownHostsMustBeFile(t *testing.T) {
	dir := t.TempDir()
	src := validSource(t, dir)
	src.KnownHosts = dir // a directory, not a file
	if _, err := sftpConfig(src); err == nil {
		t.Fatal("expected an error for known_hosts pointing at a directory, got nil")
	}
}

// TestSftpConfig_PortOmittedWhenZero checks the config only carries a "port"
// entry when one was actually configured, matching the previous adapter
// behaviour this refactor preserves.
func TestSftpConfig_PortOmittedWhenZero(t *testing.T) {
	dir := t.TempDir()
	src := validSource(t, dir)
	src.Port = 0
	cfg, err := sftpConfig(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := cfg.Get("port"); ok {
		t.Error("expected no port entry when Port is 0")
	}
}

// TestSftpConfig_OnlyAllowlistedKeysAreSet is the structural proof behind
// "password authentication should not be silently reachable". It does not
// just check that a "pass" key is absent today, it pins the entire set of
// keys sftpConfig is allowed to produce, so an unreviewed future change that
// starts forwarding an option this adapter does not already know about
// (password, key_pem, ask_password, key_use_agent, pin_host_key, host_keys,
// the external ssh option, ...) breaks this test rather than shipping
// silently.
func TestSftpConfig_OnlyAllowlistedKeysAreSet(t *testing.T) {
	dir := t.TempDir()
	src := validSource(t, dir)
	cfg, err := sftpConfig(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	allowed := map[string]bool{
		"host":             true,
		"port":             true,
		"user":             true,
		"key_file":         true,
		"known_hosts_file": true,
	}
	for k := range cfg {
		if !allowed[k] {
			t.Errorf("sftpConfig set unexpected key %q; every key this function can set must be reviewed for FR-6 impact", k)
		}
	}

	// And the values that matter for FR-6 are exactly what was configured,
	// not silently substituted or defaulted to something looser.
	if v, _ := cfg.Get("key_file"); v != src.KeyFile {
		t.Errorf("key_file = %q, want %q", v, src.KeyFile)
	}
	if v, _ := cfg.Get("known_hosts_file"); v != src.KnownHosts {
		t.Errorf("known_hosts_file = %q, want %q", v, src.KnownHosts)
	}
}

// ---------------------------------------------------------------------------
// Integration test: a real Docker SFTP server, attacked.
//
// A happy-path test only proves that a correct known_hosts entry is accepted.
// It says nothing about whether verification is actually load-bearing, since
// a backend that never checks anything would pass it too. The tests below
// stand up a disposable OpenSSH/SFTP server in Docker, record its host key
// the way an operator would (connect once, capture the key it presented),
// and then prove two attacks are refused:
//
//   - a server at an address this adapter has never seen before (unknown
//     host key);
//   - the same server address now answering with a different host key
//     (changed host key, i.e. a MITM or an unnoticed server replacement).
//
// Every "docker run" of the fixture image generates its own fresh SSH host
// keys at container start (nothing is baked into the image, nothing is
// persisted), so two separate containers from the same image reliably
// present two different host keys. That is what makes the "changed key" case
// reproducible without hand-crafting key material.
// ---------------------------------------------------------------------------

// sftpFixtureDockerfile builds a minimal Alpine sshd configured as a
// restricted, key-only SFTP endpoint: no password/keyboard-interactive
// login, chrooted to the backup user's home directory, sftp-only via
// ForceCommand. This mirrors the restricted-account posture docs/ssh-setup.md
// asks a real deployment for, though it exists here purely as a test
// fixture, not as guidance.
const sftpFixtureDockerfile = `FROM alpine:3.20

RUN apk add --no-cache openssh-server openssh-sftp-server \
 && mkdir -p /var/run/sshd \
 && addgroup -S backup \
 && adduser -S -G backup -h /home/backup -s /sbin/nologin backup \
 && mkdir -p /home/backup/.ssh /home/backup/upload \
 && chown root:root /home/backup \
 && chmod 755 /home/backup \
 && chown backup:backup /home/backup/upload \
 && chown backup:backup /home/backup/.ssh \
 && chmod 700 /home/backup/.ssh \
 && passwd -u backup

COPY authorized_keys /home/backup/.ssh/authorized_keys
RUN chown backup:backup /home/backup/.ssh/authorized_keys && chmod 600 /home/backup/.ssh/authorized_keys

COPY sshd_config /etc/ssh/sshd_config

EXPOSE 22

# No host key is baked into the image and none is persisted across restarts,
# so every container start gets a brand new identity.
CMD ["sh", "-c", "ssh-keygen -A && exec /usr/sbin/sshd -D -e"]
`

const sftpFixtureSSHDConfig = `Port 22
HostKey /etc/ssh/ssh_host_ed25519_key

PubkeyAuthentication yes
PasswordAuthentication no
ChallengeResponseAuthentication no
KbdInteractiveAuthentication no
UsePAM no
PermitRootLogin no
AuthorizedKeysFile /home/backup/.ssh/authorized_keys

Subsystem sftp internal-sftp

Match User backup
    ChrootDirectory /home/backup
    ForceCommand internal-sftp
    AllowTcpForwarding no
    X11Forwarding no
`

const sftpFixtureImageTag = "rclone-manager-sftp-fixture:test"

// requireDocker skips the test when Docker is not available, so this file
// stays runnable in environments without it. Where Docker is present, this
// test runs for real: it is the whole point of this file.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH, skipping SFTP host-key integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not reachable, skipping SFTP host-key integration test: %v: %s", err, out)
	}
}

// generateClientSSHKeyPair creates a throwaway ed25519 key pair for the test
// client identity. It is generated fresh for each test run, lives only under
// the test's temp directory, and authenticates against a disposable
// container that is destroyed at the end of the test: there is no real
// secret here to leak or to commit.
func generateClientSSHKeyPair(t *testing.T) (privateKeyPath string, authorizedKeyLine string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh.NewPublicKey: %v", err)
	}
	authorizedKeyLine = string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(sshPub)))

	block, err := ssh.MarshalPrivateKey(priv, "rclone-manager-sftp-test-client")
	if err != nil {
		t.Fatalf("ssh.MarshalPrivateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(block)

	privateKeyPath = filepath.Join(t.TempDir(), "client_ed25519")
	if err := os.WriteFile(privateKeyPath, pemBytes, 0o600); err != nil {
		t.Fatalf("writing client private key: %v", err)
	}
	return privateKeyPath, authorizedKeyLine
}

// buildSFTPFixtureImage builds the disposable sshd image used by every
// subtest below, baking in the given client's authorized_keys entry.
func buildSFTPFixtureImage(t *testing.T, authorizedKeyLine string) string {
	t.Helper()
	dir := t.TempDir()
	writeMust := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	writeMust("Dockerfile", sftpFixtureDockerfile)
	writeMust("sshd_config", sftpFixtureSSHDConfig)
	writeMust("authorized_keys", authorizedKeyLine+"\n")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", sftpFixtureImageTag, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker build failed: %v\n%s", err, out)
	}
	return sftpFixtureImageTag
}

// containerNameFor derives a Docker-safe container name from the running
// test's name, so parallel or repeated runs do not collide.
func containerNameFor(t *testing.T, label string) string {
	t.Helper()
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			return r
		default:
			return '-'
		}
	}, t.Name())
	return fmt.Sprintf("rclone-manager-sftp-test-%s-%s-%d", safe, label, time.Now().UnixNano())
}

// startFixtureContainer starts a fresh instance of the fixture image
// publishing container port 22 on 127.0.0.1:hostPort, and returns once its
// sshd is confirmed listening and its host key confirmed generated. It
// retries the docker-run itself briefly on a port-in-use error, since a
// container stopped moments earlier by this same test can take a beat to
// fully release its port.
func startFixtureContainer(t *testing.T, image string, hostPort int, label string) (containerID, hostKeyLine string) {
	t.Helper()
	name := containerNameFor(t, label)

	var out []byte
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		cmd := exec.Command("docker", "run", "-d", "--name", name,
			"-p", fmt.Sprintf("127.0.0.1:%d:22", hostPort), image)
		out, err = cmd.CombinedOutput()
		if err == nil {
			break
		}
		msg := string(out)
		if !strings.Contains(msg, "port is already allocated") && !strings.Contains(msg, "address already in use") {
			t.Fatalf("docker run failed: %v\n%s", err, out)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("docker run failed after retries: %v\n%s", err, out)
	}
	containerID = strings.TrimSpace(string(out))
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", containerID).Run()
	})

	hostKeyLine = waitForFixtureReady(t, containerID, hostPort)
	return containerID, hostKeyLine
}

// stopFixtureContainer removes a container immediately, so its host port is
// free for the next container in the same test to reuse. (The container's
// own t.Cleanup still runs at test end; removing an already-removed
// container there is a harmless no-op.)
func stopFixtureContainer(containerID string) {
	_ = exec.Command("docker", "rm", "-f", containerID).Run()
}

// waitForFixtureReady polls until the container has generated its host key
// and sshd is accepting TCP connections, then returns the ed25519 host
// public key line (as produced by ssh-keygen, e.g. "ssh-ed25519 AAAA... comment").
func waitForFixtureReady(t *testing.T, containerID string, hostPort int) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "exec", containerID, "cat", "/etc/ssh/ssh_host_ed25519_key.pub").CombinedOutput()
		if err != nil {
			lastErr = fmt.Errorf("reading host key: %w: %s", err, out)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		line := strings.TrimSpace(string(out))
		if line == "" {
			lastErr = fmt.Errorf("host key file was empty")
			time.Sleep(200 * time.Millisecond)
			continue
		}
		conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort), 500*time.Millisecond)
		if dialErr != nil {
			lastErr = dialErr
			time.Sleep(200 * time.Millisecond)
			continue
		}
		_ = conn.Close()
		return line
	}
	logs, _ := exec.Command("docker", "logs", containerID).CombinedOutput()
	t.Fatalf("sftp fixture container %s never became ready: %v\ncontainer logs:\n%s", containerID, lastErr, logs)
	return ""
}

// writeKnownHosts writes a single known_hosts entry, in the exact format
// rclone's sftp backend parses (via golang.org/x/crypto/ssh/knownhosts, the
// same library that produces the "key mismatch" / "key is unknown" errors
// this test asserts on), for host:port -> the given ssh-keygen public key
// line.
func writeKnownHosts(t *testing.T, path, host string, port int, hostKeyLine string) {
	t.Helper()
	pubKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hostKeyLine))
	if err != nil {
		t.Fatalf("parsing host key line %q: %v", hostKeyLine, err)
	}
	addr := knownhosts.Normalize(fmt.Sprintf("%s:%d", host, port))
	line := knownhosts.Line([]string{addr}, pubKey)
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("writing known_hosts: %v", err)
	}
}

func TestSFTPHostKeyVerification(t *testing.T) {
	requireDocker(t)

	clientKeyPath, authorizedKeyLine := generateClientSSHKeyPair(t)
	image := buildSFTPFixtureImage(t, authorizedKeyLine)

	host := "127.0.0.1"
	port := freeTCPPort(t)
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")

	contA, hostKeyA := startFixtureContainer(t, image, port, "server-a")
	writeKnownHosts(t, knownHostsPath, host, port, hostKeyA)

	src := transport.Source{
		ID:         "sftp-fixture",
		Type:       "sftp",
		Host:       host,
		Port:       port,
		User:       "backup",
		KeyFile:    clientKeyPath,
		KnownHosts: knownHostsPath,
	}

	adapter := New()
	ctx := context.Background()

	// Positive control: prove the harness itself is sound before trusting
	// any refusal below. If this fails, the "attacks" prove nothing, since
	// they could just as well be failing for an unrelated reason (wrong
	// port, wrong user, key mismatch, sshd misconfigured).
	t.Run("recorded host key with the configured SSH key succeeds", func(t *testing.T) {
		if _, err := adapter.List(ctx, src); err != nil {
			t.Fatalf("List against the recorded host key should have succeeded, got: %v", err)
		}
	})

	stopFixtureContainer(contA)

	t.Run("unknown host key is refused", func(t *testing.T) {
		unknownPort := freeTCPPort(t)
		contU, _ := startFixtureContainer(t, image, unknownPort, "server-unknown")
		defer stopFixtureContainer(contU)

		unknownSrc := src
		unknownSrc.Port = unknownPort // known_hosts has no entry for this host:port at all

		_, err := adapter.List(ctx, unknownSrc)
		if err == nil {
			t.Fatal("List against a host with no known_hosts entry should have been refused, it succeeded")
		}
		if !strings.Contains(err.Error(), "knownhosts: key is unknown") {
			t.Fatalf("expected an unknown-host-key error, got: %v", err)
		}
	})

	t.Run("changed host key is refused (MITM)", func(t *testing.T) {
		// Same host:port as the one recorded in known_hosts, but a freshly
		// started container, so a freshly generated, different host key:
		// exactly the shape of a MITM, or a server quietly replaced.
		contB, hostKeyB := startFixtureContainer(t, image, port, "server-b")
		defer stopFixtureContainer(contB)

		if hostKeyB == hostKeyA {
			t.Fatal("test setup bug: server B generated the same host key as server A, so this proves nothing")
		}

		_, err := adapter.List(ctx, src) // src is unchanged: same host:port, known_hosts still pinned to A's key
		if err == nil {
			t.Fatal("List against a changed host key should have been refused, it succeeded")
		}
		if !strings.Contains(err.Error(), "knownhosts: key mismatch") {
			t.Fatalf("expected a host-key-mismatch error, got: %v", err)
		}
	})
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocating a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
