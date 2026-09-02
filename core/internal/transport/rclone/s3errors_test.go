package rclone

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// fakeAPIError is a stand-in for what the AWS SDK returns: an error
// carrying a service error CODE. It is a local type rather than the real
// smithy one because this repository does not import a provider SDK (see
// backends_test.go's TestNoCloudProviderSDKIsImportedDirectly), and because
// Go interfaces are structural, so a value with the right method is matched
// by errors.As whoever wrote it.
//
// This makes the unit tests below evidence about the TABLE and about the
// ordering inside Classify, and deliberately not evidence that the real SDK
// errors satisfy the interface. That second claim needs a real endpoint and
// is proved in the MinIO integration suite, against real NoSuchBucket,
// NoSuchKey and AccessDenied responses.
type fakeAPIError struct {
	code string
	msg  string
}

func (e fakeAPIError) Error() string     { return fmt.Sprintf("api error %s: %s", e.code, e.msg) }
func (e fakeAPIError) ErrorCode() string { return e.code }

func TestClassify_S3ErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		code string
		want transport.Category
		why  string
	}{
		{"SlowDown", transport.Transient, "throttling is the case bounded backoff exists for"},
		{"InternalError", transport.Transient, "a 5xx from the service is worth another attempt"},
		{"ServiceUnavailable", transport.Transient, "same"},
		{"NoSuchBucket", transport.Configuration, "a bucket that does not exist does not start existing on the second attempt"},
		{"PermanentRedirect", transport.Configuration, "what a wrong region looks like from the outside"},
		{"AccessDenied", transport.Authentication, "FR-28 puts denial with the credentials, because that is what an operator has to go and fix"},
		{"SignatureDoesNotMatch", transport.Authentication, "a signature failure is a credential failure"},
		{"InvalidAccessKeyId", transport.Authentication, "same"},
		{"NoSuchKey", transport.NotFound, "one object, absent"},
		{"NotFound", transport.NotFound, "what a HEAD returns for an absent key"},
		{"BadDigest", transport.IntegrityFailure, "retrying would re-upload the same corrupt payload"},
		{"InvalidObjectState", transport.UnsupportedCapability, "an archive object needs a restore first, and that is #241"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			if got := Classify(fakeAPIError{code: tc.code, msg: "the service said so"}); got != tc.want {
				t.Errorf("Classify(%s) = %s, want %s: %s", tc.code, got, tc.want, tc.why)
			}
		})
	}
}

// TestClassify_S3CodeWinsOverTheWrappingSentinel is the ordering claim. A
// missing BUCKET and a missing KEY are the same HTTP status and completely
// different problems, and the only thing that separates them is the code,
// so the code has to be consulted before anything that has already flattened
// them together.
func TestClassify_S3CodeWinsOverTheWrappingSentinel(t *testing.T) {
	wrapped := fmt.Errorf("listing: %w", fakeAPIError{code: "NoSuchBucket", msg: "The specified bucket does not exist"})
	if got := Classify(wrapped); got != transport.Configuration {
		t.Errorf("Classify(wrapped NoSuchBucket) = %s, want %s", got, transport.Configuration)
	}

	// And an unrecognised code does not swallow the rest of Classify: it
	// falls through to whatever the chain says next.
	fellThrough := fmt.Errorf("%w: %w", fakeAPIError{code: "SomeCodeNobodyTabulated", msg: "?"}, errors.New("plain"))
	if got := Classify(fellThrough); got != transport.Permanent {
		t.Errorf("Classify(unknown code) = %s, want %s; an unrecognised code must not be treated as safe to retry or safe to ignore", got, transport.Permanent)
	}
}

// TestClassify_UnresolvableEndpointIsConfiguration covers FR-28's other
// Configuration case. It has to beat fserrors.ShouldRetry, which treats a
// DNS failure as a network condition: NXDOMAIN is not weather, it is a
// hostname that does not exist.
func TestClassify_UnresolvableEndpointIsConfiguration(t *testing.T) {
	nxdomain := &net.DNSError{Err: "no such host", Name: "s3.typo.invalid", IsNotFound: true}
	if got := Classify(fmt.Errorf("dialing: %w", nxdomain)); got != transport.Configuration {
		t.Errorf("Classify(NXDOMAIN) = %s, want %s", got, transport.Configuration)
	}

	// The bound: a resolver that is temporarily unhappy is a genuine
	// transient, and is left to rclone's own judgement rather than
	// relabelled here.
	temporary := &net.DNSError{Err: "server misbehaving", Name: "s3.example.com", IsTemporary: true}
	if got := Classify(fmt.Errorf("dialing: %w", temporary)); got == transport.Configuration {
		t.Error("Classify(temporary DNS failure) = configuration; a resolver hiccup is not a typo in the config")
	}
}

// TestClassify_AlreadyPresentIsConflict pins the sentinel UploadFromLocal
// returns when it refuses to overwrite. Conflict rather than Permanent
// because it is the resumable case: the caller's next move is to confirm
// and continue, not to give up.
func TestClassify_AlreadyPresentIsConflict(t *testing.T) {
	err := fmt.Errorf("%w: production/pg/2026-09-01.dump", ErrObjectAlreadyPresent)
	if got := Classify(err); got != transport.Conflict {
		t.Errorf("Classify(ErrObjectAlreadyPresent) = %s, want %s", got, transport.Conflict)
	}
}

// TestClassify_S3CodesDoNotDisturbTheSFTPPath is the regression half. The
// sftp classification is untouched by this change, and the way to say so
// with a test rather than with a sentence is to re-run the categories that
// have nothing to do with S3 through the same function.
func TestClassify_S3CodesDoNotDisturbTheSFTPPath(t *testing.T) {
	if got := Classify(errors.New("ssh: unable to authenticate, attempted methods [none publickey]")); got != transport.Authentication {
		t.Errorf("Classify(ssh auth failure) = %s, want %s", got, transport.Authentication)
	}
	if got := Classify(fmt.Errorf("%w: backend %q cannot compute sha256", ErrUnsupportedHash, "sftp")); got != transport.UnsupportedCapability {
		t.Errorf("Classify(unsupported hash) = %s, want %s", got, transport.UnsupportedCapability)
	}
	if got := Classify(nil); got != transport.Unclassified {
		t.Errorf("Classify(nil) = %s, want %s", got, transport.Unclassified)
	}
}
