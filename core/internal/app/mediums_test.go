package app

import (
	"reflect"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// The production placement.MediumResolver (issue #239, inherited from
// #238's acceptance line). #238 landed the seam with nothing but a test
// double behind it; this is the implementation, and it is the one place
// FR-31's upload_verification -> verification class mapping is written.

func mediumsFixture() []config.StorageMedium {
	return []config.StorageMedium{
		{
			ID:           "offsite_s3",
			Type:         config.StorageMediumTypeS3,
			Region:       "us-east-1",
			Endpoint:     "https://minio.example:9000",
			Bucket:       "nas-backups",
			Prefix:       "rclone-manager",
			StorageClass: config.StorageClassStandardIA,
			Credentials:  config.MediumCredentials{File: "/var/lib/backup-manager/s3/offsite.creds"},
		},
		{
			ID:                 "trusted_s3",
			Type:               config.StorageMediumTypeS3,
			Region:             "eu-west-1",
			Bucket:             "other",
			UploadVerification: config.UploadVerificationAttested,
			Credentials:        config.MediumCredentials{Env: "OFFSITE_CREDS"},
		},
	}
}

// TestMediumResolver_CarriesEveryConfiguredFieldThroughUnchanged is the
// whole-struct control this repository asks for: a resolver that dropped a
// field would still reach the right bucket in every happy-path test, and
// the artifact would land in the wrong storage class or through the wrong
// endpoint. Every field of the declaration is non-zero in the fixture on
// purpose.
func TestMediumResolver_CarriesEveryConfiguredFieldThroughUnchanged(t *testing.T) {
	r := MediumResolver(mediumsFixture())

	got, class, err := r.Resolve("offsite_s3")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := transport.Medium{
		ID:           "offsite_s3",
		Type:         transport.MediumTypeS3,
		Region:       "us-east-1",
		Endpoint:     "https://minio.example:9000",
		Bucket:       "nas-backups",
		Prefix:       "rclone-manager",
		StorageClass: config.StorageClassStandardIA,
		Credentials:  transport.MediumCredentials{File: "/var/lib/backup-manager/s3/offsite.creds"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve(offsite_s3) =\n\t%+v\nwant\n\t%+v", got, want)
	}
	if class != placement.Content {
		t.Errorf("class = %q, want %q: readback is the default and it is the read-back rung", class, placement.Content)
	}
}

// TestMediumResolver_MapsUploadVerificationOntoTheLadder is FR-31's
// mapping, stated as a table so neither direction can pass by never
// firing.
func TestMediumResolver_MapsUploadVerificationOntoTheLadder(t *testing.T) {
	for _, tc := range []struct {
		verification string
		want         placement.Class
	}{
		{"", placement.Content},
		{config.UploadVerificationReadback, placement.Content},
		{config.UploadVerificationAttested, placement.Attested},
	} {
		m := mediumsFixture()[0]
		m.UploadVerification = tc.verification
		_, class, err := MediumResolver([]config.StorageMedium{m}).Resolve(m.ID)
		if err != nil {
			t.Fatalf("Resolve(upload_verification=%q): %v", tc.verification, err)
		}
		if class != tc.want {
			t.Errorf("upload_verification %q resolved to class %q, want %q", tc.verification, class, tc.want)
		}
	}
}

// TestMediumResolver_NeverResolvesToExistence is the honesty rule stated
// as a property rather than as three separate assertions. Existence proves
// nothing about the bytes and is never sufficient to delete a source copy
// (FR-31), so no configuration may map onto it, including one this build
// does not understand.
func TestMediumResolver_NeverResolvesToExistence(t *testing.T) {
	for _, v := range append(config.UploadVerificationModes(), "", "trust_me") {
		m := mediumsFixture()[0]
		m.UploadVerification = v
		_, class, err := MediumResolver([]config.StorageMedium{m}).Resolve(m.ID)
		if err != nil {
			continue // a refusal is a fine answer; a weak class is not
		}
		if class == placement.Existence {
			t.Errorf("upload_verification %q resolved to %q, which proves nothing about the bytes and can never authorise deleting a source", v, class)
		}
	}
}

// TestMediumResolver_RefusesAVerificationModeItDoesNotUnderstand is the
// same fail-loud direction FR-13 and FR-31 both take. config.Validate
// already refuses an unknown mode, so this is reachable only through a
// hand-built config or a mode added to the schema without a rung here, and
// both of those are exactly when a silent default would be wrong.
func TestMediumResolver_RefusesAVerificationModeItDoesNotUnderstand(t *testing.T) {
	m := mediumsFixture()[0]
	m.UploadVerification = "trust_me"

	_, _, err := MediumResolver([]config.StorageMedium{m}).Resolve(m.ID)
	if err == nil {
		t.Fatal("Resolve accepted upload_verification: trust_me; an unrecognised verification mode has to fail loudly, never resolve to the cheapest rung")
	}
	if !strings.Contains(err.Error(), "trust_me") {
		t.Errorf("the refusal %q does not name the mode it did not understand", err)
	}
}

// TestMediumResolver_RefusesAnIdItDoesNotHold covers the two shapes of
// "that is not a medium": a name nothing declares, and the reserved local
// id, which is a backup set's own local_path and has no transport.Medium
// at all.
func TestMediumResolver_RefusesAnIdItDoesNotHold(t *testing.T) {
	r := MediumResolver(mediumsFixture())

	for _, tc := range []struct{ id, want string }{
		{"nowhere_s3", "nowhere_s3"},
		{config.MediumLocal, config.MediumLocal},
		{"", "medium"},
	} {
		_, _, err := r.Resolve(tc.id)
		if err == nil {
			t.Errorf("Resolve(%q) succeeded; nothing declares it", tc.id)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Resolve(%q) said %q, which does not name %q", tc.id, err, tc.want)
		}
	}
}

// TestMediumResolver_RefusesATypeItCannotReach keeps the closed backend
// set closed at the point of use. config.Validate refuses an unknown type
// too; this is the same refusal at the moment something is about to be
// reached, which is where this repository puts a check that matters.
func TestMediumResolver_RefusesATypeItCannotReach(t *testing.T) {
	m := mediumsFixture()[0]
	m.Type = "azure"

	_, _, err := MediumResolver([]config.StorageMedium{m}).Resolve(m.ID)
	if err == nil {
		t.Fatal("Resolve accepted type: azure")
	}
	if !strings.Contains(err.Error(), "azure") {
		t.Errorf("the refusal %q does not name the type", err)
	}
}

// TestMediumResolver_SatisfiesTheEngineSeam is the compile-time proof that
// this is the thing #238 left a hole for. A resolver that satisfied some
// other shape would be a fine object and useless to the move engine.
func TestMediumResolver_SatisfiesTheEngineSeam(t *testing.T) {
	var _ placement.MediumResolver = MediumResolver(nil)
}
