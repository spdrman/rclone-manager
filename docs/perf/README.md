# Phase 6 performance baselines and the regression gate

EPIC B (#81) says the Phase 6 refactor is expected to be performance-neutral,
and that reproducible baselines have to be captured *before* structural
refactoring begins, because once code starts moving the pre-refactor number is
gone for good. Issue #165 owns capturing them. This is where they live, what
they mean, and exactly what a later Phase 6 change has to beat.

## The one-line answer

On host `darwin-arm64-mac17-2` under workload `phase6-baseline-v1`, a later
Phase 6 change fails the performance gate if the median of five captures shows
**`GET /api/v1/backup-sets` p95 above 0.143 ms** (and more than 0.05 ms above
baseline), or **transfer throughput below 483.9 MB/s**. Three more metrics are
gated alongside them, two are recorded but not gated, and every number below is
derived from measurement rather than chosen.

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
- **`api_read_p95_ms`** is gated on a ratio **and** a measured absolute floor
  of 0.05 ms, both of which must be exceeded before it fails. 0.05 ms is 2.6x
  the observed 0.019 ms movement, and below the cost of the cheapest structural
  regression this gate exists to catch: an added loopback hop in the data path.
  Sub-millisecond latencies do not support a percentage-only gate, and
  pretending otherwise would be the kind of number nobody can act on.
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

| metric | baseline | fails when |
|---|---|---|
| `api_read_p95_ms` | 0.130 ms | above 0.143 ms **and** more than 0.05 ms above baseline |
| `transfer_mb_per_second` | 537.702 MB/s | below 483.932 MB/s |
| `idle_rss_bytes` | 98,861,056 | above 108,747,162 |
| `config_write_p95_ms` | 11.357 ms | above 12.493 ms |
| `image_size_bytes` | 43,008,762 | above 45,159,200 |

## About `working_tree_dirty: true` in the checked-in record

The record names commit `8ad3100` with `working_tree_dirty: true`, and that is
accurate rather than sloppy: the capture harness itself was the only
uncommitted thing in the tree, and it is a `_test.go` package plus three shell
scripts, none of which is compiled into the binary being measured or into the
image being sized. So the artifact measured is byte-for-byte what `8ad3100`
produces. Capturing before the move was the whole point, and there was no
earlier commit that already contained the harness to capture from.

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
