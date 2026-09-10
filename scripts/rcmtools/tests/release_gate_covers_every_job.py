#!/usr/bin/env python3
"""The gate on `release` is one check with a hand-written list behind it,
and this is what keeps the list honest (issue #575).

`release-gate` in .github/workflows/ci.yml is the job branch protection
points at. It succeeds only when every other job in that file succeeded,
and it knows which jobs those are from its own `needs:` list, because
GitHub Actions has no way to say "every job in this workflow". So the
list is an enumeration maintained by hand, sitting between a pull
request and a signed image on a public registry. An enumeration like
that goes stale the first time somebody adds a job and does not think
about this one, and the failure is silent and in the dangerous
direction: the new job runs, goes red, and the required check is green
anyway because it was never asked about it.

Four things are pinned here, and the first is the one that matters:

  1. every job in ci.yml is in release-gate's needs list;
  2. nothing in that list names a job that no longer exists, which is
     the same drift seen from the other end and would fail the workflow
     at parse time rather than silently, but fails here first and says
     why;
  3. release-gate carries `if: always()`, because without it a failed
     dependency SKIPS this job, GitHub records the skip as a check run
     whose conclusion is `skipped`, and a required status check counts
     `skipped` as satisfied. Removing that line fails OPEN: the gate
     goes quiet exactly when a job went red, and the merge is allowed
     rather than stalled;
  4. ci.yml still triggers on a pull request into `release`, which is
     the whole acceptance criterion of #575 and one line away from
     being undone by accident.

Ported from `scripts/tests/release-gate-covers-every-job.test.sh` under
EPIC I (#672 / #697). That file now execs this one. The parsing logic
below was ALREADY a `python3 -` heredoc in the bash file, lifted here
essentially verbatim: this is the one file in this domain where the port
is mechanical rather than a translation.

# PORTED-CHECK HAZARD NOTE

the positive controls (job count, GATE present, two-machine-e2e present,
non-empty needs list) run before the real assertions
  hazard in bash:   a `python3 -` heredoc has no `set -e` of its own that
                    would matter here: the risk was always the REGEX, not
                    shell propagation. A parser that quietly stopped
                    matching (a workflow reindented, a key renamed) would
                    walk away with empty sets on both sides of every
                    comparison below and report a clean sweep of nothing.
  hazard in python: STILL EXISTS, unchanged: the parser and its positive
                    controls are the same regexes over the same file,
                    just no longer inside a heredoc. Nothing about running
                    as a module instead of `python3 -` makes a silent
                    parse failure less silent.
  held by:          the four `check()` calls before the set comparison,
                    asserting job count, GATE membership, two-machine-e2e
                    membership and a non-empty needs list -- unchanged
                    from bash, and proven red-able below.

the workflow path is a real file on disk, not a string this suite invents
  hazard in bash:   `python3 - "$WORKFLOW" <<'PY'` reads a real path
                    argument; a typo there would have `open()` raise
                    FileNotFoundError under no `-e` inside the heredoc,
                    which bash's outer `set -uo pipefail` (no `-e`) does
                    not catch either -- the heredoc's own traceback and
                    non-zero exit were always what caught it.
  hazard in python: GONE as a shell-propagation question, STILL EXISTS as
                    the same "did the path resolve" question: `main()`
                    lets `FileNotFoundError` propagate uncaught, exiting
                    non-zero with a traceback, exactly as the heredoc did.
  held by:          no explicit try/except -- the traceback IS the
                    failure mode this file inherited, on purpose.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

GATE = "release-gate"


def find_jobs(lines: list[str]) -> tuple[list[str], list[str], bool]:
    jobs: list[str] = []
    needs: list[str] = []
    gate_has_always = False

    in_jobs = False
    current = None
    in_needs = False

    for line in lines:
        if re.match(r"^jobs:\s*$", line):
            in_jobs = True
            continue
        if in_jobs and line and not line.startswith(" ") and not line.startswith("#"):
            # Back at column zero: the jobs mapping is over.
            in_jobs = False
            continue
        if not in_jobs:
            continue

        m = re.match(r"^  ([A-Za-z0-9_.-]+):\s*$", line)
        if m:
            current = m.group(1)
            jobs.append(current)
            in_needs = False
            continue

        if current != GATE:
            continue

        if re.match(r"^    needs:\s*$", line):
            in_needs = True
            continue
        if re.match(r"^    if:\s*always\(\)\s*$", line):
            gate_has_always = True
            in_needs = False
            continue
        if in_needs:
            m = re.match(r"^      - ([A-Za-z0-9_.-]+)\s*$", line)
            if m:
                needs.append(m.group(1))
                continue
            if line.strip() and not line.startswith("      #"):
                in_needs = False

    return jobs, needs, gate_has_always


def main(argv: list[str]) -> int:
    workflow = Path(argv[0]) if argv else Path(__file__).resolve().parents[2].parent / ".github/workflows/ci.yml"
    print("==> release gate: the one required check still covers every job", flush=True)

    lines = workflow.read_text(encoding="utf-8").splitlines()
    jobs, needs, gate_has_always = find_jobs(lines)

    checks = 0
    failures = 0

    def check(ok: bool, ok_text: str, bad_text: str) -> None:
        nonlocal checks, failures
        checks += 1
        if ok:
            print("    ok   " + ok_text, flush=True)
        else:
            failures += 1
            print("    FAIL " + bad_text, file=sys.stderr, flush=True)

    # The positive controls come first. Everything below is a set comparison,
    # and a set comparison against an empty set is the cheapest way in the
    # world to report success while looking at nothing.
    check(
        len(jobs) >= 10,
        f"the parser found {len(jobs)} jobs in ci.yml, so it is reading the file",
        f"the parser found only {len(jobs)} jobs in ci.yml ({jobs}), which is fewer than this workflow has "
        "ever had: it has stopped matching, and every comparison below would pass against nothing",
    )
    check(
        GATE in jobs,
        f"{GATE} is one of them",
        f"{GATE} is not among the jobs the parser found ({jobs}): either the job was renamed and this test "
        "was not, or the parser is broken",
    )
    check(
        "two-machine-e2e" in jobs,
        "and so is two-machine-e2e, the proof #575 exists to put in front of a release",
        "two-machine-e2e is not in ci.yml: the two-machine proof is the one test anywhere that a fresh "
        "install can pull a real backup off a real machine, and a release gate without it is back to "
        "gating on nobody",
    )
    check(
        len(needs) > 0,
        f"{GATE} declares {len(needs)} dependencies",
        f"{GATE} has no needs list the parser could find, so it depends on nothing and would go green on "
        "its own while every other job burned",
    )

    covered = set(needs)
    expected = {j for j in jobs if j != GATE}

    missing = sorted(expected - covered)
    check(
        not missing,
        "every job in this workflow is in its needs list",
        "these jobs run in ci.yml and " + GATE + " does not wait for them, so each one could go red with "
        "the required check still green: " + ", ".join(missing),
    )

    phantom = sorted(covered - expected)
    check(
        not phantom,
        "and it names no job that does not exist",
        GATE + " depends on jobs that are not in this workflow, so the list has drifted from the file it "
        "lives in: " + ", ".join(phantom),
    )

    check(
        gate_has_always,
        f"{GATE} carries `if: always()`, so a failed job leaves it red rather than skipped, and a skipped "
        "required check would have counted as a pass",
        f"{GATE} has no `if: always()`. Without it a failed dependency SKIPS this job, GitHub reports the "
        "skip as a check run whose conclusion is `skipped`, and a required status check counts `skipped` "
        "as satisfied. That fails OPEN, not shut: the line is what keeps a red suite red, and deleting it "
        "lets exactly the merges this gate exists to stop go through, quietly, because the job that would "
        "have gone red never ran",
    )

    body = "\n".join(lines)
    trigger = re.search(r"^on:\s*$\n(?:^[ #].*$\n?)*", body, re.MULTILINE)
    trigger_text = trigger.group(0) if trigger else ""
    check(
        "pull_request:" in trigger_text and re.search(r"^\s+- release\s*$", trigger_text, re.MULTILINE) is not None,
        "ci.yml still runs on a pull request into release",
        "ci.yml no longer triggers on a pull request into `release`. Merging into that branch publishes a "
        "signed image to a public registry, and without this trigger nothing checks the change first, "
        "which is the whole of #575:\n" + (trigger_text or "(no `on:` block found at all)"),
    )

    print("", flush=True)
    if failures:
        print(f"==> release gate: FAILED ({failures} of {checks} checks)", file=sys.stderr, flush=True)
        return 1
    print(f"==> release gate: ok ({checks} checks)", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
