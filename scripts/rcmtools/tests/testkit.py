#!/usr/bin/env python3
"""The assertion vocabulary shared by every `scripts/rcmtools/tests/*` suite.

Every `scripts/tests/*.test.sh` file defined its own `checks=0; failures=0`,
its own `pass()`/`fail()` pair printing `    ok   $1` / `    FAIL $1`, and
(in several of them) its own `assert_contains`/`assert_eq`/`assert_nonzero`
helpers, byte-for-byte identical across files. `ci-local-gate.test.sh`'s own
header explains why the COUNT is part of what is being tested, not
decoration: a review neutered two independent skip sites and the suite
stayed green at "37/37" both times, which is why every port here still
prints and returns a real tally rather than just a boolean.

# PORTED-CHECK HAZARD NOTE

the tally itself cannot silently stop counting
  hazard in bash:   `checks=$((checks + 1))` inside a function is ordinary
                    arithmetic with nothing to swallow it; the risk was
                    never the increment, it was a call site that skipped
                    `pass`/`fail` entirely and asserted nothing (the exact
                    shape D3's fix, #662, exists to police).
  hazard in python: GONE as an arithmetic risk, STILL EXISTS as the same
                    call-site risk: nothing stops a ported case from
                    computing a condition and never calling `Suite.ok`/
                    `Suite.bad` on it. Not mechanically preventable in
                    either language; held by the same discipline harness.py
                    asks for a check being seen to fail once.
  held by:          every ported module's own mutation-proof section, and
                    code review reading the diff against its bash source.

a captured status is the subprocess's own, not a shell propagation of it
  hazard in bash:   `out="$(cmd)"; status=$?` under `set -uo pipefail`
                    (deliberately no `-e` in any of these files) is safe
                    already -- the risk these suites are FOR is in the
                    SCRIPT UNDER TEST, not in the driver.
  hazard in python: a driver using `subprocess.run(..., check=True)` would
                    raise on the very statuses these suites exist to read
                    (3, 127, arbitrary), turning an assertion into an
                    uncaught exception instead of a `FAIL` line. `run()`
                    below always passes `check=False` for exactly this
                    reason -- it is a test driver, not a proof.
  held by:          `run()`'s signature has no `check` parameter at all,
                    so a future edit cannot accidentally add the one shape
                    that would break every case using it.
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path
from typing import Mapping, Sequence


class Suite:
    """One `*.test.sh` file's pass/fail tally, in its own exact print shape."""

    def __init__(self, label: str) -> None:
        self.label = label
        self.checks = 0
        self.failures = 0

    def ok(self, message: str) -> None:
        self.checks += 1
        print(f"    ok   {message}", flush=True)

    def bad(self, message: str, detail: str = "") -> None:
        self.checks += 1
        self.failures += 1
        print(f"    FAIL {message}", file=sys.stderr, flush=True)
        if detail:
            for line in detail.splitlines() or [""]:
                print(f"         | {line}", file=sys.stderr, flush=True)

    def check(self, condition: bool, ok_message: str, fail_message: str, detail: str = "") -> None:
        """One assertion, worded differently on each branch (the common shape
        outside the assert_* helpers, e.g. two-machine-ci-verdict's cases)."""
        if condition:
            self.ok(ok_message)
        else:
            self.bad(fail_message, detail)

    def assert_contains(self, label: str, needle: str, haystack: str) -> None:
        self.check(needle in haystack, label, f"{label} -- expected output to contain: {needle}", haystack)

    def assert_not_contains(self, label: str, needle: str, haystack: str) -> None:
        self.check(needle not in haystack, label, f"{label} -- expected output NOT to contain: {needle}", haystack)

    def assert_eq(self, label: str, want: str, got: str) -> None:
        self.check(want == got, label, f"{label} -- want [{want}], got [{got}]")

    def assert_nonzero(self, label: str, status: int) -> None:
        self.check(status != 0, label, f"{label} -- expected a non-zero exit, got 0")

    def finish(self) -> int:
        print("", flush=True)
        if self.failures == 0:
            print(f"==> {self.label}: ok ({self.checks} checks)", flush=True)
            return 0
        print(f"==> {self.label}: FAILED ({self.failures} of {self.checks} checks)", file=sys.stderr, flush=True)
        return 1


def run(
    argv: Sequence[str],
    *,
    cwd: Path | None = None,
    env: Mapping[str, str] | None = None,
    timeout: float | None = None,
) -> tuple[str, int]:
    """Run `argv`, merge stderr into stdout, and return `(output, status)`.

    The direct equivalent of `out="$(cmd 2>&1)"; status=$?` -- `check=False`
    always, see the module docstring's hazard note.
    """
    proc = subprocess.run(
        list(argv),
        cwd=str(cwd) if cwd is not None else None,
        env=dict(env) if env is not None else None,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        timeout=timeout,
    )
    return proc.stdout, proc.returncode
