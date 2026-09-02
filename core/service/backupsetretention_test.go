package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// deploymentChain is a deployment policy that is deliberately NOT the
// product default 7/3/12. Every case below leans on that: a resolution
// bug that reaches for the documented defaults instead of this
// deployment's own policy is the exact failure #362 was written to stop,
// and it is invisible against a fixture whose global policy already IS
// the default.
const deploymentChain = "retention:\n" +
	"  timezone: America/Vancouver\n" +
	"  week_starts_on: sunday\n" +
	"  daily_days: 90\n" +
	"  weekly_months: 24\n" +
	"  monthly_months: 60\n"

func openRetentionTestService(t *testing.T) (*BackupService, string) {
	t.Helper()
	configPath := writeTestConfigFileWithRetention(t, deploymentChain)
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	return svc, configPath
}

const theSet = "production/postgres-primary"

func tierNames(tiers []RetentionTier) string {
	parts := make([]string, 0, len(tiers))
	for _, t := range tiers {
		parts = append(parts, t.Name+"/"+itoa(t.Keep))
	}
	return strings.Join(parts, " ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// TestBackupSetRetention_InheritingSetReportsTheDeploymentPolicy is the
// read half of the issue's first Given/When/Then, at the boundary the API
// and the CLI both come through: a set with no retention block of its own
// is retained under the deployment's chain, and the answer says so rather
// than leaving the caller to infer it by comparing two chains.
func TestBackupSetRetention_InheritingSetReportsTheDeploymentPolicy(t *testing.T) {
	svc, _ := openRetentionTestService(t)

	got, err := svc.BackupSetRetention(context.Background(), theSet)
	if err != nil {
		t.Fatalf("BackupSetRetention: %v", err)
	}
	if got.IsOverride {
		t.Fatalf("a set with no retention block reports IsOverride=true")
	}
	if got.Override != nil {
		t.Fatalf("a set with no retention block reports an override: %+v", got.Override)
	}
	if want := "daily/90 weekly/24 monthly/60"; tierNames(got.Effective.Tiers) != want {
		t.Fatalf("effective chain = %q, want %q", tierNames(got.Effective.Tiers), want)
	}
	if got.Effective.Timezone != "America/Vancouver" {
		t.Fatalf("effective timezone = %q, want the deployment's", got.Effective.Timezone)
	}
	// Deployment is served even while the set is inheriting it, because a
	// form about to CREATE an override pre-fills from a whole resolved
	// chain, and that is what stops the first submission being half a
	// policy.
	if tierNames(got.Deployment.Tiers) != tierNames(got.Effective.Tiers) {
		t.Fatalf("deployment chain = %q, effective = %q; an inheriting set's two answers must agree",
			tierNames(got.Deployment.Tiers), tierNames(got.Effective.Tiers))
	}
}

// TestSetBackupSetRetention_OverridesJustThatSet is the write half of the
// first Given/When/Then, and it is what the whole issue is for: two sets,
// one inheriting and one declaring its own chain, retained differently.
func TestSetBackupSetRetention_OverridesJustThatSet(t *testing.T) {
	svc, configPath := openRetentionTestService(t)

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	got, err := svc.SetBackupSetRetention(context.Background(), theSet, RetentionOverride{
		DailyDays:     3,
		WeeklyMonths:  1,
		MonthlyMonths: 2,
	})
	if err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}
	if !got.IsOverride {
		t.Fatalf("after setting an override, IsOverride is false")
	}
	if want := "daily/3 weekly/1 monthly/2"; tierNames(got.Effective.Tiers) != want {
		t.Fatalf("effective chain = %q, want %q", tierNames(got.Effective.Tiers), want)
	}
	// The deployment's own policy is untouched, and is still reported
	// beside the override so a client can show what clearing would go
	// back to.
	if want := "daily/90 weekly/24 monthly/60"; tierNames(got.Deployment.Tiers) != want {
		t.Fatalf("deployment chain = %q, want %q; setting a per-set override must not move the deployment's policy",
			tierNames(got.Deployment.Tiers), want)
	}

	// The calendar is INHERITED, not defaulted. An override that omits
	// the timezone must not silently move this set to UTC inside a
	// deployment that deliberately set something else
	// (config.resolveBackupSetRetention's own reasoning, and the cost
	// container/compose.yaml already writes down).
	if got.Effective.Timezone != "America/Vancouver" {
		t.Fatalf("override resolved to timezone %q; an omitted timezone inherits the deployment's", got.Effective.Timezone)
	}
	if got.Effective.WeekStartsOn != "sunday" {
		t.Fatalf("override resolved to week start %q; an omitted week start inherits the deployment's", got.Effective.WeekStartsOn)
	}

	// The raw override reports what the FILE says, unresolved, which is
	// what an edit form has to render. Resolving it here would turn every
	// inherited field into an explicit one the moment somebody saved.
	if got.Override == nil {
		t.Fatalf("Override is nil after setting one")
	}
	if got.Override.Timezone != "" {
		t.Fatalf("raw override carries timezone %q; it should carry what was submitted, which was nothing", got.Override.Timezone)
	}
	if got.Override.DailyDays != 3 || got.Override.WeeklyMonths != 1 || got.Override.MonthlyMonths != 2 {
		t.Fatalf("raw override = %+v, want the three scalars exactly as submitted", got.Override)
	}
	if len(got.Override.Tiers) != 0 {
		t.Fatalf("raw override gained a tiers list %v; the file was written with the scalar spelling", got.Override.Tiers)
	}

	// The file changed, and it changed by gaining a retention block under
	// the backup set rather than by having the resolved chain written
	// back over the operator's own spelling.
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) == string(before) {
		t.Fatalf("config file is unchanged after setting an override")
	}
	persisted := loadPersistedSet(t, configPath)
	if persisted.RetentionConfig == nil {
		t.Fatalf("config file carries no per-set retention block")
	}
	if persisted.RetentionConfig.DailyDays != 3 || persisted.RetentionConfig.WeeklyMonths != 1 || persisted.RetentionConfig.MonthlyMonths != 2 {
		t.Fatalf("persisted override = %+v, want the three scalars as submitted", persisted.RetentionConfig)
	}
	if len(persisted.RetentionConfig.Tiers) != 0 {
		t.Fatalf("persisted override gained a resolved tiers list: %v", persisted.RetentionConfig.Tiers)
	}
	if persisted.RetentionConfig.Timezone != "" {
		t.Fatalf("persisted override gained an explicit timezone %q, which stops it tracking a later change to the deployment's calendar",
			persisted.RetentionConfig.Timezone)
	}
}

// TestSetBackupSetRetention_TakesEffectWithoutARestart pins the half a
// file check cannot: the RUNNING service decides under the new policy
// straight away. A write that leaves the file right while this process
// goes on deciding under the old chain is the failure mode
// config.BackupSet.Retention's own doc names.
func TestSetBackupSetRetention_TakesEffectWithoutARestart(t *testing.T) {
	svc, _ := openRetentionTestService(t)

	if _, err := svc.SetBackupSetRetention(context.Background(), theSet, RetentionOverride{
		Tiers: []RetentionTier{{Name: "hourly_ish", Granularity: GranularityDay, Keep: 2}},
	}); err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}

	running := svc.state.Load().inner.Config
	set, ok := lookupBackupSet(running, "production", "postgres-primary")
	if !ok {
		t.Fatalf("the running config lost the backup set")
	}
	if len(set.Retention.Tiers) != 1 || set.Retention.Tiers[0].Name != "hourly_ish" {
		t.Fatalf("the running service still resolves this set to %v", set.Retention.EffectiveTiers())
	}
	if len(running.Retention.Tiers) != 0 || running.Retention.DailyDays != 90 {
		t.Fatalf("the deployment's own policy moved: %+v", running.Retention)
	}
}

// TestClearBackupSetRetention_ReturnsTheSetToTheDeploymentPolicy is the
// issue's third Given/When/Then, including the part that says "with no
// residue of the chain it used to declare".
func TestClearBackupSetRetention_ReturnsTheSetToTheDeploymentPolicy(t *testing.T) {
	svc, configPath := openRetentionTestService(t)

	if _, err := svc.SetBackupSetRetention(context.Background(), theSet, RetentionOverride{
		DailyDays: 3, WeeklyMonths: 1, MonthlyMonths: 2,
	}); err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}

	got, err := svc.ClearBackupSetRetention(context.Background(), theSet)
	if err != nil {
		t.Fatalf("ClearBackupSetRetention: %v", err)
	}
	if got.IsOverride {
		t.Fatalf("after clearing, IsOverride is still true")
	}
	if got.Override != nil {
		t.Fatalf("after clearing, a raw override is still reported: %+v", got.Override)
	}
	if want := "daily/90 weekly/24 monthly/60"; tierNames(got.Effective.Tiers) != want {
		t.Fatalf("effective chain after clearing = %q, want the deployment's %q", tierNames(got.Effective.Tiers), want)
	}

	// "No residue" is about the FILE, not only about the resolved answer.
	// A retention key left behind as an empty block would be refused by
	// the next config.Load->Validate this deployment does, which is a
	// daemon that will not start after an ordinary UI click.
	persisted := loadPersistedSet(t, configPath)
	if persisted.RetentionConfig != nil {
		t.Fatalf("the config file still carries a per-set retention block after clearing: %+v", persisted.RetentionConfig)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "daily_days: 3") {
		t.Fatalf("the cleared chain is still in the file:\n%s", raw)
	}
}

// TestSetBackupSetRetention_HalfAChainIsRefusedOnTheConfigLayersOwnTerms
// is the trap this issue names explicitly, checked at the boundary every
// surface comes through rather than only one layer down.
//
// The refusal has to be the CONFIG layer's, word for word, because a
// second wording here is the first step towards a second rule. So the
// assertion is on the config layer's own sentence rather than on a
// message this file could have invented.
func TestSetBackupSetRetention_HalfAChainIsRefusedOnTheConfigLayersOwnTerms(t *testing.T) {
	svc, configPath := openRetentionTestService(t)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	_, err = svc.SetBackupSetRetention(context.Background(), theSet, RetentionOverride{DailyDays: 120})
	if err == nil {
		t.Fatalf("a per-set override naming one of the three scalars was accepted; " +
			"under this deployment's 90/24/60 it would have resolved to 120/3/12 and collapsed weekly from 24 months to 3")
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("refusal is not an ErrInvalidRequest: %v", err)
	}
	for _, want := range []string{"weekly_months", "monthly_months", "replaces the deployment's whole chain"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not carry %q; it reads: %v", want, err)
		}
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("a refused override still rewrote the config file")
	}
	if svc.state.Load().inner.Config.Sources[0].BackupSets[0].RetentionIsOverride() {
		t.Fatalf("a refused override still reached the running service")
	}
}

// TestSetBackupSetRetention_IsRefusedOnTheSameTermsAsAGlobalPolicy is the
// issue's "a per-set chain is validated exactly as the global one is"
// criterion, driven through this boundary.
//
// Each policy below is one a hand-edited config.yaml is already refused
// for. The assertion is that the same submission through the API/CLI seam
// is refused too, and with the config layer's own reason: a second
// validation path that can disagree with the first is worse than no
// second path.
func TestSetBackupSetRetention_IsRefusedOnTheSameTermsAsAGlobalPolicy(t *testing.T) {
	tests := []struct {
		name     string
		override RetentionOverride
		want     string
	}{
		{
			name:     "an unloadable timezone",
			override: RetentionOverride{Timezone: "Mars/Olympus", DailyDays: 1, WeeklyMonths: 1, MonthlyMonths: 1},
			want:     "is not a loadable IANA timezone",
		},
		{
			name:     "a day that is not a day",
			override: RetentionOverride{WeekStartsOn: "caturday", DailyDays: 1, WeeklyMonths: 1, MonthlyMonths: 1},
			want:     "is not a day of the week",
		},
		{
			name:     "the reserved last-known-good tier name",
			override: RetentionOverride{Tiers: []RetentionTier{{Name: "last_known_good", Granularity: GranularityDay, Keep: 3}}},
			want:     TierLastKnownGoodName,
		},
		{
			name:     "a granularity nothing has ever accepted",
			override: RetentionOverride{Tiers: []RetentionTier{{Name: "fortnight", Granularity: "fortnight", Keep: 3}}},
			want:     "granularity",
		},
		{
			name: "both spellings of the chain at once",
			override: RetentionOverride{
				DailyDays: 1, WeeklyMonths: 1, MonthlyMonths: 1,
				Tiers: []RetentionTier{{Name: "daily", Granularity: GranularityDay, Keep: 3}},
			},
			want: "daily_days",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := openRetentionTestService(t)
			_, err := svc.SetBackupSetRetention(context.Background(), theSet, tt.override)
			if err == nil {
				t.Fatalf("accepted %+v, which the same value in the config file is refused for", tt.override)
			}
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("refusal is not an ErrInvalidRequest: %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("refusal does not carry %q; it reads: %v", tt.want, err)
			}
		})
	}
}

// TestSetBackupSetRetention_RefusesAnEmptyPolicy covers the two shapes
// that mean "I asked for nothing": an override with no field at all, and
// one whose chain is explicitly empty.
//
// An empty `retention: {}` block is the trap RetentionConfig's pointer
// exists to keep distinguishable from an absent one, and an empty tiers
// list widens rather than disables. Neither may be accepted, and the
// message for each has to say what the operator probably meant.
func TestSetBackupSetRetention_RefusesAnEmptyPolicy(t *testing.T) {
	tests := []struct {
		name     string
		override RetentionOverride
		want     string
	}{
		{
			name:     "an override naming nothing at all",
			override: RetentionOverride{},
			want:     "clear its override instead",
		},
		{
			name:     "an explicitly emptied chain",
			override: RetentionOverride{Tiers: []RetentionTier{}},
			want:     "it reinstates the default daily/weekly/monthly policy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := openRetentionTestService(t)
			_, err := svc.SetBackupSetRetention(context.Background(), theSet, tt.override)
			if err == nil {
				t.Fatalf("accepted %+v", tt.override)
			}
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("refusal is not an ErrInvalidRequest: %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("refusal does not carry %q; it reads: %v", tt.want, err)
			}
		})
	}
}

// TestSetBackupSetRetention_ReplacesRatherThanMerges pins the rule the
// whole issue turns on, at the one layer where a merge would be easy to
// write by accident: submitting a second override does not combine with
// the first.
func TestSetBackupSetRetention_ReplacesRatherThanMerges(t *testing.T) {
	svc, _ := openRetentionTestService(t)

	if _, err := svc.SetBackupSetRetention(context.Background(), theSet, RetentionOverride{
		Timezone: "Europe/Berlin",
		Tiers: []RetentionTier{
			{Name: "daily", Granularity: GranularityDay, Keep: 30},
			{Name: "annual", Granularity: GranularityYear, Keep: 7},
		},
	}); err != nil {
		t.Fatalf("first SetBackupSetRetention: %v", err)
	}

	got, err := svc.SetBackupSetRetention(context.Background(), theSet, RetentionOverride{
		Tiers: []RetentionTier{{Name: "daily", Granularity: GranularityDay, Keep: 30}},
	})
	if err != nil {
		t.Fatalf("second SetBackupSetRetention: %v", err)
	}
	if want := "daily/30"; tierNames(got.Effective.Tiers) != want {
		t.Fatalf("chain after the second submission = %q, want %q; the annual tier survived, so this merged",
			tierNames(got.Effective.Tiers), want)
	}
	if got.Effective.Timezone != "America/Vancouver" {
		t.Fatalf("timezone after the second submission = %q; the first override's Europe/Berlin survived, so this merged",
			got.Effective.Timezone)
	}
}

// TestBackupSetRetention_NamesAnUnknownSetRatherThanAnsweringForIt is the
// hazard lookupBackupSet exists for. A zero config.BackupSet has a nil
// RetentionConfig, which reads exactly like a real set that inherits, so a
// lookup that folded "missing" into a zero value would answer "retained
// under the deployment's policy" for a backup set that does not exist.
func TestBackupSetRetention_NamesAnUnknownSetRatherThanAnsweringForIt(t *testing.T) {
	svc, _ := openRetentionTestService(t)

	for _, id := range []string{"production/nope", "nope/postgres-primary", "not-an-id", "a/b/c"} {
		if _, err := svc.BackupSetRetention(context.Background(), id); !errors.Is(err, ErrBackupSetNotFound) {
			t.Fatalf("BackupSetRetention(%q) = %v, want ErrBackupSetNotFound", id, err)
		}
		if _, err := svc.ClearBackupSetRetention(context.Background(), id); !errors.Is(err, ErrBackupSetNotFound) {
			t.Fatalf("ClearBackupSetRetention(%q) = %v, want ErrBackupSetNotFound", id, err)
		}
		if _, err := svc.SetBackupSetRetention(context.Background(), id, RetentionOverride{DailyDays: 1, WeeklyMonths: 1, MonthlyMonths: 1}); !errors.Is(err, ErrBackupSetNotFound) {
			t.Fatalf("SetBackupSetRetention(%q) = %v, want ErrBackupSetNotFound", id, err)
		}
	}
}

// TestSetBackupSetRetention_AnOverrideSurvivesAnUnrelatedSettingsSave is
// the issue's second Given/When/Then: once a set declares its own chain,
// editing the DEPLOYMENT's policy does not move it.
//
// This is the whole promise of an override, and the one a resolution
// order could break silently: a save that re-resolved every set from the
// new global policy without re-reading their own blocks would leave the
// file right and every decision wrong.
func TestSetBackupSetRetention_AnOverrideSurvivesAnUnrelatedSettingsSave(t *testing.T) {
	svc, configPath := openRetentionTestService(t)

	if _, err := svc.SetBackupSetRetention(context.Background(), theSet, RetentionOverride{
		DailyDays: 3, WeeklyMonths: 1, MonthlyMonths: 2,
	}); err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}

	if _, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{
			Tiers: []RetentionTier{{Name: "weekly", Granularity: GranularityWeek, Keep: 4}},
		},
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	got, err := svc.BackupSetRetention(context.Background(), theSet)
	if err != nil {
		t.Fatalf("BackupSetRetention: %v", err)
	}
	if !got.IsOverride {
		t.Fatalf("a global settings save cleared this set's own override")
	}
	if want := "daily/3 weekly/1 monthly/2"; tierNames(got.Effective.Tiers) != want {
		t.Fatalf("the set's chain after a global save = %q, want %q", tierNames(got.Effective.Tiers), want)
	}
	if want := "weekly/4"; tierNames(got.Deployment.Tiers) != want {
		t.Fatalf("the deployment chain after the save = %q, want %q", tierNames(got.Deployment.Tiers), want)
	}

	persisted := loadPersistedSet(t, configPath)
	if persisted.RetentionConfig == nil || persisted.RetentionConfig.DailyDays != 3 {
		t.Fatalf("the persisted override did not survive a global settings save: %+v", persisted.RetentionConfig)
	}
}

// TestSetBackupSetRetention_RefusesWithoutAConfigFile matches every other
// write method on this service: a service built in memory has nothing to
// persist to, and says so rather than silently changing a running policy
// that no restart would keep.
func TestSetBackupSetRetention_RefusesWithoutAConfigFile(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.SetBackupSetRetention(context.Background(), theSet, RetentionOverride{DailyDays: 1, WeeklyMonths: 1, MonthlyMonths: 1}); !errors.Is(err, ErrConfigNotFileBacked) {
		t.Fatalf("SetBackupSetRetention = %v, want ErrConfigNotFileBacked", err)
	}
	if _, err := svc.ClearBackupSetRetention(context.Background(), theSet); !errors.Is(err, ErrConfigNotFileBacked) {
		t.Fatalf("ClearBackupSetRetention = %v, want ErrConfigNotFileBacked", err)
	}
}

// loadPersistedSet reads the ONE backup set out of the config file on
// disk, without validating, so a test can see exactly what was written
// rather than what a load would resolve it into. That distinction is the
// whole subject here: the resolved chain and the operator's own override
// block are different things and only one of them belongs in the file.
func loadPersistedSet(t *testing.T, configPath string) config.BackupSet {
	t.Helper()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, raw)
	}
	if len(cfg.Sources) == 0 || len(cfg.Sources[0].BackupSets) == 0 {
		t.Fatalf("the config file has no backup set:\n%s", raw)
	}
	return cfg.Sources[0].BackupSets[0]
}

// TestPreviewRetention_SaysWhichPolicyDecidedIt is the API half of the
// issue's "the preview says which policy it applied" criterion.
//
// The CLI's own `retention` command has said this since #362. The
// preview/apply envelope this boundary serves to the Web UI did not: it
// reported counts, verdicts and two revisions, and nothing about where
// the chain came from. "Why is this artifact being deleted" has a
// different answer, and a different fix, depending on which policy was in
// force, so a preview that cannot say which one is missing the half an
// operator acts on.
func TestPreviewRetention_SaysWhichPolicyDecidedIt(t *testing.T) {
	svc, _ := openRetentionTestService(t)
	ctx := context.Background()

	plan, err := svc.PreviewRetention(ctx, "production", "postgres-primary")
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}
	if plan.RetentionIsOverride {
		t.Fatalf("an inheriting set's plan claims an override")
	}
	if want := "daily/90 weekly/24 monthly/60"; tierNames(plan.Retention.Tiers) != want {
		t.Fatalf("plan chain = %q, want the deployment's %q", tierNames(plan.Retention.Tiers), want)
	}

	if _, err := svc.SetBackupSetRetention(ctx, theSet, RetentionOverride{
		Tiers: []RetentionTier{{Name: "daily", Granularity: GranularityDay, Keep: 4}},
	}); err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}

	plan, err = svc.PreviewRetention(ctx, "production", "postgres-primary")
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}
	if !plan.RetentionIsOverride {
		t.Fatalf("a plan computed under the set's own policy does not say so")
	}
	if want := "daily/4"; tierNames(plan.Retention.Tiers) != want {
		t.Fatalf("plan chain = %q, want the set's own %q", tierNames(plan.Retention.Tiers), want)
	}
	// The attribution is not derivable by comparing the two chains, which
	// is why it is reported rather than inferred: a set can pin a chain
	// identical to the deployment's, and the whole point of pinning it is
	// that a later edit to the deployment's policy will not move it.
	if plan.Retention.Timezone != "America/Vancouver" {
		t.Fatalf("plan timezone = %q, want the deployment's, which an override with no calendar inherits", plan.Retention.Timezone)
	}
}

// TestPreviewRetention_AnIdenticalChainIsStillAnOverride is the control
// for the sentence above. Attribution that was computed by comparing the
// effective chain against the deployment's would pass every other case in
// this file and be wrong here, which is the case an operator hits after
// deliberately pinning a set to today's policy.
func TestPreviewRetention_AnIdenticalChainIsStillAnOverride(t *testing.T) {
	svc, _ := openRetentionTestService(t)
	ctx := context.Background()

	if _, err := svc.SetBackupSetRetention(ctx, theSet, RetentionOverride{
		Timezone: "America/Vancouver", WeekStartsOn: "sunday",
		DailyDays: 90, WeeklyMonths: 24, MonthlyMonths: 60,
	}); err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}

	plan, err := svc.PreviewRetention(ctx, "production", "postgres-primary")
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}
	if !plan.RetentionIsOverride {
		t.Fatalf("a set pinned to a chain identical to the deployment's is reported as inheriting")
	}

	got, err := svc.BackupSetRetention(ctx, theSet)
	if err != nil {
		t.Fatalf("BackupSetRetention: %v", err)
	}
	if !got.IsOverride {
		t.Fatalf("BackupSetRetention reports the same set as inheriting")
	}
}
