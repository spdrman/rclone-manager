// Package contract is the reusable transport contract suite (FR-3, "Transport
// Contract Tests" in docs/EPIC.md). It exercises any transport.Transport
// implementation against a Fixtures factory that knows how to stand up and
// mutate a concrete backend, so a future transport (an rclone upgrade, or a
// non-rclone implementation entirely) runs the identical assertions without
// anyone rewriting lifecycle tests.
//
// This package also carries the reference remote-identity comparison that
// FR-16 describes: persist identity at discovery, recheck it immediately
// before deletion, and refuse the deletion if the object no longer matches.
// The lifecycle/state packages that will own this for real do not exist yet,
// so it lives here for now, next to the tests that prove it actually catches
// a replacement. A future FR-16 implementation should behave identically to
// Changed below; promoting this logic verbatim into state/lifecycle once
// those packages exist is a reasonable path.
package contract

import (
	"context"

	"github.com/spdrman/rclone-manager/internal/transport"
)

// Identity is the remote identity the manager would persist at discovery and
// recheck immediately before deletion. It layers a hash on top of
// transport.RemoteArtifact because size and modification time alone cannot
// distinguish two different files of the same size written in the same
// second, and FR-16 requires comparing "the strongest practical available
// attributes".
type Identity struct {
	Artifact transport.RemoteArtifact
	HasHash  bool
}

// Capture builds an Identity for remotePath by combining Stat with a
// best-effort RemoteHash call using alg. A backend that cannot produce alg for
// this object simply leaves HasHash false rather than failing the capture:
// whether a hash-less identity is good enough is Changed's call to make, not
// this one's.
func Capture(ctx context.Context, tr transport.Transport, source transport.Source, remotePath string, alg transport.HashAlgorithm) (Identity, error) {
	art, err := tr.Stat(ctx, source, remotePath)
	if err != nil {
		return Identity{}, err
	}

	id := Identity{Artifact: art}
	if h, hashErr := tr.RemoteHash(ctx, source, remotePath, alg); hashErr == nil && h != "" {
		id.Artifact.Hash = h
		id.Artifact.HashAlg = alg
		id.HasHash = true
	}
	return id, nil
}

// Changed reports whether current no longer corresponds to discovered, i.e.
// whether an FR-15 pre-delete recheck must refuse the deletion. confident
// reports whether that verdict rests on an unambiguous attribute (path, size,
// modification time or hash); when neither side carries a hash and the
// modification time cannot be compared either, Changed cannot rule out a
// same-second, same-size replacement, and FR-16 is explicit about what that
// means: preserve the remote object rather than guess, so callers should
// treat !confident as "refuse" regardless of the changed value returned.
func Changed(discovered, current Identity) (changed bool, confident bool) {
	if discovered.Artifact.Path != current.Artifact.Path {
		return true, true
	}
	if discovered.HasHash && current.HasHash {
		return discovered.Artifact.Hash != current.Artifact.Hash, true
	}
	if discovered.Artifact.Size != current.Artifact.Size {
		return true, true
	}
	if discovered.Artifact.ModTime != 0 && current.Artifact.ModTime != 0 {
		return discovered.Artifact.ModTime != current.Artifact.ModTime, true
	}
	// Same path, same size, no hash on at least one side, and no usable
	// modification time to fall back on: a same-second, same-size content
	// replacement is indistinguishable from no change at all here.
	return false, false
}
