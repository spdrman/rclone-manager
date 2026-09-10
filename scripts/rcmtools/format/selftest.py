#!/usr/bin/env python3
"""Positive controls for the formatting gate (issue #417).

Formatting joined this repository's checks because it was missing, and it
was missing in the way that is hardest to see: two Go files were not
gofmt-clean, and every gate step stayed green, because `go build`,
`go vet` and every linter that was enabled are all indifferent to layout.
A check that was never there and a check that is there and looking are
the same output, which is the same argument #242 made for the
compatibility cells and #417 made for the race detector. So the same
treatment: plant real unformatted code and require each half of the gate
to go red.

There are two halves, and they are not redundant.

  .golangci.yml's gofmt formatter    covers the five Go modules
  scripts/rcmtools/format/check_gofmt.py  covers every tracked .go file

The second exists because golangci-lint is invoked per module and two Go
files live outside every module and outside go.work
(scripts/api/gen-bindings.go, scripts/architecture/ownership.go). No
per-module lint run can see them, and the unformatted one of the pair was
gen-bindings.go. F2 below is the standing precondition for that claim,
and F4 is the cell the per-module half cannot make.

The mutations plant NEW files rather than editing existing source, so
there is no verbatim copy of product code here to drift, and this file
does not use `scripts/rcmtools/selftest_swap.py`'s anchored-mutation
machinery. What CAN drift is the structural fact F2 pins: the day
somebody adds scripts/go.mod, those two files stop being outside every
module, this script's whole reason changes and the comments naming them
go stale. That is what `--check-anchors` checks, in the same shape and
the same half-second as every other anchor, and
`scripts/rcmtools/selftest/check_anchors.py` runs it.

`python3 scripts/rcmtools/format/selftest.py --check-anchors` is that
check alone, building nothing.

Ported from `scripts/format/selftest.sh` under EPIC I (#672 / #662).
`scripts/format/selftest.sh` stays as a real, runnable shim:
`scripts/tests/ci-local-gate.test.sh` FABRICATES a stand-in at that
literal path, and `scripts/ci-local.sh` still names it.

# PORTED-CHECK HAZARD NOTE

F6's golangci-lint cell reporting "caught" for the wrong reason
  hazard in bash:   `grep -qF '(gofmt)' "$tmp/out"` scans golangci-lint's
                     combined output for the literal substring `(gofmt)`,
                     which is how golangci-lint tags a finding with the
                     linter that produced it. Nothing in the bash
                     verifies that string appears NEXT TO a mention of
                     the actual planted file (`bad.go`); a golangci-lint
                     config that also flags an unrelated pre-existing
                     gofmt complaint elsewhere in `$GOPATH`'s module cache
                     would make this cell "pass" without the plant having
                     mattered at all. Narrow, because the synthetic
                     module contains exactly one file and no dependency
                     that could carry such a complaint in from outside --
                     but the check itself does not rule it out.
  hazard in python: STILL EXISTS, faithfully. `run_golangci` below
                     performs the identical substring search
                     (`"(gofmt)" in out`) and no more, because narrowing
                     it further (say, requiring `bad.go` to appear on the
                     same line as `(gofmt)`) would be a strictly NEW
                     assertion this port has not proven against a real
                     golangci-lint invocation, and #672's own rule is
                     that a port narrows or strengthens nothing quietly.
                     Recorded here as a known gap rather than silently
                     inherited.
  held by:           nothing; a genuine finding, left exactly as narrow
                     as the file it replaces, for the reason above.
"""

from __future__ import annotations

import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import harness, selftest_swap

PROGRAM = "format selftest"

OUTSIDE_EVERY_MODULE = [
    "scripts/api/gen-bindings.go",
    "scripts/architecture/ownership.go",
]


def module_root_of(root: Path, rel: str) -> str | None:
    """The directory holding the go.mod that owns `rel`, or None when no
    directory above it has one."""
    directory = (root / rel).parent
    while directory != root.parent and directory != directory.parent:
        if (directory / "go.mod").is_file():
            try:
                return str(directory.relative_to(root))
            except ValueError:
                return str(directory)
        if directory == root:
            break
        directory = directory.parent
    return None


def unformatted_go(path: Path, package: str) -> None:
    """A Go file that compiles and that gofmt disagrees with. Badly spaced
    rather than mangled on purpose: the point is that this file is
    perfectly valid, builds, vets and lints clean, and is only wrong about
    layout, which is exactly why nothing caught the two real ones."""
    path.write_text(f"package {package}\n\n// Sum adds two numbers.\nfunc Sum(a int,b int) int {{\n\treturn a+b\n}}\n")


def synthetic_repo(tmp: Path, name: str) -> Path:
    """A throwaway git work tree shaped like this one: a repository root
    with no go.mod of its own, a Go module in a subdirectory, and a
    scripts/ directory outside every module. Same shape, so the cells
    below exercise check_gofmt.py's real code path (git ls-files inside a
    git work tree) rather than a second implementation written for the
    test."""
    directory = tmp / name
    (directory / "core").mkdir(parents=True)
    (directory / "scripts").mkdir(parents=True)
    (directory / "core" / "go.mod").write_text("module stubcore\n\ngo 1.21\n")
    (directory / "core" / "stub.go").write_text(
        "package core\n\n// Stub exists so the module has something to build.\nfunc Stub() int { return 1 }\n"
    )
    (directory / "scripts" / "tool.go").write_text("package main\n\nfunc main() {}\n")
    subprocess.run(["git", "init", "-q"], cwd=directory, check=True)
    subprocess.run(["git", "add", "-A"], cwd=directory, check=True)
    return directory


def run_check(check: Path, target: Path | None = None) -> tuple[int, str]:
    argv = ["bash", str(check)]
    if target is not None:
        argv.append(str(target))
    proc = subprocess.run(argv, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    return proc.returncode, proc.stdout


def body(root: Path, dry_run: bool) -> int:
    check = root / "scripts" / "format" / "check-gofmt.sh"
    config = root / ".golangci.yml"
    tally = selftest_swap.Tally()

    # ---------------------------------------------------------- F2 first
    print("==> F2 the two Go files outside every module are still outside every module")
    f2_problems: list[str] = []
    for rel in OUTSIDE_EVERY_MODULE:
        if not (root / rel).is_file():
            f2_problems.append(f"{rel} is gone, so the reason check-gofmt.sh exists no longer names it")
            continue
        owner = module_root_of(root, rel)
        if owner is not None:
            f2_problems.append(
                f"{rel} is now inside the module at {owner}, so a per-module golangci-lint run does reach it"
            )
    if not f2_problems:
        tally.ok("both files are outside every module, so only the sweep can reach them")
    else:
        tally.bad(
            "the structural precondition this corpus rests on has moved:",
            "\n".join(f2_problems)
            + "\nUpdate scripts/format/check-gofmt.sh's header, .golangci.yml's comment and\n"
            "OUTSIDE_EVERY_MODULE in this file together, rather than deleting the cell.",
        )

    if dry_run:
        print()
        if tally.failed == 0:
            print("==> format selftest anchors: ok (1 precondition checked)")
            return harness.EXIT_OK
        print("==> format selftest anchors: FAILED", file=sys.stderr)
        return harness.EXIT_FAILED

    with tempfile.TemporaryDirectory(prefix="rclone-manager-format-selftest.") as tmp_name:
        tmp = Path(tmp_name)

        # -------------------------------------------------------------- F1
        print()
        print("==> F1 negative control: the real tree is clean")
        code, out = run_check(check)
        if code == 0:
            tally.ok("check-gofmt.sh passes on the real tree, so the cells below mean something")
        else:
            tally.bad("check-gofmt.sh FAILS on the unmutated tree, so its failures say nothing", out)

        # -------------------------------------------------------------- F3
        print()
        print("==> F3 an unformatted file inside a module is caught")
        d = synthetic_repo(tmp, "inside-a-module")
        unformatted_go(d / "core" / "bad.go", "core")
        subprocess.run(["git", "add", "-A"], cwd=d, check=True)
        code, out = run_check(check, d)
        if code == 0:
            tally.bad("the sweep PASSED with an unformatted file in a module", out)
        elif "core/bad.go" not in out:
            tally.bad("the sweep failed but never named the file", out)
        else:
            tally.ok("caught, and named: core/bad.go")

        # -------------------------------------------------------------- F4
        print()
        print("==> F4 an unformatted file OUTSIDE every module is caught")
        d = synthetic_repo(tmp, "outside-every-module")
        unformatted_go(d / "scripts" / "bad.go", "main")
        subprocess.run(["git", "add", "-A"], cwd=d, check=True)
        if (d / "scripts" / "go.mod").is_file() or (d / "go.mod").is_file():
            tally.bad("the fixture's scripts/ is inside a module, so F4 is not measuring what it says")
        else:
            code, out = run_check(check, d)
            if code == 0:
                tally.bad(
                    "the sweep PASSED with an unformatted file outside every module, "
                    "which is the one thing golangci-lint cannot see",
                    out,
                )
            elif "scripts/bad.go" not in out:
                tally.bad("the sweep failed but never named the file", out)
            else:
                tally.ok("caught, and named: scripts/bad.go, which no per-module lint run reaches")

        # -------------------------------------------------------------- F5
        print()
        print("==> F5 a newly added, uncommitted file is caught, because that is the pre-commit state")
        d = synthetic_repo(tmp, "staged-not-committed")
        unformatted_go(d / "core" / "added.go", "core")
        subprocess.run(["git", "add", "core/added.go"], cwd=d, check=True)
        code, out = run_check(check, d)
        if code == 0:
            tally.bad("the sweep PASSED on a staged file, so the pre-commit hook would let it land", out)
        elif "core/added.go" not in out:
            tally.bad("the sweep failed but never named the staged file", out)
        else:
            tally.ok("caught, and named: core/added.go")

        # -------------------------------------------------------------- F6
        print()
        print("==> F6 .golangci.yml's own formatter turns an unformatted module red")
        if shutil.which("golangci-lint") is None:
            tally.bad(
                "golangci-lint is not on PATH, so the half of this gate that lives in .golangci.yml "
                "cannot be measured"
            )
        else:
            gd = tmp / "golangci"
            gd.mkdir()
            (gd / "go.mod").write_text("module fmtprobe\n\ngo 1.21\n")
            unformatted_go(gd / "bad.go", "fmtprobe")
            proc = subprocess.run(
                ["golangci-lint", "run", "--config", str(config), "./..."],
                cwd=gd,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
            )
            if proc.returncode == 0:
                tally.bad("golangci-lint PASSED an unformatted file with this repository's own config", proc.stdout)
            elif "(gofmt)" not in proc.stdout:
                tally.bad("golangci-lint failed, but not on formatting, so something else broke", proc.stdout)
            else:
                tally.ok("red, and it says (gofmt)")
                # And the other direction, which is what stops the cell
                # above from passing against a config that rejects
                # everything.
                subprocess.run(["gofmt", "-w", str(gd / "bad.go")], check=True)
                proc2 = subprocess.run(
                    ["golangci-lint", "run", "--config", str(config), "./..."],
                    cwd=gd,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.STDOUT,
                    text=True,
                )
                if proc2.returncode == 0:
                    tally.ok("green once the same file is formatted")
                else:
                    tally.bad("golangci-lint FAILS on a formatted file, so the cell above proves nothing", proc2.stdout)

    print()
    if tally.failed == 0:
        print(f"==> format selftest: ok ({tally.passed} controls)")
        return harness.EXIT_OK
    print(f"==> format selftest: {tally.failed} failed, {tally.passed} passed", file=sys.stderr)
    return harness.EXIT_FAILED


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    dry_run = selftest_swap.parse_selftest_args(argv, "selftest.py")
    root = harness.repo_root(Path(__file__))
    return harness.finish(lambda: body(root, dry_run))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
