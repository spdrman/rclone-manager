package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `rbm settings patch --policy-file` (EPIC G, G2.3, issue
// #595): the deployment's whole retention chain, from the command line.
//
// cmdSettings used to say the opposite in its own doc, and these tests are
// the reversal. The argument for the refusal was that "replacing the whole
// chain is still, and remains, a config-file edit, exactly like every
// other case the config-file answer already covers". Two things happened
// to that. A chain stopped being purely a policy about time and started
// naming where the bytes live, and EPIC G requires every capability to be
// reachable from `backup-manager`. And the config-file answer is not an
// equal-power route beside a running engine at all: this very command
// refuses a file write there, because nothing watches config.yaml (#543),
// so "edit the file" is advice that does not work in the deployment where
// a fleet-wide destination change matters most.
//
// The service layer needed nothing. RetentionUpdate.Tiers, the disclosure
// gate and the empty-chain refusal all already existed and the browser
// already used them over PATCH /settings; this was the CLI half of a
// surface that was only ever missing here.

// offsitePolicyFile writes a `retention:` block that sends monthly to the
// medium writeOffsiteTestConfig declares, and brings daily back to local.
//
// Both halves matter. The new mapping is what makes the disclosure gate
// fire, and daily losing its medium is what proves the chain is REPLACED
// rather than merged: a merge would leave daily offsite.
func offsitePolicyFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "policy.yaml")
	body := "tiers:\n" +
		"  - name: daily\n" +
		"    granularity: day\n" +
		"    keep: 7\n" +
		"  - name: monthly\n" +
		"    granularity: month\n" +
		"    keep: 12\n" +
		"    medium: cold_offsite\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestRun_SettingsPatchPolicyFileReplacesTheDeploymentChain is the
// capability itself, and the read-back is the half that matters: the chain
// has to persist, and `settings` has to print where each tier's copies go,
// so an operator can read the destination without opening YAML.
func TestRun_SettingsPatchPolicyFileReplacesTheDeploymentChain(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)
	policyPath := offsitePolicyFile(t, filepath.Dir(configPath))

	out := captureStdout(t, func() {
		args := []string{"settings", "--config", configPath, "patch",
			"--policy-file", policyPath, "--acknowledge-medium-disclosure"}
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, got)
		}
	})
	if !strings.Contains(out, "name=monthly") {
		t.Fatalf("the patch output does not carry the submitted chain.\ngot:\n%s", out)
	}
	if !strings.Contains(out, "medium=cold_offsite") {
		t.Errorf("the patch output does not say where monthly's copies go; a chain that names a destination has to report it, or an operator has to open YAML to find out.\ngot:\n%s", out)
	}

	back := captureStdout(t, func() {
		if got := run([]string{"settings", "--config", configPath}); got != 0 {
			t.Fatalf(`run(["settings"]) != 0`)
		}
	})
	if !strings.Contains(back, "medium=cold_offsite") {
		t.Errorf("a second, independent read does not see the destination, so the write did not persist.\ngot:\n%s", back)
	}
	// The chain REPLACED rather than merged: daily gave its medium up.
	for _, line := range strings.Split(back, "\n") {
		if strings.Contains(line, "name=daily") && strings.Contains(line, "medium=") {
			t.Errorf("daily still names a medium after a chain that gave it none, so the submitted chain was merged with the file's rather than replacing it.\ngot line: %s", line)
		}
	}
}

// TestRun_SettingsPatchPolicyFileNeedsTheDisclosure: the deployment-level
// write gets the same consent gate `backup-set retention` already has, and
// the refusal IS the disclosure.
func TestRun_SettingsPatchPolicyFileNeedsTheDisclosure(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)
	policyPath := offsitePolicyFile(t, filepath.Dir(configPath))

	err := captureStderr(t, func() {
		args := []string{"settings", "--config", configPath, "patch", "--policy-file", policyPath}
		if got := run(args); got == 0 {
			t.Fatalf("run(%v) = 0; a write that sends a tier somewhere new has to be refused without the acknowledgment", args)
		}
	})
	for _, want := range []string{"monthly", "cold_offsite", "acknowledg"} {
		if !strings.Contains(err, want) {
			t.Errorf("the refusal does not mention %q, so it is not carrying the disclosure it is refusing for.\ngot:\n%s", want, err)
		}
	}

	// And nothing was written. A refused consent gate that had already
	// rewritten the file would be the worst of both.
	back := captureStdout(t, func() {
		if got := run([]string{"settings", "--config", configPath}); got != 0 {
			t.Fatalf(`run(["settings"]) != 0`)
		}
	})
	if strings.Contains(back, "name=monthly") {
		t.Errorf("the refused patch was written anyway.\ngot:\n%s", back)
	}
}

// TestRun_SettingsPatchPolicyFileFromStdin is `--policy-file -`, which is
// the spelling EPIC G's terminal echoes, so the transcript it prints is
// runnable as typed with the policy block underneath it.
func TestRun_SettingsPatchPolicyFileFromStdin(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)
	policyPath := offsitePolicyFile(t, filepath.Dir(configPath))

	f, err := os.Open(policyPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	orig := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = orig })

	out := captureStdout(t, func() {
		args := []string{"settings", "--config", configPath, "patch",
			"--policy-file", "-", "--acknowledge-medium-disclosure"}
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, got)
		}
	})
	if !strings.Contains(out, "medium=cold_offsite") {
		t.Errorf("the policy read from standard input did not take.\ngot:\n%s", out)
	}
}

// TestRun_SettingsPatchPolicyFileUsageMistakes: the command line is the
// thing that is wrong in each of these, so each is a 2 and nothing is
// opened. They are the same three rules `backup-set retention` already
// applies to the identically spelled flag, because an operator moves
// between the two commands and should not meet two grammars.
func TestRun_SettingsPatchPolicyFileUsageMistakes(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)
	policyPath := offsitePolicyFile(t, filepath.Dir(configPath))

	for _, tc := range []struct {
		name string
		args []string
	}{
		{
			name: "a policy file with no path",
			args: []string{"settings", "--config", configPath, "patch", "--policy-file", ""},
		},
		{
			name: "a policy file beside a retention flag it already carries",
			args: []string{"settings", "--config", configPath, "patch", "--policy-file", policyPath, "--timezone", "UTC"},
		},
		{
			name: "an acknowledgment on a command line that writes no policy",
			args: []string{"settings", "--config", configPath, "patch", "--cap-bytes", "1", "--acknowledge-medium-disclosure"},
		},
		{
			name: "a policy flag without the patch operand",
			args: []string{"settings", "--config", configPath, "--policy-file", policyPath},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureStderr(t, func() {
				if got := run(tc.args); got != 2 {
					t.Errorf("run(%v) = %d, want 2", tc.args, got)
				}
			})
		})
	}
}

// TestRun_SettingsPatchPolicyFileRefusesTheLegacyScalars.
//
// PATCH /settings has no field for daily_days/weekly_months/monthly_months
// and neither does this flag, so a policy file spelling the chain that way
// is refused rather than silently dropped. Silently dropping it is the
// dangerous reading: the operator submitted a whole policy, the write
// would report success, and the chain would be whatever the file already
// said.
func TestRun_SettingsPatchPolicyFileRefusesTheLegacyScalars(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)
	policyPath := filepath.Join(filepath.Dir(configPath), "legacy.yaml")
	body := "daily_days: 7\nweekly_months: 3\nmonthly_months: 12\n"
	if err := os.WriteFile(policyPath, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := captureStderr(t, func() {
		args := []string{"settings", "--config", configPath, "patch", "--policy-file", policyPath}
		if got := run(args); got == 0 {
			t.Fatalf("run(%v) = 0; a chain this surface cannot express has to be refused rather than dropped", args)
		}
	})
	for _, want := range []string{"daily_days", "tiers"} {
		if !strings.Contains(err, want) {
			t.Errorf("the refusal does not mention %q, so it does not say what was submitted or what to write instead.\ngot:\n%s", want, err)
		}
	}
}
