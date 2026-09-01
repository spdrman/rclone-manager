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
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/spdrman/rclone-manager/core/tests/dockerlease"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/spdrman/rclone-manager/core/internal/transport"
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

// TestSftpConfig_KeyFileModeMustMatchExactlyWhatWasWritten is issue #293's
// RED case. importSSHKeyInto (service/backupsets.go) writes an imported key
// with os.WriteFile(path, raw, 0o600), but that mode argument only ever
// applies at creation: an operator's own chmod, a bind mount shared over
// SMB/AFP, or unrelated troubleshooting on the host can widen it afterward,
// and nothing used to look again before the key was next used.
//
// Before this check existed, drift was not merely reported opaquely, it
// was not reported at all: rclone's own embedded sftp backend
// (backend/sftp/sftp.go, vendored v1.75.0) os.ReadFile's key_file directly
// and hands the bytes to golang.org/x/crypto/ssh.ParsePrivateKey, and
// neither of those looks at the file's mode, unlike a real OpenSSH client
// or ssh-agent, which refuse a too-open key outright. A key widened to
// 0777 authenticated exactly as well as one still at 0600 through this
// project's own code path, silently.
//
// wantErr is exact-match, not "any group/other bit set": the check exists
// to notice when the mode is no longer what importSSHKeyInto wrote, so a
// mode that is merely narrower than 0600 (say, an operator's own
// well-meaning 0400) is drift too, not a stricter-and-therefore-fine case.
func TestSftpConfig_KeyFileModeMustMatchExactlyWhatWasWritten(t *testing.T) {
	cases := []struct {
		name    string
		mode    os.FileMode
		wantErr bool
	}{
		{"exactly what importSSHKeyInto writes", 0o600, false},
		{"world-writable drift, the production incident (#293)", 0o777, true},
		{"group-readable drift", 0o640, true},
		{"narrower than written is still drift", 0o400, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := validSource(t, dir)
			if err := os.Chmod(src.KeyFile, tc.mode); err != nil {
				t.Fatalf("chmod key file to %04o: %v", tc.mode, err)
			}

			_, err := sftpConfig(src)
			if tc.wantErr && err == nil {
				t.Fatalf("mode %04o: sftpConfig accepted it, want a refusal", tc.mode)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("mode %04o: sftpConfig refused an untouched key file: %v", tc.mode, err)
			}
		})
	}
}

// TestSftpConfig_DriftedKeyFileModeIsClassifiedAndActionable is the other
// half of issue #293's ask: refuse LOUDLY, with a specific diagnostic,
// rather than letting the transport's own opaque rejection (or, as the
// test above shows, its silent acceptance) be the only signal. The
// category is what lets internal/app/halt.go tell this apart from a
// rejected login (state.HaltAuthenticationFailed) in the operator-facing
// halt reason; the message is what a human reads if they go looking.
func TestSftpConfig_DriftedKeyFileModeIsClassifiedAndActionable(t *testing.T) {
	dir := t.TempDir()
	src := validSource(t, dir)
	if err := os.Chmod(src.KeyFile, 0o777); err != nil {
		t.Fatalf("chmod key file to 0777: %v", err)
	}

	_, err := sftpConfig(src)
	if err == nil {
		t.Fatal("sftpConfig accepted a key_file with drifted (0777) permissions, want a refusal")
	}
	if category, ok := transport.CategoryOf(err); !ok || category != transport.KeyPermissions {
		t.Fatalf("category = %v (ok=%v), want transport.KeyPermissions", category, ok)
	}
	for _, want := range []string{"0777", "0600"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %s, so the operator sees both the actual and the expected mode", err, want)
		}
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
// (password, ask_password, key_use_agent, pin_host_key, host_keys, the
// external ssh option, ...) breaks this test rather than shipping silently.
//
// key_pem (#74) is in the allowlist, not absent: it is now reachable, but
// only through resolveKeyFromEnv/resolveKeyFromCommand's validated output,
// never directly from a Source's own fields. See
// TestSftpConfig_KeyFileNeverProducesKeyPem and the key_env/key_command
// cases below for the other half of that claim: key_pem appears ONLY when
// the source actually chose one of those two resolvers.
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
		"key_pem":          true,
		"known_hosts_file": true,
		// Not part of the FR-6 security posture: these three exist because
		// fsFor calls info.NewFs directly and so gets none of rclone's own
		// option defaults (see the comment in sftpConfig). Without them
		// every sftp operation fails before it does anything, security
		// posture aside.
		"subsystem":   true,
		"chunk_size":  true,
		"concurrency": true,
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

// TestSftpConfig_KeyFileNeverProducesKeyPem is the other half of the
// allowlist test's claim: a source using the file resolver (the default,
// and the only one of the three that keeps key material off this process's
// heap) must never end up with key_pem set at all, on any path.
func TestSftpConfig_KeyFileNeverProducesKeyPem(t *testing.T) {
	dir := t.TempDir()
	src := validSource(t, dir)
	cfg, err := sftpConfig(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := cfg.Get("key_pem"); ok {
		t.Fatal("a key_file source produced a key_pem entry; the whole point of key_file is that this adapter never reads the key's bytes")
	}
}

// --- #74: exactly one of key_file, key_env, key_command ---

func TestSftpConfig_RequiresExactlyOneKeySource(t *testing.T) {
	dir := t.TempDir()

	t.Run("zero sources rejected", func(t *testing.T) {
		src := validSource(t, dir)
		src.KeyFile = ""
		_, err := sftpConfig(src)
		if err == nil {
			t.Fatal("a source with no key source at all was accepted")
		}
		if !strings.Contains(err.Error(), "key_file") || !strings.Contains(err.Error(), "key_env") || !strings.Contains(err.Error(), "key_command") {
			t.Errorf("error %q should name all three sources", err.Error())
		}
	})

	t.Run("key_file and key_env together rejected", func(t *testing.T) {
		src := validSource(t, dir)
		src.KeyEnv = "SOME_VAR"
		_, err := sftpConfig(src)
		if err == nil {
			t.Fatal("a source with both key_file and key_env set was accepted")
		}
	})

	t.Run("key_file and key_command together rejected", func(t *testing.T) {
		src := validSource(t, dir)
		src.KeyCommand = []string{"/bin/cat", "/dev/null"}
		_, err := sftpConfig(src)
		if err == nil {
			t.Fatal("a source with both key_file and key_command set was accepted")
		}
	})

	t.Run("key_env and key_command together rejected", func(t *testing.T) {
		src := validSource(t, dir)
		src.KeyFile = ""
		src.KeyEnv = "SOME_VAR"
		src.KeyCommand = []string{"/bin/cat", "/dev/null"}
		_, err := sftpConfig(src)
		if err == nil {
			t.Fatal("a source with both key_env and key_command set was accepted")
		}
	})
}

func TestSftpConfig_KeyEnvResolvesToKeyPem(t *testing.T) {
	dir := t.TempDir()
	clientKeyPath, _ := generateClientSSHKeyPair(t)
	pem, err := os.ReadFile(clientKeyPath)
	if err != nil {
		t.Fatalf("reading generated test key: %v", err)
	}

	const envName = "RCLONE_MANAGER_TEST_SFTPCONFIG_KEY_ENV"
	t.Setenv(envName, string(pem))

	src := validSource(t, dir)
	src.KeyFile = ""
	src.KeyEnv = envName

	cfg, err := sftpConfig(src)
	if err != nil {
		t.Fatalf("sftpConfig: %v", err)
	}
	if _, ok := cfg.Get("key_file"); ok {
		t.Error("a key_env source also set key_file")
	}
	got, ok := cfg.Get("key_pem")
	if !ok {
		t.Fatal("a key_env source did not set key_pem")
	}
	// The value has to be usable by rclone's own reconstruction
	// (strconv.Unquote("\"" + key_pem + "\"")), not merely non-empty.
	roundTripped, err := strconv.Unquote(`"` + got + `"`)
	if err != nil {
		t.Fatalf("key_pem value is not valid rclone escaping: %v", err)
	}
	if roundTripped != string(pem) {
		t.Fatal("key_pem, once unescaped the way rclone unescapes it, does not match the resolved key")
	}
}

func TestSftpConfig_KeyCommandResolvesToKeyPem(t *testing.T) {
	dir := t.TempDir()
	clientKeyPath, _ := generateClientSSHKeyPair(t)
	pem, err := os.ReadFile(clientKeyPath)
	if err != nil {
		t.Fatalf("reading generated test key: %v", err)
	}

	src := validSource(t, dir)
	src.KeyFile = ""
	src.KeyCommand = []string{"/bin/cat", clientKeyPath}

	cfg, err := sftpConfig(src)
	if err != nil {
		t.Fatalf("sftpConfig: %v", err)
	}
	got, ok := cfg.Get("key_pem")
	if !ok {
		t.Fatal("a key_command source did not set key_pem")
	}
	roundTripped, err := strconv.Unquote(`"` + got + `"`)
	if err != nil {
		t.Fatalf("key_pem value is not valid rclone escaping: %v", err)
	}
	if roundTripped != string(pem) {
		t.Fatal("key_pem, once unescaped the way rclone unescapes it, does not match the resolved key")
	}
}

// TestSftpConfig_KeyResolverFailureNeverLeaksIntoTheError proves the
// "resolved key never appears in any log line" requirement at the one
// place this adapter itself produces text that a caller might log: the
// error sftpConfig returns when a resolver fails.
func TestSftpConfig_KeyResolverFailureNeverLeaksIntoTheError(t *testing.T) {
	dir := t.TempDir()
	const secretLookingJunk = "s3kr1t-value-that-is-not-actually-a-key"

	const envName = "RCLONE_MANAGER_TEST_SFTPCONFIG_KEY_ENV_JUNK"
	t.Setenv(envName, secretLookingJunk)

	src := validSource(t, dir)
	src.KeyFile = ""
	src.KeyEnv = envName

	_, err := sftpConfig(src)
	if err == nil {
		t.Fatal("junk environment content was accepted as a key")
	}
	if strings.Contains(err.Error(), secretLookingJunk) {
		t.Fatalf("sftpConfig's error leaked the resolved value: %v", err)
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

// sftpFixtureImageTag names one build of the fixture image, and is
// deliberately unique per call rather than one fixed string.
//
// A fixed tag is shared mutable state on the docker daemon, and this
// machine runs several test processes against one daemon as a matter of
// course: the dockerlease package's own comments already list them, and two
// scripts/ci-local.sh runs from two worktrees is the ordinary case here.
// Each of those bakes ITS freshly generated client key into
// authorized_keys and rebuilds the same tag, so a container one process
// starts can be running another process's authorized_keys and will
// genuinely, permanently refuse the first process's key.
//
// That is not a startup race and no amount of waiting fixes it. It
// presents as "ssh: unable to authenticate, attempted methods [none
// publickey]" against a server whose own log says it is listening and
// closed one connection at [preauth], and it clears on an isolated re-run
// because nothing is then competing for the tag: #250's reported symptom,
// exactly. TestFixtureImageIsNotSharedBetweenClientKeys holds this.
func sftpFixtureImageTag() string {
	return fmt.Sprintf("rclone-manager-sftp-fixture:test-%d-%d", os.Getpid(), time.Now().UnixNano())
}

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

// generateEncryptedClientSSHKeyPair is generateClientSSHKeyPair's #269
// sibling: the same generated ed25519 keypair, but the private key file is
// encrypted with passphrase using x/crypto/ssh's own
// MarshalPrivateKeyWithPassphrase, rather than shelling out to ssh-keygen
// the way keysource_test.go's mustEncryptedKeyPEM does. Every other test
// in this file already generates its client keys this way (Go-native, no
// external dependency beyond what this package already imports), so this
// follows the same convention instead of introducing ssh-keygen as a new
// dependency of the Docker-fixture suite specifically.
func generateEncryptedClientSSHKeyPair(t *testing.T, passphrase string) (privateKeyPath string, authorizedKeyLine string) {
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

	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "rclone-manager-sftp-test-client-encrypted", []byte(passphrase))
	if err != nil {
		t.Fatalf("ssh.MarshalPrivateKeyWithPassphrase: %v", err)
	}
	pemBytes := pem.EncodeToMemory(block)

	privateKeyPath = filepath.Join(t.TempDir(), "client_ed25519_encrypted")
	if err := os.WriteFile(privateKeyPath, pemBytes, 0o600); err != nil {
		t.Fatalf("writing encrypted client private key: %v", err)
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

	tag := sftpFixtureImageTag()
	// Registered before the build so a build that fails halfway still has
	// its tag reclaimed, and before any container cleanup the caller adds
	// later, so t.Cleanup's LIFO order removes the containers first.
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", tag).Run() })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", tag, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker build failed: %v\n%s", err, out)
	}
	return tag
}

// TestFixtureImageIsNotSharedBetweenClientKeys is the other half of what
// made #250 legible only after someone read a container log by hand.
//
// The fixture bakes the client's authorized_keys into the image, so the
// image IS the answer to "which key does this server accept". While that
// image had one fixed tag, building a second fixture on the same daemon
// silently changed the answer for the first, and the first then met a
// server that refused its key outright. This builds two fixtures with two
// different client keys, in that order, and insists the first one still
// authenticates its own key afterwards.
//
// It runs the probe rather than the adapter because the claim is about the
// server's authorized_keys and nothing else, and because the probe is the
// thing every other test in this file now trusts.
func TestFixtureImageIsNotSharedBetweenClientKeys(t *testing.T) {
	requireDocker(t)

	firstKeyPath, firstAuthorized := generateClientSSHKeyPair(t)
	firstImage := buildSFTPFixtureImage(t, firstAuthorized)

	_, secondAuthorized := generateClientSSHKeyPair(t)
	secondImage := buildSFTPFixtureImage(t, secondAuthorized)

	if firstImage == secondImage {
		t.Fatalf("both fixtures were built as %s, so the second build overwrote the first and any process holding the first is now pointed at somebody else's authorized_keys", firstImage)
	}

	// Started AFTER the second build on purpose: that is the ordering that
	// hurts, and starting before it would prove nothing.
	port := freeTCPPort(t)
	cont, _ := startFixtureContainer(t, firstImage, port, "image-isolation", firstKeyPath)
	defer stopFixtureContainer(cont)

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if err := trySSHHandshake(addr, fixtureClientConfig(t, firstKeyPath)); err != nil {
		logs, _ := exec.Command("docker", "logs", cont).CombinedOutput()
		t.Fatalf("a container built from %s refused the very key that image was built to authorize, after a second fixture was built on the same daemon: %v\nserver logs:\n%s", firstImage, err, logs)
	}
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
// host key is confirmed generated and its sshd is confirmed willing to
// authenticate clientKeyPath, not merely to accept a TCP connection (#250).
// It retries the docker-run itself briefly on a port-in-use error, since a
// container stopped moments earlier by this same test can take a beat to
// fully release its port.
//
// clientKeyPath is the caller's own client key, the one it is about to
// hand the adapter. Readiness is checked with that exact key so that
// "ready" means the thing the caller is about to do will work, rather than
// something adjacent to it.
func startFixtureContainer(t *testing.T, image string, hostPort int, label, clientKeyPath string) (containerID, hostKeyLine string) {
	t.Helper()
	// Reclaim anything a previously KILLED run left behind (#150).
	dockerlease.Sweep()
	name := containerNameFor(t, label)

	var out []byte
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		cmd := exec.Command("docker", "run", "-d", "--name", name,
			dockerlease.LabelFlag, dockerlease.LabelSpec,
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

	hostKeyLine = waitForFixtureReady(t, containerID, hostPort, clientKeyPath)
	return containerID, hostKeyLine
}

// stopFixtureContainer removes a container immediately, so its host port is
// free for the next container in the same test to reuse. (The container's
// own t.Cleanup still runs at test end; removing an already-removed
// container there is a harmless no-op.)
func stopFixtureContainer(containerID string) {
	_ = exec.Command("docker", "rm", "-f", containerID).Run()
}

// The two halves of one readiness attempt are bounded separately, on
// purpose. ssh.ClientConfig.Timeout is documented as "the maximum amount of
// time for the TCP connection to establish", and that is all x/crypto uses
// it for: the version exchange, key exchange and user authentication that
// follow it have no deadline at all. A peer that accepts TCP and then says
// nothing is therefore not bounded by it, and that peer is not exotic here,
// it is this fixture's ordinary startup window: a published docker port
// accepts connections the moment the mapping exists, which is before sshd
// inside the container is necessarily answering. Without a deadline over
// the handshake, one such attempt outruns the polling loop's own deadline,
// because that deadline is only re-read between attempts.
//
// These mirror core/tests/sftpfixture's sshDialTimeout and
// sshHandshakeTimeout, which is the same probe against the same kind of
// container, and whose reasoning is written out at length in
// TestSSHHandshakeIsBoundedAgainstASilentPeer.
const (
	fixtureDialTimeout      = 2 * time.Second
	fixtureHandshakeTimeout = 5 * time.Second

	// fixtureHostKeyWindow and fixtureSSHReadyWindow bound the two waits
	// waitForFixtureReady makes. They are separate because they are
	// separate facts arriving in order: `ssh-keygen -A` writes the host
	// key and only then does the image's CMD exec sshd, so the key file
	// can exist for a while before anything is listening.
	fixtureHostKeyWindow  = 20 * time.Second
	fixtureSSHReadyWindow = 30 * time.Second
)

// sftpFixtureUser is the one account the fixture image creates (see
// sftpFixtureDockerfile's adduser and sftpFixtureSSHDConfig's `Match User`).
// The readiness probe authenticates as this user with the caller's client
// key, so it is deliberately the same constant the tests put in
// transport.Source.User: "the server will authenticate me" is only worth
// checking for the identity the test is actually going to use.
const sftpFixtureUser = "backup"

// waitForFixtureReady blocks until the container has generated its host key
// AND its sshd will complete a real SSH handshake and authenticate
// clientKeyPath as sftpFixtureUser, then returns the ed25519 host public key
// line (as produced by ssh-keygen, e.g. "ssh-ed25519 AAAA... comment").
//
// It authenticates rather than dialing because those are different claims,
// and only the second one is what every caller here needs (#250). A bare
// net.DialTimeout against a published docker port succeeds as soon as the
// port mapping exists, which can be well before sshd is answering, so the
// probe this replaces regularly handed tests a server that then refused
// their first real connection. TestClassify_Docker's positive control
// failed that way, with "ssh: unable to authenticate" on the client side
// and a connection closed at [preauth] in the container log.
//
// The probe deliberately does not verify the host key. Its only job is to
// establish that the server is up and will authenticate; host-key
// verification is the thing the tests themselves exercise, through the real
// adapter and the known_hosts files this file writes.
func waitForFixtureReady(t *testing.T, containerID string, hostPort int, clientKeyPath string) string {
	t.Helper()
	hostKeyLine := waitForFixtureHostKey(t, containerID)

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort))
	if err := waitForSSHAuthReady(addr, fixtureClientConfig(t, clientKeyPath), fixtureSSHReadyWindow); err != nil {
		logs, _ := exec.Command("docker", "logs", containerID).CombinedOutput()
		t.Fatalf("sftp fixture container %s published %s but never authenticated %s there: %v\ncontainer logs:\n%s",
			containerID, addr, sftpFixtureUser, err, logs)
	}
	return hostKeyLine
}

// waitForFixtureHostKey polls until the container has written its ed25519
// host public key, and returns that line. This is a docker exec rather than
// an ssh-keyscan because the tests need the key the server WILL present,
// pinned from inside the container, independently of anything on the wire.
func waitForFixtureHostKey(t *testing.T, containerID string) string {
	t.Helper()
	deadline := time.Now().Add(fixtureHostKeyWindow)
	var lastErr error
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "exec", containerID, "cat", "/etc/ssh/ssh_host_ed25519_key.pub").CombinedOutput()
		switch {
		case err != nil:
			lastErr = fmt.Errorf("reading host key: %w: %s", err, out)
		case len(bytes.TrimSpace(out)) == 0:
			lastErr = fmt.Errorf("host key file was empty")
		default:
			return string(bytes.TrimSpace(out))
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", containerID).CombinedOutput()
	t.Fatalf("sftp fixture container %s never generated its host key: %v\ncontainer logs:\n%s", containerID, lastErr, logs)
	return ""
}

// fixtureClientConfig builds the ssh client config the readiness probe
// authenticates with: the caller's own key, the fixture's one account, and
// no host-key verification (see waitForFixtureReady on why not).
func fixtureClientConfig(t *testing.T, clientKeyPath string) *ssh.ClientConfig {
	t.Helper()
	cfg, err := sshClientConfig(clientKeyPath)
	if err != nil {
		t.Fatalf("building the readiness probe's client config: %v", err)
	}
	return cfg
}

// sshClientConfig is fixtureClientConfig without the *testing.T, so that
// fixtureAuthVerdict can call it from inside a failure path without a
// second failure replacing the message the caller was trying to print.
func sshClientConfig(clientKeyPath string) (*ssh.ClientConfig, error) {
	keyPEM, err := os.ReadFile(clientKeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading client key %s: %w", clientKeyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parsing client key %s: %w", clientKeyPath, err)
	}
	return &ssh.ClientConfig{
		User:            sftpFixtureUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         fixtureDialTimeout,
	}, nil
}

// fixtureAuthVerdict re-runs the readiness probe against a fixture whose
// positive control has just failed, and describes what it found.
//
// It exists because #250 was only diagnosable after someone went and read
// the container log by hand: "List should have succeeded" on its own does
// not say whether the server stopped authenticating or the adapter stopped
// being able to authenticate against a server that is fine, and those are
// different bugs. This answers that question in the failure message.
//
// It deliberately cannot change a result. Nothing here retries the
// assertion or swallows anything: the test has already failed by the time
// this runs, and all this adds is a sentence about why.
func fixtureAuthVerdict(hostPort int, clientKeyPath string) string {
	cfg, err := sshClientConfig(clientKeyPath)
	if err != nil {
		return fmt.Sprintf("could not re-check the server, because the client key no longer loads: %v", err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort))
	if err := trySSHHandshake(addr, cfg); err != nil {
		return fmt.Sprintf("the readiness probe fails against %s too now (%v), so the server stopped authenticating rather than the adapter failing to", addr, err)
	}
	return fmt.Sprintf("the readiness probe still authenticates %s at %s with this same key, so the server is fine and the adapter is not", sftpFixtureUser, addr)
}

// waitForSSHAuthReady polls trySSHHandshake until one attempt completes a
// full handshake AND authenticates, or until within has elapsed.
//
// It returns an error instead of calling t.Fatal so that it can be pointed
// at a peer that is never going to be ready and asked what it decides. That
// is what TestFixtureReadinessProbeRefusesASilentPeer does, and it is the
// only way to show that this function refuses the exact state the bare dial
// it replaces called ready.
//
// Note that within bounds when the LAST attempt may start, not when the
// function returns: an attempt already in flight is bounded by
// fixtureHandshakeTimeout instead. That is the point of the split. The
// version it replaces had no bound on an attempt at all, so a single silent
// peer could outlive the whole loop.
func waitForSSHAuthReady(addr string, cfg *ssh.ClientConfig, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		err := trySSHHandshake(addr, cfg)
		if err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("no authenticated SSH handshake within %s: %w", within, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// trySSHHandshake makes one bounded attempt to connect and authenticate.
// It dials by hand rather than calling ssh.Dial so that the handshake gets
// a deadline of its own: ssh.Dial passes cfg.Timeout to the dial and
// nothing else, leaving everything after the TCP connect unbounded. This is
// the same shape, and the same fix, as core/tests/sftpfixture's
// trySSHHandshake.
func trySSHHandshake(addr string, cfg *ssh.ClientConfig) error {
	conn, err := net.DialTimeout("tcp", addr, fixtureDialTimeout)
	if err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Now().Add(fixtureHandshakeTimeout)); err != nil {
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

// ---------------------------------------------------------------------------
// The readiness probe, proved against peers this process stands up itself.
//
// #250 is a readiness check that could not tell "the port answers" from
// "sshd will authenticate me", so the fix is only worth anything if the new
// check can be shown to tell those apart. None of the three tests below
// needs Docker: an in-process listener reproduces each state exactly, on
// every machine, every time, which is more than the container can promise
// about a race it only sometimes loses.
//
// They come in a set on purpose. A probe hardcoded to "not ready" would
// satisfy both refusals and prove nothing at all, so the acceptance test is
// the control that stops the refusals meaning nothing.
// ---------------------------------------------------------------------------

// startInProcessSSHServer runs a real SSH server in this process, on a
// random loopback port, that authenticates exactly one public key and
// refuses every other. It exists so the probe can be pointed at a server
// whose answer is decided in advance: a container's is not.
//
// Pass a nil authorized key for a server that refuses everyone.
func startInProcessSSHServer(t *testing.T, authorized ssh.PublicKey) string {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("ssh.NewSignerFromKey: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, offered ssh.PublicKey) (*ssh.Permissions, error) {
			if authorized != nil && bytes.Equal(offered.Marshal(), authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("public key rejected by the in-process test server")
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			raw, acceptErr := ln.Accept()
			if acceptErr != nil {
				return // the listener was closed by cleanup
			}
			// No t.* calls in here: this outlives the test body, and a
			// failure reported after the test has finished panics the run
			// instead of failing the test.
			go func(c net.Conn) {
				sc, chans, reqs, hsErr := ssh.NewServerConn(c, cfg)
				if hsErr != nil {
					_ = c.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(ssh.Prohibited, "this fixture server offers no channels")
				}
				_ = sc.Close()
			}(raw)
		}
	}()

	return ln.Addr().String()
}

// clientPublicKey re-reads the authorized_keys line generateClientSSHKeyPair
// produced, as a parsed key the in-process server can compare against.
func clientPublicKey(t *testing.T, authorizedKeyLine string) ssh.PublicKey {
	t.Helper()
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKeyLine))
	if err != nil {
		t.Fatalf("parsing the generated authorized_keys line: %v", err)
	}
	return key
}

// TestFixtureReadinessProbeAcceptsAServerThatAuthenticates is the control on
// the two refusals below. Without it, a probe that answered "not ready" to
// everything, including a healthy fixture, would pass them both, and the
// only thing that would notice is the Docker suite timing out.
func TestFixtureReadinessProbeAcceptsAServerThatAuthenticates(t *testing.T) {
	clientKeyPath, authorizedKeyLine := generateClientSSHKeyPair(t)
	addr := startInProcessSSHServer(t, clientPublicKey(t, authorizedKeyLine))

	start := time.Now()
	if err := waitForSSHAuthReady(addr, fixtureClientConfig(t, clientKeyPath), fixtureSSHReadyWindow); err != nil {
		t.Fatalf("the probe refused a server that authenticates this exact key, so every refusal it reports elsewhere is worthless: %v", err)
	}
	t.Logf("accepted in %s", time.Since(start))
}

// TestFixtureReadinessProbeRefusesAServerThatRejectsTheKey is the half of
// #250 that a handshake alone would miss. "Ready" has to mean the server
// will authenticate the caller's key, not merely that it speaks SSH, so
// this points the probe at a server that completes the transport handshake
// happily and then refuses the key.
func TestFixtureReadinessProbeRefusesAServerThatRejectsTheKey(t *testing.T) {
	clientKeyPath, _ := generateClientSSHKeyPair(t)
	addr := startInProcessSSHServer(t, nil) // authorizes nobody

	const window = 500 * time.Millisecond
	start := time.Now()
	err := waitForSSHAuthReady(addr, fixtureClientConfig(t, clientKeyPath), window)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("the probe called a server ready that refuses this key outright")
	}
	// The error text is the positive control on the mechanism here. A probe
	// that never reached user authentication would still return SOME error
	// against this server if it were broken in an unrelated way, and would
	// satisfy "returned an error" while proving nothing about auth. Only a
	// probe that got through the key exchange and was then turned away at
	// authentication produces this.
	if !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("the probe failed, but not at authentication, so it says nothing about whether the probe checks auth at all: %v", err)
	}
	t.Logf("refused in %s: %v", elapsed, err)
}

// TestFixtureReadinessProbeRefusesASilentPeer is #250 stated as a test: a
// peer that completes the TCP handshake and then never sends a byte is
// precisely the state the old probe called ready, because a published
// docker port accepts connections the moment the mapping exists, before
// sshd inside is necessarily answering.
//
// The test opens with the old probe against this same listener, so the
// defect is an executable fact in the record rather than a claim in a
// comment: net.DialTimeout succeeds here, every time.
//
// Elapsed time is the control on the fix. A probe that never got past the
// dial would come back in microseconds and would satisfy "returned an
// error" while proving nothing about the handshake being bounded, so this
// insists the refusal took roughly a whole handshake deadline to arrive.
// The upper bound is the other half: ssh.ClientConfig.Timeout does not
// cover the handshake, so without a deadline of its own this call does not
// return at all.
func TestFixtureReadinessProbeRefusesASilentPeer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	accepted := make(chan net.Conn, 8)
	go func() {
		defer close(accepted)
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
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

	addr := ln.Addr().String()

	// The old probe, verbatim, against this peer.
	conn, dialErr := net.DialTimeout("tcp", addr, fixtureDialTimeout)
	if dialErr != nil {
		t.Fatalf("this peer is supposed to accept TCP and then go quiet, but the dial itself failed, so the rest of this test would be proving something else: %v", dialErr)
	}
	_ = conn.Close()
	t.Log("a bare net.DialTimeout calls this peer ready, which is #250")

	clientKeyPath, _ := generateClientSSHKeyPair(t)
	cfg := fixtureClientConfig(t, clientKeyPath)

	const window = 500 * time.Millisecond
	// window bounds when the last attempt may START; an attempt already in
	// flight is bounded by fixtureHandshakeTimeout. The slack on top is for
	// a loaded machine, and is still far short of "unbounded".
	limit := window + fixtureHandshakeTimeout + 15*time.Second

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- waitForSSHAuthReady(addr, cfg, window) }()

	select {
	case probeErr := <-done:
		elapsed := time.Since(start)
		if probeErr == nil {
			t.Fatal("the probe called a peer ready that never sent a byte, which is the whole of #250")
		}
		if elapsed < fixtureHandshakeTimeout-time.Second {
			t.Fatalf("the probe gave up after only %s, well short of the %s handshake deadline, so it cannot have got past the dial and says nothing about the handshake being bounded", elapsed, fixtureHandshakeTimeout)
		}
		t.Logf("bounded at %s: %v", elapsed, probeErr)
	case <-time.After(limit):
		t.Fatalf("the probe against a peer that accepts TCP and then says nothing was still waiting %s later; ssh.ClientConfig.Timeout bounds only the dial, so a handshake without a deadline of its own never returns here", limit)
	}
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

	contA, hostKeyA := startFixtureContainer(t, image, port, "server-a", clientKeyPath)
	writeKnownHosts(t, knownHostsPath, host, port, hostKeyA)

	src := transport.Source{
		ID:         "sftp-fixture",
		Type:       "sftp",
		Host:       host,
		Port:       port,
		User:       sftpFixtureUser,
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
			logs, _ := exec.Command("docker", "logs", contA).CombinedOutput()
			t.Fatalf("List against the recorded host key should have succeeded, got: %v\n%s\nserver logs:\n%s",
				err, fixtureAuthVerdict(port, clientKeyPath), logs)
		}
	})

	stopFixtureContainer(contA)

	t.Run("unknown host key is refused", func(t *testing.T) {
		unknownPort := freeTCPPort(t)
		contU, _ := startFixtureContainer(t, image, unknownPort, "server-unknown", clientKeyPath)
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
		contB, hostKeyB := startFixtureContainer(t, image, port, "server-b", clientKeyPath)
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

// TestSFTPKeyResolvers is #74's positive control: an end-to-end SFTP
// connection through each of the three key resolvers against the real
// Docker fixture, proving they actually authenticate a real session rather
// than merely producing bytes that look plausible in a unit test. Without
// this, code that refused every resolver would still pass every other test
// in this file.
//
// All three subtests reuse one fixture container and one client key pair:
// the fixture's authorized_keys trusts exactly one key regardless of which
// resolver names it, so the same container proves all three.
func TestSFTPKeyResolvers(t *testing.T) {
	requireDocker(t)

	clientKeyPath, authorizedKeyLine := generateClientSSHKeyPair(t)
	pem, err := os.ReadFile(clientKeyPath)
	if err != nil {
		t.Fatalf("reading generated client key: %v", err)
	}
	image := buildSFTPFixtureImage(t, authorizedKeyLine)

	host := "127.0.0.1"
	port := freeTCPPort(t)
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")

	cont, hostKeyLine := startFixtureContainer(t, image, port, "key-resolvers", clientKeyPath)
	t.Cleanup(func() { stopFixtureContainer(cont) })
	writeKnownHosts(t, knownHostsPath, host, port, hostKeyLine)

	base := transport.Source{
		ID:         "sftp-key-resolvers",
		Type:       "sftp",
		Host:       host,
		Port:       port,
		User:       sftpFixtureUser,
		KnownHosts: knownHostsPath,
	}
	adapter := New()
	ctx := context.Background()

	t.Run("file", func(t *testing.T) {
		src := base
		src.KeyFile = clientKeyPath
		if _, err := adapter.List(ctx, src); err != nil {
			logs, _ := exec.Command("docker", "logs", cont).CombinedOutput()
			t.Fatalf("List via the key_file resolver: %v\nserver logs:\n%s", err, logs)
		}
	})

	t.Run("env", func(t *testing.T) {
		const envName = "RCLONE_MANAGER_TEST_SFTP_KEY_ENV"
		t.Setenv(envName, string(pem))
		src := base
		src.KeyEnv = envName
		if _, err := adapter.List(ctx, src); err != nil {
			logs, _ := exec.Command("docker", "logs", cont).CombinedOutput()
			t.Fatalf("List via the key_env resolver: %v\nserver logs:\n%s", err, logs)
		}
	})

	t.Run("command", func(t *testing.T) {
		src := base
		src.KeyCommand = []string{"/bin/cat", clientKeyPath}
		if _, err := adapter.List(ctx, src); err != nil {
			logs, _ := exec.Command("docker", "logs", cont).CombinedOutput()
			t.Fatalf("List via the key_command resolver: %v\nserver logs:\n%s", err, logs)
		}
	})
}

// TestSFTPKeyResolvers_Passphrase is #269's end-to-end positive control:
// a real SFTP connection authenticated with a real, passphrase-protected
// private key, through each of the three passphrase resolvers, against
// the real Docker fixture -- the same "prove it against a live server,
// not just a unit test that produces plausible bytes" standard
// TestSFTPKeyResolvers already holds #74's three key resolvers to.
//
// The fixture's own readiness probe (startFixtureContainer ->
// waitForFixtureReady -> sshClientConfig) authenticates with
// ssh.ParsePrivateKey, which cannot load an encrypted key, so a second,
// unencrypted "readiness" keypair proves the container itself is up; the
// fixture's authorized_keys trusts both keys, and every actual assertion
// below authenticates with the encrypted one.
func TestSFTPKeyResolvers_Passphrase(t *testing.T) {
	requireDocker(t)

	readinessKeyPath, readinessAuthLine := generateClientSSHKeyPair(t)

	const passphrase = "correct horse battery staple"
	encryptedKeyPath, encryptedAuthLine := generateEncryptedClientSSHKeyPair(t, passphrase)
	encryptedPEM, err := os.ReadFile(encryptedKeyPath)
	if err != nil {
		t.Fatalf("reading generated encrypted client key: %v", err)
	}

	image := buildSFTPFixtureImage(t, readinessAuthLine+"\n"+encryptedAuthLine)

	host := "127.0.0.1"
	port := freeTCPPort(t)
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")

	cont, hostKeyLine := startFixtureContainer(t, image, port, "key-passphrase", readinessKeyPath)
	t.Cleanup(func() { stopFixtureContainer(cont) })
	writeKnownHosts(t, knownHostsPath, host, port, hostKeyLine)

	base := transport.Source{
		ID:         "sftp-key-passphrase",
		Type:       "sftp",
		Host:       host,
		Port:       port,
		User:       sftpFixtureUser,
		KnownHosts: knownHostsPath,
	}
	adapter := New()
	ctx := context.Background()

	// RED, and afterwards a permanent regression proof: with no passphrase
	// configured at all, an encrypted key.file fails clearly -- the exact
	// production failure #269 reported ("failed to parse private key
	// file: ... passphrase protected") -- never a hang and never a wrong
	// success.
	t.Run("key_file without a configured passphrase fails clearly", func(t *testing.T) {
		src := base
		src.KeyFile = encryptedKeyPath
		_, err := adapter.List(ctx, src)
		if err == nil {
			t.Fatal("an encrypted key with no passphrase configured was accepted")
		}
		if !strings.Contains(err.Error(), "passphrase") {
			t.Fatalf("error %q does not name the actual problem (passphrase protected): %v", err.Error(), err)
		}
	})

	t.Run("key_file with key.passphrase.env succeeds", func(t *testing.T) {
		const envName = "RCLONE_MANAGER_TEST_SFTP_PASSPHRASE_ENV"
		t.Setenv(envName, passphrase)
		src := base
		src.KeyFile = encryptedKeyPath
		src.PassphraseEnv = envName
		if _, err := adapter.List(ctx, src); err != nil {
			logs, _ := exec.Command("docker", "logs", cont).CombinedOutput()
			t.Fatalf("List via key_file + key.passphrase.env: %v\nserver logs:\n%s", err, logs)
		}
	})

	t.Run("key_file with key.passphrase.command succeeds", func(t *testing.T) {
		src := base
		src.KeyFile = encryptedKeyPath
		src.PassphraseCommand = []string{"/bin/echo", "-n", passphrase}
		if _, err := adapter.List(ctx, src); err != nil {
			logs, _ := exec.Command("docker", "logs", cont).CombinedOutput()
			t.Fatalf("List via key_file + key.passphrase.command: %v\nserver logs:\n%s", err, logs)
		}
	})

	t.Run("key_file with key.passphrase.file succeeds", func(t *testing.T) {
		passphraseFilePath := filepath.Join(t.TempDir(), "passphrase")
		// A trailing newline, exactly what `echo` (not `echo -n`) would
		// have produced: passphrase.go's own doc explains why this must
		// still work.
		if err := os.WriteFile(passphraseFilePath, []byte(passphrase+"\n"), 0o600); err != nil {
			t.Fatalf("writing passphrase file: %v", err)
		}
		src := base
		src.KeyFile = encryptedKeyPath
		src.PassphraseFile = passphraseFilePath
		if _, err := adapter.List(ctx, src); err != nil {
			logs, _ := exec.Command("docker", "logs", cont).CombinedOutput()
			t.Fatalf("List via key_file + key.passphrase.file: %v\nserver logs:\n%s", err, logs)
		}
	})

	t.Run("key_file with the wrong passphrase is refused, not hung or silently accepted", func(t *testing.T) {
		src := base
		src.KeyFile = encryptedKeyPath
		src.PassphraseCommand = []string{"/bin/echo", "-n", "definitely not the right passphrase"}
		_, err := adapter.List(ctx, src)
		if err == nil {
			t.Fatal("a wrong passphrase was accepted")
		}
	})

	t.Run("key_env with key.passphrase.env succeeds", func(t *testing.T) {
		const keyEnvName = "RCLONE_MANAGER_TEST_SFTP_KEY_ENCRYPTED_ENV"
		const passEnvName = "RCLONE_MANAGER_TEST_SFTP_PASSPHRASE_ENV2"
		t.Setenv(keyEnvName, string(encryptedPEM))
		t.Setenv(passEnvName, passphrase)
		src := base
		src.KeyEnv = keyEnvName
		src.PassphraseEnv = passEnvName
		if _, err := adapter.List(ctx, src); err != nil {
			logs, _ := exec.Command("docker", "logs", cont).CombinedOutput()
			t.Fatalf("List via key_env + key.passphrase.env: %v\nserver logs:\n%s", err, logs)
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

// TestWithSHA256_AsksRcloneForTheHashThisProjectVerifiesWith is the
// regression test for the one value config.Validation.Hash accepts being
// unreachable over the one remote backend this project ships besides
// "local".
//
// rclone's sftp backend builds its candidate hash set from its own
// "hashes" option, and seeds it with hash.MD5 and hash.SHA1 when that
// option is empty (backend/sftp/sftp.go, Hashes()). SHA-256 is never a
// candidate, so Adapter.RemoteHash's capability check refused before it
// reached the object, internal/lifecycle's Verify turned that into FAILED,
// and a backup set configured the way core/internal/config/testdata/
// full.yaml shows,
//
//	remote: {type: sftp, ...}
//	validation: {hash: sha256}
//
// failed 100% of its artifacts on every host, hardened or not. The
// documented configuration on the documented transport was the broken one.
func TestWithSHA256_AsksRcloneForTheHashThisProjectVerifiesWith(t *testing.T) {
	dir := t.TempDir()
	base, err := sftpConfig(validSource(t, dir))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg := withSHA256(base)

	got, ok := cfg.Get("hashes")
	if !ok {
		t.Fatal("withSHA256 did not set \"hashes\"; rclone then defaults to MD5+SHA1 and can never answer a sha256 request, so validation.hash: sha256 fails every artifact")
	}
	if !strings.Contains(got, "sha256") {
		t.Errorf("hashes = %q, want it to include \"sha256\"; config.Validation.Hash accepts no other non-empty value", got)
	}

	// Naming the hash is necessary and not sufficient. rclone will not
	// trust a hash command until it has probed it, and its v1.75.0
	// SHA-256 probe list pairs the sha256 commands with the SHA-1 ones
	// for the empty-input check ({"sha256sum", "sha1sum"} and
	// {"sha256 -r", "sha1 -r"}), so the probe runs sha1sum, gets SHA-1's
	// digest of empty input, compares it against SHA-256's, and rejects
	// a working sha256sum. Only its third candidate can be accepted, and
	// that one needs rclone installed on the SOURCE host. Measured
	// against a real sshd with coreutils sha256sum on PATH: with
	// "hashes" alone, RemoteHash still answered `backend "sftp" cannot
	// compute sha256`; with the pin below it returned the digest, equal
	// to the one sha256sum produced over a plain ssh session.
	if v, _ := cfg.Get("sha256sum_command"); v != "sha256sum" {
		t.Errorf("sha256sum_command = %q, want %q; without it rclone probes sha256 with sha1sum and rejects a working sha256sum", v, "sha256sum")
	}
}

// TestWithSHA256_NeverReachesTheFsThatCopies is the other half of the fix,
// and the half a gate run had to teach me.
//
// rclone's copy picks its integrity hash from Common(src.Hashes(),
// dst.Hashes()). Setting these options on the Fs that copies makes a
// hardened, shell-less account advertise SHA-256, fail to compute it at
// copy time, and rclone then compares the empty string against the local
// digest and reports `corrupted on transfer: sha256 hashes differ`.
// Measured in core/tests/sftpintegration: it turned the recommended
// deployment from "backs up, cannot hash-verify" into "cannot back up at
// all", broke the backup sets that never asked for a hash as well as the
// ones that did, and blamed corruption for a missing capability.
//
// So sftpConfig, which is what fsFor builds every list, copy and delete
// from, must come back without either key.
func TestWithSHA256_NeverReachesTheFsThatCopies(t *testing.T) {
	dir := t.TempDir()
	cfg, err := sftpConfig(validSource(t, dir))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, k := range []string{"hashes", "sha256sum_command"} {
		if v, ok := cfg.Get(k); ok {
			t.Errorf("sftpConfig set %q = %q; on the copy path that turns a missing hash capability into `corrupted on transfer` and stops the backup entirely", k, v)
		}
	}
}

// TestWithSHA256_IsNotAClaimTheServerCanHashIt guards the remaining risk:
// the pin must not turn a source that genuinely cannot hash into one that
// appears to pass.
//
// It does not, and the reason is structural rather than a value in this
// map. Pinning the command makes rclone RUN it instead of probing for it;
// where the account has no shell, the run fails, RemoteHash returns that
// error, and Verify fails the artifact exactly as it did when the
// capability came back absent. Measured both ways against real sshd
// containers: a shell account returns the digest, a forced internal-sftp
// account returns `failed to run "sha256sum ...": Process exited with
// status 1`. Neither hands back a hash the manager did not earn.
//
// What this test can pin is the narrower, checkable claim: nothing here
// reaches for one of rclone's own ways of making a hash check pass without
// performing one, or pays for digests this project never requests.
func TestWithSHA256_IsNotAClaimTheServerCanHashIt(t *testing.T) {
	dir := t.TempDir()
	base, err := sftpConfig(validSource(t, dir))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg := withSHA256(base)

	if v, ok := cfg.Get("disable_hashcheck"); ok && v != "false" {
		t.Errorf("disable_hashcheck = %q; nothing may switch off the check internal/lifecycle is being asked to enforce", v)
	}
	for _, k := range []string{"md5sum_command", "sha1sum_command"} {
		if v, ok := cfg.Get(k); ok {
			t.Errorf("%s = %q; nothing in this project asks for that digest, so pinning it only buys a wasted round trip", k, v)
		}
	}

	// And it leaves the FR-6 posture alone: every key sftpConfig set is
	// still set, to the same value.
	for k, want := range base {
		if got, _ := cfg.Get(k); got != want {
			t.Errorf("withSHA256 changed %q from %q to %q; it may only add", k, want, got)
		}
	}
}
