#!/usr/bin/env python3
"""Is every Go file in this repository gofmt-clean? (issue #417)

Two of them were not, and the interesting part is not the whitespace. It
is that nothing anywhere noticed, for as long as those files have
existed. `go build` and `go vet` are indifferent to layout, none of the
linters .golangci.yml enabled looks at it, and the gate's own step
headings say "build, vet, test" and "golangci-lint", so every one of them
was green on a file no tool had an opinion about. That is this
repository's recurring shape, arriving through formatting this time: a
check that is silently not checking.

.golangci.yml now enables the gofmt formatter, which closes it for the
five Go modules. This script is the other half, and it is not redundant
with that one. golangci-lint is invoked per module (`cd core && ...`,
`cd apps/common && ...`), and two Go files in this repository live
outside every module and outside go.work:

  scripts/api/gen-bindings.go        (run by scripts/bdtools/api/lib.py)
  scripts/architecture/ownership.go  (run by the layer-ownership check)

No per-module lint run has ever been able to see either of them, and the
unformatted one of the pair was gen-bindings.go. They are compiled, by
the `go run` that invokes them, so a syntax error would surface; nothing
else about them is checked by anything. This sweep at least holds them to
the same formatting as the rest of the tree.

Tracked files only, through `git ls-files`, which is the same set a
reviewer sees and keeps node_modules/, build output and throwaway
worktrees out without a list of exclusions to maintain.

Exit code contract, which is all a gate step needs from it:

  0        every tracked Go file is gofmt-clean
  1        at least one is not, and the run printed which
  2        a usage or environment problem

Costs about half a second on the whole repository, which is why it sits
near the top of the gate next to
`scripts/bdtools/selftest/check_anchors.py` rather than at minute 20
behind the Go suites.

Takes an optional directory: a git work tree to check instead of this
one. That is what `selftest.py` drives, so the mutation controls and the
real run go down the same code path rather than the controls exercising a
second implementation.

Ported from `scripts/format/check-gofmt.sh` under EPIC I (#672 / #662).
`scripts/format/check-gofmt.sh` stays as a real, runnable shim:
`scripts/tests/ci-local-gate.test.sh` FABRICATES a stand-in at that
literal path, and `scripts/ci-local.sh` still names it.

# PORTED-CHECK HAZARD NOTE

the tracked-file list surviving a `gofmt` that finds nothing to say
  hazard in bash:   `files=$(git ls-files -z '*.go' | xargs -0 gofmt -l
                     2>/dev/null)`. `xargs -0 gofmt -l` with ZERO tracked
                     Go files runs `gofmt -l` with no file arguments at
                     all, which makes gofmt read stdin as the file to
                     format -- and stdin here is `xargs`'s own (closed,
                     already-consumed) pipe, not a terminal, so this
                     doesn't hang; it is nonetheless a completely
                     different program (gofmt -l on stdin, formatting
                     nothing to write it back) from "gofmt -l on every
                     tracked file", which is not observable to a caller
                     as long as this repository has ANY tracked Go file,
                     which it always will.
  hazard in python: GONE, not merely unobserved. `check` below calls
                     `gofmt -l` directly with the file LIST as explicit
                     arguments (`["gofmt", "-l", *files]`), computed and
                     branched on in Python before the subprocess is ever
                     spawned, so a hypothetical zero-Go-file tree runs
                     `gofmt -l` with zero explicit paths -- which behaves
                     identically to the bash's xargs shape for the same
                     reason (no paths still means "read stdin"), and this
                     port does not call `gofmt` at all in that case
                     (`files` empty short-circuits to the OK branch
                     before any subprocess runs).
  held by:           the `if not files: ... return` guard in `check()`,
                     ahead of the `gofmt` subprocess call.
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import harness

PROGRAM = "check-gofmt"


def resolve_toplevel(root_arg: str | None) -> Path:
    start = Path(root_arg) if root_arg is not None else Path.cwd()
    if root_arg is not None and not start.is_dir():
        print(f"{PROGRAM}: {root_arg} is not a directory", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE)
    try:
        toplevel = harness.sh_out(["git", "-C", str(start), "rev-parse", "--show-toplevel"], check=True)
    except (harness.CommandFailed, harness.Failure):
        print(
            f"{PROGRAM}: {root_arg or start} is not inside a git work tree, and this reads the tracked file list",
            file=sys.stderr,
        )
        raise SystemExit(harness.EXIT_USAGE) from None
    return Path(toplevel)


def tracked_go_files(root: Path) -> list[str]:
    listed = subprocess.run(
        ["git", "ls-files", "-z", "*.go"],
        cwd=root,
        stdout=subprocess.PIPE,
        check=True,
    ).stdout
    return [p.decode("utf-8", "surrogateescape") for p in listed.split(b"\0") if p]


def body(root: Path) -> int:
    files = tracked_go_files(root)
    if not files:
        print("OK: all 0 tracked Go files are gofmt-clean.")
        return harness.EXIT_OK

    unformatted = harness.sh(["gofmt", "-l", *files], cwd=root, check=False).stdout
    unformatted_files = [line for line in unformatted.splitlines() if line]

    if not unformatted_files:
        print(f"OK: all {len(files)} tracked Go files are gofmt-clean.")
        return harness.EXIT_OK

    details = list(unformatted_files)
    details.append("")
    details.append("    Fix them with:")
    details.append("        gofmt -w " + " ".join(unformatted_files))
    details.append("")
    details.append("")
    details.append(
        "    Nothing in this repository looked at formatting until #417, which is how two"
    )
    details.append("    files stayed unformatted indefinitely while every gate step reported ok.")
    harness.die("these tracked Go files are not gofmt-clean:", *details)


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    root_arg = argv[0] if argv else None
    root = resolve_toplevel(root_arg)
    return harness.finish(lambda: body(root))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
