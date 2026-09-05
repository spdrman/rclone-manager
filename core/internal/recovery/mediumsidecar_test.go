// These cover the sidecar as it exists ON A MEDIUM rather than on a local
// filesystem: where its key goes (FR-28), that it is the same format as
// the local one rather than a second format that resembles it, and that
// nothing in it tells a reader how to reach the medium it names.
//
// The last of those is the reason this file is separate from
// manifest_test.go. A local sidecar sits in the operator's own backup
// root; an object sidecar sits inside a bucket, where the audience is
// everybody who can read the bucket. That is a different threat model
// applied to the same struct, so it gets its own tests rather than a
// couple of extra cases hidden among the round-trip ones.
package recovery

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestManifestObjectKeyMirrorsTheArtifactKey pins FR-28's sidecar layout,
// <prefix>/<source>/<set>/.manifest/<artifact-name>.json, and pins that it
// is DERIVED from the artifact's own key rather than composed again from
// the same parts.
func TestManifestObjectKeyMirrorsTheArtifactKey(t *testing.T) {
	for _, tc := range []struct {
		name        string
		artifactKey string
		want        string
	}{
		{
			name:        "with a prefix",
			artifactKey: "rclone-manager/production/postgres-primary/2026-09-01.dump",
			want:        "rclone-manager/production/postgres-primary/.manifest/2026-09-01.dump.json",
		},
		{
			name:        "a multi-segment prefix",
			artifactKey: "backups/nas/production/pg/a.dump",
			want:        "backups/nas/production/pg/.manifest/a.dump.json",
		},
		{
			name:        "no prefix at all",
			artifactKey: "production/pg/a.dump",
			want:        "production/pg/.manifest/a.dump.json",
		},
		{
			name:        "a bare name at the bucket root",
			artifactKey: "a.dump",
			want:        ".manifest/a.dump.json",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ManifestObjectKey(tc.artifactKey)
			if err != nil {
				t.Fatalf("ManifestObjectKey(%q): %v", tc.artifactKey, err)
			}
			if got != tc.want {
				t.Errorf("ManifestObjectKey(%q) = %q, want %q", tc.artifactKey, got, tc.want)
			}
			// The sidecar lives beside the artifact, never above or
			// beyond it: everything before the artifact's own name has to
			// be a prefix of the sidecar's key too. Asserted separately
			// from the exact string above, because that string is what
			// somebody would edit to "fix" a future failure, and this is
			// the property that must survive the edit.
			artifactDir := ""
			if i := strings.LastIndex(tc.artifactKey, "/"); i >= 0 {
				artifactDir = tc.artifactKey[:i]
			}
			if artifactDir != "" && !strings.HasPrefix(got, artifactDir+"/") {
				t.Errorf("the sidecar key %q does not sit under the artifact's own directory %q", got, artifactDir)
			}
		})
	}
}

// TestManifestObjectKeyRefusesWhatItCannotDeriveFrom pins that a key it
// cannot derive from produces a refusal and an EMPTY string, never a
// best-effort key.
//
// The inputs are the shapes that would otherwise silently produce a
// sidecar somewhere wrong: a directory-shaped key, "." and ".." as the
// final segment, and the bucket root itself. Returning "" alongside the
// error is asserted explicitly because a caller that ignores the error and
// uses the key is a bug that wants to fail immediately rather than write
// an object at a plausible-looking wrong place.
func TestManifestObjectKeyRefusesWhatItCannotDeriveFrom(t *testing.T) {
	for _, artifactKey := range []string{"", "production/pg/", "production/pg/.", "production/pg/..", "/"} {
		t.Run(artifactKey, func(t *testing.T) {
			got, err := ManifestObjectKey(artifactKey)
			if err == nil {
				t.Fatalf("ManifestObjectKey(%q) = %q, want a refusal", artifactKey, got)
			}
			if got != "" {
				t.Errorf("ManifestObjectKey returned %q alongside its refusal; a refused key must be empty so a caller cannot use it by accident", got)
			}
		})
	}
}

// TestEncodeDecodeManifestRoundTrips proves the object sidecar and the
// local one are the same format rather than two that happen to look alike:
// the same bytes go out and come back, placements included.
func TestEncodeDecodeManifestRoundTrips(t *testing.T) {
	m := testManifest()
	size := int64(1024)
	m.Placements = []ManifestPlacement{
		{Medium: "local", Location: "/backups/pg/backup-2026-08-20.dump", SizeBytes: &size,
			Checksum: "deadbeef", ChecksumAlgorithm: "sha256", VerificationClass: "content", Status: "ACTIVE"},
		{Medium: "offsite_s3", Location: "rclone-manager/production/postgres-primary/backup-2026-08-20.dump",
			SizeBytes: &size, Checksum: "deadbeef", ChecksumAlgorithm: "sha256",
			VerificationClass: "existence", Status: "ACTIVE"},
	}

	data, err := EncodeManifest(m)
	if err != nil {
		t.Fatalf("EncodeManifest: %v", err)
	}
	got, err := DecodeManifest(data)
	if err != nil {
		t.Fatalf("DecodeManifest: %v", err)
	}
	if !reflect.DeepEqual(got.Placements, m.Placements) {
		t.Errorf("placements did not round-trip:\n got %+v\nwant %+v", got.Placements, m.Placements)
	}
}

// TestManifestPlacementCarriesNoWayToReachTheMedium is FR-33's rule
// applied to the one recovery artifact that is written INTO a bucket: a
// sidecar object is readable by everyone who can read the bucket, so it
// names the medium by its configured id and the object by its key, and
// nothing else.
func TestManifestPlacementCarriesNoWayToReachTheMedium(t *testing.T) {
	typ := reflect.TypeOf(ManifestPlacement{})
	forbidden := []string{"endpoint", "bucket", "region", "key_id", "accesskey", "secret", "credential", "token", "password", "profile", "url", "host"}
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		for _, f := range forbidden {
			if strings.Contains(name, f) {
				t.Errorf("ManifestPlacement field %q could carry a way to reach the medium (matches %q); a sidecar object names the medium by its configured id and nothing more", typ.Field(i).Name, f)
			}
		}
	}

	// And the serialized form, because a field could carry one without its
	// NAME saying so.
	size := int64(1)
	data, err := json.Marshal(ManifestPlacement{
		Medium: "offsite_s3", Location: "p/production/pg/a.dump", SizeBytes: &size, Status: "ACTIVE",
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, f := range forbidden {
		if strings.Contains(strings.ToLower(string(data)), f) {
			t.Errorf("a serialized ManifestPlacement contains %q: %s", f, data)
		}
	}
}
