#!/usr/bin/env bash
# Phase 6's performance gate (issue #165, EPIC B #81's performance
# contract).
#
# Two modes, both driven by docs/perf/gate.json, which is the one place
# the designated benchmark host, the workload and the concrete thresholds
# are written down:
#
#   (default)          presence. Fails while no complete, checked-in
#                      baseline record exists for the designated host and
#                      workload. This is the check the RED step of #165
#                      needed: before the baselines were captured it
#                      failed, and it fails again the moment a metric goes
#                      missing or the workload identifier moves.
#
#   --compare PATH     regression. Compares a freshly captured record
#                      (scripts/perf/capture-baseline.sh --out PATH)
#                      against the checked-in baseline and fails any
#                      metric outside its threshold.
#
# It is deliberately not wired into ordinary CI in compare mode. EPIC B
# #81 allows the measurements to run on a dedicated stable benchmark
# environment rather than blocking ordinary CI on noisy numbers, and a
# shared GitHub runner is not that environment. Presence mode has no
# timing in it at all, so it is safe anywhere.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

GATE=docs/perf/gate.json
COMPARE=""
# Where a baseline record for a host is looked up. Overridable so
# scripts/perf/selftest.sh can point a deliberately mutated record somewhere
# harmless: it used to write one into docs/perf/baselines/ and delete it
# afterwards, which meant an interrupted run left a falsified baseline, one
# required metric nulled, sitting in the directory whose entire purpose is to
# be authoritative.
BASELINES_DIR=docs/perf/baselines

usage() {
  cat >&2 <<'EOF'
usage: scripts/perf/check-baseline.sh [--compare PATH] [--gate PATH]
                                      [--baselines-dir PATH]

  --compare PATH        compare this freshly captured record against the
                        checked-in baseline for the designated host
  --gate PATH           read the gate definition from here instead of
                        docs/perf/gate.json
  --baselines-dir PATH  look the designated host's record up here instead of
                        docs/perf/baselines
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --compare) COMPARE="$2"; shift 2 ;;
    --gate) GATE="$2"; shift 2 ;;
    --baselines-dir) BASELINES_DIR="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "check-baseline: unknown option $1" >&2; usage; exit 2 ;;
  esac
done

command -v jq >/dev/null 2>&1 || { echo "check-baseline: jq is required" >&2; exit 1; }

if [ ! -f "$GATE" ]; then
  echo "FAIL: $GATE does not exist, so no benchmark host, workload or threshold is pinned." >&2
  exit 1
fi

host_id=$(jq -r '.benchmark_host_id' "$GATE")
workload=$(jq -r '.workload' "$GATE")
record="${BASELINES_DIR}/${host_id}.json"

if [ "$host_id" = "null" ] || [ -z "$host_id" ]; then
  echo "FAIL: $GATE names no benchmark_host_id." >&2
  exit 1
fi

if [ ! -f "$record" ]; then
  echo "FAIL: no checked-in performance baseline for the designated benchmark host." >&2
  echo "  expected: $record" >&2
  echo "  capture it with: scripts/perf/capture-baseline.sh" >&2
  exit 1
fi

if ! jq -e . "$record" >/dev/null 2>&1; then
  echo "FAIL: $record is not valid JSON." >&2
  exit 1
fi

got_workload=$(jq -r '.workload' "$record")
if [ "$got_workload" != "$workload" ]; then
  echo "FAIL: $record was captured under workload \"$got_workload\", but $GATE pins \"$workload\"." >&2
  echo "  A baseline from a different workload is not comparable. Re-capture, or correct the gate." >&2
  exit 1
fi

# Completeness. Every metric the performance contract names must be
# present and non-null, so a record that silently dropped one (a
# --skip-image capture, say) can never pass as a baseline.
#
# The gate's own list is checked first. Without this, deleting
# required_metrics from gate.json would make the loop below iterate zero
# times and this check would print OK having verified nothing, which is the
# same fail-open shape Phase 4's conformance matrix already guards against
# by refusing an omitted capability.
if ! jq -e '(.required_metrics | type) == "array" and (.required_metrics | length) > 0' "$GATE" >/dev/null; then
  echo "FAIL: $GATE declares no required_metrics, so the completeness check would verify nothing and pass." >&2
  exit 1
fi
required=$(jq -r '.required_metrics[]' "$GATE")
missing=""
for m in $required; do
  case "$m" in
    image_size_bytes) expr=".metrics.image_size_bytes.value" ;;
    *) expr=".metrics.${m}.median" ;;
  esac
  v=$(jq -r "$expr // \"null\"" "$record")
  if [ "$v" = "null" ] || [ -z "$v" ]; then
    missing="${missing}\n  ${m} (${expr} is null or absent)"
  fi
done
if [ -n "$missing" ]; then
  echo "FAIL: $record is missing required baseline metrics:" >&2
  # shellcheck disable=SC2059
  printf "$missing\n" >&2
  exit 1
fi

if [ -z "$COMPARE" ]; then
  echo "OK: baseline present and complete for $host_id under workload $workload ($record)."
  exit 0
fi

# ---- compare mode -------------------------------------------------------

if [ ! -f "$COMPARE" ]; then
  echo "FAIL: $COMPARE does not exist." >&2
  exit 1
fi

candidate_host=$(jq -r '.host_id' "$COMPARE")
if [ "$candidate_host" != "$host_id" ]; then
  echo "FAIL: $COMPARE was captured on \"$candidate_host\", not the designated benchmark host \"$host_id\"." >&2
  echo "  Comparing across machines reports the machine, not the change." >&2
  exit 1
fi
candidate_workload=$(jq -r '.workload' "$COMPARE")
if [ "$candidate_workload" != "$workload" ]; then
  echo "FAIL: $COMPARE was captured under workload \"$candidate_workload\", not \"$workload\"." >&2
  exit 1
fi

# Same fail-open guard as the completeness check above: an empty thresholds
# object would produce an empty report and a cheerful "every gated metric is
# within its threshold" having compared nothing.
if ! jq -e '(.thresholds | type) == "object" and (.thresholds | length) > 0' "$GATE" >/dev/null; then
  echo "FAIL: $GATE declares no thresholds, so compare mode would gate nothing and pass." >&2
  exit 1
fi

# Every threshold in the gate is evaluated, and every result is printed,
# pass or fail: a gate that only prints its failures cannot be reviewed,
# because a reader cannot tell an unexercised rule from a passing one.
report=$(jq -r \
  --slurpfile base "$record" \
  --slurpfile cand "$COMPARE" \
  '
  def r3: if . == null then null else (. * 1000 | round) / 1000 end;
  .thresholds
  | to_entries[]
  | .key as $metric
  | .value as $t
  | (if $metric == "image_size_bytes" then $base[0].metrics[$metric].value else $base[0].metrics[$metric].median end) as $b
  | (if $metric == "image_size_bytes" then $cand[0].metrics[$metric].value else $cand[0].metrics[$metric].median end) as $c
  | if $b == null or $c == null then
      "FAIL\t\($metric)\tmissing (baseline=\($b), candidate=\($c))"
    elif $t.direction == "lower_is_better" then
      (if $b == 0 then null else ($c / $b) end) as $ratio
      | ($b * $t.max_ratio) as $limit
      | ($t.noise_floor_abs // 0) as $floor
      # Two conditions, both required, for a metric that carries a noise
      # floor: over the ratio AND further from baseline than the measured
      # capture-to-capture movement. A ratio alone on a sub-millisecond
      # number fails against an unchanged tree, which is a gate nobody can
      # act on. Where noise_floor_abs is absent the floor is 0 and the
      # ratio is the whole rule.
      | (if ($c > $limit) and (($c - $b) > $floor) then "FAIL" else "pass" end) as $verdict
      | "\($verdict)\t\($metric)\tbaseline=\($b) candidate=\($c) limit<=\($limit|r3) floor=\($floor) delta=\(($c - $b)|r3) ratio=\($ratio|r3)"
    else
      (if $b == 0 then null else ($c / $b) end) as $ratio
      | ($b * $t.min_ratio) as $limit
      | ($t.noise_floor_abs // 0) as $floor
      | (if ($c < $limit) and (($b - $c) > $floor) then "FAIL" else "pass" end) as $verdict
      | "\($verdict)\t\($metric)\tbaseline=\($b) candidate=\($c) limit>=\($limit|r3) floor=\($floor) delta=\(($c - $b)|r3) ratio=\($ratio|r3)"
    end
  ' "$GATE")

printf '%s\n' "$report" | column -t -s "$(printf '\t')"

if printf '%s\n' "$report" | grep -q '^FAIL'; then
  echo >&2
  echo "FAIL: at least one metric is outside its threshold (see the FAIL rows above)." >&2
  echo "  Thresholds and their justification: docs/perf/README.md" >&2
  exit 1
fi

echo
echo "OK: every gated metric is within its threshold against $record."
