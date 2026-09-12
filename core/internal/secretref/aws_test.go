package secretref_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/secretref"
)

const (
	testAccessKeyID = "AKIACANARYKEYID12345"
	testSecretKey   = "canary-secret-access-key-7c1d9e"
	testSession     = "canary-session-token-4b8f"
)

func credentialsFile(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing credentials file: %v", err)
	}

	return path
}

// TestResolveAWSRoundTrip is the shape an operator actually writes: the
// shared-credentials file the storage medium already points at, resolved
// for a repository on the same bucket.
func TestResolveAWSRoundTrip(t *testing.T) {
	t.Parallel()

	path := credentialsFile(t, "[default]\naws_access_key_id = "+testAccessKeyID+
		"\naws_secret_access_key = "+testSecretKey+"\n")

	got, err := secretref.ResolveAWS(context.Background(), secretref.Ref{File: path})
	if err != nil {
		t.Fatalf("ResolveAWS: %v", err)
	}

	if got.AccessKeyID != testAccessKeyID {
		t.Errorf("AccessKeyID = %q, want %q", got.AccessKeyID, testAccessKeyID)
	}

	if got.SecretAccessKey.Reveal() != testSecretKey {
		t.Errorf("the secret access key did not survive the round trip")
	}

	if got.HasSession {
		t.Errorf("HasSession is true for credentials that declared no session token")
	}
}

// TestResolveAWSCarriesASessionToken covers the temporary-credential case,
// and the reason it is a separate assertion is HasSession: an empty token
// and an absent one are different requests to make of a provider.
func TestResolveAWSCarriesASessionToken(t *testing.T) {
	t.Parallel()

	path := credentialsFile(t, "[default]\naws_access_key_id = "+testAccessKeyID+
		"\naws_secret_access_key = "+testSecretKey+"\naws_session_token = "+testSession+"\n")

	got, err := secretref.ResolveAWS(context.Background(), secretref.Ref{File: path})
	if err != nil {
		t.Fatalf("ResolveAWS: %v", err)
	}

	if !got.HasSession || got.SessionToken.Reveal() != testSession {
		t.Errorf("the session token did not survive the round trip (HasSession=%v)", got.HasSession)
	}
}

// TestParseSharedCredentialsRefusals is the whole point of not using a
// general INI parser: each of these is a real thing a secrets manager or a
// misconfigured file hands back, and every one of them must fail HERE
// rather than as an AccessDenied nobody can trace to a bad resolver.
func TestParseSharedCredentialsRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"empty", "", secretref.ErrCredentialsUnreadable},
		{"an error string", "error: permission denied\n", secretref.ErrCredentialsUnreadable},
		{"an HTML login page", "<html><body>Please log in</body></html>", secretref.ErrCredentialsUnreadable},
		{"a JSON blob", `{"AccessKeyId":"x","SecretAccessKey":"y"}`, secretref.ErrCredentialsUnreadable},
		{"keys before any profile", "aws_access_key_id = x\naws_secret_access_key = y\n", secretref.ErrCredentialsUnreadable},
		{"an empty profile header", "[]\naws_access_key_id = x\n", secretref.ErrCredentialsUnreadable},
		{
			"two profiles and no default",
			"[staging]\naws_access_key_id = a\naws_secret_access_key = b\n[prod]\naws_access_key_id = c\naws_secret_access_key = d\n",
			secretref.ErrCredentialsAmbiguousProfile,
		},
		{"no secret key", "[default]\naws_access_key_id = x\n", secretref.ErrCredentialsIncomplete},
		{"no key id", "[default]\naws_secret_access_key = y\n", secretref.ErrCredentialsIncomplete},
		{
			"a wrapped value",
			"[default]\naws_access_key_id = x\naws_secret_access_key = first half second half\n",
			secretref.ErrCredentialsMalformedValue,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := secretref.ParseSharedCredentials([]byte(tc.raw))
			if !errors.Is(err, tc.want) {
				t.Fatalf("ParseSharedCredentials() = %v, want %v", err, tc.want)
			}

			if got != (secretref.AWSCredentials{}) {
				t.Errorf("a refusal also returned credentials")
			}
		})
	}
}

// TestParseSharedCredentialsAcceptsOneNamedProfile is the counterpart to
// the ambiguity refusal: a file with exactly one profile is unambiguous
// whatever it is called, and refusing it would break every operator whose
// secrets manager emits [backupd] instead of [default].
func TestParseSharedCredentialsAcceptsOneNamedProfile(t *testing.T) {
	t.Parallel()

	got, err := secretref.ParseSharedCredentials([]byte(
		"; a comment\n# another\n[backupd]\naws_access_key_id = " + testAccessKeyID +
			"\naws_secret_access_key = " + testSecretKey + "\n"))
	if err != nil {
		t.Fatalf("ParseSharedCredentials: %v", err)
	}

	if got.AccessKeyID != testAccessKeyID {
		t.Errorf("AccessKeyID = %q, want %q", got.AccessKeyID, testAccessKeyID)
	}
}

// TestAWSCredentialsNeverRenderTheSecret walks every rendering path a
// credential set can reach by accident: a reflexive %+v in a debug
// statement, a %#v from somebody reaching for structure, a JSON response,
// and a structured log field. All four used to be the way FR-33's "never
// in a log line, in whole or in part" gets defeated.
func TestAWSCredentialsNeverRenderTheSecret(t *testing.T) {
	t.Parallel()

	creds, err := secretref.ResolveAWS(context.Background(), secretref.Ref{
		File: credentialsFile(t, "[default]\naws_access_key_id = "+testAccessKeyID+
			"\naws_secret_access_key = "+testSecretKey+"\naws_session_token = "+testSession+"\n"),
	})
	if err != nil {
		t.Fatalf("ResolveAWS: %v", err)
	}

	encoded, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var logged strings.Builder
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("opening repository", "credentials", creds)

	for _, rendering := range []struct {
		label string
		text  string
	}{
		{"%v", fmt.Sprintf("%v", creds)},
		{"%+v", fmt.Sprintf("%+v", creds)},
		{"%#v", fmt.Sprintf("%#v", creds)},
		{"json", string(encoded)},
		{"slog", logged.String()},
	} {
		if strings.Contains(rendering.text, testSecretKey) {
			t.Errorf("%s rendering leaks the secret access key: %s", rendering.label, rendering.text)
		}

		if strings.Contains(rendering.text, testSession) {
			t.Errorf("%s rendering leaks the session token: %s", rendering.label, rendering.text)
		}
	}
}

// TestResolveAWSErrorsNameTheSourceNotTheMaterial is the leak assertion on
// the failure path, which is the one with material in hand: the bytes that
// failed to parse are right there, and quoting them back is the obvious,
// helpful-looking mistake.
func TestResolveAWSErrorsNameTheSourceNotTheMaterial(t *testing.T) {
	t.Parallel()

	// Credentials text that IS credential-shaped in part -- a real key id
	// and secret -- but is not parseable, so the refusal is built while
	// holding material.
	path := credentialsFile(t, "aws_access_key_id = "+testAccessKeyID+
		"\naws_secret_access_key = "+testSecretKey+"\n")

	_, err := secretref.ResolveAWS(context.Background(), secretref.Ref{File: path})
	if err == nil {
		t.Fatalf("ResolveAWS accepted credentials text with no profile header")
	}

	if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal does not name the file to go and fix: %v", err)
	}

	if strings.Contains(err.Error(), testSecretKey) {
		t.Errorf("the refusal echoes the secret access key: %v", err)
	}

	if strings.Contains(err.Error(), testAccessKeyID) {
		t.Errorf("the refusal echoes the access key id: %v", err)
	}
}
