#!/usr/bin/env python3
"""What CI runs instead of calling the two-machine proof directly (#575).

The proof has three outcomes and GitHub Actions has two. That gap is the
whole reason this file exists.

scripts/bdtools/e2e/two_machine_backup.py says 0 for "the proof passed", 3
for "this machine cannot perform the proof" and anything else for "the proof
failed". scripts/ci-local.sh reads all three: a 3 goes in its ledger and the
run ends INCOMPLETE. A workflow step has no ledger. It exits zero or it does
not, and a step that ran the proof without reading the number would turn a
runner that cannot start docker-in-docker into a green required check on the
branch that publishes. That is worse than having no check at all, because a
green check is read as evidence.

So: 3 is red here, and it is red under its own word. INCOMPLETE is not
FAILED, it does not mean the product is broken, and it must not be triaged as
a flaky test. It means the runner could not perform the proof and nothing was
learned, which on a publishing branch is a reason to stop.

The exit status keeps the vocabulary rather than flattening it, so anything
wrapping this in turn can still tell the two apart:

  0            the proof passed
  3            the proof could not be performed on this machine
  anything else the proof failed, with the status it failed with

Every argument is passed straight through, so --case and --keep-on-failure
work here exactly as they do by hand.

Covered by scripts/tests/two-machine-ci-verdict.test.sh, which
scripts/ci-local.sh runs.

# PORTED-CHECK HAZARD NOTE

EPIC I / I1.6 (#672) ported this from scripts/e2e/two-machine-ci.sh, which
now execs it. The three outcomes two-machine-ci-verdict.test.sh drives:

  a proof exiting 0 -> 0, under the word PASSED
    hazard in bash:   the wrapper stops being able to go green at all, which
                      makes the required check unmergeable rather than honest.
    hazard in python: STILL EXISTS. One branch, one exit, same as before.
    held by:          case 1 of the verdict test.

  a proof exiting 3 -> 3, under the word INCOMPLETE and never FAILED
    hazard in bash:   a 0 here is a green required check standing on a run
                      that never happened.
    hazard in python: STILL EXISTS, and is not weakened by the port: the
                      child's status is read explicitly from
                      CompletedProcess.returncode, which is the same act the
                      shell's `|| status=$?` performed. There is no `set -e`
                      subtlety on this path in either language.
    held by:          cases 2 of the verdict test, which check the number,
                      the word INCOMPLETE, and the ABSENCE of the word FAILED.

  any other status -> itself, under the word FAILED
    hazard in bash:   the wrapper flattens every non-zero to one number and
                      stops being able to report a real defect, which would
                      also make case 2 pass for the wrong reason.
    hazard in python: STILL EXISTS. `return status` on the default branch.
    held by:          case 3 of the verdict test.
"""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path
from typing import Sequence

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import harness


def say(*lines: str) -> None:
    """Put lines on stdout and, on a runner, in the job summary too.

    The summary is what somebody reading a red check sees first, and a verdict
    that lives only in the middle of a scrolled log is a verdict most people
    will not find.
    """
    for line in lines:
        print(line, flush=True)
    summary = os.environ.get("GITHUB_STEP_SUMMARY", "")
    if not summary:
        return
    try:
        with open(summary, "a", encoding="utf-8") as handle:
            for line in lines:
                handle.write(line + "\n")
    except OSError:
        # Best effort by design: a summary file that cannot be written must
        # not change the verdict this script exists to report.
        pass


def annotate(level: str, message: str) -> None:
    """Raise the verdict to the top of the run's own UI.

    ::error:: and ::notice:: are Actions' own vocabulary; off a runner they
    are noise, so they are only emitted on one.
    """
    if not os.environ.get("GITHUB_ACTIONS"):
        return
    print(f"::{level} title=two-machine proof::{message}", flush=True)


def proof_argv(proof: str, args: Sequence[str]) -> list[str]:
    """How to invoke the proof.

    TWO_MACHINE_PROOF exists only so the self-test can drive all three
    outcomes without standing up two containers, and it is named rather than
    positional so no ordinary invocation can reach it by accident. Its
    stand-ins are shell scripts, and the real proof is Python, so the
    interpreter is chosen from the name rather than assumed: running a `.sh`
    stand-in under python3 would fail with a SyntaxError and be reported as
    the proof failing, which is a verdict about this wrapper's plumbing
    wearing the product's name.
    """
    if proof.endswith(".py"):
        return [sys.executable, proof, *args]
    return ["bash", proof, *args]


def main(argv: Sequence[str]) -> int:
    harness.set_program("two-machine-ci")
    repo_root = harness.repo_root(Path(__file__).resolve())
    proof = os.environ.get(
        "TWO_MACHINE_PROOF",
        str(repo_root / "scripts" / "bdtools" / "e2e" / "two_machine_backup.py"),
    )

    # The proof itself decides the verdict. Its output is this process's
    # output: no capture, because a run that takes minutes must show its
    # progress, and because the log above the verdict is how anybody triages
    # the verdict.
    status = subprocess.run(proof_argv(proof, argv)).returncode

    print("", flush=True)
    if status == 0:
        say(
            "## two-machine proof: PASSED",
            "",
            "A fresh install on a throwaway machine pulled a real backup off another",
            "throwaway machine over a temporary network, and the artifacts match the",
            "source by digest.",
        )
        annotate("notice", "PASSED: a fresh install pulled a real backup and the bytes match.")
        return 0

    if status == harness.EXIT_INCOMPLETE:
        say(
            "## two-machine proof: INCOMPLETE",
            "",
            "This runner could not perform the proof, so nothing about this change was",
            "learned. The run above says which capability was missing: usually a Docker",
            "daemon that is not there, one that refuses the privileged container",
            "docker-in-docker needs, or a kernel that will not carry the connection-cap",
            "case's iptables rule.",
            "",
            "This is red on purpose and it is not a failing test. The branch this gates",
            "publishes a signed image on merge, and a proof nobody performed is not",
            "evidence that anything works. Fix the runner, or re-run once it can.",
        )
        annotate(
            "error",
            "INCOMPLETE: this runner could not perform the proof, so nothing was learned. "
            "Not a product failure, and not a pass.",
        )
        return harness.EXIT_INCOMPLETE

    say(
        "## two-machine proof: FAILED",
        "",
        "The proof ran and did not hold. The run above names the case and the step",
        "it died on. This is the one test in the tree that covers the claim a user",
        "actually makes, so a red here is a change that should not be published.",
        "",
        f"Exit status: {status}",
    )
    annotate("error", f"FAILED: the two-machine proof ran and did not hold. Exit status {status}.")
    return status


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
