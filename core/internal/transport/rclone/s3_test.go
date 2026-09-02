package rclone

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// s3MediumWithEnvCredentials builds a medium whose credentials resolve
// through the env source, which is the case where this adapter actually
// holds the material and therefore the case worth checking hardest.
func s3MediumWithEnvCredentials(t *testing.T) transport.Medium {
	t.Helper()
	t.Setenv("RCLONE_MANAGER_TEST_S3_CREDS", canaryCredentialsINI())
	m := mediumWith(transport.MediumCredentials{Env: "RCLONE_MANAGER_TEST_S3_CREDS"})
	return m
}

// TestS3Config_OnlyAllowlistedKeysAreSet is ssh.go's
// TestSftpConfig_OnlyAllowlistedKeysAreSet transposed, and it exists for the
// identical reason: rclone's s3 backend exposes over seventy options, a
// pass-through would expose all of them, and several of them (env_auth,
// role_arn, session_token, the sse_customer_* family, download_url,
// v2_auth, use_presigned_request) change who this product authenticates as
// or how it verifies what it stored.
//
// So the set of keys s3Config may produce is pinned here rather than left
// to grow. A future change that starts forwarding an option nobody reviewed
// breaks this test instead of shipping.
func TestS3Config_OnlyAllowlistedKeysAreSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		medium  func(*testing.T) transport.Medium
		allowed map[string]bool
	}{
		{
			name:   "credentials resolved into memory (env or command)",
			medium: s3MediumWithEnvCredentials,
			allowed: map[string]bool{
				// Identity and addressing.
				"provider":          true,
				"region":            true,
				"endpoint":          true,
				"env_auth":          true,
				"access_key_id":     true,
				"secret_access_key": true,
				"storage_class":     true,
				"force_path_style":  true,
				"no_check_bucket":   true,
				// Not part of the FR-33 posture: these six exist because
				// fsForMedium calls info.NewFs directly and so gets none
				// of rclone's own option defaults, exactly as sftpConfig's
				// subsystem/chunk_size/concurrency do. Without them NewFs
				// refuses outright or an upload deadlocks. See s3Config.
				"chunk_size":         true,
				"upload_cutoff":      true,
				"copy_cutoff":        true,
				"upload_concurrency": true,
				"max_upload_parts":   true,
				"list_chunk":         true,
			},
		},
		{
			// The session-token case is its own row rather than a field
			// added to the row above, because the test asserts that every
			// allowed key IS set as well as that no other one is: a
			// medium with no session token must not produce an empty
			// session_token, and one with a token must produce it. Two
			// allowlists is the only way to say both.
			name: "resolved credentials carrying a session token",
			medium: func(t *testing.T) transport.Medium {
				t.Helper()
				t.Setenv("RCLONE_MANAGER_TEST_S3_CREDS", canaryCredentialsINI()+"aws_session_token = "+canaryToken+"\n")
				return mediumWith(transport.MediumCredentials{Env: "RCLONE_MANAGER_TEST_S3_CREDS"})
			},
			allowed: map[string]bool{
				"provider":           true,
				"region":             true,
				"endpoint":           true,
				"env_auth":           true,
				"access_key_id":      true,
				"secret_access_key":  true,
				"session_token":      true,
				"storage_class":      true,
				"force_path_style":   true,
				"no_check_bucket":    true,
				"chunk_size":         true,
				"upload_cutoff":      true,
				"copy_cutoff":        true,
				"upload_concurrency": true,
				"max_upload_parts":   true,
				"list_chunk":         true,
			},
		},
		{
			name: "credentials in a file rclone reads itself",
			medium: func(t *testing.T) transport.Medium {
				t.Helper()
				clearAmbientAWSEnvironment(t)
				return mediumWith(transport.MediumCredentials{File: writeCredentialsFile(t, canaryCredentialsINI())})
			},
			allowed: map[string]bool{
				"provider":                true,
				"region":                  true,
				"endpoint":                true,
				"env_auth":                true,
				"shared_credentials_file": true,
				"storage_class":           true,
				"force_path_style":        true,
				"no_check_bucket":         true,
				"chunk_size":              true,
				"upload_cutoff":           true,
				"copy_cutoff":             true,
				"upload_concurrency":      true,
				"max_upload_parts":        true,
				"list_chunk":              true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := s3Config(tc.medium(t))
			if err != nil {
				t.Fatalf("s3Config: %v", err)
			}
			for k := range cfg {
				if !tc.allowed[k] {
					t.Errorf("s3Config set unexpected key %q; every option this function can set changes who this product authenticates as, what it stores, or how it verifies what it stored, and must be reviewed", k)
				}
			}
			for k := range tc.allowed {
				if _, ok := cfg.Get(k); !ok {
					t.Errorf("s3Config no longer sets %q, which the allowlist says it does; an option that silently stops being set falls back to a Go zero value, not to rclone's own default", k)
				}
			}
		})
	}
}

// TestS3Config_FileSourceNeverProducesAnAccessKey is the other half of the
// allowlist's claim, and it is the one that carries the security property:
// the file source's whole point is that the secret never enters this
// process, so a key that this process would have had to read in order to
// set must never appear in the option map.
func TestS3Config_FileSourceNeverProducesAnAccessKey(t *testing.T) {
	clearAmbientAWSEnvironment(t)
	path := writeCredentialsFile(t, canaryCredentialsINI())
	cfg, err := s3Config(mediumWith(transport.MediumCredentials{File: path}))
	if err != nil {
		t.Fatalf("s3Config: %v", err)
	}
	for _, k := range []string{"access_key_id", "secret_access_key", "session_token"} {
		if v, ok := cfg.Get(k); ok {
			t.Errorf("a file-source medium produced %s=%q; rclone is meant to be the only thing that opens that file", k, v)
		}
	}
	if v, _ := cfg.Get("shared_credentials_file"); v != path {
		t.Errorf("shared_credentials_file = %q, want %q", v, path)
	}
	// env_auth is what makes rclone consult shared_credentials_file at all
	// (backend/s3/s3.go's s3Connection only reaches LoadDefaultConfig when
	// EnvAuth is set and no static key is configured), so a file source
	// with env_auth unset would silently authenticate as nobody.
	if v, _ := cfg.Get("env_auth"); v != "true" {
		t.Errorf("env_auth = %q on the file source, want \"true\"; without it rclone never reads shared_credentials_file", v)
	}
}

// TestS3Config_ResolvedCredentialsNeverEnableAmbientAuth is the mirror
// image. When this adapter holds the credentials, rclone must use exactly
// those and must never be allowed to fall back to the environment or to an
// instance role.
func TestS3Config_ResolvedCredentialsNeverEnableAmbientAuth(t *testing.T) {
	cfg, err := s3Config(s3MediumWithEnvCredentials(t))
	if err != nil {
		t.Fatalf("s3Config: %v", err)
	}
	if v, _ := cfg.Get("env_auth"); v != "false" {
		t.Errorf("env_auth = %q with resolved credentials, want \"false\"; anything else lets rclone reach for ambient credentials this product did not choose", v)
	}
	if v, _ := cfg.Get("access_key_id"); v != canaryKeyID {
		t.Errorf("access_key_id did not reach rclone")
	}
	if v, _ := cfg.Get("secret_access_key"); v != canarySecret {
		t.Errorf("secret_access_key did not reach rclone")
	}
	if _, ok := cfg.Get("shared_credentials_file"); ok {
		t.Error("a resolved-credential medium produced shared_credentials_file")
	}
}

// TestS3Config_RefusesAmbientAWSCredentialEnvironment is the ssh-agent
// refusal transposed to S3, and it is this change's least obvious finding.
//
// The file source works by setting env_auth=true, which makes rclone call
// the AWS SDK's LoadDefaultConfig. That is a CHAIN, and the shared
// credentials file is not the first link in it: an AWS_ACCESS_KEY_ID in
// this process's environment wins over the file the operator configured,
// silently, and the backup then authenticates as an account nobody chose.
// That is exactly the shape of failure sftpConfig refuses for ssh-agent,
// so it gets the same answer: refuse rather than guess.
func TestS3Config_RefusesAmbientAWSCredentialEnvironment(t *testing.T) {
	path := writeCredentialsFile(t, canaryCredentialsINI())
	for _, name := range ambientAWSCredentialEnvVars {
		t.Run(name, func(t *testing.T) {
			clearAmbientAWSEnvironment(t)
			t.Setenv(name, "something-the-operator-did-not-configure")
			_, err := s3Config(mediumWith(transport.MediumCredentials{File: path}))
			if err == nil {
				t.Fatalf("s3Config accepted a file-source medium with %s set in the environment; the SDK credential chain would prefer it over the configured file", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name %s, which is the variable an operator has to unset", err, name)
			}
		})
	}
}

// TestS3Config_AmbientEnvironmentIsIrrelevantToAResolvedCredential is the
// bound on the refusal above. Env and command set a static key, which
// short-circuits the whole SDK chain, so the ambient environment cannot
// displace anything and refusing over it would be refusing a configuration
// that works.
func TestS3Config_AmbientEnvironmentIsIrrelevantToAResolvedCredential(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "an-unrelated-key-in-the-environment")
	if _, err := s3Config(s3MediumWithEnvCredentials(t)); err != nil {
		t.Fatalf("s3Config refused a resolved-credential medium over an ambient AWS variable it does not consult: %v", err)
	}
}

// TestS3Config_ProviderFollowsTheEndpoint pins the one derived value in
// this function. There is no provider field in config.StorageMedium, so
// s3Config decides, and the decision is visible here rather than buried.
func TestS3Config_ProviderFollowsTheEndpoint(t *testing.T) {
	base := s3MediumWithEnvCredentials(t)

	aws := base
	aws.Endpoint = ""
	cfg, err := s3Config(aws)
	if err != nil {
		t.Fatalf("s3Config: %v", err)
	}
	if v, _ := cfg.Get("provider"); v != "AWS" {
		t.Errorf("provider = %q for a medium with no endpoint, want %q", v, "AWS")
	}

	other := base
	other.Endpoint = "https://minio.example:9000"
	cfg, err = s3Config(other)
	if err != nil {
		t.Fatalf("s3Config: %v", err)
	}
	if v, _ := cfg.Get("provider"); v != "Other" {
		t.Errorf("provider = %q for a medium with an endpoint, want %q", v, "Other")
	}
	// force_path_style is the option this decision exists to get right.
	// rclone's Other quirks leave it alone, and its Go zero value is
	// false, so an S3-compatible endpoint reached with virtual-host
	// addressing fails to resolve at all.
	if v, _ := cfg.Get("force_path_style"); v != "true" {
		t.Errorf("force_path_style = %q for an endpoint-addressed medium, want %q", v, "true")
	}
}

// TestS3Config_NeverCreatesABucket pins no_check_bucket. rclone's default
// is to check for the bucket and CREATE it if missing, which turns a typo
// in a bucket name into a new, empty, silently-wrong destination instead of
// a refusal an operator can read. It also means this product never needs
// s3:CreateBucket on the credentials it is given.
func TestS3Config_NeverCreatesABucket(t *testing.T) {
	cfg, err := s3Config(s3MediumWithEnvCredentials(t))
	if err != nil {
		t.Fatalf("s3Config: %v", err)
	}
	if v, _ := cfg.Get("no_check_bucket"); v != "true" {
		t.Errorf("no_check_bucket = %q, want %q; anything else lets a typo in bucket: create a bucket rather than refuse", v, "true")
	}
}

// TestS3Config_ZeroValueOptionsAreAllSet is the s3 half of the trap
// sftpConfig documents at length: fsForMedium calls info.NewFs directly, so
// configstruct.Set only reads keys present in the map and every option left
// unset comes out as its Go zero value rather than rclone's default. For
// most options that is harmless. For these six it is not, and the failure
// is not subtle degradation, it is NewFs refusing or an upload deadlocking.
//
// Each is checked against rclone's own documented default rather than
// merely "not empty", so a future edit that sets one of them to something
// arbitrary has to say so here.
func TestS3Config_ZeroValueOptionsAreAllSet(t *testing.T) {
	cfg, err := s3Config(s3MediumWithEnvCredentials(t))
	if err != nil {
		t.Fatalf("s3Config: %v", err)
	}
	for _, tc := range []struct{ key, want, why string }{
		{"chunk_size", "5Mi", "NewFs calls checkUploadChunkSize and refuses anything below 5Mi, so a zero value fails every operation"},
		{"upload_cutoff", "200Mi", "a zero cutoff makes every upload multipart, including a zero-byte one"},
		{"copy_cutoff", "4768Mi", "NewFs calls checkCopyCutoff and refuses a value below one byte, so a zero value fails every operation"},
		{"upload_concurrency", "4", "the multipart uploader calls errgroup.SetLimit with this, and SetLimit(0) means no goroutine may run"},
		{"max_upload_parts", "10000", "a zero part ceiling makes the chunk-size arithmetic in multipart upload meaningless"},
		{"list_chunk", "1000", "a zero MaxKeys is sent to S3 verbatim rather than falling back to a page size"},
	} {
		if v, _ := cfg.Get(tc.key); v != tc.want {
			t.Errorf("%s = %q, want %q: %s", tc.key, v, tc.want, tc.why)
		}
	}
}

// TestS3Config_IsAcceptedByRcloneOffline is the proof that the option map
// above is not merely well-intentioned. rclone's NewFs validates several of
// these before it makes any network call at all, and with a root naming
// only a bucket it makes none, so this catches every zero-value trap
// without needing an endpoint.
//
// It is the cheap half of the evidence. The MinIO contract run is the
// expensive half.
func TestS3Config_IsAcceptedByRcloneOffline(t *testing.T) {
	medium := s3MediumWithEnvCredentials(t)
	cfg, err := s3Config(medium)
	if err != nil {
		t.Fatalf("s3Config: %v", err)
	}
	info, err := fs.Find("s3")
	if err != nil {
		t.Fatalf("fs.Find(s3): %v", err)
	}
	if _, err := info.NewFs(context.Background(), "medium:"+medium.ID, medium.Bucket, cfg); err != nil {
		t.Fatalf("rclone refused this adapter's own s3 options before reaching the network: %v", err)
	}
}

// TestS3Config_RefusesWhatItCannotAddress keeps a half-configured medium
// from becoming a runtime failure somewhere less legible.
func TestS3Config_RefusesWhatItCannotAddress(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutum func(*transport.Medium)
	}{
		{"no bucket", func(m *transport.Medium) { m.Bucket = "" }},
		{"a bucket that is really a bucket and a prefix", func(m *transport.Medium) { m.Bucket = "nas-backups/rclone-manager" }},
		{"an unknown medium type", func(m *transport.Medium) { m.Type = "azure" }},
		{"no id", func(m *transport.Medium) { m.ID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			medium := s3MediumWithEnvCredentials(t)
			tc.mutum(&medium)
			if _, err := s3Config(medium); err == nil {
				t.Fatalf("s3Config accepted a medium with %s", tc.name)
			}
		})
	}
}

// TestS3ConfigErrorsNeverCarryACredential is the canary applied to this
// function's own failure paths. The option map itself necessarily holds the
// secret, because handing it to rclone is the entire job; what must never
// happen is that a refusal on the way there quotes it.
func TestS3ConfigErrorsNeverCarryACredential(t *testing.T) {
	for _, tc := range []struct {
		name   string
		medium func(*testing.T) transport.Medium
	}{
		{"a malformed credentials block", func(t *testing.T) transport.Medium {
			t.Helper()
			t.Setenv("RCLONE_MANAGER_TEST_S3_CREDS", "[default]\naws_access_key_id = "+canaryKeyID+"\naws_secret_access_key = "+canarySecret+"\nregion = us-east-1\n")
			return mediumWith(transport.MediumCredentials{Env: "RCLONE_MANAGER_TEST_S3_CREDS"})
		}},
		{"a well-formed credentials block on a medium with no bucket", func(t *testing.T) transport.Medium {
			t.Helper()
			m := s3MediumWithEnvCredentials(t)
			m.Bucket = ""
			return m
		}},
		{"an ambient environment refusal", func(t *testing.T) transport.Medium {
			t.Helper()
			clearAmbientAWSEnvironment(t)
			t.Setenv("AWS_SECRET_ACCESS_KEY", canarySecret)
			return mediumWith(transport.MediumCredentials{File: writeCredentialsFile(t, canaryCredentialsINI())})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s3Config(tc.medium(t))
			if err == nil {
				t.Fatal("expected a refusal, so this canary check had nothing to look at")
			}
			assertNoCanary(t, err.Error(), "an s3Config refusal for "+tc.name)
		})
	}
}

// clearAmbientAWSEnvironment unsets every variable the ambient-credential
// guard looks at, so a developer machine that legitimately has AWS
// credentials configured does not fail the tests that are about something
// else. t.Setenv's cleanup restores whatever was there.
func clearAmbientAWSEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range ambientAWSCredentialEnvVars {
		// t.Setenv records the prior value and restores it on cleanup;
		// os.Unsetenv then makes the variable genuinely absent for the
		// duration, which is not the same as present-and-empty.
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}
}

// TestS3Config_RefusalsAreConfigurationNotPermanent pins the category on
// every refusal that is a fact about what an operator wrote.
//
// It matters because Permanent and Configuration say different things to
// whoever is on the other end: Permanent means this classifier did not
// recognise the failure, and Configuration means somebody has a typo and no
// retry will help. FR-28 added the category for exactly this, and a
// category nothing ever produces is a category nothing can act on.
//
// Before this, every one of these fell through Classify to Permanent, which
// is the label for "we do not know what this was" attached to the cases we
// know best.
func TestS3Config_RefusalsAreConfigurationNotPermanent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		medium func(*testing.T) transport.Medium
	}{
		{"no bucket", func(t *testing.T) transport.Medium {
			m := s3MediumWithEnvCredentials(t)
			m.Bucket = ""
			return m
		}},
		{"a bucket and a prefix in one field", func(t *testing.T) transport.Medium {
			m := s3MediumWithEnvCredentials(t)
			m.Bucket = "nas-backups/rclone-manager"
			return m
		}},
		{"a medium type this adapter does not implement", func(t *testing.T) transport.Medium {
			m := s3MediumWithEnvCredentials(t)
			m.Type = "azure"
			return m
		}},
		{"no credential source at all", func(t *testing.T) transport.Medium {
			return mediumWith(transport.MediumCredentials{})
		}},
		{"two credential sources", func(t *testing.T) transport.Medium {
			return mediumWith(transport.MediumCredentials{File: "/tmp/x", Env: "Y"})
		}},
		{"an environment variable that is not set", func(t *testing.T) transport.Medium {
			t.Helper()
			_ = os.Unsetenv("RCLONE_MANAGER_TEST_S3_CREDS_MISSING")
			return mediumWith(transport.MediumCredentials{Env: "RCLONE_MANAGER_TEST_S3_CREDS_MISSING"})
		}},
		{"a credentials file that is not there", func(t *testing.T) transport.Medium {
			t.Helper()
			clearAmbientAWSEnvironment(t)
			return mediumWith(transport.MediumCredentials{File: filepath.Join(t.TempDir(), "absent")})
		}},
		{"a credentials file with no default profile", func(t *testing.T) transport.Medium {
			t.Helper()
			clearAmbientAWSEnvironment(t)
			return mediumWith(transport.MediumCredentials{File: writeCredentialsFile(t, "[cold]\naws_access_key_id = a\naws_secret_access_key = b\n")})
		}},
		{"an ambient AWS key in the environment", func(t *testing.T) transport.Medium {
			t.Helper()
			clearAmbientAWSEnvironment(t)
			t.Setenv("AWS_ACCESS_KEY_ID", "somebody-elses-account")
			return mediumWith(transport.MediumCredentials{File: writeCredentialsFile(t, canaryCredentialsINI())})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s3Config(tc.medium(t))
			if err == nil {
				t.Fatal("expected a refusal")
			}
			category, ok := transport.CategoryOf(err)
			if !ok {
				t.Fatalf("the refusal carries no manager-owned category at all: %v", err)
			}
			if category != transport.Configuration {
				t.Errorf("category = %s, want %s; this is a fact about what an operator wrote, and no retry changes it\nerror: %v", category, transport.Configuration, err)
			}
			if category.Retryable() {
				t.Error("this refusal reports itself retryable")
			}
		})
	}
}

// TestS3Config_APermissionRefusalKeepsItsOwnVerdict is the bound on the
// test above. A credentials file anyone can read is not filed under
// Configuration: KeyPermissions is its own considered verdict with its own
// remediation, and #293 argued it at length for the SSH key.
func TestS3Config_APermissionRefusalKeepsItsOwnVerdict(t *testing.T) {
	clearAmbientAWSEnvironment(t)
	path := writeCredentialsFile(t, canaryCredentialsINI())
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err := s3Config(mediumWith(transport.MediumCredentials{File: path}))
	if err == nil {
		t.Fatal("a world-readable credentials file was accepted")
	}
	if category, _ := transport.CategoryOf(err); category != transport.KeyPermissions {
		t.Errorf("category = %s, want %s", category, transport.KeyPermissions)
	}
}
