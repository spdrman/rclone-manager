#!/usr/bin/env python3
"""An automated control for the refusals in scripts/release/publish-image.sh.
Issue #88 (B5.2).

The script it drives is the one step in this repository that does
something irreversible: it puts bytes in a public registry under a
semantic version. Every other release artifact can be regenerated after
the fact, and a bad registry tag cannot. So its refusals get a control
for the same reason #174's generator got one in
record_release_hashes_guards.py, only more so: this one runs once, on the
day it matters, and nobody will be watching it work for the first time.

Structure copied from that file on purpose. Throwaway `git init`
repository per refusal, the real script (unchanged, still bash -- it is
not this port's domain), the GUARDS_ONLY=1 seam that stops immediately
after the guard block and before the first Docker command, and an
assertion on the exit code AND the distinct message. Every refusal exits
2, so an exit-code-only assertion cannot tell "the tree is dirty" from
"there is a private key sitting in it", and those call for very
different reactions.

Positive controls, because every assertion here is a negative one:

  * a clean checkout whose HEAD is the commit the manifest records
    reaches the seam, or these tests would pass equally against a script
    that refuses everything;
  * guard 6's two arms are driven through the same `go` stub at exit 0
    and exit 1, so a stub that was not on PATH, or a script that ignored
    the exit status, fails the pair;
  * the workflow scanner at the end is run against a file carrying the
    shape it hunts, so its silence on the real workflow is evidence.

The last section leaves the script and checks
.github/workflows/release.yml, which is the other half of the same
release path and the only other place a refusal on this path can be
written wrong.

Ported from `scripts/tests/publish-image-guards.test.sh` under EPIC I
(#672 / #697). That file now execs this one.

# PORTED-CHECK HAZARD NOTE

guard 5's fixture carries the REAL .gitignore, not a convenient subset
  hazard in bash:   `cp "${REPO_ROOT}/.gitignore" "$dir/.gitignore"`,
                     specifically so the untracked-key arm reproduces the
                     exact bug this suite was written to catch: the
                     shipped .gitignore ignores *.key, and the scan was
                     suppressing ignored files, so a fixture with no
                     .gitignore at all could not have found that.
  hazard in python: STILL EXISTS as the same fixture-fidelity risk:
                     `new_repo` below copies the same real file, and the
                     `check-ignore` positive control that follows it is
                     unchanged in position and wording.
  held by:           the `git check-ignore -q cosign.key` control before
                     the run, which fails loudly if the fixture stops
                     reproducing the shipped exclusions, and is proven
                     red-able below.

expect() checks the exit code before the message, exactly as
record_release_hashes_guards.py's does
  hazard in bash:    a wrong exit code checked after the message risks a
                      script that refused for an unrelated reason passing
                      because an unrelated substring happened to match.
  hazard in python:  STILL EXISTS, unchanged: `expect()` returns before
                      testing the message when the code differs.
  held by:            every `expect()` call in `main()`.

the workflow scanner (expansions_in_run_blocks) is a translation of a
stateful awk program, and state machines are where translations drift
  hazard in bash:    `${{ }}` interpolated into a `run:` body is shell
                      source in the job that mints the release signing
                      identity (id-token: write) -- GitHub expands it
                      textually before bash ever parses the step, so a
                      dispatch input there is a command injection, not
                      data. The awk scanner tracks one YAML `run:` block's
                      indent to know when it has ended.
  hazard in python:  STILL EXISTS as the same indentation-tracking risk;
                      `expansions_in_run_blocks` below is a line-by-line
                      port of the same state machine (in_run/run_indent),
                      not a YAML parse, for the same reason the original
                      was not one: GitHub Actions' own `${{ }}` syntax is
                      not YAML syntax a YAML parser is obliged to
                      preserve as text.
  held by:           the assertion against the real release.yml (must be
                      empty) PLUS its own positive control immediately
                      after, which requires the scanner to flag a
                      constructed `bad.yml` carrying the exact shape
                      release.yml had before the fix -- a scanner that
                      silently stopped matching would pass the real file
                      AND fail its own control, and only the second half
                      says why.
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

SCRIPTS_DIR = Path(__file__).resolve().parents[2]
REPO_ROOT = SCRIPTS_DIR.parent
SCRIPT = REPO_ROOT / "scripts" / "release" / "publish-image.sh"
PARITY_SCRIPT = REPO_ROOT / "scripts" / "release" / "verify-manifest-parity.sh"
WORKFLOW = REPO_ROOT / ".github" / "workflows" / "release.yml"

sys.path.insert(0, str(SCRIPTS_DIR))

from bdtools.release import verify_manifest_parity as parity  # noqa: E402

_UNSET_GIT_VARS = (
    "GIT_INDEX_FILE",
    "GIT_DIR",
    "GIT_WORK_TREE",
    "GIT_OBJECT_DIRECTORY",
    "GIT_COMMON_DIR",
    "GIT_PREFIX",
)


def _clean_env() -> dict[str, str]:
    env = dict(os.environ)
    for name in _UNSET_GIT_VARS:
        env.pop(name, None)
    return env


def git(repo: Path, *args: str, check: bool = True) -> str:
    proc = subprocess.run(
        ["git", "-C", str(repo), *args], env=_clean_env(), stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True
    )
    if check and proc.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)} failed: {proc.stdout}")
    return proc.stdout


def git_ok(repo: Path, *args: str) -> bool:
    proc = subprocess.run(
        ["git", "-C", str(repo), *args], env=_clean_env(), stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True
    )
    return proc.returncode == 0


def new_repo(tmpdirs: list[str]) -> Path:
    """A throwaway repository shaped like this one from the script's point
    of view: the two JSON files it reads, the paths its dirty check looks
    at, the real .gitignore, and a committed HEAD."""
    d = Path(tempfile.mkdtemp())
    tmpdirs.append(str(d))
    git(d, "init", "-q", "-b", "main")
    git(d, "config", "user.email", "t@example.invalid")
    git(d, "config", "user.name", "t")
    shutil.copy(REPO_ROOT / ".gitignore", d / ".gitignore")
    for sub in ("core", "distribution/packaging", "ui", "container", "provenance"):
        (d / sub).mkdir(parents=True)
    (d / "core" / "main.go").write_text("package main\n")
    (d / "ui" / "marker").write_text("ui\n")
    (d / "container" / "Dockerfile").write_text("FROM scratch\n")
    (d / "distribution" / "packaging" / "canonical.json").write_text(
        '{ "image": { "reference": "ghcr.io/backupdproject/backupd:1.0.0", "published": false } }\n'
    )
    (d / "container" / "release-manifest.json").write_text(
        '{ "version": "test", "commit": "0000000000000000000000000000000000000000" }\n'
    )
    (d / "provenance" / "sbom.spdx.json").write_text("{}\n")
    git(d, "add", "-A")
    git(d, "commit", "-qm", "base")
    return d


def pin_manifest_to_head(repo: Path) -> None:
    head = git(repo, "rev-parse", "HEAD").strip()
    (repo / "container" / "release-manifest.json").write_text(f'{{ "version": "test", "commit": "{head}" }}\n')


def stub_go(tmpdirs: list[str], code: int) -> Path:
    """A `go` on PATH that exits with a fixed code, so guard 6's stale
    provenance branch can be driven both ways with no Go toolchain."""
    d = Path(tempfile.mkdtemp())
    tmpdirs.append(str(d))
    bindir = d / ".stubbin"
    bindir.mkdir()
    (bindir / "go").write_text(f"#!/usr/bin/env bash\nexit {code}\n")
    (bindir / "go").chmod(0o755)
    return bindir


def _run(repo: Path, extra_env: list[str], guards_only: bool) -> tuple[int, str]:
    env = _clean_env()
    if guards_only:
        env["GUARDS_ONLY"] = "1"
    else:
        env["DRY_RUN"] = "1"
        env["SIGN"] = "0"
    for entry in extra_env:
        key, _, value = entry.partition("=")
        env[key] = value
    proc = subprocess.run(
        ["bash", str(SCRIPT)], cwd=str(repo), env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True
    )
    return proc.returncode, proc.stdout


def run_guards(repo: Path, *extra_env: str) -> tuple[int, str]:
    """Drive the script through the GUARDS_ONLY seam, which stops after
    the guard block and before the first Docker command."""
    return _run(repo, list(extra_env), guards_only=True)


def run_publish_path(repo: Path, *extra_env: str) -> tuple[int, str]:
    """Drive the script WITHOUT the GUARDS_ONLY seam. DRY_RUN=1 is the
    belt: it stops before `docker buildx build --push`."""
    return _run(repo, list(extra_env), guards_only=False)


def stub_docker(tmpdirs: list[str]) -> Path:
    """A `docker` on PATH that refuses to build, with a marker.

    The parity proof's own refusals all fire before the first `docker`
    call, so they need no Docker at all. Their positive control does: it
    has to show that a well-formed manifest gets PAST them, and the next
    thing past them is a cross-architecture build. A stub that fails
    loudly turns that into a one-second assertion instead of two real
    builds, and the marker is what stops the control passing on a
    `docker` that was never invoked."""
    d = Path(tempfile.mkdtemp())
    tmpdirs.append(str(d))
    bindir = d / ".stubbin"
    bindir.mkdir()
    (bindir / "docker").write_text('#!/usr/bin/env bash\necho "stub docker refused to build" >&2\nexit 1\n')
    (bindir / "docker").chmod(0o755)
    return bindir


def run_parity(repo: Path, *extra_env: str) -> tuple[int, str]:
    """Drive scripts/release/verify-manifest-parity.sh -- the SHIM, so the
    shim's `exec` is exercised too -- against `repo`. It has no
    GUARDS_ONLY seam: its refusals simply precede its first `docker`
    call, which is why they are drivable here and its comparison logic is
    not."""
    env = _clean_env()
    for entry in extra_env:
        key, _, value = entry.partition("=")
        env[key] = value
    proc = subprocess.run(
        ["bash", str(PARITY_SCRIPT)],
        cwd=str(repo),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    return proc.returncode, proc.stdout


def expansions_in_run_blocks(path: Path) -> str:
    """Every `N: text` line inside a YAML `run:` body that carries a
    `${{ }}` expansion. Comment lines do not count; they are not shell
    source. A line-by-line port of the original awk state machine (see
    this module's hazard note)."""
    in_run = False
    run_indent = -1
    found = []
    for lineno, line in enumerate(path.read_text().splitlines(), 1):
        if re.match(r"^\s*#", line):
            continue
        m = re.search(r"[^ \t]", line)
        indent = m.start() if m else -1
        if in_run and re.search(r"[^ \t]", line) and indent <= run_indent:
            in_run = False
        if in_run and "${{" in line:
            found.append(f"{lineno}: {line}")
        if not in_run and re.match(r"^[ \t]*run:[ \t]*[|>]", line):
            in_run = True
            run_indent = indent
    return "\n".join(found)


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
        current = "a clean checkout at the commit the manifest records reaches the seam"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 0, "every guard passed")
        expect(rc, out, 0, "Would publish ghcr.io/backupdproject/backupd:1.0.0")

        # --- guard 1: the files it reads are not there
        current = "canonical.json missing"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        (repo / "distribution" / "packaging" / "canonical.json").unlink()
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 2, "distribution/packaging/canonical.json is not readable")

        current = "canonical.json present but carrying no reference"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        (repo / "distribution" / "packaging" / "canonical.json").write_text('{ "image": { "published": false } }\n')
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 2, "no image reference in")

        # --- guard 2: the manifest has to describe a build this history has
        current = "the manifest pins a SHA that is not an object in this repository"
        repo = new_repo(tmpdirs)  # the fixture's manifest still pins all-zeroes
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 2, "which is not a commit in this repository")

        current = "the manifest pins a real commit that is not an ancestor of HEAD"
        repo = new_repo(tmpdirs)
        git(repo, "checkout", "-q", "-b", "sidebranch")
        (repo / "core" / "main.go").write_text("package main // side\n")
        git(repo, "commit", "-qam", "side")
        side = git(repo, "rev-parse", "HEAD").strip()
        git(repo, "checkout", "-q", "main")
        (repo / "container" / "release-manifest.json").write_text(f'{{ "version": "test", "commit": "{side}" }}\n')
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 2, "which is not an ancestor of HEAD")

        current = "the manifest pins a commit only a feature branch has"
        repo = new_repo(tmpdirs)
        git(repo, "checkout", "-q", "-b", "feature")
        (repo / "core" / "main.go").write_text("package main // feature\n")
        git(repo, "commit", "-qam", "feature work")
        pin_manifest_to_head(repo)
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 2, "not reachable from any rewrite-free ref")
        expect(rc, out, 2, "#174")

        current = "a commit reachable only from origin/release is publishable, and the ref is named"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        head = git(repo, "rev-parse", "HEAD").strip()
        git(repo, "update-ref", "refs/remotes/origin/release", head)
        git(repo, "branch", "-m", "main", "cut")
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 0, "is reachable from origin/release")

        current = "CHECKABLE_FROM names the rewrite-free ref for a fork"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        git(repo, "branch", "-m", "main", "stable")
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1", "CHECKABLE_FROM=stable")
        expect(rc, out, 0, "is reachable from stable")

        current = "a manifest that pins no commit at all"
        repo = new_repo(tmpdirs)
        (repo / "container" / "release-manifest.json").write_text('{ "version": "test" }\n')
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 2, "pins no commit")
        refute(out, "not an ancestor of HEAD")

        # --- guard 3: a waived manifest must never be published
        current = "a manifest stamped unsafe_local_build"
        repo = new_repo(tmpdirs)
        head = git(repo, "rev-parse", "HEAD").strip()
        (repo / "container" / "release-manifest.json").write_text(
            f'{{ "unsafe_local_build": true, "version": "test", "commit": "{head}" }}\n'
        )
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 2, "unsafe_local_build")
        expect(rc, out, 2, "public registry")

        # --- guard 4: a dirty tree is not the release
        current = "a working tree dirty in a path the image is built from"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        with (repo / "core" / "main.go").open("a") as fh:
            fh.write("uncommitted\n")
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 2, "the working tree is dirty")

        # --- guard 5: key material on disk
        current = "an untracked, gitignored private key beside the script"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        (repo / "cosign.key").write_text("not a real key\n")
        if not git_ok(repo, "check-ignore", "-q", "cosign.key"):
            fail("the fixture does not ignore cosign.key, so this arm is not testing the shipped configuration")
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 2, "private key material is present in the working tree")
        expect(rc, out, 2, "cosign.key")

        current = "a gitignored key in a subdirectory, which no unwildcarded pathspec reaches"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        (repo / "secrets").mkdir()
        (repo / "secrets" / "id_ed25519").write_text("not a real key\n")
        (repo / "secrets" / "release.pem").write_text("not a real key\n")
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 2, "private key material is present in the working tree")
        expect(rc, out, 2, "secrets/id_ed25519")
        expect(rc, out, 2, "secrets/release.pem")

        current = "a vendored .pem under node_modules is not treated as key material"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        (repo / "node_modules" / "some-pkg" / "fixtures").mkdir(parents=True)
        (repo / "ui" / "shared" / "dist").mkdir(parents=True)
        (repo / "node_modules" / "some-pkg" / "fixtures" / "test-cert.pem").write_text("not a real key\n")
        (repo / "ui" / "shared" / "dist" / "inlined.pem").write_text("not a real key\n")
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 0, "every guard passed")
        refute(out, "private key material is present in the working tree")

        current = "a tracked .pem is refused too, not only an untracked .key"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        (repo / "release-signing.pem").write_text("not a real key\n")
        git(repo, "add", "-f", "release-signing.pem")
        git(repo, "commit", "-qm", "oops")
        if not git(repo, "ls-files", "--", "release-signing.pem").strip():
            fail("the fixture never tracked release-signing.pem, so this arm is testing the untracked path again")
        pin_manifest_to_head(repo)
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 2, "private key material is present in the working tree")
        expect(rc, out, 2, "release-signing.pem")

        current = "COSIGN_KEY_FILE is refused even with no key on disk"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        rc, out = run_guards(repo, "SKIP_PROVENANCE_CHECK=1", "COSIGN_KEY_FILE=/tmp/nope.key")
        expect(rc, out, 2, "COSIGN_KEY_FILE is set")
        refute(out, "present in the working tree")

        # --- guard 6: the SBOM has to describe this tree
        current = "no SBOM in the tree at all"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        (repo / "provenance" / "sbom.spdx.json").unlink()
        rc, out = run_guards(repo)
        expect(rc, out, 2, "is not in the tree, so there is no SBOM to attest")

        current = "the provenance check failing is a refusal"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        stub = stub_go(tmpdirs, 1)
        rc, out = run_guards(repo, f"PATH={stub}{os.pathsep}{os.environ.get('PATH', '')}")
        expect(rc, out, 2, "are not what this tree generates")

        current = "the same stub at exit 0 reaches the seam (control for the pair above)"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        stub = stub_go(tmpdirs, 0)
        rc, out = run_guards(repo, f"PATH={stub}{os.pathsep}{os.environ.get('PATH', '')}")
        expect(rc, out, 0, "every guard passed")
        refute(out, "are not what this tree generates")

        # --- the SKIP_PROVENANCE_CHECK seam is test-only
        current = "SKIP_PROVENANCE_CHECK on a run that could publish is itself a refusal"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        rc, out = run_publish_path(repo, "SKIP_PROVENANCE_CHECK=1")
        expect(rc, out, 2, "SKIP_PROVENANCE_CHECK=1 removes the check")
        expect(rc, out, 2, "test-only seam")
        refute(out, "stopping before docker buildx build")

        current = "the same run without it reaches the push (control for the refusal above)"
        repo = new_repo(tmpdirs)
        pin_manifest_to_head(repo)
        stub = stub_go(tmpdirs, 0)
        rc, out = run_publish_path(repo, f"PATH={stub}{os.pathsep}{os.environ.get('PATH', '')}")
        expect(rc, out, 0, "stopping before docker buildx build")
        refute(out, "SKIP_PROVENANCE_CHECK=1 removes the check")

        # --- the parity proof publish-image.sh runs before the push
        #
        # scripts/release/verify-manifest-parity.sh (#260) is the last
        # thing that happens before anything leaves this machine, and it
        # is the only release script with no guard suite of its own: it is
        # two full cross-architecture Docker builds, so it is deliberately
        # not wired into ci-local.sh. Its REFUSALS, though, all fire
        # before the first `docker` call, and the module's hazard note
        # claims this file holds them -- so it has to, or that claim is
        # decoration. Everything below is Docker-free except the positive
        # control, which reaches the first build and stops there against a
        # stub `docker`.
        current = "a manifest stamped unsafe_local_build is not worth proving a build against"
        repo = new_repo(tmpdirs)
        (repo / "container" / "release-manifest.json").write_text(
            '{ "unsafe_local_build": true, "version": "test", "commit": "'
            + git(repo, "rev-parse", "HEAD").strip()
            + '", "architectures": [ { "architecture": "amd64", "binary_sha256": { "backupd": "x", "backupd-web": "y" } } ] }\n'
        )
        rc, out = run_parity(repo)
        expect(rc, out, 2, 'stamped "unsafe_local_build": true')
        refute(out, "==> Proving")

        current = "a manifest missing the commit it would build"
        repo = new_repo(tmpdirs)
        (repo / "container" / "release-manifest.json").write_text(
            '{ "version": "test", "commit": "", "architectures": [ { "architecture": "amd64", '
            '"binary_sha256": { "backupd": "x", "backupd-web": "y" } } ] }\n'
        )
        rc, out = run_parity(repo)
        expect(rc, out, 2, "both are needed as build arguments")
        refute(out, "==> Proving")

        current = "a manifest recording no architecture at all, which would pass by having nothing to compare"
        repo = new_repo(tmpdirs)
        (repo / "container" / "release-manifest.json").write_text(
            '{ "version": "test", "commit": "' + git(repo, "rev-parse", "HEAD").strip() + '" }\n'
        )
        rc, out = run_parity(repo)
        expect(rc, out, 2, "records no architecture at all")
        refute(out, "==> Proving")

        current = "no manifest in the tree at all"
        repo = new_repo(tmpdirs)
        (repo / "container" / "release-manifest.json").unlink()
        rc, out = run_parity(repo)
        expect(rc, out, 2, "there is nothing to check the build against")
        refute(out, "==> Proving")

        current = "a well-formed manifest gets past every refusal (control for the four above)"
        repo = new_repo(tmpdirs)
        (repo / "container" / "release-manifest.json").write_text(
            '{ "version": "test", "commit": "' + git(repo, "rev-parse", "HEAD").strip() + '", '
            '"architectures": [ { "architecture": "amd64", "binary_sha256": { "backupd": "x", "backupd-web": "y" } } ] }\n'
        )
        stub = stub_docker(tmpdirs)
        rc, out = run_parity(repo, f"PATH={stub}{os.pathsep}{os.environ.get('PATH', '')}")
        expect(rc, out, 1, "==> Proving")
        expect(rc, out, 1, "stub docker refused to build")
        refute(out, "records no architecture at all")
        refute(out, "both are needed as build arguments")
        refute(out, "there is nothing to check the build against")

        current = "recorded() indexes by architecture and cannot cross-match another one's digest"
        swapped: dict[str, object] = {
            "architectures": [
                {"architecture": "amd64", "binary_sha256": {"backupd": "AAA", "backupd-web": "AAW"}},
                {"architecture": "arm64", "binary_sha256": {"backupd": "BBB", "backupd-web": "BBW"}},
            ]
        }
        got = {
            (arch, binary): parity.recorded(swapped, arch, binary)
            for arch in ("amd64", "arm64")
            for binary in ("backupd", "backupd-web")
        }
        want = {
            ("amd64", "backupd"): "AAA",
            ("amd64", "backupd-web"): "AAW",
            ("arm64", "backupd"): "BBB",
            ("arm64", "backupd-web"): "BBW",
        }
        if got != want:
            fail(f"recorded() crossed architectures or binaries: {got} != {want}")

        current = "recorded() returns None, not an empty string, for a hash the manifest does not carry"
        for arch, binary in (("amd64", "backupd-lite"), ("s390x", "backupd")):
            value = parity.recorded(swapped, arch, binary)
            if value is not None:
                fail(
                    f"recorded({arch!r}, {binary!r}) returned {value!r} rather than None, so an absent "
                    "record is conflatable with a real hash that merely differs, and the MISMATCH line "
                    'would read "manifest records: " with nothing after it'
                )

        # --- the workflow that drives this script
        current = "release.yml never interpolates a dispatch input into a run: body"
        found = expansions_in_run_blocks(WORKFLOW)
        if found:
            fail("an expression is expanded into shell source in the release workflow", found)

        current = "the scanner finds the shape it hunts (control for the arm above)"
        ctl = Path(tempfile.mkdtemp())
        tmpdirs.append(str(ctl))
        bad = ctl / "bad.yml"
        bad.write_text(
            "jobs:\n"
            "  publish:\n"
            "    steps:\n"
            "      - name: Refuse an unconfirmed publish\n"
            "        run: |\n"
            '          tag="1.0.0"\n'
            '          if [ "${{ inputs.confirm }}" != "$tag" ]; then\n'
            "            exit 1\n"
            "          fi\n"
            "      - name: After\n"
            "        uses: actions/checkout@v7\n"
        )
        if not expansions_in_run_blocks(bad):
            fail(
                "the scanner does not flag an input interpolated straight into a run: body, so its "
                "silence on the real workflow means nothing"
            )
    finally:
        for d in tmpdirs:
            shutil.rmtree(d, ignore_errors=True)

    if failures > 0:
        print(f"publish-image guards: {failures} failing assertion(s)", file=sys.stderr, flush=True)
        return 1
    print("publish-image guards: ok", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
