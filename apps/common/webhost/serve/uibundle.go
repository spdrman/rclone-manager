// Choosing which UI bundle to serve, at run time rather than at build
// time.
//
// The shared UI picks its provider shell when it is compiled, so before
// this existed, shipping Synology's bridge meant compiling a
// Synology-specific binary, and §3.7 requires every provider package to
// carry the same core binary digest. That left a choice between the wrong
// bridge and a forbidden build, and the wrong bridge is what shipped.
// Resolving the bundle at startup removes the choice: the binary is
// identical whichever bridge a package serves.
//
// Every failure to resolve is a refusal rather than a fallback, and that
// is the rule worth keeping. A --ui-dir that quietly fell back to the
// embedded bundle would serve a UI that looks entirely correct while
// running the wrong provider's bridge, which is the same defect one layer
// down from where it was first found.
//
// The origin is recorded and logged because a deployment needs to be able
// to see what it actually loaded rather than what it was configured to
// load. Those differ exactly when something is wrong, which is the only
// time anybody looks.
package serve

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// UIBundleOrigin records which of the three sources a served bundle came
// from. It exists so a deployment can log what it actually loaded rather
// than what it was configured to load, which is the difference between
// diagnosing a wrong bridge in a minute and in an afternoon.
type UIBundleOrigin string

const (
	// UIBundleEmbedded is the bundle compiled into the binary.
	UIBundleEmbedded UIBundleOrigin = "embedded"
	// UIBundleProfileRoot is <root>/<profile>, the runtime-profile-selected
	// bundle.
	UIBundleProfileRoot UIBundleOrigin = "profile-root"
	// UIBundleExplicitDir is an operator- or package-supplied directory.
	UIBundleExplicitDir UIBundleOrigin = "explicit-dir"
)

// ErrUIBundleUnusable is what every failed resolution wraps. Resolution
// fails closed on purpose: a --ui-dir that quietly fell back to the
// embedded bundle would serve a working-looking UI running the wrong
// provider bridge, which is precisely the defect issue #180 was filed
// about, just moved one layer down.
var ErrUIBundleUnusable = errors.New("no usable UI bundle")

// UIBundleSource is where a UI bundle may come from, in the order they
// are considered.
//
// # Why this exists (issue #180, owned by #167)
//
// Before this, the shared web host embedded exactly one bundle at compile
// time and offered no alternative. ui/shared/vite.config.ts picks the
// provider shell at BUILD time from VITE_PLATFORM, so shipping Synology's
// bridge meant compiling a Synology-specific binary, and section 3.7
// requires every provider package to carry the exact same core binary
// digest. The choice was between the wrong bridge and a forbidden build,
// and PR #173 correctly took the wrong bridge.
//
// This type removes the choice. The bundle is selected at run time, so
// the binary is identical whichever bridge a package serves.
// apps/generic/tests/uibundle proves that against a real built artifact:
// one binary, four bridges, one unchanged sha256.
type UIBundleSource struct {
	// Dir is an explicit directory, from --ui-dir. It wins outright: an
	// operator or a package that named a path meant that path.
	Dir string

	// Root is a directory of per-profile bundles, from --ui-root, and
	// Profile selects one subdirectory of it. Both are required together;
	// a root with no profile is a configuration error rather than a
	// prompt to guess.
	Root    string
	Profile string

	// Embedded is the compile-time bundle, used when neither disk source
	// is configured. Rooted at the bundle's own top level, so
	// Embedded's "index.html" IS the app shell.
	Embedded fs.FS
}

// UIBundle is a resolved bundle and the record of where it came from.
type UIBundle struct {
	FS     fs.FS
	Origin UIBundleOrigin
	// Detail is the human-readable source, for a startup log line.
	Detail string
}

// ResolveUIBundle applies the precedence Dir, then Root/Profile, then
// Embedded, and refuses rather than falling through a configured source
// that turned out to be unusable.
func ResolveUIBundle(src UIBundleSource) (UIBundle, error) {
	switch {
	case src.Dir != "":
		fsys, err := usableDir(src.Dir)
		if err != nil {
			return UIBundle{}, fmt.Errorf("%w: --ui-dir %s: %w", ErrUIBundleUnusable, src.Dir, err)
		}
		return UIBundle{FS: fsys, Origin: UIBundleExplicitDir, Detail: src.Dir}, nil

	case src.Root != "":
		if src.Profile == "" {
			return UIBundle{}, fmt.Errorf("%w: --ui-root %s names a directory of per-profile bundles but no profile is selected, so there is nothing to choose", ErrUIBundleUnusable, src.Root)
		}
		clean := path.Clean("/" + filepath.ToSlash(src.Profile))
		if clean != "/"+src.Profile || strings.Contains(src.Profile, "/") {
			return UIBundle{}, fmt.Errorf("%w: profile %q is not a single directory name, so it cannot select a bundle under --ui-root %s (a %q would escape it)", ErrUIBundleUnusable, src.Profile, src.Root, "..")
		}
		dir := filepath.Join(src.Root, src.Profile)
		fsys, err := usableDir(dir)
		if err != nil {
			return UIBundle{}, fmt.Errorf("%w: --ui-root %s has no usable bundle for profile %q at %s: %w", ErrUIBundleUnusable, src.Root, src.Profile, dir, err)
		}
		return UIBundle{FS: fsys, Origin: UIBundleProfileRoot, Detail: dir}, nil

	case src.Embedded != nil:
		if _, err := fs.Stat(src.Embedded, "index.html"); err != nil {
			return UIBundle{}, fmt.Errorf("%w: the embedded bundle has no index.html: %w", ErrUIBundleUnusable, err)
		}
		return UIBundle{FS: src.Embedded, Origin: UIBundleEmbedded, Detail: "compiled into the binary"}, nil

	default:
		return UIBundle{}, fmt.Errorf("%w: no --ui-dir, no --ui-root, and no embedded bundle", ErrUIBundleUnusable)
	}
}

// usableDir is the one definition of "this directory is a UI bundle":
// it exists, it is a directory, and it has an app shell in it. An empty
// directory is the failure mode this catches: a bind mount that did not
// mount produces exactly that, and serving it would answer every route
// with 404 instead of saying what went wrong.
func usableDir(dir string) (fs.FS, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("not a directory")
	}
	fsys := os.DirFS(dir)
	if _, err := fs.Stat(fsys, "index.html"); err != nil {
		return nil, fmt.Errorf("no index.html in it: %w", err)
	}
	return fsys, nil
}
