package rclone

import (
	"context"
	"errors"
	"fmt"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/walk"

	"github.com/backupdproject/backupd/core/internal/backend"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// This file is the dispatch half of issue #792: which enumerator a source
// gets, and the refusal when the honest answer is "none".
//
// The decision is not this adapter's to invent. It is read off the
// capability matrix in core/internal/backend (bundled/*.json's
// "capabilities" block, and Manifest.PlanEnumeration, which owns the
// three-way outcome), and this file is what connects that declaration to
// the code that would do the listing. Three outcomes, in the order they
// are reached:
//
//  1. The backend declares bounded_listing: enumerate in chunks. Today
//     that is local, through transport.LocalEnumerator, which is
//     *os.File.ReadDir(n) and holds one chunk and one directory handle.
//  2. The backend does not, and the caller states a ceiling: walk it the
//     old way, one directory at a time, and refuse a directory bigger
//     than the ceiling. The peak is still one directory - that is what
//     "not bounded" means, and no code above rclone can change it - but
//     it is a number an operator chose and the failure is an error
//     somebody can act on rather than an OOM kill.
//  3. The backend does not, and no ceiling is stated: refuse, before
//     dialing anything. This is the fail-closed case #792 asks for, and
//     "before dialing" is the load-bearing part: a refusal that arrived
//     after the connection would also arrive after the directory had
//     been read into memory, which is the failure it is supposed to
//     prevent.
//
// Why sftp is case 2 and 3 rather than case 1: rclone's sftp backend
// reads a directory through github.com/pkg/sftp's ReadDir, which returns
// the whole directory as one slice and no resumable cursor, and fs.Fs.List
// itself returns fs.DirEntries. There is no partial read to reach for at
// any level of that stack, so a "streaming" sftp enumerator would be a
// slice with a callback wrapped around it - all of the cost, plus a claim
// that it had been solved. docs/adr/0008 states that as the finding it is.

// Enumerate streams one source's artifacts to yield instead of returning
// them, when the source's backend can be enumerated safely, and refuses
// by name when it cannot.
//
// opts.MaxDirectoryEntries is both an input to the decision (it is the
// ceiling backend.Manifest.PlanEnumeration wants for a backend that
// cannot stream) and a ceiling this adapter then enforces. A caller may
// state one for a streaming backend too; nothing needs it there, and
// refusing to honour it would be this adapter overriding a limit its
// caller chose.
func (a *Adapter) Enumerate(ctx context.Context, src transport.Source, opts transport.EnumerateOptions, yield func(transport.RemoteArtifact) error) error {
	m, err := manifestForRcloneBackend(src.Type)
	if err != nil {
		return transport.NewError(transport.Configuration, "enumerate", err)
	}
	plan, err := m.PlanEnumeration(opts.MaxDirectoryEntries)
	if err != nil {
		return transport.NewError(transport.Configuration, "enumerate", err)
	}

	if plan.Bounded {
		if src.Type != "local" {
			// A backend whose matrix claims bounded_listing and that
			// this build has no chunked reader for. It cannot happen
			// with the three shipped manifests (s3 is a destination, not
			// a source), and it is a refusal rather than a silent
			// fallback to the unbounded walk, because falling back would
			// turn a declaration this engine could not honour into the
			// exact peak the declaration promised to avoid.
			return transport.NewError(transport.UnsupportedCapability, "enumerate", fmt.Errorf(
				"backend %q declares bounded listing and this build has no chunked reader for the %q transport",
				m.ID, src.Type))
		}
		return transport.LocalEnumerator{}.Enumerate(ctx, src, opts, yield)
	}
	return a.enumerateWholeDirectories(ctx, src, opts, yield)
}

var _ transport.Enumerator = (*Adapter)(nil)

// enumerateWholeDirectories is case 2: rclone's own walk, with the
// entries of each directory handed to yield as they arrive and a ceiling
// refused per directory.
//
// What this buys over List, honestly: List holds every entry of every
// directory at once, plus the []RemoteArtifact built from all of them,
// plus a sort over that. This holds one directory's entries, converts
// them one at a time, and lets each go. For the FR-8 layout that prompted
// issue #737 - one directory per producer run - that is the difference
// between the whole tree and the biggest run. For one flat directory with
// a million entries in it, it is no difference at all, which is why the
// ceiling exists and why the capability matrix says sftp cannot do this
// safely.
//
// walk.Walk promises fn is never called concurrently (see its doc), so
// yield needs no lock of its own even though the listing underneath runs
// concurrently.
func (a *Adapter) enumerateWholeDirectories(ctx context.Context, src transport.Source, opts transport.EnumerateOptions, yield func(transport.RemoteArtifact) error) error {
	ctx, includeAll, err := excludeScope(oneConnectionAtATime(ctx), src)
	if err != nil {
		return WrapCtx(ctx, "enumerate", err)
	}
	f, err := a.fsFor(ctx, src)
	if err != nil {
		return WrapCtx(ctx, "enumerate", err)
	}
	defer shutdownFs(ctx, f)

	ceiling := opts.MaxDirectoryEntries
	var refusal error
	walkErr := walk.Walk(ctx, f, "", includeAll, -1, func(dir string, entries fs.DirEntries, err error) error {
		if err != nil {
			return err
		}
		if ceiling > 0 && len(entries) > ceiling {
			refusal = transport.NewError(transport.Configuration, "enumerate", fmt.Errorf(
				"%w: %q holds %d entries and the configured maximum is %d; backend %q cannot list a directory in bounded memory, so this engine refuses the directory rather than reading it whole",
				transport.ErrDirectoryTooLarge, displayRemoteDir(dir), len(entries), ceiling, src.Type))
			return refusal
		}
		for _, entry := range entries {
			obj, ok := entry.(fs.Object)
			if !ok {
				continue // a directory; the walk will visit it itself
			}
			if err := yield(toArtifact(obj)); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
	if refusal != nil {
		// walk.Walk may wrap what fn returned. The refusal is returned as
		// this adapter built it, so errors.Is against
		// transport.ErrDirectoryTooLarge and the Category both survive
		// whatever wrapping happened in between.
		return refusal
	}
	if walkErr != nil {
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			return WrapCtx(ctx, "enumerate", walkErr)
		}
		var classified *transport.Error
		if errors.As(walkErr, &classified) {
			return walkErr // already ours: a yield refusal, classified downstream
		}
		return WrapCtx(ctx, "enumerate", walkErr)
	}
	return nil
}

func displayRemoteDir(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

// manifestForRcloneBackend finds the bundled manifest describing the
// rclone backend a Source is dialed through, because the capability
// matrix is declared per backend manifest and a Source names a transport
// ("local", "sftp").
//
// A transport no manifest describes is a refusal, not a default. That is
// the same fail-closed rule the matrix itself follows: this build knows
// what it knows, and "probably behaves like the others" is the assumption
// this issue exists to delete.
//
// Two manifests naming one rclone backend is also a refusal rather than a
// first-match. It cannot happen today (each of local, s3 and sftp is
// named once) and the alternative would be a capability answer that
// depended on map ordering.
func manifestForRcloneBackend(name string) (backend.Manifest, error) {
	reg, err := backend.Bundled()
	if err != nil {
		return backend.Manifest{}, fmt.Errorf("reading the bundled backend registry: %w", err)
	}
	var found []backend.Manifest
	for _, id := range reg.IDs() {
		m, err := reg.Backend(id)
		if err != nil {
			return backend.Manifest{}, err
		}
		if m.RcloneBackend == name {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return backend.Manifest{}, fmt.Errorf(
			"%w: no bundled backend describes the %q transport, so nothing declares whether its directories can be listed safely",
			backend.ErrUnqualifiedBackend, name)
	default:
		ids := make([]string, len(found))
		for i, m := range found {
			ids[i] = m.ID
		}
		return backend.Manifest{}, fmt.Errorf(
			"%w: %d bundled backends describe the %q transport (%v), and which one's capabilities apply is not a question this adapter may answer by picking one",
			backend.ErrUnqualifiedBackend, len(found), name, ids)
	}
}
