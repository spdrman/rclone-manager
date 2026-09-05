// Issue #333's regression seam. `retention`'s six FR-18/FR-19 override
// flags (#111, B3.6) fold onto cfg.Retention, but since this issue every
// decision reads a backup set's own resolved bs.Retention instead. Nothing
// between the fold and the decision re-resolved, so every flag became a
// silent no-op: the command still printed a preview, just not the preview
// the operator asked for. retention_flags_test.go could not catch it,
// because it exercises applyRetentionOverrides as a pure function and
// never drives cmdRetention at all.
//
// These tests drive the real command over a real local-backend fetch and
// read what it actually printed, which is the only shape that stays honest
// when the resolution seam moves again.
package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// writeTwoSetTestConfig writes a config with two backup sets in one
// source: one inheriting the deployment's retention policy, one declaring
// its own. Both point at their own remote directory holding one artifact,
// so `run` produces one KEEP line for each.
func writeTwoSetTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	mk := func(name string) (string, string) {
		remoteDir := filepath.Join(dir, name, "remote")
		localDir := filepath.Join(dir, name, "local")
		if err := os.MkdirAll(remoteDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(remoteDir, "backup.dump"), []byte("payload for "+name), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return remoteDir, localDir
	}

	inheritRemote, inheritLocal := mk("inherits")
	ownRemote, ownLocal := mk("owns")

	set := func(id, remoteDir, localDir, extra string) string {
		return "      - id: " + id + "\n" +
			"        remote:\n" +
			"          type: local\n" +
			"        remote_path: " + remoteDir + "\n" +
			"        local_path: " + localDir + "\n" +
			"        include:\n" +
			"          - \"*.dump\"\n" +
			"        completion:\n" +
			"          strategy: rename\n" +
			"        stale_after: 24h\n" + extra
	}

	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		set("inherits", inheritRemote, inheritLocal, "") +
		set("owns", ownRemote, ownLocal,
			"        retention:\n"+
				"          timezone: UTC\n"+
				"          week_starts_on: monday\n"+
				"          tiers:\n"+
				"            - name: set_own\n"+
				"              granularity: year\n"+
				"              keep: 1\n") +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// TestRun_RetentionTierFlagReachesTheSetsResolvedPolicy is the proof for
// the regression itself: a -tier override has to change the chain the
// preview actually decides under, not just the copy of the policy that
// nothing downstream reads any more.
func TestRun_RetentionTierFlagReachesTheSetsResolvedPolicy(t *testing.T) {
	configPath := writeTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run: %d, want 0", got)
	}

	out := captureStdout(t, func() {
		if got := run([]string{"retention", "--config", configPath, "--dry-run",
			"-tier", "cli_only:year:1"}); got != 0 {
			t.Fatalf("retention -tier: %d, want 0", got)
		}
	})

	want := regexp.MustCompile(`\n\s+KEEP\s+backup\.dump\s+tiers=\[CLI_ONLY\(both\) LAST_KNOWN_GOOD\]\n`)
	if !want.MatchString(out) {
		t.Errorf("the -tier override never reached the set's resolved policy.\nwant a line matching %s\ngot:\n%s", want, out)
	}
}

// TestRun_RetentionScalarFlagsReachTheSetsResolvedPolicy is the same proof
// for the three scalar flags, which take a different branch through
// applyRetentionOverrides.
//
// The expectation is derived rather than recorded. The one artifact is
// staged with a producer timestamp 400 days old, so under the default
// 12-month monthly window the producer placement falls outside it and only
// the discovery placement (this manager's own clock, today) can select the
// monthly tier: MONTHLY(discovery). Widening that window to 24 months with
// -monthly-months brings the producer placement back inside it, and the
// same line has to read MONTHLY(both).
func TestRun_RetentionScalarFlagsReachTheSetsResolvedPolicy(t *testing.T) {
	configPath := writeTestConfig(t)
	stale := time.Now().AddDate(0, 0, -400)
	remote := filepath.Join(filepath.Dir(configPath), "remote", "backup.dump")
	if err := os.Chtimes(remote, stale, stale); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run: %d, want 0", got)
	}

	base := captureStdout(t, func() {
		if got := run([]string{"retention", "--config", configPath, "--dry-run"}); got != 0 {
			t.Fatalf("retention: %d, want 0", got)
		}
	})
	if !strings.Contains(base, "MONTHLY(discovery)") {
		t.Fatalf("the un-overridden preview does not select monthly by discovery alone, so widening the window proves nothing:\n%s", base)
	}

	out := captureStdout(t, func() {
		if got := run([]string{"retention", "--config", configPath, "--dry-run",
			"-monthly-months", "24"}); got != 0 {
			t.Fatalf("retention -monthly-months: %d, want 0", got)
		}
	})
	if !strings.Contains(out, "MONTHLY(both)") {
		t.Errorf("the -monthly-months override never reached the set's resolved policy: the monthly tier is still selected by discovery alone.\ngot:\n%s", out)
	}
}

// TestRun_RetentionFlagsDoNotRewriteASetsOwnPolicy is the deliberate half
// of the decision: these flags override the DEPLOYMENT's policy. A set
// that declares its own retention block wrote that against that set, not
// against this invocation, so a CLI flag aimed at the deployment must not
// reach through it.
func TestRun_RetentionFlagsDoNotRewriteASetsOwnPolicy(t *testing.T) {
	configPath := writeTwoSetTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run: %d, want 0", got)
	}

	out := captureStdout(t, func() {
		if got := run([]string{"retention", "--config", configPath, "--dry-run",
			"-tier", "cli_only:year:1"}); got != 0 {
			t.Fatalf("retention -tier: %d, want 0", got)
		}
	})

	inheriting := regexp.MustCompile(`production/inherits:[^\n]*\n\s+KEEP\s+backup\.dump\s+tiers=\[CLI_ONLY\(both\) LAST_KNOWN_GOOD\]`)
	if !inheriting.MatchString(out) {
		t.Errorf("the inheriting set did not follow the CLI override.\nwant a line matching %s\ngot:\n%s", inheriting, out)
	}

	own := regexp.MustCompile(`production/owns:[^\n]*\n\s+KEEP\s+backup\.dump\s+tiers=\[SET_OWN\(both\) LAST_KNOWN_GOOD\]`)
	if !own.MatchString(out) {
		t.Errorf("a CLI flag reached through a set that declares its own retention policy.\nwant a line matching %s\ngot:\n%s", own, out)
	}
}

// TestRun_RetentionNamesTheOverridingSetsChain covers the attribution
// line's own format string, which had no test anywhere: "this set's own
// policy" says where to go and edit, and the chain says what is there.
func TestRun_RetentionNamesTheOverridingSetsChain(t *testing.T) {
	configPath := writeTwoSetTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run: %d, want 0", got)
	}

	out := captureStdout(t, func() {
		if got := run([]string{"retention", "--config", configPath, "--dry-run"}); got != 0 {
			t.Fatalf("retention: %d, want 0", got)
		}
	})

	const want = "production/owns: (retained under this set's own policy: tiers=[set_own/1] timezone=UTC)"
	if !strings.Contains(out, want) {
		t.Errorf("the override attribution line does not name the policy.\nwant a line reading %q\ngot:\n%s", want, out)
	}
	// The inheriting set is the pinned case, and it has to stay exactly
	// what it was before any of this existed.
	if !strings.Contains(out, "production/inherits:\n") {
		t.Errorf("an inheriting set's header line changed shape, which moves the pinned CLI contract cases:\n%s", out)
	}
}
