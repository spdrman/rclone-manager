#!/usr/bin/env python3
"""Is every Go file in this repository owned by a module, and if not, is it
checked anyway? (issue #417)

"Unowned" here means exactly one thing: no directory at or above the file
contains a go.mod. Two files in this repository are unowned today:

  scripts/api/gen-bindings.go        (run by scripts/rcmtools/api/lib.py)
  scripts/architecture/ownership.go  (run by check-layer-ownership.sh)

That is not a defect on its own. They are single-file `package main`
programs that this repository's own scripts `go run`, and giving them a
module would put a sixth entry in go.work and a new row in the layer
manifest to buy very little.

What IS a defect is what follows from it. This gate lints per module
(`cd core && golangci-lint run ...`, `cd apps/common && ...`) and vets per
module the same way, so an unowned file is reached by none of it. Both of
these have therefore never been vetted or linted by anything, ever, in a
repository whose gate otherwise vets and lints everything. That is how one
of the pair came to be the only unformatted Go file in the tree
(scripts/format/check-gofmt.sh is the sweep that found it): nothing was
looking, so nothing said anything.

So this looks. `go vet` needs no module at all when it is given file
paths, which is the whole reason this can exist without a scripts/go.mod.
golangci-lint does need one, so each unowned directory is copied into a
throwaway module in $TMPDIR and linted there against this repository's own
.golangci.yml. Both files are standard-library only, so that copy resolves
offline with no go.sum and no network; GOPROXY=off below makes a file that
stops being standard-library-only fail immediately and say so, rather than
hanging on a fetch.

Paths in the output are rewritten back to the real ones, because a
complaint about /var/folders/.../T/rclone-manager-unowned.XXXX/bad.go is a
complaint nobody can act on.

Exit code contract, which is all a gate step needs from it:

  0        every unowned Go file passes go vet and golangci-lint
           (including the case where there are none)
  non-zero at least one does not, and the run printed which and why

About a second. It runs near the top of the gate with the gofmt sweep
rather than with the other architecture checks, for the reason that
applies to all of them: a check this cheap should not be reached at
minute 20, and a Go file nobody checks is worth hearing about before the
Docker-backed suites start.

Takes an optional directory: a git work tree to check instead of this
one, which is what scripts/architecture/selftest.sh drives so the
mutation controls and the real run go down the same code path.

# PORTED-CHECK HAZARD NOTE

the unowned set is DISCOVERED, never named
  hazard in bash:    `module_owner` walked from each tracked `.go` file's
                      directory up to `.`, and the set was whatever came
                      out. Naming the two known files instead would have
                      left a third one added tomorrow unchecked -- which
                      is #417 itself, since the pair this check was
                      written for went unlinted for their whole lives
                      precisely because nothing enumerated them.
  hazard in python:  STILL EXISTS, and is the easiest thing in this file
                      to get wrong: a port that writes
                      `for path in ("scripts/api/gen-bindings.go", ...)`
                      passes every existing control, matches the bash on
                      this tree byte for byte, and has silently stopped
                      being a discovery. `module_owner` below is the same
                      upward walk over the same `git ls-files '*.go'`
                      list, and the file count in the OK line is derived
                      from it rather than from a constant.
  held by:           a mutation control that adds a THIRD unowned `.go`
                      file in a directory this file has never heard of
                      and requires the count in the OK line to move and
                      the new directory to be vetted and linted.

every unowned Go file passes `go vet`
  hazard in bash:    `if ! go vet $files`, with the combined output kept
                      and quoted under the FAIL line. `set -e` was
                      deliberately OFF here (`set -uo pipefail`, no `-e`)
                      so that a first failing directory did not abort the
                      loop before the second was reported.
  hazard in python:  STILL EXISTS, in the shape harness.py warns about: a
                      `subprocess.run` whose `.returncode` nobody reads
                      discards the verdict, and this check would print
                      its OK line over a vetting failure. Guarded
                      explicitly -- `_combined()` returns the status and
                      `body()` branches on it, which is bash's `if !`,
                      not bash's `set -e`.
  held by:           a mutation control that plants an unvetted unowned
                      `.go` file (`fmt.Printf` with a bad verb) and
                      requires exit 1 and the `FAIL: go vet found
                      something in ...` line.

every unowned Go file passes `golangci-lint`
  hazard in bash:    `if ! (cd "$mod" && ... golangci-lint run ...)`,
                      against a throwaway module holding a copy of the
                      directory, with the temp path rewritten back to the
                      real one by `sed` before anything is printed.
  hazard in python:  STILL EXISTS, same discarded-status shape as the vet
                      arm, plus a second one the bash could not have: the
                      path rewrite is a regex, and a rewrite that stopped
                      matching would leave the complaint pointing at a
                      /var/folders path nobody can act on while the check
                      still went red for the right reason -- a quiet loss
                      of the thing that makes the failure actionable.
  held by:           the same mutation control as the vet arm (an
                      unformatted file is a `gofmt` finding, which is a
                      lint finding here), which asserts on the REWRITTEN
                      path: the FAIL body has to name the real directory.

a missing `golangci-lint` is a FAILURE, not a skip
  hazard in bash:    none that needed writing down: with no `have_tool`
                      guard anywhere in the script, a missing binary made
                      the subshell exit 127, `if !` took the failure
                      branch, and the "command not found" the shell wrote
                      to the redirected stream was quoted under the FAIL
                      line. `.github/workflows/ci.yml` installs the
                      binary precisely so this reports "the linter says
                      no" rather than "the binary is missing" (#575).
  hazard in python:  STILL EXISTS and points the other way -- Python's
                      `FileNotFoundError` is an EXCEPTION, so the two
                      tempting ports are both wrong in opposite
                      directions: letting it escape turns a linter that
                      is merely absent into a traceback, and wrapping it
                      in `harness.have_tool(...)` + a skip is #160's
                      silent skip wearing a different hat. Neither is
                      what the bash did. `_combined()` converts the
                      missing binary into status 127 with the diagnostic
                      as its output, so the FAIL branch is taken exactly
                      as before.
  held by:           a mutation control that runs the check with a PATH
                      that has no `golangci-lint` on it and requires
                      exit 1 plus the `FAIL: golangci-lint found
                      something in ...` line.

the tree under test is the CALLER's, not this file's
  hazard in bash:    `cd "$(git rev-parse --show-toplevel)"` after an
                      optional `cd "$1"`, so `selftest.sh` could point
                      the check at a mutant copy.
  hazard in python:  LIVE, and it is the defect the release domain's port
                      was caught on: `Path(__file__)` resolves to THIS
                      checkout no matter where the check was invoked
                      from, so every mutation control would have been
                      run against the developer's real workspace and
                      printed plausible refusals about the wrong
                      repository -- green mutants, vacuous controls.
                      Closed by `arch.resolve_toplevel()`, which is
                      `git rev-parse --show-toplevel` of the CURRENT
                      WORKING DIRECTORY.
  held by:           every mutation control below runs against a COPY of
                      the tree and requires the copy's own third file to
                      be named in the output; a `__file__` port reports
                      two files and passes nothing.

this check propagates its own exit status
  hazard in bash:    none; the explicit `exit 1` / `exit 0` / `exit 2`
                      did it.
  hazard in python:  LIVE, and domain-wide. There are two places a status
                      can be dropped now: the `.sh` (fixed by `exec`, so
                      the shim has no status of its own) and this module
                      (fixed by `harness.finish` plus `sys.exit(main(...))`).
                      A dropped status prints every FAIL line above and
                      exits 0 -- green gate, no check.
  held by:           `selftest.sh` drives the SHIM rather than this
                      module, so both halves are exercised by every
                      control, and the mutation controls assert on the
                      exit code and not only on the text.

# Two deliberate divergences from the bash, both in the safe direction

  * The bash de-duplicated the directory list with a SUBSTRING test
    (`case "\n$dirs" in *"\n$d"*)`), so a directory whose path is a
    prefix of one already collected -- `scripts/api` arriving after
    `scripts/apiv2` -- would have been treated as already present and
    never vetted or linted. Nothing in this tree triggers it and nothing
    would have said anything if it had. This port matches whole entries.
  * `git ls-files` failing left the bash with an empty list and its
    "every tracked Go file is inside a module" OK line: a vacuous pass
    at the one moment the check could not see the repository. Here it is
    unguarded on purpose, so `harness.sh`'s `check=True` turns it into a
    refusal with a status.
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import architecture as arch
from rcmtools import harness

PROGRAM = "check-unowned-go"

# What the throwaway module claims when `go env GOVERSION` cannot be read,
# which is the bash's own fallback. Old enough that no module written for a
# newer toolchain is rejected by it, new enough to have modules at all.
FALLBACK_GOVERSION = "1.21"

# The three things a golangci-lint run says when the offline copy could not
# resolve an import, in the bash's own BRE alternation order.
OFFLINE_MARKERS = ("GOPROXY=off", "cannot find module", "missing go.sum")


def _combined(argv: list[str], *, cwd: Path | None = None, env: dict[str, str] | None = None) -> tuple[int, str]:
    """Run a command and return `(status, stdout-and-stderr-as-one-stream)`.

    Not `harness.sh`, and the difference is the point rather than a
    shortcut. The bash wrote `>"$tmp/vet.out" 2>&1`: ONE file, with the
    kernel interleaving both streams in real time. golangci-lint puts its
    findings on stdout and its own errors on stderr, so keeping the streams
    apart and concatenating them afterwards would reorder a failing run's
    output -- the config error that explains the findings would print after
    them. `harness.sh` deliberately keeps them separate because a refusal
    wants to quote them separately; this call site wants what the file had.

    A missing binary comes back as status 127 with the diagnostic as its
    output, which is what the shell did with it, and NOT as an exception:
    `.github/workflows/ci.yml` installs golangci-lint so this check can
    say "the linter says no" (#575), and a check that skipped itself when
    its tool was missing would be #160 again.
    """
    try:
        proc = subprocess.run(
            argv,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            cwd=str(cwd) if cwd is not None else None,
            env=env,
            encoding="utf-8",
            errors="replace",
        )
    except FileNotFoundError:
        return 127, f"{argv[0]}: command not found\n"
    return proc.returncode, proc.stdout or ""


def module_owner(path: str) -> str:
    """The directory holding the go.mod that owns `path`, or `""`.

    The whole definition of "unowned", and a filesystem question rather
    than a manifest one: `go` finds a module by walking up from the file
    until it sees a go.mod, so that is what this walks. The walk ends
    AFTER testing `.`, because the repository root is allowed to be the
    owner.
    """
    directory = os.path.dirname(path) or "."
    while True:
        if os.path.isfile(os.path.join(directory, "go.mod")):
            return directory
        if directory == ".":
            return ""
        directory = os.path.dirname(directory) or "."


def _indent(text: str) -> list[str]:
    """`sed 's/^/    /'` over a captured stream, blank lines included."""
    return [f"    {line}" for line in text.splitlines()]


def _emit(lines: list[str]) -> None:
    for line in lines:
        print(line, file=sys.stderr, flush=True)


def _logical_pwd() -> str:
    """The directory this run started in, the way `$PWD` reported it.

    `os.getcwd()` resolves symlinks and `$PWD` does not, so on a host
    where $TMPDIR is under a symlinked /tmp the two disagree -- and this
    string goes into an operator-facing refusal that names the directory
    they typed.
    """
    env = os.environ.get("PWD", "")
    if env and os.path.isabs(env):
        try:
            if os.path.samefile(env, "."):
                return env
        except OSError:
            pass
    return os.getcwd()


def unowned_directories(go_files: list[str]) -> tuple[list[str], int]:
    """Every tracked `.go` file no module owns, grouped by its directory.

    Grouped because files in one directory are one package, and vetting
    them separately would report every reference between them as
    undefined. Returns the directories in the order `git ls-files` gave
    them, and the file count, which is what the OK line reports.
    """
    directories: dict[str, None] = {}
    count = 0
    for path in go_files:
        if not path or module_owner(path):
            continue
        count += 1
        directories[os.path.dirname(path) or "."] = None
    return list(directories), count


def go_version() -> str:
    """The toolchain's version for the throwaway module's `go` directive."""
    status, out = _combined(["go", "env", "GOVERSION"])
    if status != 0:
        return FALLBACK_GOVERSION
    version = re.sub(r"^go", "", out.strip())
    return version or FALLBACK_GOVERSION


def check_directory(directory: str, files: list[str], *, tmp: Path, toplevel: Path, goversion: str) -> bool:
    """Vet and lint one unowned directory. True when both held."""
    ok = True

    # go vet, straight at the file paths. No module, no go.work, no copy.
    status, vet_out = _combined(["go", "vet", *files], cwd=toplevel)
    if status != 0:
        _emit(
            [
                f"FAIL: go vet found something in {directory}, which no module owns and nothing else vets:",
                *_indent(vet_out),
                "",
            ]
        )
        ok = False

    # golangci-lint needs a module, so it gets a throwaway one.
    modkey = "mod-" + directory.replace("/", "-")
    mod = tmp / modkey
    mod.mkdir(parents=True, exist_ok=True)
    (mod / "go.mod").write_text(f"module unownedcheck\n\ngo {goversion}\n", encoding="utf-8")
    try:
        for path in files:
            shutil.copy(toplevel / path, mod / os.path.basename(path))
    except OSError:
        print(
            f"{PROGRAM}: could not copy {directory} into a throwaway module",
            file=sys.stderr,
            flush=True,
        )
        raise SystemExit(harness.EXIT_USAGE) from None

    env = dict(os.environ, GOFLAGS="-mod=mod", GOPROXY="off")
    status, lint_out = _combined(
        ["golangci-lint", "run", "--config", str(toplevel / ".golangci.yml"), "./..."],
        cwd=mod,
        env=env,
    )
    if status != 0:
        # golangci-lint reports paths relative to wherever it was invoked
        # from, which from a throwaway module under $TMPDIR is a long climb
        # of "../" back out. Anything ending in the throwaway module's own
        # directory name is that prefix, whether it arrived absolute or
        # relative, so strip it and put the real directory back.
        rewritten = re.sub(
            r"[^ \t\n\v\f\r]*/" + re.escape(modkey) + "/",
            lambda _m: f"{directory}/",
            lint_out,
        )
        _emit(
            [
                f"FAIL: golangci-lint found something in {directory}, which no module owns and nothing else lints:",
                *_indent(rewritten),
                "",
            ]
        )
        if any(marker in lint_out for marker in OFFLINE_MARKERS):
            _emit(
                [
                    "    That looks like a dependency this check cannot resolve. It copies each unowned",
                    "    directory into a throwaway module and lints it offline, which works because",
                    "    every unowned file here is standard-library only. One that is not needs a real",
                    "    module rather than this.",
                    "",
                ]
            )
        ok = False

    return ok


def body(argv: list[str]) -> int:
    root_arg = argv[0] if argv else ""
    if root_arg:
        if not os.path.isdir(root_arg):
            print(f"{PROGRAM}: {root_arg} is not a directory", file=sys.stderr, flush=True)
            return harness.EXIT_USAGE
        try:
            os.chdir(root_arg)
        except OSError:
            return harness.EXIT_USAGE

    # Asked before `arch.resolve_toplevel()` rather than instead of it: the
    # library's refusal is the domain's generic one, and this script's is the
    # one an operator has been reading since #417 -- it says WHICH directory
    # was not a work tree and why the check needed one.
    if not harness.sh_ok(["git", "rev-parse", "--show-toplevel"]):
        print(
            f"{PROGRAM}: {root_arg or _logical_pwd()} is not inside a git work tree, "
            "and this reads the tracked file list",
            file=sys.stderr,
            flush=True,
        )
        return harness.EXIT_USAGE
    toplevel = arch.resolve_toplevel()
    os.chdir(toplevel)

    go_files = [path for path in arch.tracked_files(toplevel) if path.endswith(".go")]
    directories, unowned_count = unowned_directories(go_files)

    if unowned_count == 0:
        print(
            "OK: every tracked Go file is inside a module, "
            "so the per-module vet and lint steps reach all of them.",
            flush=True,
        )
        return harness.EXIT_OK

    goversion = go_version()
    ok = True
    checked = 0

    with tempfile.TemporaryDirectory(
        prefix="rclone-manager-unowned.",
        dir=os.environ.get("TMPDIR") or "/tmp",
    ) as tmpname:
        tmp = Path(tmpname)
        for directory in directories:
            files = [
                path
                for path in go_files
                if (os.path.dirname(path) or ".") == directory and not module_owner(path)
            ]
            if not files:
                continue
            if not check_directory(directory, files, tmp=tmp, toplevel=toplevel, goversion=goversion):
                ok = False
            checked += 1

    if not ok:
        _emit(
            [
                "    These files are outside every module and outside go.work, so no per-module vet or",
                "    lint step in this gate can see them. This check is the only thing that does.",
            ]
        )
        return harness.EXIT_FAILED

    print(
        f"OK: {unowned_count} Go file(s) in {checked} director(ies) are owned by no module, "
        "and all of them pass go vet and golangci-lint.",
        flush=True,
    )
    return harness.EXIT_OK


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    return harness.finish(lambda: body(argv))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
