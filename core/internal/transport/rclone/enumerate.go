package rclone

import (
	"context"
	"fmt"

	"github.com/backupdproject/backupd/core/internal/backend"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// This file is the dispatch half of issue #792: which enumerator a source
// gets, and the refusal when the honest answer is "none".
//
// The decision is not this adapter's to invent. It is read off the
// capability matrix in core/internal/backend (bundled/*.json's
// "capabilities" block, and Manifest.PlanEnumeration, which owns the
// outcome), and this file is what connects that declaration to the code
// that would do the listing. Two outcomes:
//
//  1. The backend declares bounded_listing: enumerate in chunks. Today
//     that is local, through transport.LocalEnumerator, which is
//     *os.File.ReadDir(n) and holds one chunk per level of the tree.
//  2. The backend does not: refuse, before dialing anything. This is the
//     fail-closed case #792 asks for, and "before dialing" is the
//     load-bearing part: a refusal that arrived after the connection
//     would also arrive after the directory had been read into memory,
//     which is the failure it is supposed to prevent.
//
// There used to be a third outcome between those two - walk it anyway,
// and refuse a directory holding more entries than an operator
// configured - and its removal is the point worth recording. len() needs
// the slice. On a backend with no cursor the whole directory is
// materialised by the layer underneath before any code here can count
// it, so that ceiling refused directories AFTER paying for them: a
// number that read like a memory bound in a config file and was an
// epitaph in production. The capability, checked first, is the only
// gate that can fire in time.
//
// Why sftp is case 2: rclone's sftp backend reads a directory through
// github.com/pkg/sftp's ReadDir, which returns the whole directory as one
// slice and no resumable cursor, and fs.Fs.List itself returns
// fs.DirEntries. There is no partial read to reach for at any level of
// that stack, so a "streaming" sftp enumerator would be a slice with a
// callback wrapped around it - all of the cost, plus a claim that it had
// been solved. docs/adr/0008 states that as the finding it is.

// Enumerate streams one source's artifacts to yield instead of returning
// them, when the source's backend can be enumerated safely, and refuses
// by name when it cannot.
func (a *Adapter) Enumerate(ctx context.Context, src transport.Source, opts transport.EnumerateOptions, yield func(transport.RemoteArtifact) error) error {
	m, err := manifestForRcloneBackend(src.Type)
	if err != nil {
		return transport.NewError(transport.Configuration, "enumerate", err)
	}
	plan, err := m.PlanEnumeration()
	if err != nil {
		return transport.NewError(transport.Configuration, "enumerate", err)
	}
	if !plan.Bounded {
		// Unreachable while PlanEnumeration returns a plan only for a
		// bounded backend, and checked anyway: this is the branch that
		// must never become "well, walk it then".
		return transport.NewError(transport.UnsupportedCapability, "enumerate", fmt.Errorf(
			"%w: %q", backend.ErrUnboundedListing, m.ID))
	}
	if src.Type != "local" {
		// A backend whose matrix claims bounded_listing and that this
		// build has no chunked reader for. It cannot happen with the
		// three shipped manifests (s3 is a destination, not a source),
		// and it is a refusal rather than a silent fallback to a whole
		// directory read, because falling back would turn a declaration
		// this engine could not honour into the exact peak the
		// declaration promised to avoid.
		return transport.NewError(transport.UnsupportedCapability, "enumerate", fmt.Errorf(
			"backend %q declares bounded listing and this build has no chunked reader for the %q transport",
			m.ID, src.Type))
	}
	return transport.LocalEnumerator{}.Enumerate(ctx, src, opts, yield)
}

var _ transport.Enumerator = (*Adapter)(nil)

// manifestForRcloneBackend finds the bundled manifest describing the
// rclone backend a Source is dialed through.
//
// The lookup and both of its refusals belong to the registry
// (backend.Registry.ByRcloneBackend), because this file is not the only
// caller that has to get from "sftp" to a capability matrix: the backup
// source adapter asks the same question about the same three backends,
// and two copies of a fail-closed lookup is one copy that will stop
// failing closed.
func manifestForRcloneBackend(name string) (backend.Manifest, error) {
	reg, err := backend.Bundled()
	if err != nil {
		return backend.Manifest{}, fmt.Errorf("reading the bundled backend registry: %w", err)
	}

	return reg.ByRcloneBackend(name)
}
