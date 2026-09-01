package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRun_ArtifactsPrintsTheRecordedReasonForAQuarantinedArtifact is issue
// #284's end-to-end proof at the CLI boundary an operator actually types
// at: `artifacts` with no operand cannot say why an artifact failed
// (contract_test.go and main_test.go's own artifacts cases pin its
// four-field list format and never assert a reason, because today there
// is nowhere in it to put one), and until this test's fix landed, the
// only way to read state_transitions.detail at all was sqlite3 against
// the state database by hand.
//
// The failure driven here is real, not mocked: `run` transfers and
// verifies a real local-backend artifact to a durable state, the local
// final file is then corrupted on disk exactly as a bit-rot or
// operator-error scenario would, and `validate` (already-existing,
// already-tested machinery) finds the mismatch and quarantines it,
// recording the real diagnostic sentence into the journal's transition
// log. `artifacts <id>` is then asked about that exact artifact.
func TestRun_ArtifactsPrintsTheRecordedReasonForAQuarantinedArtifact(t *testing.T) {
	configPath := writeTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run([\"run\", \"--config\", %q]) = %d, want 0 (this test needs one committed artifact)", configPath, got)
	}
	const id = "production/postgres-primary/backup.dump"

	localFinal := filepath.Join(filepath.Dir(configPath), "local", "backup.dump")
	if err := os.WriteFile(localFinal, []byte("tampered bytes that do not match the recorded hash"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if got := run([]string{"validate", "--config", configPath, id}); got == 0 {
		t.Fatalf("validate against a corrupted local copy = 0, want non-zero (test setup did not actually corrupt anything)")
	}

	stdout := captureStdout(t, func() {
		if got := run([]string{"artifacts", "--config", configPath, id}); got != 0 {
			t.Errorf("run([\"artifacts\", --config, %q, %q]) = %d, want 0", configPath, id, got)
		}
	})

	if !strings.Contains(stdout, "QUARANTINED") {
		t.Errorf("artifacts %s output = %q, want it to report a quarantined state", id, stdout)
	}
	if !strings.Contains(stdout, "reason:") {
		t.Errorf("artifacts %s output = %q, want a \"reason:\" line: this is the whole point of #284", id, stdout)
	}
	if !strings.Contains(stdout, "now hashes to") || !strings.Contains(stdout, "hash recorded at verification was") {
		t.Errorf("artifacts %s output = %q, want it to contain the journal's own recorded sentence (operator-triggered validate's hash-mismatch detail), not a generic message", id, stdout)
	}
}

// TestRun_ArtifactsSingleArtifactRejectsFiltersAndExtraArguments proves
// the new operand form's own argument handling: it cannot be combined
// with the list form's --source/--backup-set filters (they have nothing
// to narrow once one artifact is already named), and it takes at most
// one operand, the same arity discipline `validate` already established
// (issue #188).
func TestRun_ArtifactsSingleArtifactRejectsFiltersAndExtraArguments(t *testing.T) {
	configPath := writeTestConfig(t)
	const id = "production/postgres-primary/backup.dump"

	tests := []struct {
		name string
		args []string
	}{
		{"combined with --source", []string{"artifacts", "--config", configPath, "--source", "production", id}},
		{"combined with --backup-set", []string{"artifacts", "--config", configPath, "--backup-set", "postgres-primary", id}},
		{"two operands", []string{"artifacts", "--config", configPath, id, id}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != 2 {
				t.Errorf("run(%v) = %d, want 2", tc.args, got)
			}
		})
	}
}

// TestRun_ArtifactsSingleArtifactRefusesAnUnknownID proves a malformed or
// unrecorded <source/backup-set/name> operand is reported as a failure,
// not a panic or an empty success, mirroring how `validate` already
// handles the identical id shape (issue #188's
// TestRun_ValidateRejectsMalformedArtifactID).
func TestRun_ArtifactsSingleArtifactRefusesAnUnknownID(t *testing.T) {
	configPath := writeTestConfig(t)
	if got := run([]string{"artifacts", "--config", configPath, "not-a-valid-id"}); got == 0 {
		t.Error("run([\"artifacts\", \"not-a-valid-id\"]) = 0, want a non-zero exit code")
	}
	if got := run([]string{"artifacts", "--config", configPath, "production/postgres-primary/never-discovered.dump"}); got == 0 {
		t.Error("run([\"artifacts\", \"production/postgres-primary/never-discovered.dump\"]) = 0, want a non-zero exit code")
	}
}
