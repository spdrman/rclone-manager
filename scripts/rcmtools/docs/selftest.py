#!/usr/bin/env python3
"""Controls for the package-documentation gate (issue #526).

#526 is a check that did not exist. Six documentation lanes moved file
openers to sit immediately above `package`, which is where go/doc reads
the package overview from, and every gate step stayed green through all
of it, because nothing in this repository ever assembled a package
overview and looked at it. `go doc ./core/service` opened with
"This file is the operator's activity feed" for days.

`check_package_doc.py` is the check that now does. This is the proof that
it can go red, and it is deliberately paired: the same mutation runs past
a second, independent check that has to STAY SILENT.

  G2  a comment promoted to sit adjacent to `package`
      -> check_package_doc.py goes red and names the file
  G3  the same mutation, unchanged
      -> check_comments_only.py stays green

G3 is not decoration. Without it, "check_package_doc.py went red" is also
what you would see from a checker that fires on any edit at all, and the
two halves of this fix would not be measuring different things. The
promotion changes what `go doc` prints and changes no token in the file,
so the pair proves each check answers its own question. G4 is G3's
positive control, because a checker that never speaks is also silent.

The mutation goes into the real core/service/activity.go and is put back,
rather than into a copy. That file is the one #526 is named for, and a
control planted in a synthetic fixture would not notice the day somebody
promotes its opener again for real. Restoration is a trap, and G5 checks
it actually happened rather than trusting it.

`python3 scripts/rcmtools/docs/selftest.py --check-anchors` dry-runs the
anchors against the real tree, building nothing and writing nothing,
which is what `scripts/rcmtools/selftest/check_anchors.py` runs.

Ported from `scripts/docs/selftest.sh` under EPIC I (#672 / #662).
`scripts/docs/selftest.sh` stays as a real, runnable shim:
`scripts/tests/ci-local-gate.test.sh` FABRICATES a stand-in at that
literal path, and `scripts/ci-local.sh` still names it.

# PORTED-CHECK HAZARD NOTE

restoration of the real, mutated file surviving an interrupted run
  hazard in bash:   `trap 'restore; rm -rf "$tmp"' EXIT` restores
                     `core/service/activity.go` from a pristine copy on
                     ANY exit, including a `set -uo pipefail`-triggered
                     one mid-control -- but the script never traps INT or
                     TERM explicitly, relying on bash's own default of
                     translating a caught signal into the same EXIT trap
                     firing during an interactive run. A `kill -9` leaves
                     the real file mutated with nothing to catch it,
                     which is a real gap the bash never claimed to close.
  hazard in python: STILL EXISTS, to the same degree and no further.
                     `harness.install_signal_handlers()` turns INT/TERM
                     into `Interrupted`, which `finish()` catches and
                     tears down through on the way out -- covering
                     exactly the signals the bash's default trap covered
                     and no more. SIGKILL is unmaskable in both languages;
                     neither this port nor the bash it replaces can do
                     anything about it.
  held by:          `harness.install_signal_handlers()` plus the
                     `finally: restore()` in `body()` below, together
                     covering precisely the same signal set the bash
                     EXIT trap did.
"""

from __future__ import annotations

import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import harness, selftest_swap

PROGRAM = "docs selftest"

SUBJECT = "core/service/activity.go"
SUBJECT_PACKAGE = "core/service"

OPENER_BELOW_IMPORTS = ')\n\n// This file is the operator\'s activity feed: the read side of the'

PACKAGE_CLAUSE = "package service\n\nimport ("

PROMOTED = (
    "// A file opener promoted to package documentation by\n"
    "// scripts/rcmtools/docs/selftest.py (issue #526). If you are reading this in a real\n"
    "// checkout, that self-test did not get to put the file back:\n"
    "//\n"
    "//\tgit checkout -- core/service/activity.go\n"
    "package service\n\n"
    "import ("
)

LISTACTIVITY = (
    "func (b *BackupService) ListActivity(ctx context.Context, limit int) ([]ActivityEvent, error) {"
)
LISTACTIVITY_RENAMED = (
    "func (b *BackupService) ListActivityRenamedBySelftest(ctx context.Context, limit int) "
    "([]ActivityEvent, error) {"
)


def run_pkgdoc(root: Path) -> tuple[int, str]:
    proc = subprocess.run(
        ["bash", str(root / "scripts/docs/check-package-doc.sh")],
        cwd=root,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    return proc.returncode, proc.stdout


def run_commentsonly(root: Path, only: str) -> tuple[int, str]:
    proc = subprocess.run(
        [sys.executable, str(root / "scripts/rcmtools/docs/check_comments_only.py"), "HEAD", "--only", only],
        cwd=root,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    return proc.returncode, proc.stdout


def git_clean(root: Path, path: str) -> bool:
    return subprocess.run(["git", "diff", "--quiet", "HEAD", "--", path], cwd=root).returncode == 0


def body(root: Path, dry_run: bool) -> int:
    tally = selftest_swap.Tally()
    subject_path = root / SUBJECT

    with tempfile.TemporaryDirectory(prefix="rclone-manager-docs-selftest.") as tmp_name:
        tmp = Path(tmp_name)
        tracker = selftest_swap.AnchorTracker(dry_run=dry_run, root=root, tmp=tmp)
        pristine = tmp / "activity.go.pristine"

        def restore() -> None:
            if pristine.is_file():
                subject_path.write_bytes(pristine.read_bytes())

        try:
            print(f"==> anchors in {SUBJECT}")
            tracker.swap_dry(subject_path, OPENER_BELOW_IMPORTS)
            tracker.swap_dry(subject_path, PACKAGE_CLAUSE)
            tracker.swap_dry(subject_path, LISTACTIVITY)
            if tracker.anchors_stale == 0:
                tally.ok("all three anchors match exactly once")
            else:
                tally.bad(
                    "an anchor no longer matches the tree it names:",
                    "\n".join(tracker.stale_pending)
                    + "\nRe-aim it at the code as it is now rather than deleting the control.",
                )
                tracker.stale_pending = []

            if dry_run:
                print()
                if tally.failed == 0:
                    print("==> docs selftest anchors: ok (3 anchors checked)")
                    return harness.EXIT_OK
                print("==> docs selftest anchors: FAILED", file=sys.stderr)
                return harness.EXIT_FAILED

            pristine.write_bytes(subject_path.read_bytes())

            # ------------------------------------------------------- G1
            print()
            print("==> G1 negative control: the real tree passes both checks")
            code, out = run_pkgdoc(root)
            if code == 0:
                tally.ok("check_package_doc.py is green on the unmutated tree, so G2's red means something")
            else:
                tally.bad("check_package_doc.py FAILS on the unmutated tree, so its failures say nothing", out)

            subject_clean = git_clean(root, SUBJECT)
            if subject_clean:
                tally.ok(f"{SUBJECT} matches HEAD, so G3 and G4 will be answering about the plant")
            else:
                stat = subprocess.run(
                    ["git", "diff", "--stat", "HEAD", "--", SUBJECT], cwd=root, capture_output=True, text=True
                ).stdout
                tally.bad(f"{SUBJECT} has uncommitted changes, so G3 and G4 would be measuring those instead", stat)

            # ------------------------------------------------------- G2
            print()
            print(r"==> G2 a comment promoted adjacent to `package` turns the package-doc check red")
            tracker.swap(subject_path, PACKAGE_CLAUSE, PROMOTED)
            if tracker.stale_pending:
                tally.bad("the promotion refused to plant:", "\n".join(tracker.stale_pending))
            else:
                code, out = run_pkgdoc(root)
                if code == 0:
                    tally.bad(
                        "check_package_doc.py PASSED with a file opener sitting in the package overview, "
                        "which is #526 exactly",
                        out,
                    )
                elif SUBJECT_PACKAGE not in out:
                    tally.bad("it went red but never named the package", out)
                elif "activity.go" not in out:
                    tally.bad("it named the package but not the file that started carrying the comment", out)
                else:
                    tally.ok(f"red, and it names both {SUBJECT_PACKAGE} and activity.go")

            # ------------------------------------------------------- G3
            print()
            print("==> G3 independence: the same mutation leaves the token-level check silent")
            if not subject_clean:
                tally.bad(f"skipped, because G1 found {SUBJECT} already modified")
            elif tracker.stale_pending:
                tally.bad("skipped, because the promotion above did not plant")
            else:
                code, out = run_commentsonly(root, SUBJECT)
                if code == 0:
                    tally.ok(
                        "green: the promotion changes what go doc prints and changes no token, "
                        "so the two checks are asking different questions"
                    )
                else:
                    tally.bad(
                        "check_comments_only.py went RED on a pure comment move, so it is not the "
                        "independent second opinion G2 needs",
                        out,
                    )

            restore()
            tracker.stale_pending = []

            # ------------------------------------------------------- G4
            print()
            print("==> G4 positive control for G3: a real token change does turn it red")
            tracker.swap(subject_path, LISTACTIVITY, LISTACTIVITY_RENAMED)
            if not subject_clean:
                tally.bad(f"skipped, because G1 found {SUBJECT} already modified")
            elif tracker.stale_pending:
                tally.bad("the rename refused to plant:", "\n".join(tracker.stale_pending))
                tracker.stale_pending = []
            else:
                code, out = run_commentsonly(root, SUBJECT)
                if code == 0:
                    tally.bad("check_comments_only.py PASSED a renamed function, so G3's silence proves nothing", out)
                elif SUBJECT not in out:
                    tally.bad("it went red but never named the file", out)
                else:
                    tally.ok(f"red, and it names {SUBJECT}")

            restore()

            # ------------------------------------------------------- G5
            print()
            print("==> G5 the real file is back the way it was")
            if git_clean(root, SUBJECT):
                tally.ok(f"{SUBJECT} matches HEAD again")
            elif not subject_clean:
                tally.ok(f"{SUBJECT} still carries the local edit G1 found, and nothing this script planted")
            else:
                diff = subprocess.run(
                    ["git", "diff", "HEAD", "--", SUBJECT], cwd=root, capture_output=True, text=True
                ).stdout
                tally.bad(
                    f"{SUBJECT} is still modified, so a mutation escaped. "
                    f"Put it back with: git checkout -- {SUBJECT}",
                    "\n".join(diff.splitlines()[:30]),
                )
        finally:
            restore()

    print()
    if tally.failed == 0:
        print(f"==> docs selftest: ok ({tally.passed} controls)")
        return harness.EXIT_OK
    print(f"==> docs selftest: {tally.failed} failed, {tally.passed} passed", file=sys.stderr)
    return harness.EXIT_FAILED


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    dry_run = selftest_swap.parse_selftest_args(argv, "selftest.py")
    root = harness.repo_root(Path(__file__))
    return harness.finish(lambda: body(root, dry_run))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
