// Test seams for the mutation harness.
//
// Every mutation these helpers perform is a REAL mutation of a REAL file on
// a real filesystem, ordered against the reader by wrapping the Source the
// reader was handed rather than by sleeping. A harness built on timing would
// be the flakiest test in the repository and would prove nothing about the
// case it is named after: "the file changed between the stat and the last
// byte of the read" is a statement about ordering, so the ordering is what
// the test controls.

package sourceconsistency

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// sha256Hex is what a capture's digest is expected to be, computed the
// obvious way so a test asserts against the content rather than against the
// reader's own arithmetic.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// hookSource wraps a Source and runs a function at one of the two moments a
// mutation can be interesting: just after the file is opened (before any
// byte is read) and just after the first chunk has been handed back (so the
// remainder of the read comes from whatever the mutation left behind).
//
// untilAttempt bounds how many of the reader's attempts get mutated, which
// is what lets one test assert "a bounded retry settles this" and another
// assert "a source that will not hold still marks the run incomplete".
type hookSource struct {
	inner        Source
	afterOpen    func()
	afterChunk   func()
	untilAttempt int
	opens        int
}

func (s *hookSource) Stat(path string) (Stat, error) { return s.inner.Stat(path) }

func (s *hookSource) Open(path string) (File, error) {
	f, err := s.inner.Open(path)
	if err != nil {
		return nil, err
	}

	s.opens++
	if s.opens > s.untilAttempt {
		return f, nil
	}

	if s.afterOpen != nil {
		s.afterOpen()
	}

	return &hookFile{File: f, afterChunk: s.afterChunk}, nil
}

type hookFile struct {
	File
	afterChunk func()
	fired      bool
}

func (f *hookFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	if n > 0 && !f.fired && f.afterChunk != nil {
		f.fired = true
		f.afterChunk()
	}

	return n, err
}

// writeFile creates a source file and returns nothing but the path, because
// no test here cares about anything else about it.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()

	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}

	return p
}

// rewrite replaces a file's content in place, leaving the filesystem to
// update size and modification time as it normally would.
func rewrite(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("rewrite %s: %v", path, err)
	}
}

// rewritePreservingMetadata is the adversary this whole package exists for:
// the same number of bytes, different content, and the modification time put
// back where it was. Anything that reasons about content from size and mtime
// alone sees nothing here, and that includes kopia's own cached-entry
// heuristic (see KopiaMetadataReuse).
//
// It is not an exotic attack. rsync --times, tar -p, an editor that restores
// timestamps and a restore-from-backup of the source itself all do exactly
// this.
func rewritePreservingMetadata(t *testing.T, path, content string) {
	t.Helper()

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	if int64(len(content)) != before.Size() {
		t.Fatalf("rewritePreservingMetadata needs %d bytes to keep the size identical, got %d", before.Size(), len(content))
	}

	rewrite(t, path, content)

	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("restat %s: %v", path, err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("the filesystem under this test will not hold size and mtime still: before %d/%v, after %d/%v",
			before.Size(), before.ModTime(), after.Size(), after.ModTime())
	}
}

// bumpModTime moves a file's modification time by a sub-second amount
// without touching its content, which is the "sub-second change" case: a
// source whose timestamps have nanosecond resolution reports this, and one
// that rounds to a second does not.
func bumpModTime(t *testing.T, path string, d time.Duration) {
	t.Helper()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	when := fi.ModTime().Add(d)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// subSecondTimestampsAvailable reports whether the filesystem under this
// test can actually hold a sub-second modification time. It is a probe
// rather than an assumption: the repository's own test matrix includes
// filesystems that round to the second, and a test that asserted a
// nanosecond timestamp on one of those would be failing for the
// filesystem's reason rather than the reader's.
func subSecondTimestampsAvailable(t *testing.T, path string) bool {
	t.Helper()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	when := fi.ModTime().Truncate(time.Second).Add(500 * time.Millisecond)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("restat %s: %v", path, err)
	}

	return after.ModTime().Nanosecond() != 0
}

// statOf reads the reader's own view of a file, so tests can build catalogue
// entries out of exactly what a real capture would have recorded.
func statOf(t *testing.T, src Source, path string) Stat {
	t.Helper()

	st, err := src.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}

	return st
}
