"""Shared anchored-mutation machinery for every ported selftest under
`scripts/rcmtools/` that proves a gate can go red by planting a real
violation in a copy of the working tree.

Ported from `scripts/lib/selftest-swap.sh` (#458) under EPIC I (#672 /
#662). That file was ALREADY the fix for the twelve-domains-reinvent-the-
same-helpers defect #672 as a whole exists to close, shared by
`scripts/compat/selftest.sh` and `scripts/conformance/selftest.sh`
(`scripts/race/selftest.sh`, `scripts/retention/selftest.sh` and
`scripts/docs/selftest.sh` all grew the same dependency since). Answering
that with a second, private Python reimplementation per ported domain
would be the exact mistake #672 is about, one language later: N copies of
the same anchor-matching and stale-verdict bookkeeping, diverging from the
moment they land. So this is the ONE port, consumed by every one of this
programme's selftests that plants an anchored mutation:
`scripts/rcmtools/conformance/selftest.py`,
`scripts/rcmtools/race/selftest.py`, `scripts/rcmtools/retention/selftest.py`
and `scripts/rcmtools/docs/selftest.py`. `scripts/compat/selftest.sh`'s own
port consumes it too, once that lands.

`scripts/lib/selftest-swap.sh` stays exactly where it is and exactly what
it is until `scripts/compat/selftest.sh` -- its last bash caller -- is
itself ported: a bash library, SOURCED, whose whole reason for existing is
this same anchor discipline, so removing it out from under a caller still
naming it would strand that caller exactly the way #672's own rule for
shims forbids.

Every plant is anchored to a verbatim copy of product source, tabs and
all, in a module the author of the product change never opens. A refactor
that moves the anchored code drifts the anchor. An anchor that no longer
matches has to REFUSE to plant rather than silently plant nothing: a
mutation that planted nothing and then ran its gate would read as a clean
pass, which is the exact failure this whole file exists to rule out. So a
stale anchor is a third verdict, distinct from PASS and FAIL:
`AnchorTracker.stale_verdict` turns it into STALE ANCHOR, counts it, and
lets the run continue, rather than one control's drift taking every
control after it down with it (#458's own postmortem: `set -e` plus a
python `sys.exit` used to do exactly that, and it hid drift for 25 minutes
of gate time, one control at a time, three separate times).

`AnchorTracker(dry_run=True, ...)` is what `--check-anchors` runs:
every anchor against the real tree, checked and never planted, building
nothing. `scripts/rcmtools/selftest/check_anchors.py` (#458) runs it, in
this shape, for every selftest that has anchors.

# PORTED-CHECK HAZARD NOTE

the stale-anchor complaint reaching the terminal at all
  hazard in bash:   `_selftest_anchor` ran its match-and-report logic in a
                     CHILD python3 process (`out=$(python3 - ... <<'PY'
                     ... 2>&1)`), and the file's own comment calls the
                     trailing `2>&1` load-bearing: python's
                     `sys.exit(message)` writes to the child's stderr, and
                     without redirecting that onto the captured stdout
                     stream, the complaint would escape straight to the
                     terminal, unindented and out of order with the rest
                     of the run, while `$out` stayed empty and the calling
                     control's own verdict said nothing was wrong.
  hazard in python: GONE. There is no subprocess boundary left to get
                     wrong: `_anchor` below runs in this process, and a
                     stale anchor is recorded by calling `_note_stale`
                     directly rather than by one process's exit status
                     surviving a pipe into another's stdout.
  held by:          `_anchor` and `_note_stale` sharing one interpreter;
                     there is no redirection for a future edit to drop.

a tracked file `mutant()` cannot find on disk
  hazard in bash:   `git ls-files -z --cached --others --exclude-standard
                     | tar -cf - --null -T -` piped straight into `tar -xf
                     -` in the destination. A path `git ls-files` names
                     that is missing from the working tree (staged for
                     deletion but not committed, the common shape while a
                     change is mid-flight) makes the packing `tar` exit
                     non-zero; the whole pipeline runs under `set -euo
                     pipefail`, so the script dies right there, uncaught,
                     with tar's own error text and no word about which
                     selftest control was even attempting a copy.
  hazard in python: STILL EXISTS, and deliberately not smoothed over.
                     `shutil.copy2` raises `FileNotFoundError` for exactly
                     the same input, uncaught here, so the failure still
                     travels and still aborts the run rather than quietly
                     copying a mutant with a file missing from it -- which
                     would be the "plant lands somewhere the control does
                     not expect" failure the whole anchor discipline
                     exists to catch, one level up. The one thing that
                     changes for the better: `mutant()`'s caller sees a
                     Python traceback naming the exact path, not `tar`'s
                     opaque exit status.
  held by:          the absence of a `try/except FileNotFoundError` around
                     the copy loop in `mutant()`; deliberate.
"""

from __future__ import annotations

import shutil
import subprocess
import sys
from pathlib import Path
from typing import Callable, Sequence


def parse_selftest_args(argv: Sequence[str], program: str) -> bool:
    """The one flag every anchored selftest takes. Returns whether
    `--check-anchors` was given; prints usage and exits (0 for help, 2 for
    anything unrecognised) exactly as `selftest_parse_args` did."""
    dry_run = False
    for arg in argv:
        if arg == "--check-anchors":
            dry_run = True
        elif arg in ("-h", "--help"):
            print(f"usage: {program} [--check-anchors]")
            print()
            print("  --check-anchors  dry-run every mutation anchor against the real")
            print("                   tree, build nothing, and report the stale ones.")
            raise SystemExit(0)
        else:
            print(f"{program}: unknown argument: {arg}", file=sys.stderr)
            print(f"usage: {program} [--check-anchors]", file=sys.stderr)
            raise SystemExit(2)
    return dry_run


def _quoted(text: str) -> str:
    return "\n".join("  | " + line for line in text.split("\n"))


class AnchorTracker:
    """Owns every anchor swapped or dry-checked by one selftest run.

    One instance per selftest process, the way the bash library's module
    globals were one set per sourcing script. `root` and `tmp` are used
    only to shorten a complaint's path the way `_selftest_display` did;
    both are optional, and a complaint just carries the longer path
    without them.
    """

    def __init__(self, *, dry_run: bool, root: Path | None = None, tmp: Path | None = None) -> None:
        self.dry_run = dry_run
        self.root = root
        self.tmp = tmp
        self.anchors_checked = 0
        self.anchors_stale = 0
        self.stale_count = 0
        self.stale_controls: list[str] = []
        self.stale_pending: list[str] = []
        self.anchors_in_control = 0
        self.anchors_last = 0

    def _display(self, path: Path) -> str:
        shown = str(path)
        if self.tmp is not None:
            prefix = str(self.tmp) + "/"
            if shown.startswith(prefix):
                rest = shown[len(prefix) :]
                # Strip the mutant's own directory component too (tmp/<name>/...),
                # the way the bash version's second ${shown#*/} did.
                if "/" in rest:
                    rest = rest.split("/", 1)[1]
                shown = rest
        if self.root is not None:
            prefix = str(self.root) + "/"
            if shown.startswith(prefix):
                shown = shown[len(prefix) :]
        return shown

    def _note_stale(self, text: str) -> None:
        self.anchors_stale += 1
        self.stale_pending.append(text)

    def _anchor(self, mode: str, path: Path, old: str, new: str = "") -> None:
        display = self._display(path)
        self.anchors_checked += 1
        self.anchors_in_control += 1
        try:
            src = path.read_text()
        except OSError as exc:
            self._note_stale(f"{display}: {exc}")
            return

        n = src.count(old)
        if n == 1:
            if mode == "plant":
                path.write_text(src.replace(old, new, 1))
            return

        if n > 1:
            self._note_stale(
                f"{display} contains this anchor {n} times, so the plant would land in more "
                f"than one place:\n{_quoted(old)}"
            )
            return

        # Where it stopped matching: the first anchor line no longer present,
        # so "somebody refactored something" becomes a line the person
        # holding the diff recognises.
        lines = old.split("\n")
        kept = 0
        while kept < len(lines) and "\n".join(lines[: kept + 1]) in src:
            kept += 1
        detail = f"{display} no longer contains this anchor:\n{_quoted(old)}"
        if kept == 0:
            detail += "\n  not even its first line survives, so the whole block moved or went away."
        else:
            detail += (
                f"\n  its first {kept} line(s) are still there; it stops matching at:\n{_quoted(lines[kept])}"
            )
        self._note_stale(detail)

    def swap(self, path: Path, old: str, new: str) -> None:
        """Replace one exact string in a mutant tree. Under `--check-anchors`
        it only looks, so the same call site is both the mutation and its
        own drift check and the two cannot disagree."""
        if self.dry_run:
            self._anchor("dry", path, old)
        else:
            self._anchor("plant", path, old, new)

    def swap_dry(self, path: Path, old: str) -> None:
        """Ask whether an anchor is still there, and never write."""
        self._anchor("dry", path, old)

    def mutate(self, path: Path, mutate: Callable[[Path], None]) -> None:
        """Run `mutate(path)` -- an in-place edit more involved than one
        literal replacement -- against `path`, or under `--check-anchors`
        against a scratch copy, exactly the way `swap` does for a literal
        one. `mutate` raises to say its precondition no longer holds; that
        exception is caught here and turned into a stale-anchor complaint
        rather than allowed to take the whole run down, which is `swap`'s
        own contract for a mismatched anchor.
        """
        display = self._display(path)
        if self.dry_run:
            self.anchors_checked += 1
            self.anchors_in_control += 1
            if self.tmp is None:
                self._note_stale(
                    f"{display}: its precondition could not be checked, "
                    "because this run has no scratch directory to copy the file into"
                )
                return
            probe = self.tmp / "anchor-probe"
            try:
                shutil.copy2(path, probe)
            except OSError:
                self._note_stale(f"{display}: the mutation names a file that is not in this tree")
                return
            try:
                mutate(probe)
            except Exception as exc:  # a mutation's own precondition failure, not a bug here
                self._note_stale(f"{display}: the mutation would refuse to plant:\n{_quoted(str(exc))}")
            return

        try:
            mutate(path)
        except Exception as exc:  # see above
            self._note_stale(f"{display}: the mutation refused to plant:\n{_quoted(str(exc))}")

    def stale_verdict(self, label: str) -> bool:
        """The third verdict. True when something refused to plant for this
        control, which means the caller must NOT run its gate: a tree with
        no violation in it would pass, and reading that as a pass is the
        whole thing being guarded against."""
        self.anchors_last = self.anchors_in_control
        self.anchors_in_control = 0
        if not self.stale_pending:
            return False
        print(f"STALE ANCHOR: {label}. Nothing was planted, so this control did not run.", file=sys.stderr)
        for block in self.stale_pending:
            for line in block.splitlines():
                print(f"    {line}", file=sys.stderr)
        self.stale_count += 1
        self.stale_controls.append(label)
        self.stale_pending = []
        return True

    def anchors_only(self, label: str) -> bool:
        """True when this run is only checking anchors, so the caller
        returns before building or running anything."""
        if not self.dry_run:
            return False
        if self.anchors_last == 0:
            print(f"  (no anchors):  {label}")
        elif self.anchors_last == 1:
            print(f"  ok (1 anchor): {label}")
        else:
            print(f"  ok ({self.anchors_last} anchors): {label}")
        return True

    def stale_summary(self) -> None:
        """Every stale control from this one run, printed once at the end,
        which is the point of #458: one run names the whole list instead of
        one gate per stale control."""
        if self.stale_count == 0:
            return
        print(file=sys.stderr)
        print(
            f"STALE ANCHORS: {self.stale_count} of these controls could not plant their violation, "
            "so they proved nothing:",
            file=sys.stderr,
        )
        for label in self.stale_controls:
            print(f"  - {label}", file=sys.stderr)
        print(
            "Each one quotes product source that has since moved. Re-anchor it to the code as",
            file=sys.stderr,
        )
        print(
            "it is now rather than deleting the control, and check every anchor in seconds with:",
            file=sys.stderr,
        )
        print("    python3 scripts/rcmtools/selftest/check_anchors.py", file=sys.stderr)


def mutant(root: Path, tmp: Path, name: str, *, dry_run: bool) -> Path:
    """A copy of the working tree (tracked files plus untracked, non-ignored
    ones -- never plain `git ls-files`, which is what let a stale selftest
    elsewhere in this repository "catch" every mutation by invoking a check
    that did not exist in the copy at all) at `tmp/name`.

    Under `dry_run` nothing is planted, so there is nothing to copy into and
    no reason to spend a copy per control: every swap reads the real tree
    instead, which is the tree whose drift is being looked for, and this
    returns `root` unchanged.
    """
    if dry_run:
        return root
    dest = tmp / name
    dest.mkdir(parents=True, exist_ok=True)
    listed = subprocess.run(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        cwd=root,
        stdout=subprocess.PIPE,
        check=True,
    ).stdout
    for raw in listed.split(b"\0"):
        if not raw:
            continue
        rel = raw.decode("utf-8", "surrogateescape")
        src = root / rel
        dst = dest / rel
        dst.parent.mkdir(parents=True, exist_ok=True)
        # No guard against a missing `src`: see this module's PORTED-CHECK
        # HAZARD NOTE. A tracked path git believes exists and does not is a
        # precondition failure worth aborting loudly for, not a file to skip.
        shutil.copy2(src, dst, follow_symlinks=False)
    return dest


class Tally:
    """Pass/fail bookkeeping, shared by every selftest's `ok`/`bad` pair
    (`scripts/docs/selftest.sh` and `scripts/format/selftest.sh` both had
    their own copy of exactly this)."""

    def __init__(self) -> None:
        self.passed = 0
        self.failed = 0

    def ok(self, label: str, detail: str | None = None) -> None:
        print(f"  ok:   {label}")
        if detail:
            print(f"      -> {detail}")
        self.passed += 1

    def bad(self, label: str, detail: str = "") -> None:
        print(f"SELFTEST FAIL: {label}", file=sys.stderr)
        if detail:
            for line in detail.splitlines():
                print(f"    {line}", file=sys.stderr)
        self.failed += 1
