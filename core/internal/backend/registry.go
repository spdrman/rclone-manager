package backend

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
)

//go:embed bundled/*.json
var bundledFS embed.FS

// ProbeStepNames is mediumcheck's step vocabulary, in mediumcheck's own
// order. It is a second copy of a fact this package may not import (see
// doc.go), pinned in both directions by
// TestTheProbeStepVocabularyMatchesMediumcheckSteps in
// core/internal/mediumcheck's external test package.
var ProbeStepNames = []string{
	"credentials", "reach", "deliverable", "write",
	"read_back", "storage_class", "verification", "delete",
}

// SupportedRcloneBackends is the closed set a manifest's rclone_backend
// may name. See doc.go: this is the FR-4 floor, and it is pinned - as a
// SUBSET of what the binary actually registers, not as an equal set - by
// TestEveryBundledManifestNamesABackendThisBinaryRegisters in
// core/internal/transport/rclone's external test package.
//
// sftp is #731's addition, and it is the review #665 section 4.2 asked
// for rather than a widening of the dependency surface: the backend was
// already registered and already dialed, for a backup SOURCE, so the
// entry below buys a DESTINATION for no blank import and no binary-size
// delta (see doc.go, "What is genuinely weakened").
var SupportedRcloneBackends = map[string]bool{"local": true, "s3": true, "sftp": true}

// ReservedInstanceID is config.MediumLocal, duplicated because this
// package may not import config (see doc.go), and pinned to it by
// TestTheReservedInstanceIdMatchesConfigsOwn in core/internal/config's
// external test package.
const ReservedInstanceID = "local"

// Registry is every manifest this build knows, keyed by id.
type Registry struct {
	byID map[string]Manifest
	ids  []string // sorted
}

// bundledOnce memoises Bundled(). One answer per process, not a speed
// optimisation: the parse is of two small embedded files, and
// memoisation exists so a hot config reload cannot re-parse and, in
// principle, disagree with an earlier answer inside the same run.
var bundledOnce = sync.OnceValues(func() (*Registry, error) {
	// //go:embed bundled/*.json embeds paths rooted at this package's own
	// directory, so bundledFS's tree has "bundled" as its first path
	// segment. fs.Sub re-roots it there, so Load's own glob ("*.json" at
	// the fs.FS root) is the same pattern whether it is handed this or a
	// test's fstest.MapFS with no such prefix at all.
	sub, err := fs.Sub(bundledFS, "bundled")
	if err != nil {
		return nil, fmt.Errorf("backend: opening the embedded bundled/ directory: %w", err)
	}
	return Load(sub)
})

// Bundled returns the registry built from the manifests this build
// ships, embedded at build time. It is the only production entry point
// into this package: see doc.go, "Bundled only".
func Bundled() (*Registry, error) {
	return bundledOnce()
}

// Load parses every *.json file an fs.FS holds into a Registry.
//
// Exported ONLY so a test can plant a malformed manifest in an
// fstest.MapFS without shipping one in bundled/ - see doc.go. A
// production caller passing anything but the embed.FS Bundled() already
// wraps is a review failure: this package imports no filesystem package
// itself (TestThisPackageCannotOpenAFile), so nothing here can construct
// a second fs.FS a caller could reach for without that becoming the
// change under review.
//
// Files are read in sorted name order, and on ANY problem across ANY
// file this returns (nil, err) rather than the well-formed manifests
// with a warning about the bad one - see doc.go, "A malformed bundled
// manifest fails the whole registry", which also states why that answer
// is scoped to bundled manifests and may differ for a future upload
// path.
func Load(fsys fs.FS) (*Registry, error) {
	names, err := fs.Glob(fsys, "*.json")
	if err != nil {
		return nil, fmt.Errorf("backend: listing manifests: %w", err)
	}
	sort.Strings(names)

	var problems []error
	byID := make(map[string]Manifest, len(names))
	var ids []string
	for _, name := range names {
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			problems = append(problems, manifestErrorf(name, "could not be read: %v", err))
			continue
		}
		var m Manifest
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&m); err != nil {
			problems = append(problems, manifestErrorf(name, "does not parse: %v", err))
			continue
		}
		problems = append(problems, validateManifest(name, m)...)
		if _, dup := byID[m.ID]; dup {
			problems = append(problems, manifestErrorf(name, "declares id %q, which another manifest in this registry already declares", m.ID))
			continue
		}
		byID[m.ID] = m
		ids = append(ids, m.ID)
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%w:\n%w", ErrMalformedManifest, errors.Join(problems...))
	}
	sort.Strings(ids)
	return &Registry{byID: byID, ids: ids}, nil
}

// manifestErrorf builds one problem, named by the file it came from and
// the rule it broke, in that order - every message in this package
// follows that shape so an operator (or, before an operator ever sees
// one, a reviewer) can find the file first and the rule second.
func manifestErrorf(file, format string, args ...any) manifestError {
	return manifestError(file + ": " + fmt.Sprintf(format, args...))
}

// Backend returns the manifest declared under id, or ErrUnknownBackend
// naming every id that IS declared.
func (r *Registry) Backend(id string) (Manifest, error) {
	if m, ok := r.byID[id]; ok {
		return m, nil
	}
	return Manifest{}, fmt.Errorf("%w: %q (declared: %s)", ErrUnknownBackend, id, strings.Join(r.ids, ", "))
}

// IDs returns every backend id this registry declares, sorted.
func (r *Registry) IDs() []string {
	return append([]string(nil), r.ids...)
}

// ByRcloneBackend finds the manifest describing an rclone backend by the
// name rclone knows it under ("local", "sftp", "s3"), because the
// capability matrix is declared per backend manifest while everything on
// the transport side names a backend the way rclone does.
//
// Both refusals are deliberate and neither is a fallback.
//
// A name no manifest describes is ErrUnqualifiedBackend, not a zero
// Manifest: this build knows what it knows, and "probably behaves like
// the others" is the assumption the capability matrix exists to delete.
//
// Two manifests naming one rclone backend is also a refusal rather than a
// first match. It cannot happen with the three bundled today, and the
// alternative is a capability answer that depends on map ordering, which
// is the worst possible way to be wrong about whether a directory can be
// listed in bounded memory.
func (r *Registry) ByRcloneBackend(name string) (Manifest, error) {
	var found []Manifest
	for _, id := range r.ids {
		if m := r.byID[id]; m.RcloneBackend == name {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return Manifest{}, fmt.Errorf(
			"%w: no bundled backend describes the %q transport, so nothing declares what it can be asked to do",
			ErrUnqualifiedBackend, name)
	default:
		ids := make([]string, len(found))
		for i, m := range found {
			ids[i] = m.ID
		}
		return Manifest{}, fmt.Errorf(
			"%w: %d bundled backends describe the %q transport (%v), and which one's capabilities apply is not a question that may be answered by picking one",
			ErrUnqualifiedBackend, len(found), name, ids)
	}
}

// Len returns how many backends this registry declares.
func (r *Registry) Len() int {
	return len(r.ids)
}
