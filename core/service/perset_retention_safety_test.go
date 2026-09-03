package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// This file is the safety half of issue #333, and it is separate from
// backupsetretention_test.go because it asks a different question. That
// file asks "does the override work". This one asks "what did giving one
// backup set an override do to every OTHER set, and to the file itself",
// which is the question that matters on a deployment that already exists
// and already has backups it expects to keep.
//
// Retention decides what gets DELETED, so the dangerous part of this
// feature was never the feature. It is the upgrade: every backup set
// written before this schema existed has no retention block, and every
// one of them has to keep being retained exactly as it was until somebody
// deliberately says otherwise. There is no "migrate" step to run, which
// is the point (nothing rewrites an operator's file on upgrade), so the
// proofs below are about the two things that COULD rewrite it: a write to
// one set, and a write that fails halfway.

// twoSetConfig writes a deployment with two backup sets under one source,
// neither of them declaring a retention policy, plus a deployment policy
// that is deliberately not the product default 7/3/12. Both halves
// matter: the second set is the pre-existing row every assertion here is
// really about, and a deployment chain that already WAS the default would
// hide a resolution bug that reaches for the defaults instead of this
// deployment's own policy.
//
// The config file lives in its own directory, separate from the state
// database, so a test can take write permission away from the
// configuration directory without also breaking SQLite underneath the
// running service.
func twoSetConfig(t *testing.T) (configPath string, confDir string) {
	t.Helper()
	dir := t.TempDir()
	confDir = filepath.Join(dir, "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	remoteDir := filepath.Join(dir, "remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	set := func(name string) string {
		return "      - id: " + name + "\n" +
			"        remote:\n" +
			"          type: local\n" +
			"        remote_path: " + remoteDir + "\n" +
			"        local_path: " + filepath.Join(dir, "local-"+name) + "\n" +
			"        include:\n" +
			"          - \"*.dump\"\n" +
			"        completion:\n" +
			"          strategy: rename\n" +
			"        stale_after: 24h\n"
	}

	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		set("postgres-primary") +
		set("media-share") +
		deploymentChain

	configPath = filepath.Join(confDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath, confDir
}

func openTwoSetService(t *testing.T) (*BackupService, string, string) {
	t.Helper()
	configPath, confDir := twoSetConfig(t)
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	return svc, configPath, confDir
}

// parsePersistedConfig reads the configuration file the way a fresh boot
// would, WITHOUT validating it, and puts it through one marshal/unmarshal
// cycle before handing it back.
//
// Unvalidated is the point: Validate fills in resolved fields, and a
// comparison of two validated configs would compare what this package
// computed rather than what it wrote down.
//
// The extra cycle is what makes a hand-written file comparable with one
// this service wrote. Every write path here re-marshals the whole Config,
// and yaml's round trip normalises an absent list into an empty one
// (`command: []` reads back as []string{} where the hand-written file had
// nil). That is pre-existing behaviour of every write method on this
// service rather than anything #333 introduced, and it is not the
// question these tests ask, so both sides get it done to them once and
// the comparison is left to be about content.
func parsePersistedConfig(t *testing.T, configPath string) config.Config {
	t.Helper()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, raw)
	}
	round, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var normalised config.Config
	if err := yaml.Unmarshal(round, &normalised); err != nil {
		t.Fatalf("Unmarshal of the re-marshalled config: %v\n%s", err, round)
	}
	return normalised
}

// describeConfigDifference names WHERE two parsed configurations differ,
// down to the field, instead of printing two whole documents and leaving
// the reader to spot it.
//
// This exists because the assertion it serves is deliberately a
// whole-file one: a write here re-marshals every block, so the only
// honest bound on its blast radius is "nothing else changed at all". That
// assertion is worth nothing if its failure is unreadable, and two
// hundred-field struct dumps side by side is unreadable.
func describeConfigDifference(before, after config.Config) string {
	bv := reflect.ValueOf(before)
	av := reflect.ValueOf(after)
	for i := 0; i < bv.NumField(); i++ {
		name := bv.Type().Field(i).Name
		b := bv.Field(i).Interface()
		a := av.Field(i).Interface()
		if reflect.DeepEqual(b, a) {
			continue
		}
		if name == "Sources" {
			if d := describeSourcesDifference(before.Sources, after.Sources); d != "" {
				return d
			}
		}
		return fmt.Sprintf("%s:\n  before: %#v\n  after:  %#v", name, b, a)
	}
	return ""
}

func describeSourcesDifference(before, after []config.Source) string {
	if len(before) != len(after) {
		return fmt.Sprintf("the number of sources changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if len(before[i].BackupSets) != len(after[i].BackupSets) {
			return fmt.Sprintf("sources[%d] %q: the number of backup sets changed: %d -> %d",
				i, before[i].Name, len(before[i].BackupSets), len(after[i].BackupSets))
		}
		for j := range before[i].BackupSets {
			b := reflect.ValueOf(before[i].BackupSets[j])
			a := reflect.ValueOf(after[i].BackupSets[j])
			for k := 0; k < b.NumField(); k++ {
				bf := b.Field(k).Interface()
				af := a.Field(k).Interface()
				if reflect.DeepEqual(bf, af) {
					continue
				}
				return fmt.Sprintf("sources[%d].backup_sets[%d] (%s/%s) field %s:\n  before: %#v\n  after:  %#v",
					i, j, before[i].Name, before[i].BackupSets[j].Name, b.Type().Field(k).Name, bf, af)
			}
		}
	}
	return ""
}

func persistedSetNamed(t *testing.T, cfg config.Config, name string) *config.BackupSet {
	t.Helper()
	for i := range cfg.Sources {
		for j := range cfg.Sources[i].BackupSets {
			if cfg.Sources[i].BackupSets[j].Name == name {
				return &cfg.Sources[i].BackupSets[j]
			}
		}
	}
	t.Fatalf("the config file has no backup set named %q", name)
	return nil
}

const otherSet = "production/media-share"

// TestSetBackupSetRetention_ChangesExactlyOneFieldInTheWholeFile is the
// upgrade-safety proof this issue actually needs, and it is deliberately
// not phrased as "the other set still inherits".
//
// A write here re-marshals the ENTIRE configuration file: every source,
// every backup set, every unrelated block. So the blast radius of giving
// one set a retention policy is the whole file, and the only honest way
// to bound it is to diff the whole file. The assertion is therefore
// exact: parse before, parse after, put the one field back, and require
// the two to be identical structures. Anything this write touched that it
// was not asked to touch fails here, including a field nobody has thought
// to write a test for yet.
func TestSetBackupSetRetention_ChangesExactlyOneFieldInTheWholeFile(t *testing.T) {
	svc, configPath, _ := openTwoSetService(t)

	before := parsePersistedConfig(t, configPath)

	if _, err := svc.SetBackupSetRetention(context.Background(), theSet, RetentionOverride{
		DailyDays: 3, WeeklyMonths: 1, MonthlyMonths: 2,
	}); err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}

	after := parsePersistedConfig(t, configPath)

	// Precondition: the write actually happened. Without this the
	// comparison below would pass just as well against a no-op.
	overridden := persistedSetNamed(t, after, "postgres-primary")
	if overridden.RetentionConfig == nil {
		t.Fatal("the set that was given an override carries no retention block in the file")
	}
	if overridden.RetentionConfig.DailyDays != 3 {
		t.Fatalf("persisted override = %+v, want the submitted three scalars", overridden.RetentionConfig)
	}

	// Put the one intended change back, and require everything else to be
	// untouched.
	overridden.RetentionConfig = nil
	if d := describeConfigDifference(before, after); d != "" {
		t.Fatalf("giving one backup set a retention policy changed something else in the file: %s", d)
	}
}

// TestSetBackupSetRetention_LeavesTheOtherSetRetainedExactlyAsBefore is
// the same property asked at the layer that decides, rather than at the
// file. A file can be right while the running process decides wrongly:
// resolution happens on every load, so a set that gained no block could
// still be re-resolved onto the wrong chain.
func TestSetBackupSetRetention_LeavesTheOtherSetRetainedExactlyAsBefore(t *testing.T) {
	svc, _, _ := openTwoSetService(t)
	ctx := context.Background()

	before, err := svc.BackupSetRetention(ctx, otherSet)
	if err != nil {
		t.Fatalf("BackupSetRetention(%s): %v", otherSet, err)
	}
	if before.IsOverride {
		t.Fatalf("precondition failed: %s already declares an override", otherSet)
	}
	if want := "daily/90 weekly/24 monthly/60"; tierNames(before.Effective.Tiers) != want {
		t.Fatalf("precondition failed: %s is retained under %q, want the deployment's %q", otherSet, tierNames(before.Effective.Tiers), want)
	}

	if _, err := svc.SetBackupSetRetention(ctx, theSet, RetentionOverride{
		DailyDays: 3, WeeklyMonths: 1, MonthlyMonths: 2,
	}); err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}

	after, err := svc.BackupSetRetention(ctx, otherSet)
	if err != nil {
		t.Fatalf("BackupSetRetention(%s) after the write: %v", otherSet, err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the set that was not touched is now retained differently.\nbefore: %+v\nafter:  %+v", before, after)
	}
}

// TestBackupSetRetentionWrites_AreIdempotent is the "run it twice" proof.
//
// Every write here re-marshals the whole file, so "did anything change"
// is a question about bytes rather than about meaning: a write that
// normalised something on each pass would keep producing a different file
// from the same request, and an operator (or a config-management tool)
// repeating a call would keep seeing a diff. Both directions are checked,
// because setting and clearing are two different mutations of the same
// document.
func TestBackupSetRetentionWrites_AreIdempotent(t *testing.T) {
	svc, configPath, _ := openTwoSetService(t)
	ctx := context.Background()

	// A tiers chain rather than the three scalars, because a chain is the
	// shape a merge bug shows up in: a write that folded the submission
	// onto what the set already declared, instead of replacing it, leaves
	// a scalar policy looking identical and a chain twice as long.
	policy := RetentionOverride{
		Tiers: []RetentionTier{
			{Name: "daily", Granularity: GranularityDay, Keep: 14},
			{Name: "annual", Granularity: GranularityYear, Keep: 5},
		},
	}

	if _, err := svc.SetBackupSetRetention(ctx, theSet, policy); err != nil {
		t.Fatalf("first SetBackupSetRetention: %v", err)
	}
	firstSet, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if _, err := svc.SetBackupSetRetention(ctx, theSet, policy); err != nil {
		t.Fatalf("second SetBackupSetRetention: %v", err)
	}
	secondSet, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(firstSet) != string(secondSet) {
		t.Fatalf("writing the same policy twice produced two different files.\nfirst:\n%s\nsecond:\n%s", firstSet, secondSet)
	}

	if _, err := svc.ClearBackupSetRetention(ctx, theSet); err != nil {
		t.Fatalf("first ClearBackupSetRetention: %v", err)
	}
	firstClear, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if _, err := svc.ClearBackupSetRetention(ctx, theSet); err != nil {
		t.Fatalf("second ClearBackupSetRetention: %v", err)
	}
	secondClear, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(firstClear) != string(secondClear) {
		t.Fatalf("clearing twice produced two different files.\nfirst:\n%s\nsecond:\n%s", firstClear, secondClear)
	}
}

// TestClearBackupSetRetention_LeavesNoResidueAtAll is this issue's third
// Given/When/Then taken literally: a set whose retention block is removed
// is retained under the deployment's chain again, "with no residue of the
// chain it used to declare".
//
// backupsetretention_test.go already checks the resolved answer and that
// the old numbers are gone from the text. This checks the whole document:
// the file after set-then-clear has to be byte-identical to the file
// after a write that never carried an override at all. The baseline is a
// clear on a set that has nothing to clear, because every write
// re-marshals, so comparing against the hand-written original would be
// comparing two different formattings rather than two different meanings.
func TestClearBackupSetRetention_LeavesNoResidueAtAll(t *testing.T) {
	svc, configPath, _ := openTwoSetService(t)
	ctx := context.Background()

	if _, err := svc.ClearBackupSetRetention(ctx, theSet); err != nil {
		t.Fatalf("baseline ClearBackupSetRetention: %v", err)
	}
	baseline, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if _, err := svc.SetBackupSetRetention(ctx, theSet, RetentionOverride{
		Timezone:     "Europe/Berlin",
		WeekStartsOn: "sunday",
		Tiers: []RetentionTier{
			{Name: "daily", Granularity: GranularityDay, Keep: 30},
			{Name: "annual", Granularity: GranularityYear, Keep: 7},
		},
	}); err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}
	withOverride, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(withOverride) == string(baseline) {
		t.Fatal("precondition failed: writing an override left the file unchanged")
	}

	if _, err := svc.ClearBackupSetRetention(ctx, theSet); err != nil {
		t.Fatalf("ClearBackupSetRetention: %v", err)
	}
	cleared, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(cleared) != string(baseline) {
		t.Fatalf("clearing an override did not return the file to what it was.\nbaseline:\n%s\ncleared:\n%s", baseline, cleared)
	}
}

// TestBackupSetRetentionWrites_ASetThatInheritsGainsNoRetentionKey is the
// upgrade property stated where an operator would notice it: a
// configuration file written before this schema existed must not sprout a
// retention block under a backup set just because something saved it.
//
// It matters beyond tidiness. Load's KnownFields(true) makes any key an
// older build does not know a hard parse error, so a set-level retention
// block written by accident is a config file a rollback cannot read at
// all, and retention is the surface an operator reaches for during an
// incident, which is also when a rollback is most likely.
func TestBackupSetRetentionWrites_ASetThatInheritsGainsNoRetentionKey(t *testing.T) {
	svc, configPath, _ := openTwoSetService(t)
	ctx := context.Background()

	// Three writes of three different shapes, none of them about the
	// second set.
	if _, err := svc.SetBackupSetRetention(ctx, theSet, RetentionOverride{
		DailyDays: 3, WeeklyMonths: 1, MonthlyMonths: 2,
	}); err != nil {
		t.Fatalf("SetBackupSetRetention: %v", err)
	}
	if _, err := svc.UpdateSettings(ctx, UpdateSettingsRequest{
		Retention: &RetentionUpdate{
			Tiers: []RetentionTier{{Name: "weekly", Granularity: GranularityWeek, Keep: 4}},
		},
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if _, err := svc.ClearBackupSetRetention(ctx, theSet); err != nil {
		t.Fatalf("ClearBackupSetRetention: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	cfg := parsePersistedConfig(t, configPath)
	for _, src := range cfg.Sources {
		for _, bs := range src.BackupSets {
			if bs.RetentionConfig != nil {
				t.Fatalf("backup set %s/%s gained a retention block it never asked for: %+v\n%s",
					src.Name, bs.Name, bs.RetentionConfig, raw)
			}
		}
	}

	// And the same question asked of the text, because the struct check
	// above cannot see the spelling that actually breaks a rollback: a
	// nil pointer written out as `retention: null` unmarshals straight
	// back to nil and looks clean, while an older build reads it as a key
	// it does not know and refuses the whole file under
	// KnownFields(true).
	//
	// Indentation is what tells the two retention keys apart: the
	// deployment's own block sits at column zero and any per-set one is
	// indented under a backup set. The precondition below is what stops
	// this loop from being a check that could never fire. If the
	// deployment's own key were not found at column zero, the scan is
	// looking at something other than what it thinks it is.
	sawDeploymentKey := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if !strings.HasPrefix(trimmed, "retention:") {
			continue
		}
		if indent := len(line) - len(trimmed); indent > 0 {
			t.Fatalf("a retention key is written under a backup set, which an older build would refuse to parse at all:\n%s", raw)
		}
		sawDeploymentKey = true
	}
	if !sawDeploymentKey {
		t.Fatalf("no top-level retention key at column zero; this scan is not reading what it thinks it is:\n%s", raw)
	}
}

// TestSetBackupSetRetention_AFailedWriteLeavesTheOldPolicyDeciding is
// what an interruption halfway leaves behind.
//
// writeBackupSetRetention validates, plans the validator catalog, writes
// the file and only then swaps the running state, and the write itself is
// a temp file plus an atomic rename. So there is no window in which the
// file is half a config: either the rename happened or it did not. This
// pins the "did not" branch, which is the one that decides what a
// retention pass does next: the file still parses, still says exactly
// what it said, and the RUNNING service is still deciding under the
// policy that was in force before the call.
//
// A retention policy is not something you can leave in an unknown state.
// A write that failed after mutating the in-memory config, or that left a
// half-written file behind, would hand the next prune a chain nobody
// wrote.
func TestSetBackupSetRetention_AFailedWriteLeavesTheOldPolicyDeciding(t *testing.T) {
	if os.Geteuid() == 0 {
		// Not a skip for convenience: root ignores the directory
		// permission this test uses to make the write fail, so the run
		// would prove nothing rather than proving something weaker.
		t.Skip("this test makes a write fail by removing write permission on a directory, which root ignores")
	}
	svc, configPath, confDir := openTwoSetService(t)
	ctx := context.Background()

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	policyBefore, err := svc.BackupSetRetention(ctx, theSet)
	if err != nil {
		t.Fatalf("BackupSetRetention: %v", err)
	}

	if err := os.Chmod(confDir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(confDir, 0o755) })

	_, err = svc.SetBackupSetRetention(ctx, theSet, RetentionOverride{
		DailyDays: 3, WeeklyMonths: 1, MonthlyMonths: 2,
	})
	if err == nil {
		t.Fatal("a write into a directory with no write permission reported success")
	}
	if !strings.Contains(err.Error(), "persisting configuration") {
		t.Fatalf("SetBackupSetRetention error = %v, want the persist step to be the one that failed", err)
	}

	if err := os.Chmod(confDir, 0o755); err != nil {
		t.Fatalf("Chmod back: %v", err)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile after the failed write: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("a failed write changed the configuration file.\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// Nothing half-written left in the directory either: the temp file
	// this write path creates is removed on every exit, and a leftover
	// config.yaml.tmp-* is a file an operator would find and wonder
	// about.
	entries, err := os.ReadDir(confDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("a failed write left a temporary file behind: %s", e.Name())
		}
	}

	policyAfter, err := svc.BackupSetRetention(ctx, theSet)
	if err != nil {
		t.Fatalf("BackupSetRetention after the failed write: %v", err)
	}
	if !reflect.DeepEqual(policyBefore, policyAfter) {
		t.Fatalf("a failed write changed the policy the running service decides under.\nbefore: %+v\nafter:  %+v", policyBefore, policyAfter)
	}
	if policyAfter.IsOverride {
		t.Fatal("a write that failed to persist still left the set overriding, so a restart would silently undo it")
	}

	// And the file is still loadable and still says the same thing, which
	// is what a restart after this failure would read.
	reloaded, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("the configuration file no longer loads after a failed write: %v", err)
	}
	if reloaded.Sources[0].BackupSets[0].RetentionConfig != nil {
		t.Fatal("the configuration file carries an override the failed write was supposed not to persist")
	}
}
