package artifactstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// Local is the local backup root: the store every existing configuration
// uses, and the only implementation today.
//
// It owns the formula for where a committed artifact sits under a backup
// set's configured local directory. That formula previously existed twice
// on purpose, in lifecycle's finalPath and in internal/retention's
// pruneFinalPath, each carrying a comment saying it and the other were
// "the only two places in the whole project allowed to compute it". Two
// guarded copies was the right answer while there was nowhere better to
// put it. There is now: a store is exactly the thing that knows where its
// own bytes go, so both ask a Local and cannot drift apart, and since
// #235 they ask it through Locator rather than through a free function
// taking a directory string.
//
// Those two, not every join in the project. retention's
// pruneVerifySafeToDelete finishes by joining the artifact's name onto the
// root filepath.EvalSymlinks handed back, which is a different
// computation on a different input and stays where it is: that function's
// whole job is producing the canonical, symlink-free path, and routing
// that join back through the configured root would undo the resolution it
// had just proved.
type Local struct {
	// root is one backup set's configured local_path, taken at
	// construction rather than passed to every call. See NewLocal.
	root string
}

// compile-time proof that Local satisfies the seam.
var _ Store = Local{}

// NewLocal returns the local store for one backup set, rooted at that
// set's configured local_path.
//
// This is the constructor convention a second backend follows: whatever a
// store needs to know about where its bytes go arrives here, once, and no
// method afterwards takes configuration. NewS3 would take a bucket and a
// prefix the same way.
//
// root is the raw local_path as it comes out of YAML, which is meaningful
// before config.Validate has run. Nothing else on config.BackupSet is
// passed in, deliberately: see the Store interface's Locator doc for why
// a store must not be handed a struct whose fields have two different
// lifecycles.
//
// An empty root is refused here rather than at the first Locator call,
// because filepath.Join("", name) is a path relative to the process
// working directory and a store that can be built in that state is a
// store that can write an artifact somewhere nobody is backing up.
func NewLocal(root string) (Local, error) {
	if root == "" {
		return Local{}, errors.New("artifactstore: the local store needs a backup set's local_path as its root")
	}
	return Local{root: root}, nil
}

// Kind reports KindLocal.
func (Local) Kind() Kind { return KindLocal }

// localLocator is Locator's formula, and nothing outside this package can
// reach it any more.
//
// It used to be exported, as LocalLocator, for the two callers that held a
// directory string rather than a Store: lifecycle's finalPath and
// retention's pruneFinalPath. Its localDir parameter is a filesystem path,
// which hard-codes exactly the assumption the Store interface exists to
// remove, and issue #334's own doc called that deliberate and temporary:
// keeping it was what let #334 land as a refactor with no behaviour in it,
// and converting those two call sites was named as the first backend's job.
//
// #235 is that backend, so the conversion is done and the escape hatch is
// gone. Both call sites now build a Local and ask it, and there is no
// longer any way for a caller to compute an artifact's location without
// going through a store. That is the whole point of the seam, and an
// exported free function that bypasses it is a standing invitation not to.
//
// artifact.Name is guaranteed a bare basename when the ArtifactID was
// built through model.NewArtifactID, which refuses "/", "\\", "." and
// "..". A record whose ArtifactID did not come through there can still
// carry a crafted name, which is exactly why internal/retention re-derives
// containment from the resolved path immediately before it deletes
// anything rather than trusting this join. See that package's
// pruneVerifySafeToDelete.
func localLocator(localDir string, artifact model.ArtifactID) string {
	return filepath.Join(localDir, artifact.Name)
}

// Locator returns the artifact's final path under this store's root.
//
// The zero Local is refused rather than silently joining onto "": a
// Local is meant to come from NewLocal, and the one way to get one that
// has not is a composite literal, which is a mistake worth naming.
func (l Local) Locator(artifact model.ArtifactID) (string, error) {
	if l.root == "" {
		return "", fmt.Errorf("artifactstore: this local store has no root, so it cannot say where %s belongs; build it with NewLocal", artifact)
	}
	return localLocator(l.root, artifact), nil
}

// Stat reports the file's size and modification time, or ErrNotPresent.
//
// Lstat, not Stat: a symlink at an artifact's final path is an anomaly
// outside every invariant this pipeline maintains, since Commit only ever
// produces a final name by hard-linking a .partial and then removing that
// name. Following one here would report on a file this store never placed.
func (Local) Stat(_ context.Context, locator string) (Stat, error) {
	fi, err := os.Lstat(locator)
	if errors.Is(err, os.ErrNotExist) {
		return Stat{}, ErrNotPresent
	}
	if err != nil {
		return Stat{}, err
	}
	if !fi.Mode().IsRegular() {
		return Stat{}, fmt.Errorf("artifactstore: %s is not a regular file (mode %s)", locator, fi.Mode())
	}
	size := fi.Size()
	mod := fi.ModTime()
	return Stat{Size: &size, ModTime: &mod}, nil
}

// Open reads the artifact's bytes.
func (Local) Open(_ context.Context, locator string) (io.ReadCloser, error) {
	f, err := os.Open(locator)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotPresent
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Put writes bytes through a temporary file in the same directory, hard
// links that into place without clobbering anything, drops the temporary
// name, and fsyncs the directory.
//
// That sequence is what discharges all three of the interface's
// obligations, and each step is load-bearing:
//
//   - The temp file plus link is the ATOMICITY half. A reader never sees
//     a partial object under the artifact's own name, so a mover's
//     confirming Stat cannot succeed against a truncated copy.
//   - Link rather than rename is the REFUSAL. os.Rename replaces
//     whatever is at the destination; os.Link fails with EEXIST, which
//     is reported as ErrAlreadyPresent. This is the only method here
//     that could destroy an artifact's bytes, and a rename over a live
//     one is not recoverable. lifecycle's linkWithoutClobbering makes
//     the same choice for FR-12's commit, for the same reason.
//   - The directory fsync is the DURABILITY half, and it is the step
//     people skip. A directory is a separate inode from the file it
//     names, with its own writeback state, so fsyncing the content says
//     nothing about whether the NAME survives a power loss. Skip it and
//     a crash between the origin's Remove reaching disk and this entry
//     reaching disk leaves zero copies, which is the one outcome this
//     package's doc promises is impossible. commit.go's FR-14 treatment
//     is the long version of the same argument.
//
// Nothing calls this yet. The local commit path still writes its own
// .partial and hard-links it, because that path carries FR-12 crash-safety
// obligations this method does not reproduce and must not be quietly
// swapped for. That is a comment on this end and a test on the other:
// TestLifecycleUsesOnlyTheSharedFormulaFromThisPackage fails if anything
// under internal/lifecycle reaches for this package for anything but the
// shared join.
func (Local) Put(_ context.Context, locator string, r io.Reader) error {
	dir := filepath.Dir(locator)

	// A fixed-length prefix, not one derived from the artifact's name. An
	// artifact name near the filesystem's NAME_MAX turns a name-derived
	// pattern into ENAMETOOLONG, which reports a put that was never
	// attempted as a put that failed. The leading dot keeps the temp file
	// out of a casual listing of a backup root; nothing reads it, and it
	// is gone on every path out of this function.
	tmp, err := os.CreateTemp(dir, ".artifactstore-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	linked := false
	defer func() {
		tmp.Close()
		if !linked {
			os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, r); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Link(tmpName, locator); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrAlreadyPresent, locator)
		}
		return err
	}
	linked = true

	// Drop the temporary name before the fsync, not after, so the one
	// directory fsync covers both the entry this created and the one it
	// removed. A failure here means the artifact is at locator and this
	// still reports an error, which is the safe direction: a mover that
	// does not get a success does not remove the origin, so the outcome
	// is two copies rather than a lost one.
	if err := os.Remove(tmpName); err != nil {
		return err
	}
	return fsyncDir(dir)
}

// Remove deletes the file at locator.
//
// An already-absent file is not an error: the caller's intent, that these
// bytes not be in this store, is already true.
//
// This method is deliberately NOT where FR-20's six pre-delete checks
// live, and it deliberately performs no safety proof of its own. Those
// checks are in internal/retention, which re-derives every one of them
// from the artifact's own journal record and the backup set's own
// configured root immediately before it calls anything that deletes. That
// placement is the point: see this package's doc on why safety proofs
// belong to the caller rather than to the seam, see the Store interface's
// own Remove doc for what a store IS obliged to do, and see
// internal/retention/prune.go's package doc for why it re-checks rather
// than trusting upstream filtering.
func (Local) Remove(_ context.Context, locator string) error {
	err := os.Remove(locator)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// fsyncDir fsyncs a directory: open it (POSIX only ever allows opening a
// directory read-only anyway) and Sync the resulting descriptor. See Put's
// own comment for why a rename or a link is not durable without this.
//
// This is a third copy of a function internal/lifecycle and core/service
// each already have, and it stays a copy on purpose. lifecycle imports
// this package now, so this package cannot import lifecycle back, and
// hoisting the one durability primitive out of commit.go's FR-14 path
// belongs in a change whose subject is that path, not in a refactor whose
// whole claim is that it changed no behaviour.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
