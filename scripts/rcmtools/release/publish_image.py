#!/usr/bin/env python3
"""Push the canonical multi-architecture image, sign it, attest its SBOM.

Ported off `scripts/release/publish-image.sh`. Issue #88 (B5.2).

NOTHING HAS BEEN PUSHED YET by importing or running this with
`GUARDS_ONLY=1` or `DRY_RUN=1`. Running this for real is an operator
action, not a gate step, and it is deliberately not wired into
`scripts/ci-local.sh` or any workflow that runs on its own. Three
reasons, in order of how much they matter:

  1. It publishes. A registry tag is not a thing you take back cleanly.
  2. It needs a credential this repository does not and must not hold
     (see "Credentials" in the original bash header; unchanged here).
  3. Pushing from a feature branch would put an image in the registry
     built from a commit that is not on main (#174, moved into the
     registry, where no ancestry check can reach it). Guard 2b refuses
     it.

No signing key is generated, stored or read from disk by this script.
Signing is keyless (Sigstore) in the release workflow; a hand-signed
release passes a key through `COSIGN_PRIVATE_KEY` in the environment.
Guard 5 refuses a key FILE unconditionally.

This domain's hazard, restated because it is the one every guard here
answers to: a publish-time guard that stops guarding fails as a BAD
RELEASE, not a red build. `verify_manifest_parity` carries the ninth
guard (binary parity), run here immediately before the push because it
needs a Docker build and so cannot be exercised through the
`GUARDS_ONLY=1` seam.

# PORTED-CHECK HAZARD NOTE

  guard 2/2b: the manifest names a commit this history can actually
  reach, and one that will stay reachable after a squash merge (#174)
    hazard in bash:   `git merge-base --is-ancestor` exits 1 for "no" and
                       128 (or any other non-zero) when it cannot decide;
                       collapsing that into "ancestry_rc != 0 means no"
                       reports a shallow clone as history rejecting the
                       commit.
    hazard in python: STILL EXISTS AS A DESIGN HAZARD, not a language
                       one, for the same reason as in
                       `record_release_hashes.py`: a port that folded the
                       three-way branch into one boolean would erase the
                       distinction. `_is_ancestor` below keeps the
                       explicit ("yes"|"no"|"undecidable", rc) return.
    held by:          test_guards.py's ancestry arms (not-reachable,
                       feature-branch-only, origin/release, CHECKABLE_FROM)

  guard 5: private key material in the working tree
    hazard in bash:   two `git ls-files` passes -- `--cached --others
                       --exclude-standard` for tracked/normal-untracked,
                       `--others --ignored --exclude-standard` for
                       ignored -- unioned with `sort -u`. Running only the
                       first (the guard's own predecessor, per the
                       header comment) goes blind to a key `cosign
                       generate-key-pair` writes into the working
                       directory, which `.gitignore` covers and which is
                       exactly the case this guard exists for.
    hazard in python: STILL EXISTS AS A DESIGN HAZARD: a port that ran
                       only one `git ls-files` invocation (as `_git_ok`
                       below could easily be called once instead of
                       twice) would reintroduce the same blind spot.
                       `scan_key_material` below issues both passes
                       explicitly and unions the results, mirroring the
                       bash brace group.
    held by:          test_guards.py's paired "untracked, gitignored
                       key" and "vendored .pem under node_modules is not
                       treated as key material" arms -- the first proves
                       the ignored-file pass runs at all, the second
                       proves the exclusion scoping it still works once
                       ignored files are in scope.

  guard 6: the provenance bundle describes this tree
    hazard in bash:   `SKIP_PROVENANCE_CHECK=1` removes the refusal
                       entirely; it is guarded by `[ "${GUARDS_ONLY:-0}"
                       != "1" ]` so the seam cannot be combined with a run
                       that could actually push. A collapsed guard that
                       checked `SKIP_PROVENANCE_CHECK` without also
                       checking `GUARDS_ONLY` would let a real release run
                       skip the one check protecting a signed, permanent
                       artifact.
    hazard in python: STILL EXISTS AS A DESIGN HAZARD: dropping the
                       `and not guards_only` half of the condition in
                       `main()` below would silently reopen this. Kept as
                       an explicit two-part `if` rather than folded into
                       one flag.
    held by:          test_guards.py "SKIP_PROVENANCE_CHECK on a run that
                       could publish is itself a refusal" and its control
                       (the same run without it reaches the push)
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import harness

PROGRAM = "publish-image"

CANONICAL_RELATIVE = "distribution/packaging/canonical.json"
MANIFEST_RELATIVE = "container/release-manifest.json"
SBOM_RELATIVE = "provenance/sbom.spdx.json"

KEY_PATHSPECS = [
    "*.key", "*.pem", "cosign.key", "*cosign*.key", "*.p12", "*.pfx",
    "id_rsa", "*/id_rsa", "id_ed25519", "*/id_ed25519",
]
KEY_EXCLUDES = ["node_modules/*", "ui/shared/dist/*"]


def refuse(message: str, *details: str) -> None:
    print(f"refusing: {message}", file=sys.stderr)
    for detail in details:
        print(detail, file=sys.stderr)
    raise SystemExit(harness.EXIT_USAGE)


def json_string(path: Path, key: str) -> str:
    """One top-level-ish `"key": "value"` pair, read with the same
    deliberately dumb `sed`-shaped scan the bash used: the two files this
    reads are generated with a fixed two-space shape, and a JSON parser
    is not worth a dependency in a script whose failure mode is caught by
    guard 1 or 6 anyway."""
    pattern = re.compile(r'"' + re.escape(key) + r'"\s*:\s*"([^"]*)"')
    for line in path.read_text(encoding="utf-8").splitlines():
        m = pattern.search(line)
        if m:
            return m.group(1)
    return ""


def _git_lines(args: list[str], *, cwd: Path) -> list[str]:
    out = harness.sh(["git", *args], cwd=cwd, check=False).stdout
    return [line for line in out.splitlines() if line]


def scan_key_material(root: Path) -> list[str]:
    """Every tracked-or-untracked-normal, plus every ignored, path
    matching a key-shaped name, excluding vendored dependency trees. Two
    `git ls-files` passes unioned, exactly as the bash brace group ran
    them -- see this module's hazard note for why one pass alone is not
    enough."""
    pathspecs = [*KEY_PATHSPECS, *(f":(exclude){e}" for e in KEY_EXCLUDES)]
    found = set()
    found.update(_git_lines(["ls-files", "--cached", "--others", "--exclude-standard", "--", *pathspecs], cwd=root))
    found.update(_git_lines(["ls-files", "--others", "--ignored", "--exclude-standard", "--", *pathspecs], cwd=root))
    return sorted(found)


def _is_ancestor(commit: str, ref: str, *, cwd: Path) -> tuple[str, int]:
    rc = harness.sh(["git", "merge-base", "--is-ancestor", commit, ref], cwd=cwd, check=False).returncode
    if rc == 0:
        return "yes", rc
    if rc == 1:
        return "no", rc
    return "undecidable", rc


def _rev_exists(rev: str, *, cwd: Path) -> bool:
    return harness.sh(["git", "rev-parse", "--verify", "--quiet", rev], cwd=cwd, check=False).returncode == 0


def guard_1_reads(root: Path) -> tuple[Path, Path, str]:
    canonical = root / CANONICAL_RELATIVE
    manifest = root / MANIFEST_RELATIVE
    for f in (canonical, manifest):
        if not f.is_file():
            refuse(
                f"{f.relative_to(root)} is not readable from {Path.cwd()}, so this script cannot tell what it "
                "would be publishing."
            )
    reference = os.environ.get("REFERENCE") or json_string(canonical, "reference")
    if not reference:
        refuse(f"no image reference in {CANONICAL_RELATIVE}, and REFERENCE is not set.")
    return canonical, manifest, reference


def guard_2_reachability(root: Path, manifest: Path) -> str:
    manifest_commit = json_string(manifest, "commit")
    if not manifest_commit:
        refuse(f"{MANIFEST_RELATIVE} pins no commit, so there is nothing to check the working tree against.")
    if not _rev_exists(f"{manifest_commit}^{{commit}}", cwd=root):
        refuse(f"{manifest} pins {manifest_commit}, which is not a commit in this repository.")

    head_commit = harness.sh(["git", "rev-parse", "HEAD"], cwd=root).stdout.strip()
    verdict, rc = _is_ancestor(manifest_commit, head_commit, cwd=root)
    if verdict == "no":
        refuse(f"{manifest} pins {manifest_commit}, which is not an ancestor of HEAD ({head_commit}).")
    if verdict != "yes":
        refuse(
            f"git could not decide whether {manifest_commit} is an ancestor of HEAD "
            f"(merge-base --is-ancestor exited {rc})."
        )

    return manifest_commit


def guard_2b_checkable(root: Path, manifest_commit: str) -> None:
    refs = (os.environ.get("CHECKABLE_FROM") or "origin/main origin/release main release").split()
    reachable_from = None
    for ref in refs:
        if not _rev_exists(f"{ref}^{{commit}}", cwd=root):
            continue
        verdict, _rc = _is_ancestor(manifest_commit, ref, cwd=root)
        if verdict == "yes":
            reachable_from = ref
            break
    if reachable_from is None:
        refuse(
            f"{manifest_commit} is not reachable from any rewrite-free ref ({', '.join(refs)}).",
            "A commit that only a feature branch has does not survive that branch's squash merge, and a "
            "registry tag cannot be corrected the way a file can (#174).",
        )
    print(f"==> {manifest_commit} is reachable from {reachable_from}", file=sys.stderr)


def guard_3_unsafe(manifest: Path) -> None:
    text = manifest.read_text(encoding="utf-8")
    if re.search(r'"unsafe_local_build"\s*:\s*true', text):
        refuse(
            f"{MANIFEST_RELATIVE} is stamped unsafe_local_build, and a waived build must never reach a public registry."
        )


def guard_4_dirty(root: Path) -> None:
    dirty = "\n".join(
        _git_lines(["status", "--porcelain", "--", "core", "apps", "ui", "container/Dockerfile"], cwd=root)
    )
    if dirty:
        refuse(
            "the working tree is dirty in a path the image is built from, so the pushed image would not match HEAD:",
            dirty,
        )


def guard_5_keys(root: Path) -> None:
    keyfiles = scan_key_material(root)
    if keyfiles:
        refuse(
            "private key material is present in the working tree, and this script will not run beside it:",
            *keyfiles,
            "Keyless signing needs no key file at all. A hand-signed release passes its key through the "
            "environment (cosign --key env://COSIGN_PRIVATE_KEY) and never writes it down.",
            "Being listed in .gitignore is not an answer: this scan looks at ignored files too, because "
            "ignored is where a generated key lands.",
        )
    if os.environ.get("COSIGN_KEY_FILE"):
        refuse(
            f"COSIGN_KEY_FILE is set ({os.environ['COSIGN_KEY_FILE']}). This script signs keylessly, or from "
            "env://COSIGN_PRIVATE_KEY; it does not read a key off disk."
        )


def guard_6_provenance(root: Path, guards_only: bool) -> None:
    skip = os.environ.get("SKIP_PROVENANCE_CHECK", "0") == "1"
    if skip and not guards_only:
        refuse(
            "SKIP_PROVENANCE_CHECK=1 removes the check that the SBOM about to be attested describes this "
            "tree, and this run is not a guards-only run, so it could push and sign.",
            "It is a test-only seam: set GUARDS_ONLY=1 alongside it, or unset it and regenerate the bundle "
            "with (cd distribution && go run ./cmd/provenance -write).",
        )
    if skip:
        print(
            "==> SKIP_PROVENANCE_CHECK=1: guard 6 is not being run. Only reachable on a GUARDS_ONLY=1 run, "
            "which stops before the push.",
            file=sys.stderr,
        )
        return

    sbom = root / SBOM_RELATIVE
    if not sbom.is_file():
        refuse(
            f"{SBOM_RELATIVE} is not in the tree, so there is no SBOM to attest. Generate it with: "
            "(cd distribution && go run ./cmd/provenance -write)"
        )

    env = dict(os.environ)
    env["GOWORK"] = "off"
    proc = subprocess.run(["go", "run", "./cmd/provenance"], cwd=root / "distribution", env=env, capture_output=True)
    if proc.returncode != 0:
        refuse(
            "the compliance artifacts in provenance/ are not what this tree generates, so the SBOM about to "
            "be attested to a published image describes a different tree.",
            "Regenerate them with: (cd distribution && go run ./cmd/provenance -write)",
        )


def run_guards(root: Path, guards_only: bool) -> tuple[str, str]:
    _canonical, manifest, reference = guard_1_reads(root)
    manifest_commit = guard_2_reachability(root, manifest)
    guard_2b_checkable(root, manifest_commit)
    guard_3_unsafe(manifest)
    guard_4_dirty(root)
    guard_5_keys(root)
    guard_6_provenance(root, guards_only)
    return reference, manifest_commit


def publish(root: Path, reference: str, manifest_commit: str, manifest: Path) -> int:
    platforms = os.environ.get("PLATFORMS", "linux/amd64,linux/arm64")
    sign = os.environ.get("SIGN", "1") == "1"

    print(f"==> Publishing {reference} ({platforms}) from {manifest_commit}", file=sys.stderr)
    if os.environ.get("DRY_RUN", "0") == "1":
        print("==> DRY_RUN=1: stopping before docker buildx build --push", file=sys.stderr)
        return harness.EXIT_OK

    # The bytes half of the old guard 2 (#260), and the last thing that
    # happens before anything leaves this machine.
    harness.sh(
        [sys.executable, str(Path(__file__).with_name("verify_manifest_parity.py"))],
        cwd=root,
        capture=False,
    )

    version = json_string(manifest, "version")
    harness.sh(
        [
            "docker", "buildx", "build",
            "--platform", platforms,
            "--build-arg", f"VERSION={version}",
            "--build-arg", f"COMMIT={manifest_commit}",
            "-f", "container/Dockerfile",
            "-t", reference,
            "--push",
            ".",
        ],
        cwd=root,
        capture=False,
    )

    print("==> Reading digests back from the registry", file=sys.stderr)
    index_digest = harness.sh_out(
        ["docker", "buildx", "imagetools", "inspect", reference, "--format", "{{.Manifest.Digest}}"]
    )
    print(f"==> index digest: {index_digest}", file=sys.stderr)

    if sign:
        harness.sh(["cosign", "sign", "--yes", f"{reference}@{index_digest}"], capture=False)

    print(file=sys.stderr)
    raw = harness.sh_out(["docker", "buildx", "imagetools", "inspect", reference, "--raw"])
    for d in re.findall(r'"digest"\s*:\s*"([^"]*)"', raw):
        print(f"  registry_digest candidate: {d}", file=sys.stderr)
    print(file=sys.stderr)
    print(
        f"Then set image.published to true in {CANONICAL_RELATIVE}, regenerate the provenance bundle",
        file=sys.stderr,
    )
    print("((cd distribution && go run ./cmd/provenance -write)), and run the gate.", file=sys.stderr)
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
    guards_only = os.environ.get("GUARDS_ONLY", "0") == "1"

    reference, manifest_commit = run_guards(root, guards_only)

    if guards_only:
        platforms = os.environ.get("PLATFORMS", "linux/amd64,linux/arm64")
        print(
            f"==> GUARDS_ONLY=1: every guard passed; stopping before the push. Would publish {reference} "
            f"for {platforms}",
            file=sys.stderr,
        )
        return harness.EXIT_OK

    manifest = root / MANIFEST_RELATIVE
    return harness.finish(lambda: publish(root, reference, manifest_commit, manifest))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
