package main

import (
	"strings"
	"testing"
)

// `backup-manager backup-set edit-hold`, against a real route.
//
// The three edit-hold routes have existed since #350 and nothing outside
// a browser could reach one, so a hold taken by accident could be given
// back from exactly one place. What is tested here is that the CLI reads
// and changes the ENGINE's own hold rather than an idea of one: the fake
// engine serves these off the real BackupService, so a release performed
// through this command is the release core/service performs.

// oneServedDeployment is one configuration file with an engine serving
// it, which is the arrangement a real CLI and a real engine are in.
//
// One file rather than two, deliberately: a read compares config revisions
// and refuses when they differ, and two files naming different paths hash
// differently. That refusal is correct and it is not what these cases are
// about, so a fixture that drove it would test the refusal three times and
// the verb never.
func oneServedDeployment(t *testing.T) (string, *fakeEngine) {
	t.Helper()
	configPath := writeTestConfig(t)
	e := startFakeEngine(t, configPath)
	e.attach(t)
	return configPath, e
}

func TestBackupSetEditHold_ReportsAHoldTheEngineIsHolding(t *testing.T) {
	cliConfig, e := oneServedDeployment(t)

	// Taken through the service the engine is serving, which is the only
	// way a hold exists at all: the registry is in that process's memory.
	if _, err := e.svc.BeginBackupSetEdit(t.Context(), "production/postgres-primary"); err != nil {
		t.Fatalf("taking the hold on the engine: %v", err)
	}

	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{"backup-set", "edit-hold", "production/postgres-primary", "--config", cliConfig})
	})
	if code != 0 {
		t.Fatalf("backup-set edit-hold = %d, want 0; it printed %q", code, stdout)
	}
	if !strings.Contains(stdout, "held: true") {
		t.Errorf("the report does not say the set is held:\n%s", stdout)
	}
	if !strings.Contains(stdout, "expires_at:") {
		t.Errorf("the report does not say when the lease expires, which is the whole reason a hold is a lease:\n%s", stdout)
	}
	if !strings.Contains(stdout, "production/postgres-primary") {
		t.Errorf("the report does not name the backup set it is about:\n%s", stdout)
	}
}

func TestBackupSetEditHold_ReleaseGivesTheEnginesOwnHoldBack(t *testing.T) {
	cliConfig, e := oneServedDeployment(t)

	if _, err := e.svc.BeginBackupSetEdit(t.Context(), "production/postgres-primary"); err != nil {
		t.Fatalf("taking the hold on the engine: %v", err)
	}

	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{"backup-set", "edit-hold", "production/postgres-primary", "--release", "--config", cliConfig})
	})
	if code != 0 {
		t.Fatalf("backup-set edit-hold --release = %d, want 0; it printed %q", code, stdout)
	}
	if !strings.Contains(stdout, "held: false") {
		t.Errorf("the report after a release still says the set is held:\n%s", stdout)
	}

	// Asked of the ENGINE, not of the printed report: a command that
	// printed "held: false" from its own optimism would pass every
	// assertion above while the set stayed paused.
	state, err := e.svc.BackupSetEditState(t.Context(), "production/postgres-primary")
	if err != nil {
		t.Fatalf("reading the hold back off the engine: %v", err)
	}
	if state.Held {
		t.Error("the engine is still holding this backup set, so the release reached nothing and the set stays paused until the lease lapses")
	}
}

func TestBackupSetEditHold_AnUnheldSetSaysSoRatherThanFailing(t *testing.T) {
	cliConfig, _ := oneServedDeployment(t)

	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{"backup-set", "edit-hold", "production/postgres-primary", "--config", cliConfig})
	})
	if code != 0 {
		t.Fatalf("backup-set edit-hold on an unheld set = %d, want 0; it printed %q", code, stdout)
	}
	if !strings.Contains(stdout, "held: false") {
		t.Errorf("an unheld set is not reported as unheld:\n%s", stdout)
	}
	// The contract makes `running` null when no cycle is inside the set,
	// and an empty artifact with an empty stage would read as a pass that
	// is running and will not say what it is doing.
	if !strings.Contains(stdout, "running: nothing") {
		t.Errorf("a set with no cycle inside it does not say so:\n%s", stdout)
	}
}

// TestBackupSetEditHold_RefusesWithNoRouteRatherThanAnsweringFromNowhere
// is the claim that keeps this honest.
//
// A hold lives in the memory of the process serving this deployment and
// does not survive it, so with nothing serving there is no hold and no
// file that could be consulted. Answering "not held" from a world where
// the question has no meaning is worse than refusing, because it looks
// like an answer.
func TestBackupSetEditHold_RefusesWithNoRouteRatherThanAnsweringFromNowhere(t *testing.T) {
	configPath := writeTestConfig(t)

	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{"backup-set", "edit-hold", "production/postgres-primary", "--config", configPath})
	})
	if code == 0 {
		t.Fatalf("backup-set edit-hold with no engine to reach exited 0, so it answered from somewhere: %q", stdout)
	}
	if strings.Contains(stdout, "held:") {
		t.Errorf("it printed a hold state with no engine to read one from:\n%s", stdout)
	}
}

func TestBackupSetEditHold_RefusesAnIdThatIsNotOne(t *testing.T) {
	configPath := writeTestConfig(t)

	if got := run([]string{"backup-set", "edit-hold", "postgres-primary", "--config", configPath}); got != 2 {
		t.Errorf("backup-set edit-hold with a bare set name = %d, want 2: a backup set id is exactly source/name, and that is wrong on every deployment rather than on this one", got)
	}
	if got := run([]string{"backup-set", "edit-hold", "--config", configPath}); got != 2 {
		t.Errorf("backup-set edit-hold with no id = %d, want 2", got)
	}
}

func TestBackupSetEditHold_IsRegisteredAndDiscoverable(t *testing.T) {
	if _, ok := backupSetVerbs["edit-hold"]; !ok {
		t.Fatal("`edit-hold` is not in backupSetVerbs, so nothing dispatches it")
	}
	text := captureStderr(t, usage)
	if !strings.Contains(text, "  backup-set edit-hold ") {
		t.Errorf("usage() has no entry line for `backup-set edit-hold`, so the only reference an operator has does not mention it:\n%s", text)
	}
}
