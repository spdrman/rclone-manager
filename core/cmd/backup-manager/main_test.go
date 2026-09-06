package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The dispatch smoke tests, and the fixture most of this package's other
// suites are built on.
//
// Everything here calls run() with an argv rather than calling a cmd
// function, because the thing being checked is what an operator's shell
// gets back: an unknown command, a missing operand and a working config all
// have exit statuses this binary promises, and only the top of the dispatch
// decides them.
//
// writeTestConfig is the shared fixture and it earns its place by being
// real. A local directory standing in as the remote, wired through the local
// transport backend, means a `run` in these tests performs an actual
// discover, transfer, verify, commit and delete against actual files, with
// no network and no Docker. That is what makes the exit-status cells in the
// files around this one worth anything: they are reading the status of a
// cycle that happened.

func TestRun_NoArgsPrintsUsageAndFails(t *testing.T) {
	if got := run(nil); got != 2 {
		t.Errorf("run(nil) = %d, want 2", got)
	}
}

func TestRun_UnknownCommandFails(t *testing.T) {
	if got := run([]string{"not-a-real-command"}); got != 2 {
		t.Errorf("run([\"not-a-real-command\"]) = %d, want 2", got)
	}
}

func TestRun_VersionSucceeds(t *testing.T) {
	if got := run([]string{"version"}); got != 0 {
		t.Errorf("run([\"version\"]) = %d, want 0", got)
	}
}

// writeTestConfig builds a minimal, valid config against real temp
// directories: a one-file "remote" (a plain local directory) and an empty
// local destination, wired through the "local" transport backend so this
// test needs no network and no Docker.
func writeTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return writeTestConfigIn(t, dir, filepath.Join(dir, "state.db"))
}

// writeTestConfigFor is writeTestConfig for a SECOND configuration file
// that describes the same deployment as other.
//
// It exists for the engine-attached tests (#543, #555). Those stand an
// engine up over a configuration file of its own and point the CLI at a
// different one, which is what lets them assert that a routed write
// changed the engine's file and not the CLI's. What they must not do is
// make the two files two DEPLOYMENTS: a routed write now checks that the
// engine it reached is the deployment the command was typed at
// (deploymentcheck.go), so two journals would be refused, correctly, and
// every one of those tests would be driving the refusal instead of the
// route it is about.
//
// The journal is the deployment, so both files name one journal and
// everything else about them is their own. That is also the truthful
// arrangement: a real CLI and a real engine share one config.yaml and one
// journal, and the second file here is a fixture's way of watching one end
// without disturbing the other.
func writeTestConfigFor(t *testing.T, other string) string {
	t.Helper()
	return writeTestConfigIn(t, t.TempDir(), journalNamedByTestConfig(t, other))
}

// journalNamedByTestConfig reads the state database out of a configuration
// this file wrote, so the caller above does not have to reconstruct a path
// from a convention and then drift from it.
func journalNamedByTestConfig(t *testing.T, configPath string) string {
	t.Helper()
	const key = "  database: "
	for _, line := range strings.Split(readFile(t, configPath), "\n") {
		if strings.HasPrefix(line, key) {
			return strings.TrimSpace(strings.TrimPrefix(line, key))
		}
	}
	t.Fatalf("%s names no state database, so nothing here can say which deployment it is", configPath)
	return ""
}

// writeTestConfigIn is the fixture itself: one deployment, in dir, with
// its journal at dbPath.
func writeTestConfigIn(t *testing.T, dir, dbPath string) string {
	t.Helper()
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "backup.dump"), []byte("cli smoke test payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + dbPath + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + localDir + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// TestRun_CheckAgainstAWorkingConfig is this binary's own end-to-end
// smoke test for `check`: a real config file, a real (freshly created)
// state database, no mocks.
func TestRun_CheckAgainstAWorkingConfig(t *testing.T) {
	configPath := writeTestConfig(t)
	if got := run([]string{"check", "--config", configPath}); got != 0 {
		t.Errorf("run([\"check\", \"--config\", %q]) = %d, want 0", configPath, got)
	}
}

// TestRun_RunCommandProcessesAnArtifactEndToEnd drives `run` itself
// (FR-1's "run performs one processing cycle and exits") against a real
// local-backend remote and confirms the artifact actually lands in the
// configured local destination directory, exactly as an operator invoking
// this binary for real would observe.
func TestRun_RunCommandProcessesAnArtifactEndToEnd(t *testing.T) {
	configPath := writeTestConfig(t)

	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run([\"run\", \"--config\", %q]) = %d, want 0", configPath, got)
	}

	localFinal := filepath.Join(filepath.Dir(configPath), "local", "backup.dump")
	if _, err := os.Stat(localFinal); err != nil {
		t.Errorf("expected the artifact to land at %s: %v", localFinal, err)
	}

	// status must now see this backup set as HEALTHY (a fresh, known-good
	// backup with nothing else needing attention), and exit 0 accordingly.
	if got := run([]string{"status", "--config", configPath}); got != 0 {
		t.Errorf("run([\"status\", \"--config\", %q]) = %d, want 0 (a fresh backup should report HEALTHY)", configPath, got)
	}

	// sources, artifacts and retention are all read-only and must succeed
	// against the same config without needing a remote.
	for _, args := range [][]string{
		{"sources", "--config", configPath},
		{"artifacts", "--config", configPath},
		{"retention", "--config", configPath, "--dry-run"},
	} {
		if got := run(args); got != 0 {
			t.Errorf("run(%v) = %d, want 0", args, got)
		}
	}
}

// TestRun_CatalogRequiresRebuildSubcommand proves `catalog` with no
// subcommand, and `catalog` with an unknown one, both fail with a usage
// error rather than a panic or a silent no-op.
func TestRun_CatalogRequiresRebuildSubcommand(t *testing.T) {
	if got := run([]string{"catalog"}); got != 2 {
		t.Errorf("run([\"catalog\"]) = %d, want 2", got)
	}
	if got := run([]string{"catalog", "frobnicate"}); got != 2 {
		t.Errorf("run([\"catalog\", \"frobnicate\"]) = %d, want 2", got)
	}
}

// TestRun_CatalogRebuildAgainstAWorkingConfig is this binary's own
// end-to-end smoke test for `catalog rebuild`: run a real cycle so a
// committed artifact and its sidecar recovery manifest both exist, then
// confirm `catalog rebuild --dry-run` (against the still-intact journal,
// so every artifact is already present) and the real `catalog rebuild`
// both succeed.
func TestRun_CatalogRebuildAgainstAWorkingConfig(t *testing.T) {
	configPath := writeTestConfig(t)

	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run([\"run\", \"--config\", %q]) = %d, want 0", configPath, got)
	}

	for _, args := range [][]string{
		{"catalog", "rebuild", "--dry-run", "--config", configPath},
		{"catalog", "rebuild", "--config", configPath},
	} {
		if got := run(args); got != 0 {
			t.Errorf("run(%v) = %d, want 0", args, got)
		}
	}
}

// TestRun_FetchRequiresSourceAndBackupSet proves fetch's required flags
// are actually enforced with a clear usage error, not a nil-pointer panic.
func TestRun_FetchRequiresSourceAndBackupSet(t *testing.T) {
	configPath := writeTestConfig(t)
	if got := run([]string{"fetch", "--config", configPath}); got != 2 {
		t.Errorf("run([\"fetch\"]) with no --source/--backup-set = %d, want 2", got)
	}
}

// TestRun_ValidateRejectsMalformedArtifactID proves a malformed
// <artifact-id> argument is reported as a usage-shaped failure, not a
// panic.
func TestRun_ValidateRejectsMalformedArtifactID(t *testing.T) {
	configPath := writeTestConfig(t)
	if got := run([]string{"validate", "--config", configPath, "not-a-valid-id"}); got == 0 {
		t.Error("run([\"validate\", \"not-a-valid-id\"]) = 0, want a non-zero exit code")
	}
}
