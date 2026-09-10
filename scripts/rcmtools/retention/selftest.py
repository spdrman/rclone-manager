#!/usr/bin/env python3
"""Positive controls for FR-20's retention apply (issue #602).

`core/service/retentionapplyevidence_test.go` and
`core/internal/retention/lastknowngoodpresence_test.go` are the evidence
that an apply removes exactly the previewed DELETE set and nothing else.
An assertion nobody has watched fail is indistinguishable from one that
cannot, and a delete is the assertion where that costs the most: the
cheapest way to pass "every previewed DELETE is gone, every KEEP
survives" is to delete nothing, or to plan nothing.

So every claim that evidence makes is mutation-tested here against the
real tree: a copy of the working tree gets one deliberate violation
planted in a real product file, the suite runs, and it must fail AND
name the check whose promise the violation broke. Naming the check, not
merely failing, is what stops a mutation that broke the build for an
unrelated reason from reading as a pass. `scripts/rcmtools/conformance/`
and `scripts/rcmtools/compat/` (once ported) are the models and this is
deliberately their sibling; all three share `selftest_swap`'s anchor
machinery rather than each growing a private copy of it.

The last control is the one to read first. It plants its violation in
the TEST, not in the product: `retentionEvidenceCompare` is made to
return no complaints at all, and the suite still has to go red, because
the in-test positive control is what proves the comparison would notice
a file the plan never named going missing. Without that, this whole file
would be certifying a guard whose own guard nobody had checked.

Every mutation below is anchored to a verbatim copy of product source,
tabs and all, which means a refactor over there can leave an anchor here
naming code that is no longer in the tree. That is the third verdict,
STALE ANCHOR: the control is skipped, because a tree with nothing
planted in it would pass and reading that as a pass is the exact failure
this file exists to rule out, and the run carries on so one run names
every stale control instead of dying on the first (#458).

`python3 scripts/rcmtools/retention/selftest.py --check-anchors` is that
check on its own: every anchor against the real tree, building nothing,
in about a second. `scripts/rcmtools/selftest/check_anchors.py` runs it
alongside the others.

This is not fast. Each mutant rebuilds `core/` and runs a narrowed slice
of three packages, so budget a few minutes. It needs no Docker and no
container.

Ported from `scripts/retention/selftest.sh` under EPIC I (#672 / #602).
`scripts/retention/selftest.sh` stays as a real, runnable shim:
`scripts/ci-local.sh` runs `bash scripts/retention/selftest.sh` by that
literal path, and `scripts/tests/ci-local-gate.test.sh` fabricates a
stand-in there.

# PORTED-CHECK HAZARD NOTE

an `expect_gate_fails` control that behaves as required (the gate goes
red) must be RECORDED as a pass, not raise
  hazard in bash:   `if (cd "$dir" && retention_gate "$pattern") >"$tmp/out" 2>&1; then ... else ... fi`
                     -- the `if` itself already stops `set -euo pipefail`
                     from taking a non-zero `retention_gate` status down
                     as the whole script's own exit. Dropping that `if`
                     and calling `retention_gate` bare is the mistake
                     this hazard is about: the very outcome every
                     `expect_gate_fails` control is FOR (the gate fails)
                     would abort the script instead of being asserted,
                     and every control after it would never run.
  hazard in python: STILL EXISTS, in the equivalent shape. `harness.sh()`
                     defaults to `check=True` and raises `CommandFailed`
                     on any non-zero status. `retention_gate()` below
                     calls it with `check=False` and reads
                     `proc.returncode` itself for exactly the reason the
                     bash `if` exists: an expected failure must become a
                     value this file inspects, not an exception that
                     unwinds through `body()` to `harness.finish()` and
                     reports the whole run as a crash after the first
                     control, the same shape #458's postmortem describes
                     for a stale anchor once taking the rest of a gate
                     down with it.
  held by:          `retention_gate()`'s `check=False`, and
                     `expect_gate_fails`/`expect_gate_passes` reading
                     `code` themselves rather than trusting a subprocess
                     helper's default to raise for them.

an `expect_gate_fails` control must fail for the NAMED reason, not any reason
  hazard in bash:   `grep -qF "$needle" "$tmp/out"` after the `if` already
                     established non-zero -- a suite that failed to
                     BUILD (a typo in the plant) looks identical to a
                     suite that ran and asserted correctly unless the
                     failure text is checked for the promise the
                     violation was supposed to break.
  hazard in python: STILL EXISTS, unchanged in shape: `needle not in out`
                     is checked only after `code != 0`, and a build
                     failure with unrelated text in it is reported as
                     "failed, but never named the promise that was
                     broken" rather than folded into the same bucket as
                     "reached the wrong verdict".
  held by:          the explicit `needle not in out` branch in
                     `expect_gate_fails`, distinct from the `code == 0`
                     branch above it.

a STALE ANCHOR must stop its own control's gate from running, not merely warn
  hazard in bash:   `selftest_stale_verdict "$label"` returns non-zero
                     (0 in bash's own truthiness) only when nothing was
                     stale, so `if selftest_stale_verdict ...; then return
                     0; fi` at the top of both `expect_gate_*` skips the
                     gate entirely when a plant refused. Forgetting that
                     `return` and running the gate anyway is the tree
                     described throughout this file: nothing planted, the
                     suite is clean, and that clean run gets recorded as
                     a pass for a control that never fired.
  hazard in python: STILL EXISTS, unchanged in shape:
                     `tracker.stale_verdict(label)` returning `True`
                     must be an early `return` in both
                     `expect_gate_fails` and `expect_gate_passes`, kept
                     here exactly where the bash put it, before
                     `retention_gate()` is ever called for that control.
  held by:          the `if tracker.stale_verdict(label): return` guard
                     opening both functions, ahead of everything else in
                     them -- and `AnchorTracker.stale_verdict` itself,
                     which this file consumes rather than reimplements
                     (see `selftest_swap.py`'s own hazard note for the
                     anchor-matching machinery underneath it).

the copy a mutation is planted into is a real, complete working tree
  hazard in bash:   `mutant()`'s `git ls-files -z --cached --others
                     --exclude-standard | tar` -- a stale selftest
                     elsewhere in this repository once "caught" every
                     mutation by invoking a check that did not exist in
                     a `git ls-files`-only copy at all, because
                     `--cached` alone drops files that are new but not
                     yet committed while a suite is mid-write.
  hazard in python: GONE from THIS file, by construction rather than by
                     re-derivation: `mutant()` below is
                     `selftest_swap.mutant`, the one shared
                     implementation every anchored selftest under
                     `scripts/rcmtools/` consumes, so a second, private
                     Python copy of the `--cached --others
                     --exclude-standard` invariant does not exist here to
                     drift from it.
  held by:          `selftest_swap.mutant`'s own implementation and its
                     own hazard note in `selftest_swap.py`; this file
                     has no `mutant()` of its own to get wrong.
"""

from __future__ import annotations

import os
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import harness, selftest_swap

PROGRAM = "retention selftest"

PACKAGES = ("./service/", "./internal/retention/", "./cmd/backup-manager/")

PRUNE_GO = "core/internal/retention/prune.go"
LASTKNOWNGOOD_GO = "core/internal/retention/lastknowngood.go"
RETENTIONAPPLY_GO = "core/cmd/backup-manager/retentionapply.go"
RETENTIONAPPLYEVIDENCE_TEST_GO = "core/service/retentionapplyevidence_test.go"

PASS_PATTERN = (
    "TestRetentionApplyEvidence|TestPruneRefusesEveryDelete|TestPruneStillDeletes|"
    "TestPruneDeletesWhenTheOperator|TestPruneConfirmsA|TestPruneReconfirmsTheLastKnownGood|"
    "TestPruneHoldsTheRestOfThePass|TestRun_RetentionApply"
)

# -- apply-deletes-nothing: the cheapest way to pass every "what
# survived" assertion ever written is to delete nothing.
APPLY_DELETES_NOTHING_OLD = "\t\tif err := os.Remove(safePath); err != nil {"
APPLY_DELETES_NOTHING_NEW = (
    "\t\t// PLANTED VIOLATION (scripts/rcmtools/retention/selftest.py): an apply\n"
    "\t\t// that confirms a plan and then carries none of it out.\n"
    "\t\tif err := func(string) error { return nil }(safePath); err != nil {"
)

# -- apply-reaches-past-its-plan: the other direction, invisible to an
# assertion that only ever looks up the paths the plan already named.
APPLY_REACHES_PAST_ITS_PLAN_OLD = "\t\tif err := os.Remove(safePath); err != nil {"
APPLY_REACHES_PAST_ITS_PLAN_NEW = (
    "\t\t// PLANTED VIOLATION (scripts/rcmtools/retention/selftest.py): an apply\n"
    "\t\t// that clears the directory rather than the artifact, which is\n"
    "\t\t// the FR-20 guarantee that only a journal record can identify a\n"
    "\t\t// file to this package at all.\n"
    "\t\tif err := func(p string) error {\n"
    "\t\t\tdir := filepath.Dir(p)\n"
    "\t\t\tentries, readErr := os.ReadDir(dir)\n"
    "\t\t\tif readErr != nil {\n"
    "\t\t\t\treturn readErr\n"
    "\t\t\t}\n"
    "\t\t\tfor _, e := range entries {\n"
    "\t\t\t\tif !e.IsDir() {\n"
    "\t\t\t\t\t_ = os.Remove(filepath.Join(dir, e.Name()))\n"
    "\t\t\t\t}\n"
    "\t\t\t}\n"
    "\t\t\treturn nil\n"
    "\t\t}(safePath); err != nil {"
)

# -- symlink-followed: stat THROUGH the link rather than at it.
SYMLINK_FOLLOWED_OLD = "\tinfo, err := os.Lstat(expected)"
SYMLINK_FOLLOWED_NEW = (
    "\t// PLANTED VIOLATION (scripts/rcmtools/retention/selftest.py): stat through\n"
    "\t// the link rather than at it, so a symlink at an artifact FINAL path\n"
    "\t// reports as the regular file it points at.\n"
    "\tinfo, err := os.Stat(expected)"
)

# -- last-known-good-never-decided: the newest restore point found and
# then not protected.
LAST_KNOWN_GOOD_NEVER_DECIDED_OLD = "\tresult.Protected = true\n\tresult.Artifact = newest.artifact"
LAST_KNOWN_GOOD_NEVER_DECIDED_NEW = (
    "\t// PLANTED VIOLATION (scripts/rcmtools/retention/selftest.py): FR-19 finds the\n"
    "\t// newest eligible restore point and then protects nothing.\n"
    "\tresult.Protected = false\n"
    "\tresult.Artifact = newest.artifact"
)

# -- last-known-good-copy-not-confirmed: the FR-30 confirmation
# short-circuited.
LAST_KNOWN_GOOD_COPY_NOT_CONFIRMED_OLD = '\tif !lkg.Protected {\n\t\treturn ""\n\t}'
LAST_KNOWN_GOOD_COPY_NOT_CONFIRMED_NEW = (
    "\t// PLANTED VIOLATION (scripts/rcmtools/retention/selftest.py): the FR-30\n"
    "\t// confirmation short-circuited, so a last known good whose file is\n"
    "\t// gone still authorises deleting everything else.\n"
    '\tif true {\n\t\treturn ""\n\t}'
)

# -- apply-time-last-known-good-not-reconfirmed: the apply carries the
# plan's own confirmation into the delete loop instead of re-taking it.
APPLY_TIME_LAST_KNOWN_GOOD_NOT_RECONFIRMED_OLD = (
    '\t\tif why := pruneLastKnownGoodUnconfirmed(bs, recByArtifact, lkg, where); why != "" {\n'
    "\t\t\tpruneHoldEveryDelete(verdicts[i:], why)\n"
    "\t\t\tcontinue\n"
    "\t\t}"
)
APPLY_TIME_LAST_KNOWN_GOOD_NOT_RECONFIRMED_NEW = (
    "\t\t// PLANTED VIOLATION (scripts/rcmtools/retention/selftest.py): the apply\n"
    "\t\t// carries the plan's own confirmation into the delete loop, so\n"
    "\t\t// a last known good that goes away after the plan was drawn up,\n"
    "\t\t// or partway through the pass, authorises every remaining delete.\n"
    "\t\t_ = lkg"
)

# -- acknowledge-not-required: the confirmation defaulted to given.
ACKNOWLEDGE_NOT_REQUIRED_OLD = '\tacknowledge := fs.Bool("acknowledge", false,'
ACKNOWLEDGE_NOT_REQUIRED_NEW = (
    "\t// PLANTED VIOLATION (scripts/rcmtools/retention/selftest.py): the\n"
    "\t// confirmation defaulted to given, so a mistyped command line\n"
    "\t// deletes restore points.\n"
    '\tacknowledge := fs.Bool("acknowledge", true,'
)

# -- comparison-cannot-fail: the guard on the guard, planted in the test
# rather than the product.
COMPARISON_CANNOT_FAIL_OLD = (
    "func retentionEvidenceCompare(before, after map[string]retentionEvidenceEntry, "
    "wantRemoved []string) []string {\n"
    "\tvar complaints []string"
)
COMPARISON_CANNOT_FAIL_NEW = (
    "func retentionEvidenceCompare(before, after map[string]retentionEvidenceEntry, "
    "wantRemoved []string) []string {\n"
    "\t// PLANTED VIOLATION (scripts/rcmtools/retention/selftest.py): the whole\n"
    "\t// comparison made vacuous. Every assertion built on it still passes;\n"
    "\t// only its own positive control can see this.\n"
    "\tif true {\n"
    "\t\treturn nil\n"
    "\t}\n"
    "\tvar complaints []string"
)


def retention_gate(root: Path, pattern: str) -> tuple[int, str]:
    """Run the narrowed evidence suite in `root`, never raising on its own
    non-zero status -- see this file's hazard note above for why
    `check=False` is load-bearing rather than incidental here."""
    env = dict(os.environ)
    env["GOWORK"] = "off"
    proc = harness.sh(
        ["go", "test", "-count=1", "-timeout", "15m", "-run", pattern, *PACKAGES],
        check=False,
        capture=True,
        cwd=root / "core",
        env=env,
    )
    return proc.returncode, (proc.stdout or "") + (proc.stderr or "")


def expect_gate_fails(
    tracker: selftest_swap.AnchorTracker,
    tally: selftest_swap.Tally,
    label: str,
    directory: Path,
    needle: str,
    pattern: str,
) -> None:
    if tracker.stale_verdict(label):
        return
    if tracker.anchors_only(label):
        return
    code, out = retention_gate(directory, pattern)
    if code == 0:
        tally.bad(f"{label}. The evidence PASSED against a planted violation.", out)
        return
    if needle not in out:
        tally.bad(
            f"{label}. The suite failed, but never named the promise that was broken.",
            f"expected its output to mention: {needle}\n{out}",
        )
        return
    matched = next((line.strip()[:400] for line in out.splitlines() if needle in line), "")
    tally.ok(f"(caught): {label}", matched)


def expect_gate_passes(
    tracker: selftest_swap.AnchorTracker,
    tally: selftest_swap.Tally,
    label: str,
    directory: Path,
    pattern: str = ".",
) -> None:
    """The negative control. A mutation that turns a suite red proves
    nothing about the mutation if the suite was red already, and the two
    early returns matter for the reason the docstring above gives: a
    control whose anchor no longer matches planted nothing, and under
    `--check-anchors` nothing is built at all."""
    if tracker.stale_verdict(label):
        return
    if tracker.anchors_only(label):
        return
    code, out = retention_gate(directory, pattern)
    if code == 0:
        tally.ok(f"(clean): {label}")
    else:
        tally.bad(f"{label}. The evidence FAILED against an unmutated tree, so its failures mean nothing.", out)


def body(root: Path, dry_run: bool) -> int:
    tally = selftest_swap.Tally()

    with tempfile.TemporaryDirectory(prefix="rclone-manager-retention-selftest.") as tmp_name:
        tmp = Path(tmp_name)
        tracker = selftest_swap.AnchorTracker(dry_run=dry_run, root=root, tmp=tmp)

        print("==> negative control: the retention apply evidence is clean on the real tree")
        expect_gate_passes(tracker, tally, "the retention apply evidence on an unmutated tree", root, PASS_PATTERN)

        print()
        print("==> the apply carries out the plan")
        d = selftest_swap.mutant(root, tmp, "apply-deletes-nothing", dry_run=dry_run)
        tracker.swap(d / PRUNE_GO, APPLY_DELETES_NOTHING_OLD, APPLY_DELETES_NOTHING_NEW)
        expect_gate_fails(
            tracker,
            tally,
            "an apply that deletes nothing at all",
            d,
            "is still on disk and the plan marked it DELETE",
            "TestRetentionApplyEvidence_RemovesExactlyThePreviewedDeleteSet",
        )

        print()
        print("==> the apply removes ONLY what the plan named")
        d = selftest_swap.mutant(root, tmp, "apply-reaches-past-its-plan", dry_run=dry_run)
        tracker.swap(d / PRUNE_GO, APPLY_REACHES_PAST_ITS_PLAN_OLD, APPLY_REACHES_PAST_ITS_PLAN_NEW)
        expect_gate_fails(
            tracker,
            tally,
            "an apply that removes a file no verdict named",
            d,
            "was removed and no verdict in the plan named it",
            "TestRetentionApplyEvidence_RemovesExactlyThePreviewedDeleteSet",
        )

        print()
        print("==> FR-20: a symlink at a final path is refused, never followed")
        d = selftest_swap.mutant(root, tmp, "symlink-followed", dry_run=dry_run)
        tracker.swap(d / PRUNE_GO, SYMLINK_FOLLOWED_OLD, SYMLINK_FOLLOWED_NEW)
        expect_gate_fails(
            tracker,
            tally,
            "a symlink at an artifact's final path treated as the artifact",
            d,
            "whose final path is a symlink",
            "TestRetentionApplyEvidence_ASymlinkAtAnArtifactsPathIsRefusedNotFollowed|"
            "TestPruneRefusesSymlinkAtFinalPath",
        )

        print()
        print("==> FR-19 and FR-30: the last known good")
        d = selftest_swap.mutant(root, tmp, "last-known-good-never-decided", dry_run=dry_run)
        tracker.swap(d / LASTKNOWNGOOD_GO, LAST_KNOWN_GOOD_NEVER_DECIDED_OLD, LAST_KNOWN_GOOD_NEVER_DECIDED_NEW)
        expect_gate_fails(
            tracker,
            tally,
            "the newest restore point found and then not protected",
            d,
            "this backup set's last known good",
            "TestRetentionApplyEvidence_LastKnownGoodSurvivesATierVerdictThatWouldRemoveIt|"
            "TestPruneDecideKeepsLastKnownGoodArtifact",
        )

        d = selftest_swap.mutant(root, tmp, "last-known-good-copy-not-confirmed", dry_run=dry_run)
        tracker.swap(
            d / PRUNE_GO, LAST_KNOWN_GOOD_COPY_NOT_CONFIRMED_OLD, LAST_KNOWN_GOOD_COPY_NOT_CONFIRMED_NEW
        )
        expect_gate_fails(
            tracker,
            tally,
            "a last known good confirmed from its journal row alone",
            d,
            "FR-30",
            "TestRetentionApplyEvidence_NothingIsRemovedWhenTheLastKnownGoodCopyIsGone|"
            "TestPruneRefusesEveryDeleteWhenTheLastKnownGood",
        )

        d = selftest_swap.mutant(root, tmp, "apply-time-last-known-good-not-reconfirmed", dry_run=dry_run)
        tracker.swap(
            d / PRUNE_GO,
            APPLY_TIME_LAST_KNOWN_GOOD_NOT_RECONFIRMED_OLD,
            APPLY_TIME_LAST_KNOWN_GOOD_NOT_RECONFIRMED_NEW,
        )
        expect_gate_fails(
            tracker,
            tally,
            "an apply that re-confirms nothing the plan already confirmed",
            d,
            "the copy FR-19 protects",
            "TestPruneReconfirmsTheLastKnownGood|TestPruneHoldsTheRestOfThePass",
        )

        print()
        print("==> the CLI verb's confirmation")
        d = selftest_swap.mutant(root, tmp, "acknowledge-not-required", dry_run=dry_run)
        tracker.swap(d / RETENTIONAPPLY_GO, ACKNOWLEDGE_NOT_REQUIRED_OLD, ACKNOWLEDGE_NOT_REQUIRED_NEW)
        expect_gate_fails(
            tracker,
            tally,
            "an apply verb whose confirmation is assumed rather than asked for",
            d,
            "want 2",
            "TestRun_RetentionApplyRefusesWithoutTheAcknowledgement",
        )

        print()
        print("==> the guard on the guard")
        d = selftest_swap.mutant(root, tmp, "comparison-cannot-fail", dry_run=dry_run)
        tracker.swap(d / RETENTIONAPPLYEVIDENCE_TEST_GO, COMPARISON_CANNOT_FAIL_OLD, COMPARISON_CANNOT_FAIL_NEW)
        expect_gate_fails(
            tracker,
            tally,
            "an exact-set comparison that cannot report anything wrong",
            d,
            "which makes every other assertion in this file worthless",
            "TestRetentionApplyEvidence_TheComparisonNoticesWhatItIsAskedTo",
        )

        tracker.stale_summary()
        stale_count = tracker.stale_count
        anchors_checked = tracker.anchors_checked

    total = tally.passed + tally.failed + stale_count
    if tally.failed or stale_count:
        print(file=sys.stderr)
        print(
            f"FAIL: {tally.failed + stale_count} of {total} retention-apply controls did not behave as required "
            f"({tally.failed} reached the wrong verdict, {stale_count} could not plant their violation at all).",
            file=sys.stderr,
        )
        return harness.EXIT_FAILED

    print()
    if dry_run:
        print(f"OK: all {anchors_checked} retention-apply mutation anchors still name code that is in this tree.")
    else:
        print(
            f"OK: all {tally.passed} retention-apply controls behaved as required (every claim was shown to go "
            "red against a real planted violation, and shown not to on the real tree)."
        )
    return harness.EXIT_OK


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    dry_run = selftest_swap.parse_selftest_args(argv, "selftest.py")
    root = harness.repo_root(Path(__file__))
    return harness.finish(lambda: body(root, dry_run))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
