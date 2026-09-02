package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRun_BackupSetPatchChangesOneFieldAndPersists is the CLI half of
// issue #350's "the CLI can perform the same update, so the two surfaces
// do not diverge". Editing config.yaml by hand was the only way to change
// a set, which is what an operator driving this over SSH had to do; this
// proves the verb is a real, persisted, hot-reloaded write rather than an
// echoed request, by reading it back through a SECOND, independent `run`
// call that loads the file fresh.
func TestRun_BackupSetPatchChangesOneFieldAndPersists(t *testing.T) {
	configPath := writeTestConfig(t)
	newLocal := filepath.Join(t.TempDir(), "moved-local")

	out := captureStdout(t, func() {
		args := []string{"backup-set", "--config", configPath, "patch", "production/postgres-primary",
			"--local-path", newLocal}
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, got)
		}
	})
	if !strings.Contains(out, newLocal) {
		t.Errorf("patch output = %q, want it to report the new local_path %q", out, newLocal)
	}

	sourcesOut := captureStdout(t, func() {
		if got := run([]string{"sources", "--config", configPath}); got != 0 {
			t.Fatalf(`run(["sources"]) != 0`)
		}
	})
	if !strings.Contains(sourcesOut, "postgres-primary") {
		t.Errorf("sources output = %q, want it to still list the set", sourcesOut)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), newLocal) {
		t.Errorf("the config file does not carry the new local_path:\n%s", raw)
	}
}

// TestRun_BackupSetPatchLeavesUnnamedFieldsAlone is the same per-box
// isolation the HTTP route and the service both hold, checked at the
// surface an operator drives from a script: a flag nobody passed must not
// send a zero value over the wire, or every CLI patch would quietly
// rewrite every field to its flag default.
func TestRun_BackupSetPatchLeavesUnnamedFieldsAlone(t *testing.T) {
	configPath := writeTestConfig(t)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	remoteBefore := lineWithPrefix(t, string(before), "        remote_path: ")

	newLocal := filepath.Join(t.TempDir(), "moved-local")
	captureStdout(t, func() {
		args := []string{"backup-set", "--config", configPath, "patch", "production/postgres-primary",
			"--local-path", newLocal}
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, got)
		}
	})

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(after), strings.TrimSpace(remoteBefore)) {
		t.Errorf("remote_path changed after a patch that only named --local-path.\nwas: %q\nnow:\n%s", remoteBefore, after)
	}
	if !strings.Contains(string(after), "strategy: rename") {
		t.Errorf("completion.strategy changed after a patch that never named it:\n%s", after)
	}
	if !strings.Contains(string(after), "*.dump") {
		t.Errorf("include changed after a patch that never named it:\n%s", after)
	}
}

func lineWithPrefix(t *testing.T, content, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no line starting %q in:\n%s", prefix, content)
	return ""
}

// TestRun_BackupSetPatchWithNoFlagsIsAUsageError: a patch that names no
// flag would rewrite and hot-reload a whole configuration to achieve
// nothing, and reporting success for it is how an operator concludes a
// change landed. This mirrors buildSettingsPatch's own refusal.
func TestRun_BackupSetPatchWithNoFlagsIsAUsageError(t *testing.T) {
	configPath := writeTestConfig(t)
	args := []string{"backup-set", "--config", configPath, "patch", "production/postgres-primary"}
	if got := run(args); got != 2 {
		t.Errorf("run(%v) = %d, want 2", args, got)
	}
}

// TestRun_BackupSetRejectsMalformedInvocations covers the shapes an
// operator actually mistypes: a missing verb, an unknown verb, a missing
// id, and an id that is not source/name.
func TestRun_BackupSetRejectsMalformedInvocations(t *testing.T) {
	configPath := writeTestConfig(t)
	newLocal := filepath.Join(t.TempDir(), "x")
	for _, args := range [][]string{
		{"backup-set", "--config", configPath},
		{"backup-set", "--config", configPath, "frobnicate", "production/postgres-primary"},
		{"backup-set", "--config", configPath, "patch"},
		{"backup-set", "--config", configPath, "patch", "production/postgres-primary", "extra"},
	} {
		if got := run(append(args, "--local-path", newLocal)); got != 2 {
			t.Errorf("run(%v) = %d, want 2 (a usage error)", args, got)
		}
	}
}

// TestRun_BackupSetPatchUnknownSetFails: naming a set that does not exist
// is a failure with a non-zero exit code, never a silent success, because
// a script driving this over SSH has nothing but the exit code to go on.
func TestRun_BackupSetPatchUnknownSetFails(t *testing.T) {
	configPath := writeTestConfig(t)
	args := []string{"backup-set", "--config", configPath, "patch", "production/nope",
		"--local-path", filepath.Join(t.TempDir(), "x")}
	if got := run(args); got == 0 {
		t.Errorf("run(%v) = 0, want a non-zero exit for a set that does not exist", args)
	}
}

// TestRun_BackupSetPatchRefusesAnInvalidValue proves the CLI goes through
// the SAME validation the API does rather than writing whatever it is
// handed: a relative remote_path is what config.Validate refuses, and the
// file must be untouched afterwards.
func TestRun_BackupSetPatchRefusesAnInvalidValue(t *testing.T) {
	configPath := writeTestConfig(t)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	args := []string{"backup-set", "--config", configPath, "patch", "production/postgres-primary",
		"--remote-path", "relative/not/absolute"}
	if got := run(args); got == 0 {
		t.Errorf("run(%v) = 0, want a non-zero exit for a relative remote_path", args)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("a refused patch rewrote the config file:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestRun_BackupSetPatchEmptyStringIsARealValue: --user "" has to reach
// the service as an explicit empty (and be refused there, exactly as
// creation refuses an empty user), rather than being read as "this flag
// was never passed". That is the same fs.Visit discipline
// buildSettingsPatch already documents, and it is what stops a typo
// silently doing nothing.
func TestRun_BackupSetPatchEmptyStringIsARealValue(t *testing.T) {
	configPath := writeTestConfig(t)
	args := []string{"backup-set", "--config", configPath, "patch", "production/postgres-primary",
		"--user", ""}
	if got := run(args); got == 0 {
		t.Errorf("run(%v) = 0, want a non-zero exit: an explicitly empty --user is refused, not ignored", args)
	}
}

// TestRun_UsageListsTheBackupSetCommand keeps the usage banner honest:
// it is where an operator learns which commands exist, and the tests
// repository pins its lines for exactly that reason.
func TestRun_UsageListsTheBackupSetCommand(t *testing.T) {
	out := captureStderr(t, func() {
		if got := run(nil); got != 2 {
			t.Errorf("run(nil) = %d, want 2", got)
		}
	})
	if !strings.Contains(out, "backup-set patch") {
		t.Errorf("usage banner = %q, want it to list the backup-set command", out)
	}
}
