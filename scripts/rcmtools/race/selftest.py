#!/usr/bin/env python3
"""Positive controls for the gate's race detector (issue #417).

`-race` is a check with the same shape as every other one this repository
has had to learn to distrust: it passes silently, it passes when it is not
there at all, and the difference between "this tree has no data race" and
"nobody asked" is invisible in the output. The gate ran no `-race` anywhere
until #417, and the one test in this repository whose own doc says it
only means something under the detector
(`service.TestCreateBackupSet_ConcurrentWithReadersDoesNotRace`, written
for PR #155's mandatory review) had therefore never once been run the way
it says it has to be.

So the detector gets the same treatment the FR-35 compatibility cells and
the composed conformance cells got in #242: a real data race is planted
in real product source, in a copy of the working tree, and the step has
to catch it. Three cells, because catching it is only one of the three
things that have to be true:

  R1  the unmutated tree is clean under -race. Without this, R2 would
      also pass against a tree that is red for some unrelated reason.
  R2  the planted race is caught, and the report names the write that
      plants it. A bare non-zero exit is not enough: a mutant that failed
      to compile would give one.
  R3  the same mutant is INVISIBLE without -race. This is the cell that
      makes the other two mean something: it proves the detector is what
      caught the race, not an assertion that would have caught it anyway,
      which is the whole claim #417 is buying.

Whether the gate still ASKS for the detector is a different question and
is answered somewhere cheaper: Group K of
`scripts/tests/ci-local-gate.test.sh` scans `scripts/ci-local.sh` for a Go
suite that runs without -race, and proves that scan can fail. This file
is about whether the detector has teeth once it is asked.

The plant is anchored to a verbatim copy of product source, tabs and all,
in the shared way `scripts/rcmtools/compat/selftest.py` and
`scripts/rcmtools/conformance/selftest.py` already are, so a refactor
that moves the code cannot leave this control quietly planting nothing.
`python3 scripts/rcmtools/race/selftest.py --check-anchors` is that drift
check on its own, and `scripts/rcmtools/selftest/check_anchors.py` runs it
for every selftest that has anchors.

Cost: one package, one test, three runs. The mutant is one file deep in a
leaf package and Go's build cache is content-addressed, so the copies
rebuild `service` and nothing under it.

Ported from `scripts/race/selftest.sh` under EPIC I (#672 / #662).
`scripts/race/selftest.sh` stays as a real, runnable shim:
`scripts/tests/ci-local-gate.test.sh` FABRICATES a stand-in at that
literal path, and `scripts/ci-local.sh` still names it. This file
consumes `scripts/rcmtools/selftest_swap.py`'s `AnchorTracker` rather than
reimplementing anchor-matching a second time, exactly the way
`scripts/rcmtools/conformance/selftest.py` does.

# PORTED-CHECK HAZARD NOTE

`go test`'s own exit status colliding with a verdict it does not have
  hazard in bash:   `race_gate`/`plain_gate` ran inside
                     `if race_gate "$dir" >"$tmp/out" 2>&1; then ... else
                     ... fi`, so the only thing read was bash's own exit
                     status of the compound command, and `set -euo
                     pipefail` would have taken the whole script down on
                     any accidental unguarded use elsewhere -- there was
                     nowhere for a stray exit code to travel unnoticed.
  hazard in python: STILL EXISTS in the shape this whole campaign keeps
                     finding: this repository's own harness reserves exit
                     3 (`EXIT_CANNOT_RUN`, promoted to `EXIT_INCOMPLETE`
                     by `harness.finish`) for "this machine could not
                     perform the proof". `go test -race` has no such
                     meaning for 3 and is not expected to produce it, but
                     a helper that ran it through `harness.sh` (which
                     RAISES on an unguarded non-zero) rather than reading
                     its `returncode` directly would convert an expected,
                     meaningful failure -- the planted race actually being
                     caught -- into an uncaught `CommandFailed`, which
                     `harness.finish` turns into the mutant's own exit
                     status. If that status ever happened to be 3 (it is
                     not, today, but nothing in `go test`'s own contract
                     rules it out for an unrelated reason -- an internal
                     panic, a build tool failure with its own exit code),
                     the run would report CANNOT RUN for a control that
                     actually ran and caught something, which is exactly
                     the collision `perf`'s port found between `jq`'s exit
                     code and the harness's own INCOMPLETE verdict.
  held by:          `race_gate`/`plain_gate` below always run with
                     `subprocess.run(..., text=True)` directly -- never
                     through `harness.sh` -- and hand their caller an
                     explicit `(returncode, output)` pair. Every one of
                     `expect_race_caught`, `expect_race_missed` and
                     `expect_gate_passes` branches on that `returncode`
                     itself and never lets it reach `harness.finish`'s own
                     translation table, so a 3 (or any other code) from
                     `go test` is read as this file's own FAIL, not folded
                     into the machine-capability verdict.

a mutant that failed to compile reading as the race being "caught"
  hazard in bash:   `grep -qF 'WARNING: DATA RACE' "$tmp/out"` and then
                     `grep -qF "$needle" "$tmp/out"` are both required, in
                     that order, specifically so a mutant that merely
                     fails to build (a bare non-zero exit) cannot pass as
                     R2.
  hazard in python: STILL EXISTS, faithfully: `expect_race_caught` runs
                     the identical two-substring check in the same order
                     and no more. Narrowing it further is out of scope for
                     a port.
  held by:          nothing further than the two substring checks
                     themselves, exactly as inherited.
"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import harness, selftest_swap

PROGRAM = "race selftest"

# The one test that exercises the shape #417 is about: CreateBackupSet
# hot-reloading {inner, revision} while 150 goroutines read it. Its own
# doc says it proves nothing without the detector, which is exactly why
# it is the vehicle here.
RACE_TEST = "^TestCreateBackupSet_ConcurrentWithReadersDoesNotRace$"


def race_gate(tree: Path) -> tuple[int, str]:
    """Run the detector over the racing test in `tree`, the same way the
    gate's own core step runs it over the whole package set."""
    proc = subprocess.run(
        ["go", "test", "-race", "-count=1", "-run", RACE_TEST, "./service/"],
        cwd=tree / "core",
        env={**os.environ, "GOWORK": "off"},
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    return proc.returncode, proc.stdout


def plain_gate(tree: Path) -> tuple[int, str]:
    """The identical run with the detector off, which is what the gate
    did before #417."""
    proc = subprocess.run(
        ["go", "test", "-count=1", "-run", RACE_TEST, "./service/"],
        cwd=tree / "core",
        env={**os.environ, "GOWORK": "off"},
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    return proc.returncode, proc.stdout


def expect_race_caught(
    tracker: selftest_swap.AnchorTracker, tally: selftest_swap.Tally, label: str, tree: Path, needle: str
) -> None:
    """Red is not enough. The report has to name the racing access, or a
    mutant that simply broke the build reads as this control passing."""
    if tracker.stale_verdict(label):
        return
    if tracker.anchors_only(label):
        return
    code, out = race_gate(tree)
    if code == 0:
        tally.bad(f"{label}. -race PASSED against a planted data race.", out)
        return
    if "WARNING: DATA RACE" not in out:
        tally.bad(f"{label}. The run failed, but not with a data race report, so something else broke.", out)
        return
    if needle not in out:
        tally.bad(
            f"{label}. A race was reported, but not the one that was planted.",
            f"expected the report to name: {needle}\n{out}",
        )
        return
    tally.ok(f"caught: {label}")


def expect_race_missed(
    tracker: selftest_swap.AnchorTracker, tally: selftest_swap.Tally, label: str, tree: Path
) -> None:
    """The other direction, and the reason the other cells mean anything:
    the same tree, the same test, the detector off, and it must go
    green."""
    if tracker.stale_verdict(label):
        return
    if tracker.anchors_only(label):
        return
    code, out = plain_gate(tree)
    if code == 0:
        tally.ok(f"missed: {label}")
    else:
        tally.bad(
            f"{label}. The planted race was caught WITHOUT -race, so this corpus does not measure the detector.",
            out,
        )


def expect_gate_passes(
    tracker: selftest_swap.AnchorTracker, tally: selftest_swap.Tally, label: str, tree: Path
) -> None:
    """The negative control: the racing test has to be clean under -race
    on the unmutated tree, or R2's red says nothing about the mutation. It
    returns early on a stale anchor and under --check-anchors for the same
    reason its siblings do, since neither of those is a run whose result
    means anything."""
    if tracker.stale_verdict(label):
        return
    if tracker.anchors_only(label):
        return
    code, out = race_gate(tree)
    if code == 0:
        tally.ok(f"clean: {label}")
    else:
        tally.bad(f"{label}. -race FAILED against an unmutated tree, so its failures mean nothing.", out)


def plant_revision_cache_race(tracker: selftest_swap.AnchorTracker, tree: Path) -> None:
    """The mutation: memoise the configuration revision in a plain field
    on BackupService, and have every caller of ConfigRevision write it.
    Two anchors, both in service.go, because the field and the write to
    it are in different places.

    It is a real mistake rather than a contrived one. Caching a computed
    value in a struct field is the most ordinary optimisation there is,
    and this is the exact defect service.go's own doc records this code
    having had once: "inner and revision were two plain fields written
    under configMu but read by every one of those call sites with no lock
    at all". The mutant compiles, vets, and returns the same string;
    nothing about it is observable except through the memory model, which
    is what R3 rests on. computeConfigRevision returns a fixed
    16-character hex digest, so even a torn read of that string header
    cannot produce an out-of-bounds slice, which is why the
    un-instrumented run is not merely usually green but reliably so.

    # Why it is here rather than in adoptConfig, which is where it started

    The first version of this control planted the same class of race in
    adoptConfig: publish by mutating the configState every reader already
    holds instead of swapping in a new one. That is the more evocative
    shape, and it worked on this branch, 10 runs out of 10.

    It stopped working the moment this branch was composed with the other
    lanes of its wave, and the reason is worth writing down, because it is
    the same failure this whole file exists to catch. #411's create-path
    repoint check added two journal queries to CreateBackupSet BEFORE it
    reaches adoptConfig. That pushed the planted write past the point
    where the 150 reader goroutines in the racing test had finished, and
    ThreadSanitizer keeps only a few shadow entries per word, so the
    readers' accesses had aged out by the time the write landed. The race
    was still there in the code; the detector simply had nothing left to
    compare it against. Measured: 0 catches in 10 runs composed, 10 in 10
    with that one new pre-write check short-circuited.

    So the old plant's visibility depended on a timing overlap it did not
    control, and any lane adding work ahead of adoptConfig could silently
    make this control prove nothing. The cell did fail loudly rather than
    go green, which is the one thing that went right, but a control that
    has to be re-earned every time somebody edits CreateBackupSet is not a
    control.

    ConfigRevision has no such dependence. The racing test calls it from
    fifty goroutines directly and fifty more through SubmitRunCycle, all
    concurrent with each other by construction rather than by scheduling,
    so a write in there races roughly a hundred ways at once no matter
    what any other code path does first. Verified 10 of 10 on this branch
    AND 10 of 10 on the composed tree that broke the old one, with the
    un-instrumented run green 10 of 10 in both.
    """
    # The field the mutant caches into. Nothing reads it except the
    # method below; it exists so the plant has somewhere unsynchronised
    # to write.
    tracker.swap(
        tree / "core/service/service.go",
        "\tstate atomic.Pointer[configState]\n\n\tjournal *state.Journal",
        "\tstate atomic.Pointer[configState]\n"
        "\n"
        "\t// PLANTED DATA RACE (scripts/race/selftest.sh). The memoised\n"
        "\t// revision ConfigRevision writes below, in a plain field, exactly\n"
        "\t// as inner and revision themselves were before #155 made them one\n"
        "\t// atomic pointer.\n"
        "\tplantedRevisionCache string\n"
        "\n"
        "\tjournal *state.Journal",
    )

    tracker.swap(
        tree / "core/service/service.go",
        "func (b *BackupService) ConfigRevision() string {\n\treturn b.state.Load().revision\n}",
        "func (b *BackupService) ConfigRevision() string {\n"
        "\t// PLANTED DATA RACE (scripts/race/selftest.sh). One read turned\n"
        "\t// into a write, on a field every concurrent caller shares.\n"
        "\tb.plantedRevisionCache = b.state.Load().revision\n"
        "\treturn b.plantedRevisionCache\n}",
    )


def body(root: Path, dry_run: bool) -> int:
    tally = selftest_swap.Tally()

    with tempfile.TemporaryDirectory(prefix="rclone-manager-race-selftest.") as tmp_name:
        tmp = Path(tmp_name)
        tracker = selftest_swap.AnchorTracker(dry_run=dry_run, root=root, tmp=tmp)

        def mutant(name: str) -> Path:
            return selftest_swap.mutant(root, tmp, name, dry_run=dry_run)

        print("==> R1 negative control: the racing test is clean under -race on the real tree")
        expect_gate_passes(tracker, tally, "core/service under -race, unmutated", root)

        print()
        print("==> R2 the planted race is caught, and named")
        d = mutant("revision-cached-in-a-plain-field")
        plant_revision_cache_race(tracker, d)
        # The write side of the planted race, named in the report's own
        # stack. A bare non-zero exit is not enough: a mutant that failed
        # to compile gives one too.
        expect_race_caught(
            tracker,
            tally,
            "a revision memoised into a plain field every reader writes",
            d,
            "core/service.(*BackupService).ConfigRevision",
        )

        print()
        print("==> R3 the same mutant is invisible without the detector")
        d = mutant("revision-cached-in-a-plain-field-no-race")
        plant_revision_cache_race(tracker, d)
        expect_race_missed(tracker, tally, "the same tree, the same test, -race off", d)

    print()
    tracker.stale_summary()
    if tally.failed == 0 and tracker.stale_count == 0:
        if dry_run:
            print(f"==> race selftest anchors: ok ({tracker.anchors_checked} checked)")
        else:
            print(f"==> race selftest: ok ({tally.passed} controls)")
        return harness.EXIT_OK
    print(
        f"==> race selftest: {tally.failed} failed, {tracker.stale_count} stale, {tally.passed} passed",
        file=sys.stderr,
    )
    return harness.EXIT_FAILED


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    dry_run = selftest_swap.parse_selftest_args(argv, "selftest.py")
    root = harness.repo_root(Path(__file__))
    return harness.finish(lambda: body(root, dry_run))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
