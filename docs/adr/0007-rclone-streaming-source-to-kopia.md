# ADR 0007: An rclone stream reaches Kopia as a streaming file, with no staging and no fake Seek

## Status

Accepted as a feasibility finding (phase 0 of the embedded-engine epic). The
code it describes is a spike kept as a compatibility smoke test, not a
shipped feature: `core/internal/backupengine` is not wired into the service,
the CLI, the API or the catalog, and nothing outside its own tests calls it.

It ships as part of the one boundary change ADR 0006 records, not as a
parallel one. The finding was first prototyped as its own engine — a second
`Engine` type, in the manager-owned package, importing Kopia directly — and
that is two boundaries rather than one capability, so the streaming path now
lives in `core/internal/backupengine/kopia` behind the `Engine` /
`Repository` port, reached through `backupengine.StreamingRepository`. What
the spike measured did not change in the move; where it lives did, and the
boundary tests in ADR 0006 are what keep it there.

The Kopia pin it was measured against, `github.com/kopia/kopia@v0.23.1`, is
the one ADR 0006 records. Whether to embed Kopia at all is that decision,
taken with this ADR's findings, the dependency and binary-size measurements
and the source-consistency findings in front of it. This ADR answers exactly
one of the questions on that list.

## Context

This product reads backup sources over rclone. `transport.Transport`'s one
read verb is `CopyToLocal`, whose contract is "this remote file is now a
local file": internal/lifecycle hands it a `.partial` path, rclone fills it,
and the manager renames it. That contract is right for a pull-and-verify
cycle and wrong for an incremental backup engine, which wants to hash,
chunk, compress and encrypt the bytes as they arrive. Staging first means
writing every byte twice and needing room for the whole file once, which for
the sources this product exists to protect is the difference between a
backup that runs on a NAS and one that does not.

Kopia is the candidate engine. Kopia's own source model is a filesystem:
`fs.File.Open` returns an `fs.Reader`, and `fs.Reader` embeds `io.Seeker`.
An rclone `Object.Open` returns an `io.ReadCloser` and nothing more. The
obvious bridge between those two is a `Seek` implemented by closing and
reopening the remote, or by a ranged request, or by discarding bytes until
the offset arrives -- and every one of those is a correctness and
performance trap wearing an interface's clothes. It was worth knowing,
before any of the rest of the embedded-engine epic was decided, whether that bridge is needed at
all.

## Decision

**An rclone stream is given to Kopia as an `fs.StreamingFile`. Nothing is
staged, nothing is buffered whole, and no `Seek` is implemented, faked or
required.**

### The exact Kopia API used

All of it is in `core/internal/backupengine/kopia` (`stream.go`), against
v0.23.1:

| What | Where |
| --- | --- |
| `fs.StreamingFile` -- `Entry` + `GetReader(ctx) (io.ReadCloser, error)` | `fs/entry.go:53` |
| `virtualfs.NewStaticDirectory(name, []fs.Entry)` | `fs/virtualfs/virtualfs.go:90` |
| `upload.NewUploader(repo.RepositoryWriter)`, `(*Uploader).Upload` | `snapshot/upload/upload.go:1234`, `:1267` |
| `(*Uploader).uploadStreamingFileInternal` -- the path taken | `snapshot/upload/upload.go:341` |
| `snapshot.SaveSnapshot`, `snapshot.LoadSnapshot`, `snapshot.ListSnapshots` | `snapshot/manager.go` |
| `snapshotfs.SnapshotRoot` for restore | `snapshot/snapshotfs/repofs.go:294` |
| `repo.Initialize` / `repo.Connect` / `repo.Open` / `repo.WriteSession` | `repo/` |
| `filesystem.New` for the repository blob store | `repo/blob/filesystem` |

The adapter implements `fs.StreamingFile` itself rather than using
`virtualfs.StreamingFileFromReader`, for two reasons that both turned out to
matter. `virtualfs` takes the reader in its constructor, so the remote would
be opened before Kopia decided it wanted it; and it wraps nothing, so there
is no place to make cancellation reach a reader that is blocked in `Read`.
`streamingEntry` opens lazily inside `GetReader` and wraps what it gets in
`guardedReader`.

### One name, derived once

A snapshot's stored entry name and the name a restore looks up are the same
string or the snapshot is unrestorable, and the first cut of this spike had
them derived two different ways: the entry took the source's whole
slash-separated name, the restore took `filepath.Base` of the recorded
source path. An object called `runs/2026/db.dump` -- an ordinary object name
on every backend this product speaks -- therefore snapshotted successfully,
reported its bytes, and failed to open with `entry not found`. An
unrestorable backup that every statistic calls healthy is the exact failure
this product exists to prevent, so both halves now call one `leafName`
function on the identity the manifest stores, `TestNestedObjectNameRoundTrips`
snapshots and restores that path, and `TestLeafNameIsDerivedOnce` pins the
derivation itself.

The identity is carried by `StreamSnapshotRequest.Source`, not by the stream:
`StreamSource` is an opener and a modtime, and it has no `Name`. A remote
path and a backup set's name in the repository are different concerns, and
having one type answer both is what let them disagree. `Source.Path` is
required to be rooted (`/runs/2026/db.dump`) and is cleaned, never resolved
against the filesystem -- `filepath.Abs` on a remote object path would mix
the process working directory into a snapshot's identity, so the same object
would list differently depending on where backupd was started from.

### Why this is not an accidentally-seekable path

Kopia's `newDirEntry` (`snapshot/upload/upload.go:450`) type-switches
`case fs.File, fs.StreamingFile`, with `fs.File` FIRST. An entry that grew
an `Open(ctx) (fs.Reader, error)` method would silently leave the streaming
path and be asked for a seekable reader, and nothing would say so. So
`TestStreamingEntryIsNotSeekable` asserts the negative directly: the entry
satisfies `fs.StreamingFile`, does NOT satisfy `fs.File`, and the reader it
hands over implements neither `io.Seeker` nor `io.ReaderAt`.

`streamingEntry.Size()` returns 0 and `LocalFilesystemPath()` returns `""`.
Both are honest rather than lazy: the length of a stream is unknown until it
ends, and there is no local path. Kopia agrees about the first --
`uploadStreamingFileInternal` overwrites `DirEntry.FileSize` with the number
of bytes it actually copied -- and the second is precisely the method a
staging implementation would have had to fill in.

### The one new thing on the rclone side

`Adapter.OpenSourceStream` (`core/internal/transport/rclone/sourcestream.go`)
is the read half `CopyToLocal` never exposed. It returns an `io.ReadCloser`
and no rclone type, the same promise every other method on that adapter
makes, and it releases its `Fs` through `fsBoundReadCloser` on the reader's
own `Close` -- the arrangement `OpenObject` already uses for mediums,
because a method that hands back a live reader has no way out to defer a
release on.

What the boundary depends on is that one method and not that adapter:
`backupengine.SourceStreamer` is a single-method interface and
`NewRcloneSource` takes it, so the manager-owned package does not pull in the
transport's backend set, its connection accounting or its dependency graph
through one field. The app layer holds the real adapter and passes it in;
`TestRcloneSourceOpensLazilyThroughTheInterface` satisfies the same
dependency in eleven lines and also pins the laziness -- constructing a
source must not open anything, because a run that fails before it reaches
this object should never have touched the remote.

### Memory is bounded by Kopia's own copy loop

`uploadStreamingFileInternal` calls `copyWithProgress`, which takes a
pooled buffer from `internal/iocopy` -- `iocopy.BufSize` is **65536** -- and
loops read/write over it. There is no `io.ReadAll` anywhere on the path and
this engine adds none. The measurements below are the proof; the constant is
the reason.

## Measurements

Taken on darwin/arm64 (Apple M5), Go 1.27.0, Kopia v0.23.1, filesystem
repository with content caching switched off (an empty `CacheDirectory`,
which is how Kopia spells "no cache" -- a cache would be a second place the
source's bytes could land, and the claim here is that there is no such
place). Reproduce with `go test ./internal/backupengine/kopia/` from `core/`.

### Bounded memory on a multi-GB stream

`TestLargeStreamIsBoundedInMemory` generates 2 GiB from a deterministic
xorshift reader (nothing multi-GB is committed, and the stream never exists
in full anywhere), snapshots it, and samples `runtime.MemStats.HeapAlloc`
throughout:

```
streamed       2,147,483,648 bytes  (2.00 GiB)
peak heap         71,834,056 bytes  (68.5 MiB)  = 3.35% of the stream
                  71,967,488 on a second run    = 3.35%, sampler jitter
                  71,769,016 re-measured behind the consolidated boundary
                                    (68.4 MiB)  = 3.34%
repository     2,147,750,025 bytes              = +0.012% over the payload
largest blob      28,942,705 bytes  (27.6 MiB)  = 1.35% of the stream
```

Peak live heap is a working set, not a function of file size. The test fails
if peak heap reaches either 256 MiB or one eighth of the stream, so an
implementation that started buffering would be caught long before it
buffered the file.

The test does not call `t.Parallel()`, and that is not tidiness: `peakHeap`
samples process-wide `HeapAlloc`, so a sibling test holding a 64 MiB payload
is charged to this one. Run alongside the rest of the package it reads
865 MiB and means nothing; run alone it reads 68.4 MiB and means what it
says.

The repository overhead of +0.012% on incompressible data is the other half
of the same statement: the bytes were chunked and packed as they went past.
The largest single file anywhere under the repository is a 27.6 MiB pack
blob, against a 2 GiB source; the test's ceiling is 64 MiB.

No throughput figure is quoted. The same test took 11.4 s alone and 23.4 s
while the rest of the package ran beside it, which is an honest statement
about a contended laptop and no statement at all about Kopia. Phase 0 asked
whether memory is bounded, and that is what is measured here.

### No local mirror

`assertNothingStaged` walks the entire test root EXCEPT the repository and
fails on any regular file over 64 KiB. It runs in the small-file test, the
multi-GB test and the end-to-end rclone test. Across all three the only
thing outside the repository is Kopia's own connection config. There is no
spool, no `.partial`, no temp file.

### Content reuse on an unchanged second snapshot

`TestSecondUnchangedSnapshotReusesContent` snapshots 64 MiB of
incompressible data twice, measuring both bytes pushed to blob storage
(through `repo.WriteSessionOptions.OnUpload`) and the repository's size on
disk:

```
payload                 67,108,864 bytes
first snapshot uploaded 67,123,307 bytes
second snapshot uploaded     4,607 bytes   = 0.0069% of the payload
repository growth            4,441 bytes   = 0.0066% of the payload
```

The second run still READ all 67,108,864 bytes -- `StreamSnapshotInfo.Bytes`
is asserted to be the whole payload -- and stored none of them again. Both
snapshots restore to the payload's SHA-256.

That is content-level dedupe, reached deliberately. Kopia would also have
skipped the read entirely: `findCachedEntry`
(`snapshot/upload/upload.go:727`) takes a `commonMetadataEquals` branch for
`fs.StreamingFile` and will reuse a previous entry on matching mode and
modtime alone, if previous manifests are passed to `Upload`. `SnapshotStream`
passes none, and that is now a stated property of the port rather than a
detail of this implementation: a streamed object has no size, so "unchanged"
would mean "same modification time", and a source that rewrites an object in
place while preserving its mtime -- a restored database file, a dump written
by a tool that copies timestamps, a remote with a coarse clock -- would have
its new content skipped without a byte being read. The snapshot would then be
well-formed, its content intact, and the wrong content, which nothing
downstream can detect.
`TestChangedContentWithPreservedModTimeIsReRead` snapshots two different 4 MiB
payloads under one identity with one frozen modtime and asserts the second
restores to the second payload. **If a later phase wants the cheap path, that
is a policy decision about trusting source metadata and it needs its own
argument** -- and the source-consistency findings on mtime precision are the
input to it.

### Round trip

`TestSnapshotAndRestoreSmallStream` (3 MiB),
`TestNestedObjectNameRoundTrips` (a 1 MiB object called
`/runs/2026/db.dump`) and `TestRcloneObjectStreamsStraightIntoTheRepository`
(8 MiB through a real rclone `local` Fs, a real `fs.Object`, and the
`io.ReadCloser` its `Open` returns) all assert SHA-256 equality between what
went in and what came back.

## Consequences

### What this settles

Kopia does not need a seekable source. The narrowest thing rclone can offer
-- an `io.ReadCloser` -- is exactly what `fs.StreamingFile` asks for, and
`rcloneSource` is four lines of glue with nothing invented in it. Whatever
else the epic decides, it does not have to decide it around a fake `Seek`.

### What a stream costs, stated plainly

**No resume.** A stream that breaks is re-opened from byte zero. There is no
partial-file recovery, because there is no seek to recover to.
`StreamSnapshotRequest.MaxAttempts` (default 3, the same shape
`internal/transport/retry` uses; a negative value is refused rather than
quietly meaning "never try") bounds that, and
`TestReadErrorIsBoundedAndSavesNothing` pins it: a
stream that always breaks is opened exactly `MaxAttempts` times, every
reader is closed, the error reaches the caller, and no snapshot exists
afterwards. For a multi-GB source over a flaky link, "retry" means "read it
all again", and that is a real operational cost this ADR is not going to
dress up. Kopia's own answer to it is mid-upload checkpointing
(`DefaultCheckpointInterval`, 45 minutes), which this spike leaves switched
off and which a later phase should look at before treating the cost as settled.

**No size up front.** Progress reporting over a streamed source cannot show
a percentage until it is done. Kopia's estimator walks a source to size it;
a stream has nothing to walk.

**Orphan blobs after a failed attempt.** An attempt runs inside one
`repo.WriteSession`; returning an error closes it without flushing, so no
manifest is saved and a torn upload is not reachable as a snapshot. But a
long attempt that filled a pack blob before it broke has already written
that blob. It is unreferenced garbage that Kopia's maintenance collects, not
a torn snapshot -- but it is disk in the meantime, and Phase 1 will need
maintenance scheduled for reasons this is only one of.

### Cancellation needed work Kopia does not do

`Uploader.Cancel` sets a flag that `copyWithProgress` checks BETWEEN reads.
A reader parked on a remote socket is not reachable from a flag, so
cancelling a snapshot of a stalled remote would have hung until the
transport timed out on its own.

`guardedReader` and `openStreams` close that. Every `Read` is refused once
the context is done, and `openStreams.closeAll` closes the reader from
outside to unblock one already in flight. Because a read torn down that way
reports whatever the transport felt like reporting, an error raised while
the context is done is reported AS the context's error: a caller that
cancelled sees `context.Canceled`, not `use of closed network connection`.
`TestCancellationClosesTheRemoteReader` asserts that every reader opened was
closed, that no snapshot was left behind, and that the goroutine count
settles back to where it was.

This is the one place where "Kopia handles it" was not true, and it is small
and it is in the adapter, which is where it belongs.

### The boundary this keeps

`core/internal/backupengine/kopia` is the only package in this repository
that imports Kopia, exactly as `core/internal/transport/rclone` is the only
one that imports rclone, and for the reason FR-3 gives: upstream churn stays
in one adapter. The streaming path is inside that adapter with the rest of
it; what the manager-owned package declares is the capability, in its own
types.

What crosses the edge is `StreamSource` (a modtime and an `Open` returning
`io.ReadCloser`), `StreamSnapshotRequest`, `StreamSnapshotInfo`,
`SourceStreamer`, `SnapshotID` and `error`. No Kopia type appears in any of
them, and `TestNoVendorImportOutsideTheAdapter` plus
`TestOneEngineAndOneRepositoryDeclaration` (ADR 0006) are what keep that
true rather than aspirational -- the first version of this spike failed both.

`StreamSource` deliberately has no `Size`, no `Seek` and no `Name`. The first
two absences are the whole finding expressed as an interface: a streaming
backup source cannot honestly offer either, and it turns out it does not have
to. The third is the naming bug's fix: identity belongs to the request, not
to the thing that produces bytes.

## Alternatives considered

### Implement `fs.File` and fake `Seek` by reopening

Rejected, and this is the alternative this spike existed to rule out. It is
plausible on a first read -- Kopia's uploader mostly seeks forward -- and it
is wrong in three ways at once. It turns one sequential read into an
unbounded number of remote opens, each with its own handshake; on an SFTP
source that is the connection-cap problem this project already fixed once, arriving
by a new door. It makes the cost of a backup depend on Kopia's internal
access pattern, which is not a contract and can change in a patch release.
And it hides all of that behind an interface that says "seek", so nothing
about a run that took forty minutes instead of four would point at it.

The spike never needed it, so the argument is now academic, which is the
best outcome available.

### Stage to a local file and use `localfs`

Rejected. It is what `CopyToLocal` already does, it is the thing the epic exists
to stop doing, and it needs room for the source. Recorded here because it is
the fallback if a future Kopia drops the streaming path -- which is a real
risk worth naming: `fs.StreamingFile` has fewer users than `fs.File`, and
this ADR's whole claim rests on it.

### `virtualfs.StreamingFileFromReader`

Rejected in favour of implementing the interface, as described under
Decision: it opens eagerly and leaves no place for cancellation to reach a
blocked reader. This is a small, deliberate divergence from Kopia's own
convenience constructor and the reason is written down here so a future
reader does not "simplify" it back.
