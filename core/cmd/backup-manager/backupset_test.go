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

// TestRun_BackupSetRetentionShowSetClear is issue #333's CLI half, end to
// end through the real binary against a real config file: show reports
// inheritance, set gives the one set its own chain and persists it, show
// then says so, and clear puts it back with no residue.
//
// Driven as one sequence rather than four cases on purpose. What the
// issue actually asks for is that the three verbs compose into a round
// trip an operator can make and unmake, and four independent cases would
// each prove a step while leaving that unchecked.
func TestRun_BackupSetRetentionShowSetClear(t *testing.T) {
	configPath := writeTestConfig(t)

	inherited := captureStdout(t, func() {
		if got := run([]string{"backup-set", "--config", configPath, "retention", "show", "production/postgres-primary"}); got != 0 {
			t.Fatal("retention show on an inheriting set != 0")
		}
	})
	if !strings.Contains(inherited, "inherited") {
		t.Errorf("show does not say the policy is inherited:\n%s", inherited)
	}

	set := captureStdout(t, func() {
		args := []string{"backup-set", "--config", configPath, "retention", "set", "production/postgres-primary",
			"--tier", "daily:day:30", "--tier", "monthly:month:24"}
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) != 0", args)
		}
	})
	if !strings.Contains(set, "daily/30") || !strings.Contains(set, "monthly/24") {
		t.Errorf("set does not report the chain it just wrote:\n%s", set)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "name: daily") {
		t.Errorf("the config file does not carry the set's own chain:\n%s", raw)
	}

	overridden := captureStdout(t, func() {
		if got := run([]string{"backup-set", "--config", configPath, "retention", "show", "production/postgres-primary"}); got != 0 {
			t.Fatal("retention show on an overridden set != 0")
		}
	})
	if !strings.Contains(overridden, "this set's own retention policy") {
		t.Errorf("show does not name the set's own policy:\n%s", overridden)
	}
	// And what clearing would go back to, so deciding whether to clear
	// does not need a second command.
	if !strings.Contains(overridden, "deployment policy:") {
		t.Errorf("show does not report what clearing would return to:\n%s", overridden)
	}

	cleared := captureStdout(t, func() {
		if got := run([]string{"backup-set", "--config", configPath, "retention", "clear", "production/postgres-primary"}); got != 0 {
			t.Fatal("retention clear != 0")
		}
	})
	if !strings.Contains(cleared, "inherited") {
		t.Errorf("clear does not report the set back on the deployment's policy:\n%s", cleared)
	}
	raw, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "name: daily") {
		t.Errorf("the cleared chain is still in the config file:\n%s", raw)
	}
}

// TestRun_BackupSetRetentionSetRefusesAHalfChain: the CLI cannot express
// half a chain at all (it writes tiers, never the three legacy scalars),
// and naming no tier is a usage error pointing at `clear`, which is what
// an operator who wanted the deployment's policy actually meant.
func TestRun_BackupSetRetentionSetRefusesAHalfChain(t *testing.T) {
	configPath := writeTestConfig(t)
	args := []string{"backup-set", "--config", configPath, "retention", "set", "production/postgres-primary"}
	var code int
	stderr := captureStderr(t, func() { code = run(args) })
	if code != 2 {
		t.Errorf("run(%v) = %d, want 2", args, code)
	}
	if !strings.Contains(stderr, "clear") {
		t.Errorf("the refusal does not point at `retention clear`:\n%s", stderr)
	}
}

// TestRun_BackupSetRefusesAFlagTheVerbDoesNotOwn. A flag silently parsed
// and then ignored by a command that mutates a live configuration is the
// trap #323 fixed on `settings`: the operator reads exit 0 as "that
// landed". Both directions are checked, because a one-way check would
// pass against a command that accepted everything everywhere.
func TestRun_BackupSetRefusesAFlagTheVerbDoesNotOwn(t *testing.T) {
	configPath := writeTestConfig(t)
	for _, args := range [][]string{
		{"backup-set", "--config", configPath, "patch", "production/postgres-primary", "--tier", "daily:day:7"},
		{"backup-set", "--config", configPath, "retention", "set", "production/postgres-primary", "--tier", "daily:day:7", "--host", "elsewhere.internal"},
		{"backup-set", "--config", configPath, "retention", "clear", "production/postgres-primary", "--tier", "daily:day:7"},
		{"backup-set", "--config", configPath, "retention", "show", "production/postgres-primary", "--timezone", "UTC"},
	} {
		if got := run(args); got != 2 {
			t.Errorf("run(%v) = %d, want 2 (a usage error)", args, got)
		}
	}
}

// TestRun_BackupSetRetentionMalformedInvocations covers the verb shapes
// an operator mistypes. `retention` alone is the one that matters: read
// as a verb on its own it would have to guess which of show, set and
// clear was meant, and two of those three write.
func TestRun_BackupSetRetentionMalformedInvocations(t *testing.T) {
	configPath := writeTestConfig(t)
	for _, args := range [][]string{
		{"backup-set", "--config", configPath, "retention"},
		{"backup-set", "--config", configPath, "retention", "production/postgres-primary"},
		{"backup-set", "--config", configPath, "retention", "frobnicate", "production/postgres-primary"},
		{"backup-set", "--config", configPath, "retention", "show", "not-an-id"},
		{"backup-set", "--config", configPath, "retention", "show", "production/postgres-primary", "extra"},
	} {
		if got := run(args); got != 2 {
			t.Errorf("run(%v) = %d, want 2 (a usage error)", args, got)
		}
	}
}

// TestRun_BackupSetRetentionSetInheritsTheCalendarItDoesNotName, at the
// surface an operator actually types: a chain written without a timezone
// is reckoned in the deployment's, not in UTC. Getting this wrong moves
// which day every restore point in that one set belongs to.
func TestRun_BackupSetRetentionSetInheritsTheCalendarItDoesNotName(t *testing.T) {
	configPath := writeTestConfigWithRetentionBlock(t, "retention:\n  timezone: America/Vancouver\n  week_starts_on: sunday\n")

	out := captureStdout(t, func() {
		args := []string{"backup-set", "--config", configPath, "retention", "set", "production/postgres-primary",
			"--tier", "daily:day:30"}
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) != 0", args)
		}
	})
	if !strings.Contains(out, "timezone=America/Vancouver") {
		t.Errorf("the override did not inherit the deployment's timezone:\n%s", out)
	}
	if !strings.Contains(out, "week_starts_on=sunday") {
		t.Errorf("the override did not inherit the deployment's week start:\n%s", out)
	}
}
