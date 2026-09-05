package config

import (
	"strings"
	"testing"

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

	// The whole Config, which is what the real save path marshals
	// (core/service's UpdateSettings). Marshalling the BackupSet on its own
	// would prove the same thing about a value nothing ever writes, and it
	// could not tell an emitted set-level key apart from the top-level one
	// that is supposed to be there.
	out, err := yaml.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := strings.Count(string(out), "retention:"); got != 1 {
		t.Errorf("marshalled config carries %d retention keys, want exactly 1 (the deployment's own); an inheriting set that gains one is frozen at whatever the global policy was on the day of the save:\n%s", got, out)
	}
}

// TestPerSetRetention_AnOverrideMarshalsExactlyOneExtraRetentionKey is the
// positive control for the count above: 1 has to mean "the deployment's,
// and no set's", not "this assertion cannot see a set-level key at all".
func TestPerSetRetention_AnOverrideMarshalsExactlyOneExtraRetentionKey(t *testing.T) {
	c := validConfig()
	c.Sources[0].BackupSets[0].RetentionConfig = &Retention{
		Tiers: []RetentionTier{
			{Name: "yearly", Granularity: GranularityYear, Keep: 5},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	out, err := yaml.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := strings.Count(string(out), "retention:"); got != 2 {
		t.Errorf("marshalled config carries %d retention keys, want 2 (the deployment's and the set's):\n%s", got, out)
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

// A per-set override goes through the same resolution the global policy
// does, so it inherits the same idempotence requirement, and it has a
// sharper failure: the API layer saves a config after every settings
// change, so an override that drifted on each pass would quietly retain
// less every time somebody edited something unrelated.
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
}

// --- What an override does NOT say ---
//
// validateRetention reads a zero scalar as "fill in the documented
// default" (7/3/12), an empty timezone as UTC and an empty week_starts_on
// as monday. That rule is right at the top level, where the alternative is
// no policy at all, and wrong for an override, where there IS another
// policy: the deployment's, which the operator can see three lines further
// down the same file. The tests below pin the two halves of the answer
// this PR settled on. The chain has to be written out in full, because
// half a chain is a policy nobody wrote. Everything that is not the chain
// (the calendar the chain is reckoned in, and FR-19's protection posture)
// is inherited from the resolved deployment policy, because those change
// how ANY chain is evaluated rather than what the chain says.

// TestPerSetRetention_APartialChainIsRefused is the data-loss case. A
// deployment retaining 90/24/60 with one set writing only daily_days: 120
// used to resolve that set to 120/3/12: weekly collapsed from 24 months to
// 3 and monthly from 60 to 12, deleting data the operator believes is
// retained, reported as nothing at all.
func TestPerSetRetention_APartialChainIsRefused(t *testing.T) {
	cases := []struct {
		name        string
		override    Retention
		wantNamedIn []string
	}{
		{
			name:        "one scalar out of three",
			override:    Retention{DailyDays: 120},
			wantNamedIn: []string{"weekly_months", "monthly_months"},
		},
		{
			name:        "two scalars out of three",
			override:    Retention{DailyDays: 120, WeeklyMonths: 24},
			wantNamedIn: []string{"monthly_months"},
		},
		{
			// An empty block is the case the pointer's own doc says it
			// exists to keep distinguishable from an absent one. Resolving
			// it to the product defaults is exactly the confusion the
			// pointer was supposed to prevent, one layer down.
			name:        "an empty override block",
			override:    Retention{},
			wantNamedIn: []string{"daily_days", "weekly_months", "monthly_months"},
		},
		{
			name:        "a calendar but no chain",
			override:    Retention{Timezone: "UTC", WeekStartsOn: "sunday"},
			wantNamedIn: []string{"daily_days", "weekly_months", "monthly_months"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Retention.DailyDays, c.Retention.WeeklyMonths, c.Retention.MonthlyMonths = 90, 24, 60
			override := tc.override
			c.Sources[0].BackupSets[0].RetentionConfig = &override

			err := c.Validate()
			if err == nil {
				t.Fatalf("a partial override resolved to %+v instead of being refused",
					c.Sources[0].BackupSets[0].Retention)
			}
			if !strings.Contains(err.Error(), "sources[0].backup_sets[0].retention") {
				t.Errorf("the refusal does not say which set it came from: %v", err)
			}
			for _, name := range tc.wantNamedIn {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("the refusal does not name the missing %s: %v", name, err)
				}
			}
		})
	}
}

// TestPerSetRetention_ACompleteChainIsAccepted is the positive control for
// the test above: the refusal has to be about the chain being partial, not
// about overrides in general.
func TestPerSetRetention_ACompleteChainIsAccepted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override Retention
	}{
		{"all three scalars", Retention{DailyDays: 120, WeeklyMonths: 24, MonthlyMonths: 60}},
		{"an explicit tiers list", Retention{Tiers: []RetentionTier{
			{Name: "yearly", Granularity: GranularityYear, Keep: 5},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			override := tc.override
			c.Sources[0].BackupSets[0].RetentionConfig = &override
			if err := c.Validate(); err != nil {
				t.Fatalf("a complete override was refused: %v", err)
			}
		})
	}
}

// TestPerSetRetention_AnOverrideInheritsTheDeploymentsCalendar is the
// timezone half, and it is the sharpest of the two because the project
// already wrote down why it matters: container/compose.yaml's TZ comment
// says that left at UTC, the day an operator thinks a restore point
// belongs to and the day retention assigns it to are silently different
// for most of the world. An override that omits timezone must not
// reintroduce that for one set inside a deployment that got it right.
func TestPerSetRetention_AnOverrideInheritsTheDeploymentsCalendar(t *testing.T) {
	c := validConfig()
	c.Retention.WeekStartsOn = "sunday"
	protect := false
	c.Retention.ProtectLastKnownGood = &protect
	c.Sources[0].BackupSets[0].RetentionConfig = &Retention{
		Tiers: []RetentionTier{
			{Name: "yearly", Granularity: GranularityYear, Keep: 5},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	got := c.Sources[0].BackupSets[0].Retention
	if got.Timezone != "America/Vancouver" {
		t.Errorf("resolved timezone = %q, want the deployment's America/Vancouver; UTC here would silently move which day a restore point belongs to", got.Timezone)
	}
	if got.WeekStartsOn != "sunday" {
		t.Errorf("resolved week_starts_on = %q, want the deployment's sunday", got.WeekStartsOn)
	}
	if got.ProtectLastKnownGood == nil || *got.ProtectLastKnownGood {
		t.Errorf("resolved protect_last_known_good = %v, want the deployment's explicit false", got.ProtectLastKnownGood)
	}
}

// TestPerSetRetention_AnOverrideCanStillNameItsOwnCalendar is the other
// side of the same rule: inheriting is what an omitted field means, not
// what every field means.
func TestPerSetRetention_AnOverrideCanStillNameItsOwnCalendar(t *testing.T) {
	c := validConfig()
	protect := true
	c.Sources[0].BackupSets[0].RetentionConfig = &Retention{
		Timezone:             "Asia/Tokyo",
		WeekStartsOn:         "sunday",
		ProtectLastKnownGood: &protect,
		Tiers: []RetentionTier{
			{Name: "yearly", Granularity: GranularityYear, Keep: 5},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	got := c.Sources[0].BackupSets[0].Retention
	if got.Timezone != "Asia/Tokyo" {
		t.Errorf("resolved timezone = %q, want the override's Asia/Tokyo", got.Timezone)
	}
	if got.WeekStartsOn != "sunday" {
		t.Errorf("resolved week_starts_on = %q, want the override's sunday", got.WeekStartsOn)
	}
}

// TestPerSetRetention_ResolvingDoesNotWriteBackToTheOperatorsOverride:
// resolution fills in defaults, and it has to fill them into its own copy.
// Filling them into the raw pointer is invisible today only because
// core/service's UpdateSettings happens to marshal the config before it
// validates it, which makes the round-trip safety a property of one call
// site's statement order rather than of this resolver.
func TestPerSetRetention_ResolvingDoesNotWriteBackToTheOperatorsOverride(t *testing.T) {
	c := validConfig()
	c.Sources[0].BackupSets[0].RetentionConfig = &Retention{
		Tiers: []RetentionTier{
			{Name: "yearly", Granularity: GranularityYear, Keep: 5},
		},
	}
	before := *c.Sources[0].BackupSets[0].RetentionConfig

	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	after := *c.Sources[0].BackupSets[0].RetentionConfig
	if after.Timezone != before.Timezone {
		t.Errorf("Validate wrote timezone %q into the operator's own override, which said %q", after.Timezone, before.Timezone)
	}
	if after.WeekStartsOn != before.WeekStartsOn {
		t.Errorf("Validate wrote week_starts_on %q into the operator's own override, which said %q", after.WeekStartsOn, before.WeekStartsOn)
	}
	if after.ProtectLastKnownGood != before.ProtectLastKnownGood {
		t.Errorf("Validate wrote protect_last_known_good into the operator's own override")
	}
}

// TestPerSetRetention_EveryNullSpellingInherits pins what the README tells
// an operator: a set inherits by having no retention key at all, and also
// by an explicitly null one. Load's KnownFields(true) makes a typo in this
// key a parse error rather than a silent inherit, so the spellings that DO
// mean inherit are worth being sure of.
func TestPerSetRetention_EveryNullSpellingInherits(t *testing.T) {
	for _, spelling := range []string{
		"", // no key at all
		"        retention:\n",
		"        retention: null\n",
		"        retention: ~\n",
	} {
		name := strings.TrimSpace(spelling)
		if name == "" {
			name = "no key at all"
		}
		t.Run(name, func(t *testing.T) {
			doc := "poll_interval: 15m\n" +
				"state:\n  database: /var/lib/backup-manager/state.db\n" +
				"sources:\n  - id: production\n    backup_sets:\n" +
				"      - id: postgres-primary\n        remote:\n          type: local\n" +
				"        remote_path: /backups/postgres\n        local_path: /backups/production/postgres\n" +
				"        include:\n          - \"*.dump\"\n" +
				"        completion:\n          strategy: rename\n" +
				"        stale_after: 24h\n" + spelling +
				"retention:\n  timezone: Asia/Tokyo\n  daily_days: 90\n  weekly_months: 24\n  monthly_months: 60\n"

			var c Config
			dec := yaml.NewDecoder(strings.NewReader(doc))
			dec.KnownFields(true)
			if err := dec.Decode(&c); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := c.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}

			bs := c.Sources[0].BackupSets[0]
			if bs.RetentionIsOverride() {
				t.Fatal("this spelling reported an override rather than inheriting")
			}
			if got := bs.Retention.DailyDays; got != 90 {
				t.Errorf("resolved daily_days = %d, want the deployment's 90", got)
			}
			if got := bs.Retention.Timezone; got != "Asia/Tokyo" {
				t.Errorf("resolved timezone = %q, want the deployment's Asia/Tokyo", got)
			}
		})
	}
}
