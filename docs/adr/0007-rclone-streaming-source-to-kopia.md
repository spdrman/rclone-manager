# ADR 0007: An rclone stream reaches Kopia as a streaming file, with no staging and no fake Seek

## Status

Accepted as a feasibility finding (#791, phase 0 of EPIC #779). The code it
describes is a spike kept as a compatibility smoke test, not a shipped
feature: `core/internal/backupengine` is not wired into the service, the
CLI, the API or the catalog, and nothing outside its own tests calls it.

The Kopia pin it was measured against, `github.com/kopia/kopia@v0.23.1`, was
chosen by #790. Whether to embed Kopia at all is EPIC #779's decision, taken
with this ADR, #790's dependency and binary-size measurements and #792's and
#793's findings in front of it. This ADR answers exactly one of the
questions on that list.

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
before any of the rest of #779 was decided, whether that bridge is needed at
all.

## Decision

**An rclone stream is given to Kopia as an `fs.StreamingFile`. Nothing is
staged, nothing is buffered whole, and no `Seek` is implemented, faked or
required.**

### The exact Kopia API used

All of it is in `core/internal/backupengine`, against v0.23.1:

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

The engine implements `fs.StreamingFile` itself rather than using
`virtualfs.StreamingFileFromReader`, for two reasons that both turned out to
matter. `virtualfs` takes the reader in its constructor, so the remote would
be opened before Kopia decided it wanted it; and it wraps nothing, so there
is no place to make cancellation reach a reader that is blocked in `Read`.
`streamingEntry` opens lazily inside `GetReader` and wraps what it gets in
`guardedReader`.

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
place). Reproduce with `go test ./internal/backupengine/` from `core/`.

### Bounded memory on a multi-GB stream

`TestLargeStreamIsBoundedInMemory` generates 2 GiB from a deterministic
xorshift reader (nothing multi-GB is committed, and the stream never exists
in full anywhere), snapshots it, and samples `runtime.MemStats.HeapAlloc`
throughout:

```
streamed       2,147,483,648 bytes  (2.00 GiB)
peak heap         71,834,056 bytes  (68.5 MiB)  = 3.35% of the stream
                  71,967,488 on a second run    = 3.35%, sampler jitter
repository     2,147,750,025 bytes              = +0.012% over the payload
largest blob      28,942,705 bytes  (27.6 MiB)  = 1.35% of the stream
```

Peak live heap is a working set, not a function of file size. The test fails
if peak heap reaches either 256 MiB or one eighth of the stream, so an
implementation that started buffering would be caught long before it
buffered the file.

The repository overhead of +0.012% on incompressible data is the other half
of the same statement: the bytes were chunked and packed as they went past.
The largest single file anywhere under the repository is a 27.6 MiB pack
blob, against a 2 GiB source; the test's ceiling is 64 MiB.

No throughput figure is quoted. The same test took 16.7 s and 57.4 s on two
runs of this machine while other work was running on it, which is an honest
statement about a contended laptop and no statement at all about Kopia.
Phase 0 asked whether memory is bounded, and that is what is measured here.

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

The second run still READ all 67,108,864 bytes -- `Snapshot.Bytes` is
asserted to be the whole payload -- and stored none of them again. Both
snapshots restore to the payload's SHA-256.

That is content-level dedupe, reached deliberately. Kopia would also have
skipped the read entirely: `findCachedEntry`
(`snapshot/upload/upload.go:727`) takes a `commonMetadataEquals` branch for
`fs.StreamingFile` and will reuse a previous entry on matching mode and
modtime alone, if previous manifests are passed to `Upload`. This engine
passes none. A metadata match is a statement of trust in a remote's clock,
not a measurement, and phase 0 is here to measure. **If #779 later wants the
cheap path, that is a policy decision about trusting source metadata and it
needs its own argument** -- and #792's mtime-precision findings are the
input to it.

### Round trip

`TestSnapshotAndRestoreSmallStream` (3 MiB) and
`TestRcloneObjectStreamsStraightIntoKopia` (8 MiB through a real rclone
`local` Fs, a real `fs.Object`, and the `io.ReadCloser` its `Open` returns)
both assert SHA-256 equality between what went in and what came back.

## Consequences

### What this settles

Kopia does not need a seekable source. The narrowest thing rclone can offer
-- an `io.ReadCloser` -- is exactly what `fs.StreamingFile` asks for, and
`rcloneSource` is four lines of glue with nothing invented in it. Whatever
else #779 decides, it does not have to decide it around a fake `Seek`.

### What a stream costs, stated plainly

**No resume.** A stream that breaks is re-opened from byte zero. There is no
partial-file recovery, because there is no seek to recover to.
`Config.MaxAttempts` (default 3, the same shape `internal/transport/retry`
uses) bounds that, and `TestReadErrorIsBoundedAndSavesNothing` pins it: a
stream that always breaks is opened exactly `MaxAttempts` times, every
reader is closed, the error reaches the caller, and no snapshot exists
afterwards. For a multi-GB source over a flaky link, "retry" means "read it
all again", and that is a real operational cost this ADR is not going to
dress up. Kopia's own answer to it is mid-upload checkpointing
(`DefaultCheckpointInterval`, 45 minutes), which this spike leaves switched
off and which #779 should look at before treating the cost as settled.

**No size up front.** Progress reporting over a streamed source cannot show
a percentage until it is done. Kopia's estimator walks a source to size it;
a stream has nothing to walk.

**Orphan blobs after a failed attempt.** An attempt runs inside one
`repo.WriteSession`; returning an error closes it without flushing, so no
manifest is saved and a torn upload is not reachable as a snapshot. But a
long attempt that filled a pack blob before it broke has already written
that blob. It is unreferenced garbage that Kopia's maintenance collects, not
a torn snapshot -- but it is disk in the meantime, and #779 will need
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

`core/internal/backupengine` is the only package in this repository that
imports Kopia, exactly as `core/internal/transport/rclone` is the only one
that imports rclone, and for the reason FR-3 gives: upstream churn stays in
one adapter. No Kopia type appears in any exported signature. What crosses
the edge is `StreamSource` (a name, a modtime, and an `Open` returning
`io.ReadCloser`), `Config`, `Snapshot`, and `error`.

`StreamSource` deliberately has no `Size` and no `Seek`. That is the whole
finding expressed as an interface: a streaming backup source cannot honestly
offer either, and it turns out it does not have to.

## Alternatives considered

### Implement `fs.File` and fake `Seek` by reopening

Rejected, and this is the alternative #791 existed to rule out. It is
plausible on a first read -- Kopia's uploader mostly seeks forward -- and it
is wrong in three ways at once. It turns one sequential read into an
unbounded number of remote opens, each with its own handshake; on an SFTP
source that is the connection-cap problem #264 already fixed once, arriving
by a new door. It makes the cost of a backup depend on Kopia's internal
access pattern, which is not a contract and can change in a patch release.
And it hides all of that behind an interface that says "seek", so nothing
about a run that took forty minutes instead of four would point at it.

The spike never needed it, so the argument is now academic, which is the
best outcome available.

### Stage to a local file and use `localfs`

Rejected. It is what `CopyToLocal` already does, it is the thing #779 exists
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
