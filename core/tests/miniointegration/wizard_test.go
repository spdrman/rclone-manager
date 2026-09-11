package miniointegration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/backupd/core/internal/transport"
	"github.com/spdrman/backupd/core/internal/transport/rclone"
	"github.com/spdrman/backupd/core/service"
	"github.com/spdrman/backupd/core/tests/machines"
)

// G2.2 (#594): declaring a storage destination and proving it BEFORE it is
// written down, against a real S3 API rather than a double.
//
// preflight_test.go beside this one already proves the eight checks
// compose into a real answer through the real adapter. What it cannot
// prove is the thing this issue is actually about, which is an ORDERING
// across two surfaces: that a destination nobody has declared can be
// checked at all, that checking it declares nothing, and that a check
// which fails leaves the operator's configuration file byte for byte as it
// was.
//
// Every case here drives core/service against a real config.yaml on disk,
// because the property under test is what ends up in that file. A test
// that held the configuration in memory could not tell a refusal that
// wrote nothing from one that wrote and rolled back, and those are
// different promises.
//
// The canary discipline is the same one core/internal/mediumcheck and
// apps/common/webhost already apply, aimed one layer further out: the
// fixture's secret is generated fresh per run, this file plants it through
// the import, and then searches the config file and every refusal for it,
// with a positive control proving it was in play.

// wizardService writes a minimal, valid config.yaml with NO storage
// medium in it and opens a real BackupService on it.
//
// No medium on purpose. This is the state an operator is in before they
// have declared anything, which is the only state the wizard's whole
// question ("does this destination work?") can be asked from, and it is
// also FR-35's medium-free deployment, so a test that broke it here would
// be noticed.
func wizardService(t *testing.T) (*service.BackupService, string) {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote")
	local := filepath.Join(dir, "local")
	if err := os.MkdirAll(remote, 0o755); err != nil {
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
		"        remote_path: " + remote + "\n" +
		"        local_path: " + local + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	svc, cleanup, err := service.Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("service.Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	return svc, configPath
}

// specFor describes the fixture's own bucket the way the wizard's step 1
// and step 2 together do: every field an operator types, plus a credential
// REFERENCE and no material.
func specFor(medium transport.Medium, credentialsID string) service.StorageMediumSpec {
	return service.StorageMediumSpec{
		ID:           "offsite_s3",
		Type:         "s3",
		Region:       medium.Region,
		Endpoint:     medium.Endpoint,
		Bucket:       medium.Bucket,
		Prefix:       "monthly",
		StorageClass: "STANDARD",
		Credentials:  service.StorageMediumCredentials{ID: credentialsID},
	}
}

func checkOf(t *testing.T, report service.MediumPreflight, step string) service.MediumPreflightCheck {
	t.Helper()
	for _, c := range report.Checks {
		if c.Step == step {
			return c
		}
	}
	t.Fatalf("the report has no %q check: %+v", step, report.Checks)
	return service.MediumPreflightCheck{}
}

// TestMinioWizard_ProvesACandidateBeforeItIsDeclaredAndThenDeclaresIt is
// the whole flow the wizard drives, against a real endpoint.
//
// The load-bearing line is the one that asserts the configuration still
// declares nothing AFTER a successful check: verifying is not saving, and
// before this issue there was no way to reach that state at all, because
// the only preflight there was read its medium out of config.yaml.
func TestMinioWizard_ProvesACandidateBeforeItIsDeclaredAndThenDeclaresIt(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	medium := fixture.NewBucket(t)
	svc, configPath := wizardService(t)
	ctx := context.Background()

	ref, err := svc.ImportStorageCredentials(ctx, fixture.AccessKeyID, fixture.SecretAccessKey, "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}
	if ref.ID == "" {
		t.Fatal("the import returned no id, so nothing can reference the credential")
	}

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	report, err := svc.PreflightStorageMediumCandidate(ctx, specFor(medium, ref.ID))
	if err != nil {
		t.Fatalf("PreflightStorageMediumCandidate: %v", err)
	}
	if !report.OK {
		t.Fatalf("a real, working bucket did not pass as a candidate: %+v", report.Checks)
	}
	if len(report.Checks) != 8 {
		t.Fatalf("the candidate report carries %d checks, want all 8", len(report.Checks))
	}

	// Nothing was declared by checking. This is the ordering the whole
	// issue is about.
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("a candidate preflight changed config.yaml:\n%s", after)
	}
	settings, err := svc.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if declared := declaredMediums(settings.Mediums); len(declared) != 0 {
		t.Fatalf("a candidate preflight declared %+v", declared)
	}

	// The probe object is gone, asked of the endpoint rather than of the
	// report. A probe left in somebody's bucket is litter nothing in this
	// product ever collects.
	objects, err := rclone.New().ListObjects(ctx, medium, "")
	if err != nil {
		t.Fatalf("listing the bucket after the candidate preflight: %v", err)
	}
	if len(objects) != 0 {
		t.Fatalf("the candidate preflight left %d object(s) behind: %+v", len(objects), objects)
	}

	// Now the save, which is the first thing in this flow that writes.
	saved, err := svc.CreateStorageMedium(ctx, specFor(medium, ref.ID))
	if err != nil {
		t.Fatalf("CreateStorageMedium: %v", err)
	}
	if saved.ID != "offsite_s3" || saved.Bucket != medium.Bucket {
		t.Fatalf("the saved destination is %+v", saved)
	}

	// And the saved destination is the same destination that was proven,
	// which is why one shape describes both: the by-id preflight now
	// passes against exactly what the candidate check passed against.
	byID, err := svc.PreflightStorageMedium(ctx, "offsite_s3")
	if err != nil {
		t.Fatalf("PreflightStorageMedium: %v", err)
	}
	if !byID.OK {
		t.Fatalf("the destination that passed as a candidate does not pass as a declaration: %+v", byID.Checks)
	}

	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(written)
	if !strings.Contains(text, "id: offsite_s3") {
		t.Fatalf("config.yaml does not declare the destination:\n%s", text)
	}
	// The positive control: the fixture's secret really is a live
	// credential this flow authenticated with, so its absence below is a
	// statement about the write path rather than about a value nothing
	// used.
	if fixture.SecretAccessKey == "" {
		t.Fatal("the fixture has no secret, so the canary search proves nothing")
	}
	for _, forbidden := range []string{
		fixture.SecretAccessKey, fixture.AccessKeyID, "access_key_id", "secret_access_key",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("config.yaml carries %q, which FR-33 says it never may:\n%s", forbidden, text)
		}
	}
}

// TestMinioWizard_AWrongSecretIsARefusalThatWritesNothing is the refusal
// path, driven against a real endpoint so the classification is the
// endpoint's own rather than a double's.
//
// Three things have to hold together, and only the third needs a real
// config file: the credential was OBTAINED (it is a readable file, so that
// half passes), the endpoint REJECTED it (authentication, not
// configuration, because those send an operator to two different
// machines), and the operator's configuration is byte for byte what it
// was.
func TestMinioWizard_AWrongSecretIsARefusalThatWritesNothing(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	medium := fixture.NewBucket(t)
	svc, configPath := wizardService(t)
	ctx := context.Background()

	// A syntactically valid credential that this server will not accept.
	// Not the fixture's own, and obviously not a real key anywhere.
	ref, err := svc.ImportStorageCredentials(ctx, "EXAMPLEKEYIDNOTREAL0", "EXAMPLE-SECRET-NOT-A-REAL-KEY-000000", "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	report, err := svc.PreflightStorageMediumCandidate(ctx, specFor(medium, ref.ID))
	if err != nil {
		t.Fatalf("PreflightStorageMediumCandidate: %v", err)
	}
	if report.OK {
		t.Fatal("a candidate with a credential this endpoint rejects passed")
	}

	if c := checkOf(t, report, "credentials"); c.Outcome != "passed" {
		t.Errorf("credentials = %q, want passed: the file was there and readable, so reporting a credential problem would send somebody to the wrong machine (%s)", c.Outcome, c.Detail)
	}
	reach := checkOf(t, report, "reach")
	if reach.Outcome != "failed" {
		t.Fatalf("reach = %q, want failed: %s", reach.Outcome, reach.Detail)
	}
	if reach.Category != "authentication" {
		t.Errorf("reach category = %q, want authentication: a rejected key is a policy at the provider, not a line of configuration to fix here", reach.Category)
	}
	skipped := 0
	for _, c := range report.Checks {
		if c.Outcome == "skipped" {
			skipped++
		}
	}
	if skipped != 6 {
		t.Errorf("%d checks were skipped, want 6: nothing may be attempted against an endpoint that would not take the credential", skipped)
	}

	// Nothing about the failure reached the report's own sentences.
	for _, c := range report.Checks {
		if strings.Contains(c.Detail, "EXAMPLE-SECRET") || strings.Contains(c.Detail, "EXAMPLEKEYIDNOTREAL0") {
			t.Errorf("check %q carries credential material in its detail: %s", c.Step, c.Detail)
		}
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("a failing candidate preflight changed config.yaml:\n%s", after)
	}
	settings, err := svc.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if declared := declaredMediums(settings.Mediums); len(declared) != 0 {
		t.Fatalf("a failing candidate preflight declared %+v", declared)
	}
}

// declaredMediums drops the local hard drive from a destinations list,
// leaving what the operator actually declared in config.yaml.
//
// The list is total since H2.2 (#622): it carries the drive backups land
// on as well as the buckets, and that entry is synthesised from the
// configuration rather than declared in it. These cases are about a
// candidate preflight writing NOTHING, so what they have to count is the
// declarations, and a count over the whole list would go red for the
// addition rather than for the behaviour it pins.
func declaredMediums(mediums []service.StorageMediumSummary) []service.StorageMediumSummary {
	out := make([]service.StorageMediumSummary, 0, len(mediums))
	for _, m := range mediums {
		if m.IsLocal {
			continue
		}
		out = append(out, m)
	}
	return out
}

// TestMinioWizard_ABucketThatIsNotThereNamesTheBucket is the other refusal
// an operator meets, and the reason the two are separate cases: a missing
// bucket is one line of their own configuration to fix, and a rejected key
// is a policy at their provider.
func TestMinioWizard_ABucketThatIsNotThereNamesTheBucket(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	svc, configPath := wizardService(t)
	ctx := context.Background()

	ref, err := svc.ImportStorageCredentials(ctx, fixture.AccessKeyID, fixture.SecretAccessKey, "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	spec := specFor(fixture.MediumForBucket("no-such-bucket-zzzzzzzz"), ref.ID)
	report, err := svc.PreflightStorageMediumCandidate(ctx, spec)
	if err != nil {
		t.Fatalf("PreflightStorageMediumCandidate: %v", err)
	}
	if report.OK {
		t.Fatal("a candidate naming a bucket that is not there passed")
	}
	reach := checkOf(t, report, "reach")
	if reach.Category != "configuration" {
		t.Errorf("reach category = %q, want configuration", reach.Category)
	}
	if !strings.Contains(reach.Detail, "no-such-bucket-zzzzzzzz") {
		t.Errorf("the refusal does not name the bucket: %q", reach.Detail)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("a failing candidate preflight changed config.yaml:\n%s", after)
	}
}

// TestMinioDestinationMark_IsWrittenBySkippingAndClearedByAPassingCheck
// is issue #636's whole shape against a real endpoint, and it is here
// because the half that matters most cannot be proven anywhere else: a
// check that PASSES needs a bucket that answers.
//
// Four things have to hold together and the case asks all four.
//
// A create that skips the check writes the destination and MARKS it, so
// an operator who declared one offline can tell it apart tomorrow from
// one checked against a real bucket. A test connection that passes takes
// the mark off, in the file as well as on the read surface, which is what
// makes it a state rather than a permanent scar. A second unproven
// destination beside it KEEPS its own mark, which is the control that
// stops this passing on a build that clears every mark it can find. And a
// create that does not skip goes through against the same bucket, which
// is the proof that #636's check is a gate on unprovable destinations
// rather than on all of them.
func TestMinioDestinationMark_IsWrittenBySkippingAndClearedByAPassingCheck(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	medium := fixture.NewBucket(t)
	svc, configPath := wizardService(t)
	ctx := context.Background()

	ref, err := svc.ImportStorageCredentials(ctx, fixture.AccessKeyID, fixture.SecretAccessKey, "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}

	// Declared offline, against a bucket that would in fact have answered.
	// The skip is what does the work here, and the case below is what
	// shows the check would otherwise have run and passed.
	skipped := specFor(medium, ref.ID)
	skipped.SkipConnectionCheck = true
	unproven, err := svc.CreateStorageMedium(ctx, skipped)
	if err != nil {
		t.Fatalf("CreateStorageMedium with the check skipped: %v", err)
	}
	if !unproven.ConnectionUnverified {
		t.Fatal("a destination declared with the check skipped reads back as one that was checked")
	}

	// A second one, also unproven, pointed at a bucket that is not there.
	// It is the control: nothing this case does to the first destination
	// may touch this one's mark.
	otherSpec := specFor(medium, ref.ID)
	otherSpec.ID = "cold_vault"
	otherSpec.Bucket = medium.Bucket + "-does-not-exist"
	otherSpec.SkipConnectionCheck = true
	if _, err := svc.CreateStorageMedium(ctx, otherSpec); err != nil {
		t.Fatalf("CreateStorageMedium for the control destination: %v", err)
	}

	if raw := mustReadFile(t, configPath); strings.Count(raw, "connection_unverified: true") != 2 {
		t.Fatalf("two destinations were declared unproven and the file does not say so twice:\n%s", raw)
	}

	// The check that earns the removal.
	report, err := svc.PreflightStorageMedium(ctx, "offsite_s3")
	if err != nil {
		t.Fatalf("PreflightStorageMedium: %v", err)
	}
	if !report.OK {
		t.Fatalf("a real, working bucket did not pass its own check: %+v", report.Checks)
	}

	proven, err := svc.GetStorageMedium(ctx, "offsite_s3")
	if err != nil {
		t.Fatalf("GetStorageMedium: %v", err)
	}
	if proven.ConnectionUnverified {
		t.Error("a check that passed left the destination marked as never proven")
	}
	control, err := svc.GetStorageMedium(ctx, "cold_vault")
	if err != nil {
		t.Fatalf("GetStorageMedium(cold_vault): %v", err)
	}
	if !control.ConnectionUnverified {
		t.Error("proving one destination cleared another one's mark, so the mark says 'somebody ran a check somewhere'")
	}
	if raw := mustReadFile(t, configPath); strings.Count(raw, "connection_unverified: true") != 1 {
		t.Errorf("the file does not carry exactly the one mark that is still earned:\n%s", raw)
	}

	// And the ordinary path: a create that runs its own check against the
	// same bucket is written, and carries no mark at all.
	checked := specFor(medium, ref.ID)
	checked.ID = "second_offsite"
	written, err := svc.CreateStorageMedium(ctx, checked)
	if err != nil {
		t.Fatalf("CreateStorageMedium against a bucket that answers: %v", err)
	}
	if written.ConnectionUnverified {
		t.Error("a destination whose check passed on the way in is marked as never proven")
	}
}

// TestMinioDestination_ACreateThatCannotBeProvenIsRefused is #636's
// refusal against a real endpoint, so the classification is the
// endpoint's own rather than a double's.
//
// The bucket is one MinIO really does not have, which is a reach failure
// the endpoint decides rather than a transport failure this host decides.
// A create refused on it must leave the configuration byte for byte as it
// was: a destination half-declared and then reported as failed is the
// shape a retry silently folds into.
func TestMinioDestination_ACreateThatCannotBeProvenIsRefused(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	medium := fixture.NewBucket(t)
	svc, configPath := wizardService(t)
	ctx := context.Background()

	ref, err := svc.ImportStorageCredentials(ctx, fixture.AccessKeyID, fixture.SecretAccessKey, "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}
	before := mustReadFile(t, configPath)

	spec := specFor(medium, ref.ID)
	spec.Bucket = medium.Bucket + "-does-not-exist"
	_, err = svc.CreateStorageMedium(ctx, spec)
	if !errors.Is(err, service.ErrStorageMediumNotProven) {
		t.Fatalf("CreateStorageMedium against a bucket that is not there returned %v, want ErrStorageMediumNotProven", err)
	}
	if !strings.Contains(err.Error(), "reach") {
		t.Errorf("the refusal does not name the step that failed: %v", err)
	}
	// The positive control on the canary search below: the fixture's
	// secret really is the live credential this create authenticated with,
	// so its absence is a statement about the refusal rather than about a
	// value nothing used.
	if fixture.SecretAccessKey == "" {
		t.Fatal("the fixture has no secret, so the canary search proves nothing")
	}
	for _, forbidden := range []string{fixture.SecretAccessKey, fixture.AccessKeyID} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("the refusal carries credential material: %v", err)
		}
	}

	if after := mustReadFile(t, configPath); after != before {
		t.Errorf("a refused create wrote the configuration:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	settings, err := svc.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if declared := declaredMediums(settings.Mediums); len(declared) != 0 {
		t.Fatalf("a refused create declared %+v", declared)
	}
}

// mustReadFile is this file's own read helper, so the cases above read as
// what they assert rather than as error handling.
func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return string(raw)
}
