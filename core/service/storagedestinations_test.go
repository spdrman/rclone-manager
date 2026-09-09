package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// H2.2 (#622): the local hard drive as a first-class storage destination,
// the default destination, and the three invariants that hold the list
// together.
//
// Every test here is about the same underlying question, asked from a
// different side: "where does this tier's backups go" must have exactly
// one answer, whichever surface asks and whichever surface writes. Before
// this issue the answer was spelled two ways, absence in the
// configuration file and a reserved id everywhere else, and nothing
// forced the two to agree. The cases below are what forces it.
//
// The round-trip cases are the load-bearing ones. A settings save
// REPLACES the operator's whole retention chain (RetentionUpdate.Tiers),
// so a read that reports one spelling and a write that accepts another is
// a save that moves somebody's backups by the act of changing something
// else. TestLocalMedium_ChainSurvivesAReadModifyWrite is the one that
// would catch that, and it is deliberately written the way a form
// actually behaves: read the whole chain, change one unrelated number,
// send the whole chain back.

// localFixture opens a service whose configuration declares one S3
// destination beside the implicit local one, so the list has two entries
// and the invariants have something to be about.
//
// The credential is a file this test's own temp directory owns and
// authorises access to nothing, which is core/service's standing rule for
// medium fixtures: nothing here is a real key and nothing here reaches an
// endpoint.
func localFixture(t *testing.T) (*BackupService, string) {
	t.Helper()
	svc, configPath := openTestService(t)

	credentials := filepath.Join(filepath.Dir(configPath), "fixture-credentials")
	body := "[default]\naws_access_key_id = " + testCanaryAccessKeyID + "\naws_secret_access_key = " + testCanarySecret + "\n"
	if err := os.WriteFile(credentials, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := svc.CreateStorageMedium(context.Background(), StorageMediumSpec{
		ID:          "offsite_s3",
		Type:        "s3",
		Region:      "us-east-1",
		Bucket:      "example-bucket",
		Credentials: StorageMediumCredentials{File: credentials},
		// Issue #636: the create proves its destination now, and this
		// fixture's whole point is that nothing here reaches an
		// endpoint. The skip is what keeps that true; #636's own cases
		// turn it back off.
		SkipConnectionCheck: true,
	}); err != nil {
		t.Fatalf("CreateStorageMedium: %v", err)
	}
	return svc, configPath
}

// TestListStorageMediums_AlwaysCarriesTheLocalHardDrive is the first half
// of #622's own complaint: the destinations list omitted the one
// destination every deployment has and every unset tier means.
func TestListStorageMediums_AlwaysCarriesTheLocalHardDrive(t *testing.T) {
	svc, _ := openTestService(t)

	mediums, err := svc.ListStorageMediums(context.Background())
	if err != nil {
		t.Fatalf("ListStorageMediums: %v", err)
	}
	if len(mediums) == 0 {
		t.Fatal("a deployment that declares no S3 destination lists none at all, so the list omits the drive its backups actually land on")
	}
	local := mediums[0]
	if local.ID != StorageMediumLocalID {
		t.Fatalf("the first destination is %q, want the local one (%q) first", local.ID, StorageMediumLocalID)
	}
	if local.Path == "" {
		t.Error("the local destination names no path, so the list cannot say which drive it writes to")
	}
	if !local.IsDefault {
		t.Error("the local destination is not the default in a deployment that has declared nothing else, so a newly created tier would start nowhere")
	}
}

// TestListStorageMediums_IsNeverEmpty is the second invariant, stated and
// tested rather than left as a side effect of local always being present.
func TestListStorageMediums_IsNeverEmpty(t *testing.T) {
	svc, _ := localFixture(t)

	if err := svc.RemoveStorageMedium(context.Background(), "offsite_s3"); err != nil {
		t.Fatalf("RemoveStorageMedium: %v", err)
	}
	mediums, err := svc.ListStorageMediums(context.Background())
	if err != nil {
		t.Fatalf("ListStorageMediums: %v", err)
	}
	if len(mediums) != 1 {
		t.Fatalf("after removing the only declared destination the list holds %d, want exactly 1 (the local hard drive)", len(mediums))
	}
	if !mediums[0].IsDefault {
		t.Error("the one remaining destination is not the default, so this deployment has no destination a new tier could start on")
	}
}

// TestGetStorageMedium_AnswersForTheLocalHardDrive: every surface that
// resolves a tier's destination by id has to be able to resolve the local
// one, or the picker's own default is a dangling reference.
func TestGetStorageMedium_AnswersForTheLocalHardDrive(t *testing.T) {
	svc, _ := openTestService(t)

	local, err := svc.GetStorageMedium(context.Background(), StorageMediumLocalID)
	if err != nil {
		t.Fatalf("GetStorageMedium(%q): %v", StorageMediumLocalID, err)
	}
	if local.ID != StorageMediumLocalID {
		t.Errorf("GetStorageMedium(%q).ID = %q", StorageMediumLocalID, local.ID)
	}
	if local.Path == "" {
		t.Error("the local destination names no path")
	}
}

// TestSetDefaultStorageMedium_MovesItAndMovesNothingElse pins what
// "default" governs. It decides where a NEWLY created tier starts, and it
// is emphatically not a relocation: an existing tier keeps naming
// whatever it named.
func TestSetDefaultStorageMedium_MovesItAndMovesNothingElse(t *testing.T) {
	svc, configPath := localFixture(t)

	before, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}

	if _, err := svc.SetDefaultStorageMedium(context.Background(), "offsite_s3"); err != nil {
		t.Fatalf("SetDefaultStorageMedium: %v", err)
	}

	mediums, err := svc.ListStorageMediums(context.Background())
	if err != nil {
		t.Fatalf("ListStorageMediums: %v", err)
	}
	defaults := make([]string, 0, 1)
	for _, m := range mediums {
		if m.IsDefault {
			defaults = append(defaults, m.ID)
		}
	}
	if len(defaults) != 1 || defaults[0] != "offsite_s3" {
		t.Fatalf("the destinations marked default are %v, want exactly [offsite_s3]", defaults)
	}

	after, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	for i := range before.Retention.Tiers {
		if got, want := after.Retention.Tiers[i].Medium, before.Retention.Tiers[i].Medium; got != want {
			t.Errorf("tier %q moved from %q to %q when the default moved; moving the default must not move a backup that is already somewhere",
				before.Retention.Tiers[i].Name, want, got)
		}
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "default_storage_medium: offsite_s3") {
		t.Errorf("the configuration file does not record the default:\n%s", raw)
	}
}

// TestSetDefaultStorageMedium_BackToLocalLeavesNoKeyBehind is FR-35's
// round-trip rule applied to the new key: local is spelled by absence in
// the file, exactly as a tier's local destination is, so a deployment
// that moves the default out and back has the file it started with.
func TestSetDefaultStorageMedium_BackToLocalLeavesNoKeyBehind(t *testing.T) {
	svc, configPath := localFixture(t)

	if _, err := svc.SetDefaultStorageMedium(context.Background(), "offsite_s3"); err != nil {
		t.Fatalf("SetDefaultStorageMedium(offsite_s3): %v", err)
	}
	if _, err := svc.SetDefaultStorageMedium(context.Background(), StorageMediumLocalID); err != nil {
		t.Fatalf("SetDefaultStorageMedium(local): %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "default_storage_medium") {
		t.Errorf("moving the default back to the local hard drive left the key in the file, so local now has two spellings:\n%s", raw)
	}

	mediums, err := svc.ListStorageMediums(context.Background())
	if err != nil {
		t.Fatalf("ListStorageMediums: %v", err)
	}
	if !mediums[0].IsDefault || mediums[0].ID != StorageMediumLocalID {
		t.Errorf("after moving the default back, the default is %+v", mediums[0])
	}
}

// TestSetDefaultStorageMedium_RefusesADestinationNothingDeclares keeps the
// default from becoming a dangling reference, which is the one way this
// new key could point a new tier at nowhere.
func TestSetDefaultStorageMedium_RefusesADestinationNothingDeclares(t *testing.T) {
	svc, _ := localFixture(t)

	_, err := svc.SetDefaultStorageMedium(context.Background(), "typo_s3")
	if err == nil {
		t.Fatal("SetDefaultStorageMedium accepted a destination nothing declares")
	}
	if !errors.Is(err, ErrMediumNotFound) {
		t.Errorf("SetDefaultStorageMedium error = %v, want ErrMediumNotFound so the surface above says \"that is not one of yours\" rather than 500", err)
	}
}

// TestRemoveStorageMedium_RefusesTheDefault is the first invariant. It is
// an ADDITIONAL refusal beside MEDIUM_IN_USE rather than a replacement,
// which the next test pins from the other side.
func TestRemoveStorageMedium_RefusesTheDefault(t *testing.T) {
	svc, configPath := localFixture(t)

	if _, err := svc.SetDefaultStorageMedium(context.Background(), "offsite_s3"); err != nil {
		t.Fatalf("SetDefaultStorageMedium: %v", err)
	}

	err := svc.RemoveStorageMedium(context.Background(), "offsite_s3")
	if err == nil {
		t.Fatal("RemoveStorageMedium removed the default destination")
	}
	if !AsStorageMediumIsDefault(err) {
		t.Errorf("RemoveStorageMedium error = %v, want ErrStorageMediumIsDefault", err)
	}
	if !strings.Contains(err.Error(), "default") {
		t.Errorf("the refusal does not say why it refused: %v", err)
	}

	raw, err2 := os.ReadFile(configPath)
	if err2 != nil {
		t.Fatalf("ReadFile: %v", err2)
	}
	if !strings.Contains(string(raw), "offsite_s3") {
		t.Errorf("a refused removal still took the destination out of the file:\n%s", raw)
	}
}

// TestRemoveStorageMedium_RefusesTheLocalHardDrive: local is not declared
// and cannot be un-declared. Without this, the second invariant is one
// DELETE away from being false.
func TestRemoveStorageMedium_RefusesTheLocalHardDrive(t *testing.T) {
	svc, _ := localFixture(t)

	err := svc.RemoveStorageMedium(context.Background(), StorageMediumLocalID)
	if err == nil {
		t.Fatal("RemoveStorageMedium removed the local hard drive")
	}
	if !AsStorageMediumIsDefault(err) && !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("RemoveStorageMedium(local) error = %v, want a named refusal", err)
	}
}

// TestRemoveStorageMedium_LeavingOneMakesItTheDefault is the third
// invariant, applied unconditionally on the way out of a removal rather
// than inferred from the fact that local is always there.
func TestRemoveStorageMedium_LeavingOneMakesItTheDefault(t *testing.T) {
	svc, _ := localFixture(t)

	if _, err := svc.SetDefaultStorageMedium(context.Background(), "offsite_s3"); err != nil {
		t.Fatalf("SetDefaultStorageMedium: %v", err)
	}
	// Moved off it first, because the default cannot be removed. That is
	// the two invariants composing, and the sequence an operator actually
	// walks.
	if _, err := svc.SetDefaultStorageMedium(context.Background(), StorageMediumLocalID); err != nil {
		t.Fatalf("SetDefaultStorageMedium(local): %v", err)
	}
	if err := svc.RemoveStorageMedium(context.Background(), "offsite_s3"); err != nil {
		t.Fatalf("RemoveStorageMedium: %v", err)
	}

	mediums, err := svc.ListStorageMediums(context.Background())
	if err != nil {
		t.Fatalf("ListStorageMediums: %v", err)
	}
	if len(mediums) != 1 {
		t.Fatalf("%d destinations remain, want 1", len(mediums))
	}
	if !mediums[0].IsDefault {
		t.Error("the one remaining destination is not the default")
	}
}

// TestSettings_EveryTierNamesADestination is the read half of the one
// decision this issue turns on. Above core/service a tier always names
// where its backups live, so no caller has to know that absence used to
// mean something.
func TestSettings_EveryTierNamesADestination(t *testing.T) {
	svc, _ := openTestService(t)

	settings, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if len(settings.Retention.Tiers) == 0 {
		t.Fatal("the deployment reports no tiers, so this test compared nothing")
	}
	for _, tier := range settings.Retention.Tiers {
		if tier.Medium != StorageMediumLocalID {
			t.Errorf("tier %q reports medium %q, want %q: a tier that names no destination is a tier every caller has to interpret for itself",
				tier.Name, tier.Medium, StorageMediumLocalID)
		}
	}
}

// TestLocalMedium_ChainSurvivesAReadModifyWrite is the case the whole
// two-spellings problem exists to prevent, driven the way a form actually
// drives it: read the chain, change one unrelated number, send the whole
// chain back.
//
// If the read and the write disagreed about how local is spelled, the
// write would either be refused by config.Validate (which refuses "medium:
// local" outright) or would land a medium: key on every tier of a
// medium-free configuration, which is FR-35's compatibility break.
func TestLocalMedium_ChainSurvivesAReadModifyWrite(t *testing.T) {
	svc, configPath := openTestService(t)

	loaded, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	chain := append([]RetentionTier(nil), loaded.Retention.Tiers...)
	chain[0].Keep = chain[0].Keep + 1

	after, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Tiers: chain},
	})
	if err != nil {
		t.Fatalf("UpdateSettings with the chain exactly as it was read back: %v", err)
	}
	for _, tier := range after.Retention.Tiers {
		if tier.Medium != StorageMediumLocalID {
			t.Errorf("tier %q now reports medium %q; a save that changed one keep moved a tier's destination", tier.Name, tier.Medium)
		}
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "medium:") {
		t.Errorf("saving a chain of local tiers wrote a medium: key into a configuration that declares no medium (FR-35):\n%s", raw)
	}
}

// TestUpdateSettings_LocalToLocalIsNotADisclosure keeps the FR-27 consent
// gate honest under the new vocabulary: naming the local hard drive is
// naming where the backups already are, so it asks nobody to acknowledge
// anything.
func TestUpdateSettings_LocalToLocalIsNotADisclosure(t *testing.T) {
	svc, _ := localFixture(t)

	loaded, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	chain := append([]RetentionTier(nil), loaded.Retention.Tiers...)
	chain[0].Keep = chain[0].Keep + 1
	for i := range chain {
		chain[i].Medium = StorageMediumLocalID
	}

	if _, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Tiers: chain},
	}); err != nil {
		t.Fatalf("a chain naming the local hard drive on every tier was refused: %v", err)
	}
}

// TestUpdateSettings_TierCanBePointedAtADeclaredDestination is the write
// half of the picker: what the form sends when an operator chooses S3.
func TestUpdateSettings_TierCanBePointedAtADeclaredDestination(t *testing.T) {
	svc, configPath := localFixture(t)

	loaded, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	chain := append([]RetentionTier(nil), loaded.Retention.Tiers...)
	chain[len(chain)-1].Medium = "offsite_s3"

	after, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention:                   &RetentionUpdate{Tiers: chain},
		AcknowledgeMediumDisclosure: true,
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	last := after.Retention.Tiers[len(after.Retention.Tiers)-1]
	if last.Medium != "offsite_s3" {
		t.Errorf("tier %q reports medium %q, want offsite_s3", last.Name, last.Medium)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "medium: offsite_s3") {
		t.Errorf("the configuration file does not carry the tier's destination:\n%s", raw)
	}
}

// TestPreflightStorageMedium_WorksOnTheLocalHardDrive is #622's last
// added acceptance: "Test connection" answers for the local entry rather
// than being greyed out because no network is involved.
func TestPreflightStorageMedium_WorksOnTheLocalHardDrive(t *testing.T) {
	svc, configPath := openTestService(t)

	// The fixture's backup root is a path no transfer has run into yet,
	// so it does not exist until something creates it. Creating it is
	// what makes this the PASSING control beside the missing-path case
	// below: without it the two cases would be the same case, and a check
	// that always fails looks exactly like one that works.
	if err := os.MkdirAll(filepath.Join(filepath.Dir(configPath), "local"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	report, err := svc.PreflightStorageMedium(context.Background(), StorageMediumLocalID)
	if err != nil {
		t.Fatalf("PreflightStorageMedium(local): %v", err)
	}
	if report.Medium != StorageMediumLocalID {
		t.Errorf("report.Medium = %q, want %q", report.Medium, StorageMediumLocalID)
	}
	if len(report.Checks) == 0 {
		t.Fatal("the local check reported no steps at all, so it certifies nothing")
	}
	if !report.OK {
		t.Errorf("a writable local backup root did not pass: %+v", report.Checks)
	}
}

// TestPreflightStorageMedium_LocalReportsAMissingPathDistinctly is one of
// the three answers #622 asks the local check for. A missing path is a
// different problem from a permission one and has to read as one.
func TestPreflightStorageMedium_LocalReportsAMissingPathDistinctly(t *testing.T) {
	svc, configPath := openTestService(t)

	// Created and then taken away, rather than merely left absent. A NAS
	// volume that did not mount is what this case is about, and asserting
	// against a directory that was never there would pass for a service
	// that only ever fails.
	root := filepath.Join(filepath.Dir(configPath), "local")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	report, err := svc.PreflightStorageMedium(context.Background(), StorageMediumLocalID)
	if err != nil {
		t.Fatalf("PreflightStorageMedium(local): %v", err)
	}
	if report.OK {
		t.Fatal("the local check passed against a backup root that does not exist")
	}
	failed := failedStepsOf(report)
	if _, missing := failed["reach"]; !missing {
		t.Errorf("a missing backup root did not fail the reach step; failures were %v", failed)
	}
	if got := failed["reach"]; got != "not_found" {
		t.Errorf("the reach failure classified as %q, want not_found so a surface can tell it from a permission problem", got)
	}
}

// failedStepsOf indexes a report's failures by step, so a case says which
// step failed and what it classified as rather than scanning a slice by
// hand three times.
func failedStepsOf(report MediumPreflight) map[string]string {
	out := map[string]string{}
	for _, c := range report.Checks {
		if c.Outcome == "failed" {
			out[c.Step] = c.Category
		}
	}
	return out
}
