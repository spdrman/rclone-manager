package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/backupdproject/backupd/core/internal/transport/rclone"
)

// Importing S3 credentials: the same one-way door POST /ssh-keys already
// is, for the secret that unlocks a whole storage medium (G2.2, #594).
//
// # Why this exists at all
//
// config.MediumCredentials names three sources and there is no fourth,
// deliberately: file, env, command, and no field an inline secret fits
// in. That is the right schema and this file does not change it. What it
// does is close the gap the schema leaves for a person: an operator who
// has just been handed an access key by their provider cannot use any of
// the three without leaving the browser first, putting a file on the
// host, or standing up a secrets manager. So the material comes in ONCE,
// here, and what comes back is an id that resolves to the `file` source
// the schema already prefers.
//
// It is deliberately the same shape as ImportSSHKey (backupsets.go) down
// to the directory-beside-config.yaml and the 0600, because this project
// already decided how a secret arrives and where it lives, and inventing
// a second answer for S3 would mean two custody models to keep correct.
//
// # What never happens here
//
// The material is never returned, never logged, and never echoed in a
// refusal. A refusal reports the SHAPE of the problem, which is
// rclone.RenderImportedMediumCredentials' own contract. There is no read
// side: nothing in this package can hand back what was written, so the
// only way to learn a stored secret is to be root on the host, which is
// the position the file's 0600 already assumes.

// mediumCredentialsDirName is the directory imported credentials files
// live in, beside config.yaml.
//
// Beside config.yaml, and emphatically NOT under the backup root, which
// is the rule config.MediumCredentials.File's own doc states and the
// reason it states: the backup root is what a NAS deployment exports over
// SMB or AFP, and #298 was filed over precisely that exposure for the SSH
// key. Beside config.yaml it mounts, and backs up, with the file it is
// referenced from, which is the same locality keysDirIn chose for
// ssh_keys.
const mediumCredentialsDirName = "s3_credentials"

// MediumCredentialRef identifies one imported credential, without ever
// carrying its material.
//
// It is SSHKeyRef with one field fewer, and the missing field is the
// point. SSHKeyRef carries an Algorithm and a Fingerprint because an SSH
// public key's fingerprint is a safe, useful thing to show a person. An
// S3 credential has no such half: FR-33 treats the ACCESS KEY ID as
// secret too (see rclone's resolvedCredentials, where the access key id
// is an obs.Secret alongside the secret access key), so there is nothing
// derived from this material that may be displayed, and this type
// therefore displays nothing.
type MediumCredentialRef struct {
	// ID is the opaque reference a caller carries around and later passes
	// back as StorageMediumCredentials.ID. It is a bare uuid: it names
	// nothing about this host, so it can travel on a request body, in a
	// URL, in an echoed command line and in an exported terminal
	// transcript without disclosing anything.
	ID string

	// File is the server-side path the reference resolves to. It is used
	// inside this package to write the medium's declaration and is never
	// sent back over the wire by the HTTP layer, exactly as
	// SSHKeyRef.KeyFile is not: a path is a fact about this host's
	// filesystem an API caller has no use for.
	File string
}

// mediumCredentialsDirIn is where ImportStorageCredentials persists an
// imported credentials file. See keysDirIn, which this mirrors: it takes
// configPath rather than hanging off *BackupService so a surface without
// one resolves the same directory from the same path.
//
// 0700 on the directory as well as 0600 on the file, because
// rclone's own checkCredentialsFileCustody refuses a credentials file
// whose containing directory is group- or world-writable: a mode that
// passed here and failed there would be an import that succeeded and a
// backup that could never authenticate.
func mediumCredentialsDirIn(configPath string) (string, error) {
	if configPath == "" {
		return "", ErrConfigNotFileBacked
	}
	dir := filepath.Join(filepath.Dir(configPath), mediumCredentialsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// ErrMediumCredentialNotFound is what a reference to a credential this
// deployment never minted resolves to.
//
// Its own sentinel rather than an ErrInvalidRequest, for
// ErrSSHKeyNotFound's reason: a caller has to be able to tell "that id is
// not one of mine" from "your request was malformed", because the first
// one has an action attached (import the credential first) and the second
// one does not.
var ErrMediumCredentialNotFound = fmt.Errorf("service: storage credential not found")

// resolveMediumCredentialsFileIn turns a MediumCredentialRef.ID back into
// the server-side path it was written to, confirming the file is there.
//
// id is checked for a path separator BEFORE it is joined onto the
// directory, exactly as resolveSSHKeyFileIn checks its own: this package
// only ever mints a bare uuid.NewString(), so an id carrying "/" or "\"
// is never a legitimate reference and is refused outright rather than let
// filepath.Join resolve it somewhere else on this host. That check is
// what makes it safe for this id to be the ONE credential-shaped thing an
// API caller sends, which is the whole basis of the verify-before-save
// design (see mediums.go).
//
// The refusal never quotes the id back. A caller that sent a traversal
// attempt learns that it was refused and learns nothing about where it
// would have landed.
func resolveMediumCredentialsFileIn(configPath, id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("%w: a credentials id is required", ErrInvalidRequest)
	}
	if strings.ContainsAny(id, "/\\") || id == "." || id == ".." {
		return "", fmt.Errorf("%w: that is not a credentials id this deployment issued", ErrInvalidRequest)
	}
	dir, err := mediumCredentialsDirIn(configPath)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, id)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%w: %s", ErrMediumCredentialNotFound, id)
	}
	return path, nil
}

// ImportStorageCredentials writes an access key id and a secret access key
// into a 0600 AWS shared-credentials file beside config.yaml and returns
// an opaque id for it.
//
// The material is validated BEFORE anything is written
// (rclone.RenderImportedMediumCredentials, which is the same parser the
// `env` and `command` sources' own output goes through at connection
// time), so an import that is refused leaves nothing behind and the two
// paths cannot disagree about what usable credentials are.
//
// sessionToken is "" for the ordinary long-lived key a provider console
// hands out. It is accepted because a temporary credential is a perfectly
// reasonable thing for an operator to be holding, and refusing to store
// one would push them to the `command` source for no reason.
//
// Nothing here returns, logs or echoes the material. See this file's
// package doc.
func (b *BackupService) ImportStorageCredentials(_ context.Context, accessKeyID, secretAccessKey, sessionToken string) (MediumCredentialRef, error) {
	return importStorageCredentialsInto(b.configPath, accessKeyID, secretAccessKey, sessionToken)
}

// ImportStorageCredentialsText is ImportStorageCredentials for material
// that already IS shared-credentials text, which is what `backupd
// medium import-credentials --stdin` reads.
//
// It exists as its own method rather than as a parse in the CLI for the
// reason every other pair in this package is a pair: the CLI and the API
// have to agree about what a usable credential is, and two parsers is two
// answers. The text is validated (a [default] profile, a readable key
// pair) and then written VERBATIM, so a file an operator already trusts
// is stored as the thing they trusted rather than as this product's
// re-rendering of it.
func (b *BackupService) ImportStorageCredentialsText(_ context.Context, raw []byte) (MediumCredentialRef, error) {
	if err := rclone.ValidateImportedMediumCredentials(raw); err != nil {
		return MediumCredentialRef{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return writeMediumCredentials(b.configPath, raw)
}

// importStorageCredentialsInto is ImportStorageCredentials' configPath-only
// half; see keysDirIn's own doc for why this package splits methods this
// way.
func importStorageCredentialsInto(configPath, accessKeyID, secretAccessKey, sessionToken string) (MediumCredentialRef, error) {
	raw, err := rclone.RenderImportedMediumCredentials(accessKeyID, secretAccessKey, sessionToken)
	if err != nil {
		return MediumCredentialRef{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return writeMediumCredentials(configPath, raw)
}

// writeMediumCredentials persists already-validated credentials text and
// mints its reference.
//
// os.WriteFile with 0600 rather than the atomic replace the config file
// gets, and the difference is deliberate: this file is created at a name
// nothing has ever read, so there is no reader to tear and nothing to
// replace. What matters is that the mode is right at creation rather than
// fixed afterwards, which os.WriteFile's own O_CREATE|mode gives (subject
// to umask, which is why the mode is asserted rather than assumed below).
func writeMediumCredentials(configPath string, raw []byte) (MediumCredentialRef, error) {
	dir, err := mediumCredentialsDirIn(configPath)
	if err != nil {
		return MediumCredentialRef{}, err
	}
	id := uuid.NewString()
	path := filepath.Join(dir, id)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		// The path, not the material. A write failure is about this
		// host's filesystem and says so.
		return MediumCredentialRef{}, fmt.Errorf("service: persisting imported storage credentials: %w", err)
	}
	// A umask can only ever REMOVE bits from the mode WriteFile asks for,
	// so this cannot widen the file; it is here because a mode that came
	// out narrower than 0600 (0400 under an exotic umask) would make
	// every later write to this file fail in a way nothing else would
	// explain.
	if err := os.Chmod(path, 0o600); err != nil {
		return MediumCredentialRef{}, fmt.Errorf("service: setting permissions on imported storage credentials: %w", err)
	}
	return MediumCredentialRef{ID: id, File: path}, nil
}
