package service

import (
	"context"
	"os"
	"strings"
	"testing"
)

// This file is the settings write path for issue #286's storage cap: the
// route the Settings page's cap field calls, and the refusals that stand
// between a typed number and a configuration this product cannot honour.
//
// Everything here goes through the same UpdateSettings the retention form
// already uses, on purpose: one enumerated write endpoint with one
// validate-then-persist-then-hot-reload sequence, so a cap edit cannot
// half-apply in a way a retention edit cannot.

func ptrInt64(n int64) *int64 { return &n }

// TestSettings_ReportsTheCapacityBlockInEffect is the read half. A form
// cannot render a cap it was never told, and it must be told the number in
// bytes, since the MB/GB choice is display only.
func TestSettings_ReportsTheCapacityBlockInEffect(t *testing.T) {
	configPath := writeTestConfigFileWithRetention(t, defaultChainRetentionBlock+
		"capacity:\n"+
		"  cap_bytes: 107374182400\n"+
		"  warning_free_bytes: 21474836480\n"+
		"  critical_free_bytes: 10737418240\n")

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cleanup() }()

	got, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if got.Capacity.CapBytes != 107374182400 {
		t.Errorf("CapBytes = %d, want 107374182400", got.Capacity.CapBytes)
	}
	if got.Capacity.WarningFreeBytes != 21474836480 || got.Capacity.CriticalFreeBytes != 10737418240 {
		t.Errorf("thresholds = %d / %d, want 21474836480 / 10737418240", got.Capacity.WarningFreeBytes, got.Capacity.CriticalFreeBytes)
	}
	if got.Capacity.BackupRoot == "" {
		t.Error("BackupRoot is empty: the form has to be able to say which filesystem the reading is taken from")
	}
	if got.Capacity.BackupRootConfigured {
		t.Error("BackupRootConfigured = true for a config that never named one: the derived answer must not read as an operator's choice")
	}
}

// TestSettings_ACapOfZeroReadsBackAsZero is the sentinel surviving a round
// trip through the API. Anything that "helpfully" substituted a number here
// would put a ceiling on a deployment that asked for none.
func TestSettings_ACapOfZeroReadsBackAsZero(t *testing.T) {
	svc, cleanup := openConfiguredService(t, defaultChainRetentionBlock)
	defer cleanup()

	got, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if got.Capacity.CapBytes != 0 {
		t.Errorf("CapBytes = %d for a config with no capacity block, want 0 (no cap)", got.Capacity.CapBytes)
	}
}

// TestUpdateSettings_PersistsACapAndHotReloadsIt is the write half, end to
// end: the file on disk carries the number, and the running process is
// enforcing it before the call returns.
func TestUpdateSettings_PersistsACapAndHotReloadsIt(t *testing.T) {
	svc, cleanup := openConfiguredService(t, defaultChainRetentionBlock)
	defer cleanup()

	const cap = int64(50) << 30
	got, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Capacity: &CapacityUpdate{CapBytes: ptrInt64(cap)},
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if got.Capacity.CapBytes != cap {
		t.Errorf("returned CapBytes = %d, want %d", got.Capacity.CapBytes, cap)
	}

	raw, err := os.ReadFile(svc.configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "cap_bytes: 53687091200") {
		t.Errorf("the config file does not carry the cap in bytes:\n%s", raw)
	}

	// The running process, not just the file: the whole point of a cap is
	// that the next transfer is weighed against it.
	if live := svc.state.Load().inner.Capacity.CapBytes; live != uint64(cap) {
		t.Errorf("the hot-reloaded service is enforcing a cap of %d, want %d", live, cap)
	}
}

// TestUpdateSettings_ACapOfZeroTurnsTheCapOff is the other direction, and
// the one a pointer field exists for: an explicit 0 is a request ("remove
// my cap"), not an omission.
func TestUpdateSettings_ACapOfZeroTurnsTheCapOff(t *testing.T) {
	svc, cleanup := openConfiguredService(t, defaultChainRetentionBlock+
		"capacity:\n  cap_bytes: 107374182400\n")
	defer cleanup()

	got, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Capacity: &CapacityUpdate{CapBytes: ptrInt64(0)},
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if got.Capacity.CapBytes != 0 {
		t.Errorf("CapBytes = %d, want 0: an explicit zero removes the cap", got.Capacity.CapBytes)
	}
	if live := svc.state.Load().inner.Capacity.CapBytes; live != 0 {
		t.Errorf("the hot-reloaded service still enforces a cap of %d", live)
	}
}

// TestUpdateSettings_RefusesANegativeCap sends the refusal all the way from
// config.Validate back out through the API boundary, so the operator sees
// the message that names the field and says what zero means.
func TestUpdateSettings_RefusesANegativeCap(t *testing.T) {
	svc, cleanup := openConfiguredService(t, defaultChainRetentionBlock)
	defer cleanup()

	before, err := os.ReadFile(svc.configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	_, err = svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Capacity: &CapacityUpdate{CapBytes: ptrInt64(-1)},
	})
	if err == nil {
		t.Fatal("UpdateSettings with a negative cap = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "cap_bytes") {
		t.Errorf("refusal %q never names the field", err)
	}

	after, err := os.ReadFile(svc.configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Error("the config file changed after a refused write: a refusal must leave both the file and the running policy exactly as they were")
	}
}

// TestUpdateSettings_RefusesACapUnderTheCriticalFloor is the joint refusal,
// driven through the write path rather than only through Validate: this is
// the combination an operator can actually produce by lowering a cap on a
// deployment that already has a floor set.
func TestUpdateSettings_RefusesACapUnderTheCriticalFloor(t *testing.T) {
	svc, cleanup := openConfiguredService(t, defaultChainRetentionBlock+
		"capacity:\n  warning_free_bytes: 21474836480\n  critical_free_bytes: 10737418240\n")
	defer cleanup()

	_, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Capacity: &CapacityUpdate{CapBytes: ptrInt64(1 << 30)},
	})
	if err == nil {
		t.Fatal("UpdateSettings with a 1 GiB cap under a 10 GiB critical floor = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "critical_free_bytes") {
		t.Errorf("refusal %q never names the floor the cap collides with", err)
	}
}

// TestUpdateSettings_PositiveControlForTheCapacityRefusals is what the two
// refusals above need to mean anything: the same call shape, with numbers
// that are fine, is accepted.
func TestUpdateSettings_PositiveControlForTheCapacityRefusals(t *testing.T) {
	svc, cleanup := openConfiguredService(t, defaultChainRetentionBlock+
		"capacity:\n  warning_free_bytes: 21474836480\n  critical_free_bytes: 10737418240\n")
	defer cleanup()

	got, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Capacity: &CapacityUpdate{CapBytes: ptrInt64(100 << 30)},
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v, want nil for a 100 GiB cap over a 10 GiB floor", err)
	}
	if got.Capacity.CapBytes != 100<<30 {
		t.Errorf("CapBytes = %d, want %d", got.Capacity.CapBytes, int64(100)<<30)
	}
}

// TestUpdateSettings_ACapacitySectionThatNamesNothingIsRefused extends the
// existing structural guard to the new section. `{"capacity":{}}` would
// otherwise rewrite the file, move ConfigRevision (invalidating every
// outstanding retention preview) and answer 200 for a request with no
// content.
func TestUpdateSettings_ACapacitySectionThatNamesNothingIsRefused(t *testing.T) {
	svc, cleanup := openConfiguredService(t, defaultChainRetentionBlock)
	defer cleanup()

	if _, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Capacity: &CapacityUpdate{},
	}); err == nil {
		t.Fatal("UpdateSettings with an empty capacity section = nil error, want a refusal")
	}
}

// TestUpdateSettings_ACapacityWriteLeavesRetentionAlone is the partial-
// update contract, now that there are two sections that could tread on
// each other.
func TestUpdateSettings_ACapacityWriteLeavesRetentionAlone(t *testing.T) {
	svc, cleanup := openConfiguredService(t, "retention:\n  timezone: Europe/Berlin\n  week_starts_on: sunday\n  daily_days: 14\n")
	defer cleanup()

	got, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Capacity: &CapacityUpdate{CapBytes: ptrInt64(1 << 40)},
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if got.Retention.Timezone != "Europe/Berlin" {
		t.Errorf("Timezone = %q, want Europe/Berlin: a capacity write must not touch retention", got.Retention.Timezone)
	}

	raw, err := os.ReadFile(svc.configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "daily_days: 14") {
		t.Errorf("the legacy retention spelling was rewritten by a capacity-only edit:\n%s", raw)
	}
}

// TestUpdateSettings_RetentionAndCapacityInOneCall is the other half: two
// sections in one request both apply, or the endpoint is not the generic
// one it claims to be.
func TestUpdateSettings_RetentionAndCapacityInOneCall(t *testing.T) {
	svc, cleanup := openConfiguredService(t, defaultChainRetentionBlock)
	defer cleanup()

	got, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Timezone: ptrString("Europe/Berlin")},
		Capacity:  &CapacityUpdate{CapBytes: ptrInt64(64 << 30)},
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if got.Retention.Timezone != "Europe/Berlin" {
		t.Errorf("Timezone = %q, want Europe/Berlin", got.Retention.Timezone)
	}
	if got.Capacity.CapBytes != 64<<30 {
		t.Errorf("CapBytes = %d, want %d", got.Capacity.CapBytes, int64(64)<<30)
	}
}

// TestUpdateSettings_ARequestWithNoSectionAtAllIsStillRefused keeps the
// existing guard honest now that "retention is nil" no longer implies
// "nothing was named".
func TestUpdateSettings_ARequestWithNoSectionAtAllIsStillRefused(t *testing.T) {
	svc, cleanup := openConfiguredService(t, defaultChainRetentionBlock)
	defer cleanup()

	if _, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{}); err == nil {
		t.Fatal("UpdateSettings naming no section = nil error, want a refusal")
	}
}

// openConfiguredService is the shared fixture: a real config file on disk,
// opened through the production constructor, so every write here goes
// through the same re-read/validate/persist/hot-reload sequence a running
// deployment does.
func openConfiguredService(t *testing.T, retention string) (*BackupService, func()) {
	t.Helper()
	configPath := writeTestConfigFileWithRetention(t, retention)
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return svc, func() { _ = cleanup() }
}
