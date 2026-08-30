package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"
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
	defer func() { _ = cleanup() }()

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
			_ = cleanup()
		}
		t.Fatal("Open with a nonexistent config path: error = nil, want an error")
	}
}

// TestOpen_SweepsInterruptedOperationsFromAPreviousProcess is issue #118
// item 2's startup-sweep requirement, proven at Open itself (the
// production entry point), not just at New (which Open calls and which
// this file's other tests exercise indirectly): an operation row left at
// "running" by a process that was killed mid-cycle must not go on
// reporting "running" forever to a client polling
// GET /api/v1/operations/{id} against the next process that opens this
// same journal.
func TestOpen_SweepsInterruptedOperationsFromAPreviousProcess(t *testing.T) {
	configPath := writeTestConfigFile(t)

	cfg, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}

	// Stand in for a process that created and started an operation, then
	// was killed before it ever reached a terminal status: open the exact
	// journal file Open (below) will itself open, seed a "running" row
	// directly against it, and close it. This deliberately does not go
	// through New/Open's own path to create the row, so the assertion
	// below actually proves Open's sweep found a row it did not create,
	// not merely that this test's own process behaves consistently.
	seedJournal, err := state.Open(context.Background(), cfg.State.Database)
	if err != nil {
		t.Fatalf("state.Open (seed): %v", err)
	}
	outcome, err := seedJournal.CreateOperation(context.Background(), state.OperationRequest{
		OperationID:    "op_interrupted",
		IdempotencyKey: "idem-interrupted",
		Actor:          "alice",
		ConfigRevision: "rev-does-not-matter-for-this-test",
		Action:         ActionRunCycle,
		Parameters:     "{}",
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateOperation (seed): %v", err)
	}
	if err := seedJournal.MarkOperationRunning(context.Background(), outcome.Operation.OperationID, time.Now().UTC()); err != nil {
		t.Fatalf("MarkOperationRunning (seed): %v", err)
	}
	if err := seedJournal.Close(); err != nil {
		t.Fatalf("close seed journal: %v", err)
	}

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cleanup() }()

	op, err := svc.GetOperation(context.Background(), "op_interrupted")
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if op.Status != "failed" {
		t.Fatalf("Status = %q, want %q (Open must sweep an operation left running by a previous process)", op.Status, "failed")
	}
	if op.Error == "" {
		t.Error("Error is empty on a swept operation, want a reason")
	}
}
