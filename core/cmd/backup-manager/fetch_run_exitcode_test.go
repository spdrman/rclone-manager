package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFailingVerificationConfig mirrors writeTestConfig (main_test.go),
// with one difference: the backup set names an application validator
// (config.Validation.Command) that always refuses. Every artifact this
// backup set transfers therefore fails FR-13 verification and lands in
// QUARANTINED rather than VERIFIED -- issue #283's repro of "discovery
// succeeds, then every artifact fails afterward", reproduced against the
// real local-backend path rather than a hand-fabricated journal state.
func writeFailingVerificationConfig(t *testing.T) string {
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

	scriptPath := filepath.Join(dir, "always-fail.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("writing validator script: %v", err)
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
		"        validation:\n" +
		"          command:\n" +
		"            executable: " + scriptPath + "\n" +
		"            timeout: 5s\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// TestRun_FetchExitsNonZeroWhenEveryArtifactFailsVerification is issue
// #283's Behavioral Contract, first Given/When/Then: a backup set whose
// artifact transfers and then fails verification must make `fetch` exit
// non-zero, and its summary line must say how many artifacts ended
// failed, not just report the discovery/reconcile counters that never
// saw the failure at all.
func TestRun_FetchExitsNonZeroWhenEveryArtifactFailsVerification(t *testing.T) {
	configPath := writeFailingVerificationConfig(t)

	var got int
	out := captureStdout(t, func() {
		got = run([]string{"fetch", "--config", configPath, "--source", "production", "--backup-set", "postgres-primary"})
	})

	if got == 0 {
		t.Errorf("fetch exit code = 0, want non-zero: the one artifact this cycle discovered was transferred and then quarantined at verification, not backed up.\nstdout:\n%s", out)
	}
	if !strings.Contains(out, "failed=1") {
		t.Errorf("fetch's summary line does not report the failed artifact count (want it to contain %q).\nstdout:\n%s", "failed=1", out)
	}
}

// TestRun_RunExitsNonZeroWhenEveryArtifactFailsVerification is the same
// repro through `run`, the multi-set sibling `fetch` is meant to agree
// with.
func TestRun_RunExitsNonZeroWhenEveryArtifactFailsVerification(t *testing.T) {
	configPath := writeFailingVerificationConfig(t)

	if got := run([]string{"run", "--config", configPath}); got == 0 {
		t.Error("run exit code = 0, want non-zero: the one artifact this cycle discovered was transferred and then quarantined at verification, not backed up")
	}
}

// TestRun_FetchAndRunAgreeOnAFailedCycle is issue #283's third acceptance
// criterion pinned directly: given equivalent single-backup-set configs
// where every artifact fails verification, `fetch` and `run` must reach
// the same verdict (both non-zero) rather than one trusting the journal
// and the other trusting only discovery/reconcile counters.
func TestRun_FetchAndRunAgreeOnAFailedCycle(t *testing.T) {
	fetchConfig := writeFailingVerificationConfig(t)
	runConfig := writeFailingVerificationConfig(t)

	fetchExit := run([]string{"fetch", "--config", fetchConfig, "--source", "production", "--backup-set", "postgres-primary"})
	runExit := run([]string{"run", "--config", runConfig})

	if fetchExit == 0 || runExit == 0 {
		t.Fatalf("fetch and run must both fail this cycle: fetch=%d run=%d, want both non-zero", fetchExit, runExit)
	}
}

// TestRun_FetchExitsZeroOnACleanCycle is the Behavioral Contract's second
// Given/When/Then: a backup set whose artifact reaches a good state must
// still exit 0, exactly as before this fix.
func TestRun_FetchExitsZeroOnACleanCycle(t *testing.T) {
	configPath := writeTestConfig(t)

	out := captureStdout(t, func() {
		if got := run([]string{"fetch", "--config", configPath, "--source", "production", "--backup-set", "postgres-primary"}); got != 0 {
			t.Errorf("fetch exit code = %d, want 0 for a clean cycle", got)
		}
	})
	if !strings.Contains(out, "failed=0") {
		t.Errorf("fetch's summary line should report failed=0 on a clean cycle.\nstdout:\n%s", out)
	}
}

// TestRun_FetchExitsNonZeroWhenReconciliationFindsAnIrrecoverableLoss is
// the adversarial review's High finding on PR #303: `fetch`'s own
// reconcile pass, run before discovery and before this cycle's own
// forward pipeline, can discover on its own that a previously-durable
// artifact's local final copy has gone missing after its remote source
// was already cleaned up -- COMPLETE -> QUARANTINED_LOST, total,
// permanent loss of that restore point. Reconcile itself returns no error
// for this (finding and recording the loss is reconciliation doing its
// job correctly), so before this fix a cron-scheduled fetch that hit this
// exact case still exited 0.
func TestRun_FetchExitsNonZeroWhenReconciliationFindsAnIrrecoverableLoss(t *testing.T) {
	configPath := writeTestConfig(t)

	// First fetch drives the one seeded artifact all the way to COMPLETE:
	// local final copy durable, remote source already deleted.
	if got := run([]string{"fetch", "--config", configPath, "--source", "production", "--backup-set", "postgres-primary"}); got != 0 {
		t.Fatalf("precondition: first fetch = %d, want 0 (a clean cycle)", got)
	}

	localFinal := filepath.Join(filepath.Dir(configPath), "local", "backup.dump")
	if err := os.Remove(localFinal); err != nil {
		t.Fatalf("corrupting the durable local copy: %v", err)
	}

	// Second fetch has nothing new to discover on the remote (already
	// deleted), but reconciliation finds the durable local copy gone with
	// the remote already confirmed gone too, an irrecoverable loss
	// discovered during an otherwise successful reconciliation pass.
	var got int
	out := captureStdout(t, func() {
		got = run([]string{"fetch", "--config", configPath, "--source", "production", "--backup-set", "postgres-primary"})
	})
	if got == 0 {
		t.Errorf("fetch exit code = 0, want non-zero: reconciliation just discovered this artifact's only remaining copy is gone (QUARANTINED_LOST).\nstdout:\n%s", out)
	}
}

// TestRun_RunExitsNonZeroWhenReconciliationFindsAnIrrecoverableLoss is the
// same repro through `run`, the multi-set sibling `fetch` is meant to
// agree with (see TestRun_FetchAndRunAgreeOnAFailedCycle above).
func TestRun_RunExitsNonZeroWhenReconciliationFindsAnIrrecoverableLoss(t *testing.T) {
	configPath := writeTestConfig(t)

	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("precondition: first run = %d, want 0 (a clean cycle)", got)
	}

	localFinal := filepath.Join(filepath.Dir(configPath), "local", "backup.dump")
	if err := os.Remove(localFinal); err != nil {
		t.Fatalf("corrupting the durable local copy: %v", err)
	}

	if got := run([]string{"run", "--config", configPath}); got == 0 {
		t.Error("run exit code = 0, want non-zero: reconciliation just discovered this artifact's only remaining copy is gone (QUARANTINED_LOST)")
	}
}

// TestRun_FetchDryRunExitsZeroRegardlessOfJournalHistory is issue #283's
// fourth acceptance criterion: --dry-run inspects the remote only, never
// the journal's per-artifact outcomes, so it must exit 0 even against a
// backup set whose journal already carries QUARANTINED artifacts from an
// earlier, real fetch.
func TestRun_FetchDryRunExitsZeroRegardlessOfJournalHistory(t *testing.T) {
	configPath := writeFailingVerificationConfig(t)

	// A real fetch first, so the journal actually holds a QUARANTINED
	// artifact by the time --dry-run runs.
	if got := run([]string{"fetch", "--config", configPath, "--source", "production", "--backup-set", "postgres-primary"}); got == 0 {
		t.Fatal("precondition failed: the real fetch above was supposed to fail this cycle")
	}

	if got := run([]string{"fetch", "--config", configPath, "--source", "production", "--backup-set", "postgres-primary", "--dry-run"}); got != 0 {
		t.Errorf("fetch --dry-run exit code = %d, want 0 regardless of the journal's FAILED/QUARANTINED history", got)
	}
}
