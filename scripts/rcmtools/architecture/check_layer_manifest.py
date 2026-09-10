#!/usr/bin/env python3
"""Completeness guard for the three-layer manifest (issue #165).

Every other check in this domain reads `scripts/architecture/layers.conf`
to decide which layer a path is in. A manifest with a hole in it makes
all of them fail open: an unclassified directory is checked by nothing,
and nothing says so. Phase 4 already learned this lesson on the
conformance matrix, where omitting a capability had to fail rather than
shrink the matrix; this is the same guard for the layer split.

It fails when:
  - any tracked file is not classified by some manifest entry;
  - any manifest entry names a path that does not exist, or one whose
    shape leaves the repository (absolute, or carrying a ".." segment);
  - any entry uses a layer or kind outside the declared vocabulary;
  - a kind is set on a non-distribution layer, or missing on a
    distribution one;
  - any of the three product layers is empty, so the split cannot pass
    vacuously by classifying everything as infrastructure.

Static, no worktree: it inspects the WORKING TREE so a contributor sees
the failure before committing, unlike the deletion proofs next to it,
which necessarily run against HEAD.

`note()` records a failure and KEEPS GOING, so one run reports every
violation rather than the first one. That matters more here than it
looks: these findings arrive in batches (a manifest edit that misses
several files, a new module that imports across two layers at once), and
a check that stops at the first turns one fix into several runs. It is
also why this module returns a status instead of calling `harness.die`
on the first refusal.

# PORTED-CHECK HAZARD NOTE

Ported from `scripts/architecture/check-layer-manifest.sh` (EPIC I,
I1.6 / #672 / #697), which stays on disk as an `exec` shim. This check
has caught two real defects in one day (#686's unclassified
CONTRIBUTING.md and this programme's own CHANGELOG.md), so every
assertion below is load bearing rather than decorative.

  the manifest file exists at all
    hazard in bash:   `[ ! -f "$manifest" ]`, before anything reads it.
    hazard in python: STILL EXISTS, and it changes shape: bash's
                       `arch::manifest_rows` on a missing file printed
                       nothing and `set -e` killed the run, whereas
                       `arch.manifest_rows()` raises FileNotFoundError,
                       which would reach the caller as a traceback and
                       an exit status nobody chose. Closed by testing
                       `arch.manifest(root).is_file()` first and exiting
                       1 with the bash's own sentence, exactly as the
                       bash did before sourcing a single row.
    held by:          selftest.py's missing-manifest arm, and the
                       differential run against the real tree (both
                       implementations print the identical line when the
                       manifest is moved aside).

  every row's layer is in the declared vocabulary
    hazard in bash:   `case "$layer" in core|platform|distribution|
                       infrastructure) ;; *) note ...` -- a typo'd layer
                       classifies nothing, because `arch::classify`
                       matches on PATH, so the row still "covers" its
                       files while every layer-specific check
                       (`layer_paths core`, ...) silently skips them.
    hazard in python: STILL EXISTS, unchanged: `arch.LAYERS` membership
                       is an ordinary `in` test and a port that dropped
                       it would print nothing. The failure is invisible
                       precisely because the completeness guard further
                       down still passes -- the files ARE classified,
                       just into a layer no check asks about.
    held by:          mutation: `core` -> `kore` in layers.conf makes
                       this check red naming that row (recorded in the
                       port's evidence), and selftest.py's vocabulary
                       arm.

  a distribution row's kind is "adapter" or "canonical"
    hazard in bash:   `case "$kind" in adapter|canonical) ;; *) note ...`
    hazard in python: STILL EXISTS. This one is not cosmetic: a
                       `distribution` row whose kind is anything else is
                       skipped by `layer_paths("distribution",
                       "adapter")`, so `verify_core_without_distribution`
                       deletes one directory fewer and still passes. A
                       check that stopped watching this would make a
                       DELETION PROOF weaker without touching the
                       deletion proof.
    held by:          mutation: setting a `distribution adapter` row's
                       kind to `-` makes this check red, and the
                       companion "no adapter at all" assertion below
                       catches the case where it was the last one.

  a non-distribution row's kind is "-"
    hazard in bash:   the `else` arm, `[ "$kind" != "-" ]`.
    hazard in python: STILL EXISTS. `kind` is meaningful only on the
                       distribution layer, so a kind set anywhere else
                       is a row whose author believed something this
                       repository does not implement -- and it reads as
                       intent, which is worse than noise.
    held by:          the same mutation family as above (a kind token
                       moved onto a `core` row), and selftest.py's kind
                       arm.

  a manifest path's SHAPE is checked BEFORE its existence
    hazard in bash:   `if problem=$(arch::manifest_path_problem "$path")
                       ; then note ...; continue; fi`, deliberately
                       ahead of `[ ! -e "$path" ]`.
    hazard in python: STILL EXISTS, and the ORDER is the assertion. An
                       entry that escapes the repository can SATISFY the
                       existence test by pointing at something real
                       outside the tree, and it is invisible to the
                       completeness guard because `arch.classify` only
                       ever matches entries against tracked files. It is
                       not invisible to
                       `verify_core_without_distribution`, which hands
                       every `distribution adapter` entry to `rm -rf`.
                       The `continue` is load bearing too: without it a
                       shape-refused entry ALSO reports "does not
                       exist", which is a second sentence about a
                       different problem than the one that matters.
    held by:          mutation: an `apps/../../elsewhere` row must be
                       refused for its SHAPE with no existence line
                       beside it (recorded in the port's evidence), plus
                       selftest.py's manifest-shape arms.

  every manifest path exists
    hazard in bash:   `[ ! -e "$path" ]`, relative to the repository
                       root the script `cd`-ed into.
    hazard in python: STILL EXISTS, with a new way to get it wrong: the
                       relative path must be joined onto the TARGET
                       tree, not tested against the process's cwd and
                       not against `Path(__file__)`. A stale entry
                       silently narrows every check that reads this
                       file.
    held by:          mutation: a row naming `core/does-not-exist`
                       makes this check red, and `arch.resolve_toplevel`
                       (never `__file__`) keeps the join pointed at the
                       tree under test, which is what lets selftest.py
                       aim this check at a mutant copy.

  none of core/platform/distribution is empty
    hazard in bash:   `[ -z "$(arch::layer_paths "$layer")" ]` -- an
                       empty string test on captured stdout.
    hazard in python: STILL EXISTS, and the bash idiom does not survive
                       translation literally: `layer_paths` returns a
                       LIST, and a truthiness test on it is right only
                       because the list is empty exactly when the bash's
                       capture was. Three layers that are not all
                       populated is two layers with a comment, and the
                       split would pass vacuously by classifying
                       everything as infrastructure.
    held by:          selftest.py's empty-layer arm; the vocabulary
                       mutation above also trips it, because a row with
                       a typo'd layer leaves the real layer empty.

  at least one distribution path is marked "adapter"
    hazard in bash:   `[ -z "$(arch::layer_paths distribution adapter)" ]`
    hazard in python: STILL EXISTS. This is the vacuity guard for
                       another script: with no adapter path,
                       `verify_core_without_distribution` deletes
                       nothing, builds an untouched tree, and passes
                       while proving nothing at all. Note the kind
                       argument must be passed positionally-equivalent
                       to the bash's second word: `layer_paths(...,
                       kind="adapter")`, because the default `"any"`
                       would match every row and make this assertion
                       unfailable.
    held by:          mutation: flipping the last `adapter` row to `-`
                       must produce BOTH this line and the kind line.

  every tracked file is classified by some entry
    hazard in bash:   `git ls-files` piped through `arch::classify`, with
                       the misses accumulated into `unclassified` as
                       "\n  ${file}" and reported in one message.
    hazard in python: STILL EXISTS, and it is the assertion this whole
                       file exists for. Two ways to lose it silently:
                       asking the FILESYSTEM instead of git (a walk
                       classifies build output and node_modules, so the
                       check becomes noise and then gets narrowed), and
                       treating `classify` as a boolean when it returns
                       a `Row | None` (a `Row` is always truthy, so
                       `if not arch.classify(...)` is right only because
                       `None` is the miss -- an `except`-swallowing or
                       `.path`-dereferencing port turns a miss into a
                       crash or into nothing).
    held by:          mutation: `git add -N` of a new top-level file
                       must make this check red NAMING that file
                       (recorded in the port's evidence). That is the
                       mutation that caught both of today's real
                       defects.

  the printed count is every tracked file, not every unclassified one
    hazard in bash:   `count=$((count + 1))` on every `git ls-files`
                       line, including the classified ones, printed in
                       the final OK line.
    hazard in python: STILL EXISTS, and it is the only thing that makes
                       a silently-dropped manifest row VISIBLE on a
                       green run: the number is how an operator notices
                       that fewer files are being checked than
                       yesterday. Counting only the classified ones, or
                       only the misses, would still print a plausible
                       sentence. `len(arch.tracked_files(root))` is
                       every line git printed.
    held by:          the differential run: the two implementations must
                       print the same number for the same tree, and any
                       row that stops being read moves it.

  the run's exit status is this check's own verdict
    hazard in bash:   none; `set -euo pipefail` plus the explicit
                       `exit 1` under `[ "$fail" -ne 0 ]`.
    hazard in python: STILL EXISTS, and it is the domain-wide one. There
                       are two places the status can be dropped -- the
                       shim (fixed by `exec`) and this module (fixed by
                       returning `harness.EXIT_FAILED` through
                       `harness.finish`, and by `harness.sh`'s default
                       `check=True` on the `git ls-files` call). A
                       dropped status prints every refusal above and
                       exits 0: green output, no check.
    held by:          selftest.py drives the SHIM rather than this
                       module, so both halves are exercised by every
                       control; the differential run compares exit codes
                       as well as bytes.
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import architecture as arch
from rcmtools import harness

PROGRAM = "check-layer-manifest"


class Notes:
    """The bash's `fail` variable and its `note()`, in one object.

    `note` records the failure and keeps going; `failed` is what the exit
    status is built from at the end. Every line goes to stderr and is
    printed VERBATIM -- no program prefix, no indentation -- because
    `scripts/architecture/selftest.sh` greps these strings and
    `docs/architecture/layers.md` quotes some of them.
    """

    def __init__(self) -> None:
        self.failed = False

    def note(self, text: str) -> None:
        print(text, file=sys.stderr, flush=True)
        self.failed = True


def check_rows(root: Path, notes: Notes) -> None:
    """Vocabulary and shape, one row at a time, in the bash's order."""
    for row in arch.manifest_rows(root):
        layer, kind, path = row.layer, row.kind, row.path

        if layer not in arch.LAYERS:
            notes.note(
                f'FAIL: unknown layer "{layer}" for {path} '
                "(expected core, platform, distribution or infrastructure)."
            )

        if layer == "distribution":
            if kind not in arch.DISTRIBUTION_KINDS:
                notes.note(
                    f'FAIL: distribution entry {path} has kind "{kind}"; a distribution path must be '
                    '"adapter" or "canonical", because verify-core-without-distribution.sh deletes '
                    "exactly the adapter ones."
                )
        elif kind != "-":
            notes.note(
                f'FAIL: {layer} entry {path} has kind "{kind}"; kind is meaningful only on the '
                'distribution layer, so it must be "-" here.'
            )

        # Shape before existence. An entry that escapes the repository can
        # satisfy the existence test below by pointing at something real
        # outside the tree, and it is invisible to the completeness guard
        # further down because arch.classify only ever matches entries
        # against tracked files. It is not invisible to
        # verify_core_without_distribution, which deletes every entry marked
        # "distribution adapter".
        problem = arch.manifest_path_problem(path)
        if problem is not None:
            notes.note(
                f'FAIL: manifest entry "{path}" {problem}. Every check here joins a manifest path '
                "onto a directory, and verify-core-without-distribution.sh hands the adapter ones "
                "to rm -rf, so an entry that leaves the repository is a delete nobody reviewed."
            )
            continue

        if not (root / path).exists():
            notes.note(
                f"FAIL: manifest entry {path} does not exist. A stale entry silently narrows every "
                "check that reads this file."
            )


def check_layers_populated(root: Path, notes: Notes) -> None:
    """No layer may be empty, and one distribution path must be an adapter."""
    for layer in ("core", "platform", "distribution"):
        if not arch.layer_paths(layer, root=root):
            notes.note(
                f"FAIL: the {layer} layer has no paths. Three layers that are not all populated is "
                "two layers with a comment."
            )

    if not arch.layer_paths("distribution", "adapter", root=root):
        notes.note(
            'FAIL: no distribution path is marked "adapter", so '
            "verify-core-without-distribution.sh would delete nothing and pass vacuously."
        )


def check_every_tracked_file(root: Path, manifest: str, notes: Notes) -> int:
    """Every tracked file is classified. Returns the count that is printed.

    `git ls-files` rather than a filesystem walk: the question is what the
    repository contains, not what happens to be lying in the working tree
    (a node_modules, a build output, a scratch file).
    """
    tracked = arch.tracked_files(root)
    # The two-space indented shape the bash accumulated with
    # `unclassified="${unclassified}\n  ${file}"`, leading newline and all.
    unclassified = "".join(f"\n  {file}" for file in tracked if arch.classify(file, root) is None)

    if unclassified:
        notes.note(f"FAIL: tracked file(s) belong to no layer. Classify them in {manifest}:{unclassified}")

    # Every line git printed, including the classified ones: the number is
    # how a silently-dropped manifest row becomes visible on a green run.
    return len(tracked)


def body(root: Path) -> int:
    manifest = arch.MANIFEST
    if not arch.manifest(root).is_file():
        print(f"FAIL: {manifest} does not exist, so no layer is declared at all.", file=sys.stderr, flush=True)
        return harness.EXIT_FAILED

    notes = Notes()
    check_rows(root, notes)
    check_layers_populated(root, notes)
    count = check_every_tracked_file(root, manifest, notes)

    if notes.failed:
        print("", file=sys.stderr, flush=True)
        print("  The layers and what each owns: docs/architecture/layers.md", file=sys.stderr, flush=True)
        return harness.EXIT_FAILED

    print(f"OK: all {count} tracked files are classified by {manifest}, and every entry exists.", flush=True)
    return harness.EXIT_OK


def main() -> int:
    harness.set_program(PROGRAM)
    root = arch.resolve_toplevel()
    return harness.finish(lambda: body(root))


if __name__ == "__main__":
    sys.exit(main())
