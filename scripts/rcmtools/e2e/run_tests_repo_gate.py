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
     rbm built from THIS working tree;
  2. the browser suite against THIS working tree's ui/shared.

Both come from spdrman/rclone-manager-tests at the sha in tests-repo.pin,
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
inventing a new one: a missing browser is a hard failure that names the
command that fixes it, and CI_LOCAL_SKIP_E2E=1 is the out-loud opt-out
that ledgers the skip in ci-local.sh so the run ends INCOMPLETE and cannot
be merge evidence. That answers #197's first open question with this
repository's own precedent: Docker is the higher-consequence capability
and it is refuse-by-default with a ledgered opt-out, so a browser gets the
same shape and not a weaker one.

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

# scripts/, so `rcmtools` is importable from a run started anywhere. The
# entry-point shape harness.py documents; parents[2] of
# scripts/rcmtools/e2e/x.py is scripts/.
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import harness
from rcmtools.e2e import tests_pin

PROGRAM = "e2e gate"

# Appended to every refusal this script makes, which is why it is here
# rather than repeated at eleven call sites. `refuse()` below is a wrapper
# around harness.die and deliberately not a second `die`: the taxonomy,
# the prefix and the exit status stay the harness's.
SKIP_CODA = (
    "Fix it, or choose the skip out loud with CI_LOCAL_SKIP_E2E=1. A run that",
    "skips it ends INCOMPLETE and is not merge evidence.",
)

# The browser probe, byte for byte what the bash ran. It has to run from
# inside the pinned checkout's suites/web-ui so `require` resolves
# playwright-core out of THAT node_modules rather than out of anything
# this repository happens to have installed.
BROWSER_PROBE = """
const { chromium } = require("playwright-core");
require("node:fs").accessSync(chromium.executablePath());
"""

# One free port, obtained the way the bash obtained it: from node, in the
# runtime that is about to bind it. Kept as node rather than rewritten
# with Python's socket module because the guard below ("printed nothing
# and exited 0") is only a check while something can still do that.
FREE_PORT_PROBE = """
const { createServer } = require("node:net");
const s = createServer();
s.on("error", () => process.exit(1));
s.listen({ host: "127.0.0.1", port: 0, exclusive: true }, () => {
  const port = s.address().port;
  s.close(() => process.stdout.write(String(port)));
});
"""


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
    cache_root = Path(cache_home) / "rclone-manager-tests-gate"
    checkout = cache_root / pin["sha"]

    if (checkout / ".complete").is_file():
        return checkout

    harness.step(f"fetching rclone-manager-tests at {pin['sha'][:12]}")
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
    """rbm, built from THIS working tree.

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

    harness.step("building rbm from this working tree")
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
            str(work / "rbm"),
            "./cmd/backup-manager",
        ],
        capture=False,
        cwd=root / "core",
        env=dict(os.environ, GOWORK="off"),
    )
    return {"binary": str(work / "rbm"), "commit": head_sha}


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

    # ui/shared has to be installed, which ci-local.sh's own preflight already
    # refuses without, so reaching here with it missing means this script was run
    # standalone. Say so rather than letting `npm run dev` fail sixty seconds
    # later inside a webServer timeout.
    if not (root / "ui" / "shared" / "node_modules").is_dir():
        refuse(
            "ui/shared has no installed dependencies, so its dev server cannot start.",
            "Fix it with: cd ui/shared && npm ci",
        )

    # A browser this machine does not have is the one capability question #197
    # left open. Refuse, name the fix, and let ci-local.sh ledger the opt-out.
    #
    # harness.sh_ok would be the shape for this and takes no cwd, and the
    # cwd is the whole point: the probe has to resolve playwright-core out
    # of the pinned checkout. check=False plus a returncode read is the
    # explicit form of the same tolerance.
    probed = harness.sh(["node", "-e", BROWSER_PROBE], check=False, cwd=web_ui)
    if probed.returncode != 0:
        refuse(
            "Playwright has no installed Chromium on this machine, so the browser suite cannot run.",
            f"Fix it with: cd {web_ui} && npx playwright install chromium",
        )

    # The suite's own unit test of its port helper comes with it. In the old
    # home ui/shared's vitest ran it; nothing else does now, and it is the
    # thing that stops an E2E_PORT typo becoming port 0 or NaN.
    harness.step("the browser suite's own unit tests")
    harness.sh(["npm", "run", "--silent", "unit"], capture=False, cwd=web_ui)

    # One port, chosen here, and handed to the suite through E2E_PORT.
    #
    # Not decoration, and not the same thing as letting the suite derive its
    # own. Playwright re-evaluates playwright.config.ts inside every worker
    # process, so anything the config COMPUTES has to come out the same in the
    # runner and in each worker. The suite's default derivation probes for a
    # free port, and by the time a worker probes, the runner's own Vite is
    # already holding the one the runner picked, so the worker can walk to the
    # next slot and end up with a baseURL nothing is listening on. That is
    # exactly what happened on the first full gate run here: the runner said
    # 5930 and one worker navigated to 5931 and got ERR_CONNECTION_REFUSED, one
    # test out of 165.
    #
    # E2E_PORT is read from the environment rather than computed, so the runner
    # and every worker read the same number. The residual race (something else
    # grabs the port between this probe and Vite's bind) is loud rather than
    # silent: --strictPort makes Vite refuse to slide, and reuseExistingServer
    # is false, so a lost race fails to start instead of testing somebody
    # else's server.
    e2e_port = harness.sh_out(["node", "-e", FREE_PORT_PROBE])
    if not e2e_port:
        refuse("could not obtain a free port for the browser suite.")

    harness.step(
        f"Suite B browser suite on port {e2e_port}, against this working tree's ui/shared"
    )
    harness.sh(
        ["npm", "run", "--silent", "e2e"],
        capture=False,
        cwd=web_ui,
        env=dict(
            os.environ,
            RM_UI_DIR=str(root / "ui" / "shared"),
            E2E_PORT=e2e_port,
        ),
    )


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
    # that `go build ./cmd/backup-manager` and every path in a diagnostic
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
#                     in a dirty tree and reading `rbm version`.
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
# node's free-port probe is a port and not an empty string
#   hazard in bash:   `e2e_port="$(node -e ...)"` -- an assignment IS the
#                     command, so `set -e` caught node exiting 1, and the
#                     `[ -n ... ]` guard caught the other case: exit 0
#                     with nothing printed.
#   hazard in python: STILL EXISTS, both halves. sh_out's default
#                     check=True raises on a non-zero node, and the
#                     `if not e2e_port` refusal catches a silent success.
#                     The probe stays a node program for this reason: a
#                     Python socket bind cannot return an empty port, so
#                     rewriting it would have made the second half of this
#                     check unable to fail.
#   held by:          sh_out(check=True) and browser_half's `if not
#                     e2e_port` refusal.
#
# a machine with no browser is refused with the fix named
#   hazard in bash:   a missing Chromium otherwise surfaces as a
#                     Playwright timeout sixty seconds in, with no remedy.
#   hazard in python: STILL EXISTS. Same probe, same message, same exit 1
#                     -- and it stays exit 1 rather than becoming
#                     cannot_run, because the ledgered opt-out for this is
#                     CI_LOCAL_SKIP_E2E=1 in ci-local.sh and #197's answer
#                     was one opt-out, chosen out loud, not two.
#   held by:          browser_half's probe branch; proven after the port by
#                     making the capability absent (a stand-in `node` that
#                     fails the probe) rather than by reading the code.
#
# ui/shared is installed before its dev server is asked for
#   hazard in bash:   without it `npm run dev` fails inside a webServer
#                     timeout a minute later.
#   hazard in python: STILL EXISTS: same directory test, same refusal.
#   held by:          browser_half's node_modules branch.
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
