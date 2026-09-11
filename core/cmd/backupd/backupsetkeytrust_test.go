// Issue #572 at the surface an operator drives over SSH: `backup-set
// patch` can rotate the key and re-trust the host, and the second one
// refuses until it has been acknowledged.
//
// The lines here are rendered from real, generated keys rather than
// written out, because the refusal these tests are about names two
// fingerprints and a made-up line has none.
package main

import (
	"crypto/ed25519"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/spdrman/backupd/core/internal/config"
)

// hostKeyLineFor renders a known_hosts line for host:port from a fresh
// key, with the SHA256 fingerprint an operator would be shown beside it.
func hostKeyLineFor(t *testing.T, host string, port int) (line, fingerprint string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatalf("NewPublicKey: %v", err)
	}
	return knownhosts.Line([]string{net.JoinHostPort(host, strconv.Itoa(port))}, pub), ssh.FingerprintSHA256(pub)
}

// privateKeyFingerprint reports the fingerprint of the key at path, so a
// rotation test can assert which key the set will actually authenticate
// with rather than which id it wrote down.
func privateKeyFingerprint(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		t.Fatalf("ParsePrivateKey(%s): %v", path, err)
	}
	return ssh.FingerprintSHA256(signer.PublicKey())
}

// createSFTPSetForPatch creates one sftp-shaped set through the CLI's own
// create verb and returns the line it was created trusting, so the tests
// below start from a set that has both a key and a trust anchor.
func createSFTPSetForPatch(t *testing.T, configPath, id string) (line, fingerprint string) {
	t.Helper()
	keyPath := writeTestPrivateKey(t)
	line, fingerprint = hostKeyLineFor(t, "source.example.internal", 2222)
	args := createArgs(configPath, keyPath, id)
	for i, a := range args {
		if a == aKnownHostsLine {
			args[i] = line
		}
	}
	captureStdout(t, func() {
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, got)
		}
	})
	return line, fingerprint
}

// TestRun_BackupSetPatchRotatesTheSSHKey is the ordinary case: the key on
// the source host was replaced, and until this issue the only route was to
// remove the set and create it again.
func TestRun_BackupSetPatchRotatesTheSSHKey(t *testing.T) {
	configPath := writeTestConfig(t)
	createSFTPSetForPatch(t, configPath, "api/rotate")

	replacement := writeTestPrivateKey(t)
	want := privateKeyFingerprint(t, replacement)
	out := captureStdout(t, func() {
		// --no-verify because source.example.internal is not a machine.
		// Every case in this file is about what the edit DOES to the key
		// or to the trust anchor, and #624's check in front of it would
		// refuse each of them for a host that was never meant to answer.
		// The check has its own cases in backupsetverify_test.go.
		args := []string{"backup-set", "--config", configPath, "patch", "api/rotate", "--ssh-key-file", replacement, "--no-verify"}
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, got)
		}
	})
	if !strings.Contains(out, want) {
		t.Errorf("the patch does not report the key it adopted (%s):\n%s", want, out)
	}

	// Read back off disk, through the key file itself rather than the
	// report: what has to be true is that the material the next
	// connection will authenticate with is the replacement's.
	keyPath := persistedKeyFile(t, configPath, "api", "rotate")
	if got := privateKeyFingerprint(t, keyPath); got != want {
		t.Errorf("the set authenticates with %s, want the replacement %s", got, want)
	}
}

// TestRun_BackupSetPatchRefusesAChangedHostKeyUntilAcknowledged: the one
// field whose whole purpose is to be updated when something changes is
// also the one where a silent update is indistinguishable from a machine
// in the middle, so the refusal comes first and names both fingerprints.
func TestRun_BackupSetPatchRefusesAChangedHostKeyUntilAcknowledged(t *testing.T) {
	configPath := writeTestConfig(t)
	_, oldFingerprint := createSFTPSetForPatch(t, configPath, "api/retrust")
	newLine, newFingerprint := hostKeyLineFor(t, "source.example.internal", 2222)

	// --no-verify for the reason the rotation case above gives: this is
	// about the refusal in front of a host-key change, not about whether
	// a fixture hostname resolves.
	args := []string{"backup-set", "--config", configPath, "patch", "api/retrust", "--known-hosts-line", newLine, "--no-verify"}
	out := captureStderr(t, func() {
		if got := run(args); got == 0 {
			t.Fatalf("run(%v) = 0, want a refusal: the host key changed and nothing acknowledged it", args)
		}
	})
	for _, want := range []string{oldFingerprint, newFingerprint} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not name %s:\n%s", want, out)
		}
	}

	// The control: the same edit goes through once it has been said out
	// loud, and it really lands on disk.
	ackArgs := append(append([]string{}, args...), "--acknowledge-host-key-change")
	captureStdout(t, func() {
		if got := run(ackArgs); got != 0 {
			t.Fatalf("run(%v) = %d, want 0 once acknowledged", ackArgs, got)
		}
	})
	if !strings.Contains(trustedLinesFor(t, configPath), newLine) {
		t.Error("the acknowledged re-trust did not reach the set's known_hosts file")
	}
}

// TestRun_BackupSetPatchTakesTheKeyAndTrustFlags is the parity assertion:
// every flag the contract's UpdateBackupSetRequest now carries is a flag
// this verb accepts, and one it refuses is one an operator cannot reach.
func TestRun_BackupSetPatchTakesTheKeyAndTrustFlags(t *testing.T) {
	configPath := writeTestConfig(t)
	line, _ := createSFTPSetForPatch(t, configPath, "api/parity")

	// Re-sending the line the set already trusts changes no trust, so it
	// needs no acknowledgement and must be accepted.
	args := []string{"backup-set", "--config", configPath, "patch", "api/parity", "--known-hosts-line", line, "--no-verify"}
	captureStdout(t, func() {
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, got)
		}
	})
}

// persistedKeyFile re-reads the configuration file and reports the key
// path the named backup set is actually configured with. Through the
// config loader rather than a scan of the YAML, because a scan finds the
// LAST "file:" in the document and the fixture's own local-transport set
// writes an empty one of those.
func persistedKeyFile(t *testing.T, configPath, sourceName, setName string) string {
	t.Helper()
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	for _, src := range cfg.Sources {
		if src.Name != sourceName {
			continue
		}
		for _, bs := range src.BackupSets {
			if bs.Name == setName {
				if bs.Remote.Key.File == "" {
					t.Fatalf("%s/%s is persisted with no key file at all", sourceName, setName)
				}
				return bs.Remote.Key.File
			}
		}
	}
	t.Fatalf("the persisted config has no backup set %s/%s", sourceName, setName)
	return ""
}

// trustedLinesFor reads every known_hosts file this deployment wrote,
// concatenated, so a test can ask what the deployment trusts without
// reconstructing the per-set filename convention.
func trustedLinesFor(t *testing.T, configPath string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(configPath), "known_hosts.d")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var all strings.Builder
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		all.Write(raw)
	}
	return all.String()
}
