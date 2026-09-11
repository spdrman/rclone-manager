#!/usr/bin/env python3
"""Reading `scripts/e2e/tests-repo.pin`, for the two scripts that read it.

Both `run_tests_repo_gate` and `bump_tests_pin` used to get at the pin
with `. "$pin_file"`, which is to say by EXECUTING it as shell. That
worked because the file is two assignments and two hundred lines of
comment explaining them, and it is exactly the arrangement this parser
replaces: sourcing a file gives it the running script's privileges to say
what the pin is worth having, and the pin's whole job is to be one
reviewable line rather than a program.

Two real callers in the same change, which is the bar
`bdtools.harness`'s own docblock sets for a shared helper existing at
all. It is here rather than in the harness because the harness has no
business knowing what a pin is.

Ported from `scripts/e2e/run-tests-repo-gate.sh` and
`scripts/e2e/bump-tests-pin.sh` under #672 / #662.
"""

from __future__ import annotations

import re
from pathlib import Path

# The path both scripts name, relative to the repository root, as a string
# because it appears in operator-visible refusals in exactly this form.
PIN_RELATIVE = "scripts/e2e/tests-repo.pin"

# A full 40-character commit sha, and nothing shorter. The gate refuses a
# pin that is not one of these and `bump_tests_pin` uses the same shape to
# tell an exact commit from a ref to resolve, so the two cannot disagree
# about what "a sha" is.
SHA = re.compile(r"^[0-9a-f]{40}$")

# `KEY=VALUE`, anchored, with everything else -- comments, blank lines, and
# any shell that a sourced file would have RUN -- ignored rather than
# interpreted.
_ASSIGNMENT = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)=(.*)$")


def read_pin(path: Path) -> dict[str, str]:
    """The pin's assignments, last one winning.

    Last one winning is `.`-sourcing's own behaviour, kept because the pin
    file's header has been rewritten in place a dozen times and a stray
    earlier assignment must not quietly outrank the line at the bottom.

    A quoted value is unquoted, once, on the outside only: the shell would
    have done that, and a pin whose sha arrived wrapped in the quotes would
    otherwise fail the 40-character check with a message about the sha
    rather than about the quotes.
    """
    values: dict[str, str] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        match = _ASSIGNMENT.match(line)
        if match is None:
            continue
        value = match.group(2).strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in ("'", '"'):
            value = value[1:-1]
        values[match.group(1)] = value
    return values
