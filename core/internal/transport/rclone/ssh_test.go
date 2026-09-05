package rclone

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/spdrman/rclone-manager/core/internal/obs"
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

// TestSftpConfig_RefusesWorldWritableAncestorDirectory is the other half of
// issue #293 the PR #311 review flagged as unaddressed: checkKeyFileMode
// only ever looks at the key file's OWN mode. The production incident
// this whole file exists for drifted the key AND the directory chain down
// to the backup root to world-writable, and a world-writable directory
// lets any local actor unlink/replace/rename the entry inside it
// regardless of what mode the file itself carries — Unix directory-write
// permission governs entry changes independent of the target's own mode
// bits. Before checkKeyDirChainMode existed, a key file left at a
// pristine 0600 sailed through unnoticed even while its own directory (or,
// as here, a directory two levels further up, well past its immediate
// parent) was wide open, which is exactly the "more dangerous half" the
// review named: the file-mode check gives false confidence while the real
// exposure, swapping the key out entirely, still stands.
func TestSftpConfig_RefusesWorldWritableAncestorDirectory(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "level1", "level2")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("mkdir chain: %v", err)
	}
	src := validSource(t, root)
	src.KeyFile = touchFile(t, nested, "id_ed25519")

	// Positive control: an untouched chain (every directory 0700, the key
	// file itself 0600) must be accepted, otherwise the refusal below
	// would prove nothing.
	if _, err := sftpConfig(src); err != nil {
		t.Fatalf("sftpConfig refused a key_file with an untouched directory chain: %v", err)
	}

	// Widen "level1", two levels above the key file and one above its own
	// immediate parent ("level2"), which stays at 0700 throughout. The key
	// file itself is never touched: this is the drift the finding says
	// checkKeyFileMode alone cannot see.
	level1 := filepath.Join(root, "level1")
	if err := os.Chmod(level1, 0o777); err != nil {
		t.Fatalf("chmod level1 to 0777: %v", err)
	}

	_, err := sftpConfig(src)
	if err == nil {
		t.Fatal("sftpConfig accepted a key_file whose directory chain contains a world-writable component, want a refusal")
	}
	if category, ok := transport.CategoryOf(err); !ok || category != transport.KeyPermissions {
		t.Fatalf("category = %v (ok=%v), want transport.KeyPermissions (the same halt-reason path checkKeyFileMode uses)", category, ok)
	}
	if !strings.Contains(err.Error(), level1) {
		t.Errorf("error %q should name the drifted directory %q", err, level1)
	}
}

// TestSftpConfig_AllowsStickyWorldWritableAncestorDirectory locks in the
// one deliberate exception to the check above: a directory with the
// sticky bit set (mode 1777, /tmp's standard permissions on every
// mainstream Unix) is not refused for being world-writable. That is not a
// gap in the check, it is the same fact that makes a world-writable /tmp
// safe to have on every real system: POSIX restricts unlink/rename inside
// a sticky directory to the entry's own owner, the directory's owner, or
// root, regardless of who else can write there, which is exactly the
// attack checkKeyDirChainMode exists to close. Without this exception the
// check would refuse a perfectly ordinary, correctly configured system
// any time a key file's path happened to share an ancestor with the
// system's shared temp directory.
func TestSftpConfig_AllowsStickyWorldWritableAncestorDirectory(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "level1", "level2")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("mkdir chain: %v", err)
	}
	level1 := filepath.Join(root, "level1")
	if err := os.Chmod(level1, 0o777|os.ModeSticky); err != nil {
		t.Fatalf("chmod level1 to 1777: %v", err)
	}

	src := validSource(t, root)
	src.KeyFile = touchFile(t, nested, "id_ed25519")

	if _, err := sftpConfig(src); err != nil {
		t.Fatalf("sftpConfig refused a key_file under a world-writable-but-sticky ancestor directory: %v", err)
	}
}

// TestSftpConfig_DirChainCheckAppliesToAnEncryptedKeyToo composes #311's
// directory-chain check with #298's at-rest encryption: a world-writable
// ancestor directory must still be refused when key_encryption is
// configured, exactly as it is on the plaintext path. #298's own
// ciphertext is authenticated (AES-GCM), which already defeats a silent
// CONTENT forgery, but a world-writable directory still lets any local
// actor unlink/replace/rename the encrypted key file wholesale, so the
// permission-drift risk this check exists for is identical either way.
// Before ssh.go's key_file case was restructured to run these checks
// unconditionally, resolveKeyFileForSFTP's own os.ReadFile ran first
// whenever key_encryption was configured, leaving this exact gap.
func TestSftpConfig_DirChainCheckAppliesToAnEncryptedKeyToo(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "level1", "level2")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("mkdir chain: %v", err)
	}
	src := validSource(t, root)
	src.KeyFile = touchFile(t, nested, "id_ed25519")
	if err := os.WriteFile(src.KeyFile, mustUnencryptedKeyPEM(t), 0o600); err != nil {
		t.Fatalf("writing a real test key over the placeholder: %v", err)
	}

	const envName = "RCLONE_MANAGER_TEST_SFTPCONFIG_DIRCHAIN_KEYENC_ENV"
	t.Setenv(envName, "dirchain-encrypted-key-dek")
	src.KeyEncryptionEnv = envName

	// Positive control: an untouched chain must still resolve through the
	// encrypted path, otherwise the refusal below proves nothing.
	if _, err := sftpConfig(src); err != nil {
		t.Fatalf("sftpConfig refused an encrypted key_file with an untouched directory chain: %v", err)
	}

	level1 := filepath.Join(root, "level1")
	if err := os.Chmod(level1, 0o777); err != nil {
		t.Fatalf("chmod level1 to 0777: %v", err)
	}

	_, err := sftpConfig(src)
	if err == nil {
		t.Fatal("sftpConfig accepted an encrypted key_file whose directory chain contains a world-writable component, want a refusal")
	}
	if category, ok := transport.CategoryOf(err); !ok || category != transport.KeyPermissions {
		t.Fatalf("category = %v (ok=%v), want transport.KeyPermissions (the same halt-reason path checkKeyFileMode uses)", category, ok)
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
//
// It runs over several sources rather than one, and that is load-bearing
// rather than thoroughness for its own sake. A key sftpConfig only sets
// inside an `if` is invisible to a fixture that never takes that branch,
// so a single-source version of this test claims to pin the whole
// producible set while actually pinning whichever subset one source
// happens to reach. #355 found exactly that: `connections` had been a
// producible key for a while and this test had never once seen it,
// because the source it used left the ceiling at zero.
func TestSftpConfig_OnlyAllowlistedKeysAreSet(t *testing.T) {
	allowed := map[string]bool{
		"host":             true,
		"port":             true,
		"user":             true,
		"key_file":         true,
		"key_pem":          true,
		"key_file_pass":    true,
		"known_hosts_file": true,
		// Not part of the FR-6 security posture: these four exist because
		// fsFor calls info.NewFs directly and so gets none of rclone's own
		// option defaults (see the comment in sftpConfig). Without the
		// first three every sftp operation fails before it does anything,
		// security posture aside; the fourth restores rclone's own pool
		// drainer, which without it never exists at all.
		"subsystem":    true,
		"chunk_size":   true,
		"concurrency":  true,
		"idle_timeout": true,
		// #264/#355: the operator's connection ceiling, set only when one
		// was actually asked for.
		"connections": true,
	}

	for _, tc := range []struct {
		name    string
		mutate  func(t *testing.T, src *transport.Source)
		mustSet []string
	}{
		{
			name:    "plain",
			mutate:  func(*testing.T, *transport.Source) {},
			mustSet: []string{"key_file", "known_hosts_file", "subsystem", "chunk_size", "concurrency", "idle_timeout"},
		},
		{
			name: "with a connection ceiling",
			mutate: func(_ *testing.T, src *transport.Source) {
				src.MaxConnections = 2
			},
			mustSet: []string{"connections"},
		},
		{
			name: "with a key passphrase",
			mutate: func(t *testing.T, src *transport.Source) {
				t.Setenv("TEST_SFTP_KEY_PASSPHRASE", "not-a-real-passphrase")
				src.PassphraseEnv = "TEST_SFTP_KEY_PASSPHRASE"
			},
			mustSet: []string{"key_file_pass"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := validSource(t, dir)
			tc.mutate(t, &src)
			cfg, err := sftpConfig(src)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			for k := range cfg {
				if !allowed[k] {
					t.Errorf("sftpConfig set unexpected key %q; every key this function can set must be reviewed for FR-6 impact", k)
				}
			}

			// The branch this case exists to reach really was reached, so
			// "no unexpected key" above is a statement about a config
			// that actually contains the key in question.
			for _, k := range tc.mustSet {
				if _, ok := cfg.Get(k); !ok {
					t.Errorf("this case is supposed to make sftpConfig set %q and it did not, so it pins nothing", k)
				}
			}

			// And the values that matter for FR-6 are exactly what was
			// configured, not silently substituted or defaulted to
			// something looser.
			if v, _ := cfg.Get("key_file"); v != src.KeyFile {
				t.Errorf("key_file = %q, want %q", v, src.KeyFile)
			}
			if v, _ := cfg.Get("known_hosts_file"); v != src.KnownHosts {
				t.Errorf("known_hosts_file = %q, want %q", v, src.KnownHosts)
			}
		})
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

// --- #298: key_file at rest encryption, exercised through sftpConfig itself ---

// TestSftpConfig_KeyFileWithNoKeyEncryptionStaysOffTheHeap is #298's
// regression guarantee proven at the sftpConfig level rather than
// resolveKeyFileForSFTP's own unit level: with no key_encryption source
// configured, a key_file source behaves EXACTLY as TestSftpConfig_
// KeyFileNeverProducesKeyPem already proves it always has, key_pem is
// never set and key_file is forwarded untouched, even though this
// specific source's file happens to hold a real key on disk (validSource's
// touchFile only ever writes a placeholder; this test uses a real one so a
// regression that started reading key_file's content unconditionally would
// still only be caught here, not by the placeholder-based test).
func TestSftpConfig_KeyFileWithNoKeyEncryptionStaysOffTheHeap(t *testing.T) {
	dir := t.TempDir()
	src := validSource(t, dir)
	if err := os.WriteFile(src.KeyFile, mustUnencryptedKeyPEM(t), 0o600); err != nil {
		t.Fatalf("writing a real test key over the placeholder: %v", err)
	}

	cfg, err := sftpConfig(src)
	if err != nil {
		t.Fatalf("sftpConfig: %v", err)
	}
	if _, ok := cfg.Get("key_pem"); ok {
		t.Fatal("key_pem was set with no key_encryption source configured")
	}
	if v, _ := cfg.Get("key_file"); v != src.KeyFile {
		t.Errorf("key_file = %q, want %q", v, src.KeyFile)
	}
}

// TestSftpConfig_KeyFileEncryptedAtRestResolvesToKeyPem is #298's
// migration path exercised through the real production entry point: a
// plaintext key_file plus a configured key_encryption source is migrated
// to at-rest encryption AND authenticates this one connection attempt, in
// a single sftpConfig call, exactly the way an upgraded installation's
// first real connection after configuring key_encryption behaves.
func TestSftpConfig_KeyFileEncryptedAtRestResolvesToKeyPem(t *testing.T) {
	dir := t.TempDir()
	src := validSource(t, dir)
	plaintext := mustUnencryptedKeyPEM(t)
	if err := os.WriteFile(src.KeyFile, plaintext, 0o600); err != nil {
		t.Fatalf("writing a real test key over the placeholder: %v", err)
	}

	const envName = "RCLONE_MANAGER_TEST_SFTPCONFIG_KEYENC_ENV"
	t.Setenv(envName, "sftpconfig-level-migration-dek")
	src.KeyEncryptionEnv = envName

	cfg, err := sftpConfig(src)
	if err != nil {
		t.Fatalf("sftpConfig: %v", err)
	}
	if _, ok := cfg.Get("key_file"); ok {
		t.Error("an at-rest-encrypted key source also set key_file")
	}
	got, ok := cfg.Get("key_pem")
	if !ok {
		t.Fatal("an at-rest-encrypted key source did not set key_pem")
	}
	roundTripped, err := strconv.Unquote(`"` + got + `"`)
	if err != nil {
		t.Fatalf("key_pem value is not valid rclone escaping: %v", err)
	}
	if roundTripped != string(plaintext) {
		t.Fatal("key_pem, once unescaped, does not match the original key")
	}

	onDisk, err := os.ReadFile(src.KeyFile)
	if err != nil {
		t.Fatalf("reading key_file after sftpConfig: %v", err)
	}
	if !isEncryptedKeyMaterial(onDisk) {
		t.Fatal("sftpConfig did not migrate the plaintext key_file to at-rest encryption")
	}
}

// TestSftpConfig_KeyFileEncryptionWrongDEKFails proves a misconfigured
// key_encryption source fails the whole connection attempt loudly, the
// same way a bad key.env/key.command resolver already does, rather than
// silently falling back to key_file or authenticating with garbage.
func TestSftpConfig_KeyFileEncryptionWrongDEKFails(t *testing.T) {
	dir := t.TempDir()
	src := validSource(t, dir)

	ciphertext, err := encryptKeyMaterial(obs.NewSecret("the-real-dek"), mustUnencryptedKeyPEM(t))
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}
	if err := os.WriteFile(src.KeyFile, ciphertext, 0o600); err != nil {
		t.Fatalf("writing a pre-encrypted key over the placeholder: %v", err)
	}

	const envName = "RCLONE_MANAGER_TEST_SFTPCONFIG_KEYENC_WRONG"
	t.Setenv(envName, "not-the-right-dek")
	src.KeyEncryptionEnv = envName

	if _, err := sftpConfig(src); err == nil {
		t.Fatal("sftpConfig succeeded with a key_encryption source that does not decrypt key_file")
	}
}

// ---------------------------------------------------------------------------
// The Docker half of this file has moved.
// ---------------------------------------------------------------------------
//
// Everything between here and TestWithSHA256 below used to be an SFTP
// fixture of this file's own: an alpine image with a client key baked into
// authorized_keys, rebuilt inside six separate test functions, plus the
// container lifecycle, the readiness probe and the host-key attack cases
// that drove it.
//
// It is core/tests/machinegate and core/tests/machines now (#448, #450).
// The attack cases needed a server whose host key could be substituted and
// whose authorized_keys could be added to, which is a machine rather than a
// per-test `docker build`, and running them from this package meant
// `go test ./internal/...` needed a Docker daemon in order to say anything
// about sftpConfig, which is pure.
//
// What stays here is the pure half, and it is most of the file: every
// TestSftpConfig_ case above, and the three withSHA256 cases below.
//
// Three helpers came back with it, because the pure half uses them and
// none of them touches Docker: two ed25519 key generators and a free-port
// finder.

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

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocating a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

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
