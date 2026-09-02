package transport_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// TestMediumStoreOffersNoMoveMethod is the FR-3/FR-30 decision, asserted
// the way artifactstore's TestSeamOffersNoMoveMethod asserts the same one
// about the local seam: a move is upload, verify and an explicit delete,
// composed in one auditable place by the move engine (#238), never a
// transport primitive that lets each implementation pick its own ordering.
//
// It matches on the method NAME rather than on a signature, for the reason
// that test records: an earlier version of it pinned one exact signature
// and an artifact-addressed Move went straight past it.
func TestMediumStoreOffersNoMoveMethod(t *testing.T) {
	typ := reflect.TypeOf((*transport.MediumStore)(nil)).Elem()
	if typ.NumMethod() == 0 {
		t.Fatalf("transport.MediumStore reports no methods at all, so this gate would pass vacuously")
	}
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		switch m.Name {
		case "Move", "Rename", "Migrate":
			t.Errorf("transport.MediumStore gained a %s method (%s); a migration is upload, verify and an explicit delete, composed by the move engine", m.Name, m.Type)
		}
	}
}

// TestMediumStoreNamesNoRcloneType is FR-3's other half: the boundary is
// manager-owned, so no rclone type may appear in any signature on it. A
// leak here is how lifecycle code ends up depending on an upstream API
// this project pins and upgrades on its own schedule.
func TestMediumStoreNamesNoRcloneType(t *testing.T) {
	typ := reflect.TypeOf((*transport.MediumStore)(nil)).Elem()
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		for _, named := range append(inTypes(m.Type), outTypes(m.Type)...) {
			if strings.Contains(named, "rclone") {
				t.Errorf("transport.MediumStore.%s names %q, which is an rclone type crossing the boundary", m.Name, named)
			}
		}
	}
}

func inTypes(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumIn(); i++ {
		out = append(out, t.In(i).String())
	}
	return out
}

func outTypes(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumOut(); i++ {
		out = append(out, t.Out(i).String())
	}
	return out
}

// TestMediumCredentialsNameOnlyWhereASecretComesFrom is the transport-side
// half of config.MediumCredentials' "there is no field for a literal key,
// and that is the enforcement". config refuses an inline secret because
// Load decodes with KnownFields(true); this type has no decoder to lean
// on, so the guard is that the three fields are exactly the three
// REFERENCES, and a fourth one holding a value would fail here.
func TestMediumCredentialsNameOnlyWhereASecretComesFrom(t *testing.T) {
	typ := reflect.TypeOf(transport.MediumCredentials{})
	want := map[string]bool{"File": true, "Env": true, "Command": true}
	if typ.NumField() != len(want) {
		t.Errorf("transport.MediumCredentials has %d fields, want exactly %d (file, env, command)", typ.NumField(), len(want))
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !want[name] {
			t.Errorf("transport.MediumCredentials gained field %q; the three sources name where a secret comes FROM, and a fourth is how a literal one gets held", name)
		}
	}
}

// TestMediumRendersNoCredentialValue proves the descriptor cannot leak,
// because it holds nothing to leak. Every field is filled with the canary,
// including the credential references, and every rendering a careless
// caller might reach for is checked: the descriptor is meant to be safe to
// log, and this is what makes that a fact rather than a hope.
//
// The references themselves ARE echoed here (a path and a variable name
// are not the secret), so the canary is placed only where a VALUE would
// have to live if the struct ever grew a field for one.
func TestMediumRendersNoCredentialValue(t *testing.T) {
	typ := reflect.TypeOf(transport.Medium{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		lower := strings.ToLower(f.Name)
		for _, banned := range []string{"accesskey", "secretkey", "secretaccess", "password", "token", "keyid"} {
			if strings.Contains(lower, banned) {
				t.Errorf("transport.Medium.%s looks like a field a literal credential fits into; credentials are named by reference, never carried", f.Name)
			}
		}
	}
}

// TestMediumKeyLayout pins FR-28's deterministic key layout,
// <prefix>/<source>/<set>/<artifact-name>, joined with "/". No timestamp
// and no random component, which is what makes re-running an interrupted
// upload target the same key rather than a second copy.
func TestMediumKeyLayout(t *testing.T) {
	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, "2026-09-01.dump")
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}

	for _, tc := range []struct {
		name   string
		prefix string
		want   string
	}{
		{"with a prefix", "backups/nas", "backups/nas/production/postgres-primary/2026-09-01.dump"},
		{"no prefix puts the layout at the bucket root", "", "production/postgres-primary/2026-09-01.dump"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := transport.MediumKey(tc.prefix, artifact)
			if err != nil {
				t.Fatalf("MediumKey: %v", err)
			}
			if got != tc.want {
				t.Errorf("MediumKey(%q) = %q, want %q", tc.prefix, got, tc.want)
			}
		})
	}
}

// TestMediumKeyRefusesWhatItCannotAddress is the negative half. A key is
// not only ever a key: a restore writes it to a local path derived from
// it, so an artifact id this store cannot address must be refused rather
// than joined into a key with an empty or traversing segment.
func TestMediumKeyRefusesWhatItCannotAddress(t *testing.T) {
	good, err := model.NewBackupSetID("production", "pg")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	for _, tc := range []struct {
		name     string
		prefix   string
		artifact model.ArtifactID
	}{
		{"a zero artifact id", "", model.ArtifactID{}},
		{"an artifact with no name", "", model.ArtifactID{Set: good}},
		{"an artifact whose name traverses", "", model.ArtifactID{Set: good, Name: "../escape"}},
		{"an artifact with no set", "", model.ArtifactID{Name: "a.dump"}},
		{"a prefix with an empty segment", "a//b", model.ArtifactID{Set: good, Name: "a.dump"}},
		{"a prefix that traverses", "a/../b", model.ArtifactID{Set: good, Name: "a.dump"}},
		{"a prefix with a leading slash", "/a", model.ArtifactID{Set: good, Name: "a.dump"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := transport.MediumKey(tc.prefix, tc.artifact)
			if err == nil {
				t.Fatalf("MediumKey(%q, %+v) returned %q, want a refusal", tc.prefix, tc.artifact, got)
			}
			if got != "" {
				t.Errorf("MediumKey returned key %q alongside its refusal; a refused key must be empty so a caller cannot use it by accident", got)
			}
		})
	}
}

// TestConfigurationCategoryIsNotRetryable adds FR-28's fourth S3 verdict
// to the FR-22 vocabulary. NoSuchBucket and a failure to resolve the
// configured endpoint are facts about the configuration, and retrying
// either just spends the backoff budget on a request that cannot start
// succeeding until a person edits the config.
func TestConfigurationCategoryIsNotRetryable(t *testing.T) {
	if got := transport.Configuration.String(); got != "configuration" {
		t.Errorf("transport.Configuration.String() = %q, want %q", got, "configuration")
	}
	if transport.Configuration.Retryable() {
		t.Error("transport.Configuration reports itself retryable; a bucket that does not exist does not start existing on the second attempt")
	}
	// The category has to be distinct from every other one, or a switch on
	// it silently takes another branch.
	seen := map[transport.Category]string{}
	for c := transport.Category(0); c < 32; c++ {
		name := c.String()
		if strings.HasPrefix(name, "Category(") {
			continue
		}
		if other, dup := seen[c]; dup {
			t.Errorf("category %d renders as both %q and %q", c, other, name)
		}
		seen[c] = name
	}
	if len(seen) < 12 {
		t.Errorf("only %d categories have names; the vocabulary lost one", len(seen))
	}
}

// TestObjectInfoCarriesNoETag is FR-32 made structural. An ETag is never a
// content hash (multipart and encrypted objects make it not one), so the
// safest place to keep it out of a comparison is out of the type: a field
// nobody can read is a field nobody can compare to a recorded hash.
func TestObjectInfoCarriesNoETag(t *testing.T) {
	typ := reflect.TypeOf(transport.ObjectInfo{})
	for i := 0; i < typ.NumField(); i++ {
		lower := strings.ToLower(typ.Field(i).Name)
		for _, banned := range []string{"etag", "hash", "checksum", "md5"} {
			if strings.Contains(lower, banned) {
				t.Errorf("transport.ObjectInfo.%s exposes a medium-reported digest; FR-32 says an ETag is never a content hash, and ObjectChecksum is where an attestation is asked for on purpose", typ.Field(i).Name)
			}
		}
	}
}

// TestMediumRendersUnderEveryVerb is a smoke test for the claim that a
// descriptor is safe to hand to a log line: it must not panic under any
// of the renderings a debugging operator reaches for.
func TestMediumRendersUnderEveryVerb(t *testing.T) {
	m := transport.Medium{
		ID: "cold", Type: transport.MediumTypeS3, Region: "us-east-1",
		Endpoint: "https://minio.example:9000", Bucket: "nas-backups",
		Prefix: "rclone-manager", StorageClass: "STANDARD",
		Credentials: transport.MediumCredentials{File: "/var/lib/backup-manager/s3.creds"},
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := fmt.Sprintf(verb, m); out == "" {
			t.Errorf("rendering a Medium with %s produced nothing", verb)
		}
	}
	if _, err := json.Marshal(m); err != nil {
		t.Errorf("json.Marshal(Medium): %v", err)
	}
}

// TestMediumStoreSurfaceIsExactlyThis pins the method set, not just the
// absence of Move. An interface that is only ever guarded negatively grows
// by whatever nobody thought to ban, and every method here costs a second
// implementation the work of implementing it honestly or the temptation to
// stub it, which artifactstore's own doc calls out as worse than absence.
//
// Restore is deliberately not here. FR-28 sketches RestoreStatus and
// InitiateRestore on this interface; they belong to #241, which owns the
// archive storage classes that make them mean anything, and MinIO cannot
// exercise a Glacier restore, so landing them now would land two methods
// with no implementation and no test behind them.
func TestMediumStoreSurfaceIsExactlyThis(t *testing.T) {
	typ := reflect.TypeOf((*transport.MediumStore)(nil)).Elem()
	want := map[string]bool{
		"StatObject":      true,
		"UploadFromLocal": true,
		"OpenObject":      true,
		"ObjectChecksum":  true,
		"DeleteObject":    true,
		"ListObjects":     true,
	}
	got := map[string]bool{}
	for i := 0; i < typ.NumMethod(); i++ {
		got[typ.Method(i).Name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("transport.MediumStore is missing %s", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("transport.MediumStore gained %s; every method here is one a second implementation has to discharge honestly, so adding one is a decision, not a convenience", name)
		}
	}
}

// TestMediumTypeIsAClosedSet mirrors config.StorageMediumTypeS3's own
// closed set on this side of the boundary. A type this package accepted
// that config cannot spell would be a medium nothing can configure; a type
// config can spell that this package does not know would be a config that
// validates and cannot be served.
func TestMediumTypeIsAClosedSet(t *testing.T) {
	if got := string(transport.MediumTypeS3); got != "s3" {
		t.Errorf("transport.MediumTypeS3 = %q, want %q, which is what config.StorageMediumTypeS3 spells", got, "s3")
	}
}
