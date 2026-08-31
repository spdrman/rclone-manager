package config

import (
	"strings"
	"testing"
)

// Tests for issue #156 (B3.8): Retention.Tiers, the generalized FR-18
// retention chain, and its relationship to the three legacy scalars.

func retentionWithTiers(tiers ...RetentionTier) Retention {
	return Retention{Timezone: "UTC", WeekStartsOn: "monday", Tiers: tiers}
}

func mustValidateRetention(t *testing.T, r *Retention) {
	t.Helper()
	if err := ValidateRetention(r); err != nil {
		t.Fatalf("ValidateRetention: %v", err)
	}
}

// TestValidateRetention_OmittedTiersKeepsTheLegacyDefaults is the
// backward-compatibility floor: a config that never mentions tiers gets
// exactly the resolved policy it got before the field existed.
func TestValidateRetention_OmittedTiersKeepsTheLegacyDefaults(t *testing.T) {
	var r Retention
	mustValidateRetention(t, &r)

	if r.DailyDays != 7 || r.WeeklyMonths != 3 || r.MonthlyMonths != 12 {
		t.Errorf("resolved to %d/%d/%d, want 7/3/12", r.DailyDays, r.WeeklyMonths, r.MonthlyMonths)
	}
	if len(r.Tiers) != 0 {
		t.Errorf("Tiers = %+v, want it left empty: validation must not rewrite an operator's file shape", r.Tiers)
	}

	// The effective chain those scalars stand for.
	got := r.EffectiveTiers()
	want := DefaultTierChain(7, 3, 12)
	if len(got) != len(want) {
		t.Fatalf("EffectiveTiers() = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("EffectiveTiers()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if want[1].Name != "weekly" || want[1].WindowUnit != GranularityMonth {
		t.Errorf("the default weekly tier must look back in calendar months, got %+v", want[1])
	}
}

// TestValidateRetention_ExplicitTiersSuppressLegacyDefaulting proves the
// two spellings do not silently blend: an operator who writes a chain does
// not also get 7/3/12 injected behind it, which is what would make the
// "you set both" check below unfireable.
func TestValidateRetention_ExplicitTiersSuppressLegacyDefaulting(t *testing.T) {
	r := retentionWithTiers(RetentionTier{Name: "annual", Granularity: GranularityYear, Keep: 10})
	mustValidateRetention(t, &r)

	if r.DailyDays != 0 || r.WeeklyMonths != 0 || r.MonthlyMonths != 0 {
		t.Errorf("explicit tiers still got the legacy scalars defaulted to %d/%d/%d, want them left at zero",
			r.DailyDays, r.WeeklyMonths, r.MonthlyMonths)
	}
	if len(r.EffectiveTiers()) != 1 {
		t.Errorf("EffectiveTiers() = %+v, want just the configured annual tier", r.EffectiveTiers())
	}
}

// TestValidateRetention_RefusesBothSpellings: writing both a scalar and a
// chain asks two different questions, and a silent precedence rule is how
// a retention policy ends up deleting on terms nobody wrote.
func TestValidateRetention_RefusesBothSpellings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutch func(*Retention)
		field string
	}{
		{"daily_days", func(r *Retention) { r.DailyDays = 7 }, "daily_days"},
		{"weekly_months", func(r *Retention) { r.WeeklyMonths = 3 }, "weekly_months"},
		{"monthly_months", func(r *Retention) { r.MonthlyMonths = 12 }, "monthly_months"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := retentionWithTiers(RetentionTier{Name: "annual", Granularity: GranularityYear, Keep: 10})
			tc.mutch(&r)
			err := ValidateRetention(&r)
			if err == nil {
				t.Fatal("ValidateRetention accepted both a legacy scalar and an explicit tiers list")
			}
			if !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "tiers") {
				t.Errorf("error %q should name both %s and tiers so the operator knows which two keys collided", err, tc.field)
			}
		})
	}

	t.Run("control: the chain alone is accepted", func(t *testing.T) {
		r := retentionWithTiers(RetentionTier{Name: "annual", Granularity: GranularityYear, Keep: 10})
		mustValidateRetention(t, &r)
	})
}

// TestValidateRetention_IsIdempotentWithTiers: Validate's own doc promises
// a second call is a no-op, and the CLI relies on it (config.LoadAndValidate
// then applyRetentionOverrides then ValidateRetention again).
func TestValidateRetention_IsIdempotentWithTiers(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Retention
	}{
		{"legacy scalars", Retention{}},
		{"explicit chain", retentionWithTiers(
			RetentionTier{Name: "daily", Granularity: GranularityDay, Keep: 7},
			RetentionTier{Name: "annual", Granularity: GranularityYear, Keep: 10},
		)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := tc.r
			mustValidateRetention(t, &first)
			second := first
			mustValidateRetention(t, &second)
			if second.DailyDays != first.DailyDays ||
				second.WeeklyMonths != first.WeeklyMonths ||
				second.MonthlyMonths != first.MonthlyMonths ||
				len(second.Tiers) != len(first.Tiers) {
				t.Errorf("a second ValidateRetention changed the policy:\n first  = %+v\n second = %+v", first, second)
			}
		})
	}
}

func TestValidateRetention_TierFieldRules(t *testing.T) {
	cases := []struct {
		name     string
		tiers    []RetentionTier
		wantErr  string
		accepted bool
	}{
		{
			name:     "a full six-link chain is accepted",
			accepted: true,
			tiers: []RetentionTier{
				{Name: "daily", Granularity: GranularityDay, Keep: 7},
				{Name: "weekly", Granularity: GranularityWeek, Keep: 3, WindowUnit: GranularityMonth},
				{Name: "monthly", Granularity: GranularityMonth, Keep: 12},
				{Name: "quarterly", Granularity: GranularityQuarter, Keep: 8},
				{Name: "semi_annual", Granularity: GranularityHalfYear, Keep: 6},
				{Name: "annual", Granularity: GranularityYear, Keep: 10},
			},
		},
		{
			name:     "a custom period is accepted with a length",
			accepted: true,
			tiers:    []RetentionTier{{Name: "fortnightly", Granularity: GranularityDays, PeriodDays: 14, Keep: 26}},
		},
		{
			name:    "a name must not be empty",
			tiers:   []RetentionTier{{Granularity: GranularityDay, Keep: 7}},
			wantErr: "name",
		},
		{
			name:    "a name must be lower_snake_case",
			tiers:   []RetentionTier{{Name: "Semi-Annual", Granularity: GranularityHalfYear, Keep: 6}},
			wantErr: "name",
		},
		{
			name: "names must be unique",
			tiers: []RetentionTier{
				{Name: "daily", Granularity: GranularityDay, Keep: 7},
				{Name: "daily", Granularity: GranularityDay, Keep: 3},
			},
			wantErr: "duplicate",
		},
		{
			name:    "last_known_good is reserved for FR-19",
			tiers:   []RetentionTier{{Name: TierLastKnownGoodName, Granularity: GranularityDay, Keep: 7}},
			wantErr: "reserved",
		},
		{
			name:    "an unknown granularity is refused",
			tiers:   []RetentionTier{{Name: "weird", Granularity: "fortnight", Keep: 7}},
			wantErr: "granularity",
		},
		{
			name:    "keep must be positive",
			tiers:   []RetentionTier{{Name: "daily", Granularity: GranularityDay, Keep: 0}},
			wantErr: "keep",
		},
		{
			name:    "keep must not be negative",
			tiers:   []RetentionTier{{Name: "daily", Granularity: GranularityDay, Keep: -1}},
			wantErr: "keep",
		},
		{
			name:     "keep is accepted right up to its ceiling",
			accepted: true,
			tiers:    []RetentionTier{{Name: "daily", Granularity: GranularityDay, Keep: retentionTierKeepMax}},
		},
		{
			// Large enough to wrap time.Date's own int64 arithmetic, which
			// puts the window's start date after today and makes the tier
			// select nothing at all, with no error: the same silent empty
			// selection every other rule in this function refuses.
			name:    "keep is bounded from above",
			tiers:   []RetentionTier{{Name: "annual", Granularity: GranularityYear, Keep: 300000000000}},
			wantErr: "keep",
		},
		{
			name:     "period_days is accepted right up to its ceiling",
			accepted: true,
			tiers:    []RetentionTier{{Name: "decadal", Granularity: GranularityDays, PeriodDays: retentionTierPeriodDaysMax, Keep: 4}},
		},
		{
			name:    "period_days is bounded from above",
			tiers:   []RetentionTier{{Name: "fortnightly", Granularity: GranularityDays, PeriodDays: 300000000000, Keep: 26}},
			wantErr: "period_days",
		},
		{
			name:    "period_days is required for a custom period",
			tiers:   []RetentionTier{{Name: "fortnightly", Granularity: GranularityDays, Keep: 26}},
			wantErr: "period_days",
		},
		{
			name:    "period_days is meaningless on a named granularity",
			tiers:   []RetentionTier{{Name: "daily", Granularity: GranularityDay, PeriodDays: 14, Keep: 7}},
			wantErr: "period_days",
		},
		{
			name:    "window_unit cannot be the custom-period keyword",
			tiers:   []RetentionTier{{Name: "daily", Granularity: GranularityDay, Keep: 7, WindowUnit: GranularityDays}},
			wantErr: "window_unit",
		},
		{
			name:    "an unknown window_unit is refused",
			tiers:   []RetentionTier{{Name: "daily", Granularity: GranularityDay, Keep: 7, WindowUnit: "fortnight"}},
			wantErr: "window_unit",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := retentionWithTiers(tc.tiers...)
			err := ValidateRetention(&r)
			if tc.accepted {
				if err != nil {
					t.Fatalf("ValidateRetention refused a valid chain: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateRetention accepted %+v", tc.tiers)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q, so it does not tell the operator which field to fix", err, tc.wantErr)
			}
			// Every message must locate the offending tier by index, the
			// way the rest of this package locates a bad source or set.
			if !strings.Contains(err.Error(), "retention.tiers[0]") {
				t.Errorf("error %q does not point at the offending tier", err)
			}
		})
	}
}

// TestLoad_RetentionTiersRoundTripFromYAML proves the schema is actually
// spellable in a config file (KnownFields(true) means a mis-tagged field
// would be a parse error, not a silently ignored one).
func TestLoad_RetentionTiersRoundTripFromYAML(t *testing.T) {
	cfg, err := Load("testdata/retention-tiers.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Retention.Tiers
	want := []RetentionTier{
		{Name: "daily", Granularity: GranularityDay, Keep: 7},
		{Name: "weekly", Granularity: GranularityWeek, Keep: 3, WindowUnit: GranularityMonth},
		{Name: "monthly", Granularity: GranularityMonth, Keep: 12},
		{Name: "semi_annual", Granularity: GranularityHalfYear, Keep: 6},
		{Name: "annual", Granularity: GranularityYear, Keep: 10},
		{Name: "fortnightly", Granularity: GranularityDays, PeriodDays: 14, Keep: 26},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d tier(s), want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tiers[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate on the parsed chain: %v", err)
	}
}

// TestValidateRetention_KeepMessageDoesNotAdviseEmptyingTheChain covers
// the one piece of advice in this package that pointed somewhere
// dangerous. "Leave a tier out of the chain rather than writing it with a
// zero window" is right for one tier and wrong for all of them: an
// operator who follows it down to the last tier arrives at tiers: [],
// which is indistinguishable from an absent key and reinstates the
// default 7/3/12 chain rather than the narrow policy they were writing.
func TestValidateRetention_KeepMessageDoesNotAdviseEmptyingTheChain(t *testing.T) {
	r := retentionWithTiers(RetentionTier{Name: "daily", Granularity: GranularityDay, Keep: 0})
	err := ValidateRetention(&r)
	if err == nil {
		t.Fatal("ValidateRetention accepted a zero keep")
	}
	msg := err.Error()
	if !strings.Contains(msg, "retention.tiers[0]") || !strings.Contains(msg, "keep") {
		t.Fatalf("error %q does not locate the offending field", msg)
	}
	// The message has to carry the fallback with it, because the fallback
	// is what makes the advice safe to follow.
	for _, want := range []string{"daily", "weekly", "monthly"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not warn that emptying retention.tiers falls back to the default %s tier", msg, want)
		}
	}
}

// TestValidateRetention_ExplicitlyEmptyTiersFallsBackToTheDefaultChain
// pins the reading the message above now warns about, so the behaviour is
// documented by a test rather than only by prose. This is deliberately
// fail-safe (more retention, not less); refusing it outright needs a
// schema change and is tracked separately.
func TestValidateRetention_ExplicitlyEmptyTiersFallsBackToTheDefaultChain(t *testing.T) {
	r := Retention{Timezone: "UTC", WeekStartsOn: "monday", Tiers: []RetentionTier{}}
	mustValidateRetention(t, &r)

	if r.DailyDays != 7 || r.WeeklyMonths != 3 || r.MonthlyMonths != 12 {
		t.Errorf("resolved to %d/%d/%d, want 7/3/12", r.DailyDays, r.WeeklyMonths, r.MonthlyMonths)
	}
	if got := len(r.EffectiveTiers()); got != 3 {
		t.Errorf("EffectiveTiers() has %d tier(s), want the 3-tier default chain", got)
	}
}
