package machinegate_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/service"
)

// A deliberate copy of two of core/service's own test helpers, and the one
// number that had to change on the way over.
//
// The copy is the smaller cost. The alternative is exporting a test scaffold
// from core/service so a single machine-tier test can reach it, which puts
// it on that package's public API permanently for the sake of one caller,
// while two dozen pure tests keep the original.
//
// What is not copied is the deadline arithmetic the original does against
// `go test -timeout`. This tier does not run under a fixed timeout at all: it
// runs under a watchdog that derives its bound from observed progress. So
// the budget here is a single stated number with its own reasoning, rather
// than a fraction of a deadline that does not exist.

// The two helpers below are core/service's own openTestService and
// waitForTerminalStatus, restated here rather than shared.
//
// #448 moved the end-to-end backup-set test out of package service, which
// is where those helpers live and where they have to stay: they are used by
// two dozen pure tests in that package, and exporting them so one machine-
// tier test could reach them would put a test scaffold on the service's
// public API for the rest of time.
//
// So this is a deliberate copy, and it is a small one because it only needs
// the exported surface: service.Open, svc.GetOperation and the operation's
// own status. What it does NOT copy is the deadline-aware budget arithmetic
// that package's version has, because the machine tier does not run under a
// fixed `go test -timeout` at all (#256): it runs under gotestwatch, which
// derives its bound from progress. One number, stated here.

// terminalStatusBudget is how long a run driven against a real machine gets
// to reach a terminal status. Thirty seconds is the number package service
// arrived at for exactly this test (#385): a real Docker plus SSH round
// trip needs far more than the two seconds its in-process tests allow, and
// thirty is long enough that a correct cycle cannot lose it on a loaded
// host, so reaching it means the operation is wedged rather than slow.
const terminalStatusBudget = 30 * time.Second

// openService opens a BackupService over a throwaway config file. The
// config names a local-backend backup set that nothing in this file uses;
// it is there because service.Open validates the file it is handed, and the
// backup set the test cares about is created through the API afterwards.
func openService(t *testing.T) *service.BackupService {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "backup.dump"), []byte("placeholder payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + filepath.Join(dir, "local") + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n  timezone: UTC\n  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	svc, cleanup, err := service.Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("service.Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	return svc
}

// waitForTerminalStatus polls one operation until it completes or fails.
//
// The poll backs off rather than staying at its opening interval, for the
// reason package service's own copy gives: GetOperation reads the same
// journal the running cycle is writing to, over the one connection
// internal/state.Open allows, so every poll is a poll that cycle cannot be
// using.
func waitForTerminalStatus(t *testing.T, svc *service.BackupService, id string) service.Operation {
	t.Helper()
	const (
		firstPoll = 2 * time.Millisecond
		maxPoll   = 50 * time.Millisecond
	)
	poll := firstPoll
	deadline := time.Now().Add(terminalStatusBudget)
	for {
		op, err := svc.GetOperation(context.Background(), id)
		if err != nil {
			t.Fatal(fmt.Errorf("GetOperation(%q): %w", id, err))
		}
		if op.Status == "completed" || op.Status == "failed" {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %q did not reach a terminal status within %s (last status %q). That budget is not a latency bound: it is long enough that a correct cycle cannot lose it on a loaded host, so reaching it means the operation is wedged, not slow", id, terminalStatusBudget, op.Status)
		}
		time.Sleep(poll)
		if poll *= 2; poll > maxPoll {
			poll = maxPoll
		}
	}
}
