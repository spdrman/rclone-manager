package artifactstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// Local is the local backup root: the store every existing configuration
// uses, and the only implementation today.
//
// It owns the one formula for where a committed artifact sits under a
// backup set's configured local directory. That formula previously
// existed twice on purpose, in lifecycle's finalPath and in
// internal/retention's pruneFinalPath, each carrying a comment saying it
// and the other were "the only two places in the whole project allowed to
// compute it". Two guarded copies was the right answer while there was
// nowhere better to put it. There is now: a store is exactly the thing
// that knows where its own bytes go, so both delegate here and the
// formula exists once.
type Local struct{}

// compile-time proof that Local satisfies the seam.
var _ Store = Local{}

// Kind reports KindLocal.
func (Local) Kind() Kind { return KindLocal }

// LocalLocator is Locator's formula, available as a plain function for
// the two callers that need it before they have a Store value and cannot
// take one without a wider refactor than issue #334's seam justifies.
//
// artifact.Name is guaranteed a bare basename when the ArtifactID was
// built through model.NewArtifactID, which refuses "/", "\\", "." and
// "..". A record whose ArtifactID did not come through there can still
// carry a crafted name, which is exactly why internal/retention re-derives
// containment from the resolved path immediately before it deletes
// anything rather than trusting this join. See that package's
// pruneVerifySafeToDelete.
func LocalLocator(localDir string, artifact model.ArtifactID) string {
	return filepath.Join(localDir, artifact.Name)
}

// Locator returns the artifact's final path under the backup set's
// configured local directory.
func (Local) Locator(bs config.BackupSet, artifact model.ArtifactID) (string, error) {
	if bs.LocalPath == "" {
		return "", fmt.Errorf("artifactstore: backup set %q has no local_path configured", bs.ID)
	}
	return LocalLocator(bs.LocalPath, artifact), nil
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

// Put writes bytes to locator through a temporary file in the same
// directory, then renames it into place, so a reader never observes a
// half-written artifact under its final name.
//
// Nothing calls this yet. The local commit path still writes its own
// .partial and hard-links it, because that path carries FR-12 crash-safety
// obligations this method does not reproduce and must not be quietly
// swapped for. This exists so the seam is one a mover could be built on;
// see the package doc.
func (Local) Put(_ context.Context, locator string, r io.Reader) error {
	dir := filepath.Dir(locator)
	tmp, err := os.CreateTemp(dir, filepath.Base(locator)+".artifactstore-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
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
	return os.Rename(tmpName, locator)
}

// Remove deletes the file at locator.
//
// An already-absent file is not an error: the caller's intent, that these
// bytes not be in this store, is already true.
//
// This method is deliberately NOT where FR-20's six pre-delete checks
// live. Those checks are in internal/retention, which re-derives every one
// of them from the artifact's own journal record and the backup set's own
// configured root immediately before it calls anything that deletes. That
// placement is the point: see this package's doc on why safety proofs
// belong to the implementation's caller-side policy rather than to the
// seam, and see internal/retention/prune.go's own package doc for why it
// re-checks rather than trusting upstream filtering.
func (Local) Remove(_ context.Context, locator string) error {
	err := os.Remove(locator)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
