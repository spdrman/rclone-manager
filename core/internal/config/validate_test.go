package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

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
							Strategy:          "stable",
							StableFor:         Duration(10 * time.Minute),
							DeleteSafetyDelay: Duration(30 * time.Minute),
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

// retentionEqual compares two Retention values field by field, dereferencing
// ProtectLastKnownGood rather than comparing its pointer identity: two
// independently-built Retention values that both resolved to "true" have
// different *bool pointers, and a plain == or reflect.DeepEqual on the
// struct would wrongly report them as disagreeing.
func retentionEqual(a, b Retention) bool {
	if a.Timezone != b.Timezone || a.WeekStartsOn != b.WeekStartsOn ||
		a.DailyDays != b.DailyDays || a.WeeklyMonths != b.WeeklyMonths || a.MonthlyMonths != b.MonthlyMonths {
		return false
	}
	if (a.ProtectLastKnownGood == nil) != (b.ProtectLastKnownGood == nil) {
		return false
	}
	if a.ProtectLastKnownGood != nil && *a.ProtectLastKnownGood != *b.ProtectLastKnownGood {
		return false
	}
	return true
}

func retentionString(r Retention) string {
	protect := "nil"
	if r.ProtectLastKnownGood != nil {
		protect = fmt.Sprintf("%v", *r.ProtectLastKnownGood)
	}
	return fmt.Sprintf("{Timezone:%q WeekStartsOn:%q DailyDays:%d WeeklyMonths:%d MonthlyMonths:%d ProtectLastKnownGood:%s}",
		r.Timezone, r.WeekStartsOn, r.DailyDays, r.WeeklyMonths, r.MonthlyMonths, protect)
}

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
	if !reflect.DeepEqual(first.Retention, cfg.Retention) {
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

// --- ValidateRetention: the exported, standalone entry point issue #111's
// CLI (and any future UI-backing surface) must funnel through, pinned
// field-by-field against exactly what an embedded retention block resolves
// to through cfg.Validate(), so a later refactor of either path can never
// silently let the two disagree. ---

// TestValidateRetentionMatchesConfigValidateForEverySixFields is the
// locked baseline issue #111's RED plan calls for: for each of the six
// retention fields, the absent/invalid/(for the bool) explicit-false
// behavior of the standalone ValidateRetention(r) call must be identical,
// error text included, to what the same value produces embedded in a
// whole Config through cfg.Validate(). A caller (the CLI's override
// flags, or a future settings endpoint) that validates a candidate value
// through ValidateRetention must be refused, or accepted, for the exact
// same reason the config file itself would be.
func TestValidateRetentionMatchesConfigValidateForEverySixFields(t *testing.T) {
	errText := func(err error) string {
		if err == nil {
			return ""
		}
		return err.Error()
	}

	cases := []struct {
		name   string
		mutate func(*Retention)
	}{
		{"all six absent", func(r *Retention) { *r = Retention{} }},
		{"timezone: empty defaults to UTC", func(r *Retention) { r.Timezone = "" }},
		{"timezone: unloadable is refused", func(r *Retention) { r.Timezone = "Mars/Phobos" }},
		{"week_starts_on: empty defaults to monday", func(r *Retention) { r.WeekStartsOn = "" }},
		{"week_starts_on: non-weekday is refused", func(r *Retention) { r.WeekStartsOn = "someday" }},
		{"week_starts_on: mixed case is normalized", func(r *Retention) { r.WeekStartsOn = "Tuesday" }},
		{"daily_days: zero defaults to 7", func(r *Retention) { r.DailyDays = 0 }},
		{"daily_days: negative is refused", func(r *Retention) { r.DailyDays = -1 }},
		{"weekly_months: zero defaults to 3", func(r *Retention) { r.WeeklyMonths = 0 }},
		{"weekly_months: negative is refused", func(r *Retention) { r.WeeklyMonths = -1 }},
		{"monthly_months: zero defaults to 12", func(r *Retention) { r.MonthlyMonths = 0 }},
		{"monthly_months: negative is refused", func(r *Retention) { r.MonthlyMonths = -1 }},
		{"protect_last_known_good: absent defaults to true", func(r *Retention) { r.ProtectLastKnownGood = nil }},
		{"protect_last_known_good: explicit true is honored", func(r *Retention) { r.ProtectLastKnownGood = boolPtr(true) }},
		{"protect_last_known_good: explicit false is honored, not coerced to true", func(r *Retention) { r.ProtectLastKnownGood = boolPtr(false) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Path 1: embedded in a whole Config, exactly as the YAML file
			// itself goes through.
			viaConfig := validConfig()
			tc.mutate(&viaConfig.Retention)
			configErr := viaConfig.Validate()

			// Path 2: the standalone, exported entry point issue #111
			// adds for the CLI/UI.
			viaStandalone := validConfig().Retention
			tc.mutate(&viaStandalone)
			standaloneErr := ValidateRetention(&viaStandalone)

			if (configErr == nil) != (standaloneErr == nil) {
				t.Fatalf("cfg.Validate() err=%v, ValidateRetention() err=%v: presence of an error disagrees", configErr, standaloneErr)
			}

			// Retention-specific problems are a substring of cfg.Validate's
			// aggregate ValidationError text; ValidateRetention's own error
			// (when there is exactly one problem, which every case above
			// produces) must be the identical sentence, not a rephrasing.
			if standaloneErr != nil && !strings.Contains(errText(configErr), errText(standaloneErr)) {
				t.Fatalf("error text disagrees:\n  cfg.Validate():      %s\n  ValidateRetention():  %s", errText(configErr), errText(standaloneErr))
			}

			if !retentionEqual(viaConfig.Retention, viaStandalone) {
				t.Fatalf("resolved retention disagrees:\n  cfg.Validate():      %s\n  ValidateRetention(): %s", retentionString(viaConfig.Retention), retentionString(viaStandalone))
			}
		})
	}
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
		cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: "stable", DeleteSafetyDelay: Duration(30 * time.Minute)}
		if err := cfg.Validate(); err == nil {
			t.Fatal("strategy stable with no stable_for was accepted")
		}
	})
	// WP3.2: "stable" with no delete_safety_delay must load, and must come
	// back carrying the documented default rather than a literal zero.
	//
	// Both halves matter and they fail in opposite directions. Refusing
	// the config would stop the daemon loading a file that was valid
	// before the key existed, and would reject every stable-strategy
	// backup set service.CreateBackupSet builds, since that request type
	// has no field for this key. Reading the zero literally would leave
	// the gate in internal/lifecycle/remotedelete.go comparing against a
	// zero delay, which is the same thing as not having the gate.
	t.Run("stable defaults delete_safety_delay", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: "stable", StableFor: Duration(10 * time.Minute)}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("strategy stable with no delete_safety_delay was rejected: %v", err)
		}
		if got := cfg.Sources[0].BackupSets[0].Completion.DeleteSafetyDelay.Duration(); got != DefaultDeleteSafetyDelay {
			t.Fatalf("delete_safety_delay resolved to %s, want the default %s", got, DefaultDeleteSafetyDelay)
		}
	})
	// An explicit value is the operator's, and stays theirs: defaulting
	// must only ever fill a hole, never overwrite an answer.
	t.Run("stable keeps an explicit delete_safety_delay", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Completion = Completion{
			Strategy:          "stable",
			StableFor:         Duration(10 * time.Minute),
			DeleteSafetyDelay: Duration(90 * time.Minute),
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid stable completion was rejected: %v", err)
		}
		if got, want := cfg.Sources[0].BackupSets[0].Completion.DeleteSafetyDelay.Duration(), 90*time.Minute; got != want {
			t.Fatalf("delete_safety_delay = %s, want the operator's %s", got, want)
		}
	})
	// A negative delay is the one value that can only have been typed on
	// purpose and that would make the gate a no-op, so it is still refused.
	t.Run("stable rejects a negative delete_safety_delay", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Completion = Completion{
			Strategy:          "stable",
			StableFor:         Duration(10 * time.Minute),
			DeleteSafetyDelay: Duration(-time.Minute),
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("strategy stable with a negative delete_safety_delay was accepted")
		}
		if !strings.Contains(err.Error(), "delete_safety_delay") {
			t.Fatalf("error does not name the offending field: %v", err)
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
		t.Run(strategy+" rejects delete_safety_delay", func(t *testing.T) {
			cfg := validConfig()
			cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: strategy, DeleteSafetyDelay: Duration(time.Minute)}
			if err := cfg.Validate(); err == nil {
				t.Fatalf("strategy %s with delete_safety_delay set was accepted", strategy)
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

// --- manifest_marker (issue #291) ---

func TestManifestMarkerValidation(t *testing.T) {
	t.Run("unset defaults to _SUCCESS, so an existing config is unaffected", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: "marker"}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("strategy marker with no manifest_marker was rejected: %v", err)
		}
		if got := cfg.Sources[0].BackupSets[0].Completion.ManifestMarker; got != DefaultManifestMarker {
			t.Fatalf("manifest_marker resolved to %q, want the default %q", got, DefaultManifestMarker)
		}
	})

	t.Run("an explicit bare filename is kept, not overwritten", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: "marker", ManifestMarker: "SHA256SUMS"}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("a valid manifest_marker was rejected: %v", err)
		}
		if got, want := cfg.Sources[0].BackupSets[0].Completion.ManifestMarker, "SHA256SUMS"; got != want {
			t.Fatalf("manifest_marker = %q, want the operator's %q", got, want)
		}
	})

	for _, tc := range []struct {
		name   string
		marker string
	}{
		{"contains forward slash", "sub/SHA256SUMS"},
		{"contains backslash", `sub\SHA256SUMS`},
		{"is a single dot", "."},
		{"is a double dot", ".."},
	} {
		t.Run(tc.name+" rejected", func(t *testing.T) {
			cfg := validConfig()
			cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: "marker", ManifestMarker: tc.marker}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("manifest_marker %q was accepted", tc.marker)
			}
			if !strings.Contains(err.Error(), "manifest_marker") {
				t.Fatalf("error does not name the offending field: %v", err)
			}
		})
	}

	// A literal glob metacharacter is not rejected: manifest_marker is
	// matched as an exact literal filename in internal/discovery/complete.go,
	// never as a pattern (unlike include), so a character that would be an
	// invalid glob is still a perfectly valid literal filename here.
	t.Run("a glob-looking literal is accepted, since it is never used as a pattern", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: "marker", ManifestMarker: "[unterminated"}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("a literal manifest_marker containing glob metacharacters was rejected: %v", err)
		}
	})

	for _, strategy := range []string{"rename", "stable"} {
		t.Run(strategy+" rejects manifest_marker", func(t *testing.T) {
			cfg := validConfig()
			c := Completion{Strategy: strategy, ManifestMarker: "SHA256SUMS"}
			if strategy == "stable" {
				c.StableFor = Duration(10 * time.Minute)
			}
			cfg.Sources[0].BackupSets[0].Completion = c
			if err := cfg.Validate(); err == nil {
				t.Fatalf("strategy %s with manifest_marker set was accepted", strategy)
			}
		})
	}
}

// --- manifest_marker vs include collision (safety & reliability finding on
// #307): before manifest_marker was operator-configurable it was always the
// fixed literal "_SUCCESS", implicitly never a real payload name. Now that
// an operator picks it, a marker name that happens to match the backup
// set's own include patterns would make discovery.Discover silently and
// permanently drop that payload on every run: isMarkerObject is checked
// before include filtering and unconditionally skips a match, with no
// error, no rejection entry, no warning. ---

func TestManifestMarkerIncludeCollision(t *testing.T) {
	t.Run("explicit marker matching an explicit include pattern is rejected", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Include = []string{"*.dump.zst"}
		cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: "marker", ManifestMarker: "backup.dump.zst"}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("a manifest_marker matching the set's own include pattern was accepted")
		}
		if !strings.Contains(err.Error(), "manifest_marker") || !strings.Contains(err.Error(), "include") {
			t.Fatalf("error does not name both colliding fields: %v", err)
		}
	})

	t.Run("defaulted marker matching an explicit include pattern is rejected", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Include = []string{"_SUCCESS", "*.dump.zst"}
		cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: "marker"}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("a defaulted _SUCCESS marker matching the set's own include pattern was accepted")
		}
		if !strings.Contains(err.Error(), "manifest_marker") {
			t.Fatalf("error does not name the offending field: %v", err)
		}
	})

	t.Run("marker matching one of several include patterns is rejected", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Include = []string{"*.dump.zst", "MANIFEST-*", "*.tar"}
		cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: "marker", ManifestMarker: "MANIFEST-001"}
		if err := cfg.Validate(); err == nil {
			t.Fatal("a manifest_marker matching one of several include patterns was accepted")
		}
	})

	t.Run("marker not matching any include pattern is accepted", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Include = []string{"*.dump.zst"}
		cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: "marker", ManifestMarker: "SHA256SUMS"}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("a non-colliding manifest_marker was rejected: %v", err)
		}
	})

	t.Run("no include patterns configured, nothing to collide with", func(t *testing.T) {
		// An empty include list has no configured patterns to cross-check
		// against; that is the same "no explicit filter" baseline that let
		// the old hardcoded "_SUCCESS" work unconditionally before this
		// field existed, so it stays out of scope for this check.
		cfg := validConfig()
		cfg.Sources[0].BackupSets[0].Include = nil
		cfg.Sources[0].BackupSets[0].Completion = Completion{Strategy: "marker", ManifestMarker: "_SUCCESS"}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("a manifest_marker with no include patterns configured was rejected: %v", err)
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

// TestValidateValidatorID is issue #162's config-side rule set. The
// validator_id key exists so a backup set can name a validator this
// process's own registered catalog (core/service's validator.go) owns,
// WITHOUT the config file ever carrying the resolved executable path:
// that path is process- and deployment-specific, and a config.yaml
// holding a stale one would quarantine every artifact in the set after a
// restart.
//
// This package deliberately does not know the catalog (it cannot import
// core/service, which imports it), so it never decides whether an id is
// registered. What it does decide is the shape: an id is one token, and
// naming both a validator_id and a command is a contradiction rather
// than a precedence puzzle.
func TestValidateValidatorID(t *testing.T) {
	tests := []struct {
		name       string
		validation Validation
		wantErr    string
	}{
		{
			name:       "a plain registered-looking id is accepted",
			validation: Validation{Hash: "sha256", ValidatorID: "trailer-marker"},
		},
		{
			name:       "no validator at all is still accepted",
			validation: Validation{Hash: "sha256"},
		},
		{
			name: "a command with no validator_id is still accepted",
			validation: Validation{
				Hash:    "sha256",
				Command: &Command{Executable: "/usr/local/bin/validate", Timeout: Duration(time.Minute)},
			},
		},
		{
			name: "both a validator_id and a command is a contradiction",
			validation: Validation{
				Hash:        "sha256",
				ValidatorID: "trailer-marker",
				Command:     &Command{Executable: "/usr/local/bin/validate", Timeout: Duration(time.Minute)},
			},
			wantErr: "validator_id",
		},
		{
			name:       "a validator_id naming a path is refused",
			validation: Validation{Hash: "sha256", ValidatorID: "/usr/local/bin/validate"},
			wantErr:    "validator_id",
		},
		{
			name:       "a validator_id with a traversal is refused",
			validation: Validation{Hash: "sha256", ValidatorID: "../../etc/passwd"},
			wantErr:    "validator_id",
		},
		{
			name:       "a validator_id with surrounding whitespace is refused",
			validation: Validation{Hash: "sha256", ValidatorID: " trailer-marker "},
			wantErr:    "validator_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Sources[0].BackupSets[0].Validation = tt.validation

			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate rejected %+v: %v", tt.validation, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate accepted %+v; want an error mentioning %q", tt.validation, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestValidatorIDRoundTripsThroughYAML pins the wire key itself. The id
// is what an operator hand-edits and what CreateBackupSet writes back, so
// renaming the key silently is a breaking change to every existing
// config file, not an internal refactor.
func TestValidatorIDRoundTripsThroughYAML(t *testing.T) {
	const yamlSnippet = "hash: sha256\nvalidator_id: trailer-marker\n"
	var got Validation
	if err := yaml.Unmarshal([]byte(yamlSnippet), &got); err != nil {
		t.Fatalf("unmarshalling %q: %v", yamlSnippet, err)
	}
	if got.ValidatorID != "trailer-marker" {
		t.Fatalf("ValidatorID = %q, want %q (the YAML key must be validator_id)", got.ValidatorID, "trailer-marker")
	}
	if got.Command != nil {
		t.Fatalf("Command = %+v, want nil", got.Command)
	}
}
