package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Issue #555: who a deployment IS, as opposed to what it currently holds.
//
// # The gap this fills
//
// A read command compares the engine's config_revision against the one it
// computed from the file it loaded, and refuses when they disagree
// (readmode.go, #544). A routed WRITE compared nothing at all, so a
// `backup-set create` typed at one deployment with another deployment's
// address in $BACKUP_MANAGER_API_URL landed in the other one, with the
// near deployment's config.yaml untouched and both surfaces reporting
// success. That is the wrong way round: a read that goes astray shows an
// operator something confusing, a write that goes astray changes a
// deployment nobody was looking at.
//
// Reusing config_revision for the write would have closed the case that
// was demonstrated and left the sharper one open. A revision is a hash of
// configuration CONTENT (configrevision.go says so in as many words), so
// two deployments built from one template hold the same one. A staging and
// a production instance from the same compose file are exactly that, and
// they are also exactly the pair an operator is most likely to point the
// wrong address at.
//
// So this is a different fact: an identity for the deployment itself,
// stable across every configuration edit and distinct between instances.
//
// # Where it comes from, and why not from the alternatives
//
// It is minted once, at random, and kept in a file beside the journal,
// exactly where the three advisory locks already live (startup.go's
// suffixes). "Beside the journal" makes "same deployment" and "same
// identity" the identical question, with no configuration to get wrong,
// which is the same argument those locks are placed by.
//
// Deriving it from the state database's own identity instead (its inode,
// its device, its creation time) was the first idea and it is wrong twice.
// A journal restored from a backup is a new inode, so a deployment that
// was restored would stop being itself and every routed write against it
// would refuse. And the engine reads that file from inside a container
// while the CLI may read it across a bind mount, so the two ends can
// legitimately see different device numbers for one file.
//
// Deriving it from something already in the journal (the first migration's
// applied_at is the only candidate) fails on collisions and costs more to
// read: two deployments initialised by one script share a timestamp, and
// getting at it means opening SQLite, which a command holding the startup
// lock has no business doing. A file is one os.ReadFile.
//
// # What happens to a deployment that is restored, or cloned
//
// Restored: the identity comes back with the state directory, so the
// deployment is still itself and routed writes carry on working. If only
// the .db file is restored, into a state directory with no identity file,
// the next process to open the journal mints a fresh one. That changes
// what the deployment is called and breaks nothing, because both ends read
// the same file: they still agree with each other, and they still differ
// from every other deployment.
//
// Cloned from one image: each clone runs its own first start against its
// own state directory and mints its own identity, so two instances built
// from one template are told apart even though their configurations are
// byte-identical. That is the case a content hash gets wrong and this
// gets right.
//
// The one case it cannot see is a whole state directory copied wholesale
// to seed a second deployment. Both copies then carry one identity and
// claim to be each other. Nothing stored inside a deployment's own data
// can tell those apart, and the alternative (deriving the identity from
// the host) breaks the restore case, which is the commoner one by a long
// way. It is a limitation, not an oversight.
//
// # Nobody is authenticated by this
//
// It is an identity in the "which one is this" sense, not a credential. It
// is served to any authenticated client on GET /system/version, it names
// no path and no secret, and holding it grants nothing. A client that
// makes up a matching one is a client with an address and credentials for
// an engine already, which is the whole of what it needed anyway.

// deploymentIDSuffix names the file, next to the journal and next to the
// three lock files startup.go describes, that carries this deployment's
// identity.
//
// A suffix on the journal path rather than an entry in the state
// directory, so a deployment whose journal is not in the place the
// packaging puts it still gets exactly one identity, and two journals in
// one directory (which nothing ships, but nothing forbids either) get one
// each.
const deploymentIDSuffix = ".deployment-id"

// deploymentIDBytes is how much randomness is minted. 128 bits, spelled as
// 32 hex characters, which is enough that two deployments colliding is not
// a case anybody has to reason about, and short enough to fit in a refusal
// an operator reads on one line beside a second one.
const deploymentIDBytes = 16

// DeploymentIdentity reports the identity of the deployment whose journal
// is dbPath, or the empty string when that deployment has not been given
// one.
//
// It reads and never mints, which is the whole difference between it and
// ensureDeploymentIdentity below. The caller is a command that is about to
// hand a change to somebody else's process, and a process that is not
// serving a deployment has no business naming it: an identity invented
// here would be compared against the engine's and would refuse for a
// reason this command created.
//
// An empty answer is not an error. It means the journal has never been
// opened by a build that mints one, which a caller has to be able to tell
// apart from "the identity is X" without unwrapping an error, because the
// two lead to different sentences.
func DeploymentIdentity(dbPath string) (string, error) {
	if dbPath == "" {
		return "", nil
	}
	raw, err := os.ReadFile(dbPath + deploymentIDSuffix)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("service: reading this deployment's identity: %w", err)
	}
	return validDeploymentIdentity(string(raw)), nil
}

// ensureDeploymentIdentity returns dbPath's deployment identity, minting
// one if that journal has never had one.
//
// It is called from runStartupSequence while the startup lock is held
// exclusively, and that is what makes the read-then-write below safe
// rather than a race: every process that opens a journal goes through
// that sequence, and no two of them are inside it at once. The lock is
// also why a deployment cannot end up with two identities, which would be
// worse than having none.
//
// A file that exists and holds something this build does not recognise is
// replaced rather than refused. The only ways to get one are a crash
// between create and write, or somebody editing it by hand, and refusing
// to start a deployment over a file that carries no information is a
// bigger outage than re-minting a name nothing has stored a copy of.
func ensureDeploymentIdentity(dbPath string) (string, error) {
	existing, err := DeploymentIdentity(dbPath)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}

	buf := make([]byte, deploymentIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("service: minting this deployment's identity: %w", err)
	}
	id := hex.EncodeToString(buf)

	// Written to a temporary name and renamed into place, so a reader
	// that arrives mid-write finds either the old state (no file) or the
	// whole identity, and never a half-written one. The temporary lives
	// in the same directory because a rename across filesystems is not
	// atomic and, on some, not possible at all.
	dir := filepath.Dir(dbPath)
	tmp, err := os.CreateTemp(dir, filepath.Base(dbPath)+deploymentIDSuffix+".*")
	if err != nil {
		return "", fmt.Errorf("service: minting this deployment's identity: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(id + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("service: minting this deployment's identity: %w", err)
	}
	// Flushed before the rename, not after: a rename that lands ahead of
	// the bytes leaves a deployment whose identity file is present and
	// empty, which is the one state this function treats as "never had
	// one" and would re-mint on the next start, quietly changing the
	// deployment's name after a power cut.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("service: minting this deployment's identity: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("service: minting this deployment's identity: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("service: minting this deployment's identity: %w", err)
	}
	if err := os.Rename(tmpPath, dbPath+deploymentIDSuffix); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("service: minting this deployment's identity: %w", err)
	}
	return id, nil
}

// validDeploymentIdentity is what this build accepts as an identity: the
// exact shape it mints, and nothing else.
//
// Anything else reads as absent rather than as an error, so a truncated or
// hand-edited file is re-minted by the next start instead of stopping a
// deployment from coming up. Being strict about the shape is what makes
// that safe: a partial write cannot be mistaken for a shorter identity.
func validDeploymentIdentity(raw string) string {
	id := strings.TrimSpace(raw)
	if len(id) != hex.EncodedLen(deploymentIDBytes) {
		return ""
	}
	if _, err := hex.DecodeString(id); err != nil {
		return ""
	}
	return id
}
