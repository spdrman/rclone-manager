#!/usr/bin/env python3
"""Phase 6's performance gate (issue #165, EPIC B #81's performance
contract).

Two modes, both driven by `docs/perf/gate.json`, which is the one place
the designated benchmark host, the workload and the concrete thresholds
are written down:

  (default)          presence. Fails while no complete, checked-in
                     baseline record exists for the designated host and
                     workload. This is the check the RED step of #165
                     needed: before the baselines were captured it
                     failed, and it fails again the moment a metric goes
                     missing or the workload identifier moves.

  --compare PATH     regression. Compares a freshly captured record
                     (`python3 scripts/rcmtools/perf/capture_baseline.py
                     --out PATH`) against the checked-in baseline and
                     fails any metric outside its threshold.

It is deliberately not wired into ordinary CI in compare mode. EPIC B
#81 allows the measurements to run on a dedicated stable benchmark
environment rather than blocking ordinary CI on noisy numbers, and a
shared GitHub runner is not that environment. Presence mode has no
timing in it at all, so it is safe anywhere.

Ported from `scripts/perf/check-baseline.sh` under EPIC I (#672 / #662).
That file now execs this one: `scripts/tests/ci-local-gate.test.sh`
fabricates a stand-in AT THAT LITERAL PATH, so it stays a real, runnable
shim rather than becoming a symlink or disappearing.

Unlike the bash it replaces, this needs no `jq`: every read is
`json.loads` and every report line is built with `%`-formatting, in the
one language already on every machine this runs on.

# PORTED-CHECK HAZARD NOTE

every `$GATE`/`$record`/`$COMPARE` read is well-formed JSON
  hazard in bash:   `host_id=$(jq -r '.benchmark_host_id' "$GATE")` is a
                    command substitution under `set -e`: a $GATE holding
                    malformed JSON makes jq exit non-zero, and jq's own
                    documented exit statuses do not guarantee avoiding 3.
                    If jq ever produced exactly 3 for a parse failure, the
                    script's own `set -e` would end it with 3 -- the exact
                    number `scripts/ci-local.sh` reads as "this machine
                    could not perform the proof" -- over what is actually
                    a corrupted committed record, a real defect that
                    should FAIL the gate rather than be ledgered INCOMPLETE
                    and skipped.
  hazard in python: GONE. `json.loads` raises `json.JSONDecodeError`, a
                    Python exception with no process exit code at all;
                    `load_json` below catches it and calls `harness.die`,
                    which can only ever produce the harness's own
                    controlled statuses through `finish()`. There is no
                    code path left by which a malformed file's parser
                    error becomes this script's exit status.
  held by:          `load_json`'s except clause, and `finish()` never
                    producing 3 for anything but `harness.cannot_run`.

the completeness check cannot pass having verified nothing
  hazard in bash:   `required_metrics` or `thresholds` deleted from
                    `$GATE` would make the following loop, or the
                    following `to_entries[]`, iterate zero times -- and a
                    loop with no iterations is a check that always
                    passes.
  hazard in python: STILL EXISTS as a real Python possibility (an empty
                    list still iterates zero times), so the port keeps
                    the same explicit fail-open guards before either
                    loop runs, unchanged in shape from the bash.
  held by:          the two `required_metrics`/`thresholds` shape checks
                    in `body()`, both raising before their loop, and
                    `selftest.py`'s `gate-noreq`/`gate-nothresholds`
                    controls.

a metric missing from `required_metrics` cannot be silently un-required
  hazard in bash:   none beyond the fail-open case above; this is the
                    same rule restated per-metric.
  hazard in python: not applicable; same guard, same iteration.
  held by:          `selftest.py`'s per-metric "is missing required
                    baseline metrics" loop.

a `--compare` mode threshold fails on the wider of ratio and noise floor
  hazard in bash:   an arithmetic slip in the `and` between the two `jq`
                    conditions would silently widen or narrow every
                    gated metric's effective budget at once.
  hazard in python: STILL EXISTS as the same two-condition `and`,
                    translated line for line; `render_report` below is
                    checked against `docs/perf/gate.json`'s real
                    thresholds by `selftest.py`'s ratio-boundary controls
                    (see its own hazard note for the floor-versus-ratio
                    band specifically).
  held by:          `render_report`'s `lower_is_better`/`higher_is_better`
                    branches and every `expect_fail`/`expect_pass` pair in
                    `selftest.py` that exercises a threshold.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import harness

PROGRAM = "check-baseline"

USAGE = """\
usage: scripts/perf/check-baseline.sh [--compare PATH] [--gate PATH]
                                      [--baselines-dir PATH]

  --compare PATH        compare this freshly captured record against the
                        checked-in baseline for the designated host
  --gate PATH           read the gate definition from here instead of
                        docs/perf/gate.json
  --baselines-dir PATH  look the designated host's record up here instead of
                        docs/perf/baselines
"""


def jq_str(value: object) -> str:
    """Mirror `jq -r`'s stringification of a possibly null/missing field."""
    if value is None:
        return "null"
    return str(value)


def jq_num(value: object) -> str:
    """Mirror `jq`'s number formatting closely enough for a report line."""
    if value is None:
        return "null"
    if isinstance(value, float) and value.is_integer():
        return str(int(value))
    return str(value)


def r3(value: float | None) -> str:
    """Round to 3 decimal places, the way `check-baseline.sh`'s `r3` does."""
    if value is None:
        return "null"
    return jq_num(round(value * 1000) / 1000)


def load_json(path: Path) -> dict[str, Any]:
    """Parse `path` as a JSON object, or refuse naming which file and why."""
    try:
        data = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        harness.die(f"{path} is not valid JSON.")
    if not isinstance(data, dict):
        harness.die(f"{path} is not valid JSON.")
    return data


def metric_value(record: dict[str, Any], metric: str) -> Any:
    metrics = record.get("metrics")
    if not isinstance(metrics, dict):
        return None
    entry = metrics.get(metric)
    if not isinstance(entry, dict):
        return None
    key = "value" if metric == "image_size_bytes" else "median"
    return entry.get(key)


def metric_expr(metric: str) -> str:
    if metric == "image_size_bytes":
        return ".metrics.image_size_bytes.value"
    return f".metrics.{metric}.median"


def render_report(
    gate: dict[str, Any], base: dict[str, Any], candidate: dict[str, Any]
) -> tuple[list[tuple[str, str, str]], bool]:
    """One row per gated threshold, and whether any of them is a FAIL.

    Every threshold is evaluated and every result is a row, pass or fail:
    a report that only prints its failures cannot be reviewed, because a
    reader cannot tell an unexercised rule from a passing one.
    """
    rows: list[tuple[str, str, str]] = []
    any_fail = False
    thresholds = gate.get("thresholds", {})
    for metric, t in thresholds.items():
        b = metric_value(base, metric)
        c = metric_value(candidate, metric)
        if b is None or c is None:
            rows.append(("FAIL", metric, f"missing (baseline={jq_str(b)}, candidate={jq_str(c)})"))
            any_fail = True
            continue
        direction = t.get("direction")
        floor = t.get("noise_floor_abs") or 0
        ratio: float | None = None if b == 0 else c / b
        if direction == "lower_is_better":
            limit = b * t.get("max_ratio")
            failed = c > limit and (c - b) > floor
            cmp_word = "limit<="
        else:
            limit = b * t.get("min_ratio")
            failed = c < limit and (b - c) > floor
            cmp_word = "limit>="
        verdict = "FAIL" if failed else "pass"
        if failed:
            any_fail = True
        message = (
            f"baseline={jq_str(b)} candidate={jq_str(c)} {cmp_word}{r3(limit)} "
            f"floor={jq_num(floor)} delta={r3(c - b)} ratio={r3(ratio)}"
        )
        rows.append((verdict, metric, message))
    return rows, any_fail


def render_table(rows: list[tuple[str, str, str]]) -> str:
    """`column -t`'s alignment, close enough that every regex against this
    report (`^FAIL[[:space:]]+metric[[:space:]]`) still matches."""
    if not rows:
        return ""
    width0 = max(len(row[0]) for row in rows)
    width1 = max(len(row[1]) for row in rows)
    return "\n".join(f"{row[0]:<{width0}}  {row[1]:<{width1}}  {row[2]}" for row in rows)


def parse_args(argv: list[str]) -> tuple[Path, Path | None, Path]:
    gate = Path("docs/perf/gate.json")
    compare: Path | None = None
    baselines_dir = Path("docs/perf/baselines")
    i = 0
    while i < len(argv):
        arg = argv[i]
        if arg in ("--compare", "--gate", "--baselines-dir"):
            if i + 1 >= len(argv):
                print(f"check-baseline: {arg} needs an argument", file=sys.stderr, end="\n")
                print(USAGE, file=sys.stderr, end="")
                raise SystemExit(2)
            value = Path(argv[i + 1])
            if arg == "--compare":
                compare = value
            elif arg == "--gate":
                gate = value
            else:
                baselines_dir = value
            i += 2
        elif arg in ("-h", "--help"):
            print(USAGE, file=sys.stderr, end="")
            raise SystemExit(0)
        else:
            print(f"check-baseline: unknown option {arg}", file=sys.stderr)
            print(USAGE, file=sys.stderr, end="")
            raise SystemExit(2)
    return gate, compare, baselines_dir


def body(gate_path: Path, compare_path: Path | None, baselines_dir: Path) -> int:
    if not gate_path.is_file():
        harness.die(f"{gate_path} does not exist, so no benchmark host, workload or threshold is pinned.")

    gate = load_json(gate_path)
    host_id = jq_str(gate.get("benchmark_host_id"))
    workload = jq_str(gate.get("workload"))

    if host_id in ("null", ""):
        harness.die(f"{gate_path} names no benchmark_host_id.")

    record_path = baselines_dir / f"{host_id}.json"
    if not record_path.is_file():
        harness.die(
            "no checked-in performance baseline for the designated benchmark host.",
            f"expected: {record_path}",
            "capture it with: python3 scripts/rcmtools/perf/capture_baseline.py",
        )

    record = load_json(record_path)
    got_workload = jq_str(record.get("workload"))
    if got_workload != workload:
        harness.die(
            f'{record_path} was captured under workload "{got_workload}", but {gate_path} pins "{workload}".',
            "A baseline from a different workload is not comparable. Re-capture, or correct the gate.",
        )

    # Completeness. Every metric the performance contract names must be
    # present and non-null, so a record that silently dropped one (a
    # --skip-image capture, say) can never pass as a baseline.
    #
    # The gate's own list is checked first. Without this, deleting
    # required_metrics from gate.json would make the loop below iterate
    # zero times and this check would print OK having verified nothing.
    required = gate.get("required_metrics")
    if not (isinstance(required, list) and len(required) > 0):
        harness.die(
            f"{gate_path} declares no required_metrics, so the completeness check would verify nothing and pass."
        )

    missing = []
    for metric in required:
        value = metric_value(record, metric)
        if value is None or value == "":
            missing.append(f"{metric} ({metric_expr(metric)} is null or absent)")
    if missing:
        harness.die(f"{record_path} is missing required baseline metrics:", *missing)

    if compare_path is None:
        print(f"OK: baseline present and complete for {host_id} under workload {workload} ({record_path}).")
        return harness.EXIT_OK

    # ---- compare mode ---------------------------------------------------

    if not compare_path.is_file():
        harness.die(f"{compare_path} does not exist.")

    candidate = load_json(compare_path)
    candidate_host = jq_str(candidate.get("host_id"))
    if candidate_host != host_id:
        harness.die(
            f'{compare_path} was captured on "{candidate_host}", not the designated benchmark host "{host_id}".',
            "Comparing across machines reports the machine, not the change.",
        )
    candidate_workload = jq_str(candidate.get("workload"))
    if candidate_workload != workload:
        harness.die(f'{compare_path} was captured under workload "{candidate_workload}", not "{workload}".')

    # Same fail-open guard as the completeness check above: an empty
    # thresholds object would produce an empty report and a cheerful
    # "every gated metric is within its threshold" having compared nothing.
    thresholds = gate.get("thresholds")
    if not (isinstance(thresholds, dict) and len(thresholds) > 0):
        harness.die(f"{gate_path} declares no thresholds, so compare mode would gate nothing and pass.")

    rows, any_fail = render_report(gate, record, candidate)
    table = render_table(rows)
    if table:
        print(table)

    if any_fail:
        harness.die(
            "at least one metric is outside its threshold (see the FAIL rows above).",
            "Thresholds and their justification: docs/perf/README.md",
        )

    print()
    print(f"OK: every gated metric is within its threshold against {record_path}.")
    return harness.EXIT_OK


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    gate_path, compare_path, baselines_dir = parse_args(argv)
    return harness.finish(lambda: body(gate_path, compare_path, baselines_dir))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
