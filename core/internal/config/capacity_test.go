package config

import (
	"strings"
	"testing"
)

// This file covers the capacity block issue #286 adds: the operator's cap
// on how much space this manager may occupy, and the FR-21 warning /
// critical / safety-margin numbers internal/capacity has always taken as
// inputs and internal/config has never carried.
//
// They land together on purpose. handlers_storage.go said the two
// thresholds were "structurally zero until internal/config grows capacity
// fields"; the cap Rom asked for IS those capacity fields, so adding one
// and deferring the others would have meant writing this block twice.

const gib = int64(1) << 30

// capacityConfig is a minimal valid config with the capacity block under
// test, so every case below differs only in the thing it is about.
func capacityConfig(t *testing.T, c Capacity) *Config {
	t.Helper()
	return &Config{
		PollInterval: Duration(15 * 60 * 1e9),
		State:        State{Database: "/data/state/backup.db"},
		Capacity:     c,
		Sources: []Source{{
			Name: "production",
			BackupSets: []BackupSet{{
				Name:       "postgres",
				RemotePath: "/srv/out",
				LocalPath:  "/data/backups/production/postgres",
				Remote:     Remote{Type: "local"},
				Completion: Completion{Strategy: "marker"},
				StaleAfter: Duration(24 * 60 * 60 * 1e9),
			}},
		}},
	}
}

// TestACapOfZeroIsNoCapAndValidates is the sentinel Rom asked for, pinned
// at the layer that would otherwise be tempted to "helpfully" resolve it to
// something. Completion.DeleteSafetyDelay resolves a zero to a documented
// default precisely because reading ITS zero literally would turn a safety
// gate off; a zero here has to survive untouched for the opposite reason.
func TestACapOfZeroIsNoCapAndValidates(t *testing.T) {
	cfg := capacityConfig(t, Capacity{})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil: a config that never mentions a cap is the product default", err)
	}
	if cfg.Capacity.CapBytes != 0 {
		t.Errorf("CapBytes = %d after Validate, want 0: nothing may resolve the no-cap sentinel to a number", cfg.Capacity.CapBytes)
	}
}

// TestANegativeCapIsRefused is the other half of the sentinel's validation:
// zero has a meaning, and everything below zero has none at all.
func TestANegativeCapIsRefused(t *testing.T) {
	cfg := capacityConfig(t, Capacity{CapBytes: -1})
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil for a negative cap, want a refusal")
	}
	if !strings.Contains(err.Error(), "capacity.cap_bytes") {
		t.Errorf("refusal %q never names the field an operator has to fix", err)
	}
	// The message has to say what zero means, or an operator who wanted
	// "no cap" and typed -1 is told only that -1 is wrong.
	if !strings.Contains(err.Error(), "0") || !strings.Contains(err.Error(), "no cap") {
		t.Errorf("refusal %q never says that 0 is the way to ask for no cap", err)
	}
}

// TestNegativeThresholdsAreRefused covers the other three byte counts. They
// are separate assertions rather than one loop over a slice because the
// field name in each message is the whole value of the message.
func TestNegativeThresholdsAreRefused(t *testing.T) {
	for field, c := range map[string]Capacity{
		"capacity.warning_free_bytes":  {WarningFreeBytes: -1},
		"capacity.critical_free_bytes": {CriticalFreeBytes: -1},
		"capacity.safety_margin_bytes": {SafetyMarginBytes: -1},
	} {
		t.Run(field, func(t *testing.T) {
			err := capacityConfig(t, c).Validate()
			if err == nil {
				t.Fatalf("Validate() = nil for a negative %s, want a refusal", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("refusal %q never names %s", err, field)
			}
		})
	}
}

// TestAWarningBelowCriticalIsRefusedAtLoad moves internal/capacity's own
// Thresholds.Validate rule forward to config time. Without it the pair is
// accepted here and then refuses at every assessment, which surfaces as
// every backup set reading "misconfigured" on the storage panel with
// nothing naming the two numbers that did it.
func TestAWarningBelowCriticalIsRefusedAtLoad(t *testing.T) {
	cfg := capacityConfig(t, Capacity{WarningFreeBytes: 5 * gib, CriticalFreeBytes: 10 * gib})
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil for a warning line below the critical floor, want a refusal")
	}
	if !strings.Contains(err.Error(), "warning_free_bytes") || !strings.Contains(err.Error(), "critical_free_bytes") {
		t.Errorf("refusal %q does not name both numbers, so an operator cannot tell which to move", err)
	}
}

// TestACriticalFloorOnlyIsRefused is the case the rule above exists for and
// the one an operator is most likely to write: setting a critical floor and
// leaving the warning line at its "off" zero. That pair means "warn me
// never, refuse me at 10 GB", which internal/capacity cannot honour.
func TestACriticalFloorOnlyIsRefused(t *testing.T) {
	err := capacityConfig(t, Capacity{CriticalFreeBytes: 10 * gib}).Validate()
	if err == nil {
		t.Fatal("Validate() = nil for a critical floor with no warning line, want a refusal")
	}
}

// TestACapUnderTheCriticalFloorIsRefused catches a configuration that is
// individually valid in every field and jointly means "refuse every
// transfer forever". Left to run, it looks exactly like a broken product.
func TestACapUnderTheCriticalFloorIsRefused(t *testing.T) {
	cfg := capacityConfig(t, Capacity{
		CapBytes:          10 * gib,
		WarningFreeBytes:  40 * gib,
		CriticalFreeBytes: 20 * gib,
	})
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil for a cap at or below the critical floor, want a refusal")
	}
	if !strings.Contains(err.Error(), "cap_bytes") {
		t.Errorf("refusal %q never names the cap", err)
	}
}

// TestAValidCapacityBlockSurvivesValidate is the positive control the
// refusals above need: a rule that refuses everything passes every refusal
// test there is.
func TestAValidCapacityBlockSurvivesValidate(t *testing.T) {
	cfg := capacityConfig(t, Capacity{
		CapBytes:          100 * gib,
		WarningFreeBytes:  20 * gib,
		CriticalFreeBytes: 10 * gib,
		SafetyMarginBytes: 1 * gib,
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// ---------------------------------------------------------------------------
// The backup root: which filesystem the reading is taken from
// ---------------------------------------------------------------------------

// TestBackupRootDefaultsToTheCommonAncestorOfEveryLocalPath is what makes
// the storage panel work with no extra configuration at all. The container
// bind-mounts one backup volume and every set lands under it, so the
// directory every local_path shares IS the filesystem to measure.
func TestBackupRootDefaultsToTheCommonAncestorOfEveryLocalPath(t *testing.T) {
	cfg := capacityConfig(t, Capacity{})
	cfg.Sources[0].BackupSets = append(cfg.Sources[0].BackupSets, BackupSet{
		Name:       "media",
		RemotePath: "/srv/media",
		LocalPath:  "/data/backups/media",
		Remote:     Remote{Type: "local"},
		Completion: Completion{Strategy: "marker"},
		StaleAfter: Duration(24 * 60 * 60 * 1e9),
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.EffectiveBackupRoot(); got != "/data/backups" {
		t.Errorf("EffectiveBackupRoot() = %q, want %q", got, "/data/backups")
	}
}

// TestASingleBackupSetIsItsOwnBackupRoot: with one set there is nothing to
// find in common, and the set's own destination is the honest answer.
func TestASingleBackupSetIsItsOwnBackupRoot(t *testing.T) {
	cfg := capacityConfig(t, Capacity{})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.EffectiveBackupRoot(); got != "/data/backups/production/postgres" {
		t.Errorf("EffectiveBackupRoot() = %q, want %q", got, "/data/backups/production/postgres")
	}
}

// TestAnExplicitBackupRootWins lets an operator (or a platform package)
// name the mount directly rather than have it inferred, which is the answer
// for a deployment whose sets genuinely do sit on different volumes.
func TestAnExplicitBackupRootWins(t *testing.T) {
	cfg := capacityConfig(t, Capacity{BackupRoot: "/volume1/backups/rclone-manager"})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.EffectiveBackupRoot(); got != "/volume1/backups/rclone-manager" {
		t.Errorf("EffectiveBackupRoot() = %q, want the configured root", got)
	}
}

// TestARelativeBackupRootIsRefused holds the new path to the same standard
// local_path and state.database are already held to.
func TestARelativeBackupRootIsRefused(t *testing.T) {
	err := capacityConfig(t, Capacity{BackupRoot: "backups/../etc"}).Validate()
	if err == nil {
		t.Fatal("Validate() = nil for a relative, traversing backup_root, want a refusal")
	}
	if !strings.Contains(err.Error(), "capacity.backup_root") {
		t.Errorf("refusal %q never names the field", err)
	}
}

// TestSetsOnDifferentVolumesHaveNoDerivedBackupRoot is the container-mount
// trap, refused rather than answered. Two sets whose only shared ancestor
// is "/" would derive the container's own root filesystem, which reports a
// confident number about the wrong disk: worse than reporting nothing,
// because nobody would notice. An empty answer means "not known yet", which
// a caller must render as exactly that.
func TestSetsOnDifferentVolumesHaveNoDerivedBackupRoot(t *testing.T) {
	cfg := capacityConfig(t, Capacity{})
	cfg.Sources[0].BackupSets = append(cfg.Sources[0].BackupSets, BackupSet{
		Name:       "media",
		RemotePath: "/srv/media",
		LocalPath:  "/mnt/other-volume/media",
		Remote:     Remote{Type: "local"},
		Completion: Completion{Strategy: "marker"},
		StaleAfter: Duration(24 * 60 * 60 * 1e9),
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.EffectiveBackupRoot(); got != "" {
		t.Errorf("EffectiveBackupRoot() = %q, want \"\": the only ancestor these two share is the container's own root filesystem", got)
	}
}

// TestAnExplicitRootStillWinsAcrossVolumes: the derivation gives up, but an
// operator who names the mount is answered.
func TestAnExplicitRootStillWinsAcrossVolumes(t *testing.T) {
	cfg := capacityConfig(t, Capacity{BackupRoot: "/mnt"})
	cfg.Sources[0].BackupSets[0].LocalPath = "/mnt/one/x"
	cfg.Sources[0].BackupSets = append(cfg.Sources[0].BackupSets, BackupSet{
		Name:       "media",
		RemotePath: "/srv/media",
		LocalPath:  "/mnt/two/y",
		Remote:     Remote{Type: "local"},
		Completion: Completion{Strategy: "marker"},
		StaleAfter: Duration(24 * 60 * 60 * 1e9),
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.EffectiveBackupRoot(); got != "/mnt" {
		t.Errorf("EffectiveBackupRoot() = %q, want %q", got, "/mnt")
	}
}
