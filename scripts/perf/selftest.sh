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
#
# Three things this file gets wrong are worth naming, because they are the
# shapes it is supposed to be guarding against and it had them itself:
#
#   - expect_fail counted ANY non-zero exit as "refused as required", so a
#     mutation that made the gate crash before it compared anything was a
#     passing control. It now takes the reason it expects and asserts it,
#     the way scripts/architecture/selftest.sh's expect_check_fails already
#     did, and there is a control below proving that assertion is load-bearing.
#   - the metric lists were read out of the very gate.json a regression would
#     be hidden in, so deleting a metric from it silently stopped gating that
#     metric AND silently stopped testing it. The lists are pinned here now,
#     gate.json is asserted to match them, and the total control count is
#     pinned too, so a shrink is a red line rather than a smaller number in a
#     log nobody diffs.
#   - the mutated record was written into docs/perf/baselines/, the directory
#     whose entire purpose is to be authoritative, and cleaned up outside the
#     EXIT trap. check-baseline.sh now takes --baselines-dir, so nothing here
#     writes under docs/ at all.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

command -v jq >/dev/null 2>&1 || { echo "perf selftest: jq is required" >&2; exit 1; }

GATE=docs/perf/gate.json
BASELINES=docs/perf/baselines
host_id=$(jq -r '.benchmark_host_id' "$GATE")
record="$BASELINES/${host_id}.json"

# Pinned rather than read out of the gate. See the header: a case list derived
# from the file under test cannot notice that file losing a case.
PINNED_GATED="api_read_p95_ms config_write_p95_ms idle_rss_bytes image_size_bytes transfer_mb_per_second"
PINNED_UNGATED="idle_cpu_seconds_total startup_to_healthy_ms"

# The number of controls this file must run, pinned for the same reason. It
# is 2 negative controls + 4 presence-mode gate mutations + one per required
# metric + two per gated metric + two floor-boundary controls + 2 compare-mode
# provenance guards + 1 harness control + 1 tracked-tree control.
EXPECTED_CONTROLS=29

tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-perf-selftest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# What docs/perf/baselines held when this run started. Asserted again at the
# end: this script must leave it exactly as it found it.
baselines_before=$(ls "$BASELINES" | sort)

pass=0
fail=0

# ---- the gate's own shape, before any control runs ------------------------
#
# These are preconditions rather than controls: every loop below iterates a
# pinned list, so if gate.json has drifted from those lists the controls would
# still all pass while testing something other than the gate that ships.

# Unquoted on purpose: the argument is a space-separated list that has to
# word-split before it can be sorted. Both sides of every comparison below
# go through this, so the two are compared as sets rather than in whatever
# order each source happened to list them.
sorted() { printf '%s\n' $1 | sort | tr '\n' ' '; }

want_gated=$(sorted "$PINNED_GATED")
got_gated=$(jq -r '.thresholds | keys[]' "$GATE" | sort | tr '\n' ' ')
if [ "$want_gated" != "$got_gated" ]; then
  echo "FAIL: $GATE gates a different set of metrics than this self-test pins." >&2
  echo "  pinned here: $want_gated" >&2
  echo "  in the gate: $got_gated" >&2
  echo "  A metric dropped from thresholds stops being gated. Update both, deliberately, or put it back." >&2
  exit 1
fi

want_required=$(sorted "$PINNED_GATED $PINNED_UNGATED")
got_required=$(jq -r '.required_metrics[]' "$GATE" | sort | tr '\n' ' ')
if [ "$want_required" != "$got_required" ]; then
  echo "FAIL: $GATE requires a different set of metrics than this self-test pins." >&2
  echo "  pinned here: $want_required" >&2
  echo "  in the gate: $got_required" >&2
  echo "  Every required metric must either carry a threshold or be listed above as deliberately ungated." >&2
  exit 1
fi

# ---- harness --------------------------------------------------------------

# refused_with <expected-regex> -- <command...>
#   0  the command failed AND said why
#   1  the command succeeded
#   2  the command failed for some other reason
# Split out of expect_fail so the expected-reason assertion can itself be
# controlled below.
refused_with() {
  local expect=$1; shift; shift
  if "$@" >"$tmp/out" 2>&1; then
    return 1
  fi
  grep -qE "$expect" "$tmp/out" || return 2
  return 0
}

# expect_fail <name> <expected-regex> -- <command...>
expect_fail() {
  local name=$1 expect=$2; shift 2
  local rc=0
  refused_with "$expect" "$@" || rc=$?
  case "$rc" in
    0)
      echo "  ok (refused as required): $name"
      pass=$((pass + 1))
      ;;
    1)
      echo "SELFTEST FAIL: $name. The gate PASSED a case it must refuse." >&2
      sed 's/^/    /' "$tmp/out" >&2
      fail=$((fail + 1))
      ;;
    *)
      echo "SELFTEST FAIL: $name. The gate refused, but not for the required reason." >&2
      echo "    expected its output to match: $expect" >&2
      sed 's/^/    /' "$tmp/out" >&2
      fail=$((fail + 1))
      ;;
  esac
}

# expect_pass <name> -- <command...>
expect_pass() {
  local name=$1; shift; shift
  if "$@" >"$tmp/out" 2>&1; then
    echo "  ok (accepted as required): $name"
    pass=$((pass + 1))
  else
    echo "SELFTEST FAIL: $name. The gate REFUSED a case it must accept." >&2
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
expect_fail "a gate naming a host with no checked-in record" \
  "no checked-in performance baseline" \
  -- ./scripts/perf/check-baseline.sh --gate "$tmp/gate-nohost.json"

jq '.workload = "some-other-workload"' "$GATE" > "$tmp/gate-workload.json"
expect_fail "a gate whose workload does not match the record's" \
  "A baseline from a different workload is not comparable" \
  -- ./scripts/perf/check-baseline.sh --gate "$tmp/gate-workload.json"

# The two fail-open shapes: a gate that lists nothing must refuse rather
# than report success over an empty list. Phase 4 learned this on the
# conformance matrix, where omitting a capability had to fail rather than
# shrink the matrix.
jq 'del(.required_metrics)' "$GATE" > "$tmp/gate-noreq.json"
expect_fail "a gate declaring no required_metrics" \
  "declares no required_metrics" \
  -- ./scripts/perf/check-baseline.sh --gate "$tmp/gate-noreq.json"

jq '.thresholds = {}' "$GATE" > "$tmp/gate-nothresholds.json"
expect_fail "a gate declaring no thresholds" \
  "declares no thresholds" \
  -- ./scripts/perf/check-baseline.sh --gate "$tmp/gate-nothresholds.json" --compare "$record"

# A record missing one required metric, once per metric: this is the guard
# that stops a --skip-image capture (or any future harness that quietly
# stops emitting something) from being accepted as a baseline.
#
# The mutated record goes in $tmp under a throwaway host id, and
# --baselines-dir points the gate at it. Nothing is written under docs/.
for metric in $PINNED_GATED $PINNED_UNGATED; do
  if [ "$metric" = "image_size_bytes" ]; then
    expr='.metrics.image_size_bytes.value = null'
  else
    expr=".metrics.\"${metric}\".median = null"
  fi
  jq "$expr" "$record" > "$tmp/selftest-tmp.json"
  jq '.benchmark_host_id = "selftest-tmp"' "$GATE" > "$tmp/gate-missing.json"
  expect_fail "a record whose $metric is null" \
    "is missing required baseline metrics" \
    -- ./scripts/perf/check-baseline.sh --gate "$tmp/gate-missing.json" --baselines-dir "$tmp"
done

echo
echo "==> compare-mode mutations, one per gated threshold"

for metric in $PINNED_GATED; do
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
  # mutation tests the rule rather than the arithmetic at its boundary. The
  # boundary itself is exercised separately, below.
  if [ "$direction" = "lower_is_better" ]; then
    jq "${write_prefix} = (${read_expr} * 2)" "$record" > "$tmp/candidate-bad.json"
    jq "${write_prefix} = (${read_expr} * 1.001)" "$record" > "$tmp/candidate-ok.json"
  else
    jq "${write_prefix} = (${read_expr} / 2)" "$record" > "$tmp/candidate-bad.json"
    jq "${write_prefix} = (${read_expr} * 0.999)" "$record" > "$tmp/candidate-ok.json"
  fi

  expect_fail "a candidate whose $metric regressed by 2x" \
    "^FAIL[[:space:]]+${metric}[[:space:]]" \
    -- ./scripts/perf/check-baseline.sh --compare "$tmp/candidate-bad.json"
  expect_pass "a candidate whose $metric moved 0.1%" -- ./scripts/perf/check-baseline.sh --compare "$tmp/candidate-ok.json"
done

echo
echo "==> the band between the ratio limit and the noise floor"

# api_read_p95_ms is the one metric carrying both a ratio and an absolute
# noise floor, and both conditions must hold before it fails. That means the
# floor binds first and the effective budget is much wider than the ratio
# suggests, which docs/perf/gate.json and docs/perf/README.md now say
# explicitly. Nothing exercised the region in between, which is exactly where
# a small structural regression lands, so these two controls sit either side
# of the floor: one just under it that must be accepted, one just over it
# that must be refused.
api_b=$(jq -r '.metrics.api_read_p95_ms.median' "$record")
api_floor=$(jq -r '.thresholds.api_read_p95_ms.noise_floor_abs' "$GATE")
api_ratio=$(jq -r '.thresholds.api_read_p95_ms.max_ratio' "$GATE")

# The "just under the floor" candidate is only meaningful while it is ALSO
# over the ratio limit, which is what makes the floor the binding condition.
# If a re-measurement ever changes that, this says so rather than quietly
# testing nothing.
if [ "$(jq -n --argjson b "$api_b" --argjson f "$api_floor" --argjson r "$api_ratio" \
        '(($b + $f * 0.99) > ($b * $r))')" != "true" ]; then
  echo "FAIL: the api_read_p95_ms noise floor no longer binds before the ratio." >&2
  echo "  baseline=$api_b floor=$api_floor max_ratio=$api_ratio" >&2
  echo "  These two controls exist to pin which condition binds; re-derive them, and fix the claim in $GATE and docs/perf/README.md at the same time." >&2
  exit 1
fi

jq --argjson b "$api_b" --argjson f "$api_floor" \
  '.metrics.api_read_p95_ms.median = ($b + $f * 0.99)' "$record" > "$tmp/candidate-under-floor.json"
jq --argjson b "$api_b" --argjson f "$api_floor" \
  '.metrics.api_read_p95_ms.median = ($b + $f * 1.01)' "$record" > "$tmp/candidate-over-floor.json"

expect_pass "a candidate over the api_read_p95_ms ratio limit but inside the noise floor" \
  -- ./scripts/perf/check-baseline.sh --compare "$tmp/candidate-under-floor.json"
expect_fail "a candidate just past the api_read_p95_ms noise floor" \
  "^FAIL[[:space:]]+api_read_p95_ms[[:space:]]" \
  -- ./scripts/perf/check-baseline.sh --compare "$tmp/candidate-over-floor.json"

echo
echo "==> compare-mode host and workload guards"
jq '.host_id = "some-other-machine"' "$record" > "$tmp/candidate-otherhost.json"
expect_fail "a candidate captured on another machine" \
  "not the designated benchmark host" \
  -- ./scripts/perf/check-baseline.sh --compare "$tmp/candidate-otherhost.json"
jq '.workload = "some-other-workload"' "$record" > "$tmp/candidate-otherload.json"
expect_fail "a candidate captured under another workload" \
  "was captured under workload" \
  -- ./scripts/perf/check-baseline.sh --compare "$tmp/candidate-otherload.json"

echo
echo "==> controls on this file's own controls"

# The expected-reason argument is what stops a gate that fell over before it
# compared anything from counting as a refusal. So: a real refusal, asserted
# against a reason the gate never prints, must NOT be counted as a pass. If
# this control ever goes green the wrong way, every expect_fail above proves
# only that something went wrong somewhere.
if refused_with "a reason this gate never prints" -- ./scripts/perf/check-baseline.sh --gate "$tmp/gate-nohost.json"; then
  echo "SELFTEST FAIL: the harness accepted a refusal that did not match the expected reason." >&2
  fail=$((fail + 1))
else
  echo "  ok (rejected as required): a refusal for the wrong reason does not count as a control"
  pass=$((pass + 1))
fi

# Nothing here may write into the tracked baselines directory. A mutated
# record with a required metric nulled is exactly the artifact this phase asks
# everyone to trust was not re-cut, and it used to be written there and
# removed outside the EXIT trap, so an interrupted run left one behind.
baselines_after=$(ls "$BASELINES" | sort)
if [ "$baselines_after" = "$baselines_before" ]; then
  echo "  ok (untouched as required): $BASELINES is exactly as this run found it"
  pass=$((pass + 1))
else
  echo "SELFTEST FAIL: this script changed the contents of $BASELINES." >&2
  echo "    before: $(printf '%s' "$baselines_before" | tr '\n' ' ')" >&2
  echo "    after:  $(printf '%s' "$baselines_after" | tr '\n' ' ')" >&2
  fail=$((fail + 1))
fi

echo
if [ "$fail" -ne 0 ]; then
  echo "FAIL: $fail of $((pass + fail)) performance-gate controls did not behave as required." >&2
  exit 1
fi

total=$((pass + fail))
if [ "$total" -ne "$EXPECTED_CONTROLS" ]; then
  echo "FAIL: $total controls ran, but this file pins $EXPECTED_CONTROLS." >&2
  echo "  A control count that can shrink without anyone noticing is the whole failure this pin exists to catch." >&2
  exit 1
fi
echo "OK: all $pass performance-gate controls behaved as required, which is the pinned count (every threshold was shown to fail, shown not to fail on noise, and the band between the ratio limit and the noise floor was visited from both sides)."
