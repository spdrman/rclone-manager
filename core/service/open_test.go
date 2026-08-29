package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeTestConfigFile builds a minimal, valid config.yaml against real temp
// directories, wired through the "local" transport backend so this test
// needs no network and no Docker — the same fixture shape
// cmd/backup-manager/main_test.go's writeTestConfig uses for its own
// end-to-end smoke tests, reproduced here because Open is this package's
// equivalent "load a real file off disk" entry point and had no direct
// test of its own otherwise.
func writeTestConfigFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "backup.dump"), []byte("service.Open smoke test payload"), 0o644); err != nil {
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

// TestOpen_WiresARealServiceAgainstARealConfigFile proves the production
// constructor (the one apps/common/webhost actually calls, since it
// cannot construct a *config.Config or *state.Journal itself to call New
// directly) loads a real YAML file, opens/migrates a real SQLite journal,
// wires a real rclone transport, and the result actually works end to
// end: SubmitRunCycle against it drives a real local-to-local copy, not
// just an empty, zero-source no-op the way this file's other tests (built
// through New) do.
func TestOpen_WiresARealServiceAgainstARealConfigFile(t *testing.T) {
	configPath := writeTestConfigFile(t)

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cleanup()

	if svc.ConfigRevision() == "" {
		t.Fatal("ConfigRevision is empty for a successfully opened service")
	}

	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-1",
		Actor:          "alice",
		ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}

	done := waitForTerminalStatus(t, svc, op.ID)
	if done.Status != "completed" {
		t.Fatalf("Status = %q, want %q (Error = %q)", done.Status, "completed", done.Error)
	}
}

// TestOpen_InvalidConfigPathFails is Open's negative control: a bad path
// must fail loudly, not return a half-usable BackupService.
func TestOpen_InvalidConfigPathFails(t *testing.T) {
	_, cleanup, err := Open(context.Background(), filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("Open with a nonexistent config path: error = nil, want an error")
	}
}
