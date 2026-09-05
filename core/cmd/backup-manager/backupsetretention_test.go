package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestConfigWithDeploymentPolicy is writeTestConfig with a
// deployment retention policy that is deliberately NOT the product
// default 7/3/12.
//
// Every case here leans on that. An inheritance bug that reaches for the
// documented defaults instead of this deployment's own policy is the
// exact failure #362 was written to stop, and it is invisible against a
// fixture whose deployment policy already IS the default.
func writeTestConfigWithDeploymentPolicy(t *testing.T) string {
	t.Helper()
	configPath := writeTestConfig(t)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	updated := strings.Replace(string(raw),
		"retention:\n  timezone: UTC\n  week_starts_on: monday\n",
		"retention:\n  timezone: America/Vancouver\n  week_starts_on: sunday\n"+
			"  daily_days: 90\n  weekly_months: 24\n  monthly_months: 60\n", 1)
	if updated == string(raw) {
		t.Fatalf("the fixture's retention block did not match; this helper is out of date:\n%s", raw)
	}
	if err := os.WriteFile(configPath, []byte(updated), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

const cliSet = "production/postgres-primary"

func retentionArgs(configPath string, rest ...string) []string {
	return append([]string{"backup-set", "retention", "--config", configPath, cliSet}, rest...)
}

// TestRun_BackupSetRetentionAcceptsFlagsOnEitherSideOfTheVerb pins what
// the dispatcher exists for.
//
// parseFlagsAroundOperands accepts flags before the operands, and
// `settings --config X patch` already works that way, so a dispatcher
// that took the verb off the front and passed the rest on would drop
// --config here. That does not fail loudly: it runs against the DEFAULT
// configuration path, which on a developer machine is a file that is not
// there and on a real deployment is the live one.
func TestRun_BackupSetRetentionAcceptsFlagsOnEitherSideOfTheVerb(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)

	out := captureStdout(t, func() {
		args := []string{"backup-set", "--config", configPath, "retention", cliSet}
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, got)
		}
	})
	if !strings.Contains(out, "retained under: the deployment's policy (inherited)") {
		t.Errorf("a flag before the verb did not reach the command:\n%s", out)
	}
}

// TestRun_BackupSetRetentionShowsWhichPolicyIsInForce is the show
// operation, and the assertion is on the ATTRIBUTION as much as on the
// chain.
//
// The CLI's `retention --dry-run` itemisation names the policy only when
// a set overrides, leaving absence to stand in for "the deployment's".
// This command says which one every time, because "which policy is in
// force for this set" is the entire question it exists to answer, and an
// answer whose common case is silence is one an operator has to already
// know the convention to read.
func TestRun_BackupSetRetentionShowsWhichPolicyIsInForce(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)

	out := captureStdout(t, func() {
		if got := run(retentionArgs(configPath)); got != 0 {
			t.Fatalf("run(show) = %d, want 0", got)
		}
	})
	for _, want := range []string{
		"backup set: production/postgres-primary",
		"retained under: the deployment's policy (inherited)",
		"timezone: America/Vancouver",
		"week_starts_on: sunday",
		"name=daily granularity=day keep=90",
		"name=monthly granularity=month keep=60",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show output is missing %q:\n%s", want, out)
		}
	}
	// An inheriting set is shown ONE policy. Printing the deployment's
	// again under a second heading would teach an operator that the two
	// headings mean the same thing, which is exactly the reading that
	// makes an override look like a no-op later.
	if strings.Contains(out, "--inherit would return this set to") {
		t.Errorf("an inheriting set was shown the deployment's policy twice:\n%s", out)
	}
}

// TestRun_BackupSetRetentionSetsShowsAndClears walks the whole cycle the
// issue asks the CLI for, and asserts each step against a SEPARATE show
// invocation rather than against the write's own echo, because "this was
// persisted and hot-reloaded" is the claim and an echo cannot make it.
func TestRun_BackupSetRetentionSetsShowsAndClears(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)

	if got := run(retentionArgs(configPath, "--daily-days", "3", "--weekly-months", "1", "--monthly-months", "2")); got != 0 {
		t.Fatalf("run(set) = %d, want 0", got)
	}

	out := captureStdout(t, func() {
		if got := run(retentionArgs(configPath)); got != 0 {
			t.Fatalf("run(show after set) = %d, want 0", got)
		}
	})
	if !strings.Contains(out, "retained under: this backup set's own policy") {
		t.Errorf("after setting an override, show does not say so:\n%s", out)
	}
	if !strings.Contains(out, "name=daily granularity=day keep=3") {
		t.Errorf("the set's own chain is not in force:\n%s", out)
	}
	// The calendar is inherited, not defaulted: an override that names no
	// timezone must not move this set to UTC inside a deployment that
	// deliberately set something else.
	if !strings.Contains(out, "timezone: America/Vancouver") {
		t.Errorf("the override did not inherit the deployment's timezone:\n%s", out)
	}
	// And the operator is shown what clearing would go back to, which is
	// the one thing they cannot work out from this set alone.
	if !strings.Contains(out, "--inherit would return this set to") {
		t.Errorf("an overriding set is not shown the deployment's policy:\n%s", out)
	}
	if !strings.Contains(out, "keep=90") {
		t.Errorf("the deployment's chain is not shown beside the override:\n%s", out)
	}

	if got := run(retentionArgs(configPath, "--inherit")); got != 0 {
		t.Fatalf("run(--inherit) = %d, want 0", got)
	}

	out = captureStdout(t, func() {
		if got := run(retentionArgs(configPath)); got != 0 {
			t.Fatalf("run(show after clear) = %d, want 0", got)
		}
	})
	if !strings.Contains(out, "retained under: the deployment's policy (inherited)") {
		t.Errorf("after --inherit, the set is not back on the deployment's policy:\n%s", out)
	}
	if !strings.Contains(out, "keep=90") {
		t.Errorf("after --inherit, the deployment's chain is not in force:\n%s", out)
	}

	// "No residue" is about the file, not only the resolved answer: a
	// retention key left behind as an empty block is refused by the next
	// config.Load, which is a daemon that will not start after an
	// ordinary operator action.
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "daily_days: 3") {
		t.Errorf("the cleared chain is still in the file:\n%s", raw)
	}
}

// TestRun_BackupSetRetentionSetsAWholeChainFromAPolicyFile is the only
// way this command can name a tiers chain, and it exists rather than a
// compact --tiers grammar because a second spelling of something this
// project already spells one way is a second thing to keep in step.
//
// "-" reads standard input, so a policy can be piped in without ever
// touching the filesystem.
func TestRun_BackupSetRetentionSetsAWholeChainFromAPolicyFile(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	policy := "" +
		"timezone: Europe/Berlin\n" +
		"tiers:\n" +
		"  - name: fortnightly\n" +
		"    granularity: days\n" +
		"    period_days: 14\n" +
		"    keep: 6\n" +
		"  - name: annual\n" +
		"    granularity: year\n" +
		"    keep: 5\n"
	if err := os.WriteFile(policyPath, []byte(policy), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	out := captureStdout(t, func() {
		if got := run(retentionArgs(configPath, "--policy-file", policyPath)); got != 0 {
			t.Fatalf("run(--policy-file) = %d, want 0", got)
		}
	})
	for _, want := range []string{
		"retained under: this backup set's own policy",
		"timezone: Europe/Berlin",
		"name=fortnightly granularity=days keep=6 period_days=14",
		"name=annual granularity=year keep=5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("policy-file output is missing %q:\n%s", want, out)
		}
	}
	// The week start was not in the file, so it still inherits.
	if !strings.Contains(out, "week_starts_on: sunday") {
		t.Errorf("a field the policy file omitted did not inherit:\n%s", out)
	}
}

// TestRun_BackupSetRetentionPolicyFileIsStrictAboutKeys pins the parse
// this command delegates to core/service.
//
// A misspelled key in a retention policy is silent data loss: the value
// the operator meant to set is simply not set, and what is left is a
// perfectly valid policy that keeps something else. Writing "retention:"
// at the top of the file is the likeliest version of that mistake, since
// it is how the same block appears inside config.yaml, and it has to be
// refused by name rather than parsed as an empty policy.
func TestRun_BackupSetRetentionPolicyFileIsStrictAboutKeys(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)
	dir := t.TempDir()

	cases := map[string]string{
		"a misspelled key":         "daily_dayz: 7\nweekly_months: 3\nmonthly_months: 12\n",
		"the retention key itself": "retention:\n  daily_days: 7\n  weekly_months: 3\n  monthly_months: 12\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".yaml")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if got := run(retentionArgs(configPath, "--policy-file", path)); got == 0 {
				t.Fatalf("run(--policy-file %s) = 0, want a refusal", name)
			}
			out, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if strings.Contains(string(out), "daily_days: 7") {
				t.Fatalf("a refused policy file still reached the config:\n%s", out)
			}
		})
	}
}

// TestRun_BackupSetRetentionRefusesHalfAChainWithTheConfigLayersWords is
// the trap the whole issue names, at the CLI.
//
// The message has to be config.Validate's own, not a paraphrase this
// command invented, because a second wording is the first step towards a
// second rule and this is the rule that decides whether restore points
// survive.
func TestRun_BackupSetRetentionRefusesHalfAChainWithTheConfigLayersWords(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)

	stderr := captureStderr(t, func() {
		if got := run(retentionArgs(configPath, "--daily-days", "120")); got == 0 {
			t.Fatal("naming one of the three scalars was accepted; under this deployment's 90/24/60 it would have " +
				"resolved to 120/3/12 and collapsed weekly from 24 months to 3")
		}
	})
	for _, want := range []string{"weekly_months", "monthly_months", "replaces the deployment's whole chain"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, stderr)
		}
	}

	out := captureStdout(t, func() {
		if got := run(retentionArgs(configPath)); got != 0 {
			t.Fatalf("run(show) = %d, want 0", got)
		}
	})
	if !strings.Contains(out, "retained under: the deployment's policy (inherited)") {
		t.Errorf("a refused override still changed the set:\n%s", out)
	}
}

// TestRun_BackupSetRetentionRefusesContradictoryFlags covers the two
// combinations that ask for two different things at once. Both are usage
// errors rather than precedence rules, on the same reasoning
// config.Validate refuses a policy that writes both spellings of a chain:
// picking one silently is how a retention policy ends up deciding on
// terms nobody wrote.
func TestRun_BackupSetRetentionRefusesContradictoryFlags(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(policyPath, []byte("daily_days: 1\nweekly_months: 1\nmonthly_months: 1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for _, args := range [][]string{
		retentionArgs(configPath, "--policy-file", ""),
		retentionArgs(configPath, "--inherit", "--daily-days", "3"),
		retentionArgs(configPath, "--inherit", "--policy-file", policyPath),
		retentionArgs(configPath, "--policy-file", policyPath, "--timezone", "UTC"),
	} {
		if got := run(args); got != 2 {
			t.Errorf("run(%v) = %d, want 2", args[2:], got)
		}
	}
}

// TestRun_BackupSetRetentionRefusesAThingThatIsNotABackupSetID keeps a
// malformed operand from reaching the service and coming back as a
// not-found for something that was never an id.
func TestRun_BackupSetRetentionRefusesAThingThatIsNotABackupSetID(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)
	for _, id := range []string{"postgres-primary", "a/b/c", "/postgres-primary", "production/"} {
		args := []string{"backup-set", "retention", "--config", configPath, id}
		if got := run(args); got != 2 {
			t.Errorf("run(retention %q) = %d, want 2", id, got)
		}
	}
	for _, args := range [][]string{
		{"backup-set", "--config", configPath},
		{"backup-set", "frobnicate", "--config", configPath, cliSet},
		{"backup-set", "retention", "--config", configPath},
		{"backup-set", "retention", "--config", configPath, cliSet, "extra"},
	} {
		if got := run(args); got != 2 {
			t.Errorf("run(%v) = %d, want 2", args, got)
		}
	}
}

// TestRun_BackupSetRetentionExplicitFalseProtectionIsAWrite is the same
// distinction service.CapacityUpdate's doc calls load-bearing, applied to
// the one flag here whose zero value is already its default: --protect-
// last-known-good=false has to count as "the operator named a policy
// field", or an operator turning FR-19's protection off for one set would
// get a show instead of a write and no indication that nothing happened.
//
// It is refused, and that is the correct outcome rather than a
// limitation: turning protection off is not a whole chain, so this
// submission is half a policy and config.Validate says so. What is being
// pinned is that it REACHED config.Validate at all.
func TestRun_BackupSetRetentionExplicitFalseProtectionIsAWrite(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)

	stderr := captureStderr(t, func() {
		if got := run(retentionArgs(configPath, "--protect-last-known-good=false")); got == 0 {
			t.Fatal("--protect-last-known-good=false alone was accepted as a whole policy")
		}
	})
	if !strings.Contains(stderr, "whole chain") {
		t.Errorf("--protect-last-known-good=false was treated as a show rather than a write; stderr:\n%s", stderr)
	}
}

// TestRun_BackupSetRetentionRefusesAFirstMediumMappingUntilAcknowledged is
// FR-27's consent at the CLI, on the one path this command has that can
// name a medium: a policy file. The refusal is the disclosure, printed as
// the error, and --acknowledge-medium-disclosure is what lets the same
// write through. The flag rides beside the policy rather than inside the
// file, so a policy file cannot consent on the operator's behalf every
// time it is applied.
func TestRun_BackupSetRetentionRefusesAFirstMediumMappingUntilAcknowledged(t *testing.T) {
	configPath := writeTestConfigWithDeploymentPolicy(t)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	declared := string(raw) +
		"storage_mediums:\n" +
		"  - id: offsite_s3\n" +
		"    type: s3\n" +
		"    region: us-east-1\n" +
		"    bucket: nas-backups\n" +
		"    credentials:\n" +
		"      env: BACKUP_S3_CLI_TEST\n"
	if err := os.WriteFile(configPath, []byte(declared), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	policy := "" +
		"tiers:\n" +
		"  - name: daily\n" +
		"    granularity: day\n" +
		"    keep: 7\n" +
		"    medium: offsite_s3\n"
	if err := os.WriteFile(policyPath, []byte(policy), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	stderr := captureStderr(t, func() {
		if got := run(retentionArgs(configPath, "--policy-file", policyPath)); got == 0 {
			t.Fatal("a policy file sending a tier to a medium was written with no acknowledgment")
		}
	})
	if !strings.Contains(stderr, "delete the copy on this machine") || !strings.Contains(stderr, "daily -> offsite_s3") {
		t.Errorf("the refusal is not the disclosure:\n%s", stderr)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) != declared {
		t.Errorf("a refused write changed the configuration file:\n%s", after)
	}

	// The acknowledgment on its own writes nothing, so it is a usage
	// error rather than a silently ignored flag.
	if got := run(retentionArgs(configPath, "--acknowledge-medium-disclosure")); got != 2 {
		t.Errorf("run(--acknowledge-medium-disclosure alone) = %d, want 2", got)
	}

	out := captureStdout(t, func() {
		if got := run(retentionArgs(configPath, "--policy-file", policyPath, "--acknowledge-medium-disclosure")); got != 0 {
			t.Fatalf("run(--policy-file --acknowledge-medium-disclosure) = %d, want 0", got)
		}
	})
	for _, want := range []string{"retained under: this backup set's own policy", "name=daily granularity=day keep=7 medium=offsite_s3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}
