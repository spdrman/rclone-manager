// Package miniointegration_test is E1.3's evidence against a real S3
// endpoint: the MediumStore contract suite driven through the real rclone
// s3 adapter, the FR-22 classification of real S3 failures, and FR-33's
// credential canary resolved through all three sources.
//
// It exists because the adapter was written carefully against rclone's own
// source and was still wrong in four places that only a real endpoint could
// show. Each of them would have passed against a mock, and each of them was
// a data-safety bug rather than a cosmetic one:
//
//   - DeleteObject against a mistyped bucket returned nil. Under FR-30's
//     medium-aware prune that marks a placement GONE for an artifact nobody
//     deleted.
//   - ListObjects against a mistyped bucket returned an empty listing and
//     no error, so a catalog rebuild would conclude the medium holds
//     nothing.
//   - StatObject and OpenObject against one reported NotFound, which a
//     reconciler reads as "the medium lost the artifact".
//   - Every 403 classified as Permanent, because a HEAD response has no
//     body for the SDK to read a real error code out of, so bad credentials
//     told the operator to look at anything except their credentials.
//
// The fixes are confirmBucket and the Forbidden entry in errors.go's table.
// The tests below are what stops them regressing.
package miniointegration_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	rclonefs "github.com/rclone/rclone/fs"

	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/mediumcontract"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/miniofixture"
)

// minioFixtures adapts a running MinIO container to the contract suite.
// Each medium gets its own prefix inside the one bucket, which is the
// isolation the suite asks for and is also how a real deployment separates
// two backup sets sharing a bucket.
type minioFixtures struct {
	fixture *miniofixture.Fixture
	creds   transport.MediumCredentials

	// namespace separates one Fixtures value's mediums from another's.
	// The three credential sources below share one bucket and one running
	// server, so without it the second source's first medium reuses the
	// first source's first prefix and finds its objects already there.
	// The contract suite caught exactly that, which is a decent
	// advertisement for a suite that asserts isolation rather than
	// assuming it.
	namespace string

	mu sync.Mutex
	n  int
}

func (f *minioFixtures) NewMedium(t *testing.T) transport.Medium {
	t.Helper()
	f.mu.Lock()
	f.n++
	n := f.n
	f.mu.Unlock()
	return transport.Medium{
		ID:           fmt.Sprintf("minio-%d", n),
		Type:         transport.MediumTypeS3,
		Region:       "us-east-1",
		Endpoint:     f.fixture.Endpoint,
		Bucket:       f.fixture.Bucket,
		Prefix:       fmt.Sprintf("rclone-manager/%s/run-%d", f.namespace, n),
		StorageClass: "STANDARD",
		Credentials:  f.creds,
	}
}

func (f *minioFixtures) Context(*testing.T) context.Context { return f.fixture.Context() }

// AttestsChecksums is false, and that is the honest answer rather than a
// convenience. rclone v1.75's s3 backend reports MD5 and nothing else, and
// that MD5 is the object's ETag, which FR-32 says is never a content hash.
// So FR-31's `attested` class is not reachable through this backend, and
// the suite asserts the explicit capability refusal FR-13 requires instead
// of a silently weaker answer.
func (f *minioFixtures) AttestsChecksums() bool { return false }

// TestMediumStoreContractAgainstMinIO is FR-28's "MinIO contract run green
// in integration", once per credential source.
//
// All three sources run the whole suite rather than one smoke case each,
// because the credential path is not a preamble to the behaviour: it
// decides whether rclone gets a static key or is sent to the AWS SDK's
// credential chain, and those are two different code paths inside the
// backend. A file source that authenticates but cannot list would be
// invisible to a shallower test.
func TestMediumStoreContractAgainstMinIO(t *testing.T) {
	fixture := miniofixture.Start(t)
	adapter := rclone.New()

	envVar := "RCLONE_MANAGER_MINIO_TEST_CREDS"
	t.Setenv(envVar, fixture.CredentialsINI())
	credCommand := writeCredentialsCommand(t, fixture.CredentialsFile)

	for _, tc := range []struct {
		name  string
		creds transport.MediumCredentials
	}{
		{"credentials.file", transport.MediumCredentials{File: fixture.CredentialsFile}},
		{"credentials.env", transport.MediumCredentials{Env: envVar}},
		{"credentials.command", transport.MediumCredentials{Command: []string{credCommand}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearAmbientAWSEnvironment(t)
			mediumcontract.Run(t, adapter, &minioFixtures{
				fixture:   fixture,
				creds:     tc.creds,
				namespace: strings.ReplaceAll(tc.name, ".", "-"),
			})
		})
	}
}

// TestS3ErrorClassificationAgainstMinIO is the runtime proof errors.go's
// own doc promises.
//
// Classify matches the S3 error code through an interface declared locally
// rather than imported, because this repository refuses to import a
// provider SDK. Go interfaces are structural so that works, but it means
// there is no compile-time proof that the SDK's real error types satisfy
// it. This test is that proof, against a real endpoint producing real
// NoSuchBucket, real NoSuchKey and real 403 responses.
func TestS3ErrorClassificationAgainstMinIO(t *testing.T) {
	fixture := miniofixture.Start(t)
	clearAmbientAWSEnvironment(t)
	adapter := rclone.New()
	ctx := fixture.Context()

	envVar := "RCLONE_MANAGER_MINIO_TEST_CREDS"
	t.Setenv(envVar, fixture.CredentialsINI())
	good := transport.Medium{
		ID: "classify", Type: transport.MediumTypeS3, Region: "us-east-1",
		Endpoint: fixture.Endpoint, Bucket: fixture.Bucket,
		Prefix: "rclone-manager/classify", StorageClass: "STANDARD",
		Credentials: transport.MediumCredentials{Env: envVar},
	}

	seed := filepath.Join(t.TempDir(), "seed.dump")
	if err := os.WriteFile(seed, []byte("seed"), 0o600); err != nil {
		t.Fatalf("writing the seed file: %v", err)
	}
	seedKey := "rclone-manager/classify/src/set/seed.dump"
	if _, err := adapter.UploadFromLocal(ctx, good, seed, seedKey); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	missingBucket := good
	missingBucket.Bucket = "no-such-bucket-anywhere"

	badEnv := "RCLONE_MANAGER_MINIO_TEST_BAD_CREDS"
	t.Setenv(badEnv, "[default]\naws_access_key_id = notthekey\naws_secret_access_key = notthesecret\n")
	badCredentials := good
	badCredentials.Credentials = transport.MediumCredentials{Env: badEnv}

	for _, tc := range []struct {
		name string
		op   func() error
		want transport.Category
		why  string
	}{
		{
			name: "NoSuchKey is NotFound",
			op: func() error {
				_, err := adapter.StatObject(ctx, good, "rclone-manager/classify/src/set/absent.dump")
				return err
			},
			want: transport.NotFound,
			why:  "one artifact is not on the medium, which is a fact about that artifact",
		},
		{
			name: "NoSuchBucket on upload is Configuration",
			op: func() error {
				_, err := adapter.UploadFromLocal(ctx, missingBucket, seed, "rclone-manager/classify/src/set/x.dump")
				return err
			},
			want: transport.Configuration,
			why:  "PutObject leaves the real NoSuchBucket code visible, and no retry makes a bucket appear",
		},
		{
			name: "NoSuchBucket on stat is Configuration, not NotFound",
			op:   func() error { _, err := adapter.StatObject(ctx, missingBucket, seedKey); return err },
			want: transport.Configuration,
			why:  "rclone flattens the 404 into ErrorObjectNotFound, so confirmBucket is what keeps a missing bucket from reading as a missing artifact",
		},
		{
			name: "NoSuchBucket on list is Configuration, not an empty medium",
			op: func() error {
				_, err := adapter.ListObjects(ctx, missingBucket, "rclone-manager/classify")
				return err
			},
			want: transport.Configuration,
			why:  "an empty listing here would tell a catalog rebuild the medium holds nothing",
		},
		{
			name: "NoSuchBucket on delete is Configuration, not success",
			op:   func() error { return adapter.DeleteObject(ctx, missingBucket, seedKey) },
			want: transport.Configuration,
			why:  "reporting success would let prune mark a placement GONE for an artifact nobody deleted",
		},
		{
			name: "a 403 is Authentication",
			op:   func() error { _, err := adapter.StatObject(ctx, badCredentials, seedKey); return err },
			want: transport.Authentication,
			why:  "a HEAD has no body, so the SDK synthesises the code from the status line and the real AccessDenied never appears",
		},
		{
			name: "a 403 on a listing is Authentication",
			op: func() error {
				_, err := adapter.ListObjects(ctx, badCredentials, "rclone-manager/classify")
				return err
			},
			want: transport.Authentication,
			why:  "a GET does carry a body, so this one arrives as the real InvalidAccessKeyId",
		},
		{
			name: "an occupied key is Conflict",
			op: func() error {
				_, err := adapter.UploadFromLocal(ctx, good, seed, seedKey)
				return err
			},
			want: transport.Conflict,
			why:  "the caller's next move is confirm-then-continue, and only a resumable category says so",
		},
		{
			name: "an absent attestation is UnsupportedCapability",
			op: func() error {
				_, err := adapter.ObjectChecksum(ctx, good, seedKey, transport.SHA256)
				return err
			},
			want: transport.UnsupportedCapability,
			why:  "FR-13 says an unavailable verification is an explicit capability result, never a silent weakening",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.op()
			if err == nil {
				t.Fatalf("expected a failure, got none: %s", tc.why)
			}
			category, ok := transport.CategoryOf(err)
			if !ok {
				t.Fatalf("the error carries no manager-owned category at all: %v", err)
			}
			if category != tc.want {
				t.Errorf("category = %s, want %s (%s)\nerror: %v", category, tc.want, tc.why, err)
			}
		})
	}
}

// TestMediumCredentialCanary is FR-33's enforcement, and the shape of it
// matters as much as the assertion: a canary value nothing else in the
// process could produce is resolved through each of the three sources, a
// full set of operations is driven with rclone's own logging turned all the
// way up, and then every observable output is searched for it.
//
// "Every observable output" here means: rclone's own log stream (which is
// where a leak would most plausibly appear, since rclone logs its own
// configuration at debug level and this product does not control what it
// says), this product's obs.Logger stream, every error string returned, and
// every returned value rendered with the verbs a debugging operator
// reaches for.
//
// The canary is a real working credential for the throwaway container, not
// a decoy planted in an unused field. A decoy proves only that an unused
// field is unused; this proves the value that actually authenticates every
// request never comes back out.
func TestMediumCredentialCanary(t *testing.T) {
	fixture := miniofixture.Start(t)
	clearAmbientAWSEnvironment(t)
	adapter := rclone.New()

	// The canary is the fixture's own secret access key: the value that
	// signs every request below.
	canary := fixture.SecretAccessKey
	if len(canary) < 16 {
		t.Fatalf("the fixture's secret is only %d characters, which is too short to search for reliably", len(canary))
	}

	envVar := "RCLONE_MANAGER_MINIO_CANARY_CREDS"
	t.Setenv(envVar, fixture.CredentialsINI())
	credCommand := writeCredentialsCommand(t, fixture.CredentialsFile)

	for _, tc := range []struct {
		name  string
		creds transport.MediumCredentials
	}{
		{"credentials.file", transport.MediumCredentials{File: fixture.CredentialsFile}},
		{"credentials.env", transport.MediumCredentials{Env: envVar}},
		{"credentials.command", transport.MediumCredentials{Command: []string{credCommand}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captured, rendered := exerciseEverything(t, fixture, adapter, tc.creds)

			// The planted violation. In an ordinary build this does
			// nothing at all; built with -tags canaryviolation it logs the
			// resolved medium configuration verbatim, exactly the way a
			// careless debugging line would, and this test then fails.
			// See violation_planted_test.go, and the PR body for the
			// recorded failing run.
			plantCanaryViolation(fixture)

			for what, text := range map[string]string{
				"rclone's own log stream":                captured.rcloneLog(),
				"this product's structured log stream":   captured.productLog(),
				"the errors returned by every operation": captured.errors(),
				"the values returned by every operation": rendered,
			} {
				if text == "" {
					t.Errorf("%s came back empty, so this canary check had nothing to look at", what)
					continue
				}
				if strings.Contains(text, canary) {
					t.Errorf("the resolved credential appears in %s.\nFR-33: it may appear in no log line at any level, no error message, and no returned value", what)
				}
			}
		})
	}
}

// TestCredentialsFilePostureAgainstMinIO proves the file source's checks
// are checks and not decoration, against a file that genuinely works.
func TestCredentialsFilePostureAgainstMinIO(t *testing.T) {
	fixture := miniofixture.Start(t)
	adapter := rclone.New()
	ctx := fixture.Context()

	medium := transport.Medium{
		ID: "posture", Type: transport.MediumTypeS3, Region: "us-east-1",
		Endpoint: fixture.Endpoint, Bucket: fixture.Bucket,
		Prefix: "rclone-manager/posture", StorageClass: "STANDARD",
		Credentials: transport.MediumCredentials{File: fixture.CredentialsFile},
	}

	t.Run("the positive control", func(t *testing.T) {
		clearAmbientAWSEnvironment(t)
		if _, err := adapter.ListObjects(ctx, medium, "rclone-manager/posture"); err != nil {
			t.Fatalf("a correctly configured file source could not list: %v", err)
		}
	})

	t.Run("an ambient AWS key in the environment is refused", func(t *testing.T) {
		clearAmbientAWSEnvironment(t)
		t.Setenv("AWS_ACCESS_KEY_ID", "an-account-nobody-configured")
		_, err := adapter.ListObjects(ctx, medium, "rclone-manager/posture")
		if err == nil {
			t.Fatal("the file source was used while AWS_ACCESS_KEY_ID was set; the SDK chain puts the environment ahead of the file, so the backup would have run as that account")
		}
		if !strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID") {
			t.Errorf("error %q does not name the variable an operator has to unset", err)
		}
	})

	t.Run("a group-readable credentials file is refused", func(t *testing.T) {
		clearAmbientAWSEnvironment(t)
		if err := os.Chmod(fixture.CredentialsFile, 0o644); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(fixture.CredentialsFile, 0o600) })
		_, err := adapter.ListObjects(ctx, medium, "rclone-manager/posture")
		if err == nil {
			t.Fatal("a credentials file readable by every account on the host was accepted")
		}
	})
}

// --- helpers ---

// captureStreams holds everything an operation could have spoken through.
type captureStreams struct {
	rclone  *bytes.Buffer
	product *bytes.Buffer
	errs    []string
}

func (c *captureStreams) rcloneLog() string  { return c.rclone.String() }
func (c *captureStreams) productLog() string { return c.product.String() }
func (c *captureStreams) errors() string     { return strings.Join(c.errs, "\n") }

// exerciseEverything drives every MediumStore method, in both its
// succeeding and its failing shape, with rclone's log level at its most
// verbose, and returns what was said and what was returned.
//
// Turning rclone's own log level up is the point rather than a detail. At
// the default level rclone says almost nothing, so a canary test at that
// level would mostly be asserting that a silent library is silent. Debug is
// where a backend prints its own configuration, and it is the level an
// operator turns on when something is wrong, which is exactly when a leak
// would land in a support bundle.
func exerciseEverything(t *testing.T, fixture *miniofixture.Fixture, adapter *rclone.Adapter, creds transport.MediumCredentials) (*captureStreams, string) {
	t.Helper()

	captured := &captureStreams{rclone: &bytes.Buffer{}, product: &bytes.Buffer{}}

	// rclone logs through slog.Default(), whose handler forwards to the
	// standard log package, so redirecting that captures it. The flags are
	// reset alongside it so a timestamp cannot be mistaken for content.
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(captured.rclone)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldWriter); log.SetFlags(oldFlags) })

	ctx := fixture.Context()
	ci := rclonefs.GetConfig(ctx)
	oldLevel := ci.LogLevel
	ci.LogLevel = rclonefs.LogLevelDebug
	t.Cleanup(func() { ci.LogLevel = oldLevel })

	logger := obs.New(captured.product, obs.LevelDebug)

	medium := transport.Medium{
		ID: "canary", Type: transport.MediumTypeS3, Region: "us-east-1",
		Endpoint: fixture.Endpoint, Bucket: fixture.Bucket,
		Prefix: "rclone-manager/canary", StorageClass: "STANDARD",
		Credentials: creds,
	}
	missingBucket := medium
	missingBucket.Bucket = "no-such-bucket-anywhere"

	local := filepath.Join(t.TempDir(), "canary.dump")
	if err := os.WriteFile(local, []byte("canary payload"), 0o600); err != nil {
		t.Fatalf("writing the local file: %v", err)
	}
	key := "rclone-manager/canary/src/set/canary.dump"

	var rendered strings.Builder
	record := func(label string, value any, err error) {
		if err != nil {
			captured.errs = append(captured.errs, fmt.Sprintf("%s: %v", label, err))
			// The product's own error event is one of the observable
			// outputs FR-33 lists, so it is exercised rather than assumed.
			logger.Error(ctx, label, err)
		}
		for _, verb := range []string{"%v", "%+v", "%#v"} {
			fmt.Fprintf(&rendered, "%s "+verb+"\n", label, value)
		}
	}

	// The medium descriptor itself is the first thing a careless log line
	// would carry, so it is rendered here on purpose.
	record("the medium descriptor", medium, nil)

	up, err := adapter.UploadFromLocal(ctx, medium, local, key)
	record("upload", up, err)

	info, err := adapter.StatObject(ctx, medium, key)
	record("stat", info, err)

	objs, err := adapter.ListObjects(ctx, medium, "rclone-manager/canary")
	record("list", objs, err)

	if rc, oerr := adapter.OpenObject(ctx, medium, key); oerr == nil {
		body, rerr := io.ReadAll(rc)
		rc.Close()
		record("open", string(body), rerr)
	} else {
		record("open", nil, oerr)
	}

	att, err := adapter.ObjectChecksum(ctx, medium, key, transport.SHA256)
	record("checksum", att, err)

	// Failing shapes, because an error path is where a value most often
	// gets formatted into a message by somebody in a hurry.
	_, err = adapter.UploadFromLocal(ctx, medium, local, key)
	record("upload onto an occupied key", nil, err)
	_, err = adapter.StatObject(ctx, medium, "rclone-manager/canary/src/set/absent.dump")
	record("stat an absent object", nil, err)
	_, err = adapter.StatObject(ctx, missingBucket, key)
	record("stat in a missing bucket", nil, err)
	_, err = adapter.ListObjects(ctx, missingBucket, "rclone-manager/canary")
	record("list a missing bucket", nil, err)
	record("delete in a missing bucket", nil, adapter.DeleteObject(ctx, missingBucket, key))
	record("delete", nil, adapter.DeleteObject(ctx, medium, key))

	if len(captured.errs) == 0 {
		t.Fatal("no operation failed, so the error half of this canary check had nothing to look at")
	}
	return captured, rendered.String()
}

// writeCredentialsCommand writes an executable that prints the credentials
// file, which is how the command source is exercised without inventing a
// second credential format.
func writeCredentialsCommand(t *testing.T, credentialsFile string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials-resolver.sh")
	body := "#!/bin/sh\nexec /bin/cat " + credentialsFile + "\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("writing the credentials command: %v", err)
	}
	return path
}

// clearAmbientAWSEnvironment unsets the AWS variables s3Config refuses a
// file source over, so a developer machine with real AWS credentials
// configured does not fail the tests that are about something else.
func clearAmbientAWSEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY", "AWS_SECRET_KEY",
		"AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_SHARED_CREDENTIALS_FILE",
		"AWS_CONFIG_FILE", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
	} {
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}
}
