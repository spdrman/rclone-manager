package main

import (
	"os"
	"strings"
	"testing"
)

// H2.2 (#622) on the command line: the local hard drive in `medium list`,
// the default and how it moves, and `test-connection` as the name the rest
// of this product uses for the check `preflight` already ran.
//
// These drive `run` end to end against a real configuration file rather
// than calling the verbs directly, because what this issue adds is
// operator-visible: an id an operator types, a line an operator reads,
// and an exit code a script branches on. A test that called mediumDefault
// with a fake route would prove the plumbing and none of that.

// TestRun_MediumListCarriesTheLocalHardDrive is #622's own complaint
// against the list an operator actually gets: the drive backups land on
// was not in it.
func TestRun_MediumListCarriesTheLocalHardDrive(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)

	out := captureStdout(t, func() {
		if got := run([]string{"medium", "list", "--config", configPath}); got != 0 {
			t.Fatalf("medium list: %d, want 0", got)
		}
	})

	if !strings.Contains(out, "local\n") {
		t.Errorf("medium list does not carry the local hard drive:\n%s", out)
	}
	if !strings.Contains(out, "cold_offsite") {
		t.Errorf("medium list dropped the declared destination:\n%s", out)
	}
	// The drive it writes to, which is the fact that makes the entry
	// worth having rather than a row saying only "local".
	if !strings.Contains(out, "path:") {
		t.Errorf("the local entry does not name the drive it writes to:\n%s", out)
	}
	if !strings.Contains(out, "default:") {
		t.Errorf("no destination is marked as the one a new tier starts on:\n%s", out)
	}
}

// TestRun_MediumDefaultMovesItAndSaysWhatItDidNot pins both halves of the
// output. The sentence about what did NOT happen is load-bearing: an
// operator reading "the default is now cold_offsite" beside a settings
// page could reasonably fear their backups just started moving, and that
// fear is what makes somebody reach for a rollback.
func TestRun_MediumDefaultMovesItAndSaysWhatItDidNot(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)

	out := captureStdout(t, func() {
		if got := run([]string{"medium", "default", "--config", configPath, "cold_offsite"}); got != 0 {
			t.Fatalf("medium default cold_offsite: %d, want 0", got)
		}
	})
	if !strings.Contains(out, "cold_offsite is now this deployment's default storage destination") {
		t.Errorf("the output does not say the default moved:\n%s", out)
	}
	if !strings.Contains(out, "nothing moved") {
		t.Errorf("the output does not say that no backup was relocated:\n%s", out)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "default_storage_medium: cold_offsite") {
		t.Errorf("the configuration file does not record the default:\n%s", raw)
	}
	// The tier that was already on cold_offsite is still on it, and the
	// fixture's chain is untouched. This is the assertion that separates
	// "the default moved" from "everything moved".
	if got := tierMediumCount(string(raw), "cold_offsite"); got != 1 {
		t.Errorf("moving the default left %d tier(s) on cold_offsite, want 1:\n%s", got, raw)
	}
}

// TestRun_MediumRemoveRefusesTheDefault is the first invariant at the
// surface an operator reaches it from, exit code included: a script that
// removes destinations has to be able to tell a refusal from a success.
func TestRun_MediumRemoveRefusesTheDefault(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)
	if got := run([]string{"medium", "default", "--config", configPath, "cold_offsite"}); got != 0 {
		t.Fatal("precondition failed: medium default did not take")
	}

	err := captureStderr(t, func() {
		if got := run([]string{"medium", "remove", "--config", configPath, "cold_offsite"}); got == 0 {
			t.Fatal("medium remove exited 0 for the default destination")
		}
	})
	for _, want := range []string{"default", "Make another destination the default first"} {
		if !strings.Contains(err, want) {
			t.Errorf("the refusal does not mention %q, so it does not say what to do about it:\n%s", want, err)
		}
	}

	raw, rerr := os.ReadFile(configPath)
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if !strings.Contains(string(raw), "cold_offsite") {
		t.Errorf("a refused removal still took the destination out of the file:\n%s", raw)
	}
}

// TestRun_MediumRemoveRefusesTheLocalHardDrive is the second invariant at
// the same surface. Local is not declared, so it cannot be un-declared,
// and without this the "there is never zero destinations" rule is one
// command away from being false.
func TestRun_MediumRemoveRefusesTheLocalHardDrive(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)

	err := captureStderr(t, func() {
		if got := run([]string{"medium", "remove", "--config", configPath, "local"}); got == 0 {
			t.Fatal("medium remove exited 0 for the local hard drive")
		}
	})
	if !strings.Contains(err, "cannot be un-declared") {
		t.Errorf("the refusal does not say why local cannot go:\n%s", err)
	}
}

// TestRun_MediumTestConnectionIsPreflightUnderTheProductsOwnName is the
// naming half of #622. The two verbs are one check, so the proof is that
// they produce the same report rather than that both exist.
func TestRun_MediumTestConnectionIsPreflightUnderTheProductsOwnName(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)

	underNewName := captureStdout(t, func() {
		// A configuration whose credential is an environment variable
		// nothing sets, so the check fails at its first step. That is the
		// point rather than a limitation: this case is about the two
		// verbs agreeing, and a failing report has more shape to disagree
		// about than a passing one would.
		if got := run([]string{"medium", "test-connection", "--config", configPath, "cold_offsite"}); got != 1 {
			t.Fatalf("medium test-connection: %d, want 1 for a destination that cannot be reached", got)
		}
	})
	underOldName := captureStdout(t, func() {
		if got := run([]string{"medium", "preflight", "--config", configPath, "cold_offsite"}); got != 1 {
			t.Fatalf("medium preflight: %d, want 1", got)
		}
	})

	if stripLogLines(underNewName) != stripLogLines(underOldName) {
		t.Errorf("test-connection and preflight printed different reports, so they are two checks rather than one under two names.\ntest-connection:\n%s\npreflight:\n%s",
			stripLogLines(underNewName), stripLogLines(underOldName))
	}
	if stripLogLines(underNewName) == "" {
		t.Fatal("both verbs printed nothing, so this comparison would pass against a build where neither runs")
	}
}

// TestRun_MediumTestConnectionAnswersForTheLocalHardDrive is #622's added
// acceptance on the command line: the local entry has something to test,
// and a missing backup root is reported as its own failed step rather
// than as a verb that refuses to run.
func TestRun_MediumTestConnectionAnswersForTheLocalHardDrive(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)
	// The fixture points its backup set at a directory nothing has run a
	// cycle into, so it does not exist yet. Creating it is what makes the
	// first half of this case a PASSING control rather than a second copy
	// of the failing one.
	if err := os.MkdirAll(localBackupRootOf(t, configPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// The passing control first, against a backup root that exists. Both
	// halves are needed: a failing check and a verb that always fails
	// look identical from one case.
	out := captureStdout(t, func() {
		if got := run([]string{"medium", "test-connection", "--config", configPath, "local"}); got != 0 {
			t.Fatalf("medium test-connection local against a writable backup root: %d, want 0", got)
		}
	})
	for _, want := range []string{"storage medium local", "reach", "write", "read_back", "delete"} {
		if !strings.Contains(out, want) {
			t.Errorf("the local report does not carry %q:\n%s", want, out)
		}
	}

	// And now with the drive gone, which on a NAS is a volume that did
	// not mount. It has to read as a missing path rather than as a
	// generic failure.
	if err := os.RemoveAll(localBackupRootOf(t, configPath)); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	missing := captureStdout(t, func() {
		if got := run([]string{"medium", "test-connection", "--config", configPath, "local"}); got != 1 {
			t.Fatalf("medium test-connection local against a missing backup root: %d, want 1", got)
		}
	})
	if !strings.Contains(missing, "not_found") {
		t.Errorf("a missing backup root is not reported as not_found, so it cannot be told from a permission problem:\n%s", missing)
	}
}

// localBackupRootOf is the directory writeOffsiteTestConfig points its one
// backup set at, which is what config.EffectiveBackupRoot derives the
// local destination's path from. Read out of the file rather than rebuilt
// from the fixture's own layout, so this stays true if that fixture moves
// its directories.
func localBackupRootOf(t *testing.T, configPath string) string {
	t.Helper()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if _, path, ok := strings.Cut(strings.TrimSpace(line), "local_path: "); ok {
			return path
		}
	}
	t.Fatalf("no local_path in the fixture configuration:\n%s", raw)
	return ""
}

// TestRun_SettingsPatchTierMediumMovesOneTierAndLeavesTheRest is the CLI
// parity #623 requires for the picker under a tier, and the assertion is
// what it does NOT touch as much as what it does. RetentionUpdate.Tiers
// replaces the whole chain, so the interesting failure is a patch that
// moves one tier's destination and quietly loses another's.
func TestRun_SettingsPatchTierMediumMovesOneTierAndLeavesTheRest(t *testing.T) {
	configPath := writeTwoTierMediumConfig(t)

	if got := run([]string{
		"settings", "--config", configPath, "patch",
		"--tier-medium", "weekly=cold_offsite",
		"--acknowledge-medium-disclosure",
	}); got != 0 {
		t.Fatalf("settings patch --tier-medium: %d, want 0", got)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Count(string(raw), "medium: cold_offsite") != 2 {
		t.Errorf("want the tier that was already there plus the one just moved, got:\n%s", raw)
	}
	if !strings.Contains(string(raw), "name: monthly") {
		t.Errorf("the patch lost a tier it was not asked to touch:\n%s", raw)
	}
}

// TestRun_SettingsPatchTierMediumMovesATierBackToLocal is the other
// direction, and the one the picker needs most: an operator who sent a
// tier to S3 has to be able to bring it back, and "local" is the name
// every surface in this product now uses for where it comes back to.
func TestRun_SettingsPatchTierMediumMovesATierBackToLocal(t *testing.T) {
	configPath := writeTwoTierMediumConfig(t)

	if got := run([]string{
		"settings", "--config", configPath, "patch",
		"--tier-medium", "daily=local",
	}); got != 0 {
		t.Fatalf("settings patch --tier-medium daily=local: %d, want 0", got)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "medium: local") {
		t.Errorf("moving a tier back wrote the reserved local id into the file, which config.Validate refuses and which gives local a second spelling:\n%s", raw)
	}
	if strings.Contains(string(raw), "medium: cold_offsite") {
		t.Errorf("the tier is still on the declared destination:\n%s", raw)
	}
}

// TestRun_SettingsPatchTierMediumRefusesATierTheChainDoesNotHave: a name
// the chain does not carry is a typo, and creating a tier from it would
// be inventing a retention rule that says neither what to keep nor for
// how long.
func TestRun_SettingsPatchTierMediumRefusesATierTheChainDoesNotHave(t *testing.T) {
	configPath := writeTwoTierMediumConfig(t)

	err := captureStderr(t, func() {
		if got := run([]string{
			"settings", "--config", configPath, "patch",
			"--tier-medium", "montly=cold_offsite",
		}); got == 0 {
			t.Fatal("settings patch accepted a tier name the chain does not carry")
		}
	})
	if !strings.Contains(err, "montly") || !strings.Contains(err, "monthly") {
		t.Errorf("the refusal does not name both the typo and the tiers there are:\n%s", err)
	}

	raw, rerr := os.ReadFile(configPath)
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if strings.Count(string(raw), "medium: cold_offsite") != 1 {
		t.Errorf("a refused patch changed the chain:\n%s", raw)
	}
}

// writeTwoTierMediumConfig is writeOffsiteTestConfig with a three-tier
// chain, one of whose tiers already names the declared destination.
//
// Three tiers rather than one, because every case above is about what a
// whole-chain replace does to the tiers it was NOT asked about, and a
// one-tier chain cannot tell a correct patch from one that dropped
// everything else.
func writeTwoTierMediumConfig(t *testing.T) string {
	t.Helper()
	configPath := writeOffsiteTestConfig(t)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	extended := strings.Replace(string(raw),
		"    - name: daily\n      granularity: day\n      keep: 7\n      medium: cold_offsite\n",
		"    - name: daily\n      granularity: day\n      keep: 7\n      medium: cold_offsite\n"+
			"    - name: weekly\n      granularity: week\n      keep: 3\n      window_unit: month\n"+
			"    - name: monthly\n      granularity: month\n      keep: 12\n", 1)
	if extended == string(raw) {
		t.Fatalf("the fixture's retention chain is not the shape this helper extends:\n%s", raw)
	}
	if err := os.WriteFile(configPath, []byte(extended), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// stripLogLines drops this binary's structured log from a captured
// stream, leaving what an operator reads.
//
// It exists because the two verbs above have to produce the SAME report,
// and the log lines carry timestamps, so comparing the raw streams would
// compare two clocks and fail for a reason that has nothing to do with
// either verb. The report itself is compared byte for byte, which is the
// claim being made.
func stripLogLines(stream string) string {
	var kept []string
	for _, line := range strings.Split(stream, "\n") {
		if strings.HasPrefix(line, "{") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// tierMediumCount counts the retention tiers naming one destination in a
// written configuration file.
//
// It exists because "medium: cold_offsite" is a substring of
// "default_storage_medium: cold_offsite", so a plain count over the file
// reads the default key as a tier and reports one more than there is.
// That is the exact false positive #622's own first run of these cases
// produced, and it would have read as "moving the default rewrote the
// chain" when the chain was untouched.
func tierMediumCount(raw, medium string) int {
	n := 0
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "medium: "+medium {
			n++
		}
	}
	return n
}
