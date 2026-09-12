package sourceconsistency

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Stat is the three facts a capture compares across its own read window.
// Deliberately not os.FileInfo: the comparison has to work the same way for
// a local directory and for a remote source reached over a transport, and
// most of os.FileInfo has no answer on the far side.
type Stat struct {
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
type File interface {
	io.Reader
	Stat() (Stat, error)
	Close() error
}

// Source is the tree a run reads. Two methods, because that is all a
// capture needs, and a narrow interface is what lets the mutation harness
// order real mutations against a real reader without any test seam in the
// reader itself.
type Source interface {
	Stat(path string) (Stat, error)
	Open(path string) (File, error)
}

// ErrEscapesRoot is returned for a path that resolves outside the source's
// root. It is its own error rather than an os error because it is not the
// filesystem's answer, it is this package refusing to ask the question.
var ErrEscapesRoot = errors.New("path resolves outside the source root")

// OSSource is a local directory tree. Paths are relative to Root.
//
// The containment check is lexical: Root is cleaned, the path is joined and
// the result must still be under Root. That refuses "../" and an absolute
// path, which is what a directory listing can hand back (FR-8 treats every
// name off a source as untrusted). It does NOT resolve symlinks, and that
// is a deliberate boundary rather than an omission: whether a symlink is
// stored, followed or skipped is the backend capability matrix's
// symlink_semantics decision, and a reader that quietly followed one would
// be making that decision by accident, in the most expensive possible place.
type OSSource struct {
	Root string
}

func (s OSSource) resolve(path string) (string, error) {
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

func (s OSSource) Stat(path string) (Stat, error) {
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

func (s OSSource) Open(path string) (File, error) {
	full, err := s.resolve(path)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(full)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return osFile{f}, nil
}

type osFile struct {
	f *os.File
}

func (o osFile) Read(p []byte) (int, error) { return o.f.Read(p) }
func (o osFile) Close() error               { return o.f.Close() }

func (o osFile) Stat() (Stat, error) {
	fi, err := o.f.Stat()
	if err != nil {
		return Stat{}, fmt.Errorf("stat open file: %w", err)
	}

	return statOfFileInfo(fi), nil
}

func statOfFileInfo(fi os.FileInfo) Stat {
	return Stat{
		Size:         fi.Size(),
		ModTimeNanos: fi.ModTime().UnixNano(),
		Identity:     fileIdentity(fi),
	}
}

// describeMovement reports, in one operator-readable sentence, what moved
// between two stats of the same file, and the empty string when nothing did.
//
// Identity is checked first and is decisive, because a different object at
// the same path makes the size and timestamp comparison meaningless: two
// different files can easily be the same length, and a careful writer's
// rename-into-place produces exactly that.
func describeMovement(before, after Stat) string {
	if before.Identity != "" && after.Identity != "" && before.Identity != after.Identity {
		return fmt.Sprintf("the path stopped naming the object that was read (%s became %s), which is what a rename into place looks like", before.Identity, after.Identity)
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
