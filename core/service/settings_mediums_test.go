package service

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Tests for issue #234 (EPIC E, E1.2) at the settings boundary.
//
// This package writes the operator's config file. Two things therefore
// have to hold here rather than only in internal/config: a settings save
// on a config that names no medium must not inject either new key into
// the file (FR-35's round-trip rule, at the layer that actually does the
// writing), and a settings save on a config that DOES name one must not
// silently drop it, because the write path replaces the whole retention
// chain with whatever the caller submitted.

// mediumKeyLine matches either key this change introduces, at any
// indentation, in a written config file.
var mediumKeyLine = regexp.MustCompile(`(?m)^\s*(storage_mediums|medium):`)

// legacyTiersRetention is a retention block in the explicit tiers
// spelling that names no medium at all: the exact shape FR-35 says must
// come back from a settings save byte for byte as it went in.
const legacyTiersRetention = "retention:\n" +
	"  timezone: UTC\n" +
	"  week_starts_on: monday\n" +
	"  tiers:\n" +
	"    - name: daily\n" +
	"      granularity: day\n" +
	"      keep: 7\n" +
	"    - name: monthly\n" +
	"      granularity: month\n" +
	"      keep: 12\n"

// mediumsRetention is the same chain with a declared medium and one tier
// pointed at it.
const mediumsRetention = "retention:\n" +
	"  timezone: UTC\n" +
	"  week_starts_on: monday\n" +
	"  tiers:\n" +
	"    - name: daily\n" +
	"      granularity: day\n" +
	"      keep: 7\n" +
	"    - name: monthly\n" +
	"      granularity: month\n" +
	"      keep: 12\n" +
	"      medium: offsite_s3\n" +
	"storage_mediums:\n" +
	"  - id: offsite_s3\n" +
	"    type: s3\n" +
	"    region: us-east-1\n" +
	"    bucket: nas-backups\n" +
	"    credentials:\n" +
	"      env: BACKUP_S3_OFFSITE\n"

// TestUpdateSettings_ANoMediumConfigGainsNoMediumKeys is FR-35's
// round-trip rule at the layer that writes the file. A settings save
// re-marshals the whole config, so an omitempty that is missing anywhere
// in this change turns every operator's next save into a file an older
// binary refuses under Load's KnownFields(true).
func TestUpdateSettings_ANoMediumConfigGainsNoMediumKeys(t *testing.T) {
	configPath := writeTestConfigFileWithRetention(t, legacyTiersRetention)
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	if _, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Timezone: ptrString("America/Vancouver")},
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	written := string(raw)

	// The write did happen. Without this the absence below is satisfied
	// by a save that never touched the file.
	if !strings.Contains(written, "America/Vancouver") {
		t.Fatalf("precondition failed: the settings save did not reach the file:\n%s", written)
	}
	if found := mediumKeyLine.FindAllString(written, -1); len(found) != 0 {
		t.Errorf("a settings save injected %v into a config that configured no medium:\n%s", found, written)
	}
}

// TestUpdateSettings_KeepsATiersMediumItWasNotAskedToChange is the other
// direction, and it is the reason this package had to change at all.
//
// RetentionUpdate.Tiers REPLACES the whole chain, so every field of a
// tier that the boundary type cannot carry is a field a settings save
// silently deletes from the operator's file. A form editing daily's keep
// would have moved monthly's artifacts back onto local disk without
// saying so, which is a configuration change nobody asked for, made by
// the act of changing something else.
func TestUpdateSettings_KeepsATiersMediumItWasNotAskedToChange(t *testing.T) {
	configPath := writeTestConfigFileWithRetention(t, mediumsRetention)
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	// The read side has to carry it first, or a caller doing the ordinary
	// read-modify-write cannot send it back even in principle.
	before, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if len(before.Retention.Tiers) != 2 {
		t.Fatalf("Settings reported %d tier(s), want 2", len(before.Retention.Tiers))
	}
	if before.Retention.Tiers[0].Medium != "" {
		t.Errorf("the local tier reports medium %q, want it empty", before.Retention.Tiers[0].Medium)
	}
	if before.Retention.Tiers[1].Medium != "offsite_s3" {
		t.Fatalf("Settings dropped the configured medium: tiers[1].Medium = %q, want offsite_s3", before.Retention.Tiers[1].Medium)
	}

	// The ordinary form submission: send the chain back with one number
	// changed, exactly as a settings page that rendered it would.
	next := append([]RetentionTier(nil), before.Retention.Tiers...)
	next[0].Keep = 9
	after, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Tiers: next},
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if after.Retention.Tiers[1].Medium != "offsite_s3" {
		t.Errorf("the settings write dropped the medium: tiers[1].Medium = %q, want offsite_s3", after.Retention.Tiers[1].Medium)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	written := string(raw)
	if !strings.Contains(written, "keep: 9") {
		t.Fatalf("precondition failed: the settings save did not reach the file:\n%s", written)
	}
	if !strings.Contains(written, "medium: offsite_s3") {
		t.Errorf("the written config lost its tier-to-medium mapping:\n%s", written)
	}
	if !strings.Contains(written, "id: offsite_s3") {
		t.Errorf("the written config lost its storage_mediums declaration:\n%s", written)
	}
}

// TestUpdateSettings_RefusesATierNamingAnUndeclaredMedium proves the
// refusal an operator gets through this write path is internal/config's
// own, not a second rule this package grew. The acceptance criterion for
// #234 is that no package outside core/internal/config gained a
// validation rule, and the way that stays true is that UpdateSettings
// validates the whole config and reports what Validate said.
func TestUpdateSettings_RefusesATierNamingAnUndeclaredMedium(t *testing.T) {
	configPath := writeTestConfigFileWithRetention(t, mediumsRetention)
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	_, err = svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Tiers: []RetentionTier{
			{Name: "daily", Granularity: "day", Keep: 7},
			{Name: "monthly", Granularity: "month", Keep: 12, Medium: "never_declared"},
		}},
	})
	if err == nil {
		t.Fatal("UpdateSettings accepted a tier naming a medium no storage_mediums entry declares")
	}
	if !strings.Contains(err.Error(), "never_declared") {
		t.Errorf("error %q does not name the medium that does not resolve", err)
	}

	// The file is untouched: a refused write leaves the operator's own
	// configuration exactly as it was.
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "medium: offsite_s3") {
		t.Errorf("a refused settings write damaged the config file:\n%s", raw)
	}
}
