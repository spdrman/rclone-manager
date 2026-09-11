#!/usr/bin/env python3
"""The layer-OWNERSHIP check (issue #165), under EPIC I (#672 / #697).

Ported from `scripts/architecture/check-layer-ownership.sh`, which stays on
disk as an `exec` shim.

The dependency checks next to this one answer "who may import whom". This
one answers a different question that #165's acceptance criteria ask
separately:

  "GIVEN a provider or distribution package that attempts to hold
   lifecycle state, retention policy, validation rules, catalog truth or
   backup policy, WHEN the ownership check runs, THEN it fails, because
   those may only live in the provider-neutral core."

An import rule cannot catch this. A runtime profile that grows its own
retention type imports nothing it should not; it just quietly becomes a
second place retention is decided, which is exactly how a fork starts.

The scanning itself is `scripts/architecture/ownership.go`, which parses Go
rather than grepping it, because the thing being detected is a declaration
and grep cannot tell one from a comment. That program is NOT ported: it
stays a standalone module-less Go file invoked with `GOWORK=off go run`,
for the same reason `scripts/api/gen-bindings.go` does. This module is the
wrapper, and it decides WHAT to scan: every path the layer manifest puts in
the runtime-platform or distribution layer, so adding a platform or an
adapter brings it under the rule automatically.

# PORTED-CHECK HAZARD NOTE

Everything this module asserts on its own is a REFUSAL ABOUT ITS OWN
INPUTS -- the manifest exists, and it names something to scan. The verdict
itself belongs to a subprocess, which makes this wrapper exactly the shape
the porting programme keeps catching: three lines of glue, each of which
can turn the check into one that cannot fail while printing green.

  the manifest-missing refusal still refuses
    hazard in bash:   `manifest=$(arch::manifest)`, then
                       `[ ! -f "$manifest" ]` -> `exit 1`, evaluated with
                       the CWD at the repository top level.
    hazard in python: STILL EXISTS, and it changes shape: `arch.manifest()`
                       defaults to `root="."`, so a port that tested
                       `arch.manifest().is_file()` would be asking about
                       the process's CWD rather than about the tree under
                       test. Held by testing `arch.manifest(root)` (rooted,
                       so it is the tree this run was pointed at) while
                       printing `arch.MANIFEST` (relative, so the operator
                       message is byte-identical to the bash's and does not
                       leak a temp path from a self-test mutant).
    held by:          selftest.py's ownership arms run the shim from inside
                       each mutant copy, so a refusal that read the wrong
                       tree would report the real workspace's manifest and
                       pass every mutation.

  the zero-target vacuity guard still fires
    hazard in bash:   `targets=()` filled from
                       `arch::layer_paths platform; arch::layer_paths
                       distribution`, then
                       `[ "${#targets[@]}" -eq 0 ]` -> `exit 1`.
    hazard in python: LIVE, and it is the load-bearing one. `ownership.go`
                       is given a LIST; hand it an empty list and it prints
                       `usage: ownership <repo-root> <path>...` and exits 2
                       -- which is at least non-zero, but a port that
                       instead skipped the run, or ran it and reported "no
                       violations", would print an OK line for a scan of
                       nothing. The manifest is the only thing that says
                       which paths this rule covers, so a manifest that
                       covers none of them is a check that has been turned
                       off, not a clean tree. Held by keeping the explicit
                       count test before the subprocess exists.
    held by:          a mutation in this port's own report: the two
                       `platform`/`distribution` rows removed from a copy of
                       layers.conf, which produces this exact refusal and
                       exit 1 rather than a green scan. (selftest.py's
                       `own-nothing-to-scan` arm exercises the OTHER empty
                       set -- targets that exist but hold no Go or
                       TypeScript -- which is `ownership.go`'s own
                       "verified nothing" guard, not this one.)

  the scanner's verdict is this script's exit status
    hazard in bash:   none; `set -euo pipefail` made the final `go run` the
                       script's status.
    hazard in python: LIVE, and the domain-wide trap: `subprocess.run`
                       returns a status object and Python discards it
                       unless something looks. Closed by returning
                       `scan.returncode` as `body()`'s value, through
                       `harness.finish`.
                       `check=False` is deliberate here, and it is the one
                       call in this module where it is right: this
                       subprocess IS the check, so its non-zero must arrive
                       at the caller as ITSELF and with only the scanner's
                       own `FAIL:` block on stderr. The default
                       `check=True` would raise `CommandFailed`, and
                       `finish` would print a second failure banner listing
                       all thirty-four target paths on top of the message
                       `selftest.sh` greps for. A returned status is not a
                       dropped status; an ignored one is, and the mutation
                       in the report is what tells the two apart.
                       `capture=False` for the same reason: the scanner's
                       stdout and stderr are the operator's output, not
                       material for a quoted refusal.
    held by:          the ownership mutation in this port's report (a
                       planted `RetentionPolicy` in a real platform package
                       -> exit 1 through this wrapper), plus every
                       `expect_check_fails` arm in selftest.sh, which fails
                       a check that exits 0 with a violation planted.

  the target tree comes from the CWD, never from `__file__`
    hazard in bash:   `cd "$(git rev-parse --show-toplevel)"`.
    hazard in python: LIVE. `arch.resolve_toplevel()` is what holds it, and
                       the reason is in its docstring: resolving from
                       `Path(__file__)` would check the developer's real
                       workspace no matter which tree the shim was run from,
                       so every self-test mutant would pass. The root is
                       then passed to `harness.sh(cwd=root)` and the scanner
                       is given `.`, exactly as the bash did after its `cd`
                       -- rather than this process calling `os.chdir`, which
                       would be a global side effect for a value only one
                       subprocess needs.
    held by:          resolve_toplevel's own contract, and structurally by
                       this module never mentioning `__file__` except in the
                       `sys.path` line that finds the package.

  GOWORK=off, and the target ORDER
    hazard in bash:   `GOWORK=off go run ...`, a per-command environment
                       assignment; and `platform` paths before
                       `distribution` paths, because that is the order the
                       two `arch::layer_paths` calls were written in.
    hazard in python: STILL EXISTS. Without `GOWORK=off`, `go run` reads
                       `go.work` and refuses a file no module in it owns,
                       so the check would fail for a reason that has nothing
                       to do with ownership. `harness.sh(env=...)` REPLACES
                       the environment rather than extending it, so the
                       assignment is built from a copy of `os.environ`;
                       passing `{"GOWORK": "off"}` alone would drop PATH and
                       HOME and break `go` itself. Order is preserved by
                       concatenating the two `layer_paths` calls in the bash
                       order: `ownership.go` sorts its findings by path
                       before printing, so order does not change the
                       verdict, but it does change the count line and the
                       argv a failing run quotes.
    held by:          the differential test in this port's report: identical
                       stdout, stderr and exit status against the real tree,
                       including the `==> scanning 34 ...` count.

CGO: not needed. `ownership.go` imports only the standard library's pure-Go
packages, and `go run` of it succeeds on this host with cgo linking broken,
so no `CGO_ENABLED=0` is set -- which would have been a host workaround
sitting in the middle of a checked-in check.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import architecture as arch
from bdtools import harness

PROGRAM = "check-layer-ownership"

# The scanner, relative to the repository top level, which is what the
# subprocess's CWD is.
SCANNER = "scripts/architecture/ownership.go"


def body(root: Path) -> int:
    if not arch.manifest(root).is_file():
        print(
            f"FAIL: {arch.MANIFEST} does not exist, so there is no way to know which paths this rule applies to.",
            file=sys.stderr,
        )
        return harness.EXIT_FAILED

    # Platform first, then distribution: the bash's two calls, in its order.
    targets = arch.layer_paths("platform", root=root) + arch.layer_paths("distribution", root=root)

    if not targets:
        print(
            "FAIL: the manifest puts no path in the runtime-platform or distribution layer, "
            "so this check would scan nothing.",
            file=sys.stderr,
        )
        return harness.EXIT_FAILED

    print(f"==> scanning {len(targets)} runtime-platform and distribution path(s) for core-owned declarations")

    env = dict(os.environ)
    env["GOWORK"] = "off"

    scan = harness.sh(["go", "run", SCANNER, ".", *targets], cwd=root, env=env, capture=False, check=False)
    return scan.returncode


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    if argv:
        print(f"usage: {sys.argv[0]}", file=sys.stderr)
        return harness.EXIT_USAGE
    root = arch.resolve_toplevel()
    return harness.finish(lambda: body(root))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
