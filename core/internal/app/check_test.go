package app

import (
	"context"
	"path/filepath"
	"testing"
)

// Two cases, and the second one is the whole file.
//
// Proving that a valid configuration opens and closes a journal is a smoke
// test. Proving that an invalid one fails BEFORE the database is touched is
// the ordering `check` promises, and it is the reason the invalid case uses a
// config with no state.database in it at all: if validation were skipped or
// ran second, the run would fail on a missing database path rather than on
// the duration it cannot parse, and both spellings look like a passing
// refusal from the outside.
//
// Neither case contacts a remote, which is the point of `check` rather than
// an omission in its tests.

func TestCheck_ValidConfigOpensAndClosesTheJournal(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	configPath := filepath.Join(dir, "config.yaml")

	mustWriteFile(t, configPath, `
poll_interval: 15m
state:
  database: `+dbPath+`
sources:
  - id: production
    backup_sets:
      - id: postgres-primary
        remote:
          type: local
        remote_path: /backups
        local_path: `+filepath.Join(dir, "local")+`
        completion:
          strategy: rename
        stale_after: 24h
`)

	cfg, err := Check(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if cfg.State.Database != dbPath {
		t.Errorf("cfg.State.Database = %q, want %q", cfg.State.Database, dbPath)
	}
}

func TestCheck_InvalidConfigFailsBeforeTouchingTheDatabase(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	mustWriteFile(t, configPath, "poll_interval: not-a-duration\n")

	if _, err := Check(context.Background(), configPath); err == nil {
		t.Error("Check with an invalid config = nil error, want an error")
	}
}
