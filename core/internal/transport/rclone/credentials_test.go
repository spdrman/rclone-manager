package rclone

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// canary is the value every test in this file plants and then hunts for.
// FR-33's enforcement is that a resolved credential appears in no log line,
// no error message, no API response and no returned value, and the way to
// prove that of a resolver is to give it a value nothing else in the
// process could produce and then look for it everywhere the resolver can
// speak.
//
// It is a literal in a test file, which is the one place a credential-shaped
// string is allowed to live: nothing here reaches a real endpoint and
// nothing is written anywhere it outlives the test.
const (
	canarySecret = "CANARY-SECRET-2fbd41e6-nothing-else-produces-this"
	canaryKeyID  = "CANARYKEYID7QF3XPLA"
	canaryToken  = "CANARY-SESSION-TOKEN-8813aa02"
)

func canaryCredentialsINI() string {
	return fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n", canaryKeyID, canarySecret)
}

// mediumWith builds a Medium carrying one credential source and otherwise
// enough to be well-formed. Nothing here connects to anything.
func mediumWith(creds transport.MediumCredentials) transport.Medium {
	return transport.Medium{
		ID:           "cold",
		Type:         transport.MediumTypeS3,
		Region:       "us-east-1",
		Endpoint:     "https://minio.invalid:9000",
		Bucket:       "nas-backups",
		Prefix:       "rclone-manager",
		StorageClass: "STANDARD",
		Credentials:  creds,
	}
}

// writeCredentialsFile writes an AWS shared-credentials file at 0600 inside
// a 0700 directory, which is the posture resolveMediumCredentials requires.
func writeCredentialsFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod temp dir: %v", err)
	}
	path := filepath.Join(dir, "s3.credentials")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing credentials file: %v", err)
	}
	return path
}

// --- exactly one source, the sftpConfig backstop transposed ---

func TestResolveMediumCredentials_RequiresExactlyOneSource(t *testing.T) {
	for _, tc := range []struct {
		name  string
		creds transport.MediumCredentials
	}{
		{"none at all", transport.MediumCredentials{}},
		{"file and env", transport.MediumCredentials{File: "/tmp/x", Env: "X"}},
		{"env and command", transport.MediumCredentials{Env: "X", Command: []string{"/bin/true"}}},
		{"all three", transport.MediumCredentials{File: "/tmp/x", Env: "X", Command: []string{"/bin/true"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveMediumCredentials(mediumWith(tc.creds)); err == nil {
				t.Fatal("resolveMediumCredentials accepted a medium that does not name exactly one credential source")
			}
		})
	}
}

// --- the file source: rclone reads it, this process does not ---

func TestResolveMediumCredentials_FileNeverReadsTheSecret(t *testing.T) {
	path := writeCredentialsFile(t, canaryCredentialsINI())
	got, err := resolveMediumCredentials(mediumWith(transport.MediumCredentials{File: path}))
	if err != nil {
		t.Fatalf("resolveMediumCredentials: %v", err)
	}
	if !got.usingSharedFile {
		t.Fatal("the file source did not select the shared-credentials-file path; rclone has to be the one that opens it")
	}
	if got.sharedFile != path {
		t.Errorf("sharedFile = %q, want %q", got.sharedFile, path)
	}
	if got.accessKeyID.Reveal() != "" || got.secretAccessKey.Reveal() != "" {
		t.Error("the file source put credential material into this process's memory; the whole reason it is the preferred source is that it does not")
	}
}

func TestResolveMediumCredentials_FileRefusesAWiderMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			path := writeCredentialsFile(t, canaryCredentialsINI())
			if err := os.Chmod(path, mode); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			_, err := resolveMediumCredentials(mediumWith(transport.MediumCredentials{File: path}))
			if err == nil {
				t.Fatalf("a credentials file at %04o was accepted; anyone on the host can read every retained artifact with it", mode)
			}
			if !strings.Contains(err.Error(), "permissions") {
				t.Errorf("error %q does not name the permission problem", err)
			}
		})
	}
}

// TestResolveMediumCredentials_FileAcceptsANarrowerMode is the other half.
// The SSH key's own check is an EXACT match on 0600 because this program
// wrote that file itself and drift means something changed it. Nothing
// writes a credentials file yet, so an operator's own 0400 is a legitimate
// spelling and refusing it would be refusing correct configuration.
func TestResolveMediumCredentials_FileAcceptsANarrowerMode(t *testing.T) {
	path := writeCredentialsFile(t, canaryCredentialsINI())
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := resolveMediumCredentials(mediumWith(transport.MediumCredentials{File: path})); err != nil {
		t.Fatalf("a credentials file at 0400 was refused: %v", err)
	}
}

func TestResolveMediumCredentials_FileRefusesAWorldWritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root defeats a directory-mode check")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "s3.credentials")
	if err := os.WriteFile(path, []byte(canaryCredentialsINI()), 0o600); err != nil {
		t.Fatalf("writing credentials file: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	_, err := resolveMediumCredentials(mediumWith(transport.MediumCredentials{File: path}))
	if err == nil {
		t.Fatal("a credentials file inside a world-writable directory was accepted; any local actor can swap it for their own")
	}
}

func TestResolveMediumCredentials_FileMustExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there")
	if _, err := resolveMediumCredentials(mediumWith(transport.MediumCredentials{File: missing})); err == nil {
		t.Fatal("a missing credentials file was accepted")
	}
}

// --- the env and command sources: validated by shape, wrapped, never echoed ---

func TestResolveMediumCredentials_Env(t *testing.T) {
	t.Setenv("RCLONE_MANAGER_TEST_S3_CREDS", canaryCredentialsINI())
	got, err := resolveMediumCredentials(mediumWith(transport.MediumCredentials{Env: "RCLONE_MANAGER_TEST_S3_CREDS"}))
	if err != nil {
		t.Fatalf("resolveMediumCredentials: %v", err)
	}
	if got.usingSharedFile {
		t.Error("the env source selected the shared-file path")
	}
	if got.accessKeyID.Reveal() != canaryKeyID {
		t.Errorf("accessKeyID did not round-trip")
	}
	if got.secretAccessKey.Reveal() != canarySecret {
		t.Errorf("secretAccessKey did not round-trip")
	}
	if got.hasSessionToken {
		t.Error("a credentials block with no session token reported one")
	}
}

func TestResolveMediumCredentials_EnvWithSessionToken(t *testing.T) {
	body := canaryCredentialsINI() + "aws_session_token = " + canaryToken + "\n"
	t.Setenv("RCLONE_MANAGER_TEST_S3_CREDS", body)
	got, err := resolveMediumCredentials(mediumWith(transport.MediumCredentials{Env: "RCLONE_MANAGER_TEST_S3_CREDS"}))
	if err != nil {
		t.Fatalf("resolveMediumCredentials: %v", err)
	}
	if !got.hasSessionToken || got.sessionToken.Reveal() != canaryToken {
		t.Error("aws_session_token did not round-trip")
	}
}

func TestResolveMediumCredentials_EnvUnset(t *testing.T) {
	_ = os.Unsetenv("RCLONE_MANAGER_TEST_S3_CREDS_ABSENT")
	_, err := resolveMediumCredentials(mediumWith(transport.MediumCredentials{Env: "RCLONE_MANAGER_TEST_S3_CREDS_ABSENT"}))
	if err == nil {
		t.Fatal("an unset environment variable was accepted")
	}
	if !strings.Contains(err.Error(), "RCLONE_MANAGER_TEST_S3_CREDS_ABSENT") {
		t.Errorf("error %q does not name the missing variable, which is the one thing an operator needs to fix it", err)
	}
}

func TestResolveMediumCredentials_Command(t *testing.T) {
	script := writeScript(t, "#!/bin/sh\ncat <<'EOF'\n"+canaryCredentialsINI()+"EOF\n")
	got, err := resolveMediumCredentials(mediumWith(transport.MediumCredentials{Command: []string{script}}))
	if err != nil {
		t.Fatalf("resolveMediumCredentials: %v", err)
	}
	if got.secretAccessKey.Reveal() != canarySecret {
		t.Error("the command resolver did not round-trip the secret")
	}
}

// TestResolveMediumCredentials_CommandArgvIsNeverAShell mirrors the SSH
// key resolver's own proof. A metacharacter-laden argument has to arrive at
// the executable as one literal byte sequence, never as a second command,
// and the marker file is what makes "never ran" a fact rather than an
// absence of evidence.
func TestResolveMediumCredentials_CommandArgvIsNeverAShell(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "marker")
	credsPath := writeCredentialsFile(t, canaryCredentialsINI())
	script := writeScript(t, "#!/bin/sh\n# $1 is the injected argument, $2 the marker, $3 the credentials\ncat \"$3\"\n")
	injected := "; touch " + markerPath + " ; echo pwned"

	got, err := resolveMediumCredentials(mediumWith(transport.MediumCredentials{
		Command: []string{script, injected, markerPath, credsPath},
	}))
	if err != nil {
		t.Fatalf("resolveMediumCredentials with a metacharacter-laden argument: %v", err)
	}
	if got.secretAccessKey.Reveal() != canarySecret {
		t.Error("the resolver did not return the credentials the command printed")
	}
	if _, err := os.Stat(markerPath); err == nil {
		t.Fatal("the injected argument was interpreted by a shell; exec.CommandContext must never invoke one")
	}
}

func TestResolveMediumCredentials_CommandSurfacesStderrButNeverStdout(t *testing.T) {
	script := writeScript(t, "#!/bin/sh\necho '"+canaryCredentialsINI()+"'\necho 'vault: not authenticated' >&2\nexit 1\n")
	_, err := resolveMediumCredentials(mediumWith(transport.MediumCredentials{Command: []string{script}}))
	if err == nil {
		t.Fatal("a failing credentials command was accepted")
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Errorf("error %q does not surface the command's stderr, which is the diagnostic an operator needs", err)
	}
	assertNoCanary(t, err.Error(), "the error from a failing credentials command")
}

func TestResolveMediumCredentials_CommandTimeoutIsEnforced(t *testing.T) {
	restore := resolverCommandTimeout
	resolverCommandTimeout = 250 * time.Millisecond
	t.Cleanup(func() { resolverCommandTimeout = restore })

	script := writeScript(t, "#!/bin/sh\nsleep 30\n")
	start := time.Now()
	_, err := resolveMediumCredentials(mediumWith(transport.MediumCredentials{Command: []string{script}}))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a hanging credentials command was accepted")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error %q does not mention the timeout", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("resolveMediumCredentials took %s; the timeout does not appear to have been enforced", elapsed)
	}
}

func TestResolveMediumCredentials_CommandOutputIsBounded(t *testing.T) {
	_, err := resolveMediumCredentials(mediumWith(transport.MediumCredentials{
		Command: []string{"/bin/dd", "if=/dev/zero", "bs=1024", "count=2048"},
	}))
	if err == nil {
		t.Fatal("an unbounded credentials command output was accepted")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error %q does not explain the size refusal", err)
	}
}

// --- shape validation, and the rule that a failure never echoes the bytes ---

func TestParseSharedCredentials_RefusesEveryMalformedShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"empty", "", "empty"},
		{"no profile section", "aws_access_key_id = " + canaryKeyID + "\naws_secret_access_key = " + canarySecret + "\n", "profile"},
		{"two profile sections", "[default]\naws_access_key_id = A\naws_secret_access_key = " + canarySecret + "\n[other]\naws_access_key_id = B\naws_secret_access_key = " + canarySecret + "\n", "profile"},
		{"missing the access key id", "[default]\naws_secret_access_key = " + canarySecret + "\n", "aws_access_key_id"},
		{"missing the secret", "[default]\naws_access_key_id = " + canaryKeyID + "\n", "aws_secret_access_key"},
		{"an empty secret", "[default]\naws_access_key_id = " + canaryKeyID + "\naws_secret_access_key =\n", "aws_secret_access_key"},
		{"an unknown key", "[default]\naws_access_key_id = " + canaryKeyID + "\naws_secret_access_key = " + canarySecret + "\nregion = us-east-1\n", "region"},
		{"a JSON blob a secrets manager returned instead", `{"AccessKeyId":"` + canaryKeyID + `","SecretAccessKey":"` + canarySecret + `"}`, ""},
		{"an HTML login page", "<html><body>please sign in</body></html>", ""},
		{"a bare line with no equals", "[default]\nnonsense\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateAndWrapCredentials([]byte(tc.body))
			if err == nil {
				t.Fatalf("validateAndWrapCredentials accepted %q", tc.name)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			assertNoCanary(t, err.Error(), "a shape-validation error for "+tc.name)
		})
	}
}

// TestParseSharedCredentials_AcceptsTheSpellingsAFileActuallyUses keeps the
// shape check from being so strict it refuses a real credentials file:
// comments, blank lines, CRLF from a file that travelled through Windows,
// and no spaces around the equals are all ordinary.
func TestParseSharedCredentials_AcceptsTheSpellingsAFileActuallyUses(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no spaces around equals", "[default]\naws_access_key_id=" + canaryKeyID + "\naws_secret_access_key=" + canarySecret + "\n"},
		{"comments and blank lines", "# my medium\n\n[default]\n\n; another comment\naws_access_key_id = " + canaryKeyID + "\naws_secret_access_key = " + canarySecret + "\n\n"},
		{"CRLF line endings", "[default]\r\naws_access_key_id = " + canaryKeyID + "\r\naws_secret_access_key = " + canarySecret + "\r\n"},
		{"a profile that is not called default", "[cold-storage]\naws_access_key_id = " + canaryKeyID + "\naws_secret_access_key = " + canarySecret + "\n"},
		{"upper-case keys", "[default]\nAWS_ACCESS_KEY_ID = " + canaryKeyID + "\nAWS_SECRET_ACCESS_KEY = " + canarySecret + "\n"},
		{"no trailing newline", "[default]\naws_access_key_id = " + canaryKeyID + "\naws_secret_access_key = " + canarySecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateAndWrapCredentials([]byte(tc.body))
			if err != nil {
				t.Fatalf("validateAndWrapCredentials refused an ordinary credentials file (%s): %v", tc.name, err)
			}
			if got.secretAccessKey.Reveal() != canarySecret {
				t.Errorf("secret did not round-trip for %s", tc.name)
			}
		})
	}
}

// --- the canary: every observable output of every source ---

// TestResolvedCredentialsRenderRedacted is FR-33's structural half. A
// resolved credential set is a value this adapter holds, so the thing that
// keeps it out of a log line is that every rendering of it is a
// placeholder, not a rule somebody has to remember at each call site.
func TestResolvedCredentialsRenderRedacted(t *testing.T) {
	t.Setenv("RCLONE_MANAGER_TEST_S3_CREDS", canaryCredentialsINI()+"aws_session_token = "+canaryToken+"\n")
	got, err := resolveMediumCredentials(mediumWith(transport.MediumCredentials{Env: "RCLONE_MANAGER_TEST_S3_CREDS"}))
	if err != nil {
		t.Fatalf("resolveMediumCredentials: %v", err)
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		assertNoCanary(t, fmt.Sprintf(verb, got), "a resolvedCredentials rendered with "+verb)
		assertNoCanary(t, fmt.Sprintf(verb, got.secretAccessKey), "a secretAccessKey rendered with "+verb)
		assertNoCanary(t, fmt.Sprintf(verb, got.accessKeyID), "an accessKeyID rendered with "+verb)
		assertNoCanary(t, fmt.Sprintf(verb, got.sessionToken), "a sessionToken rendered with "+verb)
	}
}

// assertNoCanary is the single check every canary assertion in this package
// funnels through, so "absent from every observable output" means the same
// thing everywhere and gains a new needle in one edit.
func assertNoCanary(t *testing.T, rendered, what string) {
	t.Helper()
	for _, needle := range []string{canarySecret, canaryKeyID, canaryToken} {
		if strings.Contains(rendered, needle) {
			t.Errorf("%s contains a resolved credential; FR-33 says it may appear in no log line, no error message and no returned value", what)
		}
	}
	// A rendering that is empty proves nothing, and an assertion that
	// cannot fail is worse than no assertion. Every caller here renders
	// something.
	if rendered == "" {
		t.Errorf("%s rendered as the empty string, so this check had nothing to look at", what)
	}
}

// writeScript writes an executable shell script into a fresh temp dir.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resolver.sh")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("writing script: %v", err)
	}
	return path
}
