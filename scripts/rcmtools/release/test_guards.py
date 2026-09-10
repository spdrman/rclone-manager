#!/usr/bin/env python3
"""Automated controls for the refusals in `record_release_hashes.py` and
`publish_image.py`. Issues #174 and #88 (B5.2).

Ported off `scripts/tests/record-release-hashes-guards.test.sh` and
`scripts/tests/publish-image-guards.test.sh`. Those two bash files drove
the real scripts, at their literal `scripts/release/*.sh` paths, with
`GUARDS_ONLY=1`/`DRY_RUN=1` env-var seams -- real callers, not
`scripts/tests/ci-local-gate.test.sh` fabrications, so this port deletes
the bash guard tests along with the bash scripts they drove rather than
keeping either as a shim.

Every case here drives the real Python module, as a subprocess, inside a
throwaway `git init` repository -- exactly the bash structure -- and
asserts BOTH the exit code and the distinct message. Every refusal in
both modules exits `harness.EXIT_USAGE` (2), so an exit-code-only
assertion cannot tell "the tree is dirty" from "there is a private key
sitting in it", and those call for very different reactions.

# PORTED-CHECK HAZARD NOTE

  every `expect()` call in this file
    hazard in bash:   `expect` read a bash function's one return channel
                       (`rc=$?` immediately after, `out` captured
                       separately) and asserted both. A caller that only
                       checked `$?` after some intervening command would
                       silently check the wrong status.
    hazard in python: STILL EXISTS AS A DESIGN HAZARD: `subprocess.run`'s
                       `CompletedProcess` is a value, not a raised
                       exception, so a test that calls `run_guards(...)`
                       and never reads `.returncode` passes regardless of
                       what the script under test did.
    hazard in python: GONE here because `run_guards`/`run_publish_path`
                       below return a `subprocess.CompletedProcess`
                       directly and every test method asserts
                       `.returncode` via `assertEqual` before ever
                       looking at `.stdout`/`.stderr` -- there is no path
                       through any test method that skips the exit-code
                       assertion, and `expect()` (a thin wrapper) takes
                       the wanted code as a required, positional
                       argument rather than an optional one.
    held by:           every positive control below (a clean checkout
                       must reach the seam with exit 0), which fails if
                       the exit-code assertion is ever silently dropped
                       from a refusal-checking test method, because the
                       same helper is shared.
"""

from __future__ import annotations

import contextlib
import os
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path
from typing import Iterator

REPO_ROOT = Path(__file__).resolve().parents[3]
RRH_SCRIPT = REPO_ROOT / "scripts" / "rcmtools" / "release" / "record_release_hashes.py"
PUBLISH_SCRIPT = REPO_ROOT / "scripts" / "rcmtools" / "release" / "publish_image.py"
WELL_FORMED_UNKNOWN_SHA = "0123456789abcdef0123456789abcdef01234567"


def git(*args: str, cwd: Path, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(["git", *args], cwd=cwd, capture_output=True, text=True, check=check)


def run(argv: list[str], *, cwd: Path, env: dict[str, str]) -> subprocess.CompletedProcess[str]:
    return subprocess.run(argv, cwd=cwd, env=env, capture_output=True, text=True, check=False)


class RecordReleaseHashesGuards(unittest.TestCase):
    """Mirrors record-release-hashes-guards.test.sh."""

    def new_repo(self, tmp: Path) -> Path:
        dir_ = tmp / "repo"
        dir_.mkdir()
        git("init", "-q", "-b", "main", cwd=dir_)
        git("config", "user.email", "t@example.invalid", cwd=dir_)
        git("config", "user.name", "t", cwd=dir_)
        for sub in ("core", "apps", "ui", "container"):
            (dir_ / sub).mkdir()
        (dir_ / "core" / "main.go").write_text("package main\n")
        (dir_ / "apps" / "marker").write_text("apps\n")
        (dir_ / "ui" / "marker").write_text("ui\n")
        (dir_ / "container" / "Dockerfile").write_text("FROM scratch\n")
        git("add", "-A", cwd=dir_)
        git("commit", "-qm", "base", cwd=dir_)
        return dir_

    def stub_git(self, tmp: Path, code: int) -> Path:
        """A `git` on PATH that answers `merge-base --is-ancestor` with a
        fixed exit code and forwards everything else to the real git."""
        bindir = tmp / "stubbin"
        bindir.mkdir()
        real_git_path = subprocess.run(["which", "git"], capture_output=True, text=True).stdout.strip()
        stub = bindir / "git"
        stub.write_text(
            textwrap.dedent(
                f"""\
                #!/usr/bin/env bash
                if [ "$1" = "merge-base" ] && [ "$2" = "--is-ancestor" ]; then
                    exit {code}
                fi
                exec "{real_git_path}" "$@"
                """
            )
        )
        stub.chmod(0o755)
        return bindir

    def run_guards(self, repo: Path, **overrides: str) -> subprocess.CompletedProcess[str]:
        env = dict(os.environ)
        env["GUARDS_ONLY"] = "1"
        env.update(overrides)
        return run([sys.executable, str(RRH_SCRIPT)], cwd=repo, env=env)

    def test_clean_checkout_passes_every_guard(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(tmp)
            proc = self.run_guards(repo, REACHABLE_FROM="main")
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertIn("Would write", proc.stderr)
            self.assertIn("release-manifest.json", proc.stderr)

    def test_commit_names_nothing(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(tmp)
            proc = self.run_guards(repo, REACHABLE_FROM="main", COMMIT=WELL_FORMED_UNKNOWN_SHA)
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("does not name a commit in this repository", proc.stderr)

    def test_commit_resolves_but_is_not_head(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(tmp)
            base = git("rev-parse", "HEAD", cwd=repo).stdout.strip()
            (repo / "core" / "main.go").write_text("package main\nsecond\n")
            git("commit", "-qam", "second", cwd=repo)
            proc = self.run_guards(repo, REACHABLE_FROM="main", COMMIT=base)
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("is not HEAD", proc.stderr)

    def test_dirty_tree_in_a_build_path(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(tmp)
            with open(repo / "core" / "main.go", "a") as fh:
                fh.write("uncommitted\n")
            proc = self.run_guards(repo, REACHABLE_FROM="main")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("the working tree is dirty", proc.stderr)

    def test_reachable_from_does_not_resolve(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(tmp)
            proc = self.run_guards(repo)  # default origin/main does not exist here
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("cannot resolve origin/main", proc.stderr)

    def test_commit_not_an_ancestor_of_reachable_from(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(tmp)
            git("checkout", "-q", "-b", "feature", cwd=repo)
            (repo / "core" / "main.go").write_text("package main\nfeature\n")
            git("commit", "-qam", "feature work", cwd=repo)
            proc = self.run_guards(repo, REACHABLE_FROM="main")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("is not an ancestor of main", proc.stderr)
            self.assertNotIn("could not decide", proc.stderr)

    def test_git_128_is_undecidable_not_a_no(self) -> None:
        """Pins the exact hazard: merge-base exiting 128 (shallow clone,
        missing object) must not be reported as "not an ancestor"."""
        with self._tmp() as tmp:
            repo = self.new_repo(tmp)

            stub = self.stub_git(tmp, 128)
            env = dict(os.environ)
            env["GUARDS_ONLY"] = "1"
            env["REACHABLE_FROM"] = "main"
            env["PATH"] = f"{stub}:{env['PATH']}"
            proc = run([sys.executable, str(RRH_SCRIPT)], cwd=repo, env=env)
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("could not decide", proc.stderr)
            self.assertIn("exited 128", proc.stderr)
            self.assertNotIn("is not an ancestor of", proc.stderr)

    def test_git_1_is_still_a_plain_no(self) -> None:
        """Control for the arm above: the same stub at exit 1 must still
        report the ordinary refusal, proving the branch above is not
        just "any non-zero is undecidable"."""
        with self._tmp() as tmp:
            repo = self.new_repo(tmp)

            stub = self.stub_git(tmp, 1)
            env = dict(os.environ)
            env["GUARDS_ONLY"] = "1"
            env["REACHABLE_FROM"] = "main"
            env["PATH"] = f"{stub}:{env['PATH']}"
            proc = run([sys.executable, str(RRH_SCRIPT)], cwd=repo, env=env)
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("is not an ancestor of main", proc.stderr)
            self.assertNotIn("could not decide", proc.stderr)

    def test_unsafe_waives_guards_but_writes_gitignored_path(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(tmp)
            with open(repo / "core" / "main.go", "a") as fh:
                fh.write("uncommitted\n")  # would fail the dirty guard
            proc = self.run_guards(repo, UNSAFE_LOCAL_BUILD="1", COMMIT=WELL_FORMED_UNKNOWN_SHA)
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertIn("release-manifest.local.json", proc.stderr)
            self.assertIn("unsafe_local_build", proc.stderr)
            self.assertNotIn("Would write container/release-manifest.json", proc.stderr)

    def test_unsafe_still_honours_explicit_out(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(tmp)
            proc = self.run_guards(repo, UNSAFE_LOCAL_BUILD="1", OUT=str(repo / "explicit.json"))
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertIn(f"Would write {repo}/explicit.json", proc.stderr)

    @contextlib.contextmanager
    def _tmp(self) -> Iterator[Path]:
        with tempfile.TemporaryDirectory() as tmp:
            yield Path(tmp)


class PublishImageGuards(unittest.TestCase):
    """Mirrors publish-image-guards.test.sh."""

    def new_repo(self, tmp: Path) -> Path:
        dir_ = tmp / "repo"
        dir_.mkdir()
        git("init", "-q", "-b", "main", cwd=dir_)
        git("config", "user.email", "t@example.invalid", cwd=dir_)
        git("config", "user.name", "t", cwd=dir_)
        gitignore = REPO_ROOT / ".gitignore"
        (dir_ / ".gitignore").write_text(gitignore.read_text())
        for sub in ("core", "distribution/packaging", "ui", "container", "provenance"):
            (dir_ / sub).mkdir(parents=True)
        (dir_ / "core" / "main.go").write_text("package main\n")
        (dir_ / "ui" / "marker").write_text("ui\n")
        (dir_ / "container" / "Dockerfile").write_text("FROM scratch\n")
        (dir_ / "distribution" / "packaging" / "canonical.json").write_text(
            '{ "image": { "reference": "ghcr.io/spdrman/backup-manager:1.0.0", "published": false } }\n'
        )
        (dir_ / "container" / "release-manifest.json").write_text(
            f'{{ "version": "test", "commit": "{WELL_FORMED_UNKNOWN_SHA}" }}\n'
        )
        (dir_ / "provenance" / "sbom.spdx.json").write_text("{}\n")
        git("add", "-A", cwd=dir_)
        git("commit", "-qm", "base", cwd=dir_)
        return dir_

    def pin_manifest_to_head(self, repo: Path) -> None:
        head = git("rev-parse", "HEAD", cwd=repo).stdout.strip()
        (repo / "container" / "release-manifest.json").write_text(f'{{ "version": "test", "commit": "{head}" }}\n')

    def stub_go(self, tmp: Path, code: int) -> Path:
        bindir = tmp / "stubbin"
        bindir.mkdir()
        stub = bindir / "go"
        stub.write_text(f"#!/usr/bin/env bash\nexit {code}\n")
        stub.chmod(0o755)
        return bindir

    def run_guards(self, repo: Path, **overrides: str) -> subprocess.CompletedProcess[str]:

        env = dict(os.environ)
        env["GUARDS_ONLY"] = "1"
        env.update(overrides)
        return run([sys.executable, str(PUBLISH_SCRIPT)], cwd=repo, env=env)

    def run_publish_path(self, repo: Path, **overrides: str) -> subprocess.CompletedProcess[str]:

        env = dict(os.environ)
        env["DRY_RUN"] = "1"
        env["SIGN"] = "0"
        env.update(overrides)
        return run([sys.executable, str(PUBLISH_SCRIPT)], cwd=repo, env=env)

    @contextlib.contextmanager
    def _tmp(self) -> Iterator[Path]:
        with tempfile.TemporaryDirectory() as tmp:
            yield Path(tmp)

    def test_clean_checkout_reaches_the_seam(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertIn("Would publish ghcr.io/spdrman/backup-manager:1.0.0", proc.stderr)

    def test_canonical_missing(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            (repo / "distribution" / "packaging" / "canonical.json").unlink()
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("distribution/packaging/canonical.json is not readable", proc.stderr)

    def test_canonical_with_no_reference(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            (repo / "distribution" / "packaging" / "canonical.json").write_text('{ "image": { "published": false } }\n')
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("no image reference in", proc.stderr)

    def test_manifest_pins_sha_not_in_repo(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))  # fixture manifest still pins all-zeroes
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("which is not a commit in this repository", proc.stderr)

    def test_manifest_pins_real_commit_not_ancestor_of_head(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            git("checkout", "-q", "-b", "sidebranch", cwd=repo)
            (repo / "core" / "main.go").write_text("package main // side\n")
            git("commit", "-qam", "side", cwd=repo)
            side = git("rev-parse", "HEAD", cwd=repo).stdout.strip()
            git("checkout", "-q", "main", cwd=repo)
            (repo / "container" / "release-manifest.json").write_text(f'{{ "version": "test", "commit": "{side}" }}\n')
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("which is not an ancestor of HEAD", proc.stderr)

    def test_manifest_pins_commit_only_on_feature_branch(self) -> None:
        """#174 in the registry: reachable from HEAD is not enough; it
        must be reachable from a rewrite-free ref."""
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            git("checkout", "-q", "-b", "feature", cwd=repo)
            (repo / "core" / "main.go").write_text("package main // feature\n")
            git("commit", "-qam", "feature work", cwd=repo)
            self.pin_manifest_to_head(repo)
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("not reachable from any rewrite-free ref", proc.stderr)
            self.assertIn("#174", proc.stderr)

    def test_reachable_only_from_origin_release(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            head = git("rev-parse", "HEAD", cwd=repo).stdout.strip()
            git("update-ref", "refs/remotes/origin/release", head, cwd=repo)
            git("branch", "-m", "main", "cut", cwd=repo)
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertIn("is reachable from origin/release", proc.stderr)

    def test_checkable_from_names_ref_for_a_fork(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            git("branch", "-m", "main", "stable", cwd=repo)
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1", CHECKABLE_FROM="stable")
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertIn("is reachable from stable", proc.stderr)

    def test_manifest_pins_no_commit_at_all(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            (repo / "container" / "release-manifest.json").write_text('{ "version": "test" }\n')
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("pins no commit", proc.stderr)
            self.assertNotIn("not an ancestor of HEAD", proc.stderr)

    def test_unsafe_local_build_manifest_never_published(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            head = git("rev-parse", "HEAD", cwd=repo).stdout.strip()
            (repo / "container" / "release-manifest.json").write_text(
                f'{{ "unsafe_local_build": true, "version": "test", "commit": "{head}" }}\n'
            )
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("unsafe_local_build", proc.stderr)
            self.assertIn("public registry", proc.stderr)

    def test_dirty_tree_in_a_build_path(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            with open(repo / "core" / "main.go", "a") as fh:
                fh.write("uncommitted\n")
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("the working tree is dirty", proc.stderr)

    def test_untracked_gitignored_key_beside_the_script(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            (repo / "cosign.key").write_text("not a real key\n")
            ignored = git("check-ignore", "-q", "cosign.key", cwd=repo, check=False)
            self.assertEqual(ignored.returncode, 0, "the fixture does not ignore cosign.key")
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("private key material is present in the working tree", proc.stderr)
            self.assertIn("cosign.key", proc.stderr)

    def test_ignored_key_in_subdirectory(self) -> None:
        """No unwildcarded pathspec anchors below the repository root."""
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            (repo / "secrets").mkdir()
            (repo / "secrets" / "id_ed25519").write_text("not a real key\n")
            (repo / "secrets" / "release.pem").write_text("not a real key\n")
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("secrets/id_ed25519", proc.stderr)
            self.assertIn("secrets/release.pem", proc.stderr)

    def test_vendored_pem_under_node_modules_is_excluded(self) -> None:
        """The scoping half: proves the exclusion works once ignored
        files are in scope, using the two arms above as its own positive
        control that the scan looks at ignored files at all."""
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            (repo / "node_modules" / "some-pkg" / "fixtures").mkdir(parents=True)
            (repo / "ui" / "shared" / "dist").mkdir(parents=True)
            (repo / "node_modules" / "some-pkg" / "fixtures" / "test-cert.pem").write_text("not a real key\n")
            (repo / "ui" / "shared" / "dist" / "inlined.pem").write_text("not a real key\n")
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertNotIn("private key material is present in the working tree", proc.stderr)

    def test_tracked_pem_refused_too(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            (repo / "release-signing.pem").write_text("not a real key\n")
            git("add", "-f", "release-signing.pem", cwd=repo)
            git("commit", "-qm", "oops", cwd=repo)
            tracked = git("ls-files", "--", "release-signing.pem", cwd=repo).stdout.strip()
            self.assertTrue(tracked, "the fixture never tracked release-signing.pem")
            self.pin_manifest_to_head(repo)
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("release-signing.pem", proc.stderr)

    def test_cosign_key_file_refused_even_with_no_key_on_disk(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            proc = self.run_guards(repo, SKIP_PROVENANCE_CHECK="1", COSIGN_KEY_FILE="/tmp/nope.key")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("COSIGN_KEY_FILE is set", proc.stderr)
            self.assertNotIn("present in the working tree", proc.stderr)

    def test_no_sbom_in_the_tree(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            (repo / "provenance" / "sbom.spdx.json").unlink()
            proc = self.run_guards(repo)
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("is not in the tree, so there is no SBOM to attest", proc.stderr)

    def test_stale_provenance_is_a_refusal(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            stub = self.stub_go(Path(tmp), 1)

            proc = self.run_guards(repo, PATH=f"{stub}:{os.environ['PATH']}")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("are not what this tree generates", proc.stderr)

    def test_fresh_provenance_reaches_the_seam(self) -> None:
        """Control for the arm above."""
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            stub = self.stub_go(Path(tmp), 0)

            proc = self.run_guards(repo, PATH=f"{stub}:{os.environ['PATH']}")
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertNotIn("are not what this tree generates", proc.stderr)

    def test_skip_provenance_on_pushable_run_is_itself_a_refusal(self) -> None:
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            proc = self.run_publish_path(repo, SKIP_PROVENANCE_CHECK="1")
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("SKIP_PROVENANCE_CHECK=1 removes the check", proc.stderr)
            self.assertIn("test-only seam", proc.stderr)
            self.assertNotIn("stopping before docker buildx build", proc.stderr)

    def test_same_run_without_skip_reaches_the_push(self) -> None:
        """Control for the arm above."""
        with self._tmp() as tmp:
            repo = self.new_repo(Path(tmp))
            self.pin_manifest_to_head(repo)
            stub = self.stub_go(Path(tmp), 0)

            proc = self.run_publish_path(repo, PATH=f"{stub}:{os.environ['PATH']}")
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertIn("stopping before docker buildx build", proc.stderr)
            self.assertNotIn("SKIP_PROVENANCE_CHECK=1 removes the check", proc.stderr)


def expansions_in_run_blocks(text: str) -> list[str]:
    """Every `line: text` inside a `run:` body carrying a `${{ }}`
    expansion. Comment lines do not count; they are not shell source.
    Ported line-for-line off the bash `awk` in
    publish-image-guards.test.sh, which itself scans .github/workflows/
    for a dispatch input interpolated straight into a run: body -- the
    worst place for it, because GitHub expands `${{ }}` textually into
    the script before bash ever parses it."""
    found: list[str] = []
    in_run = False
    run_indent = 0
    for lineno, line in enumerate(text.splitlines(), start=1):
        stripped = line.strip()
        if stripped.startswith("#"):
            continue
        indent = len(line) - len(line.lstrip())
        if in_run and line.strip() and indent <= run_indent:
            in_run = False
        if in_run and "${{" in line:
            found.append(f"{lineno}: {line}")
        run_value = stripped[4:].strip()
        if not in_run and stripped.startswith("run:") and (run_value.startswith("|") or run_value.startswith(">")):
            in_run = True
            run_indent = indent
    return found


class ReleaseWorkflowScanner(unittest.TestCase):
    """release.yml's publish job holds id-token: write and packages:
    write, minting the Sigstore identity; a dispatch input interpolated
    into a run: body there is shell source, in the job with the most
    privilege on this path."""

    def test_release_workflow_never_interpolates_a_dispatch_input(self) -> None:
        workflow = REPO_ROOT / ".github" / "workflows" / "release.yml"
        found = expansions_in_run_blocks(workflow.read_text())
        self.assertEqual(found, [], f"an expression is expanded into shell source in the release workflow: {found}")

    def test_scanner_finds_the_shape_it_hunts(self) -> None:
        """Positive control for the assertion above, which is a negative
        one: this is the exact shape release.yml carried before the
        input was moved into env:."""
        bad = textwrap.dedent(
            """\
            jobs:
              publish:
                steps:
                  - name: Refuse an unconfirmed publish
                    run: |
                      tag="1.0.0"
                      if [ "${{ inputs.confirm }}" != "$tag" ]; then
                        exit 1
                      fi
                  - name: After
                    uses: actions/checkout@v7
            """
        )
        found = expansions_in_run_blocks(bad)
        self.assertTrue(found, "the scanner does not flag an input interpolated straight into a run: body")


if __name__ == "__main__":
    unittest.main()
