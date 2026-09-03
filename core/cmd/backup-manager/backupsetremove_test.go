package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// setNamesIn reads configPath the way a fresh boot would and lists the
// backup set ids it declares.
func setNamesIn(t *testing.T, configPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, raw)
	}
	var out []string
	for _, src := range cfg.Sources {
		for _, bs := range src.BackupSets {
			out = append(out, src.Name+"/"+bs.Name)
		}
	}
	return out
}

// TestRun_BackupSetRemove_TakesTheSetOutOfTheFileAndSaysWhatStayed is the
// CLI's half of issue #391.
//
// The verb exists so the operator standing at a real NAS with no browser
// can do what the Web UI does, which is the same reason `create` and
// `patch` are here. What it prints matters as much as what it writes: a
// removal that only said "removed" would leave the operator with no way
// to see the half of the promise that is not visible in the file
// afterwards, which is that the backups are still there.
func TestRun_BackupSetRemove_TakesTheSetOutOfTheFileAndSaysWhatStayed(t *testing.T) {
	configPath := writeTestConfig(t)
	if got := setNamesIn(t, configPath); len(got) != 1 {
		t.Fatalf("the fixture declares %v, want exactly one set; without one this test cannot tell removal from a no-op", got)
	}

	var out string
	code := 1
	out = captureStdout(t, func() {
		code = run([]string{"backup-set", "--config", configPath, "remove", "production/postgres-primary"})
	})
	if code != 0 {
		t.Fatalf("run(backup-set remove) = %d, want 0. Output:\n%s", code, out)
	}

	if got := setNamesIn(t, configPath); len(got) != 0 {
		t.Errorf("the configuration still declares %v after the removal", got)
	}
	if !strings.Contains(out, "production/postgres-primary") {
		t.Errorf("the output does not name what it removed:\n%s", out)
	}
	if !strings.Contains(out, "stay on storage") {
		t.Errorf("the output does not say the backups stay on storage, which is the half of this an operator cannot see in the file:\n%s", out)
	}
}

// TestRun_BackupSetRemove_UnknownSetFailsAndWritesNothing. The refusal
// matters more than the exit code on its own: a removal that answered
// "fine" to a mistyped name, and left the real set running, is the exact
// shape of the defect this issue is about.
func TestRun_BackupSetRemove_UnknownSetFailsAndWritesNothing(t *testing.T) {
	configPath := writeTestConfig(t)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	out := captureStderr(t, func() {
		if code := run([]string{"backup-set", "--config", configPath, "remove", "production/typo"}); code == 0 {
			t.Error("run(backup-set remove) on a set that does not exist = 0, want a failure")
		}
	})
	if !strings.Contains(out, "production/typo") {
		t.Errorf("the refusal does not name what it could not find:\n%s", out)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("a refused removal rewrote the configuration:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestRun_BackupSetRemove_RefusesEveryOtherVerbsFlags. remove names a set
// and removes it, so every flag this command declares belongs to another
// verb. Accepting one silently is the failure
// TestRun_BackupSetVerbsRefuseEachOthersFlags already describes, and it
// is worse here: `backup-set remove a/b --read-only` exiting 0 having
// removed the set is a command that did something other than what it was
// asked.
func TestRun_BackupSetRemove_RefusesEveryOtherVerbsFlags(t *testing.T) {
	for _, wrong := range [][]string{
		{"--read-only"},
		{"--disabled"},
		{"--run"},
		{"--acknowledge-repoint"},
		{"--host", "elsewhere.internal"},
		{"--local-path", "/tmp/elsewhere"},
		{"--include", "*.sql"},
	} {
		configPath := writeTestConfig(t)
		args := append([]string{"backup-set", "--config", configPath, "remove", "production/postgres-primary"}, wrong...)

		out := captureStderr(t, func() {
			if code := run(args); code != 2 {
				t.Errorf("run(%v) = %d, want 2 (a usage error)", args, code)
			}
		})
		if !strings.Contains(out, strings.TrimPrefix(wrong[0], "--")) {
			t.Errorf("the refusal for %s does not name the flag it refused: %q", wrong[0], out)
		}
		if got := setNamesIn(t, configPath); len(got) != 1 {
			t.Errorf("the refused removal still changed the configuration, which now declares %v", got)
		}
	}

	// The control. Without it every case above would also pass against a
	// remove verb that refused every invocation it was ever given.
	configPath := writeTestConfig(t)
	if code := run([]string{"backup-set", "--config", configPath, "remove", "production/postgres-primary"}); code != 0 {
		t.Fatalf("the same removal with no extra flag = %d, want 0; the refusals above prove nothing without this", code)
	}
}

// TestRun_BackupSetRemove_ArtifactsStillListsWhatStayed pins the sentence
// the verb prints: the backups "stay listed by `backup-manager
// artifacts`". The unfiltered list has to ASK for a removed set's rows
// (internal/app widens only when asked, because the quarantine read is
// the same call and must not), so a terminal that forgot to ask would
// print that sentence and contradict it on the very next command.
func TestRun_BackupSetRemove_ArtifactsStillListsWhatStayed(t *testing.T) {
	configPath := writeTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run(run) = %d, want 0; without a cycle there is nothing on record to survive the removal", got)
	}
	before := captureStdout(t, func() { _ = run([]string{"artifacts", "--config", configPath}) })
	if !strings.Contains(before, "production/postgres-primary/backup.dump") {
		t.Fatalf("the cycle left no artifact on record, so nothing below would be evidence:\n%s", before)
	}

	_ = captureStdout(t, func() {
		if code := run([]string{"backup-set", "--config", configPath, "remove", "production/postgres-primary"}); code != 0 {
			t.Fatalf("run(backup-set remove) = %d, want 0", code)
		}
	})

	after := captureStdout(t, func() {
		if code := run([]string{"artifacts", "--config", configPath}); code != 0 {
			t.Errorf("run(artifacts) after the removal = %d, want 0", code)
		}
	})
	if !strings.Contains(after, "production/postgres-primary/backup.dump") {
		t.Errorf("`artifacts` no longer lists the removed set's backup, which the removal just promised it would:\n%s", after)
	}
}
