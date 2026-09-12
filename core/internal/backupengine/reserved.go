package backupengine

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/backupdproject/backupd/core/internal/model"
)

// This file is the one rule that keeps a local repository and the
// artifacts it protects from being mistaken for each other.
//
// # The failure being prevented
//
// A backup root is a populated, exported, walked directory. Artifacts
// arrive in it, a catalog records them, retention decides which ones stop
// being protected, and a prune deletes those. A content-addressed
// repository placed in that same directory is a few thousand files with
// names like "p0a1b2c3..." and no sidecar manifest, and every one of the
// mechanisms above is a candidate for treating them as something: an
// unrecognised artifact to adopt, an orphan to quarantine, a file with no
// placement to delete. The most likely outcome is not an error. It is a
// prune that removes pack files, and a repository that cannot restore
// anything, discovered later.
//
// # The rule
//
// Repository bytes live under one path, derived from the repository's own
// stable id, and it is reserved: artifact management may not enter it, and
// nothing in it is ever an artifact. Reserving it costs one predicate,
// LocalPathIsReserved, and buys a question with a decidable answer --
// "is this path repository internals?" -- in place of a naming convention
// everybody has to remember.
//
// The dot prefix is not the mechanism, it is politeness towards the
// operator browsing the share over SMB. The mechanism is the predicate,
// and TestReservedNamespaceIsInvisibleToArtifactManagement is what keeps
// every byte the adapter writes on the right side of it.

// reservedDirName is the directory, directly under a backup root, holding
// everything this manager keeps there that is not an artifact.
//
// It is a single name rather than a pattern because an operator has to be
// able to exclude one path from a share, a scanner or a backup of the
// backup, and "exclude .backupd" is instruction they can follow.
const reservedDirName = ".backupd"

// engineDirName separates repository storage from anything else this
// manager might later keep under the reserved directory. A path with the
// engine's own segment in it is unambiguously repository internals, which
// matters when the question is asked about a path rather than about a
// directory tree that is still there to inspect.
const engineDirName = "repositories"

// stateDirName holds this manager's own local state about the
// repositories under a backup root: connection config, index caches and
// maintenance-ownership records.
//
// It is a sibling of the repository storage rather than a directory
// inside it, and that separation is load-bearing rather than tidy: the
// vendor's filesystem storage lists the directory it is given and treats
// what it finds as blobs, so a config file or a cache directory placed
// among the blobs would be a foreign object in a repository's own
// namespace.
const stateDirName = "state"

// ReservedLocalStateDir returns where this manager keeps local state
// about the repositories under one backup root.
//
// It is inside the reserved namespace, so LocalPathIsReserved covers it
// too: a cache directory in a backup root is exactly as wrong to prune as
// a pack file is.
//
// A backup root is not the right place for this in production -- it
// belongs under /var/lib/backupd, which is why RepositoryLocation.StateDir
// exists and is honoured when set. This is the fallback for the case
// where a caller has a backup root and nothing else, and it is reserved so
// that the fallback is safe rather than merely convenient.
func ReservedLocalStateDir(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("backupengine: a local state directory needs a backup root")
	}

	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("backupengine: backup root %q is relative; a reserved namespace under a relative root is reserved only while the working directory holds still", root)
	}

	return filepath.Join(filepath.Clean(root), reservedDirName, stateDirName), nil
}

// ReservedLocalDir returns the one directory a local repository's bytes
// live in: <root>/.backupd/repositories/<domain>.
//
// It is the only function that composes this path. Every caller that needs
// it -- create, open, health, and the test that proves artifact management
// cannot see it -- goes through here, because two spellings of a reserved
// namespace is the same as not having one.
//
// The domain id is not cleaned or escaped on its way in, and it does not
// need to be: model.NewRepositoryDomainID already refuses an id that is
// empty, that carries a path separator of either slash, that is "." or
// "..", or that holds a control character, precisely so that it can name a
// directory here. What this function does refuse is a root that is not
// absolute, because a relative backup root turns a reserved namespace into
// a reserved namespace relative to whatever the working directory happens
// to be.
func ReservedLocalDir(root string, domain model.RepositoryDomainID) (string, error) {
	if domain.IsZero() {
		return "", fmt.Errorf("backupengine: a local repository needs a repository domain id; without one its storage has no stable name")
	}

	if root == "" {
		return "", fmt.Errorf("backupengine: a local repository needs a backup root")
	}

	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("backupengine: backup root %q is relative; a reserved namespace under a relative root is reserved only while the working directory holds still", root)
	}

	return filepath.Join(filepath.Clean(root), reservedDirName, engineDirName, domain.String()), nil
}

// LocalPathIsReserved reports whether path is inside the namespace
// repository internals live in, and is therefore invisible to artifact
// management.
//
// This is the predicate catalog, discovery, retention and prune consult,
// and it is phrased about the whole reserved directory rather than about
// one repository's subdirectory on purpose: a caller asking "may I touch
// this file" must get "no" for a repository that is not the one it knows
// about, for a repository that has since been removed, and for whatever
// else this manager later keeps beside them.
//
// Both paths are cleaned and compared as paths, never as strings, so
// "<root>/.backupdata" is not inside "<root>/.backupd" and
// "<root>/./.backupd/x" is.
func LocalPathIsReserved(root, path string) bool {
	if root == "" || path == "" {
		return false
	}

	reserved := filepath.Join(filepath.Clean(root), reservedDirName)

	rel, err := filepath.Rel(reserved, filepath.Clean(path))
	if err != nil {
		return false
	}

	// Rel answers with a path that climbs out when path is elsewhere, and
	// with "." when it IS the reserved directory. Both are inside the
	// answer this predicate owes: the directory itself is as untouchable
	// as its contents.
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
