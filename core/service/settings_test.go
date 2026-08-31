package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// defaultChainRetentionBlock is the retention block writeTestConfigFile
// already uses: neither a tiers list nor any of the three legacy scalars,
// so validateRetention resolves it to FR-18's documented 7/3/12 default.
const defaultChainRetentionBlock = "retention:\n  timezone: UTC\n  week_starts_on: monday\n"

func ptrString(s string) *string { return &s }
func ptrBool(b bool) *bool       { return &b }

// TestSettings_ReportsTheRunningRetentionPolicyAsAResolvedChain is the
// read half of issue #140's contract: whatever spelling the config file
// happens to use, Settings answers with the chain that is actually
// deciding, so a settings form renders the policy in effect rather than
// the sugar it was written with.
func TestSettings_ReportsTheRunningRetentionPolicyAsAResolvedChain(t *testing.T) {
	tests := []struct {
		name      string
		retention string
		want      []RetentionTier
	}{
		{
			name:      "a file with neither spelling resolves to the 7/3/12 default chain",
			retention: defaultChainRetentionBlock,
			want: []RetentionTier{
				{Name: "daily", Granularity: GranularityDay, Keep: 7},
				{Name: "weekly", Granularity: GranularityWeek, Keep: 3, WindowUnit: GranularityMonth},
				{Name: "monthly", Granularity: GranularityMonth, Keep: 12},
			},
		},
		{
			name: "the legacy scalars are reported as the chain they are sugar for",
			retention: "retention:\n" +
				"  timezone: UTC\n" +
				"  week_starts_on: monday\n" +
				"  daily_days: 14\n",
			want: []RetentionTier{
				{Name: "daily", Granularity: GranularityDay, Keep: 14},
				{Name: "weekly", Granularity: GranularityWeek, Keep: 3, WindowUnit: GranularityMonth},
				{Name: "monthly", Granularity: GranularityMonth, Keep: 12},
			},
		},
		{
			name: "an explicit chain is reported exactly as written, order included",
			retention: "retention:\n" +
				"  timezone: Europe/Berlin\n" +
				"  week_starts_on: sunday\n" +
				"  tiers:\n" +
				"    - name: fortnightly\n" +
				"      granularity: days\n" +
				"      period_days: 14\n" +
				"      keep: 6\n" +
				"    - name: annual\n" +
				"      granularity: year\n" +
				"      keep: 5\n",
			want: []RetentionTier{
				{Name: "fortnightly", Granularity: GranularityDays, PeriodDays: 14, Keep: 6},
				{Name: "annual", Granularity: GranularityYear, Keep: 5},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := writeTestConfigFileWithRetention(t, tt.retention)
			svc, cleanup, err := Open(context.Background(), configPath)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = cleanup() })

			got, err := svc.Settings(context.Background())
			if err != nil {
				t.Fatalf("Settings: %v", err)
			}
			assertTiersEqual(t, got.Retention.Tiers, tt.want)
			if !got.Retention.ProtectLastKnownGood {
				t.Error("ProtectLastKnownGood = false, want true (an absent key defaults to the safe reading)")
			}
		})
	}
}

func TestSettings_ReportsAnExplicitlyDisabledLastKnownGoodProtection(t *testing.T) {
	configPath := writeTestConfigFileWithRetention(t, defaultChainRetentionBlock+"  protect_last_known_good: false\n")
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	got, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if got.Retention.ProtectLastKnownGood {
		t.Error("ProtectLastKnownGood = true, want false (the file says false explicitly)")
	}
}

// TestUpdateSettings_PersistsARetentionChainAndHotReloadsIt is the write
// half: the chain lands in the file, the file still loads, and the SAME
// BackupService reports the new policy with no restart.
func TestUpdateSettings_PersistsARetentionChainAndHotReloadsIt(t *testing.T) {
	svc, configPath := openTestService(t)
	revisionBefore := svc.ConfigRevision()

	want := []RetentionTier{
		{Name: "daily", Granularity: GranularityDay, Keep: 10},
		{Name: "fortnightly", Granularity: GranularityDays, PeriodDays: 14, Keep: 6},
		{Name: "annual", Granularity: GranularityYear, Keep: 5},
	}
	got, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Tiers: want},
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	assertTiersEqual(t, got.Retention.Tiers, want)

	// Visible to the same service, with no restart.
	after, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings after update: %v", err)
	}
	assertTiersEqual(t, after.Retention.Tiers, want)

	if svc.ConfigRevision() == revisionBefore {
		t.Errorf("ConfigRevision is still %q after a settings write; the hot reload did not recompute it", revisionBefore)
	}

	// And durably, in a form the daemon's own next start accepts.
	reloaded, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("the config this product wrote no longer loads: %v", err)
	}
	if len(reloaded.Retention.Tiers) != len(want) {
		t.Fatalf("reloaded tiers = %d, want %d", len(reloaded.Retention.Tiers), len(want))
	}
	if reloaded.Retention.Tiers[1].PeriodDays != 14 {
		t.Errorf("reloaded tiers[1].PeriodDays = %d, want 14", reloaded.Retention.Tiers[1].PeriodDays)
	}
}

// TestUpdateSettings_NeverWritesBothRetentionSpellings is the invariant
// issue #156's schema makes load-bearing: daily_days/weekly_months/
// monthly_months and tiers are mutually exclusive, and config.Validate
// refuses a file carrying both. A settings form writing a chain onto a
// config that used the scalars has to clear them, not sit beside them.
func TestUpdateSettings_NeverWritesBothRetentionSpellings(t *testing.T) {
	configPath := writeTestConfigFileWithRetention(t, "retention:\n"+
		"  timezone: UTC\n"+
		"  week_starts_on: monday\n"+
		"  daily_days: 7\n"+
		"  weekly_months: 3\n"+
		"  monthly_months: 12\n")
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	if _, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Tiers: []RetentionTier{
			{Name: "daily", Granularity: GranularityDay, Keep: 7},
			{Name: "annual", Granularity: GranularityYear, Keep: 5},
		}},
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	got := string(written)

	// Positive control: without this, the three absence assertions below
	// would pass just as happily against a file that lost the chain too.
	if !strings.Contains(got, "name: annual") {
		t.Fatalf("the written config does not carry the submitted chain, so nothing below is being measured:\n%s", got)
	}
	for _, key := range []string{"daily_days", "weekly_months", "monthly_months"} {
		if strings.Contains(got, key) {
			t.Errorf("the written config carries %s alongside the submitted tiers list, the one combination config.Validate refuses:\n%s", key, got)
		}
	}
	if _, err := config.LoadAndValidate(configPath); err != nil {
		t.Fatalf("the config this product wrote no longer loads: %v", err)
	}
}

// TestUpdateSettings_LeavingTheChainUnnamedPreservesALegacyFilesSpelling
// is the control that gives the test above its teeth: clearing the
// scalars is a consequence of submitting a chain, not something
// UpdateSettings does to every config it touches. An operator who only
// flips protect_last_known_good must not have their file silently
// migrated to the general spelling.
func TestUpdateSettings_LeavingTheChainUnnamedPreservesALegacyFilesSpelling(t *testing.T) {
	configPath := writeTestConfigFileWithRetention(t, "retention:\n"+
		"  timezone: UTC\n"+
		"  week_starts_on: monday\n"+
		"  daily_days: 7\n")
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	if _, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{ProtectLastKnownGood: ptrBool(false)},
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	got := string(written)
	if !strings.Contains(got, "daily_days: 7") {
		t.Errorf("the written config lost the operator's own daily_days spelling:\n%s", got)
	}
	if strings.Contains(got, "tiers:") {
		t.Errorf("the written config migrated a legacy file to a tiers list nobody asked for:\n%s", got)
	}
	if !strings.Contains(got, "protect_last_known_good: false") {
		t.Errorf("the written config does not carry the change that was actually requested:\n%s", got)
	}
}

// TestUpdateSettings_RefusesAnEmptyRetentionChain guards the one shape a
// form can produce that the schema reads as the opposite of what the
// operator meant: an emptied tiers list is not "keep nothing", it
// reinstates FR-18's 7/3/12 default (Retention.Tiers' own doc). Refusing
// it here is what stops "I removed every tier" silently widening
// retention instead of narrowing it.
func TestUpdateSettings_RefusesAnEmptyRetentionChain(t *testing.T) {
	svc, configPath := openTestService(t)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}

	_, err = svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Tiers: []RetentionTier{}},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("UpdateSettings error = %v, want ErrInvalidRequest", err)
	}
	// The refusal has to name the fallback, or an operator reads it as
	// "the list cannot be empty" and never learns what empty means.
	if !strings.Contains(err.Error(), "default") {
		t.Errorf("the refusal does not tell the operator that an empty chain reinstates the default policy: %v", err)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if string(after) != string(before) {
		t.Error("a refused settings write still rewrote the config file")
	}
}

// TestUpdateSettings_RefusesInsteadOfPartiallyApplying walks every rule
// config.Validate enforces on a tier and proves each one is refused with
// the config file left byte-for-byte untouched.
func TestUpdateSettings_RefusesInsteadOfPartiallyApplying(t *testing.T) {
	tests := []struct {
		name    string
		update  RetentionUpdate
		wantMsg string
	}{
		{
			name:    "a tier name outside lower_snake_case",
			update:  RetentionUpdate{Tiers: []RetentionTier{{Name: "Daily", Granularity: GranularityDay, Keep: 7}}},
			wantMsg: "lower_snake_case",
		},
		{
			name:    "the reserved last_known_good tier name",
			update:  RetentionUpdate{Tiers: []RetentionTier{{Name: TierLastKnownGoodName, Granularity: GranularityDay, Keep: 7}}},
			wantMsg: "reserved",
		},
		{
			name: "a duplicate tier name",
			update: RetentionUpdate{Tiers: []RetentionTier{
				{Name: "daily", Granularity: GranularityDay, Keep: 7},
				{Name: "daily", Granularity: GranularityWeek, Keep: 3},
			}},
			wantMsg: "duplicate",
		},
		{
			name:    "a granularity outside the closed set",
			update:  RetentionUpdate{Tiers: []RetentionTier{{Name: "daily", Granularity: "fortnight", Keep: 7}}},
			wantMsg: "granularity",
		},
		{
			name:    "a zero keep window",
			update:  RetentionUpdate{Tiers: []RetentionTier{{Name: "daily", Granularity: GranularityDay, Keep: 0}}},
			wantMsg: "keep",
		},
		{
			name:    "a keep window past the ceiling",
			update:  RetentionUpdate{Tiers: []RetentionTier{{Name: "daily", Granularity: GranularityDay, Keep: RetentionTierKeepMax + 1}}},
			wantMsg: "keep",
		},
		{
			name:    "period_days on a granularity that has no use for it",
			update:  RetentionUpdate{Tiers: []RetentionTier{{Name: "daily", Granularity: GranularityDay, Keep: 7, PeriodDays: 14}}},
			wantMsg: "period_days",
		},
		{
			name:    "a custom period with no period_days",
			update:  RetentionUpdate{Tiers: []RetentionTier{{Name: "custom", Granularity: GranularityDays, Keep: 6}}},
			wantMsg: "period_days",
		},
		{
			name:    "period_days past the ceiling",
			update:  RetentionUpdate{Tiers: []RetentionTier{{Name: "custom", Granularity: GranularityDays, Keep: 6, PeriodDays: RetentionTierPeriodDaysMax + 1}}},
			wantMsg: "period_days",
		},
		{
			name:    "window_unit spelled as the custom period",
			update:  RetentionUpdate{Tiers: []RetentionTier{{Name: "weekly", Granularity: GranularityWeek, Keep: 3, WindowUnit: GranularityDays}}},
			wantMsg: "window_unit",
		},
		{
			name:    "a timezone that is not loadable",
			update:  RetentionUpdate{Timezone: ptrString("Mars/Olympus_Mons")},
			wantMsg: "timezone",
		},
		{
			name:    "a week_starts_on that is not a weekday",
			update:  RetentionUpdate{WeekStartsOn: ptrString("caturday")},
			wantMsg: "week_starts_on",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, configPath := openTestService(t)
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("reading the config: %v", err)
			}

			update := tt.update
			_, err = svc.UpdateSettings(context.Background(), UpdateSettingsRequest{Retention: &update})
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("UpdateSettings error = %v, want ErrInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("refusal %q does not mention %q", err, tt.wantMsg)
			}

			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("reading the config back: %v", err)
			}
			if string(after) != string(before) {
				t.Errorf("a refused settings write still rewrote the config file:\nbefore:\n%s\nafter:\n%s", before, after)
			}

			// Nothing partially applied in memory either.
			live, err := svc.Settings(context.Background())
			if err != nil {
				t.Fatalf("Settings: %v", err)
			}
			if live.Retention.Timezone != "UTC" || live.Retention.WeekStartsOn != "monday" {
				t.Errorf("the running policy moved on a refused write: %+v", live.Retention)
			}
		})
	}
}

// TestUpdateSettings_PositiveControlForTheRefusalCases proves the table
// above is measuring refusals rather than a method that refuses
// everything: the same shapes, spelled legally, are accepted and written.
func TestUpdateSettings_PositiveControlForTheRefusalCases(t *testing.T) {
	svc, configPath := openTestService(t)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}

	got, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{
			Timezone:     ptrString("Europe/Berlin"),
			WeekStartsOn: ptrString("sunday"),
			Tiers: []RetentionTier{
				{Name: "daily", Granularity: GranularityDay, Keep: 7},
				{Name: "custom", Granularity: GranularityDays, Keep: 6, PeriodDays: 14},
				{Name: "weekly", Granularity: GranularityWeek, Keep: 3, WindowUnit: GranularityMonth},
			},
		},
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if got.Retention.Timezone != "Europe/Berlin" || got.Retention.WeekStartsOn != "sunday" {
		t.Errorf("Retention = %+v, want Europe/Berlin + sunday", got.Retention)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if string(after) == string(before) {
		t.Error("an accepted settings write left the config file unchanged, so the refusal table's file comparison proves nothing")
	}
}

// TestUpdateSettings_DisablingLastKnownGoodProtectionIsHonouredExactly:
// protect_last_known_good is a *bool in the schema precisely so an
// explicit false and an absent key stay distinguishable. A settings write
// that turns it off has to produce the explicit false, not an omitted key
// that reads back as "protected".
func TestUpdateSettings_DisablingLastKnownGoodProtectionIsHonouredExactly(t *testing.T) {
	svc, configPath := openTestService(t)

	got, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{ProtectLastKnownGood: ptrBool(false)},
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if got.Retention.ProtectLastKnownGood {
		t.Fatal("ProtectLastKnownGood = true immediately after being turned off")
	}

	reloaded, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}
	if reloaded.Retention.ProtectLastKnownGood == nil {
		t.Fatal("protect_last_known_good came back absent; an absent key defaults to true, which is the opposite of what was written")
	}
	if *reloaded.Retention.ProtectLastKnownGood {
		t.Error("protect_last_known_good came back true after being turned off")
	}

	// Turning it back on is honoured just as exactly (the control for the
	// assertion above: it must be able to report both values).
	if _, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{ProtectLastKnownGood: ptrBool(true)},
	}); err != nil {
		t.Fatalf("UpdateSettings (re-enable): %v", err)
	}
	back, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if !back.Retention.ProtectLastKnownGood {
		t.Error("ProtectLastKnownGood stayed false after being turned back on")
	}
}

// TestUpdateSettings_RereadsTheFileSoAnOutOfBandEditIsNotClobbered
// mirrors CreateBackupSet's own "always read fresh" discipline: a change
// made to the config file by hand (or by a second process) since this
// service loaded it survives a settings write that does not touch it.
func TestUpdateSettings_RereadsTheFileSoAnOutOfBandEditIsNotClobbered(t *testing.T) {
	svc, configPath := openTestService(t)

	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}
	edited := strings.Replace(string(original), "poll_interval: 15m", "poll_interval: 45m", 1)
	if edited == string(original) {
		t.Fatal("the out-of-band edit did not change anything, so this test would prove nothing")
	}
	if err := os.WriteFile(configPath, []byte(edited), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{ProtectLastKnownGood: ptrBool(false)},
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	reloaded, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}
	if reloaded.PollInterval.Duration().String() != "45m0s" {
		t.Errorf("poll_interval = %s, want 45m0s; the settings write clobbered an out-of-band edit with its own stale in-memory copy", reloaded.PollInterval.Duration())
	}
}

func TestUpdateSettings_RefusesARequestThatNamesNothing(t *testing.T) {
	svc, _ := openTestService(t)

	if _, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("UpdateSettings error = %v, want ErrInvalidRequest", err)
	}
}

func TestUpdateSettings_WithoutAConfigFileReturnsErrConfigNotFileBacked(t *testing.T) {
	svc := newTestService(t)

	_, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{ProtectLastKnownGood: ptrBool(false)},
	})
	if !errors.Is(err, ErrConfigNotFileBacked) {
		t.Fatalf("UpdateSettings error = %v, want ErrConfigNotFileBacked", err)
	}
}

// TestRetentionSchema_MatchesWhatConfigValidateActuallyEnforces keeps the
// closed value sets a settings form validates against from drifting away
// from the ones config.Validate applies server-side. Both come from
// internal/config, so this asserts the re-export is complete rather than
// re-deriving the lists here.
func TestRetentionSchema_MatchesWhatConfigValidateActuallyEnforces(t *testing.T) {
	schema := RetentionSchema()

	if len(schema.Granularities) != 7 {
		t.Errorf("Granularities = %v, want all seven config.Granularity* constants", schema.Granularities)
	}
	for _, g := range schema.Granularities {
		tier := config.RetentionTier{Name: "probe", Granularity: g, Keep: 1}
		if g == config.GranularityDays {
			tier.PeriodDays = 1
		}
		r := config.Retention{Tiers: []config.RetentionTier{tier}}
		if err := config.ValidateRetention(&r); err != nil {
			t.Errorf("granularity %q is advertised by the schema but refused by config.ValidateRetention: %v", g, err)
		}
	}

	// window_unit accepts every granularity except the custom period.
	if len(schema.WindowUnits) != len(schema.Granularities)-1 {
		t.Errorf("WindowUnits = %v, want the granularity list minus %q", schema.WindowUnits, config.GranularityDays)
	}
	for _, u := range schema.WindowUnits {
		if u == config.GranularityDays {
			t.Errorf("WindowUnits advertises %q, which config.ValidateRetention refuses outright", u)
		}
	}

	if schema.ReservedTierName != config.TierLastKnownGoodName {
		t.Errorf("ReservedTierName = %q, want %q", schema.ReservedTierName, config.TierLastKnownGoodName)
	}
	if schema.KeepMax != RetentionTierKeepMax || schema.PeriodDaysMax != RetentionTierPeriodDaysMax {
		t.Errorf("schema ceilings = %d/%d, want %d/%d", schema.KeepMax, schema.PeriodDaysMax, RetentionTierKeepMax, RetentionTierPeriodDaysMax)
	}

	// The ceilings are the ones actually enforced, not numbers that merely
	// look like them: one past each is refused.
	overKeep := config.Retention{Tiers: []config.RetentionTier{{Name: "probe", Granularity: config.GranularityDay, Keep: schema.KeepMax + 1}}}
	if err := config.ValidateRetention(&overKeep); err == nil {
		t.Errorf("keep = %d (one past the advertised ceiling) was accepted", schema.KeepMax+1)
	}
	overPeriod := config.Retention{Tiers: []config.RetentionTier{{Name: "probe", Granularity: config.GranularityDays, Keep: 1, PeriodDays: schema.PeriodDaysMax + 1}}}
	if err := config.ValidateRetention(&overPeriod); err == nil {
		t.Errorf("period_days = %d (one past the advertised ceiling) was accepted", schema.PeriodDaysMax+1)
	}
	// Positive control for the two assertions above: the ceilings
	// themselves are accepted, so "one past is refused" is measuring a
	// boundary rather than a rule that refuses every large number.
	atKeep := config.Retention{Tiers: []config.RetentionTier{{Name: "probe", Granularity: config.GranularityDay, Keep: schema.KeepMax}}}
	if err := config.ValidateRetention(&atKeep); err != nil {
		t.Errorf("keep = %d (exactly the advertised ceiling) was refused: %v", schema.KeepMax, err)
	}
}

func assertTiersEqual(t *testing.T, got, want []RetentionTier) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tiers = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tiers[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
