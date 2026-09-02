package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeCappedConfigFile is writeTestConfigFile (open_test.go) with a
// storage cap small enough that FR-21's admission check refuses the one
// artifact waiting on the remote. That produces issue #361's shape through
// the real wiring: a cycle that ran perfectly, reported no error of any
// kind, and backed nothing up.
func writeCappedConfigFile(t *testing.T, capBytes string) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "backup.dump"), []byte("a payload that will never fit"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"capacity:\n" +
		"  cap_bytes: " + capBytes + "\n" +
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
		"retention:\n  timezone: UTC\n  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

func runOneCycleSummary(t *testing.T, configPath string) cycleSummary {
	t.Helper()
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cleanup() }()

	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-1", Actor: "alice", ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}
	done := waitForTerminalStatus(t, svc, op.ID)

	var summary cycleSummary
	if err := json.Unmarshal([]byte(done.Result), &summary); err != nil {
		t.Fatalf("unmarshalling the cycle summary %q: %v", done.Result, err)
	}
	return summary
}

// TestSubmitRunCycle_SummaryTellsACycleThatDeliveredFromOneThatDidNot is
// issue #361 on the API surface. An operation row completes when the cycle
// ran, which is this package's own deliberate boundary (see
// validator_integration_test.go: an artifact's quarantine is a business
// outcome, not an operation failure), so "completed" alone cannot tell a
// cycle that backed everything up apart from one that backed nothing up.
// The summary has to, and this pins both directions from the same wiring.
func TestSubmitRunCycle_SummaryTellsACycleThatDeliveredFromOneThatDidNot(t *testing.T) {
	delivered := runOneCycleSummary(t, writeTestConfigFile(t))
	if delivered.ArtifactsWalked != 1 || delivered.ArtifactsThrough != 1 {
		t.Errorf("cycle that backed the artifact up: walked=%d through=%d, want 1/1", delivered.ArtifactsWalked, delivered.ArtifactsThrough)
	}

	refused := runOneCycleSummary(t, writeCappedConfigFile(t, "1"))
	if refused.ArtifactsWalked != 1 {
		t.Errorf("cycle that backed nothing up: walked=%d, want 1", refused.ArtifactsWalked)
	}
	if refused.ArtifactsThrough != 0 {
		t.Errorf("cycle that backed nothing up: through=%d, want 0", refused.ArtifactsThrough)
	}
}

// TestSubmitRunCycle_SummaryOfAnIdleCycleWalksNothing keeps the same
// distinction the exit status keeps: a cycle with nothing waiting reports
// nothing walked, so a reader can tell it apart from a cycle whose
// artifacts all failed to get through.
func TestSubmitRunCycle_SummaryOfAnIdleCycleWalksNothing(t *testing.T) {
	// cap_bytes: 0 is the documented sentinel for "no cap", so this is the
	// same fixture as the refused cycle above with the refusal taken away:
	// the positive control that the cap, and nothing about the fixture
	// itself, is what stops the artifact getting through.
	uncapped := runOneCycleSummary(t, writeCappedConfigFile(t, "0"))
	if uncapped.ArtifactsWalked != 1 || uncapped.ArtifactsThrough != 1 {
		t.Fatalf("positive control: an uncapped run of this fixture should deliver its one artifact, got walked=%d through=%d", uncapped.ArtifactsWalked, uncapped.ArtifactsThrough)
	}

	// A backup set with nothing waiting on its remote, which is what a
	// poll interval sees almost every time it runs. It walks nothing, so
	// nothing can have failed to get through, and no reader of these two
	// numbers can mistake it for the capped cycle above.
	idle := runOneCycleSummary(t, writeEmptyRemoteConfigFile(t))
	if idle.ArtifactsWalked != 0 || idle.ArtifactsThrough != 0 {
		t.Errorf("idle cycle: walked=%d through=%d, want 0/0", idle.ArtifactsWalked, idle.ArtifactsThrough)
	}
}

// writeEmptyRemoteConfigFile is writeTestConfigFile with no object on the
// remote at all: the shape a poll interval sees almost every time it runs.
func writeEmptyRemoteConfigFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
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
	return configPath
}
