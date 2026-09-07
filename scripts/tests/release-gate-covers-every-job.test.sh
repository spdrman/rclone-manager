#!/usr/bin/env bash
# The gate on `release` is one check with a hand-written list behind it,
# and this is what keeps the list honest (issue #575).
#
# `release-gate` in .github/workflows/ci.yml is the job branch protection
# points at. It succeeds only when every other job in that file succeeded,
# and it knows which jobs those are from its own `needs:` list, because
# GitHub Actions has no way to say "every job in this workflow". So the
# list is an enumeration maintained by hand, sitting between a pull
# request and a signed image on a public registry. An enumeration like
# that goes stale the first time somebody adds a job and does not think
# about this one, and the failure is silent and in the dangerous
# direction: the new job runs, goes red, and the required check is green
# anyway because it was never asked about it.
#
# Four things are pinned here, and the first is the one that matters:
#
#   1. every job in ci.yml is in release-gate's needs list;
#   2. nothing in that list names a job that no longer exists, which is
#      the same drift seen from the other end and would fail the workflow
#      at parse time rather than silently, but fails here first and says
#      why;
#   3. release-gate carries `if: always()`, because without it a failed
#      dependency SKIPS this job, a skipped check reports no conclusion,
#      and branch protection reads no conclusion as "still waiting"
#      rather than as a failure;
#   4. ci.yml still triggers on a pull request into `release`, which is
#      the whole acceptance criterion of #575 and one line away from
#      being undone by accident.
#
# Run directly (`bash scripts/tests/release-gate-covers-every-job.test.sh`)
# or let the gate run it: scripts/ci-local.sh invokes it in every run.
set -uo pipefail

SCRIPTS_DIR="$(cd "$(dirname "$0")/.." && pwd)"
REPO_ROOT="$(cd "$SCRIPTS_DIR/.." && pwd)"
WORKFLOW="$REPO_ROOT/.github/workflows/ci.yml"

echo "==> release gate: the one required check still covers every job"

# Parsed with the standard library rather than a YAML module, so this runs
# on any machine with python3 and needs nothing installed. The shape it
# relies on is narrow (two-space job keys under a column-zero `jobs:`,
# six-space `- name` entries under `needs:`) and every reliance is backed
# by a positive control below: a parser that quietly stopped matching
# would otherwise report a clean sweep of nothing, which is the failure
# this file exists to catch wearing a different hat.
python3 - "$WORKFLOW" <<'PY'
import re, sys

path = sys.argv[1]
lines = open(path, encoding="utf-8").read().splitlines()

GATE = "release-gate"

jobs = []
needs = []
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

failures = []
checks = 0


def check(ok, ok_text, bad_text):
    global checks
    checks += 1
    if ok:
        print("    ok   " + ok_text)
    else:
        failures.append(bad_text)
        print("    FAIL " + bad_text, file=sys.stderr)


# The positive controls come first. Everything below is a set comparison,
# and a set comparison against an empty set is the cheapest way in the
# world to report success while looking at nothing.
check(
    len(jobs) >= 10,
    f"the parser found {len(jobs)} jobs in ci.yml, so it is reading the file",
    f"the parser found only {len(jobs)} jobs in ci.yml ({jobs}), which is fewer than this workflow has ever had: it has stopped matching, and every comparison below would pass against nothing",
)
check(
    GATE in jobs,
    f"{GATE} is one of them",
    f"{GATE} is not among the jobs the parser found ({jobs}): either the job was renamed and this test was not, or the parser is broken",
)
check(
    "two-machine-e2e" in jobs,
    "and so is two-machine-e2e, the proof #575 exists to put in front of a release",
    "two-machine-e2e is not in ci.yml: the two-machine proof is the one test anywhere that a fresh install can pull a real backup off a real machine, and a release gate without it is back to gating on nobody",
)
check(
    len(needs) > 0,
    f"{GATE} declares {len(needs)} dependencies",
    f"{GATE} has no needs list the parser could find, so it depends on nothing and would go green on its own while every other job burned",
)

covered = set(needs)
expected = {j for j in jobs if j != GATE}

missing = sorted(expected - covered)
check(
    not missing,
    "every job in this workflow is in its needs list",
    "these jobs run in ci.yml and " + GATE + " does not wait for them, so each one could go red with the required check still green: " + ", ".join(missing),
)

phantom = sorted(covered - expected)
check(
    not phantom,
    "and it names no job that does not exist",
    GATE + " depends on jobs that are not in this workflow, so the list has drifted from the file it lives in: " + ", ".join(phantom),
)

check(
    gate_has_always,
    f"{GATE} carries `if: always()`, so a failed job leaves it red rather than skipped",
    f"{GATE} has no `if: always()`. Without it a failed dependency SKIPS this job, a skipped check reports no conclusion at all, and a branch protection rule waiting on it reads that as 'not finished yet' rather than as a failure: the merge button stays blocked forever instead of the change being refused, and re-running to clear it is the obvious wrong fix",
)

body = "\n".join(lines)
trigger = re.search(r"^on:\s*$\n(?:^[ #].*$\n?)*", body, re.MULTILINE)
trigger_text = trigger.group(0) if trigger else ""
check(
    "pull_request:" in trigger_text and re.search(r"^\s+- release\s*$", trigger_text, re.MULTILINE) is not None,
    "ci.yml still runs on a pull request into release",
    "ci.yml no longer triggers on a pull request into `release`. Merging into that branch publishes a signed image to a public registry, and without this trigger nothing checks the change first, which is the whole of #575:\n" + (trigger_text or "(no `on:` block found at all)"),
)

print()
if failures:
    print(f"==> release gate: FAILED ({len(failures)} of {checks} checks)", file=sys.stderr)
    sys.exit(1)
print(f"==> release gate: ok ({checks} checks)")
PY
