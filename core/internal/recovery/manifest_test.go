// These cover the manifest format itself: what a sidecar must carry, what
// it must never carry, and what happens when one arrives that this binary
// cannot honestly read.
//
// The shape of the file follows from what a recovery manifest is for. It
// is read exactly once, at the worst moment, by code trying to rebuild a
// journal that is already gone, so every test here is about refusing to
// half-succeed: a manifest either round-trips every field a rebuild needs
// or it is rejected with a reason, and there is no path where a partially
// parsed one becomes a partially reconstructed row.
package recovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestManifestFieldsExcludeSecrets is issue #102's RED test for EPIC-B
// section 19.3's exclusion list: a recovery manifest must never contain an
// SSH private key, an authentication token, a remote password, or a
// secret environment value. Manifest's fields are hand-picked from
// internal/state.Record, which has no secret fields to begin with, but
// this test pins that guarantee structurally: it fails the moment a
// future field name even looks like it could carry one, for every field
// this package writes.
func TestManifestFieldsExcludeSecrets(t *testing.T) {
	typ := reflect.TypeOf(Manifest{})
	forbidden := []string{"key", "token", "password", "secret", "credential", "passphrase", "auth"}
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		for _, f := range forbidden {
			if strings.Contains(name, f) {
				t.Errorf("Manifest field %q looks like it could carry a secret (matches %q); EPIC-B section 19.3 forbids SSH private keys, auth tokens, remote passwords and secret env values in recovery manifests", typ.Field(i).Name, f)
			}
		}
	}
}

// testManifest is the one valid manifest every test here starts from,
// mutating a single field to make its own case.
//
// The three timestamps are deliberately all different, and deliberately
// not in the order you would guess: retention (02:59) is before received
// (03:05), and the producer's own (03:00) sits between them. A fixture
// that used one value for all three would let a field-crossing bug in
// encode or decode round-trip perfectly, and the retention timestamps are
// exactly the fields where crossing two of them changes which artifacts
// FR-18 keeps.
func testManifest() Manifest {
	produced := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)
	received := time.Date(2026, 8, 20, 3, 5, 0, 0, time.UTC)
	retention := time.Date(2026, 8, 20, 2, 59, 0, 0, time.UTC)
	passed := true
	return Manifest{
		FormatVersion:      CurrentFormatVersion,
		Source:             "production",
		BackupSet:          "postgres-primary",
		ArtifactName:       "backup-2026-08-20.dump",
		RemotePath:         "incoming/backup-2026-08-20.dump",
		ProducerTimestamp:  &produced,
		ReceivedTimestamp:  received,
		RetentionTimestamp: retention,
		SizeBytes:          1024,
		Checksum:           "deadbeef",
		ChecksumAlgorithm:  "sha256",
		ValidationPassed:   &passed,
		ValidationDetail:   "checksum verified",
	}
}

// TestWriteReadManifest_RoundTrips is the whole contract in one case: a
// manifest written to a directory and read back is the same manifest.
//
// It asserts the timestamps with Equal rather than ==, because these have
// been through JSON and a time.Time that came back from RFC 3339 carries a
// different location pointer than the one that went in even when it names
// the same instant.
func TestWriteReadManifest_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	m := testManifest()

	if err := WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	got, err := ReadManifest(ManifestPath(dir, m.ArtifactName))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}

	switch {
	case got.FormatVersion != m.FormatVersion:
		t.Errorf("FormatVersion = %d, want %d", got.FormatVersion, m.FormatVersion)
	case got.Source != m.Source:
		t.Errorf("Source = %q, want %q", got.Source, m.Source)
	case got.BackupSet != m.BackupSet:
		t.Errorf("BackupSet = %q, want %q", got.BackupSet, m.BackupSet)
	case got.ArtifactName != m.ArtifactName:
		t.Errorf("ArtifactName = %q, want %q", got.ArtifactName, m.ArtifactName)
	case got.RemotePath != m.RemotePath:
		t.Errorf("RemotePath = %q, want %q", got.RemotePath, m.RemotePath)
	case got.SizeBytes != m.SizeBytes:
		t.Errorf("SizeBytes = %d, want %d", got.SizeBytes, m.SizeBytes)
	case got.Checksum != m.Checksum || got.ChecksumAlgorithm != m.ChecksumAlgorithm:
		t.Errorf("checksum = %s/%s, want %s/%s", got.Checksum, got.ChecksumAlgorithm, m.Checksum, m.ChecksumAlgorithm)
	case got.ValidationDetail != m.ValidationDetail:
		t.Errorf("ValidationDetail = %q, want %q", got.ValidationDetail, m.ValidationDetail)
	}
	if got.ValidationPassed == nil || *got.ValidationPassed != *m.ValidationPassed {
		t.Errorf("ValidationPassed = %v, want %v", got.ValidationPassed, m.ValidationPassed)
	}
	if got.ProducerTimestamp == nil || !got.ProducerTimestamp.Equal(*m.ProducerTimestamp) {
		t.Errorf("ProducerTimestamp = %v, want %v", got.ProducerTimestamp, m.ProducerTimestamp)
	}
	if !got.ReceivedTimestamp.Equal(m.ReceivedTimestamp) {
		t.Errorf("ReceivedTimestamp = %v, want %v", got.ReceivedTimestamp, m.ReceivedTimestamp)
	}
	if !got.RetentionTimestamp.Equal(m.RetentionTimestamp) {
		t.Errorf("RetentionTimestamp = %v, want %v", got.RetentionTimestamp, m.RetentionTimestamp)
	}
}

// TestManifestPath_UsesArtifactBasenamePlusFixedSuffix pins the layout
// that lets the writer in lifecycle/commit.go and the reader in catalog
// rebuild find the same file without either of them knowing about the
// other. The suffix is spelled out here rather than referenced through
// manifestSuffix on purpose: a test that computes the answer the same way
// the code does cannot catch the code changing it.
func TestManifestPath_UsesArtifactBasenamePlusFixedSuffix(t *testing.T) {
	got := ManifestPath("/data/backups/postgres-primary", "backup.dump")
	want := filepath.Join("/data/backups/postgres-primary", "backup.dump.manifest.json")
	if got != want {
		t.Errorf("ManifestPath = %q, want %q", got, want)
	}
}

// TestWriteManifest_RejectsInvalidManifest pins that Validate runs before
// anything reaches disk, one broken field at a time.
//
// Refusing at write time and not only at read time is the point. A
// manifest is written when everything is fine and read when nothing is, so
// a bad one that is only caught on the way back in is caught by the
// rebuild, at which point there is nothing left to reconstruct it from.
//
// "path-shaped name" is the case that is not just a missing value:
// ArtifactName goes into a filesystem path, and a name containing ".."
// would write the sidecar outside the backup set's own directory.
func TestWriteManifest_RejectsInvalidManifest(t *testing.T) {
	cases := map[string]func(*Manifest){
		"empty artifact name": func(m *Manifest) { m.ArtifactName = "" },
		"empty source":        func(m *Manifest) { m.Source = "" },
		"empty remote path":   func(m *Manifest) { m.RemotePath = "" },
		"zero retention time": func(m *Manifest) { m.RetentionTimestamp = time.Time{} },
		"zero received time":  func(m *Manifest) { m.ReceivedTimestamp = time.Time{} },
		"negative size":       func(m *Manifest) { m.SizeBytes = -1 },
		"zero format version": func(m *Manifest) { m.FormatVersion = 0 },
		"path-shaped name":    func(m *Manifest) { m.ArtifactName = "../escape.dump" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := testManifest()
			mutate(&m)
			if err := WriteManifest(t.TempDir(), m); err == nil {
				t.Fatalf("WriteManifest with %s: want an error, got nil", name)
			}
		})
	}
}

// TestReadManifest_RejectsNewerFormatVersion pins the refuse-rather-than-
// guess rule this package shares with state/migrate.go. A manifest from a
// newer binary may spell a field this one also has but means differently,
// and a rebuild that read it anyway would produce a journal row that looks
// right and is not, which is worse than a rebuild that stops and says so.
//
// It writes the JSON by hand rather than through WriteManifest, because
// WriteManifest validates and would refuse to produce the fixture.
func TestReadManifest_RejectsNewerFormatVersion(t *testing.T) {
	dir := t.TempDir()
	m := testManifest()
	m.FormatVersion = CurrentFormatVersion + 1
	path := ManifestPath(dir, m.ArtifactName)

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := ReadManifest(path); err == nil {
		t.Fatal("ReadManifest with a future format_version: want an error, got nil")
	}
}

// TestReadManifest_MissingFile pins that a single missing sidecar is an
// error for the caller that asked for that one artifact. It is the
// deliberate opposite of ScanManifests' answer for a missing DIRECTORY: a
// directory that was never created carries no claim, while a manifest
// somebody asked for by name and did not get is a fact about that
// artifact.
func TestReadManifest_MissingFile(t *testing.T) {
	if _, err := ReadManifest(filepath.Join(t.TempDir(), "nope.manifest.json")); err == nil {
		t.Fatal("ReadManifest for a missing file: want an error, got nil")
	}
}

// TestReadManifest_RejectsMalformedJSON covers the shape a crash actually
// leaves behind: a truncated file. WriteManifest writes to a temp path and
// renames precisely so this cannot happen at the real path, so this is the
// check that the guarantee is still enforced on the way in even when
// something outside this package put the bytes there.
func TestReadManifest_RejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.manifest.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := ReadManifest(path); err == nil {
		t.Fatal("ReadManifest for malformed JSON: want an error, got nil")
	}
}

// TestManifestArtifact_RebuildsIdentity pins that the identity comes back
// out of the three stored halves exactly as it went in. The manifest
// stores source, set and name separately rather than one rendered string
// so the rebuild goes back through model's own constructors and gets their
// validation; this asserts that detour does not quietly change anything.
func TestManifestArtifact_RebuildsIdentity(t *testing.T) {
	m := testManifest()
	artifact, err := m.Artifact()
	if err != nil {
		t.Fatalf("Artifact: %v", err)
	}
	if artifact.Set.Source != m.Source || artifact.Set.Set != m.BackupSet || artifact.Name != m.ArtifactName {
		t.Errorf("Artifact() = %+v, want source=%s set=%s name=%s", artifact, m.Source, m.BackupSet, m.ArtifactName)
	}
}
