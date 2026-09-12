// Package sourceconsistency is the layer that decides when the backup
// engine's own "this file has not changed" answer is not good enough, and
// the reader that proves a file was captured coherently rather than torn.
//
// # The finding this package exists because of
//
// kopia v0.23.1 (the version pinned for this epic) reuses a file's content
// from a previous snapshot when the current directory entry and the stored
// one agree on name, mode, owner, modification time and size. That is the
// whole test. It is `metadataEquals` in `snapshot/upload/upload.go`
// (lines 694-720 at that tag), reached from `findCachedEntry`
// (722-754) and `processSingle` (852-868); `commonMetadataEquals` compares
// `ModTime()` with `time.Time.Equal` at full resolution, then `Mode()`, then
// `Owner()`, and `metadataEquals` adds `Size()`.
//
// Three consequences follow, and all three are load-bearing here:
//
//  1. NO content is re-read for a file that looks unchanged. The only escape
//     hatch is `Uploader.ForceHashPercentage`, a random re-hash probability
//     (`maybeIgnoreCachedEntry`, 756-767) whose zero default means never.
//  2. The metadata compared is captured at SCAN time, not at read time.
//     `fs/localfs`'s entry caches size, mtime, mode and owner from the stat
//     that the directory walk performed (`fs/localfs/local_fs.go:16-45`), and
//     `Open` does not re-stat (108-115). `newDirEntry` (upload.go:450-474)
//     writes those cached values into the stored entry, with `FileSize`
//     then overwritten by the number of bytes actually read
//     (`uploadFileData`, 237-300).
//  3. So a file that was rewritten in place DURING its own read, with the
//     same length and its modification time restored, is stored as a
//     coherent file with pre-read metadata, and every later run agrees it
//     has not changed. Nothing in the engine is in a position to notice;
//     the information needed was destroyed before the bytes were read.
//
// None of that is a defect in kopia. It is the standard, correct heuristic
// for a snapshot tool, it is what makes an incremental pass fast, and the
// alternative - hash every byte of every file every run - is what this
// package's conservative preset actually does when an operator asks for it.
// What is not acceptable is inheriting the heuristic silently, because the
// claim this product makes is not "fast incremental backups", it is that a
// restore point contains what the operator thinks it contains.
//
// # What this package adds
//
//   - Decide: the re-read decision, driven by a model.VerificationPolicy
//     derived from the source's metadata-trust class. Where kopia would
//     reuse on metadata alone and the class does not support it, this says
//     read. The engine's own rule is kopiaMetadataReuse in decide_test.go,
//     which keeps the gap between the two executable without putting "what
//     would kopia do" on this package's API surface.
//   - Reader: a capture that bounds its own window. It classifies the path,
//     stats before the read, stats the OPEN DESCRIPTOR after it, re-stats
//     the path, and - unless the mode promises a frozen image - reads a
//     second time and compares digests, which is the only thing that
//     catches a mutation whose metadata was put back. Every capture ends in
//     a deterministic outcome: settled on the first attempt, settled after
//     a bounded retry, or not verified. There is no path on which unproven
//     bytes are recorded as proven, and no way to build a Reader that skips
//     the confirmation.
//   - Run: the accumulator that makes a run's completeness a property of its
//     captures, and that separates "this source moved" from "this source
//     moved and the operator told us it could not", and both of those from
//     "this source would not answer".
//
// # What this package deliberately does not do
//
// It does not import kopia, and no type here is a kopia type. The engine
// lives behind core/internal/backupengine; this package sits above that
// boundary and speaks only in model vocabulary and its own.
//
// It also does not decide symlink or enumeration semantics. Whether a
// symlink is stored, followed or skipped, and how a directory is listed
// within a bounded memory budget, belong to the backend capability matrix
// and the enumeration contract. A symlink reaches a caller as a capture
// whose Kind says what it is and whose outcome says there was no content to
// prove; what happens to it next is the matrix's decision, not this
// package's. The only capability facts read here are the five that bear on
// trust, copied into model.SourceSignals.
package sourceconsistency
