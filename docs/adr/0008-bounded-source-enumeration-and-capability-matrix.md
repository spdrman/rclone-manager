# ADR 0008: Bounded source enumeration, and a capability matrix that refuses what it cannot bound

## Status

Accepted and implemented (issue #792, EPIC #779 Phase 0). This is a
feasibility gate, not a product feature: the enumerator and the matrix
ship, nothing in the daemon calls them yet, and the harnesses that prove
them stay as compatibility smoke tests. The FR-3 doctrine of ADR 0001
("the answer to an upstream API shape is not a fork") is the constraint
this decision was taken under, and option A below is where that bites.

## Context

EPIC #779 wants Kopia as an in-process incremental backup engine, fed by
sources this product already reads over rclone. Phase 0 asks four
questions that would each sink the epic if the answer were no. This is
the third: **what does this process do when a source holds one directory
with a million entries in it?**

Today it does the worst available thing, quietly.

`Transport.List` returns `[]transport.RemoteArtifact`, so every entry
under a source's root is live at once — and, on the way there, so is
rclone's own `fs.Objects` slice for the same set, because
`Adapter.List` calls `walk.GetAll` and then converts. Then it sorts. Cost
is linear in entry count, there is no ceiling anywhere in the path, and
nothing tells a caller how big the answer will be before it is built.

Measured on this machine (Apple M5, APFS, Go 1.27, one process), against a
real flat directory:

| entries   | path                   | peak heap | total    | time to first entry | peak goroutines |
| --------- | ---------------------- | --------- | -------- | ------------------- | --------------- |
| 100,000   | `Adapter.List`         | 44.4 MiB  | 320 ms   | 320 ms              | 4               |
| 100,000   | bounded enumerator     | 3.5 MiB   | 214 ms   | 1.974 ms            | 3               |
| 1,000,000 | `Adapter.List`         | 493.4 MiB | 102.45 s | 102.45 s            | 4               |
| 1,000,000 | bounded enumerator     | 4.2 MiB   | 72.67 s  | 12.26 ms            | 3               |

(`TestStreamingCostsFarLessThanListAtScale` in
`core/internal/transport/rclone`; the million-entry row is the same test
with `BACKUPD_HUGE_DIR_ENTRIES=1000000`, which also costs 3m34s of
fixture creation and is why CI does not run it. Peak goroutines includes
the measuring goroutine itself, so the two columns are comparable to each
other rather than absolute; the enumerator starts none of its own. Wall
times vary run to run with the page cache — an earlier 100,000-entry run
on a colder cache measured 2.213 s for List against 1.724 s streaming,
with peak heaps of 51.6 MiB and 4.7 MiB. The heap figures are the stable
ones, and they are what this decision turns on.)

Half a gigabyte of live heap, to list one directory, on a NAS. And the
number that matters as much as the peak: **List delivers nothing for 102
seconds**, because its contract is a slice. A consumer cannot begin, and
cannot be cancelled usefully in the middle either.

The same shape, isolated from the filesystem so the enumerator rather
than the disk is what is being measured
(`TestEnumerationMemoryDoesNotScaleWithEntryCount`, a synthetic chunked
directory reader):

| entries   | peak heap | max RSS delta | total  | time to first entry | goroutines left behind | directory handles held |
| --------- | --------- | ------------- | ------ | ------------------- | ---------------------- | ---------------------- |
| 100,000   | 4.5 MiB   | 4.8 MiB       | 58 ms  | 699 µs              | 0                      | 1                      |
| 1,000,000 | 5.4 MiB   | 1.7 MiB       | 486 ms | 865 µs              | 0                      | 1                      |

Ten times the entries, 1.2× the memory. (The RSS delta is smaller at
1,000,000 than at 100,000 because `Maxrss` is a per-process high-water
mark and the smaller run went first: the larger run needed nothing new,
which is the point being made.)

Open connections do not appear in either table because for a local source
there are none, and the handle count is the local analogue: one, held
depth-first, versus rclone's walk which opens one goroutine per
`--checkers` over a backend with no native recursive listing (already
pinned to 1 by `oneConnectionAtATime`, for the connection-cap reasons in
its own doc).

### The finding that shaped the decision

There is no bounded directory read in rclone, and there cannot be one
without changing rclone:

```go
// fs.Fs
List(ctx context.Context, dir string) (entries DirEntries, err error)
```

The signature **is** a slice. A backend cannot hand back a cursor even
when the protocol underneath it has one. `walk.ListR` looks like the
streaming answer and is not: it uses the backend's native `ListR` when
there is one (s3 pages at 1000), and otherwise falls back to walking
directory by directory through exactly the `List` above. For one flat
directory — the case in the title — the fallback is the whole directory in
one slice.

And per backend:

- **local**: rclone reads the directory whole. The stdlib does not:
  `(*os.File).ReadDir(n)` takes a count, which is a bounded read this
  process can drive itself.
- **sftp**: rclone's sftp backend reads through `github.com/pkg/sftp`'s
  `ReadDir`, which returns the whole directory as one slice and no
  resumable cursor. There is no partial read at any level of that stack.
- **s3**: `ListR` is native and paged, so an object store genuinely can
  be enumerated in bounded memory — though s3 is a destination in this
  product, not a source.

So "stream everything" is not an available answer, and a design that
claimed it for sftp would be a slice with a callback wrapped around it:
all of the cost, plus the claim that it had been solved.

## Decision

**Two mechanisms, and the honest thing about them is that the second one
is a refusal.**

### 1. A backend capability matrix, owned by backupd, where silence means unqualified

`core/internal/backend/capability.go` declares ten keys, closed in Go
(`CapabilityKeys`) and declared as data in each `bundled/*.json`:

`bounded_listing`, `recursive_listing`, `streaming_open`, `range_open`,
`mtime_precision`, `hash_support`, `stable_size`, `symlink_semantics`,
`metadata_support`, `case_sensitivity`.

The shipped values:

| key                 | local_volume           | s3            | sftp        |
| ------------------- | ---------------------- | ------------- | ----------- |
| `bounded_listing`   | true                   | true          | **false**   |
| `recursive_listing` | false                  | true          | false       |
| `streaming_open`    | true                   | true          | true        |
| `range_open`        | true                   | true          | true        |
| `mtime_precision`   | 1ns                    | 1ms           | 1s          |
| `hash_support`      | md5, sha1, sha256      | md5           | *(none)*    |
| `stable_size`       | true                   | true          | true        |
| `symlink_semantics` | skip                   | unsupported   | skip        |
| `metadata_support`  | full                   | partial       | partial     |
| `case_sensitivity`  | **unknown**            | sensitive     | **unknown** |

The four entries worth arguing about:

- **`bounded_listing` is a claim about the whole path**, protocol up to
  this process — not about the protocol alone. local is true *because*
  this repository now has an enumerator that uses `ReadDir(n)`. Before
  this issue it would have been false.
- **sftp is false**, for the reason in the finding above, and that is
  what makes a refusal the only honest third option.
- **`recursive_listing` means NATIVE recursive listing**, which is
  exactly rclone's `fs.Features().ListR`, not "can this engine recurse"
  (it always can, by walking). It is the difference between one round
  trip and one per directory — the cost issue #737 measured at 1.6k
  directories. `TestTheCapabilityMatrixMatchesWhatRcloneReportsForLocal`
  pins local's answer against the live rclone registry, in
  `transport/rclone`, the only package that may import both sides (the
  same arrangement `TestEveryBundledManifestNamesABackendThisBinaryRegisters`
  already uses).
- **`case_sensitivity` is "unknown" for local and sftp**, and that is an
  answer rather than a shrug. A local volume on this product's target
  hardware is whatever an operator plugged in — APFS, exFAT on a USB
  stick, ext4 — and the process cannot know which without probing that
  specific path. A matrix that guessed "sensitive" would be wrong on the
  first Mac and on every FAT-formatted disk. `hash_support` is empty for
  sftp for the same class of reason: rclone probes the far host for an
  `md5sum` binary, so hash availability is a property of somebody else's
  `PATH`.
- **`hash_support` says a hash is OBTAINABLE, not that it is cheap**, and
  that distinction was sharpened by #793 during Phase 0 rather than
  anticipated here. local's three are computed by reading the whole file;
  s3's md5 is an ETag the store already holds. So this row and #793's
  `model.SourceSignals.RemoteHashAlgorithms` disagree ON PURPOSE —
  #793 lists none for local, because a hash that costs a full read *is*
  the read and cannot be the signal that lets one be skipped. ADR 0009
  section 3 records the narrowing and tells Phase 1 to project these
  capabilities through it (with an mtime floor at what the transport
  actually carries) rather than copying them. Neither row is wrong; they
  answer different questions, and the one thing that would be wrong is a
  future reader "fixing" one to match the other.

Silence is **unqualified, not a default**. A manifest with no
capabilities block describes a backend `Manifest.PlanEnumeration` refuses
by name (`ErrUnqualifiedBackend`), and the zero `Capabilities` value is
the least capable backend the type can describe. This deliberately breaks
the format's own habit — `Configurable` defaults to true — and the
asymmetry is the decision: there is no ordinary answer to "can a
directory here be read without holding all of it", so a default would be
a guess, and the optimistic guess is an OOM kill that only ever arrives
in production on the first directory that got big. A block that IS
present must answer all ten keys, because a missing key and a declared
false are different statements that nothing downstream could tell apart.

### 2. A backupd-owned bounded enumerator, plus an explicit refusal where one is impossible

`core/internal/transport.LocalEnumerator` streams entries to a callback:
`(*os.File).ReadDir(n)` in chunks of `ChunkEntries` (default 4096),
depth-first with an explicit stack of pending directory *paths*, one
directory handle open at a time, no goroutines of its own, cancellation
checked per chunk, `Source.ExcludePaths` pruned before a directory is
ever opened. It is stdlib-only and deliberately NOT routed through
rclone, because routing it there would materialise the slice it exists to
avoid — which is why it lives in `transport` (the boundary that owns what
a `Source` is) and not in `transport/rclone` (which owns dialing
protocols).

`Adapter.Enumerate` is the dispatch, and it takes its decision from the
matrix rather than inventing one:

1. `bounded_listing` → chunked enumeration.
2. not `bounded_listing`, ceiling configured → rclone's walk, streaming
   per directory, refusing any directory above the ceiling with
   `transport.ErrDirectoryTooLarge` (category `Configuration`: a retry
   re-reads the same directory; what changes the outcome is a bigger
   ceiling or a smaller directory, both a person's decision).
3. not `bounded_listing`, no ceiling → `backend.ErrUnboundedListing`,
   **before anything is dialed**. Fail closed.

"Before anything is dialed" is load-bearing, and
`TestEnumeratingAnSftpSourceIsRefusedBeforeAnythingIsDialed` asserts it
by timing: a refusal that arrived after the connection would also have
arrived after the directory was read into memory, which is the failure it
exists to prevent.

## Alternatives considered

### Option A: get a bounded iterator into rclone upstream

The version that fixes the problem for everyone: add a paged or
callback-driven directory read to `fs.Fs`, or a `ListP`-style optional
interface, and let each backend implement it where the protocol allows.

Rejected for this phase, and not because it is wrong. It is a change to
the central interface every rclone backend implements; realistically it
lands, if it lands, on a timescale measured in rclone releases, and Phase
0 is a gate that has to answer now. Carrying it as a local patch in the
meantime is exactly what ADR 0001's closing section says needs its own
ADR, for the same reason it rejected forking: a permanent patch creates a
continuous-integration-with-upstream obligation. And for the specific
case in the title it would not even be sufficient — sftp's own client
library returns whole directories, so an upstream iterator would have to
reach into `github.com/pkg/sftp` as well.

What this ADR does instead is make the gap *nameable*:
`bounded_listing: false` in `bundled/sftp.json` is the one-line
description of what upstream would have to change, and the day rclone
grows the interface, flipping that value and adding a reader is a small,
reviewed diff with tests already written against the behaviour.

### Option B alone: a backupd streaming enumerator for everything

Chosen for local, and it is only half an answer, which is why it is not
the whole decision. Writing a "streaming" enumerator for sftp means
wrapping `pkg/sftp`'s whole-directory `ReadDir` in a callback: the peak is
unchanged and the API now claims otherwise. That is the worst of the
options considered, because it converts a known limitation into a
confident-looking abstraction — and the first person to hit it would hit
it in production, on the directory nobody tested, with a matrix that said
it was fine.

Bypassing rclone for sftp entirely (talking to `pkg/sftp` directly, with
its own key handling, host-key verification and connection caps) is a
second SSH client in a product whose FR-6 story is that there is exactly
one. Not for a Phase 0 spike, and probably not ever for this reason.

### Option C alone: reject any directory above a configured limit

The pure fail-closed answer: no enumerator at all, just a ceiling and a
refusal. It is honest and it is cheap, and it leaves the actual capability
on the table — a local source's directories CAN be walked in bounded
memory with a stdlib call, and refusing them because sftp cannot would be
this engine being conservative with somebody else's data at no benefit.

### Keep `List` and cap the result

Refuse when `len(out)` exceeds a limit. This is the version that looks
like a fix in a code review and is not one: by the time anything can
count the slice, the memory is spent. It is a message printed over the
OOM it failed to prevent.

### Discover capabilities by probing instead of declaring them

Ask the backend at runtime — list a directory and see, `Features()`,
stat a file and compare timestamps. Attractive because it cannot drift
from reality, and rejected for Phase 0 on two grounds. Probing for
`bounded_listing` means performing the operation whose cost is the
question. And a probe's answer is per-path and per-moment, so it has to
be cached, invalidated and reasoned about, which is a larger design than
the honest static statement plus the one cross-check that IS cheap
(`Features().ListR`, pinned in `transport/rclone`). `case_sensitivity:
"unknown"` is where the static matrix admits its own limit, and a
future per-path probe is the right way to resolve exactly that key.

## Consequences

### What we get

- A million-entry flat directory is enumerable in 4.2 MiB instead of 493
  MiB, with the first entry delivered in 12 ms instead of 102 seconds,
  and a cancellation that takes effect within one chunk.
- Enumeration a Kopia-style uploader can actually be fed from: it wants
  entries as it walks, not a slice it has to wait for.
- A capability matrix that answers questions the rest of Phase 0 needs:
  #793 classifies metadata trust from `mtime_precision`, `hash_support`,
  `stable_size` and `metadata_support`.
- The thing this product exists for: a source it cannot enumerate safely
  produces a refusal naming the backend and the reason, at configuration
  time, instead of a daemon killed mid-backup.
- An honest, reviewable statement of what upstream rclone would have to
  change, in one JSON value with tests behind it.

### What it costs, honestly

- **No global ordering.** `List` sorts its answer and can afford to,
  because it has the whole answer; a stream cannot sort what it has not
  seen without buffering it, which is the cost being removed. `List`'s
  sort exists for a real reason (`model.ArtifactID` identifies an
  artifact by basename, so two artifacts sharing one must at least
  collide *repeatably* — see `Adapter.List`'s own doc), so a consumer
  that needs determinism has to get it elsewhere. **Per-directory**
  ordering is not free either, and this matters concretely for #779:
  Kopia's uploader wants a directory's entries in name order, and a flat
  directory of a million entries is precisely where sorting one directory
  is this whole problem again. A bounded external merge (sorted runs of
  `ChunkEntries`, merged on the way out) is the known answer and is
  deliberately not built here; Phase 1 owns it, and it is the one thing a
  caller of `Enumerate` could be surprised by today.
- **The sftp path is bounded by a number, not by a buffer.** With a
  ceiling configured, the peak is still the largest directory. The
  refusal arrives after that directory has been read, because "not
  bounded" is what that means. It is a late refusal by design and it is
  better than no refusal; it is not the same thing as bounded, and the
  matrix says so rather than the code implying otherwise.
- **Two listing paths now exist.** `List` is unchanged and still used by
  everything in the daemon; `Enumerate` is additive and has no production
  caller yet. That is deliberate for a gate (the `backend` package
  shipped the same way, "nothing reads this yet"), and it is a real cost
  until Phase 1 migrates discovery: for now, a reader has to know which
  one they are looking at.
- **`Enumerate` is not on the `Transport` interface.** It is a separate
  `transport.Enumerator`, because `Transport` is implemented by fakes and
  decorators across this repo (`core/tests/classifytransport`,
  crashmatrix's `timedKillTransport`, reconcile's fake) and a method
  there is a method all of them must grow whether or not they have an
  answer. The cost is that a caller has to type-assert for streaming, and
  that is the honest shape: not every transport can stream.
- **`mtime_precision` currently overstates what this engine keeps.**
  `local_volume` declares `1ns` and `RemoteArtifact.ModTime` is unix
  seconds, in `toArtifact` and now in `toLocalArtifact` too. The matrix
  describes the BACKEND; the truncation is ours, it predates this issue,
  and it is written down here rather than papered over in the matrix —
  #793's metadata-trust classification is where it becomes a decision.
- **The matrix is not served on `/api/v1/backends`.** `core/service`'s
  `projectManifest` is a longhand copy and does not carry it, so there is
  no contract change and no generated-binding churn in this diff. When a
  surface needs it, that is a reviewed API diff of its own.
- **One new import edge**, `transport/rclone` → `backend`, for the
  dispatch to read the matrix. It is the safe direction (`backend`
  imports nothing but the standard library, which is what `backend/doc.go`
  exists to keep true) and the same edge `backends_test.go` already has.
  `transport` itself does not import `backend`: `EnumerationPlan` is two
  plain fields precisely so that the capability owner does not have to
  name a transport type.

### The measurement harnesses stay

`TestEnumerationMemoryDoesNotScaleWithEntryCount` (synthetic, 100k and
1M, bounded-peak assertion),
`TestCancellingMidEnumerationStopsPromptlyAndLeaksNothing`,
`TestADirectoryOverTheConfiguredCeilingIsRefusedByName` and
`TestStreamingCostsFarLessThanListAtScale` are compatibility smoke tests,
not product code. They are what makes a future rclone bump, or a future
change to the chunking, fail loudly instead of quietly costing 490 MiB
again. The million-entry real-filesystem variant is opt-in
(`BACKUPD_HUGE_DIR_ENTRIES`) because creating the fixture takes three and
a half minutes and removing it takes as long; the synthetic million-entry
run is in the default suite and takes half a second.
