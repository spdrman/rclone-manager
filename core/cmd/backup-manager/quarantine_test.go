// The three quarantine actions, against artifacts driven into quarantine
// through the real lifecycle.
//
// The fixture writes against the journal directly and bypasses the CLI, and
// it has to: these three verbs exist for artifacts an ordinary cycle can
// never produce on demand. What it does not do is poke a row that merely
// reads like a quarantined artifact. It walks the same lifecycle sequence
// the app layer's own suite uses, so a verdict reached here is reached over
// a genuine history.
//
// The corrupt variant is the one that makes the cells discriminate. An
// intact copy has to be trusted again and a corrupt one has to be refused,
// and a revalidation that always said yes would satisfy every cell that only
// staged healthy artifacts.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

var quarantineFixtureEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// stageQuarantinedArtifact drives one artifact down the real lifecycle
// graph, through the real journal, into QUARANTINED (or, with corrupt
// set, into QUARANTINED with a local file whose bytes no longer match the
// hash recorded at VERIFIED) -- the exact same sequence
// core/internal/app/reinstatedhealth_test.go's own stageArtifact uses to
// prove issue #227's reinstatement count, so a fixture built here is not
// a hand-poked row that merely reads like a real quarantined artifact.
//
// It writes directly against the config's own state database and local
// directory, bypassing the CLI entirely, because these three commands
// (revalidate/retry/reinstate) are unreachable from the ordinary `run`
// pipeline: nothing in a healthy end-to-end cycle ever produces a
// QUARANTINED artifact to act on.
func stageQuarantinedArtifact(t *testing.T, configPath, name string, corrupt bool) model.ArtifactID {
	t.Helper()
	dir := filepath.Dir(configPath)
	dbPath := filepath.Join(dir, "state.db")
	localDir := filepath.Join(dir, "quarantine-local")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	ctx := context.Background()
	j, err := state.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = j.Close() }()

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID(%s): %v", name, err)
	}

	payload := "payload for " + name
	local := filepath.Join(localDir, name)
	if err := os.WriteFile(local, []byte(payload), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sum := sha256.Sum256([]byte(payload))
	hashes := &state.HashUpdate{Alg: "sha256", Hash: hex.EncodeToString(sum[:])}

	if _, err := j.Discover(ctx, artifact, name+"-discover", "/backups/"+name, state.RemoteIdentity{}, quarantineFixtureEpoch); err != nil {
		t.Fatalf("Discover %s: %v", name, err)
	}

	steps := []struct {
		from, to  lifecycle.State
		localPath *string
		hashes    *state.HashUpdate
	}{
		{from: lifecycle.Discovered, to: lifecycle.Transferring},
		{from: lifecycle.Transferring, to: lifecycle.Transferred},
		{from: lifecycle.Transferred, to: lifecycle.Verifying},
		{from: lifecycle.Verifying, to: lifecycle.Verified, hashes: hashes},
		{from: lifecycle.Verified, to: lifecycle.Committing},
		{from: lifecycle.Committing, to: lifecycle.Committed, localPath: &local},
		{from: lifecycle.Committed, to: lifecycle.Quarantined},
	}
	for i, s := range steps {
		if _, err := j.RecordTransition(ctx, state.Transition{
			Artifact:   artifact,
			Key:        fmt.Sprintf("%s-%d-%s", name, i, s.to),
			From:       string(s.from),
			To:         string(s.to),
			LocalPath:  s.localPath,
			Hashes:     s.hashes,
			Detail:     "test fixture",
			OccurredAt: quarantineFixtureEpoch.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("%s: %s -> %s: %v", name, s.from, s.to, err)
		}
	}

	// corrupt is applied AFTER the real COMMITTED/VERIFIED hash was
	// recorded, so revalidate/reinstate's local-file check has something
	// real to catch: the recorded hash and the file on disk genuinely
	// disagree, not a fixture that never matched in the first place.
	if corrupt {
		if err := os.WriteFile(local, []byte("corrupted"), 0o644); err != nil {
			t.Fatalf("WriteFile (corrupt): %v", err)
		}
	}

	return artifact
}

// writeQuarantineTestConfig is writeTestConfig's counterpart for the
// quarantine tests: same shape, but the state database and quarantined
// fixture are staged directly (see stageQuarantinedArtifact), never
// through a `run` cycle, since a healthy cycle never produces one.
func writeQuarantineTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
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
		"        local_path: " + filepath.Join(dir, "quarantine-local") + "\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

func TestRun_QuarantineRequiresVerbAndArtifact(t *testing.T) {
	configPath := writeQuarantineTestConfig(t)
	for _, args := range [][]string{
		{"quarantine", "--config", configPath},
		{"quarantine", "--config", configPath, "revalidate"},
		{"quarantine", "--config", configPath, "not-a-real-verb", "production/postgres-primary/x"},
	} {
		if got := run(args); got != 2 {
			t.Errorf("run(%v) = %d, want 2", args, got)
		}
	}
}

// TestRun_QuarantineRevalidateRefusesAHealthyArtifact proves revalidate
// is genuinely a QUARANTINE action, not `validate` under a new name: it
// refuses an artifact that was never quarantined, the mirror image of
// `validate` refusing a QUARANTINED one (TestRun_ValidateRejectsMalformedArtifactID
// covers validate's own input-shape refusal; this is the state-shape one).
func TestRun_QuarantineRevalidateRefusesAHealthyArtifact(t *testing.T) {
	configPath := writeTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run([\"run\"]) = %d, want 0", got)
	}
	args := []string{"quarantine", "--config", configPath, "revalidate", "production/postgres-primary/backup.dump"}
	if got := run(args); got == 0 {
		t.Errorf("run(%v) on a COMMITTED (never-quarantined) artifact = 0, want non-zero", args)
	}
}

// TestRun_QuarantineAcceptsFlagsOnEitherSideOfItsOperands mirrors issue
// #188's own fix for `validate` (TestRun_ValidateAcceptsFlagsOnEitherSideOfItsOperand):
// `quarantine` takes TWO operands (a verb and an artifact id), not one, but
// parseFlagsAroundOperands (setup.go) is the identical mechanism, so
// --config has to work before, between and after them.
func TestRun_QuarantineAcceptsFlagsOnEitherSideOfItsOperands(t *testing.T) {
	configPath := writeQuarantineTestConfig(t)
	id := stageQuarantinedArtifact(t, configPath, "flag-order.dump", false)

	tests := []struct {
		name string
		args []string
	}{
		{"the flag before both operands", []string{"quarantine", "--config", configPath, "revalidate", id.String()}},
		{"the flag between the two operands", []string{"quarantine", "revalidate", "--config", configPath, id.String()}},
		{"the flag after both operands", []string{"quarantine", "revalidate", id.String(), "--config", configPath}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != 0 {
				t.Errorf("run(%v) = %d, want 0", tc.args, got)
			}
		})
	}
}

func TestRun_QuarantineRevalidateAgainstAnIntactQuarantinedArtifact(t *testing.T) {
	configPath := writeQuarantineTestConfig(t)
	id := stageQuarantinedArtifact(t, configPath, "revalidate-ok.dump", false)

	args := []string{"quarantine", "--config", configPath, "revalidate", id.String()}
	if got := run(args); got != 0 {
		t.Errorf("run(%v) = %d, want 0", args, got)
	}

	// Revalidate never moves the artifact: a second revalidate must still
	// see it as QUARANTINED, not refuse it as no-longer-quarantined.
	if got := run(args); got != 0 {
		t.Errorf("second run(%v) = %d, want 0 (revalidate must write nothing)", args, got)
	}
}

func TestRun_QuarantineRevalidateAgainstACorruptQuarantinedArtifact(t *testing.T) {
	configPath := writeQuarantineTestConfig(t)
	id := stageQuarantinedArtifact(t, configPath, "revalidate-bad.dump", true)

	args := []string{"quarantine", "--config", configPath, "revalidate", id.String()}
	if got := run(args); got == 0 {
		t.Errorf("run(%v) on a corrupted local copy = 0, want non-zero (failed verdict)", args)
	}
}

func TestRun_QuarantineRetryReEntersThePipeline(t *testing.T) {
	configPath := writeQuarantineTestConfig(t)
	id := stageQuarantinedArtifact(t, configPath, "retry-me.dump", false)

	args := []string{"quarantine", "--config", configPath, "retry", id.String()}
	if got := run(args); got != 0 {
		t.Fatalf("run(%v) = %d, want 0", args, got)
	}

	// The artifact is DISCOVERED now, not QUARANTINED: a second retry on
	// the same id must be refused, proving the first one actually wrote a
	// real transition rather than reporting success without moving it.
	if got := run(args); got == 0 {
		t.Errorf("second run(%v) = 0, want non-zero (artifact is no longer QUARANTINED)", args)
	}
}

func TestRun_QuarantineReinstateRefusesACorruptCopy(t *testing.T) {
	configPath := writeQuarantineTestConfig(t)
	id := stageQuarantinedArtifact(t, configPath, "reinstate-bad.dump", true)

	args := []string{"quarantine", "--config", configPath, "reinstate", id.String()}
	if got := run(args); got == 0 {
		t.Errorf("run(%v) on a corrupted local copy = 0, want non-zero (failed verdict, not reinstated)", args)
	}
}

func TestRun_QuarantineReinstateTrustsAnIntactCopyAgain(t *testing.T) {
	configPath := writeQuarantineTestConfig(t)
	id := stageQuarantinedArtifact(t, configPath, "reinstate-ok.dump", false)

	args := []string{"quarantine", "--config", configPath, "reinstate", id.String(), "--note", "verified by hand"}
	if got := run(args); got != 0 {
		t.Fatalf("run(%v) = %d, want 0", args, got)
	}

	// Reinstated back to COMMITTED: retry must now refuse it (no longer
	// QUARANTINED), proving reinstate actually wrote the transition.
	retryArgs := []string{"quarantine", "--config", configPath, "retry", id.String()}
	if got := run(retryArgs); got == 0 {
		t.Errorf("run(%v) after reinstatement = 0, want non-zero (artifact is COMMITTED, not QUARANTINED)", retryArgs)
	}
}
