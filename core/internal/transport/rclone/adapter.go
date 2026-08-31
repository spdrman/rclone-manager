// Package rclone is the only package in this repository that imports rclone.
//
// Exactly two backends are registered. Importing all of them for convenience
// would cost binary size, dependency surface, initialization complexity and
// accidental configuration exposure, so a third backend is an architecture
// decision rather than an import line (FR-4).
package rclone

import (
	"context"
	"errors"
	"fmt"
	"sort"

	// local and sftp are the two backends FR-4 requires. Importing them,
	// together with fs/operations below, also registers crypt transitively.
	// See backends.go for the traced cause, why it's accepted rather than
	// removed, and the test that keeps this exact set enforced.
	_ "github.com/rclone/rclone/backend/local"
	_ "github.com/rclone/rclone/backend/sftp"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/walk"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Adapter implements transport.Transport over embedded rclone packages.
type Adapter struct{}

// New returns an adapter. It takes no rclone types, by design.
func New() *Adapter { return &Adapter{} }

var _ transport.Transport = (*Adapter)(nil)

// ErrUnsupportedHash is returned by RemoteHash when the requested algorithm
// is one this adapter does not know how to translate to an rclone hash.Type
// at all, or one the backend behind src cannot compute for this object.
//
// It exists so errors.go's Classify can recognize this case by identity
// (errors.Is) instead of matching this same package's own error strings a
// second time. Matching a dependency's wording is unavoidable in a couple of
// documented spots in errors.go because no typed value exists to reach for
// instead; matching a string adapter.go itself produces never had that
// excuse, since a sentinel defined right here is exactly as cheap and does
// not silently drift if one file's wording changes without the other's.
var ErrUnsupportedHash = errors.New("rclone: unsupported hash")

// fsFor builds an rclone Fs for a source without touching any on-disk rclone
// config file. Everything comes from the manager's own configuration, so there
// is no ambient rclone state to leak in.
//
// sftp options are built by sftpConfig in ssh.go, which owns the SSH
// authentication and host-key verification posture required by FR-6.
func (a *Adapter) fsFor(ctx context.Context, src transport.Source) (fs.Fs, error) {
	info, err := fs.Find(src.Type)
	if err != nil {
		return nil, fmt.Errorf("backend %q is not registered in this binary: %w", src.Type, err)
	}

	cfg := configmap.Simple{}
	if src.Type == "sftp" {
		sftpCfg, err := sftpConfig(src)
		if err != nil {
			return nil, err
		}
		cfg = sftpCfg
	}

	f, err := info.NewFs(ctx, src.ID, src.Root, cfg)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", src.ID, err)
	}
	return f, nil
}

func toArtifact(o fs.Object) transport.RemoteArtifact {
	return transport.RemoteArtifact{
		Path:    o.Remote(),
		Size:    o.Size(),
		ModTime: o.ModTime(context.Background()).Unix(),
	}
}

// List recurses the whole tree beneath src's root, not just the top
// directory.
//
// It used to call f.List(ctx, ""), which lists exactly one directory level.
// A producer that writes one directory per run, for example
// gitea-runs/<RUN_ID>/*.dump, is a normal FR-8 layout, not an edge case, and
// under the old call every artifact placed in a subdirectory went missing
// from List with no error at all: Stat and CopyToLocal could still reach it
// by path, so the gap was invisible until something went looking for a
// backup that discovery had silently never seen. That is exactly the
// protection-dies-quietly failure this project exists to prevent, so it is
// not something a caller can be trusted to notice and route around: List
// itself has to stop lying about what exists.
//
// The fix is unconditional, full recursion rather than a configurable depth
// knob, because there is nothing here for a depth setting to mean: FR-5's
// per-backup-set include patterns (config.BackupSet.Include) are validated
// as filename patterns, never path patterns (see
// internal/config/validate.go's rejection of any "/" in an include
// pattern), so they already only ever match a candidate's basename
// regardless of how deep it sits. A "recurse N levels" option would filter
// something the configuration has no way to describe in the first place.
// Unconditional recursion is therefore the reading that is actually
// consistent with the configuration surface that exists today, not an
// implicit behaviour bolted on beside it. internal/discovery is where a
// basename that turns up at two different depths (and therefore collides as
// one model.ArtifactID) gets handled explicitly, loudly, and by name.
//
// walk.GetAll with maxLevel -1 is rclone's own idiom for this (see its
// fstest package): it uses the backend's native recursive listing when one
// is available and falls back to walking directory by directory otherwise,
// so this works identically against local and sftp. includeAll=true is
// passed explicitly because this adapter never configures an rclone filter
// for a caller to accidentally rely on; making that explicit here is
// cheaper than leaving the answer to whatever ambient default happens to be
// in effect.
func (a *Adapter) List(ctx context.Context, src transport.Source) ([]transport.RemoteArtifact, error) {
	f, err := a.fsFor(ctx, src)
	if err != nil {
		return nil, Wrap("list", err)
	}
	objs, _, err := walk.GetAll(ctx, f, "", true, -1)
	if err != nil {
		return nil, Wrap("list", err)
	}
	out := make([]transport.RemoteArtifact, 0, len(objs))
	for _, o := range objs {
		out = append(out, toArtifact(o))
	}

	// Sort by remote path so listing is deterministic.
	//
	// walk.GetAll returns whatever order the backend produced, which for the
	// local backend is directory-read order and is not stable between runs.
	// That does not matter while every artifact is independent, but it stops
	// being harmless the moment two remote paths share a basename, because
	// model.ArtifactID identifies an artifact by basename alone. Two run
	// directories that both contain backup.dump collide as one identity, and
	// with an unordered listing the winner is whichever the backend happened
	// to yield first. That makes ingestion a coin flip: one cycle takes
	// run-1's copy and reports run-2 as a conflict, the next cycle does the
	// reverse, and neither is reliably backed up.
	//
	// Sorting does not fix the collision, it makes the outcome repeatable,
	// which is the difference between a conflict an operator can reason about
	// and a nondeterministic one they cannot. The real fix is for an artifact
	// identity to carry more than a basename, which is a change to
	// model.ArtifactID and to everything keyed on it.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Stat resolves one object and, unlike List, asks the backend for the
// strongest identity attributes it can supply.
//
// WHY STAT HASHES AND LIST DOES NOT. Stat is the pre-delete recheck path.
// FR-16 compares the identity captured at discovery against the identity
// observed now, and model.CompareIdentity can only reach ConfidenceStrong on
// a hash or a backend-supplied stable id. Without one of those it returns
// Unconfirmed at Weak confidence, Preserve() is true, and FR-15 refuses the
// delete. Stat returning only path, size and mtime therefore did not merely
// weaken the check, it made a successful delete unreachable on every backend,
// including ones that hash perfectly well.
//
// List deliberately does not do this. It runs over every object under a
// source root, and on sftp rclone computes a hash by running a command on the
// server, so hashing during a listing would be one round trip per artifact
// for an attribute discovery does not need yet.
//
// A backend that cannot hash still returns an empty hash here, which is the
// correct outcome rather than a failure: a hardened shell-less sftp account
// genuinely cannot answer, so the comparison stays Weak and the delete stays
// refused, which is what FR-16 asks for.
func (a *Adapter) Stat(ctx context.Context, src transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	f, err := a.fsFor(ctx, src)
	if err != nil {
		return transport.RemoteArtifact{}, Wrap("stat", err)
	}
	o, err := f.NewObject(ctx, remotePath)
	if err != nil {
		return transport.RemoteArtifact{}, Wrap("stat", err)
	}

	art := toArtifact(o)

	if f.Hashes().Contains(hash.SHA256) {
		// A hash failure is not fatal here. The artifact is still usable with
		// weaker identity, and reporting "I could not hash" as "stat failed"
		// would turn a degraded check into an outage.
		if h, herr := o.Hash(ctx, hash.SHA256); herr == nil && h != "" {
			art.Hash = h
			art.HashAlg = transport.SHA256
		}
	}
	if ider, ok := o.(fs.IDer); ok {
		art.ID = ider.ID()
	}
	return art, nil
}

func (a *Adapter) CopyToLocal(ctx context.Context, src transport.Source, remotePath, localPartialPath string) (transport.TransferResult, error) {
	srcFs, err := a.fsFor(ctx, src)
	if err != nil {
		return transport.TransferResult{}, Wrap("copy_to_local", err)
	}
	o, err := srcFs.NewObject(ctx, remotePath)
	if err != nil {
		return transport.TransferResult{}, Wrap("copy_to_local", err)
	}
	dstDir, dstName := splitPath(localPartialPath)
	dstFs, err := fs.NewFs(ctx, dstDir)
	if err != nil {
		return transport.TransferResult{}, Wrap("copy_to_local", err)
	}
	// Copy, never Move. The remote source is deleted later, by the lifecycle
	// manager, and only after a durable commit (FR-11, FR-15).
	//
	// copyWithProgress is a wrapper, not a second code path: with no
	// transport.ProgressReporter on ctx (the ordinary case, and every
	// caller before issue #221) it calls the closure and nothing else
	// happens. o.Size() is the only total that can honestly be reported
	// for this copy, and it is read here, from the object the copy is
	// actually about, rather than guessed downstream.
	var dst fs.Object
	if err := copyWithProgress(ctx, o.Size(), func(ctx context.Context) error {
		var copyErr error
		dst, copyErr = operations.Copy(ctx, dstFs, nil, dstName, o)
		return copyErr
	}); err != nil {
		return transport.TransferResult{}, Wrap("copy_to_local", err)
	}
	return transport.TransferResult{BytesTransferred: dst.Size()}, nil
}

func (a *Adapter) RemoteHash(ctx context.Context, src transport.Source, remotePath string, alg transport.HashAlgorithm) (string, error) {
	f, err := a.fsFor(ctx, src)
	if err != nil {
		return "", Wrap("remote_hash", err)
	}
	o, err := f.NewObject(ctx, remotePath)
	if err != nil {
		return "", Wrap("remote_hash", err)
	}
	var ht hash.Type
	switch alg {
	case transport.SHA256:
		ht = hash.SHA256
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedHash, alg)
	}
	// An unsupported remote hash must surface as an explicit capability result,
	// never as a silent downgrade of configured verification (FR-13).
	if !f.Hashes().Contains(ht) {
		return "", fmt.Errorf("%w: backend %q cannot compute %s", ErrUnsupportedHash, src.Type, alg)
	}
	return o.Hash(ctx, ht)
}

func (a *Adapter) DeleteRemote(ctx context.Context, src transport.Source, remotePath string) error {
	f, err := a.fsFor(ctx, src)
	if err != nil {
		return Wrap("delete_remote", err)
	}
	o, err := f.NewObject(ctx, remotePath)
	if err != nil {
		return Wrap("delete_remote", err)
	}
	return Wrap("delete_remote", o.Remove(ctx))
}

func splitPath(p string) (dir, name string) {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i], p[i+1:]
		}
	}
	return ".", p
}
