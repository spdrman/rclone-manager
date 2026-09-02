package app

import (
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// sourceFor is the seam where configuration becomes a transport.Source, and it
// is easy to add a config field and forget this function exists. That is
// exactly what happened with #74: the adapter grew env and command key
// resolvers with sixteen passing tests, and a real run could still only ever
// use a file, because this function forwarded KeyFile alone. It failed loudly
// rather than silently, which is the only reason it was not worse.
//
// So walk all three sources rather than checking one.
func TestSourceForForwardsEveryKeySource(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  config.Key
	}{
		{"file", config.Key{File: "/etc/backup-manager/id_ed25519"}},
		{"env", config.Key{Env: "BACKUP_SSH_KEY"}},
		{"command", config.Key{Command: []string{"op", "read", "op://infra/backup/key"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bs := testBackupSet(t, "/var/backups/postgres")
			bs.Remote.Type = "sftp"
			bs.Remote.Key = tc.key

			cfg := &config.Config{}
			got := sourceFor(cfg, testSource("production", bs), bs)

			if got.KeyFile != tc.key.File {
				t.Errorf("KeyFile = %q, want %q", got.KeyFile, tc.key.File)
			}
			if got.KeyEnv != tc.key.Env {
				t.Errorf("KeyEnv = %q, want %q", got.KeyEnv, tc.key.Env)
			}
			if len(got.KeyCommand) != len(tc.key.Command) {
				t.Fatalf("KeyCommand = %v, want %v", got.KeyCommand, tc.key.Command)
			}
			for i := range tc.key.Command {
				if got.KeyCommand[i] != tc.key.Command[i] {
					t.Errorf("KeyCommand[%d] = %q, want %q", i, got.KeyCommand[i], tc.key.Command[i])
				}
			}

			// Whichever source was configured, exactly one must arrive set.
			// config.Validate already refuses two, so more than one here
			// would mean this function invented it.
			n := 0
			if got.KeyFile != "" {
				n++
			}
			if got.KeyEnv != "" {
				n++
			}
			if len(got.KeyCommand) > 0 {
				n++
			}
			if n != 1 {
				t.Fatalf("expected exactly one key source on the transport.Source, got %d (%+v)", n, got)
			}
		})
	}
}

// TestSourceForForwardsEveryPassphraseSource is #269's own version of the
// test above, for the exact same reason: Key.Passphrase is a config field
// with three sources of its own, and it is just as easy to forget this
// function has to forward all three of those too.
func TestSourceForForwardsEveryPassphraseSource(t *testing.T) {
	for _, tc := range []struct {
		name       string
		passphrase config.Passphrase
	}{
		{"file", config.Passphrase{File: "/etc/backup-manager/id_ed25519.passphrase"}},
		{"env", config.Passphrase{Env: "BACKUP_SSH_KEY_PASSPHRASE"}},
		{"command", config.Passphrase{Command: []string{"op", "read", "op://infra/backup/key-passphrase"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bs := testBackupSet(t, "/var/backups/postgres")
			bs.Remote.Type = "sftp"
			bs.Remote.Key = config.Key{File: "/etc/backup-manager/id_ed25519", Passphrase: tc.passphrase}

			got := sourceFor(&config.Config{}, testSource("production", bs), bs)

			if got.PassphraseFile != tc.passphrase.File {
				t.Errorf("PassphraseFile = %q, want %q", got.PassphraseFile, tc.passphrase.File)
			}
			if got.PassphraseEnv != tc.passphrase.Env {
				t.Errorf("PassphraseEnv = %q, want %q", got.PassphraseEnv, tc.passphrase.Env)
			}
			if len(got.PassphraseCommand) != len(tc.passphrase.Command) {
				t.Fatalf("PassphraseCommand = %v, want %v", got.PassphraseCommand, tc.passphrase.Command)
			}
			for i := range tc.passphrase.Command {
				if got.PassphraseCommand[i] != tc.passphrase.Command[i] {
					t.Errorf("PassphraseCommand[%d] = %q, want %q", i, got.PassphraseCommand[i], tc.passphrase.Command[i])
				}
			}

			n := 0
			if got.PassphraseFile != "" {
				n++
			}
			if got.PassphraseEnv != "" {
				n++
			}
			if len(got.PassphraseCommand) > 0 {
				n++
			}
			if n != 1 {
				t.Fatalf("expected exactly one passphrase source on the transport.Source, got %d (%+v)", n, got)
			}
		})
	}
}

// TestSourceForForwardsKeyEncryption is #298's version of the test above,
// for the same documented reason: KeyEncryption is config-wide rather than
// per-Remote, so it is exactly the kind of field this function's own doc
// warns is easy to add and forget to forward.
func TestSourceForForwardsKeyEncryption(t *testing.T) {
	for _, tc := range []struct {
		name string
		ke   config.KeyEncryption
	}{
		{"unset", config.KeyEncryption{}},
		{"file", config.KeyEncryption{File: "/etc/backup-manager/key.dek"}},
		{"env", config.KeyEncryption{Env: "BACKUP_KEY_DEK"}},
		{"command", config.KeyEncryption{Command: []string{"op", "read", "op://infra/backup/dek"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bs := testBackupSet(t, "/var/backups/postgres")
			bs.Remote.Type = "sftp"
			bs.Remote.Key = config.Key{File: "/etc/backup-manager/id_ed25519"}

			cfg := &config.Config{KeyEncryption: tc.ke}
			got := sourceFor(cfg, testSource("production", bs), bs)

			if got.KeyEncryptionFile != tc.ke.File {
				t.Errorf("KeyEncryptionFile = %q, want %q", got.KeyEncryptionFile, tc.ke.File)
			}
			if got.KeyEncryptionEnv != tc.ke.Env {
				t.Errorf("KeyEncryptionEnv = %q, want %q", got.KeyEncryptionEnv, tc.ke.Env)
			}
			if len(got.KeyEncryptionCommand) != len(tc.ke.Command) {
				t.Fatalf("KeyEncryptionCommand = %v, want %v", got.KeyEncryptionCommand, tc.ke.Command)
			}
			for i := range tc.ke.Command {
				if got.KeyEncryptionCommand[i] != tc.ke.Command[i] {
					t.Errorf("KeyEncryptionCommand[%d] = %q, want %q", i, got.KeyEncryptionCommand[i], tc.ke.Command[i])
				}
			}
		})
	}
}

// TestSourceForForwardsTheConnectionCeiling is #355's version of the tests
// above, and it exists because this line had none: deleting
// `MaxConnections: r.MaxConnections` from sourceFor left every test in
// ./internal/... and ./service/... green, so the one line carrying an
// operator's configured ceiling into the adapter was unpinned. A ceiling
// that never arrives is worse than no ceiling at all, because the operator
// has been told they set one.
func TestSourceForForwardsTheConnectionCeiling(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  int
	}{
		{"unset", 0},
		{"a real ceiling", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bs := testBackupSet(t, "/var/backups/postgres")
			bs.Remote.Type = "sftp"
			bs.Remote.Key = config.Key{File: "/etc/backup-manager/id_ed25519"}
			bs.Remote.MaxConnections = tc.set

			got := sourceFor(&config.Config{}, testSource("production", bs), bs)

			if got.MaxConnections != tc.set {
				t.Errorf("MaxConnections = %d, want %d: an operator's configured ceiling has to reach the adapter, or it is only enforced by the host refusing the connection", got.MaxConnections, tc.set)
			}
		})
	}
}
