package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "backup.dump"), []byte("cli smoke test payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "state.db")
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
