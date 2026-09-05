package rclone

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/argon2"

	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file covers #298's at-rest key encryption, and the property it
// spends most of its length on is not "the round trip works".
//
// It is that a file in the OLD format still opens, and stops being in the
// old format afterwards. There are three on-disk states an installation
// can be in (plaintext PEM, V1's unsalted SHA-256 derivation, V2's salted
// Argon2id one) and resolveKeyFileForSFTP is expected to read all three
// and leave the file in the third. A migration that silently failed to
// write back would look identical to a successful one on the call that
// mattered, and would keep looking identical until somebody read the file.
//
// The crypto assertions are made against the real primitives rather than
// against this package's own helpers wherever that is possible: the test
// derives a DEK with argon2 directly and opens the ciphertext with
// crypto/cipher, so an encryptKeyMaterial that had quietly stopped salting,
// or had reused a nonce, fails here rather than agreeing with itself.
//
// The tamper cases are the ones to keep if this file is ever trimmed. GCM's
// authentication is the entire reason this format is more than obfuscation,
// and a decrypt that accepted modified bytes would be indistinguishable
// from a working one on every other case in this file.

// --- format: encrypt/decrypt/detect ---

func TestEncryptDecryptKeyMaterial_RoundTrip(t *testing.T) {
	secret := obs.NewSecret("correct-horse-battery-staple")
	plaintext := mustUnencryptedKeyPEM(t)

	ciphertext, err := encryptKeyMaterial(secret, plaintext)
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}
	if !isEncryptedKeyMaterial(ciphertext) {
		t.Fatal("encryptKeyMaterial's own output was not recognized by isEncryptedKeyMaterial")
	}
	if bytes.Contains(ciphertext, plaintext) {
		t.Fatal("ciphertext contains the plaintext key verbatim; this is not encrypted at all")
	}

	got, err := decryptKeyMaterial(secret, ciphertext)
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
// secret twice must never reuse a nonce, or GCM's authentication guarantee
// breaks. Asserting the two ciphertexts differ is the black-box way to
// catch a hard-coded or reused nonce without reaching into the format.
func TestEncryptKeyMaterial_FreshNonceEveryTime(t *testing.T) {
	secret := obs.NewSecret("same-dek-every-time")
	plaintext := mustUnencryptedKeyPEM(t)

	first, err := encryptKeyMaterial(secret, plaintext)
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}
	second, err := encryptKeyMaterial(secret, plaintext)
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("encrypting the same plaintext twice under the same secret produced identical ciphertext: the salt/nonce is not actually fresh")
	}
	// Both must still decrypt to the same plaintext regardless.
	for _, ct := range [][]byte{first, second} {
		got, err := decryptKeyMaterial(secret, ct)
		if err != nil {
			t.Fatalf("decryptKeyMaterial: %v", err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatal("a freshly-nonced ciphertext did not decrypt back to the original plaintext")
		}
	}
}

// TestEncryptKeyMaterial_FreshSaltEveryTime is the salt-specific half of
// the property above: two encryptions of the same plaintext under the same
// secret must carry two different salts, not just two different nonces, or
// an offline guesser could precompute one Argon2id pass per candidate and
// reuse it against every key file this process ever writes.
func TestEncryptKeyMaterial_FreshSaltEveryTime(t *testing.T) {
	secret := obs.NewSecret("same-dek-every-time")
	plaintext := mustUnencryptedKeyPEM(t)

	first, err := encryptKeyMaterial(secret, plaintext)
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}
	second, err := encryptKeyMaterial(secret, plaintext)
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}

	saltStart := len(encryptedKeyMagicV2)
	saltEnd := saltStart + keyEncryptionSaltLen
	firstSalt := first[saltStart:saltEnd]
	secondSalt := second[saltStart:saltEnd]
	if bytes.Equal(firstSalt, secondSalt) {
		t.Fatal("encrypting the same plaintext twice under the same secret produced the same salt")
	}
}

func TestDecryptKeyMaterial_WrongDEKFails(t *testing.T) {
	right := obs.NewSecret("the-real-dek")
	wrong := obs.NewSecret("a-different-dek")
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
	secret := obs.NewSecret("some-dek")
	ciphertext, err := encryptKeyMaterial(secret, mustUnencryptedKeyPEM(t))
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}
	// Flip one byte well past the header, inside the ciphertext/tag.
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0xFF

	if _, err := decryptKeyMaterial(secret, tampered); err == nil {
		t.Fatal("decrypting tampered ciphertext succeeded; GCM's authentication should have caught this")
	}
}

func TestDecryptKeyMaterial_RejectsPlainPEM(t *testing.T) {
	secret := obs.NewSecret("some-dek")
	if _, err := decryptKeyMaterial(secret, mustUnencryptedKeyPEM(t)); err == nil {
		t.Fatal("decryptKeyMaterial accepted plain PEM content as its own encrypted format")
	}
}

// TestDecryptKeyMaterial_StillReadsLegacyV1Format is #298's DEK-derivation
// hardening backward-compatibility guarantee: a key file encrypted under
// the original, now-superseded V1 scheme (unsalted SHA-256) must still
// decrypt, so an installation is never locked out of its own key file the
// moment this file's KDF changes underneath it.
func TestDecryptKeyMaterial_StillReadsLegacyV1Format(t *testing.T) {
	secret := obs.NewSecret("a-legacy-v1-dek")
	plaintext := mustUnencryptedKeyPEM(t)
	dek := deriveKeyEncryptionDEKV1(secret)

	legacyCiphertext, err := encryptKeyMaterialV1ForTest(dek, plaintext)
	if err != nil {
		t.Fatalf("encrypting a V1-format fixture: %v", err)
	}
	if !isEncryptedKeyMaterial(legacyCiphertext) {
		t.Fatal("a V1-format ciphertext was not recognized as this program's own encrypted format")
	}
	if !isLegacyEncryptedKeyMaterial(legacyCiphertext) {
		t.Fatal("a V1-format ciphertext was not recognized as legacy")
	}

	got, err := decryptKeyMaterial(secret, legacyCiphertext)
	if err != nil {
		t.Fatalf("decryptKeyMaterial did not read a legacy V1-format file: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatal("decrypting a legacy V1-format file did not round-trip the original plaintext")
	}
}

// TestIsLegacyEncryptedKeyMaterial_FalseForV2 proves the two formats are
// told apart correctly: resolveKeyFileForSFTP relies on this to decide
// whether a freshly-decrypted file also needs an upgrade to V2.
func TestIsLegacyEncryptedKeyMaterial_FalseForV2(t *testing.T) {
	secret := obs.NewSecret("some-v2-dek")
	ciphertext, err := encryptKeyMaterial(secret, mustUnencryptedKeyPEM(t))
	if err != nil {
		t.Fatalf("encryptKeyMaterial: %v", err)
	}
	if isLegacyEncryptedKeyMaterial(ciphertext) {
		t.Fatal("a current V2-format ciphertext was reported as legacy V1")
	}
}

// --- DEK derivation: Argon2id hardening (mandatory review finding #1) ---

// TestDeriveKeyEncryptionDEK_DiffersFromPlainSHA256 is the central proof
// this file no longer derives its DEK the weak way: #298's original
// deriveKeyEncryptionDEK was a bare sha256.Sum256 of the resolved secret,
// with no salt and no cost at all, so an attacker who obtained an
// encrypted key file but not the DEK source secret could try candidate
// secrets at the cost of one SHA-256 call each. The hardened derivation
// must never coincide with that bare hash.
func TestDeriveKeyEncryptionDEK_DiffersFromPlainSHA256(t *testing.T) {
	secret := obs.NewSecret("a-weak-guessable-passphrase")
	salt := bytes.Repeat([]byte{0x42}, keyEncryptionSaltLen)

	dek := deriveKeyEncryptionDEK(secret, salt)
	plainSHA256 := sha256.Sum256([]byte(secret.Reveal()))

	if dek == plainSHA256 {
		t.Fatal("deriveKeyEncryptionDEK produced the same output as a bare SHA-256 hash; the point of this fix is that it no longer does")
	}
}

// TestDeriveKeyEncryptionDEK_UsesArgon2idWithThisFilesParameters proves
// deriveKeyEncryptionDEK really is Argon2id, run with this file's own
// published cost parameters, rather than merely "not SHA-256": it must
// match a direct golang.org/x/crypto/argon2 call using the same inputs and
// the same keyEncryptionArgon2Time/Memory/Threads/KeyLen constants
// keyencryption.go declares.
func TestDeriveKeyEncryptionDEK_UsesArgon2idWithThisFilesParameters(t *testing.T) {
	secret := obs.NewSecret("some-secret-value")
	salt := []byte("0123456789abcdef") // keyEncryptionSaltLen bytes

	got := deriveKeyEncryptionDEK(secret, salt)
	want := argon2.IDKey([]byte(secret.Reveal()), salt, keyEncryptionArgon2Time, keyEncryptionArgon2Memory, keyEncryptionArgon2Threads, keyEncryptionArgon2KeyLen)

	if !bytes.Equal(got[:], want) {
		t.Fatal("deriveKeyEncryptionDEK did not match a direct argon2.IDKey call using this file's own published parameters")
	}
}

// TestDeriveKeyEncryptionDEK_SaltChangesTheOutput is Argon2id's whole
// reason for taking a salt at all: the same secret through two different
// salts must never derive the same DEK, or persisting a salt in the V2
// format would be theater.
func TestDeriveKeyEncryptionDEK_SaltChangesTheOutput(t *testing.T) {
	secret := obs.NewSecret("same-secret-both-times")
	saltA := bytes.Repeat([]byte{0x01}, keyEncryptionSaltLen)
	saltB := bytes.Repeat([]byte{0x02}, keyEncryptionSaltLen)

	dekA := deriveKeyEncryptionDEK(secret, saltA)
	dekB := deriveKeyEncryptionDEK(secret, saltB)

	if dekA == dekB {
		t.Fatal("two different salts derived the same DEK from the same secret")
	}
}

// TestKeyEncryptionArgon2Params_AreNotTrivial is a lightweight guard
// against silently regressing to cost parameters cheap enough to defeat
// this file's whole reason for using Argon2id: values mirror the floor
// apps/common/auth/local/password.go's own doc cites (OWASP's minimum
// m=19MiB, t=2, p=1). This does not measure wall-clock cost (that would be
// slow and flaky); it pins the published constants themselves.
func TestKeyEncryptionArgon2Params_AreNotTrivial(t *testing.T) {
	const owaspMinMemoryKiB = 19 * 1024
	if keyEncryptionArgon2Memory < owaspMinMemoryKiB {
		t.Fatalf("keyEncryptionArgon2Memory = %d KiB, below OWASP's %d KiB minimum", keyEncryptionArgon2Memory, owaspMinMemoryKiB)
	}
	if keyEncryptionArgon2Time < 2 {
		t.Fatalf("keyEncryptionArgon2Time = %d, below OWASP's minimum of 2", keyEncryptionArgon2Time)
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

// --- key_encryption secret caching (mandatory review finding #2) ---

// TestResolveKeyEncryptionSecretCached_OnlyResolvesOnce is the central
// proof of the caching fix: List/Stat/CopyToLocal/RemoteHash/DeleteRemote
// each independently reach resolveKeyFileForSFTP, which used to call
// resolveKeyEncryptionSecret -- and, for a command source, spawn a fresh
// subprocess -- from scratch on every single call. Three calls against the
// identical key_encryption source must invoke the resolver command exactly
// once.
func TestResolveKeyEncryptionSecretCached_OnlyResolvesOnce(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "counter")
	script := filepath.Join(dir, "resolve-dek.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf x >> \"$1\"\nprintf 'cached-dek-value'\n"), 0o700); err != nil {
		t.Fatalf("writing test resolver script: %v", err)
	}

	src := transport.Source{KeyEncryptionCommand: []string{script, counter}}

	for i := 0; i < 3; i++ {
		secret, ok, err := resolveKeyEncryptionSecretCached(src)
		if err != nil {
			t.Fatalf("call %d: resolveKeyEncryptionSecretCached: %v", i, err)
		}
		if !ok || secret.Reveal() != "cached-dek-value" {
			t.Fatalf("call %d: got (%q, %v), want (%q, true)", i, secret.Reveal(), ok, "cached-dek-value")
		}
	}

	counted, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("reading counter file: %v", err)
	}
	if len(counted) != 1 {
		t.Fatalf("resolver command ran %d times across 3 calls with the same key_encryption source; want exactly 1", len(counted))
	}
}

// TestResolveKeyEncryptionSecretCached_DistinctSourcesResolveIndependently
// guards against a caching bug in the other direction: two DIFFERENT
// key_encryption sources must never share a cache entry.
func TestResolveKeyEncryptionSecretCached_DistinctSourcesResolveIndependently(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "counter")
	script := filepath.Join(dir, "resolve-dek.sh")
	// Echoes back its second argument, so two distinct sources (distinct
	// argv) resolve to two distinct secrets.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf x >> \"$1\"\nprintf '%s' \"$2\"\n"), 0o700); err != nil {
		t.Fatalf("writing test resolver script: %v", err)
	}

	srcA := transport.Source{KeyEncryptionCommand: []string{script, counter, "secret-a"}}
	srcB := transport.Source{KeyEncryptionCommand: []string{script, counter, "secret-b"}}

	secretA, ok, err := resolveKeyEncryptionSecretCached(srcA)
	if err != nil || !ok {
		t.Fatalf("resolving source A: ok=%v err=%v", ok, err)
	}
	secretB, ok, err := resolveKeyEncryptionSecretCached(srcB)
	if err != nil || !ok {
		t.Fatalf("resolving source B: ok=%v err=%v", ok, err)
	}

	if secretA.Reveal() != "secret-a" || secretB.Reveal() != "secret-b" {
		t.Fatalf("got secretA=%q secretB=%q, want distinct sources to resolve independently", secretA.Reveal(), secretB.Reveal())
	}

	counted, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("reading counter file: %v", err)
	}
	if len(counted) != 2 {
		t.Fatalf("resolver command ran %d times across 2 distinct sources; want exactly 2 (one per source)", len(counted))
	}
}

// TestResolveKeyEncryptionSecretCached_CachesFailureToo proves a failing
// resolver is not retried within the same process either: repeating the
// subprocess spawn on every call is exactly the failure mode this cache
// exists to prevent, whether the resolver succeeds or not.
func TestResolveKeyEncryptionSecretCached_CachesFailureToo(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "counter")
	script := filepath.Join(dir, "always-fails.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf x >> \"$1\"\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("writing test resolver script: %v", err)
	}

	src := transport.Source{KeyEncryptionCommand: []string{script, counter}}

	for i := 0; i < 3; i++ {
		if _, _, err := resolveKeyEncryptionSecretCached(src); err == nil {
			t.Fatalf("call %d: a failing resolver command was reported as successful", i)
		}
	}

	counted, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("reading counter file: %v", err)
	}
	if len(counted) != 1 {
		t.Fatalf("resolver command ran %d times across 3 failing calls; want exactly 1", len(counted))
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
	if isLegacyEncryptedKeyMaterial(onDisk) {
		t.Fatal("a freshly migrated key file was written in the legacy V1 format instead of the current V2 one")
	}

	// The migrated file must itself decrypt back to the exact original
	// key with the same DEK source, and permissions must stay owner-only.
	decrypted, err := decryptKeyMaterial(obs.NewSecret("the-configured-dek"), onDisk)
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
// state after migration: a file already in #298's current format is
// decrypted in memory and used, unchanged on disk.
func TestResolveKeyFileForSFTP_DecryptsAlreadyEncryptedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "imported_key")
	plaintext := mustUnencryptedKeyPEM(t)

	const envName = "RCLONE_MANAGER_TEST_STEADYSTATE_DEK"
	t.Setenv(envName, "the-steady-state-dek")
	ciphertext, err := encryptKeyMaterial(obs.NewSecret("the-steady-state-dek"), plaintext)
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

// TestResolveKeyFileForSFTP_UpgradesLegacyV1FileInPlace is #298's
// DEK-derivation hardening migration path: a key file encrypted before
// this hardening landed -- V1's unsalted SHA-256-derived DEK -- gets
// transparently re-encrypted under V2 (Argon2id, salted) the next time
// this function runs against it, the same self-heal-on-next-touch
// discipline the plaintext migration above already established.
func TestResolveKeyFileForSFTP_UpgradesLegacyV1FileInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "imported_key")
	plaintext := mustUnencryptedKeyPEM(t)

	const envName = "RCLONE_MANAGER_TEST_V1UPGRADE_DEK"
	t.Setenv(envName, "the-v1-dek")
	legacyDEK := deriveKeyEncryptionDEKV1(obs.NewSecret("the-v1-dek"))
	legacyCiphertext, err := encryptKeyMaterialV1ForTest(legacyDEK, plaintext)
	if err != nil {
		t.Fatalf("encrypting a V1-format fixture: %v", err)
	}
	if err := os.WriteFile(path, legacyCiphertext, 0o600); err != nil {
		t.Fatalf("writing a legacy V1-format key file: %v", err)
	}

	src := transport.Source{KeyFile: path, KeyEncryptionEnv: envName}
	secret, ok, err := resolveKeyFileForSFTP(src)
	if err != nil {
		t.Fatalf("resolveKeyFileForSFTP: %v", err)
	}
	if !ok || secret.Reveal() != string(plaintext) {
		t.Fatal("resolveKeyFileForSFTP did not resolve a legacy V1 file back to the original plaintext")
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading upgraded key file: %v", err)
	}
	if bytes.Equal(onDisk, legacyCiphertext) {
		t.Fatal("a legacy V1 key file was left unchanged instead of being upgraded to V2")
	}
	if isLegacyEncryptedKeyMaterial(onDisk) {
		t.Fatal("the upgraded key file is still in the legacy V1 format")
	}
	if !isEncryptedKeyMaterial(onDisk) {
		t.Fatal("the upgraded key file does not carry this program's encrypted-format marker")
	}

	decrypted, err := decryptKeyMaterial(obs.NewSecret("the-v1-dek"), onDisk)
	if err != nil {
		t.Fatalf("decrypting the upgraded file: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatal("the upgraded file does not decrypt back to the original key")
	}

	// A SECOND call must find the file already on V2 and simply decrypt
	// it, never touching it again.
	secret2, ok2, err := resolveKeyFileForSFTP(src)
	if err != nil {
		t.Fatalf("resolveKeyFileForSFTP (second call): %v", err)
	}
	if !ok2 || secret2.Reveal() != string(plaintext) {
		t.Fatal("a second resolveKeyFileForSFTP call against an already-upgraded file did not return the same usable key")
	}
	onDiskAfterSecondCall, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading key file after second call: %v", err)
	}
	if !bytes.Equal(onDiskAfterSecondCall, onDisk) {
		t.Fatal("a second call against an already-upgraded (V2) file rewrote it again")
	}
}

// TestResolveKeyFileForSFTP_WrongDEKFailsClearly proves a misconfigured
// key_encryption source is refused, by name, rather than silently
// authenticating with garbage or panicking.
func TestResolveKeyFileForSFTP_WrongDEKFailsClearly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "imported_key")
	ciphertext, err := encryptKeyMaterial(obs.NewSecret("the-real-dek"), mustUnencryptedKeyPEM(t))
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

// encryptKeyMaterialV1ForTest builds a V1-format ciphertext directly from
// an already-derived DEK, bypassing encryptKeyMaterial (which only ever
// writes the current V2 format). It exists purely so tests can construct
// the legacy on-disk shape a real pre-hardening installation would have
// left behind, the same fixture-building role
// TestResolveKeyFileForSFTP_MigratesPlaintextKeyInPlace already plays for
// the plaintext case.
func encryptKeyMaterialV1ForTest(dek [32]byte, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(dek[:])
	if err != nil {
		return nil, fmt.Errorf("initializing cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initializing AES-GCM: %w", err)
	}
	nonce := make([]byte, gcmNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating a nonce: %w", err)
	}
	out := make([]byte, 0, len(encryptedKeyMagicV1)+len(nonce)+len(plaintext)+gcm.Overhead())
	out = append(out, encryptedKeyMagicV1...)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plaintext, nil)
	return out, nil
}
