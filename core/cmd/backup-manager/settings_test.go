package main

import (
	"strings"
	"testing"
)

func TestRun_SettingsGetAgainstAWorkingConfig(t *testing.T) {
	configPath := writeTestConfig(t)
	out := captureStdout(t, func() {
		if got := run([]string{"settings", "--config", configPath}); got != 0 {
			t.Errorf("run([\"settings\"]) = %d, want 0", got)
		}
	})
	if !strings.Contains(out, "retention:") || !strings.Contains(out, "capacity:") {
		t.Errorf("settings output = %q, want it to report both retention: and capacity:", out)
	}
	// writeTestConfig's own retention block sets timezone: UTC, so a
	// resolved GET has to report it back, proving this reads the real
	// config rather than printing a hardcoded shape.
	if !strings.Contains(out, "timezone: UTC") {
		t.Errorf("settings output = %q, want it to report the configured timezone", out)
	}
}

func TestRun_SettingsPatchRejectsAnUnknownSubcommand(t *testing.T) {
	configPath := writeTestConfig(t)
	for _, args := range [][]string{
		{"settings", "--config", configPath, "frobnicate"},
		{"settings", "--config", configPath, "patch", "extra-operand"},
	} {
		if got := run(args); got != 2 {
			t.Errorf("run(%v) = %d, want 2", args, got)
		}
	}
}

// TestRun_SettingsPatchChangesRetentionAndPersists proves a patch is a
// real, persisted, hot-reloaded write, not merely an echoed request: a
// second, independent `settings` invocation (its own process-equivalent
// call to `run`, reading configPath fresh) has to see the new value.
func TestRun_SettingsPatchChangesRetentionAndPersists(t *testing.T) {
	configPath := writeTestConfig(t)

	patchOut := captureStdout(t, func() {
		args := []string{"settings", "--config", configPath, "patch",
			"--timezone", "America/Vancouver",
			"--protect-last-known-good=false",
		}
		if got := run(args); got != 0 {
			t.Fatalf("run(%v) = %d, want 0", args, got)
		}
	})
	if !strings.Contains(patchOut, "timezone: America/Vancouver") {
		t.Errorf("patch output = %q, want it to report the new timezone", patchOut)
	}
	if !strings.Contains(patchOut, "protect_last_known_good: false") {
		t.Errorf("patch output = %q, want it to report protect_last_known_good: false", patchOut)
	}

	getOut := captureStdout(t, func() {
		if got := run([]string{"settings", "--config", configPath}); got != 0 {
			t.Fatalf("run([\"settings\"]) = %d, want 0", got)
		}
	})
	if !strings.Contains(getOut, "timezone: America/Vancouver") {
		t.Errorf("settings output after patch = %q, want the change to have persisted", getOut)
	}
	if !strings.Contains(getOut, "protect_last_known_good: false") {
		t.Errorf("settings output after patch = %q, want protect_last_known_good: false to have persisted", getOut)
	}
}

// TestRun_SettingsPatchCapacityZeroMeansRemoveTheCap proves --cap-bytes=0,
// passed explicitly, is read as "remove the cap" (a real, meaningful
// write) rather than "this flag was not named" -- the exact distinction
// service.CapacityUpdate's own doc calls load-bearing.
func TestRun_SettingsPatchCapacityZeroMeansRemoveTheCap(t *testing.T) {
	configPath := writeTestConfig(t)

	setArgs := []string{"settings", "--config", configPath, "patch", "--cap-bytes", "1000000"}
	if got := run(setArgs); got != 0 {
		t.Fatalf("run(%v) = %d, want 0", setArgs, got)
	}

	clearOut := captureStdout(t, func() {
		clearArgs := []string{"settings", "--config", configPath, "patch", "--cap-bytes", "0"}
		if got := run(clearArgs); got != 0 {
			t.Fatalf("run(%v) = %d, want 0", clearArgs, got)
		}
	})
	if !strings.Contains(clearOut, "cap_bytes: 0") {
		t.Errorf("patch output = %q, want cap_bytes: 0 (explicitly cleared)", clearOut)
	}
}

// TestRun_SettingsPatchWithNoFlagsIsRefused mirrors
// UpdateSettingsRequest.namesNothing's own contract: a patch that names
// no field must never silently succeed.
func TestRun_SettingsPatchWithNoFlagsIsRefused(t *testing.T) {
	configPath := writeTestConfig(t)
	args := []string{"settings", "--config", configPath, "patch"}
	if got := run(args); got == 0 {
		t.Errorf("run(%v) with no patch flags = 0, want non-zero (a patch must name at least one setting)", args)
	}
}

// TestRun_SettingsWithPatchFlagsButNoPatchOperandIsRefused is the mirror
// case of TestRun_SettingsPatchWithNoFlagsIsRefused: a patch-shaped flag
// given WITHOUT the "patch" operand must never be silently accepted and
// ignored. Before this test's fix, `settings --timezone ...` parsed
// successfully, dropped the flag on the floor, and printed the unchanged
// current settings with exit code 0 -- identical to a plain `settings`
// call, and indistinguishable from an operator who forgot the word
// "patch" believing their change took effect on a live daemon.
func TestRun_SettingsWithPatchFlagsButNoPatchOperandIsRefused(t *testing.T) {
	configPath := writeTestConfig(t)
	args := []string{"settings", "--config", configPath, "--timezone", "America/Vancouver"}

	var got int
	errOut := captureStderr(t, func() {
		got = run(args)
	})

	if got == 0 {
		t.Errorf("run(%v) = 0, want non-zero (a patch-only flag with no \"patch\" operand must be refused)", args)
	}
	if !strings.Contains(errOut, "timezone") {
		t.Errorf("run(%v) stderr = %q, want it to name the ignored flag", args, errOut)
	}
	if !strings.Contains(errOut, "patch") {
		t.Errorf("run(%v) stderr = %q, want it to mention the missing \"patch\" operand", args, errOut)
	}

	// Confirm the setting was NOT silently applied: a follow-up plain
	// `settings` read must still report the config's original value, not
	// America/Vancouver.
	getOut := captureStdout(t, func() {
		if rc := run([]string{"settings", "--config", configPath}); rc != 0 {
			t.Fatalf("run([\"settings\"]) = %d, want 0", rc)
		}
	})
	if strings.Contains(getOut, "timezone: America/Vancouver") {
		t.Errorf("settings output after refused patch = %q, want the timezone to be unchanged", getOut)
	}
}
