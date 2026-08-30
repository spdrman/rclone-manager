package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sqliteSideFileSuffixes are the sidecar files a WAL-mode SQLite database
// may have alongside its main file (internal/state.Open always runs in
// WAL mode — see that function's own doc). A snapshot has to capture
// whichever of these actually exist, not just the main file: a process
// that stopped uncleanly can leave committed data sitting in the -wal
// file rather than checkpointed into the main one, and a snapshot that
// dropped it would silently lose that data if it were ever restored from.
var sqliteSideFileSuffixes = []string{walSuffix, shmSuffix}

const (
	walSuffix = "-wal"
	// shmSuffix names SQLite's derived shared-memory index. restore treats
	// it differently from the other two — see restore's own doc.
	shmSuffix = "-shm"
)

// sqliteSnapshot is a pre-migration, in-memory copy of a journal's on-disk
// files, taken before section 46.1's "run transactional migrations" step.
// If migration fails, restore puts every captured file back exactly as it
// was (and removes anything migration created that did not exist before),
// so "the previous data is preserved unchanged" holds regardless of what
// state a failed migration attempt left the live files in.
//
// Held in memory, not copied to a second on-disk location: a fresh
// deployment's journal starts empty and even a long-lived production
// journal is expected to be modest (FR-9's lifecycle rows, not the backup
// artifacts themselves), so the memory cost is small next to the
// correctness this buys — restore has nothing further to read off disk
// that a second failure could have corrupted in between.
type sqliteSnapshot struct {
	// paths is every path this snapshot considered, in a fixed order (main
	// file first, then -wal, then -shm), so restore's own behaviour is
	// deterministic and covers exactly what snapshotSQLite looked at.
	paths []string
	// existed[path] reports whether path had content at snapshot time.
	existed map[string]bool
	// content[path] is that content, present only when existed[path] is
	// true.
	content map[string][]byte
}

// snapshotSQLite captures dbPath and its WAL/SHM sidecars (whichever
// currently exist) into memory. A brand-new deployment, where dbPath does
// not exist yet at all, produces a valid, "nothing existed" snapshot:
// restoring it later means "remove whatever migration created", which is
// exactly "preserve the previous (nonexistent) data unchanged" for that
// case.
func snapshotSQLite(dbPath string) (*sqliteSnapshot, error) {
	snap := &sqliteSnapshot{
		paths:   append([]string{dbPath}, withSuffixes(dbPath, sqliteSideFileSuffixes)...),
		existed: make(map[string]bool),
		content: make(map[string][]byte),
	}

	for _, path := range snap.paths {
		content, err := os.ReadFile(path)
		switch {
		case err == nil:
			snap.existed[path] = true
			snap.content[path] = content
		case errors.Is(err, os.ErrNotExist):
			snap.existed[path] = false
		default:
			return nil, fmt.Errorf("service: pre-migration snapshot of %s: %w", path, err)
		}
	}

	return snap, nil
}

// restore writes every file this snapshot captured back to its original
// path, verbatim, and removes any path that did not exist at snapshot
// time but exists now (a WAL file a failed migration attempt created, for
// example) — so the filesystem afterward matches the pre-migration state
// exactly, not just the main database file's content.
//
// The one file it deliberately does NOT write back is -shm. That file is
// not data: it is a derived shared-memory index that has to correspond to
// the -wal file sitting beside it, and SQLite rebuilds it from that -wal
// on the next open whenever it is absent. Putting captured -shm bytes back
// next to a restored -wal asserts a correspondence this snapshot cannot
// guarantee (the three files are read one after another, not as one atomic
// read), and asserting it wrongly is worse than not asserting it at all,
// so restore removes -shm and lets SQLite do the one thing here that can
// actually be sure it is right. It is captured in the first place only so
// the "did it exist" bookkeeping below stays uniform across all three
// paths.
func (s *sqliteSnapshot) restore() error {
	for _, path := range s.paths {
		if !s.existed[path] || strings.HasSuffix(path, shmSuffix) {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("service: restoring pre-migration snapshot, removing %s: %w", path, err)
			}
			continue
		}
		if err := writeFileAtomically(path, s.content[path], 0o600); err != nil {
			return fmt.Errorf("service: restoring pre-migration snapshot, writing %s: %w", path, err)
		}
	}
	return nil
}

func withSuffixes(base string, suffixes []string) []string {
	out := make([]string, len(suffixes))
	for i, suffix := range suffixes {
		out[i] = base + suffix
	}
	return out
}

// writeFileAtomically writes content to path via a temp-file-plus-rename
// in path's own directory, fsyncing the temp file before the rename AND
// the containing directory after it, so a reader (or a crash) never
// observes a partially written file at path — and, once this returns, a
// crash cannot lose the rename either.
//
// The directory fsync is not belt-and-braces here. Every caller is on a
// path that runs because something has already gone wrong (restore, above,
// undoing a failed migration), so "the bytes were written but the rename
// never reached the disk" would mean the recovery silently did not happen
// on exactly the reboot that needed it.
//
// Mirrors backupsets.go's writeConfigAtomically, generalised to arbitrary
// byte content rather than a marshalled config.Config.
func writeFileAtomically(path string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".restore-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return fsyncDir(dir)
}

// fsyncDir flushes dir's own entries, which is what actually persists a
// rename: fsyncing the renamed file only promises its CONTENT survives a
// crash, never that the directory entry pointing at it does.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
