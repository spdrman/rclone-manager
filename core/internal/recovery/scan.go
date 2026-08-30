package recovery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ScanError reports one sidecar manifest ScanManifests could not use.
type ScanError struct {
	Path string
	Err  error
}

func (e ScanError) Error() string { return fmt.Sprintf("%s: %v", e.Path, e.Err) }

// ScanManifests reads every sidecar manifest directly inside localDir.
// It never recurses: internal/lifecycle/transfer.go's finalPath already
// places every artifact for one backup set directly under its configured
// LocalPath, with no subdirectories, so a manifest never lives anywhere
// else.
//
// A localDir that does not exist at all is not an error: an operator
// adopting a fresh backup root, or one this manager has never pointed a
// journal at before (EPIC-B section 71 Work Package 3.3's "recovery/
// adoption of existing backup root"), is expected to report zero
// manifests, exactly the same outcome as an existing-but-empty directory,
// not a failure.
//
// One unreadable or malformed manifest is collected into the returned
// errs slice rather than aborting the whole scan, mirroring
// internal/discovery.Result's own Errors bucket: a single corrupted
// sidecar file must never hide every other artifact's legitimate recovery
// metadata. The returned manifests are sorted by artifact name so the
// result never depends on directory-listing order.
func ScanManifests(localDir string) (manifests []Manifest, errs []ScanError) {
	entries, err := os.ReadDir(localDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []ScanError{{Path: localDir, Err: err}}
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), manifestSuffix) {
			continue
		}
		path := filepath.Join(localDir, entry.Name())
		m, err := ReadManifest(path)
		if err != nil {
			errs = append(errs, ScanError{Path: path, Err: err})
			continue
		}
		manifests = append(manifests, m)
	}

	sort.Slice(manifests, func(i, j int) bool { return manifests[i].ArtifactName < manifests[j].ArtifactName })
	return manifests, errs
}
