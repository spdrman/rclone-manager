package rclone

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// writeCreds writes a credentials file at the custody this package
// demands, so a case about the CHAIN cannot accidentally pass because the
// permission check refused it first.
func writeCreds(t *testing.T, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "creds")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("creating the directory: %v", err)
	}
	path := filepath.Join(dir, "offsite.creds")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the credentials file: %v", err)
	}
	return path
}

func fileMedium(path string) transport.Medium {
	return transport.Medium{
		ID:          "offsite_s3",
		Type:        transport.MediumTypeS3,
		Bucket:      "backups",
		Credentials: transport.MediumCredentials{File: path},
	}
}

const goodCredsBody = "[default]\naws_access_key_id = AKIAEXAMPLEEXAMPLE\naws_secret_access_key = examplesecretexamplesecret\n"

// TestTheFileSourceRefusesAnAmbientAWSEnvironment is the guard on the one
// property that makes `file` the PREFERRED source.
//
// rclone reaches a credentials file through env_auth=true, which is the
// AWS SDK's whole credential CHAIN (backend/s3/s3.go:1514-1525), and the
// configured file is not the first link in it. An AWS_ACCESS_KEY_ID in
// this process's environment wins silently, and the backup then runs as an
// account nobody chose. Without this refusal there is no error, no log
// line and no symptom: the artifacts simply go somewhere else.
func TestTheFileSourceRefusesAnAmbientAWSEnvironment(t *testing.T) {
	path := writeCreds(t, goodCredsBody)

	// Every variable on the list, one case each, because a list is only
	// as good as its least-checked entry and the subtle ones (the two
	// file variables) are exactly the ones a shorter test would omit.
	for _, name := range ambientAWSCredentialEnvVars {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "something-somebody-else-set")
			_, err := mediumAuthOptions(fileMedium(path))
			if err == nil {
				t.Fatalf("credentials.file was accepted while %s was set. The AWS credential chain consults that BEFORE "+
					"the configured file, so the backup would have run as whatever account it names, silently", name)
			}
			if category, ok := transport.CategoryOf(err); !ok || category != transport.Configuration {
				t.Errorf("classified as %v (recognised=%v), want %s", category, ok, transport.Configuration)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the refusal does not name %s, which is the whole content of the fix: %v", name, err)
			}
			if strings.Contains(err.Error(), "something-somebody-else-set") {
				t.Errorf("the refusal echoed the VALUE of %s; one of these variables holds a secret key: %v", name, err)
			}
		})
	}

	t.Run("set but empty does not count", func(t *testing.T) {
		// The SDK treats an empty AWS_ACCESS_KEY_ID as unset, and
		// clearing a variable by setting it empty is an ordinary thing to
		// do in a unit file. Refusing that would refuse a correct
		// deployment.
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		if _, err := mediumAuthOptions(fileMedium(path)); err != nil {
			t.Fatalf("an empty AWS_ACCESS_KEY_ID was treated as present: %v", err)
		}
	})

	t.Run("a clean environment is accepted", func(t *testing.T) {
		for _, name := range ambientAWSCredentialEnvVars {
			t.Setenv(name, "")
		}
		cfg, err := mediumAuthOptions(fileMedium(path))
		if err != nil {
			t.Fatalf("a correctly configured file source was refused in a clean environment: %v", err)
		}
		if got, _ := cfg.Get("shared_credentials_file"); got != path {
			t.Errorf("shared_credentials_file = %q, want %q", got, path)
		}
		if got, _ := cfg.Get("env_auth"); got != "true" {
			t.Errorf("env_auth = %q, want true; without it rclone never opens the file at all", got)
		}
	})

	t.Run("the env and command sources are not affected", func(t *testing.T) {
		// They set a static key, and rclone consults the SDK chain only
		// when there is none (s3.go:1514), so the ambient environment is
		// already unreachable for them. Refusing there would be a rule
		// with no failure behind it.
		t.Setenv("AWS_ACCESS_KEY_ID", "somebody-elses-account")
		t.Setenv("MEDIUM_CREDS_FOR_THIS_CASE", goodCredsBody)
		medium := fileMedium("")
		medium.Credentials = transport.MediumCredentials{Env: "MEDIUM_CREDS_FOR_THIS_CASE"}
		if _, err := mediumAuthOptions(medium); err != nil {
			t.Fatalf("the env source was refused because of an ambient variable it cannot be reached by: %v", err)
		}
	})
}

// TestTheFileSourceRefusesAFileWithNoDefaultProfile is the guard on a
// failure that does not fail: it HANGS.
//
// A credentials file whose only profile is named anything but `default`
// is not rejected by the chain. It falls through to EC2 instance metadata,
// and on a host that is not an EC2 instance that is a connection to
// 169.254.169.254 which nothing answers, so the operation stalls for as
// long as the caller allows. Measured against a real MinIO with a
// 12-second deadline: 7ms for [default], 12.002s for a named profile. A
// deployment experiences that as "backups got slow", never as a typo in a
// profile name.
func TestTheFileSourceRefusesAFileWithNoDefaultProfile(t *testing.T) {
	for _, name := range ambientAWSCredentialEnvVars {
		t.Setenv(name, "")
	}

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"a named profile", "[cold-storage]\naws_access_key_id = AKIA\naws_secret_access_key = s\n", "cold-storage"},
		{"an SDK-style profile prefix", "[profile cold-storage]\naws_access_key_id = AKIA\naws_secret_access_key = s\n", "profile cold-storage"},
		{"no header at all", "aws_access_key_id = AKIA\naws_secret_access_key = s\n", "no [profile] header"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mediumAuthOptions(fileMedium(writeCreds(t, tc.body)))
			if err == nil {
				t.Fatal("a credentials file the AWS chain cannot resolve was accepted. It does not fail, it stalls on " +
					"instance metadata until the operation times out, which reads as a slow backup and not as a typo")
			}
			if category, ok := transport.CategoryOf(err); !ok || category != transport.Configuration {
				t.Errorf("classified as %v (recognised=%v), want %s", category, ok, transport.Configuration)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not tell the operator what to rename (%q missing): %v", tc.want, err)
			}
		})
	}

	t.Run("[default] anywhere in the file is accepted", func(t *testing.T) {
		body := "[cold-storage]\naws_access_key_id = AKIA\n\n[default]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = sekrit\n"
		if _, err := mediumAuthOptions(fileMedium(writeCreds(t, body))); err != nil {
			t.Fatalf("a file that does carry a [default] profile was refused: %v", err)
		}
	})
}

// TestCredentialsFileProfileCheckReadsNoValues holds the profile check to
// the promise that makes `file` preferred at all.
//
// This is the ONE place this process opens a credentials file. It reads
// lines to find HEADERS, keeps only header names, and compares them
// against one literal. Nothing else in the file may reach the refusal, in
// whole or in part: not a setting's name, not its value.
func TestCredentialsFileProfileCheckReadsNoValues(t *testing.T) {
	for _, name := range ambientAWSCredentialEnvVars {
		t.Setenv(name, "")
	}
	// A file full of things a less careful reader would echo, none of
	// which is a [default] profile, so the refusal path is the one taken.
	body := strings.Join([]string{
		"[cold-storage]",
		"aws_access_key_id = " + canary + "-KEYID",
		"aws_secret_access_key = " + canary + "-SECRET",
		"aws_session_token = " + canary + "-TOKEN",
		"role_arn = arn:aws:iam::" + canary + ":role/nope",
		"region = " + canary + "-region",
		"",
	}, "\n")

	_, err := mediumAuthOptions(fileMedium(writeCreds(t, body)))
	if err == nil {
		t.Fatal("the file was accepted, so this test proved nothing about the refusal's content")
	}
	for _, forbidden := range []string{canary, "aws_access_key_id", "aws_secret_access_key", "aws_session_token", "role_arn", "region"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("the refusal carries %q out of the credentials file; it may report profile HEADER names and nothing else: %v", forbidden, err)
		}
	}
	// The positive control: it does report the header name, which is the
	// whole content of the fix, so the assertions above are measuring
	// restraint rather than an error that says nothing.
	if !strings.Contains(err.Error(), "cold-storage") {
		t.Errorf("the refusal does not name the profile to rename, so an operator cannot act on it: %v", err)
	}
}

// TestResolvedCredentialsRedactUnderEveryVerb closes the hole obs.Secret
// cannot: it does not protect a value in an UNEXPORTED field, and every
// field of resolvedCredentials is unexported.
//
// Measured on this type before its Format/String/MarshalJSON/LogValue
// existed:
//
//	%+v => {accessKeyID:{v:AKIAPROBEKEY} secretAccessKey:{v:probesecretvalue} ...}
//
// That is FR-33 defeated by a reflex %+v in a debug statement.
func TestResolvedCredentialsRedactUnderEveryVerb(t *testing.T) {
	creds := resolvedCredentials{
		accessKeyID:     obs.NewSecret(canary + "-KEYID"),
		secretAccessKey: obs.NewSecret(canary + "-SECRET"),
		sessionToken:    obs.NewSecret(canary + "-TOKEN"),
		hasSessionToken: true,
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X"} {
		rendered := fmt.Sprintf(verb, creds)
		if strings.Contains(rendered, canary) {
			t.Errorf("%s rendered the resolved credential in the clear: %s", verb, rendered)
		}
	}
	// Reveal still works, because the whole point is that reaching the
	// value takes a deliberate act rather than a formatting verb.
	if creds.accessKeyID.Reveal() != canary+"-KEYID" {
		t.Error("redaction broke the only supported way to get the value out")
	}
}
