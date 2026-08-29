package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// validConfig returns a Config that Validate accepts as-is. Individual
// tests copy it and break exactly one thing, so a failing test points at
// the one rule it's checking rather than at an unrelated fixture problem.
func validConfig() Config {
	return Config{
		PollInterval: Duration(15 * time.Minute),
		State: State{
			Database: "/var/lib/backup-manager/state.db",
		},
		Sources: []Source{
			{
				Name: "production",
				BackupSets: []BackupSet{
					{
						Name: "postgres-primary",
						Remote: Remote{
							Type:       "sftp",
							Host:       "production.example.internal",
							Port:       22,
							User:       "backup",
							KeyFile:    "/run/secrets/backup_ssh_key",
							KnownHosts: "/etc/backup-manager/known_hosts",
						},
						RemotePath: "/backups/postgres",
						LocalPath:  "/backups/production/postgres",
						Include:    []string{"*.dump.zst"},
						Completion: Completion{
							Strategy:  "stable",
							StableFor: Duration(10 * time.Minute),
						},
						StaleAfter: Duration(30 * time.Hour),
						Validation: Validation{
							Hash: "sha256",
							Command: &Command{
								Executable: "/usr/local/bin/validate-postgres-backup",
								Timeout:    Duration(10 * time.Minute),
							},
						},
					},
				},
			},
		},
		Retention: Retention{
			Timezone:             "America/Vancouver",
			WeekStartsOn:         "monday",
			DailyDays:            7,
			WeeklyMonths:         3,
			MonthlyMonths:        12,
			ProtectLastKnownGood: boolPtr(true),
		},
	}
}

func boolPtr(b bool) *bool { return &b }

func TestValidConfigPasses(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected a config that should pass: %v", err)
	}
}

func TestValidateIsIdempotent(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("first Validate: %v", err)
	}
	first := cfg
	if err := cfg.Validate(); err != nil {
		t.Fatalf("second Validate: %v", err)
	}
	if first.Retention != cfg.Retention {
		t.Fatalf("second Validate changed already-resolved retention: %#v vs %#v", first.Retention, cfg.Retention)
	}
	if first.Sources[0].BackupSets[0].ID != cfg.Sources[0].BackupSets[0].ID {
		t.Fatalf("second Validate changed an already-resolved backup set id")
	}
}

// --- retention: the "zero or missing tier" cases the task calls out ---

func TestMissingRetentionTierDefaultsSafely(t *testing.T) {
	cfg := validConfig()
	cfg.Retention = Retention{} // everything zero/absent, as if the whole block was left out of the file

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected an all-defaults retention block: %v", err)
	}

	// The whole point: zero/missing must not read as "keep nothing".
	if cfg.Retention.DailyDays != 7 {
		t.Errorf("DailyDays = %d, want the documented default 7, not 0 (\"keep nothing\")", cfg.Retention.DailyDays)
	}
	if cfg.Retention.WeeklyMonths != 3 {
		t.Errorf("WeeklyMonths = %d, want the documented default 3", cfg.Retention.WeeklyMonths)
	}
	if cfg.Retention.MonthlyMonths != 12 {
		t.Errorf("MonthlyMonths = %d, want the documented default 12", cfg.Retention.MonthlyMonths)
	}
	if cfg.Retention.Timezone != "UTC" {
		t.Errorf("Timezone = %q, want default UTC", cfg.Retention.Timezone)
	}
	if cfg.Retention.WeekStartsOn != "monday" {
		t.Errorf("WeekStartsOn = %q, want default monday", cfg.Retention.WeekStartsOn)
	}
	if cfg.Retention.ProtectLastKnownGood == nil || !*cfg.Retention.ProtectLastKnownGood {
		t.Errorf("ProtectLastKnownGood = %v, want default true", cfg.Retention.ProtectLastKnownGood)
	}
}

func TestNegativeRetentionTiersRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Retention)
	}{
		{"daily", func(r *Retention) { r.DailyDays = -1 }},
		{"weekly", func(r *Retention) { r.WeeklyMonths = -1 }},
		{"monthly", func(r *Retention) { r.MonthlyMonths = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg.Retention)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("a negative retention tier was accepted")
			}
		})
	}
}

func TestProtectLastKnownGoodDefaultsTrueWhenAbsent(t *testing.T) {
	cfg := validConfig()
	cfg.Retention.ProtectLastKnownGood = nil

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Retention.ProtectLastKnownGood == nil || !*cfg.Retention.ProtectLastKnownGood {
		t.Fatalf("an absent protect_last_known_good must default to true (FR-19), got %v", cfg.Retention.ProtectLastKnownGood)
	}
}

func TestExplicitProtectLastKnownGoodFalseIsRespected(t *testing.T) {
	cfg := validConfig()
	cfg.Retention.ProtectLastKnownGood = boolPtr(false)

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Retention.ProtectLastKnownGood == nil || *cfg.Retention.ProtectLastKnownGood {
		t.Fatalf("an explicit false must not be overridden by the default, got %v", cfg.Retention.ProtectLastKnownGood)
	}
}

func TestRetentionTimezoneValidated(t *testing.T) {
	cfg := validConfig()
	cfg.Retention.Timezone = "Mars/Phobos"
	if err := cfg.Validate(); err == nil {
		t.Fatal("an unloadable timezone was accepted")
	}
}

func TestRetentionWeekStartsOnValidated(t *testing.T) {
	t.Run("normalizes case", func(t *testing.T) {
		cfg := validConfig()
		cfg.Retention.WeekStartsOn = "Monday"
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if cfg.Retention.WeekStartsOn != "monday" {
			t.Fatalf("WeekStartsOn = %q, want normalized to lowercase", cfg.Retention.WeekStartsOn)
		}
	})
	t.Run("rejects non-weekday", func(t *testing.T) {
		cfg := validConfig()
		cfg.Retention.WeekStartsOn = "someday"
		if err := cfg.Validate(); err == nil {
			t.Fatal("a non-weekday week_starts_on was accepted")
		}
	})
}

// --- stale_after: the other case the task calls out ---

func TestStaleAfterZeroIsRejected(t *testing.T) {
	cfg := validConfig()
	cfg.Sources[0].BackupSets[0].StaleAfter = 0
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a zero stale_after was accepted, which would make every backup set immediately STALE")
	}
	if !strings.Contains(err.Error(), "stale_after") {
		t.Fatalf("error %q does not mention stale_after", err.Error())
	}
}

func TestStaleAfterNegativeIsRejected(t *testing.T) {
	cfg := validConfig()
	cfg.Sources[0].BackupSets[0].StaleAfter = Duration(-time.Hour)
	if err := cfg.Validate(); err == nil {
		t.Fatal("a negative stale_after was accepted")
	}
}

// --- backup set identity: must go through model.NewBackupSetID ---

func TestBackupSetIDBuiltThroughModel(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	want, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("model.NewBackupSetID: %v", err)
	}
	if got := cfg.Sources[0].BackupSets[0].ID; got != want {
		t.Fatalf("BackupSet.ID = %#v, want %#v", got, want)
	}
}

func TestBackupSetIDRejectsWhatModelRejects(t *testing.T) {
	// If this package ever regressed to building the id by string
	// concatenation, a source or set name containing the separator would
	// silently produce a colliding identity instead of an error. Proving
	// model.NewBackupSetID's own rule (no "/" in either half) is enforced
	// here is how this package protects against that regression.
	cfg := validConfig()
	cfg.Sources[0].Name = "a/b"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a source id containing \"/\" was accepted")
	}
}

func TestDuplicateBackupSetIDRejected(t *testing.T) {
	cfg := validConfig()
	// Two different Source entries that both claim the same source name and
	// set name resolve to the same BackupSetID; FR-7 requires every set to
	// be independently addressable, so this must be refused.
	cfg.Sources = append(cfg.Sources, cfg.Sources[0])

	err := cfg.Validate()
	if err == nil {
		t.Fatal("two backup sets resolving to the same id were accepted")
	}
	if !strings.Contains(err.Error(), "already used by") {
		t.Fatalf("error %q does not explain the collision", err.Error())
	}
}

// --- remote / sftp ---

func TestSftpRequiredFieldsMissing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Remote)
	}{
		{"host", func(r *Remote) { r.Host = "" }},
		{"user", func(r *Remote) { r.User = "" }},
		{"key_file", func(r *Remote) { r.KeyFile = "" }},
		{"known_hosts", func(r *Remote) { r.KnownHosts = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg.Sources[0].BackupSets[0].Remote)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("a config missing %s was accepted", tc.name)
			}
		})
	}
}

// --- #74: key_file, key.file, key.env, key.command ---

func TestKeyExactlyOneSourceRequired(t *testing.T) {
	t.Run("zero sources rejected", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Remote.KeyFile = ""
		err := cfg.Validate()
		if err == nil {
			t.Fatal("a remote with no key source at all was accepted")
		}
		if !strings.Contains(err.Error(), "key_file") {
			t.Fatalf("error %q does not mention the missing key source", err.Error())
		}
	})

	t.Run("key_file alone is accepted (deprecated alias)", func(t *testing.T) {
		cfg := validConfig()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		r := cfg.Sources[0].BackupSets[0].Remote
		if r.Key.File != r.KeyFile {
			t.Fatalf("Key.File = %q, want it normalized to match KeyFile %q", r.Key.File, r.KeyFile)
		}
	})

	t.Run("key.file alone is accepted and normalized into KeyFile", func(t *testing.T) {
		cfg := validConfig()
		r := &cfg.Sources[0].BackupSets[0].Remote
		r.KeyFile = ""
		r.Key = Key{File: "/etc/backup-manager/id_ed25519"}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if r.KeyFile != "/etc/backup-manager/id_ed25519" {
			t.Fatalf("KeyFile = %q, want it normalized to match Key.File", r.KeyFile)
		}
	})

	t.Run("key.env alone is accepted", func(t *testing.T) {
		cfg := validConfig()
		r := &cfg.Sources[0].BackupSets[0].Remote
		r.KeyFile = ""
		r.Key = Key{Env: "BACKUP_SSH_KEY"}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("key.command alone is accepted", func(t *testing.T) {
		cfg := validConfig()
		r := &cfg.Sources[0].BackupSets[0].Remote
		r.KeyFile = ""
		r.Key = Key{Command: []string{"/usr/local/bin/op", "read", "op://infra/backup-manager/private-key"}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("key_file and key.file set to the same value is accepted", func(t *testing.T) {
		cfg := validConfig()
		r := &cfg.Sources[0].BackupSets[0].Remote
		r.Key = Key{File: r.KeyFile}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("key_file and key.file set to different values rejected", func(t *testing.T) {
		cfg := validConfig()
		r := &cfg.Sources[0].BackupSets[0].Remote
		r.Key = Key{File: "/a/different/path"}
		if err := cfg.Validate(); err == nil {
			t.Fatal("key_file and key.file set to two different values were accepted")
		}
	})

	for _, tc := range []struct {
		name string
		key  Key
	}{
		{"key_file and key.env", Key{Env: "BACKUP_SSH_KEY"}},
		{"key_file and key.command", Key{Command: []string{"/usr/local/bin/op", "read", "x"}}},
	} {
		t.Run(tc.name+" together rejected", func(t *testing.T) {
			cfg := validConfig()
			r := &cfg.Sources[0].BackupSets[0].Remote
			r.Key = tc.key // KeyFile is still set from validConfig()
			if err := cfg.Validate(); err == nil {
				t.Fatalf("%s set together were accepted", tc.name)
			}
		})
	}

	t.Run("key.env and key.command together rejected", func(t *testing.T) {
		cfg := validConfig()
		r := &cfg.Sources[0].BackupSets[0].Remote
		r.KeyFile = ""
		r.Key = Key{Env: "BACKUP_SSH_KEY", Command: []string{"/usr/local/bin/op", "read", "x"}}
		if err := cfg.Validate(); err == nil {
			t.Fatal("key.env and key.command set together were accepted")
		}
	})
}

func TestKeyCommandExecutableMustBeAbsolute(t *testing.T) {
	cfg := validConfig()
	r := &cfg.Sources[0].BackupSets[0].Remote
	r.KeyFile = ""
	r.Key = Key{Command: []string{"op", "read", "op://infra/backup-manager/private-key"}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a relative key.command executable was accepted")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error %q does not explain the absolute-path requirement", err.Error())
	}
}

func TestKeyCommandEmptyExecutableRejected(t *testing.T) {
	cfg := validConfig()
	r := &cfg.Sources[0].BackupSets[0].Remote
	r.KeyFile = ""
	r.Key = Key{Command: []string{""}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a key.command with an empty executable was accepted")
	}
}

func TestKeyValidateIsIdempotent(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  Key
	}{
		{"deprecated key_file alone", Key{}},
		{"key.file", Key{File: "/etc/backup-manager/id_ed25519"}},
		{"key.env", Key{Env: "BACKUP_SSH_KEY"}},
		{"key.command", Key{Command: []string{"/usr/local/bin/op", "read", "x"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			r := &cfg.Sources[0].BackupSets[0].Remote
			if !tc.key.isZero() {
				r.KeyFile = ""
				r.Key = tc.key
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("first Validate: %v", err)
			}
			first := *r
			if err := cfg.Validate(); err != nil {
				t.Fatalf("second Validate: %v", err)
			}
			if !reflect.DeepEqual(*r, first) {
				t.Fatalf("second Validate changed an already-resolved remote: %#v vs %#v", first, *r)
			}
		})
	}
}

func TestKnownHostsNoneRejected(t *testing.T) {
	for _, v := range []string{"none", "None", " none ", "NONE"} {
		t.Run(v, func(t *testing.T) {
			cfg := validConfig()
			cfg.Sources[0].BackupSets[0].Remote.KnownHosts = v
			if err := cfg.Validate(); err == nil {
				t.Fatalf("known_hosts value %q was accepted, which disables host-key verification (FR-6)", v)
			}
		})
	}
}

func TestSftpPortRange(t *testing.T) {
	for _, tc := range []struct {
		name string
		port int
		ok   bool
	}{
		{"default (zero)", 0, true},
		{"typical", 22, true},
		{"max", 65535, true},
		{"negative", -1, false},
		{"too large", 65536, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Sources[0].BackupSets[0].Remote.Port = tc.port
			err := cfg.Validate()
			if tc.ok && err != nil {
				t.Fatalf("port %d rejected: %v", tc.port, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("port %d accepted", tc.port)
			}
		})
	}
}

func TestUnsupportedRemoteTypeRejected(t *testing.T) {
	for _, typ := range []string{"", "s3", "webdav", "SFTP"} {
		t.Run(typ, func(t *testing.T) {
			cfg := validConfig()
			cfg.Sources[0].BackupSets[0].Remote.Type = typ
			if err := cfg.Validate(); err == nil {
				t.Fatalf("remote type %q was accepted; only local and sftp are registered (FR-4)", typ)
			}
		})
	}
}

func TestLocalTypeRejectsSftpFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Remote)
	}{
		{"host", func(r *Remote) { r.Host = "example" }},
		{"user", func(r *Remote) { r.User = "backup" }},
		{"port", func(r *Remote) { r.Port = 22 }},
		{"key_file", func(r *Remote) { r.KeyFile = "/key" }},
		{"key.file", func(r *Remote) { r.Key = Key{File: "/key"} }},
		{"key.env", func(r *Remote) { r.Key = Key{Env: "SOME_VAR"} }},
		{"key.command", func(r *Remote) { r.Key = Key{Command: []string{"/bin/cat", "/dev/null"}} }},
		{"known_hosts", func(r *Remote) { r.KnownHosts = "/known_hosts" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			r := &cfg.Sources[0].BackupSets[0].Remote
			*r = Remote{Type: "local"}
			tc.mutate(r)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("type local with %s set was accepted", tc.name)
			}
		})
	}

	t.Run("clean local remote is accepted", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Remote = Remote{Type: "local"}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("a clean type-local remote was rejected: %v", err)
		}
	})
}

// --- paths ---

func TestPathsMustBeAbsolute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"local_path", func(c *Config) { c.Sources[0].BackupSets[0].LocalPath = "relative/path" }},
		{"remote_path", func(c *Config) { c.Sources[0].BackupSets[0].RemotePath = "relative/path" }},
		{"state.database", func(c *Config) { c.State.Database = "relative/state.db" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("a relative %s was accepted", tc.name)
			}
		})
	}
}

func TestPathsRejectTraversal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"local_path", func(c *Config) { c.Sources[0].BackupSets[0].LocalPath = "/backups/../etc" }},
		{"remote_path", func(c *Config) { c.Sources[0].BackupSets[0].RemotePath = "/backups/../etc" }},
		{"state.database", func(c *Config) { c.State.Database = "/var/lib/../etc/state.db" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("a %s containing \"..\" was accepted", tc.name)
			}
		})
	}
}

func TestEmptyPathsRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"local_path", func(c *Config) { c.Sources[0].BackupSets[0].LocalPath = "" }},
		{"remote_path", func(c *Config) { c.Sources[0].BackupSets[0].RemotePath = "" }},
		{"state.database", func(c *Config) { c.State.Database = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("an empty %s was accepted", tc.name)
			}
		})
	}
}

// --- completion ---

func TestCompletionStrategyValidation(t *testing.T) {
	t.Run("stable requires stable_for", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: "stable"}
		if err := cfg.Validate(); err == nil {
			t.Fatal("strategy stable with no stable_for was accepted")
		}
	})
	for _, strategy := range []string{"rename", "marker"} {
		t.Run(strategy+" rejects stable_for", func(t *testing.T) {
			cfg := validConfig()
			cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: strategy, StableFor: Duration(time.Minute)}
			if err := cfg.Validate(); err == nil {
				t.Fatalf("strategy %s with stable_for set was accepted", strategy)
			}
		})
		t.Run(strategy+" alone is accepted", func(t *testing.T) {
			cfg := validConfig()
			cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: strategy}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("strategy %s was rejected: %v", strategy, err)
			}
		})
	}
	t.Run("empty strategy rejected", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Completion = Completion{}
		if err := cfg.Validate(); err == nil {
			t.Fatal("an empty completion strategy was accepted")
		}
	})
	t.Run("unknown strategy rejected", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: "vibes"}
		if err := cfg.Validate(); err == nil {
			t.Fatal("an unknown completion strategy was accepted")
		}
	})
}

// --- include patterns ---

func TestIncludePatternValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		pat  string
	}{
		{"empty", ""},
		{"contains slash", "sub/*.dump.zst"},
		{"contains backslash", `sub\*.dump.zst`},
		{"invalid glob", "[unterminated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Sources[0].BackupSets[0].Include = []string{tc.pat}
			if err := cfg.Validate(); err == nil {
				t.Fatalf("include pattern %q was accepted", tc.pat)
			}
		})
	}

	t.Run("no include patterns is accepted", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Include = nil
		if err := cfg.Validate(); err != nil {
			t.Fatalf("an absent include list was rejected: %v", err)
		}
	})
}

// --- validation block ---

func TestValidationHash(t *testing.T) {
	for _, tc := range []struct {
		hash string
		ok   bool
	}{
		{"", true},
		{"sha256", true},
		{"md5", false},
		{"SHA256", false},
	} {
		t.Run(tc.hash, func(t *testing.T) {
			cfg := validConfig()
			cfg.Sources[0].BackupSets[0].Validation.Hash = tc.hash
			err := cfg.Validate()
			if tc.ok && err != nil {
				t.Fatalf("hash %q rejected: %v", tc.hash, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("hash %q accepted", tc.hash)
			}
		})
	}
}

func TestValidationCommand(t *testing.T) {
	t.Run("nil command is accepted", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Validation.Command = nil
		if err := cfg.Validate(); err != nil {
			t.Fatalf("a nil command was rejected: %v", err)
		}
	})
	t.Run("empty executable rejected", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Validation.Command.Executable = ""
		if err := cfg.Validate(); err == nil {
			t.Fatal("an empty command executable was accepted")
		}
	})
	t.Run("relative executable rejected", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Validation.Command.Executable = "validate-postgres-backup"
		if err := cfg.Validate(); err == nil {
			t.Fatal("a relative command executable was accepted")
		}
	})
	t.Run("zero timeout rejected", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Validation.Command.Timeout = 0
		if err := cfg.Validate(); err == nil {
			t.Fatal("a zero command timeout was accepted")
		}
	})
}

func TestRevalidationDisabledByDefault(t *testing.T) {
	cfg := validConfig()
	if cfg.Sources[0].BackupSets[0].Revalidation != (Revalidation{}) {
		t.Fatalf("validConfig() fixture already sets Revalidation, want the zero value for this test: %#v", cfg.Sources[0].BackupSets[0].Revalidation)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a config with no revalidation block was rejected: %v", err)
	}
}

func TestRevalidationRequiresIntervalAndScopeWhenEnabled(t *testing.T) {
	t.Run("hash alone requires interval and max_per_cycle", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Revalidation = Revalidation{Hash: true}
		if err := cfg.Validate(); err == nil {
			t.Fatal("hash: true with no interval or max_per_cycle was accepted")
		}
	})
	t.Run("command alone requires interval and max_per_cycle", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Revalidation = Revalidation{
			Command: &Command{Executable: "/usr/local/bin/restore-test", Timeout: Duration(5 * time.Minute)},
		}
		if err := cfg.Validate(); err == nil {
			t.Fatal("a command with no interval or max_per_cycle was accepted")
		}
	})
	t.Run("interval and max_per_cycle with neither hash nor command rejected", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Revalidation = Revalidation{Interval: Duration(24 * time.Hour), MaxPerCycle: 5}
		if err := cfg.Validate(); err == nil {
			t.Fatal("interval/max_per_cycle set with neither hash nor command was accepted")
		}
	})
	t.Run("fully specified hash-only revalidation is accepted", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Revalidation = Revalidation{
			Interval:    Duration(720 * time.Hour),
			MaxPerCycle: 10,
			Hash:        true,
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("a fully specified hash-only revalidation block was rejected: %v", err)
		}
	})
	t.Run("zero max_per_cycle rejected once enabled", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Revalidation = Revalidation{Interval: Duration(24 * time.Hour), Hash: true}
		if err := cfg.Validate(); err == nil {
			t.Fatal("max_per_cycle left at zero was accepted once revalidation was enabled")
		}
	})
	t.Run("negative max_per_cycle rejected", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Revalidation = Revalidation{Interval: Duration(24 * time.Hour), MaxPerCycle: -1, Hash: true}
		if err := cfg.Validate(); err == nil {
			t.Fatal("a negative max_per_cycle was accepted")
		}
	})
}

func TestRevalidationCommand(t *testing.T) {
	base := func() Revalidation {
		return Revalidation{
			Interval:    Duration(720 * time.Hour),
			MaxPerCycle: 5,
			Command:     &Command{Executable: "/usr/local/bin/restore-test", Timeout: Duration(5 * time.Minute)},
		}
	}
	t.Run("fully specified command is accepted", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Revalidation = base()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("a fully specified restore-test command was rejected: %v", err)
		}
	})
	t.Run("empty executable rejected", func(t *testing.T) {
		cfg := validConfig()
		r := base()
		r.Command.Executable = ""
		cfg.Sources[0].BackupSets[0].Revalidation = r
		if err := cfg.Validate(); err == nil {
			t.Fatal("an empty command executable was accepted")
		}
	})
	t.Run("relative executable rejected", func(t *testing.T) {
		cfg := validConfig()
		r := base()
		r.Command.Executable = "restore-test"
		cfg.Sources[0].BackupSets[0].Revalidation = r
		if err := cfg.Validate(); err == nil {
			t.Fatal("a relative command executable was accepted")
		}
	})
	t.Run("zero timeout rejected", func(t *testing.T) {
		cfg := validConfig()
		r := base()
		r.Command.Timeout = 0
		cfg.Sources[0].BackupSets[0].Revalidation = r
		if err := cfg.Validate(); err == nil {
			t.Fatal("a zero command timeout was accepted")
		}
	})
}

// --- top level ---

func TestPollIntervalMustBePositive(t *testing.T) {
	for _, d := range []Duration{0, Duration(-time.Minute)} {
		cfg := validConfig()
		cfg.PollInterval = d
		if err := cfg.Validate(); err == nil {
			t.Fatalf("poll_interval %s was accepted", d)
		}
	}
}

func TestAtLeastOneSourceRequired(t *testing.T) {
	cfg := validConfig()
	cfg.Sources = nil
	if err := cfg.Validate(); err == nil {
		t.Fatal("a config with no sources was accepted")
	}
}

func TestAtLeastOneBackupSetRequired(t *testing.T) {
	cfg := validConfig()
	cfg.Sources[0].BackupSets = nil
	if err := cfg.Validate(); err == nil {
		t.Fatal("a source with no backup sets was accepted")
	}
}

func TestDuplicateSourceNameRejected(t *testing.T) {
	cfg := validConfig()
	second := cfg.Sources[0]
	second.BackupSets = []BackupSet{
		{
			Name:       "other-set",
			Remote:     cfg.Sources[0].BackupSets[0].Remote,
			RemotePath: "/backups/other",
			LocalPath:  "/backups/production/other",
			Completion: Completion{Strategy: "rename"},
			StaleAfter: Duration(time.Hour),
		},
	}
	cfg.Sources = append(cfg.Sources, second)

	err := cfg.Validate()
	if err == nil {
		t.Fatal("two sources with the same id were accepted")
	}
	if !strings.Contains(err.Error(), "duplicate source id") {
		t.Fatalf("error %q does not explain the duplicate source", err.Error())
	}
}

// --- error aggregation ---

func TestValidationErrorAggregatesAllProblems(t *testing.T) {
	cfg := validConfig()
	cfg.PollInterval = 0
	cfg.State.Database = ""
	cfg.Sources[0].BackupSets[0].StaleAfter = 0

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected an error")
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if len(verr.Problems) < 3 {
		t.Fatalf("expected at least 3 problems reported together, got %d: %v", len(verr.Problems), verr.Problems)
	}
}

func TestValidationErrorUnwrapExposesEachProblem(t *testing.T) {
	inner := errors.New("boom")
	verr := &ValidationError{Problems: []error{inner, errors.New("also boom")}}

	if !errors.Is(verr, inner) {
		t.Fatal("errors.Is could not reach an individual problem through Unwrap")
	}
	if got := verr.Unwrap(); len(got) != 2 {
		t.Fatalf("Unwrap() returned %d errors, want 2", len(got))
	}
}
