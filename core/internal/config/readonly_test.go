// Issue #282's inheritance: read_only declared once on a source, overridden
// per backup set, and resolved into BackupSet.ReadOnly by Validate.
//
// The three states are tested separately because they fail differently and
// the consequence is a delete. A set that inherited when it should have
// overridden deletes on a host the operator declared read-only; a set that
// overrode when it should have inherited stops deleting on a host where
// that is the whole point of the configuration.
//
// The first test is the regression guarantee and the one to keep if
// anything ever has to go: a config that never mentions read_only anywhere
// has to resolve to false for every set, which is exactly what every
// deployment written before the field existed already does.

package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestReadOnlyDefaultsFalse is this issue's (#282) core regression
// guarantee at the config layer: a configuration written before this field
// existed, or one that simply never mentions read_only anywhere, resolves
// to ReadOnly == false for every backup set, exactly the delete-eligible
// behaviour every existing deployment already runs with.
func TestReadOnlyDefaultsFalse(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Sources[0].BackupSets[0].ReadOnly {
		t.Fatal("ReadOnly resolved true with neither the source nor the set ever mentioning read_only")
	}
}

// TestReadOnlySourceDefaultPropagatesToItsSets proves the source-level
// default the issue asks for: a source declared read_only: true makes
// every backup set under it read-only, with no per-set key required.
func TestReadOnlySourceDefaultPropagatesToItsSets(t *testing.T) {
	cfg := validConfig()
	cfg.Sources[0].ReadOnly = true

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !cfg.Sources[0].BackupSets[0].ReadOnly {
		t.Fatal("BackupSet.ReadOnly did not inherit the parent source's read_only default")
	}
}

// TestReadOnlyPerSetOverrideWinsOverSourceDefault proves the per-set
// override the issue calls "the minimum" shape: a set can declare
// read_only explicitly even when its source does not, and per-set always
// takes precedence over the source's default.
func TestReadOnlyPerSetOverrideWinsOverSourceDefault(t *testing.T) {
	trueVal := true
	falseVal := false

	t.Run("set opts in while source default is false", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].ReadOnly = false
		cfg.Sources[0].BackupSets[0].ReadOnlyConfig = &trueVal

		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !cfg.Sources[0].BackupSets[0].ReadOnly {
			t.Fatal("per-set read_only: true did not override the source's false default")
		}
	})

	t.Run("set opts out while source default is true", func(t *testing.T) {
		cfg := validConfig()
		cfg.Sources[0].ReadOnly = true
		cfg.Sources[0].BackupSets[0].ReadOnlyConfig = &falseVal

		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if cfg.Sources[0].BackupSets[0].ReadOnly {
			t.Fatal("per-set read_only: false did not override the source's true default")
		}
	})
}

// TestReadOnlyParsesFromYAML is the whole feature's entry point: an
// operator's actual file, not a struct literal a test built by hand. It
// pins the yaml key name (read_only) at both levels and proves the two
// resolve together exactly as the struct-literal tests above already
// prove piece by piece.
func TestReadOnlyParsesFromYAML(t *testing.T) {
	doc := []byte(`
poll_interval: 15m
state:
  database: /var/lib/backup-manager/state.db
sources:
  - id: production
    read_only: true
    backup_sets:
      - id: postgres-primary
        remote:
          type: sftp
          host: production.example.internal
          user: backup
          key_file: /run/secrets/backup_ssh_key
          known_hosts: /etc/backup-manager/known_hosts
        remote_path: /backups/postgres
        local_path: /backups/production/postgres
        stale_after: 30h
        completion:
          strategy: rename
      - id: staging-mirror
        read_only: false
        remote:
          type: sftp
          host: production.example.internal
          user: backup
          key_file: /run/secrets/backup_ssh_key
          known_hosts: /etc/backup-manager/known_hosts
        remote_path: /backups/staging
        local_path: /backups/production/staging
        stale_after: 30h
        completion:
          strategy: rename
`)

	var cfg Config
	if err := yaml.Unmarshal(doc, &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	sets := cfg.Sources[0].BackupSets
	if !sets[0].ReadOnly {
		t.Errorf("postgres-primary: ReadOnly = false, want true (inherited from the source)")
	}
	if sets[1].ReadOnly {
		t.Errorf("staging-mirror: ReadOnly = true, want false (explicit per-set override)")
	}
}
