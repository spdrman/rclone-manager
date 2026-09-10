#!/usr/bin/env python3
"""The shared vocabulary every script in this repository speaks.

`step`, `note`, `die`, `cannot_run`, `wait_or_die`, one exit-code
taxonomy, one help renderer, one docker capability gate, and one place
where a run's status is translated for its caller.

# This module's API is a published contract (EPIC I, I1.6 / #672)

Not an internal helper for whoever ported first. `scripts/` is sixty
files and roughly thirty thousand lines across twelve domains, and each
of those domains grew its own `check-*.sh`, `selftest.sh` and `lib.sh`
saying the same four things in four different ways. That is why two
scripts that both meant "this machine could not perform the proof" said
it with two different numbers. This module is the one place those live
now, and the following MAY NOT be redefined anywhere else under
`scripts/`:

  * `die`, `note`, `step`, `wait_or_die`;
  * the exit-code taxonomy, `EXIT_CANNOT_RUN` -> 3 above all;
  * the HELP-START/HELP-END renderer;
  * the docker capability gate (`require_docker`, `require_tools`);
  * the INCOMPLETE ledger shape `scripts/ci-local.sh` reads.

A domain that needs something this module does not have widens it WITH A
REAL CALLER IN THE SAME CHANGE. Nothing is added here for a consumer that
does not exist yet: a harness designed for eight hypothetical domains
before any of them has ported is exactly how the twelve `lib.sh` copies
happened in the first place.

One file that looks like it belongs here and does not:
`scripts/install/install_docker_host.py` is copied to a NAS on its own,
with no checkout around it, and must never import this package. Its
standard-library-only rule is the same rule; its no-local-imports rule is
stricter, and deliberately.

# The rule every ported check is held to

A port can silently convert a check into one that cannot fail. The worked
example is `scripts/tests/two-machine-exit-status.test.sh` case 2: it
plants a fake `git` that exits 3 to prove an unguarded 3 never becomes
the gate's own "could not run" verdict. In bash that hazard is
`set -euo pipefail` propagating a subprocess status. In Python a
subprocess status nobody reads is simply discarded, so the regression
becomes structurally impossible and the case goes VACUOUSLY GREEN --
worse than a deleted check, because nothing anywhere says it stopped
watching.

So every ported check carries a section of its own, in its own file,
headed exactly:

    # PORTED-CHECK HAZARD NOTE

with one entry per assertion the check makes, each answering three
questions in this order:

    <assertion>
      hazard in bash:   what could go wrong that this caught
      hazard in python: STILL EXISTS | GONE, and why
      held by:          what keeps it red-able now

"GONE" is only an acceptable answer when the row also says what replaced
it. A later porter fills this in mechanically; that is the point of
fixing the shape here rather than describing it in prose.

# Why the exit codes are a contract rather than a convention

Three separate static checks already hold this repository to one number,
and they were written before there was anywhere to put it:

  * `scripts/tests/two-machine-exit-status.test.sh` pins that the
    two-machine proof says 3 for "this machine could not perform the
    proof" and for nothing else;
  * `scripts/tests/two-machine-ci-verdict.test.sh` pins that a 3 reaching
    CI goes red under the word INCOMPLETE rather than green under no word
    at all;
  * `scripts/tests/ci-local-gate.test.sh` fabricates stand-in scripts that
    exit 3 and requires `scripts/ci-local.sh` to ledger them as INCOMPLETE
    rather than as a pass.

So 3 is not this module's opinion. It is a published number with three
tests standing on it, and `EXIT_CANNOT_RUN` below exists because a script
must be able to reach that verdict without ever having 3 travel through
its own body -- see that constant's own comment.

# Why a Python port had to be careful here

In bash, `set -euo pipefail` makes every unguarded command's non-zero
status the script's own status. That is the hazard
`two-machine-exit-status.test.sh` case 2 exists to catch: a `bm` call
meeting the CLI's own exit 3 (#551, "another process is already serving
this deployment") would have ended the proof with the number that means
"this machine could not try", and a failed proof would have been ledgered
as a skip.

Python has no `set -e`. A `subprocess.run` whose status nobody reads is
simply ignored, so that hazard does not exist here by default -- and a
check whose hazard cannot occur is a check that cannot fail, which is
worse than a deleted one because nothing says it stopped watching. So
`sh()` below RESTORES the propagation deliberately: `check=True` raises
`CommandFailed` carrying the status, and `finish()` is the single place
that decides what any status means to the caller. The regression stays
reachable, the test stays red-able, and the translation is in one
function instead of being a property of the shell.
"""

from __future__ import annotations

import os
import re
import signal
import subprocess
import sys
import time
import types
from pathlib import Path
from typing import Callable, Iterable, NoReturn, Sequence

# ---------------------------------------------------------------------
# Exit codes
# ---------------------------------------------------------------------
#
# The same reasoning `scripts/install/install_docker_host.py` gives for
# its own table: a bare non-zero tells a caller that something went wrong
# and nothing about what to do next, and the caller here is usually a gate
# script branching on the answer rather than a human reading prose.

EXIT_OK = 0

# A proof that ran and did not hold. `die()` produces this.
EXIT_FAILED = 1

# A command line no deployment would have accepted.
EXIT_USAGE = 2

# "This machine could not perform the proof." What the CALLER sees, and
# what `scripts/lib/ci-local-gate.sh` ledgers as INCOMPLETE.
EXIT_INCOMPLETE = 3

# The same verdict, as it travels INSIDE a script, and the reason the two
# are different numbers.
#
# The CLI these scripts drive has its own meaning for 3 (#551: another
# process is already serving this deployment). A script that used 3
# internally would be unable to tell its own verdict apart from a
# subprocess that happened to produce one, and the failure mode is silent
# in the worst direction: `ci-local.sh` ledgers it, the run ends
# INCOMPLETE, and `.husky/pre-commit` lets an INCOMPLETE gate commit. The
# one test anywhere that proves a backup can be pulled off a real machine
# would have failed and been read as a machine that never tried.
#
# So the verdict gets a number nothing else produces, and `finish()` is
# the single place either number is spoken to the caller.
EXIT_CANNOT_RUN = 97

# What a script exits with when it is interrupted. The shell's own
# convention (128 + signal number), kept because a caller reading these
# is usually a shell.
EXIT_INTERRUPTED = 130
EXIT_TERMINATED = 143


# ---------------------------------------------------------------------
# Refusals
# ---------------------------------------------------------------------


class Failure(Exception):
    """A proof that ran and did not hold. Raised by `die()`.

    Carries the headline separately from the detail lines because they
    answer different questions: the headline says what is not true, and
    the details say what was measured. `finish()` prints them the way
    `die` did in bash, indented under one FAILED line.
    """

    def __init__(self, message: str, *details: str) -> None:
        super().__init__(message)
        self.message = message
        self.details: list[str] = [d for d in details if d]


class CannotRun(Exception):
    """A capability this machine does not have.

    Not a failure and not a pass either. See `EXIT_CANNOT_RUN` above for
    why this does not carry 3 itself.
    """

    def __init__(self, message: str, *details: str) -> None:
        super().__init__(message)
        self.message = message
        self.details: list[str] = [d for d in details if d]


class CommandFailed(Exception):
    """A subprocess exited non-zero and nobody guarded it.

    This is the Python restoration of `set -e`, and it exists so an
    unguarded status still reaches `finish()` and gets translated there.
    A caller that means to tolerate a non-zero status passes
    `check=False` and reads `.returncode`, which is the explicit form of
    bash's `|| true`.
    """

    def __init__(self, status: int, argv: Sequence[str], stdout: str = "", stderr: str = "") -> None:
        super().__init__(f"{' '.join(argv)} exited {status}")
        self.status = status
        self.argv = list(argv)
        self.stdout = stdout
        self.stderr = stderr


class Interrupted(Exception):
    """A signal arrived. Carries the status the script should end with."""

    def __init__(self, status: int, signame: str) -> None:
        super().__init__(signame)
        self.status = status
        self.signame = signame


# ---------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------
#
# `step` and `note` are the two shapes every script in this repository
# prints, and the prefix is the script's own name so an operator reading a
# combined gate log can tell which proof is talking. `flush=True` for the
# reason install_docker_host.py's `say()` gives: almost every interesting
# run happens with stdout on a pipe, where Python buffers in blocks, so a
# `docker compose up` that takes ninety seconds would otherwise print
# nothing and then everything -- with any subprocess output that inherited
# the real file descriptor landing ahead of the lines explaining what was
# being attempted.

_program = "rcmtools"


def set_program(name: str) -> None:
    """Name the running script, for every line printed after this."""
    global _program
    _program = name


def program() -> str:
    return _program


def step(text: str) -> None:
    """Announce the thing about to be attempted."""
    print("", flush=True)
    print(f"==> {_program}: {text}", flush=True)


def note(text: str) -> None:
    """Record something that just held, indented under its step."""
    print(f"    {text}", flush=True)


def die(message: str, *details: str) -> NoReturn:
    """Refuse: this proof ran and did not hold.

    Raises rather than exiting, so the teardown in `finish()` runs on the
    way out and a caller that wants to add context can catch it. Typed
    NoReturn so a checker knows the code after a `die()` call is
    unreachable, which is what the bash `die ... ; exit 1` said.
    """
    raise Failure(message, *details)


def cannot_run(message: str, *details: str) -> NoReturn:
    """Refuse: this machine cannot perform the proof at all.

    Reporting a pass for a run that never happened is the one thing these
    scripts must not do, and reporting a failure for it is nearly as bad:
    somebody triages a product defect that is really a runner without
    privileged containers. This is the third answer.
    """
    raise CannotRun(message, *details)


def report_failure(failure: Failure) -> int:
    """Print a Failure and return the status, for refusals outside `finish()`.

    Option parsing happens before a run has created anything, and the bash
    these scripts replace parsed its arguments BEFORE installing its EXIT
    trap for exactly that reason: `--help` and `unknown option` must not
    print a teardown banner for a run that never started. That ordering is
    load bearing -- `scripts/tests/e2e-help.test.sh` compares `--help`'s
    combined output against a golden byte for byte, and a teardown line
    would be in it.
    """
    _print_failure(failure.message, failure.details)
    return EXIT_FAILED


# ---------------------------------------------------------------------
# Running things
# ---------------------------------------------------------------------


def sh(
    argv: Sequence[str],
    *,
    check: bool = True,
    capture: bool = True,
    stdin_text: str | None = None,
    stdin_file: Path | None = None,
    timeout: float | None = None,
    cwd: Path | None = None,
    env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess[str]:
    """Run a command, and let an unguarded failure travel.

    `capture=True` keeps both streams so a refusal can quote what the
    command actually said. `capture=False` inherits this process's own
    stdout and stderr, which is what a long `docker build` or a backup
    cycle wants: its output IS the run's progress, and buffering it until
    the end would hide a hang.

    Nothing here uses DEVNULL. A subprocess whose stderr is discarded is
    how a script reports success on a failed step.
    """
    stdin_handle = None
    opened = None
    try:
        if stdin_file is not None:
            opened = open(stdin_file, "rb")
            stdin_handle = opened
        try:
            proc = subprocess.run(
                list(argv),
                stdout=subprocess.PIPE if capture else None,
                stderr=subprocess.PIPE if capture else None,
                stdin=stdin_handle,
                input=stdin_text if stdin_file is None else None,
                # utf-8/replace rather than the platform-default strict
                # decoding: a CI runner or an appliance account can
                # plausibly be under a C/POSIX locale, and docker's output
                # carries non-ASCII. A strict-decode failure here would be
                # an unhandled UnicodeDecodeError rather than a coded
                # refusal, on exactly the calls whose failure matters most.
                encoding="utf-8" if (capture or stdin_text is not None) else None,
                errors="replace" if (capture or stdin_text is not None) else None,
                timeout=timeout,
                cwd=str(cwd) if cwd is not None else None,
                env=env,
            )
        except FileNotFoundError as exc:
            # 127 is what a shell says for "command not found", and these
            # scripts are read by shells. Raised rather than refused
            # because a missing tool at a call site is a bug in the
            # script; a missing tool the run DEPENDS on is caught by
            # `require_tools` in preflight, where it becomes a
            # `cannot_run` with a remedy.
            raise CommandFailed(127, argv, "", str(exc)) from exc
        except subprocess.TimeoutExpired as exc:
            raise Failure(
                f"{' '.join(argv)} did not finish within {timeout}s.",
                "Every wait in these scripts is bounded; this one was reached and the command never returned.",
            ) from exc
    finally:
        if opened is not None:
            opened.close()

    if check and proc.returncode != 0:
        raise CommandFailed(
            proc.returncode,
            argv,
            proc.stdout or "" if capture else "",
            proc.stderr or "" if capture else "",
        )
    return proc


def sh_ok(argv: Sequence[str], *, timeout: float | None = None) -> bool:
    """True when the command exits 0, with both streams discarded.

    The one place output is thrown away on purpose: this is the shape of a
    readiness probe, where the question is only whether the command
    succeeded and the output is a health check's chatter rather than a
    diagnosis.
    """
    try:
        return sh(argv, check=False, capture=True, timeout=timeout).returncode == 0
    except CommandFailed:
        return False
    except Failure:
        return False


def sh_out(argv: Sequence[str], *, check: bool = True, timeout: float | None = None) -> str:
    """The command's stdout, stripped of its trailing newline.

    The equivalent of bash's `$(...)`, including that an unguarded failure
    still travels: `check` defaults to True here for the same reason
    `set -e` was on in the scripts this replaces.
    """
    out: str = sh(argv, check=check, capture=True, timeout=timeout).stdout
    return out.rstrip("\n")


def have_tool(name: str) -> bool:
    """Whether an external tool is on PATH."""
    from shutil import which

    return which(name) is not None


def require_tools(*names: str) -> None:
    """Refuse, as a capability verdict, if any named tool is missing.

    `cannot_run` rather than `die`: a machine without `ssh-keygen` has not
    disproved anything, and the gate must ledger it rather than report a
    product failure.
    """
    for name in names:
        if not have_tool(name):
            cannot_run(f"{name} is not on PATH, and this test needs it.")


def require_docker() -> None:
    """The docker capability gate, in the one place that owns it."""
    if not sh_ok(["docker", "info"]):
        cannot_run(
            "the Docker daemon is not reachable.",
            "Start Docker and re-run. This test cannot be performed without it, and reporting a pass "
            "for a run that never happened is the one thing it must not do.",
        )


# ---------------------------------------------------------------------
# Waiting
# ---------------------------------------------------------------------


def wait_or_die(budget: int, what: str, probe: Callable[[], bool], *, interval: float = 1.0) -> None:
    """Poll until `probe()` is true, or refuse after `budget` seconds.

    Every wait in these scripts goes through here, so none of them can be
    the one that hangs a cold machine forever, and every timeout says what
    it was waiting for rather than only that it waited.
    """
    deadline = time.time() + budget
    while True:
        if probe():
            return
        if time.time() >= deadline:
            die(f"timed out after {budget}s waiting for {what}.")
        time.sleep(interval)


# ---------------------------------------------------------------------
# Help
# ---------------------------------------------------------------------
#
# --help prints the block between two markers in the file, and
# deliberately not a range of line numbers (#514). The old form was
# `sed -n '2,110p' "$0"`, so the help an operator read was a set of
# coordinates rather than a piece of text: inserting a comment above the
# boundary rewrote it, deleting one truncated it, and nothing anywhere
# would have noticed either. Both had already happened by the time #514
# was written.
#
# Markers move with the text they delimit. The block lives in the module
# docstring now rather than in a comment block, which is the Python shape
# of the same idea, and the markers are still read out of the FILE rather
# than out of `__doc__`: a renderer reading the docstring could not tell
# a block with its markers deleted apart from a module with no docstring,
# and refusing loudly in that case is the property
# `scripts/tests/e2e-help.test.sh` case D3 exists to hold.

HELP_START = "HELP-START"
HELP_END = "HELP-END"

_COMMENT_PREFIX = re.compile(r"^# ?")


def render_help(path: Path) -> str:
    """The help text between the markers in `path`.

    Strips one leading `#` and at most one space from each line, so the
    same renderer serves a bash comment block and a Python docstring. A
    file that has lost either marker is refused out loud rather than
    answered with an empty help and a zero exit, which is #160's silent
    skip wearing a different hat.
    """
    opened = False
    closed = False
    inside = False
    out: list[str] = []
    for raw in path.read_text(encoding="utf-8").splitlines():
        stripped = raw.strip()
        if stripped == HELP_END or stripped == "# " + HELP_END:
            closed = True
            inside = False
            continue
        if stripped == HELP_START or stripped == "# " + HELP_START:
            opened = True
            inside = True
            continue
        if inside:
            out.append(_COMMENT_PREFIX.sub("", raw))
    if not opened or not closed:
        die(
            f"the help block is missing from {path}",
            "--help renders the lines between the HELP-START and HELP-END markers,",
            "and this file has lost one or both of them.",
        )
    return "\n".join(out)


# ---------------------------------------------------------------------
# The exit path
# ---------------------------------------------------------------------


def _print_failure(message: str, details: Iterable[str]) -> None:
    print("", file=sys.stderr, flush=True)
    print(f"==> {_program}: FAILED. {message}", file=sys.stderr, flush=True)
    for line in details:
        for physical in str(line).splitlines() or [""]:
            print(f"    {physical}", file=sys.stderr, flush=True)


def _print_cannot_run(message: str, details: Iterable[str]) -> None:
    print("", file=sys.stderr, flush=True)
    print(f"==> {_program}: CANNOT RUN. {message}", file=sys.stderr, flush=True)
    for line in details:
        for physical in str(line).splitlines() or [""]:
            print(f"    {physical}", file=sys.stderr, flush=True)


def install_signal_handlers() -> None:
    """Turn INT and TERM into an exception, so teardown still runs.

    Three handlers rather than one, and the difference is what makes the
    interrupt claim true rather than intended. The bash this replaces
    needed `trap ... INT` separate from `trap ... EXIT` because a bash
    trap on a signal runs the handler and then RESUMES the script. Python
    has the same trap in a different shape: a signal handler that only
    sets a flag lets the interrupted call finish and the script carry on
    against containers that are about to be removed. Raising unwinds to
    `finish()`, whose `finally` is the single teardown.
    """

    def handler(signum: int, _frame: types.FrameType | None) -> None:
        if signum == signal.SIGINT:
            raise Interrupted(EXIT_INTERRUPTED, "SIGINT")
        raise Interrupted(EXIT_TERMINATED, "SIGTERM")

    signal.signal(signal.SIGINT, handler)
    signal.signal(signal.SIGTERM, handler)


def finish(
    body: Callable[[], int],
    *,
    teardown: Callable[[int], None] | None = None,
) -> int:
    """Run `body`, tear down, and translate the status for the caller.

    The single place either number is spoken, which is what lets the exit
    codes be a contract instead of a habit:

      EXIT_CANNOT_RUN  ->  3, the verdict a gate ledgers as INCOMPLETE.
      3                ->  1, because nothing here means 3, so a 3 arriving
                           here came from a command with its own meaning
                           for it (the CLI's is "another process is already
                           serving this deployment", #551). That is a
                           failed proof, and it is reported as one instead
                           of borrowing a verdict about the machine.
      anything else    ->  itself, untouched.

    The last line of that table is not decoration either: without it the
    translation would be "anything that failed becomes 1", and the check
    that a 3 does not become the machine verdict would pass against a
    script that had lost every other status it can report.
    """
    status = EXIT_OK
    try:
        status = body()
    except Failure as failure:
        _print_failure(failure.message, failure.details)
        status = EXIT_FAILED
    except CannotRun as refusal:
        _print_cannot_run(refusal.message, refusal.details)
        status = EXIT_CANNOT_RUN
    except CommandFailed as failed:
        # An unguarded non-zero, which is what `set -e` used to turn into
        # the script's own status. Printed with what the command said,
        # because a status alone sends whoever reads it back to the log.
        _print_failure(
            f"{' '.join(failed.argv)} exited {failed.status}.",
            [
                line
                for line in (
                    ("--- stdout ---\n" + failed.stdout.rstrip()) if failed.stdout.strip() else "",
                    ("--- stderr ---\n" + failed.stderr.rstrip()) if failed.stderr.strip() else "",
                )
                if line
            ],
        )
        status = failed.status
    except Interrupted as interrupted:
        print("", file=sys.stderr, flush=True)
        print(f"==> {_program}: {interrupted.signame}, tearing down.", file=sys.stderr, flush=True)
        status = interrupted.status
    finally:
        if teardown is not None:
            teardown(status)

    if status == EXIT_CANNOT_RUN:
        return EXIT_INCOMPLETE
    if status == EXIT_INCOMPLETE:
        print("", file=sys.stderr, flush=True)
        print(f"==> {_program}: FAILED. A command exited 3.", file=sys.stderr, flush=True)
        for line in (
            'This script reserves 3 for the gate\'s "this machine could not perform the proof" verdict and never',
            "produces it itself, so a 3 here came from something with its own meaning for that status: most",
            "likely the CLI refusing because another process is already serving the deployment (#551), from a",
            "call with no guard on it. Reported as a failure (1), which is what a proof that did not finish",
            "is. The run above says which step it died on.",
        ):
            print(f"    {line}", file=sys.stderr, flush=True)
        return EXIT_FAILED
    return status


def repo_root(start: Path) -> Path:
    """The checkout `start` lives in, found by walking up to `scripts/`.

    Every entry point resolves its own root before it changes directory,
    for the reason the bash it replaces spelled out: a `$0` the shell left
    relative stops resolving the moment the run cd's somewhere, and both
    e2e drivers cd to the repository root before parsing their arguments.
    `Path.resolve()` settles it once, here, from the file's real location.
    """
    here = start.resolve()
    for candidate in [here, *list(here.parents)]:
        if (candidate / "scripts").is_dir() and (candidate / ".git").exists():
            return candidate
    # A sandbox copy (scripts/tests/e2e-help.test.sh makes one) has the
    # directory shape and no .git. Fall back to the shape alone rather
    # than refusing, because rendering --help must work from a bare copy.
    for candidate in [here, *list(here.parents)]:
        if (candidate / "scripts").is_dir():
            return candidate
    return here.parents[len(here.parents) - 1]


def env_flag(name: str, default: str = "0") -> bool:
    """An environment variable read as a 0/1 switch, the way the gate reads them."""
    return os.environ.get(name, default) == "1"
