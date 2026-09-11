#!/usr/bin/env python3
"""Self-test for the one number scripts/e2e/two-machine-backup.sh says to
its caller, and for the collision that made it worth pinning.

The chain, end to end: scripts/lib/ci-local-gate.sh uses 3 for "the gate
performed less than it was asked to" (#160), scripts/ci-local.sh reads a
3 from the two-machine proof as exactly that and ledgers it, and
.husky/pre-commit turns an INCOMPLETE gate into a commit that is allowed
with a warning. Then issue #551 gave the CLI an exit status of its own
at 3 (another process is already serving this deployment), and every
`bm` call in that script runs the real CLI under `set -euo pipefail`. An
unguarded one meeting that refusal would have ended the script with the
number that means "this machine could not try", so a failed proof would
have been ledgered as a skip and the commit would have gone through.

It was latent when it was found: every `bm` that can meet the refusal is
guarded with `|| die`, and the only unguarded substitution runs
`version`. That is safety by review. These cases are what makes it
safety by construction again, which is what it used to be when the CLI
had no 3 at all.

The proof plants a command that exits 3 at a call site the script does
NOT guard (the `git rev-parse` the build step starts with), rather than
at a `bm` call, and that is deliberate: the guarantee worth having is
about any unguarded 3, not about the one command that happens to be able
to produce one today.

Ported from `scripts/tests/two-machine-exit-status.test.sh` under EPIC I
(#672 / #697). That file now execs this one.

# PORTED-CHECK HAZARD NOTE

EPIC I / I1.6 (#672) moved the proof this drives from bash to
scripts/bdtools/e2e/two_machine_backup.py, behind an exec shim at the old
path. A port can silently convert a check into one that CANNOT FAIL, and
this file is the worked example the epic cites, so every assertion it makes
answers for itself below.

case 1: a machine with no reachable Docker still exits 3
  hazard in bash:   the proof stops saying "could not run" at all, and the
                    gate's INCOMPLETE ledger silently stops being fed.
  hazard in python: STILL EXISTS. cannot_run() raises CannotRun,
                    harness.finish() maps EXIT_CANNOT_RUN 97 -> 3. Delete
                    either half and this goes red.
  held by:          scripts/bdtools/harness.py finish(), one translation
                    for every ported script.

case 2: an unguarded command exiting 3 must NOT become the machine verdict
  hazard in bash:   `set -euo pipefail` propagates a subprocess's status, so
                    a `bm` call meeting the CLI's own exit 3 (#551) ended the
                    script with the number that means "this machine could
                    not try". A failed proof ledgered as a skip, and
                    .husky/pre-commit lets an INCOMPLETE gate commit.
  hazard in python: STILL EXISTS, BUT ONLY BY CONSTRUCTION. Python has no
                    `set -e`; a subprocess status nobody reads is discarded,
                    so this regression would have become impossible and this
                    case would have gone VACUOUSLY GREEN -- worse than a
                    deleted check, because nothing would say it had stopped
                    watching. harness.sh(check=True) therefore RESTORES the
                    propagation deliberately: it raises CommandFailed
                    carrying the status, and finish() decides what the
                    status means in one place.
  held by:          harness.sh + harness.finish. PROVEN RED, not assumed:
                    replacing finish()'s `if status == EXIT_INCOMPLETE`
                    branch with `if False` fails 2 of this file's 6 checks
                    (case 2's status assertion and its "reserves 3" message
                    assertion). Re-run that mutation if you change either
                    function -- see this file's own docstring for the exact
                    command.

case 3: a status that means nothing here reaches the caller unchanged
  hazard in bash:   the translation degenerates to "anything that failed
                    becomes 1", which would make case 2 pass against a
                    script that had lost every other status it can report.
  hazard in python: STILL EXISTS. finish() returns the status untouched on
                    its default branch; collapsing that to EXIT_FAILED
                    reddens this.
  held by:          harness.finish()'s final `return status`.

What the fake tools reach, and why that did not change: the stand-ins are
prepended to PATH, and the Python proof shells out to `docker` and `git`
through harness.sh, which resolves them on PATH exactly as the shell did.
The planted `git` is still reached at the build step, and case 2 asserts
that it was reached rather than assuming it.

This driver itself carries one hazard of its own, not present in the
original because the shell had no equivalent call: `subprocess.run` here
uses `check=False` throughout (see testkit's own hazard note) so an
expected-nonzero status from the PROOF UNDER TEST is read rather than
raised.
"""

from __future__ import annotations

import os
import shutil
import stat
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools.tests import testkit

SCRIPTS_DIR = Path(__file__).resolve().parents[2]
REPO_ROOT = SCRIPTS_DIR.parent
SCRIPT = SCRIPTS_DIR / "e2e" / "two-machine-backup.sh"


def fake_bin(directory: Path, name: str, status: int) -> None:
    """Write a stand-in for a real tool that exits with `status`, printing
    nothing, into a PATH directory of this case's own."""
    directory.mkdir(parents=True, exist_ok=True)
    path = directory / name
    path.write_text(f"#!/usr/bin/env bash\nexit {status}\n")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)


def run_case(case_dir: Path) -> tuple[str, int]:
    """Run the proof with `case_dir/bin` prepended to PATH, returning its
    combined output and exit status."""
    env = dict(os.environ)
    env["PATH"] = f"{case_dir / 'bin'}{os.pathsep}{env.get('PATH', '')}"
    env["E2E_RUN_ID"] = f"exit-status-test-{os.getpid()}"
    return testkit.run(["bash", str(SCRIPT), "--case", "plain"], cwd=REPO_ROOT, env=env)


def main() -> int:
    print("==> two-machine exit status: the verdict, and what must not be mistaken for it", flush=True)
    suite = testkit.Suite("two-machine exit status")

    sandbox = REPO_ROOT / ".two-machine-exit-test"
    if sandbox.parent != REPO_ROOT or sandbox.name != ".two-machine-exit-test":
        print(f"refusing to use sandbox path [{sandbox}]", file=sys.stderr)
        return 1
    shutil.rmtree(sandbox, ignore_errors=True)
    sandbox.mkdir(parents=True)
    try:
        # ------------------------------------------------------- case 1
        c1 = sandbox / "cannot-run"
        fake_bin(c1 / "bin", "docker", 1)
        out, status = run_case(c1)
        suite.check(
            status == 3,
            "a machine with no reachable Docker daemon still exits 3, the status ci-local.sh ledgers",
            f"a machine with no reachable Docker daemon exited {status}, want 3",
            out,
        )
        suite.assert_contains(
            "and it says CANNOT RUN, so the 3 is the verdict and not something else that happened to be 3",
            "CANNOT RUN",
            out,
        )

        # ------------------------------------------------------- case 2
        c2 = sandbox / "planted-three"
        fake_bin(c2 / "bin", "docker", 0)
        fake_bin(c2 / "bin", "git", 3)
        out, status = run_case(c2)
        suite.check(
            "building the image under test" in out,
            "the planted command really is reached: the run got past the preflight to the build step",
            "the run never reached the step the 3 is planted in, so it proves nothing about an unguarded 3",
            out,
        )
        if status == 3:
            suite.bad(
                "an unguarded command exiting 3 left the script with 3, the status the gate reads as "
                "'this machine could not perform the proof': a failed proof would be ledgered as a skip and "
                ".husky/pre-commit would allow the commit",
                out,
            )
        elif status == 1:
            suite.ok("an unguarded command exiting 3 leaves the script with 1, so the gate reads a failure")
        else:
            suite.bad(f"an unguarded command exiting 3 left the script with {status}, want 1", out)
        suite.check(
            "reserves 3" in out,
            "and it says why the status changed, so nobody has to find this file to understand the 1",
            "the run turned a 3 into a 1 without saying so anywhere in its output",
            out,
        )

        # ------------------------------------------------------- case 3
        c3 = sandbox / "passthrough"
        fake_bin(c3 / "bin", "docker", 0)
        fake_bin(c3 / "bin", "git", 7)
        out, status = run_case(c3)
        suite.check(
            status == 7,
            "a status that means nothing to this script reaches the caller unchanged",
            f"a command exiting 7 left the script with {status}, want 7: the translation is rewriting statuses "
            "it has no business touching",
            out,
        )
    finally:
        shutil.rmtree(sandbox, ignore_errors=True)

    return suite.finish()


if __name__ == "__main__":
    sys.exit(main())
