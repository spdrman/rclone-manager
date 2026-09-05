package machinegate_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/machines"
)

// The three ways a private key can be named, each proved by completing a
// real SSH session with it.
//
// Every one of these resolvers can be unit tested against its own output,
// and that is exactly what makes this file necessary: a resolver that
// produced plausible-looking bytes no server would ever accept passes those
// tests and fails in front of an operator. The claim here is authentication,
// which is a server's answer and nobody else's.
//
// All three share one machine and one client key. The machine trusts that
// one key however it is named, so the same server proves all three routes
// without paying for three servers, and a failure is unambiguously about the
// resolver rather than about which fixture it drew.

// TestSFTPKeyResolvers is #74's positive control: an end-to-end SFTP
// connection through each of the three key resolvers against a real
// machine, proving they actually authenticate a real session rather than
// merely producing bytes that look plausible in a unit test. Without this,
// code that refused every resolver would still pass every other test in
// core/internal/transport/rclone.
//
// All three subtests share one machine and one client key: the machine's
// authorized_keys trusts exactly one key regardless of which resolver names
// it, so the same server proves all three.
func TestSFTPKeyResolvers(t *testing.T) {
	src := machines.Start(t).Source(t)
	keyPEM, err := os.ReadFile(src.KeyFile)
	if err != nil {
		t.Fatalf("reading the machine's client key: %v", err)
	}

	base := src.TransportSource("sftp-key-resolvers", "")
	base.KeyFile = ""
	adapter := rclone.New()
	ctx := context.Background()

	t.Run("file", func(t *testing.T) {
		s := base
		s.KeyFile = src.KeyFile
		if _, err := adapter.List(ctx, s); err != nil {
			t.Fatalf("List via the key_file resolver: %v\nthe machine's own connection table:\n%s", err, src.ConnectionTable(t))
		}
	})

	t.Run("env", func(t *testing.T) {
		const envName = "RCLONE_MANAGER_TEST_SFTP_KEY_ENV"
		t.Setenv(envName, string(keyPEM))
		s := base
		s.KeyEnv = envName
		if _, err := adapter.List(ctx, s); err != nil {
			t.Fatalf("List via the key_env resolver: %v", err)
		}
	})

	t.Run("command", func(t *testing.T) {
		s := base
		s.KeyCommand = []string{"/bin/cat", src.KeyFile}
		if _, err := adapter.List(ctx, s); err != nil {
			t.Fatalf("List via the key_command resolver: %v", err)
		}
	})
}

// TestSFTPKeyResolvers_Passphrase is #269's end-to-end positive control: a
// real SFTP connection authenticated with a real, passphrase-protected
// private key, through each of the three passphrase resolvers, against a
// real machine. Same standard TestSFTPKeyResolvers holds #74's three key
// resolvers to: prove it against a live server, not just in a unit test
// that produces plausible bytes.
//
// The encrypted key is a SECOND identity on the same machine. The harness's
// own readiness probe authenticates with ssh.ParsePrivateKey, which cannot
// load an encrypted key, so the machine's own unencrypted key is what
// proves the server is up and AuthorizeKey adds the encrypted one on top.
// Before #450 that second identity needed its own image build, with both
// keys baked into authorized_keys.
func TestSFTPKeyResolvers_Passphrase(t *testing.T) {
	src := machines.Start(t).Source(t)

	const passphrase = "correct horse battery staple"
	encryptedKeyPath, encryptedAuthLine := encryptedClientKey(t, passphrase)
	encryptedPEM, err := os.ReadFile(encryptedKeyPath)
	if err != nil {
		t.Fatalf("reading the generated encrypted client key: %v", err)
	}
	src.AuthorizeKey(t, encryptedAuthLine)

	base := src.TransportSource("sftp-key-passphrase", "")
	base.KeyFile = ""
	adapter := rclone.New()
	ctx := context.Background()

	// The control on AuthorizeKey, and it has to come first: every subtest
	// below reads "the encrypted key authenticated", which is also what a
	// server that never learned about it would fail to say. If this is red,
	// nothing underneath means anything.
	t.Run("the machine really did learn the encrypted key", func(t *testing.T) {
		s := base
		s.KeyFile = encryptedKeyPath
		s.PassphraseCommand = []string{"/bin/echo", "-n", passphrase}
		if _, err := adapter.List(ctx, s); err != nil {
			t.Fatalf("the encrypted key was authorized on this machine and still could not log in: %v\nEverything below this point is asserting against a server that does not trust the key it is being handed", err)
		}
	})

	// RED, and afterwards a permanent regression proof: with no passphrase
	// configured at all, an encrypted key.file fails clearly, which is the
	// exact production failure #269 reported ("failed to parse private key
	// file: ... passphrase protected"), never a hang and never a wrong
	// success.
	t.Run("key_file without a configured passphrase fails clearly", func(t *testing.T) {
		s := base
		s.KeyFile = encryptedKeyPath
		_, err := adapter.List(ctx, s)
		if err == nil {
			t.Fatal("an encrypted key with no passphrase configured was accepted")
		}
		if !strings.Contains(err.Error(), "passphrase") {
			t.Fatalf("error %q does not name the actual problem (passphrase protected): %v", err.Error(), err)
		}
	})

	t.Run("key_file with key.passphrase.env succeeds", func(t *testing.T) {
		const envName = "RCLONE_MANAGER_TEST_SFTP_PASSPHRASE_ENV"
		t.Setenv(envName, passphrase)
		s := base
		s.KeyFile = encryptedKeyPath
		s.PassphraseEnv = envName
		if _, err := adapter.List(ctx, s); err != nil {
			t.Fatalf("List via key_file + key.passphrase.env: %v", err)
		}
	})

	t.Run("key_file with key.passphrase.file succeeds", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "passphrase")
		// A trailing newline, exactly what `echo` (not `echo -n`) would
		// have produced: passphrase.go's own doc explains why this must
		// still work.
		if err := os.WriteFile(path, []byte(passphrase+"\n"), 0o600); err != nil {
			t.Fatalf("writing the passphrase file: %v", err)
		}
		s := base
		s.KeyFile = encryptedKeyPath
		s.PassphraseFile = path
		if _, err := adapter.List(ctx, s); err != nil {
			t.Fatalf("List via key_file + key.passphrase.file: %v", err)
		}
	})

	t.Run("key_file with the wrong passphrase is refused, not hung or silently accepted", func(t *testing.T) {
		s := base
		s.KeyFile = encryptedKeyPath
		s.PassphraseCommand = []string{"/bin/echo", "-n", "definitely not the right passphrase"}
		if _, err := adapter.List(ctx, s); err == nil {
			t.Fatal("a wrong passphrase was accepted")
		}
	})

	t.Run("key_env with key.passphrase.env succeeds", func(t *testing.T) {
		const keyEnvName = "RCLONE_MANAGER_TEST_SFTP_KEY_ENCRYPTED_ENV"
		const passEnvName = "RCLONE_MANAGER_TEST_SFTP_PASSPHRASE_ENV2"
		t.Setenv(keyEnvName, string(encryptedPEM))
		t.Setenv(passEnvName, passphrase)
		s := base
		s.KeyEnv = keyEnvName
		s.PassphraseEnv = passEnvName
		if _, err := adapter.List(ctx, s); err != nil {
			t.Fatalf("List via key_env + key.passphrase.env: %v", err)
		}
	})
}

// encryptedClientKey generates a passphrase-protected ed25519 client
// identity. It is generated fresh for each run, lives only under the test's
// temp directory and authenticates only against a disposable machine, so
// there is no real secret here.
func encryptedClientKey(t *testing.T, passphrase string) (privateKeyPath string, authorizedKeyLine string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating an encrypted client key: %v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	if err != nil {
		t.Fatalf("marshalling the encrypted client key: %v", err)
	}
	privateKeyPath = filepath.Join(t.TempDir(), "id_ed25519_encrypted")
	if err := os.WriteFile(privateKeyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("writing the encrypted client key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("converting the encrypted key's public half: %v", err)
	}
	return privateKeyPath, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
}
