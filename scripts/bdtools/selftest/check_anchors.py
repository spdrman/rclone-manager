#!/usr/bin/env python3
"""Are the mutation anchors in the selftests still anchored to real code?

`scripts/compat/selftest.sh`, `scripts/conformance/selftest.sh`,
`scripts/race/selftest.sh`, `scripts/format/selftest.sh`,
`scripts/docs/selftest.sh` and `scripts/retention/selftest.sh` plant
deliberate violations to prove each cell of their gate can go red. Every
plant is anchored to a verbatim copy of product source living in a
script the author of the product change never opens, so a refactor
drifts the anchor and the mutation stops planting anything. That is
caught, loudly, but until #458 it was only caught at the end of a
25-minute gate run, one stale anchor at a time.

This is that same check with nothing else attached: every anchor in
every one of them, dry-run against the real tree, building nothing, in
about a second. Belongs at the top of the gate, so drift costs seconds.

Exit code contract, which is all a gate step needs from it:

  0        every anchor is present exactly once
  non-zero at least one is not, and the run printed which ones

Every one of the six always runs, even when the first one has stale
anchors, so one run gives the whole list. Fixing them one gate at a time
is the thing this script exists to stop.

Each of the six is invoked exactly as `scripts/ci-local.sh` invokes it --
`bash scripts/<domain>/selftest.sh --check-anchors` -- rather than by
importing its Python module directly, deliberately: a domain still
mid-port is a real bash file at that path and a ported one is a shim
that execs Python underneath it, and this file must not care which,
because caring is how six domains porting concurrently would end up
coupled to each other's internals through this one aggregator. Whichever
domains have not ported yet stay real, runnable bash.

Ported from `scripts/selftest/check-anchors.sh` under EPIC I (#672 /
#458). `scripts/selftest/check-anchors.sh` stays as a real, runnable
shim: `scripts/ci-local.sh` runs `bash scripts/selftest/check-anchors.sh`
by that literal path, and `scripts/tests/ci-local-gate.test.sh`
fabricates a stand-in there.

# PORTED-CHECK HAZARD NOTE

a domain's `--check-anchors` exit status must be READ, not merely produced
  hazard in bash:   this file's own `set -uo pipefail` does not include
                     `-e`, on purpose (the header comment on the loop
                     says why: every selftest must run even when an
                     earlier one is stale), so an unguarded
                     `bash "$selftest" --check-anchors` would not even
                     abort the script -- it would just discard the
                     status and fall through to the next selftest,
                     silently. The bash avoids that with an explicit
                     `if ! bash "$selftest" --check-anchors; then
                     status=1; fi`: the guard is deliberate, not a side
                     effect of a shell option.
  hazard in python: STILL EXISTS, in exactly the same shape and for
                     exactly the same reason: `subprocess.run` (which is
                     what `harness.sh` calls) returns a
                     `CompletedProcess` and propagates nothing on its
                     own. This file's `check=False` on every one of the
                     six calls is deliberate for the same reason the bash
                     `if` is deliberate -- an aggregator that runs six
                     subprocesses and never reads one `.returncode` is
                     green forever across all six domains at once, which
                     is the single highest-leverage vacuous check it is
                     possible to write in this repository.
  held by:          `run_one()`'s explicit `proc.returncode`, and
                     `body()`'s `if code != 0: failing.append(selftest)`
                     reading it for every one of the six before any
                     verdict is produced.

one stale domain must not stop the rest from being checked in the same run
  hazard in bash:   the loop has no `break` and no `exit` inside it --
                     every one of the six `bash "$selftest"
                     --check-anchors` calls happens even when an earlier
                     one set `status=1`, which is the entire point of
                     #458: one run names every stale control instead of
                     a gate finding them one at a time, 25 minutes apart.
  hazard in python: STILL EXISTS as a real way to reintroduce #458 one
                     level up: a `body()` that returned
                     `harness.EXIT_FAILED` as soon as the first `code !=
                     0` was seen would check six domains in name only,
                     since only the first stale one would ever be
                     reported and the other five would never run this
                     gate at all that day.
  held by:          `body()`'s `for selftest in SELFTESTS` running to
                     completion unconditionally, accumulating every
                     failing name in `failing` before a single verdict
                     line is printed.

the list of six subjects cannot silently shrink to zero or drop a domain
  hazard in bash:   none directly -- `for selftest in a b c d e f; do
                     ...; done` over a fixed, inline word list has no
                     failure mode where the list is empty without the
                     literal text of the script itself changing, and a
                     diff to that text is visible in review.
  hazard in python: a real possibility this port has to guard against
                     that bash's inline word list did not need to:
                     `SELFTESTS` is a module-level tuple here, one level
                     removed from the literal loop the way
                     `scripts/bdtools/perf/selftest.py`'s `PINNED_GATED`
                     is one level removed from `gate.json` -- a future
                     edit could build it from a config file, an
                     environment variable, or a glob, any of which can
                     silently produce fewer entries than six without the
                     loop itself changing at all, and a `for` over zero
                     items is a clean, silent, zero-second pass.
  held by:          `EXPECTED_SELFTESTS = 6`, pinned as a literal
                     independent of `len(SELFTESTS)`, and the
                     `ran != EXPECTED_SELFTESTS` check in `body()` that
                     refuses rather than reporting OK for a run that
                     checked fewer domains than it claims to.
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import harness

PROGRAM = "selftest check-anchors"

# The six selftests with anchors of their own. Not architecture or api:
# neither uses the selftest_swap anchor pattern, and adding a domain with
# no anchors to an anchor checker is a row that always passes -- a new
# vacuous check created inside the very port whose subject is vacuous
# checks. A domain joins this list the day it grows anchors, with the
# anchors as the evidence.
SELFTESTS = (
    "scripts/compat/selftest.sh",
    "scripts/conformance/selftest.sh",
    "scripts/race/selftest.sh",
    "scripts/format/selftest.sh",
    "scripts/docs/selftest.sh",
    "scripts/retention/selftest.sh",
)

# Pinned independent of len(SELFTESTS) -- see this file's hazard note.
EXPECTED_SELFTESTS = 6


def run_one(root: Path, selftest: str) -> int:
    """`bash <selftest> --check-anchors`, output inherited straight
    through to this process's own streams exactly as `scripts/ci-local.sh`
    would see it, and the status handed back rather than trusted."""
    print(f"==> {selftest} --check-anchors")
    proc = harness.sh(["bash", str(root / selftest), "--check-anchors"], check=False, capture=False)
    print()
    return proc.returncode


def body(root: Path) -> int:
    failing: list[str] = []
    ran = 0
    for selftest in SELFTESTS:
        ran += 1
        if run_one(root, selftest) != 0:
            failing.append(selftest)

    if ran != EXPECTED_SELFTESTS:
        harness.die(
            f"only checked {ran} of the {EXPECTED_SELFTESTS} selftests this file is pinned to run.",
            "SELFTESTS shrank (or grew) without EXPECTED_SELFTESTS being updated to match; "
            "fix whichever one is wrong rather than silently checking fewer domains than this "
            "gate step claims to.",
        )

    if failing:
        print("FAIL: at least one mutation anchor no longer matches the tree it names.", file=sys.stderr)
        print(
            "      Each STALE ANCHOR above is a control that would plant nothing, so re-anchor",
            file=sys.stderr,
        )
        print("      it to the code as it is now rather than deleting it.", file=sys.stderr)
        print(file=sys.stderr)
        print(f"      stale in: {', '.join(failing)}", file=sys.stderr)
        return harness.EXIT_FAILED

    print(
        "OK: every mutation anchor and precondition in the compat, conformance, race, format, docs "
        "and retention selftests still matches the real tree."
    )
    return harness.EXIT_OK


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    if argv:
        print(f"usage: {sys.argv[0]}", file=sys.stderr)
        return harness.EXIT_USAGE
    root = harness.repo_root(Path(__file__))
    return harness.finish(lambda: body(root))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
