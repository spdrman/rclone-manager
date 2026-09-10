#!/usr/bin/env python3
"""Is every Go change between a ref and this work tree a comment change and
nothing else? (issue #526)

#526's fix moves prose. It touches around thirty files and changes no
behaviour, and "changes no behaviour" is the kind of claim a reviewer
cannot check by reading, because the diffs are large, the moved blocks
are long, and a real edit hidden in the middle of one would look like
more of the same.

So it gets proven instead. Every changed Go file is scanned at both
revisions with go/scanner in its default mode, which does not emit
comments at all, and the two token streams have to be identical. That is
the compiler's own view of the file with the prose taken out: same
tokens, same program.

On its own that would be too weak in one specific way, and #526's fix
walks straight into it. A `//go:build` line is a comment to the scanner
and a build constraint to the toolchain, so a header moved from above it
to below it changes which platforms compile the file while the token
stream stays byte-identical. core/service/lock_unix.go and
core/service/lock_other.go are exactly that shape and both were in the
fix. So core/cmd/docguard prints every //go: directive and // +build line
alongside the tokens, with which side of the package clause it sits on,
and this compares those too.

A file that exists at only one of the two revisions has no token stream
to compare, so this cannot speak for it either way. It is named in the
output rather than passed over silently: the reader has to be able to see
the edge of what was proven, or a branch that added a whole file of new
behaviour alongside the comment moves would read as "comments-only" on
the strength of a check that never looked at it.

Exit code contract:

  0        every Go file present at BOTH revisions differs only in
           comments; any added or deleted file is named in the output
  1        at least one differs in tokens or in directives, and the run
           printed which
  2        a usage or environment problem

usage: check_comments_only.py <ref> [work-tree] [--only <path>]...

  ref        what to compare against, usually origin/main
  work-tree  a git work tree to check instead of this one
  --only     restrict the comparison to one path, repeatable. That is
             what `selftest.py` drives: its controls plant one mutation
             in one real file, and a whole-tree comparison would also be
             answering for whatever else the person running the gate
             happens to have uncommitted.

Ported from `scripts/docs/check-comments-only.sh` under EPIC I (#672 /
#662). `scripts/docs/check-comments-only.sh` is DELETED outright, not
kept as a shim: `scripts/tests/ci-local-gate.test.sh` fabricates no
stand-in at that path, and its only caller was `scripts/docs/selftest.sh`
(now `selftest.py`), which calls `body()` here directly.

# PORTED-CHECK HAZARD NOTE

a changed path git reports that no longer exists on disk
  hazard in bash:   `if [ ! -f "$path" ]; then removed=...; continue; fi`
                     ran under `set -uo pipefail` but NOT `set -e` (this
                     script never opts into it), so a path git considers
                     changed but that has since vanished from the work
                     tree was already handled deliberately, in an
                     explicit branch. No hazard in the original; this is
                     the ONE ported file in this domain whose bash was
                     already careful about the exact failure mode the
                     other hazard notes in this package are about.
  hazard in python: preserved as the same explicit branch below
                     (`if not after_path.is_file()`), for the same reason.
                     Recorded here so a reader scanning every file in this
                     package for "was this checked" finds an answer
                     rather than a gap.
  held by:          the explicit `is_file()` check in `body()` below,
                     checked before ANY read of the file is attempted.
"""

from __future__ import annotations

import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import harness

PROGRAM = "check-comments-only"

USAGE = "usage: check_comments_only.py <ref> [work-tree] [--only <path>]...\n"


def parse_args(argv: list[str]) -> tuple[str, str | None, list[str]]:
    ref: str | None = None
    root: str | None = None
    only: list[str] = []
    want_only = False
    for arg in argv:
        if want_only:
            only.append(arg)
            want_only = False
            continue
        if arg == "--only":
            want_only = True
        elif arg in ("-h", "--help"):
            print(USAGE, end="")
            raise SystemExit(harness.EXIT_OK)
        elif arg.startswith("-"):
            print(f"{PROGRAM}: unknown argument: {arg}", file=sys.stderr)
            raise SystemExit(harness.EXIT_USAGE)
        else:
            if ref is None:
                ref = arg
            else:
                root = arg
    if want_only:
        print(f"{PROGRAM}: --only needs a path", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE)
    if ref is None:
        print(USAGE, end="", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE)
    return ref, root, only


def resolve_toplevel(root_arg: str | None) -> Path:
    start = Path(root_arg) if root_arg is not None else Path.cwd()
    if root_arg is not None and not start.is_dir():
        print(f"{PROGRAM}: {root_arg} is not a directory", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE)
    try:
        toplevel = harness.sh_out(["git", "-C", str(start), "rev-parse", "--show-toplevel"], check=True)
    except (harness.CommandFailed, harness.Failure):
        print(f"{PROGRAM}: {root_arg or start} is not inside a git work tree", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE) from None
    return Path(toplevel)


def body(root: Path, ref: str, only: list[str]) -> int:
    verify = subprocess.run(
        ["git", "rev-parse", "--verify", "--quiet", f"{ref}^{{commit}}"],
        cwd=root,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    if verify.returncode != 0:
        print(f"{PROGRAM}: {ref} is not a commit in this repository", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE)

    with tempfile.TemporaryDirectory(prefix="rclone-manager-comments-only.") as tmp_name:
        tmp = Path(tmp_name)

        # Built once. `go run` per file would be a compile per file, and
        # this is meant to be cheap enough to run on every branch.
        guard = tmp / "docguard"
        build = harness.sh(["go", "build", "-o", str(guard), "./cmd/docguard"], cwd=root / "core", check=False)
        if build.returncode != 0:
            print(f"{PROGRAM}: could not build core/cmd/docguard", file=sys.stderr)
            for line in (build.stdout or "").splitlines():
                print(f"    {line}", file=sys.stderr)
            raise SystemExit(harness.EXIT_USAGE)

        if only:
            # git diff still decides what actually changed, so --only
            # narrows the question and never answers it: a path that is
            # named and unchanged is reported as such rather than counted
            # as agreement.
            changed_out = harness.sh(["git", "diff", "--name-only", ref, "--", *only], cwd=root).stdout.rstrip("\n")
            changed = [line for line in changed_out.splitlines() if line]
            if not changed:
                print(
                    f"{PROGRAM}: none of the {len(only)} path(s) named with --only differ from {ref},",
                    file=sys.stderr,
                )
                print("                     so there is nothing here to prove anything about.", file=sys.stderr)
                raise SystemExit(harness.EXIT_USAGE)
        else:
            changed_out = harness.sh(["git", "diff", "--name-only", ref, "--", "*.go"], cwd=root).stdout.rstrip("\n")
            changed = [line for line in changed_out.splitlines() if line]

        same = 0
        added = 0
        removed = 0
        problems: list[str] = []
        outside: list[str] = []

        for rel in changed:
            before_path = tmp / "before.go"
            after_path = root / rel

            got_before = subprocess.run(
                ["git", "show", f"{ref}:{rel}"],
                cwd=root,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
            )
            if got_before.returncode != 0:
                added += 1
                outside.append(f"{rel} (added, so it has no earlier token stream to differ from)")
                continue
            before_path.write_bytes(got_before.stdout)

            if not after_path.is_file():
                removed += 1
                outside.append(f"{rel} (deleted, so it has no current token stream)")
                continue

            before_tok = harness.sh([str(guard), "tokens", str(before_path)], check=False)
            if before_tok.returncode != 0:
                problems.append(f"{rel} could not be scanned at {ref}: {(before_tok.stderr or '').strip()}")
                continue
            after_tok = harness.sh([str(guard), "tokens", str(after_path)], check=False)
            if after_tok.returncode != 0:
                problems.append(f"{rel} could not be scanned in the work tree: {(after_tok.stderr or '').strip()}")
                continue

            if before_tok.stdout == after_tok.stdout:
                same += 1
                continue

            import difflib

            diff = list(
                difflib.unified_diff(
                    before_tok.stdout.splitlines(),
                    after_tok.stdout.splitlines(),
                )
            )[:20]
            problems.append(f"{rel} differs in more than comments:")
            problems.extend(f"      {line}" for line in diff)

        total = len(changed)
        compared = total - added - removed

        if not problems:
            details = []
            if outside:
                details.append("")
                details.append(f"{added} added and {removed} deleted, which this cannot speak for either way:")
                details.extend(f"  {line}" for line in outside)
            print(
                f"OK: all {compared} of {total} changed Go files present at both revisions have the same"
            )
            print(f"    token stream and the same //go: directives as at {ref}, so those changes are")
            print("    comments and nothing else.")
            for line in details:
                print(f"    {line}" if line else "")
            return harness.EXIT_OK

        harness.die(f"the change against {ref} is not comments-only.", *problems)


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    ref, root_arg, only = parse_args(argv)
    root = resolve_toplevel(root_arg)
    return harness.finish(lambda: body(root, ref, only))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
