// Package contract is the reusable transport contract suite (FR-3, "Transport
// Contract Tests" in docs/EPIC.md). It exercises any transport.Transport
// implementation against a Fixtures factory that knows how to stand up and
// mutate a concrete backend, so a future transport (an rclone upgrade, or a
// non-rclone implementation entirely) runs the identical assertions without
// anyone rewriting lifecycle tests.
//
// This package also carries Capture and Changed, which the
// changed-object-detection case below exercises for FR-16: persist identity
// at discovery, recheck it immediately before deletion, and refuse the
// deletion if the object no longer matches with sufficient confidence. The
// actual comparison logic lives in internal/model (RemoteIdentity,
// CompareIdentity), which every other package should call directly, since
// model depends on nothing and lifecycle/state/discovery can all reach it
// without depending on contract. Changed below is a thin adapter over
// model.CompareIdentity, kept only so this file's existing (bool, bool)
// signature keeps compiling for the suite in contract.go. It is deliberately
// not a second, independent implementation of the comparison: two answers to
// "has this object changed" that can disagree with each other is a worse
// hazard than either answer being wrong on its own, given that this is the
// check that decides whether a delete proceeds.
package contract

import (
	"context"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/transport"
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

// toModel maps this contract-local Identity onto model.RemoteIdentity, the
// type model.CompareIdentity actually operates on. HasHash gates whether the
// hash fields carry across: Capture only ever sets Artifact.Hash/HashAlg
// alongside HasHash together, but a value built by hand (as several table
// tests here do) might not, so this does not just trust that the two always
// travel as a pair.
func (id Identity) toModel() model.RemoteIdentity {
	r := model.RemoteIdentity{
		Path:     id.Artifact.Path,
		Size:     id.Artifact.Size,
		ModTime:  id.Artifact.ModTime,
		StableID: id.Artifact.ID,
	}
	if id.HasHash {
		r.Hash = id.Artifact.Hash
		r.HashAlg = string(id.Artifact.HashAlg)
	}
	return r
}

// Capture builds an Identity for remotePath by combining Stat with a
// best-effort RemoteHash call using alg. A backend that cannot produce alg for
// this object simply leaves HasHash false rather than failing the capture:
// whether a hash-less identity is good enough is model.CompareIdentity's call
// to make, not this one's.
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
// reports whether that verdict rests on model.ConfidenceStrong evidence: a
// decisive hash agreement/disagreement, backend stable-identifier
// agreement/disagreement, or an outright mismatch on path, size, or
// modification time. When the comparison can only reach
// model.ConfidenceWeak or model.ConfidenceNone, confident is false
// regardless of the changed value returned, and FR-16 is explicit about
// what that means: preserve the remote object rather than guess, so callers
// should treat !confident as "refuse" regardless of changed.
//
// This used to compare attributes itself, and treated a bare size and
// modification time agreement (no hash on either side) as confident and
// unchanged. That was wrong: modification-time granularity (commonly one
// second on many filesystems, and on rclone backends that cannot do better)
// cannot see a same-second, same-size replacement, so calling that
// confident is a green light to delete a file this manager never actually
// verified. Changed now delegates to model.CompareIdentity, which reaches
// that same case as ConfidenceWeak, not ConfidenceStrong.
func Changed(discovered, current Identity) (changed bool, confident bool) {
	cmp := model.CompareIdentity(discovered.toModel(), current.toModel())
	return cmp.Verdict == model.VerdictChanged, cmp.Confidence == model.ConfidenceStrong
}
