package rclone_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/contract"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// localMediumFixtures runs the MediumStore contract suite against rclone's
// local backend, in-tree, with no container and no network.
//
// It is the same discipline the Transport contract suite already uses: run
// the whole contract against the backend that is always available first,
// so a real backend's integration run is checking the BACKEND rather than
// discovering the suite's own bugs. The MinIO run in
// core/tests/miniointegration is the other half, and it is the one that
// says anything about S3.
type localMediumFixtures struct{}

func (localMediumFixtures) NewMedium(t *testing.T) transport.Medium {
	t.Helper()
	return transport.Medium{
		ID:     "contract_local",
		Type:   transport.MediumTypeLocalDir,
		Bucket: t.TempDir(),
	}
}

// AttestsSHA256 is true here, and it is the only place in this repository
// where it is. rclone's local backend can hash a file it can read, so this
// is the run that exercises ObjectChecksum's SUCCESS path; the s3 backend
// (v1.75.0) exposes MD5 and nothing else, so the MinIO run exercises the
// refusal. Neither branch of FR-31's rule is left untested.
func (localMediumFixtures) AttestsSHA256() bool { return true }

func TestRcloneAdapter_LocalBackend_MediumContractSuite(t *testing.T) {
	contract.RunMedium(t, rclone.New(), localMediumFixtures{})
}

// TestMediumFsRefusesWhatItCannotServe covers the three ways a Medium can
// be unserveable before any backend is reached. Each one is a
// Configuration verdict, because each one is a line somebody has to edit.
func TestMediumFsRefusesWhatItCannotServe(t *testing.T) {
	ctx := context.Background()
	adapter := rclone.New()

	for _, tc := range []struct {
		name   string
		medium transport.Medium
	}{
		{"no id", transport.Medium{Type: transport.MediumTypeLocalDir, Bucket: t.TempDir()}},
		{"no bucket", transport.Medium{ID: "m", Type: transport.MediumTypeLocalDir}},
		{"a type this adapter does not implement", transport.Medium{ID: "m", Type: transport.MediumType("azure"), Bucket: t.TempDir()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := adapter.StatObject(ctx, tc.medium, "some/key")
			if err == nil {
				t.Fatal("StatObject accepted an unserveable medium")
			}
			if category, ok := transport.CategoryOf(err); !ok || category != transport.Configuration {
				t.Errorf("classified as %v (recognised=%v), want %s", category, ok, transport.Configuration)
			}
		})
	}
}

// TestUploadRefusesAnEmptyKey is the smallest destructive-adjacent guard
// this file has: an upload with no key would put an object at the bucket
// root under whatever name the backend invented, which is an object no
// placement record could ever name again.
func TestUploadRefusesAnEmptyKey(t *testing.T) {
	adapter := rclone.New()
	medium := transport.Medium{ID: "m", Type: transport.MediumTypeLocalDir, Bucket: t.TempDir()}
	local := filepath.Join(t.TempDir(), "artifact.dump")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing the source file: %v", err)
	}

	_, err := adapter.UploadFromLocal(context.Background(), medium, local, "", transport.UploadOptions{})
	if err == nil {
		t.Fatal("UploadFromLocal accepted an empty key")
	}
	if category, ok := transport.CategoryOf(err); !ok || category != transport.Configuration {
		t.Errorf("classified as %v (recognised=%v), want %s", category, ok, transport.Configuration)
	}
}

// s3APIError stands in for an error from the AWS SDK under rclone's s3
// backend. It reproduces the one thing Classify actually matches on, the
// ErrorCode() string method every S3 API error carries, which is how that
// classification manages to work without this repository importing an AWS
// SDK of its own (see apiErrorCode's doc in errors.go).
//
// A hand-made error can only prove the TABLE, never that a real endpoint
// produces these codes in these situations. That second half is proved for
// real against MinIO in core/tests/miniointegration, which drives an
// actual AccessDenied and an actual NoSuchBucket through this same
// Classify call.
type s3APIError struct {
	code string
}

func (e s3APIError) Error() string     { return "api error " + e.code + ": something the endpoint said" }
func (e s3APIError) ErrorCode() string { return e.code }

func TestClassifyS3APIErrors(t *testing.T) {
	for _, tc := range []struct {
		code string
		want transport.Category
	}{
		{"NoSuchBucket", transport.Configuration},
		{"InvalidBucketName", transport.Configuration},
		{"AccessDenied", transport.Authentication},
		{"InvalidAccessKeyId", transport.Authentication},
		{"SignatureDoesNotMatch", transport.Authentication},
		{"NoSuchKey", transport.NotFound},
		{"NotFound", transport.NotFound},
		{"SlowDown", transport.Transient},
		{"ServiceUnavailable", transport.Transient},
		{"InternalError", transport.Transient},
	} {
		t.Run(tc.code, func(t *testing.T) {
			// Wrapped, not bare, because that is how it arrives: rclone
			// and the SDK both wrap on the way out, and a classifier that
			// only worked on a top-level error would work in this test
			// and nowhere else.
			err := errors.Join(errors.New("operation error S3: HeadObject"), s3APIError{code: tc.code})
			if got := rclone.Classify(err); got != tc.want {
				t.Errorf("Classify(%s) = %s, want %s", tc.code, got, tc.want)
			}
		})
	}
}

// TestClassifyLeavesAnUnknownS3CodeAlone is the negative half of the table
// above, and the more important one. A code nobody has classified must
// fall through to the generic checks and end at Permanent, never at
// Transient (which would spend a retry budget on something that cannot
// succeed) and never at NotFound (which would let a caller treat a
// mystery as an absence).
func TestClassifyLeavesAnUnknownS3CodeAlone(t *testing.T) {
	got := rclone.Classify(s3APIError{code: "SomeCodeInventedNextYear"})
	if got != transport.Permanent {
		t.Errorf("Classify(an unlisted S3 code) = %s, want %s", got, transport.Permanent)
	}
}

// TestClassifyTreatsAnUnresolvableEndpointAsConfiguration is FR-28's
// "endpoint resolution failures are Configuration". Only the NXDOMAIN
// shape: a lookup that failed for any other reason is a blip, and calling
// a blip a configuration error would stop a retry that would have worked.
func TestClassifyTreatsAnUnresolvableEndpointAsConfiguration(t *testing.T) {
	nxdomain := &net.DNSError{Err: "no such host", Name: "s3.example.invalid", IsNotFound: true}
	if got := rclone.Classify(nxdomain); got != transport.Configuration {
		t.Errorf("Classify(NXDOMAIN) = %s, want %s", got, transport.Configuration)
	}

	timeout := &net.DNSError{Err: "i/o timeout", Name: "s3.example.com", IsTimeout: true}
	if got := rclone.Classify(timeout); got == transport.Configuration {
		t.Error("Classify(a DNS timeout) = configuration; a lookup that timed out may well succeed on the next attempt")
	}
}
