#!/usr/bin/env python3
"""Does `core/` still build and pass its whole test suite with `apps/`
deleted entirely? (EPIC-B WP1.1 behavioral contract,
docs/EPIC-B-multi-nas.md §7.1, §69 WP1.1.)

Proves it by actually deleting `apps/` in a throwaway worktree, rather
than trusting a static import scan to have caught every path. An import
scan reads the text of an import line; it cannot see a package resolved
by a `go:embed` pattern, a path assembled at run time, or a test fixture
in `core/` that reaches sideways into a provider directory. A deletion
can.

`GOWORK=off` on both Go commands, and the reason is not incidental: the
repository root's `go.work` also lists `./apps/common` for local
multi-module development convenience, and `apps/` is gone in this
worktree. Without it, `go build` walks up to that `go.work` and fails on
the missing `apps/common` -- a workspace-tooling artifact, not the thing
this check exists to prove. `core/`'s own `go.mod` is what must stand
alone.

Ported from `scripts/architecture/verify-core-without-apps.sh` under
EPIC I (I1.6 / #672 / #697). That path stays as a real, runnable `exec`
shim: `scripts/ci-local.sh` and `.github/workflows/ci.yml` both name it,
and `scripts/tests/ci-local-gate.test.sh` FABRICATES a file at that
literal path in its synthetic-tree fixture.

# PORTED-CHECK HAZARD NOTE

  the build and the test suite inside the worktree actually decide the verdict
    hazard in bash:   none; `set -euo pipefail` made the status of
                       `(cd "$wt/core" && GOWORK=off go build ./...)`
                       the script's own status.
    hazard in python: LIVE, and it is defect #1 of this programme in its
                       exact shape. A `subprocess.run` whose returncode
                       nobody reads is discarded, so a port that ran the
                       build for its output and never looked at the
                       status would delete `apps/`, watch the build
                       fail, and print
                       "OK: core/ builds and its full test suite passes
                       with apps/ deleted entirely." A deletion proof
                       that cannot fail is worse than a deleted one:
                       `ci-local.sh` would ledger it green forever.
    held by:          `harness.sh`'s default `check=True` raising
                       `CommandFailed` with the status, and
                       `harness.finish` translating it -- the module
                       returns the command's own exit code. Mutation
                       control: planting a `core/` package that imports
                       `apps/generic` turns this red (see the port's
                       mutation log).

  core/ exists at HEAD before anything is deleted
    hazard in bash:   `[ ! -d "$wt/core" ]` -> `FAIL: core/ module does
                       not exist yet.` This is the non-vacuity guard the
                       script has. Without it a worktree with no `core/`
                       would run two `go` commands in a directory that
                       does not exist and the failure would read as a
                       build error rather than as "there is nothing here
                       to prove".
    hazard in python: LIVE. `Path.is_dir()` is false for a missing path
                       AND for a file of that name, exactly as `-d` is,
                       and the refusal happens before the `rm -rf`.
    held by:          the `if not (tree / "core").is_dir()` guard below,
                       mutation-proved by committing a tree with `core/`
                       removed.

  apps/ was actually there to delete
    hazard in bash:   NOT GUARDED, and this is reported rather than
                       fixed. `rm -rf "$wt/apps"` on an absent `apps/`
                       is a silent no-op, so a HEAD with no `apps/` at
                       all passes this check having deleted nothing.
                       The bash had no guard for it and neither does
                       this port: `scripts/architecture/layers.conf`'s
                       completeness guard plus `check-layer-manifest.sh`
                       are what would notice a tree that lost its whole
                       `apps/` subtree, and inventing a second opinion
                       here during a port is how two checks drift apart.
    hazard in python: STILL EXISTS, identically and deliberately. The
                       one thing this port does NOT do is silently
                       upgrade the check while claiming to have ported
                       it.
    held by:          nothing here, by design. Stated out loud so the
                       gap is a decision on the record instead of an
                       accident. `verify_core_without_distribution` is
                       the check in this domain that DOES have the
                       corresponding guard (`count == 0` after its
                       delete loop), because its delete list comes out
                       of an editable text file.

  the throwaway worktree is removed even when the build dies mid-way
    hazard in bash:   `trap cleanup EXIT`, deliberately a trap rather
                       than a line at the end, because the build runs
                       inside a tree full of holes: an interrupt or a
                       failing build would otherwise leave a REGISTERED
                       git worktree behind and the next run inherits it.
    hazard in python: GONE as a hand-written concern, replaced by
                       `arch.worktree`'s `contextmanager`: its `finally`
                       runs `git worktree remove --force` on any exit
                       path, including an exception from a failed build.
                       `harness.install_signal_handlers` is what keeps
                       the INT/TERM path covered too -- it turns both
                       signals into an exception, so the `finally` still
                       runs, which is strictly more than bash's EXIT
                       trap gave on SIGTERM.
    held by:          `arch.worktree`'s `finally` clause, plus
                       `harness.install_signal_handlers()` in `main`.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import architecture as arch
from bdtools import harness

PROGRAM = "verify-core-without-apps"


def go(args: list[str], module_dir: Path) -> None:
    """One `GOWORK=off go ...` inside the worktree, with its status kept.

    `capture=False` so the build's own output reaches the operator's
    terminal as it happens, exactly as the bash subshell's did; the
    status still travels, because `harness.sh` defaults to `check=True`.
    """
    harness.sh(
        ["go", *args],
        cwd=module_dir,
        capture=False,
        env={**os.environ, "GOWORK": "off"},
    )


def body(root: Path) -> int:
    with arch.worktree(root) as tree:
        if not (tree / "core").is_dir():
            print("FAIL: core/ module does not exist yet.", file=sys.stderr)
            return harness.EXIT_FAILED

        harness.sh(["rm", "-rf", str(tree / "apps")])

        print("==> go build ./... (core/, with apps/ deleted entirely)")
        go(["build", "./..."], tree / "core")

        print("==> go test ./... (core/, with apps/ deleted entirely)")
        go(["test", "./..."], tree / "core")

    print("OK: core/ builds and its full test suite passes with apps/ deleted entirely.")
    return harness.EXIT_OK


def main() -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    root = arch.resolve_toplevel()
    return harness.finish(lambda: body(root))


if __name__ == "__main__":
    sys.exit(main())
