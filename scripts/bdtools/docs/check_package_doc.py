#!/usr/bin/env python3
"""Does every package's `go doc` overview still say what it is recorded as
saying, and is it still assembled from the same files? (issue #526)

A comment sitting immediately above `package` is the package doc. Not
"looks like it", is it: go/doc collects every one of them across a
package and concatenates them in sorted file order. Six documentation
lanes put a per-file opener there, so `go doc ./core/service` opened with
"This file is the operator's activity feed", activity.go introducing
itself, with the real overview several topics down. core/internal/state
went from one overview to eleven stacked openers the same way.

Every gate step was green throughout. `go build` and `go vet` do not care
where a comment sits, none of the linters .golangci.yml enables looks at
package documentation, and the campaign's own reviewers were reading
diffs of individual files, where an opener above `package` looks exactly
like an opener below the imports. The overview only exists once the files
are put together, and nothing was putting them together.

So this does, and records two things per package rather than one:

  the digest of the assembled doc   catches text that changed
  the files that carry a comment    catches a file that started carrying
    adjacent to `package`             one, which is the defect itself

The second is the durable half. A promoted opener always shows up as a
new carrier, in a line a reviewer can read, before anybody has to think
about what the digest means.

Size is deliberately not one of them. go/doc concatenates in sorted
order, so a promoted opener can land BEFORE the real overview and lead
it, which reads as a replacement and can leave the header shorter than it
was.

Exit code contract, which is all a gate step needs from it:

  0        every package matches scripts/docs/package-doc.baseline
  1        at least one does not, and the run printed which and how
  2        a usage or environment problem: not a git work tree, no
           baseline recorded yet, docguard itself could not read the
           tree, or a bad --against ref

A deliberate documentation change is meant to fail this once. Read what
it says, confirm the new carrier list is the one you meant, and record it:

  python3 scripts/bdtools/docs/check_package_doc.py --update

--against <ref> asks the other question, and it is the one #526 was
actually closed with: is every package overview in this tree the same TEXT
it was at some earlier commit? It extracts that commit, assembles both
sides through the same go/doc call, and prints a unified diff per package
that moved. The baseline above records digests, which tell a reviewer that
something changed; this shows them what.

Takes an optional directory: a git work tree to check instead of this
one. That is what `selftest.py` drives, so the mutation controls and the
real run go down the same code path rather than the controls exercising a
second implementation.

Ported from `scripts/docs/check-package-doc.sh` under EPIC I (#672 /
#662). `scripts/docs/check-package-doc.sh` stays as a real, runnable
shim: `scripts/tests/ci-local-gate.test.sh` FABRICATES a stand-in at that
literal path, and `scripts/ci-local.sh` still names it.

# PORTED-CHECK HAZARD NOTE

the per-package report when the baseline and the tree disagree
  hazard in bash:   the report was a `python3 - "$baseline" "$nowfile"
                     <<'PY'` heredoc: a SECOND, embedded interpreter,
                     invoked correctly here but exactly the shape (a
                     script calling out to an ad hoc inline script for
                     "the interesting part") that has hidden a silenced
                     check elsewhere in this repository before. A typo in
                     the heredoc delimiter or a stray `$` inside it that
                     bash decided to interpolate would have broken silently:
                     the outer `set -uo pipefail` does not extend into a
                     heredoc's OWN interpreter's exit status the way it
                     does a command's, so a python `SyntaxError` inside
                     the heredoc still exits non-zero and is still caught
                     correctly here (bash does check `$?` off the
                     substitution) -- but a python 2-vs-3 shebang mismatch
                     or a `python3` not on PATH inside a `--update` run
                     nobody re-tested since would have surfaced only when
                     someone actually broke a baseline, not before.
  hazard in python: GONE. `report_mismatch` below is an ordinary function
                     in the same interpreter as everything that calls it;
                     there is no second script, no heredoc delimiter, and
                     no process boundary for a shell-quoting mistake to
                     hide in.
  held by:          `report_mismatch` being a plain function, imported
                     and called, not a string template invoking a
                     subprocess.

a docguard read failure being an operational refusal, not a proof failure
  hazard in bash:   `nowfile=$(... ); status=$?; if status -ne 0: exit 2`
                     used `exit 2` -- this script's own usage/environment
                     code -- for a build or parse failure in docguard
                     itself, deliberately distinct from `exit 1`, which
                     this file reserves for an actual baseline mismatch.
                     A reviewer reading a bare exit status would still be
                     unable to tell "the documentation changed" from "the
                     tree does not build" apart without reading the text.
  hazard in python: STILL EXISTS, preserved rather than "fixed": this
                     port keeps the same two-code split (`SystemExit(2)`
                     for docguard failing to read the tree,
                     `harness.die` -> 1 for an actual mismatch) rather
                     than collapsing them, because collapsing them would
                     be an unannounced behaviour change to a contract
                     `scripts/ci-local.sh` and every caller of this
                     script's exit status already depend on.
  held by:           the explicit `raise SystemExit(harness.EXIT_USAGE)`
                     in `run_docguard_packages` below, kept distinct from
                     every `harness.die` call in this file.
"""

from __future__ import annotations

import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import harness

PROGRAM = "check-package-doc"
BASELINE_RELATIVE = "scripts/docs/package-doc.baseline"

USAGE = """\
usage: check_package_doc.py [--update | --against <ref>] [work-tree]

  --update         record the tree as it is now, instead of checking it
  --against <ref>  compare every package overview's TEXT against that
                   commit, and print a diff per package that moved
"""


def parse_args(argv: list[str]) -> tuple[bool, str | None, str | None]:
    update = False
    against: str | None = None
    root: str | None = None
    want_against = False
    for arg in argv:
        if want_against:
            against = arg
            want_against = False
            continue
        if arg == "--update":
            update = True
        elif arg == "--against":
            want_against = True
        elif arg in ("-h", "--help"):
            print(USAGE, end="")
            raise SystemExit(harness.EXIT_OK)
        elif arg.startswith("-"):
            print(f"{PROGRAM}: unknown argument: {arg}", file=sys.stderr)
            raise SystemExit(harness.EXIT_USAGE)
        else:
            root = arg
    if want_against:
        print(f"{PROGRAM}: --against needs a commit", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE)
    if against is not None and update:
        print(f"{PROGRAM}: --update and --against ask different questions; pick one", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE)
    return update, against, root


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


def run_docguard_packages(root: Path, *, full: bool = False, at: Path | None = None) -> str:
    """`(cd core && go run ./cmd/docguard packages ..)`, or, with `at` set,
    a pre-built docguard binary against an arbitrary tree -- the shape
    `--against` mode needs, since it reads two trees with one binary."""
    if at is not None:
        proc = harness.sh([str(at), *(["-full"] if full else []), "packages", str(root)], check=False)
    else:
        argv = ["go", "run", "./cmd/docguard", *(["-full"] if full else []), "packages", ".."]
        proc = harness.sh(argv, cwd=root / "core", check=False)
    if proc.returncode != 0:
        print(f"{PROGRAM}: docguard could not read the tree", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE)
    return proc.stdout


def parse_records(text: str) -> dict[tuple[str, str], tuple[str, list[str]]]:
    records: dict[tuple[str, str], tuple[str, list[str]]] = {}
    for line in text.splitlines():
        if not line.strip():
            continue
        path, name, digest, carriers = line.split("\t")
        records[(path, name)] = (digest, carriers.split(","))
    return records


def report_mismatch(baseline_text: str, now_text: str) -> list[str]:
    """One line per package that moved, naming the file that started or
    stopped carrying a comment adjacent to `package` rather than a raw
    diff of digest lines, which tells a reviewer holding a changed sha256
    nothing at all."""
    now = parse_records(now_text)
    was = parse_records(baseline_text)
    lines: list[str] = []
    for key in sorted(set(was) | set(now)):
        path, name = key
        if was.get(key) == now.get(key):
            continue
        if key not in was:
            lines.append(f"{path} ({name}) is a package the baseline has never seen.")
            lines.append(f"    carriers: {', '.join(now[key][1])}")
            continue
        if key not in now:
            lines.append(f"{path} ({name}) is in the baseline and not in the tree.")
            continue
        old_digest, old_carriers = was[key]
        new_digest, new_carriers = now[key]
        lines.append(f"{path} ({name})")
        gained = [f for f in new_carriers if f not in old_carriers]
        lost = [f for f in old_carriers if f not in new_carriers]
        if gained:
            lines.append("    these files now carry a comment adjacent to `package`, so their")
            lines.append(f"    text is part of the package overview: {', '.join(gained)}")
        if lost:
            lines.append(f"    these files no longer carry one: {', '.join(lost)}")
        if old_digest != new_digest and not gained and not lost:
            lines.append("    the same files carry it, and the text changed.")
        lines.append(f"    see it with: go doc ./{path}")
    return lines


def body_against(root: Path, against: str) -> int:
    try:
        harness.sh(["git", "rev-parse", "--verify", "--quiet", f"{against}^{{commit}}"], cwd=root, capture=True)
    except harness.CommandFailed:
        print(f"{PROGRAM}: {against} is not a commit in this repository", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE) from None

    with tempfile.TemporaryDirectory(prefix="backupd-package-doc-against.") as atmp_name:
        atmp = Path(atmp_name)
        tree_dir = atmp / "tree"
        tree_dir.mkdir()
        archive = subprocess.Popen(["git", "archive", against], cwd=root, stdout=subprocess.PIPE)
        assert archive.stdout is not None
        extract = subprocess.Popen(["tar", "-x", "-C", str(tree_dir)], stdin=archive.stdout)
        archive.stdout.close()
        extract.communicate()
        archive.wait()
        if archive.returncode != 0 or extract.returncode != 0:
            print(f"{PROGRAM}: could not extract {against}", file=sys.stderr)
            raise SystemExit(harness.EXIT_USAGE)

        docguard = atmp / "docguard"
        build = harness.sh(["go", "build", "-o", str(docguard), "./cmd/docguard"], cwd=root / "core", check=False)
        if build.returncode != 0:
            print(f"{PROGRAM}: could not build core/cmd/docguard", file=sys.stderr)
            for line in (build.stdout or "").splitlines():
                print(f"    {line}", file=sys.stderr)
            raise SystemExit(harness.EXIT_USAGE)

        # The same binary reads both trees, so a difference is a difference
        # in the documentation and never a difference in how it was read.
        before = run_docguard_packages(tree_dir, full=True, at=docguard)
        after = run_docguard_packages(root, full=True, at=docguard)

    if before == after:
        print(f"OK: every package overview is byte-identical to {against}.")
        return harness.EXIT_OK

    import difflib

    diff_lines = [line.rstrip("\n") for line in difflib.unified_diff(before.splitlines(), after.splitlines())]
    harness.die(
        f"every package overview is not byte-identical to {against}.",
        f"These package overviews differ from {against}:",
        "",
        *diff_lines,
        "",
        "Each hunk is inside a === <path> <package> block, which is that package's",
        "assembled go/doc overview and nothing else.",
    )


def body(root: Path, update: bool, against: str | None) -> int:
    if against is not None:
        return body_against(root, against)

    baseline_path = root / BASELINE_RELATIVE
    now_text = run_docguard_packages(root)

    if update:
        baseline_path.write_text(now_text)
        count = sum(1 for line in now_text.splitlines() if line.strip())
        print(f"OK: recorded {count} packages in {BASELINE_RELATIVE}.")
        return harness.EXIT_OK

    if not baseline_path.is_file():
        print(f"{PROGRAM}: {BASELINE_RELATIVE} is missing. Record it with --update.", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE)

    baseline_text = baseline_path.read_text()
    if baseline_text == now_text:
        count = sum(1 for line in now_text.splitlines() if line.strip())
        print(f"OK: all {count} package overviews match {BASELINE_RELATIVE}, carriers included.")
        return harness.EXIT_OK

    details = report_mismatch(baseline_text, now_text)
    details.append("")
    details.append("A file opener belongs BELOW the imports, where it is a file comment and a")
    details.append("reader still meets it on opening the file. Only the package's overview")
    details.append("belongs above `package`.")
    details.append("")
    details.append("If the change is deliberate, record it:")
    details.append("    python3 scripts/bdtools/docs/check_package_doc.py --update")
    harness.die(f"a package overview is not what {BASELINE_RELATIVE} records.", *details)


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    update, against, root_arg = parse_args(argv)
    root = resolve_toplevel(root_arg)
    return harness.finish(lambda: body(root, update, against))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
