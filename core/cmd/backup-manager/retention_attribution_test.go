package main

import (
	"regexp"
	"testing"
)

// Issue #218. `retention` itemises one line per artifact, and since #215
// a KEEP on that line can have come from either of FR-18's two
// placements: the timestamp this manager discovered the artifact, or the
// producer's own timestamp on the remote object. Those have different
// trust properties (FR-8 distrusts the second), so a tier name on its own
// is only half of what an operator needs to answer "why is this kept".
//
// This test drives the real command over a real local-backend fetch, the
// same way TestRun_RunCommandProcessesAnArtifactEndToEnd does, and reads
// what it actually printed. The expectation is derived, not recorded: one
// artifact in a backup set is the only candidate in every bucket it lands
// in, under both placements, so every GFS tier that keeps it must be
// attributed to BOTH passes. FR-19's protection is not a placement and so
// carries no attribution at all.
var retentionKeepLine = regexp.MustCompile(
	`\n\s+KEEP\s+backup\.dump\s+tiers=\[DAILY\(both\) WEEKLY\(both\) MONTHLY\(both\) LAST_KNOWN_GOOD\]\n`)

func TestRun_RetentionLineSaysWhichPlacementSelectedEachTier(t *testing.T) {
	configPath := writeTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run: %d, want 0", got)
	}

	out := captureStdout(t, func() {
		if got := run([]string{"retention", "--config", configPath, "--dry-run"}); got != 0 {
			t.Fatalf("retention --dry-run: %d, want 0", got)
		}
	})

	if !retentionKeepLine.MatchString(out) {
		t.Errorf("the per-artifact retention line does not say which placement selected each tier.\nwant a line matching %s\ngot:\n%s", retentionKeepLine, out)
	}
}
