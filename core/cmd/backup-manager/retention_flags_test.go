package main

import (
	"flag"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// captureStderr redirects os.Stderr for the duration of fn, so a test can
// assert on exactly what fail() (setup.go) printed, e.g. that a rejected
// retention override surfaces config.ValidateRetention's own error text
// rather than a separate, CLI-specific message. Restored unconditionally
// via t.Cleanup, including if fn itself calls t.Fatal.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })

	fn()

	w.Close()
	os.Stderr = orig
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stderr: %v", err)
	}
	return string(out)
}

// captureStdout mirrors captureStderr for os.Stdout, so a test can compare
// what `retention --dry-run` actually printed under two different ways of
// arriving at the same resolved policy.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	fn()

	w.Close()
	os.Stdout = orig
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stdout: %v", err)
	}
	return string(out)
}

// boolPtr mirrors config's own test helper of the same name (unexported to
// that package, so duplicated here rather than exported just for tests).
func boolPtr(b bool) *bool { return &b }

// --- applyRetentionOverrides: the pure fold-and-validate step ---

// resolvedRetention returns the six-field policy an omitted retention:
// block resolves to, exactly what a loaded config file gives cmdRetention
// today: the baseline every override test below starts from, so "an unset
// flag leaves config.Retention exactly as the config file resolved it" is
// checked against the same resolved shape the real CLI actually sees, not
// a hand-picked fixture.
func resolvedRetention(t *testing.T) config.Retention {
	t.Helper()
	var r config.Retention
	if err := config.ValidateRetention(&r); err != nil {
		t.Fatalf("resolving a baseline retention policy: %v", err)
	}
	return r
}

func TestApplyRetentionOverrides_NoOverridesLeavesResolvedConfigUntouched(t *testing.T) {
	base := resolvedRetention(t)
	r := base
	if err := applyRetentionOverrides(&r, retentionOverrides{}); err != nil {
		t.Fatalf("applyRetentionOverrides with no overrides: %v", err)
	}
	if r.Timezone != base.Timezone || r.WeekStartsOn != base.WeekStartsOn ||
		r.DailyDays != base.DailyDays || r.WeeklyMonths != base.WeeklyMonths || r.MonthlyMonths != base.MonthlyMonths {
		t.Fatalf("a zero-valued retentionOverrides changed the resolved config: got %+v, want %+v", r, base)
	}
	if r.ProtectLastKnownGood == nil || *r.ProtectLastKnownGood != *base.ProtectLastKnownGood {
		t.Fatalf("ProtectLastKnownGood changed with no override: got %v, want %v", r.ProtectLastKnownGood, base.ProtectLastKnownGood)
	}
}

func TestApplyRetentionOverrides_EachFieldOverridesIndependently(t *testing.T) {
	cases := []struct {
		name    string
		o       retentionOverrides
		checkFn func(t *testing.T, r config.Retention)
	}{
		{"timezone", retentionOverrides{timezone: "America/Vancouver"}, func(t *testing.T, r config.Retention) {
			if r.Timezone != "America/Vancouver" {
				t.Errorf("Timezone = %q, want America/Vancouver", r.Timezone)
			}
		}},
		{"week-starts-on", retentionOverrides{weekStartsOn: "sunday"}, func(t *testing.T, r config.Retention) {
			if r.WeekStartsOn != "sunday" {
				t.Errorf("WeekStartsOn = %q, want sunday", r.WeekStartsOn)
			}
		}},
		{"daily-days", retentionOverrides{dailyDays: 14}, func(t *testing.T, r config.Retention) {
			if r.DailyDays != 14 {
				t.Errorf("DailyDays = %d, want 14", r.DailyDays)
			}
		}},
		{"weekly-months", retentionOverrides{weeklyMonths: 6}, func(t *testing.T, r config.Retention) {
			if r.WeeklyMonths != 6 {
				t.Errorf("WeeklyMonths = %d, want 6", r.WeeklyMonths)
			}
		}},
		{"monthly-months", retentionOverrides{monthlyMonths: 24}, func(t *testing.T, r config.Retention) {
			if r.MonthlyMonths != 24 {
				t.Errorf("MonthlyMonths = %d, want 24", r.MonthlyMonths)
			}
		}},
		{"protect-last-known-good explicit false", retentionOverrides{protectLastKnownGood: boolPtr(false)}, func(t *testing.T, r config.Retention) {
			if r.ProtectLastKnownGood == nil || *r.ProtectLastKnownGood {
				t.Errorf("ProtectLastKnownGood = %v, want explicit false", r.ProtectLastKnownGood)
			}
		}},
		{"protect-last-known-good explicit true", retentionOverrides{protectLastKnownGood: boolPtr(true)}, func(t *testing.T, r config.Retention) {
			if r.ProtectLastKnownGood == nil || !*r.ProtectLastKnownGood {
				t.Errorf("ProtectLastKnownGood = %v, want explicit true", r.ProtectLastKnownGood)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := resolvedRetention(t)
			if err := applyRetentionOverrides(&r, tc.o); err != nil {
				t.Fatalf("applyRetentionOverrides: %v", err)
			}
			tc.checkFn(t, r)
		})
	}
}

func TestApplyRetentionOverrides_InvalidValueRefusedWithConfigValidateRetentionsOwnText(t *testing.T) {
	cases := []struct {
		name   string
		o      retentionOverrides
		mutate func(*config.Retention) // the identical raw value, applied directly, as if the config file itself carried it
	}{
		{"negative daily-days", retentionOverrides{dailyDays: -1}, func(r *config.Retention) { r.DailyDays = -1 }},
		{"negative weekly-months", retentionOverrides{weeklyMonths: -1}, func(r *config.Retention) { r.WeeklyMonths = -1 }},
		{"negative monthly-months", retentionOverrides{monthlyMonths: -1}, func(r *config.Retention) { r.MonthlyMonths = -1 }},
		{"unloadable timezone", retentionOverrides{timezone: "Mars/Phobos"}, func(r *config.Retention) { r.Timezone = "Mars/Phobos" }},
		{"non-weekday week-starts-on", retentionOverrides{weekStartsOn: "someday"}, func(r *config.Retention) { r.WeekStartsOn = "someday" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			viaOverride := resolvedRetention(t)
			overrideErr := applyRetentionOverrides(&viaOverride, tc.o)
			if overrideErr == nil {
				t.Fatalf("an invalid retention override (%+v) was accepted", tc.o)
			}

			// Refused for the identical reason config.ValidateRetention
			// would refuse the same value directly: not a separate,
			// CLI-specific message.
			viaDirect := resolvedRetention(t)
			tc.mutate(&viaDirect)
			directErr := config.ValidateRetention(&viaDirect)
			if directErr == nil {
				t.Fatalf("test setup bug: config.ValidateRetention accepted %+v directly", viaDirect)
			}

			if overrideErr.Error() != directErr.Error() {
				t.Fatalf("error text disagrees:\n  applyRetentionOverrides: %s\n  config.ValidateRetention:  %s", overrideErr, directErr)
			}
		})
	}
}

// --- resolveRetentionFlags: the *bool tri-state distinction ---

func TestResolveRetentionFlags_ProtectLastKnownGoodUnsetStaysNil(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	rf := registerRetentionFlags(fs)
	if err := fs.Parse([]string{"--daily-days", "5"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	o := resolveRetentionFlags(rf)
	if o.protectLastKnownGood != nil {
		t.Fatalf("protectLastKnownGood = %v, want nil (flag was never passed)", *o.protectLastKnownGood)
	}
	if o.dailyDays != 5 {
		t.Fatalf("dailyDays = %d, want 5", o.dailyDays)
	}
}

func TestResolveRetentionFlags_ProtectLastKnownGoodExplicitFalseIsDistinguishedFromUnset(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	rf := registerRetentionFlags(fs)
	if err := fs.Parse([]string{"--protect-last-known-good=false"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	o := resolveRetentionFlags(rf)
	if o.protectLastKnownGood == nil {
		t.Fatal("protectLastKnownGood = nil, want a non-nil pointer to false: the flag was explicitly passed")
	}
	if *o.protectLastKnownGood != false {
		t.Fatalf("protectLastKnownGood = %v, want false", *o.protectLastKnownGood)
	}
}

func TestResolveRetentionFlags_ProtectLastKnownGoodExplicitTrueIsDistinguishedFromUnset(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	rf := registerRetentionFlags(fs)
	if err := fs.Parse([]string{"--protect-last-known-good=true"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	o := resolveRetentionFlags(rf)
	if o.protectLastKnownGood == nil || !*o.protectLastKnownGood {
		t.Fatalf("protectLastKnownGood = %v, want a non-nil pointer to true", o.protectLastKnownGood)
	}
}

// --- end-to-end through run(): the CLI surface an operator actually sees ---

func TestRun_RetentionRejectsInvalidOverrideWithoutMutatingBehavior(t *testing.T) {
	configPath := writeTestConfig(t)
	got := run([]string{"retention", "--config", configPath, "--dry-run", "--daily-days", "-3"})
	if got == 0 {
		t.Fatal("run(retention --daily-days -3) = 0, want a non-zero exit code (negative daily-days must be refused)")
	}
}

func TestRun_RetentionAcceptsValidOverrides(t *testing.T) {
	configPath := writeTestConfig(t)
	got := run([]string{
		"retention", "--config", configPath, "--dry-run",
		"--daily-days", "3", "--weekly-months", "1", "--monthly-months", "1",
		"--timezone", "America/Vancouver", "--week-starts-on", "sunday",
		"--protect-last-known-good=false",
	})
	if got != 0 {
		t.Fatalf("run(retention with valid overrides) = %d, want 0", got)
	}
}

func TestRun_RetentionRejectsUnloadableTimezoneOverrideWithConfigsOwnErrorText(t *testing.T) {
	configPath := writeTestConfig(t)
	stderr := captureStderr(t, func() {
		got := run([]string{"retention", "--config", configPath, "--timezone", "Mars/Phobos"})
		if got == 0 {
			t.Fatal("run(retention --timezone Mars/Phobos) = 0, want a non-zero exit code")
		}
	})
	if !strings.Contains(stderr, "not a loadable IANA timezone") {
		t.Fatalf("stderr = %q, want it to contain config.ValidateRetention's own timezone error text", stderr)
	}
}

// writeTestConfigWithRetentionBlock is writeTestConfig plus an explicit,
// caller-supplied retention: YAML block, for tests that need to compare
// "the policy came from the file" against "the policy came from a CLI
// override".
func writeTestConfigWithRetentionBlock(t *testing.T, retentionYAML string) string {
	t.Helper()
	configPath := writeTestConfig(t)
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading generated config: %v", err)
	}
	content := strings.Split(string(data), "retention:\n")[0] + retentionYAML
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("rewriting config with a custom retention block: %v", err)
	}
	return configPath
}

// TestRun_CLIOverrideAndEquivalentConfigFileValueProduceIdenticalOutput is
// this issue's own boundary requirement: "a value set through the CLI and
// the same value set through the YAML file have to mean exactly the same
// thing." Neither config here has any journal records yet (writeTestConfig
// never runs `run`), so both reduce to the same "nothing eligible, nothing
// kept" report; what this test actually proves is that the CLI's override
// path and the file's own resolved value produce byte-identical printed
// output when they carry the same numbers, not merely "both succeed."
func TestRun_CLIOverrideAndEquivalentConfigFileValueProduceIdenticalOutput(t *testing.T) {
	viaFile := writeTestConfigWithRetentionBlock(t, "retention:\n"+
		"  timezone: America/Vancouver\n"+
		"  week_starts_on: sunday\n"+
		"  daily_days: 5\n"+
		"  weekly_months: 2\n"+
		"  monthly_months: 6\n")
	viaFlags := writeTestConfig(t) // retention: block omitted; every value below arrives as a CLI override instead

	fileOut := captureStdout(t, func() {
		if got := run([]string{"retention", "--config", viaFile, "--dry-run"}); got != 0 {
			t.Fatalf("run(retention) against the file-configured policy = %d, want 0", got)
		}
	})
	flagOut := captureStdout(t, func() {
		got := run([]string{
			"retention", "--config", viaFlags, "--dry-run",
			"--timezone", "America/Vancouver", "--week-starts-on", "sunday",
			"--daily-days", "5", "--weekly-months", "2", "--monthly-months", "6",
		})
		if got != 0 {
			t.Fatalf("run(retention) with equivalent CLI overrides = %d, want 0", got)
		}
	})

	if fileOut != flagOut {
		t.Fatalf("output disagrees between a file-configured policy and the identical policy set via CLI overrides:\n--- file ---\n%s\n--- flags ---\n%s", fileOut, flagOut)
	}
}
