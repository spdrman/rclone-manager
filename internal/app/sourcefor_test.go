package app

import (
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/internal/config"
	"github.com/spdrman/rclone-manager/internal/model"
)

// TestSourceForForwardsAllThreeKeyResolvers is #75: sourceFor is the one
// place a config.Remote's key configuration turns into the
// transport.Source ssh.go's sftpConfig actually resolves against. #74
// added key.env and key.command as siblings of key_file/key.file, and
// config.Validate accepts and normalizes all four spellings, but until this
// test (and the fix it pins) sourceFor only ever forwarded KeyFile: a real
// run configured with key.env or key.command would pass Validate and then
// fail loudly the first time it tried to connect, because the
// transport.Source it was actually handed carried none of the three
// sources sftpConfig looks for.
func TestSourceForForwardsAllThreeKeyResolvers(t *testing.T) {
	src := config.Source{Name: "production"}
	setID, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("model.NewBackupSetID: %v", err)
	}

	base := func(key config.Key) config.BackupSet {
		return config.BackupSet{
			Name:       "postgres-primary",
			ID:         setID,
			RemotePath: "/backups/postgres",
			Remote: config.Remote{
				Type:       "sftp",
				Host:       "production.example.internal",
				User:       "backup",
				KnownHosts: "/etc/backup-manager/known_hosts",
				Key:        key,
			},
		}
	}

	t.Run("key.file (and its deprecated key_file alias)", func(t *testing.T) {
		bs := base(config.Key{})
		bs.Remote.KeyFile = "/etc/backup-manager/id_ed25519"
		got := sourceFor(src, bs)
		if got.KeyFile != "/etc/backup-manager/id_ed25519" {
			t.Errorf("KeyFile = %q, want the configured path", got.KeyFile)
		}
		if got.KeyEnv != "" || len(got.KeyCommand) != 0 {
			t.Errorf("a key_file source also carried KeyEnv=%q KeyCommand=%v, want both empty", got.KeyEnv, got.KeyCommand)
		}
	})

	t.Run("key.env", func(t *testing.T) {
		bs := base(config.Key{Env: "BACKUP_SSH_KEY"})
		got := sourceFor(src, bs)
		if got.KeyEnv != "BACKUP_SSH_KEY" {
			t.Errorf("KeyEnv = %q, want %q", got.KeyEnv, "BACKUP_SSH_KEY")
		}
		if got.KeyFile != "" || len(got.KeyCommand) != 0 {
			t.Errorf("a key.env source also carried KeyFile=%q KeyCommand=%v, want both empty", got.KeyFile, got.KeyCommand)
		}
	})

	t.Run("key.command", func(t *testing.T) {
		want := []string{"/usr/local/bin/op", "read", "op://infra/backup-manager/private-key"}
		bs := base(config.Key{Command: want})
		got := sourceFor(src, bs)
		if len(got.KeyCommand) != len(want) {
			t.Fatalf("KeyCommand = %#v, want %#v", got.KeyCommand, want)
		}
		for i := range want {
			if got.KeyCommand[i] != want[i] {
				t.Errorf("KeyCommand[%d] = %q, want %q", i, got.KeyCommand[i], want[i])
			}
		}
		if got.KeyFile != "" || got.KeyEnv != "" {
			t.Errorf("a key.command source also carried KeyFile=%q KeyEnv=%q, want both empty", got.KeyFile, got.KeyEnv)
		}
	})
}

// TestSourceForNormalizedKeyFileMatchesValidate proves the two together:
// once config.Validate has run (as it always has by the time a real
// backup set reaches sourceFor), a Remote written with the new key.file
// block, not the deprecated top-level key_file, still produces a
// transport.Source with KeyFile populated, because Validate normalizes
// KeyFile and Key.File to agree before sourceFor ever sees the Remote.
func TestSourceForNormalizedKeyFileMatchesValidate(t *testing.T) {
	cfg := config.Config{
		PollInterval: config.Duration(15 * time.Minute),
		State:        config.State{Database: "/var/lib/backup-manager/state.db"},
		Sources: []config.Source{
			{
				Name: "production",
				BackupSets: []config.BackupSet{
					{
						Name: "postgres-primary",
						Remote: config.Remote{
							Type:       "sftp",
							Host:       "production.example.internal",
							User:       "backup",
							KnownHosts: "/etc/backup-manager/known_hosts",
							Key:        config.Key{File: "/etc/backup-manager/id_ed25519"},
						},
						RemotePath: "/backups/postgres",
						LocalPath:  "/backups/production/postgres",
						Completion: config.Completion{Strategy: "rename"},
						StaleAfter: config.Duration(30 * time.Hour),
					},
				},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	got := sourceFor(cfg.Sources[0], cfg.Sources[0].BackupSets[0])
	if got.KeyFile != "/etc/backup-manager/id_ed25519" {
		t.Fatalf("KeyFile = %q, want the key.file value Validate normalized into KeyFile", got.KeyFile)
	}
}
