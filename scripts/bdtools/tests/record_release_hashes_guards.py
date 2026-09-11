#!/usr/bin/env python3
"""Issue #174, PR #182 review M1: an automated control for the refusals in
scripts/release/record-release-hashes.sh.

Those refusals are the half of #174's fix that keeps the net empty. The
net itself (distribution/packaging's release-manifest checks) is tested
exhaustively; the generator was exercised once by hand at release time,
after two Docker cross-builds, and its result was recorded in a pull
request description. Two of its refusals have no downstream net at all:
a manifest generated from a dirty tree, or with COMMIT hand-set, pins a
perfectly reachable commit whose binary_sha256 values describe
different bytes, and nothing downstream can tell.

So this drives the real script (unchanged, still bash -- it is not this
port's domain), in throwaway `git init` repositories, one per refusal,
asserting exit code 2 AND the distinct message. The exit code alone
proves nothing: all five refusals exit 2, so an exit-code-only assertion
cannot tell "the tree is dirty" from "git could not decide", which is
exactly the confusion M4 was filed about.

The script stops at the GUARDS_ONLY=1 seam, immediately after the guard
block and before the first Docker build, so this runs in about a second
on any machine and needs no Docker daemon.

Positive controls, because every assertion here is a negative one:

  * a clean checkout at a commit on the reachable ref must pass all
    five guards and reach the seam, or these tests would pass just as
    happily against a script that refuses everything;
  * the two ancestry branches are driven through the same `git` stub at
    two different exit codes, 1 and 128, and each must produce the
    other's message and not its own. A stub that was not on PATH, or a
    script that treated every non-zero exit the same, fails that pair.

Ported from `scripts/tests/record-release-hashes-guards.test.sh` under
EPIC I (#672 / #697). That file now execs this one.

# PORTED-CHECK HAZARD NOTE

the M4 pair: git exiting 128 must read as undecidable, never as a plain no
  hazard in bash:   `env GUARDS_ONLY=1 "$@" bash "$SCRIPT"` splices
                    caller-supplied `KEY=VALUE` strings into `env`'s own
                    argument list; a case that got the ORDER wrong (PATH
                    set before GUARDS_ONLY, say) would silently stop
                    stubbing `git` and both branches of the pair would
                    read the real git's exit code instead of the fixture's.
  hazard in python: GONE as an argv-splicing risk: `run_guards` below
                    builds one `dict[str, str]` and passes it as
                    `subprocess.run(..., env=...)`, so there is no argument
                    order for a PATH override to lose to -- the stub's PATH
                    entry and GUARDS_ONLY are two keys in the same mapping,
                    applied together.
  held by:          the "git exiting 128" case (expects "could not decide",
                    refutes "is not an ancestor of") and its control at
                    exit 1 (the opposite pair), both proven red-able below.

expect() checks the exit code before the message, so a wrong exit code is
never misread as the right refusal for the wrong reason
  hazard in bash:   `expect` returns early on a code mismatch without
                    checking `$out` at all -- if it checked the message
                    first, a script that refused for an unrelated reason
                    but happened to print an unrelated substring could
                    pass.
  hazard in python: STILL EXISTS, unchanged: `expect()` below returns
                    before ever testing the message when the code differs.
  held by:          every one of the eight `expect()` calls sharing the
                    one function.

the positive control (a clean checkout passes every guard) runs first
  hazard in bash:   without it, every refusal assertion below could pass
                    against a script that refuses unconditionally, and
                    nothing here would have noticed.
  hazard in python: STILL EXISTS, in the same shape: `run_guards` on a
                    freshly built `new_repo()` with no mutation is the
                    first case in `main()`, unchanged in position.
  held by:          that case's own `expect(..., "every guard passed")`.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

SCRIPTS_DIR = Path(__file__).resolve().parents[2]
REPO_ROOT = SCRIPTS_DIR.parent
SCRIPT = REPO_ROOT / "scripts" / "release" / "record-release-hashes.sh"
REAL_GIT = shutil.which("git") or "git"
WELL_FORMED_UNKNOWN_SHA = "0123456789abcdef0123456789abcdef01234567"

# git sets these when this runs from the pre-commit hook, and a relative
# GIT_INDEX_FILE resolved inside a throwaway repository is how you get
# "index file open failed". ci-local.sh already unsets them; do it again
# so this is safe to run standalone.
_UNSET_GIT_VARS = (
    "GIT_INDEX_FILE",
    "GIT_DIR",
    "GIT_WORK_TREE",
    "GIT_OBJECT_DIRECTORY",
    "GIT_COMMON_DIR",
    "GIT_PREFIX",
)


def git(repo: Path, *args: str, check: bool = True) -> str:
    env = dict(os.environ)
    for name in _UNSET_GIT_VARS:
        env.pop(name, None)
    proc = subprocess.run(
        ["git", "-C", str(repo), *args], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True
    )
    if check and proc.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)} failed: {proc.stdout}")
    return proc.stdout


def new_repo(tmpdirs: list[str]) -> Path:
    """A throwaway repository holding the paths the guards look at:
    core/, apps/, ui/ and container/Dockerfile."""
    d = Path(tempfile.mkdtemp())
    tmpdirs.append(str(d))
    git(d, "init", "-q", "-b", "main")
    git(d, "config", "user.email", "t@example.invalid")
    git(d, "config", "user.name", "t")
    (d / "core").mkdir()
    (d / "apps").mkdir()
    (d / "ui").mkdir()
    (d / "container").mkdir()
    (d / "core" / "main.go").write_text("package main\n")
    (d / "apps" / "marker").write_text("apps\n")
    (d / "ui" / "marker").write_text("ui\n")
    (d / "container" / "Dockerfile").write_text("FROM scratch\n")
    git(d, "add", "-A")
    git(d, "commit", "-qm", "base")
    return d


def stub_git(tmpdirs: list[str], code: int) -> Path:
    """A `git` on PATH that answers `merge-base --is-ancestor` with a
    fixed exit code and forwards everything else to the real git.

    Exit 1 is git saying no; 128 is git saying it could not decide, which
    is what a shallow clone or a missing object produces and which no
    fixture can create on demand.
    """
    d = Path(tempfile.mkdtemp())
    tmpdirs.append(str(d))
    bindir = d / "stubbin"
    bindir.mkdir()
    (bindir / "git").write_text(
        "#!/usr/bin/env bash\n"
        'for arg in "$@"; do\n'
        '  if [ "$arg" = "--is-ancestor" ]; then\n'
        f'    echo "fatal: Not a valid commit name (stubbed exit {code})" >&2\n'
        f"    exit {code}\n"
        "  fi\n"
        "done\n"
        f'exec "{REAL_GIT}" "$@"\n'
    )
    (bindir / "git").chmod(0o755)
    return bindir


def run_guards(repo: Path, *extra_env: str) -> tuple[int, str]:
    """Run the real script inside `repo` with GUARDS_ONLY=1, returning
    `(exit code, combined output)`. `extra_env` entries are `KEY=VALUE`
    strings, the same shape `env KEY=VALUE ... bash "$SCRIPT"` took."""
    env = dict(os.environ)
    for name in _UNSET_GIT_VARS:
        env.pop(name, None)
    env["GUARDS_ONLY"] = "1"
    for entry in extra_env:
        key, _, value = entry.partition("=")
        env[key] = value
    proc = subprocess.run(
        ["bash", str(SCRIPT)], cwd=str(repo), env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True
    )
    return proc.returncode, proc.stdout


def main() -> int:
    tmpdirs: list[str] = []
    failures = 0
    current = ""

    def fail(message: str, out: str = "") -> None:
        nonlocal failures
        failures += 1
        print(f"FAIL: {current}: {message}", file=sys.stderr, flush=True)
        if out:
            print("--- script output ---", file=sys.stderr, flush=True)
            print(out, file=sys.stderr, flush=True)
            print("---------------------", file=sys.stderr, flush=True)

    def expect(rc: int, out: str, want_rc: int, want: str) -> None:
        if rc != want_rc:
            fail(f"exit {rc}, want {want_rc}", out)
            return
        if want not in out:
            fail(f"output does not contain: {want}", out)

    def refute(out: str, unwanted: str) -> None:
        if unwanted in out:
            fail(f"output contains, and must not: {unwanted}", out)

    try:
        # --- positive control, first, so the refusals below mean something
        current = "a clean checkout at a commit on the reachable ref passes every guard"
        repo = new_repo(tmpdirs)
        rc, out = run_guards(repo, "REACHABLE_FROM=main")
        expect(rc, out, 0, "every guard passed")
        expect(rc, out, 0, "Would write container/release-manifest.json")

        # --- refusal 1: COMMIT names nothing
        current = "COMMIT that names no commit here"
        repo = new_repo(tmpdirs)
        rc, out = run_guards(repo, "REACHABLE_FROM=main", f"COMMIT={WELL_FORMED_UNKNOWN_SHA}")
        expect(rc, out, 2, "does not name a commit in this repository")

        # --- refusal 2: COMMIT is not HEAD
        current = "COMMIT that resolves but is not HEAD"
        repo = new_repo(tmpdirs)
        base = git(repo, "rev-parse", "HEAD").strip()
        with (repo / "core" / "main.go").open("a") as fh:
            fh.write("second\n")
        git(repo, "commit", "-qam", "second")
        rc, out = run_guards(repo, "REACHABLE_FROM=main", f"COMMIT={base}")
        expect(rc, out, 2, "is not HEAD")

        # --- refusal 3: dirty tree
        current = "a working tree dirty in a path the image is built from"
        repo = new_repo(tmpdirs)
        with (repo / "core" / "main.go").open("a") as fh:
            fh.write("uncommitted\n")
        rc, out = run_guards(repo, "REACHABLE_FROM=main")
        expect(rc, out, 2, "the working tree is dirty")

        # --- refusal 4: the reachable ref does not resolve
        current = "REACHABLE_FROM that does not resolve"
        repo = new_repo(tmpdirs)
        rc, out = run_guards(repo)  # the default origin/main does not exist here
        expect(rc, out, 2, "cannot resolve origin/main")

        # --- refusal 5: the commit is not on the reachable ref
        current = "a commit that is not an ancestor of the reachable ref"
        repo = new_repo(tmpdirs)
        git(repo, "checkout", "-q", "-b", "feature")
        with (repo / "core" / "main.go").open("a") as fh:
            fh.write("feature\n")
        git(repo, "commit", "-qam", "feature work")
        rc, out = run_guards(repo, "REACHABLE_FROM=main")
        expect(rc, out, 2, "is not an ancestor of main")
        refute(out, "could not decide")

        # --- M4: git could not decide is not git saying no
        current = "git exiting 128 is reported as undecidable, not as a no"
        repo = new_repo(tmpdirs)
        stub = stub_git(tmpdirs, 128)
        rc, out = run_guards(repo, "REACHABLE_FROM=main", f"PATH={stub}{os.pathsep}{os.environ.get('PATH', '')}")
        expect(rc, out, 2, "could not decide")
        expect(rc, out, 2, "exited 128")
        refute(out, "is not an ancestor of")

        current = "the same stub at exit 1 still reports a plain no (control for the pair above)"
        repo = new_repo(tmpdirs)
        stub = stub_git(tmpdirs, 1)
        rc, out = run_guards(repo, "REACHABLE_FROM=main", f"PATH={stub}{os.pathsep}{os.environ.get('PATH', '')}")
        expect(rc, out, 2, "is not an ancestor of main")
        refute(out, "could not decide")

        # --- M3: the waiver is loud and does not default to the tracked path
        current = "UNSAFE_LOCAL_BUILD=1 waives the guards but writes somewhere gitignored"
        repo = new_repo(tmpdirs)
        with (repo / "core" / "main.go").open("a") as fh:
            fh.write("uncommitted\n")  # would fail the dirty guard
        rc, out = run_guards(repo, "UNSAFE_LOCAL_BUILD=1", f"COMMIT={WELL_FORMED_UNKNOWN_SHA}")
        expect(rc, out, 0, "Would write container/.generated/release-manifest.local.json")
        expect(rc, out, 0, "unsafe_local_build")
        refute(out, "Would write container/release-manifest.json")

        current = "UNSAFE_LOCAL_BUILD=1 still honours an explicit OUT"
        repo = new_repo(tmpdirs)
        rc, out = run_guards(repo, "UNSAFE_LOCAL_BUILD=1", f"OUT={repo}/explicit.json")
        expect(rc, out, 0, f"Would write {repo}/explicit.json")
    finally:
        for d in tmpdirs:
            shutil.rmtree(d, ignore_errors=True)

    if failures > 0:
        print(f"record-release-hashes guards: {failures} failing assertion(s)", file=sys.stderr, flush=True)
        return 1
    print("record-release-hashes guards: ok", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
