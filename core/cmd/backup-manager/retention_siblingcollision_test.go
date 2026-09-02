package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// Issue #292's own reproduction: a producer (the issue's example is a
// Gitea backup host) writes one restore point as two files sharing the
// same run timestamp -- a portable archive and a database dump. GFS picks
// one of them as its bucket's representative and the other comes back
// tiers=[], which prints identically to a genuinely superseded artifact.
// This drives the real `retention --dry-run` command, over a real local
// backend fetch, the same way retention_attribution_test.go's
// TestRun_RetentionLineSaysWhichPlacementSelectedEachTier does, and reads
// what it actually printed.

// siblingRunFixtureTimestamp is the shared run instant both files in this
// fixture carry as their on-disk modification time. internal/discovery
// captures a producer timestamp at whole-second (Unix) resolution (see
// discovery.go), so both files need only agree to the second, not the
// nanosecond, for GFS's producer placement to tie them.
var siblingRunFixtureTimestamp = time.Date(2026, 8, 30, 3, 30, 0, 0, time.UTC)

// writeSiblingRunConfig builds a config whose one backup set's remote
// directory holds two files sharing siblingRunFixtureTimestamp as their
// mtime, both matched by one `include` pattern -- exactly the "producer
// writes several files per run" scenario the issue describes, and
// exactly the scenario docs/EPIC.md's FR-18 "Multi-file restore points"
// section says to instead split into one backup set per file pattern.
func writeSiblingRunConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	names := []string{
		"gitea-dump-20260830T033000Z.tar.gz",
		"gitea-db-20260830T033000Z.dump",
	}
	for _, name := range names {
		p := filepath.Join(remoteDir, name)
		if err := os.WriteFile(p, []byte("payload for "+name), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := os.Chtimes(p, siblingRunFixtureTimestamp, siblingRunFixtureTimestamp); err != nil {
			t.Fatalf("Chtimes %s: %v", p, err)
		}
	}

	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "state.db")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + dbPath + "\n" +
		"sources:\n" +
		"  - id: cicd-pipeline\n" +
		"    backup_sets:\n" +
		"      - id: gitea-forge\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + localDir + "\n" +
		"        include:\n" +
		"          - \"gitea-*\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// retentionSiblingCollisionLine matches this issue's own new indented
// warning line (see GFSVerdict.SiblingCollisionLines and this command's
// print loop) right under the losing artifact's tiers=[] verdict.
var retentionSiblingCollisionLine = regexp.MustCompile(
	`\n\s+DELETE\s+gitea-db-20260830T033000Z\.dump\s+tiers=\[\]\n\s+! sibling collision: gitea-db-20260830T033000Z\.dump shares an identical timestamp with gitea-dump-20260830T033000Z\.tar\.gz`)

// TestRun_RetentionDryRunFlagsASiblingCollision is issue #292's own
// acceptance criterion, end to end: `retention --dry-run` must not let
// this split through as a bare, indistinguishable tiers=[].
func TestRun_RetentionDryRunFlagsASiblingCollision(t *testing.T) {
	configPath := writeSiblingRunConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run: %d, want 0", got)
	}

	out := captureStdout(t, func() {
		if got := run([]string{"retention", "--config", configPath, "--dry-run"}); got != 0 {
			t.Fatalf("retention --dry-run: %d, want 0", got)
		}
	})

	if !retentionSiblingCollisionLine.MatchString(out) {
		t.Errorf("the losing sibling's DELETE line does not carry a sibling-collision warning.\nwant a line matching %s\ngot:\n%s", retentionSiblingCollisionLine, out)
	}

	// The winner survived the tie-break and must print exactly as an
	// ordinary KEEP always has: no collision line under it. This is the
	// positive control -- without it, a change that stamped the warning
	// onto every verdict in the report would also pass the assertion
	// above.
	winnerNoCollision := regexp.MustCompile(`\n\s+KEEP\s+gitea-dump-20260830T033000Z\.tar\.gz\s+tiers=[^\n]*\n\s+! `)
	if winnerNoCollision.MatchString(out) {
		t.Errorf("the tie-break winner must not carry a sibling-collision warning of its own:\n%s", out)
	}
}
