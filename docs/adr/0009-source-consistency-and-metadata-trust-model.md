# ADR 0009: What a source promises, what its metadata proves, and when this product reads the bytes anyway

## Status

Accepted as a Phase 0 feasibility gate for the embedded backup engine
(K0.4). The model, the classification and the capture reader are
implemented and tested in `core/internal/model/consistency.go` and
`core/internal/sourceconsistency`; nothing in Phase 1 may treat a source's
metadata as proof of content without going through them.

It is the source-side counterpart to ADR 0001's decision about rclone and
to the argument `core/internal/model/identity.go` already makes about the
destination side. That file's whole premise transfers word for word: this
product cannot always prove identity, so the strength of the evidence is
part of the answer rather than something the answer hides.

## Context

An incremental backup is fast because it does not read most of the source.
Whatever decides which files to skip is therefore the single most
load-bearing correctness decision in the product: a wrong skip is not a
slow backup, it is a restore point that silently does not contain the file
an operator believes it contains, discovered at the worst possible moment.

### What the pinned engine does

kopia v0.23.1 (the version pinned by K0.1) decides a file's content can be
reused from the previous snapshot when the current directory entry and the
stored one agree on **name, mode, owner, modification time and size**.
That is the complete test. In `snapshot/upload/upload.go` at that tag:

| What | Where (v0.23.1) |
| --- | --- |
| `commonMetadataEquals` - `ModTime()` via `time.Time.Equal`, `Mode()`, `Owner()` | `snapshot/upload/upload.go:694-708` |
| `metadataEquals` - the above plus `Size()` | `snapshot/upload/upload.go:710-720` |
| `findCachedEntry` - looks the name up in previous snapshots' directories | `snapshot/upload/upload.go:722-754` |
| `maybeIgnoreCachedEntry` - random re-hash, probability `ForceHashPercentage`, default 0 | `snapshot/upload/upload.go:756-767` |
| the cache hit itself | `snapshot/upload/upload.go:852-868` |
| `newDirEntry` - writes the SCAN-time metadata into the stored entry | `snapshot/upload/upload.go:450-474` |
| `filesystemEntry` - caches size, mtime, mode, owner from the walk's stat | `fs/localfs/local_fs.go:16-45` |
| `Open` - does NOT re-stat | `fs/localfs/local_fs.go:108-115` |

Three facts follow, and they are the reason this ADR exists:

1. **No content is re-read for a file that looks unchanged.** The only
   escape hatch is `ForceHashPercentage`, whose zero default means never.
2. **The metadata compared was captured at scan time, not at read time.**
   The local entry caches its stat from the directory walk and `Open` does
   not refresh it, so the stored entry's mtime is a value the file may no
   longer have by the time its bytes are read. (`FileSize` is the one field
   overwritten with reality, from the byte count in `uploadFileData`,
   `upload.go:237-300`.)
3. **Therefore a file rewritten in place during its own read, at the same
   length, with its modification time restored, is stored as a coherent
   file** and every later run agrees it has not changed. The engine is not
   in a position to notice: the information needed was destroyed before the
   bytes were read.

None of that is a defect. It is the correct, standard heuristic for a
snapshot tool, it is what makes an incremental pass fast, and the
alternative is to hash every byte every run - which is exactly what this
ADR's conservative preset does when an operator asks for it. What is not
acceptable is inheriting the heuristic **silently**.

### Why "same path, same size, same mtime" is not identity

The tempting rule is that those three agreeing means the same content, and
with nanosecond timestamps the window where it is wrong looks vanishingly
small. It is not a window. A modification time is **settable**:
`rsync --times`, `tar -p`, `cp -p`, an editor that preserves timestamps,
and restoring the source itself from a backup all write new content and
then put the old timestamp back. Every one of those defeats the rule
completely, at any resolution. This is the same argument `identity.go`
makes about the delete path, where the conclusion was that size and mtime
agreeing reaches `ConfidenceWeak` and never confirms a match.

And the resolutions available are not fine. SFTP carries mtime as a count
of seconds in its attribute structure, so there is no sub-second
information to have; the capability matrix's measured answer for s3 is a
millisecond.

A local filesystem is the exception and it is the interesting one, because
the resolution the disk keeps is not the resolution that arrives here.
APFS, ext4, XFS, btrfs and ZFS all keep nanoseconds and the capability
matrix says `1ns`, because the matrix describes the BACKEND. But
`transport.RemoteArtifact.ModTime` is unix **seconds** on every path into
this engine (`core/internal/transport/transport.go:152`), so the sub-second
half is discarded before any comparison in this design can see it. The
matrix describes the backend; the truncation is ours. This ADR's table
therefore records `1s` for `local_volume`, and
`TestLocalVolumeRecordsTheResolutionThisEngineKeepsNotTheOneTheDiskHas`
holds it there with the reason string an operator is quoted. Recording
`1ns` would be claiming a resolution this engine throws away, which is
precisely the unproven metadata assumption the classification exists to
refuse. (Widening the transport to nanoseconds is a separate change, and it
would move that row on purpose; the pre-existing truncation is written down
in ADR 0008.)

### Why two axes and not one

Two different questions get asked about a source and they have different
answers:

- **Can this source change while a run reads it?** A property of how the
  operator arranged things, not of the backend.
- **Is what this backend reports strong enough to stand in for a file's
  content across two runs?** A property of the backend's protocol, not of
  the arrangement.

A frozen LVM snapshot cannot be torn by a concurrent writer and tells you
nothing about whether a file changed since last week. A versioned object
store proves content identity across runs and is being written to while
you read it. One "is this source safe" flag loses whichever half it was
not about.

## Decision

### 1. Source-consistency mode: what the operator arranged

Declared per source, never detected, because none of the three can be
proven from this side. What recording it buys is that a mutation observed
during a run can be reported **against** the claim.

| Mode | What it means | Point in time? | A mutation during the run is |
| --- | --- | --- | --- |
| `live_best_effort` | read while whatever writes to the source keeps writing | no | ordinary - the mode exists to accommodate it |
| `externally_quiesced` | the operator stopped, flushed or locked the writers for the run | no | a **contract violation**: the arrangement did not hold |
| `external_snapshot` | the run reads a frozen image (LVM, ZFS, VSS, read-only clone) | **yes** | a **contract violation**: the path was not the snapshot |

`live_best_effort` is the default because it needs no cooperation, and it
promises the least: each file is captured coherently or reported, and the
set of files is not a point in time. Only `external_snapshot` may be
rendered anywhere as "consistent".

Reporting a violation is the most useful thing this manager can say about
the other two modes, because both failures are otherwise silent: a quiesce
hook that stopped the wrong container, and a "snapshot path" that was
actually the live mount. Such a run is **complete** (every file was
captured coherently) and the operator's belief about it is **false**, and
those are two different sentences in a run report.

### 2. Metadata-trust class: what the backend proves

Derived, not declared, from five facts about the backend
(`model.SourceSignals`, corresponding key for key to the backend
capability matrix's `mtime_precision`, `stable_size`, `hash_support`,
`metadata_support`, plus an object generation/version flag):

| Class | Reached by |
| --- | --- |
| `strong` | a content hash the backend computes **without this manager reading the object**, or a generation/version identifier that **changes when the object is overwritten** |
| `weak` | metadata that is understood and is not enough: stable sizes and a known mtime resolution, no content evidence |
| `unknown` | not established: no readable mtime resolution, no metadata, or sizes that do not hold still |

Strong is tested first and does not depend on metadata completeness: an
object store with no usable timestamp that versions every object proves
content identity by the version, and demoting it for a timestamp it does
not need would force a full re-read of a source that can answer exactly.

**No timestamp resolution reaches `strong`.** This is the decision, stated
as a property, and `TestStrongTrustRequiresHashOrGenerationEvidence` is
what holds it: the most flattering possible metadata-only source
(nanosecond mtime, stable sizes, full metadata) classifies `weak`.

The `hash_support` narrowing is the subtle part and the one most likely to
be got wrong when copying from the capability matrix. rclone advertises
md5 and sha1 for a local filesystem and for sftp, and computes both by
reading the entire file - over the SSH session, via `sha1sum`, in the sftp
case, which the shell-less forced-subsystem account this project
recommends cannot run at all. **A hash that costs a full read is not a
signal that lets a read be skipped; it is the read.** Recording it would
classify a local disk as strong on the strength of work that has not
happened.

`unknown` is treated more harshly than `weak` on purpose: a weak signal
can be sampled around, an unestablished one cannot.

### 3. The per-backend matrix this build ships

`sourceconsistency.BundledSourceSignals`, one row per bundled backend,
reviewed as a diff. `TestEveryBundledBackendHasSourceSignals` reads the
shipped manifests rather than a list in the test, so a fourth backend
cannot be shipped without a classification.

| Backend | mtime precision (as kept here) | stable size | backend hash (no read) | generation id | metadata | Class |
| --- | --- | --- | --- | --- | --- | --- |
| `local_volume` | 1s (disk keeps 1ns; see above) | yes | none | no | full | **weak** |
| `s3` | 1ms | yes | md5 (ETag, single-part only) | yes | partial | **strong** |
| `sftp` | 1s | yes | none | no | partial | **weak** |

s3's md5 is the ETag, and the caveat is recorded rather than omitted: a
multipart-uploaded object's ETag is a hash of hashes, not a content hash,
so such an object reports no usable md5. That is a **per-object** gap in a
per-backend capability, and `Decide` closes it - a policy whose premise is
a remote hash, applied to an object that has none, reads the object
(`TestRemoteHashPolicyRestsOnEvidenceAndReadsWhenThereIsNone`).

Two of these rows deliberately disagree with the backend capability matrix
(ADR 0008), and both disagreements are the narrowing this ADR argues for
rather than a stale copy:

- **`local_volume` mtime is `1s` here and `1ns` there.** The matrix
  describes the backend; this table describes what survives the transport's
  seconds truncation. Explained above.
- **`local_volume` hash is `none` here and `[md5, sha1, sha256]` there.**
  All three are computed by reading the entire file. The matrix is right
  about what rclone can produce; this table is about what arrives without a
  read, and a hash that costs a full read is the read.

`sftp`'s empty hash list is the one row where the two agree, for the same
underlying reason from two directions: the matrix reports empty because
hash availability there is a property of somebody else's `PATH`, and this
table reports none because the account this project recommends has no shell
to run `sha1sum` in.

The matrix's "silence is an explicit refusal, never a default" rule is
honoured on this side too, and it lands on `unknown` rather than on the
middle class: `SourceSignalsFor` returns false for an unknown backend, and
the zero `SourceSignals` classifies `unknown`, whose policy is to read and
hash everything under either preset.

### 4. The content-verification policy an operator chooses

One setting, `metadata-trust`, with two values, because the real question
is "would you rather pay I/O or carry risk" and that has two ends.

Neither value is called `strong`. `strong` is already a **trust class** - a
derived statement about what a backend can prove - and spelling the preset
the same way made the word mean two unrelated things in one sentence: a
source could be `strong` under the `strong` preset or `weak` under it, and a
run report that said "strong" told an operator nothing about which half it
meant. The preset names what the operator is asking for
(`trust_metadata`), the class names what the source can back up (`strong`,
`weak`, `unknown`).

| Trust class | `metadata-trust = trust_metadata` | `metadata-trust = conservative` |
| --- | --- | --- |
| `strong` | compare the backend's hash or generation id every run; read any object it cannot answer for; full read floor **90 days** | compare as well, and read every path in full **every 30 days** regardless |
| `weak` | re-read **5% of paths** every run (deterministic sample), every path at least **weekly** | **read and hash every file every run** |
| `unknown` | read and hash every file every run | read and hash every file every run |

Two properties hold over every input, including classes and presets this
code does not recognise, and both are tested rather than left to reading
(`TestNoPolicyEverPermitsSkippingContentForever`):

- **`VerifyNever` is never produced.** There is no class and no preset that
  buys "trust the metadata forever".
- **Any policy that may skip content names the interval after which it
  stops skipping.** Every path in every source is read on a bounded
  cadence.

An unrecognised preset is treated as `conservative` and an unrecognised
class as `unknown`: a typo in a configuration key must never buy a weaker
policy than the one that was asked for.

Sampling is a hash of the path, not a random draw. A random sample makes a
run's cost and a run's coverage both unreproducible, so an operator who
saw an unexplained re-read cannot ask whether it will happen again, and a
test cannot pin it at all.

### 5. The capture: every read has a proven window

`sourceconsistency.Reader` bounds its own read window, and every capture
ends in one of exactly six outcomes. There is no "probably fine".

| Check | What only it can see |
| --- | --- |
| the **kind** from the first stat | whether there is content here at all - a symlink and a directory are not files to read |
| stat before the read | gives the comparison something to compare against |
| **fstat on the open descriptor** after the read | answers about the object the bytes came from, even after the path was replaced |
| byte count vs that fstat's size | a read that ended early or ran long while metadata happened to agree |
| stat of the **path** after the read | a rename into place, or a delete - both leave the descriptor perfectly readable |
| device+inode comparison | distinguishes "this file was rewritten" from "a different file was renamed onto this path" |
| **second full read, digests compared** | a mutation whose metadata was restored - nothing else sees this |

| Outcome | Verified? | Run incomplete? | Meaning |
| --- | --- | --- | --- |
| `stable` | yes | no | nothing moved; metadata identical before and after |
| `retried` | yes | no | something moved; a later attempt within the bound saw a file that held still. The stored digest is the settled content, never the torn read |
| `incomplete` | **no** | yes | the source kept moving: not captured coherently within the retry bound |
| `unreadable` | **no** | yes | the source did not answer: an open, read or stat failed, the source would not say what is at the path, or the run was cancelled |
| `vanished` | **no** | yes | the path no longer names anything |
| `not_a_file` | no | **no** | the path names a symlink, directory, socket or device; `Kind` says which |

`incomplete` and `unreadable` were one outcome and are now two, because
they are different facts with different remedies and folding them together
made a permission error look like a busy file. The distinction is
load-bearing in one more place: only `incomplete`, `retried` and `vanished`
are EVIDENCE that the source moved, so only they can be reported as a
contract violation under a quiesced or snapshot mode. An `EACCES` is not
evidence that somebody wrote to the source, and reporting it as a broken
quiesce claim sent an operator to look at the wrong thing.

`not_a_file` is the other outcome that used to be wrong. A symlink's path
stat is the link and a read through it fstats the TARGET, so the reader's
own identity check saw two different objects, retried three times and
reported **every symlink in the tree** as an incomplete capture - a false
alarm loud enough to hide the real ones. The reader now classifies the path
before it opens anything, reports what is there, and opens with
`O_NOFOLLOW` so a link put in place after that stat cannot be traversed
either. Whether a symlink is stored, followed or skipped stays where it
belongs: the capability matrix's `symlink_semantics` decision.

The retry bound is 3 attempts. The bound is the point: one attempt cannot
distinguish "a writer touched this once" from "this file is written
continuously", and an unbounded retry on an active log is a run that never
ends.

`Run` makes completeness a property of its captures - a single missing
file makes `Complete()` false - and reports contract violations separately,
per the mode table above. Its state is unexported and every accessor hands
back a copy: a run report is the record of what a run proved, and a caller
that could append to the slice it was handed could make that record say
something the run never observed.

**The confirm read is armed by the mode and by nothing else**
(`ConfirmReadRequired(mode)`): off under `external_snapshot`, where a
frozen image cannot change and a second pass buys nothing, and on under
every other mode.

It deliberately does not consult the verification policy, and the earlier
version of this design that did was wrong in a way worth recording. That
version also disarmed the confirm read under any policy which lets metadata
skip content, reasoning that an operator who asked to read less should not
have the sampled paths read twice. But the policy decides WHICH paths are
read; it says nothing about what a read PROVES. Under the sampled policy a
weak source's 5% sample is the only thing standing between it and a
silently stale restore point, and disarming the confirm read left that
sample unable to detect the one mutation it exists to catch - an in-place
rewrite with a restored timestamp, invisible to every stat-based check by
construction. The requirement is now immutable in the code as well as on
paper: `Reader`'s fields are unexported and set once by `NewReader`, so no
call site can bank an unconfirmed read as verified content by forgetting a
flag.

The price is that the sampled policy costs two reads per sampled path
rather than one - 10% of a full pass on a weak source instead of 5%. That
is the price of the sample meaning anything.

### 6. What a Phase 1 engine feed may and may not do

Both halves of the finding in the Context above turn into constraints on
whatever code eventually hands files to the engine, and both are the kind
that produce no error and no log line when broken. They are written here
because this is the ADR that established them; ADR 0008 records the
enumeration side of the same two facts and points back.

**A feed must not hand the engine a truncated modification time.** The
engine's reuse test compares mtime at full nanosecond resolution, so the
resolution it is *given* is the resolution of its own change detection.
`transport.RemoteArtifact.ModTime` is unix seconds; feeding that field to
the engine would widen its reuse window from a nanosecond to a second for
every file on a local source, silently, and would do it while this table
honestly declares `1s`. The two are not in tension - the declaration is
about what this design can reason about, the constraint is about not making
the engine worse than it is. A feed either widens the field (a change to a
type the catalogue persists, deliberately out of Phase 0's diff) or takes
the modification time from the filesystem directly. It must never read the
truncated value as though it were what the disk said.

**A feed must not hand the engine a scan-time modification time either,
whatever its resolution.** This is the worse half. The engine stores the
metadata its directory walk captured and never re-stats before reading
(`fs/localfs/local_fs.go:16-45`, `Open` at 108-115), so a file mutated
between the walk and the read is recorded under metadata it no longer has -
which is what makes the mutation permanently invisible to every later run,
not merely missed by this one. `Capture.ModTimeNanos` is deliberately the
value taken from the **post-read** fstat, for exactly this: it is the only
modification time that was true across the whole window the bytes came
from. A feed passes that, or it reintroduces the hole this package was
built to close while appearing to use it.

**A feed must derive `SourceSignals` from the capability matrix, not from
the table in this package.** This is the Phase 1 half of the duplication
recorded under Consequences below, and it now has a named authority to
derive from: K0.3 adds a generation/version-identity key to
`backend.Capabilities`, the manifest key `generation_identity` (Go
constant `backend.CapGenerationIdentity`), whose vocabulary is
`versioned` | `none` | `unknown`:

| `generation_identity` | What it asserts | `SourceSignals` |
| --- | --- | --- |
| `versioned` | the backend assigns every write an identifier returned with the object (S3 `versionId`, GCS generation), so it changes on overwrite | `ObjectGeneration` true - strong |
| `none` | nothing but the path or slot; an overwrite that preserves size and mtime is invisible. A stable slot id is **not** a version identity | no content evidence |
| `unknown` | not established without probing the object | no content evidence |

Phase 1 retires `BundledSourceSignals` in favour of that key plus the two
named narrowings in section 3 (`hash_support` filtered to hashes obtainable
without a read, `mtime_precision` floored at what the transport carries).
It is recorded rather than built here because the key lands on K0.3's
branch and a Phase 0 gate that could not be evaluated until two spikes
merged would not be a gate. This ADR names the key by string on purpose:
nothing in this package imports `backend`.

The general shape of all three: this design's job is not to second-guess
the engine's heuristic everywhere, it is to make sure the heuristic is fed
inputs that mean what it thinks they mean, and to read the bytes itself
wherever they do not.

## Measurements

Reproducible, because a published cost nobody can re-run is a claim rather
than a measurement. The benchmarks are
`core/internal/sourceconsistency/bench_test.go` and these are the exact
commands:

```
cd core && go test ./internal/sourceconsistency/ -run '^$' -bench Capture -benchtime 1x -count 5
cd core && go test ./internal/sourceconsistency/ -run '^$' -bench Decide -count 5
```

`-benchtime 1x` for the `Capture` rows because each iteration stages its
own fixture (a 512 MiB file, a tree of 20,000 ten-byte files) and a larger
iteration count would mostly measure the filesystem writing it. `-count 5`
because wall-clock figures on a laptop vary by a factor of two between
runs; the figures below are the best of the five, and the comparisons that
matter are the ratios between neighbouring rows and the allocation figures,
which are deterministic to within a rounding of the runtime's own
bookkeeping.

Environment: Apple M5 (`goarch arm64`, 10 procs), macOS 26.6.2 (Darwin
25.6.0), Go 1.27.1, APFS under `TMPDIR`, the repository's pinned
dependencies. Every figure below is the best of five from ONE invocation of
the command above, so the ratios compare rows measured under the same
conditions. The large-file rows read a file that has just been written and
is therefore in the page cache: they price this package's own work, the
hashing and the window, not a disk.

| Measurement | Benchmark | Result |
| --- | --- | --- |
| 512 MiB single file, one read + sha256 | `CaptureLargeFile` | 182 ms, **2,814 MiB/s** |
| the same with the confirm read (two reads, two hashes) | `CaptureLargeFileConfirmed` | 362 ms, **1,413 MiB/s** |
| **cost of torn-read detection** | the ratio of those two | **1.99x wall time** - exactly the doubling of read bandwidth the design predicts |
| 20,000 x 10-byte files, full window (3 stats + open + hash) | `CaptureSmallFileTree/window` | 17.1 us/file, 58,500 files/s, 5,800 B and 24 allocs per file |
| the same with the confirm read | `CaptureSmallFileTree/window,_confirmed` | 26.6 us/file, 6,544 B and 35 allocs per file |
| the same work hand-rolled without the window (1 stat + open + hash) | `CaptureSmallFileTree/no_window` | 14.2 us/file, 4,776 B and 8 allocs per file |
| **cost of the window** | the ratio of those two | **1.20x**, +2.9 us/file - two extra stats on a ten-byte file |
| the same, with the fixed 128 KiB read buffer this package used to allocate per read | `CaptureSmallFileTree/no_window,_fixed_buffer` | 22.1 us/file, **131,752 B per file** - 2.45 GiB of garbage over the tree |
| **cost of a read buffer not sized from the stat** | the ratio of those two | **1.56x wall time and 27.6x the allocated bytes** |
| `Decide` | `Decide` | 110 ns, 128 B, 3 allocs, **9.1M decisions/s** |

Three things in that table are findings rather than confirmations, and one
of them corrects an earlier revision of this ADR.

**The read buffer.** It was a fixed 128 KiB per file whatever the file's
length, and is now sized from the stat (`Reader.bufferSize`) and reused
across the up-to-six reads one capture can perform. The earlier revision
called the fixed buffer "nearly tripling" the per-file cost on a small-file
tree; a committed benchmark says 1.56x on wall time and 27.6x on allocated
bytes. The allocation figure is the real finding and the wall-clock one was
noise read too confidently - which is the argument for committing the
benchmark rather than the number. The reuse is visible in the table too:
the confirmed row reads every file twice and allocates 6,544 B per file
rather than the ~9,900 a second per-read buffer would have cost, because
the buffer belongs to the capture and not to the read.

**The window.** Three stats and an fstat instead of one stat cost 2.9
us/file on ten-byte files, a fifth of the total, and nothing measurable on
anything larger - the earlier revision reported this as lost in the noise,
which was true of the noisier runs it was measured in and is not true of a
quiet one. It is the correct order of magnitude for two extra `stat`
syscalls and it is the price of knowing that the bytes and the metadata
recorded for them describe the same object.

**The confirm read** is the real cost of correctness here: 1.99x on bytes,
1.56x on a tree of tiny files where the fixed per-file work dominates. It
is charged wherever a path is read under a mode that does not promise a
frozen image.

## Consequences

### What we get

- The engine's reuse heuristic is no longer inherited by default. On the
  one pair that matters - same path, same size, same modification time,
  different content - the engine's rule says reuse and `Decide` says read,
  and a test asserts exactly that disagreement
  (`TestOnThePairKopiaWouldReuseAWeakSourceStillReads`). The engine's rule
  is `kopiaMetadataReuse` in `decide_test.go`: it is a model of somebody
  else's code, asserted against and never called in a run, so it is not on
  this package's API surface. A caller reaching for "what would kopia do"
  is a caller about to take back the decision this package exists to make.
- Every bundled backend's trust class is derived from signals that are
  written down, and `TestNoBackendReachesStrongTrustWithoutContentEvidence`
  refuses a row that reached `strong` without a hash or a generation id.
  No supported backend rests on an unproven metadata assumption.
- A mutation during a read has a deterministic outcome for every shape it
  comes in: same-size-with-restored-mtime, sub-second, mid-read write,
  rename into place, rename away, delete. Either a bounded retry settled
  it or the run is incomplete. Nothing is recorded as proven that was not.
- A path that is read is proven, whatever made it eligible to be skipped.
  The confirm read follows from the mode alone, and `Reader` cannot be
  constructed without it, so a sampled or periodic policy can no longer
  bank an unconfirmed read as verified content.
- A hung source is cancellable. Every method on `Source` and `File` takes a
  context, including the read itself, so a run against an SSH session whose
  connection has been black-holed ends when the run is cancelled instead of
  waiting forever inside a syscall no bound on retries can reach.
- A symlink is reported as a symlink rather than as a file that would not
  hold still, so an ordinary tree no longer produces one false
  `incomplete` per link, and the reader cannot read through one by
  accident.
- The residual risk that has no metadata answer at all - an in-place
  rewrite with a restored timestamp - has a mechanism (the confirm read), a
  price (a doubling of read bandwidth), and exactly one configuration under
  which it is not paid: a source declared `external_snapshot`, where the
  claim is that it cannot happen. It is a decision an operator makes, not a
  hole nobody mentioned.
- `external_snapshot` is the only mode that may be called a point in time
  anywhere, so the word cannot leak onto a live read.

### What it costs, honestly

- **The conservative preset on a weak source reads and hashes the entire
  source every run, twice.** On this machine that is 1,413 MiB/s instead of
  2,814, measured against a file already in the page cache. For a 2 TB
  source it is over twenty minutes of hashing before anything is uploaded,
  and on a source whose bandwidth is a network or a spinning disk rather
  than a page cache it is far longer. This is the real price of the honest
  answer and the reason the default preset is `trust_metadata`, which
  samples.
- **The `trust_metadata` preset on a weak source now reads twice as much as
  it used to**: the 5% sample is read twice, so it costs 10% of a full
  pass. That is a deliberate regression in cost, taken because the earlier
  5% could not detect the mutation the sample exists to catch (section 5).
- **`local_volume` is classified `weak`,** which will read as a
  downgrade to anyone who assumes a local filesystem is the most
  trustworthy source there is. It has the richest metadata of the three
  and no content evidence whatsoever, and those are different things.
  The `Reason` on every classification exists for exactly this
  conversation.
- **The three modes are declared and unverified.** `externally_quiesced`
  is an operator's claim about a process this manager does not control and
  `external_snapshot` is a claim about a volume it did not create. All this
  design can do is record the claim and report when a capture contradicts
  it. It cannot refuse a source whose claim is false, and it will not
  detect a false claim at all if nothing happens to change during the run.
- **`BundledSourceSignals` is a second place backend facts live**, beside
  the capability matrix in `core/internal/backend` (ADR 0008). Phase 0
  keeps them separate deliberately: the matrix landed on its own branch,
  and a Phase 0 gate that could not be evaluated until two spikes merged
  would not be a gate. The cost is real and it has already been paid once -
  the matrix's honest values arrived after this table was written and moved
  three of the five fields in it (`local_volume` mtime, `s3` mtime, `sftp`
  metadata support), which is exactly the drift a second home for the same
  facts produces.

  It is still not a copy, and Phase 1 must not make it one by deleting this
  table: two of the three shipped rows' hash lists differ from the matrix
  ON PURPOSE, for the reasons in section 3. `SourceSignalsFor` should read
  the manifest's capabilities - including the `generation_identity` key
  K0.3 adds, which is the single authority for the strong-evidence half of
  the classification (section 6) - and apply the narrowing as an explicit,
  named projection: `hash_support` filtered to hashes obtainable without a
  read, and `mtime_precision` floored at whatever the transport actually
  carries, with a test per shipped backend asserting the projected value
  rather than the declared one. A straight copy would classify a local disk
  `strong` on the strength of three hashes that each cost a full read.
- **`OSSource` does not resolve symlinks, and a symlink's content is not
  in a run.** The containment check is lexical, `Stat` lstats, `Open`
  passes `O_NOFOLLOW`, and a symlink capture is `not_a_file`, which does
  NOT make the run incomplete. That is the right answer for a content
  reader and it is also a silence: a run whose symlinks were never stored
  reports itself complete, because whether a symlink is stored, followed or
  skipped is the capability matrix's `symlink_semantics` decision and
  nothing here is in a position to make it. Whatever consumes captures in
  Phase 1 has to route `not_a_file` somewhere, and `Capture.Kind` is what
  it routes on. A reader that quietly followed a link would be making that
  decision by accident, in the most expensive possible place.
- **The confirm read is not atomicity.** It proves the bytes hashed twice
  in this run agree; it does not freeze the file. A writer that mutates
  during both reads in the same way twice is not distinguishable, and a
  source that is continuously written will exhaust the retry and be
  reported incomplete rather than captured. That is the correct answer and
  it is also the answer an operator will find annoying about an active
  database file, which is what `external_snapshot` is for.

## Alternatives considered

### Trust the engine's heuristic and ship

The cheapest option, and the one every other backup tool built on a
snapshot library takes. It is defensible for a desktop tool, where the
user is the writer and knows what they just changed. It is not defensible
for a product whose whole claim is that a restore point contains what an
operator thinks it contains, and whose sources include object stores
written by other software and SFTP accounts on machines nobody logs into.
The failure is silent, permanent, and discovered during a restore.

### Raise `ForceHashPercentage` and call it verification

kopia already has a knob for this: re-hash a random percentage of
otherwise-cached files. It would have been one line.

Two things are wrong with it. It is **random**, so coverage and cost are
both unreproducible and no operator can be told when a given file will next
be verified - which is precisely the "skip forever" property the policy
matrix exists to bound. And it is a knob on the engine, so the decision
would live behind the `backupengine` boundary where no policy, no trust
class and no operator preset can reach it. The sampled policy here is the
same idea with the two properties that make it a promise instead of a
hope: deterministic per path, and floored by an interval.

### One "source trust" setting instead of two axes

Simpler to explain and it loses information that cannot be recovered.
"Trusted" would have to mean both "cannot change under us" and "proves its
own content", and the two are independent in both directions: a frozen
snapshot of a local disk is the first and not the second, an object store
under active write is the second and not the first. Any single flag
answers one of the two questions and quietly guesses the other.

### Always re-read everything

Correct, simple, and it makes an incremental backup engine pointless: the
entire value of the thing being embedded is that it does not read the
source twice. It survives here as the conservative preset's behaviour on a
weak source, which is the right place for it - a choice an operator makes
for a source that needs it, with the cost measured above and written down.

### Detect the mode instead of declaring it

Probe the source: look for a snapshot mount, watch for changes for a
while, ask the kernel. Every version of this is a guess that would be
believed. A tree that happened not to change during a thirty-second probe
is not quiesced, and reporting it as such would manufacture exactly the
false confidence this ADR is trying to remove. A declared mode is
auditable, and a mutation observed against a declared mode is a fact worth
reporting; a detected mode makes both of those impossible.
