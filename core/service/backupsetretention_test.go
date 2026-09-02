package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// tierChain is the shorthand these tests write a chain with. A chain
// rather than the three legacy scalars everywhere, because that is the
// only spelling this surface accepts, for the reason
// backupsetretention.go's own doc gives.
func tierChain(tiers ...RetentionTier) []RetentionTier { return tiers }

func dailyTier(keep int) RetentionTier {
	return RetentionTier{Name: "daily", Granularity: GranularityDay, Keep: keep}
}

func monthlyTier(keep int) RetentionTier {
	return RetentionTier{Name: "monthly", Granularity: GranularityMonth, Keep: keep}
}

// TestGetBackupSetRetention_ASetWithNoBlockInheritsTheDeploymentPolicy is
// the base case the whole feature rests on, and it is the one an upgrade
// has to keep true: a configuration written before per-set retention
// existed carries no block on any set, and every one of those sets has to
// go on being retained exactly as it was.
func TestGetBackupSetRetention_ASetWithNoBlockInheritsTheDeploymentPolicy(t *testing.T) {
	svc, _ := openTestService(t)

	got, err := svc.GetBackupSetRetention(context.Background(), fixtureSetID)
	if err != nil {
		t.Fatalf("GetBackupSetRetention: %v", err)
	}
	if got.IsOverride {
		t.Error("IsOverride is true for a set whose configuration carries no retention block")
	}
	if DescribeRetentionPolicy(got.Policy) != DescribeRetentionPolicy(got.DeploymentPolicy) {
		t.Errorf("an inheriting set decides under %q, but the deployment policy is %q",
			DescribeRetentionPolicy(got.Policy), DescribeRetentionPolicy(got.DeploymentPolicy))
	}
	// A resolved chain, never the raw block: the question is what this
	// set is retained under, and a caller must not have to redo the
	// legacy-scalar resolution in its head.
	if len(got.Policy.Tiers) == 0 {
		t.Error("the reported policy names no tier at all; it has to be the RESOLVED chain")
	}
}

// TestSetBackupSetRetention_TheSetThenDecidesForItself is the feature: one
// set on its own chain, the deployment's own policy untouched, and the
// change visible immediately through the same service that wrote it.
func TestSetBackupSetRetention_TheSetThenDecidesForItself(t *testing.T) {
	svc, configPath := openTestService(t)
	before, err := svc.GetBackupSetRetention(context.Background(), fixtureSetID)
	if err != nil {
		t.Fatalf("GetBackupSetRetention: %v", err)
	}

	got, err := svc.SetBackupSetRetention(context.Background(), fixtureSetID, BackupSetRetentionOverride{
		Tiers: tierChain(dailyTier(30), monthlyTier(24)),
	})
	if err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}
	if !got.IsOverride {
		t.Error("IsOverride is false immediately after setting an override")
	}
	if len(got.Policy.Tiers) != 2 || got.Policy.Tiers[0].Keep != 30 || got.Policy.Tiers[1].Keep != 24 {
		t.Errorf("the set decides under %q, want the 30-day/24-month chain that was just set", DescribeRetentionPolicy(got.Policy))
	}
	// The deployment's own policy is not what was edited.
	if DescribeRetentionPolicy(got.DeploymentPolicy) != DescribeRetentionPolicy(before.DeploymentPolicy) {
		t.Errorf("the deployment policy moved from %q to %q while one set's override was being set",
			DescribeRetentionPolicy(before.DeploymentPolicy), DescribeRetentionPolicy(got.DeploymentPolicy))
	}
	// And it is durable, not only in memory: the whole point is that an
	// operator no longer has to write this key by hand.
	raw := mustRead(t, configPath)
	if !strings.Contains(raw, "retention:") || !strings.Contains(raw, "name: daily") {
		t.Errorf("the configuration file does not carry the set's own chain:\n%s", raw)
	}
}

// TestSetBackupSetRetention_AGlobalEditDoesNotMoveAnOverriddenSet is the
// issue's own GIVEN/WHEN/THEN: a set that declares its own chain does not
// change its retention decisions when the deployment's policy is edited.
func TestSetBackupSetRetention_AGlobalEditDoesNotMoveAnOverriddenSet(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	if _, err := svc.SetBackupSetRetention(ctx, fixtureSetID, BackupSetRetentionOverride{
		Tiers: tierChain(dailyTier(30), monthlyTier(24)),
	}); err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}

	if _, err := svc.UpdateSettings(ctx, UpdateSettingsRequest{
		Retention: &RetentionUpdate{Tiers: tierChain(dailyTier(2))},
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	after, err := svc.GetBackupSetRetention(ctx, fixtureSetID)
	if err != nil {
		t.Fatalf("GetBackupSetRetention: %v", err)
	}
	if len(after.Policy.Tiers) != 2 || after.Policy.Tiers[0].Keep != 30 {
		t.Errorf("the overridden set now decides under %q; editing the deployment's policy moved it", DescribeRetentionPolicy(after.Policy))
	}
	if len(after.DeploymentPolicy.Tiers) != 1 {
		t.Fatalf("the deployment policy did not actually change (%q), so this test proves nothing", DescribeRetentionPolicy(after.DeploymentPolicy))
	}
}

// TestClearBackupSetRetention_ReturnsTheSetToTheDeploymentPolicy, with no
// residue of the chain it used to declare: the issue asks for that
// specifically, and a leftover key would freeze the set at whatever the
// deployment's policy was the day it was cleared.
func TestClearBackupSetRetention_ReturnsTheSetToTheDeploymentPolicy(t *testing.T) {
	svc, configPath := openTestService(t)
	ctx := context.Background()

	if _, err := svc.SetBackupSetRetention(ctx, fixtureSetID, BackupSetRetentionOverride{
		Tiers: tierChain(dailyTier(30)),
	}); err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}

	cleared, err := svc.ClearBackupSetRetention(ctx, fixtureSetID)
	if err != nil {
		t.Fatalf("ClearBackupSetRetention: %v", err)
	}
	if cleared.IsOverride {
		t.Error("IsOverride is still true after clearing")
	}
	if DescribeRetentionPolicy(cleared.Policy) != DescribeRetentionPolicy(cleared.DeploymentPolicy) {
		t.Errorf("after clearing, the set decides under %q while the deployment policy is %q",
			DescribeRetentionPolicy(cleared.Policy), DescribeRetentionPolicy(cleared.DeploymentPolicy))
	}
	if raw := mustRead(t, configPath); strings.Contains(raw, "name: daily") {
		t.Errorf("the cleared chain is still in the configuration file:\n%s", raw)
	}
}

// TestClearBackupSetRetention_OnASetThatNeverOverrodeChangesNothing.
// Clearing what is not there is the state the caller asked for, so it is
// a success; it must also not rewrite the operator's file, because a
// no-op write moves ConfigRevision and invalidates every outstanding
// retention preview for a request that changed nothing.
func TestClearBackupSetRetention_OnASetThatNeverOverrodeChangesNothing(t *testing.T) {
	svc, configPath := openTestService(t)
	before := mustRead(t, configPath)

	got, err := svc.ClearBackupSetRetention(context.Background(), fixtureSetID)
	if err != nil {
		t.Fatalf("ClearBackupSetRetention on a set with no override: %v", err)
	}
	if got.IsOverride {
		t.Error("IsOverride is true for a set that never had one")
	}
	if after := mustRead(t, configPath); after != before {
		t.Error("clearing an override that did not exist rewrote the configuration file")
	}
}

// TestSetBackupSetRetention_RefusesAChainOnTheSameTermsAGlobalOneIsRefused
// is the issue's "a per-set chain that would be refused as a global one
// being refused identically". It drives the SAME bad chains through both
// write paths and requires the same reason out of each, because a second
// wording is the first step towards a second rule.
func TestSetBackupSetRetention_RefusesAChainOnTheSameTermsAGlobalOneIsRefused(t *testing.T) {
	bad := []struct {
		name  string
		tiers []RetentionTier
	}{
		{"reserved tier name", tierChain(RetentionTier{Name: TierLastKnownGoodName, Granularity: GranularityDay, Keep: 7})},
		{"unknown granularity", tierChain(RetentionTier{Name: "daily", Granularity: "fortnight", Keep: 7})},
		{"keep of zero", tierChain(RetentionTier{Name: "daily", Granularity: GranularityDay, Keep: 0})},
		{"duplicate tier names", tierChain(dailyTier(7), dailyTier(9))},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			perSet, _ := openTestService(t)
			_, setErr := perSet.SetBackupSetRetention(context.Background(), fixtureSetID, BackupSetRetentionOverride{Tiers: tc.tiers})
			if setErr == nil {
				t.Fatal("a per-set override accepted a chain a deployment policy would be refused for")
			}

			global, _ := openTestService(t)
			_, globalErr := global.UpdateSettings(context.Background(), UpdateSettingsRequest{
				Retention: &RetentionUpdate{Tiers: tc.tiers},
			})
			if globalErr == nil {
				t.Fatal("the deployment-wide path accepted this chain, so there is no refusal to compare against")
			}

			// The reason itself, not the path it came down. Both are
			// wrapped as an invalid request and both carry the resolver's
			// own sentence; the per-set one prepends which set it was
			// about, which is information, not a different rule.
			reason := resolverReason(t, globalErr.Error())
			if !strings.Contains(setErr.Error(), reason) {
				t.Errorf("the per-set refusal says\n  %v\nbut a deployment-wide one says\n  %v\nand the two have to give the same reason", setErr, globalErr)
			}
			if !errors.Is(setErr, ErrInvalidRequest) || !errors.Is(globalErr, ErrInvalidRequest) {
				t.Errorf("the two refusals are not the same class of error: per-set %v, deployment %v", setErr, globalErr)
			}
		})
	}
}

// resolverReason returns the resolver's own sentence out of a validation
// error, dropping the config path it is filed under.
//
// The two write paths legitimately differ in that path (a deployment
// policy is "retention.tiers[0]", a set's is
// "sources[0].backup_sets[0].retention.tiers[0]") and comparing whole
// messages would compare the paths rather than the rule. It cuts at the
// FIRST ": " after the "invalid config: " header, because the path never
// contains one and the reason routinely does ("is not one of: day, week,
// ...").
//
// It fails the test rather than returning something short when it cannot
// find the shape it expects. An earlier version of this helper cut at the
// LAST occurrence of "retention" instead, which landed inside the message
// body and left it comparing fragments like " tier": a comparison that
// would have passed against two genuinely different reasons.
func resolverReason(t *testing.T, msg string) string {
	t.Helper()
	const header = "invalid config: "
	i := strings.Index(msg, header)
	if i < 0 {
		t.Fatalf("this refusal is not a config validation error, so there is no reason to compare: %s", msg)
	}
	rest := msg[i+len(header):]
	j := strings.Index(rest, ": ")
	if j < 0 {
		t.Fatalf("this refusal carries no config path, so the comparison below would be against the whole message: %s", msg)
	}
	reason := rest[j+2:]
	if len(reason) < 20 {
		t.Fatalf("the extracted reason %q is too short to be a real comparison; the message shape has changed: %s", reason, msg)
	}
	return reason
}

// TestSetBackupSetRetention_RefusesAnEmptyChain. There is no "keep
// nothing" spelling in this schema, and an empty chain read as one would
// put every managed backup in the set on the delete side. Going back to
// the deployment's policy is what clear is for, and the refusal says so.
func TestSetBackupSetRetention_RefusesAnEmptyChain(t *testing.T) {
	svc, configPath := openTestService(t)
	before := mustRead(t, configPath)

	_, err := svc.SetBackupSetRetention(context.Background(), fixtureSetID, BackupSetRetentionOverride{})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "clear") {
		t.Errorf("the refusal does not point at clearing the override, which is what the caller probably meant: %v", err)
	}
	if after := mustRead(t, configPath); after != before {
		t.Error("a refused override rewrote the configuration file")
	}
}

// TestSetBackupSetRetention_InheritsTheCalendarItDoesNotName. The
// timezone and the week start decide how ANY chain is reckoned rather
// than what the chain says, so an override that omits them must not
// silently reintroduce the day-boundary problem for one set inside a
// deployment that got it right.
func TestSetBackupSetRetention_InheritsTheCalendarItDoesNotName(t *testing.T) {
	configPath := writeTestConfigFileWithRetention(t,
		"retention:\n  timezone: America/Vancouver\n  week_starts_on: sunday\n")
	svc := openServiceAt(t, configPath)

	got, err := svc.SetBackupSetRetention(context.Background(), fixtureSetID, BackupSetRetentionOverride{
		Tiers: tierChain(dailyTier(30)),
	})
	if err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}
	if got.Policy.Timezone != "America/Vancouver" {
		t.Errorf("Timezone = %q, want the deployment's America/Vancouver", got.Policy.Timezone)
	}
	if got.Policy.WeekStartsOn != "sunday" {
		t.Errorf("WeekStartsOn = %q, want the deployment's sunday", got.Policy.WeekStartsOn)
	}
	// And an override that DOES name its own calendar keeps it:
	// inheriting is what an omitted field means, not what every field
	// means.
	named, err := svc.SetBackupSetRetention(context.Background(), fixtureSetID, BackupSetRetentionOverride{
		Tiers:    tierChain(dailyTier(30)),
		Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("SetBackupSetRetention with its own timezone: %v", err)
	}
	if named.Policy.Timezone != "UTC" {
		t.Errorf("Timezone = %q, want the override's own UTC", named.Policy.Timezone)
	}
}

// TestBackupSetRetention_UnknownSetIsNotFound on all three operations. A
// success for a name this deployment does not configure would read as
// "that set now has this policy", which is the worst possible answer for
// a retention write.
func TestBackupSetRetention_UnknownSetIsNotFound(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()

	for _, id := range []string{"production/nope", "nope/postgres-primary", "no-slash", ""} {
		if _, err := svc.GetBackupSetRetention(ctx, id); !errors.Is(err, ErrBackupSetNotFound) {
			t.Errorf("GetBackupSetRetention(%q) error = %v, want ErrBackupSetNotFound", id, err)
		}
		if _, err := svc.SetBackupSetRetention(ctx, id, BackupSetRetentionOverride{Tiers: tierChain(dailyTier(7))}); !errors.Is(err, ErrBackupSetNotFound) {
			t.Errorf("SetBackupSetRetention(%q) error = %v, want ErrBackupSetNotFound", id, err)
		}
		if _, err := svc.ClearBackupSetRetention(ctx, id); !errors.Is(err, ErrBackupSetNotFound) {
			t.Errorf("ClearBackupSetRetention(%q) error = %v, want ErrBackupSetNotFound", id, err)
		}
	}
}

// TestSetBackupSetRetention_LeavesTheRestOfTheSetAlone. Setting a
// retention override goes through the same sparse update path every other
// field does, so nothing else may move; a whole-set write here would be
// the silent clobber that path exists to prevent.
func TestSetBackupSetRetention_LeavesTheRestOfTheSetAlone(t *testing.T) {
	svc, configPath := openTestService(t)
	ctx := context.Background()
	// Through the same YAML round trip the write path performs, so the
	// comparison is about what the update did and not about
	// yaml.Marshal turning a nil []string into an empty one (see
	// yamlNormalized's own doc).
	before := yamlNormalized(t, readBackupSetFromDisk(t, configPath, "production", "postgres-primary"))

	if _, err := svc.SetBackupSetRetention(ctx, fixtureSetID, BackupSetRetentionOverride{
		Tiers: tierChain(dailyTier(30)),
	}); err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}

	after := readBackupSetFromDisk(t, configPath, "production", "postgres-primary")
	if after.RetentionConfig == nil {
		t.Fatal("the override was not written at all, so this isolation check would pass vacuously")
	}
	// Neutralised, so the comparison is about everything EXCEPT the field
	// under test rather than about the field under test.
	after.RetentionConfig = before.RetentionConfig
	if !reflect.DeepEqual(before, after) {
		t.Errorf("setting a retention override changed something else on the set:\nbefore %+v\nafter  %+v", before, after)
	}
}
