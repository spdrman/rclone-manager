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

// This file is the always-available half of the MediumStore evidence: the
// full contract suite against rclone's local backend, plus the refusals
// that happen before any backend is reached at all.
//
// Running the contract against the easy backend first is the same
// discipline the Transport suite uses, and the reason is that an
// integration run which is also the suite's first run cannot tell "S3 is
// wrong" from "the suite is wrong". By the time core/tests/miniointegration
// executes, every assertion here has already passed against something, so
// a failure there is about the endpoint.
//
// What the local backend can and cannot stand in for is the thing to keep
// straight. It CAN stand in for the boundary's own logic: key handling,
// the not-found and bucket-absent distinction, delete semantics, listing
// order. It CANNOT stand in for anything about S3's wire behaviour, which
// is why the API-error table below classifies fabricated error codes
// (against a locally declared ErrorCode interface, exactly as errors.go
// matches them) rather than pretending a directory can produce a
// NoSuchBucket.
//
// AttestsSHA256 is true here and false everywhere else, which is what
// makes this the one run that exercises ObjectChecksum's success path.

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

// NewMedium hands back a medium rooted at a fresh t.TempDir, so the
// isolation MediumFixtures asks for comes from the testing package's own
// cleanup rather than from this fixture remembering to tear anything down.
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

// TestClassifyS3APIErrors walks the whole s3Categories table, and the row
// worth understanding before editing it is Forbidden.
//
// Forbidden and Unauthorized are not S3 codes at all; they are what the
// SDK synthesises from an HTTP status when the response has no body to
// read a code out of. A HEAD is exactly that response, and a HEAD is how
// this adapter stats an object, so those are the codes a wrong credential
// actually produces on the commonest call. An earlier version of the table
// was written from S3's documentation alone and did not have them, which
// is why every entry here is a claim about what an endpoint really emits
// rather than about what the docs list.
//
// The error is built with errors.Join around a message resembling the
// SDK's own, because apiErrorCode finds its interface anywhere in the
// chain and a test that put the coder at the top would not exercise that.
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
			if got := rclone.ClassifyCtx(context.Background(), err); got != tc.want {
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
	got := rclone.ClassifyCtx(context.Background(), s3APIError{code: "SomeCodeInventedNextYear"})
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
	if got := rclone.ClassifyCtx(context.Background(), nxdomain); got != transport.Configuration {
		t.Errorf("Classify(NXDOMAIN) = %s, want %s", got, transport.Configuration)
	}

	timeout := &net.DNSError{Err: "i/o timeout", Name: "s3.example.com", IsTimeout: true}
	if got := rclone.ClassifyCtx(context.Background(), timeout); got == transport.Configuration {
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

// TestEveryMediumOperationBoundsItsRetries is the guard on the other half
// of #264's per-operation discipline: an operation that does not bound
// rclone's low-level retries costs almost four minutes of silence against
// an endpoint that is not there.
//
// It has to be checked as "mediumContext BEFORE mediumFs", not merely
// "mediumContext somewhere", because the s3 backend reads LowLevelRetries
// exactly once, in s3Connection, which NewFs calls at construction
// (backend/s3/s3.go:1589 and :1869). A bound applied to the context the
// operation RUNS under, after the Fs was built with a different one, is a
// bound that does nothing at all and looks correct in review.
func TestEveryMediumOperationBoundsItsRetries(t *testing.T) {
	source, err := os.ReadFile("medium.go")
	if err != nil {
		t.Fatalf("reading medium.go: %v", err)
	}
	text := string(source)

	for _, method := range []string{"StatObject", "UploadFromLocal", "OpenObject", "ObjectChecksum", "DeleteObject", "ListObjects"} {
		t.Run(method, func(t *testing.T) {
			body := methodBody(t, text, method)
			bound := strings.Index(body, "ctx = mediumContext(ctx)")
			if bound < 0 {
				t.Fatalf("%s never bounds rclone's low-level retries; against an endpoint that is not there this costs "+
					"3m47s per call instead of 2.4s", method)
			}
			built := strings.Index(body, "a.mediumFs(ctx")
			if built < 0 {
				return // builds no Fs of its own
			}
			if bound > built {
				t.Errorf("%s bounds retries AFTER building its Fs. The s3 backend reads LowLevelRetries once, in "+
					"s3Connection, which NewFs calls: a bound applied later does nothing and looks right", method)
			}
		})
	}
}

// TestTheRetryBoundScanCanActuallyFail is the scan's positive control. A
// source scan that matches something no method has is a check that passes
// on an empty file, and three of those were caught in this repository in
// one day.
func TestTheRetryBoundScanCanActuallyFail(t *testing.T) {
	unbounded := "func (a *Adapter) Invented(ctx context.Context) error {\n\tf, err := a.mediumFs(ctx, medium)\n\t_ = f\n\treturn err\n}\n"
	body := methodBody(t, unbounded, "Invented")
	if strings.Contains(body, "ctx = mediumContext(ctx)") {
		t.Fatal("the scan's own subject matched a bound that is not there")
	}

	tooLate := "func (a *Adapter) Invented(ctx context.Context) error {\n\tf, err := a.mediumFs(ctx, medium)\n\tctx = mediumContext(ctx)\n\t_ = f\n\treturn err\n}\n"
	body = methodBody(t, tooLate, "Invented")
	if strings.Index(body, "ctx = mediumContext(ctx)") < strings.Index(body, "a.mediumFs(ctx") {
		t.Fatal("the ordering check cannot tell a bound applied after construction from one applied before it, " +
			"which is the only mistake it exists to catch")
	}
}

// methodBody extracts one method's source text from a file, and fails
// rather than returning nothing when the method is not there: a scan that
// silently finds no subject is a scan that passes.
func methodBody(t *testing.T, text, method string) string {
	t.Helper()
	start := strings.Index(text, "func (a *Adapter) "+method+"(")
	if start < 0 {
		t.Fatalf("no %s to scan; if it moved, move this check with it rather than dropping the method", method)
	}
	end := strings.Index(text[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("could not find the end of %s", method)
	}
	return text[start : start+end]
}
