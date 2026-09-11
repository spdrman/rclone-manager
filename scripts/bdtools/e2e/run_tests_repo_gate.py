#!/usr/bin/env python3
"""The replacement e2e signal (issues #158 and #197).

Until this existed, the browser suite had no automated execution at all:
nightly-e2e.yml's schedule was commented out, no workflow here triggered
on anything, and scripts/ci-local.sh never invoked Playwright. So the
suite ran when somebody remembered to run it, which is how a
deterministically red spec sat on main through four merges and got
dismissed twice as an ordering flake (#172, then #197).

This script is what ci-local.sh calls instead, on every non-FAST run, and
since ci-local.sh runs under `set -e` from .husky/pre-commit, a red suite
refuses the commit. It does two things:

  1. the CLI smoke slice (55 of Suite A's 60 cases) against a
     backupd built from THIS working tree;
  2. the browser suite against a real deployment built from THIS
     working tree, over the wire, through
     scripts/e2e/three-machine-web-ui.sh (#687: this used to start
     ui/shared's own Vite dev server over createMockApi and drive it
     through RM_UI_DIR, until the tests repository dropped RM_UI_DIR
     and moved Suite B onto RM_BASE_URL against a real deployment).

Both come from spdrman/backupd-tests at the sha in tests-repo.pin,
so the tests are versioned independently of the product and a new test
cannot break in-flight work here until the pin is bumped. What is under
test is never the pin's own idea of a build: it is the tree being
committed. Those are different things and the run says which is which.

Costs, measured on this machine: about 11 seconds for the smoke slice and
about 22 for the browser suite, against a gate that already runs
Docker-backed crash matrices for minutes. The first run at a new pin also
clones the tests repository and installs its Playwright, which is a minute
or two, once per pin.

Capability refusals follow gate_require_docker's shape rather than
inventing a new one: a missing capability the rig itself discovers (most
often Docker) is a hard failure that names it, by way of
three-machine-web-ui.sh's own output, and CI_LOCAL_SKIP_E2E=1 is the
out-loud opt-out that ledgers the skip in ci-local.sh so the run ends
INCOMPLETE and cannot be merge evidence. That answers #197's first open
question with this repository's own precedent: Docker is the
higher-consequence capability and it is refuse-by-default with a
ledgered opt-out, so a browser gets the same shape and not a weaker one.

# The port (#672, EPIC I / #662)

Was scripts/e2e/run-tests-repo-gate.sh, 204 lines of bash. That path
still exists and still runs this: it is a four-line exec shim, and it is a
FILE rather than a symlink because scripts/tests/ci-local-gate.test.sh
fabricates a stand-in at that literal path inside a sandbox tree and
writes `exit 3` into it. scripts/ci-local.sh therefore still invokes the
shim rather than this file directly -- calling Python from the gate would
leave that fabricated stub unexecuted and turn six of Group G's
assertions green against a step they no longer reach.

Every refusal below says what it said in bash. The two differences an
operator can see are named where they happen: steps print through
harness.step, which puts a blank line above each one, and a refusal's
"choose the skip out loud" coda follows the details without a blank line
between.
"""

from __future__ import annotations

import os
import sys
import time
from pathlib import Path
from typing import NoReturn

# scripts/, so `bdtools` is importable from a run started anywhere. The
# entry-point shape harness.py documents; parents[2] of
# scripts/bdtools/e2e/x.py is scripts/.
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import harness
from bdtools.e2e import tests_pin

PROGRAM = "e2e gate"

# Appended to every refusal this script makes, which is why it is here
# rather than repeated at eleven call sites. `refuse()` below is a wrapper
# around harness.die and deliberately not a second `die`: the taxonomy,
# the prefix and the exit status stay the harness's.
SKIP_CODA = (
    "Fix it, or choose the skip out loud with CI_LOCAL_SKIP_E2E=1. A run that",
    "skips it ends INCOMPLETE and is not merge evidence.",
)


def refuse(message: str, *details: str) -> NoReturn:
    """harness.die, plus the two lines every refusal here ends with."""
    harness.die(message, *details, *SKIP_CODA)


def read_the_pin(root: Path) -> dict[str, str]:
    """The pinned tests repository, or a refusal naming what is wrong.

    The bash read this with `. "$pin_file"`, so an environment variable of
    the same name was visible to `${TESTS_REPO_SHA:-}` underneath it. That
    is gone on purpose and it is the one deliberate narrowing in this
    file: an exported TESTS_REPO_SHA could decide which tests a commit was
    gated against without the pin changing, which is the whole thing the
    pin exists to stop. The pin file is now the only source.
    """
    pin_file = root / tests_pin.PIN_RELATIVE
    if not pin_file.is_file():
        refuse(f"there is no {tests_pin.PIN_RELATIVE}, so this gate does not know which tests to run.")

    values = tests_pin.read_pin(pin_file)
    url = values.get("TESTS_REPO_URL", "")
    sha = values.get("TESTS_REPO_SHA", "")

    if not tests_pin.SHA.match(sha):
        refuse(
            f"{tests_pin.PIN_RELATIVE} does not carry a full 40-character commit sha "
            f"(TESTS_REPO_SHA={sha or 'unset'}).",
            "A short sha or a branch name would let the tests under this gate change without the pin changing,",
            "which is the whole thing the pin exists to stop.",
        )
    if not url:
        refuse(f"{tests_pin.PIN_RELATIVE} does not carry TESTS_REPO_URL.")
    return {"url": url, "sha": sha}



def require_the_tools() -> None:
    """git, go, node and npm, refused as a FAILURE rather than as a verdict.

    Deliberately not harness.require_tools, which refuses with
    `cannot_run`: that would turn a machine with no Go toolchain into an
    INCOMPLETE the gate ledgers as a skip, and the bash this replaces
    called `die`. A toolchain is not an optional capability here, it is
    the thing every commit needs, and #197's answer was that only the
    browser gets the ledgered opt-out.
    """
    for tool in ("git", "go", "node", "npm"):
        if not harness.have_tool(tool):
            refuse(f"{tool} is not on PATH, and this gate needs it.")


def pinned_checkout(pin: dict[str, str]) -> Path:
    """The pinned tests checkout, cloned once per pin.

    Keyed by sha, so a populated directory is immutable and two concurrent
    gate runs (this machine carries ~50 worktrees of this repository) never
    fight over one working tree. The clone and its npm install both happen in
    a scratch sibling that is renamed into place only once it is complete, so
    a half-finished or interrupted attempt can never be mistaken for a good
    checkout.

    A losing or failed scratch directory is left where it is rather than
    deleted. This path is outside any workspace directory, and leaking one
    directory under a cache is a far cheaper mistake than a recursive delete
    there.
    """
    cache_home = os.environ.get("XDG_CACHE_HOME") or str(Path.home() / ".cache")
    cache_root = Path(cache_home) / "backupd-tests-gate"
    checkout = cache_root / pin["sha"]

    if (checkout / ".complete").is_file():
        return checkout

    harness.step(f"fetching backupd-tests at {pin['sha'][:12]}")
    cache_root.mkdir(parents=True, exist_ok=True)
    scratch = cache_root / f"scratch.{os.getpid()}.{int(time.time())}"
    scratch.mkdir(parents=True, exist_ok=True)

    # The bash ran these four in a subshell and guarded the whole subshell
    # with `|| die`. One try around the four is that same guard: any of
    # them failing is one refusal naming the scratch directory, and none
    # of them failing silently. capture=False for the same reason the bash
    # left git's stderr alone: the refusal below says what to do about a
    # failed fetch, and git says what went wrong, and both are needed.
    try:
        harness.sh(["git", "init", "-q", "."], capture=False, cwd=scratch)
        harness.sh(["git", "remote", "add", "origin", pin["url"]], capture=False, cwd=scratch)
        harness.sh(
            ["git", "fetch", "-q", "--depth", "1", "origin", pin["sha"]],
            capture=False,
            cwd=scratch,
        )
        harness.sh(["git", "checkout", "-q", "--detach", "FETCH_HEAD"], capture=False, cwd=scratch)
    except harness.CommandFailed:
        refuse(
            f"could not fetch {pin['sha']} from {pin['url']}.",
            f"Scratch directory left at {scratch} for inspection.",
            "If the sha is on an unpushed branch, push it before pinning it.",
        )

    harness.step("installing the browser suite's dependencies (once per pin)")
    # `npm ci --no-audit --no-fund >/dev/null` in the bash: stdout dropped,
    # stderr live. Captured instead of dropped, and quoted on failure --
    # the refusal used to name the directory and leave the operator to
    # rerun npm by hand to find out why.
    installed = harness.sh(
        ["npm", "ci", "--no-audit", "--no-fund"],
        check=False,
        cwd=scratch / "suites" / "web-ui",
    )
    if installed.returncode != 0:
        refuse(
            f"npm ci failed in the pinned tests checkout at {scratch}/suites/web-ui.",
            *(installed.stderr or "").strip().splitlines()[-10:],
        )

    (scratch / ".complete").write_text("", encoding="utf-8")
    # Losing this race is not an error: the winner published the same sha.
    try:
        os.rename(str(scratch), str(checkout))
    except OSError:
        pass
    if not (checkout / ".complete").is_file():
        refuse(
            f"could not publish the pinned tests checkout to {checkout}.",
            f"Scratch directory left at {scratch}.",
        )
    return checkout


def build_under_test(root: Path) -> dict[str, str]:
    """backupd, built from THIS working tree.

    The tests repository refuses to run against a build that will not say which
    commit it is, so the -ldflags here are load-bearing rather than cosmetic:
    without them the binary reports "commit none" and the identity handshake
    aborts the run. That is the handshake doing its job, not a problem to work
    around.

    Under a pre-commit hook HEAD is the parent commit and the tree carries the
    staged change, so the build genuinely is HEAD plus something. It says so
    with a -dirty suffix, which the handshake tolerates against a clean pin.
    """
    work = root / ".e2e-gate"
    work.mkdir(parents=True, exist_ok=True)
    head_sha = harness.sh_out(["git", "rev-parse", "HEAD"])
    build_commit = head_sha
    # Both `git diff` calls are guarded, as they were in bash, where a
    # bare `!` on them was the only thing keeping `set -e` from ending the
    # run on a tree that simply had changes in it.
    unstaged = harness.sh(["git", "diff", "--quiet", "HEAD"], check=False).returncode
    staged = harness.sh(["git", "diff", "--cached", "--quiet"], check=False).returncode
    if unstaged != 0 or staged != 0:
        build_commit = head_sha + "-dirty"

    harness.step("building backupd from this working tree")
    # The short sha comes out of its own checked call rather than out of a
    # command substitution inside the argument. In bash a failing `$(git
    # rev-parse --short HEAD)` there left the -ldflags with an empty
    # version and built anyway.
    short_sha = harness.sh_out(["git", "rev-parse", "--short", "HEAD"])
    harness.sh(
        [
            "go",
            "build",
            "-ldflags",
            f"-X main.version={short_sha} -X main.commit={build_commit}",
            "-o",
            str(work / "backupd"),
            "./cmd/backupd",
        ],
        capture=False,
        cwd=root / "core",
        env=dict(os.environ, GOWORK="off"),
    )
    return {"binary": str(work / "backupd"), "commit": head_sha}


def smoke_slice(root: Path, checkout: Path, build: dict[str, str]) -> None:
    harness.step("Suite A smoke slice, against that binary")
    harness.sh(
        ["make", "-C", str(checkout), "smoke"],
        capture=False,
        env=dict(
            os.environ,
            RM_MODE="local",
            RM_BINARY=build["binary"],
            RM_COMMIT=build["commit"],
            RM_SOURCE_DIR=str(root),
        ),
    )


def browser_half(root: Path, checkout: Path) -> None:
    web_ui = checkout / "suites" / "web-ui"

    # The suite's own unit test comes with it, and needs nothing this
    # rewrite touches: no browser, no rig, just node and the checkout.
    harness.step("the browser suite's own unit tests")
    harness.sh(["npm", "run", "--silent", "unit"], capture=False, cwd=web_ui)

    # #687: this used to start ui/shared's own Vite dev server over
    # createMockApi and drive it through RM_UI_DIR, so a case's pass or
    # fail was a claim about a component rendering given a fixture and
    # not about the path an operator meets (browser -> serve-ui ->
    # reverse proxy -> serve -> SQLite). The tests repository retired
    # RM_UI_DIR along with that suite (backupd-tests#65) in
    # favour of RM_BASE_URL against a real deployment, and
    # scripts/e2e/three-machine-web-ui.sh is that deployment: three
    # private Docker networks, the product's own two containers built
    # from this working tree, a real sshd standing in for the machine
    # being backed up, and a client container carrying the browser and
    # the Playwright runner together. #197's host-Chromium probe and the
    # free-port picking above it are both gone with the Vite server they
    # served: the browser now lives inside a container this step builds,
    # not on this machine, and RM_BASE_URL is a container name the rig
    # hands the suite rather than a port this process has to pick.
    #
    # The rig's own exit code carries the same three-outcome vocabulary
    # this harness uses: 0 passed, 3 is CANNOT RUN (a capability this
    # machine does not have, most often no reachable Docker daemon),
    # anything else failed. harness.finish's rule is that an unguarded 3
    # is reported as a failure rather than borrowed as this gate's own
    # verdict (see the PORTED-CHECK HAZARD NOTE below), so it is caught
    # here and turned into a named refusal instead of the generic
    # "exited 3" message, which would have been true but would not have
    # said what to fix.
    harness.step(
        "Suite B browser suite, against a real deployment built from this working tree, over the wire"
    )
    rig = root / "scripts" / "e2e" / "three-machine-web-ui.sh"
    try:
        harness.sh(["bash", str(rig), "--suite", str(web_ui)], capture=False)
    except harness.CommandFailed as failed:
        if failed.status == 3:
            refuse(
                "three-machine-web-ui.sh could not perform the proof on this machine (exit 3).",
                "Its own output above names the missing capability, most likely Docker.",
            )
        raise


def body(root: Path) -> int:
    pin = read_the_pin(root)
    require_the_tools()
    checkout = pinned_checkout(pin)
    build = build_under_test(root)
    smoke_slice(root, checkout, build)
    browser_half(root, checkout)
    return harness.EXIT_OK


def main() -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    root = harness.repo_root(Path(__file__))
    # The bash cd'd to the repository root and then used relative paths
    # (core, ui/shared, scripts/e2e/tests-repo.pin) throughout. Kept, so
    # that `go build ./cmd/backupd` and every path in a diagnostic
    # read the same as they did. Arguments are ignored, as they were: this
    # gate takes none and the shim passes "$@" through.
    os.chdir(root)
    return harness.finish(lambda: body(root))


if __name__ == "__main__":
    sys.exit(main())


# PORTED-CHECK HAZARD NOTE
#
# Every `set -e`, `pipefail`, `trap`, subshell status and `$(...)` in
# scripts/e2e/run-tests-repo-gate.sh, one row each. The rule is
# harness.py's: a port can silently convert a check into one that cannot
# fail, and a check that cannot fail is worse than a deleted one because
# nothing says it stopped watching.
#
# the pin file exists, carries a 40-character sha, and carries a URL
#   hazard in bash:   a missing, short or branch-named pin lets the tests
#                     under this gate change without the pin changing.
#   hazard in python: STILL EXISTS. Three separate refusals over data read
#                     from the file; nothing about the port makes the file
#                     correct.
#   held by:          read_the_pin's three `refuse` calls, and
#                     scripts/tests/ci-local-gate.test.sh G5/G6, which
#                     scan the real pin and a deliberately bad copy.
#
# the pin is read rather than executed
#   hazard in bash:   `. "$pin_file"` runs the pin as shell, and leaves an
#                     exported TESTS_REPO_SHA visible to the `${...:-}`
#                     defaults underneath it, so the environment could
#                     decide which tests a commit was gated against.
#   hazard in python: GONE, replaced by tests_pin.read_pin, which matches
#                     KEY=VALUE and ignores everything else. The narrowing
#                     is deliberate and is named in read_the_pin's
#                     docstring.
#   held by:          tests_pin.read_pin being the only reader; an
#                     environment variable now cannot reach the sha at
#                     all, so there is no path left to watch.
#
# git, go, node and npm are on PATH
#   hazard in bash:   a machine missing one of them fails somewhere deep
#                     instead of in the first second.
#   hazard in python: STILL EXISTS, and the VERDICT is the hazard here:
#                     harness.require_tools refuses with `cannot_run`,
#                     which ci-local.sh ledgers as INCOMPLETE. Using it
#                     would have turned a missing Go toolchain into a
#                     ledgered skip.
#   held by:          require_the_tools calling `refuse` (harness.die,
#                     exit 1) and saying in its docstring why the
#                     harness's own helper is the wrong one here.
#
# the pinned checkout is complete before it is used
#   hazard in bash:   an interrupted clone or npm install leaves a
#                     directory that looks like a checkout; the `.complete`
#                     marker plus publish-by-rename is what stops that.
#   hazard in python: STILL EXISTS. Same two-phase scheme, same marker.
#   held by:          pinned_checkout's `.complete` test on entry, the
#                     rename, and the refusal if `.complete` is absent
#                     after it.
#
# a failed clone refuses rather than continuing
#   hazard in bash:   `( cd ...; git init; git fetch; ... ) || die` -- the
#                     subshell's status is the last command's, so a failed
#                     `git init` would have been masked by a later
#                     success. It could not happen in that order, but the
#                     shape is the hazard.
#   hazard in python: STILL EXISTS as a raised CommandFailed on the FIRST
#                     failing call, which is stricter than the subshell
#                     was: no later success can mask an earlier failure.
#   held by:          the four `harness.sh(check=True)` calls inside one
#                     `try`, and `refuse` in the handler.
#
# npm ci's failure is not mistaken for success
#   hazard in bash:   `npm ci ... >/dev/null || die` dropped stdout and
#                     guarded the status.
#   hazard in python: STILL EXISTS: check=False and an explicit
#                     `.returncode` read, which is the explicit form of
#                     the same guard.
#   held by:          pinned_checkout's returncode branch, which now also
#                     quotes npm's last ten stderr lines.
#
# the publish race is tolerated, and only the race
#   hazard in bash:   `mv "$scratch" "$checkout" 2>/dev/null || true`
#                     swallows every failure, not just a lost race.
#   hazard in python: STILL EXISTS: `except OSError: pass` swallows the
#                     same set.
#   held by:          the `.complete` check immediately after it, which is
#                     what makes the swallow safe: a lost race leaves the
#                     winner's marker in place and anything else has no
#                     marker at all.
#
# the build under test is THIS working tree, stamped with its own commit
#   hazard in bash:   `$(git rev-parse --short HEAD)` sat inside the
#                     -ldflags argument, where `set -e` does not reach: a
#                     failing rev-parse left an EMPTY version and built
#                     anyway, and an empty version is exactly the "commit
#                     none" the tests repository's handshake aborts on --
#                     except that only main.commit is checked, so the
#                     empty one would have gone unnoticed.
#   hazard in python: GONE, replaced by a checked `harness.sh_out` on its
#                     own line: an unguarded failure raises CommandFailed
#                     and ends the run before `go build` is reached.
#   held by:          build_under_test's two sh_out calls (check defaults
#                     to True) and finish() translating CommandFailed.
#
# the -dirty suffix survives a tree with changes in it
#   hazard in bash:   `! git diff --quiet HEAD` -- an unguarded `git diff`
#                     under `set -e` would have ended the run on any dirty
#                     tree, which is every pre-commit run.
#   hazard in python: STILL EXISTS in the sense that matters: the two
#                     calls are check=False and their statuses are READ,
#                     so a tree with changes still produces `-dirty`
#                     rather than an exception.
#   held by:          build_under_test's two check=False calls and the
#                     `!= 0` branch; measured after the port by building
#                     in a dirty tree and reading `backupd version`.
#
# a red suite refuses the commit
#   hazard in bash:   `make -C ... smoke` and `npm run e2e` were
#                     unguarded, so `set -e` made their status the
#                     script's.
#   hazard in python: STILL EXISTS. harness.sh(check=True) raises
#                     CommandFailed and finish() returns the command's own
#                     status, which is the deliberate restoration of
#                     `set -e`. Proven red-able after the port with a
#                     stand-in `go` on PATH exiting 7: the gate exits 7.
#   held by:          finish()'s CommandFailed branch, and
#                     scripts/tests/ci-local-gate.test.sh G4, which
#                     replaces the shim with a failing one and requires
#                     'ci-local: FAILED (browser e2e + CLI smoke'.
#
# a subprocess's own exit 3 does not become the gate's INCOMPLETE
#   hazard in bash:   THE hazard in this file, and it was live. `make
#                     smoke`, `npm run e2e` or the CLI under them exiting
#                     3 (#551: another process is already serving this
#                     deployment) propagated through `set -e` as the
#                     script's status, and 3 is exactly what
#                     scripts/lib/ci-local-gate.sh means by INCOMPLETE.
#                     .husky/pre-commit tolerates an INCOMPLETE run out
#                     loud, so a red suite would have been allowed to
#                     commit as a run that could not look.
#   hazard in python: GONE, replaced by finish()'s translation: a 3
#                     arriving from a command becomes 1 under "A command
#                     exited 3", and only harness.cannot_run reaches 3.
#   held by:          harness.finish, and after the port a stand-in `go`
#                     exiting 3, which makes this gate exit 1.
#
# #687: the browser suite is a real deployment now, not this process's
# own Vite dev server, so the free-port probe, the RM_UI_DIR node_modules
# preflight and the host-Chromium probe above them are gone rather than
# ported: nothing here binds a port, starts a dev server or launches a
# browser on THIS machine any more, so there was nothing left in any of
# the three to translate. What replaced them is one new hazard.
#
# three-machine-web-ui.sh's own exit 3 does not become this gate's
# INCOMPLETE
#   hazard in bash:   the rig's `finish()` translates its internal
#                     EXIT_CANNOT_RUN into exit 3, the same number
#                     scripts/lib/ci-local-gate.sh reads as INCOMPLETE,
#                     and the old bash gate ran it under `set -e`: an
#                     unguarded 3 would have propagated as this script's
#                     own status and been ledgered as a skip rather than
#                     reported as the failure a "cannot run" verdict
#                     borrowed from a subprocess actually is.
#   hazard in python: the same shape, one level up: harness.sh(check=True)
#                     raises CommandFailed on the rig's exit 3 exactly as
#                     it would on any other nonzero, and finish()'s own
#                     rule (see the row above) reports an untranslated 3
#                     as a generic "exited 3" failure -- true, but naming
#                     nothing an operator could fix.
#   held by:          browser_half's own try/except CommandFailed,
#                     which catches status == 3 before it reaches
#                     finish() and turns it into a named refusal quoting
#                     the rig's own output; proven by requiring
#                     `three-machine-web-ui.sh could not perform the
#                     proof` on this machine and a plain exit 1, not 3
#                     nor the generic message, and re-verified after this
#                     port by two full runs against real Docker on this
#                     machine, both `191 passed, 69 skipped, 8 expected
#                     failures, exit 0`.
#
# pipefail
#   hazard in bash:   `set -o pipefail` was on, so a pipeline's first
#                     stage could not be masked by its last.
#   hazard in python: not applicable: this script has no pipeline. Nothing
#                     was carried across because there was nothing to
#                     carry, and no call here builds one with shell=True.
#   held by:          harness.sh taking an argv list and never a shell
#                     string, so a pipeline cannot be introduced without
#                     writing one on purpose.
#
# trap
#   hazard in bash:   there was none. This script creates one thing that
#                     outlives it, .e2e-gate/, which is gitignored on
#                     purpose (see .gitignore) and reused rather than
#                     cleaned.
#   hazard in python: not applicable. finish() is called with no teardown,
#                     which is the same decision written down.
#   held by:          the .gitignore entry naming this script.
