// This file covers the deployment-wide settings write, which is the one
// route that edits an operator's config.yaml wholesale rather than one
// field of one set.
//
// That makes the file itself the thing under test as much as the values
// in it. A save has to re-read from disk rather than serialise this
// process's memory, so an out-of-band edit is not clobbered; it must not
// write both spellings of a retention policy; it must keep a legacy
// file's own spelling when the request says nothing about it; and it must
// not freeze this release's resolved defaults into a file whose owner
// never chose them. Every one of those is a way a save can succeed and
// still leave an operator with a file they did not write.
//
// The refusals are proved to be all-or-nothing, each with a positive
// control beside it. A test that asserts "the file is unchanged" passes
// perfectly on a build where the write never happens at all, so the
// control is what keeps the refusal cases honest.
//
// The two schema tests at the end guard the other direction: what this
// boundary advertises as valid has to be what the config layer actually
// enforces, and the advertised defaults have to be what an unconfigured
// policy really resolves to rather than a copy that drifts.
package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

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

// TestUpdateSettings_RefusesARequestThatNamesNothing covers both spellings
// of "nothing". The second is mandatory review finding M3: a present but
// entirely empty retention section passed the old per-section guard, so a
// zero-content request re-marshalled and rewrote the operator's config
// file, moved ConfigRevision (invalidating every outstanding retention
// preview for every backup set) and answered success. The refusal has to be
// structural, and the file has to be untouched by it.
func TestUpdateSettings_RefusesARequestThatNamesNothing(t *testing.T) {
	tests := []struct {
		name string
		req  UpdateSettingsRequest
	}{
		{name: "no section at all", req: UpdateSettingsRequest{}},
		{name: "a retention section naming no field", req: UpdateSettingsRequest{Retention: &RetentionUpdate{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, configPath := openTestService(t)
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			revisionBefore := svc.ConfigRevision()

			if _, err := svc.UpdateSettings(context.Background(), tt.req); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("UpdateSettings error = %v, want ErrInvalidRequest", err)
			}

			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if string(after) != string(before) {
				t.Errorf("a refused request rewrote the configuration file:\n--- before ---\n%s\n--- after ---\n%s", before, after)
			}
			if svc.ConfigRevision() != revisionBefore {
				t.Errorf("a refused request moved ConfigRevision from %q to %q, invalidating every outstanding retention preview", revisionBefore, svc.ConfigRevision())
			}
		})
	}
}

// TestUpdateSettings_PositiveControlForTheNamesNothingRefusal proves the
// table above measures something: the identical fixture, with one field
// actually named, does rewrite the file and does move ConfigRevision.
func TestUpdateSettings_PositiveControlForTheNamesNothingRefusal(t *testing.T) {
	svc, configPath := openTestService(t)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	revisionBefore := svc.ConfigRevision()

	if _, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Timezone: ptrString("Europe/Berlin")},
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) == string(before) {
		t.Error("control failed: a request that DOES name a setting left the file unchanged, so the refusal assertions above prove nothing")
	}
	if svc.ConfigRevision() == revisionBefore {
		t.Error("control failed: a request that DOES name a setting left ConfigRevision unchanged")
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

// TestUpdateSettings_HotReloadStillResolvesEveryRegisteredValidator is
// mandatory review finding M1 on PR #171. UpdateSettings is the third
// config-loading, state-swapping path in this package, and it was the only
// one that skipped validator resolution: config.Load reads
// validation.validator_id and never turns it into the
// validation.Command internal/lifecycle/verify.go runs, so hot-reloading
// without planValidatorCatalog swapped in an app.Service where every
// registered-validator backup set held an id and a nil command. That
// combination is exactly what config.Validation.ResolvedCommand refuses
// with ErrValidatorNotResolved, so every artifact in the deployment failed
// verification until the process was restarted, triggered by a settings
// write that did not mention validators at all.
//
// The retention field this test PATCHes is deliberately unrelated to
// validation, because the report is that an unrelated edit breaks it.
func TestUpdateSettings_HotReloadStillResolvesEveryRegisteredValidator(t *testing.T) {
	configPath, _, _ := writeValidatorConfigFile(t, string(ValidatorTrailerMarker), []byte("payload"))
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	ctx := context.Background()

	// Positive control. Without this the assertion after the PATCH could
	// pass against a fixture whose validator was never resolved in the
	// first place, which is the one way a test of "resolution survives X"
	// proves nothing at all.
	before := findBackupSet(svc.state.Load().inner.Config, "production", "postgres-primary")
	beforeCmd, err := before.Validation.ResolvedCommand()
	if err != nil {
		t.Fatalf("precondition failed: the freshly opened service has not resolved its validator: %v", err)
	}
	if beforeCmd == nil {
		t.Fatal("precondition failed: the fixture's backup set names no validator, so this test would prove nothing")
	}

	if _, err := svc.UpdateSettings(ctx, UpdateSettingsRequest{
		Retention: &RetentionUpdate{Timezone: ptrString("Europe/Berlin")},
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	after := findBackupSet(svc.state.Load().inner.Config, "production", "postgres-primary")
	afterCmd, err := after.Validation.ResolvedCommand()
	if err != nil {
		t.Fatalf("after a settings write the running config no longer resolves its validator: %v; every artifact in this backup set would now fail verification until a restart", err)
	}
	if afterCmd == nil {
		t.Fatal("after a settings write the running config reports NO validator for a backup set that configures one")
	}
	if afterCmd.Executable != beforeCmd.Executable {
		t.Errorf("resolved Executable = %q, want %q (unchanged by an unrelated settings write)", afterCmd.Executable, beforeCmd.Executable)
	}
	if _, statErr := os.Stat(afterCmd.Executable); statErr != nil {
		t.Errorf("the resolved validator script is not on disk after the settings write: %v", statErr)
	}

	// The other half of the same invariant: what is PERSISTED is still an
	// id and never the resolved path, so the settings write did not leak
	// this process's own filesystem layout into the operator's file.
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "validator_id: "+string(ValidatorTrailerMarker)) {
		t.Errorf("the written config no longer names validator_id %q:\n%s", ValidatorTrailerMarker, raw)
	}
	if strings.Contains(string(raw), afterCmd.Executable) {
		t.Errorf("the written config leaked the resolved validator path %q:\n%s", afterCmd.Executable, raw)
	}
}

// TestUpdateSettings_ConfigRevisionMatchesWhatAFreshOpenComputes is M1's
// secondary effect. computeConfigRevision is the staleness token both
// ApplyRetentionPlan and POST /operations compare against, and Open
// computes it over the validated, validator-RESOLVED config. Skipping
// resolution here made one file yield two different revisions depending on
// which path last reloaded it, so a client holding a revision from before a
// settings write and one holding it from after a restart disagreed about
// the same bytes on disk.
func TestUpdateSettings_ConfigRevisionMatchesWhatAFreshOpenComputes(t *testing.T) {
	configPath, _, _ := writeValidatorConfigFile(t, string(ValidatorTrailerMarker), []byte("payload"))
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	if _, err := svc.UpdateSettings(ctx, UpdateSettingsRequest{
		Retention: &RetentionUpdate{Timezone: ptrString("Europe/Berlin")},
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	afterWrite := svc.ConfigRevision()
	if err := cleanup(); err != nil {
		t.Fatalf("closing the service: %v", err)
	}

	// A restart reading the very file the write just produced.
	restarted, cleanup2, err := Open(ctx, configPath)
	if err != nil {
		t.Fatalf("Open (restart): %v", err)
	}
	t.Cleanup(func() { _ = cleanup2() })

	if restarted.ConfigRevision() != afterWrite {
		t.Errorf("ConfigRevision after the write = %q, after a restart on the same file = %q; one file must not yield two revisions", afterWrite, restarted.ConfigRevision())
	}
}

// writeStableStrategyConfigFile writes a loadable config whose backup set
// uses the "stable" completion strategy and does NOT spell
// delete_safety_delay, alerts, or any retention chain: exactly the shape a
// hand-authored or pre-WP3.2 file has, and therefore the shape whose
// resolved defaults a settings write must not freeze into it.
func writeStableStrategyConfigFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + localDir + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: stable\n" +
		"          stable_for: 5m\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// TestUpdateSettings_DoesNotFreezeResolvedDefaultsIntoTheOperatorsFile is
// mandatory review finding M4. config.Validate fills in defaults IN PLACE,
// so marshalling the validated struct persisted every one of them: a file
// that never mentioned completion.delete_safety_delay gained this
// release's value for it, permanently opting that deployment out of any
// future change to a DELETION-safety knob, as a side effect of an
// unrelated retention edit nobody connected to it. Same for
// alerts.repeated_failure_threshold and for the 7/3/12 legacy scalars,
// which pin a file that would otherwise track the default chain.
//
// The write must therefore persist the folded PRE-validate config: the
// operator's own omissions survive, and every default is resolved in
// memory on load exactly as it always was.
func TestUpdateSettings_DoesNotFreezeResolvedDefaultsIntoTheOperatorsFile(t *testing.T) {
	configPath := writeStableStrategyConfigFile(t)
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	if _, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Timezone: ptrString("Europe/Berlin")},
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	written := string(raw)

	// The write did happen: without this the three absences below would be
	// satisfied by a no-op.
	if !strings.Contains(written, "Europe/Berlin") {
		t.Fatalf("precondition failed: the settings write did not reach the file:\n%s", written)
	}

	// Every spelling below is a resolved default. yaml.Marshal emits most
	// of these keys either way (only the three legacy retention scalars
	// carry omitempty), so what matters is the VALUE: a zero, or a null,
	// is "the operator did not choose, resolve it on load", while the
	// resolved number is that choice frozen into their file forever.
	frozen := []string{
		"delete_safety_delay: " + config.DefaultDeleteSafetyDelay.String(),
		"repeated_failure_threshold: " + strconv.Itoa(config.DefaultRepeatedFailureThreshold),
		"daily_days:",
		"weekly_months:",
		"monthly_months:",
		"protect_last_known_good: true",
	}
	for _, spelling := range frozen {
		if strings.Contains(written, spelling) {
			t.Errorf("the settings write froze the resolved default %q into a file that never chose it:\n%s", spelling, written)
		}
	}

	// The opposite half, so the assertions above cannot be satisfied by a
	// write that simply dropped the keys: the omission is still there,
	// spelled as the "not chosen" value config.Validate resolves on load.
	//
	// This list used to carry "protect_last_known_good: null" too, and
	// issue #333 moved that key from "emitted as null" to "omitted": a
	// per-set retention override that inherits the deployment's FR-19
	// posture must not come back from a save with the key written under
	// it, and the same omitempty that fixes it one level down applies
	// here. Absence is a STRONGER form of "the operator did not choose"
	// than an explicit null, and the frozen list above is what keeps this
	// honest: it still requires "protect_last_known_good: true" to be
	// absent, so a write that had frozen the resolved default fails there
	// rather than passing here.
	for _, kept := range []string{"delete_safety_delay: 0s"} {
		if !strings.Contains(written, kept) {
			t.Errorf("the written config no longer carries %q, so the operator's omission was not preserved:\n%s", kept, written)
		}
	}
	if strings.Contains(written, "protect_last_known_good:") {
		t.Errorf("the written config carries protect_last_known_good at all; a file that never chose it must not gain the key:\n%s", written)
	}

	// Positive control: every frozen spelling IS produced by encoding the
	// VALIDATED config, which is what this write path used to do. If this
	// control ever stops finding them, the assertions above have become
	// vacuous and prove nothing.
	validated, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}
	encoded, err := yaml.Marshal(validated)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, spelling := range frozen {
		if !strings.Contains(string(encoded), spelling) {
			t.Errorf("control failed: encoding the validated config does not produce %q, so asserting its absence from the file proves nothing", spelling)
		}
	}

	// And the deployment still RUNS with every default resolved: not
	// persisting them changes what is in the file, never what is decided.
	live := findBackupSet(svc.state.Load().inner.Config, "production", "postgres-primary")
	if live.Completion.DeleteSafetyDelay.Duration() != config.DefaultDeleteSafetyDelay {
		t.Errorf("running delete_safety_delay = %s, want the resolved default %s", live.Completion.DeleteSafetyDelay, config.DefaultDeleteSafetyDelay)
	}
}

// TestRetentionSchema_DefaultTiersIsWhatAnUnconfiguredPolicyResolvesTo is
// mandatory review finding M5's server half. The Web UI's "Restore default
// chain" button used to fill its form from a literal 7/3/12 chain written
// into RetentionPolicyCard.tsx, a second spelling of something
// config.DefaultTierChain's own doc says has exactly one. That is not a
// display string: saving it writes an explicit tiers list, which clears
// the legacy scalars and permanently migrates a config that would have
// tracked the product's default onto a frozen, possibly narrower, copy of
// it. The schema now serves the default chain, and this pins the served
// value to the one an unconfigured policy actually resolves to, so the two
// cannot drift.
func TestRetentionSchema_DefaultTiersIsWhatAnUnconfiguredPolicyResolvesTo(t *testing.T) {
	// A retention block spelling neither a chain nor a legacy scalar, put
	// through the very same config.Validate a config file goes through, so
	// this compares against the resolution rather than against a literal.
	cfg, err := config.LoadAndValidate(writeTestConfigFileWithRetention(t, defaultChainRetentionBlock))
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}

	resolved := toRetentionTiers(cfg.Retention.EffectiveTiers())
	if len(resolved) == 0 {
		t.Fatal("precondition failed: an unconfigured retention policy resolved to no tiers at all")
	}
	assertTiersEqual(t, RetentionSchema().DefaultTiers, resolved)
}

// TestSettings_DefaultTiersDoesNotTrackTheRunningPolicy keeps the two
// halves of the settings response distinct: `retention.tiers` is what this
// deployment is deciding with, `schema.retention.default_tiers` is what
// the product's default is. A form that restored "the default" and got
// back the policy it was already running would silently do nothing, and a
// test comparing them on a default-configured deployment would pass either
// way, which is why this one runs against a deliberately non-default
// chain.
func TestSettings_DefaultTiersDoesNotTrackTheRunningPolicy(t *testing.T) {
	configPath := writeTestConfigFileWithRetention(t, "retention:\n"+
		"  timezone: UTC\n"+
		"  week_starts_on: monday\n"+
		"  tiers:\n"+
		"    - name: annual\n"+
		"      granularity: year\n"+
		"      keep: 2\n")
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	got, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	assertTiersEqual(t, got.Retention.Tiers, []RetentionTier{{Name: "annual", Granularity: GranularityYear, Keep: 2}})
	assertTiersEqual(t, RetentionSchema().DefaultTiers, []RetentionTier{
		{Name: "daily", Granularity: GranularityDay, Keep: config.DefaultDailyDays},
		{Name: "weekly", Granularity: GranularityWeek, Keep: config.DefaultWeeklyMonths, WindowUnit: GranularityMonth},
		{Name: "monthly", Granularity: GranularityMonth, Keep: config.DefaultMonthlyMonths},
	})
}
