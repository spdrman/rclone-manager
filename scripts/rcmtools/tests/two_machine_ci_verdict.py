#!/usr/bin/env python3
"""Self-test for scripts/e2e/two-machine-ci.sh, the translation that stands
between the two-machine proof and a required check on `release` (#575).

The failure this exists to prevent has one shape. The proof says 3 when
the machine it is running on cannot perform it, scripts/ci-local.sh
reads that as INCOMPLETE and ledgers it, and a workflow step reading the
same 3 as "nothing to report" would publish a signed image on the
strength of a proof that never ran. Docker-in-docker needs a privileged
container and the connection-cap case needs a kernel that carries an
iptables connlimit rule, so "this runner cannot" is a live outcome on
hosted runners and not a hypothetical.

So all three outcomes are pinned here, and the middle one is pinned
twice: red, and red under a word that is not FAILED. A run that could
not look must not be mistaken for a run that looked and liked what it
saw, and it must not be mistaken for a broken product either, because
the second is triaged by reading the diff and the first is triaged by
fixing the runner.

Ported from `scripts/tests/two-machine-ci-verdict.test.sh` under EPIC I
(#672 / #697). That file now execs this one.

# PORTED-CHECK HAZARD NOTE

case 2: a proof that could not run must never leave the wrapper with 0
  hazard in bash:   a wrapper bug that swallowed a non-zero `bash` exit
                    (an unguarded `$(...)` under no `-e`, or a stray
                    `|| true`) would surface as a green required check on
                    the branch that publishes a signed image.
  hazard in python: STILL EXISTS as a real possibility in
                    scripts/rcmtools/e2e/two_machine_ci.py, whose own hazard
                    note (see that file) answers this exact case; this
                    driver is unchanged in shape from bash because it never
                    ran the proof `-e`-guarded to begin with (`set -uo
                    pipefail`, no `-e`, same in the original).
  held by:          this suite's own case 2 and case 5 (log and summary),
                    both driving the REAL shim at scripts/e2e/two-machine-ci.sh
                    rather than the module directly, so the exec chain is
                    exercised end to end.

case 6: the Actions annotation is conditional, not merely harmless
  hazard in bash:   `env -u GITHUB_ACTIONS -u GITHUB_STEP_SUMMARY` unsets
                    variables Actions injects into every step; a case that
                    only declined to SET them would inherit both from this
                    suite's own CI run and assert the opposite of what it
                    claims -- found on a runner (#575), not locally.
  hazard in python: STILL EXISTS as the same environment-inheritance risk:
                    `env` below is built from `os.environ` with the two keys
                    POPPED, not a dict that merely omits them, for the exact
                    reason the bash comment gives.
  held by:          the `env.pop(...)` calls in `run_wrapper_unset`, and
                    case 6 itself, which fails if `::error` leaks or if
                    INCOMPLETE goes missing without the summary file.

This driver's own subprocess-capture hazard is testkit's (see that
module's docstring): `run()` never raises on the wrapper's exit status,
because every case here reads an EXPECTED status rather than treating a
non-zero as a refusal.
"""

from __future__ import annotations

import os
import shutil
import stat
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools.tests import testkit

SCRIPTS_DIR = Path(__file__).resolve().parents[2]
REPO_ROOT = SCRIPTS_DIR.parent
WRAPPER = SCRIPTS_DIR / "e2e" / "two-machine-ci.sh"


def fake_proof(sandbox: Path, status: int) -> Path:
    """Write a stand-in for the real proof that exits with `status`."""
    path = sandbox / f"proof-{status}.sh"
    path.write_text(f"#!/usr/bin/env bash\necho \"fake proof, exiting {status}\"\nexit {status}\n")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    return path


def run_wrapper(sandbox: Path, status: int) -> tuple[str, int]:
    """Run the wrapper against a proof exiting `status`, with GITHUB_ACTIONS
    and GITHUB_STEP_SUMMARY set so the annotation and summary can be read."""
    proof = fake_proof(sandbox, status)
    summary = sandbox / f"summary-{status}.md"
    summary.write_text("")
    env = dict(os.environ)
    env["TWO_MACHINE_PROOF"] = str(proof)
    env["GITHUB_ACTIONS"] = "true"
    env["GITHUB_STEP_SUMMARY"] = str(summary)
    return testkit.run(["bash", str(WRAPPER)], env=env)


def run_wrapper_unset(sandbox: Path, status: int) -> str:
    """Run the wrapper with GITHUB_ACTIONS/GITHUB_STEP_SUMMARY POPPED from
    the environment (case 6's own hazard note: unset, not merely omitted)."""
    proof = fake_proof(sandbox, status)
    env = dict(os.environ)
    env.pop("GITHUB_ACTIONS", None)
    env.pop("GITHUB_STEP_SUMMARY", None)
    env["TWO_MACHINE_PROOF"] = str(proof)
    out, _status = testkit.run(["bash", str(WRAPPER)], env=env)
    return out


def main() -> int:
    print("==> two-machine CI verdict: three outcomes, and the one that must not read as a pass", flush=True)
    suite = testkit.Suite("two-machine CI verdict")

    sandbox = REPO_ROOT / ".two-machine-ci-test"
    if sandbox.parent != REPO_ROOT or sandbox.name != ".two-machine-ci-test":
        print(f"refusing to use sandbox path [{sandbox}]", file=sys.stderr)
        return 1
    shutil.rmtree(sandbox, ignore_errors=True)
    sandbox.mkdir(parents=True)
    try:
        # ------------------------------------------------------- case 1
        out, status = run_wrapper(sandbox, 0)
        suite.check(status == 0, "a proof that passed leaves the wrapper with 0, so the check can go green",
                    f"a proof that passed left the wrapper with {status}, want 0", out)
        suite.assert_contains("and it says PASSED", "PASSED", out)

        # ------------------------------------------------------- case 2
        out, status = run_wrapper(sandbox, 3)
        if status == 0:
            suite.bad(
                "a proof that COULD NOT RUN left the wrapper with 0: a runner that cannot start "
                "docker-in-docker would report a green gate on the branch that publishes a signed image",
                out,
            )
        elif status == 3:
            suite.ok(
                "a proof that could not run leaves the wrapper with 3: red, and still telling the caller "
                "which of the two reds it is"
            )
        else:
            suite.bad(f"a proof that could not run left the wrapper with {status}, want 3", out)
        suite.assert_contains("and it says INCOMPLETE", "INCOMPLETE", out)
        suite.check(
            "FAILED" not in out,
            "and it does not call it FAILED, because nothing failed: the runner could not look",
            "a proof that could not run was reported as FAILED, which sends the reader to the diff when "
            "the problem is the runner",
            out,
        )

        # ------------------------------------------------------- case 3
        out, status = run_wrapper(sandbox, 1)
        suite.check(status == 1, "a proof that failed leaves the wrapper with 1",
                    f"a proof that failed left the wrapper with {status}, want 1", out)
        suite.assert_contains("and it says FAILED, so a real defect is not filed under a runner limitation",
                               "FAILED", out)
        suite.check(
            "INCOMPLETE" not in out,
            "and not INCOMPLETE",
            "a failing proof was also called INCOMPLETE, so the two verdicts are not distinguishable",
            out,
        )

        # ------------------------------------------------------- case 4
        out, status = run_wrapper(sandbox, 127)
        suite.check(status == 127, "a status the wrapper has no meaning for reaches the caller unchanged",
                    f"a proof exiting 127 left the wrapper with {status}, want 127", out)

        # ------------------------------------------------------- case 5
        proof = fake_proof(sandbox, 3)
        summary = sandbox / "summary-file.md"
        summary.write_text("")
        env = dict(os.environ)
        env["TWO_MACHINE_PROOF"] = str(proof)
        env["GITHUB_ACTIONS"] = "true"
        env["GITHUB_STEP_SUMMARY"] = str(summary)
        testkit.run(["bash", str(WRAPPER)], env=env)
        summary_text = summary.read_text()
        suite.check(
            "INCOMPLETE" in summary_text,
            "the verdict is written to the job summary, not only to the log",
            "GITHUB_STEP_SUMMARY was set and the verdict never reached it",
            summary_text,
        )

        # ------------------------------------------------------- case 6
        out = run_wrapper_unset(sandbox, 3)
        suite.check(
            "::error" not in out,
            "off a runner no Actions workflow command is emitted, and the verdict is still printed",
            "the Actions annotation is emitted off a runner too, so a local run prints workflow commands "
            "nothing will read",
            out,
        )
        suite.assert_contains("and the verdict itself is still there without GITHUB_STEP_SUMMARY set",
                               "INCOMPLETE", out)
    finally:
        shutil.rmtree(sandbox, ignore_errors=True)

    return suite.finish()


if __name__ == "__main__":
    sys.exit(main())
