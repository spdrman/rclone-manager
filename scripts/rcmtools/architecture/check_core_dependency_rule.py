#!/usr/bin/env python3
"""The dependency-direction check (issue #106 WP1.1, extended to three
layers by issue #165).

EPIC-B WP1.1 asked for "a dependency-rule check (import-graph test via
`go list` or a lint rule) asserting core/ has zero imports from apps/ or
any provider SDK" (docs/EPIC-B-multi-nas.md §69 WP1.1, §7.1). Phase 6
widens it, because EPIC B #81's standing constraint names a direction
with three layers in it, not two:

  core/app  ─X─► distribution/*
  core/app  ─X─► NAS SDKs
  httpapi   ───► app/core
  web       ───► versioned API contract
  platform  ───► app/core contracts only
  adapters  ───► canonical image/Compose/runtime metadata

So this check inspects every Go module in the repository against the
layer it is declared to be in (scripts/architecture/layers.conf):

  a core-layer module      may import neither platform nor distribution
  a platform-layer module  may not import distribution
  any core or platform module may not import a NAS/provider SDK

This is the fast, static safety net: it inspects the resolved import
graph in place, on every run, with no filesystem mutation.
verify_core_without_apps and verify_core_without_distribution are the
heavier, literal proofs of the same claims, by actually deleting the
trees in a throwaway worktree.

Ported from `scripts/architecture/check-core-dependency-rule.sh` (EPIC I,
I1.6 / #672 / #697), which stays on disk as an `exec` shim because
`scripts/ci-local.sh` runs it twice, `.github/workflows/ci.yml` runs it
once, and `scripts/tests/ci-local-gate.test.sh` fabricates a file at that
literal path.

# PORTED-CHECK HAZARD NOTE

"an import" is exactly `GOWORK=off go list -deps ./...`, and nothing else
  hazard in bash:   the notion of an import was the RESOLVED import graph
                     of each module, taken from `go list -deps ./...` with
                     `GOWORK=off` so a module is judged against its own
                     go.mod rather than against the repo-root go.work that
                     lists its siblings for local development.
  hazard in python: STILL EXISTS, and substituting a "better" notion is the
                     failure this programme hunts. Reading import blocks
                     with a regex or `go/parser` would be a DIFFERENT
                     check: it sees only direct imports, so a core package
                     reaching distribution through one intermediate package
                     stops being a violation, and it sees `import _ "x"`
                     in a file excluded by a build tag as one that is not.
                     A parse was therefore NOT substituted; the port shells
                     out to the same `go list`, in the same directory, with
                     the same `GOWORK=off`, and reads the same lines. It
                     also keeps `2>&1`: the merged stream is what the loop
                     iterates and what a refusal quotes, so a diagnostic
                     line go printed on stderr is part of "the output" here
                     exactly as it was in the bash.
  held by:          selftest.py's dep-core-to-distribution and
                     dep-platform-to-distribution controls plant an
                     import that RESOLVES (they add a `go mod edit`
                     replace for it) precisely so the graph, not the text,
                     is what fires.

`go list`'s own exit status is checked BEFORE its output is inspected
  hazard in bash:   `if ! output=$(cd "$dir" && GOWORK=off go list -deps
                     ./... 2>&1); then` -- and the comment above it records
                     that `... | grep ... || true` alone had already failed
                     OPEN here once: `go list` erroring for a reason with
                     nothing to do with layering (a module that does not
                     even build) produces error text that matches no grep,
                     `|| true` swallows the real status, and the check
                     prints OK over a module whose graph it never saw.
  hazard in python: LIVE, and shaped worse than in bash: `subprocess.run`
                     returns a status nobody is obliged to read, so the
                     equivalent mistake is invisible rather than merely
                     easy. Closed by `go_list_deps` returning
                     `(returncode, output)` as one value, so a caller
                     cannot obtain the output without also being handed
                     the status; both call sites branch on it before
                     looking at a single line. `check=False` on the
                     subprocess is deliberate and is NOT an unguarded call:
                     it is the `if !` guard, and the two branches print the
                     bash's own two refusals.
  held by:          the mutation pair in this port's report (a syntax
                     error planted in a real .go file, in core/ and in
                     apps/generic/, produces the "go list -deps ./...
                     itself failed:" refusal and a non-zero exit), and
                     selftest.py's dep-* controls, whose planted imports
                     would report the wrong thing entirely if a
                     resolution failure could pass for a clean graph.

core/ imports nothing whose path contains `/apps/` or `/distribution/`
  hazard in bash:   `grep -F '/apps/'` and `grep -F '/distribution/'` over
                     core's dep list -- FIXED-STRING, substring, and
                     deliberately kept as its own assertion rather than
                     folded into the layer loop, because ci.yml names the
                     step "core/ has zero imports from apps/ (static
                     check)" and a reader should find that exact sentence
                     being checked.
  hazard in python: LIVE. `in` is the faithful port; a "tidier" version
                     that split the path and compared segments, or that
                     anchored on the module prefix, would answer a
                     different question, and the one it stops answering is
                     the literal WP1.1 claim. The `|| true` that made the
                     bash version tolerate grep's no-match status has no
                     analogue and needs none: an empty list is falsy, and
                     the emptiness is the pass.
  held by:          the mutation pair in this port's report (a resolving
                     import of apps/common and of distribution/packaging
                     added to a real core package), and by the layer loop
                     independently reporting the same import through the
                     `core` -> `platform`/`distribution` rule, so the two
                     assertions hold each other honest.

an in-repository import the manifest classifies into no layer is a failure
  hazard in bash:   `dep_layer=$(arch::classify "$rel" | awk '{print $1}')
                     || dep_layer=""` then `[ -z "$dep_layer" ]`. An
                     unclassified dependency is refused rather than
                     skipped, because "no layer" is precisely the state in
                     which no layer rule can fire, so skipping it would be
                     a hole shaped like a new product directory.
  hazard in python: LIVE, and the tempting shape fails open: `arch.classify`
                     returns `None`, and `row.layer if row else ""`
                     collapses to an empty string that a `continue`-on-
                     falsy loop would silently pass over. The port asserts
                     on the empty string, exactly as the bash did.
  held by:          selftest.py's dep-unclassified-module control greps for
                     "classified into no layer", and the mutation pair in
                     this port's report plants an unclassified PACKAGE
                     (`apps/common/selftest_unclassified/`) as well as an
                     unclassified module, which are two different refusals
                     sharing that substring.

a module may not import a layer its own layer forbids
  hazard in bash:   `forbidden_layers_for` (core -> platform,
                     distribution; platform -> distribution; distribution
                     and infrastructure -> nothing), matched with
                     `grep -qx`, i.e. a WHOLE-LINE match on the layer name.
  hazard in python: LIVE. `in` on a tuple is the whole-string equality
                     `grep -qx` gave; `in` on a JOINED STRING would make
                     `distribution` match a layer named `distributionx`,
                     and a substring test the other way round would make
                     every layer forbidden to nobody. The tuple is also why
                     an unknown layer (the bash's `*)` arm, which printed
                     nothing) forbids nothing here: `.get(layer, ())`.
  held by:          selftest.py's dep-core-to-distribution and
                     dep-platform-to-distribution controls, anchored on the
                     rule lines "core/app ─X─► distribution" and
                     "platform/app ─X─► distribution".

no core- or platform-layer module imports a provider SDK
  hazard in bash:   `printf '%s' "$pkg" | grep -qiE "$PROVIDER_SDK_PATTERN"`
                     -- case-insensitive ERE, substring, applied only to
                     paths OUTSIDE this repository's own module prefix, so
                     our own apps/truenas is reported once by the layer
                     rule instead of twice with two explanations.
  hazard in python: LIVE in three separate ways, and each fails open. The
                     pattern must stay a substring search (`re.search`,
                     never `fullmatch`); it must stay case-insensitive
                     (`re.IGNORECASE`, because a vendor path is as likely
                     to say `Synology` as `synology`); and it must stay
                     restricted to the else-branch of the module-prefix
                     test. It also must stay the FULL Phase 6 platform
                     list, not the platforms with a directory today: the
                     point of a negative check is to already be in place
                     when someone reaches for the SDK. The literal is
                     therefore copied verbatim, character for character,
                     and is NOT rebuilt from `layers.conf` -- nothing in
                     the manifest names a third-party SDK, so deriving it
                     would silently shrink it to the provider directories
                     that happen to exist.
  held by:          selftest.py's dep-core-to-provider-sdk control, which
                     plants a resolving `github.com/truenas/...` import and
                     greps for "─X─► NAS SDKs", plus the mutation pair in
                     this port's report.

every TRACKED go.mod is classified, and the list comes from the INDEX
  hazard in bash:   `git ls-files -- '*go.mod' | sort`, and issue #207 is
                     in the header: a filesystem walk answers a different
                     question (what is lying on this disk), descends into
                     agent worktrees under .claude/worktrees/, and reported
                     161 unclassified modules in the primary checkout while
                     a fresh clone passed.
  hazard in python: LIVE, and `Path.rglob("go.mod")` or `os.walk` is the
                     obvious thing to reach for -- it is the #207 defect,
                     re-introduced. The port asks git, through
                     `arch.tracked_files()`, and filters on the same
                     suffix git's `*go.mod` pathspec matched (git's `*`
                     crosses `/`, so that pathspec is exactly "ends with
                     go.mod"). `sorted()` is a byte sort, which is the
                     order `git ls-files` already emits and the order the
                     differential run reproduces line for line.
  held by:          selftest.py's matched pair: dep-unclassified-module
                     (a TRACKED unclassified module must fail) and
                     dep-untracked-module (a module in .claude/worktrees/
                     and a plain untracked one must NOT), the second of
                     which additionally asserts the run still named
                     "==> core (core layer)".

a run that inspected nothing must not report success
  hazard in bash:   `if [ "$checked" -eq 0 ]` -- the vacuity guard, and
                     its comment names what it is for: a rename, a
                     pathspec that stops matching, or a run from outside a
                     repository would otherwise print a cheerful OK over
                     zero modules.
  hazard in python: LIVE and unchanged in kind, but the ways to reach it
                     are new: an exception swallowed around the loop, a
                     generator consumed twice, or `tracked_files()`
                     returning nothing because the port resolved the repo
                     from `__file__` and ran git somewhere else. Kept
                     verbatim, and note that `checked` counts only modules
                     that were actually inspected -- an unclassified module
                     `continue`s WITHOUT incrementing it, so a tree in
                     which nothing is classified fails twice rather than
                     passing once.
  held by:          the mutation in this port's report (`git rm --cached`
                     every go.mod, so the index names none) and
                     selftest.py's dep-untracked-module control, which
                     pins that the passing run named a real module.

the target tree is the CURRENT WORKING DIRECTORY's repository
  hazard in bash:   `cd "$(git rev-parse --show-toplevel)"`, which is what
                     lets selftest.py point this check at a mutant copy of
                     the tree.
  hazard in python: LIVE, and it is the defect the release domain's port
                     was caught on: resolving the root from
                     `Path(__file__)` would check the developer's real
                     workspace instead, print plausible refusals about the
                     wrong repository, and make every selftest control
                     vacuous. Closed by `arch.resolve_toplevel()` plus an
                     `os.chdir` to it, so every relative path below means
                     what it meant in the bash. One deliberate divergence,
                     owned by the library rather than by this file: outside
                     a work tree the bash died on `cd ""` with git's own
                     `fatal:` on stderr and status 1, while
                     `resolve_toplevel()` names the problem and exits
                     EXIT_USAGE (2).
  held by:          selftest.py runs the shim from inside each mutant tree
                     with no arguments, so a port that resolved the root
                     from `__file__` would fail every control at once.

this check propagates its own exit status
  hazard in bash:   none; `set -euo pipefail` did it, and `exit 1` at each
                     refusal.
  hazard in python: LIVE, the domain-wide one. Two places a status can be
                     dropped: the shim (fixed by `exec`) and the module
                     (fixed by returning `harness.EXIT_FAILED` from `body`
                     through `harness.finish`). A dropped status prints
                     every refusal above and exits 0 -- green output, no
                     check. The non-ASCII rule lines are the one remaining
                     way an unexpected exception could arrive here (a
                     stderr that is not UTF-8), and that direction fails
                     CLOSED: the exception escapes `finish` and the process
                     exits non-zero rather than green.
  held by:          selftest.py drives the SHIM, not this module, so both
                     halves are exercised by every control; the
                     differential run in this port's report compares the
                     exit code as well as both streams.
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
from pathlib import Path, PurePosixPath

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import architecture as arch
from rcmtools import harness

PROGRAM = "check-core-dependency-rule"

MODULE_PREFIX = "github.com/spdrman/rclone-manager/"

# The set of third-party NAS, container-manager and app-store SDK names no
# core- or platform-layer module may pull in. The full Phase 6 platform
# list, not just the ones with a directory today: the point of a negative
# check is to already be in place when someone reaches for the SDK, and
# adding the name after the import lands is too late to prevent anything.
#
# Matched case-insensitively as a substring of a dependency's import path,
# and only on paths OUTSIDE this repository's own module prefix: our own
# apps/truenas is caught by the layer rules, and matching it here too would
# report one violation twice with two different explanations.
#
# Copied verbatim from the bash, and deliberately NOT derived from
# layers.conf: the manifest classifies this repository's own paths and
# names no third-party SDK at all, so deriving the list would shrink it to
# the provider directories that already exist -- the exact opposite of what
# this literal is for. Every list this check takes FROM the manifest
# (which layer a path is in) comes from `arch.classify`.
PROVIDER_SDK_PATTERN = (
    "ugreen|ugos|truenas|ixsystems|unraid|synology|openmediavault|omv-|proxmox|portainer|casaos|zimaos|dockge"
)
_PROVIDER_SDK = re.compile(PROVIDER_SDK_PATTERN, re.IGNORECASE)

# The layers a module in each layer may not import from. `distribution` is
# listed with an empty tuple rather than omitted because the bash listed
# it: it says out loud that the bottom layer forbids nothing, rather than
# leaving a reader to wonder whether it was forgotten. Anything else --
# `infrastructure`, or a layer a future manifest invents -- lands on the
# bash's `*)` arm, which also forbade nothing.
FORBIDDEN_LAYERS: dict[str, tuple[str, ...]] = {
    "core": ("platform", "distribution"),
    "platform": ("distribution",),
    "distribution": (),
}


def go_list_deps(directory: str) -> tuple[int, str]:
    """`cd <directory> && GOWORK=off go list -deps ./... 2>&1`, with the
    status returned NEXT TO the output rather than instead of it.

    `GOWORK=off`: resolve each module's import graph against its own
    go.mod only, never against the repo-root go.work (which lists sibling
    modules for local development convenience). A module standing alone is
    the claim.

    Both streams are merged, as the bash's `2>&1` merged them, because the
    caller iterates the result AND quotes it in a refusal: a diagnostic go
    printed on stderr is part of the output this check reasons about.
    `harness.sh` has no stream-merging mode, which is why this is the one
    subprocess call in the file that is not `harness.sh` -- and
    `check=False` here is not an unguarded call but the bash's own
    `if ! output=$(...)` guard, which both call sites below honour.
    """
    env = dict(os.environ)
    env["GOWORK"] = "off"
    try:
        proc = subprocess.run(
            ["go", "list", "-deps", "./..."],
            cwd=directory,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            encoding="utf-8",
            errors="replace",
            check=False,
        )
    except FileNotFoundError as exc:
        # What the subshell's status was when `go` was not on PATH: 127,
        # with the shell's complaint as the output. A missing toolchain is
        # a failed check here, exactly as it was in the bash, and never a
        # skip.
        return 127, str(exc)
    # `$(...)` strips trailing newlines; the loop and the quoted refusal
    # both saw the stripped form.
    return proc.returncode, proc.stdout.rstrip("\n")


class Refusals:
    """The bash's `note()`: record the failure and keep going, so one run
    reports every violation rather than the first one.

    That matters more here than it looks: these findings arrive in batches
    (a manifest edit that misses several files, a new module that imports
    across two layers at once), and a check that stops at the first turns
    one fix into several runs.

    Deliberately not `harness.note`, which indents under a `harness.step`
    and prints to stdout. Every line below is byte-identical to the bash's,
    on the bash's stream, because `scripts/rcmtools/architecture/selftest.py`
    greps for these strings and docs/architecture/layers.md quotes them.
    """

    def __init__(self) -> None:
        self.failed = False

    def __call__(self, text: str) -> None:
        print(text, file=sys.stderr, flush=True)
        self.failed = True


def check_module(directory: str, layer: str, refuse: Refusals) -> None:
    """Hold one module's resolved import graph to its layer's rules."""
    status, output = go_list_deps(directory)
    if status != 0:
        refuse(f"FAIL: {directory} (GOWORK=off) go list -deps ./... itself failed:")
        print(output, file=sys.stderr, flush=True)
        return

    forbidden = FORBIDDEN_LAYERS.get(layer, ())

    for pkg in output.splitlines():
        if not pkg:
            continue

        # node_modules is not repository code: nothing in it is tracked,
        # and the layer manifest classifies tracked files. It shows up here
        # at all only because `go list ./...` walks into it, and one npm
        # package (flatted) happens to ship a Go implementation inside a
        # directory that a Go module pattern therefore matches. Skipping it
        # keeps the check from reporting a violation that exists only on a
        # developer machine that has run npm ci, and never on CI.
        if "/node_modules/" in pkg:
            continue

        if pkg.startswith(MODULE_PREFIX):
            # An in-repository dependency: classify it and compare layers.
            rel = pkg[len(MODULE_PREFIX) :]
            row = arch.classify(rel)
            dep_layer = row.layer if row is not None else ""
            if not dep_layer:
                refuse(
                    f"FAIL: {directory} imports {pkg}, which scripts/architecture/layers.conf "
                    "classifies into no layer."
                )
                continue
            if dep_layer in forbidden:
                refuse(
                    f"FAIL: {directory} is in the {layer} layer and imports {pkg}, "
                    f"which is in the {dep_layer} layer."
                )
                refuse(
                    f"      rule violated: {layer}/app \u2500X\u2500\u25ba {dep_layer} "
                    '(EPIC B #81, "Dependency rule")'
                )
        # A third-party dependency: the only rule that applies is the
        # provider-SDK ban, and only for core and platform modules.
        elif layer in ("core", "platform") and _PROVIDER_SDK.search(pkg):
            refuse(f"FAIL: {directory} is in the {layer} layer and imports the provider SDK {pkg}.")
            refuse(
                f"      rule violated: {layer}/app \u2500X\u2500\u25ba NAS SDKs "
                '(EPIC B #81, "Dependency rule")'
            )


def body() -> int:
    if not Path("core").is_dir():
        print("FAIL: core/ module does not exist yet (dependency rule is inapplicable).", file=sys.stderr, flush=True)
        return harness.EXIT_FAILED

    refuse = Refusals()

    # The literal WP1.1 claim, kept as its own explicit assertion rather
    # than folded into the layer loop below: ci.yml names this step "core/
    # has zero imports from apps/ (static check)", and a reader should be
    # able to find that exact sentence being checked.
    status, core_deps = go_list_deps("core")
    if status != 0:
        print("FAIL: core/ (GOWORK=off) go list -deps ./... itself failed:", file=sys.stderr, flush=True)
        print(core_deps, file=sys.stderr, flush=True)
        return harness.EXIT_FAILED
    for needle in ("/apps/", "/distribution/"):
        bad = [line for line in core_deps.splitlines() if needle in line]
        if bad:
            refuse(f"FAIL: core/ imports code from {needle.strip('/')}/:")
            print("\n".join(bad), file=sys.stderr, flush=True)

    # Every Go module the repository TRACKS, classified by the manifest.
    # Discovered rather than listed, so a module added without being
    # classified is a manifest failure (check_layer_manifest) rather than a
    # module this check silently never looked at.
    #
    # The INDEX, not a filesystem walk (issue #207). The question this
    # check asks is what the repository contains, and a walk answers a
    # different one: what happens to be lying on this disk. Agent
    # worktrees live under .claude/worktrees/, untracked but very much
    # present, and a walk descends into all of them and reports every
    # module it meets there as unclassified. In the primary checkout that
    # was 161 failures and a gate that no commit could get past, while a
    # fresh clone and each individual worktree passed, which is why it
    # stayed invisible until the Phase 6 merge.
    #
    # It also settles a disagreement rather than only fixing a bug:
    # check_layer_manifest already classifies the index, so before this the
    # two checks held different opinions about which files the repository
    # consists of.
    #
    # `endswith("go.mod")` is git's `-- '*go.mod'` pathspec: git's `*`
    # crosses `/`, so that glob is a suffix test and nothing more.
    checked = 0
    for gomod in sorted(f for f in arch.tracked_files() if f.endswith("go.mod")):
        # Kept from the bash. `git ls-files` does not emit a "./" prefix,
        # but everything downstream of here assumed the stripped form.
        gomod = gomod.removeprefix("./")
        directory = str(PurePosixPath(gomod).parent)
        row = arch.classify(gomod)
        layer = row.layer if row is not None else ""
        if not layer:
            refuse(
                f"FAIL: the Go module at {directory} is classified into no layer by "
                "scripts/architecture/layers.conf."
            )
            continue
        print(f"==> {directory} ({layer} layer)", flush=True)
        check_module(directory, layer, refuse)
        checked += 1

    # A run that inspected nothing must not report success. Without this,
    # anything that made the listing above return no go.mod (a rename, a
    # pathspec that stops matching, a run from outside a repository) would
    # print a cheerful OK over zero modules.
    if checked == 0:
        print("FAIL: no Go module was inspected, so this check verified nothing.", file=sys.stderr, flush=True)
        return harness.EXIT_FAILED

    if refuse.failed:
        print("", file=sys.stderr, flush=True)
        print("  The layers and what each owns: docs/architecture/layers.md", file=sys.stderr, flush=True)
        return harness.EXIT_FAILED

    print(
        f"OK: {checked} Go module(s) respect the layer dependency direction, "
        "and core/ imports nothing from apps/ or distribution/.",
        flush=True,
    )
    return harness.EXIT_OK


def main(argv: list[str]) -> int:
    # The bash took no arguments and ignored any it was given, so this does
    # too: refusing an extra argument here would be a new refusal (and a
    # new exit status) that no caller has ever seen.
    del argv
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    os.chdir(arch.resolve_toplevel())
    return harness.finish(body)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
