#!/usr/bin/env python3
"""EPIC B #81's dependency rule, the literal half (issue #165):

  "The core and generic application must build and pass tests if the
   distribution adapter tree is deleted."

`verify_core_without_apps` already proves the narrower, older claim that
`core/` alone stands with all of `apps/` gone. This proves the Phase 6
one, which is different in two ways: it deletes the distribution ADAPTER
tree specifically, wherever those paths happen to live today, and it
requires the generic application to survive too, not only core.

It deletes and rebuilds rather than scanning imports, for the same reason
the rest of this domain does: a static scan can be fooled by a path built
at runtime, a `go:embed`, or a test fixture reaching across the boundary.
An actual deletion cannot.

What it deletes is not hard-coded here. It is every path
`scripts/architecture/layers.conf` marks "distribution adapter", read
through `arch.layer_paths("distribution", "adapter", tree)`, so adding an
adapter without declaring it fails `check-layer-manifest.sh` rather than
silently escaping this proof.

What it deliberately does NOT delete is the distribution paths marked
"canonical": `container/` (the canonical image and Compose runtime) and
`distribution/packaging` (the shared metadata and the conformance suite).
#81's claim is about the adapter tree, not about the canonical runtime the
adapters wrap, and the generic application legitimately depends on that
runtime existing. `distribution/packaging`'s own tests are not run here
for the mirror-image reason: they check the adapters, so with the adapters
deleted they SHOULD fail, and running them would prove nothing about core.

`core/`'s own test suite is deliberately not re-run here either.
`verify_core_without_apps` already runs it with ALL of `apps/` deleted,
which is a strictly stronger deletion than this one (every adapter path
under `apps/` is gone there too), so running it again would cost the
Docker-backed crash matrix and the SFTP integration suite a second time to
prove something already proven. What is genuinely new in this check is the
two application modules, so those do run their tests.

`GOWORK=off` throughout: the repo-root `go.work` lists sibling modules for
local development convenience, and `apps/synology/go.mod` is one of the
files just deleted. Without it, `go build` would walk up to `go.work` and
fail on the missing module, which is a workspace-tooling artifact rather
than the thing this check exists to prove.

Ported from `scripts/architecture/verify-core-without-distribution.sh`
under EPIC I (I1.6 / #672 / #697). That path stays as a real, runnable
`exec` shim: `scripts/ci-local.sh` and `.github/workflows/ci.yml` both
name it, `scripts/architecture/selftest.sh` drives it directly inside two
mutant copies of the tree, and `scripts/tests/ci-local-gate.test.sh`
FABRICATES a file at that literal path in its synthetic-tree fixture.

# PORTED-CHECK HAZARD NOTE

  the delete list comes from the manifest, never from a list in this file
    hazard in bash:   `adapters=$(arch::layer_paths distribution adapter
                       "$wt")`, read out of the worktree's OWN manifest
                       (HEAD's), not the working tree's. A port that
                       read the working tree's manifest would delete
                       today's uncommitted adapter list out of a
                       yesterday-shaped worktree, and every mismatch
                       would land in the "not present at HEAD" branch --
                       which prints and CONTINUES, so a wholly wrong
                       list degrades into a quiet, smaller proof.
    hazard in python: LIVE. `arch.layer_paths(..., root=tree)` takes the
                       worktree explicitly; the default `root="."` would
                       silently be the working tree.
    held by:          the `root=tree` argument below, and the
                       "(N adapter path(s) deleted, M not present at
                       HEAD)" tally line, which is the number that goes
                       wrong first if the wrong manifest is read.

  a manifest that exists only in the working tree is refused, not ignored
    hazard in bash:   `[ ! -f "$wt/$(arch::manifest)" ]`. Without it,
                       `sed` on a missing file would fail with a status
                       and `set -e` would end the run on a message about
                       sed rather than about the manifest.
    hazard in python: LIVE and sharper: `arch.manifest_rows` would raise
                       `FileNotFoundError`, an uncaught traceback rather
                       than a coded refusal, and a traceback exits 1 --
                       the same number as a failed proof, from a
                       different cause.
    held by:          the `is_file()` guard below, ahead of every
                       manifest read.

  the check refuses to prove nothing: an empty adapter list is a FAILURE
    hazard in bash:   two guards, and they are not the same guard.
                       `[ -z "$adapters" ]` catches a manifest that
                       marks no path "distribution adapter" at all --
                       the vacuity this whole check is exposed to,
                       because deleting nothing and then building
                       successfully looks exactly like a passing proof.
                       `[ "$count" -eq 0 ]` catches the second shape:
                       a manifest with adapter entries, every one of
                       which is absent at HEAD, so the loop skipped them
                       all and again deleted nothing.
    hazard in python: LIVE, and the second guard is the one a port
                       loses. Nothing about `for path in paths:` makes
                       an all-skipped loop observable; the `count`
                       counter is load bearing, not bookkeeping for the
                       tally line. Dropping either guard leaves a check
                       that reports success having removed no file.
    held by:          both guards below, mutation-proved as a matched
                       pair: a manifest with its `adapter` kinds
                       rewritten to `canonical` (empty list) and a
                       manifest whose only adapter entry names a path
                       absent at HEAD (`count == 0`).

  an entry handed to `rm -rf` is checked twice: on its shape and on its outcome
    hazard in bash:   `arch::manifest_path_problem` (textual, shared with
                       `check-layer-manifest.sh` so a contributor sees
                       the refusal before committing) AND resolving the
                       path with `pwd -P` and requiring the answer to
                       still be under `$wt_real`. The shape test alone
                       would trust the input; symlinks mean the input is
                       not the whole story. The bash re-checked the
                       shape AT THIS CALL SITE rather than relying on
                       `check-layer-manifest.sh` having done it, and
                       that is kept: the manifest in the WORKTREE is a
                       committed file this run never validated, and the
                       static check runs against the WORKING TREE.
    hazard in python: LIVE and the most tempting hazard in this file.
                       `Path.resolve()` or `os.path.normpath` on the
                       ENTRY would accept `apps/../apps/unraid` because
                       the `..` cancels out -- so a port that "tidied"
                       the shape test into a resolve would delete a path
                       nobody reviewed. `arch.manifest_path_problem`
                       stays textual for exactly that reason, and the
                       containment test resolves the TARGET (not the
                       entry) and compares it textually against
                       `wt_real + "/"`, which is what catches the
                       symlink case the shape test structurally cannot.
    held by:          `scripts/architecture/selftest.sh`'s
                       `delete-traversal` and `delete-symlink-escape`
                       arms, which grep this file's own refusal text
                       ("refuses to run against a manifest entry it
                       cannot vouch for", "which is outside the
                       throwaway worktree") and then assert the
                       symlink's target survived. Those two strings are
                       therefore byte-pinned by a caller.

  every build, vet and test inside the worktree decides the verdict
    hazard in bash:   none; `set -euo pipefail` made each subshell's
                       status the script's own. Eight commands run here
                       (three builds, three vets, two test suites) and
                       any of them failing is the answer.
    hazard in python: LIVE; defect #1 of this programme, multiplied by
                       eight. A `subprocess.run` nobody reads discards
                       the status, and this script's OK line
                       ("core/, apps/common and apps/generic build and
                       pass their tests with the distribution adapter
                       tree deleted entirely.") would be printed over
                       eight failed commands.
    held by:          `harness.sh`'s default `check=True` raising
                       `CommandFailed` with the status and
                       `harness.finish` translating it. Mutation
                       control: an `apps/generic` package importing a
                       deleted adapter path turns this red.

  the throwaway worktree is removed even when a build dies mid-way
    hazard in bash:   `trap cleanup EXIT`, deliberately a trap rather
                       than a line at the end, because the build runs
                       inside a tree full of holes: an interrupt or a
                       failing build would otherwise leave a REGISTERED
                       git worktree behind and the next run inherits it.
    hazard in python: GONE as a hand-written concern, replaced by
                       `arch.worktree`'s `contextmanager`: its `finally`
                       runs `git worktree remove --force` on any exit
                       path, including the refusals above and an
                       exception from a failed build.
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

PROGRAM = "verify-core-without-distribution"

# The three modules that must still build and vet with the adapter tree
# gone, and the two that must still pass their tests. core/ is in the
# first list and not the second on purpose -- see the module docstring.
BUILD_MODULES = ("core", "apps/common", "apps/generic")
TEST_MODULES = ("apps/common", "apps/generic")


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


def delete_adapters(tree: Path, paths: list[str]) -> int | None:
    """Delete every adapter path in the worktree; the count, or `None`.

    `None` means a refusal was printed and the run is over. The operand
    of the only `rm -rf` this stack adds comes out of an editable text
    file, so it is checked twice before anything is removed: once on its
    shape and once on its outcome. See the hazard note.
    """
    tree_real = tree.resolve()

    count = 0
    skipped = 0
    for path in paths:
        # Parity with the bash's `[ -n "$path" ] || continue`. Its
        # heredoc-fed loop saw a blank line for an empty list;
        # `arch.layer_paths` cannot emit one, and the guard stays
        # because a list this loop hands to `rm -rf` is the wrong place
        # to reason about what cannot happen.
        if not path:
            continue

        problem = arch.manifest_path_problem(path)
        if problem is not None:
            print(
                f'FAIL: {arch.MANIFEST} marks "{path}" as a distribution adapter, '
                f"but that entry {problem}.",
                file=sys.stderr,
            )
            print(
                "  This check deletes every adapter path, so it refuses to run against a "
                "manifest entry it cannot vouch for.",
                file=sys.stderr,
            )
            return None

        target = tree_real / path
        # `-e`, so a broken symlink counts as absent exactly as it did
        # in bash. `os.path.exists` follows links; `lexists` would not.
        if not os.path.exists(target):
            # check-layer-manifest.sh is what reports a stale entry,
            # against the working tree, where a contributor can act on
            # it. Here the entry is simply not present at HEAD, so there
            # is nothing to delete and nothing this check can usefully
            # say about it.
            print(f"    (not present at HEAD, nothing to delete: {path})")
            skipped += 1
            continue

        try:
            parent = target.parent.resolve(strict=True)
        except OSError:
            print(
                f'FAIL: cannot resolve the parent directory of adapter path "{path}" '
                "inside the throwaway worktree.",
                file=sys.stderr,
            )
            return None

        resolved = parent / target.name
        # Textual containment against the resolved worktree, as the
        # bash's `case "$resolved" in "$wt_real"/*)` was: the worktree
        # root itself does not match, and neither does a sibling
        # directory whose name merely starts with the same characters.
        if not str(resolved).startswith(str(tree_real) + "/"):
            print(
                f'FAIL: adapter path "{path}" resolves to {resolved}, which is outside '
                f"the throwaway worktree {tree_real}.",
                file=sys.stderr,
            )
            print(
                "  Refusing to delete it. A manifest entry may only name something "
                "inside the repository.",
                file=sys.stderr,
            )
            return None

        print(f"    rm -rf {path}")
        harness.sh(["rm", "-rf", "--", str(resolved)])
        count += 1

    print(f"    ({count} adapter path(s) deleted, {skipped} not present at HEAD)")
    return count


def body(root: Path) -> int:
    with arch.worktree(root) as tree:
        if not (tree / "core").is_dir():
            print("FAIL: core/ module does not exist yet.", file=sys.stderr)
            return harness.EXIT_FAILED

        if not (tree / arch.MANIFEST).is_file():
            print(f"FAIL: {arch.MANIFEST} does not exist at HEAD.", file=sys.stderr)
            print(
                "  This check builds a throwaway worktree of HEAD, so a manifest that exists "
                "only in your working tree is invisible to it.",
                file=sys.stderr,
            )
            print("  Commit the manifest first, then re-run.", file=sys.stderr)
            return harness.EXIT_FAILED

        adapters = arch.layer_paths("distribution", "adapter", root=tree)
        if not adapters:
            print(
                f'FAIL: {arch.MANIFEST} marks no path "distribution adapter", so this check '
                "would delete nothing and pass without proving anything.",
                file=sys.stderr,
            )
            return harness.EXIT_FAILED

        print("==> deleting the distribution adapter tree in a throwaway worktree")

        count = delete_adapters(tree, adapters)
        if count is None:
            return harness.EXIT_FAILED

        if count == 0:
            print(
                "FAIL: no adapter path was actually deleted, so this check proved nothing.",
                file=sys.stderr,
            )
            print(
                f'  Every path {arch.MANIFEST} marks "distribution adapter" is missing at HEAD.',
                file=sys.stderr,
            )
            return harness.EXIT_FAILED

        for module in BUILD_MODULES:
            print(f"==> go build ./... ({module}, with the distribution adapter tree deleted)")
            go(["build", "./..."], tree / module)

            print(f"==> go vet ./... ({module}, with the distribution adapter tree deleted)")
            go(["vet", "./..."], tree / module)

        for module in TEST_MODULES:
            print(f"==> go test ./... ({module}, with the distribution adapter tree deleted)")
            go(["test", "./..."], tree / module)

    print(
        "OK: core/, apps/common and apps/generic build and pass their tests with the "
        "distribution adapter tree deleted entirely."
    )
    return harness.EXIT_OK


def main() -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    root = arch.resolve_toplevel()
    return harness.finish(lambda: body(root))


if __name__ == "__main__":
    sys.exit(main())
