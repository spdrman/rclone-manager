package main

import (
	"crypto/ed25519"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// writeTestPrivateKey writes a fresh, unencrypted ed25519 private key in
// OpenSSH format and returns its path. Generated per test rather than
// checked in, for the reason core/tests/sftpfixture generates its own: a
// private key in a repository is a private key forever, whatever it was
// generated for.
func writeTestPrivateKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "backup-set create test")
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// aKnownHostsLine is a syntactically real known_hosts line for a host
// nothing in these tests ever dials. Every create below carries one
// because CreateBackupSetRequest requires the trust anchor to have been
// decided before the set is persisted; none of these tests connects, so
// which key it names is irrelevant to them.
const aKnownHostsLine = "[source.example.internal]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ7Zq1i0i7Xw3v0m7d3Wl1nZk5Q9tJm2fVYy0m9c8ZqR"

// createArgs is one complete, valid `backup-set create` invocation, so a
// test that is about ONE refusal can state only the field it is changing
// instead of restating nine that are not the point.
func createArgs(configPath, keyPath, id string, extra ...string) []string {
	args := []string{
		"backup-set", "--config", configPath, "create", id,
		"--host", "source.example.internal",
		"--port", "2222",
		"--user", "backupuser",
		"--ssh-key-file", keyPath,
		"--known-hosts-line", aKnownHostsLine,
		"--remote-path", "/srv/backups",
		"--local-path", "/data/backups/api",
		"--completion-strategy", "rename",
	}
	return append(args, extra...)
}

func TestRun_BackupSetCreateRefusesWithoutASubcommandOrAnID(t *testing.T) {
	configPath := writeTestConfig(t)
	for _, args := range [][]string{
		{"backup-set", "--config", configPath},
		{"backup-set", "--config", configPath, "create"},
		{"backup-set", "--config", configPath, "frobnicate", "api/postgres"},
		{"backup-set", "--config", configPath, "create", "api/postgres", "and-another"},
		// A backup set id is exactly source/name. Anything else names
		// nothing, and guessing which half was meant is worse than saying so.
		{"backup-set", "--config", configPath, "create", "postgres"},
		{"backup-set", "--config", configPath, "create", "api/postgres/extra"},
		{"backup-set", "--config", configPath, "create", "/postgres"},
		{"backup-set", "--config", configPath, "create", "api/"},
	} {
		if got := run(args); got != 2 {
			t.Errorf("run(%v) = %d, want 2", args, got)
		}
	}
}

// TestRun_BackupSetCreateAddsToAnExistingConfig is the acceptance case
// for issue #356's first half: the CLI can do what POST /backup-sets can,
// and the proof is a SECOND, independent invocation reading the file
// fresh, not an echo of the request.
func TestRun_BackupSetCreateAddsToAnExistingConfig(t *testing.T) {
	configPath := writeTestConfig(t)
	keyPath := writeTestPrivateKey(t)

	out := captureStdout(t, func() {
		args := createArgs(configPath, keyPath, "api/postgres", "--include", "*.dump.zst, *.sql.gz")
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, got)
		}
	})
	for _, want := range []string{
		"backup set: api/postgres",
		"host: source.example.internal",
		"port: 2222",
		"user: backupuser",
		"remote_path: /srv/backups",
		"local_path: /data/backups/api",
		"include: *.dump.zst, *.sql.gz",
		"completion_strategy: rename",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("create output = %q, want it to contain %q", out, want)
		}
	}

	sourcesOut := captureStdout(t, func() {
		if got := run([]string{"sources", "--config", configPath}); got != 0 {
			t.Fatalf("sources = %d, want 0", got)
		}
	})
	if !strings.Contains(sourcesOut, "postgres") {
		t.Errorf("sources output = %q, want the newly created set in it", sourcesOut)
	}

	// The set that was already there has to survive. A create that
	// rewrites the file must fold in, never replace.
	if !strings.Contains(sourcesOut, "postgres-primary") {
		t.Errorf("sources output = %q, want the pre-existing backup set still there", sourcesOut)
	}
}

// TestRun_BackupSetCreateWritesTheFirstConfigOnAFreshInstall is the case
// the two-machine end-to-end test actually walks: the installer leaves an
// EMPTY config directory on purpose (issue #176), so the first thing the
// CLI is asked to do has no config.yaml to fold a set into.
func TestRun_BackupSetCreateWritesTheFirstConfigOnAFreshInstall(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "state.db")
	keyPath := writeTestPrivateKey(t)

	args := createArgs(configPath, keyPath, "api/postgres", "--state-database", dbPath)
	if got := run(args); got != 0 {
		t.Fatalf("run(%v) = %d, want 0", args, got)
	}

	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("no configuration was written at %s: %v", configPath, err)
	}
	if got := run([]string{"check", "--config", configPath}); got != 0 {
		t.Errorf("check against the freshly created config = %d, want 0", got)
	}

	// The state database the first configuration names is the deployment's
	// to decide, never the request's, so it has to be the one the flag
	// asked for.
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), dbPath) {
		t.Errorf("config.yaml = %q, want it to name the state database %q", raw, dbPath)
	}
}

// TestRun_BackupSetCreateRefusesASecondFirstConfig pins the one-time
// door: once a configuration exists, a create folds into it, and a create
// that was ASKED to write a first one still folds in rather than
// replacing what is there.
func TestRun_BackupSetCreateNeverReplacesAnExistingConfig(t *testing.T) {
	configPath := writeTestConfig(t)
	keyPath := writeTestPrivateKey(t)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	args := createArgs(configPath, keyPath, "api/postgres", "--state-database", filepath.Join(t.TempDir(), "elsewhere.db"))
	if got := run(args); got != 0 {
		t.Fatalf("run(%v) = %d, want 0", args, got)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(after), "postgres-primary") {
		t.Errorf("config.yaml after create = %q, want the pre-existing set kept", after)
	}
	if strings.Contains(string(after), "elsewhere.db") {
		t.Errorf("config.yaml after create = %q, want --state-database ignored against an existing configuration", after)
	}
	if len(after) <= len(before) {
		t.Errorf("config.yaml did not grow (%d -> %d bytes), so nothing was added", len(before), len(after))
	}
}

// TestRun_BackupSetCreateRefusesWhatTheServiceRefuses is the anti-drift
// case this verb exists to make possible. Every refusal below is
// core/service's own, reached through the CLI, so a second set of
// validation rules growing here would show up as one of these passing.
func TestRun_BackupSetCreateRefusesWhatTheServiceRefuses(t *testing.T) {
	keyPath := writeTestPrivateKey(t)

	cases := []struct {
		name  string
		extra []string
		drop  string
	}{
		{name: "an unregistered validator id", extra: []string{"--validator-id", "/bin/sh"}},
		{name: "a completion strategy nothing implements", extra: []string{"--completion-strategy", "vibes"}},
		{name: "a stable strategy with no window", extra: []string{"--completion-strategy", "stable"}},
		{name: "no host", drop: "--host"},
		{name: "no user", drop: "--user"},
		{name: "no remote path", drop: "--remote-path"},
		{name: "no local path", drop: "--local-path"},
		{name: "no trust anchor at all", drop: "--known-hosts-line"},
		{name: "no key", drop: "--ssh-key-file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configPath := writeTestConfig(t)
			args := createArgs(configPath, keyPath, "api/postgres", tc.extra...)
			if tc.drop != "" {
				args = withoutFlag(args, tc.drop)
			}
			if got := run(args); got == 0 {
				t.Fatalf("run(%v) = 0, want a refusal", args)
			}
			raw, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if strings.Contains(string(raw), "api") && strings.Contains(string(raw), "source.example.internal") {
				t.Errorf("a refused create still wrote something: %q", raw)
			}
		})
	}
}

// TestRun_BackupSetCreateRefusesTwoTrustAnchorsAndTwoKeys keeps the two
// ways of naming each input from being silently combined. Both pairs are
// alternatives, and a caller who passes both has not said what they meant.
func TestRun_BackupSetCreateRefusesContradictoryInputs(t *testing.T) {
	configPath := writeTestConfig(t)
	keyPath := writeTestPrivateKey(t)

	both := createArgs(configPath, keyPath, "api/postgres", "--trust-host-key")
	if got := run(both); got != 2 {
		t.Errorf("--known-hosts-line together with --trust-host-key = %d, want 2", got)
	}

	twoKeys := createArgs(configPath, keyPath, "api/postgres", "--ssh-key-id", "already-imported")
	if got := run(twoKeys); got != 2 {
		t.Errorf("--ssh-key-file together with --ssh-key-id = %d, want 2", got)
	}
}

// TestRun_BackupSetCreateSavesDisabled proves the flags that are not
// about SSH reach the persisted set: a set created disabled must come
// back disabled, and a read-only one read-only.
func TestRun_BackupSetCreateCarriesTheNonConnectionFlags(t *testing.T) {
	configPath := writeTestConfig(t)
	keyPath := writeTestPrivateKey(t)

	out := captureStdout(t, func() {
		args := createArgs(configPath, keyPath, "api/postgres",
			"--disabled", "--read-only", "--stale-after", "72h")
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, got)
		}
	})
	if !strings.Contains(out, "disabled: true") {
		t.Errorf("create output = %q, want disabled: true", out)
	}
	if !strings.Contains(out, "read_only: true") {
		t.Errorf("create output = %q, want read_only: true", out)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "72h") {
		t.Errorf("config.yaml = %q, want the requested stale_after persisted", raw)
	}
}

// withoutFlag removes one flag and its value from an argument list.
func withoutFlag(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// TestProbePortFor pins the one place the CLI resolves "no port
// configured" into a number. A backup set stores 0 to mean the default
// SSH port, and a host-key probe opens a real connection, so it cannot
// carry the 0 through. Getting this wrong is not subtle in production and
// was not subtle here either: --trust-host-key against a set with no
// --port failed with "port 0 is out of range" from inside the engine
// container, which names the symptom and not the decision.
func TestProbePortFor(t *testing.T) {
	if got := probePortFor(0); got != 22 {
		t.Errorf("probePortFor(0) = %d, want 22", got)
	}
	if got := probePortFor(2222); got != 2222 {
		t.Errorf("probePortFor(2222) = %d, want it left alone", got)
	}
}

// TestRun_BackupSetCreateRefusesRunOnAFreshInstall pins the one flag this
// verb cannot honour on one of its two paths. core/service ignores
// RunImmediately while writing a first configuration, because a
// first-run instance has no service to submit a cycle to yet. Accepting
// the flag and doing nothing with it would tell an operator who asked for
// a backup that one started, so the CLI refuses instead of inheriting the
// silence.
func TestRun_BackupSetCreateRefusesRunOnAFreshInstall(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	keyPath := writeTestPrivateKey(t)

	args := createArgs(configPath, keyPath, "api/postgres",
		"--state-database", filepath.Join(dir, "state.db"), "--run")
	if got := run(args); got != 2 {
		t.Fatalf("run(%v) = %d, want 2", args, got)
	}
	if _, err := os.Stat(configPath); err == nil {
		t.Errorf("a refused create still wrote a configuration at %s", configPath)
	}
}
