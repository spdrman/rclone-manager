package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Issue #333. Retention was global only: every backup set was retained
// under the one top-level policy, and a set that wanted a different chain
// had no way to say so. These cover the three states that distinction
// creates (inherit, override, and going back to inherit) plus the rule
// that an override is validated on exactly the terms the global policy is.

// TestPerSetRetention_AbsentInheritsTheGlobalPolicy is the case every
// config written before #333 is in: no retention block on the set, so the
// set is retained under the global chain and says so.
func TestPerSetRetention_AbsentInheritsTheGlobalPolicy(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("fixture does not validate: %v", err)
	}

	bs := c.Sources[0].BackupSets[0]
	if bs.RetentionConfig != nil {
		t.Fatalf("RetentionConfig should stay nil when the YAML names no override, got %+v", bs.RetentionConfig)
	}
	if bs.RetentionIsOverride() {
		t.Error("a set with no retention block must not report an override")
	}
	if got, want := bs.Retention.Timezone, c.Retention.Timezone; got != want {
		t.Errorf("resolved timezone = %q, want the global %q", got, want)
	}
	if got, want := len(bs.Retention.EffectiveTiers()), len(c.Retention.EffectiveTiers()); got != want {
		t.Errorf("resolved chain length = %d, want the global %d", got, want)
	}
}

// TestPerSetRetention_OverrideIsUsedInsteadOfTheGlobalPolicy is the point
// of the issue: the set's own chain decides, and the global one does not
// leak into it.
func TestPerSetRetention_OverrideIsUsedInsteadOfTheGlobalPolicy(t *testing.T) {
	c := validConfig()
	c.Sources[0].BackupSets[0].RetentionConfig = &Retention{
		Timezone: "UTC",
		Tiers: []RetentionTier{
			{Name: "hourly_ish", Granularity: GranularityDay, Keep: 3, WindowUnit: GranularityDay},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("a set-level override should validate: %v", err)
	}

	bs := c.Sources[0].BackupSets[0]
	if !bs.RetentionIsOverride() {
		t.Fatal("a set with its own retention block must report an override")
	}
	if got := bs.Retention.Timezone; got != "UTC" {
		t.Errorf("resolved timezone = %q, want the override's UTC", got)
	}
	tiers := bs.Retention.EffectiveTiers()
	if len(tiers) != 1 || tiers[0].Name != "hourly_ish" {
		t.Fatalf("resolved chain = %+v, want the override's single hourly-ish tier", tiers)
	}
	// The global policy is untouched by a set declaring its own.
	if c.Retention.Timezone != "America/Vancouver" {
		t.Errorf("global timezone changed to %q; an override must not write back", c.Retention.Timezone)
	}
}

// TestPerSetRetention_EditingTheGlobalPolicyDoesNotMoveAnOverriddenSet is
// the issue's second Given/When/Then: an overriding set's decisions do not
// follow a global edit.
func TestPerSetRetention_EditingTheGlobalPolicyDoesNotMoveAnOverriddenSet(t *testing.T) {
	c := validConfig()
	c.Sources[0].BackupSets[0].RetentionConfig = &Retention{
		Timezone: "UTC",
		Tiers: []RetentionTier{
			{Name: "keep_a_few", Granularity: GranularityDay, Keep: 2, WindowUnit: GranularityDay},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	before := c.Sources[0].BackupSets[0].Retention

	// The first Validate resolved the global scalars to their defaults, so
	// they have to be cleared before writing an explicit chain: setting
	// both is refused, by design, and that rule is not what this test is
	// about.
	c.Retention.Timezone = "Asia/Tokyo"
	c.Retention.DailyDays, c.Retention.WeeklyMonths, c.Retention.MonthlyMonths = 0, 0, 0
	c.Retention.Tiers = []RetentionTier{
		{Name: "totally_different", Granularity: GranularityMonth, Keep: 99, WindowUnit: GranularityMonth},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("re-validate: %v", err)
	}
	after := c.Sources[0].BackupSets[0].Retention

	if after.Timezone != before.Timezone {
		t.Errorf("overridden set's timezone moved from %q to %q when the global policy was edited", before.Timezone, after.Timezone)
	}
	if got := after.EffectiveTiers(); len(got) != 1 || got[0].Name != "keep_a_few" {
		t.Errorf("overridden set's chain moved to %+v when the global policy was edited", got)
	}
}

// TestPerSetRetention_ClearingTheOverrideReturnsToTheGlobalPolicy is the
// third Given/When/Then, and the one a resolved-in-place field can get
// wrong: dropping the override must leave no residue of the chain the set
// used to declare.
func TestPerSetRetention_ClearingTheOverrideReturnsToTheGlobalPolicy(t *testing.T) {
	c := validConfig()
	c.Sources[0].BackupSets[0].RetentionConfig = &Retention{
		Timezone: "UTC",
		Tiers: []RetentionTier{
			{Name: "temporary", Granularity: GranularityDay, Keep: 1, WindowUnit: GranularityDay},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate with override: %v", err)
	}

	c.Sources[0].BackupSets[0].RetentionConfig = nil
	if err := c.Validate(); err != nil {
		t.Fatalf("validate after clearing: %v", err)
	}

	bs := c.Sources[0].BackupSets[0]
	if bs.RetentionIsOverride() {
		t.Error("a cleared override must stop reporting as an override")
	}
	if got, want := bs.Retention.Timezone, c.Retention.Timezone; got != want {
		t.Errorf("timezone after clearing = %q, want the global %q", got, want)
	}
	for _, tier := range bs.Retention.EffectiveTiers() {
		if tier.Name == "temporary" {
			t.Fatalf("the cleared override's tier is still in the resolved chain: %+v", bs.Retention.EffectiveTiers())
		}
	}
}

// TestPerSetRetention_AnOverrideIsRefusedOnTheSameTermsAsTheGlobalPolicy
// is the acceptance criterion about one validation path. Each input is
// run twice, once as the global policy and once as a set's override, and
// the underlying reason has to be the same both times: a second path that
// could disagree with the first is the thing this is guarding against.
func TestPerSetRetention_AnOverrideIsRefusedOnTheSameTermsAsTheGlobalPolicy(t *testing.T) {
	cases := []struct {
		name string
		bad  Retention
	}{
		{
			name: "unloadable timezone",
			bad:  Retention{Timezone: "Not/AZone"},
		},
		{
			name: "reserved tier name",
			bad: Retention{
				Tiers: []RetentionTier{
					{Name: TierLastKnownGoodName, Granularity: GranularityDay, Keep: 3, WindowUnit: GranularityDay},
				},
			},
		},
		{
			name: "both spellings at once",
			bad: Retention{
				DailyDays: 7,
				Tiers: []RetentionTier{
					{Name: "daily", Granularity: GranularityDay, Keep: 7, WindowUnit: GranularityDay},
				},
			},
		},
		{
			name: "not a day of the week",
			bad:  Retention{WeekStartsOn: "caturday"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asGlobal := validConfig()
			asGlobal.Retention = tc.bad
			globalErr := asGlobal.Validate()
			if globalErr == nil {
				t.Fatalf("fixture is not actually refused as a global policy, so this proves nothing")
			}

			asSet := validConfig()
			bad := tc.bad
			asSet.Sources[0].BackupSets[0].RetentionConfig = &bad
			setErr := asSet.Validate()
			if setErr == nil {
				t.Fatalf("refused as a global policy but accepted as a set override:\n  global: %v", globalErr)
			}

			// The reason has to be the same reason. The set-level error
			// additionally names which set it came from, which the global
			// one has no need to, so this checks containment rather than
			// equality.
			reason := globalErr.Error()
			if i := strings.Index(reason, "retention"); i >= 0 {
				reason = reason[i:]
			}
			if !strings.Contains(setErr.Error(), strings.TrimSpace(reason)) {
				t.Errorf("different reasons for the same bad policy:\n  global: %v\n  set:    %v", globalErr, setErr)
			}
		})
	}
}

// TestPerSetRetention_RoundTripsThroughYAML matters because this config
// file is rewritten by the product itself on every settings save, so a
// field that does not survive a marshal/unmarshal cycle silently loses an
// operator's override the first time they touch an unrelated setting.
func TestPerSetRetention_RoundTripsThroughYAML(t *testing.T) {
	c := validConfig()
	c.Sources[0].BackupSets[0].RetentionConfig = &Retention{
		Timezone: "UTC",
		Tiers: []RetentionTier{
			{Name: "monthly", Granularity: GranularityMonth, Keep: 12, WindowUnit: GranularityMonth},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	out, err := yaml.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var back Config
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	dec.KnownFields(true)
	if err := dec.Decode(&back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("validate after round trip: %v", err)
	}

	bs := back.Sources[0].BackupSets[0]
	if !bs.RetentionIsOverride() {
		t.Fatal("the override did not survive a YAML round trip")
	}
	if got := bs.Retention.EffectiveTiers(); len(got) != 1 || got[0].Name != "monthly" {
		t.Errorf("chain after round trip = %+v, want the monthly override", got)
	}
}

// TestPerSetRetention_AbsentOverrideEmitsNoRetentionKey guards the same
// round-trip hazard from the other side. A set that inherits must not come
// back from a settings save with a retention block injected into it: that
// would turn every inheriting set into an overriding one, frozen at
// whatever the global policy happened to be on the day of the save.
func TestPerSetRetention_AbsentOverrideEmitsNoRetentionKey(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	out, err := yaml.Marshal(c.Sources[0].BackupSets[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "retention:") {
		t.Errorf("an inheriting set marshalled a retention key, which would freeze it at today's global policy:\n%s", out)
	}
}

// TestPerSetRetention_ResolvedChainIsNotAliasedToTheGlobalOne is a
// memory-aliasing check, not a policy one. Retention carries a slice, and
// resolving inheritance by assigning the struct copies the slice header
// rather than the backing array, so a later edit through one set could
// otherwise be visible through the global policy or through a sibling set.
func TestPerSetRetention_ResolvedChainIsNotAliasedToTheGlobalOne(t *testing.T) {
	c := validConfig()
	c.Retention.Tiers = []RetentionTier{
		{Name: "daily", Granularity: GranularityDay, Keep: 7, WindowUnit: GranularityDay},
	}
	c.Retention.DailyDays, c.Retention.WeeklyMonths, c.Retention.MonthlyMonths = 0, 0, 0
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	resolved := c.Sources[0].BackupSets[0].Retention.EffectiveTiers()
	if len(resolved) == 0 {
		t.Fatal("resolved chain is empty")
	}
	resolved[0].Name = "mutated_through_the_set"

	if c.Retention.Tiers[0].Name != "daily" {
		t.Errorf("editing a set's resolved chain changed the global policy: %q", c.Retention.Tiers[0].Name)
	}
}

func TestPerSetRetention_OverrideSurvivesRepeatedValidate(t *testing.T) {
	c := validConfig()
	c.Sources[0].BackupSets[0].RetentionConfig = &Retention{
		Timezone: "UTC",
		Tiers: []RetentionTier{
			{Name: "weekly", Granularity: GranularityWeek, Keep: 4, WindowUnit: GranularityMonth},
		},
	}
	for i := 0; i < 3; i++ {
		if err := c.Validate(); err != nil {
			t.Fatalf("validate pass %d: %v", i+1, err)
		}
	}
	bs := c.Sources[0].BackupSets[0]
	if got := bs.Retention.EffectiveTiers(); len(got) != 1 || got[0].Name != "weekly" {
		t.Errorf("chain after three Validate passes = %+v", got)
	}
	if bs.Retention.Timezone != "UTC" {
		t.Errorf("timezone after three Validate passes = %q", bs.Retention.Timezone)
	}
	_ = time.Now
}
