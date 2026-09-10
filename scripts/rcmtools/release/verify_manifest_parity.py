#!/usr/bin/env python3
"""Prove `container/release-manifest.json` describes what this tree builds.

Ported off `scripts/release/verify-manifest-parity.sh`. Issue #260.

This is the half of the old guard 2 in `publish_image` that a SHA
comparison could not answer. That guard asked whether the manifest's
commit equalled HEAD, which no tree can satisfy (the manifest is
committed, so committing it always produces a commit the manifest does
not name) and which would not have answered the real question even if it
could: what matters is whether the bytes about to be pushed are the
bytes the manifest records, and only a rebuild says that.

Run standalone to check a tree before cutting a release:

    bash scripts/release/verify-manifest-parity.sh

`publish_image` runs it automatically, after every guard and before
`docker buildx build --push`, because that command publishes in the same
breath as it builds and there is no after.

It is deliberately NOT wired into `scripts/ci-local.sh`. It is two full
cross-architecture Docker builds, the manifest only changes at a
release, and distribution/packaging already fails the build on the
cheap questions.

# PORTED-CHECK HAZARD NOTE

  the manifest names an architecture the build cannot find a recorded
  hash for
    hazard in bash:   `recorded()` returns an empty string for an
                       architecture or binary the manifest does not
                       carry, so the comparison below has to treat "" as
                       "missing" rather than as a hash that happens not
                       to match; a `[ "$want" != "$got" ]` alone would
                       report a MISMATCH with an empty "manifest
                       records:" line instead of naming the real problem.
    hazard in python: STILL EXISTS AS A DESIGN HAZARD: `recorded()`
                       below returns `None` rather than `""` specifically
                       so `None`/empty are not conflatable with a real,
                       merely-different, hash. The "no hash recorded"
                       branch is kept as an explicit `if want is None`
                       ahead of the inequality check, exactly as the bash
                       ordered its `elif`.
    held by:          publish_image_guards.py "a manifest recording no hash for an
                       architecture the parity check builds anyway"

  a regex silently matching the wrong architecture's digest
    hazard in bash:   `recorded()` used a `sed`/`awk` extraction over the
                       flattened JSON text in the original guard 2 this
                       replaced; a nested per-architecture map with two
                       binaries apiece is exactly the shape a textual
                       pattern can cross-match (the module docstring for
                       this whole domain calls this out: "a regex that
                       silently matches the wrong top-level key would
                       compare a hash against itself and pass").
    hazard in python: GONE. `json.load` builds a real nested structure
                       and `recorded()` indexes it by architecture and
                       binary name; there is no regex anywhere in this
                       module.
    held by:          publish_image_guards.py's architecture-swap fixture, which
                       gives two architectures each other's hash and
                       proves both are reported as MISMATCH rather than
                       one masking the other.
"""

from __future__ import annotations

import hashlib
import json
import shutil
import sys
import tempfile
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import harness

PROGRAM = "verify-manifest-parity"

Manifest = dict[str, Any]


def refuse(message: str, *details: str) -> None:
    print(f"refusing: {message}", file=sys.stderr)
    for detail in details:
        print(detail, file=sys.stderr)
    raise SystemExit(harness.EXIT_USAGE)


def sha256_of(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def recorded(manifest: Manifest, arch: str, binary: str) -> str | None:
    """The manifest's hash for `binary` under `arch`, or `None` if the
    manifest carries no such entry. `None` rather than `""`: an empty
    string and a genuinely absent record must stay distinguishable, or a
    caller that treats them the same reports a hash mismatch against
    nothing instead of naming the real problem."""
    for entry in manifest.get("architectures", []):
        if entry.get("architecture") == arch:
            value = entry.get("binary_sha256", {}).get(binary)
            return str(value) if value is not None else None
    return None


def load_manifest(manifest_path: Path) -> Manifest:
    if not manifest_path.is_file():
        refuse(f"{manifest_path} is not readable from {Path.cwd()}, so there is nothing to check the build against.")
    with open(manifest_path, encoding="utf-8") as fh:
        loaded: Manifest = json.load(fh)
        return loaded


def run_guards(root: Path, manifest_path: Path) -> tuple[str, str, Manifest, list[str]]:
    manifest = load_manifest(manifest_path)

    if bool(manifest.get("unsafe_local_build", False)):
        refuse(
            f'{manifest_path} is stamped "unsafe_local_build": true, so it was generated with every '
            "reproducibility guard waived. There is no point proving a build matches it."
        )

    version = manifest.get("version", "")
    commit = manifest.get("commit", "")
    if not commit or not version:
        refuse(
            f"{manifest_path} records version '{version}' and commit '{commit}'; both are needed as build "
            "arguments, and a missing one would silently build something else."
        )

    arches = [entry["architecture"] for entry in manifest.get("architectures", [])]
    if not arches:
        refuse(
            f"{manifest_path} records no architecture at all, so this check would pass by having nothing to compare."
        )

    return version, commit, manifest, arches


def check_one(root: Path, arch: str, manifest: Manifest) -> list[str]:
    """Build `arch`, hash its two binaries, and return every mismatch line."""
    tag = f"backup-manager:parity-{arch}"
    print(f"==> Building linux/{arch}", file=sys.stderr)
    version = manifest["version"]
    commit = manifest["commit"]
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
    mismatches: list[str] = []
    try:
        harness.sh(["docker", "cp", f"{cid}:/rbm", str(tmp / "rbm")])
        harness.sh(["docker", "cp", f"{cid}:/rbm-web", str(tmp / "rbm-web")])
        harness.sh(["docker", "rm", cid])

        for binary in ("rbm", "rbm-web"):
            want = recorded(manifest, arch, binary)
            got = sha256_of(tmp / binary)
            if want is None:
                print(
                    f"MISMATCH {arch}/{binary}: the manifest records no hash for it at all, so nothing was compared.",
                    file=sys.stderr,
                )
                mismatches.append(f"{arch}/{binary}")
            elif want != got:
                print(f"MISMATCH {arch}/{binary}:", file=sys.stderr)
                print(f"  manifest records: {want}", file=sys.stderr)
                print(f"  this build made:  {got}", file=sys.stderr)
                mismatches.append(f"{arch}/{binary}")
            else:
                print(f"    ok {arch}/{binary} {got}", file=sys.stderr)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    return mismatches


def body(root: Path, manifest_path: Path, version: str, commit: str, manifest: Manifest, arches: list[str]) -> int:
    print(
        f"==> Proving {manifest_path} describes what this tree builds (version={version} commit={commit})",
        file=sys.stderr,
    )

    mismatches: list[str] = []
    for arch in arches:
        mismatches.extend(check_one(root, arch, manifest))

    if mismatches:
        print(file=sys.stderr)
        print(f"refusing: {len(mismatches)} recorded hash(es) do not describe what this tree builds.", file=sys.stderr)
        print(
            f"Either the manifest is stale (regenerate it: VERSION={version} "
            "scripts/release/record-release-hashes.sh, from a clean checkout of a commit "
            f"already on main), or something between {commit} and HEAD changed the image, in which case the "
            "release is not the build the manifest claims.",
            file=sys.stderr,
        )
        print("Nothing has been pushed.", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE)

    print("==> Every recorded binary hash matches this build.", file=sys.stderr)
    return harness.EXIT_OK


def resolve_toplevel() -> Path:
    """`git rev-parse --show-toplevel` of the CURRENT WORKING DIRECTORY,
    not of this file's own location -- the Python equivalent of the bash
    `cd "$(git rev-parse --show-toplevel)"`. This script proves things
    about whatever repository it is invoked from (a throwaway fixture built
    by scripts/rcmtools/tests/publish_image_guards.py, or a real release
    checkout), never about the checkout `rcmtools` happens to live in.
    Resolving the target from `__file__` instead is the defect this port
    was caught on."""
    try:
        return Path(harness.sh_out(["git", "rev-parse", "--show-toplevel"]))
    except (harness.CommandFailed, harness.Failure):
        print(f"{PROGRAM}: not inside a git work tree", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE) from None


def main(argv: list[str]) -> int:
    del argv
    harness.set_program(PROGRAM)
    root = resolve_toplevel()
    manifest_path = root / "container" / "release-manifest.json"
    version, commit, manifest, arches = run_guards(root, manifest_path)
    return harness.finish(lambda: body(root, manifest_path, version, commit, manifest, arches))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
