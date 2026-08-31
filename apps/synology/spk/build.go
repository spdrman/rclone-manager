package spk

import "errors"

// errNotImplemented marks the RED stage of this work package: the
// behavioural contract and its conformance tests exist, the behaviour
// does not yet.
var errNotImplemented = errors.New("apps/synology/spk: not implemented yet")

// BuildOptions is everything needed to wrap one architecture's release
// binaries in a `.spk`.
type BuildOptions struct {
	// GOARCH selects both the DSM arch family written into INFO and the
	// release-manifest entry the result must later verify against.
	GOARCH string

	// Version is INFO's `version`.
	Version string

	// BinariesDir holds the ALREADY BUILT release binaries, one file per
	// CoreBinaries entry. Nothing here compiles anything: §3.7 requires
	// the SPK to carry the exact release digest, so a rebuild would be
	// the one thing this package must never do.
	BinariesDir string

	// OutDir is where the `.spk` is written.
	OutDir string
}

// Build assembles the package and returns the path it wrote.
func Build(_ BuildOptions) (string, error) { return "", errNotImplemented }

// sha256File hashes one file, hex-encoded, the same way
// scripts/release/record-release-hashes.sh does.
func sha256File(_ string) (string, error) { return "", errNotImplemented }
