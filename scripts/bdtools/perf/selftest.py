#!/usr/bin/env python3
"""Positive controls for the performance gate (issue #165).

Every assertion `scripts/perf/check-baseline.sh` makes is a negative one
("no metric regressed", "no metric is missing"), and a negative assertion
that has never been seen to fail is indistinguishable from one that
cannot. This script mutates a real baseline record, one property at a
time, and requires the gate to fail for each mutation and to pass for the
unmutated original and for a mutation that stays just inside a threshold.

It runs in a few seconds and takes no measurements of its own, so it is
safe in ordinary CI even though the capture harness is not.

Ported from `scripts/perf/selftest.sh` under EPIC I (#672 / #662). That
file now execs this one: `scripts/tests/ci-local-gate.test.sh` fabricates
a stand-in AT THAT LITERAL PATH, so it stays a real, runnable shim.

# PORTED-CHECK HAZARD NOTE

expect_fail asserts the REASON the gate refused, not just that it refused
  hazard in bash:   an earlier version of this file counted any non-zero
                    exit as "refused as required", so a mutation that made
                    the gate crash before comparing anything was a passing
                    control. Fixed before the port, in bash, and the fix
                    itself is what this port has to keep.
  hazard in python: STILL EXISTS as the thing this file is FOR: every
                    `expect_fail` call below takes an expected-reason regex
                    and `refused_with` asserts it with `re.search`.
  held by:          the "controls on this file's own controls" section,
                    which asserts that a refusal for the WRONG reason is
                    not counted as a pass -- proven by construction:
                    `refused_with` returns a distinct code (2) for that
                    case, and its one caller in that section requires it.

the pinned metric lists and the pinned control count cannot silently shrink
  hazard in bash:   the metric lists used to be read out of the very
                    `gate.json` a regression would be hidden in, so
                    deleting a metric from it silently stopped gating that
                    metric AND silently stopped testing it.
  hazard in python: STILL EXISTS as a real possibility -- PINNED_GATED,
                    PINNED_UNGATED and EXPECTED_CONTROLS are literal
                    constants here too, not derived from the gate.
  held by:          the two precondition checks at the top of `main()`
                    (gate.json's thresholds/required_metrics must equal
                    the pinned sets) and the `total != EXPECTED_CONTROLS`
                    check at the end, both unchanged in shape from bash.

this script leaves docs/perf/baselines exactly as it found it
  hazard in bash:   the mutated record used to be written into
                    docs/perf/baselines/ itself and deleted outside the
                    EXIT trap, so an interrupted run left a falsified
                    baseline (one required metric nulled) in the one
                    directory whose entire purpose is to be authoritative.
  hazard in python: STILL EXISTS as a real possibility if a future edit
                    reintroduces a write under docs/; every mutated file
                    in this port is written under `tempfile.mkdtemp()`
                    (never cleaned up outside a `finally`, unlike the bash
                    EXIT trap workaround this replaces) and
                    `check-baseline.sh --baselines-dir` points reads at it.
  held by:          the before/after directory-listing comparison at the
                    end of `main()`, and the `finally: shutil.rmtree(tmp)`
                    around every control.
"""

from __future__ import annotations

import json
import re
import sys
import tempfile
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import harness

PROGRAM = "perf selftest"

GATE = Path("docs/perf/gate.json")
BASELINES = Path("docs/perf/baselines")
CHECK_BASELINE = Path("scripts/perf/check-baseline.sh")

# Pinned rather than read out of the gate. See the module docstring: a case
# list derived from the file under test cannot notice that file losing a
# case.
PINNED_GATED = [
    "api_read_p95_ms",
    "config_write_p95_ms",
    "idle_rss_bytes",
    "image_size_bytes",
    "transfer_mb_per_second",
]
PINNED_UNGATED = ["idle_cpu_seconds_total", "startup_to_healthy_ms"]

# 2 negative controls + 4 presence-mode gate mutations + one per required
# metric + two per gated metric + two floor-boundary controls + 2
# compare-mode provenance guards + 1 harness control + 1 tracked-tree
# control.
EXPECTED_CONTROLS = 29


class Tally:
    def __init__(self) -> None:
        self.passed = 0
        self.failed = 0


def load_json(path: Path) -> dict[str, Any]:
    data: dict[str, Any] = json.loads(path.read_text())
    return data


def write_json(path: Path, data: object) -> None:
    path.write_text(json.dumps(data))


def metric_field(metric: str) -> str:
    return "value" if metric == "image_size_bytes" else "median"


def set_metric(record: dict[str, Any], metric: str, value: Any) -> dict[str, Any]:
    out: dict[str, Any] = json.loads(json.dumps(record))  # deep copy through JSON, like jq's own value semantics
    out.setdefault("metrics", {}).setdefault(metric, {})[metric_field(metric)] = value
    return out


def get_metric(record: dict[str, Any], metric: str) -> Any:
    return record.get("metrics", {}).get(metric, {}).get(metric_field(metric))


def run(extra_args: list[str], root: Path) -> tuple[int, str]:
    proc = harness.sh([str(root / CHECK_BASELINE), *extra_args], check=False, capture=True, cwd=root)
    return proc.returncode, (proc.stdout or "") + (proc.stderr or "")


def refused_with(expect: str, argv: list[str], root: Path) -> tuple[int, str]:
    """0 refused, and said why. 1 succeeded. 2 refused for a different reason."""
    status, out = run(argv, root)
    if status == 0:
        return 1, out
    if not re.search(expect, out, re.MULTILINE):
        return 2, out
    return 0, out


def indent(text: str) -> str:
    return "\n".join(f"    {line}" for line in text.splitlines())


def expect_fail(tally: Tally, name: str, expect: str, argv: list[str], root: Path) -> None:
    rc, out = refused_with(expect, argv, root)
    if rc == 0:
        print(f"  ok (refused as required): {name}")
        tally.passed += 1
    elif rc == 1:
        print(f"SELFTEST FAIL: {name}. The gate PASSED a case it must refuse.", file=sys.stderr)
        print(indent(out), file=sys.stderr)
        tally.failed += 1
    else:
        print(f"SELFTEST FAIL: {name}. The gate refused, but not for the required reason.", file=sys.stderr)
        print(f"    expected its output to match: {expect}", file=sys.stderr)
        print(indent(out), file=sys.stderr)
        tally.failed += 1


def expect_pass(tally: Tally, name: str, argv: list[str], root: Path) -> None:
    status, out = run(argv, root)
    if status == 0:
        print(f"  ok (accepted as required): {name}")
        tally.passed += 1
    else:
        print(f"SELFTEST FAIL: {name}. The gate REFUSED a case it must accept.", file=sys.stderr)
        print(indent(out), file=sys.stderr)
        tally.failed += 1


def body(root: Path) -> int:
    tally = Tally()

    gate = load_json(root / GATE)
    host_id = gate.get("benchmark_host_id")
    record_path = root / BASELINES / f"{host_id}.json"
    record = load_json(record_path)

    # ---- the gate's own shape, before any control runs --------------------
    #
    # These are preconditions rather than controls: every loop below iterates
    # a pinned list, so if gate.json has drifted from those lists the
    # controls would still all pass while testing something other than the
    # gate that ships.
    want_gated = sorted(PINNED_GATED)
    got_gated = sorted(gate.get("thresholds", {}).keys())
    if want_gated != got_gated:
        harness.die(
            f"{GATE} gates a different set of metrics than this self-test pins.",
            f"pinned here: {' '.join(want_gated)}",
            f"in the gate: {' '.join(got_gated)}",
            "A metric dropped from thresholds stops being gated. Update both, deliberately, or put it back.",
        )

    want_required = sorted(PINNED_GATED + PINNED_UNGATED)
    got_required = sorted(gate.get("required_metrics", []))
    if want_required != got_required:
        harness.die(
            f"{GATE} requires a different set of metrics than this self-test pins.",
            f"pinned here: {' '.join(want_required)}",
            f"in the gate: {' '.join(got_required)}",
            "Every required metric must either carry a threshold or be listed above as deliberately ungated.",
        )

    baselines_before = sorted(p.name for p in (root / BASELINES).iterdir())

    with tempfile.TemporaryDirectory(prefix="backupd-perf-selftest-") as tmp_str:
        tmp = Path(tmp_str)

        print("==> negative control: the real baseline and gate as checked in")
        expect_pass(tally, "presence mode against the checked-in record", [], root)
        expect_pass(tally, "compare mode against the record itself", ["--compare", str(record_path)], root)

        print()
        print("==> presence-mode mutations")

        gate_nohost = {**gate, "benchmark_host_id": "a-host-that-was-never-benchmarked"}
        write_json(tmp / "gate-nohost.json", gate_nohost)
        expect_fail(
            tally,
            "a gate naming a host with no checked-in record",
            "no checked-in performance baseline",
            ["--gate", str(tmp / "gate-nohost.json")],
            root,
        )

        gate_workload = {**gate, "workload": "some-other-workload"}
        write_json(tmp / "gate-workload.json", gate_workload)
        expect_fail(
            tally,
            "a gate whose workload does not match the record's",
            "A baseline from a different workload is not comparable",
            ["--gate", str(tmp / "gate-workload.json")],
            root,
        )

        # The two fail-open shapes: a gate that lists nothing must refuse
        # rather than report success over an empty list.
        gate_noreq = {k: v for k, v in gate.items() if k != "required_metrics"}
        write_json(tmp / "gate-noreq.json", gate_noreq)
        expect_fail(
            tally,
            "a gate declaring no required_metrics",
            "declares no required_metrics",
            ["--gate", str(tmp / "gate-noreq.json")],
            root,
        )

        gate_nothresholds = {**gate, "thresholds": {}}
        write_json(tmp / "gate-nothresholds.json", gate_nothresholds)
        expect_fail(
            tally,
            "a gate declaring no thresholds",
            "declares no thresholds",
            ["--gate", str(tmp / "gate-nothresholds.json"), "--compare", str(record_path)],
            root,
        )

        # A record missing one required metric, once per metric: the guard
        # that stops a --skip-image capture (or any future harness that
        # quietly stops emitting something) from being accepted as a
        # baseline. Written under tmp, never under docs/.
        for metric in PINNED_GATED + PINNED_UNGATED:
            mutated = set_metric(record, metric, None)
            write_json(tmp / "selftest-tmp.json", mutated)
            gate_missing = {**gate, "benchmark_host_id": "selftest-tmp"}
            write_json(tmp / "gate-missing.json", gate_missing)
            expect_fail(
                tally,
                f"a record whose {metric} is null",
                "is missing required baseline metrics",
                ["--gate", str(tmp / "gate-missing.json"), "--baselines-dir", str(tmp)],
                root,
            )

        print()
        print("==> compare-mode mutations, one per gated threshold")

        for metric in PINNED_GATED:
            direction = gate["thresholds"][metric]["direction"]
            base_value = get_metric(record, metric)
            if direction == "lower_is_better":
                bad = set_metric(record, metric, base_value * 2)
                ok = set_metric(record, metric, base_value * 1.001)
            else:
                bad = set_metric(record, metric, base_value / 2)
                ok = set_metric(record, metric, base_value * 0.999)
            write_json(tmp / "candidate-bad.json", bad)
            write_json(tmp / "candidate-ok.json", ok)

            expect_fail(
                tally,
                f"a candidate whose {metric} regressed by 2x",
                rf"^FAIL[ \t]+{re.escape(metric)}[ \t]",
                ["--compare", str(tmp / "candidate-bad.json")],
                root,
            )
            expect_pass(
                tally,
                f"a candidate whose {metric} moved 0.1%",
                ["--compare", str(tmp / "candidate-ok.json")],
                root,
            )

        print()
        print("==> the band between the ratio limit and the noise floor")

        # api_read_p95_ms is the one metric carrying both a ratio and an
        # absolute noise floor, and both conditions must hold before it
        # fails. These two controls sit either side of the floor: one just
        # under it that must be accepted, one just over it that must be
        # refused.
        api_b = get_metric(record, "api_read_p95_ms")
        api_floor = gate["thresholds"]["api_read_p95_ms"]["noise_floor_abs"]
        api_ratio = gate["thresholds"]["api_read_p95_ms"]["max_ratio"]

        if not (api_b + api_floor * 0.99) > (api_b * api_ratio):
            harness.die(
                "the api_read_p95_ms noise floor no longer binds before the ratio.",
                f"baseline={api_b} floor={api_floor} max_ratio={api_ratio}",
                "These two controls exist to pin which condition binds; re-derive them, and fix the claim in "
                f"{GATE} and docs/perf/README.md at the same time.",
            )

        write_json(tmp / "candidate-under-floor.json", set_metric(record, "api_read_p95_ms", api_b + api_floor * 0.99))
        write_json(tmp / "candidate-over-floor.json", set_metric(record, "api_read_p95_ms", api_b + api_floor * 1.01))

        expect_pass(
            tally,
            "a candidate over the api_read_p95_ms ratio limit but inside the noise floor",
            ["--compare", str(tmp / "candidate-under-floor.json")],
            root,
        )
        expect_fail(
            tally,
            "a candidate just past the api_read_p95_ms noise floor",
            r"^FAIL[ \t]+api_read_p95_ms[ \t]",
            ["--compare", str(tmp / "candidate-over-floor.json")],
            root,
        )

        print()
        print("==> compare-mode host and workload guards")
        write_json(tmp / "candidate-otherhost.json", {**record, "host_id": "some-other-machine"})
        expect_fail(
            tally,
            "a candidate captured on another machine",
            "not the designated benchmark host",
            ["--compare", str(tmp / "candidate-otherhost.json")],
            root,
        )
        write_json(tmp / "candidate-otherload.json", {**record, "workload": "some-other-workload"})
        expect_fail(
            tally,
            "a candidate captured under another workload",
            "was captured under workload",
            ["--compare", str(tmp / "candidate-otherload.json")],
            root,
        )

        print()
        print("==> controls on this file's own controls")

        # A real refusal, asserted against a reason the gate never prints,
        # must NOT be counted as a pass. If this control ever goes green the
        # wrong way, every expect_fail above proves only that something went
        # wrong somewhere.
        rc, _ = refused_with("a reason this gate never prints", ["--gate", str(tmp / "gate-nohost.json")], root)
        if rc == 0:
            print(
                "SELFTEST FAIL: the harness accepted a refusal that did not match the expected reason.",
                file=sys.stderr,
            )
            tally.failed += 1
        else:
            print("  ok (rejected as required): a refusal for the wrong reason does not count as a control")
            tally.passed += 1

    # Nothing here may write into the tracked baselines directory.
    baselines_after = sorted(p.name for p in (root / BASELINES).iterdir())
    if baselines_after == baselines_before:
        print(f"  ok (untouched as required): {BASELINES} is exactly as this run found it")
        tally.passed += 1
    else:
        print(f"SELFTEST FAIL: this script changed the contents of {BASELINES}.", file=sys.stderr)
        print(f"    before: {' '.join(baselines_before)}", file=sys.stderr)
        print(f"    after:  {' '.join(baselines_after)}", file=sys.stderr)
        tally.failed += 1

    print()
    if tally.failed != 0:
        harness.die(
            f"{tally.failed} of {tally.passed + tally.failed} performance-gate controls did not behave as required."
        )

    total = tally.passed + tally.failed
    if total != EXPECTED_CONTROLS:
        harness.die(
            f"{total} controls ran, but this file pins {EXPECTED_CONTROLS}.",
            "A control count that can shrink without anyone noticing is the whole failure this pin exists to catch.",
        )
    print(
        f"OK: all {tally.passed} performance-gate controls behaved as required, which is the pinned count "
        "(every threshold was shown to fail, shown not to fail on noise, and the band between the ratio limit "
        "and the noise floor was visited from both sides)."
    )
    return harness.EXIT_OK


def main() -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    root = harness.repo_root(Path(__file__))
    return harness.finish(lambda: body(root))


if __name__ == "__main__":
    sys.exit(main())
