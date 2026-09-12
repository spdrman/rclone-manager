# ADR 0012: Reading a backup source once, safely, and never twice

## Status

Accepted as the Phase 1 production source adapter for the embedded backup
engine (EPIC #779, issue #782). Implemented in
`core/internal/backupengine/source`, with the enumeration half in
`core/internal/transport` and the source-side stat in
`core/internal/transport/rclone/sourcestream.go`.

It supersedes the Phase 0 streaming spike (ADR 0007) as the thing
production wires, consumes the bounded enumeration and capability matrix
of ADR 0008, and is bound by the trust model of ADR 0009. Nothing in
Phase 1 or later may read a backup source into the engine except through
this package.

## Context

The engine boundary (ADR 0006) can store an object it can only read once,
forward (`backupengine.StreamingRepository`). The transport can hand over
exactly such a reader (`OpenSourceStream`). ADR 0007's spike proved the
two meet with four lines of glue.

Four lines of glue is not a backup product. What sits between them in
production has to decide, per object, whether it may be read at all,
whether what the source says about it may be believed, what to do when it
moves while it is being read, and what to do with the things in a
directory that are not files. Each of those decisions has a wrong answer
that is invisible until a restore.

The forbidden shape is the obvious one and it is worth naming so it stays
forbidden: `rclone copy` the source to a staging directory, then point the
engine at the staging directory. It needs as much free disk as the source,
writes every byte twice, and turns a 100 GB source into 200 GB of I/O for
a backup that changed nothing.

## Decision

### 1. The adapter reads; a Sink stores

`source.Adapter` owns "read this source safely" and nothing else. Where an
object's bytes go is a `source.Sink`, and `source.RepositorySink` is the
production one: one streamed snapshot per object.

One snapshot per object is what the merged streaming boundary offers, and
it is deliberately behind the interface rather than in the adapter.
Gathering a source's objects into a single tree manifest is
snapshot-lifecycle work (#783); when it lands it replaces a sink, and not
one line of the reading path - path safety, cancellation, mutation
detection, the capability gates - has to be re-proven for it.

### 2. Every capability question is asked of the matrix, once

`source.Profile` is the projection of one backend's declared capability
matrix onto the questions this adapter asks. It is the only place a
capability is read.

| Matrix key | What the adapter does with it |
| --- | --- |
| `streaming_open` | false is a refusal (`ErrUnstreamable`). There is no fallback, because the fallback is the staging copy. |
| `bounded_listing` | false refuses `Backup` before anything is dialed. Such a source is backed up with `BackupPaths`. |
| `mtime_precision` | unreadable means the object is recorded with NO modification time, not with the epoch. |
| `generation_identity` | only `versioned` lets an object id be read as CONTENT identity. A path is a slot. |
| `stable_size` | decides whether a reported size may be compared with the bytes a read produced. |
| `symlink_semantics` | `preserve` on a backend that does not declare `store` is a configuration refusal. |
| `case_sensitivity` | anything but `sensitive` makes exclusion matching fold case. |

The last row is the only one where the conservative direction is not
obvious, so it is argued rather than asserted: an exclusion that matches
too much leaves a file out of the backup, which the run report says out
loud, and an exclusion that matches too little backs up the directory of
secrets the operator explicitly named. Only one of those is discovered by
reading the report.

`hash_support` is deliberately NOT projected onto
`model.SourceSignals.RemoteHashAlgorithms`. The matrix key answers "could
a hash be obtained at all", including by reading every byte; the signal
answers "does one arrive without reading". They look alike and are
different questions, and conflating them would classify a local disk as
strong on the strength of work that has not happened. Phase 0's
hand-written `BundledSourceSignals` table is retired in favour of the
projection for the four keys that DO project (`sourceconsistency`), with
the residual hash column left as a small named table whose default is
empty.

### 3. One retry authority

The adapter's `MaxAttempts` is the only retry bound in the run. The sink
asks the engine for exactly one attempt
(`StreamSnapshotRequest.MaxAttempts: 1`).

Three bounds of three multiply to twenty-seven reads of an object that was
never going to be readable: a backup window spent on one file, and a
report that says "attempted 3". A retry is a fresh open from byte zero,
because a remote stream cannot be resumed without a seek this boundary
refuses to pretend it has.

### 4. Mutation during read: discard, retry, or say the run is incomplete

A streamed object is read ONCE, so the confirmation ADR 0009's
`sourceconsistency.Reader` gets from a second pass is not available: the
bytes are in the repository by the time anything can be compared.

What is available is metadata on both sides of the read window plus the
byte count the read actually produced. If size, modification time or a
versioned generation identity moved across the window, or the read's
length disagrees with the source's post-read size, the object stored on
that attempt is **discarded from the repository** and the read is retried
against the new baseline. Beyond the bound the object is reported
incomplete, nothing is left behind, and the run is not complete.

`sourceconsistency.DescribeMovement` is exported and reused so that an
operator cannot tell which code path noticed that their file moved.

#### What this does not catch, stated plainly

An in-place rewrite of the SAME LENGTH within the SAME SECOND is
invisible. `transport.RemoteArtifact.ModTime` is unix seconds, sizes
match, and a local filesystem offers no content identity. Closing that
window needs a second read and a digest comparison - which is exactly
`sourceconsistency.Reader`, and exactly what a single-pass stream cannot
do. An operator whose source has writers that do this has two real
answers: a consistency mode that freezes the source, or a backend that
versions content. Pretending otherwise is what this ADR exists to
prevent.

### 5. Paths are untrusted, including the source's own

`SafeRelPath` refuses traversal, absolute injection, Windows drive and UNC
spellings, NUL, invalid UTF-8, backslashes and over-long names, and
canonicalises the harmless spellings. Refused entries are REPORTED, never
silently dropped and never rewritten into something plausible.

Backslash is refused rather than escaped because it is a separator on one
platform and a filename character on another: one name would mean two
different trees in two builds, and a restore of the wrong one is a silent,
plausible-looking wrong answer.

Case folding is not part of the safety check, and that is a decision. A
global fold set would be memory linear in the source - the exact
allocation ADR 0008 exists to refuse. What matters for safety is that no
case variation reaches a different answer than its lowercase spelling,
which is a property over arbitrary input and is fuzzed as one
(`FuzzSafeRelPathCannotEscapeTheRoot`, which also pins idempotence, and
which found a real non-idempotence in the first implementation: `./C:`
cleaned to `C:`, which the same function then refused).

### 6. Symlinks are not followed, under either policy

`preserve` and `ignore`, and no `follow`. Following is not a policy this
product has decided it can defend: a followed link can leave the backup
root, produce a cycle, cross onto another filesystem, or pull in a tree
nobody asked for, and each needs its own answer before the word appears in
a config file.

`preserve` stores the link's TARGET as the object's content, under the
link's own path, and requires both a backend declaring
`symlink_semantics: store` and a transport that can read a target without
following it. No bundled backend declares `store` today, so `preserve` is
a refusal on all three - by name, at the start of the run, rather than a
silent degrade to `ignore`. A target that leaves the root is refused
lexically, without resolving anything, because resolving it would be
following it.

### 7. Special files are named, not guessed

Sockets, fifos and device nodes are counted, reported and never opened.
Never opened is the load-bearing half: a read of a fifo with no writer
does not return, so an adapter without a policy for one hangs a backup
window on a file nobody meant to back up. A real fifo is in the test suite
for that reason - the failure mode is a hang, so a fake would not have
proven it.

This required the bounded enumerator to REPORT what it previously skipped
(`EnumerateOptions.ReportNonRegular`, `RemoteArtifact.Kind`). An entry
nobody classified is `EntryKindUnknown`, which is skipped rather than
opened: unknown is an absence of an answer, and the answer that costs an
open is the one that may not be assumed.

The one exception is a path the CALLER named through `BackupPaths`, which
is the operator asserting that it is an object. No transport in this build
can classify a remote path anyway - rclone's `Fs.NewObject` returns an
object for a fifo, of size zero, and follows a symlink to its target - so
the choice is between the operator's assertion and a guess dressed up as a
resolution.

### 8. A source-side stat that does not read the object

`transport.Transport.Stat` computes a SHA-256 where the backend advertises
one, and rclone's local backend advertises one by reading the whole file.
That is right for the destination side, where a stat is how a copy is
proven. It is catastrophic here: the post-read check stats every object it
has just streamed, so using it would read every byte of a 100 GB source a
second time to answer a question about its size.

`Adapter.StatSource` is metadata only. The interface the source adapter
asks for names that method rather than `Stat`, so the hashing one cannot
be wired in by accident: it does not satisfy the interface.

### 9. Bounded on every axis, and cancellable

- Enumeration is ADR 0008's chunked walk: one chunk per LEVEL of the tree.
- Concurrency is a fixed worker pool over an unbuffered channel, so the
  walk blocks when the workers are busy. Without that backpressure a fast
  local listing builds a queue of every path in the source while four
  workers read four of them.
- The report is counters plus a capped sample of sentences
  (`maxReportedReasons = 64`), never a record per file. A caller that
  needs per-file detail gets it streamed through `OnResult`, for the same
  reason the directory listing is streamed. This is why the adapter does
  NOT accumulate a `sourceconsistency.Run`: that type retains one
  `Capture` per file.
- Cancellation closes readers from the OUTSIDE, from a watcher goroutine
  running beside the in-flight `Store`. A context is not something a
  `Read` already blocked on a socket consults, and the sink does not
  return while it is blocked, so "after Store returns" is too late to be
  the only place a reader is closed.

## Consequences

**What this buys.** A source is read once, its bytes never exist outside
the repository, every capability answer comes from one authority, a torn
read is never left behind as a restore point, and a run report says what
it does and does not contain.

**What it costs.** One goroutine per in-flight object for the cancellation
watcher (bounded by the worker count). One extra metadata round trip per
object for the post-read stat, skipped only under a consistency mode that
guarantees a point in time. One snapshot per object until #783 replaces
the sink. And the same-second same-length blind window in §4, which is a
property of single-pass streaming and not of this implementation.

**What is refused rather than supported.** A backend that cannot stream; a
walk of a backend that cannot be listed within a bound; a `preserve`
symlink policy on a backend that does not store links; a backend that
reports nothing a read window could be checked against, unless the mode
freezes the source. Each refusal names the capability it rests on, so an
operator reads a sentence about their backend rather than a stack trace.
