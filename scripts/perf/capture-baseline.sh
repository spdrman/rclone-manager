#!/usr/bin/env bash
# Capture the Phase 6 performance baseline (issue #165, EPIC B #81's
# performance contract).
#
# The contract names seven metrics and says to capture them BEFORE any
# structural refactoring, because once code moves the pre-refactor number
# is gone. This script is the one supported way to produce that record, so
# a later run compares like with like:
#
#   idle RSS, startup-to-healthy, /api/v1 read latency, configuration
#   write latency, backup transfer throughput, image size, idle CPU.
#
# Three harnesses produce them, each owned by the layer it measures:
#
#   apps/generic/tests/perfbaseline  the five process/API metrics, against
#                                    the real binary over real HTTP
#   core/tests/perfbaseline          transfer throughput, through core's
#                                    own transport adapter
#   this script                      image size, by building the image
#
# The result is written to docs/perf/baselines/<host-id>.json and is meant
# to be committed. docs/perf/README.md defines the host, the workload and
# the threshold a later run is judged against; scripts/perf/check-baseline.sh
# is what judges it.
#
# Nothing here handles a credential. The runtime harness reads the
# engine's single-use enrollment token from a pipe and keeps it in memory
# (see that harness's own doc); no harness output carries anything but
# measurements, which is why the records are safe to commit.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

REPEAT=3
OUT=""
SKIP_IMAGE=0
IMAGE_PLATFORM="linux/$(go env GOARCH)"

# The flags are documented here and nowhere else, so this text is the
# reference. Each one says what the number means as well as what it sets,
# because a baseline captured with the wrong repeat count or on the wrong
# platform is not obviously wrong later: it is just a number that will not
# reproduce.
usage() {
  cat >&2 <<'EOF'
usage: scripts/perf/capture-baseline.sh [options]

  --repeat N        how many runtime captures to take (default 3). The
                    recorded number for each runtime metric is the median
                    of these, and the observed spread is recorded next to
                    it, because a single capture is not separable from
                    machine noise.
  --out PATH        write the record here instead of
                    docs/perf/baselines/<host-id>.json
  --skip-image      do not build the OCI image (leaves image size null;
                    a record with a null image size is incomplete and
                    check-baseline.sh says so)
  --platform PLAT   image platform to build and measure
                    (default linux/$(go env GOARCH))
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --repeat) REPEAT="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    --skip-image) SKIP_IMAGE=1; shift ;;
    --platform) IMAGE_PLATFORM="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "capture-baseline: unknown option $1" >&2; usage; exit 2 ;;
  esac
done

for tool in jq go git; do
  command -v "$tool" >/dev/null 2>&1 || { echo "capture-baseline: $tool is required" >&2; exit 1; }
done

# shellcheck source=./hostid.sh
source scripts/perf/hostid.sh
host_id=$(perf::host_id)
host_json=$(perf::host_json)

if [ -z "$OUT" ]; then
  OUT="docs/perf/baselines/${host_id}.json"
fi

commit=$(git rev-parse HEAD)
dirty=false
if [ -n "$(git status --porcelain)" ]; then
  dirty=true
  # Not fatal: the baseline for #165 is captured from a working tree that
  # already holds the harness itself, which by definition is not yet
  # committed. Recorded honestly so a reader knows.
  echo "capture-baseline: NOTE the working tree is dirty; recording dirty=true against $commit" >&2
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-perf.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# Neither capture below runs under -race, and neither ever may. Every other
# `go test` in this repository does since #417, so this is the one place
# where the consistent-looking edit is the wrong one: the race detector
# slows an instrumented binary by several times, and a baseline captured
# from one would set a number no uninstrumented run could ever be compared
# against. scripts/perf/check-baseline.sh reads what these write.
echo "==> runtime harness (apps/generic/tests/perfbaseline), ${REPEAT} capture(s)"
runtime_files=()
for i in $(seq 1 "$REPEAT"); do
  f="$tmp/runtime-$i.json"
  echo "    capture $i/$REPEAT"
  (
    cd apps/generic
    PERF_BASELINE=1 PERF_BASELINE_OUT="$f" GOWORK=off \
      go test ./tests/perfbaseline/ -run TestCaptureRuntimeBaseline -count=1 -timeout 20m >/dev/null
  )
  runtime_files+=("$f")
done

echo "==> transfer harness (core/tests/perfbaseline)"
transfer_file="$tmp/transfer.json"
(
  cd core
  PERF_BASELINE=1 PERF_BASELINE_OUT="$transfer_file" GOWORK=off \
    go test ./tests/perfbaseline/ -run TestCaptureTransferBaseline -count=1 -timeout 20m >/dev/null
)

image_size=null
image_arch=null
if [ "$SKIP_IMAGE" = "0" ]; then
  echo "==> image size (docker build --platform $IMAGE_PLATFORM -f container/Dockerfile)"
  command -v docker >/dev/null 2>&1 || { echo "capture-baseline: docker is required unless --skip-image" >&2; exit 1; }
  tag="rclone-manager-perfbaseline:${commit:0:12}"
  docker build --platform "$IMAGE_PLATFORM" -f container/Dockerfile -t "$tag" . >/dev/null
  image_size=$(docker image inspect "$tag" --format '{{.Size}}')
  image_arch=$(docker image inspect "$tag" --format '"{{.Architecture}}"')
fi

echo "==> merging into $OUT"
mkdir -p "$(dirname "$OUT")"

# The merge is deliberately explicit about medians and spread rather than
# averaging: an average lets one slow capture move the recorded number,
# and the whole point of repeating is that it should not.
jq -n \
  --argjson host "$host_json" \
  --arg host_id "$host_id" \
  --arg commit "$commit" \
  --argjson dirty "$dirty" \
  --arg captured_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson repeat "$REPEAT" \
  --argjson image_size "$image_size" \
  --argjson image_arch "$image_arch" \
  --arg image_platform "$IMAGE_PLATFORM" \
  --slurpfile runtime <(cat "${runtime_files[@]}") \
  --slurpfile transfer "$transfer_file" \
  '
  def med: sort | if length == 0 then null elif length % 2 == 1 then .[length/2|floor] else (.[length/2-1] + .[length/2]) / 2 end;
  def r3: if . == null then null else (. * 1000 | round) / 1000 end;
  def stat(f): ($runtime | map(f)) as $vals
    | { median: ($vals | med | r3), min: ($vals | min | r3), max: ($vals | max | r3),
        spread_percent_of_median: (($vals | med) as $m | if $m == 0 then null else ((($vals | max) - ($vals | min)) / $m * 100 | r3) end) };
  {
    schema: "rclone-manager/perf-baseline/1",
    workload: ($runtime[0].workload),
    host_id: $host_id,
    host: $host,
    commit: $commit,
    working_tree_dirty: $dirty,
    captured_at: $captured_at,
    runtime_captures: $repeat,
    backup_sets: ($runtime[0].backup_sets),
    metrics: {
      startup_to_healthy_ms: stat(.startup_to_healthy_ms),
      idle_rss_bytes:        stat(.idle_rss_bytes),
      idle_cpu_percent:      stat(.idle_cpu_percent),
      idle_cpu_seconds_total: stat(.idle_cpu_seconds_total),
      api_read_p95_ms:       stat(.api_read_latency_ms.p95_ms),
      api_read_p50_ms:       stat(.api_read_latency_ms.p50_ms),
      config_write_p95_ms:   stat(.config_write_latency_ms.p95_ms),
      config_write_p50_ms:   stat(.config_write_latency_ms.p50_ms),
      transfer_mb_per_second: {
        median: $transfer[0].median_mb_per_second,
        min: $transfer[0].slowest_mb_per_second,
        max: $transfer[0].fastest_mb_per_second,
        spread_percent_of_median: $transfer[0].spread_percent_of_median
      },
      image_size_bytes: { value: $image_size, architecture: $image_arch, platform: $image_platform }
    },
    detail: {
      idle_cpu_floor_percent: ($runtime[0].idle_cpu_floor_percent),
      idle_cpu_window_seconds: ($runtime[0].idle_cpu_window_seconds),
      api_read_endpoint: ($runtime[0].api_read_latency_ms.endpoint),
      api_read_response_bytes: ($runtime[0].api_read_latency_ms.response_bytes),
      api_read_samples_per_capture: ($runtime[0].api_read_latency_ms.samples),
      config_write_endpoint: ($runtime[0].config_write_latency_ms.endpoint),
      config_write_samples_per_capture: ($runtime[0].config_write_latency_ms.samples),
      transfer_backend: ($transfer[0].backend),
      transfer_artifact_bytes: ($transfer[0].artifact_bytes),
      transfer_repetitions: ($transfer[0].repetitions)
    },
    runtime_raw: $runtime
  }
  ' > "$OUT"

echo "OK: wrote $OUT"
jq '.metrics' "$OUT"
