#!/usr/bin/env python3
"""Record `container/release-manifest.json` (issue #82/B4.1, #174).

Ported off `scripts/release/record-release-hashes.sh`.

Builds `container/Dockerfile` for each of linux/amd64 and linux/arm64,
extracts both shipped binaries (`/rbm` and `/rbm-web`) from each built
image, hashes them with SHA-256, and writes the result to
`container/release-manifest.json` -- a record a reviewer (or a future
provider package) can diff against a claim of core parity, rather than
trusting it by assertion.

What this does NOT do: push anything to a registry. It writes
`registry_digest: null` per architecture and a null `index_digest`; the
release workflow's `publish_image` fills those in after a real push.

Where this runs matters as much as what it records. The manifest is only
worth anything while the commit it pins stays in main's history, and a
commit made on a feature branch does not: a squash merge rewrites it,
and the manifest is left pinning a SHA nobody can check out (#174). Run
this from a clean checkout of a commit that is already on main.

The guards are driven, on every non-FAST `ci-local.sh` run, by
`scripts/rcmtools/release/test_guards.py`, which builds a throwaway
repository per refusal and asserts both the exit code and the message,
through the `GUARDS_ONLY=1` seam below. A refusal that is only ever
executed at release time, after two Docker cross-builds, is a refusal
nobody has watched work.

# PORTED-CHECK HAZARD NOTE

Five reproducibility refusals below, one exit(2) apiece in bash. Bash's
hazard was `set -euo pipefail` silently promoting an unrelated
subprocess status into one of these five meanings; Python's is a
subprocess status nobody reads at all -- `subprocess.run`'s return value
is inert unless something inspects `.returncode`.

  COMMIT does not name a commit in this repository
    hazard in bash:   under `set -e`, a transient `git rev-parse` failure
                       (not "unknown revision", something else) would
                       abort the whole script with a raw git error rather
                       than this refusal's text.
    hazard in python: GONE. `_rev_exists` calls with `check=False` and
                       inspects `returncode` itself; nothing swallows it.
    held by:          test_guards.py "COMMIT that names no commit here"

  COMMIT is not HEAD
    hazard in python: STILL EXISTS if ported naively: `subprocess.run`'s
                       exit status is discarded by default, so a broken
                       `git rev-parse HEAD` (corrupt checkout) could
                       silently compare COMMIT against an empty string
                       and either falsely pass or falsely fail depending
                       on COMMIT's own value.
                       GONE here: `_git` uses `harness.sh` with its
                       default `check=True`, so a failing `git rev-parse
                       HEAD` raises `CommandFailed` instead of resolving
                       to an empty string.
    held by:          test_guards.py "COMMIT that resolves but is not HEAD"

  the working tree is dirty in a build path
    hazard in python: STILL EXISTS if ported naively, same shape as
                       above: a failed `git status --porcelain` (corrupt
                       index) would read as empty output, i.e. "clean".
                       GONE: `_git` defaults to `check=True` and raises on
                       a non-zero `git status`, which the harness reports
                       as a failure rather than a silent "nothing to see".
    held by:          test_guards.py "a working tree dirty in a path the
                       image is built from" plus the positive control
                       (clean tree reaches the seam)

  REACHABLE_FROM cannot be resolved
    hazard in python: STILL EXISTS if ported naively: same discarded-exit
                       hazard as the first refusal above.
                       GONE: `_rev_exists` reads `returncode` explicitly.
    held by:          test_guards.py "REACHABLE_FROM that does not resolve"

  COMMIT is not an ancestor of REACHABLE_FROM (#174, the one that already
  bit us)
    hazard in bash:   `git merge-base --is-ancestor` exits 1 for "no" and
                       128 when it cannot decide (shallow clone, missing
                       object); the two must stay distinguishable or a
                       shallow clone is reported as history rejecting the
                       commit rather than as "could not check".
    hazard in python: STILL EXISTS AS A DESIGN HAZARD, not a language
                       one: a port that collapsed "any non-zero" into one
                       refusal would erase exactly this distinction. It
                       is closed by keeping the three-way branch (`0` /
                       `1` / anything else) explicit in `_is_ancestor`,
                       mirroring the bash `elif`.
    held by:          test_guards.py's paired "git exiting 128 is
                       reported as undecidable, not as a no" and its
                       exit-1 control

Everywhere else in this module, an unguarded `harness.sh(...)` call is
the Python restoration of `set -e`: a status nobody reads raises
`CommandFailed`, which `harness.finish()` still reports rather than
discarding.
"""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import harness

PROGRAM = "record-release-hashes"


def refuse(message: str, *details: str) -> None:
    print(f"refusing: {message}", file=sys.stderr)
    for detail in details:
        print(detail, file=sys.stderr)
    raise SystemExit(harness.EXIT_USAGE)


def sha256_of(path: Path) -> str:
    """The same digest `sha256sum`/`shasum -a 256` would print, without
    depending on either being on `PATH`: stdlib `hashlib` produces the
    identical value on Linux and macOS, which is the property the bash
    two-tool dispatch existed for."""
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _git(args: list[str], *, cwd: Path, check: bool = True) -> str:
    return harness.sh(["git", *args], cwd=cwd, check=check).stdout.strip()


def _rev_exists(rev: str, *, cwd: Path) -> bool:
    return harness.sh(["git", "rev-parse", "--verify", "--quiet", rev], cwd=cwd, check=False).returncode == 0


def _is_ancestor(commit: str, ref: str, *, cwd: Path) -> tuple[str, int]:
    """("yes" | "no" | "undecidable", raw exit code), mirroring bash's
    three-way branch on `git merge-base --is-ancestor`'s exit code."""
    rc = harness.sh(["git", "merge-base", "--is-ancestor", commit, ref], cwd=cwd, check=False).returncode
    if rc == 0:
        return "yes", rc
    if rc == 1:
        return "no", rc
    return "undecidable", rc


def run_guards(root: Path, env: dict[str, str]) -> tuple[str, str, Path, list[str], bool]:
    """The five reproducibility guards, in the same order as the bash.

    Returns (version, commit, out, arches, unsafe) once every guard has
    passed, or exits 2 through `refuse`.
    """
    unsafe = env.get("UNSAFE_LOCAL_BUILD", "0") == "1"
    version = env.get("VERSION") or _git(["describe", "--tags", "--always"], cwd=root)
    commit = env.get("COMMIT") or _git(["rev-parse", "HEAD"], cwd=root)
    arches = (env.get("ARCHES") or "amd64 arm64").split()
    reachable_from = env.get("REACHABLE_FROM", "origin/main")

    if unsafe:
        out = Path(env["OUT"]) if env.get("OUT") else root / "container" / ".generated" / "release-manifest.local.json"
        out.parent.mkdir(parents=True, exist_ok=True)
        print("warning: UNSAFE_LOCAL_BUILD=1 waives all five reproducibility guards.", file=sys.stderr)
        print(
            'warning: the manifest is stamped "unsafe_local_build": true, which distribution/packaging '
            "refuses, and it defaults to a gitignored path. Do not commit it.",
            file=sys.stderr,
        )
        return version, commit, out, arches, unsafe

    out = Path(env["OUT"]) if env.get("OUT") else root / "container" / "release-manifest.json"

    if not _rev_exists(f"{commit}^{{commit}}", cwd=root):
        refuse(
            f"COMMIT={commit} does not name a commit in this repository, so nothing recorded against it "
            "could be checked out."
        )

    head_commit = _git(["rev-parse", "HEAD"], cwd=root)
    if commit != head_commit:
        refuse(
            f"COMMIT={commit} is not HEAD ({head_commit}), so the recorded commit would not describe "
            "the tree being built."
        )

    dirty = _git(["status", "--porcelain", "--", "core", "apps", "ui", "container/Dockerfile"], cwd=root)
    if dirty:
        refuse(
            f"the working tree is dirty in a path the image is built from, so these hashes would not be "
            f"reproducible from {commit}:",
            dirty,
        )

    if not _rev_exists(f"{reachable_from}^{{commit}}", cwd=root):
        refuse(f"cannot resolve {reachable_from} to check {commit} against it. Fetch it, or set REACHABLE_FROM.")

    verdict, rc = _is_ancestor(commit, reachable_from, cwd=root)
    if verdict == "no":
        refuse(
            f"{commit} is not an ancestor of {reachable_from}.",
            "A commit that is not already on main does not survive a squash merge, and the manifest would pin a "
            "SHA that leaves the history (#174).",
            "Regenerate from a checkout of a commit that is already on main.",
        )
    if verdict != "yes":
        refuse(
            f"git could not decide whether {commit} is an ancestor of {reachable_from} "
            f"(merge-base --is-ancestor exited {rc}, which is neither 0 nor 1).",
            "That is a fact about this checkout, not about the manifest: a shallow clone (git fetch "
            "--unshallow) or a missing object produces it, and the commit may well be perfectly reachable.",
            "Nothing is recorded, because a check that did not run is not a check that passed.",
        )

    return version, commit, out, arches, unsafe


def build_and_hash(root: Path, arch: str, version: str, commit: str) -> dict[str, object]:
    tag = f"backup-manager:release-hashes-{arch}"
    harness.step(f"Building linux/{arch} ({tag})")
    harness.sh(
        [
            "docker", "buildx", "build",
            "--platform", f"linux/{arch}",
            "--build-arg", f"VERSION={version}",
            "--build-arg", f"COMMIT={commit}",
            "-f", "container/Dockerfile",
            "-t", tag,
            "--load",
            ".",
        ],
        cwd=root,
        capture=False,
    )

    cid = harness.sh_out(["docker", "create", "--platform", f"linux/{arch}", tag, "/rbm", "version"])
    tmp = Path(tempfile.mkdtemp())
    try:
        # /rbm and /rbm-web, never the compatibility symlinks beside them
        # (the 0.3.3 CLI rename): `docker cp` without `-L` copies a link
        # as a link, and the local names below are the keys
        # `binary_sha256` records under, not the renamed CLI.
        harness.sh(["docker", "cp", f"{cid}:/rbm", str(tmp / "rbm")])
        harness.sh(["docker", "cp", f"{cid}:/rbm-web", str(tmp / "rbm-web")])
        harness.sh(["docker", "rm", cid])

        backup_manager_sha = sha256_of(tmp / "rbm")
        backup_manager_web_sha = sha256_of(tmp / "rbm-web")
        local_image_id = (
            harness.sh_out(["docker", "images", "--no-trunc", "--format", "{{.ID}}", tag])
            .splitlines()[0]
            .removeprefix("sha256:")
        )
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    return {
        "architecture": arch,
        "binary_sha256": {"rbm": backup_manager_sha, "rbm-web": backup_manager_web_sha},
        "local_image_id_sha256": local_image_id,
        "registry_digest": None,
    }


NOTE = (
    "Regenerate with scripts/rcmtools/release/record_release_hashes.py, and only at a commit that is "
    "ALREADY on main. A commit recorded from a feature branch stops existing the moment that branch is "
    "squash merged, and the manifest is then pinned to a SHA nobody can check out: that is issue #174, "
    "which is why the script refuses to record a commit that is not an ancestor of origin/main and why "
    "distribution/packaging's release-manifest-integrity check re-asks the same question on every run. "
    "binary_sha256 is hashed from the two binaries extracted out of the built image, so it is real "
    "evidence of what was compiled. registry_digest is the digest ghcr.io assigns "
    "ghcr.io/spdrman/backup-manager on push (docker buildx build --push prints it, docker buildx "
    "imagetools inspect reads it back)."
)


def body(root: Path, version: str, commit: str, out: Path, arches: list[str], unsafe: bool) -> int:
    harness.step(f"Recording release hashes for VERSION={version} COMMIT={commit}")

    entries = [build_and_hash(root, arch, version, commit) for arch in arches]

    manifest: dict[str, object] = {}
    if unsafe:
        manifest["unsafe_local_build"] = True
    manifest["version"] = version
    manifest["commit"] = commit
    manifest["generated_at"] = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    manifest["note"] = NOTE
    manifest["architectures"] = entries
    manifest["index_digest"] = None

    text = json.dumps(manifest, indent=2) + "\n"
    out.write_text(text, encoding="utf-8")
    print(f"==> Wrote {out}", file=sys.stderr)
    print(text, end="")
    return harness.EXIT_OK


def resolve_toplevel() -> Path:
    """`git rev-parse --show-toplevel` of the CURRENT WORKING DIRECTORY,
    not of this file's own location -- the Python equivalent of the bash
    `cd "$(git rev-parse --show-toplevel)"`. This script proves things
    about whatever repository it is invoked from (a throwaway fixture in
    test_guards.py, or a real release checkout), never about the
    checkout `rcmtools` happens to live in."""
    try:
        return Path(harness.sh_out(["git", "rev-parse", "--show-toplevel"]))
    except (harness.CommandFailed, harness.Failure):
        print(f"{PROGRAM}: not inside a git work tree", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE) from None


def main(argv: list[str]) -> int:
    del argv
    harness.set_program(PROGRAM)
    root = resolve_toplevel()
    env = dict(os.environ)

    version, commit, out, arches, unsafe = run_guards(root, env)

    if env.get("GUARDS_ONLY", "0") == "1":
        print(
            f"==> GUARDS_ONLY=1: every guard passed; stopping before the Docker build. Would write {out}",
            file=sys.stderr,
        )
        return harness.EXIT_OK

    return harness.finish(lambda: body(root, version, commit, out, arches, unsafe))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
