package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// This file is issue #792: enumerating a source directory without holding
// the directory in memory.
//
// # What was wrong with List
//
// Transport.List returns []RemoteArtifact, so every entry of every
// directory under a source's root is live at once, and on the way there
// so is rclone's own fs.Objects slice for the same set. Cost is linear in
// entry count with a ceiling nowhere, and the measurements in
// docs/adr/0008-bounded-source-enumeration-and-capability-matrix.md put
// the peak at roughly 300 MiB for one flat directory of 1,000,000
// entries. A daemon whose job is to still be running tomorrow does not
// get to allocate that because a producer wrote a lot of files, and the
// failure mode is the worst kind: invisible until the day it is an OOM
// kill in the middle of a backup.
//
// Nothing about that is rclone's fault and it is not fixable in the
// adapter. fs.Fs.List's signature IS a slice - List(ctx, dir)
// (fs.DirEntries, error) - so a backend has no way to hand back a cursor
// even when the protocol has one, and FR-3 says the answer to an upstream
// API shape is not a fork. See the ADR's option A for the version of this
// that starts by patching rclone, and why it was not taken.
//
// # What this is instead
//
// A callback enumerator over a chunked directory reader. Entries are read
// in groups of ChunkEntries, converted, handed to the caller, and
// dropped; the peak is the chunk, and a caller can start work on the
// first entry before the walk has finished (the time-to-first-entry
// column in the ADR: microseconds here against tens of seconds for
// List).
//
// The local reader is *os.File.ReadDir(n), which takes a count and is the
// stdlib primitive the whole of this file exists to reach. It is
// deliberately NOT routed through rclone: there is no rclone call that
// reads part of a directory, so going through the adapter would mean
// materialising the slice this file exists to avoid. That is why a
// stdlib-only enumerator lives in this package - the boundary that owns
// what a Source IS - rather than in transport/rclone, which owns dialing
// something over a protocol.
//
// # What it does not do
//
// It makes no global ordering promise. List sorts its answer, which it
// can afford because it has the whole answer; a stream cannot sort what
// it has not seen without buffering it, which is the cost being removed
// here. Per-directory ordering is likewise not free (a flat directory of
// a million entries is exactly where sorting one directory is the same
// problem again), and a consumer that needs sorted directory entries -
// Kopia's snapshot uploader does - has to do its own bounded merge. The
// ADR says so in "What it costs, honestly", because it is the one thing a
// caller could be surprised by.

// DefaultChunkEntries is how many directory entries one read asks for
// when a caller has not said. It is the bound: at ~200 bytes per entry
// between the fs.DirEntry and the RemoteArtifact built from it, 4096
// entries is well under a megabyte, and it is large enough that the
// syscall count is not what a listing costs.
const DefaultChunkEntries = 4096

// ErrDirectoryTooLarge is the refusal a configured ceiling produces. It
// is the fail-closed half of #792: for a backend whose directories cannot
// be read in chunks at all, this engine refuses a directory bigger than
// the operator said to expect rather than reading it and finding out what
// that costs.
//
// It is a Configuration failure (never Transient): a retry re-reads the
// same too-large directory, and the two things that change the outcome
// are a bigger configured ceiling or a smaller directory. Both are a
// person's decision.
var ErrDirectoryTooLarge = errors.New("directory holds more entries than the configured maximum")

// Enumerator streams a source's artifacts to a callback instead of
// returning them.
//
// It is a separate interface from Transport, not a method on it, and that
// is on purpose. Transport is implemented by fakes and decorators across
// this repository (core/tests/classifytransport, crashmatrix's
// timedKillTransport, reconcile's own fake); a method added there is a
// method every one of them has to grow whether or not it has an answer.
// A caller that needs streaming asks for this and gets a compile error
// from an implementation that does not have it, which is the honest
// shape: not every transport can stream.
type Enumerator interface {
	// Enumerate calls yield once per artifact beneath src.Root, in no
	// promised order, and stops at the first error from yield or from the
	// walk. A cancelled context stops it within one chunk.
	Enumerate(ctx context.Context, src Source, opts EnumerateOptions, yield func(RemoteArtifact) error) error
}

// EnumerateOptions is what bounds one enumeration.
type EnumerateOptions struct {
	// ChunkEntries is how many entries are read, converted and delivered
	// at a time. Zero means DefaultChunkEntries: zero is what every
	// caller that has not thought about this passes, and it must not mean
	// "read the whole directory".
	ChunkEntries int

	// MaxDirectoryEntries refuses a single directory holding more than
	// this many entries, with ErrDirectoryTooLarge. Zero means no
	// ceiling.
	//
	// Zero is the right answer for a backend that streams, and a ceiling
	// there would be an arbitrary refusal of a directory this engine can
	// in fact walk in bounded memory. It is NOT the right answer for a
	// backend whose listing is one slice: see
	// backend.Manifest.PlanEnumeration, which is where the capability
	// matrix turns into this number, and which refuses outright rather
	// than defaulting when an unbounded backend has no ceiling
	// configured.
	MaxDirectoryEntries int
}

func (o EnumerateOptions) chunk() int {
	if o.ChunkEntries <= 0 {
		return DefaultChunkEntries
	}
	return o.ChunkEntries
}

// DirReader reads one directory in bounded groups. Next returns io.EOF
// when the directory is exhausted, and every reader is Closed even when
// the walk stops early.
type DirReader interface {
	Next(n int) ([]fs.DirEntry, error)
	Close() error
}

// DirOpener opens one directory for chunked reading. dir is an absolute
// path on the local filesystem for the os-backed opener.
type DirOpener func(ctx context.Context, dir string) (DirReader, error)

// LocalEnumerator is the bounded enumerator for a local source: one
// directory handle open at a time, one chunk of entries live at a time,
// no goroutines of its own.
//
// No goroutines is worth stating rather than leaving to be noticed. The
// reason List fans out is rclone's walk, which runs --checkers goroutines
// over directories a backend cannot list recursively (see
// oneConnectionAtATime in transport/rclone, which pins that at one for
// connection reasons). A depth-first chunked walk has nothing to
// parallelise across and therefore nothing to leak, which is why
// cancellation here is a context check and not a shutdown protocol.
type LocalEnumerator struct {
	// OpenDir is how a directory is opened, and nil - the ordinary value
	// - means the operating system.
	//
	// It is exported ONLY so a test can enumerate a directory of a
	// million entries without creating a million files (see
	// enumerate_test.go, and Registry.Load in core/internal/backend for
	// the same arrangement and the same reasoning). A production caller
	// setting this is a review failure.
	OpenDir DirOpener
}

var _ Enumerator = LocalEnumerator{}

// Enumerate walks src.Root depth first, delivering one chunk of entries
// at a time.
//
// Depth first, with an explicit stack, for the memory reason this whole
// file is about: a breadth-first walk holds every directory of a level
// before it descends, and the deployments this product runs against have
// one directory per producer run. Depth first holds one open handle and a
// stack of pending directory PATHS, so the resident cost is the chunk
// plus the shape of the tree, never the number of files in it.
//
// Symbolic links are skipped, which is what bundled/local_volume.json's
// capability matrix declares (symlink_semantics: "skip") and what
// rclone's local backend does by default. An entry that vanishes between
// being listed and being stat'ed is also skipped: a backup source is a
// live directory somebody else is writing to, and a file deleted while
// this walk was in it is not an error this process should fail a backup
// over. How far that can be trusted at all is #793's question
// (source-consistency modes); what belongs here is not pretending a race
// is a malfunction.
func (e LocalEnumerator) Enumerate(ctx context.Context, src Source, opts EnumerateOptions, yield func(RemoteArtifact) error) error {
	open := e.OpenDir
	if open == nil {
		open = openOSDir
	}
	excluded := excludedDirs(src.ExcludePaths)

	// The stack holds directories relative to src.Root ("" is the root
	// itself), so a pending entry costs a path and not a handle.
	stack := []string{""}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return classifyEnumerate(err)
		}
		rel := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		children, err := e.readDirectory(ctx, open, src.Root, rel, excluded, opts, yield)
		if err != nil {
			return err
		}
		stack = append(stack, children...)
	}
	return nil
}

// readDirectory reads one directory to its end, yields its objects and
// returns the subdirectories to descend into.
func (e LocalEnumerator) readDirectory(
	ctx context.Context,
	open DirOpener,
	root, rel string,
	excluded map[string]bool,
	opts EnumerateOptions,
	yield func(RemoteArtifact) error,
) ([]string, error) {
	dir := root
	if rel != "" {
		dir = filepath.Join(root, filepath.FromSlash(rel))
	}
	reader, err := open(ctx, dir)
	if err != nil {
		return nil, classifyEnumerate(err)
	}
	defer reader.Close() //nolint:errcheck // a read-only directory handle has nothing to report on close

	var children []string
	seen := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, classifyEnumerate(err)
		}
		entries, err := reader.Next(opts.chunk())
		if errors.Is(err, io.EOF) {
			return children, nil
		}
		if err != nil {
			return nil, classifyEnumerate(err)
		}
		seen += len(entries)
		if max := opts.MaxDirectoryEntries; max > 0 && seen > max {
			return nil, NewError(Configuration, "enumerate", fmt.Errorf(
				"%w: %q holds more than %d entries, and this backend cannot list a directory in bounded memory",
				ErrDirectoryTooLarge, displayDir(rel), max))
		}
		for _, entry := range entries {
			switch {
			case entry.IsDir():
				child := path.Join(rel, entry.Name())
				if !excluded[child] {
					children = append(children, child)
				}
			case entry.Type().IsRegular():
				artifact, ok, err := toLocalArtifact(rel, entry)
				if err != nil {
					return nil, classifyEnumerate(err)
				}
				if !ok {
					continue
				}
				if err := yield(artifact); err != nil {
					return nil, err
				}
			}
			// Everything else - symlinks, sockets, devices - is not an
			// artifact this product copies, and is skipped rather than
			// reported. See Enumerate's doc.
		}
		if len(entries) == 0 {
			// A reader that returns no entries and no error would
			// otherwise spin forever. No correct DirReader does this;
			// this is the guard that says so out loud rather than the
			// hang that would be debugged from a stuck daemon.
			return children, nil
		}
	}
}

// toLocalArtifact builds the RemoteArtifact for one entry. ok is false
// when the entry disappeared between being listed and being stat'ed.
func toLocalArtifact(rel string, entry fs.DirEntry) (RemoteArtifact, bool, error) {
	info, err := entry.Info()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RemoteArtifact{}, false, nil
		}
		return RemoteArtifact{}, false, err
	}
	artifact := RemoteArtifact{
		Path: path.Join(rel, entry.Name()),
		Size: info.Size(),
	}
	if t := info.ModTime(); !t.IsZero() {
		// Unix seconds, matching what transport/rclone's toArtifact
		// produces for every other path into this type. The local
		// filesystem's own precision is finer (bundled/
		// local_volume.json declares 1ns) and this is where it is lost;
		// docs/adr/0008 records that as a known truncation rather than
		// leaving two paths into one field disagreeing about units.
		artifact.ModTime = t.Unix()
	}
	return artifact, true, nil
}

// excludedDirs turns Source.ExcludePaths into the set of directories the
// walk declines to descend into, keyed the way the walk names them
// (slash-separated, relative to the root).
func excludedDirs(paths []string) map[string]bool {
	if len(paths) == 0 {
		return nil
	}
	out := make(map[string]bool, len(paths))
	for _, p := range paths {
		clean := path.Clean(filepath.ToSlash(p))
		clean = trimSlashes(clean)
		if clean == "" || clean == "." {
			continue
		}
		out[clean] = true
	}
	return out
}

func trimSlashes(p string) string {
	for len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	for len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return p
}

func displayDir(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

// classifyEnumerate gives every failure out of this enumerator a
// manager-owned Category, so lifecycle code can branch on it the same way
// it branches on one from the rclone adapter (FR-22, and errors.go's
// "Category is not a label, it is a branch").
func classifyEnumerate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return NewError(Cancelled, "enumerate", err)
	case errors.Is(err, os.ErrNotExist):
		return NewError(NotFound, "enumerate", err)
	case errors.Is(err, os.ErrPermission):
		return NewError(PermissionDenied, "enumerate", err)
	default:
		return NewError(Permanent, "enumerate", err)
	}
}

// osDir is the chunked directory reader this enumerator exists to use:
// *os.File.ReadDir(n) is the one directory-listing primitive in reach
// that takes a count.
type osDir struct{ f *os.File }

func openOSDir(_ context.Context, dir string) (DirReader, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	return &osDir{f: f}, nil
}

func (d *osDir) Next(n int) ([]fs.DirEntry, error) { return d.f.ReadDir(n) }

func (d *osDir) Close() error { return d.f.Close() }
