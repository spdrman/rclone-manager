package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

func TestLoadParsesFullExample(t *testing.T) {
	cfg, err := Load("testdata/full.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got, want := cfg.PollInterval.Duration(), 15*time.Minute; got != want {
		t.Fatalf("PollInterval = %s, want %s", got, want)
	}
	if got, want := cfg.State.Database, "/var/lib/backup-manager/state.db"; got != want {
		t.Fatalf("State.Database = %q, want %q", got, want)
	}
	if len(cfg.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(cfg.Sources))
	}
	src := cfg.Sources[0]
	if src.Name != "production" {
		t.Fatalf("Sources[0].Name = %q, want %q", src.Name, "production")
	}
	if len(src.BackupSets) != 1 {
		t.Fatalf("expected 1 backup set, got %d", len(src.BackupSets))
	}
	bs := src.BackupSets[0]
	if bs.Name != "postgres-primary" {
		t.Fatalf("BackupSets[0].Name = %q, want %q", bs.Name, "postgres-primary")
	}
	// Load must not build the identity: that's Validate's job, done through
	// model.NewBackupSetID.
	if !bs.ID.IsZero() {
		t.Fatalf("Load populated BackupSet.ID = %#v, want the zero value until Validate runs", bs.ID)
	}
	if bs.Remote.Type != "sftp" || bs.Remote.Host != "production.example.internal" || bs.Remote.Port != 22 {
		t.Fatalf("Remote decoded wrong: %#v", bs.Remote)
	}
	if len(bs.Include) != 1 || bs.Include[0] != "*.dump.zst" {
		t.Fatalf("Include decoded wrong: %#v", bs.Include)
	}
	if bs.Completion.Strategy != "stable" || bs.Completion.StableFor.Duration() != 10*time.Minute || bs.Completion.DeleteSafetyDelay.Duration() != 60*time.Minute {
		t.Fatalf("Completion decoded wrong: %#v", bs.Completion)
	}
	if got, want := bs.StaleAfter.Duration(), 30*time.Hour; got != want {
		t.Fatalf("StaleAfter = %s, want %s", got, want)
	}
	if bs.Validation.Hash != "sha256" {
		t.Fatalf("Validation.Hash = %q, want %q", bs.Validation.Hash, "sha256")
	}
	if bs.Validation.Command == nil {
		t.Fatalf("Validation.Command = nil, want a populated command")
	}
	if bs.Validation.Command.Executable != "/usr/local/bin/validate-postgres-backup" {
		t.Fatalf("Command.Executable = %q", bs.Validation.Command.Executable)
	}
	if bs.Validation.Command.Timeout.Duration() != 10*time.Minute {
		t.Fatalf("Command.Timeout = %s", bs.Validation.Command.Timeout)
	}
	if got, want := bs.Revalidation.Interval.Duration(), 720*time.Hour; got != want {
		t.Fatalf("Revalidation.Interval = %s, want %s", got, want)
	}
	if bs.Revalidation.MaxPerCycle != 5 {
		t.Fatalf("Revalidation.MaxPerCycle = %d, want 5", bs.Revalidation.MaxPerCycle)
	}
	if !bs.Revalidation.Hash {
		t.Fatalf("Revalidation.Hash = false, want true")
	}
	if bs.Revalidation.Command == nil || bs.Revalidation.Command.Executable != "/usr/local/bin/validate-postgres-backup" {
		t.Fatalf("Revalidation.Command decoded wrong: %#v", bs.Revalidation.Command)
	}

	if cfg.Retention.Timezone != "America/Vancouver" {
		t.Fatalf("Retention.Timezone = %q", cfg.Retention.Timezone)
	}
	if cfg.Retention.DailyDays != 7 || cfg.Retention.WeeklyMonths != 3 || cfg.Retention.MonthlyMonths != 12 {
		t.Fatalf("Retention tiers decoded wrong: %#v", cfg.Retention)
	}
	if cfg.Retention.ProtectLastKnownGood == nil || !*cfg.Retention.ProtectLastKnownGood {
		t.Fatalf("Retention.ProtectLastKnownGood = %v, want true", cfg.Retention.ProtectLastKnownGood)
	}
}

func TestLoadThenValidateFullExample(t *testing.T) {
	cfg, err := Load("testdata/full.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	want, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("model.NewBackupSetID: %v", err)
	}
	got := cfg.Sources[0].BackupSets[0].ID
	if got != want {
		t.Fatalf("BackupSet.ID = %#v, want %#v (built through model.NewBackupSetID)", got, want)
	}
}

func TestLoadAndValidateMinimalExampleAppliesDefaults(t *testing.T) {
	cfg, err := LoadAndValidate("testdata/minimal.yaml")
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}

	// The whole retention block is absent from minimal.yaml. Every tier
	// should come back at its documented default, not its Go zero value.
	if cfg.Retention.DailyDays != 7 {
		t.Errorf("DailyDays = %d, want default 7", cfg.Retention.DailyDays)
	}
	if cfg.Retention.WeeklyMonths != 3 {
		t.Errorf("WeeklyMonths = %d, want default 3", cfg.Retention.WeeklyMonths)
	}
	if cfg.Retention.MonthlyMonths != 12 {
		t.Errorf("MonthlyMonths = %d, want default 12", cfg.Retention.MonthlyMonths)
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

	bs := cfg.Sources[0].BackupSets[0]
	if bs.ID.IsZero() {
		t.Errorf("BackupSet.ID is zero after Validate, want it populated")
	}
	if got, want := bs.ID.String(), "production/postgres-primary"; got != want {
		t.Errorf("BackupSet.ID = %q, want %q", got, want)
	}
}

// TestLoadParsesKeyCommand is #74's shape from a real YAML file, not just a
// Go literal: the command argv, unlike every other string field this
// package parses, has to survive as a []string, in order.
func TestLoadParsesKeyCommand(t *testing.T) {
	cfg, err := LoadAndValidate("testdata/key_resolvers.yaml")
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}
	r := cfg.Sources[0].BackupSets[0].Remote
	want := []string{"/usr/local/bin/op", "read", "op://infra/backup-manager/private-key"}
	if len(r.Key.Command) != len(want) {
		t.Fatalf("Key.Command = %#v, want %#v", r.Key.Command, want)
	}
	for i := range want {
		if r.Key.Command[i] != want[i] {
			t.Fatalf("Key.Command[%d] = %q, want %q", i, r.Key.Command[i], want[i])
		}
	}
	if r.KeyFile != "" {
		t.Fatalf("KeyFile = %q, want empty: a key.command source has no file to normalize into it", r.KeyFile)
	}
}

// TestLoadThenValidateTwoKeySourcesRejected proves the "exactly one" rule
// end to end from a real file: key_file and key.env both set is a config
// error, not a precedence order LoadAndValidate resolves silently.
func TestLoadThenValidateTwoKeySourcesRejected(t *testing.T) {
	_, err := LoadAndValidate("testdata/key_two_sources.yaml")
	if err == nil {
		t.Fatal("a config with both key_file and key.env set was accepted")
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected a *ValidationError, got %T: %v", err, err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("testdata/does-not-exist.yaml"); err == nil {
		t.Fatal("expected an error loading a missing file")
	}
}

func TestLoadAndValidatePropagatesLoadError(t *testing.T) {
	if _, err := LoadAndValidate("testdata/does-not-exist.yaml"); err == nil {
		t.Fatal("expected a Load error to propagate out of LoadAndValidate")
	}
}

func TestLoadAndValidatePropagatesValidateError(t *testing.T) {
	// This file parses cleanly (Load succeeds) but has no sources, so the
	// error must come from Validate, not Load.
	_, err := LoadAndValidate("testdata/invalid_no_sources.yaml")
	if err == nil {
		t.Fatal("expected a Validate error to propagate out of LoadAndValidate")
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected a *ValidationError, got %T: %v", err, err)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	if _, err := Load("testdata/empty.yaml"); err == nil {
		t.Fatal("expected an error loading an empty file")
	}
}

func TestLoadMalformedYAML(t *testing.T) {
	if _, err := Load("testdata/malformed.yaml"); err == nil {
		t.Fatal("expected a parse error loading malformed YAML")
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	_, err := Load("testdata/unknown_field.yaml")
	if err == nil {
		t.Fatal("expected an error for a config with an unknown top-level field")
	}
	// The point of KnownFields(true) is that a typo like "pol_interval"
	// is reported directly, not left to surface later as a mysteriously
	// zero poll_interval.
	if !strings.Contains(err.Error(), "pol_interval") {
		t.Fatalf("error %q does not mention the unknown field", err.Error())
	}
}
