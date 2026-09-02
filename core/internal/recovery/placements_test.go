package recovery

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func sizePtr(v int64) *int64 { return &v }

func manifestWithPlacements() Manifest {
	verified := time.Date(2026, 8, 20, 3, 4, 0, 0, time.UTC)
	m := testManifest()
	m.Placements = []ManifestPlacement{
		{
			Medium:            "local",
			Location:          "/backups/postgres-primary/backup-2026-08-20.dump",
			SizeBytes:         sizePtr(1024),
			Checksum:          "deadbeef",
			ChecksumAlgorithm: "sha256",
			VerificationClass: VerificationContent,
			VerifiedAt:        &verified,
			Status:            PlacementActive,
		},
		{
			Medium:            "cold_s3",
			Location:          "backups/production/postgres-primary/backup-2026-08-20.dump",
			SizeBytes:         sizePtr(1024),
			Checksum:          "deadbeef",
			ChecksumAlgorithm: "sha256",
			VerificationClass: VerificationExistence,
			Status:            PlacementActive,
		},
	}
	return m
}

func TestManifestPlacementsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := manifestWithPlacements()
	if err := WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	got, err := ReadManifest(ManifestPath(dir, m.ArtifactName))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if len(got.Placements) != len(m.Placements) {
		t.Fatalf("read back %d placements, want %d", len(got.Placements), len(m.Placements))
	}
	for i := range m.Placements {
		want, have := m.Placements[i], got.Placements[i]
		if want.Medium != have.Medium || want.Location != have.Location || want.Status != have.Status ||
			want.Checksum != have.Checksum || want.ChecksumAlgorithm != have.ChecksumAlgorithm ||
			want.VerificationClass != have.VerificationClass {
			t.Errorf("placement %d round-tripped as %+v, want %+v", i, have, want)
		}
		if (want.SizeBytes == nil) != (have.SizeBytes == nil) || (want.SizeBytes != nil && *want.SizeBytes != *have.SizeBytes) {
			t.Errorf("placement %d size %v, want %v", i, have.SizeBytes, want.SizeBytes)
		}
		if (want.VerifiedAt == nil) != (have.VerifiedAt == nil) || (want.VerifiedAt != nil && !want.VerifiedAt.Equal(*have.VerifiedAt)) {
			t.Errorf("placement %d verified_at %v, want %v", i, have.VerifiedAt, want.VerifiedAt)
		}
	}
}

// The format version is deliberately not bumped for placements, so a
// binary that predates EPIC E has to be able to read a manifest this build
// writes and reconstruct exactly the row it always did. Decoding into a
// struct that has never heard of the field is what that binary does.
func TestAManifestWithPlacementsStillReadsOnAnOlderBinary(t *testing.T) {
	dir := t.TempDir()
	m := manifestWithPlacements()
	if err := WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	data, err := os.ReadFile(ManifestPath(dir, m.ArtifactName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	// The pre-EPIC-E shape, field for field, with no placements in it.
	type olderManifest struct {
		FormatVersion      int        `json:"format_version"`
		Source             string     `json:"source"`
		BackupSet          string     `json:"backup_set"`
		ArtifactName       string     `json:"artifact_name"`
		RemotePath         string     `json:"remote_path"`
		ProducerTimestamp  *time.Time `json:"producer_timestamp,omitempty"`
		ReceivedTimestamp  time.Time  `json:"received_timestamp"`
		RetentionTimestamp time.Time  `json:"retention_timestamp"`
		SizeBytes          int64      `json:"size_bytes"`
		Checksum           string     `json:"checksum,omitempty"`
		ChecksumAlgorithm  string     `json:"checksum_algorithm,omitempty"`
		ValidationPassed   *bool      `json:"validation_passed,omitempty"`
		ValidationDetail   string     `json:"validation_detail,omitempty"`
	}
	var older olderManifest
	if err := json.Unmarshal(data, &older); err != nil {
		t.Fatalf("an older binary could not parse this manifest: %v", err)
	}
	if older.FormatVersion != CurrentFormatVersion {
		t.Fatalf("format_version = %d, want %d: bumping it would make every manifest this build writes unreadable to the build an operator might roll back to",
			older.FormatVersion, CurrentFormatVersion)
	}
	if older.ArtifactName != m.ArtifactName || older.Checksum != m.Checksum || !older.RetentionTimestamp.Equal(m.RetentionTimestamp) {
		t.Errorf("an older binary read back %+v, want the same identity, checksum and retention timestamp as %+v", older, m)
	}
}

// A sidecar is untrusted input, so what it may CONTAIN is checked, and a
// garbled one is refused rather than half-believed.
func TestManifestRefusesAGarbledPlacement(t *testing.T) {
	for name, mutate := range map[string]func(*ManifestPlacement){
		"no medium":        func(p *ManifestPlacement) { p.Medium = "" },
		"unknown status":   func(p *ManifestPlacement) { p.Status = "PROBABLY_THERE" },
		"unknown class":    func(p *ManifestPlacement) { p.VerificationClass = "vibes" },
		"negative size":    func(p *ManifestPlacement) { p.SizeBytes = sizePtr(-1) },
		"no status at all": func(p *ManifestPlacement) { p.Status = "" },
	} {
		m := manifestWithPlacements()
		mutate(&m.Placements[0])
		if err := m.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
}

// FR-28's sidecar key layout, which a rebuild from a medium has to be able
// to compute without asking whoever uploaded the object.
func TestObjectManifestKeyFor(t *testing.T) {
	for _, tc := range []struct{ object, want string }{
		{"backups/production/postgres/db.dump", "backups/production/postgres/.manifest/db.dump.json"},
		{"production/postgres/db.dump", "production/postgres/.manifest/db.dump.json"},
	} {
		if got := ObjectManifestKeyFor(tc.object); got != tc.want {
			t.Errorf("ObjectManifestKeyFor(%q) = %q, want %q", tc.object, got, tc.want)
		}
	}

	// It has to be deterministic: an interrupted upload retried later must
	// target the same object rather than leaving a second manifest behind.
	first := ObjectManifestKeyFor("backups/production/postgres/db.dump")
	if second := ObjectManifestKeyFor("backups/production/postgres/db.dump"); first != second {
		t.Errorf("the sidecar key is not deterministic: %q then %q", first, second)
	}
	if !strings.Contains(first, "/"+objectManifestDir+"/") {
		t.Errorf("the sidecar key %q does not put the manifest in its own %s namespace, so a plain prefix listing of the bucket would return one for every artifact", first, objectManifestDir)
	}
}
