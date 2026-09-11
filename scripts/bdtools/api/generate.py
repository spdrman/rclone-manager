#!/usr/bin/env python3
"""Regenerate the /api/v1 bindings from the authoritative contract (issue #166).

Run this after editing `api/v1/openapi.json`. The two files it writes are
generated output and carry a DO NOT EDIT banner; `check_contract_drift.py`
fails CI if either one stops matching what this produces.

Ported from `scripts/api/generate.sh`. That file now execs this one:
`scripts/api/selftest.sh` (itself a shim onto `selftest.py`) drives it as a
subprocess to prove `check-contract-drift.sh` fails on a contract change
nobody regenerated for -- see the module docstring's hazard note for why
that matters.
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import harness
from bdtools.api import lib

PROGRAM = "api generate"


def body() -> int:
    lib.API_GO_BINDING.parent.mkdir(parents=True, exist_ok=True)
    lib.API_TS_BINDING.parent.mkdir(parents=True, exist_ok=True)

    proc = lib.generate(lib.API_GO_BINDING, lib.API_TS_BINDING)
    if proc.returncode != 0:
        harness.die(
            f"the generator refused {lib.API_CONTRACT}:",
            *(proc.stdout or "").splitlines(),
            *(proc.stderr or "").splitlines(),
        )

    print("OK: regenerated")
    print(f"  {lib.API_GO_BINDING}")
    print(f"  {lib.API_TS_BINDING}")
    return harness.EXIT_OK


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    if argv:
        print(f"usage: {sys.argv[0]}", file=sys.stderr)
        return harness.EXIT_USAGE
    return harness.finish(body)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
