# Phase 6 performance baselines and the regression gate

EPIC B (#81) says the Phase 6 refactor is expected to be performance-neutral,
and that reproducible baselines have to be captured *before* structural
refactoring begins, because once code starts moving the pre-refactor number is
gone for good. Issue #165 owns capturing them. This is where they live, what
they mean, and exactly what a later Phase 6 change has to beat.

## The one-line answer

On host `darwin-arm64-mac17-2` under workload `phase6-baseline-v1`, a later
Phase 6 change fails the performance gate if the median of five captures shows
**`GET /api/v1/backup-sets` p95 above 0.218 ms**, or **transfer throughput below
672.2 MB/s** (capture that one on a quiet machine, or it fails for reasons that
are not the tree; "What moved" below has six measurements of why). That p95
number carries two conditions and both have to hold, so
the 0.05 ms absolute floor is what binds rather than the 10% ratio; the section
below works the arithmetic through. Three more metrics are gated alongside them,
two are recorded but not gated, and every number below is derived from
measurement rather than chosen.

Six of the seven comparisons are manual. `--compare` is a step somebody takes
deliberately on the benchmark host, and what CI enforces for those six is that a
complete baseline exists and that the gate can still fail.

**`image_size_bytes` is the exception, and is enforced on every full local gate
run** (#635). It is not a timing measurement: two builds of one commit produce
byte-identical sizes, so nothing about a loaded machine can move it, and
`apps/generic/tests/dockercli` already builds `container/Dockerfile` for its
licence and CLI proofs. `TestTheBuiltImageIsInsideTheRecordedSizeBudget` reads
the size off that image and applies this file's own ratio to the record below,
which costs one `docker image inspect`. See "Running it" at the bottom.

## What is here

| file | what it is |
|---|---|
| `gate.json` | machine-readable: the designated host, the workload, which metrics must be recorded, and each gated metric's threshold |
| `baselines/<host-id>.json` | one captured record per benchmark host |
| `../../scripts/perf/capture-baseline.sh` | the capture driver |
| `../../scripts/perf/check-baseline.sh` | the gate, in presence mode and compare mode |
| `../../scripts/perf/selftest.sh` | the gate's own positive controls |

## The benchmark host

```
host_id  darwin-arm64-mac17-2
model    Mac17,2 (Apple M5, 10 cores, 32 GB)
os       Darwin 26.6.2, arm64
```

This is the machine the work was done on, and naming it honestly matters more
than naming an aspirational one. `check-baseline.sh` refuses to compare a
capture taken anywhere else, so a run on a different machine reports "wrong
host" rather than a regression that is really a different computer. When Phase 6
gets a dedicated benchmark environment, capture a record there, add it under
`baselines/`, and point `gate.json`'s `benchmark_host_id` at it: nothing else
has to change.

## The workload

`phase6-baseline-v1`, defined by the harness constants rather than by prose, so
it cannot drift from what actually ran (see
`apps/generic/tests/perfbaseline/runtime_test.go`):

- the real `backup-manager-web serve` binary, built from `apps/generic` with
  `GOWORK=off`, driven over real HTTP on loopback with one keep-alive
  connection, never an in-process `httptest` handler;
- a configuration of **15 backup sets across 3 sources**, local remotes, with
  real empty directories behind each one;
- **startup to healthy** measured from `exec` to the first 2xx on
  `/health/ready`, polled every 2 ms;
- **idle RSS and idle CPU** sampled after a 20 s settle, before any API load,
  with CPU averaged over a further 30 s window;
- **`GET /api/v1/backup-sets`**: 40 discarded warmups, then 400 timed requests.
  The response is 6,318 bytes, so this is a real serialisation of all fifteen
  sets and not an empty-list round trip. Percentiles are nearest-rank, so every
  reported figure is a latency that actually happened;
- **`PATCH /api/v1/settings`**: 40 warmups, then 60 timed requests, each of
  which really rewrites the YAML config and moves the config revision;
- **transfer throughput**: a 256 MiB incompressible artifact copied five times
  through `core/internal/transport/rclone`'s `CopyToLocal`, local backend, disk
  to disk, warm page cache (see `core/tests/perfbaseline/transfer_test.go`).
  Median of the five;
- **image size**: `docker build --platform linux/arm64 -f container/Dockerfile`,
  then the image's own reported size.

Each metric's recorded value is the **median of five whole captures**, and the
observed spread is recorded next to it. One capture is not separable from
machine noise; see the next section for how much that matters.

## Why the recorded numbers are medians of five, and why some metrics are not gated

Two independent five-capture baselines were taken back to back on an otherwise
quiet machine, at the same commit, with no code change between them. Comparing
their medians is what the gate's tolerances are built on, because it is the
run-to-run movement of the *gated statistic* that a threshold has to clear, not
the spread of individual samples:

| metric | baseline A | baseline B | movement |
|---|---|---|---|
| `api_read_p95_ms` | 0.130 | 0.149 | **+14.6%** |
| `api_read_p50_ms` | 0.085 | 0.097 | +14.1% |
| `startup_to_healthy_ms` | 19.652 | 22.012 | **+12.0%** |
| `idle_cpu_seconds_total` | 0.12 | 0.14 | +16.7% |
| `config_write_p95_ms` | 11.357 | 11.901 | +4.8% |
| `config_write_p50_ms` | 9.754 | 10.433 | +7.0% |
| `transfer_mb_per_second` | 537.702 | 542.702 | **+0.93%** |
| `idle_rss_bytes` | 98,861,056 | 99,074,048 | **+0.22%** |
| `image_size_bytes` | 43,008,762 | 43,008,762 | **0%** |

That table is the whole argument. A naive "within 10% of the recorded number"
rule applied to `api_read_p95_ms` would have failed against an unchanged tree,
and a gate that goes red on an unchanged tree teaches everyone to ignore it.
So:

- **`transfer_mb_per_second`, `idle_rss_bytes`, `image_size_bytes`** are gated
  on a ratio alone. Their noise is 11x, 45x and infinitely below the budget
  respectively, so the ratio is a real gate.

  That holds for two of the three, and **not for `transfer_mb_per_second`**
  (#635). The 0.93% is real and it is the wrong quantity: both baselines in the
  table were taken back to back on a quiet machine, so what was measured is
  repeatability under one condition. Six runs of an unchanged tree on a loaded
  machine span **1.398x**, which is fourteen times the budget rather than a
  ninth of it. See "What moved" below for the numbers and for what to do when
  this metric fails. Nothing about the other two changes: idle RSS is a process
  property and an image size is a function of the tree.
- **`api_read_p95_ms`** is gated on a ratio **and** a measured absolute floor
  of 0.05 ms, and **both** must be exceeded before it fails. Two conditions that
  must both hold means the wider one is what is enforced: against the 0.168 ms
  baseline the ratio allows 0.185 ms and the floor allows 0.218 ms, so the floor
  always binds and the budget this metric really carries is **+29.8%**, not
  +10%. (Those two numbers were 0.143 ms and 0.180 ms, for +38.5%, against the
  0.130 ms baseline this record replaced in #635. The floor is absolute, so a
  larger baseline narrows the effective budget rather than widening it, and
  `scripts/perf/selftest.sh` refuses the whole run if the floor ever stops
  binding first.) The floor is there because 0.05 ms is 2.6x the observed 0.019 ms
  movement, and sub-millisecond latencies do not support a percentage-only gate;
  a gate that goes red on an unchanged tree teaches everyone to ignore it.

  The thing that floor was sized against was the cheapest structural regression
  this gate exists to catch, an added loopback hop in the data path. That hop
  was **later measured at 0.047 ms** at p95 on this same host, which is just
  under the floor. So the honest statement is that **this gate would not catch
  that hop**, and the earlier claim that the floor sat below its cost is wrong.
  Closing the gap means lowering the floor, which means reducing measurement
  noise first (more timed samples per capture, or more captures per baseline)
  until the median's own movement is below the new floor. Writing a smaller
  number over the same noise would only produce a gate that fails against an
  unchanged tree. `scripts/perf/selftest.sh` pins which condition binds, with
  one control just under the floor that must pass and one just over it that
  must fail, so the band between 0.185 ms and 0.218 ms is exercised rather than
  assumed. Those two controls are derived from the record rather than written
  down, so they follow a re-capture on their own.
- **`config_write_p95_ms`** is gated on a ratio, with about two times headroom
  over its noise. It is the tightest of the gated metrics and the one most
  likely to need re-measurement rather than a fix if it trips.
- **`startup_to_healthy_ms`, `idle_cpu_percent`, `idle_cpu_seconds_total`** and
  the two p50s are **recorded but not gated**. Startup moves 12% on its own,
  and the process consumes less CPU at idle than `ps` can resolve
  (`idle_cpu_floor_percent` in the record says what the floor is). They are
  review signals: a Phase 6 issue that moves one of them materially has to say
  so and explain it, which is what #81 asks for on idle memory too.

All seven metrics EPIC B #81's contract names are still **required to be
present**. `check-baseline.sh` refuses a record that is missing any of them, so
a `--skip-image` capture can never be mistaken for a real baseline.

## The gate, concretely

Derived from `gate.json` and `baselines/darwin-arm64-mac17-2.json`:

| metric | baseline | fails when | effective threshold |
|---|---|---|---|
| `api_read_p95_ms` | 0.168 ms | above 0.185 ms **and** more than 0.05 ms above baseline | **0.218 ms (+29.8%)** |
| `transfer_mb_per_second` | 746.867 MB/s | below 672.180 MB/s | 672.180 MB/s (-10%) |
| `idle_rss_bytes` | 106,725,376 | above 117,397,913.6 | 117,397,913.6 (+10%) |
| `config_write_p95_ms` | 11.586 ms | above 12.745 ms | 12.745 ms (+10%) |
| `image_size_bytes` | 69,704,266 | above 73,189,479.3 | 73,189,479.3 (+5%) |

`api_read_p95_ms` is the only row with two conditions, and because both have to
hold, the wider of the two is what is enforced. Here that is the floor, so read
the last column rather than the ratio: 0.218 ms, not 0.185 ms. Every other row
has a single condition and the two columns agree.

## What moved between `8ad3100` and `186ba0c7`, and why each of it moved

The record here was captured at `8ad3100` on 2026-08-31 and replaced at
`186ba0c7` on 2026-09-08, 834 commits later. Three of the seven metrics moved
enough to need an account, and `image_size_bytes` moved to 1.62x of a metric
gated at 1.05x. A baseline is not allowed to absorb a number like that on the
grounds that it is the current number, so this is the accounting that was done
before it was re-captured (#635). Every figure below was produced on this host
on 2026-09-08.

### `image_size_bytes`, 43,008,762 -> 69,704,266 (1.621x)

Two builds, one at each commit, with the flags the capture driver uses:

```sh
docker build --platform linux/arm64 -f container/Dockerfile -t <tag> .
docker image inspect <tag> --format '{{.Size}}'
```

`8ad3100` came back at **43,008,762**: the recorded value, to the byte, eight
days and 834 commits later, on a host under a load average of 4.76. That is
worth stating on its own, because it is the premise of everything else here.
`gate.json` says this metric has no noise to allow for, and this is what that
claim looks like when it is re-tested rather than quoted.

The components, copied back out of both images with `docker create` plus
`docker cp` and sized with `stat`:

| component | `8ad3100` | `186ba0c7` | delta |
|---|---|---|---|
| `/backup-manager` | 19,792,032 | 31,391,904 | +11,599,872 |
| `/backup-manager-web` | 21,102,752 | 32,637,088 | +11,534,336 |
| `/ui/bundles`, five adapter bundles | not carried | 3,503,996 | +3,503,996 |
| `/licenses` | not carried | 57,300 | +57,300 |
| distroless base layers | 2,113,978 | 2,113,978 | 0 |
| **image** | **43,008,762** | **69,704,266** | **+26,695,504** |

Both columns sum to their image exactly, so nothing is unattributed.

**9,502,720 bytes of each binary is rclone's S3 backend**, which #369 imported
for EPIC E's MediumStore. Measured by building each command for `linux/arm64`
with the Dockerfile's own flags and then again with that one blank import
commented out: `backup-manager` goes 31,391,904 -> 21,889,184 and
`backup-manager-web` goes 31,981,728 -> 22,479,008. Identical deltas, because it
is the same dependency tree in both: the AWS SDK v2, the IBM COS SDK, Swift,
go-openapi and the rest of what arrived in `core/go.mod` alongside it. So
19,005,440 bytes, **71.2% of the whole move, is one shipped feature**.

The remaining 4,128,768 across the two binaries is product code. 77,422 net
lines of non-test Go landed under `core/`, `apps/common/` and `apps/generic/`
between the two commits, which is about 56 bytes of image per line: an
unremarkable ratio, and the same order in both binaries.

`/ui/bundles` is #180's five adapter bundles and `/licenses` is #407's licence
material. Neither existed to be measured when the old record was taken.

Ruled out, so that "expected" means something: the Go builder and the distroless
runtime are pinned by **identical digest** at both commits, so no part of this is
toolchain drift. Both build stages carry `-ldflags "-s -w"`, so there are no
debug symbols in either binary. There is no vendor tree. `rclone` stayed at
v1.75.0. Each bundle's JS chunk is distinct (its own provider bridge), so there
is no duplicate to remove there.

One real duplication, recorded rather than blessed: the seven IBM Plex woff2
faces #632 added are byte-identical in all five bundles and embedded a sixth
time in `backup-manager-web`. That is 139,744 bytes per copy and **558,976 bytes
of pure redundancy** in `/ui/bundles`. It follows from a bundle being a
self-contained document root, which is what `serve-ui --ui-root <root>/<profile>`
resolves, so removing it needs a shared asset route and a change to every
bundle's CSS. That is a design change, not a fix, and it is on #635 rather than
quietly absorbed here.

### `idle_rss_bytes`, 98,861,056 -> 106,725,376 (1.08x, inside its 1.10 gate)

**6,144,000 of the 7,864,320 is the same S3 backend.** Three runtime captures
with the blank import and three without, everything else identical: the median
idle RSS is 106,774,528 with it and 100,630,528 without. That is 78% of the
growth, and it is the cost of a backend registering itself and its dependency
tree at init.

### `startup_to_healthy_ms`, 19.652 -> 41.061 (2.09x, not gated)

This one is not what it looks like, and the workload definition is why.

The harness gives every capture a fresh temporary directory, so **every capture
measures a first boot**: `state.db` is created and every migration is applied
inside the window being timed. Four consecutive starts of each engine against
one state directory, so run 1 pays for the migrations and runs 2 to 4 do not:

| | `8ad3100` (3 migrations) | `186ba0c7` (8 migrations) |
|---|---|---|
| run 1, cold | 22.421 ms | 45.817 ms |
| runs 2-4, warm | 14.631 / 14.447 / 14.402 ms | 19.533 / 17.126 / 17.059 ms |
| first-boot migration cost | 7.974 ms | 28.691 ms |

So of the 21.4 ms the recorded metric moved, about **20.7 ms is first-boot
schema migration** (`core/migrations/` went from three files to eight over this
range, at roughly 3 ms each) and about 2.7 ms is the engine itself starting
slower. It is not the S3 backend: the differential above puts that at 0.3 ms of
startup, which is inside this metric's own run-to-run movement.

That is worth reading twice, because it says something about the metric rather
than about the tree. As defined by this workload, `startup_to_healthy_ms` is a
**first-boot** number, and it will keep climbing every time a migration is added,
for as long as the workload gives each capture an empty database. An operator
restarting a container that already has its schema pays the warm number, which
moved 14.4 ms -> 17.1 ms. Anyone reading this metric as "how long the engine
takes to come up" is reading the wrong thing, and #635 carries the note.

### `transfer_mb_per_second`, 537.702 -> 746.867 (1.39x, and the weakest number here)

This one is recorded WITHOUT an attribution, and that is a statement about the
metric rather than a gap somebody will close later. Read the recorded value as
"what a quiet machine produced on 2026-09-08", not as "what this tree does".

The account that was available for the image is not available here. The transfer
harness did not exist at `8ad3100` (`core/tests/perfbaseline` is one of the
uncommitted files that made that record `working_tree_dirty`), so there is no
old-code side to run today and no way to separate a code improvement from a
machine that happened to be faster.

What can be measured is how much of this number is the machine, and the answer is
most of it. Six runs of the SAME unchanged tree later the same day, while a full
gate was running and the load average sat between 11.3 and 12.6:

```
479.584  492.995  619.020  624.583  651.716  670.434   MB/s
```

That is a **1.398x spread with nothing changed**, and every one of the six is
below the 672.180 MB/s floor this record now sets. The recorded 746.867 was taken
at 88.33% idle CPU on a load average of 2.84, and is 1.114x the best of the six.

So the practical consequences, plainly:

- **A candidate captured on a busy machine will fail this metric for reasons that
  have nothing to do with the tree.** If it fails and nothing touched the
  transport, re-capture on a quiet host before looking for a regression.
- **Recording a favourable state raises a floor for everyone.** It is the mirror
  of a generously recorded regression: that one fails to catch things, this one
  fails runs that deserve to pass. The old 537.702 had the same property and
  nobody had measured it.
- The "0.93% median-to-median movement" in the noise study above is still
  accurate and still does not cover this. Those two baselines were taken back to
  back on a quiet machine, so what they measured is repeatability under ONE
  condition, not sensitivity to condition. A disk-to-disk copy of 256 MiB is the
  metric here most exposed to what else the machine is doing, and the study could
  not see that by construction.

Nothing here changes what is gated, because that is a decision about the contract
rather than a measurement. It is written down so the decision is made with the
numbers in front of whoever makes it.

### The other two

`config_write_p95_ms` moved 1.02x. `api_read_p95_ms` moved 1.292x in ratio terms
and passed on its absolute floor, delta 0.038 ms against a floor of 0.05 ms,
which is the gate working the way the section above describes rather than being
lenient: that metric's own spread within this capture was 64% of its median.

## About `working_tree_dirty: true` in the checked-in record

The record names commit `186ba0c7` with `working_tree_dirty: true`, and that is
accurate rather than sloppy. What was uncommitted at capture time is #635's own
change: two `_test.go` files in `apps/generic/tests/dockercli`, and comment-only
edits to `container/Dockerfile`, `ui/shared/scripts/build-bundles.mjs`,
`distribution/packaging` and two documents. None of it is compiled into the
engine binary the runtime harness measures, and none of it changes a byte of the
image.

That last part is measured rather than argued. `186ba0c7` was built clean, before
any of those edits existed, and came to 69,704,266 bytes; the dirty tree's build
during the capture came to 69,704,266 bytes. So the artifact measured is
byte-for-byte what `186ba0c7` produces.

The record this replaced named `8ad3100`, also dirty, for the same reason one
step earlier: the capture harness itself was the only uncommitted thing in that
tree, and there was no earlier commit that already contained it to capture from.

## Running it

```sh
# Capture (about six minutes; needs Docker for the image metric)
scripts/perf/capture-baseline.sh --repeat 5

# Presence: is there a complete, checked-in baseline for the designated host?
scripts/perf/check-baseline.sh

# Regression: does a fresh capture beat the checked-in one?
scripts/perf/capture-baseline.sh --repeat 5 --out /tmp/candidate.json
scripts/perf/check-baseline.sh --compare /tmp/candidate.json

# Positive controls for the gate itself
scripts/perf/selftest.sh
```

Presence mode and the self-test take no measurements and run in seconds, so
they are safe in ordinary CI. Compare mode is not wired into ordinary CI on
purpose: #81 allows the measurements to run on a dedicated stable benchmark
environment rather than blocking ordinary CI on noisy numbers, and a shared
runner is not that environment.

Plainly, so nobody reads "gated" as "enforced on every change": **no gate
anywhere runs `--compare`,** and for six of the seven metrics that is still the
whole story. `scripts/ci-local.sh` and `.github/workflows/ci.yml` wire presence
mode and the mutation self-test. Every timing number in a Phase 6 pull request
is therefore an author self-report taken by hand on the host named above, and a
reviewer who wants to reproduce one has to capture on that host.

**`image_size_bytes` is the exception** (#635). It is not a timing measurement,
so none of the reasoning above applies to it: two builds of one commit produce
byte-identical sizes, load cannot move it, and nothing about a shared runner
makes it noisy. What made it awkward to gate was cost, not noise, and that turned
out to be a false constraint: `apps/generic/tests/dockercli` already builds
`container/Dockerfile` once per test process for the licence and CLI proofs, so
`TestTheBuiltImageIsInsideTheRecordedSizeBudget` reads the size off an image that
exists and applies this file's ratio to the record. One `docker image inspect`,
no second build, and it runs on every full `scripts/ci-local.sh`.

It compares only when the built image's architecture matches the one the record
names, and skips with that reason otherwise, because an image size is a property
of the tree AND the target architecture.

That one condition is the whole limit on this arm, and it is worth stating
precisely rather than as "it only runs locally". GitHub CI **does** run this
package: `.github/workflows/ci.yml`'s `apps/generic build, vet, test` job runs
`go test -race ./...` in `apps/generic` on `ubuntu-latest`, `tests/dockercli` is
in that package list, there are no build tags or env guards on it, and Docker is
present on the runner. So CI builds the image and then takes the skip, because
`runtime.GOARCH` there is amd64 and the only checked-in record is arm64. CI pays
for the container build and asserts nothing about its size.

Closing that means an amd64 record, which means a designated amd64 benchmark
host, because `benchmark_host_id` pins one machine and therefore one
architecture. Until there is one, every automated statement about this metric
comes from a run on `darwin-arm64-mac17-2`, and what CI enforces is presence plus
the self-test.

The reason it exists at all is that this metric drifted to 1.62x with nothing
going red for eight days and 834 commits, which is what a gate nobody runs looks
like from the inside.

## Nothing here writes a credential to disk

The engine prints a single-use enrollment bootstrap token on its own stdout.
The harness reads it from a pipe and keeps it in memory, and the password it
enrolls with is generated per run and never leaves memory either. No harness
output carries anything but measurements, which is why the records are safe to
commit.

## If a number moves

Moving files should not move any of these numbers. If one does, that is a
finding to explain, not a baseline to re-cut. Re-capturing the baseline to make
a red gate green is how the contract stops meaning anything; the record carries
`commit`, `captured_at` and `working_tree_dirty` so a re-cut is visible in
review.

Shipping features does move them, and the record has been re-cut once, at
`186ba0c7` (#635). The order that made that legitimate is the part to copy:
**account for it first, capture second.** "What moved between `8ad3100` and
`186ba0c7`" above is what a re-cut has to look like. Every component of the
26,695,504-byte image move is attributed to a named change by a measurement
somebody can repeat, the parts that are NOT explained by the obvious candidate
are called out as such, and the one piece of real waste it turned up is written
down rather than folded into the new number. A re-capture with none of that
behind it records a regression as the new normal, which is the one use this
directory must never be put to.
