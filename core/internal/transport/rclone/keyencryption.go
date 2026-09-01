// This file is #298's answer to an imported SSH private key sitting on
// disk in the clear, with only Unix file permissions (#293) standing
// between it and anything with read access to that filesystem --
// including, per #298's own report, an SMB or AFP share exported from
// the same NAS volume, which can bypass owner-only file-mode bits
// entirely depending on how the share itself is configured.
//
// # What this defends, and what it deliberately does not
//
// This encrypts a key.file's content at rest, with a key (the "DEK",
// data-encryption key) resolved through config.KeyEncryption's own
// file/env/command sources, mirroring keysource.go's and passphrase.go's
// identical shape for the identical reason: no field anywhere in this
// configuration for pasting the actual encryption key into YAML.
//
// It does NOT defend a live process's memory. Decrypting a key to
// authenticate a connection necessarily puts the plaintext key material
// into this process's own memory for the duration of that attempt,
// exactly like a key.env or key.command resolver already does; see
// resolveKeyFileForSFTP's doc for why key_file cannot keep its "never
// enters memory at all" property once encryption is configured for it.
//
// It does NOT defend the DEK itself sitting next to the key it protects.
// If config.KeyEncryption's own file lives inside the same directory
// tree an SMB/AFP share exports, anything that could previously read the
// key file in the clear can now read the DEK and the encrypted key file
// side by side, decrypt it locally, and end up exactly where #298 started.
// The DEK's storage location MUST be outside whatever backup root or
// share this deployment exports; docs/ssh-setup.md's "Encrypting the key
// store at rest" section says this explicitly, because leaving the DEK
// reachable from the same share as the key it protects defeats the whole
// point of this file.
//
// # The on-disk format, and why it is what it is
//
// Encrypted key material is:
//
//	encryptedKeyMagic || nonce (12 bytes) || AES-256-GCM(nonce, plaintext) || GCM tag (16 bytes)
//
// encryptedKeyMagic is chosen so it can never collide with a real key: an
// unencrypted PEM-format private key (the only format ValidateImportedPrivateKey
// and #74's resolvers ever accept) always begins "-----BEGIN ", so a
// byte-prefix check is a complete, unambiguous format detector with no
// parsing and no chance of mistaking one for the other in either
// direction. That is also why this needs no separate version field
// beyond what is baked into the magic string itself (a V2 format, if one
// is ever needed, gets its own magic and this file's detection keeps
// working against both).
//
// AES-256-GCM is used because it is what crypto/aes and crypto/cipher
// already provide as a complete, authenticated (tamper-evident) cipher:
// #298 asked for exactly this, "AES-GCM or equivalent", specifically so
// this project does not take on a new crypto dependency for one feature.
// A 12-byte nonce is cipher.NewGCM's own standard nonce size, generated
// fresh with crypto/rand for every encryption (including every
// migration, see below): reusing a nonce with the same key is the one
// mistake that breaks GCM's authentication property outright, so this
// file never does.
//
// # Why the DEK is derived, not used raw
//
// config.KeyEncryption's three sources produce a Secret of whatever
// length an operator's file, environment variable or command happens to
// hold, exactly like Passphrase's three sources do, not necessarily the
// 32 raw bytes AES-256 needs. sha256.Sum256 turns whatever comes back
// into exactly 32 bytes, deterministically, so a human-typed passphrase,
// a short token or a genuinely random 32-byte file all work as this
// program's DEK without a separate "must be exactly N bytes" validation
// rule to trip over. This is a length-normalizing hash, not a
// password-hardening KDF (no salt, no iteration count): #298 asks for
// protection against a stolen file being readable in the clear, not
// against an offline brute force of a weak operator-chosen value, and
// adding scrypt/Argon2 here would be a new dependency this issue
// explicitly asked this file not to take on.
package rclone

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rclone/rclone/lib/env"

	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// encryptedKeyMagic prefixes every key file this program has encrypted at
// rest (#298). See this file's package doc for why a byte-prefix check
// against this exact string is a complete detector: a real PEM private
// key can never begin with it.
var encryptedKeyMagic = []byte("RCLONEMGR-KEYENC-V1:")

// gcmNonceSize is AES-GCM's standard nonce size. cipher.NewGCM defaults to
// exactly this; it is named here only so encryptKeyMaterial and
// decryptKeyMaterial agree on it without either hard-coding "12" twice.
const gcmNonceSize = 12

// keyEncryptionCommandTimeout mirrors keyCommandTimeout and
// passphraseCommandTimeout: the same shape of work (one secrets-manager
// round trip), the same bound. A separate var, not a shared one, so a
// test can shorten this resolver's timeout without affecting the other
// two (keysource_test.go and, in #269, passphrase_test.go already rely on
// that same isolation for their own timeouts).
var keyEncryptionCommandTimeout = 15 * time.Second

// maxResolvedKeyEncryptionSecretSize bounds how many bytes of a command's
// stdout, or a key-encryption file's content, this resolver will accept.
// Mirrors maxResolvedPassphraseSize's reasoning exactly: far more
// generous than any realistic DEK material needs, a margin against a
// misbehaving or compromised resolver, not a realistic ceiling.
const maxResolvedKeyEncryptionSecretSize = 1 << 16 // 64 KiB

// isEncryptedKeyMaterial reports whether raw is this program's own #298
// at-rest encryption format, rather than a plain PEM private key.
func isEncryptedKeyMaterial(raw []byte) bool {
	return bytes.HasPrefix(raw, encryptedKeyMagic)
}

// deriveKeyEncryptionDEK turns secret's resolved content into the exact
// 32 bytes AES-256 needs. See this file's package doc, "Why the DEK is
// derived, not used raw", for why this is SHA-256 and not a
// password-hardening KDF.
func deriveKeyEncryptionDEK(secret obs.Secret) [32]byte {
	return sha256.Sum256([]byte(secret.Reveal()))
}

// encryptKeyMaterial encrypts plaintext (a key file's real PEM content)
// under dek, returning encryptedKeyMagic || nonce || ciphertext-with-tag,
// exactly the format isEncryptedKeyMaterial/decryptKeyMaterial expect.
func encryptKeyMaterial(dek [32]byte, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(dek[:])
	if err != nil {
		return nil, fmt.Errorf("initializing cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initializing AES-GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating a nonce: %w", err)
	}

	out := make([]byte, 0, len(encryptedKeyMagic)+len(nonce)+len(plaintext)+gcm.Overhead())
	out = append(out, encryptedKeyMagic...)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// decryptKeyMaterial is encryptKeyMaterial's inverse. raw MUST already
// have been confirmed to carry encryptedKeyMagic (isEncryptedKeyMaterial);
// this returns an error, rather than panicking, for anything shorter than
// a valid header, on the same "never trust a file's shape just because
// its prefix matched" discipline the rest of this project's resolvers
// apply to external input.
func decryptKeyMaterial(dek [32]byte, raw []byte) ([]byte, error) {
	if !isEncryptedKeyMaterial(raw) {
		return nil, errors.New("not in this program's key-encryption format")
	}
	rest := raw[len(encryptedKeyMagic):]
	if len(rest) < gcmNonceSize {
		return nil, errors.New("truncated: shorter than a nonce")
	}
	nonce, ciphertext := rest[:gcmNonceSize], rest[gcmNonceSize:]

	block, err := aes.NewCipher(dek[:])
	if err != nil {
		return nil, fmt.Errorf("initializing cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initializing AES-GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// GCM's Open fails closed on any tamper or wrong key, and never
		// distinguishes the two in its own error: reported here exactly
		// that generically, on the same "report the shape of the
		// problem, never leak anything that could help an attacker
		// narrow it down" principle validateAndWrapKey's own doc states.
		return nil, errors.New("failed to authenticate: wrong key_encryption source, or the file has been modified")
	}
	return plaintext, nil
}

// resolveKeyEncryptionSecret reads src's configured key-encryption
// source, if any, and returns it wrapped in obs.Secret. ok is false, with
// a nil err, when none of KeyEncryptionFile/KeyEncryptionEnv/
// KeyEncryptionCommand is set: the default for every Source built before
// #298, and never itself an error -- an unconfigured DEK means "leave
// key_file exactly as before", not "this deployment is broken".
func resolveKeyEncryptionSecret(src transport.Source) (secret obs.Secret, ok bool, err error) {
	switch {
	case src.KeyEncryptionFile != "":
		secret, err = resolveKeyEncryptionFromFile(src.KeyEncryptionFile)
	case src.KeyEncryptionEnv != "":
		secret, err = resolveKeyEncryptionFromEnv(src.KeyEncryptionEnv)
	case len(src.KeyEncryptionCommand) > 0:
		secret, err = resolveKeyEncryptionFromCommand(src.KeyEncryptionCommand)
	default:
		return obs.Secret{}, false, nil
	}
	if err != nil {
		return obs.Secret{}, false, err
	}
	return secret, true, nil
}

// resolveKeyEncryptionFromFile reads path (shell-expanded exactly like
// Key.File and Passphrase.File already are) and returns its content,
// trailing newline trimmed exactly like resolvePassphraseFromFile, so a
// DEK file written with `echo` or `printf` behaves the same way a
// passphrase file already does.
func resolveKeyEncryptionFromFile(path string) (obs.Secret, error) {
	expanded := env.ShellExpand(path)
	raw, err := os.ReadFile(expanded)
	if err != nil {
		return obs.Secret{}, fmt.Errorf("key_encryption file %q: %w", path, err)
	}
	defer zeroBytes(raw)

	trimmed := strings.TrimRight(string(raw), "\r\n")
	if trimmed == "" {
		return obs.Secret{}, fmt.Errorf("key_encryption file %q: resolved value is empty", path)
	}
	return obs.NewSecret(trimmed), nil
}

// resolveKeyEncryptionFromEnv reads name from the environment, unmodified
// (no trimming, mirroring resolvePassphraseFromEnv's own reasoning: a
// shell's own export mechanism never appends a trailing newline the way
// `echo` does to a line of output).
func resolveKeyEncryptionFromEnv(name string) (obs.Secret, error) {
	val, ok := os.LookupEnv(name)
	if !ok {
		return obs.Secret{}, fmt.Errorf("environment variable %q is not set", name)
	}
	if val == "" {
		return obs.Secret{}, fmt.Errorf("environment variable %q: resolved value is empty", name)
	}
	return obs.NewSecret(val), nil
}

// resolveKeyEncryptionFromCommand runs argv and treats its stdout,
// trailing newline trimmed, as the encryption key. Mirrors
// resolvePassphraseFromCommand's subprocess posture exactly (no shell, a
// fixed minimal environment, its own process group so a bounded timeout
// can kill anything it spawned, bounded captured output), duplicated
// rather than shared on this project's own stated convention (see that
// function's doc) that a small resolver-shaped duplicate beats a
// cross-cutting shared helper for call sites that only look alike today.
func resolveKeyEncryptionFromCommand(argv []string) (obs.Secret, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return obs.Secret{}, errors.New("key_encryption command: no executable configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), keyEncryptionCommandTimeout)
	defer cancel()

	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		if err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	c.WaitDelay = 5 * time.Second

	stdout := &boundedBuffer{limit: maxResolvedKeyEncryptionSecretSize}
	stderr := &boundedBuffer{limit: maxCapturedStderr}
	c.Stdout = stdout
	c.Stderr = stderr
	defer stdout.zero()

	runErr := c.Run()

	switch {
	case ctx.Err() != nil:
		return obs.Secret{}, fmt.Errorf("key_encryption command %q: killed after exceeding its %s timeout", argv[0], keyEncryptionCommandTimeout)
	case runErr != nil:
		return obs.Secret{}, fmt.Errorf("key_encryption command %q: %v (stderr: %s)", argv[0], runErr, stderr.String())
	case stdout.truncated:
		return obs.Secret{}, fmt.Errorf("key_encryption command %q: output exceeded %d bytes, refusing to treat truncated output as a key", argv[0], maxResolvedKeyEncryptionSecretSize)
	}

	trimmed := strings.TrimRight(stdout.buf.String(), "\r\n")
	if trimmed == "" {
		return obs.Secret{}, fmt.Errorf("key_encryption command %q: resolved value is empty", argv[0])
	}
	return obs.NewSecret(trimmed), nil
}

// resolveKeyFileForSFTP is sftpConfig's key_file handling once a
// key-encryption source (#298) is in play. It returns ok == false, with a
// nil secret and nil error, when src has no key-encryption source
// configured at all: sftpConfig reads that as "leave KeyFile exactly
// alone", the same key_file behaviour this project has always had, key
// bytes never entering this process's memory. That is the common case
// today and the only one #298's regression tests care about matching
// exactly.
//
// When a key-encryption source IS configured, this always reads
// keyFilePath and returns its resolved PEM content for key_pem, in one of
// two ways:
//
//   - the file already carries encryptedKeyMagic (isEncryptedKeyMaterial):
//     decrypted in memory with the resolved DEK and returned. This is the
//     steady state for any key imported, or already migrated, since a
//     key-encryption source was configured.
//   - the file is still plaintext PEM (#298's migration path): encrypted
//     with the resolved DEK and written back to keyFilePath in place,
//     replacing the plaintext copy on disk, before the ORIGINAL plaintext
//     bytes (already in memory; never re-read from the file this call
//     just rewrote) are returned for key_pem. This is what makes an
//     installation upgraded from before #298 self-heal the first time it
//     actually connects, with no separate migration command an operator
//     has to remember to run, and covers a key imported today by
//     ImportSSHKey, which -- unchanged by #298 -- still writes plaintext
//     at import time; this function is the ONE place either kind of
//     plaintext-on-disk key gets encrypted, so import time and "an old
//     install upgraded" never need two different code paths.
//
// A migration write failure (a read-only filesystem, a full disk) is
// reported in the returned error rather than silently swallowed: the
// plaintext this process already holds would still authenticate, but
// returning success while leaving that failure unreported would hide a
// real "this deployment's key is still unprotected" condition from
// whatever surfaces this error (the operator's own retry, or their own
// next investigation, then has an honest signal to act on rather than a
// gap this file quietly papered over).
func resolveKeyFileForSFTP(src transport.Source) (secret obs.Secret, ok bool, err error) {
	encSecret, hasEnc, err := resolveKeyEncryptionSecret(src)
	if err != nil {
		return obs.Secret{}, false, fmt.Errorf("resolving the key_encryption source: %w", err)
	}
	if !hasEnc {
		return obs.Secret{}, false, nil
	}

	dek := deriveKeyEncryptionDEK(encSecret)
	defer func() { dek = [32]byte{} }()

	keyFilePath := env.ShellExpand(src.KeyFile)
	raw, err := os.ReadFile(keyFilePath)
	if err != nil {
		return obs.Secret{}, false, fmt.Errorf("key_file %q: %w", src.KeyFile, err)
	}
	defer zeroBytes(raw)

	if isEncryptedKeyMaterial(raw) {
		plaintext, err := decryptKeyMaterial(dek, raw)
		if err != nil {
			return obs.Secret{}, false, fmt.Errorf("key_file %q: %w", src.KeyFile, err)
		}
		defer zeroBytes(plaintext)
		return obs.NewSecret(string(plaintext)), true, nil
	}

	// #298 migration: raw is plaintext, exactly what an installation from
	// before this feature (or an import made before a key_encryption
	// source was configured) left on disk, defended only by #293's
	// permission hardening.
	ciphertext, err := encryptKeyMaterial(dek, raw)
	if err != nil {
		return obs.Secret{}, false, fmt.Errorf("key_file %q: encrypting for migration: %w", src.KeyFile, err)
	}
	if err := writeKeyFileAtomically(keyFilePath, ciphertext); err != nil {
		return obs.Secret{}, false, fmt.Errorf("key_file %q: migrating to at-rest encryption: %w", src.KeyFile, err)
	}

	return obs.NewSecret(string(raw)), true, nil
}

// writeKeyFileAtomically replaces path's content with b via a
// temp-file-plus-rename in the same directory, so a reader (this
// process's own next connection attempt, an operator's own inspection)
// never observes a half-written key file, mirroring core/service's
// writeConfigBytesAtomically. Duplicated here rather than shared, since
// core/service already imports this package (backupsets.go's
// ValidateImportedPrivateKey) and importing it back would be a cycle;
// see keysource.go's package doc for why core/internal/transport/rclone
// exists as the one place this project's rclone-adjacent logic lives, a
// boundary a shared helper in the other direction would cross.
func writeKeyFileAtomically(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temporary file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("setting permissions: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing key file: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("syncing the containing directory: %w", err)
	}
	defer d.Close()
	return d.Sync()
}
