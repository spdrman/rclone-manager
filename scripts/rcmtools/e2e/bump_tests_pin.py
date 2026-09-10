#!/usr/bin/env python3
"""Move scripts/e2e/tests-repo.pin to a newer rclone-manager-tests commit.

  python3 scripts/rcmtools/e2e/bump_tests_pin.py              # the pinned branch's tip
  python3 scripts/rcmtools/e2e/bump_tests_pin.py main         # a named ref
  python3 scripts/rcmtools/e2e/bump_tests_pin.py <full sha>   # an exact commit

The bump is one line in one file, and it does not carry its own proof:
the commit that lands it runs through .husky/pre-commit like any other,
which runs the gate, which runs the newly pinned suites. So a pin that
points at a red or unreachable tests commit cannot be committed without
--no-verify. That is the whole safety story, and it is why this script
deliberately does not "verify" anything itself: a check that runs here and
again in the gate would just be a check that can disagree with the gate.

Ported from scripts/e2e/bump-tests-pin.sh under #672 (EPIC I / #662),
which is why the paragraph above is still exactly one paragraph: the
temptation a port invites is to add the verification the original refuses
to carry, and the refusal is the design.

One check IS added, and it is about this script rather than about the
pin: the rewrite asserts that it replaced exactly one line. The `sed`
this replaces would have exited 0 having matched nothing, printed the old
and new shas, and left the file untouched.
"""

from __future__ import annotations

import os
import re
import sys
from pathlib import Path

# scripts/, so `rcmtools` is importable from a run started anywhere.
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import harness
from rcmtools.e2e import tests_pin

PROGRAM = "bump-tests-pin"


def resolve(url: str, target: str) -> str:
    """The sha to pin: the argument itself, or what the remote says a ref is."""
    if tests_pin.SHA.match(target):
        return target

    ref = target or "HEAD"
    # Plain print rather than harness.step for the four lines this script
    # prints: they are its output rather than an announcement of work, and
    # an operator reads the two shas as a before/after pair.
    print(f"==> resolving {ref} in {url}", flush=True)
    # `git ls-remote ... | awk 'NR==1 {print $1}'` in the bash. The pipe is
    # gone, which is the point: under `pipefail` the pipeline's status was
    # the worse of the two, and without it awk's success would have hidden
    # a git that could not reach the remote at all. Here git's status is
    # unguarded, so it travels, and the first field of the first line is
    # read in Python.
    listed = harness.sh_out(["git", "ls-remote", url, ref])
    lines = [line for line in listed.splitlines() if line.strip()]
    sha = lines[0].split()[0] if lines else ""
    if not sha:
        harness.die(
            f"{url} has no ref matching '{ref}'.",
            "Pass a full 40-character sha to pin one that is not a ref tip.",
        )
    return sha


def rewrite(pin_file: Path, sha: str) -> None:
    """In place, preserving the header.

    The header is the only place the pin's reasoning is written down, and a
    rewrite that dropped it would leave the next reader with a bare sha and
    no argument. So one line changes and every other byte in the file is
    the byte it was.
    """
    text = pin_file.read_text(encoding="utf-8")
    replaced, count = re.subn(
        r"(?m)^TESTS_REPO_SHA=.*$", "TESTS_REPO_SHA=" + sha, text
    )
    if count != 1:
        harness.die(
            f"the rewrite matched {count} TESTS_REPO_SHA lines in {pin_file} rather than one.",
            "Nothing was written. A bump that matched nothing would have printed the new sha and",
            "left the pin exactly where it was, which is the one failure this script can have.",
        )
    tmp = Path(str(pin_file) + ".bump")
    tmp.write_text(replaced, encoding="utf-8")
    os.replace(str(tmp), str(pin_file))


def body(root: Path, target: str) -> int:
    pin_file = root / tests_pin.PIN_RELATIVE
    pin = tests_pin.read_pin(pin_file)
    url = pin.get("TESTS_REPO_URL", "")
    current = pin.get("TESTS_REPO_SHA", "")
    if not url:
        harness.die(f"{tests_pin.PIN_RELATIVE} does not carry TESTS_REPO_URL.")

    sha = resolve(url, target)

    if sha == current:
        print(f"==> already pinned to {sha}, nothing to do", flush=True)
        return harness.EXIT_OK

    rewrite(pin_file, sha)

    print(f"==> {current}", flush=True)
    print(f"==> {sha}", flush=True)
    print("", flush=True)
    print(
        "Commit it. The pre-commit gate will run the newly pinned suites against this",
        flush=True,
    )
    print("working tree, so a bad bump fails there rather than after it has landed.", flush=True)
    return harness.EXIT_OK


def main() -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    root = harness.repo_root(Path(__file__))
    os.chdir(root)
    target = sys.argv[1] if len(sys.argv) > 1 else ""
    return harness.finish(lambda: body(root, target))


if __name__ == "__main__":
    sys.exit(main())


# PORTED-CHECK HAZARD NOTE
#
# scripts/e2e/bump-tests-pin.sh was 55 lines and made three assertions.
# Every `set -e`, `pipefail`, subshell and `$(...)` in it, one row each,
# under harness.py's rule.
#
# a target that is not a sha is resolved against the real remote
#   hazard in bash:   `sha="$(git ls-remote "$URL" "$ref" | awk 'NR==1
#                     {print $1}')"`. Two hazards in one line. `pipefail`
#                     was carrying the first: without it, awk's success
#                     would have masked a git that could not reach the
#                     remote, and the empty-sha branch below would have
#                     reported "no ref matching" for a network failure.
#                     The second is that a command substitution in an
#                     ASSIGNMENT is the whole command, so `set -e` did end
#                     the run on a failing git -- silently, with git's own
#                     stderr as the only clue.
#   hazard in python: STILL EXISTS for the first and GONE for the second.
#                     The pipe is gone, so nothing can mask git's status;
#                     harness.sh_out's default check=True raises
#                     CommandFailed, and finish() reports it WITH the
#                     command, the status and git's stderr, which the bash
#                     never did.
#   held by:          resolve()'s sh_out call (checked), and the
#                     `if not sha` refusal, which now can only mean what
#                     it says: the remote answered and had no such ref.
#
# a ref the remote does not have refuses rather than pinning nothing
#   hazard in bash:   `[ -z "$sha" ]`, then two lines on stderr and
#                     exit 1.
#   hazard in python: STILL EXISTS: same emptiness test, same two
#                     sentences, through harness.die so the prefix is the
#                     repository's one refusal shape. The prose is the
#                     bash's; the "bump-tests-pin:" prefix is now
#                     "==> bump-tests-pin: FAILED.".
#   held by:          resolve()'s `if not sha` branch.
#
# a bump that is not a bump says so and writes nothing
#   hazard in bash:   `[ "$sha" = "$TESTS_REPO_SHA" ]` and exit 0. Reading
#                     TESTS_REPO_SHA required SOURCING the pin, which ran
#                     it as shell.
#   hazard in python: STILL EXISTS for the comparison; the sourcing is
#                     GONE, replaced by tests_pin.read_pin, which matches
#                     KEY=VALUE and executes nothing.
#   held by:          body()'s equality branch and tests_pin.read_pin.
#
# the rewrite actually rewrote something
#   hazard in bash:   ABSENT, and this is the one check the port adds
#                     rather than carries. `sed "s|^TESTS_REPO_SHA=.*|...|"
#                     "$pin_file" >"$tmp"` exits 0 whether it matched or
#                     not, so a pin file whose assignment had been
#                     reworded (indented, quoted, renamed) would have been
#                     copied through unchanged while this script printed
#                     the old and the new sha and told the operator to
#                     commit it. The gate would then have run the OLD pin
#                     and passed, and the bump would have been a no-op
#                     nobody could see.
#   hazard in python: STILL EXISTS -- re.subn matches nothing just as
#                     readily -- but it is now caught: the count is
#                     checked, nothing is written unless it is exactly 1,
#                     and the refusal says so.
#   held by:          rewrite()'s `if count != 1` branch, which runs
#                     BEFORE the temporary file is created.
#
# the header survives the rewrite
#   hazard in bash:   a rewrite that regenerated the file would have
#                     dropped two hundred lines of reasoning and left a
#                     bare sha.
#   hazard in python: STILL EXISTS as the same requirement, met the same
#                     way: one regex substitution over the file's text and
#                     an atomic replace of the whole file, so every other
#                     byte is the byte it was.
#   held by:          rewrite()'s single re.subn plus os.replace, and the
#                     byte-comparison in this change's own report: the pin
#                     after a bump-and-bump-back is identical to the pin
#                     before it.
#
# the bump carries no proof of its own
#   hazard in bash:   none -- this is the ABSENCE of a check, and it is
#                     deliberate: a verification here is a second gate
#                     that can disagree with the real one.
#   hazard in python: the hazard a port introduces is adding it. Not
#                     added. The paragraph explaining why is in the module
#                     docstring, verbatim, so the next reader meets the
#                     argument before the temptation.
#   held by:          the docstring, and this row.
