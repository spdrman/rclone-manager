#!/usr/bin/env bash
# Positive controls for the performance gate (issue #165).
#
# Every assertion scripts/perf/check-baseline.sh makes is a negative one
# ("no metric regressed", "no metric is missing"), and a negative assertion
# that has never been seen to fail is indistinguishable from one that
# cannot. This script mutates a real baseline record, one property at a
# time, and requires the gate to fail for each mutation and to pass for the
# unmutated original and for a mutation that stays just inside a threshold.
#
# It runs in a few seconds and takes no measurements of its own, so it is
# safe in ordinary CI even though the capture harness is not.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

command -v jq >/dev/null 2>&1 || { echo "perf selftest: jq is required" >&2; exit 1; }

GATE=docs/perf/gate.json
host_id=$(jq -r '.benchmark_host_id' "$GATE")
record="docs/perf/baselines/${host_id}.json"

tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-perf-selftest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

# expect_fail <name> -- <command...>
expect_fail() {
  local name=$1; shift; shift
  if "$@" >"$tmp/out" 2>&1; then
    echo "SELFTEST FAIL: $name — the gate PASSED a case it must refuse." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  else
    echo "  ok (refused as required): $name"
    pass=$((pass + 1))
  fi
}

# expect_pass <name> -- <command...>
expect_pass() {
  local name=$1; shift; shift
  if "$@" >"$tmp/out" 2>&1; then
    echo "  ok (accepted as required): $name"
    pass=$((pass + 1))
  else
    echo "SELFTEST FAIL: $name — the gate REFUSED a case it must accept." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  fi
}

echo "==> negative control: the real baseline and gate as checked in"
expect_pass "presence mode against the checked-in record" -- ./scripts/perf/check-baseline.sh
expect_pass "compare mode against the record itself" -- ./scripts/perf/check-baseline.sh --compare "$record"

echo
echo "==> presence-mode mutations"

jq '.benchmark_host_id = "a-host-that-was-never-benchmarked"' "$GATE" > "$tmp/gate-nohost.json"
expect_fail "a gate naming a host with no checked-in record" -- ./scripts/perf/check-baseline.sh --gate "$tmp/gate-nohost.json"

jq '.workload = "some-other-workload"' "$GATE" > "$tmp/gate-workload.json"
expect_fail "a gate whose workload does not match the record's" -- ./scripts/perf/check-baseline.sh --gate "$tmp/gate-workload.json"

# A record missing one required metric, once per metric: this is the guard
# that stops a --skip-image capture (or any future harness that quietly
# stops emitting something) from being accepted as a baseline.
for metric in $(jq -r '.required_metrics[]' "$GATE"); do
  if [ "$metric" = "image_size_bytes" ]; then
    expr='.metrics.image_size_bytes.value = null'
  else
    expr=".metrics.\"${metric}\".median = null"
  fi
  # check-baseline.sh resolves a record from docs/perf/baselines/<host>.json,
  # so the mutated record is written there under a throwaway host id and the
  # gate is pointed at that id. Mutating the real record in place and
  # restoring it afterwards would leave the repository wrong if this script
  # were interrupted.
  jq "$expr" "$record" > "docs/perf/baselines/selftest-tmp.json"
  jq '.benchmark_host_id = "selftest-tmp"' "$GATE" > "$tmp/gate-missing.json"
  expect_fail "a record whose $metric is null" -- ./scripts/perf/check-baseline.sh --gate "$tmp/gate-missing.json"
  rm -f "docs/perf/baselines/selftest-tmp.json"
done

echo
echo "==> compare-mode mutations, one per gated threshold"

for metric in $(jq -r '.thresholds | keys[]' "$GATE"); do
  direction=$(jq -r ".thresholds.\"${metric}\".direction" "$GATE")
  if [ "$metric" = "image_size_bytes" ]; then
    read_expr=".metrics.image_size_bytes.value"
    write_prefix=".metrics.image_size_bytes.value"
  else
    read_expr=".metrics.\"${metric}\".median"
    write_prefix=".metrics.\"${metric}\".median"
  fi

  # Well past the threshold in the losing direction: doubling (or halving)
  # clears both the ratio and any noise floor by a wide margin, so the
  # mutation tests the rule rather than the arithmetic at its boundary.
  if [ "$direction" = "lower_is_better" ]; then
    jq "${write_prefix} = (${read_expr} * 2)" "$record" > "$tmp/candidate-bad.json"
    jq "${write_prefix} = (${read_expr} * 1.001)" "$record" > "$tmp/candidate-ok.json"
  else
    jq "${write_prefix} = (${read_expr} / 2)" "$record" > "$tmp/candidate-bad.json"
    jq "${write_prefix} = (${read_expr} * 0.999)" "$record" > "$tmp/candidate-ok.json"
  fi

  expect_fail "a candidate whose $metric regressed by 2x" -- ./scripts/perf/check-baseline.sh --compare "$tmp/candidate-bad.json"
  expect_pass "a candidate whose $metric moved 0.1%" -- ./scripts/perf/check-baseline.sh --compare "$tmp/candidate-ok.json"
done

echo
echo "==> compare-mode host and workload guards"
jq '.host_id = "some-other-machine"' "$record" > "$tmp/candidate-otherhost.json"
expect_fail "a candidate captured on another machine" -- ./scripts/perf/check-baseline.sh --compare "$tmp/candidate-otherhost.json"
jq '.workload = "some-other-workload"' "$record" > "$tmp/candidate-otherload.json"
expect_fail "a candidate captured under another workload" -- ./scripts/perf/check-baseline.sh --compare "$tmp/candidate-otherload.json"

echo
if [ "$fail" -ne 0 ]; then
  echo "FAIL: $fail of $((pass + fail)) performance-gate controls did not behave as required." >&2
  exit 1
fi
echo "OK: all $pass performance-gate controls behaved as required (every threshold was shown to fail, and shown not to fail on noise)."
