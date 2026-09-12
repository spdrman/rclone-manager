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

We register exactly one storage backend, `repo/blob/filesystem`, not Kopia's
full backend catalog. A repository on a mounted NAS share is a filesystem
repository, which is the case this product has.

Two properties are enforced by tests rather than by convention, because both
are the kind of rule that decays quietly:

- `engine.go` contains no occurrence of the string `kopia`. A Kopia type
  cannot appear in a signature without its package qualifier appearing in the
  file, so this one grep is a complete check of "no upstream type crosses the
  boundary". `internal/backupengine/boundary_test.go` performs it.
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

| Measure | Baseline | After | Delta |
| --- | --- | --- | --- |
| `go list -m all` (build list) | 338 | 400 | +62 modules |
| module requirements in `core/go.mod` | — | — | +26 lines |
| pre-existing versions changed by MVS | — | — | 1 |
| Kopia's own source in the module cache | — | 9 MiB | — |

The one pre-existing dependency minimal version selection moved is
`github.com/coreos/go-systemd/v22`, v22.6.0 -> v22.7.0. Nothing else that was
already there changed version.

+62 modules is a smaller graph shock than rclone's was, and it is the same
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
- **One pre-existing dependency moved.** `github.com/coreos/go-systemd/v22`
  went v22.6.0 -> v22.7.0 by minimal version selection. Every bump of this pin
  can move unrelated versions the same way, and the diff to check is
  `core/go.mod`, not just the Kopia line.

The spike's lifecycle test is retained, unchanged in intent, as the
compatibility smoke test for all of the above. It is not product code and it
is not a unit test: it is the thing that fails when an upgrade breaks one of
these assumptions, which is the only reason it survives the spike.

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
  maintenance.
- A narrow, auditable, test-enforced boundary: one package imports Kopia, one
  grep proves no Kopia type escapes, one test proves no subprocess exists.

### What actually hurts

- **+7.0 MiB of binary, the moment Phase 1 links it.** Not negotiable and not
  visible yet, which is the dangerous combination; it is written down here so
  it is not discovered as a surprise in a release note.
- **+62 modules of dependency graph** for one registered backend, including
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
