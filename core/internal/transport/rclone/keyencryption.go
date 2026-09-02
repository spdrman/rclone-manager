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
// Current (V2) encrypted key material is:
//
//	encryptedKeyMagicV2 || salt (16 bytes) || nonce (12 bytes) || AES-256-GCM(nonce, plaintext) || GCM tag (16 bytes)
//
// A file still carrying the original (V1) format, not yet touched since
// the DEK derivation below was hardened, is:
//
//	encryptedKeyMagicV1 || nonce (12 bytes) || AES-256-GCM(nonce, plaintext) || GCM tag (16 bytes)
//
// Each magic is chosen so it can never collide with a real key: an
// unencrypted PEM-format private key (the only format ValidateImportedPrivateKey
// and #74's resolvers ever accept) always begins "-----BEGIN ", so a
// byte-prefix check is a complete, unambiguous format detector with no
// parsing and no chance of mistaking one for the other in either
// direction. That is also why this needs no separate version field beyond
// what is baked into the magic string itself: V2 is exactly the "a V2
// format, if one is ever needed, gets its own magic" case this file
// always planned for, added when the DEK derivation itself needed to
// change shape (a salt) rather than just its cost.
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
// 32 raw bytes AES-256 needs, and #298's own report raised a second
// problem alongside the length one: key_encryption.env/.command are
// documented as accepting "a secrets manager", which an operator can just
// as easily point at a typed passphrase, and a stolen encrypted key file
// is exactly the shape of thing an offline dictionary attack is run
// against. A fast, unsalted, non-iterated hash (this file's original
// sha256.Sum256, kept below as deriveKeyEncryptionDEKV1 for backward
// compatibility only) turns whatever comes back into 32 bytes
// deterministically, but offers a guesser nothing to slow them down:
// every candidate costs one SHA-256 call.
//
// deriveKeyEncryptionDEK instead runs the resolved secret through
// Argon2id (golang.org/x/crypto/argon2) before it ever becomes an AES
// key, with the same parameters #322/#327's
// apps/common/auth/local/password.go already uses for hashing the Web UI
// administrator's password (see keyEncryptionArgon2Time/Memory/Threads/
// KeyLen) -- this is not a new crypto dependency, it is this project's
// existing one, reused for the same reason it exists there: making each
// guess cost real memory and real time instead of one hash call.
//
// Argon2id needs a salt, and unlike password.go's per-record hash (where
// the salt travels inside that one stored string and is never reused
// elsewhere), this DEK is config-wide: every Source built from the same
// key_encryption block derives it the same way. This file's on-disk
// format is already the one place a fresh per-encryption value lives (see
// the nonce below), so the salt lives right next to it: format V2 adds a
// salt field, generated fresh by encryptKeyMaterial exactly like the
// nonce already is, and read back by decryptKeyMaterial from whichever
// file it is decrypting. That keeps the salt self-contained inside the
// one file it protects, needs no new state anywhere else (no new
// config.yaml field, no side-car file), and gives every encrypted key its
// own salt rather than one shared across every Source that happens to use
// the same key_encryption config.
//
// A key file already encrypted under the original SHA-256 scheme
// (encryptedKeyMagicV1) still decrypts -- deriveKeyEncryptionDEKV1 exists
// for exactly that, and only that -- and resolveKeyFileForSFTP upgrades it
// to V2 in place the next time anything touches it, the same self-heal
// discipline the plaintext-to-V1 migration below already established.
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
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/rclone/rclone/lib/env"

	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// encryptedKeyMagicV1 prefixes a key file encrypted under #298's original
// scheme: an unsalted sha256.Sum256-derived DEK (deriveKeyEncryptionDEKV1).
// decryptKeyMaterial still reads it, for a file migrated before this DEK
// derivation was hardened to Argon2id; nothing in this file writes it any
// more. See this file's package doc for why a byte-prefix check against an
// exact string like this is a complete detector: a real PEM private key
// can never begin with it.
var encryptedKeyMagicV1 = []byte("RCLONEMGR-KEYENC-V1:")

// encryptedKeyMagicV2 prefixes a key file encrypted under the current
// scheme: an Argon2id-derived DEK, salted with the 16 bytes that
// immediately follow this magic (keyEncryptionSaltLen). Every fresh
// encryption this file performs -- a first migration, a V1-to-V2 upgrade,
// a later import -- writes this format.
var encryptedKeyMagicV2 = []byte("RCLONEMGR-KEYENC-V2:")

// gcmNonceSize is AES-GCM's standard nonce size. cipher.NewGCM defaults to
// exactly this; it is named here only so encryptKeyMaterial and
// decryptKeyMaterial agree on it without either hard-coding "12" twice.
const gcmNonceSize = 12

// keyEncryptionSaltLen is the size of the random salt format V2 stores
// alongside the ciphertext, mirroring apps/common/auth/local/password.go's
// own saltLen exactly (16 bytes).
const keyEncryptionSaltLen = 16

// keyEncryptionArgon2Time/Memory/Threads/KeyLen are Argon2id's cost
// parameters for deriving a config-wide DEK from a resolved
// key_encryption secret. Kept identical to
// apps/common/auth/local/password.go's own argon2Time/Memory/Threads/
// KeyLen (#322/#327): the same shape of problem -- turning a
// human-plausible secret into something an offline guesser cannot cheaply
// brute-force -- gets the same answer, comfortably above OWASP's minimum
// (m=19MiB, t=2, p=1) without making every SFTP connection attempt a
// noticeable pause.
const (
	keyEncryptionArgon2Time    uint32 = 3
	keyEncryptionArgon2Memory  uint32 = 64 * 1024 // 64 MiB
	keyEncryptionArgon2Threads uint8  = 2
	keyEncryptionArgon2KeyLen  uint32 = 32
)

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

// isEncryptedKeyMaterial reports whether raw is either of this program's
// own #298 at-rest encryption formats (V1 or V2, see encryptedKeyMagicV1/
// V2's own docs), rather than a plain PEM private key.
func isEncryptedKeyMaterial(raw []byte) bool {
	return bytes.HasPrefix(raw, encryptedKeyMagicV1) || bytes.HasPrefix(raw, encryptedKeyMagicV2)
}

// isLegacyEncryptedKeyMaterial reports whether raw is specifically V1's
// format: encrypted, but under the original unsalted SHA-256 derivation
// rather than the current Argon2id one. resolveKeyFileForSFTP uses this to
// decide whether a file it just decrypted also needs an in-place upgrade
// to V2, the same self-heal-on-next-touch discipline it already applies to
// a still-plaintext file.
func isLegacyEncryptedKeyMaterial(raw []byte) bool {
	return bytes.HasPrefix(raw, encryptedKeyMagicV1)
}

// deriveKeyEncryptionDEK turns secret's resolved content, plus salt, into
// the exact 32 bytes AES-256 needs, via Argon2id. See this file's package
// doc, "Why the DEK is derived, not used raw", for why this is Argon2id
// (not the length-normalizing SHA-256 deriveKeyEncryptionDEKV1 uses) and
// why salt lives in the on-disk format rather than anywhere else.
func deriveKeyEncryptionDEK(secret obs.Secret, salt []byte) [32]byte {
	raw := argon2.IDKey([]byte(secret.Reveal()), salt, keyEncryptionArgon2Time, keyEncryptionArgon2Memory, keyEncryptionArgon2Threads, keyEncryptionArgon2KeyLen)
	defer zeroBytes(raw)

	var dek [32]byte
	copy(dek[:], raw)
	return dek
}

// deriveKeyEncryptionDEKV1 is #298's original DEK derivation: an unsalted
// sha256.Sum256 of secret's resolved content. Kept ONLY so a key file
// still in V1's on-disk format can be decrypted; resolveKeyFileForSFTP
// upgrades every V1 file it touches to V2 (deriveKeyEncryptionDEK,
// Argon2id) immediately afterward, and nothing in this file ever calls
// this to encrypt new material.
func deriveKeyEncryptionDEKV1(secret obs.Secret) [32]byte {
	return sha256.Sum256([]byte(secret.Reveal()))
}

// encryptKeyMaterial encrypts plaintext (a key file's real PEM content)
// under a freshly Argon2id-derived DEK from secret, returning
// encryptedKeyMagicV2 || salt (keyEncryptionSaltLen bytes) || nonce (12
// bytes) || ciphertext-with-tag, exactly the format
// isEncryptedKeyMaterial/decryptKeyMaterial's V2 branch expect. Every
// call -- a first migration, a V1-to-V2 upgrade, a later import -- gets
// its own fresh salt AND fresh nonce, generated independently: reusing
// either across two encryptions under the same secret is the mistake this
// function exists to never make.
func encryptKeyMaterial(secret obs.Secret, plaintext []byte) ([]byte, error) {
	salt := make([]byte, keyEncryptionSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating a salt: %w", err)
	}
	dek := deriveKeyEncryptionDEK(secret, salt)
	defer func() { dek = [32]byte{} }()

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

	out := make([]byte, 0, len(encryptedKeyMagicV2)+len(salt)+len(nonce)+len(plaintext)+gcm.Overhead())
	out = append(out, encryptedKeyMagicV2...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// decryptKeyMaterial is encryptKeyMaterial's inverse, plus V1 backward
// compatibility. raw MUST already have been confirmed to carry
// encryptedKeyMagicV1 or encryptedKeyMagicV2 (isEncryptedKeyMaterial);
// this returns an error, rather than panicking, for anything shorter than
// a valid header, on the same "never trust a file's shape just because
// its prefix matched" discipline the rest of this project's resolvers
// apply to external input.
func decryptKeyMaterial(secret obs.Secret, raw []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(raw, encryptedKeyMagicV2):
		rest := raw[len(encryptedKeyMagicV2):]
		if len(rest) < keyEncryptionSaltLen+gcmNonceSize {
			return nil, errors.New("truncated: shorter than a salt and a nonce")
		}
		salt, rest := rest[:keyEncryptionSaltLen], rest[keyEncryptionSaltLen:]
		nonce, ciphertext := rest[:gcmNonceSize], rest[gcmNonceSize:]
		dek := deriveKeyEncryptionDEK(secret, salt)
		defer func() { dek = [32]byte{} }()
		return openKeyMaterialGCM(dek, nonce, ciphertext)
	case bytes.HasPrefix(raw, encryptedKeyMagicV1):
		rest := raw[len(encryptedKeyMagicV1):]
		if len(rest) < gcmNonceSize {
			return nil, errors.New("truncated: shorter than a nonce")
		}
		nonce, ciphertext := rest[:gcmNonceSize], rest[gcmNonceSize:]
		dek := deriveKeyEncryptionDEKV1(secret)
		defer func() { dek = [32]byte{} }()
		return openKeyMaterialGCM(dek, nonce, ciphertext)
	default:
		return nil, errors.New("not in this program's key-encryption format")
	}
}

// openKeyMaterialGCM is decryptKeyMaterial's shared final step, once a
// version-specific branch has produced dek, nonce and ciphertext: both
// formats end at the identical AES-GCM open.
func openKeyMaterialGCM(dek [32]byte, nonce, ciphertext []byte) ([]byte, error) {
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

// keyEncryptionSecretCache memoizes resolveKeyEncryptionSecret's result per
// distinct key_encryption source, for the life of this process.
//
// Without it, every one of sftpConfig's callers -- List, Stat,
// CopyToLocal, RemoteHash, DeleteRemote, once per artifact those touch --
// reaches resolveKeyFileForSFTP, which reached resolveKeyEncryptionSecret
// again from scratch every single time: a fresh key_encryption.command
// subprocess spawn (its own 15s timeout), a fresh file read, or a fresh
// env lookup, on every call. A set with many artifacts turns that into
// potentially hundreds of subprocess spawns per cycle, and a
// slow/flaky/rate-limited secrets manager inflates cycle duration or
// introduces failure modes that have nothing to do with the backup work
// actually being done.
//
// Keyed by keyEncryptionSourceIdentity(src), not by transport.Source
// itself: Source carries fields (Host, Root, ID, ...) that have nothing to
// do with which key_encryption source produced this secret, and keying by
// the whole struct would fragment one config-wide secret into one cache
// entry per backup set for no reason. transport.Source's own doc confirms
// KeyEncryptionFile/Env/Command are themselves config-wide -- identical
// across every Source built from the same *config.Config -- so in
// practice this cache holds at most a handful of entries, one per distinct
// key_encryption configuration this process has ever seen, and a config
// reload that changes key_encryption lands on a new cache key naturally,
// with no separate invalidation hook needed: core/service's hot reload
// (see internal/app/alerts.go's AdoptAlerts doc for the general shape)
// carries this same *Adapter forward across a reload, so nothing here
// clears the cache; a key_encryption block that did not change simply
// keeps its already-resolved secret, which is the correct answer either
// way.
//
// A resolution FAILURE is cached too. The whole point is "do not repeat
// the subprocess spawn"; a slow or failing key_encryption.command is
// exactly the case worth NOT retrying on every single call within one
// cycle, so it fails once per process, not once per artifact.
var (
	keyEncryptionSecretCacheMu sync.Mutex
	keyEncryptionSecretCache   = map[string]keyEncryptionSecretCacheEntry{}
)

type keyEncryptionSecretCacheEntry struct {
	secret obs.Secret
	ok     bool
	err    error
}

// keyEncryptionSourceIdentity returns a string that uniquely identifies
// which of src's three key_encryption sources resolveKeyEncryptionSecret
// would resolve. Two Sources with the same file path, the same env var
// name, or the same command argv always produce the same identity; this
// mirrors resolveKeyEncryptionSecret's own dispatch order and fields
// exactly, so the two can never disagree about what "the same source"
// means.
func keyEncryptionSourceIdentity(src transport.Source) string {
	switch {
	case src.KeyEncryptionFile != "":
		return "file:" + src.KeyEncryptionFile
	case src.KeyEncryptionEnv != "":
		return "env:" + src.KeyEncryptionEnv
	case len(src.KeyEncryptionCommand) > 0:
		return "command:" + strings.Join(src.KeyEncryptionCommand, "\x00")
	default:
		return ""
	}
}

// resolveKeyEncryptionSecretCached is resolveKeyFileForSFTP's entry point
// into key_encryption resolution: identical to calling
// resolveKeyEncryptionSecret directly the first time a given source is
// seen by this process, and a cache hit -- no subprocess, no file read, no
// env lookup -- on every call after that. See keyEncryptionSecretCache's
// own doc for why this is safe and what it is keyed by.
func resolveKeyEncryptionSecretCached(src transport.Source) (obs.Secret, bool, error) {
	identity := keyEncryptionSourceIdentity(src)
	if identity == "" {
		// No key_encryption source configured at all: resolveKeyEncryptionSecret
		// is a no-op in this case (its own default branch), so there is
		// nothing worth caching and no reason to take the lock.
		return obs.Secret{}, false, nil
	}

	keyEncryptionSecretCacheMu.Lock()
	entry, cached := keyEncryptionSecretCache[identity]
	keyEncryptionSecretCacheMu.Unlock()
	if cached {
		return entry.secret, entry.ok, entry.err
	}

	secret, ok, err := resolveKeyEncryptionSecret(src)

	keyEncryptionSecretCacheMu.Lock()
	keyEncryptionSecretCache[identity] = keyEncryptionSecretCacheEntry{secret: secret, ok: ok, err: err}
	keyEncryptionSecretCacheMu.Unlock()

	return secret, ok, err
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
// three ways:
//
//   - the file already carries encryptedKeyMagicV2 (the current format):
//     decrypted in memory with the resolved DEK and returned unchanged.
//     This is the steady state for any key imported, or already migrated,
//     since this file's Argon2id hardening landed.
//   - the file carries encryptedKeyMagicV1 (isLegacyEncryptedKeyMaterial,
//     #298's original, now-superseded scheme): decrypted with the legacy
//     SHA-256-derived DEK, then immediately re-encrypted under V2 (a fresh
//     salt, the current Argon2id-derived DEK) and written back in place,
//     before the plaintext already in memory is returned for key_pem. An
//     installation that migrated before the DEK derivation was hardened
//     self-heals onto the stronger format the next time it connects,
//     exactly like the plaintext case below, and never re-reads what it
//     just rewrote.
//   - the file is still plaintext PEM (#298's original migration path):
//     encrypted (V2) and written back to keyFilePath in place, replacing
//     the plaintext copy on disk, before the ORIGINAL plaintext bytes
//     (already in memory; never re-read from the file this call just
//     rewrote) are returned for key_pem. This is what makes an
//     installation upgraded from before #298 self-heal the first time it
//     actually connects, with no separate migration command an operator
//     has to remember to run, and covers a key imported today by
//     ImportSSHKey, which -- unchanged by #298 -- still writes plaintext
//     at import time; this function is the ONE place any kind of
//     plaintext-or-weaker-than-current key material gets (re-)encrypted,
//     so import time, "an old install upgraded", and "an install
//     upgraded again onto a stronger KDF" never need three different code
//     paths.
//
// A migration or upgrade write failure (a read-only filesystem, a full
// disk) is reported in the returned error rather than silently swallowed:
// the plaintext this process already holds would still authenticate, but
// returning success while leaving that failure unreported would hide a
// real "this deployment's key is still unprotected (or still on the
// weaker derivation)" condition from whatever surfaces this error (the
// operator's own retry, or their own next investigation, then has an
// honest signal to act on rather than a gap this file quietly papered
// over).
func resolveKeyFileForSFTP(src transport.Source) (secret obs.Secret, ok bool, err error) {
	encSecret, hasEnc, err := resolveKeyEncryptionSecretCached(src)
	if err != nil {
		return obs.Secret{}, false, fmt.Errorf("resolving the key_encryption source: %w", err)
	}
	if !hasEnc {
		return obs.Secret{}, false, nil
	}

	keyFilePath := env.ShellExpand(src.KeyFile)
	raw, err := os.ReadFile(keyFilePath)
	if err != nil {
		return obs.Secret{}, false, fmt.Errorf("key_file %q: %w", src.KeyFile, err)
	}
	defer zeroBytes(raw)

	if isEncryptedKeyMaterial(raw) {
		plaintext, err := decryptKeyMaterial(encSecret, raw)
		if err != nil {
			return obs.Secret{}, false, fmt.Errorf("key_file %q: %w", src.KeyFile, err)
		}
		defer zeroBytes(plaintext)

		if isLegacyEncryptedKeyMaterial(raw) {
			if err := reencryptKeyFile(keyFilePath, encSecret, plaintext); err != nil {
				return obs.Secret{}, false, fmt.Errorf("key_file %q: upgrading to a stronger key derivation: %w", src.KeyFile, err)
			}
		}

		return obs.NewSecret(string(plaintext)), true, nil
	}

	// #298 migration: raw is plaintext, exactly what an installation from
	// before this feature (or an import made before a key_encryption
	// source was configured) left on disk, defended only by #293's
	// permission hardening.
	if err := reencryptKeyFile(keyFilePath, encSecret, raw); err != nil {
		return obs.Secret{}, false, fmt.Errorf("key_file %q: migrating to at-rest encryption: %w", src.KeyFile, err)
	}

	return obs.NewSecret(string(raw)), true, nil
}

// reencryptKeyFile encrypts plaintext under secret (the current V2 format:
// Argon2id, a fresh salt) and writes it to keyFilePath atomically. The one
// helper behind both of resolveKeyFileForSFTP's write-back paths -- a
// plaintext file's first encryption, and a V1 file's upgrade to V2 -- so
// the two never drift into encrypting or writing back differently.
func reencryptKeyFile(keyFilePath string, secret obs.Secret, plaintext []byte) error {
	ciphertext, err := encryptKeyMaterial(secret, plaintext)
	if err != nil {
		return fmt.Errorf("encrypting: %w", err)
	}
	return writeKeyFileAtomically(keyFilePath, ciphertext)
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
