package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/service"
)

// Issue #543, Phase 2 of #536: a mutating backup-set command REACHES the
// engine when one is attached, instead of being refused because this
// build had no way to hand the change over.
//
// # What these tests are arranged to catch
//
// Two configurations, not one. The CLI is pointed at its own config.yaml
// and the engine serves a different one, so "the change reached the
// engine" and "the change was written locally" are two observations that
// cannot be confused for each other. #535 is exactly the case where they
// were: one file, two processes, and no way to tell which of them had
// acted.
//
// So every case below asserts both halves. The engine's configuration
// changed, and the CLI's own configuration is byte for byte what it was.

// attachEngineTo announces that something is serving the deployment
// configPath names, which is what makes the command under test decide
// engine-attached mode.
//
// It announces from THIS process rather than starting a child, which
// core/service's own liveengine_test.go does for the same reason: flock
// attaches to the open file description, so the announcement is a fact the
// kernel reports whoever asks. What is being arranged here is the world
// the mode decision reads, and a real daemon would arrange the same world
// far more slowly.
func attachEngineTo(t *testing.T, configPath string) {
	t.Helper()
	release, err := service.AnnounceServing(configPath)
	if err != nil {
		t.Fatalf("announcing that this deployment is served: %v", err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("releasing the serving announcement: %v", err)
		}
	})
}

// TestBackupSetCreateReachesTheAttachedEngine is #543's first half: the
// set exists in the configuration the ENGINE holds, and the CLI's own file
// is untouched.
func TestBackupSetCreateReachesTheAttachedEngine(t *testing.T) {
	cliConfig := writeTestConfig(t)
	engine := startFakeEngine(t, writeTestConfig(t))
	engine.attach(t)
	attachEngineTo(t, cliConfig)

	before := readFile(t, cliConfig)
	keyPath := writeTestPrivateKey(t)

	args := createArgs(cliConfig, keyPath, "api/postgres",
		"--include", "*.dump,*.sql",
		"--stale-after", "48h",
		"--read-only",
		"--disabled",
	)
	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int { return run(args) })
	})
	if code != 0 {
		t.Fatalf("backup-set create against an attached engine exited %d, want 0\nstderr: %s", code, stderr)
	}

	if after := readFile(t, cliConfig); after != before {
		t.Errorf("the create changed the CLI's own config.yaml as well as reaching the engine; a routed change must not also be written here\nbefore:\n%s\nafter:\n%s", before, after)
	}

	got, err := engine.svc.GetBackupSet(t.Context(), "api/postgres")
	if err != nil {
		t.Fatalf("the engine does not hold the set the CLI created: %v", err)
	}

	// Every field the CLI passed, checked at the engine. A create whose
	// mapping drops a field would otherwise pass this test having written
	// a set that is not the one the operator asked for, which is the
	// failure a whole-struct assertion with a sparse fixture never sees.
	for _, f := range []struct {
		name      string
		got, want any
	}{
		{"host", got.Host, "source.example.internal"},
		{"port", got.Port, 2222},
		{"user", got.User, "backupuser"},
		{"remote_path", got.RemotePath, "/srv/backups"},
		{"local_path", got.LocalPath, "/data/backups/api"},
		{"completion_strategy", got.CompletionStrategy, "rename"},
		{"stale_after", got.StaleAfter, 48 * time.Hour},
		{"read_only", got.ReadOnly, true},
		{"disabled", got.Disabled, true},
		{"include", strings.Join(got.Include, ","), "*.dump,*.sql"},
	} {
		if f.got != f.want {
			t.Errorf("the engine's %s is %v, and the CLI asked for %v", f.name, f.got, f.want)
		}
	}

	// The key material went to the engine's key store, which is the point
	// of routing the import: on a deployment where the key store is the
	// NAS's, importing into whatever filesystem the CLI happens to be on
	// is importing into the wrong place.
	if got.ID == "" {
		t.Error("the engine's set has no id")
	}
	if !containsRequest(engine.requests(), "POST /ssh-keys") {
		t.Errorf("the key was not imported through the engine; the engine saw %v", engine.requests())
	}
}

// TestBackupSetPatchAndRemoveReachTheAttachedEngine is the same claim for
// the other two verbs, against a set the engine already holds.
func TestBackupSetPatchAndRemoveReachTheAttachedEngine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  func(configPath string) []string
		check func(t *testing.T, e *fakeEngine)
	}{
		{
			name: "patch",
			args: func(configPath string) []string {
				return []string{"backup-set", "--config", configPath, "patch", cliSet, "--stale-after", "72h"}
			},
			check: func(t *testing.T, e *fakeEngine) {
				t.Helper()
				got, err := e.svc.GetBackupSet(t.Context(), cliSet)
				if err != nil {
					t.Fatalf("reading the patched set back off the engine: %v", err)
				}
				if got.StaleAfter != 72*time.Hour {
					t.Errorf("the engine's stale_after is %s, and the CLI patched it to 72h", got.StaleAfter)
				}
			},
		},
		{
			name: "remove",
			args: func(configPath string) []string {
				return []string{"backup-set", "--config", configPath, "remove", cliSet}
			},
			check: func(t *testing.T, e *fakeEngine) {
				t.Helper()
				if _, err := e.svc.GetBackupSet(t.Context(), cliSet); err == nil {
					t.Error("the engine still holds the set the CLI removed")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cliConfig := writeTestConfig(t)
			engine := startFakeEngine(t, writeTestConfig(t))
			engine.attach(t)
			attachEngineTo(t, cliConfig)

			before := readFile(t, cliConfig)
			var code int
			stderr := captureStderr(t, func() {
				code = captureStdoutCode(t, func() int { return run(tc.args(cliConfig)) })
			})
			if code != 0 {
				t.Fatalf("backup-set %s against an attached engine exited %d, want 0\nstderr: %s", tc.name, code, stderr)
			}
			if after := readFile(t, cliConfig); after != before {
				t.Errorf("backup-set %s changed the CLI's own config.yaml as well as reaching the engine", tc.name)
			}
			tc.check(t, engine)
		})
	}
}

// TestAnEngineAttachedWriteIsStillRefusedWithNoRouteToTheEngine keeps
// #538's guard where it was. A `backup-manager daemon` serves no HTTP at
// all, and an operator who has told this command nothing about the engine
// has given it no way to hand the change over, so the refusal is still the
// only honest answer and the file still must not change.
func TestAnEngineAttachedWriteIsStillRefusedWithNoRouteToTheEngine(t *testing.T) {
	cliConfig := writeTestConfig(t)
	attachEngineTo(t, cliConfig)
	before := readFile(t, cliConfig)

	args := []string{"backup-set", "--config", cliConfig, "patch", cliSet, "--stale-after", "48h"}
	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int { return run(args) })
	})
	if code == 0 {
		t.Errorf("backup-set patch exited 0 beside an engine this command has no route to\nstderr: %s", stderr)
	}
	if after := readFile(t, cliConfig); after != before {
		t.Errorf("backup-set patch changed config.yaml beside an engine this command has no route to")
	}
	if !strings.Contains(stderr, "nothing was written") {
		t.Errorf("the refusal does not say the file was left alone:\n%s", stderr)
	}
}

// TestAnEngineAttachedWriteIsRefusedWhenTheRouteDoesNotAnswer is the same
// rule one step further in: a route that was named and cannot be reached
// is a refusal, never a fallback to writing the file directly. A quiet
// fallback is #535 wearing a different hat.
func TestAnEngineAttachedWriteIsRefusedWhenTheRouteDoesNotAnswer(t *testing.T) {
	cliConfig := writeTestConfig(t)
	attachEngineTo(t, cliConfig)
	before := readFile(t, cliConfig)

	// A port nothing is listening on. 127.0.0.1 rather than a name, so
	// this test never depends on how the machine running it resolves.
	t.Setenv("BACKUP_MANAGER_API_URL", "http://127.0.0.1:1")
	t.Setenv("BACKUP_MANAGER_API_USERNAME", "operator")
	t.Setenv("BACKUP_MANAGER_API_PASSWORD", "not-a-real-password")

	args := []string{"backup-set", "--config", cliConfig, "patch", cliSet, "--stale-after", "48h"}
	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int { return run(args) })
	})
	if code == 0 {
		t.Errorf("backup-set patch exited 0 with a route that does not answer\nstderr: %s", stderr)
	}
	if after := readFile(t, cliConfig); after != before {
		t.Errorf("backup-set patch fell back to writing config.yaml when the engine did not answer; that is the write the engine never sees")
	}
}

// TestTheModeIsAnnouncedOnBothRoutes keeps #542's promise true now that
// there are two things engine-attached can mean. An operator reading
// scroll back has to be able to say which world the command ran in.
func TestTheModeIsAnnouncedOnBothRoutes(t *testing.T) {
	t.Run("engine-attached", func(t *testing.T) {
		cliConfig := writeTestConfig(t)
		engine := startFakeEngine(t, writeTestConfig(t))
		engine.attach(t)
		attachEngineTo(t, cliConfig)

		var out string
		stderr := captureStderr(t, func() {
			out = captureStdout(t, func() {
				run([]string{"backup-set", "--config", cliConfig, "patch", cliSet, "--stale-after", "72h"})
			})
		})
		if !strings.Contains(out+stderr, modeLinePrefix+string(engineAttachedMode)) {
			t.Errorf("no engine-attached mode line:\nstdout: %s\nstderr: %s", out, stderr)
		}
	})

	t.Run("direct", func(t *testing.T) {
		cliConfig := writeTestConfig(t)
		var out string
		stderr := captureStderr(t, func() {
			out = captureStdout(t, func() {
				run([]string{"backup-set", "--config", cliConfig, "patch", cliSet, "--stale-after", "72h"})
			})
		})
		if !strings.Contains(out+stderr, modeLinePrefix+string(directMode)) {
			t.Errorf("no direct mode line:\nstdout: %s\nstderr: %s", out, stderr)
		}
	})
}

func containsRequest(seen []string, want string) bool {
	for _, r := range seen {
		if r == want {
			return true
		}
	}
	return false
}

// TestAnUnusableRouteIsRefusedAndNeverPrinted covers the setting an
// operator got wrong, which is the case a quiet default would swallow.
//
// A base URL this client cannot build from is a refusal rather than a
// downgrade: somebody wrote that value and meant it, and carrying on
// directly would perform, against a live engine, the write they were
// trying to send to it. The credential case is the same rule with a second
// reason on top, and it is why the refusal is asserted not to echo: an
// address carrying a password is printed by the very failure most likely
// to be pasted into a support ticket.
func TestAnUnusableRouteIsRefusedAndNeverPrinted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		url     string
		secret  string
		wantSay string
	}{
		{
			name:    "not a URL at all",
			url:     "nas.local:8080",
			wantSay: apiURLEnv,
		},
		{
			name: "the API's own URL rather than the engine's",
			url:  "http://nas.local:8080/api/v1",
			// The likeliest paste, and left alone it would build
			// /api/v1/api/v1/... and 404 everything.
			wantSay: apiURLEnv,
		},
		{
			name:    "credentials in the address",
			url:     "http://admin:hunter2@nas.local:8080",
			secret:  "hunter2",
			wantSay: apiURLEnv,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cliConfig := writeTestConfig(t)
			attachEngineTo(t, cliConfig)
			before := readFile(t, cliConfig)

			t.Setenv("BACKUP_MANAGER_API_URL", tc.url)
			t.Setenv("BACKUP_MANAGER_API_USERNAME", "operator")
			t.Setenv("BACKUP_MANAGER_API_PASSWORD", "not-a-real-password")

			args := []string{"backup-set", "--config", cliConfig, "patch", cliSet, "--stale-after", "48h"}
			var code int
			var out string
			stderr := captureStderr(t, func() {
				out = captureStdout(t, func() { code = run(args) })
			})

			if code == 0 {
				t.Errorf("backup-set patch exited 0 with an unusable route\nstderr: %s", stderr)
			}
			if after := readFile(t, cliConfig); after != before {
				t.Error("backup-set patch wrote config.yaml directly when the route it was given could not be built; that is the write the engine never sees")
			}
			// The refusal has to be about the ADDRESS, not the one
			// engine-attached mode prints when nothing named a route at
			// all. Those send an operator to two different places: fix
			// what you set, or stop the process. Asserting only that the
			// variable is named somewhere cannot tell them apart, because
			// the no-route refusal names it too, as the remedy.
			if !strings.Contains(stderr, "$"+tc.wantSay+" does not name an engine") {
				t.Errorf("the refusal does not say that $%s itself is the problem, so an operator is sent to stop a process over a setting they got wrong:\n%s", tc.wantSay, stderr)
			}
			if tc.secret != "" && strings.Contains(out+stderr, tc.secret) {
				t.Errorf("the refusal printed the password out of the address it was refusing:\nstdout: %s\nstderr: %s", out, stderr)
			}
		})
	}
}

// TestAFirstConfigurationIsNotRouted keeps the one write that must not
// acquire a route by accident.
//
// `backup-set create` against a path with no config.yaml writes a whole
// FIRST configuration. Finding a process serving the journal it names
// means that deployment is already configured, which is what makes this
// case a refusal in the first place, and POST /system/first-run is not an
// operation an already-configured engine has any business being sent. So
// naming a route must change nothing here.
func TestAFirstConfigurationIsNotRouted(t *testing.T) {
	cliConfig := writeTestConfig(t)
	keyPath := writeTestPrivateKey(t)
	dbPath := filepath.Join(filepath.Dir(cliConfig), "state.db")

	engine := startFakeEngine(t, writeTestConfig(t))
	engine.attach(t)
	attachEngineTo(t, cliConfig)

	// The path the operator meant to type, one letter out, against the
	// live deployment's own journal.
	mistyped := filepath.Join(filepath.Dir(cliConfig), "confg.yaml")
	args := createArgs(mistyped, keyPath, "api/postgres", "--state-database", dbPath)

	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int { return run(args) })
	})
	if code == 0 {
		t.Errorf("backup-set create exited 0 writing a FIRST configuration beside a serving engine, with a route named\nstderr: %s", stderr)
	}
	if _, err := os.Stat(mistyped); !os.IsNotExist(err) {
		t.Errorf("a first configuration was written at %s (stat err = %v)", mistyped, err)
	}
	if seen := engine.requests(); len(seen) != 0 {
		t.Errorf("the first-run write reached the engine: %v", seen)
	}
}

// TestAWindowTheWireCannotCarryIsRefusedRatherThanTruncated records a real
// asymmetry between the two routes, deliberately, because the alternative
// is worse and because an asymmetry nobody wrote down is one nobody knows
// about.
//
// core/service carries a completion window as a time.Duration and the API
// carries it as a whole number of seconds. So `--stable-for 1500ms` is a
// value the direct route can persist and the wire cannot express. Sending
// it anyway would put 1s in the engine's configuration and 1.5s in one
// written directly, from the same command line, and both would report
// success: two deployments disagreeing about when a file is finished, with
// nothing anywhere saying so.
//
// The engine route therefore refuses it, before anything is sent, and says
// what to type instead.
func TestAWindowTheWireCannotCarryIsRefusedRatherThanTruncated(t *testing.T) {
	cliConfig := writeTestConfig(t)
	engine := startFakeEngine(t, writeTestConfig(t))
	engine.attach(t)
	attachEngineTo(t, cliConfig)

	args := createArgs(cliConfig, writeTestPrivateKey(t), "api/postgres",
		"--completion-strategy", "stable", "--stable-for", "1500ms")
	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int { return run(args) })
	})
	if code == 0 {
		t.Fatalf("a sub-second --stable-for was accepted through the engine, so the engine holds a different window from the one that was typed\nstderr: %s", stderr)
	}
	if !strings.Contains(stderr, "whole number of seconds") {
		t.Errorf("the refusal does not say what the wire can carry, so an operator cannot tell what to type instead:\n%s", stderr)
	}
	if containsRequest(engine.requests(), "POST /backup-sets") {
		t.Errorf("the create was sent anyway; a value the wire cannot carry has to be refused before the wire: %v", engine.requests())
	}
}
