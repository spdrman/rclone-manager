package app

import (
	"context"
	"path/filepath"
	"testing"
)

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
