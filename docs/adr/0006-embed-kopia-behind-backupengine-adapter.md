# 0006. Embed Kopia behind a backupengine adapter rather than shell out to its CLI

## Status

Accepted, as the Phase 0 feasibility gate for the embedded backup engine.

## Context

Up to now this project has moved whole backup artifacts: a completed dump
appears on a remote host, we pull it over SFTP, verify it, commit it durably
on the NAS, and only then delete the remote copy. That works, and it is
expensive in exactly one predictable way. Every run copies the whole artifact
again, whether or not anything inside it changed, and retention keeps N whole
copies of a mostly-identical tree.

The fix is an incremental, deduplicated, content-addressed repository: store
content once, address it by hash, and let the second backup of a
mostly-unchanged tree cost the changed part plus some metadata. We do not
want to write that. Chunking, content-addressed storage, index management,
encryption, garbage collection under concurrent writers, and repository
maintenance are a multi-year problem that Kopia already solves, with a
maintenance story and a user base.

The decision is not "should we use Kopia". It is how we consume it, and the
options are the same three ADR 0001 faced for rclone: fork it, embed it as a
library behind our own interface, or shell out to the `kopia` CLI. ADR 0001
picked embedding for rclone, wrote down what that cost, and named subprocess
invocation as the fallback if embedding failed. This decision is the same
shape, which is both convenient and a trap: the reasons are similar but the
numbers are not, and "we already did this once" is not evidence.

So Phase 0 required a spike: real code, running, proving the full lifecycle
in process, with measurements. This ADR records what it proved.

## Decision

We embed Kopia `v0.23.1` and quarantine every Kopia import behind one
package, `core/internal/backupengine/kopia`, which implements a
manager-owned `backupengine.Engine` / `backupengine.Repository` pair declared
in `core/internal/backupengine/engine.go`. Nothing outside that adapter may
import Kopia. Lifecycle, retention, catalog and CLI code only ever see our
own `RepositoryLocation`, `Source`, `SnapshotID`, `SnapshotInfo`,
`VerifyReport`, `RestoreReport` and `MaintenanceReport`, never Kopia's.

Streaming sources are part of that same boundary and not a second one. A
source whose bytes can only be read once, forward — the case ADR 0007
measures — is carried as a capability on this port: `StreamSource`
(`ModTime` plus `Open(ctx) (io.ReadCloser, error)`) and
`StreamingRepository` (`SnapshotStream`, `OpenSnapshotStream`), declared in
`engine.go` in our own types, implemented in `kopia/stream.go` with
everything that knows the vendor's name. The streaming spike originally
arrived as a second, Kopia-importing `Engine` in the manager-owned package;
that would have been two boundaries rather than one, so it was folded in
behind this port instead, which cost `engine.go` four declarations and no
imports.

We register exactly one storage backend, `repo/blob/filesystem`, not Kopia's
full backend catalog. A repository on a mounted NAS share is a filesystem
repository, which is the case this product has.

Four properties are enforced by tests rather than by convention, because all
four are the kind of rule that decays quietly:

- `engine.go` contains no occurrence of the string `kopia`. A Kopia type
  cannot appear in a signature without its package qualifier appearing in the
  file, so this one grep is a complete check of "no upstream type crosses the
  boundary". `internal/backupengine/boundary_test.go` performs it.
- No file anywhere else in the module imports `github.com/kopia/kopia`,
  including test files. One clean file stopped being the interesting property
  the moment this package held more than one, and a helper in
  `internal/lifecycle` reaching around the adapter would leave `engine.go`
  spotless and the quarantine gone. `TestNoVendorImportOutsideTheAdapter`
  walks the module and also asserts the adapter *does* import Kopia, so it
  cannot pass vacuously.
- `Engine` and `Repository` are each declared exactly once under
  `internal/backupengine`, in `engine.go`. Two declarations is not a merge
  conflict, it is two ports: callers pick one, the second accumulates its own
  semantics, and the quarantine stops meaning anything because there are two
  doors. `TestOneEngineAndOneRepositoryDeclaration` makes the return of the
  second one a failing build rather than an argument.
- The adapter package references no process-spawning API at all (`os/exec`,
  `exec.Command`, `exec.CommandContext`, `syscall.Exec`, `StartProcess`),
  including in its own test files.
  `internal/backupengine/kopia/lifecycle_test.go`'s `TestNoSubprocess`
  performs it. "In process" is not observable from a passing lifecycle test,
  because a wrapper that shelled out to an installed CLI would pass every
  assertion; what makes it checkable is that the package contains no way to
  start a process.

The version is pinned exactly in `core/go.mod`. Bumping it is a project
event, not a background update, under the same regime `docs/rclone-upgrade.md`
imposes on rclone.

## What the spike proved

`core/internal/backupengine/kopia/lifecycle_test.go` drives the whole
sequence in one process against a filesystem repository in a temp dir:

create repository -> refuse to create over it -> refuse a wrong passphrase ->
refuse an empty location -> open -> snapshot -> **second, incremental
snapshot** -> list -> verify -> restore -> compare restored bytes against
source SHA-256 -> delete the first snapshot -> refuse to delete it twice ->
full maintenance -> **verify the surviving snapshot again**.

The sequence is the claim. A snapshot that cannot be verified, restored,
deleted and then garbage collected without damaging its surviving sibling is
not a backup, and the last step is the one that matters most: full
maintenance is the operation that actually deletes bytes, and the test
asserts the surviving snapshot still reads back every byte afterwards.

Incrementality is proved twice, from two directions that cannot both be
fooled by the same mistake:

- From Kopia's own accounting: with one small file rewritten and one small
  file added to a five-file tree, the second snapshot reports 3 files reused
  and 2 files read.
- From the filesystem, which cannot be talked into agreeing: the repository
  directory grew **8,399,021 bytes** for the first snapshot of an 8 MiB
  incompressible file and **9,049 bytes** for the second. That is
  **0.11%**, or roughly 1/928th of the first snapshot's physical cost.

The second half of the spike is the streaming capability, in
`internal/backupengine/kopia/stream_test.go`: a source that can only be read
once, forward, round-trips byte-exactly, a 2 GiB stream is stored with a peak
live heap of 68.4 MiB and nothing staged outside the repository, a cancelled
stream closes its reader and saves no snapshot, and an object named
`/runs/2026/db.dump` restores. ADR 0007 records those measurements and what
a stream costs; the reason it is one PR with this one is that a second
`Engine` would have been a second boundary.

## Measurements

Host: Apple M5, darwin/arm64, Go 1.27.0, `CGO_ENABLED=0 -trimpath`.
Baseline is `origin/main` at f65a87ef.

### Binary size

`core/cmd/backupd` does not import the adapter yet, so the dependency on its
own costs nothing. The row that matters is the linked one, measured by
temporarily blank-importing the adapter into `cmd/backupd`, which is what
Phase 1 will do for real.

| Target | Baseline | Dep in go.mod, adapter not linked | Adapter linked | Delta when linked |
| --- | --- | --- | --- | --- |
| darwin/arm64 | 47,116,210 | 47,116,210 | 54,459,922 | +7,343,712 (+15.6%) |
| linux/arm64 | 45,208,272 | 45,208,272 | 52,238,146 | +7,029,874 (+15.5%) |

Adding the requirement to `go.mod` moves the binary by zero bytes, because
the Go linker keeps only what is reachable from `main`. The real cost is
+7.0 MiB, arriving the moment Phase 1 wires the adapter in. This is the
"module graph weight, linked size and registered backend count are three
different numbers" lesson from ADR 0001, applied deliberately this time.

### Dependency count

Measured on the tidied graph: `go mod tidy -diff` is a no-op on this branch,
which matters because an untidied one flatters nobody — the streaming spike
that is now folded in here carried **705** modules and 197 requirement lines
before `tidy` removed 175 of them, mostly GCP monitoring/storage and OTLP
requirements for code nothing in this repository calls. The numbers below are
what the committed `core/go.mod` and `core/go.sum` actually produce, re-read
with `go list -m all` on Go 1.27.1.

| Measure | Baseline | After | Delta |
| --- | --- | --- | --- |
| `go list -m all` (build list) | 334 | 400 | +66 modules |
| module requirements in `core/go.mod` | 102 | 126 | +24 lines |
| module paths added / dropped | — | — | +68 / -2 |
| pre-existing versions changed by MVS | — | — | 5 |
| Kopia's own source in the module cache | — | 9 MiB | — |

The five pre-existing dependencies minimal version selection moved are
`github.com/coreos/go-systemd/v22` (v22.6.0 -> v22.7.0), `github.com/fatih/color`
(v1.16.0 -> v1.19.0), `github.com/godbus/dbus/v5` (v5.1.0 -> v5.2.2),
`github.com/golang/protobuf` (v1.5.0 -> v1.5.4) and `google.golang.org/api`
(v0.279.0 -> v0.283.0). Two module paths left the build list entirely
(`github.com/creack/pty`, `github.com/kr/pty`), which is MVS reshuffling
test-only dependencies of other modules, not us dropping anything.

Folding the streaming capability in adds **zero** modules: it uses `fs`,
`fs/virtualfs` and `snapshot/upload` from the module already pinned here, so
the streaming finding costs dependency graph nothing on top of this table.

66 modules is a smaller graph shock than rclone's was, and it is the same
*kind* of cost: cloud SDKs for backends we do not register, arriving because
Kopia's module graph does not care which of its backends we import. They are
CVE surface to monitor, not bytes in the binary.

### Resident memory

Same binary in all rows. `idle RSS` is `ps -o rss=` ten seconds after the
labelled point, with a `runtime.GC()` first. `peak` is `Maxrss` from
`getrusage(RUSAGE_SELF)`.

| Process state | peak | idle RSS | Go heap in use |
| --- | --- | --- | --- |
| linked, never touches a repository | 19.5 MB | 13.3 MB | 1.6 MB |
| after `CreateRepository` | 87.3 MB | — | 69.0 MB |
| **repository open, idle** | **156.0 MB** | **150.4 MB** | **10.5 MB** |
| open + one 8 MiB snapshot, idle | 166.1 MB | 155.8 MB | 37.5 MB |
| repository open, idle, after `debug.FreeOSMemory()` | 156.0 MB | **18.1 MB** | 10.5 MB |

This is the one measurement that surprised us and the one Phase 1 has to act
on. Opening a repository costs about 137 MB of resident set, while the Go
heap in use afterwards is 10.5 MB. The gap is not a leak: Kopia derives the
repository key with scrypt, whose working set is tens of megabytes, and the
Go runtime returns those freed pages to the OS lazily (`MADV_FREE` on
darwin), so they stay counted in RSS until something needs them. One
`debug.FreeOSMemory()` call after open drops idle RSS from 150.4 MB to
18.1 MB, which confirms the diagnosis rather than merely being consistent
with it.

For a daemon on a memory-constrained NAS the difference between 150 MB and
18 MB of apparently-resident idle footprint is the difference between fitting
and not fitting, so Phase 1 owns an explicit reclaim after repository open.
`GODEBUG=madvdontneed=1` does not help on darwin; the Linux/arm64 number
needs its own measurement on the target device, because the page-return
behaviour is exactly the part that is OS-specific.

## Alternatives considered

### Subprocess: shell out to the `kopia` CLI

This is a stronger option here than it was for rclone, and it deserves more
than a reflexive rejection, because Kopia's CLI has `--json` on the commands
that matter and its snapshot/restore/verify output is genuinely
machine-readable. It was rejected for four reasons that the spike makes
concrete rather than theoretical:

- **The safety-critical operation is a delete.** `DeleteSnapshot` followed by
  full maintenance is what reclaims space, and full maintenance is
  irreversible. Doing that through argv means no compile-time check that we
  passed the snapshot ID we think we passed, for the one operation whose
  failure mode is destroying the last good backup.
- **Verification is the product.** The spike's verify step reads every byte
  back and reports per-object findings as a list, with a nil error meaning
  "ran and found nothing". Through a CLI that becomes exit-code plus text
  parsing, and "verification could not run" and "verification ran and found
  damage" are the two states we must never confuse. They are different
  return values in Go and adjacent exit codes in a shell.
- **A second binary on the NAS.** A `kopia` executable on PATH is version
  skew we cannot pin, on a device we do not fully control, for a repository
  format where the writer's version matters.
- **`context.Context` all the way down.** Snapshot and restore are long
  operations that must cancel cleanly. Embedded, cancellation is the same
  `ctx` as everything else in this codebase; as a subprocess it is signal
  management plus reaping plus deciding what a half-written snapshot means.

As with ADR 0001, subprocess is not ruled out forever. It remains the named
fallback if a future Kopia release makes the Go API untenable.

### Fork Kopia

Rejected for the reason ADR 0001 gives and this decision does not improve on:
a fork relocates maintenance onto us permanently, forever, for functionality
that is mostly not ours. Kopia is larger and more actively developed than the
slice of rclone we use. The backup-policy logic that is genuinely ours is
small. Maintaining something enormous to protect something small is backwards.

This stays off the table regardless of how future upgrades go. A failed
upgrade is evidence that the Go API surface is too unstable to build on,
which argues for a harder boundary (the CLI's stable contract), not for
taking on a fork to route around the instability.

### Write our own incremental engine

Not seriously considered, and worth saying so. Content-addressed storage with
concurrent-safe garbage collection is a correctness problem where the bugs
are silent and the symptom is an unrestorable backup discovered during a
recovery. We have no business being the third-newest implementation of it.

## Upgrade risk

This is the part to read before bumping the pin.

- **The Go API is not Kopia's stable contract; the CLI is.** Nothing upstream
  promises that `repo`, `snapshot`, `snapshot/upload`, `snapshot/restore` or
  `snapshot/snapshotfs` keep their shape across releases. We depend on 14
  Kopia packages; the exact list and what each is used for is in the adapter's
  imports and must be re-read on every bump.
- **It has already moved once, in the most load-bearing place.** The uploader
  is `snapshot/upload.NewUploader` in v0.23.1. In earlier releases it was
  `snapshotfs.NewUploader`. The single most important call in the adapter has
  already changed packages in Kopia's recent history, which is the clearest
  available evidence about how stable this surface is.
- **Two defaults are traps whose zero value is wrong.**
  `restore.Options.RestoreDirEntryAtDepth` defaults to 0, which means "write
  `.kopia-entry` placeholder stubs instead of files" — a restore that
  satisfies every statistic and contains none of the data. The spike hit this
  exactly once, and it was caught only because the test compares restored
  bytes against source hashes rather than trusting the restore's own report.
  `snapshotfs.VerifierOptions.MaxErrors` defaults to 1, turning a
  verification report into "the first thing that is wrong". Both are set
  explicitly in the adapter with comments saying why. A future release
  changing either default silently changes what our restore and our verify
  mean, and only a byte-comparing test notices.
- **Content reuse trusts source metadata, not content.** The uploader reuses
  a previous entry when name, size, mtime, mode and owner match; it does not
  re-read the file. A file rewritten in place with size and mtime preserved is
  reused unread. That is not a Kopia bug and the adapter cannot fix it from
  below; it is the subject of the source-consistency work, and any change to
  `findCachedEntry`/`metadataEquals` upstream changes our incrementality
  guarantees.
- **Maintenance is forced past Kopia's ownership check.** The adapter calls
  `snapshotmaintenance.Run` with `force=true`, because deferring to an owner
  string written by whichever machine created the repository would mean a
  NAS-side maintenance window that silently never runs. It uses
  `maintenance.SafetyFull`, not `SafetyNone`: full safety keeps
  recently-written content out of garbage collection, which is what makes
  maintenance safe to run concurrently with a snapshot. `SafetyNone` exists,
  is faster, and must stay unused, because the thing it trades away is the
  only property that matters.
- **Repository format defaults are inherited, not chosen.**
  `repo.NewRepositoryOptions` is left at its defaults, so hash, encryption,
  splitter and format version are whatever the pinned version recommends at
  creation time. That is deliberate: "it was the default when this repository
  was created" ages better than a stale opinion pinned in our code. It does
  mean a Kopia release that changes defaults produces repositories with
  different parameters than earlier ones, and both must stay readable.
- **`Ran` is read back, not inferred.** `snapshotmaintenance.Run` returns nil
  both when it did work and when it declined (nothing due, or another process
  holds the lock). `MaintenanceReport.Ran` is computed by comparing the
  repository's own recorded maintenance-run count before and after, because an
  operator needs to distinguish "maintenance ran" from "maintenance quietly
  did not".
- **A streamed source is never reused on metadata, on purpose.** A streaming
  entry reports no size, so Kopia's `findCachedEntry` degrades to comparing
  mode and modification time and would skip the read entirely if previous
  manifests were passed to `Upload`. `SnapshotStream` passes none: a source
  rewritten in place with its mtime preserved would otherwise have its new
  content skipped silently, and the snapshot would claim to hold bytes it
  never read. `TestChangedContentWithPreservedModTimeIsReRead` is the guard,
  and an upstream change to `findCachedEntry`/`commonMetadataEquals` cannot
  weaken it without failing that test. The cost is real and stated in
  ADR 0007: every streamed run re-reads every byte, and dedupe happens on
  content instead.
- **A verification that did not finish is not a verification.** Kopia's tree
  walker records a cancelled read as a per-object finding *and* returns an
  error, so deciding "did it run" by counting findings reports a cancelled
  verify as a completed one that found damage. `Verify` therefore returns the
  operational error alongside the partial report, and a nil error means
  exactly "completed and found nothing". Both halves are pinned, by
  `TestVerifyReportsCancellationNotJustFindings` and
  `TestVerifyReportsDamageAsBothErrorAndFindings`, because a future upstream
  change to where that error surfaces would otherwise quietly restore the
  masking.
- **`OpenRepository` connects the storage it was asked for, every time.** The
  config file records which storage it belongs to, so reusing an existing one
  because it happens to be present hands back a repository the caller did not
  ask for and writes snapshots into it successfully. `Connect` is
  unconditional and the opened repository's own connection info is compared
  against the requested path afterwards, because "we wrote the right config"
  and "we are talking to the right storage" are different claims.
- **Five pre-existing dependencies moved.** Minimal version selection moved
  `go-systemd`, `fatih/color`, `godbus/dbus`, `golang/protobuf` and
  `google.golang.org/api` (versions in the dependency table above). Every bump
  of this pin can move unrelated versions the same way, and the diff to check
  is `core/go.mod`, not just the Kopia line.

The spike's lifecycle test is retained, unchanged in intent, as the
compatibility smoke test for all of the above. It is not product code and it
is not a unit test: it is the thing that fails when an upgrade breaks one of
these assumptions, which is the only reason it survives the spike.

## What Phase 1 owes this boundary

Three shapes are missing from the port on purpose. Each was identified while
consolidating this spike, each is a design decision rather than an omission,
and none is built here: the Phase 0 gate is a feasibility answer, and
building Phase 1 shapes before the gate merges is how a spike quietly becomes
the architecture. They are recorded here so they arrive as requirements on
the Phase 1 work rather than as rediscoveries.

- **A source data-plane slot on `SnapshotRequest`.** Today `Snapshot` takes a
  `Source` whose `Path` the adapter opens with `localfs`, and streaming lives
  on a separate capability because a stream is not a path. Phase 1 wants one
  request that carries *how the bytes are produced* — a stream, a directory
  enumerator, or a per-entry decision provider — so that "what to back up"
  and "how to read it" stop being the same field. The current pair
  (`SnapshotRequest` + `StreamSnapshotRequest`) is honest for two cases and
  does not generalise to three.
- **Backup-set identity instead of the engine's host/user/path tuple.**
  `Source{Host, User, Path}` is Kopia's own snapshot identity wearing our
  field names, and it is the thing snapshot lineage is keyed on: two runs are
  "the same source" iff all three match. That makes a renamed path or a
  changed hostname look like a brand new source with no history, and it makes
  one backup set spanning two locations impossible to express. Phase 1 needs
  a backup-set identity that we own and that survives both, with the engine
  tuple derived from it inside the adapter.
- **`Decision.Action` threaded through the port.** The source-consistency work
  produces a per-entry decision (back up, skip, quarantine) that this port has
  no way to accept, so the engine currently re-derives what to include from
  its own policy tree. Phase 1 should pass those decisions in rather than
  duplicating the logic on both sides of the boundary, which is the only way
  the two can be made to agree by construction instead of by review.

## Consequences

### What we get

- One deployable binary. No `kopia` executable on the NAS, no PATH
  dependency, no version skew between a system Kopia and what we tested.
- Incremental storage with real numbers behind it: 0.11% of the first
  snapshot's physical cost for a second snapshot of a mostly-unchanged tree.
- Typed Go errors across the boundary (`ErrRepositoryExists`,
  `ErrRepositoryNotFound`, `ErrPassphrase`, `ErrSnapshotNotFound`), matched
  upstream by `errors.Is` against real sentinel values, never by message text,
  for the reason `internal/transport/rclone/errors.go` explains at length.
- Real `context.Context` cancellation through snapshot, restore, verify and
  maintenance, including the case Kopia cannot reach on its own: a reader
  blocked in `Read` on a stalled remote (ADR 0007).
- Sources that cannot be staged, statted or seeked, stored at a bounded
  memory cost, behind the same port and with no vendor type in sight.
- A narrow, auditable, test-enforced boundary: one package imports Kopia, a
  module-wide scan proves no import escapes it, one test proves there is only
  one `Engine`, and one test proves no subprocess exists.

### What actually hurts

- **+7.0 MiB of binary, the moment Phase 1 links it.** Not negotiable and not
  visible yet, which is the dangerous combination; it is written down here so
  it is not discovered as a surprise in a release note.
- **+66 modules of dependency graph** for one registered backend, including
  cloud SDKs we will never call. Same category of cost as rclone's, smaller
  in magnitude, identical in kind.
- **150 MB of idle resident set unless we reclaim explicitly.** The cause is
  understood and the fix is one line, but it is a line Phase 1 must actually
  write, and the Linux number is still unmeasured.
- **We now embed two large third-party data planes.** rclone moves bytes,
  Kopia stores them, and both are pinned, both gate upgrades on human review,
  and both are one adapter away from the rest of the codebase. That is twice
  the upgrade ceremony and twice the CVE monitoring, permanently.
- **`repo.Connect` writes a config file we have to own.** Kopia's natural
  habitat is a per-user config in a well-known location; a process-wide
  default would be shared mutable state between unrelated backupd
  invocations. `RepositoryLocation.ConfigPath` and `CachePath` exist so the
  placement is ours, and that is one more thing to get right in deployment
  rather than something upstream handles.

### A permanent local patch to Kopia needs its own ADR

Same position as ADR 0001 takes for rclone. This decision assumes we consume
Kopia unmodified. If we find an API gap the adapter cannot work around, the
preference is to contribute upstream. Carrying a local patch indefinitely
creates the same continuous-integration-with-upstream obligation that "why
not fork" rejects, scoped to one patch instead of the whole tree, and it
needs its own ADR rather than being waved through as an implementation
detail of this one.
