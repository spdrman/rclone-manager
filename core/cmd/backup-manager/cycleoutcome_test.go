// What a cycle's exit status means, driven through the real command.
//
// This is what a cron job reads and the only thing it reads, so each cell
// runs `run` or `fetch` over a real local-backend config and checks the
// status against a cycle that really happened: nothing waiting is not a
// failure, a steady state is not a failure, and nothing getting through is.
//
// The pairs are the point. `run` and `fetch` had quietly grown two different
// definitions of a failed cycle, each defensible on its own and only visible
// side by side, so the cells here ask both commands the same question about
// the same deployment and require the same answer.
package main

import (
	"bytes"
	"encoding/json"
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
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			got = run([]string{"run", "--config", cfg.path})
		})
	})

	if got == 0 {
		t.Errorf("run exit code = 0, want non-zero: one artifact was waiting on the remote, the cycle refused to transfer it, and nothing was backed up.\nstderr:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(cfg.localDir, "backup.dump")); err == nil {
		t.Fatalf("precondition: %s exists; this test only means anything if nothing was actually backed up", filepath.Join(cfg.localDir, "backup.dump"))
	}
	if !strings.Contains(stderr, "1 walked") || !strings.Contains(stderr, "0 got through") {
		t.Errorf("run's non-zero exit does not name how many artifacts were walked and how many got through.\nstderr:\n%s", stderr)
	}
	// stdout is the FR-23 event stream and nothing else: every line of it
	// has to stay parseable as JSON, or the sentence above breaks every
	// consumer reading that stream a line at a time.
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		var probe map[string]any
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Errorf("stdout carries a line that is not JSON: %q", line)
		}
	}
}

// TestRun_FetchExitsNonZeroWhenNothingGotThrough is issue #361's fourth
// acceptance criterion: `fetch` reaches the same verdict as `run` on the
// same cycle, because both go through the same seam.
func TestRun_FetchExitsNonZeroWhenNothingGotThrough(t *testing.T) {
	cfg := writeCycleConfig(t, true, capacityRefusesEverything)

	var got int
	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			got = run([]string{"fetch", "--config", cfg.path, "--source", "production", "--backup-set", "postgres-primary"})
		})
	})

	if got == 0 {
		t.Errorf("fetch exit code = 0, want non-zero for the same cycle `run` fails.\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "1 walked") || !strings.Contains(stderr, "0 got through") {
		t.Errorf("fetch's non-zero exit does not name how many artifacts were walked and how many got through.\nstderr:\n%s", stderr)
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
// issue #361's other half, end to end, and it is deliberately built so
// that only that half can make it pass.
//
// lifecycle.Transfer refuses a transfer whose final local name is already
// taken, records that refusal as FAILED itself, and returns the error.
// processArtifact's error path then reported the state the record carried
// BEFORE the step ran, so the cycle counted no failed artifact at all and
// exited 0 while its own journal said that artifact had failed.
//
// The second artifact is the whole point of the fixture. It transfers
// cleanly, so this cycle got something through and the "nothing got
// through" rule cannot fire. The only thing left that can produce a
// non-zero exit is the cycle counting the artifact its own journal says
// failed, which is exactly the reading-back this test exists to pin.
func TestRun_ExitsNonZeroWhenTheJournalRecordedAFailureTheCycleDidNotSee(t *testing.T) {
	cfg := writeCycleConfig(t, true, "")
	if err := os.WriteFile(filepath.Join(cfg.remoteDir, "second.dump"), []byte("the artifact that gets through"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.MkdirAll(cfg.localDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Something is already sitting at the first artifact's final local
	// name, so its transfer refuses rather than overwriting a possible
	// known-good backup.
	if err := os.WriteFile(filepath.Join(cfg.localDir, "backup.dump"), []byte("a backup that is already here"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var got int
	out := captureStdout(t, func() {
		got = run([]string{"run", "--config", cfg.path})
	})

	if _, err := os.Stat(filepath.Join(cfg.localDir, "second.dump")); err != nil {
		t.Fatalf("precondition: the second artifact was supposed to transfer cleanly, so that this cycle is a partial one rather than a barren one: %v", err)
	}
	if strings.Contains(out, "got nothing through") {
		t.Fatalf("precondition: this cycle got an artifact through, so the barren rule must not be what fails it.\nstdout:\n%s", out)
	}
	if got == 0 {
		t.Errorf("run exit code = 0, want non-zero: one artifact's transfer refused and the journal recorded FAILED for it.\nstdout:\n%s", out)
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
	partial := app.CycleVerdict{Set: "production/postgres-primary", Progress: app.CycleProgress{Walked: 3, Durable: 2}}
	quiet := app.CycleVerdict{Set: "production/postgres-primary"}

	cases := []struct {
		name            string
		verdicts        []app.CycleVerdict
		want            int
		wantSays        string
		wantSilentAbout string
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
			verdicts: []app.CycleVerdict{{Set: "production/postgres-primary", FailedArtifacts: 1, Progress: app.CycleProgress{Walked: 2, Durable: 1}}},
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
		{
			// The arithmetic qualifies and saying so would be wrong: this
			// cycle walked nothing because it never reached its pipeline,
			// and the failure it actually hit is already reported.
			name:            "a cycle that stopped early is not called barren",
			verdicts:        []app.CycleVerdict{{Set: "production/postgres-primary", Systemic: true, Progress: app.CycleProgress{Walked: 3}}},
			want:            1,
			wantSilentAbout: "got nothing through",
		},
		{
			// The same arithmetic with nothing wrong behind it. An
			// operator pressed Edit (issue #350), the pass stopped where
			// it stood, and its in-flight row is counted and did not
			// land. Exit 1 and "backed nothing up this cycle" would both
			// be false alarms about a deliberate act, which is the one
			// thing a backup tool cannot afford to get wrong.
			name:            "a set stopped for editing is neither failed nor barren",
			verdicts:        []app.CycleVerdict{{Set: "production/postgres-primary", Stopped: true, Progress: app.CycleProgress{Walked: 3}}},
			want:            0,
			wantSilentAbout: "got nothing through",
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
			if tc.wantSilentAbout != "" && strings.Contains(out.String(), tc.wantSilentAbout) {
				t.Errorf("cycleExit printed %q, which must not mention %q", out.String(), tc.wantSilentAbout)
			}
			if tc.want == 0 && out.Len() != 0 {
				t.Errorf("cycleExit printed %q for a cycle that did not fail; a healthy cycle must say nothing", out.String())
			}
		})
	}
}
