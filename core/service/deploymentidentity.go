package service

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
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
// lock has no business doing. A file is one bounded read.
//
// # Who mints, and why only them
//
// One process mints: the one that has just announced it is about to serve
// this deployment (AnnounceServing, liveengine.go). Every other process
// reads, and gets "" when there is nothing to read.
//
// That split is structural rather than a convention, and it has to be.
// The mint used to sit in runStartupSequence, which every `backup-manager`
// subcommand goes through, so a `backup-manager status` on a deployment
// whose identity file was missing (restore only the .db, which the
// restore section below names as supported) MINTED one. On a host where
// the engine was still up holding the old identity, that one read command
// renamed the deployment, and every routed write afterwards refused
// against its own engine while telling the operator to go and check
// $BACKUP_MANAGER_API_URL. deploymentcheck.go promises the near side is
// never minted by the command doing the comparing; putting the mint
// behind the serving announcement is what makes that promise something
// the code shape holds rather than something a caller remembers.
//
// It also makes "this deployment has no identity" a state that exists:
// a deployment nothing has served on this build reports none, the routed
// write refuses instead of comparing two nothings, and the remedy is the
// restart that mints one.
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
// An empty answer is not an error. It means no process serving this
// deployment has minted one yet, which a caller has to be able to tell
// apart from "the identity is X" without unwrapping an error, because the
// two lead to different sentences.
//
// Anything at that path that is not an ordinary file reads as empty
// rather than as an error, and that covers two things at once. A symlink
// is not followed: nothing this product ships puts one there, the reader
// may be running as a different user from the one that owns the state
// directory, and following a link is how "read the identity beside the
// journal" turns into "read whatever somebody pointed this at". A
// DIRECTORY at that path is the shape that used to stop a deployment
// starting for good, because every read of it failed forever; it is a
// deployment that cannot name itself, which is a state this whole file is
// built to carry.
//
// The read is bounded. An identity is 33 bytes and this build will not
// look at more than maxDeploymentIDFileBytes of whatever is there, so a
// path that has been pointed at something enormous costs a short read
// rather than that file's size in memory.
func DeploymentIdentity(dbPath string) (string, error) {
	if dbPath == "" {
		return "", nil
	}
	path := dbPath + deploymentIDSuffix
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("service: reading this deployment's identity: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("service: reading this deployment's identity: %w", err)
	}
	defer func() { _ = f.Close() }()
	// One byte more than anything valid, so a longer file is recognised as
	// junk rather than silently read as its first 32 hex characters.
	raw, err := io.ReadAll(io.LimitReader(f, maxDeploymentIDFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("service: reading this deployment's identity: %w", err)
	}
	if len(raw) > maxDeploymentIDFileBytes {
		return "", nil
	}
	return validDeploymentIdentity(string(raw)), nil
}

// ensureDeploymentIdentity returns dbPath's deployment identity, minting
// one if no process serving that journal ever has.
//
// It is called from AnnounceServing, and from nowhere else, while that
// function holds the serving lock EXCLUSIVELY. That is what makes the
// read-then-write below safe rather than a race: the serving lock admits
// one process at a time, nothing that is not about to serve gets here at
// all, and a deployment with two identities would be worse than one with
// none.
//
// A file that exists and holds something this build does not recognise is
// replaced rather than refused. The only ways to get one are a crash
// between create and write, or somebody editing it by hand, and refusing
// to start a deployment over a file that carries no information is a
// bigger outage than re-minting a name nothing has stored a copy of.
func ensureDeploymentIdentity(dbPath string) (string, error) {
	if dbPath == "" {
		return "", nil
	}
	sweepMintLeftovers(dbPath)

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

	// snapshot.go's writeFileAtomically, rather than a second copy of it
	// here. It writes to a temporary beside the target, syncs the bytes
	// before the rename (a rename that lands ahead of them leaves an
	// identity file that is present and empty, which this function reads
	// as "never had one" and would re-mint, quietly renaming the
	// deployment after a power cut), and then syncs the DIRECTORY, which
	// is the half a hand-rolled copy of this dropped: fsyncing the file
	// promises its content survives a crash and never that the directory
	// entry pointing at it does.
	if err := writeFileAtomically(dbPath+deploymentIDSuffix, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("service: minting this deployment's identity: %w", err)
	}
	return id, nil
}

// maxDeploymentIDFileBytes is the most of an identity file this build
// will look at: 32 hex characters, the newline the mint writes, and room
// for whitespace somebody's editor added. Anything longer is not an
// identity this build wrote, so it reads as absent.
const maxDeploymentIDFileBytes = 64

// sweepMintLeftovers removes the temporaries an interrupted mint leaves
// beside the journal.
//
// writeFileAtomically removes its own on every failure it can see, but a
// process killed between creating one and renaming it into place sees
// nothing, and the file it left is named for a temporary nobody will ever
// come back to. Without this they accumulate one per hard kill, forever,
// in the state directory an operator is meant to be able to look at.
//
// The directory is walked by prefix rather than matched with a glob,
// because a state database path is operator-supplied and a `*` or a `[`
// in it would make a pattern mean something else entirely. The prefix
// carries the journal's own name, so two journals sharing a directory
// sweep only their own.
//
// Failures are ignored on purpose. This runs on the way to serving a
// deployment, and a leftover temporary that could not be removed is not a
// reason to refuse to come up.
func sweepMintLeftovers(dbPath string) {
	dir := filepath.Dir(dbPath)
	prefix := filepath.Base(dbPath) + deploymentIDSuffix + "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
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
