package recovery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This file is the read side of the sidecar manifests manifest.go writes:
// given the local directory of one backup set, hand back every recovery
// manifest sitting in it.
//
// It exists separately because catalog rebuild has a different appetite
// for failure from every other reader here. ReadManifest is called by code
// that wants one named artifact and is entitled to fail when that one
// artifact's sidecar is broken. A rebuild runs when the journal is already
// gone, which is the worst possible moment to let one bad file decide that
// none of the others count, so this walks the directory and splits the
// outcome in two: what it could read, and what it could not, with the
// second half carrying enough detail for an operator to go and look.

// ScanError reports one sidecar manifest ScanManifests could not use.
//
// It names the file as well as the failure, which is the whole reason it
// exists rather than a bare error: a scan reports a batch, and an operator
// looking at a rebuild that recovered eleven artifacts out of twelve needs
// to be told which path to go and open.
type ScanError struct {
	// Path is the sidecar file, not the artifact and not the directory
	// that was scanned. It is what an operator opens to see what went
	// wrong, so it is spelled out in full rather than left to be
	// reassembled from the directory and the artifact name.
	Path string

	// Err is why the file was unusable: an os error from reading it, or a
	// parse or validation refusal from DecodeManifest.
	Err error
}

// Error renders as "path: reason", so a slice of these prints as a usable
// list.
//
// There is deliberately no Unwrap here, unlike CandidateError and
// ArtifactError elsewhere in this tree. Nothing routes on the underlying
// error's identity: a scan reports what it could not read and leaves the
// decision to a human, because the only caller is a rebuild that has
// already lost its journal and has no automatic recovery left to choose
// between.
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
