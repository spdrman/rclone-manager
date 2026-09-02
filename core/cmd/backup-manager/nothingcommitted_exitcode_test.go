package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeCappedConfig writes a config identical in shape to writeTestConfig
// (main_test.go) with two knobs this file needs: which files exist on the
// remote, and a storage cap.
//
// The cap is how these tests reproduce issue #361's shape through the real
// binary rather than through a double. FR-21's admission check refuses a
// transfer it can see will not fit, leaving the artifact exactly where it
// was for a later cycle to pick up: a per-artifact refusal that is not a
// systemic error and does not put the artifact in FAILED, which is the
// only combination that used to make `run` claim success while backing
// nothing up. Nothing here is contrived about the operator's situation
// either, "the allowance is full so nothing is getting through" is one of
// the ways a real deployment stops backing up.
func writeCappedConfig(t *testing.T, capBytes int, remoteFiles map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for name, content := range remoteFiles {
		if err := os.WriteFile(filepath.Join(remoteDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}

	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "state.db")
	capacityBlock := ""
	if capBytes > 0 {
		capacityBlock = "capacity:\n  cap_bytes: " + strconv.Itoa(capBytes) + "\n"
	}
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + dbPath + "\n" +
		capacityBlock +
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
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// TestRun_ExitsNonZeroWhenTheCycleBackedNothingUp is issue #361's first
// acceptance criterion, driven through the real binary: one artifact
// waiting, nothing able to get through, nothing on disk, and an exit
// status a cron job reads as success.
func TestRun_ExitsNonZeroWhenTheCycleBackedNothingUp(t *testing.T) {
	configPath := writeCappedConfig(t, 1, map[string]string{"backup.dump": "a payload that will never fit"})

	var got int
	errOut := captureStderr(t, func() {
		got = run([]string{"run", "--config", configPath})
	})

	if got == 0 {
		t.Errorf("run exit code = 0, want non-zero: one artifact was waiting, none got through, and nothing landed on disk.\nstderr:\n%s", errOut)
	}

	localDir := filepath.Join(filepath.Dir(configPath), "local")
	if entries, err := os.ReadDir(localDir); err == nil {
		for _, e := range entries {
			t.Errorf("local destination holds %q; this cycle backed nothing up", e.Name())
		}
	}
}

// TestRun_NamesWhatDidAndDidNotGetThrough is #361's fifth acceptance
// criterion: the reason has to be printed, not only encoded in the exit
// status, and it has to carry the two numbers that make the difference
// between "nothing was waiting" and "nothing got through".
//
// It goes to stderr on purpose. This binary's stdout is the FR-23
// newline-delimited JSON event stream (see setup.go's logger), and a
// sentence in the middle of it would break every consumer that parses it.
func TestRun_NamesWhatDidAndDidNotGetThrough(t *testing.T) {
	configPath := writeCappedConfig(t, 1, map[string]string{"backup.dump": "a payload that will never fit"})

	errOut := captureStderr(t, func() {
		run([]string{"run", "--config", configPath})
	})

	for _, want := range []string{"1 artifact", "0"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr does not contain %q, so it never says how many artifacts were walked and how many got through.\nstderr:\n%s", want, errOut)
		}
	}
	if !strings.Contains(errOut, "production/postgres-primary") {
		t.Errorf("stderr does not name the backup set that backed nothing up.\nstderr:\n%s", errOut)
	}
}

// TestRun_ExitsZeroWhenThereWasNothingToDo is #361's third
// Given/When/Then, and the one that stops this whole change from becoming
// a pager that fires on every poll interval: an empty remote is not a
// failed cycle.
func TestRun_ExitsZeroWhenThereWasNothingToDo(t *testing.T) {
	configPath := writeCappedConfig(t, 0, nil)

	var got int
	errOut := captureStderr(t, func() {
		got = run([]string{"run", "--config", configPath})
	})
	if got != 0 {
		t.Errorf("run exit code = %d, want 0: there was nothing waiting on the remote, which is not the same thing as nothing getting through.\nstderr:\n%s", got, errOut)
	}
}

// TestRun_AnIdleCycleExitsZeroEvenWhenStatusCallsTheSetDegraded pins the
// one place `run` and `status` deliberately answer differently, so nobody
// later reads it as the same bug #361 was about.
//
// They are answering two different questions. `run`'s exit code is about
// the cycle that just ran: did this pass do its job. `status`'s is about
// the backup set's standing: is there a fresh known-good backup right
// now. A backup set nobody has ever put anything into has no known-good
// backup, so status is right to call it DEGRADED, and the cycle that just
// found nothing waiting on the remote did exactly what it was asked, so
// run is right to exit 0. The failure #361 reported is the opposite
// direction and is the one that must never happen: run claiming success
// for a cycle that got nothing through.
func TestRun_AnIdleCycleExitsZeroEvenWhenStatusCallsTheSetDegraded(t *testing.T) {
	configPath := writeCappedConfig(t, 0, nil)

	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Errorf("run exit code = %d, want 0 for a cycle with nothing to do", got)
	}
	captureStdout(t, func() {
		if got := run([]string{"status", "--config", configPath}); got == 0 {
			t.Error("status exit code = 0, want non-zero: this backup set has never had a known-good backup")
		}
	})
}

// TestRun_ExitsNonZeroAndStatusAgreesWhenNothingGotThrough is the check
// the other way round: on the shape #361 actually reported, the exit code
// and the health surface have to reach the same verdict. An exit code
// that says failure while status says healthy is the same bug in
// different clothes.
func TestRun_ExitsNonZeroAndStatusAgreesWhenNothingGotThrough(t *testing.T) {
	configPath := writeCappedConfig(t, 1, map[string]string{"backup.dump": "a payload that will never fit"})

	if got := run([]string{"run", "--config", configPath}); got == 0 {
		t.Fatal("run exit code = 0, want non-zero")
	}
	captureStdout(t, func() {
		if got := run([]string{"status", "--config", configPath}); got == 0 {
			t.Error("status exit code = 0 while run reported a failed cycle; the two surfaces disagree")
		}
	})
}

// TestRun_ExitsZeroWhenSomeArtifactsGotThroughAndOneDidNot is #361's
// second Given/When/Then. The cap admits the small artifact and then
// refuses the large one, so the pass did real work and left one artifact
// for next time. That is an ordinary cycle, not a failed one, and this is
// the case the whole "a transient error should not fail a cycle" half of
// the design exists to protect.
func TestRun_ExitsZeroWhenSomeArtifactsGotThroughAndOneDidNot(t *testing.T) {
	configPath := writeCappedConfig(t, 100, map[string]string{
		"a-small.dump": "tiny",
		"b-large.dump": strings.Repeat("x", 500),
	})

	var got int
	errOut := captureStderr(t, func() {
		got = run([]string{"run", "--config", configPath})
	})
	if got != 0 {
		t.Errorf("run exit code = %d, want 0: one artifact got all the way through, so this cycle did real work.\nstderr:\n%s", got, errOut)
	}

	localFinal := filepath.Join(filepath.Dir(configPath), "local", "a-small.dump")
	if _, err := os.Stat(localFinal); err != nil {
		t.Fatalf("precondition: %s should have been backed up: %v", localFinal, err)
	}
}

// TestRun_FetchAgreesWithRunWhenNothingGotThrough is #361's fourth
// acceptance criterion. `fetch` runs one backup set's share of the same
// cycle, so it reaches the same verdict through the same seam, and its
// summary line carries the same two numbers.
func TestRun_FetchAgreesWithRunWhenNothingGotThrough(t *testing.T) {
	fetchConfig := writeCappedConfig(t, 1, map[string]string{"backup.dump": "a payload that will never fit"})
	runConfig := writeCappedConfig(t, 1, map[string]string{"backup.dump": "a payload that will never fit"})

	var fetchExit int
	out := captureStdout(t, func() {
		fetchExit = run([]string{"fetch", "--config", fetchConfig, "--source", "production", "--backup-set", "postgres-primary"})
	})
	runExit := run([]string{"run", "--config", runConfig})

	if fetchExit == 0 || runExit == 0 {
		t.Fatalf("fetch and run must both fail this cycle: fetch=%d run=%d, want both non-zero.\nfetch stdout:\n%s", fetchExit, runExit, out)
	}
	if !strings.Contains(out, "walked=1") || !strings.Contains(out, "through=0") {
		t.Errorf("fetch's summary line should carry walked= and through= counts.\nstdout:\n%s", out)
	}
}

// TestRun_FetchExitsZeroWhenSomethingGotThrough is the same protection
// for `fetch` that TestRun_ExitsZeroWhenSomeArtifactsGotThroughAndOneDidNot
// gives `run`: a partial pass is still a pass.
func TestRun_FetchExitsZeroWhenSomethingGotThrough(t *testing.T) {
	configPath := writeCappedConfig(t, 100, map[string]string{
		"a-small.dump": "tiny",
		"b-large.dump": strings.Repeat("x", 500),
	})

	var got int
	out := captureStdout(t, func() {
		got = run([]string{"fetch", "--config", configPath, "--source", "production", "--backup-set", "postgres-primary"})
	})
	if got != 0 {
		t.Errorf("fetch exit code = %d, want 0: one artifact got all the way through.\nstdout:\n%s", got, out)
	}
	if !strings.Contains(out, "through=1") {
		t.Errorf("fetch's summary line should report through=1.\nstdout:\n%s", out)
	}
}

// TestRun_ExitsNonZeroWhenTheOnlyArtifactCouldNotBeTransferred is issue
// #361's first acceptance criterion in its "failed to transfer" wording,
// and the literal shape the issue was reported in: discovery works, the
// transfer does not, and nothing lands.
//
// The refusal here is FR-12's final-name collision rather than a refused
// SSH connection, because it is the one a local-backend test can produce
// on demand. Mechanically it is the same case: a transfer step that
// durably records FAILED and then returns the error describing it, which
// is exactly the combination that used to leave the cycle reporting no
// failed artifacts at all (see processArtifact's "Return value" doc). The
// retry-exhaustion form of it, with a real transient connection refusal,
// is driven in internal/app's own
// TestRunCycle_ATransferThatExhaustsItsRetriesIsCountedFailedTheSameCycle.
func TestRun_ExitsNonZeroWhenTheOnlyArtifactCouldNotBeTransferred(t *testing.T) {
	configPath := writeCappedConfig(t, 0, map[string]string{"backup.dump": "the payload on the remote"})

	// Something already sitting at the artifact's final local name, which
	// FR-12 refuses to overwrite: it could be a known-good backup.
	localDir := filepath.Join(filepath.Dir(configPath), "local")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "backup.dump"), []byte("something already here"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var got int
	errOut := captureStderr(t, func() {
		got = run([]string{"run", "--config", configPath})
	})
	if got == 0 {
		t.Errorf("run exit code = 0, want non-zero: the one artifact this cycle discovered could not be transferred.\nstderr:\n%s", errOut)
	}
	if !strings.Contains(errOut, "1 left in a failure state") {
		t.Errorf("stderr does not report the artifact as left in a failure state, which is what the journal now says about it.\nstderr:\n%s", errOut)
	}
}

// TestRun_ASystemicFailureNamesTheErrorRatherThanPrintingZeroes keeps the
// failure report honest for the one shape whose counts say nothing. A
// cycle that stops before its pipeline runs walked nothing and delivered
// nothing, so "0 artifacts walked, 0 through" reads exactly like an idle
// cycle and would be worse than silence. The error is what happened, so
// the error is what gets printed.
func TestRun_ASystemicFailureNamesTheErrorRatherThanPrintingZeroes(t *testing.T) {
	configPath := writeCappedConfig(t, 0, nil)
	// Point the backup set at a remote directory that does not exist, so
	// the listing itself fails rather than any one artifact.
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	missing := filepath.Join(filepath.Dir(configPath), "remote", "gone")
	rewritten := strings.Replace(string(body),
		"remote_path: "+filepath.Join(filepath.Dir(configPath), "remote")+"\n",
		"remote_path: "+missing+"\n", 1)
	if rewritten == string(body) {
		t.Fatal("the fixture's remote_path line did not match, so this test never changed anything")
	}
	if err := os.WriteFile(configPath, []byte(rewritten), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var got int
	errOut := captureStderr(t, func() {
		got = run([]string{"run", "--config", configPath})
	})
	if got == 0 {
		t.Fatalf("run exit code = 0, want non-zero for a backup set whose remote could not be listed.\nstderr:\n%s", errOut)
	}
	if !strings.Contains(errOut, "stopped early") {
		t.Errorf("stderr does not say this cycle stopped early.\nstderr:\n%s", errOut)
	}
	if strings.Contains(errOut, "0 artifacts walked") {
		t.Errorf("stderr reports zero counts for a cycle that never reached its pipeline; that reads like an idle cycle.\nstderr:\n%s", errOut)
	}
}
