package service

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// EPIC E, FR-34's read half and FR-27's consent, at this boundary (#240).
//
// Everything here is written against one failure. Issue #361 was a run
// cycle that backed nothing up and reported success; the surface version
// of that defect is a row reading "stored on offsite_s3" for a copy
// nobody can reach, or a tick beside a copy nobody has checked. So the
// tests below all ask the same question in different words: does this
// boundary keep "there is no copy", "there is a copy nobody can reach"
// and "there is a copy nobody has checked" apart?

func mediumsConfig(t *testing.T, mediums ...config.StorageMedium) mediumIndex {
	t.Helper()
	return indexMediums(&config.Config{StorageMediums: mediums})
}

// TestToServiceArtifact_AnArtifactStillTransferringHasNoCopyAtAll is the
// ".partial has no row" rule, at the layer a client reads.
//
// It matters because LocalPath is populated the whole time: at
// TRANSFERRING it names the partial file being written. A read model that
// reported that path as storage would tell an operator a backup is on
// their NAS at exactly the moment it demonstrably is not, and the honest
// answer, no copies, is the one that lets a surface say so.
func TestToServiceArtifact_AnArtifactStillTransferringHasNoCopyAtAll(t *testing.T) {
	rec := state.Record{
		LocalPath:  "/volume1/backups/production/pg/a.dump.partial",
		State:      "TRANSFERRING",
		Placements: nil,
	}

	a := toServiceArtifact(rec, mediumsConfig(t))

	if len(a.Placements) != 0 {
		t.Fatalf("an artifact still transferring reports %d copies: %+v", len(a.Placements), a.Placements)
	}
	// The precondition, so the emptiness above is not satisfied by a
	// record that never had a path to misreport in the first place.
	if a.LocalPath == "" {
		t.Fatal("precondition failed: this record was supposed to carry a partial path for the read model to be tempted by")
	}
}

// TestToServicePlacements_AReleasedCopyIsNotServedAsACopy is the other
// absence. state.PlacementGone means the journal KNOWS the copy is no
// longer there; a row for it on the wire reads as a copy in every layout
// anybody would write for a list of copies, so it does not go on the wire
// at all.
func TestToServicePlacements_AReleasedCopyIsNotServedAsACopy(t *testing.T) {
	rec := state.Record{Placements: []state.Placement{
		{Medium: state.MediumLocal, Location: "/volume1/backups/a.dump", Status: state.PlacementGone},
		{Medium: "offsite_s3", Location: "p/a.dump", Status: state.PlacementActive, VerificationClass: state.VerificationContent},
	}}

	a := toServiceArtifact(rec, mediumsConfig(t, config.StorageMedium{
		ID: "offsite_s3", Type: config.StorageMediumTypeS3, Bucket: "nas-backups", StorageClass: config.StorageClassStandardIA,
	}))

	if len(a.Placements) != 1 {
		t.Fatalf("got %d copies, want 1: a copy the journal knows is gone is not a copy\n%+v", len(a.Placements), a.Placements)
	}
	if a.Placements[0].Medium != "offsite_s3" {
		t.Errorf("the surviving copy is on %q, want offsite_s3", a.Placements[0].Medium)
	}
	// A DELETE_PENDING copy is the opposite case and must survive: the
	// delete is recorded and may not have happened, so the bytes are
	// probably still there and an operator is entitled to know one of
	// their copies is on its way out.
	pending := state.Record{Placements: []state.Placement{
		{Medium: state.MediumLocal, Location: "/volume1/backups/a.dump", Status: state.PlacementDeletePending},
	}}
	if got := toServiceArtifact(pending, mediumsConfig(t)); len(got.Placements) != 1 {
		t.Errorf("a copy with a recorded but unfinished delete was dropped; that is a copy, and it is one an operator should see going")
	}
}

// TestToServicePlacements_ACopyOnAMediumNobodyCanReachSaysSo is the
// distinction the whole issue turns on.
//
// The journal says a durable copy was made on offsite_s3. The
// configuration no longer declares offsite_s3, so this deployment has no
// bucket, no endpoint and no credential to reach it with. It can neither
// confirm nor deny the copy, and it must say the first thing rather than
// implying the second.
func TestToServicePlacements_ACopyOnAMediumNobodyCanReachSaysSo(t *testing.T) {
	verified := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	rec := state.Record{Placements: []state.Placement{{
		Medium:            "offsite_s3",
		Location:          "rclone-manager/production/pg/a.dump",
		Status:            state.PlacementActive,
		VerificationClass: state.VerificationContent,
		VerifiedAt:        &verified,
	}}}

	// No mediums declared: the entry was removed from config.yaml.
	a := toServiceArtifact(rec, mediumsConfig(t))

	if len(a.Placements) != 1 {
		t.Fatalf("got %d copies, want 1: an unreachable copy is still a recorded copy", len(a.Placements))
	}
	p := a.Placements[0]
	if p.Access != string(archive.Unreachable) {
		t.Errorf("Access = %q, want %q", p.Access, archive.Unreachable)
	}
	if p.Access == string(archive.Immediate) {
		t.Error("a copy nobody can reach was reported as readable on demand; that is issue #361's defect in a different medium")
	}
	// The class is not invented. This deployment genuinely does not know
	// what kind of place that was any more, and guessing "s3" would be a
	// claim it cannot support.
	if p.MediumType != "" || p.StorageClass != "" {
		t.Errorf("MediumType = %q, StorageClass = %q; want both empty for a medium the configuration no longer describes", p.MediumType, p.StorageClass)
	}
	// What WAS achieved, before contact was lost, is still reported as
	// achieved. Erasing it would be its own dishonesty in the other
	// direction.
	if p.VerificationClass != state.VerificationContent || !p.VerifiedAt.Equal(verified) {
		t.Errorf("the verification this copy did achieve was lost: class %q at %s", p.VerificationClass, p.VerifiedAt)
	}
}

// TestToServicePlacements_ACopyNobodyHasCheckedCarriesNoClass is the third
// absence: a placement with no verification class at all.
//
// Empty has to survive to the boundary as empty. The temptation is to
// default it to the weakest rung, because a string field wants a value,
// and "existence" is a claim that an object was seen at the recorded size.
// Nobody has seen anything.
func TestToServicePlacements_ACopyNobodyHasCheckedCarriesNoClass(t *testing.T) {
	rec := state.Record{Placements: []state.Placement{{
		Medium: "offsite_cold", Location: "p/a.dump", Status: state.PlacementActive,
	}}}

	a := toServiceArtifact(rec, mediumsConfig(t, config.StorageMedium{
		ID: "offsite_cold", Type: config.StorageMediumTypeS3, Bucket: "nas-archive", StorageClass: config.StorageClassDeepArchive,
	}))

	p := a.Placements[0]
	if p.VerificationClass != "" {
		t.Errorf("VerificationClass = %q for a copy nothing has verified; want empty", p.VerificationClass)
	}
	if !p.VerifiedAt.IsZero() {
		t.Errorf("VerifiedAt = %s for a copy nothing has verified", p.VerifiedAt)
	}
	// And the archive class reaches the surface, because an operator has
	// to learn this before they need the file, not while they wait for it.
	if p.Access != string(archive.RequiresRestore) {
		t.Errorf("Access = %q, want %q for a DEEP_ARCHIVE copy", p.Access, archive.RequiresRestore)
	}
	if p.StorageClass != config.StorageClassDeepArchive {
		t.Errorf("StorageClass = %q, want %q", p.StorageClass, config.StorageClassDeepArchive)
	}
}

// TestToServicePlacements_AnOrdinaryLocalCopyIsUnchanged is FR-35's
// compatibility case at this layer: every deployment written before EPIC E
// has exactly one local placement per artifact after migration 0007's
// backfill, and it must read as the plainest possible thing.
func TestToServicePlacements_AnOrdinaryLocalCopyIsUnchanged(t *testing.T) {
	verified := time.Date(2026, 8, 14, 2, 14, 11, 0, time.UTC)
	rec := state.Record{Placements: []state.Placement{{
		Medium:            state.MediumLocal,
		Location:          "/volume1/backups/production/pg/a.dump",
		Size:              int64p(0),
		Status:            state.PlacementActive,
		VerificationClass: state.VerificationContent,
		VerifiedAt:        &verified,
	}}}

	got := toServiceArtifact(rec, mediumsConfig(t)).Placements
	want := []Placement{{
		Medium:            state.MediumLocal,
		MediumType:        MediumTypeLocal,
		Location:          "/volume1/backups/production/pg/a.dump",
		SizeBytes:         int64p(0),
		VerificationClass: state.VerificationContent,
		VerifiedAt:        verified,
		Access:            string(archive.Immediate),
		Status:            state.PlacementActive,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("local placement projected as\n %+v\nwant\n %+v", got, want)
	}
	// A zero-byte artifact is a real artifact, so a recorded zero has to
	// stay distinguishable from nothing recorded at all.
	if got[0].SizeBytes == nil || *got[0].SizeBytes != 0 {
		t.Errorf("SizeBytes = %v; a recorded zero must not collapse into \"not reported\"", got[0].SizeBytes)
	}
}

// TestStorageSchema_ServesTheEngineOwnWordsForEachRung is the anti-drift
// half of the REFACTOR requirement: the ladder a surface renders comes
// from core/internal/placement, not from a paraphrase living in a
// frontend.
//
// DownloadsObject in particular is a MECHANISM: it is the predicate the
// automatic revalidation path refuses on, so a surface reading it is
// reading the same fact the engine acts on rather than a note somebody
// keeps in step by hand.
func TestStorageSchema_ServesTheEngineOwnWordsForEachRung(t *testing.T) {
	schema := StorageSchema()

	if len(schema.VerificationClasses) != len(placement.Classes) {
		t.Fatalf("the served ladder has %d rungs, the engine has %d", len(schema.VerificationClasses), len(placement.Classes))
	}
	for i, c := range placement.Classes {
		got := schema.VerificationClasses[i]
		if got.Class != string(c) {
			t.Errorf("rung %d is %q, want %q (order is meaningful: it is what \"stronger than\" means)", i, got.Class, c)
		}
		if got.Proves != c.Proves() {
			t.Errorf("%s: Proves = %q, want the engine's own %q", c, got.Proves, c.Proves())
		}
		if got.Requires != c.Cost() {
			t.Errorf("%s: Requires = %q, want the engine's own %q", c, got.Requires, c.Cost())
		}
		if got.DownloadsObject != c.CostsEgress() {
			t.Errorf("%s: DownloadsObject = %v, want %v", c, got.DownloadsObject, c.CostsEgress())
		}
	}
	// Exactly one rung downloads the bytes, and it is the strongest one.
	// A schema where none did would let a surface offer every class as
	// free.
	egress := 0
	for _, c := range schema.VerificationClasses {
		if c.DownloadsObject {
			egress++
		}
	}
	if egress != 1 {
		t.Errorf("%d rungs cost egress, want exactly 1", egress)
	}

	// The two disclosures are served, and neither carries a figure. There
	// is no price list in this product, so a number in either one would be
	// invented (the #211 rule).
	if !strings.Contains(schema.MediumDisclosure, "delete") {
		t.Errorf("the medium disclosure does not name the deletion consequence:\n%s", schema.MediumDisclosure)
	}
	for name, text := range map[string]string{"medium": schema.MediumDisclosure, "retrieval": schema.RetrievalDisclosure} {
		for _, figure := range []string{"$", "USD", "per GB", "per gigabyte", "%"} {
			if strings.Contains(text, figure) {
				t.Errorf("the %s disclosure carries %q, which is a figure this product cannot compute:\n%s", name, figure, text)
			}
		}
	}
}

// TestToStorageMediumSummaries_DescribeThePlaceAndNeverTheKey is FR-33 at
// the type level, before the canary test below reaches for a running
// service: the summary type has no field a secret could travel in, so the
// absence is structural rather than something a filter has to keep
// achieving.
func TestToStorageMediumSummaries_DescribeThePlaceAndNeverTheKey(t *testing.T) {
	cfg := &config.Config{StorageMediums: []config.StorageMedium{
		{ID: "offsite_s3", Type: config.StorageMediumTypeS3, Bucket: "nas-backups", Region: "us-east-1",
			Credentials: config.MediumCredentials{Env: "BACKUP_S3_OFFSITE"}},
		{ID: "offsite_cold", Type: config.StorageMediumTypeS3, Bucket: "nas-archive", StorageClass: config.StorageClassDeepArchive,
			Credentials: config.MediumCredentials{File: "/var/lib/backup-manager/s3/cold.creds"}},
	}}

	got := toStorageMediumSummaries(cfg)
	want := []StorageMediumSummary{
		{ID: "offsite_s3", Type: "s3", Bucket: "nas-backups", Region: "us-east-1",
			StorageClass: config.StorageClassStandard, ReadsRequireRestore: false},
		{ID: "offsite_cold", Type: "s3", Bucket: "nas-archive",
			StorageClass: config.StorageClassDeepArchive, ReadsRequireRestore: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mediums projected as\n %+v\nwant\n %+v", got, want)
	}

	// The unconfigured storage class resolves on the way out, so a client
	// never has to know what an empty value defaults to; and the archive
	// predicate is the engine's, not a second list in a frontend.
	if got[0].StorageClass != config.StorageClassStandard {
		t.Errorf("an unset storage class reached the boundary as %q", got[0].StorageClass)
	}

	// Nothing anywhere in the projection mentions where the credentials
	// come from, let alone what they are. A path is not a secret, but it
	// is a fact about this machine that an API caller has no use for and a
	// reader of an exported response has every use for.
	rendered, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, forbidden := range []string{"BACKUP_S3_OFFSITE", "/var/lib", "creds", "credential"} {
		if strings.Contains(strings.ToLower(string(rendered)), strings.ToLower(forbidden)) {
			t.Errorf("the mediums surface carries %q:\n%s", forbidden, rendered)
		}
	}
}

// --------------------------------------------------------- the gate ------

// mediumCanary is a value that exists nowhere else in this repository, so
// finding it in an output is proof of where it came from. It is the same
// enforcement shape E1.3 built for the transport layer (FR-33's canary),
// applied to the surface this issue adds.
const mediumCanary = "CANARY-240-9f31ab6c4d2e-DO-NOT-SERVE"

const mediumsRetentionBlock = "retention:\n" +
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
	"    storage_class: STANDARD_IA\n" +
	"    credentials:\n" +
	"      env: BACKUP_S3_" + mediumCanary + "\n"

const localOnlyRetentionBlock = "retention:\n" +
	"  timezone: UTC\n" +
	"  week_starts_on: monday\n" +
	"  tiers:\n" +
	"    - name: daily\n" +
	"      granularity: day\n" +
	"      keep: 7\n" +
	"    - name: monthly\n" +
	"      granularity: month\n" +
	"      keep: 12\n"

func openWithRetention(t *testing.T, retention string) (*BackupService, string) {
	t.Helper()
	configPath := writeTestConfigFileWithRetention(t, retention)
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	return svc, configPath
}

// TestSettings_TheMediumsSurfaceCarriesNoCredentialMaterial is FR-33's
// canary, end to end through the real read path an API caller reaches.
//
// The canary is planted where a medium's credentials are NAMED, which is
// the only place a secret is expressible in this schema at all: there is
// no field for a literal key, so what a leak would actually look like here
// is the reference travelling out with everything else.
func TestSettings_TheMediumsSurfaceCarriesNoCredentialMaterial(t *testing.T) {
	t.Setenv("BACKUP_S3_"+mediumCanary, "not a real credential, and it carries "+mediumCanary)
	svc, _ := openWithRetention(t, mediumsRetentionBlock)

	settings, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}

	// The positive control. Without it, an empty mediums list would
	// satisfy every absence below and this test would prove nothing.
	if len(settings.Mediums) != 1 {
		t.Fatalf("Settings reported %d mediums, want 1: the absence checks below need something to be absent FROM", len(settings.Mediums))
	}
	if settings.Mediums[0].ID != "offsite_s3" || settings.Mediums[0].Bucket != "nas-backups" {
		t.Fatalf("the medium came back as %+v, which is not the one the config declares", settings.Mediums[0])
	}

	rendered, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(rendered), mediumCanary) {
		t.Errorf("the canary reached the settings surface:\n%s", rendered)
	}
}

// TestUpdateSettings_RefusesAFirstMappingWithoutTheAcknowledgment is the
// deny half of FR-27's consent gate (TDD invariant 4).
//
// The point of enforcing it HERE rather than in a form is that a form can
// be bypassed with one curl. What is being consented to is the deletion of
// the copy on the operator's own NAS, so the gate lives where the write
// lands.
func TestUpdateSettings_RefusesAFirstMappingWithoutTheAcknowledgment(t *testing.T) {
	svc, configPath := openWithRetention(t, localOnlyRetentionBlock)

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	_, err = svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Tiers: []RetentionTier{
			{Name: "daily", Granularity: "day", Keep: 7},
			{Name: "monthly", Granularity: "month", Keep: 12, Medium: "offsite_s3"},
		}},
	})
	if err == nil {
		t.Fatal("UpdateSettings accepted a first tier-to-medium mapping with no acknowledgment")
	}
	if !errorsIsMediumDisclosure(err) {
		t.Fatalf("error = %v, want ErrMediumDisclosureRequired; a caller has to tell this from a malformed request", err)
	}
	// The refusal IS the disclosure: a client that renders the message has
	// shown the operator the right words by construction.
	if !strings.Contains(err.Error(), "delete") {
		t.Errorf("the refusal does not name the deletion consequence:\n%v", err)
	}
	if !strings.Contains(err.Error(), "monthly") || !strings.Contains(err.Error(), "offsite_s3") {
		t.Errorf("the refusal does not say which tier is about to leave:\n%v", err)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("a refused settings write changed the operator's configuration file:\n%s", after)
	}
}

// TestUpdateSettings_AcceptsTheSameWriteWithTheAcknowledgment is the allow
// half, and it is the one that proves the refusal above was about the
// acknowledgment rather than about anything else in the request.
func TestUpdateSettings_AcceptsTheSameWriteWithTheAcknowledgment(t *testing.T) {
	svc, configPath := openWithRetention(t, localOnlyRetentionBlock)

	settings, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Tiers: []RetentionTier{
			{Name: "daily", Granularity: "day", Keep: 7},
			{Name: "monthly", Granularity: "month", Keep: 12, Medium: "offsite_s3"},
		}},
		AcknowledgeMediumDisclosure: true,
	})
	// The config declares no offsite_s3, so config.Validate refuses the
	// dangling reference. That refusal is internal/config's and it is the
	// right one; what matters here is that it is NOT the disclosure
	// refusal, because the acknowledgment was given.
	if err == nil {
		t.Fatalf("expected the dangling-medium validation refusal; got settings %+v", settings)
	}
	if errorsIsMediumDisclosure(err) {
		t.Fatalf("an acknowledged write was still refused for want of an acknowledgment: %v", err)
	}
	if !strings.Contains(err.Error(), "offsite_s3") {
		t.Errorf("error = %v, want internal/config's dangling-medium refusal", err)
	}
	if _, err := os.ReadFile(configPath); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
}

// TestUpdateSettings_AcknowledgedWriteAgainstADeclaredMediumSucceeds is
// the allow half with a medium that actually exists, so the gate is proven
// to let a real write through rather than only to swap one refusal for
// another.
func TestUpdateSettings_AcknowledgedWriteAgainstADeclaredMediumSucceeds(t *testing.T) {
	// The file already declares offsite_s3 but no tier names it: a
	// staging configuration, which FR-27 says is legal.
	staged := strings.Replace(mediumsRetentionBlock, "      keep: 12\n      medium: offsite_s3\n", "      keep: 12\n", 1)
	svc, configPath := openWithRetention(t, staged)

	settings, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Tiers: []RetentionTier{
			{Name: "daily", Granularity: "day", Keep: 7},
			{Name: "monthly", Granularity: "month", Keep: 12, Medium: "offsite_s3"},
		}},
		AcknowledgeMediumDisclosure: true,
	})
	if err != nil {
		t.Fatalf("UpdateSettings with the acknowledgment: %v", err)
	}
	if settings.Retention.Tiers[1].Medium != "offsite_s3" {
		t.Errorf("the write did not take: tiers[1].Medium = %q", settings.Retention.Tiers[1].Medium)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "medium: offsite_s3") {
		t.Errorf("the acknowledged mapping did not reach the file:\n%s", raw)
	}
}

// TestUpdateSettings_DoesNotAskAgainForAMappingAlreadyInTheFile is the
// half that keeps the gate from becoming noise.
//
// FR-27's consent is given once per mapping and then moves execute as
// declared policy. An operator editing a keep count on a configuration
// that already sends monthly to offsite_s3 is not making that decision
// again, and a product that asked every time would train them to tick the
// box without reading it, which is worse than not asking.
func TestUpdateSettings_DoesNotAskAgainForAMappingAlreadyInTheFile(t *testing.T) {
	svc, _ := openWithRetention(t, mediumsRetentionBlock)

	settings, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Tiers: []RetentionTier{
			{Name: "daily", Granularity: "day", Keep: 9},
			{Name: "monthly", Granularity: "month", Keep: 12, Medium: "offsite_s3"},
		}},
	})
	if err != nil {
		t.Fatalf("UpdateSettings on an existing mapping: %v", err)
	}
	if settings.Retention.Tiers[0].Keep != 9 {
		t.Errorf("the edit did not take: daily keep = %d", settings.Retention.Tiers[0].Keep)
	}
	if settings.Retention.Tiers[1].Medium != "offsite_s3" {
		t.Errorf("the existing mapping was lost: tiers[1].Medium = %q", settings.Retention.Tiers[1].Medium)
	}
}

// TestUpdateSettings_AsksAgainForASecondTierGoingOffLocalDisk is the
// distributive reading of FR-27, held as a test because it is the reading
// this code chose.
//
// A configuration that sends monthly to offsite_s3 has consented to
// monthly leaving. It has not consented to daily leaving, and daily is a
// different set of artifacts, on a medium that may have a different
// storage class with entirely different access behaviour.
func TestUpdateSettings_AsksAgainForASecondTierGoingOffLocalDisk(t *testing.T) {
	svc, _ := openWithRetention(t, mediumsRetentionBlock)

	_, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Retention: &RetentionUpdate{Tiers: []RetentionTier{
			{Name: "daily", Granularity: "day", Keep: 7, Medium: "offsite_s3"},
			{Name: "monthly", Granularity: "month", Keep: 12, Medium: "offsite_s3"},
		}},
	})
	if err == nil {
		t.Fatal("a second tier was sent off local disk with no acknowledgment")
	}
	if !errorsIsMediumDisclosure(err) {
		t.Fatalf("error = %v, want ErrMediumDisclosureRequired", err)
	}
	if !strings.Contains(err.Error(), "daily") {
		t.Errorf("the refusal does not name the tier that is newly leaving:\n%v", err)
	}
	if strings.Contains(err.Error(), "monthly") {
		t.Errorf("the refusal re-litigates a mapping the file already had:\n%v", err)
	}
}

// TestUpdateSettings_AnAcknowledgmentAloneIsNotASettingsChange keeps the
// new field from becoming a way around the "a write must name a setting"
// rule. A request carrying only the tick asks for nothing, and honouring
// it would rewrite the operator's file and move ConfigRevision for no
// reason.
func TestUpdateSettings_AnAcknowledgmentAloneIsNotASettingsChange(t *testing.T) {
	svc, _ := openWithRetention(t, localOnlyRetentionBlock)

	_, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{AcknowledgeMediumDisclosure: true})
	if err == nil {
		t.Fatal("a request carrying nothing but an acknowledgment was accepted")
	}
	if !strings.Contains(err.Error(), "at least one setting") {
		t.Errorf("error = %v, want the \"names no setting\" refusal", err)
	}
}

// --------------------------------------------------- the cycle counts ----

// TestOperation_AFinishedCycleReportsWhatItGotThrough is the loose end
// #368 left: it recorded artifacts_walked and artifacts_through into the
// operation's summary, and nothing read them back. A cycle that walked
// twelve backups and got none through has to be visible as that, and not
// only as an exit code in a terminal nobody is watching.
func TestOperation_AFinishedCycleReportsWhatItGotThrough(t *testing.T) {
	svc := newTestService(t)
	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		return app.CycleReport{Sets: []app.BackupSetCycleResult{
			{Progress: app.CycleProgress{Walked: 12}},
		}}
	})

	op, err := svc.SubmitRunCycle(context.Background(), RunCycleRequest{
		IdempotencyKey: "idem-counts", Actor: "alice", ConfigRevision: svc.ConfigRevision(),
	})
	if err != nil {
		t.Fatalf("SubmitRunCycle: %v", err)
	}
	done := waitForTerminalStatus(t, svc, op.ID)
	if done.Status != "completed" {
		t.Fatalf("Status = %q, want completed (Error = %q)", done.Status, done.Error)
	}

	if done.Cycle == nil {
		t.Fatal("a finished run cycle reports no counts, so a surface cannot tell it from one that backed everything up")
	}
	if done.Cycle.ArtifactsWalked != 12 || done.Cycle.ArtifactsThrough != 0 {
		t.Errorf("Cycle = %+v, want walked 12, through 0", *done.Cycle)
	}
}

// TestParseCycleSummary_ReportsNothingRatherThanZeroes is the pointer's
// whole justification, and the case that would otherwise be a confident
// wrong answer: a row with no counts to read must produce no outcome, not
// an outcome of zeroes. "Nothing got through" is the loudest thing this
// field can say, and saying it about a cycle nobody measured would send an
// operator hunting a failure that did not happen.
func TestParseCycleSummary_ReportsNothingRatherThanZeroes(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  state.Operation
	}{
		{"still running", state.Operation{Action: ActionRunCycle, Status: "running"}},
		{"failed", state.Operation{Action: ActionRunCycle, Status: "failed", Error: "boom"}},
		{"no summary recorded", state.Operation{Action: ActionRunCycle, Status: state.OperationCompleted}},
		{"a summary that is not JSON", state.Operation{Action: ActionRunCycle, Status: state.OperationCompleted, Result: "done"}},
		{"a summary from a build older than #368", state.Operation{
			Action: ActionRunCycle, Status: state.OperationCompleted,
			Result: `{"backup_sets_processed":3,"duration_ms":812}`,
		}},
		{"some other action", state.Operation{Action: "restore", Status: state.OperationCompleted,
			Result: `{"artifacts_walked":12,"artifacts_through":0}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseCycleSummary(tc.rec); got != nil {
				t.Errorf("parseCycleSummary = %+v, want nil: an absent count is not a count of zero", *got)
			}
		})
	}

	// The positive control, so the nils above are not the only answer this
	// function can give.
	ok := parseCycleSummary(state.Operation{
		Action: ActionRunCycle, Status: state.OperationCompleted,
		Result: `{"backup_sets_processed":3,"artifacts_walked":12,"artifacts_through":0,"duration_ms":812}`,
	})
	if ok == nil || ok.ArtifactsWalked != 12 || ok.ArtifactsThrough != 0 || ok.BackupSetsProcessed != 3 {
		t.Fatalf("parseCycleSummary on a real summary = %+v, want 3 sets, 12 walked, 0 through", ok)
	}
}

func errorsIsMediumDisclosure(err error) bool {
	for e := err; e != nil; {
		if e == ErrMediumDisclosureRequired {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// ---------------------------------------- the same gate, per backup set ----
//
// An override is a whole chain in its own right (config.Retention.Tiers'
// own doc, and validateMediumReferences walks per-set chains for exactly
// this reason), so it can send a tier's artifacts off local disk as surely
// as the deployment's policy can. FR-27 says "any tier of a
// backup-affecting chain", and a gate that stood in front of
// UpdateSettings alone would be a gate one PUT walks around.

// perSetMediumOverride is an override that keeps the mapping this set
// already inherits (monthly -> offsite_s3) and adds one it does not
// (daily -> offsite_s3).
func perSetMediumOverride() RetentionOverride {
	return RetentionOverride{Tiers: []RetentionTier{
		{Name: "daily", Granularity: "day", Keep: 7, Medium: "offsite_s3"},
		{Name: "monthly", Granularity: "month", Keep: 12, Medium: "offsite_s3"},
	}}
}

// TestSetBackupSetRetention_RefusesAFirstMappingWithoutTheAcknowledgment is
// the deny half on the per-set write. The refusal names the tier that is
// newly leaving and NOT the one the set already inherits: re-litigating a
// mapping the deployment consented to is how a product teaches an operator
// to tick the box without reading it.
func TestSetBackupSetRetention_RefusesAFirstMappingWithoutTheAcknowledgment(t *testing.T) {
	svc, configPath := openWithRetention(t, mediumsRetentionBlock)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	_, err = svc.SetBackupSetRetention(context.Background(), "production/postgres-primary", perSetMediumOverride())
	if err == nil {
		t.Fatal("SetBackupSetRetention accepted a first tier-to-medium mapping with no acknowledgment")
	}
	if !errorsIsMediumDisclosure(err) {
		t.Fatalf("error = %v, want ErrMediumDisclosureRequired; a caller has to tell this from a malformed policy", err)
	}
	if !strings.Contains(err.Error(), "delete") {
		t.Errorf("the refusal does not name the deletion consequence:\n%v", err)
	}
	if !strings.Contains(err.Error(), "daily -> offsite_s3") {
		t.Errorf("the refusal does not say which tier is about to leave:\n%v", err)
	}
	if strings.Contains(err.Error(), "monthly") {
		t.Errorf("the refusal re-litigates a mapping this set already inherits from the deployment:\n%v", err)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("a refused per-set write changed the operator's configuration file:\n%s", after)
	}
	got, err := svc.BackupSetRetention(context.Background(), "production/postgres-primary")
	if err != nil {
		t.Fatalf("BackupSetRetention: %v", err)
	}
	if got.IsOverride {
		t.Error("a refused write left the set overriding")
	}
}

// TestSetBackupSetRetention_AcceptsTheSameWriteWithTheAcknowledgment is
// the allow half, against a medium the file declares, so the gate is
// proven to let a real per-set write through and to land the mapping
// under the set rather than on the deployment's chain.
func TestSetBackupSetRetention_AcceptsTheSameWriteWithTheAcknowledgment(t *testing.T) {
	svc, configPath := openWithRetention(t, mediumsRetentionBlock)

	o := perSetMediumOverride()
	o.AcknowledgeMediumDisclosure = true
	got, err := svc.SetBackupSetRetention(context.Background(), "production/postgres-primary", o)
	if err != nil {
		t.Fatalf("SetBackupSetRetention with the acknowledgment: %v", err)
	}
	if !got.IsOverride || len(got.Effective.Tiers) != 2 || got.Effective.Tiers[0].Medium != "offsite_s3" {
		t.Fatalf("the acknowledged write did not take: %+v", got.Effective.Tiers)
	}
	// The deployment's own daily tier is still local: the consent was
	// for this set's artifacts, and the write was to this set's block.
	if got.Deployment.Tiers[0].Medium != "" {
		t.Errorf("a per-set write moved the deployment's daily tier: %+v", got.Deployment.Tiers[0])
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Count(string(raw), "medium: offsite_s3") != 3 {
		t.Errorf("want the deployment's monthly plus the override's two mappings in the file, got:\n%s", raw)
	}
	if strings.Contains(string(raw), "acknowledge") {
		t.Errorf("the acknowledgment is a consent, not a setting, and it reached the file:\n%s", raw)
	}
}

// TestSetBackupSetRetention_DoesNotAskForAMappingTheSetAlreadyInherits is
// the half that keeps the per-set gate from becoming noise. A set that
// inherits a chain sending monthly to offsite_s3 is already sending its
// monthly artifacts there; an override that keeps that mapping and changes
// a number is not a decision about where backups live.
func TestSetBackupSetRetention_DoesNotAskForAMappingTheSetAlreadyInherits(t *testing.T) {
	svc, _ := openWithRetention(t, mediumsRetentionBlock)

	got, err := svc.SetBackupSetRetention(context.Background(), "production/postgres-primary", RetentionOverride{Tiers: []RetentionTier{
		{Name: "daily", Granularity: "day", Keep: 9},
		{Name: "monthly", Granularity: "month", Keep: 24, Medium: "offsite_s3"},
	}})
	if err != nil {
		t.Fatalf("SetBackupSetRetention on an inherited mapping: %v", err)
	}
	if !got.IsOverride || got.Effective.Tiers[1].Keep != 24 || got.Effective.Tiers[1].Medium != "offsite_s3" {
		t.Errorf("the edit did not take: %+v", got.Effective.Tiers)
	}
}

// TestSetBackupSetRetention_AsksAgainstTheSetsOwnChainOnceItHasOne pins
// which chain the gate compares against. Once a set overrides, what it is
// consenting FROM is its own chain, not the deployment's: a second write
// that sends weekly somewhere asks about weekly, and only weekly, even
// though weekly is not a tier the deployment's chain has at all.
func TestSetBackupSetRetention_AsksAgainstTheSetsOwnChainOnceItHasOne(t *testing.T) {
	svc, _ := openWithRetention(t, mediumsRetentionBlock)

	first := perSetMediumOverride()
	first.AcknowledgeMediumDisclosure = true
	if _, err := svc.SetBackupSetRetention(context.Background(), "production/postgres-primary", first); err != nil {
		t.Fatalf("the first, acknowledged write: %v", err)
	}

	second := RetentionOverride{Tiers: []RetentionTier{
		{Name: "daily", Granularity: "day", Keep: 7, Medium: "offsite_s3"},
		{Name: "weekly", Granularity: "week", Keep: 4, Medium: "offsite_s3"},
		{Name: "monthly", Granularity: "month", Keep: 12, Medium: "offsite_s3"},
	}}
	_, err := svc.SetBackupSetRetention(context.Background(), "production/postgres-primary", second)
	if err == nil {
		t.Fatal("a second tier going off local disk was accepted without an acknowledgment")
	}
	if !errorsIsMediumDisclosure(err) {
		t.Fatalf("error = %v, want ErrMediumDisclosureRequired", err)
	}
	if !strings.Contains(err.Error(), "weekly -> offsite_s3") {
		t.Errorf("the refusal does not name the tier newly leaving:\n%v", err)
	}
	if strings.Contains(err.Error(), "daily") || strings.Contains(err.Error(), "monthly") {
		t.Errorf("the refusal re-litigates mappings the set's own chain already has:\n%v", err)
	}
}
