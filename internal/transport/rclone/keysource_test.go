package rclone

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/internal/obs"
)

// mustUnencryptedKeyPEM generates a fresh, unencrypted ed25519 private key
// and returns its PEM bytes. It reuses ssh_test.go's
// generateClientSSHKeyPair (same package, same Docker-fixture client-key
// generation every other test here already trusts) rather than a second,
// slightly different way of producing test key material.
func mustUnencryptedKeyPEM(t *testing.T) []byte {
	t.Helper()
	path, _ := generateClientSSHKeyPair(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated test key: %v", err)
	}
	return data
}

// mustEncryptedKeyPEM generates a real, passphrase-protected SSH private
// key with ssh-keygen (the same external dependency tests/sftpfixture
// already requires) so the encrypted-key tests exercise actual encrypted
// bytes, not a hand-rolled approximation of the format. legacyPEM selects
// the old "BEGIN RSA PRIVATE KEY" / "Proc-Type: 4,ENCRYPTED" format
// (ssh-keygen -m PEM, RSA only) instead of the default new-style
// "BEGIN OPENSSH PRIVATE KEY" container, so both of the two encrypted
// shapes x/crypto/ssh.ParseRawPrivateKey knows about are actually covered.
func mustEncryptedKeyPEM(t *testing.T, legacyPEM bool) []byte {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not found in PATH, skipping")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "encrypted_key")
	args := []string{"-q", "-N", "correct-horse-battery-staple", "-C", "", "-f", path}
	if legacyPEM {
		args = append(args, "-t", "rsa", "-b", "2048", "-m", "PEM")
	} else {
		args = append(args, "-t", "ed25519")
	}
	if out, err := exec.Command("ssh-keygen", args...).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated encrypted key: %v", err)
	}
	return data
}

// --- validateAndWrapKey: the shared validation both resolvers funnel through ---

func TestValidateAndWrapKey_AcceptsUnencryptedKey(t *testing.T) {
	pem := mustUnencryptedKeyPEM(t)
	secret, err := validateAndWrapKey(pem)
	if err != nil {
		t.Fatalf("validateAndWrapKey: %v", err)
	}
	if secret.Reveal() != string(pem) {
		t.Fatalf("Reveal() did not round-trip the original key bytes")
	}
}

// TestValidateAndWrapKey_RejectsJunk is #74's "a resolver returning junk is
// refused before rclone sees it" requirement: an error string, an HTML
// login page, and an empty body are exactly the three shapes the issue
// calls out for a secrets manager answering a failed auth.
func TestValidateAndWrapKey_RejectsJunk(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"empty", []byte("")},
		{"whitespace only", []byte("   \n\t  \n")},
		{"error string", []byte("Error: not authenticated, please run `vault login` first")},
		{"html login page", []byte("<html><head><title>Sign in</title></head><body><form>Please log in</form></body></html>")},
		{"plain garbage", []byte("this is not a key, just some bytes a broken resolver produced")},
		{"truncated pem", []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1r\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secret, err := validateAndWrapKey(tc.raw)
			if err == nil {
				t.Fatalf("junk input was accepted as a valid key")
			}
			if secret.Reveal() != "" {
				t.Fatalf("a rejected input still produced non-empty resolved material")
			}
			// The rejection itself must never echo the junk back: it could
			// just as well have been a partially-correct secret.
			if len(tc.raw) > 0 && strings.Contains(err.Error(), string(tc.raw)) {
				t.Fatalf("error echoed the raw input back: %v", err)
			}
		})
	}
}

// TestValidateAndWrapKey_RejectsEncryptedKey is #74's "refuse a
// passphrase-protected key with a clear message rather than hanging on a
// prompt nobody will answer" requirement, against both encrypted PEM shapes
// x/crypto/ssh understands.
func TestValidateAndWrapKey_RejectsEncryptedKey(t *testing.T) {
	for _, tc := range []struct {
		name      string
		legacyPEM bool
	}{
		{"new OpenSSH format", false},
		{"legacy PEM format", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := mustEncryptedKeyPEM(t, tc.legacyPEM)
			secret, err := validateAndWrapKey(raw)
			if err == nil {
				t.Fatal("an encrypted key was accepted")
			}
			if secret.Reveal() != "" {
				t.Fatal("a rejected encrypted key still produced non-empty resolved material")
			}
			if !strings.Contains(err.Error(), "passphrase") {
				t.Fatalf("error %q does not name the actual problem (passphrase-protected)", err.Error())
			}
		})
	}
}

// --- resolveKeyFromEnv ---

func TestResolveKeyFromEnv_Succeeds(t *testing.T) {
	pem := mustUnencryptedKeyPEM(t)
	const name = "RCLONE_MANAGER_TEST_KEY_ENV"
	t.Setenv(name, string(pem))

	secret, err := resolveKeyFromEnv(name)
	if err != nil {
		t.Fatalf("resolveKeyFromEnv: %v", err)
	}
	if secret.Reveal() != string(pem) {
		t.Fatal("resolved material does not match the environment variable's content")
	}
}

func TestResolveKeyFromEnv_RejectsMissingVariable(t *testing.T) {
	const name = "RCLONE_MANAGER_TEST_KEY_ENV_DOES_NOT_EXIST"
	if _, ok := os.LookupEnv(name); ok {
		t.Fatalf("test precondition broken: %s is actually set in this environment", name)
	}
	_, err := resolveKeyFromEnv(name)
	if err == nil {
		t.Fatal("an unset environment variable was accepted")
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("error %q does not name the missing variable", err.Error())
	}
}

func TestResolveKeyFromEnv_RejectsJunkContent(t *testing.T) {
	const name = "RCLONE_MANAGER_TEST_KEY_ENV_JUNK"
	t.Setenv(name, "<html>not a key</html>")
	_, err := resolveKeyFromEnv(name)
	if err == nil {
		t.Fatal("junk environment content was accepted as a key")
	}
	if strings.Contains(err.Error(), "<html>") {
		t.Fatalf("error echoed the raw environment value back: %v", err)
	}
}

// --- resolveKeyFromCommand ---

func TestResolveKeyFromCommand_Succeeds(t *testing.T) {
	pem := mustUnencryptedKeyPEM(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, pem, 0o600); err != nil {
		t.Fatalf("writing test key: %v", err)
	}

	secret, err := resolveKeyFromCommand([]string{"/bin/cat", keyPath})
	if err != nil {
		t.Fatalf("resolveKeyFromCommand: %v", err)
	}
	if secret.Reveal() != string(pem) {
		t.Fatal("resolved material does not match the command's stdout")
	}
}

func TestResolveKeyFromCommand_RejectsEmptyArgv(t *testing.T) {
	if _, err := resolveKeyFromCommand(nil); err == nil {
		t.Fatal("an empty argv was accepted")
	}
	if _, err := resolveKeyFromCommand([]string{""}); err == nil {
		t.Fatal("an argv with an empty executable was accepted")
	}
}

// TestResolveKeyFromCommand_NonZeroExitSurfacesStderr is the "an error
// string... must fail loudly" case for a resolver that fails the honest
// way (a non-zero exit), as opposed to one that exits 0 with junk on
// stdout (covered by TestResolveKeyFromCommand_RejectsJunkStdout below).
// Stderr is diagnostic text, not secret-shaped, so unlike stdout it is
// expected to appear in the error.
func TestResolveKeyFromCommand_NonZeroExitSurfacesStderr(t *testing.T) {
	script := mustScript(t, `echo "Error: not authenticated" 1>&2
exit 1
`)
	_, err := resolveKeyFromCommand([]string{script})
	if err == nil {
		t.Fatal("a failing command was accepted")
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("error %q does not surface the command's stderr", err.Error())
	}
}

// TestResolveKeyFromCommand_RejectsJunkStdout is a secrets manager that
// exits 0 (no infrastructure error at all) but answers with something that
// is not a key: exactly the "HTML login page on failed auth" shape #74
// calls out, proven through the real command-execution path rather than
// validateAndWrapKey directly.
func TestResolveKeyFromCommand_RejectsJunkStdout(t *testing.T) {
	_, err := resolveKeyFromCommand([]string{"/bin/echo", "-n", "<html>please sign in</html>"})
	if err == nil {
		t.Fatal("a command exiting 0 with non-key stdout was accepted")
	}
	if strings.Contains(err.Error(), "please sign in") {
		t.Fatalf("error echoed the command's stdout back: %v", err)
	}
}

func TestResolveKeyFromCommand_RejectsOutputOverSizeLimit(t *testing.T) {
	// /bin/dd, not a shell pipeline: this is still our own trusted argv,
	// exercising the truncation path with a real oversized subprocess
	// rather than asserting on boundedBuffer in isolation.
	_, err := resolveKeyFromCommand([]string{"/bin/dd", "if=/dev/zero", "bs=1024", "count=2048"})
	if err == nil {
		t.Fatal("output far exceeding the size limit was accepted")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("error %q does not explain the size refusal", err.Error())
	}
}

func TestResolveKeyFromCommand_Timeout(t *testing.T) {
	old := keyCommandTimeout
	keyCommandTimeout = 200 * time.Millisecond
	defer func() { keyCommandTimeout = old }()

	script := mustScript(t, "sleep 5\n")
	start := time.Now()
	_, err := resolveKeyFromCommand([]string{script})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a command exceeding its timeout was accepted")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("error %q does not mention the timeout", err.Error())
	}
	if elapsed > 4*time.Second {
		t.Fatalf("resolveKeyFromCommand took %s, the timeout does not appear to have been enforced", elapsed)
	}
}

// TestResolveKeyFromCommand_NoShellInjection is #74's explicit ask: prove a
// command resolver cannot be made to run a shell, by passing shell
// metacharacters and showing they are treated as literal argv.
//
// argvEcho.sh writes its first argument verbatim to a marker file, byte for
// byte, and only then cats a real key file to stdout, so this test proves
// two things in one real subprocess call: the crafted argument survived
// completely unparsed (the marker file's content), and it never did
// anything a shell would have done with it, other than what argvEcho.sh
// itself asked (there IS a shell involved, but it is argvEcho.sh's own
// interpreter, invoked directly as argv[0] with fixed, trusted arguments of
// its own; nothing this program builds ever passes a whole command line to
// a shell to parse).
func TestResolveKeyFromCommand_NoShellInjection(t *testing.T) {
	dir := t.TempDir()

	script := mustScript(t, `printf '%s' "$1" > "$2"
cat "$3"
`)

	keyPath := filepath.Join(dir, "key.pem")
	pem := mustUnencryptedKeyPEM(t)
	if err := os.WriteFile(keyPath, pem, 0o600); err != nil {
		t.Fatalf("writing test key: %v", err)
	}

	markerPath := filepath.Join(dir, "marker")
	canaryPath := filepath.Join(dir, "should-not-be-deleted")
	pwnedPath := filepath.Join(dir, "pwned")
	if err := os.WriteFile(canaryPath, []byte("still here"), 0o644); err != nil {
		t.Fatalf("writing canary file: %v", err)
	}

	injected := "$(touch " + pwnedPath + "); `id` | rm -rf " + canaryPath + " #"

	secret, err := resolveKeyFromCommand([]string{script, injected, markerPath, keyPath})
	if err != nil {
		t.Fatalf("resolveKeyFromCommand with a metacharacter-laden argument: %v", err)
	}
	if secret.Reveal() != string(pem) {
		t.Fatal("the resolver did not still produce the real key despite the crafted argument")
	}

	got, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("reading marker file: %v", err)
	}
	if string(got) != injected {
		t.Fatalf("argv was not delivered literally: marker file = %q, want %q", got, injected)
	}

	if _, err := os.Stat(pwnedPath); err == nil {
		t.Fatal("the injected \"$(touch ...)\" was executed: a shell was involved somewhere it should not have been")
	}
	canaryContent, err := os.ReadFile(canaryPath)
	if err != nil || string(canaryContent) != "still here" {
		t.Fatal("the injected \"rm -rf\" ran: a shell was involved somewhere it should not have been")
	}
}

// TestResolvedKeyNeverAppearsInLogLine is #74's explicit ask, proven at the
// point where it would actually matter: both resolvers' successful return
// value, logged the ordinary way any of this project's own code would log
// a value (obs.Logger, at debug, the most verbose level there is), never
// renders the resolved key in the clear, at any level, in any of the
// several shapes slog can be asked to render a value in (bare, inside a
// group, via %v on the whole event).
func TestResolvedKeyNeverAppearsInLogLine(t *testing.T) {
	pem := mustUnencryptedKeyPEM(t)

	const envName = "RCLONE_MANAGER_TEST_KEY_ENV_LOGGING"
	t.Setenv(envName, string(pem))
	envSecret, err := resolveKeyFromEnv(envName)
	if err != nil {
		t.Fatalf("resolveKeyFromEnv: %v", err)
	}

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, pem, 0o600); err != nil {
		t.Fatalf("writing test key: %v", err)
	}
	cmdSecret, err := resolveKeyFromCommand([]string{"/bin/cat", keyPath})
	if err != nil {
		t.Fatalf("resolveKeyFromCommand: %v", err)
	}

	for _, tc := range []struct {
		name   string
		secret obs.Secret
	}{
		{"env resolver", envSecret},
		{"command resolver", cmdSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := obs.New(&buf, obs.LevelDebug)
			logger.Event(context.Background(), obs.LevelDebug, "test_event", "test",
				slog.Any("resolved_key", tc.secret),
				slog.Group("source", slog.Any("key", tc.secret)),
			)
			got := buf.String()
			if strings.Contains(got, string(pem)) {
				t.Fatalf("the resolved key appeared in a log line: %s", got)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("log line did not contain the expected redaction placeholder: %s", got)
			}
		})
	}
}

// mustScript writes an executable POSIX shell script and returns its
// absolute path, mirroring internal/lifecycle/verify_test.go's identical
// helper (a different package, so this is a deliberate small duplication
// rather than a new cross-package dependency).
func mustScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.sh")
	full := "#!/bin/sh\n" + body
	if err := os.WriteFile(path, []byte(full), 0o755); err != nil {
		t.Fatalf("writing script: %v", err)
	}
	return path
}
