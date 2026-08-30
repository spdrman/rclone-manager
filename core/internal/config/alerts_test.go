package config

import (
	"strings"
	"testing"
)

// TestValidate_AlertsDefaultToOff proves the proactive-alerting block is
// opt-in: a config file written before this block existed, or one that
// simply leaves it out, must not start notifying anyone
// (docs/EPIC-B-multi-nas.md §71, "one explicit opt-in ... mechanism").
func TestValidate_AlertsDefaultToOff(t *testing.T) {
	cfg := validConfig()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Alerts.Enabled {
		t.Error("alerts.enabled defaulted to true; it must be opt-in")
	}
}

// TestValidate_AlertsRepeatedFailureThresholdDefaults proves an operator
// who turns alerting on without naming a threshold gets the documented
// default rather than a literal zero, which would otherwise read as
// "alert on the very first failure".
func TestValidate_AlertsRepeatedFailureThresholdDefaults(t *testing.T) {
	cfg := validConfig()
	cfg.Alerts.Enabled = true

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Alerts.RepeatedFailureThreshold != DefaultRepeatedFailureThreshold {
		t.Errorf("alerts.repeated_failure_threshold = %d, want the documented default %d",
			cfg.Alerts.RepeatedFailureThreshold, DefaultRepeatedFailureThreshold)
	}
}

// TestValidate_AlertsRepeatedFailureThresholdMustBePositive proves a
// negative threshold is a refused config mistake, not something acted on.
func TestValidate_AlertsRepeatedFailureThresholdMustBePositive(t *testing.T) {
	cfg := validConfig()
	cfg.Alerts.Enabled = true
	cfg.Alerts.RepeatedFailureThreshold = -1

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a negative alerts.repeated_failure_threshold")
	}
	if !strings.Contains(err.Error(), "alerts.repeated_failure_threshold") {
		t.Errorf("Validate error = %q, want it to name the offending field", err)
	}
}

// TestValidate_AlertsExplicitThresholdIsLeftAlone proves Validate stays
// idempotent for this block too: a value the operator actually wrote is
// never overwritten by the default.
func TestValidate_AlertsExplicitThresholdIsLeftAlone(t *testing.T) {
	cfg := validConfig()
	cfg.Alerts.Enabled = true
	cfg.Alerts.RepeatedFailureThreshold = 11

	for pass := 1; pass <= 2; pass++ {
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate (pass %d): %v", pass, err)
		}
		if cfg.Alerts.RepeatedFailureThreshold != 11 {
			t.Fatalf("pass %d: threshold = %d, want the operator's own 11", pass, cfg.Alerts.RepeatedFailureThreshold)
		}
	}
}

// TestLoadParsesTheAlertsBlock proves the YAML key names in the full
// example file are the ones this package actually reads. Load uses
// KnownFields(true), so a renamed key here is a parse error rather than a
// silently-ignored field, which is exactly what makes this worth pinning.
func TestLoadParsesTheAlertsBlock(t *testing.T) {
	cfg, err := LoadAndValidate("testdata/full.yaml")
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}
	if !cfg.Alerts.Enabled {
		t.Error("alerts.enabled did not parse as true from the full example")
	}
	if cfg.Alerts.RepeatedFailureThreshold != 3 {
		t.Errorf("alerts.repeated_failure_threshold = %d, want 3", cfg.Alerts.RepeatedFailureThreshold)
	}
}

// TestMinimalConfigLeavesAlertingOff proves a config file with no alerts
// block at all, which is every config written before this work package,
// still loads and still notifies nobody.
func TestMinimalConfigLeavesAlertingOff(t *testing.T) {
	cfg, err := LoadAndValidate("testdata/minimal.yaml")
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}
	if cfg.Alerts.Enabled {
		t.Error("a config with no alerts block came back with alerting enabled")
	}
	if cfg.Alerts.RepeatedFailureThreshold != DefaultRepeatedFailureThreshold {
		t.Errorf("alerts.repeated_failure_threshold = %d, want the default %d even when the block is absent",
			cfg.Alerts.RepeatedFailureThreshold, DefaultRepeatedFailureThreshold)
	}
}
