package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/retention"
)

// FR-27/FR-30 (issue #239) on the surface FR-20 calls the mandatory
// dry-run: `backup-manager retention` has to show every MOVE it would
// make, not only every deletion, before anything runs.
//
// Both tests drive the real command over a real local-backend fetch, the
// same way TestRun_RetentionLineSaysWhichPlacementSelectedEachTier does,
// and read what it actually printed.

// writeOffsiteTestConfig is writeTestConfig with one storage medium
// declared and a chain whose single daily tier lives on it.
//
// A whole chain on one medium is what makes this testable through a real
// fetch: the artifact this command sees was discovered a moment ago, so
// the only tier that can select it is the daily one, and putting the
// medium there is what gives a freshly-ingested artifact a home that is
// not where it currently is.
func writeOffsiteTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "backup.dump"), []byte("cli placement payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"storage_mediums:\n" +
		"  - id: cold_offsite\n" +
		"    type: s3\n" +
		"    region: us-east-1\n" +
		"    bucket: nas-backups\n" +
		"    prefix: rclone-manager\n" +
		"    credentials:\n" +
		"      env: BACKUP_S3_COLD\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + localDir + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n" +
		"  tiers:\n" +
		"    - name: daily\n" +
		"      granularity: day\n" +
		"      keep: 7\n" +
		"      medium: cold_offsite\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// TestRun_RetentionNamesTheMoveItWouldMake is AC2 on this surface. The
// operator has said, by writing a medium onto the daily tier, that daily
// backups live offsite. The artifact is on local. The dry-run has to say
// so before a cycle carries it there.
func TestRun_RetentionNamesTheMoveItWouldMake(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run: %d, want 0", got)
	}

	out := captureStdout(t, func() {
		if got := run([]string{"retention", "--config", configPath, "--dry-run"}); got != 0 {
			t.Fatalf("retention --dry-run: %d, want 0", got)
		}
	})

	if !strings.Contains(out, "MOVE") {
		t.Fatalf("the dry-run names no move at all.\ngot:\n%s", out)
	}
	for _, want := range []string{"backup.dump", "local", "cold_offsite"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry-run does not mention %q; an operator has to be told what moves and where before it happens.\ngot:\n%s", want, out)
		}
	}
}

// TestRun_RetentionSaysNothingAboutPlacementWithNoMediumDeclared is the
// compatibility half, and it is the one that protects something outside
// this repository.
//
// This command's per-artifact output is pinned by the black-box contract
// suite in spdrman/rclone-manager-tests (suites/cli/cases/retention/), and
// every case there is a medium-free deployment. A deployment with nowhere
// else to put anything has nothing to say about placement, so the section
// is not printed at all, and those cases stay byte-identical. Printing an
// empty "placement:" heading would be a cross-repo lockstep change for a
// line that carries no information.
func TestRun_RetentionSaysNothingAboutPlacementWithNoMediumDeclared(t *testing.T) {
	configPath := writeTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run: %d, want 0", got)
	}

	out := captureStdout(t, func() {
		if got := run([]string{"retention", "--config", configPath, "--dry-run"}); got != 0 {
			t.Fatalf("retention --dry-run: %d, want 0", got)
		}
	})

	for _, unwanted := range []string{"placement:", "MOVE", "could not confirm"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("a medium-free deployment printed %q; this output is pinned by a contract suite in another repository and every case there is medium-free.\ngot:\n%s", unwanted, out)
		}
	}
}

// TestPrintPlacementPlan_SaysNothingWithoutAMediumEvenWithAPlanToShow is
// the medium-free guard, stated where it is decidable.
//
// The end-to-end test above cannot prove it. A cycle that ingests an
// artifact writes an ACTIVE local placement for it, so a medium-free run
// has an empty plan anyway and the empty-plan guard alone would keep the
// output clean: removing the medium-free guard leaves that test green,
// which I checked before writing this one.
//
// What the guard is actually for is a journal row with no ACTIVE
// placement in a deployment that has nowhere else to put anything: an
// artifact whose local placement went GONE, or a row written by something
// other than a completed ingestion. Without the guard, every one of those
// prints a "nothing could confirm where its durable copy is" line into
// output that is pinned, in another repository, on medium-free cases.
func TestPrintPlacementPlan_SaysNothingWithoutAMediumEvenWithAPlanToShow(t *testing.T) {
	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, "unplaced.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	report := app.RetentionSetReport{
		Set: set,
		HomePlan: retention.HomePlan{
			Unconfirmed: []model.ArtifactID{artifact},
			Moves:       []retention.HomeMove{{Artifact: artifact, From: "local", To: "cold_offsite"}},
		},
	}

	out := captureStdout(t, func() {
		printPlacementPlan(&config.Config{}, report)
	})
	if out != "" {
		t.Errorf("a deployment with no storage medium printed a placement section:\n%s", out)
	}

	// The control. Without it this would pass against a function that
	// prints nothing at all, ever.
	out = captureStdout(t, func() {
		printPlacementPlan(&config.Config{StorageMediums: []config.StorageMedium{{ID: "cold_offsite"}}}, report)
	})
	if !strings.Contains(out, "unplaced.dump") {
		t.Errorf("a deployment that DOES declare a medium printed no placement section:\n%s", out)
	}
}
