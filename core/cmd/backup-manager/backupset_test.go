package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The `backup-set patch` verb: that it writes, that it writes only what it
// was asked to, and that it refuses the edits that need an operator to say
// something out loud first.
//
// Every persistence claim is read back through a SECOND run() call rather
// than from the value the first one returned. That is what separates a real
// write from an echoed request: the second call loads the file fresh, so the
// assertion is about what is on disk and hot-reloadable, not about what the
// command said it did.
//
// The refusals are the harder half. An empty string is a real value here and
// not a missing flag, an unnamed field has to come back unchanged, and
// repointing a set that already has artifacts on record is a decision an
// operator has to acknowledge rather than something the command guesses at.

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

// TestRun_BackupSetPatchRefusesToRepointASetWithHistoryUntilAcknowledged
// is the CLI half of backupsetrepoint.go. An operator driving this over
// SSH gets exactly the same refusal, and the same one flag out of it, as
// one clicking Save in the browser: a surface that could repoint a set
// silently while the other one asked would be the divergence
// suites/equivalence exists to catch.
//
// The `run` beforehand is what gives the set history. Without it there is
// nothing to orphan, which is the case the control below covers.
func TestRun_BackupSetPatchRefusesToRepointASetWithHistoryUntilAcknowledged(t *testing.T) {
	configPath := writeTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf(`run(["run"]) = %d, want 0: the fixture has to actually back something up or this test proves nothing`, got)
	}

	newLocal := filepath.Join(t.TempDir(), "moved-local")
	refuseArgs := []string{"backup-set", "--config", configPath, "patch", "production/postgres-primary",
		"--local-path", newLocal}
	var code int
	stderr := captureStderr(t, func() { code = run(refuseArgs) })
	if code == 0 {
		t.Fatalf("run(%v) = 0, want non-zero: repointing a set that already holds artifacts must be refused until acknowledged", refuseArgs)
	}
	for _, want := range []string{"local_path", "acknowledge_repoint"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, stderr)
		}
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), newLocal) {
		t.Error("the config file carries the new local_path even though the patch was refused")
	}

	if got := run(append(refuseArgs, "--acknowledge-repoint")); got != 0 {
		t.Fatalf("run with --acknowledge-repoint = %d, want 0", got)
	}
	raw, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), newLocal) {
		t.Errorf("the acknowledged patch did not persist the new local_path:\n%s", raw)
	}
}

// TestRun_BackupSetCreateRefusesToCreateOverHistoryUntilAcknowledged is
// the CLI half of issue #411, and the same equivalence argument as the
// patch case above: an operator at a terminal is asked exactly what an
// operator in the browser is asked, and gets out of it with exactly one
// flag.
//
// The `run` gives production/postgres-primary history and the `remove`
// frees its id up, which is the whole route this refusal is about.
func TestRun_BackupSetCreateRefusesToCreateOverHistoryUntilAcknowledged(t *testing.T) {
	configPath := writeTestConfig(t)
	keyPath := writeTestPrivateKey(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf(`run(["run"]) = %d, want 0: the fixture has to actually back something up or this test proves nothing`, got)
	}
	if got := run([]string{"backup-set", "--config", configPath, "remove", "production/postgres-primary"}); got != 0 {
		t.Fatalf("removing the fixture set = %d, want 0", got)
	}

	args := createArgs(configPath, keyPath, "production/postgres-primary")
	var code int
	stderr := captureStderr(t, func() { code = run(args) })
	if code == 0 {
		t.Fatalf("run(%v) = 0, want non-zero: creating a set over an id that already holds artifacts, somewhere else, must be refused until acknowledged", args)
	}
	for _, want := range []string{"local_path", "acknowledge_repoint"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, stderr)
		}
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "/data/backups/api") {
		t.Error("the config file carries the refused set's local_path even though the create was refused")
	}

	if got := run(createArgs(configPath, keyPath, "production/postgres-primary", "--acknowledge-repoint")); got != 0 {
		t.Fatalf("run with --acknowledge-repoint = %d, want 0", got)
	}
	raw, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "/data/backups/api") {
		t.Errorf("the acknowledged create did not persist the set:\n%s", raw)
	}
}

// TestRun_BackupSetPatchAcknowledgeAloneIsStillAUsageError: the
// acknowledgement names no field to change, so a patch carrying only it
// rewrites and reloads the configuration to no effect, which is exactly
// what the no-flags refusal exists to stop.
func TestRun_BackupSetPatchAcknowledgeAloneIsStillAUsageError(t *testing.T) {
	configPath := writeTestConfig(t)
	args := []string{"backup-set", "--config", configPath, "patch", "production/postgres-primary", "--acknowledge-repoint"}
	if got := run(args); got != 2 {
		t.Errorf("run(%v) = %d, want 2", args, got)
	}
}

// TestRun_BackupSetPatchReportsTheFieldsItCanChange: printBackupSet
// claims to report every field this command can change, and a command
// that can write stale_after while printing a set without it leaves an
// operator unable to confirm what it did.
func TestRun_BackupSetPatchReportsTheFieldsItCanChange(t *testing.T) {
	configPath := writeTestConfig(t)
	out := captureStdout(t, func() {
		args := []string{"backup-set", "--config", configPath, "patch", "production/postgres-primary", "--stale-after", "36h"}
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, got)
		}
	})
	if !strings.Contains(out, "stale_after: 36h") {
		t.Errorf("patch output does not report the stale_after it just wrote:\n%s", out)
	}
}
