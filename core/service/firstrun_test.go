package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/config"
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

// TestFirstRun_CreateInitialConfig_DoesNotFreezeResolvedDefaultsIntoTheFile
// is issue #294. CreateInitialConfig's own comment says Retention and
// Alerts are left zero "on purpose" so an operator who never touches them
// keeps resolving to this product's CURRENT defaults across an upgrade,
// rather than freezing today's numbers into the file. But cfg.Validate
// resolves both IN PLACE (validateRetention's own doc says so), and the
// marshal used to read the same pointer afterwards, so the file got
// exactly the frozen numbers the comment said it would not. The
// assertion has to be on the bytes: a test on the returned BackupSet, or
// on cfg itself, is resolved either way and cannot see this.
func TestFirstRun_CreateInitialConfig_DoesNotFreezeResolvedDefaultsIntoTheFile(t *testing.T) {
	fr, configPath, _ := newTestFirstRun(t)

	if _, err := fr.CreateInitialConfig(context.Background(), firstRunCreateReq(t, fr, "nightly")); err != nil {
		t.Fatalf("CreateInitialConfig: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	written := string(raw)

	// Every spelling below is a resolved default nobody in this test
	// chose: config.Validate only fills each in when the field is still
	// at its zero value. Freezing any of them into a first-run file is
	// exactly the bug #294 reports.
	frozen := []string{
		"timezone: UTC",
		"week_starts_on: monday",
		"daily_days:",
		"weekly_months:",
		"monthly_months:",
		"protect_last_known_good: true",
		"repeated_failure_threshold: " + strconv.Itoa(config.DefaultRepeatedFailureThreshold),
	}
	for _, spelling := range frozen {
		if strings.Contains(written, spelling) {
			t.Errorf("CreateInitialConfig froze the resolved default %q into a first-run file nobody configured retention or alerts on:\n%s", spelling, written)
		}
	}

	// Positive control for the assertions above: encoding the VALIDATED
	// config -- what this function used to do -- does produce every one
	// of those spellings, so their absence from the file is not vacuous.
	validated, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}
	encoded, err := yaml.Marshal(validated)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, spelling := range frozen {
		if !strings.Contains(string(encoded), spelling) {
			t.Errorf("control failed: encoding the validated config does not produce %q, so the absence assertions above prove nothing", spelling)
		}
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

// TestFirstRun_AFailedWriteLeavesNothingBehind is the property that makes
// a failed setup recoverable instead of terminal. A truncated config.yaml
// is the worst thing this path can leave on the deployment issue #176
// exists for: Configured() reports true for any file that exists, so
// setup answers 409 from then on, and OpenConfigAndJournal finds a file
// so it never reports ErrConfigAbsent and the provider app exits. The
// container then crash-loops until somebody with a shell on the NAS
// deletes a file nobody told them about.
//
// The failure is injected at the write, which is where a real one comes
// from (ENOSPC on a NAS data volume, EIO, a quota). The retry at the end
// is the positive control: it runs the identical assertions with the
// injection lifted and requires the file to BE there, so "no config
// file" cannot pass by the fixture simply never getting that far.
func TestFirstRun_AFailedWriteLeavesNothingBehind(t *testing.T) {
	assertNothingLeftBehind := func(t *testing.T, configPath string) {
		t.Helper()
		if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
			raw, _ := os.ReadFile(configPath)
			t.Fatalf("a failed write left %s behind (stat err = %v, content %q); this install is now permanently unconfigurable", configPath, err, raw)
		}
		entries, err := os.ReadDir(filepath.Dir(configPath))
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".config-") {
				t.Errorf("a failed write left the temp file %s behind", e.Name())
			}
		}
	}

	t.Run("the link path", func(t *testing.T) {
		fr, configPath, _ := newTestFirstRun(t)
		req := firstRunCreateReq(t, fr, "nightly")

		restore := writeConfigPayload
		writeConfigPayload = func(*os.File, []byte) error {
			return fmt.Errorf("no space left on device")
		}
		t.Cleanup(func() { writeConfigPayload = restore })

		if _, err := fr.CreateInitialConfig(context.Background(), req); err == nil {
			t.Fatal("CreateInitialConfig returned no error even though the write failed")
		}
		assertNothingLeftBehind(t, configPath)
		if fr.Configured() {
			t.Error("Configured() = true after a write that failed; setup would refuse with 409 from now on")
		}

		// Positive control: the same call, the same fixture, the same
		// assertions, with the injection lifted. If this did not produce
		// a config file the checks above would be proving nothing.
		writeConfigPayload = restore
		if _, err := fr.CreateInitialConfig(context.Background(), req); err != nil {
			t.Fatalf("the retry after a failed write was refused: %v", err)
		}
		if _, err := os.Stat(configPath); err != nil {
			t.Fatalf("the retry reported success but wrote no config: %v", err)
		}
		svc, cleanup, err := Open(context.Background(), configPath)
		if err != nil {
			t.Errorf("the config the retry wrote does not open: %v", err)
		} else {
			_ = svc
			t.Cleanup(func() { _ = cleanup() })
		}
	})

	// The no-hard-links fallback is driven directly, because no
	// filesystem this suite runs on will refuse os.Link. It is the same
	// promise through a different primitive, so it gets the same test.
	t.Run("the no-links fallback", func(t *testing.T) {
		configPath, _ := emptyInstall(t)

		restore := writeConfigPayload
		writeConfigPayload = func(*os.File, []byte) error {
			return fmt.Errorf("no space left on device")
		}
		t.Cleanup(func() { writeConfigPayload = restore })

		if err := createConfigExclusivelyRemovingOnError(configPath, []byte("poll_interval: 1h\n")); err == nil {
			t.Fatal("createConfigExclusivelyRemovingOnError returned no error even though the write failed")
		}
		assertNothingLeftBehind(t, configPath)

		writeConfigPayload = restore
		if err := createConfigExclusivelyRemovingOnError(configPath, []byte("poll_interval: 1h\n")); err != nil {
			t.Fatalf("the retry after a failed write was refused: %v", err)
		}
		if _, err := os.Stat(configPath); err != nil {
			t.Fatalf("the retry reported success but wrote no config: %v", err)
		}
		if err := createConfigExclusivelyRemovingOnError(configPath, []byte("poll_interval: 1h\n")); !errors.Is(err, ErrAlreadyConfigured) {
			t.Errorf("the fallback overwrote an existing config: err = %v, want ErrAlreadyConfigured", err)
		}
	})
}

// TestFirstRun_UnderstandsTheConfigDirectorySpelling covers the spelling
// #196 made the packaged mount and config.ResolvePath invites an operator
// to type: --config naming the configuration DIRECTORY rather than the
// file inside it. Every consequence of not resolving it points the same
// way, at an install that reports success and is not set up.
func TestFirstRun_UnderstandsTheConfigDirectorySpelling(t *testing.T) {
	// A fresh install spelled either way is ABSENT, not broken. This is
	// the answer a provider app branches on to serve setup rather than
	// exit, so an unresolved directory path exiting at startup is a fresh
	// install that never comes up at all.
	t.Run("Open reports an absent config for both spellings", func(t *testing.T) {
		filePath, _ := emptyInstall(t)
		dirPath := filepath.Dir(filePath)
		for _, spelling := range []struct{ name, path string }{
			{"the file", filePath},
			{"the directory holding it", dirPath},
		} {
			t.Run(spelling.name, func(t *testing.T) {
				_, _, err := Open(context.Background(), spelling.path)
				if !errors.Is(err, ErrConfigAbsent) {
					t.Fatalf("Open(%q) error = %v, want ErrConfigAbsent", spelling.path, err)
				}
			})
		}
	})

	t.Run("everything FirstRun derives lands inside the directory", func(t *testing.T) {
		filePath, statePath := emptyInstall(t)
		dir := filepath.Dir(filePath)

		fr, err := NewFirstRun(FirstRunDefaults{ConfigPath: dir, StateDatabase: statePath})
		if err != nil {
			t.Fatalf("NewFirstRun with a directory-valued config path: %v", err)
		}
		if fr.Configured() {
			t.Fatal("Configured() = true on an empty configuration directory; a fresh install would be reported as already set up")
		}

		ref, err := fr.ImportSSHKey(context.Background(), []byte(testFixtureEd25519Key))
		if err != nil {
			t.Fatalf("ImportSSHKey: %v", err)
		}
		// The mount is the directory. A key written to its PARENT is on
		// the read-only rootfs of every shipped adapter, and it is
		// private key material.
		wantKeyDir := filepath.Join(dir, "ssh_keys")
		if got := filepath.Dir(ref.KeyFile); got != wantKeyDir {
			t.Errorf("imported key landed in %q, want %q (outside the mounted configuration directory)", got, wantKeyDir)
		}

		req := firstRunCreateReq(t, fr, "nightly")
		req.SSHKeyID = ref.ID
		if _, err := fr.CreateInitialConfig(context.Background(), req); err != nil {
			t.Fatalf("CreateInitialConfig: %v", err)
		}
		if _, err := os.Stat(filePath); err != nil {
			t.Fatalf("the first configuration was not written to %s: %v", filePath, err)
		}
		if !fr.Configured() {
			t.Error("Configured() = false after the configuration was written")
		}
		// The same directory spelling has to open the file that was just
		// written, or the instance is configured and still will not come
		// up.
		if _, cleanup, err := Open(context.Background(), dir); err != nil {
			t.Errorf("Open(%q) after setup: %v", dir, err)
		} else {
			t.Cleanup(func() { _ = cleanup() })
		}
	})

	t.Run("a path that resolves to a directory is refused", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "config.yaml"), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		_, err := NewFirstRun(FirstRunDefaults{ConfigPath: dir, StateDatabase: filepath.Join(dir, "state.db")})
		if err == nil {
			t.Fatal("NewFirstRun accepted a config path that is a directory even after resolution")
		}
		if !strings.Contains(err.Error(), "directory") {
			t.Errorf("error %q does not say the path is a directory", err)
		}
	})
}

// TestFirstRun_RefusesToWriteIntoASealedConfigDirectory is the config
// mount shape (#196) applied to the FIRST write an app-store install ever
// performs. Every other write path in the engine is already covered
// against a read-only configuration mount; CreateInitialConfig is the one
// that runs before anything else exists, so it is the one whose refusal
// an operator sees first.
func TestFirstRun_RefusesToWriteIntoASealedConfigDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory does not refuse root, so this test cannot observe the sealed shape")
	}

	newFixture := func(t *testing.T) (*FirstRun, string, CreateBackupSetRequest) {
		t.Helper()
		fr, configPath, _ := newTestFirstRun(t)
		req := firstRunCreateReq(t, fr, "nightly")
		// Pre-create the store the request's known_hosts line lands in,
		// so sealing the configuration directory itself leaves the
		// exclusive create of config.yaml as the step that refuses,
		// rather than a subdirectory creation earlier in the sequence.
		if err := os.MkdirAll(filepath.Join(filepath.Dir(configPath), "known_hosts.d"), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		return fr, configPath, req
	}

	t.Run("sealed", func(t *testing.T) {
		fr, configPath, req := newFixture(t)
		dir := filepath.Dir(configPath)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		_, err := fr.CreateInitialConfig(context.Background(), req)
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("CreateInitialConfig on a read-only configuration mount: err = %v, want one matching fs.ErrPermission", err)
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("error %q does not name the configuration directory %s, so an operator cannot tell what to remount", err, dir)
		}
		if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused write left %s behind (stat err = %v)", configPath, err)
		}
	})

	// Positive control: the identical request through the identical
	// fixture, with the seal off. Without it "it refused" would also be
	// satisfied by a request the engine rejects for its own reasons.
	t.Run("writable", func(t *testing.T) {
		fr, configPath, req := newFixture(t)
		if _, err := fr.CreateInitialConfig(context.Background(), req); err != nil {
			t.Fatalf("CreateInitialConfig on a writable configuration mount: %v", err)
		}
		if _, err := os.Stat(configPath); err != nil {
			t.Fatalf("nothing was written to %s: %v", configPath, err)
		}
	})
}
