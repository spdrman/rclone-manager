package spk

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
)

// Reading container/release-manifest.json, which is the only place the
// release binary digests exist.
//
// The type models the fields this package needs and deliberately ignores
// the OCI image id the manifest also records: an SPK contains no image, so
// an image id is not evidence about one, and modelling it would invite a
// later check that compares something meaningless.
//
// SourcePath is not a field of the file. It is filled in on load so a
// verification report can name the input its parity verdict was decided
// against, because a green line about an unnamed file is a green line
// nobody can act on when it turns red.

// ReleaseManifest is container/release-manifest.json, the file
// scripts/release/record-release-hashes.sh writes (issue #82/B4.1). §3.7
// requires a release manifest to record "core binary SHA-256 per
// architecture", and this is the only place those numbers exist.
//
// Only the fields this package needs are modelled. The manifest also
// carries local_image_id_sha256, which is deliberately ignored here: an
// SPK contains no OCI image, so an image ID says nothing about it.
type ReleaseManifest struct {
	Version       string      `json:"version"`
	Commit        string      `json:"commit"`
	Architectures []ArchEntry `json:"architectures"`

	// SourcePath is where this manifest was read from. It is not a
	// manifest field - LoadReleaseManifest fills it in - and it exists so
	// a Report can name the input its parity verdict was decided against
	// rather than printing a green line about a file nobody can identify.
	SourcePath string `json:"-"`
}

// ArchEntry is one architecture's recorded binary hashes.
type ArchEntry struct {
	Architecture string            `json:"architecture"`
	BinarySHA256 map[string]string `json:"binary_sha256"`
}

// LoadReleaseManifest reads and validates a release manifest.
//
// Validation is not politeness. A manifest that parses but records no
// usable hash would let Verify report "parity" against nothing, which is
// worse than no check at all, so every failure below is an error rather
// than a zero value.
func LoadReleaseManifest(path string) (ReleaseManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ReleaseManifest{}, fmt.Errorf("read release manifest: %w", err)
	}
	var m ReleaseManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return ReleaseManifest{}, fmt.Errorf("parse release manifest %s: %w", path, err)
	}
	if len(m.Architectures) == 0 {
		return ReleaseManifest{}, fmt.Errorf("release manifest %s records no architectures", path)
	}
	for _, entry := range m.Architectures {
		if len(entry.BinarySHA256) == 0 {
			return ReleaseManifest{}, fmt.Errorf("release manifest %s: architecture %q has an empty binary_sha256", path, entry.Architecture)
		}
		for _, name := range CoreBinaries {
			sum, ok := entry.BinarySHA256[name]
			if !ok {
				return ReleaseManifest{}, fmt.Errorf("release manifest %s: architecture %q records no binary_sha256 for %q", path, entry.Architecture, name)
			}
			if !isSHA256(sum) {
				return ReleaseManifest{}, fmt.Errorf("release manifest %s: architecture %q records %q for %q, which is not a SHA-256", path, entry.Architecture, sum, name)
			}
		}
	}
	m.SourcePath = path
	return m, nil
}

// Arch returns the entry for a GOARCH, or an error naming what the
// manifest does record. Never falls back to "the only entry" or "the
// first entry": matching the wrong architecture's hashes is exactly the
// mistake this whole file exists to make impossible.
func (m ReleaseManifest) Arch(goarch string) (ArchEntry, error) {
	have := make([]string, 0, len(m.Architectures))
	for _, entry := range m.Architectures {
		if entry.Architecture == goarch {
			return entry, nil
		}
		have = append(have, entry.Architecture)
	}
	slices.Sort(have)
	return ArchEntry{}, fmt.Errorf("the release manifest records no %s entry (it has %v), so there is nothing to check a %s package against", goarch, have, goarch)
}

// isSHA256 reports whether s is exactly 64 hex characters.
//
// It is a shape check and not a verification, and the distinction is why
// it exists at all: a truncated or placeholder digest in the manifest
// would otherwise be compared against a real one, fail parity, and be
// reported as "this package carries the wrong binary" when the actual
// fault is upstream in the manifest. Refusing it at load time names the
// right file.
func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
