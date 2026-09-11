package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #568: `retention <source/backup-set>` and the two ways it was
// wrong on a live deployment.
//
// It previewed the first configured set whatever id it was handed, and it
// took an id that named nothing at all without a word. The first is a
// wrong answer an operator can catch, because the heading names a set
// they did not ask about; the second is one they cannot, because there is
// nothing in the output to say the id was never resolved. This is the
// command an operator reads before a deletion runs, so both are pinned
// here, in both directions: the id that names the SECOND set has to
// preview the second set, the id that names the first has to still
// preview the first, and the id that names neither has to be refused.
//
// Every case drives the real command over a real local-backend fetch, the
// same way TestRun_RetentionLineSaysWhichPlacementSelectedEachTier does,
// and reads what it actually printed. Two sets with differently named
// payloads is what makes a wrong answer visible: a preview that reached
// for the wrong set prints the wrong file name as well as the wrong
// heading.

// writeOperandTestConfig is writeTestConfig with a SECOND backup set
// beside the first, each with its own one-file "remote" and its own
// payload NAME.
//
// The distinct names are why this is not writeTwoSetTestConfig
// (retention_override_endtoend_test.go), which already builds two sets:
// both of that fixture's artifacts are called backup.dump, so a preview
// that reached for the wrong set would print a line indistinguishable
// from the right one, and only the heading would give it away. Here the
// itemisation gives it away too.
func writeOperandTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	for set, payload := range map[string]string{"primary": "primary.dump", "replica": "replica.dump"} {
		remoteDir := filepath.Join(dir, "remote-"+set)
		if err := os.MkdirAll(remoteDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(remoteDir, payload), []byte("cli operand payload for "+set), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
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
		"        remote_path: " + filepath.Join(dir, "remote-primary") + "\n" +
		"        local_path: " + filepath.Join(dir, "local-primary") + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"      - id: postgres-replica\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + filepath.Join(dir, "remote-replica") + "\n" +
		"        local_path: " + filepath.Join(dir, "local-replica") + "\n" +
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

// TestRun_RetentionPreviewsTheBackupSetItWasGiven is the defect itself,
// asked from both ends.
//
// The second set is what the live report was about: `retention
// cicd-pipeline/var-backups` answered about api-server/var-backups. The
// first set is the control that keeps this honest, because a fix that
// refused or emptied everything would satisfy the second-set case on its
// own and would be a worse command than the broken one.
func TestRun_RetentionPreviewsTheBackupSetItWasGiven(t *testing.T) {
	configPath := writeOperandTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run: %d, want 0", got)
	}

	for _, tc := range []struct {
		id      string
		names   string
		absent  string
		payload string
		other   string
	}{
		{id: "production/postgres-replica", names: "production/postgres-replica:", absent: "production/postgres-primary", payload: "replica.dump", other: "primary.dump"},
		{id: "production/postgres-primary", names: "production/postgres-primary:", absent: "production/postgres-replica", payload: "primary.dump", other: "replica.dump"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			out := captureStdout(t, func() {
				if got := run([]string{"retention", "--config", configPath, tc.id, "--dry-run"}); got != 0 {
					t.Fatalf("retention %s --dry-run: %d, want 0", tc.id, got)
				}
			})

			if !strings.Contains(out, tc.names) {
				t.Errorf("the preview does not name %s at all, so it is not about the backup set it was asked about.\ngot:\n%s", tc.id, out)
			}
			if strings.Contains(out, tc.absent) {
				t.Errorf("the preview names %s, which nobody asked about; an operator reading this before a deletion is being shown another set's verdicts.\ngot:\n%s", tc.absent, out)
			}
			if !strings.Contains(out, tc.payload) {
				t.Errorf("the preview does not itemise %s, this backup set's own artifact.\ngot:\n%s", tc.payload, out)
			}
			if strings.Contains(out, tc.other) {
				t.Errorf("the preview itemises %s, which belongs to the other backup set.\ngot:\n%s", tc.other, out)
			}
		})
	}
}

// TestRun_RetentionRefusesAnIdThatNamesNoConfiguredSet is the worse half
// of #568, because an operator cannot catch it by reading the output.
//
// A refusal, and nothing printed: a preview that came out beside a
// refusal would be exactly the confidently wrong answer this is about.
// Exit 1 rather than 2, because the usage block's own table gives "a set
// or an artifact that is not there" to 1, and `backup-set retention`
// already refuses an unknown set that way.
func TestRun_RetentionRefusesAnIdThatNamesNoConfiguredSet(t *testing.T) {
	configPath := writeOperandTestConfig(t)

	var code int
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			code = run([]string{"retention", "--config", configPath, "does-not/exist", "--dry-run"})
		})
	})

	if code != 1 {
		t.Errorf("retention does-not/exist --dry-run exited %d, want 1: an id that names no configured backup set is a set that is not there, which the usage block's exit-code table gives to 1", code)
	}
	if strings.Contains(stdout, "production/postgres-") {
		t.Errorf("an id that names nothing still produced a preview of a configured set, which is a wrong answer with nothing in it to say the id was never resolved.\ngot:\n%s", stdout)
	}
	for _, want := range []string{"does-not/exist"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not name %q, so an operator cannot see which id was refused.\ngot:\n%s", want, stderr)
		}
	}
}

// TestRun_RetentionRefusesAnArgumentThatIsNotABackupSetId keeps the two
// refusals apart. A command line that never carried an id is a usage
// mistake (2); a well-formed id that names nothing is a set that is not
// there (1), which the case above pins.
func TestRun_RetentionRefusesAnArgumentThatIsNotABackupSetId(t *testing.T) {
	configPath := writeOperandTestConfig(t)

	for _, args := range [][]string{
		{"retention", "--config", configPath, "postgres-primary", "--dry-run"},
		{"retention", "--config", configPath, "production/postgres-primary", "production/postgres-replica"},
	} {
		var code int
		out := captureStdout(t, func() {
			captureStderr(t, func() { code = run(args) })
		})
		if code != 2 {
			t.Errorf("run(%v) = %d, want 2: nothing here is a backup set id this command can act on", args, code)
		}
		if strings.Contains(out, "production/postgres-") {
			t.Errorf("run(%v) previewed a backup set anyway.\ngot:\n%s", args, out)
		}
	}
}

// TestRun_RetentionWithNoOperandStillPreviewsEveryConfiguredSet is the
// compatibility control, and it is the one that protects something
// outside this repository: the no-operand form is what the black-box
// contract suite in backupdproject/backupd-tests pins
// (suites/cli/cases/retention/), and core/tests/compat's own retention
// cell drives it too. Reading an operand must not narrow the form that
// carries none.
func TestRun_RetentionWithNoOperandStillPreviewsEveryConfiguredSet(t *testing.T) {
	configPath := writeOperandTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run: %d, want 0", got)
	}

	out := captureStdout(t, func() {
		if got := run([]string{"retention", "--config", configPath, "--dry-run"}); got != 0 {
			t.Fatalf("retention --dry-run: %d, want 0", got)
		}
	})

	for _, want := range []string{"production/postgres-primary:", "production/postgres-replica:", "primary.dump", "replica.dump"} {
		if !strings.Contains(out, want) {
			t.Errorf("`retention --dry-run` with no operand does not mention %q; with no id it is still about the whole deployment.\ngot:\n%s", want, out)
		}
	}
}
