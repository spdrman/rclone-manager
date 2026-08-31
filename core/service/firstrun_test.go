package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// emptyInstall is the state a real operator actually starts in and that
// no other suite in this repository constructs: a state directory that
// does not exist yet, and no config.yaml anywhere. Every fixture helper
// this package already has (writeTestConfigFile, openTestService) writes
// a valid config first, which is exactly why the gap issue #176 describes
// stayed invisible.
//
// It returns the config path that does NOT exist and the state database
// path that does NOT exist, both under one fresh t.TempDir.
func emptyInstall(t *testing.T) (configPath, statePath string) {
	t.Helper()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	statePath = filepath.Join(dir, "state", "state.db")

	// Positive control on the fixture itself: a test that claims to
	// exercise "no config, empty state directory" is worth nothing if the
	// fixture quietly created either of them.
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture: config %s already exists (err = %v); this test would not be exercising a fresh install", configPath, err)
	}
	if _, err := os.Stat(filepath.Dir(statePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture: state directory %s already exists (err = %v); this test would not be exercising a fresh install", filepath.Dir(statePath), err)
	}
	return configPath, statePath
}

func newTestFirstRun(t *testing.T) (*FirstRun, string, string) {
	t.Helper()
	configPath, statePath := emptyInstall(t)
	fr, err := NewFirstRun(FirstRunDefaults{ConfigPath: configPath, StateDatabase: statePath})
	if err != nil {
		t.Fatalf("NewFirstRun: %v", err)
	}
	return fr, configPath, statePath
}

func firstRunCreateReq(t *testing.T, fr *FirstRun, name string) CreateBackupSetRequest {
	t.Helper()
	ref, err := fr.ImportSSHKey(context.Background(), []byte(testFixtureEd25519Key))
	if err != nil {
		t.Fatalf("ImportSSHKey: %v", err)
	}
	return CreateBackupSetRequest{
		Name:               name,
		Host:               "example.internal",
		Port:               22,
		User:               "backup-agent",
		SSHKeyID:           ref.ID,
		KnownHostsLine:     "example.internal ssh-ed25519 AAAAtestfixtureline",
		RemotePath:         "/backups/" + name,
		LocalPath:          filepath.Join(t.TempDir(), name),
		Include:            []string{"*.dump"},
		CompletionStrategy: "marker",
	}
}

// TestOpen_DistinguishesAnAbsentConfigFromAnInvalidOne is the split every
// other decision in issue #176 hangs off: an absent config is a fresh
// install and has to be reported as such, while a config that exists and
// does not validate stays the hard refusal it is today.
func TestOpen_DistinguishesAnAbsentConfigFromAnInvalidOne(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		configPath, _ := emptyInstall(t)

		_, _, err := Open(context.Background(), configPath)
		if err == nil {
			t.Fatal("Open against a missing config returned no error")
		}
		if !errors.Is(err, ErrConfigAbsent) {
			t.Errorf("Open error = %v, want one matching ErrConfigAbsent", err)
		}
	})

	t.Run("present but invalid", func(t *testing.T) {
		configPath, _ := emptyInstall(t)
		// A config that parses but does not validate: no sources, no
		// state database, no poll interval.
		if err := os.WriteFile(configPath, []byte("poll_interval: 0s\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		_, _, err := Open(context.Background(), configPath)
		if err == nil {
			t.Fatal("Open against an invalid config returned no error")
		}
		if errors.Is(err, ErrConfigAbsent) {
			t.Errorf("Open error = %v; an invalid config must NOT be reported as an absent one, or a first-run flow would offer to overwrite a real deployment's configuration", err)
		}
	})

	t.Run("present but unparseable", func(t *testing.T) {
		configPath, _ := emptyInstall(t)
		if err := os.WriteFile(configPath, []byte("this: [is not: valid yaml\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		_, _, err := Open(context.Background(), configPath)
		if err == nil {
			t.Fatal("Open against an unparseable config returned no error")
		}
		if errors.Is(err, ErrConfigAbsent) {
			t.Errorf("Open error = %v; an unparseable config must NOT be reported as an absent one", err)
		}
	})
}

// TestFirstRun_CreateInitialConfig_TurnsAnEmptyInstallIntoAServiceableOne
// is this issue's central proof, and it starts from the state no other
// test in this repository starts from: no config file, no state
// directory. It ends by handing the file it wrote to the very same
// production Open the daemon uses, so "a valid configuration" means the
// engine's own definition of valid, not this test's.
func TestFirstRun_CreateInitialConfig_TurnsAnEmptyInstallIntoAServiceableOne(t *testing.T) {
	fr, configPath, statePath := newTestFirstRun(t)

	if fr.Configured() {
		t.Fatal("Configured() = true on an install with no config file")
	}

	req := firstRunCreateReq(t, fr, "nightly")
	set, err := fr.CreateInitialConfig(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateInitialConfig: %v", err)
	}
	if set.ID != "api/nightly" {
		t.Errorf("Set.ID = %q, want %q", set.ID, "api/nightly")
	}

	if !fr.Configured() {
		t.Error("Configured() = false after CreateInitialConfig wrote the config")
	}

	// The engine's own production entry point is the oracle for "valid".
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open against the config CreateInitialConfig just wrote: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	if !svc.Ready() {
		t.Error("Ready() = false on a service opened from the first-run config")
	}
	sets, err := svc.ListBackupSets(context.Background())
	if err != nil {
		t.Fatalf("ListBackupSets: %v", err)
	}
	if len(sets) != 1 || sets[0].ID != "api/nightly" {
		t.Errorf("ListBackupSets = %+v, want exactly api/nightly", sets)
	}

	// The state database path came from the deployment, never from the
	// request: an API caller must not be able to point the journal
	// somewhere of its own choosing.
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("state database %s was not created by Open: %v", statePath, err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), statePath) {
		t.Errorf("config on disk does not name the deployment's state database %s:\n%s", statePath, raw)
	}
}

// TestFirstRun_WritesTheConfigWith0600 keeps the first config at the same
// permissions every later write already lands on (writeConfigBytesAtomically),
// rather than at whatever the process umask happens to be: it names a
// host, a user and the path to a private key.
func TestFirstRun_WritesTheConfigWith0600(t *testing.T) {
	fr, configPath, _ := newTestFirstRun(t)
	if _, err := fr.CreateInitialConfig(context.Background(), firstRunCreateReq(t, fr, "nightly")); err != nil {
		t.Fatalf("CreateInitialConfig: %v", err)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("config mode = %04o, want 0600", got)
	}
}

// TestFirstRun_RefusesToOverwriteAnExistingConfig is the negative half of
// the decision above: once a configuration exists, however it got there
// (a second wizard submission racing the first, a hand-written file, a
// restored backup), the first-run path is closed. The positive control is
// the test above, which proves the same call succeeds when nothing is
// there.
func TestFirstRun_RefusesToOverwriteAnExistingConfig(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"a valid config", "poll_interval: 1h\nstate:\n  database: /tmp/x/state.db\n"},
		{"an invalid config", "poll_interval: 0s\n"},
		{"an empty file", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr, configPath, _ := newTestFirstRun(t)
			req := firstRunCreateReq(t, fr, "nightly")
			if err := os.WriteFile(configPath, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			if fr.Configured() {
				// expected: a file is present
			} else {
				t.Error("Configured() = false with a config file present")
			}

			_, err := fr.CreateInitialConfig(context.Background(), req)
			if !errors.Is(err, ErrAlreadyConfigured) {
				t.Fatalf("CreateInitialConfig error = %v, want ErrAlreadyConfigured", err)
			}

			got, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if string(got) != tc.content {
				t.Errorf("the existing config was modified:\n got: %q\nwant: %q", got, tc.content)
			}
		})
	}
}

// TestFirstRun_RefusesAnInvalidRequestWithoutWritingAnything proves the
// same whole-config validation CreateBackupSet performs runs here too,
// and that a refusal leaves the install exactly as empty as it was.
func TestFirstRun_RefusesAnInvalidRequestWithoutWritingAnything(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*CreateBackupSetRequest)
		want string
	}{
		{"no name", func(r *CreateBackupSetRequest) { r.Name = "" }, "name is required"},
		{"no host", func(r *CreateBackupSetRequest) { r.Host = "" }, "host is required"},
		{"relative local path", func(r *CreateBackupSetRequest) { r.LocalPath = "relative/path" }, "local_path"},
		{"unknown completion strategy", func(r *CreateBackupSetRequest) { r.CompletionStrategy = "whenever" }, "completion_strategy"},
		{"unregistered validator", func(r *CreateBackupSetRequest) { r.ValidatorID = "definitely-not-registered" }, "validator_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr, configPath, _ := newTestFirstRun(t)
			req := firstRunCreateReq(t, fr, "nightly")
			tc.mut(&req)

			_, err := fr.CreateInitialConfig(context.Background(), req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("CreateInitialConfig error = %v, want one matching ErrInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q, so it does not say WHY the request was refused", err, tc.want)
			}
			if _, statErr := os.Stat(configPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a refused first-run request left a config file behind at %s (stat err = %v)", configPath, statErr)
			}
			if fr.Configured() {
				t.Error("Configured() = true after a refused first-run request")
			}
		})
	}
}

// TestFirstRun_NeverTakesTheStateDatabasePathFromTheRequest pins the
// security boundary the whole first-run surface rests on: everything an
// API caller supplies describes a backup set, and nothing it supplies can
// move this deployment's journal, its config file or its key store.
func TestFirstRun_NeverTakesTheStateDatabasePathFromTheRequest(t *testing.T) {
	fr, configPath, statePath := newTestFirstRun(t)

	req := firstRunCreateReq(t, fr, "nightly")
	// The only two path-shaped fields a caller controls. Neither may end
	// up deciding where state lives.
	elsewhere := filepath.Join(t.TempDir(), "attacker", "state.db")
	req.RemotePath = "/backups/" + filepath.Dir(elsewhere)
	req.LocalPath = filepath.Dir(elsewhere)

	if _, err := fr.CreateInitialConfig(context.Background(), req); err != nil {
		t.Fatalf("CreateInitialConfig: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var onDisk struct {
		State struct {
			Database string `yaml:"database"`
		} `yaml:"state"`
	}
	if err := yaml.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if onDisk.State.Database != statePath {
		t.Errorf("state.database = %q, want the deployment's own %q", onDisk.State.Database, statePath)
	}
}

// TestNewFirstRun_RefusesADeploymentItCannotProduceAValidConfigFor makes
// a misconfigured provider app fail at process start, where an operator
// reads the log, rather than at the end of a wizard the operator has
// already filled in.
func TestNewFirstRun_RefusesADeploymentItCannotProduceAValidConfigFor(t *testing.T) {
	cases := []struct {
		name     string
		defaults FirstRunDefaults
		want     string
	}{
		{"no config path", FirstRunDefaults{StateDatabase: "/data/state/state.db"}, "config path"},
		{"no state database", FirstRunDefaults{ConfigPath: "/etc/backup-manager/config.yaml"}, "state database"},
		{
			"relative state database",
			FirstRunDefaults{ConfigPath: "/etc/backup-manager/config.yaml", StateDatabase: "state/state.db"},
			"state database",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewFirstRun(tc.defaults)
			if err == nil {
				t.Fatal("NewFirstRun returned no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestFirstRun_ImportSSHKeyAndKnownHostsLandBesideTheConfig proves the
// two setup-only side effects a first-run install needs before it has any
// config at all end up in the same places CreateBackupSet already puts
// them, so nothing has to move once the instance is configured.
func TestFirstRun_ImportSSHKeyAndKnownHostsLandBesideTheConfig(t *testing.T) {
	fr, configPath, _ := newTestFirstRun(t)

	ref, err := fr.ImportSSHKey(context.Background(), []byte(testFixtureEd25519Key))
	if err != nil {
		t.Fatalf("ImportSSHKey: %v", err)
	}
	wantDir := filepath.Join(filepath.Dir(configPath), "ssh_keys")
	if filepath.Dir(ref.KeyFile) != wantDir {
		t.Errorf("key file %q is not in %q", ref.KeyFile, wantDir)
	}
	info, err := os.Stat(ref.KeyFile)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("imported key mode = %04o, want 0600", got)
	}

	req := firstRunCreateReq(t, fr, "nightly")
	req.SSHKeyID = ref.ID
	if _, err := fr.CreateInitialConfig(context.Background(), req); err != nil {
		t.Fatalf("CreateInitialConfig: %v", err)
	}
	knownHosts := filepath.Join(filepath.Dir(configPath), "known_hosts.d", "api_nightly_known_hosts")
	if _, err := os.Stat(knownHosts); err != nil {
		t.Errorf("known_hosts was not written to %s: %v", knownHosts, err)
	}
}

// TestFirstRun_ImportSSHKeyRejectsMaterialThatIsNotAPrivateKey is the
// negative control for the import path above: the first-run surface runs
// the same validation the configured one does, so a fresh install cannot
// be talked into persisting arbitrary bytes as a key.
func TestFirstRun_ImportSSHKeyRejectsMaterialThatIsNotAPrivateKey(t *testing.T) {
	fr, configPath, _ := newTestFirstRun(t)

	if _, err := fr.ImportSSHKey(context.Background(), []byte("not a key")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ImportSSHKey error = %v, want one matching ErrInvalidRequest", err)
	}

	entries, err := os.ReadDir(filepath.Join(filepath.Dir(configPath), "ssh_keys"))
	if err == nil && len(entries) != 0 {
		t.Errorf("a rejected key left %d file(s) behind in ssh_keys", len(entries))
	}
}
