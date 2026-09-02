package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/app"
)

// --- fixtures ---
//
// Each of these writes a real config file driving the real local backend,
// so every test below goes through the same `run`/`fetch` an operator
// types, not through a hand-built report.

type cycleConfig struct {
	path      string
	dir       string
	remoteDir string
	localDir  string
}

// writeCycleConfig is main_test.go's writeTestConfig with two knobs issue
// #361 needs: whether the remote has anything on it at all, and an extra
// block of YAML to append. Duplicated rather than parameterised in place
// so this file stays self-contained.
func writeCycleConfig(t *testing.T, seedRemote bool, extra string) cycleConfig {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if seedRemote {
		if err := os.WriteFile(filepath.Join(remoteDir, "backup.dump"), []byte("cycle outcome payload"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "state.db")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + dbPath + "\n" +
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
		extra
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return cycleConfig{path: configPath, dir: dir, remoteDir: remoteDir, localDir: localDir}
}

// capacityRefusesEverything is a storage ceiling of one byte. Every
// artifact this project exists to move is bigger than that, so
// internal/capacity refuses each transfer before it starts and the
// journal row is left exactly where it was: no FAILED, no QUARANTINED,
// nothing this cycle's older accounting could see. That is issue #361's
// shape reproduced through a config file rather than a fault injector.
const capacityRefusesEverything = "capacity:\n  cap_bytes: 1\n"

// --- end to end, through the commands an operator actually runs ---

// TestRun_ExitsNonZeroWhenNothingGotThrough is issue #361's first
// acceptance criterion: an artifact was waiting, the cycle tried to move
// it, nothing moved, and the exit status has to say so.
func TestRun_ExitsNonZeroWhenNothingGotThrough(t *testing.T) {
	cfg := writeCycleConfig(t, true, capacityRefusesEverything)

	var got int
	out := captureStdout(t, func() {
		got = run([]string{"run", "--config", cfg.path})
	})

	if got == 0 {
		t.Errorf("run exit code = 0, want non-zero: one artifact was waiting on the remote, the cycle refused to transfer it, and nothing was backed up.\nstdout:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(cfg.localDir, "backup.dump")); err == nil {
		t.Fatalf("precondition: %s exists; this test only means anything if nothing was actually backed up", filepath.Join(cfg.localDir, "backup.dump"))
	}
	if !strings.Contains(out, "1 walked") || !strings.Contains(out, "0 got through") {
		t.Errorf("run's non-zero exit does not name how many artifacts were walked and how many got through.\nstdout:\n%s", out)
	}
}

// TestRun_FetchExitsNonZeroWhenNothingGotThrough is issue #361's fourth
// acceptance criterion: `fetch` reaches the same verdict as `run` on the
// same cycle, because both go through the same seam.
func TestRun_FetchExitsNonZeroWhenNothingGotThrough(t *testing.T) {
	cfg := writeCycleConfig(t, true, capacityRefusesEverything)

	var got int
	out := captureStdout(t, func() {
		got = run([]string{"fetch", "--config", cfg.path, "--source", "production", "--backup-set", "postgres-primary"})
	})

	if got == 0 {
		t.Errorf("fetch exit code = 0, want non-zero for the same cycle `run` fails.\nstdout:\n%s", out)
	}
	if !strings.Contains(out, "1 walked") || !strings.Contains(out, "0 got through") {
		t.Errorf("fetch's non-zero exit does not name how many artifacts were walked and how many got through.\nstdout:\n%s", out)
	}
}

// TestRun_ExitsZeroWhenNothingWasWaiting is the control that keeps the
// fix honest, and issue #361's second acceptance criterion: an empty
// remote is a cycle with nothing to do, not a cycle that failed. Without
// this, "no transfers, no success" would look like a fix and would turn
// every quiet night into a false alarm.
func TestRun_ExitsZeroWhenNothingWasWaiting(t *testing.T) {
	cfg := writeCycleConfig(t, false, capacityRefusesEverything)

	var got int
	out := captureStdout(t, func() {
		got = run([]string{"run", "--config", cfg.path})
	})

	if got != 0 {
		t.Errorf("run exit code = %d, want 0: there was nothing on the remote, so there was nothing to get through.\nstdout:\n%s", got, out)
	}
}

// TestRun_ExitsZeroOnASteadyStateCycle is the same control against a
// journal that already holds a finished artifact rather than an empty
// one: the second run has genuinely nothing left to do, and a fix that
// counted journal rows rather than pending work would fail it.
func TestRun_ExitsZeroOnASteadyStateCycle(t *testing.T) {
	cfg := writeCycleConfig(t, true, "")

	if got := run([]string{"run", "--config", cfg.path}); got != 0 {
		t.Fatalf("precondition: first run = %d, want 0 (a clean cycle)", got)
	}

	var got int
	out := captureStdout(t, func() {
		got = run([]string{"run", "--config", cfg.path})
	})
	if got != 0 {
		t.Errorf("run exit code = %d, want 0: the one artifact is already COMPLETE and the remote is empty, so this cycle had nothing to do.\nstdout:\n%s", got, out)
	}
}

// TestRun_ExitsNonZeroWhenTheJournalRecordedAFailureTheCycleDidNotSee is
// issue #361's other half, end to end. lifecycle.Transfer refuses a
// transfer whose final local name is already taken, records that refusal
// as FAILED itself, and returns the error. processArtifact's error path
// then reported the state the record carried BEFORE the step ran, so the
// cycle counted no failed artifact at all and exited 0 while its own
// journal said the only artifact in the backup set had failed.
func TestRun_ExitsNonZeroWhenTheJournalRecordedAFailureTheCycleDidNotSee(t *testing.T) {
	cfg := writeCycleConfig(t, true, "")
	if err := os.MkdirAll(cfg.localDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Something is already sitting at the artifact's final local name, so
	// the transfer refuses rather than overwriting a possible known-good
	// backup.
	if err := os.WriteFile(filepath.Join(cfg.localDir, "backup.dump"), []byte("a backup that is already here"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var got int
	out := captureStdout(t, func() {
		got = run([]string{"run", "--config", cfg.path})
	})

	if got == 0 {
		t.Errorf("run exit code = 0, want non-zero: the transfer refused and recorded FAILED, so this cycle backed up nothing.\nstdout:\n%s", out)
	}
}

// --- the seam itself ---

// TestCycleExit_Matrix drives the one function `run` and `fetch` both use
// to turn a cycle into an exit status. Building the verdicts by hand is
// what lets this cover the combinations no single config file can
// produce at once, including a multi-set cycle where one set is fine and
// another got nothing through.
func TestCycleExit_Matrix(t *testing.T) {
	barren := app.CycleVerdict{Set: "production/postgres-primary", Progress: app.CycleProgress{Walked: 3}}
	partial := app.CycleVerdict{Set: "production/postgres-primary", Progress: app.CycleProgress{Walked: 3, Advanced: 2}}
	quiet := app.CycleVerdict{Set: "production/postgres-primary"}

	cases := []struct {
		name     string
		verdicts []app.CycleVerdict
		want     int
		wantSays string
	}{
		{
			name:     "nothing got through",
			verdicts: []app.CycleVerdict{barren},
			want:     1,
			wantSays: "3 walked, 0 got through",
		},
		{
			name:     "nothing was waiting",
			verdicts: []app.CycleVerdict{quiet},
			want:     0,
		},
		{
			name:     "some got through and one is for next time",
			verdicts: []app.CycleVerdict{partial},
			want:     0,
		},
		{
			name:     "an artifact ended failed",
			verdicts: []app.CycleVerdict{{Set: "production/postgres-primary", FailedArtifacts: 1, Progress: app.CycleProgress{Walked: 2, Advanced: 1}}},
			want:     1,
		},
		{
			name:     "a systemic failure",
			verdicts: []app.CycleVerdict{{Set: "production/postgres-primary", Systemic: true}},
			want:     1,
		},
		{
			name:     "reconciliation could not reach a verdict",
			verdicts: []app.CycleVerdict{{Set: "production/postgres-primary", ReconcileErrors: 1}},
			want:     1,
		},
		{
			name:     "one healthy set and one that got nothing through",
			verdicts: []app.CycleVerdict{partial, {Set: "production/redis", Progress: app.CycleProgress{Walked: 2}}},
			want:     1,
			wantSays: "production/redis",
		},
		{
			name:     "every set quiet",
			verdicts: []app.CycleVerdict{quiet, {Set: "production/redis"}},
			want:     0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got := cycleExit(&out, tc.verdicts...)
			if got != tc.want {
				t.Errorf("cycleExit = %d, want %d (verdicts %+v)", got, tc.want, tc.verdicts)
			}
			if tc.wantSays != "" && !strings.Contains(out.String(), tc.wantSays) {
				t.Errorf("cycleExit printed %q, want it to contain %q", out.String(), tc.wantSays)
			}
			if tc.want == 0 && out.Len() != 0 {
				t.Errorf("cycleExit printed %q for a cycle that did not fail; a healthy cycle must say nothing", out.String())
			}
		})
	}
}
