package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/lifecycle"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/state"
)

// Tests for G2.2 (#594): declaring a storage medium without hand-editing
// config.yaml, and proving one works BEFORE it is written down.
//
// The canary discipline every test here shares is worth stating once. A
// storage medium's secret is the one thing on this boundary that must
// never come back out, so the fixtures below use one obviously fake
// secret string and the tests search the whole world this package can
// reach for it: the returned reference, the config file, and the error
// text of every refusal. testCanarySecret is never a real credential and
// is written only into a file this test's own temp directory owns.
const (
	testCanaryAccessKeyID = "EXAMPLEKEYIDNOTREAL0"
	testCanarySecret      = "EXAMPLE-SECRET-NOT-A-REAL-KEY-0000000000"
)

// TestImportStorageCredentials_ReturnsAReferenceAndNeverTheMaterial is the
// S3 half of ImportSSHKey's own contract: the material is submitted once,
// an opaque id comes back, and nothing about the secret survives on the
// boundary.
func TestImportStorageCredentials_ReturnsAReferenceAndNeverTheMaterial(t *testing.T) {
	svc, configPath := openTestService(t)

	ref, err := svc.ImportStorageCredentials(context.Background(), testCanaryAccessKeyID, testCanarySecret, "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}
	if ref.ID == "" {
		t.Fatal("ImportStorageCredentials returned an empty id, so nothing can reference the credential it wrote")
	}
	if strings.Contains(ref.ID, testCanarySecret) || strings.Contains(ref.ID, testCanaryAccessKeyID) {
		t.Fatalf("the returned id carries credential material: %q", ref.ID)
	}

	// The file it wrote is beside config.yaml, 0600, and emphatically not
	// under a backup root (MediumCredentials.File's own doc, #298).
	path := filepath.Join(filepath.Dir(configPath), "s3_credentials", ref.ID)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("credentials file mode = %04o, want 0600", mode)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "[default]") {
		t.Errorf("the written file has no [default] profile, so the AWS credential chain would fall through it:\n%s", raw)
	}
	if !strings.Contains(string(raw), testCanarySecret) {
		t.Error("the written file does not carry the secret it was given, so nothing could authenticate with it")
	}
}

// TestImportStorageCredentials_RefusesMalformedMaterialWithoutEchoingIt
// pins the refusal shape keysource.go and mediumcreds.go both hold: the
// SHAPE of the problem is reported, never the bytes that failed.
func TestImportStorageCredentials_RefusesMalformedMaterialWithoutEchoingIt(t *testing.T) {
	svc, configPath := openTestService(t)

	_, err := svc.ImportStorageCredentials(context.Background(), "", testCanarySecret, "")
	if err == nil {
		t.Fatal("ImportStorageCredentials accepted an empty access key id")
	}
	if strings.Contains(err.Error(), testCanarySecret) {
		t.Fatalf("the refusal echoes the secret it was given: %v", err)
	}

	// Nothing was written for a refused import.
	dir := filepath.Join(filepath.Dir(configPath), "s3_credentials")
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) != 0 {
		t.Errorf("a refused import left %d file(s) in %s", len(entries), dir)
	}
}

// TestCreateStorageMedium_WritesTheDeclarationAndNoSecret is gap 1: a
// medium can be created without hand-editing config.yaml, and what lands
// in the file is a reference.
func TestCreateStorageMedium_WritesTheDeclarationAndNoSecret(t *testing.T) {
	svc, configPath := openTestService(t)

	ref, err := svc.ImportStorageCredentials(context.Background(), testCanaryAccessKeyID, testCanarySecret, "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}

	spec := StorageMediumSpec{
		ID:           "offsite_s3",
		Type:         "s3",
		Region:       "us-east-1",
		Bucket:       "nas-backups",
		Prefix:       "monthly",
		StorageClass: "STANDARD_IA",
		Credentials:  StorageMediumCredentials{ID: ref.ID},
		// Issue #636: every write in this package's suite carries the
		// skip, and that is a statement about scope rather than a
		// workaround. CreateStorageMedium and UpdateStorageMedium prove
		// the destination in front of the write, and this fixture's
		// endpoint is a bucket nothing here has, so without the skip
		// every case that needs a destination to exist would drive
		// #636's refusal, or worse reach a real provider, instead of
		// testing whatever it is about. The check has its own cases, in
		// mediumcreatecheck_test.go, and they turn the skip back off.
		SkipConnectionCheck: true,
	}
	got, err := svc.CreateStorageMedium(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateStorageMedium: %v", err)
	}
	if got.ID != "offsite_s3" {
		t.Errorf("summary id = %q, want offsite_s3", got.ID)
	}

	settings, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	declared := declaredOnly(settings.Mediums)
	if len(declared) != 1 || declared[0].ID != "offsite_s3" {
		t.Fatalf("Settings.Mediums declares %+v, want exactly offsite_s3 (the create must hot-reload)", declared)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "storage_mediums:") || !strings.Contains(text, "id: offsite_s3") {
		t.Fatalf("config.yaml does not declare the medium:\n%s", text)
	}
	for _, forbidden := range []string{testCanarySecret, testCanaryAccessKeyID, "access_key_id", "secret_access_key"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("config.yaml carries %q, which FR-33 says it never may:\n%s", forbidden, text)
		}
	}
}

// TestCreateStorageMedium_CredentialsFileIsAPathUnderTheCredentialsDirectory
// is issue #665's C1, second half: creating a medium from an imported
// credential reference and reading the written config.yaml back AS
// BYTES must show neither canary anywhere in the file, and the
// credentials.file value it does carry must be a path under
// mediumCredentialsDirName ("s3_credentials/") - never the material
// itself, and never a path this deployment did not mint.
// TestCreateStorageMedium_WritesTheDeclarationAndNoSecret already proves
// the first half; this names the second explicitly, the way #665's own
// credential-canary table asks for.
func TestCreateStorageMedium_CredentialsFileIsAPathUnderTheCredentialsDirectory(t *testing.T) {
	svc, configPath := openTestService(t)
	ref, err := svc.ImportStorageCredentials(context.Background(), testCanaryAccessKeyID, testCanarySecret, "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}
	if _, err := svc.CreateStorageMedium(context.Background(), StorageMediumSpec{
		ID: "offsite_s3", Type: "s3", Region: "us-east-1", Bucket: "nas-backups",
		Credentials:         StorageMediumCredentials{ID: ref.ID},
		SkipConnectionCheck: true,
	}); err != nil {
		t.Fatalf("CreateStorageMedium: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(raw)
	for _, canary := range []string{testCanarySecret, testCanaryAccessKeyID} {
		if strings.Contains(text, canary) {
			t.Fatalf("config.yaml carries the credential canary %q:\n%s", canary, text)
		}
	}

	found := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "file:") || !strings.Contains(trimmed, mediumCredentialsDirName) {
			continue
		}
		found = true
		if !strings.HasSuffix(trimmed, ref.ID) {
			t.Errorf("credentials.file line %q does not end with the minted reference id %q", trimmed, ref.ID)
		}
	}
	if !found {
		t.Fatalf("config.yaml has no credentials.file line under %q:\n%s", mediumCredentialsDirName, text)
	}
}

// TestCreateStorageMedium_RefusesADuplicateIdAndWritesNothing keeps the
// "refuse, never partially apply" rule this package's settings write
// already holds.
func TestCreateStorageMedium_RefusesADuplicateIdAndWritesNothing(t *testing.T) {
	svc, configPath := openTestService(t)
	ref, err := svc.ImportStorageCredentials(context.Background(), testCanaryAccessKeyID, testCanarySecret, "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}
	spec := StorageMediumSpec{
		ID: "offsite_s3", Type: "s3", Region: "us-east-1", Bucket: "nas-backups",
		Credentials:         StorageMediumCredentials{ID: ref.ID},
		SkipConnectionCheck: true,
	}
	if _, err := svc.CreateStorageMedium(context.Background(), spec); err != nil {
		t.Fatalf("CreateStorageMedium: %v", err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if _, err := svc.CreateStorageMedium(context.Background(), spec); err == nil {
		t.Fatal("a duplicate medium id was accepted")
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Error("a refused create changed config.yaml")
	}
}

// TestRemoveStorageMedium_RefusesWhileAPlacementNamesIt is FR-30 at this
// boundary: a destination artifacts already live on cannot be removed by
// accident from a settings page.
func TestRemoveStorageMedium_RefusesWhileAPlacementNamesIt(t *testing.T) {
	svc, _ := openTestService(t)
	ref, err := svc.ImportStorageCredentials(context.Background(), testCanaryAccessKeyID, testCanarySecret, "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}
	if _, err := svc.CreateStorageMedium(context.Background(), StorageMediumSpec{
		ID: "offsite_s3", Type: "s3", Region: "us-east-1", Bucket: "nas-backups",
		Credentials:         StorageMediumCredentials{ID: ref.ID},
		SkipConnectionCheck: true,
	}); err != nil {
		t.Fatalf("CreateStorageMedium: %v", err)
	}

	// Nothing references it yet, so it goes.
	if err := svc.RemoveStorageMedium(context.Background(), "offsite_s3"); err != nil {
		t.Fatalf("RemoveStorageMedium on an unreferenced medium: %v", err)
	}
	settings, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	// One left, and it is the local hard drive: the destinations list
	// always carries it (H2.2, #622), so "the S3 medium is gone" is
	// spelled as "nothing declared remains" rather than as an empty list.
	if declared := declaredOnly(settings.Mediums); len(declared) != 0 {
		t.Fatalf("Settings.Mediums still declares %+v after a remove, want none", declared)
	}
}

// TestPreflightStorageMediumCandidate_RefusesAnUnknownCredentialsID is
// the verify-before-save surface's own refusal: a candidate that names a
// credential this deployment never minted is refused rather than probed,
// and the refusal never turns the id into a path an API caller learns.
func TestPreflightStorageMediumCandidate_RefusesAnUnknownCredentialsID(t *testing.T) {
	svc, _ := openTestService(t)

	_, err := svc.PreflightStorageMediumCandidate(context.Background(), StorageMediumSpec{
		ID: "offsite_s3", Type: "s3", Region: "us-east-1", Bucket: "nas-backups",
		Credentials: StorageMediumCredentials{ID: "../../etc/shadow"},
	})
	if err == nil {
		t.Fatal("a credentials id containing a path separator was accepted")
	}
	if strings.Contains(err.Error(), "/etc/shadow") {
		t.Errorf("the refusal echoed the caller's own path back: %v", err)
	}
}

// seedMediumPlacement writes one ACTIVE copy of one artifact onto medium,
// through RecordTransition rather than an insert, so the row is the same
// one the product itself would have produced.
//
// The artifact belongs to production/postgres-primary, which is the set
// writeTestConfigFile declares, so the usage report has a real backup set
// id to name rather than an orphan.
func seedMediumPlacement(t *testing.T, svc *BackupService, medium, name string) {
	t.Helper()
	ctx := context.Background()
	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}
	if _, err := svc.journal.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        "seed-discover-" + name,
		To:         string(lifecycle.Discovered),
		OccurredAt: time.Now().UTC().Add(-48 * time.Hour),
		RemotePath: "/backups/pg/" + name,
	}); err != nil {
		t.Fatalf("seeding the artifact: %v", err)
	}
	size := int64(4096)
	if _, err := svc.journal.RecordTransition(ctx, state.Transition{
		Artifact:   artifact,
		Key:        "seed-placement-" + name,
		From:       string(lifecycle.Discovered),
		To:         string(lifecycle.Transferring),
		OccurredAt: time.Now().UTC().Add(-24 * time.Hour),
		Placement: &state.PlacementUpdate{
			Medium:            medium,
			Location:          "monthly/production/postgres-primary/" + name,
			Size:              &size,
			Hash:              strings.Repeat("a", 64),
			HashAlg:           "sha256",
			VerificationClass: state.VerificationContent,
			Status:            state.PlacementActive,
		},
	}); err != nil {
		t.Fatalf("seeding the placement: %v", err)
	}
}

// declareTestMedium is the create every FR-30 test starts from.
func declareTestMedium(t *testing.T, svc *BackupService, id string) {
	t.Helper()
	ref, err := svc.ImportStorageCredentials(context.Background(), testCanaryAccessKeyID, testCanarySecret, "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}
	if _, err := svc.CreateStorageMedium(context.Background(), StorageMediumSpec{
		ID: id, Type: "s3", Region: "us-east-1", Bucket: "nas-backups", Prefix: "monthly",
		Credentials: StorageMediumCredentials{ID: ref.ID},
		// Issue #636: every write in this package's suite carries the
		// skip, and that is a statement about scope rather than a
		// workaround. CreateStorageMedium and UpdateStorageMedium prove
		// the destination in front of the write, and this fixture's
		// endpoint is a bucket nothing here has, so without the skip
		// every case that needs a destination to exist would drive
		// #636's refusal, or worse reach a real provider, instead of
		// testing whatever it is about. The check has its own cases, in
		// mediumcreatecheck_test.go, and they turn the skip back off.
		SkipConnectionCheck: true,
	}); err != nil {
		t.Fatalf("CreateStorageMedium: %v", err)
	}
}

// TestRemoveStorageMedium_RefusesWhileCopiesNameIt is FR-30's invariant
// at the one surface that could break it with a click: removing a
// destination artifacts already reference would leave this deployment
// with no way to confirm those copies exist.
//
// The refusal has to carry the count AND the sets, because "148 copies
// affected" with nothing named is not something an operator can act on.
func TestRemoveStorageMedium_RefusesWhileCopiesNameIt(t *testing.T) {
	svc, configPath := openTestService(t)
	declareTestMedium(t, svc, "offsite_s3")
	seedMediumPlacement(t, svc, "offsite_s3", "one.dump")
	seedMediumPlacement(t, svc, "offsite_s3", "two.dump")

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	err = svc.RemoveStorageMedium(context.Background(), "offsite_s3")
	if err == nil {
		t.Fatal("a medium holding two copies was removed")
	}
	if !AsStorageMediumInUse(err) {
		t.Fatalf("removal refused with %v, which is not ErrStorageMediumInUse, so a surface cannot tell it from a malformed request", err)
	}
	if !strings.Contains(err.Error(), "production/postgres-primary") {
		t.Errorf("the refusal does not name the affected backup set: %v", err)
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("the refusal does not say the copies would read as unreachable rather than gone: %v", err)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Error("a refused removal changed config.yaml")
	}
	settings, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if declared := declaredOnly(settings.Mediums); len(declared) != 1 {
		t.Errorf("the medium is no longer declared after a refused removal: %+v", declared)
	}
}

// TestStorageMediumUsage_CountsOnlyCopiesSeparately is the number the
// FR-30 banner is actually about: how many artifacts would have no
// confirmed readable copy anywhere if this destination stopped answering.
func TestStorageMediumUsage_CountsOnlyCopiesSeparately(t *testing.T) {
	svc, _ := openTestService(t)
	declareTestMedium(t, svc, "offsite_s3")
	seedMediumPlacement(t, svc, "offsite_s3", "only-copy.dump")

	usage, err := svc.StorageMediumUsage(context.Background(), "offsite_s3")
	if err != nil {
		t.Fatalf("StorageMediumUsageOf: %v", err)
	}
	if usage.Placements != 1 {
		t.Fatalf("Placements = %d, want 1", usage.Placements)
	}
	if len(usage.BackupSets) != 1 || usage.BackupSets[0].Set != "production/postgres-primary" {
		t.Fatalf("BackupSets = %+v, want one entry for production/postgres-primary", usage.BackupSets)
	}
	if usage.BackupSets[0].OnlyCopyHere != 1 {
		t.Errorf("OnlyCopyHere = %d, want 1: this artifact has no other copy anywhere", usage.BackupSets[0].OnlyCopyHere)
	}
}

// TestUpdateStorageMedium_IsAllowedOnAMediumCopiesNameIt is the other
// half of FR-30's answer, and the half that is easy to get backwards.
// Editing an in-use destination has to stay possible: a credential that
// expired is exactly the case an operator needs to fix, and refusing the
// edit would leave rotating one as a config-file job.
func TestUpdateStorageMedium_IsAllowedOnAMediumCopiesNameIt(t *testing.T) {
	svc, _ := openTestService(t)
	declareTestMedium(t, svc, "offsite_s3")
	seedMediumPlacement(t, svc, "offsite_s3", "one.dump")

	ref, err := svc.ImportStorageCredentials(context.Background(), testCanaryAccessKeyID, testCanarySecret, "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}
	got, err := svc.UpdateStorageMedium(context.Background(), StorageMediumSpec{
		ID: "offsite_s3", Type: "s3", Region: "eu-west-1", Bucket: "nas-backups", Prefix: "monthly",
		StorageClass:        "STANDARD_IA",
		Credentials:         StorageMediumCredentials{ID: ref.ID},
		SkipConnectionCheck: true,
	})
	if err != nil {
		t.Fatalf("UpdateStorageMedium on an in-use medium: %v", err)
	}
	if got.Region != "eu-west-1" || got.StorageClass != "STANDARD_IA" {
		t.Errorf("the edit did not take: %+v", got)
	}
}

// TestUpdateStorageMedium_KeepsTheCredentialWhenTheEditNamesNone is the
// exception a whole-record replace has to carry, and the reason it has
// to: StorageMediumSummary deliberately reports nothing about the
// credential, not even its kind, so a form cannot pre-fill it and a
// caller cannot resubmit what it never received. Without this, changing a
// storage class would mean re-importing an access key.
func TestUpdateStorageMedium_KeepsTheCredentialWhenTheEditNamesNone(t *testing.T) {
	svc, configPath := openTestService(t)
	declareTestMedium(t, svc, "offsite_s3")

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	credentialLine := ""
	for _, line := range strings.Split(string(before), "\n") {
		if strings.Contains(line, "file:") && strings.Contains(line, mediumCredentialsDirName) {
			credentialLine = strings.TrimSpace(line)
		}
	}
	if credentialLine == "" {
		t.Fatalf("the declared medium has no credentials.file line to preserve:\n%s", before)
	}

	if _, err := svc.UpdateStorageMedium(context.Background(), StorageMediumSpec{
		ID: "offsite_s3", Type: "s3", Region: "us-east-1", Bucket: "nas-backups", Prefix: "monthly",
		StorageClass:        "STANDARD_IA",
		SkipConnectionCheck: true,
	}); err != nil {
		t.Fatalf("UpdateStorageMedium with no credential named: %v", err)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(after), credentialLine) {
		t.Errorf("the edit dropped the credential reference it was not asked to change; %q is gone from:\n%s", credentialLine, after)
	}
	got, err := svc.GetStorageMedium(context.Background(), "offsite_s3")
	if err != nil {
		t.Fatalf("StorageMedium: %v", err)
	}
	if got.StorageClass != "STANDARD_IA" {
		t.Errorf("StorageClass = %q, want STANDARD_IA", got.StorageClass)
	}
}

// TestCreateStorageMedium_StillNeedsACredential is the other half: a
// CREATE has nothing to inherit, so the same empty credential block that
// an edit reads as "leave it alone" is a refusal here.
func TestCreateStorageMedium_StillNeedsACredential(t *testing.T) {
	svc, _ := openTestService(t)
	if _, err := svc.CreateStorageMedium(context.Background(), StorageMediumSpec{
		ID: "offsite_s3", Type: "s3", Region: "us-east-1", Bucket: "nas-backups",
	}); err == nil {
		t.Fatal("a medium was created with no credential reference at all")
	}
}

// TestStorageMediumSummary_CarriesEveryFieldAnEditMustPreserve is the
// lossy-boundary guard RetentionTier.Medium's own doc argues for.
// UpdateStorageMedium replaces the whole record, so a field the summary
// cannot report is a field the next save silently clears.
func TestStorageMediumSummary_CarriesEveryFieldAnEditMustPreserve(t *testing.T) {
	svc, _ := openTestService(t)
	ref, err := svc.ImportStorageCredentials(context.Background(), testCanaryAccessKeyID, testCanarySecret, "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}
	spec := StorageMediumSpec{
		ID: "offsite_s3", Type: "s3", Region: "us-east-1", Endpoint: "https://minio.internal:9000",
		Bucket: "nas-backups", Prefix: "monthly", StorageClass: "STANDARD_IA",
		UploadVerification:  "readback",
		Credentials:         StorageMediumCredentials{ID: ref.ID},
		SkipConnectionCheck: true,
	}
	got, err := svc.CreateStorageMedium(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateStorageMedium: %v", err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"ID", got.ID, spec.ID},
		{"Type", got.Type, spec.Type},
		{"Region", got.Region, spec.Region},
		{"Endpoint", got.Endpoint, spec.Endpoint},
		{"Bucket", got.Bucket, spec.Bucket},
		{"Prefix", got.Prefix, spec.Prefix},
		{"StorageClass", got.StorageClass, spec.StorageClass},
		{"UploadVerification", got.UploadVerification, spec.UploadVerification},
	} {
		if c.got != c.want {
			t.Errorf("summary %s = %q, want %q; an edit form cannot pre-fill what the read surface does not report, and the next save would clear it", c.name, c.got, c.want)
		}
	}
}

// TestStorageMediumWrites_RefuseMoreThanOneCredentialSource keeps the
// closed-set rule readable at this boundary rather than only in
// config.Validate, which can only ever talk about the three fields the
// schema holds and not about the id the caller actually sent.
func TestStorageMediumWrites_RefuseMoreThanOneCredentialSource(t *testing.T) {
	svc, _ := openTestService(t)
	ref, err := svc.ImportStorageCredentials(context.Background(), testCanaryAccessKeyID, testCanarySecret, "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}
	_, err = svc.CreateStorageMedium(context.Background(), StorageMediumSpec{
		ID: "offsite_s3", Type: "s3", Region: "us-east-1", Bucket: "nas-backups",
		Credentials: StorageMediumCredentials{ID: ref.ID, Env: "BACKUP_S3_OFFSITE"},
	})
	if err == nil {
		t.Fatal("a medium naming two credential sources was accepted")
	}
	if !strings.Contains(err.Error(), "credentials_id") || !strings.Contains(err.Error(), "credentials.env") {
		t.Errorf("the refusal does not name both sources the caller sent: %v", err)
	}

	_, err = svc.CreateStorageMedium(context.Background(), StorageMediumSpec{
		ID: "offsite_s3", Type: "s3", Region: "us-east-1", Bucket: "nas-backups",
	})
	if err == nil {
		t.Fatal("a medium naming no credential source at all was accepted")
	}
}

// declaredOnly drops the local hard drive from a destinations list,
// leaving the ones an operator actually declared in config.yaml.
//
// It exists because #622 made the list total: it now carries the drive
// backups land on as well as the buckets, which is the whole point of the
// issue and is exactly not what a case about DECLARING one is asking
// about. A case that counted the whole list would go red for the addition
// rather than for the behaviour it is pinning, and one that indexed past
// the first entry would silently stop checking the thing it names.
func declaredOnly(mediums []StorageMediumSummary) []StorageMediumSummary {
	out := make([]StorageMediumSummary, 0, len(mediums))
	for _, m := range mediums {
		if m.IsLocal {
			continue
		}
		out = append(out, m)
	}
	return out
}
