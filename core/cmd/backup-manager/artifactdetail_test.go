// The single-artifact detail view: that it prints the recorded reason a
// backup is in trouble, and that it does not print two explanations for it.
//
// The recorded reason is the whole point of the view. It lives in the
// journal's append-only transition log and nowhere else, so before this
// existed an operator asking why an artifact was quarantined had to open
// SQLite by hand. The cells here drive it end to end through a real
// validator that refuses, so the sentence being printed is one the lifecycle
// actually wrote rather than one the test handed in.
//
// The second cell is about a field this view deliberately does not print.
// The API's own guess at a quarantine reason is reconstructed rather than
// read, and it is sometimes non-empty and wrong; showing it beside the real
// sentence would leave an operator with two disagreeing explanations and no
// way to tell which to trust.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRejectingValidatorScript writes an executable shell script to a
// fresh temp directory that always rejects (exit 1) and writes message to
// its combined stdout/stderr, exactly the shape internal/lifecycle/
// verify.go's runValidator captures into rec.ValidationDetail on a clean
// non-zero exit ("a verdict, not an infrastructure failure"). It returns
// the script's absolute path, since config.Validation.Command.Executable
// must be absolute (core/internal/config/validate.go).
func writeRejectingValidatorScript(t *testing.T, message string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reject.sh")
	script := "#!/bin/sh\necho " + shellQuote(message) + "\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// shellQuote wraps s in single quotes for embedding literally in a
// generated sh script, escaping any single quote s itself contains. Good
// enough for the fixed, punctuation-light messages this file's tests pass;
// not a general-purpose shell-quoting routine.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeTestConfigWithValidatorBlock is writeTestConfig plus an application
// validator (config.Validation.Command) wired onto the one backup set, so
// a test can drive a real application-validator rejection (as opposed to
// the hash-mismatch quarantine writeTestConfig's callers normally drive)
// through the actual `run` pipeline.
func writeTestConfigWithValidatorBlock(t *testing.T, executable string) string {
	t.Helper()
	configPath := writeTestConfig(t)
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading generated config: %v", err)
	}
	const marker = "        stale_after: 24h\n"
	validationYAML := "        validation:\n" +
		"          command:\n" +
		"            executable: " + executable + "\n" +
		"            timeout: 5s\n"
	content := strings.Replace(string(data), marker, marker+validationYAML, 1)
	if content == string(data) {
		t.Fatalf("writeTestConfigWithValidatorBlock: marker %q not found in generated config %q", marker, data)
	}
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("rewriting config with a validation block: %v", err)
	}
	return configPath
}

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

// TestRun_ArtifactsDetailDoesNotDisagreeWithItselfAboutWhyAnArtifactIsQuarantined
// is the adversarial review's finding on PR #312 (issues #284/#308):
// printArtifactDetail printed two different explanations for the same
// quarantine back to back -- quarantine_reason (quarantineReasonFor:
// rec.LastError falling back to rec.ValidationDetail, the derivation this
// file's own doc comment already calls "often empty" and "not the
// recommended way to learn why") sitting directly above reason/reason_at
// (the journal's own literal transition-log sentence, via
// state.LastEnteredDetail and app.ArtifactDetail.FailureReason).
//
// This drives a real application-validator rejection, the one shape of
// quarantine where the old derivation is not merely empty but actively
// disagrees: the validator process's own stdout becomes
// rec.ValidationDetail (and, with LastError still unset -- it is only
// ever written at release time -- quarantineReasonFor returns exactly
// that stdout text as quarantine_reason), while the journal records the
// fixed, generic sentence verify.go writes for every application-validator
// rejection ("application validator rejected the artifact") as this
// artifact's reason. Before the fix, an operator saw both, disagreeing,
// with nothing telling them which to trust. The fix drops
// quarantine_reason from this command's output entirely, so
// reason:/reason_at: are the only explanation left.
func TestRun_ArtifactsDetailDoesNotDisagreeWithItselfAboutWhyAnArtifactIsQuarantined(t *testing.T) {
	const validatorMessage = "custom validator: payload failed the internal schema check"
	scriptPath := writeRejectingValidatorScript(t, validatorMessage)
	configPath := writeTestConfigWithValidatorBlock(t, scriptPath)
	const id = "production/postgres-primary/backup.dump"

	run([]string{"run", "--config", configPath}) // exit code intentionally unchecked: a per-artifact quarantine need not fail the whole cycle; state is asserted directly below.

	stdout := captureStdout(t, func() {
		if got := run([]string{"artifacts", "--config", configPath, id}); got != 0 {
			t.Errorf("run([\"artifacts\", --config, %q, %q]) = %d, want 0", configPath, id, got)
		}
	})

	if !strings.Contains(stdout, "QUARANTINED") {
		t.Fatalf("artifacts %s output = %q, want it to report a quarantined state (test setup did not actually quarantine anything)", id, stdout)
	}
	if !strings.Contains(stdout, "validation_detail:   "+validatorMessage) {
		t.Fatalf("artifacts %s output = %q, want a validation_detail: line carrying the validator's own message: without it, rec.ValidationDetail is empty and this test cannot prove quarantine_reason would have disagreed", id, stdout)
	}
	if !strings.Contains(stdout, "reason:              application validator rejected the artifact\n") {
		t.Errorf("artifacts %s output = %q, want the journal-sourced reason: line with verify.go's generic rejection sentence", id, stdout)
	}
	if strings.Contains(stdout, "quarantine_reason:") {
		t.Errorf("artifacts %s output = %q, want no quarantine_reason: line at all: it is redundant with reason:/reason_at: and, per this command's own doc comment, an unreliable derivation dropped by this fix", id, stdout)
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
