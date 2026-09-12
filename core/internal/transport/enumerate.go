package transport

import (
	"context"
	"errors"
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
// Bounded has to hold on both axes, and only one of them is about
// entries. A tree whose directories each hold one file, a million times
// over, is the layout FR-8 produces, and a walk that collects child
// paths before descending is linear in it however small its chunks are.
// So the walk is over resumable directory FRAMES, one per level of the
// tree rather than one per directory in it: see Enumerate.
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

// There is no refusal sentinel in this file, on purpose. The refusal for
// a backend whose directories cannot be read in chunks belongs to the
// capability matrix (backend.ErrUnboundedListing), it fires before
// anything is dialed, and it is not an entry count: see
// backend.Manifest.PlanEnumeration for why a configured maximum
// directory size was removed rather than kept here as a second, weaker
// gate.

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

// EntryKind is what a directory entry IS, for the one consumer that may
// not find out by opening it.
//
// The set is small on purpose. A backup source adapter needs three
// answers - read it, do not read it and it is a link, do not read it and
// it is something else - and every finer distinction (block against
// character device, socket against fifo) is a distinction nothing in
// this repository branches on, so naming them here would be vocabulary
// with no consumer. What matters is that "something else" is SAID rather
// than inferred from the absence of a regular file: a source adapter
// that opened a fifo because it assumed the entry was a file would block
// in Read until the backup window ended.
type EntryKind string

const (
	// EntryKindUnknown is the zero value: nobody answered. It is not
	// "regular", and a consumer that needs to know refuses instead of
	// reading it as the optimistic answer.
	EntryKindUnknown EntryKind = ""

	// EntryKindRegular is a file with content.
	EntryKindRegular EntryKind = "regular"

	// EntryKindSymlink is a symbolic link, reported as itself. Nothing in
	// this package follows one.
	EntryKindSymlink EntryKind = "symlink"

	// EntryKindOther is a socket, a fifo, a device node, or anything else
	// a filesystem can name that is not one of the above. Grouped because
	// the answer for all of them is the same: there is no content here
	// this product copies.
	EntryKindOther EntryKind = "other"
)

// EnumerateOptions is what bounds one enumeration.
type EnumerateOptions struct {
	// ChunkEntries is how many entries are read, converted and delivered
	// at a time. Zero means DefaultChunkEntries: zero is what every
	// caller that has not thought about this passes, and it must not mean
	// "read the whole directory".
	//
	// It is the only bound there is, and that is deliberate. A maximum
	// directory size used to live here as well, for the backends that
	// cannot be read in chunks; it was removed because an entry count
	// cannot be checked before the entries exist, so on exactly the
	// backends it claimed to protect it refused after the allocation.
	// Those backends are now refused by the capability matrix instead,
	// before anything is opened (backend.Manifest.PlanEnumeration).
	ChunkEntries int

	// ReportNonRegular asks for symlinks, sockets, fifos and device nodes
	// to be yielded with their Kind set, instead of being passed over.
	//
	// It defaults to false because the callers that predate it are
	// copying artifacts and a socket is not one. It exists because the
	// caller that does not copy - the backup source adapter, which has a
	// symlink policy and a special-file policy to apply and a run report
	// to fill in - cannot apply a policy to entries it is never told
	// about. Silently dropping them makes "this backup contains your
	// source" true only for the parts of it that happened to be regular
	// files, and says so nowhere.
	//
	// A yielded non-regular entry carries the lstat's size and
	// modification time and nothing else. Nothing here opens one, and
	// nothing here resolves a link.
	ReportNonRegular bool
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

// LocalEnumerator is the bounded enumerator for a local source: one chunk
// of entries live per open directory, one open directory per level of the
// tree's depth, no goroutines of its own.
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
// # What "bounded" has to mean, and the shape that broke it
//
// Two things scale with a source: how many entries a directory holds, and
// how many directories the tree holds. A chunked read bounds the first.
// The first version of this walk kept a stack of pending directory PATHS
// - read a directory to its end, collect every child, descend - and that
// bounds nothing on the second: FR-8's layout is one directory per
// producer run, so a source with a million runs in it put a million paths
// on that stack, which is the allocation this file exists to refuse,
// arriving from the other direction.
//
// So a directory is a FRAME instead: its open handle, the chunk of
// entries last read from it, and how far through that chunk the walk has
// got. A subdirectory is descended into the moment it is seen, and the
// parent is left exactly where it was, because *os.File keeps the
// directory offset and ReadDir(n) resumes from it. What is retained is
// therefore one chunk per LEVEL of the tree - the shape of it - and never
// one entry per directory in it.
//
// The cost is one open file descriptor per level, which is the trade this
// makes knowingly: a descriptor is what makes a frame resumable at all,
// depth is bounded by the filesystem's own path limit (a path is at most
// PATH_MAX, so levels are at most a few hundred), and the Go runtime
// raises this process's descriptor limit to the hard maximum at startup.
// A million-directory tree is a million frames ONLY if it is also a
// million levels deep, which is not a directory tree, it is a path no
// operating system will open.
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
//
// Ordering is per-directory arrival order, interleaved across levels: a
// subtree's entries arrive in the middle of its parent's. Enumerate has
// never promised an order (see the file header), and ADR 0008 records the
// bounded-AND-ordered feed Kopia's uploader wants as Phase 1 work.
func (e LocalEnumerator) Enumerate(ctx context.Context, src Source, opts EnumerateOptions, yield func(RemoteArtifact) error) error {
	open := e.OpenDir
	if open == nil {
		open = openOSDir
	}
	excluded := excludedDirs(src.ExcludePaths)

	root, err := e.openFrame(ctx, open, src.Root, "")
	if err != nil {
		return err
	}
	stack := []*dirFrame{root}
	// Every frame still open when the walk stops - cancelled, refused by
	// the callback, or finished - is closed here. A walk that leaked one
	// descriptor per abandoned level would leak them per backup run.
	defer func() {
		for _, frame := range stack {
			frame.reader.Close() //nolint:errcheck // a read-only directory handle has nothing to report on close
		}
	}()

	for len(stack) > 0 {
		top := stack[len(stack)-1]

		if top.at == len(top.entries) {
			// The context check is per chunk rather than per entry:
			// ctx.Err() on every one of a million entries is a cost paid
			// for nothing, and one chunk is the latency this promises
			// (see the cancellation test).
			if err := ctx.Err(); err != nil {
				return classifyEnumerate(err)
			}
			entries, err := top.reader.Next(opts.chunk())
			switch {
			case errors.Is(err, io.EOF), err == nil && len(entries) == 0:
				// No entries and no error would otherwise spin forever.
				// No correct DirReader does that; this says so out loud
				// rather than becoming a hang debugged from a stuck
				// daemon.
				top.reader.Close() //nolint:errcheck // as above
				stack = stack[:len(stack)-1]
				continue
			case err != nil:
				return classifyEnumerate(err)
			}
			top.entries, top.at = entries, 0
		}

		entry := top.entries[top.at]
		top.at++
		switch {
		case entry.IsDir():
			child := path.Join(top.rel, entry.Name())
			if excluded[child] {
				continue
			}
			frame, err := e.openFrame(ctx, open, src.Root, child)
			if err != nil {
				return err
			}
			stack = append(stack, frame)
		case entry.Type().IsRegular(), opts.ReportNonRegular:
			// Everything that is not a directory reaches this arm when
			// the caller asked for it, and only regular files otherwise.
			// A non-regular entry is yielded with its Kind and never
			// opened: entry.Info() is the lstat the directory read
			// already performed, so a symlink is described as the link
			// and not as whatever it points at.
			artifact, ok, err := toLocalArtifact(top.rel, entry)
			if err != nil {
				return classifyEnumerate(err)
			}
			if !ok {
				continue
			}
			if err := yield(artifact); err != nil {
				return err
			}
		}
		// A non-regular entry that the caller did not ask to hear about
		// is skipped, which is what every caller predating
		// ReportNonRegular expects: a socket is not an artifact this
		// product copies.
	}
	return nil
}

// dirFrame is one directory the walk is part way through: the handle it
// is being read from, the chunk last read, and how much of that chunk has
// been dealt with. Its size is the bound - one chunk, whatever the
// directory holds - and its handle is what lets the walk descend out of
// the middle of it and come back.
type dirFrame struct {
	rel     string
	reader  DirReader
	entries []fs.DirEntry
	at      int
}

func (e LocalEnumerator) openFrame(ctx context.Context, open DirOpener, root, rel string) (*dirFrame, error) {
	dir := root
	if rel != "" {
		dir = filepath.Join(root, filepath.FromSlash(rel))
	}
	reader, err := open(ctx, dir)
	if err != nil {
		return nil, classifyEnumerate(err)
	}
	return &dirFrame{rel: rel, reader: reader}, nil
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
		Kind: entryKind(info.Mode()),
	}
	if t := info.ModTime(); !t.IsZero() {
		// Unix seconds, matching what transport/rclone's toArtifact
		// produces for every other path into this type. The local
		// filesystem's own precision is finer (bundled/
		// local_volume.json declares 1ns) and this is where it is lost;
		// docs/adr/0008 records that as a known truncation rather than
		// leaving two paths into one field disagreeing about units.
		//
		// Two things about this value a consumer has to know, because
		// both break silently. It is TRUNCATED, so it is not what the
		// disk said. And it is a SCAN-TIME capture: it is whatever the
		// directory read reported, never re-stat'ed, so a file mutated
		// after this walk passed it carries metadata it no longer has.
		// Anything deciding "this file is unchanged, skip its content"
		// from either property is deciding it from the wrong number -
		// see docs/adr/0009 section 6, which owns that rule and whose
		// own capture re-stats after the read for exactly this reason.
		artifact.ModTime = t.Unix()
	}
	return artifact, true, nil
}

// entryKind reads a mode as one of the three answers a source adapter
// branches on. It is a total function over fs.FileMode on purpose: a mode
// bit combination nobody anticipated lands in EntryKindOther, which is the
// answer that costs a skipped entry, rather than in EntryKindRegular,
// which is the answer that costs an open.
func entryKind(mode fs.FileMode) EntryKind {
	switch {
	case mode.IsRegular():
		return EntryKindRegular
	case mode&fs.ModeSymlink != 0:
		return EntryKindSymlink
	default:
		return EntryKindOther
	}
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
