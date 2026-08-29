package state_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// The journal's CHECK constraint and the state machine's set of states are a
// contract between two packages that were built independently, and a mismatch
// between them is not a compile error. QUARANTINED_LOST was added to the state
// machine while the journal still listed eleven states, and every build and
// test in this repository stayed green. The only symptom would have been a
// constraint violation at runtime, the first time something recorded
// irrecoverable data loss, which is the worst place to find out.
//
// This lives in an external test package so internal/state does not grow a
// production dependency on internal/lifecycle merely to be checked.
func TestJournalAcceptsExactlyTheStatesTheMachineDefines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "states.db")

	// Open runs the migrations, which is what actually creates the constraint.
	j, err := state.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Probe the live constraint rather than re-reading the .sql, so this tests
	// what the migrations produced and not what they claim.
	var accepted []string
	for _, s := range lifecycle.AllStates {
		if insertWithState(t, db, string(s)) {
			accepted = append(accepted, string(s))
		}
	}

	// Positive control. If the constraint accepted everything, the comparison
	// below would pass for the wrong reason.
	if insertWithState(t, db, "NOT_A_REAL_STATE") {
		t.Fatal("the journal accepted an invented state, so its CHECK constraint enforces nothing and this test proves nothing")
	}

	var defined []string
	for _, s := range lifecycle.AllStates {
		defined = append(defined, string(s))
	}
	sort.Strings(defined)
	sort.Strings(accepted)

	if len(defined) != len(accepted) {
		t.Fatalf("the state machine defines %d states but the journal accepts %d\n  machine: %v\n  journal: %v\nadd a migration widening the artifacts.state CHECK constraint",
			len(defined), len(accepted), defined, accepted)
	}
	for i := range defined {
		if defined[i] != accepted[i] {
			t.Fatalf("state sets differ: machine has %q where journal has %q\n  machine: %v\n  journal: %v",
				defined[i], accepted[i], defined, accepted)
		}
	}
}

func insertWithState(t *testing.T, db *sql.DB, s string) bool {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO artifacts (source, backup_set, artifact_name, remote_path, state, discovered_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		"probe", "set", "artifact-"+s, "/remote/"+s, s)
	return err == nil
}
