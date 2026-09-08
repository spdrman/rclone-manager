package rclone

import (
	"fmt"
	"os"

	"github.com/rclone/rclone/lib/env"
	"golang.org/x/crypto/ssh"

	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is the credentials half of EPIC G's connection test (issue
// #596): resolving a backup set's private key into something that can
// actually be offered to a server, through the SAME three sources,
// passphrase handling and at-rest decryption sftpConfig already uses.
//
// It lives here rather than in the checker for the reason keysource.go's
// package doc already gives: this package is the one place this project's
// SSH posture lives, and a second resolver that read key_file, key_env
// and key_command its own way is a second posture. The check needs a
// signer, not a rclone configmap, and that is the only difference.

// SourceSigner resolves src's configured private key and returns a signer
// for it, plus the SHA256 fingerprint of its PUBLIC half in the same form
// `ssh-keygen -lf` prints.
//
// The fingerprint is the operator-facing half and the reason this returns
// two values. It is the exact string that appears in the far side's
// authorized_keys audit, so "the key this manager would offer" and "the
// key you authorised over there" become two things that can be compared
// by eye rather than two things that are both called "the key".
//
// Nothing here writes anything, and nothing here reaches the network. A
// failure is a fact about THIS host: a file that cannot be read, an
// environment variable that is not set, a command that failed, a
// passphrase that does not open the key, or key material this build
// cannot parse.
//
// The error may name a path or a variable on this host, which is exactly
// what makes it useful in a log and exactly what makes it unfit for an
// API response. internal/sourcecheck is the boundary that enforces that
// split: it hands this error to its Observe hook and composes its own
// sentence for the wire.
func SourceSigner(src transport.Source) (ssh.Signer, string, error) {
	passphraseSecret, hasPassphrase, err := resolvePassphrase(src)
	if err != nil {
		return nil, "", fmt.Errorf("resolving the SSH key passphrase: %w", err)
	}
	passphrase := ""
	if hasPassphrase {
		passphrase = passphraseSecret.Reveal()
	}

	var material obs.Secret
	switch {
	case src.KeyFile != "":
		material, err = sourceKeyFromFile(src, passphrase)
	case src.KeyEnv != "":
		material, err = resolveKeyFromEnv(src.KeyEnv, passphrase)
	case len(src.KeyCommand) > 0:
		material, err = resolveKeyFromCommand(src.KeyCommand, passphrase)
	default:
		return nil, "", fmt.Errorf("this backup set names no SSH key: exactly one of key_file, key_env or key_command is required")
	}
	if err != nil {
		return nil, "", err
	}

	signer, err := parseSigner([]byte(material.Reveal()), passphrase)
	if err != nil {
		return nil, "", err
	}
	return signer, ssh.FingerprintSHA256(signer.PublicKey()), nil
}

// sourceKeyFromFile reads key_file, honouring #298's at-rest encryption
// exactly as sftpConfig does.
//
// The one difference from sftpConfig's key_file branch is that this one
// always ends up with the material in memory. sftpConfig can hand rclone
// a path and never open the file itself, which is the whole reason
// key_file is the documented preference; a connection test that wants to
// report the offered key's fingerprint as its own step cannot, because
// there is no fingerprint without the key. The material is held for the
// length of one check and never persisted, logged or returned.
func sourceKeyFromFile(src transport.Source, passphrase string) (obs.Secret, error) {
	// The same mode and directory-chain checks sftpConfig runs before
	// anything reads the file's content (#293/#311). A world-writable
	// directory lets any local actor replace the key, and a check that
	// skipped this would report a key as readable that a real transfer
	// refuses to use.
	keyFilePath := env.ShellExpand(src.KeyFile)
	info, err := os.Stat(keyFilePath)
	if err != nil {
		return obs.Secret{}, fmt.Errorf("key_file %q is not accessible: %w", src.KeyFile, err)
	}
	if err := checkKeyFileMode(src.ID, src.KeyFile, info); err != nil {
		return obs.Secret{}, err
	}
	if err := checkKeyDirChainMode(src.ID, src.KeyFile, keyFilePath); err != nil {
		return obs.Secret{}, err
	}

	// #298: decrypt (or migrate-then-decrypt) when this deployment
	// configures at-rest encryption. ok == false means it does not, and
	// the file is the plaintext PEM it has always been.
	secret, encrypted, err := resolveKeyFileForSFTP(src)
	if err != nil {
		return obs.Secret{}, err
	}
	if encrypted {
		return secret, nil
	}

	raw, err := os.ReadFile(keyFilePath)
	if err != nil {
		return obs.Secret{}, fmt.Errorf("key_file %q: %w", src.KeyFile, err)
	}
	defer zeroBytes(raw)
	if isEncryptedKeyMaterial(raw) {
		// The file holds #298 ciphertext and no key_encryption source is
		// configured to open it. Said plainly rather than reported as
		// unparseable key material, because the fix is a configuration
		// block and not a new key.
		return obs.Secret{}, fmt.Errorf("key_file %q holds at-rest encrypted key material and no key_encryption source is configured to open it", src.KeyFile)
	}
	return obs.NewSecret(string(raw)), nil
}

// parseSigner turns PEM into a signer, with the passphrase when one is
// configured. Both branches exist because x/crypto/ssh refuses a
// passphrase for an unencrypted key rather than ignoring it, which is the
// behaviour validateAndWrapKey already relies on.
func parseSigner(pem []byte, passphrase string) (ssh.Signer, error) {
	defer zeroBytes(pem)
	if passphrase != "" {
		signer, err := ssh.ParsePrivateKeyWithPassphrase(pem, []byte(passphrase))
		if err != nil {
			return nil, fmt.Errorf("opening the private key with the configured passphrase: %w", err)
		}
		return signer, nil
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		return nil, fmt.Errorf("parsing the private key: %w", err)
	}
	return signer, nil
}
