package rclone

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// --- format: encrypt/decrypt/detect ---

func TestEncryptDecryptKeyMaterial_RoundTrip(t *testing.T) {
	dek := deriveKeyEncryptionDEK(obs.NewSecret("correct-horse-battery-staple"))
	plaintext := mustUnencryptedKeyPEM(t)

	ciphertext, err := encryptKeyMaterial(dek, plaintext)
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}
	if !isEncryptedKeyMaterial(ciphertext) {
		t.Fatal("encryptKeyMaterial's own output was not recognized by isEncryptedKeyMaterial")
	}
	if bytes.Contains(ciphertext, plaintext) {
		t.Fatal("ciphertext contains the plaintext key verbatim; this is not encrypted at all")
	}

	got, err := decryptKeyMaterial(dek, ciphertext)
	if err != nil {
		t.Fatalf("decryptKeyMaterial: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatal("decryptKeyMaterial did not round-trip the original plaintext")
	}
}

func TestIsEncryptedKeyMaterial_FalseForPlainPEM(t *testing.T) {
	if isEncryptedKeyMaterial(mustUnencryptedKeyPEM(t)) {
		t.Fatal("a plain PEM key was reported as this program's encrypted format")
	}
}

// TestEncryptKeyMaterial_FreshNonceEveryTime is the property that actually
// makes AES-GCM safe here: encrypting the same plaintext under the same
// DEK twice must never reuse a nonce, or GCM's authentication guarantee
// breaks. Asserting the two ciphertexts differ is the black-box way to
// catch a hard-coded or reused nonce without reaching into the format.
func TestEncryptKeyMaterial_FreshNonceEveryTime(t *testing.T) {
	dek := deriveKeyEncryptionDEK(obs.NewSecret("same-dek-every-time"))
	plaintext := mustUnencryptedKeyPEM(t)

	first, err := encryptKeyMaterial(dek, plaintext)
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}
	second, err := encryptKeyMaterial(dek, plaintext)
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("encrypting the same plaintext twice under the same DEK produced identical ciphertext: the nonce is not actually fresh")
	}
	// Both must still decrypt to the same plaintext regardless.
	for _, ct := range [][]byte{first, second} {
		got, err := decryptKeyMaterial(dek, ct)
		if err != nil {
			t.Fatalf("decryptKeyMaterial: %v", err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatal("a freshly-nonced ciphertext did not decrypt back to the original plaintext")
		}
	}
}

func TestDecryptKeyMaterial_WrongDEKFails(t *testing.T) {
	right := deriveKeyEncryptionDEK(obs.NewSecret("the-real-dek"))
	wrong := deriveKeyEncryptionDEK(obs.NewSecret("a-different-dek"))
	plaintext := mustUnencryptedKeyPEM(t)

	ciphertext, err := encryptKeyMaterial(right, plaintext)
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}
	if _, err := decryptKeyMaterial(wrong, ciphertext); err == nil {
		t.Fatal("decrypting with the wrong DEK succeeded")
	}
}

func TestDecryptKeyMaterial_TamperedCiphertextFails(t *testing.T) {
	dek := deriveKeyEncryptionDEK(obs.NewSecret("some-dek"))
	ciphertext, err := encryptKeyMaterial(dek, mustUnencryptedKeyPEM(t))
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}
	// Flip one byte well past the header, inside the ciphertext/tag.
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0xFF

	if _, err := decryptKeyMaterial(dek, tampered); err == nil {
		t.Fatal("decrypting tampered ciphertext succeeded; GCM's authentication should have caught this")
	}
}

func TestDecryptKeyMaterial_RejectsPlainPEM(t *testing.T) {
	dek := deriveKeyEncryptionDEK(obs.NewSecret("some-dek"))
	if _, err := decryptKeyMaterial(dek, mustUnencryptedKeyPEM(t)); err == nil {
		t.Fatal("decryptKeyMaterial accepted plain PEM content as its own encrypted format")
	}
}

// --- DEK resolution: file/env/command, mirroring keysource.go/passphrase.go ---

func TestResolveKeyEncryptionSecret_Unset(t *testing.T) {
	secret, ok, err := resolveKeyEncryptionSecret(transport.Source{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("a Source with none of the three key_encryption sources set reported ok == true")
	}
	if secret != (obs.Secret{}) {
		t.Fatal("a Source with no key_encryption source set returned a non-zero secret")
	}
}

func TestResolveKeyEncryptionSecret_FromFile_TrimsTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dek")
	if err := os.WriteFile(path, []byte("my-dek-value\n"), 0o600); err != nil {
		t.Fatalf("writing DEK file: %v", err)
	}
	secret, ok, err := resolveKeyEncryptionSecret(transport.Source{KeyEncryptionFile: path})
	if err != nil {
		t.Fatalf("resolveKeyEncryptionSecret: %v", err)
	}
	if !ok {
		t.Fatal("ok == false for a Source with KeyEncryptionFile set")
	}
	if secret.Reveal() != "my-dek-value" {
		t.Fatalf("Reveal() = %q, want trailing newline trimmed", secret.Reveal())
	}
}

func TestResolveKeyEncryptionSecret_FromEnv_DoesNotTrim(t *testing.T) {
	const envName = "RCLONE_MANAGER_TEST_KEYENCRYPTION_ENV"
	t.Setenv(envName, "my-dek-value\n")
	secret, ok, err := resolveKeyEncryptionSecret(transport.Source{KeyEncryptionEnv: envName})
	if err != nil {
		t.Fatalf("resolveKeyEncryptionSecret: %v", err)
	}
	if !ok {
		t.Fatal("ok == false for a Source with KeyEncryptionEnv set")
	}
	if secret.Reveal() != "my-dek-value\n" {
		t.Fatalf("Reveal() = %q, an env-resolved value should not be trimmed", secret.Reveal())
	}
}

func TestResolveKeyEncryptionSecret_FromCommand(t *testing.T) {
	secret, ok, err := resolveKeyEncryptionSecret(transport.Source{
		KeyEncryptionCommand: []string{"/bin/echo", "my-dek-value"},
	})
	if err != nil {
		t.Fatalf("resolveKeyEncryptionSecret: %v", err)
	}
	if !ok {
		t.Fatal("ok == false for a Source with KeyEncryptionCommand set")
	}
	if secret.Reveal() != "my-dek-value" {
		t.Fatalf("Reveal() = %q, want %q (trailing newline trimmed)", secret.Reveal(), "my-dek-value")
	}
}

func TestResolveKeyEncryptionSecret_MissingEnvVariableFails(t *testing.T) {
	_, _, err := resolveKeyEncryptionSecret(transport.Source{KeyEncryptionEnv: "RCLONE_MANAGER_TEST_DOES_NOT_EXIST_298"})
	if err == nil {
		t.Fatal("an unset key_encryption.env variable was accepted")
	}
}

// --- resolveKeyFileForSFTP: the actual key_file-at-rest behaviour ---

// TestResolveKeyFileForSFTP_NoKeyEncryptionConfigured is #298's central
// regression guarantee: "with no DEK configured, everything behaves
// exactly as before." KeyFile is set to a path that DOES NOT EXIST, and
// none of the three key_encryption sources are set; if this function so
// much as tried to open KeyFile, it would fail with a file-not-found
// error. Getting back ok == false, err == nil instead proves the file was
// never touched at all, not merely that this run happened to succeed.
func TestResolveKeyFileForSFTP_NoKeyEncryptionConfigured(t *testing.T) {
	secret, ok, err := resolveKeyFileForSFTP(transport.Source{
		KeyFile: "/this/path/does/not/exist/at/all",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("ok == true with no key_encryption source configured; key_file should have been left completely alone")
	}
	if secret != (obs.Secret{}) {
		t.Fatal("a non-zero secret was returned with no key_encryption source configured")
	}
}

// TestResolveKeyFileForSFTP_MigratesPlaintextKeyInPlace is #298's
// migration path (issue step 3): a key file exactly as an installation
// from before this feature would have left it -- plain PEM bytes, no
// encryption at all -- gets transparently re-encrypted the first time a
// key_encryption source is configured and this function runs against it.
func TestResolveKeyFileForSFTP_MigratesPlaintextKeyInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "imported_key")
	plaintext := mustUnencryptedKeyPEM(t)
	if err := os.WriteFile(path, plaintext, 0o600); err != nil {
		t.Fatalf("simulating a pre-#298 plaintext key file: %v", err)
	}

	const envName = "RCLONE_MANAGER_TEST_MIGRATION_DEK"
	t.Setenv(envName, "the-configured-dek")
	src := transport.Source{KeyFile: path, KeyEncryptionEnv: envName}

	secret, ok, err := resolveKeyFileForSFTP(src)
	if err != nil {
		t.Fatalf("resolveKeyFileForSFTP: %v", err)
	}
	if !ok {
		t.Fatal("ok == false with a key_encryption source configured")
	}
	if secret.Reveal() != string(plaintext) {
		t.Fatal("the resolved secret for this connection attempt did not match the original plaintext key")
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading migrated key file: %v", err)
	}
	if bytes.Equal(onDisk, plaintext) {
		t.Fatal("the key file on disk is still plaintext after migration; #298's whole point is that it no longer is")
	}
	if !isEncryptedKeyMaterial(onDisk) {
		t.Fatal("the migrated key file does not carry this program's encrypted-format marker")
	}

	// The migrated file must itself decrypt back to the exact original
	// key with the same DEK, and permissions must stay owner-only.
	dek := deriveKeyEncryptionDEK(obs.NewSecret("the-configured-dek"))
	decrypted, err := decryptKeyMaterial(dek, onDisk)
	if err != nil {
		t.Fatalf("decrypting the migrated file: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatal("the migrated file does not decrypt back to the original key")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("statting migrated key file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("migrated key file mode = %o, want 0600", info.Mode().Perm())
	}

	// A SECOND call (a second connection attempt / a later cycle) must
	// find the already-migrated file and simply decrypt it, still
	// returning the same usable key, never re-migrating or corrupting it.
	secret2, ok2, err := resolveKeyFileForSFTP(src)
	if err != nil {
		t.Fatalf("resolveKeyFileForSFTP (second call): %v", err)
	}
	if !ok2 || secret2.Reveal() != string(plaintext) {
		t.Fatal("a second resolveKeyFileForSFTP call against an already-migrated file did not return the same usable key")
	}
}

// TestResolveKeyFileForSFTP_DecryptsAlreadyEncryptedFile is the steady
// state after migration: a file already in #298's format is decrypted in
// memory and used, unchanged on disk.
func TestResolveKeyFileForSFTP_DecryptsAlreadyEncryptedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "imported_key")
	plaintext := mustUnencryptedKeyPEM(t)

	const envName = "RCLONE_MANAGER_TEST_STEADYSTATE_DEK"
	t.Setenv(envName, "the-steady-state-dek")
	dek := deriveKeyEncryptionDEK(obs.NewSecret("the-steady-state-dek"))
	ciphertext, err := encryptKeyMaterial(dek, plaintext)
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}
	if err := os.WriteFile(path, ciphertext, 0o600); err != nil {
		t.Fatalf("writing pre-encrypted key file: %v", err)
	}

	secret, ok, err := resolveKeyFileForSFTP(transport.Source{KeyFile: path, KeyEncryptionEnv: envName})
	if err != nil {
		t.Fatalf("resolveKeyFileForSFTP: %v", err)
	}
	if !ok || secret.Reveal() != string(plaintext) {
		t.Fatal("an already-encrypted key file did not resolve back to the original plaintext")
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading key file after use: %v", err)
	}
	if !bytes.Equal(onDisk, ciphertext) {
		t.Fatal("an already-encrypted key file was rewritten even though no migration was needed")
	}
}

// TestResolveKeyFileForSFTP_WrongDEKFailsClearly proves a misconfigured
// key_encryption source is refused, by name, rather than silently
// authenticating with garbage or panicking.
func TestResolveKeyFileForSFTP_WrongDEKFailsClearly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "imported_key")
	dek := deriveKeyEncryptionDEK(obs.NewSecret("the-real-dek"))
	ciphertext, err := encryptKeyMaterial(dek, mustUnencryptedKeyPEM(t))
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}
	if err := os.WriteFile(path, ciphertext, 0o600); err != nil {
		t.Fatalf("writing pre-encrypted key file: %v", err)
	}

	const envName = "RCLONE_MANAGER_TEST_WRONGDEK"
	t.Setenv(envName, "a-completely-different-dek")

	_, ok, err := resolveKeyFileForSFTP(transport.Source{KeyFile: path, KeyEncryptionEnv: envName})
	if err == nil {
		t.Fatal("resolving with the wrong DEK succeeded")
	}
	if ok {
		t.Fatal("ok == true alongside a non-nil error")
	}

	onDisk, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("reading key file after failed resolution: %v", readErr)
	}
	if !bytes.Equal(onDisk, ciphertext) {
		t.Fatal("a failed decryption attempt modified the key file on disk")
	}
}
