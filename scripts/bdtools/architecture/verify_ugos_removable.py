#!/usr/bin/env python3
"""Can `apps/ugos/` be deleted without breaking core or `ui/shared`
tests? (EPIC-B WP1.1 RED plan, docs/EPIC-B-multi-nas.md §69 WP1.1.)

This is the acceptance criterion "adding/removing a provider app requires
no lifecycle changes" made concrete for the one provider that exists
furthest along today. `apps/ugos/` goes in a throwaway worktree, and then
the two suites that would notice run there: `core/`'s whole Go test suite
and `ui/shared`'s vitest run after a fresh `npm ci`.

Deletion rather than an import scan, for the reason the rest of this
domain gives: a scan reads the text of an import, and cannot see the
`@platform-entry` alias `ui/shared/vite.config.ts` resolves into
`apps/<platform>/frontend/`, a path assembled at run time, or a test
fixture reaching sideways. Removing the directory and running the real
suites cannot be fooled by any of them.

Ported from `scripts/architecture/verify-ugos-removable.sh` under EPIC I
(I1.6 / #672 / #697). That path stays as a real, runnable `exec` shim:
`scripts/ci-local.sh` and `.github/workflows/ci.yml` both name it, and
`scripts/tests/ci-local-gate.test.sh` FABRICATES a file at that literal
path in its synthetic-tree fixture.

# PORTED-CHECK HAZARD NOTE

  core/'s test suite inside the worktree decides the verdict
    hazard in bash:   none; `set -euo pipefail` made
                       `(cd "$wt/core" && GOWORK=off go test ./...)` the
                       script's own status. `GOWORK=off` because the
                       repo-root `go.work` lists sibling modules for
                       local development convenience and this worktree
                       has just lost one of the directories they sit
                       beside; without it `go` fails on workspace
                       tooling rather than on the thing being proved.
    hazard in python: LIVE, and this is defect #1 of this programme in
                       its exact shape. A `subprocess.run` whose
                       returncode nobody reads is discarded, so a port
                       that ran the suite for its output and never
                       looked at the status would delete `apps/ugos`,
                       watch `core/` fail, and print
                       "OK: apps/ugos/ can be deleted without breaking
                       core or ui/shared tests." A deletion proof that
                       cannot fail is worse than a deleted one, because
                       nothing anywhere says it stopped watching.
    held by:          `harness.sh`'s default `check=True` raising
                       `CommandFailed` with the status, and
                       `harness.finish` translating it -- the module
                       exits with the command's own code. Mutation
                       control: a `core/` package importing
                       `apps/ugos` turns this red.

  ui/shared's install AND its test run both decide it
    hazard in bash:   `(cd "$wt/ui/shared" && npm ci --no-audit
                       --no-fund && npm test)`. TWO commands chained
                       with `&&` inside one subshell, so the install
                       failing skipped the test run and the subshell's
                       status was whichever one failed.
    hazard in python: LIVE, and it has a second shape peculiar to the
                       `&&`: a port that ran the two commands
                       unconditionally would run `npm test` in a
                       directory where `npm ci` had just failed, and
                       vitest with no `node_modules` fails for a reason
                       that has nothing to do with `apps/ugos`. The
                       refusal would name the wrong cause.
    held by:          two `harness.sh` calls with the default
                       `check=True`: the first raises before the second
                       is reached, which is what `&&` did. Mutation
                       control: a `ui/shared` test importing
                       `apps/ugos/frontend/platform` turns this red at
                       the `npm test` step.

  core/ exists at HEAD
    hazard in bash:   `[ ! -d "$wt/core" ]` -> `FAIL: core/ module does
                       not exist yet.` Placed AFTER the `rm -rf`, unlike
                       `verify-core-without-apps.sh`, which checks
                       before. The order is kept exactly, because it is
                       observable: with `core/` absent, both scripts
                       refuse with the same line, but this one has
                       already deleted `apps/ugos` from the throwaway
                       worktree by then, and a port that "tidied" the
                       guard to the top would change which of two
                       refusals a tree missing both directories
                       produces.
    hazard in python: LIVE. `Path.is_dir()` is false for a missing path
                       and for a file of that name, exactly as `-d` is.
    held by:          the `if not (tree / "core").is_dir()` guard below,
                       mutation-proved by committing a tree with `core/`
                       removed.

  apps/ugos was actually there to delete
    hazard in bash:   NOT GUARDED, and this port keeps the gap rather
                       than closing it silently. `rm -rf
                       "$wt/apps/ugos"` on an absent directory is a
                       no-op, so a HEAD with no `apps/ugos` passes this
                       check having deleted nothing: the two suites run
                       against an unmodified tree and agree with
                       themselves. That is the vacuity shape this whole
                       programme exists to name.
    hazard in python: STILL EXISTS, identically. Reported rather than
                       fixed: `scripts/architecture/layers.conf` and
                       `check-layer-manifest.sh` are what hold the
                       repository's shape, and inventing a second
                       opinion about it inside a port is how two checks
                       drift apart. What makes the gap tolerable is that
                       `apps/ugos` disappearing is not a silent event --
                       it is a deleted tracked directory in a diff.
    held by:          nothing here, by design, and said out loud so it
                       is a decision on the record rather than an
                       accident. `verify_core_without_distribution` is
                       the check in this domain that DOES guard its
                       delete count, because its list comes out of an
                       editable text file.

  the throwaway worktree is removed even when a suite dies mid-way
    hazard in bash:   `trap cleanup EXIT`, deliberately a trap rather
                       than a line at the end, because the suites run
                       inside a tree with a directory missing: an
                       interrupt or a failing test would otherwise leave
                       a REGISTERED git worktree behind, containing a
                       `node_modules` this run created, and the next run
                       inherits it.
    hazard in python: GONE as a hand-written concern, replaced by
                       `arch.worktree`'s `contextmanager`: its `finally`
                       runs `git worktree remove --force` on any exit
                       path, including an exception from a failed suite.
                       `harness.install_signal_handlers` covers INT and
                       TERM by turning both into an exception, which is
                       strictly more than bash's EXIT trap gave on
                       SIGTERM.
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

PROGRAM = "verify-ugos-removable"


def body(root: Path) -> int:
    with arch.worktree(root) as tree:
        harness.sh(["rm", "-rf", str(tree / "apps" / "ugos")])

        if not (tree / "core").is_dir():
            print("FAIL: core/ module does not exist yet.", file=sys.stderr)
            return harness.EXIT_FAILED

        print("==> go test ./... (core/, with apps/ugos deleted)")
        harness.sh(
            ["go", "test", "./..."],
            cwd=tree / "core",
            capture=False,
            env={**os.environ, "GOWORK": "off"},
        )

        # One heading for two commands, as the bash had, because the
        # `&&` made them one step: an install that fails means there is
        # no test run to report on.
        print("==> npm ci && npm test (ui/shared, with apps/ugos deleted)")
        shared = tree / "ui" / "shared"
        harness.sh(["npm", "ci", "--no-audit", "--no-fund"], cwd=shared, capture=False)
        harness.sh(["npm", "test"], cwd=shared, capture=False)

    print("OK: apps/ugos/ can be deleted without breaking core or ui/shared tests.")
    return harness.EXIT_OK


def main() -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    root = arch.resolve_toplevel()
    return harness.finish(lambda: body(root))


if __name__ == "__main__":
    sys.exit(main())
