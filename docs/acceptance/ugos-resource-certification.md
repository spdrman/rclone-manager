# Hardware acceptance: resource certification on real UGREEN devices

Work package D2.1 (issue #89, EPIC D #177; originally §73 Work Package 5.3
of `docs/EPIC-B-multi-nas.md`) proves on a real UGREEN unit, rather than on
an assumption, that the app is light enough to sit on a NAS alongside the
NAS's normal job, and that "arm64 supported" is a claim backed by a real
arm64 run rather than by "it cross-compiled".

§82's hardware-only flow says the acceptance procedure is written first,
before the PoC exists and long before anything runs on a device. This is
that procedure. **Every threshold below was fixed before any measurement
was taken on any UGREEN hardware**, and every one of them is derived from
something already written down in this repository or from a published
perception limit, never from a number somebody saw and then wrote a
threshold around. `distribution/hwcert` parses the tables in this file and
has no copy of its own, so tuning a threshold to fit a result means editing
this document, in a diff, in review.

## What this decides

One question per architecture: with the canonical release installed
through the UGOS adapter, does the app stay inside the resource budget the
canonical runtime already publishes, and does the NAS keep doing its
ordinary job while it does?

## What this does not decide, and why the #165 baselines are not in it

`docs/perf/` holds the Phase 6 performance baselines from issue #165. They
were captured on `darwin-arm64-mac17-2`, a designated benchmark host, and
`scripts/perf/check-baseline.sh` refuses to compare a capture taken
anywhere else, because comparing across machines reports the machine.

UGREEN hardware is not that host. So nothing here compares a UGOS number to
a `docs/perf/` number, and a difference between the two is not a
regression, it is two different computers. UGOS numbers are their own
evidence record, gated against their own thresholds, and
`distribution/hwcert` has no code path that can read a `docs/perf/`
baseline at all.

What does carry across hosts is the **shape of the claim**, and that is
checked here rather than assumed:

- no data-path hop added by the adapter,
- no sidecar,
- no second application server.

Those are properties of the deployment, not of the hardware, so they are
decidable on any device and are part of every evidence record. The
`transfer_throughput_ratio` threshold measures the first one against the
device's own raw copy rate, so it holds whatever the disks underneath are
capable of.

## Preconditions

1. An authorized UGREEN NAS. Never someone else's production NAS, and
   never one holding backups whose loss would matter.
2. The **canonical release installed through the UGOS UPK** (#83), not a
   hand-built container and not a Compose stack copied onto the box. This
   procedure certifies the artifact an operator would actually get.
3. The device's architecture recorded from the device itself
   (`uname -m`), and a probe binary built for that same architecture. The
   harness refuses a record where those two disagree, so an amd64 run
   cannot produce an arm64 record by mistake or otherwise.
4. `config.yaml` present and valid in the adapter's configuration
   directory before the app is started. A config file that exists and does
   not validate is a hard startup failure rather than a first-run wizard
   (#176), and `startup_to_healthy_ms` measured against a refusing engine
   is not a startup measurement.
5. At least 15 backup sets configured across 3 sources, matching the
   workload shape `docs/perf/README.md` describes, so the API read is a
   real serialisation and not an empty-list round trip. The sets do not
   have to be the same data; the count is what makes the response a
   realistic size.
6. A second machine on the same LAN that can mount a NAS file share, for
   the ordinary-NAS-use measurement. It never needs a credential written
   to disk.
7. Nothing else installed on the NAS is doing work during the idle window.
   Record what is installed and what is running; a NAS mid-RAID-scrub is
   not an idle NAS.
8. Three files on the device and nothing else: `scripts/hwcert/measure-ugos.sh`,
   this document (the driver reads the sampling method out of it rather
   than carrying a second copy of the numbers), and the probe binary for
   this architecture from `scripts/hwcert/build-probe.sh`. No credential
   ever travels with them.

- [ ] `config.yaml` exists in the adapter's config directory and validates.

## Sampling methodology

These are the parameters the run has to use. The harness reads them from
here too and refuses a record whose own `method` block does not match, so a
short window cannot be passed off as the documented one.

<!-- hwcert:method -->
| parameter | value |
|---|---|
| `idle_window_seconds` | 600 |
| `idle_sample_interval_seconds` | 5 |
| `transfer_sample_interval_seconds` | 5 |
| `api_read_warmup_requests` | 40 |
| `api_read_timed_requests` | 400 |
| `config_write_warmup_requests` | 10 |
| `config_write_timed_requests` | 60 |
| `startup_repetitions` | 5 |
| `transfer_artifact_bytes` | 268435456 |
| `transfer_repetitions` | 3 |
| `share_read_artifact_bytes` | 1073741824 |
| `share_read_repetitions` | 3 |
<!-- /hwcert:method -->

A 10 minute idle window at 5 second intervals gives 121 samples, which is
long enough to contain at least one scheduler wake at the default
`poll_interval` of 15 minutes only some of the time. That is deliberate:
the idle budget below is sized so that a window containing a poll and a
window containing none both pass, and a window that fails did not fail
because of one poll.

Percentiles are nearest-rank, so every reported figure is a latency that
actually happened. Repetition counts are medianed. Both match how #165
reports, which keeps the numbers readable next to each other even though
they are never compared.

## Thresholds

Read `direction` first. `lower_is_better` and `higher_is_better` are gated:
a measurement outside the number in that architecture's column fails.
`recorded` means the metric is required to be present and is not gated on
its own, because an absolute value for it on hardware nobody has measured
yet would be a guess; each one is gated through a derived ratio below
instead, and a missing one still fails.

The two architecture columns carry the same number on every row today, and
that is the honest result rather than an oversight: every threshold here
comes from the canonical runtime contract or from a human perception
limit, and neither of those varies with the CPU. The columns exist so that
relaxing one architecture later is a visible, argued edit to one cell
rather than a change to both.

<!-- hwcert:thresholds -->
| metric | direction | amd64 | arm64 | unit | why this number |
|---|---|---|---|---|---|
| `idle_rss_bytes` | lower_is_better | 134217728 | 134217728 | bytes | 128 MiB, the engine's `memory_idle` in `container/compose.yaml`'s `x-canonical-runtime.resources`. That is what the runtime tells an operator to provision, so the app has to fit it. |
| `ui_idle_rss_bytes` | lower_is_better | 33554432 | 33554432 | bytes | 32 MiB, the `web-ui` service's `memory_idle` in the same block. It serves static files and proxies; anything above this is something it should not be doing. |
| `idle_cpu_percent` | lower_is_better | 1 | 1 | percent of one core | 1% of one core over the window is 6 CPU-seconds per 10 minutes, against a default `poll_interval` of 15 minutes. One poll of a 15-set catalogue has that many times over; a busy loop or a per-second poll does not. |
| `startup_to_healthy_ms` | lower_is_better | 5000 | 5000 | ms | the engine healthcheck's `start_period` in `container/compose.yaml` is 5s, and `web-ui` gates on `service_healthy`. An engine slower than this fails its own first probe and costs the operator another 30s interval before the UI appears. |
| `api_read_p95_ms` | lower_is_better | 100 | 100 | ms | 0.1s is the limit below which a response reads as instant. This endpoint is one call behind a page render, so the page cannot feel instant if the call is not. |
| `config_write_p95_ms` | lower_is_better | 1000 | 1000 | ms | 1s is the limit below which a user stays in flow. A save is an explicit action the user is waiting on, and it rewrites YAML and moves the config revision, so it gets the looser of the two perception limits. |
| `image_size_bytes` | lower_is_better | 104857600 | 104857600 | bytes | 100 MiB. EPIC B #81 forbids adding a production Node server to host the web UI; a Node runtime plus its modules is 50 to 120 MB on its own, so this ceiling is below the cost of the specific regression the constraint names. |
| `transfer_cpu_percent` | lower_is_better | 100 | 100 | percent of one core | the engine's `cpu_recommended` in `x-canonical-runtime.resources` is `"1"`. A transfer is the engine's heaviest normal work, and it has to stay inside the one core the runtime asks an operator for. |
| `transfer_mb_per_second` | recorded | n/a | n/a | MB/s | one of the seven metrics EPIC B #81's performance contract names, so it is required. An absolute floor for it on a device nobody has measured would be a guess, so `transfer_throughput_ratio` gates it against the same device's own raw copy rate instead. |
| `raw_copy_mb_per_second` | recorded | n/a | n/a | MB/s | the same-device control for the row above: the platform's own copy of the identical artifact, same source, same destination, same page-cache state. |
| `api_read_p95_under_transfer_ms` | recorded | n/a | n/a | ms | the concurrent API/UI overhead §73 WP 5.3 asks for. Gated through `api_read_under_transfer_ratio` rather than absolutely, so it measures what the transfer costs the API rather than what the CPU is. |
| `share_read_mb_per_second_app_running` | recorded | n/a | n/a | MB/s | ordinary NAS use, measured from a LAN client, with the app installed and idle. |
| `share_read_mb_per_second_app_stopped` | recorded | n/a | n/a | MB/s | the same-device control for the row above, with the app stopped and nothing else changed. |
| `transfer_throughput_ratio` | higher_is_better | 0.7 | 0.7 | ratio | derived. The adapter is meant to add no data-path hop, so the app's transfer should be a fraction of the device's raw copy that is explained by verification and journalling, not by an extra process in the path. 0.7 leaves room for the hashing and the SQLite writes a raw `cp` does not do, and does not leave room for a proxy. |
| `api_read_under_transfer_ratio` | lower_is_better | 3 | 3 | ratio | derived. A running transfer is allowed to make the API slower, and 3x of a p95 that is already under 100ms is still inside the 1s flow limit. Beyond that the UI stops being usable during exactly the operation an operator most wants to watch. |
| `share_read_throughput_ratio` | higher_is_better | 0.95 | 0.95 | ratio | derived. "Does not materially interfere with ordinary NAS use while idle" is §73 WP 5.3's own criterion, and this is it as a number: an idle app may cost a file-share read at most 5%, which is inside what two runs of the same read vary by anyway. |
<!-- /hwcert:thresholds -->

Derived metrics are computed by the harness from two recorded ones. The
inputs are declared here rather than in code, so a formula cannot be
quietly repointed at a friendlier pair of numbers either:

<!-- hwcert:derived -->
| metric | numerator | denominator |
|---|---|---|
| `transfer_throughput_ratio` | `transfer_mb_per_second` | `raw_copy_mb_per_second` |
| `api_read_under_transfer_ratio` | `api_read_p95_under_transfer_ms` | `api_read_p95_ms` |
| `share_read_throughput_ratio` | `share_read_mb_per_second_app_running` | `share_read_mb_per_second_app_stopped` |
<!-- /hwcert:derived -->

## Architectures and their evidence records

Every architecture the release claims gets its own record, and an
architecture with no record is uncertified. There is no path by which one
architecture's pass counts for another: the harness refuses a record whose
`architecture` does not match the one being verified, refuses a record
whose `probe.goarch` does not match its own `architecture` (so a probe
binary built for one architecture cannot emit a record for the other), and
refuses one whose `device.uname_machine` disagrees with either.

<!-- hwcert:architectures -->
| architecture | claimed | evidence record |
|---|---|---|
| `amd64` | yes | `docs/acceptance/evidence/ugos-resource-amd64.json` |
| `arm64` | yes | `docs/acceptance/evidence/ugos-resource-arm64.json` |
<!-- /hwcert:architectures -->

Both are claimed because `distribution/packaging/canonical.json`,
`container/release-manifest.json` and `container/compose.yaml`'s
`x-canonical-runtime.architectures` all say the release is built for both,
and a test holds this table to that list so an architecture cannot be
dropped from here to make the status table look better.

Read the current status with:

```sh
go run ./cmd/hwcert status        # from distribution/
```

which prints one line per claimed architecture and exits non-zero if any
architecture is claimed **certified** without a record behind it. It exits
zero while an architecture is simply uncertified, because
`docs/acceptance/README.md`'s §68 rule is that build-supported and
uncertified is an honest state to be in, and a red gate for it would just
mean the rule gets deleted.

## Shape of the claim

Checked from every record, on any device, because these are properties of
the deployment rather than of the hardware:

| check | what fails it |
|---|---|
| exactly two containers | a third one is a sidecar |
| both containers run the same image | a second application server, or a forked build for one of them |
| every container's command is the canonical binary | anything else in the data path |
| exactly one container publishes a port | a second LAN-facing listener |
| the engine's `backup-manager-web` hashes to `container/release-manifest.json`'s `binary_sha256` for this architecture | a build that is not the canonical release |

The last row is what the release manifest can actually prove today.
`registry_digest` is `null` for both architectures because nothing has
been pushed yet (#88 owns that), so the harness reports the digest as
unrecorded rather than passing a comparison it did not make. When a digest
lands, the record carries it and the check compares it too.

## Procedure

Record, for every step: wall-clock time, the exact command, and its
output. Nothing below asks for a credential to be written down, and
nothing writes one to disk. The device address stays out of the committed
record.

### Step 0. Identify the device and the release

```sh
uname -m                                    # x86_64 or aarch64
cat /etc/os-release                         # UGOS name and version
nproc; free -b                              # cores and memory
docker ps --format '{{.Names}}\t{{.Image}}\t{{.Command}}\t{{.Ports}}'
docker image inspect <image> --format '{{.Size}} {{.Id}}'
```

The UGOS firmware version goes in the record's `device.firmware_version`;
it is one of §68's required evidence fields and the harness refuses a
record without it.

### Step 1. Binary identity

Extract `/backup-manager-web` from the running engine container and hash
it. Compare against `container/release-manifest.json`'s `binary_sha256`
for this architecture. A mismatch stops the run: whatever is installed is
not the release this repository is certifying, and measuring it would
produce an evidence record for something that does not exist.

### Step 2. Startup to healthy

Restart the engine container and time from the restart to the first 2xx
on `/health/live`, polling every 10ms. Repeat `startup_repetitions` times
and take the median. This is the number the compose `start_period` is the
budget for.

### Step 3. Idle window

With no backup running and nothing else scheduled, sample
`/proc/<pid>/stat` and `/proc/<pid>/status` for the engine and for the
`web-ui` process every `idle_sample_interval_seconds` for
`idle_window_seconds`.

Every sample records whether the process was there at all, its PID and its
start time, not just its RSS and CPU. That is what separates "the app is
idle" from "the app is not running", which otherwise look identical: both
consume no CPU. A window in which the process vanished, or in which the
PID or start time moved (a crash loop restarting between samples), is not
an idle measurement and the harness refuses it rather than reporting 0%.

### Step 4. API read and configuration write latency

Against the app's own listener, over one keep-alive connection:

- `GET /api/v1/backup-sets`: `api_read_warmup_requests` discarded, then
  `api_read_timed_requests` timed. p95, nearest-rank.
- `PATCH /api/v1/settings`: `config_write_warmup_requests` discarded, then
  `config_write_timed_requests` timed. p95, nearest-rank. Each one really
  rewrites the YAML and moves the config revision.

The operator authenticates interactively. The session cookie and the
password stay in memory; neither goes into the record, a file, or a shell
history.

### Step 5. Representative transfer, and what it costs the API

1. Run a real backup of a `transfer_artifact_bytes` artifact,
   `transfer_repetitions` times, and record MB/s per run. Median.
2. While one of those transfers is running, re-run step 4's read phase.
   That is `api_read_p95_under_transfer_ms`.
3. Sample the engine's CPU every `transfer_sample_interval_seconds` for
   the duration of a transfer. That is `transfer_cpu_percent`.
4. Copy the identical artifact with the platform's own tools, same source,
   same destination, same page-cache state, `transfer_repetitions` times.
   Median. That is `raw_copy_mb_per_second`, and it is the control that
   makes the throughput number mean something on a device whose disks
   nobody has characterised.

### Step 6. Ordinary NAS use

From the LAN client, not from the NAS:

1. With the app installed, running and idle, read a
   `share_read_artifact_bytes` file from a NAS file share
   `share_read_repetitions` times, dropping the client's cache between
   runs. Median MB/s.
2. Stop the app. Change nothing else. Repeat.
3. Start the app again.

The ratio of the two is the §73 WP 5.3 criterion "does not materially
interfere with ordinary NAS use while idle", answered with a number rather
than an impression.

### Step 7. Build and verify the record

```sh
# on the device, with the probe built for it, from the raw samples the
# steps above wrote. There is no Go toolchain on a NAS and none is needed:
# scripts/hwcert/build-probe.sh produces a static binary per architecture.
./hwcert-linux-<arch> record -samples samples.json -out ugos-resource-<arch>.json

# back in a checkout, once the record is committed
go run ./cmd/hwcert verify -record docs/acceptance/evidence/ugos-resource-<arch>.json
```

`verify` prints every metric, passing or failing, with its threshold and
its margin, because a report that prints only failures cannot be reviewed:
a reader cannot tell an unexercised rule from a passing one.

## Evidence to record

Commit, per architecture, at `docs/acceptance/evidence/ugos-resource-<arch>.json`:

- architecture, and the probe's own `goarch` and the device's `uname -m`
  alongside it;
- provider (`ugos`), device vendor, model, UGOS firmware version, kernel,
  cores, memory;
- the release under test: version, image reference, the binary SHA-256 the
  device reported, and the registry digest if one exists;
- the deployment as observed: container names, images, commands, published
  ports;
- the raw idle samples, not only the aggregate, so a later reader can
  re-derive the number;
- every measurement in the thresholds table;
- the method block, matching this document's;
- captured-at, and who ran it.

Alongside it, in the issue or the PR: the command transcripts, with the
device address and any credential redacted.

## Accept / reject

**Accept** an architecture when its record exists, its shape checks pass,
every `recorded` metric is present, and every gated metric is inside its
threshold. Certification is per architecture and says nothing about the
other one.

**Reject** it when any gated metric is outside its threshold, when any
required metric is missing, when the idle window was not an idle window,
or when the shape checks fail. A reject is not a licence to move a number
in this file: either the app changes, or the claim for that architecture
is withdrawn, or a threshold moves in its own commit with its own
argument and a re-run behind it.

**Uncertified** is the state of an architecture with no record, and it is
not a failure. §68's rule is that a provider or architecture is described
as build-supported but uncertified until its acceptance test is completed,
and this procedure's status output says exactly that.

A first execution that passes every threshold by more than an order of
magnitude is itself a finding: the budgets were coarser than the hardware
needed them to be. The follow-up is to tighten them against the recorded
numbers, in a separate commit, with the record already in history so the
before and after are both visible. Tightening them in the same commit that
first recorded them would be deciding the thresholds after seeing the
numbers, which is the one thing this document exists to prevent.
