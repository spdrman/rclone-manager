package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Issue #598's CLI half: `backup-manager activity`.
//
// There was no verb for the lifecycle feed at all, which is worse than
// cosmetic on the issue this comes from. The one screen that says what the
// manager has been doing could not be scripted, could not be pasted into a
// support conversation, and could not be checked by a cron job, so an
// operator whose Activity page was failing had no second way to look at
// the same rows. `status` is the nearest thing and it reports per-set
// health, which says nothing about what happened.
//
// The fixture runs a real cycle first, deliberately. An empty journal
// makes a filter that selects nothing indistinguishable from a filter that
// selects the right thing, and every assertion below is about which rows
// come back.

// oneCycleDeployment writes the standard fixture and runs one real cycle
// over it, so the journal holds the transitions a discover/transfer/verify/
// commit pass records.
func oneCycleDeployment(t *testing.T) string {
	t.Helper()
	configPath := writeTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run([\"run\", \"--config\", %q]) = %d, want 0 (these tests need a journal with transitions in it)", configPath, got)
	}
	return configPath
}

func TestActivity_ListsTheTransitionsACycleRecorded(t *testing.T) {
	configPath := oneCycleDeployment(t)

	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{"activity", "--config", configPath})
	})

	if code != 0 {
		t.Fatalf("activity = %d, want 0; it printed %q", code, stdout)
	}
	// The three columns the page shows, in the order it shows them: when,
	// what state was entered, which backup set, which backup.
	if !strings.Contains(stdout, "production/postgres-primary") {
		t.Errorf("activity printed %q, want the backup set id on every row", stdout)
	}
	if !strings.Contains(stdout, "backup.dump") {
		t.Errorf("activity printed %q, want the artifact name", stdout)
	}
	if !strings.Contains(stdout, "COMMITTED") {
		t.Errorf("activity printed %q, want the COMMITTED transition a finished cycle records", stdout)
	}
	if !strings.Contains(stdout, "DISCOVERED") {
		t.Errorf("activity printed %q, want the DISCOVERED transition that starts a backup's life", stdout)
	}
}

func TestActivity_NewestFirst(t *testing.T) {
	configPath := oneCycleDeployment(t)

	stdout := captureStdout(t, func() {
		run([]string{"activity", "--config", configPath})
	})

	// DISCOVERED is the first transition an artifact makes and COMMITTED
	// comes several later, so newest-first puts COMMITTED above it. A feed
	// printed oldest-first would still contain both and pass every
	// assertion in the case above.
	committed := strings.Index(stdout, "COMMITTED")
	discovered := strings.Index(stdout, "DISCOVERED")
	if committed < 0 || discovered < 0 {
		t.Fatalf("activity printed %q, want both transitions", stdout)
	}
	if committed > discovered {
		t.Errorf("activity printed DISCOVERED above COMMITTED, so it is oldest-first:\n%s", stdout)
	}
}

func TestActivity_JSONEmitsTheWireObjectsUnchanged(t *testing.T) {
	configPath := oneCycleDeployment(t)

	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{"activity", "--config", configPath, "--json"})
	})
	if code != 0 {
		t.Fatalf("activity --json = %d, want 0; it printed %q", code, stdout)
	}

	// A script parses the contract rather than this binary's table, so
	// what comes out has to be the response shape api/v1/openapi.json
	// declares, field names and all.
	var body struct {
		Events []struct {
			ArtifactID  string `json:"artifact_id"`
			BackupSetID string `json:"backup_set_id"`
			From        string `json:"from"`
			To          string `json:"to"`
			OccurredAt  string `json:"occurred_at"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("activity --json printed something that is not the wire shape (%v):\n%s", err, stdout)
	}
	if len(body.Events) == 0 {
		t.Fatalf("activity --json printed no events, so this test would pass against a command that emits an empty envelope:\n%s", stdout)
	}
	for _, e := range body.Events {
		if e.ArtifactID == "" || e.To == "" || e.OccurredAt == "" {
			t.Errorf("an event is missing a required field: %+v", e)
		}
	}
}

func TestActivity_BackupSetNarrowsTheFeed(t *testing.T) {
	configPath := twoSourceDeployment(t)

	all := captureStdout(t, func() {
		run([]string{"activity", "--config", configPath})
	})
	narrowed := captureStdout(t, func() {
		run([]string{"activity", "--config", configPath, "--backup-set", "api-server/var-backups"})
	})

	if !strings.Contains(all, "cicd-pipeline/var-backups") {
		t.Fatalf("the unfiltered feed does not mention the other source, so a filter cannot be shown to narrow anything:\n%s", all)
	}
	if strings.Contains(narrowed, "cicd-pipeline/var-backups") {
		t.Errorf("activity --backup-set api-server/var-backups printed the other source's rows:\n%s", narrowed)
	}
	if !strings.Contains(narrowed, "api-server/var-backups") {
		t.Errorf("activity --backup-set api-server/var-backups printed none of its own rows:\n%s", narrowed)
	}
}

func TestActivity_SeverityNarrowsTheFeed(t *testing.T) {
	configPath := oneCycleDeployment(t)

	errorsOnly := captureStdout(t, func() {
		run([]string{"activity", "--config", configPath, "--severity", "error"})
	})

	// A clean cycle records no quarantine and no failure, so an
	// errors-only feed over it is empty. Anything printed here is a row
	// the severity filter let through that it should not have.
	if strings.Contains(errorsOnly, "COMMITTED") || strings.Contains(errorsOnly, "DISCOVERED") {
		t.Errorf("activity --severity error printed ordinary pipeline steps:\n%s", errorsOnly)
	}
}

func TestActivity_LimitBoundsTheFeed(t *testing.T) {
	configPath := oneCycleDeployment(t)

	stdout := captureStdout(t, func() {
		run([]string{"activity", "--config", configPath, "--limit", "2"})
	})

	rows := 0
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if strings.TrimSpace(line) != "" {
			rows++
		}
	}
	if rows > 2 {
		t.Errorf("activity --limit 2 printed %d rows:\n%s", rows, stdout)
	}
	if rows == 0 {
		t.Errorf("activity --limit 2 printed nothing, so the bound cannot be shown to be a bound:\n%s", stdout)
	}
}

func TestActivity_AnUnparseableCommandLineIsATwo(t *testing.T) {
	configPath := oneCycleDeployment(t)

	if got := run([]string{"activity", "--config", configPath, "--severity", "loud"}); got != 2 {
		t.Errorf("activity --severity loud = %d, want 2 (a value no deployment would accept is a command-line error)", got)
	}
}

// TestActivity_IsRegisteredAndDiscoverable is the pair #549 asks for. A
// verb in the dispatch map and not in usage() is dispatchable and
// undiscoverable, which is how `backup-set remove` shipped.
func TestActivity_IsRegisteredAndDiscoverable(t *testing.T) {
	if _, ok := commands["activity"]; !ok {
		t.Fatal("`activity` is not in the commands map, so nothing dispatches it")
	}
	text := captureStderr(t, usage)
	if !strings.Contains(text, "  activity ") {
		t.Errorf("usage() has no entry line for `activity`, so the only reference an operator has does not mention it:\n%s", text)
	}
}
