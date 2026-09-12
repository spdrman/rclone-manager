package sourceconsistency

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Kind is what a source says is at a path. It is part of Stat rather than
// derived later because the reader has to know it BEFORE it opens anything:
// reading a symlink's target is a decision about symlink semantics that
// belongs to the backend capability matrix, and a reader that made it by
// accident would be making it in the most expensive possible place.
//
// The empty Kind is not one of these values and is not treated as
// "regular". A source that does not say what is at a path has not answered
// the question, and the reader reports that loudly (OutcomeUnreadable)
// rather than assuming the answer that costs a read.
type Kind string

const (
	// KindRegular is a file with content to read. The only kind this reader
	// captures bytes from.
	KindRegular Kind = "regular"

	// KindDir is a directory. Enumeration is not this package's decision.
	KindDir Kind = "directory"

	// KindSymlink is a symbolic link, reported as itself and never followed.
	KindSymlink Kind = "symlink"

	// KindOther is a socket, device, fifo or anything else a source can
	// name. Grouped, because the reader's answer for all of them is the
	// same: there is no content here to prove coherent.
	KindOther Kind = "other"
)

func (k Kind) String() string { return string(k) }

// Stat is the facts a capture compares across its own read window.
// Deliberately not os.FileInfo: the comparison has to work the same way for
// a local directory and for a remote source reached over a transport, and
// most of os.FileInfo has no answer on the far side.
type Stat struct {
	// Kind is what the source says is at the path, and it is what decides
	// whether there is content to capture at all.
	Kind Kind

	// Size is the length the source reports.
	Size int64

	// ModTimeNanos is the modification time in nanoseconds, at whatever
	// resolution the source actually keeps. It is compared at full
	// resolution here and never rounded to a backend's declared precision:
	// the declared precision is a statement about what survives ACROSS runs,
	// and these two stats are seconds apart within one read.
	ModTimeNanos int64

	// Identity names the object rather than the path - a device and inode
	// number on a unix filesystem - and is empty where the source cannot
	// name one. It is the only signal that distinguishes "the file at this
	// path was rewritten" from "a different file was renamed onto this
	// path", and the two need different handling: the first is a mutation
	// the retry will settle, the second means the bytes just read belong to
	// a file that is no longer there.
	//
	// Empty is a legitimate value and is treated as "no opinion" rather than
	// as a match, so a source that cannot name objects falls back to size
	// and modification time and loses only the distinction, not the
	// detection.
	Identity string
}

// File is an open handle a capture reads and then re-stats. Stat is on the
// handle rather than on the path on purpose: an fstat answers about the
// object the bytes came from even after the path has been renamed onto,
// which is exactly the case a path stat cannot distinguish.
//
// Every method that can block carries a context, which is why this is not
// an io.Reader. The one source implementation in this package reads a local
// file and can only check the context between reads; the implementations
// this interface exists for reach a source over a transport, where a read
// that never returns is an ordinary Tuesday and a run that cannot be
// cancelled through it is a run that hangs forever. Handing the context to
// the source is the only place that can be fixed.
type File interface {
	// Read fills p and must abandon the read when ctx is done.
	Read(ctx context.Context, p []byte) (int, error)

	// Stat answers about the object this handle was opened on, not about
	// whatever the path names now.
	Stat(ctx context.Context) (Stat, error)

	// Close releases the handle. It takes no context: a close that hangs is
	// a bug in the source, and giving a caller a way to abandon one would
	// leak the handle rather than fix it.
	Close() error
}

// Source is the tree a run reads. Two methods, because that is all a
// capture needs, and a narrow interface is what lets the mutation harness
// order real mutations against a real reader without any test seam in the
// reader itself.
type Source interface {
	Stat(ctx context.Context, path string) (Stat, error)
	Open(ctx context.Context, path string) (File, error)
}

// ErrEscapesRoot is returned for a path that resolves outside the source's
// root. It is its own error rather than an os error because it is not the
// filesystem's answer, it is this package refusing to ask the question.
var ErrEscapesRoot = errors.New("path resolves outside the source root")

// ErrNoRoot is returned by an OSSource that was built without one.
//
// It is a refusal rather than a default because the default is dangerous in
// a way that is easy to miss: filepath.Join("", "etc/shadow") is
// "etc/shadow", so an empty root silently relocates every path in a run to
// whatever the process's working directory happens to be, and the lexical
// containment check below would pass every one of them.
var ErrNoRoot = errors.New("the source has no root directory")

// OSSource is a local directory tree. Paths are relative to Root.
//
// The containment check is lexical: Root is cleaned, the path is joined and
// the result must still be under Root. That refuses "../" and an absolute
// path, which is what a directory listing can hand back (FR-8 treats every
// name off a source as untrusted). It does NOT resolve symlinks, and that
// is a deliberate boundary rather than an omission: whether a symlink is
// stored, followed or skipped is the backend capability matrix's
// symlink_semantics decision. Stat reports a symlink as KindSymlink and
// Open refuses to traverse one, so the decision cannot be made here by
// accident.
type OSSource struct {
	Root string
}

func (s OSSource) resolve(path string) (string, error) {
	if s.Root == "" {
		return "", fmt.Errorf("%w, so %q cannot be resolved", ErrNoRoot, path)
	}

	if filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: %q is absolute", ErrEscapesRoot, path)
	}

	root := filepath.Clean(s.Root)
	full := filepath.Join(root, path)

	rel, err := filepath.Rel(root, full)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrEscapesRoot, path)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrEscapesRoot, path)
	}

	return full, nil
}

// Stat lstats the path: a symlink is reported as a symlink, never as
// whatever it points at.
func (s OSSource) Stat(ctx context.Context, path string) (Stat, error) {
	if err := ctx.Err(); err != nil {
		return Stat{}, err
	}

	full, err := s.resolve(path)
	if err != nil {
		return Stat{}, err
	}

	fi, err := os.Lstat(full)
	if err != nil {
		return Stat{}, fmt.Errorf("stat %s: %w", path, err)
	}

	return statOfFileInfo(fi), nil
}

// Open opens the path itself and not a link's target. The O_NOFOLLOW is the
// second half of the symlink boundary: the reader checks the kind from a
// stat first, and this is what refuses the race where a symlink is put in
// place between that stat and this open.
func (s OSSource) Open(ctx context.Context, path string) (File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	full, err := s.resolve(path)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(full, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return osFile{f}, nil
}

type osFile struct {
	f *os.File
}

// Read checks the context and then reads. A local read cannot be
// interrupted once the syscall is in flight, so the check is between reads
// and that is all this implementation can honestly offer; the contract
// exists for the sources where it is the difference between a cancelled run
// and a hung one.
func (o osFile) Read(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	return o.f.Read(p)
}

func (o osFile) Close() error { return o.f.Close() }

func (o osFile) Stat(ctx context.Context) (Stat, error) {
	if err := ctx.Err(); err != nil {
		return Stat{}, err
	}

	fi, err := o.f.Stat()
	if err != nil {
		return Stat{}, fmt.Errorf("stat open file: %w", err)
	}

	return statOfFileInfo(fi), nil
}

func statOfFileInfo(fi os.FileInfo) Stat {
	return Stat{
		Kind:         kindOfMode(fi.Mode()),
		Size:         fi.Size(),
		ModTimeNanos: fi.ModTime().UnixNano(),
		Identity:     fileIdentity(fi),
	}
}

func kindOfMode(mode fs.FileMode) Kind {
	switch {
	case mode.IsRegular():
		return KindRegular
	case mode.IsDir():
		return KindDir
	case mode&fs.ModeSymlink != 0:
		return KindSymlink
	default:
		return KindOther
	}
}

// DescribeMovement reports, in one operator-readable sentence, what moved
// between two stats of the same file, and the empty string when nothing did.
//
// Identity is checked first and is decisive, because a different object at
// the same path makes the size and timestamp comparison meaningless: two
// different files can easily be the same length, and a careful writer's
// rename-into-place produces exactly that.
//
// It is exported for the streaming source adapter, which cannot use this
// package's Reader at all - a remote object read straight into the backup
// engine's chunker is read ONCE, by definition, so there is no second pass
// to compare digests across - and which still has to answer the same
// question about the same two stats and say so in the same words. An
// operator reading a run report should not be able to tell which code path
// noticed that their file moved.
func DescribeMovement(before, after Stat) string {
	if before.Identity != "" && after.Identity != "" && before.Identity != after.Identity {
		return fmt.Sprintf("the path stopped naming the object that was read (%s became %s), which is what a rename into place looks like", before.Identity, after.Identity)
	}

	// A kind change with no identity change is only reachable on a source
	// that cannot name objects, which is exactly where it is the last
	// remaining signal that the path is not what it was.
	if before.Kind != after.Kind {
		return fmt.Sprintf("the path named a %s before the read and a %s after it", before.Kind, after.Kind)
	}

	if before.Size != after.Size {
		return fmt.Sprintf("the size changed from %d to %d inside the read window", before.Size, after.Size)
	}

	if before.ModTimeNanos != after.ModTimeNanos {
		return fmt.Sprintf("the modification time changed from %s to %s inside the read window",
			time.Unix(0, before.ModTimeNanos).UTC().Format(time.RFC3339Nano),
			time.Unix(0, after.ModTimeNanos).UTC().Format(time.RFC3339Nano))
	}

	return ""
}
