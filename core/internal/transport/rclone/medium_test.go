package rclone_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
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
		// A wrong REGION arrives as one of these, and not one of them
		// reads anything like "your region is wrong", which is exactly
		// why they are worth classifying instead of leaving at Permanent.
		{"PermanentRedirect", transport.Configuration},
		{"AuthorizationHeaderMalformed", transport.Configuration},
		{"IllegalLocationConstraintException", transport.Configuration},
		// The bytes arrived and did not match what was declared. This is
		// the one family where a retry is actively wrong: it would send
		// the same corrupt payload again.
		{"BadDigest", transport.IntegrityFailure},
		{"XAmzContentSHA256Mismatch", transport.IntegrityFailure},
		// An object in an archive class that has to be restored first
		// (FR-34). Nothing is wrong and no retry changes it, which is
		// what #241 will turn into an explicit restore.
		{"InvalidObjectState", transport.UnsupportedCapability},
		// A multipart upload id that expired or was aborted: the same
		// fact about a different handle.
		{"NoSuchUpload", transport.NotFound},
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

// TestEveryMediumOperationReleasesItsFs is #264's discipline held for the
// MediumStore half: every operation builds its own Fs, and an Fs nobody
// releases is a resource that grows with the number of operations a cycle
// performs and is bounded by nothing.
//
// It is a source scan rather than a live leak test, deliberately. The
// live proof for sftp works because a connection pool is observable from
// outside the process; an s3 Fs holds an HTTP client and a pacer, which
// are not, so a behavioural test here would be asserting something it
// cannot see. What CAN be checked, and is exactly the thing a future
// method would get wrong, is whether each operation released what it
// built.
func TestEveryMediumOperationReleasesItsFs(t *testing.T) {
	source, err := os.ReadFile("medium.go")
	if err != nil {
		t.Fatalf("reading medium.go: %v", err)
	}
	text := string(source)

	// OpenObject is the one method that cannot release on the way out:
	// the reader it returns is still reading through the Fs, so the
	// release rides on that reader's own Close instead.
	byReader := map[string]bool{"OpenObject": true}

	for _, method := range []string{"StatObject", "UploadFromLocal", "OpenObject", "ObjectChecksum", "DeleteObject", "ListObjects"} {
		t.Run(method, func(t *testing.T) {
			start := strings.Index(text, "func (a *Adapter) "+method+"(")
			if start < 0 {
				t.Fatalf("medium.go declares no %s; if it moved, move this check with it rather than dropping the method from the scan", method)
			}
			end := strings.Index(text[start:], "\n}\n")
			if end < 0 {
				t.Fatalf("could not find the end of %s", method)
			}
			body := text[start : start+end]

			if !strings.Contains(body, "a.mediumFs(ctx, ") && !strings.Contains(body, "a.mediumFs(ctx, dstMedium)") {
				t.Skipf("%s builds no Fs of its own", method)
			}
			switch {
			case byReader[method]:
				if !strings.Contains(body, "fsBoundReadCloser") {
					t.Errorf("%s builds an Fs and hands back a reader without binding the Fs's release to that reader's Close", method)
				}
			case !strings.Contains(body, "defer shutdownFs("):
				t.Errorf("%s builds an Fs and never releases it; every operation in this file releases what it built (#264)", method)
			}
		})
	}
}

// TestTheFsReleaseScanCanActuallyFail is the positive control: the scan
// above is an absence-shaped check over hand-written method names, which
// passes silently if the names stop matching.
func TestTheFsReleaseScanCanActuallyFail(t *testing.T) {
	source, err := os.ReadFile("medium.go")
	if err != nil {
		t.Fatalf("reading medium.go: %v", err)
	}
	if !strings.Contains(string(source), "defer shutdownFs(") {
		t.Fatal("medium.go contains no deferred release at all, so the scan above cannot be distinguishing anything")
	}
	if strings.Count(string(source), "func (a *Adapter) ") < 6 {
		t.Fatal("medium.go declares fewer than six adapter methods, so the scan's method list no longer matches the file")
	}
}
