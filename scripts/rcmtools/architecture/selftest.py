#!/usr/bin/env python3
"""Positive controls for the architecture checks (issue #165).

Every check in this domain is a negative assertion: "core imports nothing
from distribution", "no platform file declares retention", "no tracked
file is unclassified". A negative assertion that has never been seen to
fail is indistinguishable from one that cannot fail, and this repository
has already been bitten by exactly that: a scanner that looked correct
silently missed `ADMIN_PASSWORD`, because `\\b` never matches between `_`
and `p`.

So each rule here is mutation-tested against the REAL tree rather than
against a synthetic fixture. A copy of the working tree gets one
deliberate violation planted in a real package, the check runs, and it
must fail AND name the rule. Then the copy is discarded. A check that
passes a planted violation is reported as a self-test failure, which is
the whole point.

It is fast: no npm install, no Docker, no worktree of its own beyond a
file copy, because every mutation targets a check that is static.

Ported from `scripts/architecture/selftest.sh` under EPIC I (#672 /
#697). That file now execs this one.

Every control below drives the BASH SHIM (`./scripts/architecture/<name>.sh`)
from inside the mutant copy, exactly as the bash did, never the Python
module directly. That is not incidental: it means the shim's `exec` --
the one place a ported check's exit status can be silently flattened --
is exercised by all thirty-one controls rather than by none of them.

# PORTED-CHECK HAZARD NOTE

  the mutant copy contains UNCOMMITTED files
    hazard in bash:   `git ls-files -z --cached --others --exclude-standard`,
                       not plain `git ls-files`. The checks being tested
                       are themselves often uncommitted while they are
                       being written, and copying tracked files only
                       produced a self-test that silently "caught" EVERY
                       mutation -- because the check script it invoked did
                       not exist in the copy at all. That is a real defect
                       this repository shipped once.
    hazard in python: LIVE, and now worse than in bash, because this port
                       adds a second file that must be in the copy: the
                       mutant runs the copy's own `scripts/rcmtools/`, so
                       a copy missing the Python package would make every
                       control "catch" its mutation via ImportError. That
                       is the same defect wearing the port's clothes, and
                       it is defect #2 from this programme's list
                       (`e2e-help` D3: a package-less sandbox exits
                       non-zero on ImportError, satisfying "exits
                       non-zero").
    held by:          `_assert_mutant_is_runnable` below, which runs on
                       the FIRST mutant built and refuses if the copy
                       cannot execute a check at all; plus every
                       `expect_check_fails` asserting the planted
                       REASON, not merely a non-zero exit.

  expect_check_fails asserts the exit status AND the message
    hazard in bash:   without the substring, a mutation "passes" whenever
                       the check fails for ANY reason, including the check
                       script being absent from the copy or erroring
                       before it ever looked at the mutation. That exact
                       failure mode produced a green self-test here once,
                       which is why the message is part of what is
                       asserted.
    hazard in python: STILL EXISTS, and the exit-status half is newly
                       fragile: `subprocess.run` returns a status nobody
                       is obliged to read, so a port that forgot
                       `.returncode` would assert only the message and
                       pass a check that printed the right words and
                       exited 0. Closed by `_run` returning both and
                       `expect_check_fails` testing the code FIRST.
    held by:          `test_the_harness_itself` below, which runs two
                       constructed stand-ins -- one that prints the
                       expected text and exits 0, one that exits non-zero
                       printing nothing -- and requires this harness to
                       reject both. A harness with no control of its own
                       is the thing every control here depends on.

  the symlink-escape control asserts the target SURVIVED
    hazard in bash:   `[ ! -d "$tmp/selftest-escape-target/adapter" ]`.
                       The refusal message is not enough on its own: a
                       check that printed the refusal AFTER deleting
                       through the symlink would satisfy the message
                       assertion.
    hazard in python: STILL EXISTS, unchanged in shape. Kept as its own
                       assertion, after the refusal, exactly as the bash
                       ordered it.
    held by:          `_escape_target_intact` below.

  the untracked-module pair asserts WHY it passed
    hazard in bash:   "the check passed" is also what a check that
                       inspected nothing looks like, so the same run has
                       to have named a real tracked module
                       (`^==> core \\(core layer\\)$`) and neither planted
                       path. The two controls are a matched pair: a check
                       that ignores untracked modules and a check that
                       has stopped looking at anything at all produce the
                       same green.
    hazard in python: STILL EXISTS, unchanged. The regex is anchored the
                       same way; `re.MULTILINE` is what `grep -qE` did
                       per line, and an unanchored `in` test would accept
                       the string appearing inside a longer line.
    held by:          `_why_it_passed` below.

  fixture names no product will ever have
    hazard in bash:   the planted directory used to be `apps/casaos`,
                       chosen as a plausible future provider. Issue #170
                       then added CasaOS for real, the fixture became a
                       CLASSIFIED directory, the planted violation stopped
                       being one, and this control passed while proving
                       nothing. A fixture a real change can turn valid is
                       a control with an expiry date on it.
    hazard in python: STILL EXISTS as the same naming discipline. Every
                       planted name below is carried over verbatim
                       (`selftest-unclassified-provider`,
                       `selftest-untracked-module`, `selftest-unowned`,
                       ...) rather than renamed to something tidier.
    held by:          nothing automatic, and that is stated rather than
                       papered over: it is a naming rule a reviewer
                       enforces. The names are deliberately absurd so
                       that renaming one looks wrong.
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import architecture as arch
from rcmtools import harness

PROGRAM = "architecture-selftest"

# The traversal target name is deliberately one that does not exist, so a
# regression fails these controls rather than deleting something.
TRAVERSAL_ENTRY = "apps/../../rclone-manager-selftest-traversal-target"

GIT_IDENTITY = ("-c", "user.email=selftest@example.invalid", "-c", "user.name=selftest")


@dataclass
class Tally:
    passed: int = 0
    failed: int = 0
    # Set while the harness tests ITSELF. The two stand-ins it drives are
    # SUPPOSED to be rejected, so their rejection messages are the
    # control working -- printing them would put two lines reading
    # "SELFTEST FAIL" at the top of a green run, which is exactly the
    # kind of output nobody reads twice.
    quiet: bool = False

    def ok(self, kind: str, label: str) -> None:
        # Column alignment is part of the output contract: the bash
        # printed "  ok (caught): " with one space and "  ok (clean):  "
        # with two, so the labels line up. Reproduced verbatim.
        pad = {"caught": "ok (caught): ", "clean": "ok (clean):  ", "intact": "ok (intact):  ", "why": "ok (why):    "}
        if not self.quiet:
            print(f"  {pad[kind]}{label}", flush=True)
        self.passed += 1

    def bad(self, message: str, output: str = "", *details: str) -> None:
        if not self.quiet:
            print(f"SELFTEST FAIL: {message}", file=sys.stderr, flush=True)
            # Detail lines come BEFORE the captured output, and already
            # carry their own four-space indent, exactly as the bash
            # emitted them.
            for detail in details:
                print(detail, file=sys.stderr, flush=True)
            if output:
                for line in output.splitlines():
                    print(f"    {line}", file=sys.stderr, flush=True)
        self.failed += 1


def _run(cwd: Path, *argv: str) -> tuple[int, str]:
    """Run a check inside `cwd`, capturing stdout and stderr together.

    Returns BOTH the status and the output. The status is returned rather
    than raised because every caller has to test it -- see this module's
    hazard note on why a port that read only the message would pass a
    check that printed the right words and exited 0.
    """
    proc = subprocess.run(
        list(argv), cwd=str(cwd), stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, check=False
    )
    return proc.returncode, proc.stdout


class Selftest:
    def __init__(self, root: Path, tmp: Path) -> None:
        self.root = root
        self.tmp = tmp
        self.tally = Tally()
        self._checked_runnable = False

    # ---- fixtures ----------------------------------------------------

    def mutant(self, name: str) -> Path:
        """Copy the working tree into `tmp/<name>` and return its path.

        `--cached --others --exclude-standard`, not plain `git ls-files`:
        the copy has to include files that are present but not yet
        committed, because the checks being tested are themselves often
        uncommitted while they are being written. See the hazard note.
        `--exclude-standard` keeps node_modules and build output out, so
        the copy stays quick.
        """
        directory = self.tmp / name
        directory.mkdir(parents=True, exist_ok=True)

        listing = subprocess.run(
            ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
            cwd=str(self.root),
            stdout=subprocess.PIPE,
            check=True,
        )
        # NUL-separated, and piped straight into tar the same way, so a
        # path containing a space or a newline copies correctly.
        tar_c = subprocess.Popen(
            ["tar", "-cf", "-", "--null", "-T", "-"],
            cwd=str(self.root),
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
        )
        assert tar_c.stdin is not None
        tar_x = subprocess.Popen(["tar", "-xf", "-"], cwd=str(directory), stdin=tar_c.stdout)
        if tar_c.stdout is not None:
            tar_c.stdout.close()
        tar_c.stdin.write(listing.stdout)
        tar_c.stdin.close()
        if tar_c.wait() != 0 or tar_x.wait() != 0:
            harness.die(f"could not copy the working tree into {directory}")

        # The checks call `git rev-parse --show-toplevel`, so the copy
        # needs to be a repository. An empty one with a single commit is
        # enough: nothing here builds a worktree of HEAD except the
        # deletion proofs, which commit their own mutation first.
        harness.sh(["git", "-C", str(directory), "init", "-q"])
        harness.sh(["git", "-C", str(directory), "add", "-A"])
        harness.sh(["git", "-C", str(directory), *GIT_IDENTITY, "commit", "-q", "-m", "selftest baseline"])

        if not self._checked_runnable:
            self._checked_runnable = True
            self._assert_mutant_is_runnable(directory)
        return directory

    def _assert_mutant_is_runnable(self, directory: Path) -> None:
        """The copy must be able to EXECUTE a check at all.

        New in the Python port, and not optional. Each bash path is now a
        shim over `scripts/rcmtools/`, so a copy that is missing the
        Python package -- or whose package cannot import -- makes every
        `expect_check_fails` below "catch" its mutation with an
        ImportError. That is a self-test that has silently stopped
        testing anything, printing all-green: the same defect the bash's
        `--cached --others` comment describes, reached by a new route.
        """
        code, out = _run(directory, "./scripts/architecture/check-layer-manifest.sh")
        if code != 0 and ("Traceback" in out or "ModuleNotFoundError" in out or "No such file" in out):
            self.tally.bad(
                "the mutant copy cannot run a check at all, so every control below would 'catch' its "
                "mutation for the wrong reason. The copy is missing scripts/rcmtools/ or cannot import it.",
                out,
            )

    def commit_mutant(self, directory: Path, message: str) -> None:
        """The deletion proofs build a throwaway worktree of HEAD, so a
        mutation they are meant to see has to be committed inside the copy
        first. The static checks read the working tree and do not need
        this."""
        harness.sh(["git", "-C", str(directory), "add", "-A"])
        harness.sh(["git", "-C", str(directory), *GIT_IDENTITY, "commit", "-q", "-m", message])

    # ---- assertions --------------------------------------------------

    def expect_check_fails(self, label: str, directory: Path, expect: str, *argv: str) -> str:
        """The check must fail, AND fail for the planted reason.

        The expected substring is not decoration -- see this module's
        hazard note. The status is tested FIRST, so a check that refused
        for an unrelated reason cannot pass because an unrelated
        substring happened to match.
        """
        code, out = _run(directory, *argv)
        if code == 0:
            self.tally.bad(f"{label}. The check PASSED against a planted violation.", out)
        elif expect not in out:
            self.tally.bad(
                f"{label}. The check failed, but not for the planted reason.",
                out,
                f"    expected its output to contain: {expect}",
            )
        else:
            self.tally.ok("caught", label)
        return out

    def expect_check_passes(self, label: str, directory: Path, *argv: str) -> str:
        code, out = _run(directory, *argv)
        if code == 0:
            self.tally.ok("clean", label)
        else:
            self.tally.bad(
                f"{label}. The check FAILED against an unmutated tree, so its failures mean nothing.", out
            )
        return out


def test_the_harness_itself(st: Selftest) -> None:
    """A control for `expect_check_fails` itself.

    Every control in this file rests on that one function testing two
    things -- a non-zero status and the planted reason -- and this port
    makes both newly droppable: `subprocess.run`'s status is inert unless
    read, and a substring test alone would accept a check that printed
    the right words and exited 0. So the harness gets two constructed
    stand-ins it must reject, and one it must accept.
    """
    bindir = st.tmp / "harness-control"
    bindir.mkdir(parents=True, exist_ok=True)

    liar = bindir / "prints-and-passes.sh"
    liar.write_text('#!/usr/bin/env bash\necho "the planted marker"\nexit 0\n')
    liar.chmod(0o755)

    mute = bindir / "fails-silently.sh"
    mute.write_text("#!/usr/bin/env bash\nexit 1\n")
    mute.chmod(0o755)

    honest = bindir / "fails-loudly.sh"
    honest.write_text('#!/usr/bin/env bash\necho "the planted marker" >&2\nexit 1\n')
    honest.chmod(0o755)

    # The claim is not "these stand-ins behave as built" -- it is that
    # expect_check_fails REJECTS both of them. So drive the real
    # assertion against each and require it to have recorded a failure,
    # then roll the tally back: those two recorded failures are the
    # control working, not the suite failing.
    def rejects(label: str, script: Path, expect: str) -> bool:
        before_failed, before_passed = st.tally.failed, st.tally.passed
        # `quiet` while probing: these two rejections are the control
        # WORKING, so printing them would put two lines reading
        # "SELFTEST FAIL" at the top of an all-green run.
        st.tally.quiet = True
        try:
            st.expect_check_fails(label, bindir, expect, str(script))
        finally:
            st.tally.quiet = False
        rejected = st.tally.failed == before_failed + 1
        st.tally.failed, st.tally.passed = before_failed, before_passed
        return rejected

    # A check that prints exactly the expected marker and exits 0. Only
    # the exit-status half of the assertion can catch this one, and that
    # half is what a Python port drops by forgetting `.returncode`.
    if rejects("harness control: prints the marker, exits 0", liar, "the planted marker"):
        st.tally.ok("why", "expect_check_fails rejects a check that prints the marker and exits 0")
    else:
        st.tally.bad(
            "expect_check_fails ACCEPTED a check that printed the expected marker and exited 0. "
            "Its exit-status half is not being tested, so every 'ok (caught)' below is worthless."
        )

    # A check that exits non-zero saying nothing. Only the message half
    # can catch this one -- the failure mode the bash comment records,
    # where the check script was simply absent from the mutant copy.
    if rejects("harness control: exits non-zero, says nothing", mute, "the planted marker"):
        st.tally.ok("why", "expect_check_fails rejects a check that fails without the planted reason")
    else:
        st.tally.bad(
            "expect_check_fails ACCEPTED a check that failed for an unrelated reason. Its message half "
            "is not being tested, so a mutation 'caught' by an ImportError would read as green."
        )

    # And the positive control for the two above: a check that fails FOR
    # the planted reason must be accepted, or the two rejections mean
    # only that this harness rejects everything.
    st.expect_check_fails(
        "a check that fails FOR the planted reason is accepted (control for the two above)",
        bindir,
        "the planted marker",
        str(honest),
    )


def main(argv: list[str]) -> int:
    del argv
    harness.set_program(PROGRAM)
    root = arch.resolve_toplevel()
    os.chdir(root)

    tmp = Path(tempfile.mkdtemp(prefix="rclone-manager-arch-selftest.", dir=os.environ.get("TMPDIR", "/tmp")))
    st = Selftest(root, tmp)
    try:
        return harness.finish(lambda: body(st))
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def body(st: Selftest) -> int:
    root, tmp = st.root, st.tmp

    print("==> the self-test's own harness", flush=True)
    test_the_harness_itself(st)

    print(flush=True)
    print("==> negative controls: every static check is clean on the real tree", flush=True)
    for name in (
        "check-layer-manifest",
        "check-layer-ownership",
        "check-core-dependency-rule",
        "check-unowned-go",
    ):
        st.expect_check_passes(name, root, f"./scripts/architecture/{name}.sh")

    print(flush=True)
    print("==> layer-manifest completeness", flush=True)

    # The planted directory is NOT the name of a platform anyone might
    # add. See the hazard note's "fixture names no product will ever
    # have" entry: this one used to be apps/casaos, and #170 added CasaOS
    # for real.
    d = st.mutant("manifest-unclassified")
    (d / "apps/selftest-unclassified-provider/frontend").mkdir(parents=True)
    (d / "apps/selftest-unclassified-provider/frontend/platform.ts").write_text(
        'export const selftestBridge = { id: "selftest-unclassified-provider" };\n'
    )
    harness.sh(["git", "-C", str(d), "add", "-A"])
    st.expect_check_fails(
        "a new provider directory nobody classified",
        d,
        "apps/selftest-unclassified-provider/frontend/platform.ts",
        "./scripts/architecture/check-layer-manifest.sh",
    )

    d = st.mutant("manifest-stale")
    _append(d / arch.MANIFEST, "\ncore            -           a/path/that/does/not/exist\n")
    st.expect_check_fails(
        "a manifest entry pointing at a path that does not exist",
        d,
        "does not exist",
        "./scripts/architecture/check-layer-manifest.sh",
    )

    d = st.mutant("manifest-badkind")
    _sub(
        d / arch.MANIFEST,
        r"^distribution    adapter     apps/unraid/template$",
        "distribution    -           apps/unraid/template",
    )
    st.expect_check_fails(
        "a distribution entry with no adapter/canonical kind",
        d,
        'must be "adapter" or "canonical"',
        "./scripts/architecture/check-layer-manifest.sh",
    )

    d = st.mutant("manifest-noadapters")
    _sub(d / arch.MANIFEST, r"^distribution    adapter ", "distribution    canonical ")
    st.expect_check_fails(
        "a manifest with no adapter paths at all, which would make the deletion proof vacuous",
        d,
        "would delete nothing and pass vacuously",
        "./scripts/architecture/check-layer-manifest.sh",
    )

    print(flush=True)
    print("==> manifest paths that leave the repository", flush=True)

    # verify-core-without-distribution.sh deletes every path the manifest
    # marks "distribution adapter", so the manifest is an input to an
    # rm -rf. A path with a ".." segment in the middle of it resolves and
    # deletes normally, and before these controls nothing looked at the
    # shape of an entry at all: it matches no tracked file, so the
    # completeness guard never mentions it, and the existence test is
    # satisfied by whatever is really out there.
    d = st.mutant("manifest-traversal")
    _append(d / arch.MANIFEST, f"\ndistribution    adapter     {TRAVERSAL_ENTRY}\n")
    st.expect_check_fails(
        'a manifest entry with a ".." segment in the middle of it',
        d,
        "escapes the repository",
        "./scripts/architecture/check-layer-manifest.sh",
    )

    d = st.mutant("manifest-absolute")
    _append(d / arch.MANIFEST, "\ndistribution    adapter     /rclone-manager-selftest-absolute-target\n")
    st.expect_check_fails(
        "a manifest entry naming an absolute path",
        d,
        "is absolute",
        "./scripts/architecture/check-layer-manifest.sh",
    )

    # And the same entry against the check that would actually delete it.
    # This runs the deletion proof, but it never reaches a build: the
    # refusal happens in the delete loop, before any go build.
    d = st.mutant("delete-traversal")
    _append(d / arch.MANIFEST, f"\ndistribution    adapter     {TRAVERSAL_ENTRY}\n")
    st.commit_mutant(d, "plant a traversal entry in the layer manifest")
    st.expect_check_fails(
        "the adapter-deletion proof handed a manifest entry that escapes the repository",
        d,
        "refuses to run against a manifest entry it cannot vouch for",
        "./scripts/architecture/verify-core-without-distribution.sh",
    )

    # The control for the containment assertion specifically, which is
    # the half that does not trust the input: this entry has a clean
    # shape, and it still resolves outside the worktree because a
    # directory on the way is a symlink. Nothing but resolving the path
    # and looking at the answer catches it.
    d = st.mutant("delete-symlink-escape")
    escape_target = tmp / "selftest-escape-target"
    (escape_target / "adapter").mkdir(parents=True, exist_ok=True)
    (d / "apps/selftest-escape").symlink_to(escape_target)
    _append(d / arch.MANIFEST, "\ndistribution    adapter     apps/selftest-escape/adapter\n")
    st.commit_mutant(d, "plant a symlinked adapter path in the layer manifest")
    st.expect_check_fails(
        "the adapter-deletion proof handed a path that resolves outside the worktree through a symlink",
        d,
        "which is outside the throwaway worktree",
        "./scripts/architecture/verify-core-without-distribution.sh",
    )
    # The refusal message is not enough on its own: a check that printed
    # it AFTER deleting through the symlink would satisfy the assertion
    # above. So the target's survival is its own assertion.
    if not (escape_target / "adapter").is_dir():
        st.tally.bad("the adapter-deletion proof deleted through the symlink anyway.")
    else:
        st.tally.ok("intact", "the symlink's target survived the refusal")

    print(flush=True)
    print("==> dependency direction", flush=True)

    d = st.mutant("dep-core-to-distribution")
    (d / "apps/common/webhost").mkdir(parents=True, exist_ok=True)
    (d / "apps/common/webhost/selftest_reverse_import.go").write_text(
        "package webhost\n\nimport _ \"github.com/spdrman/rclone-manager/distribution/packaging\"\n"
    )
    # The reverse import has to resolve, so the mutant module gets a
    # replace pointing at its own sibling copy. Without it `go list`
    # fails for a dependency-resolution reason and the check would
    # "catch" the wrong thing.
    _go_mod_edit(d / "apps/common", "../../distribution")
    st.expect_check_fails(
        "a core-layer package importing distribution",
        d,
        "core/app \u2500X\u2500\u25ba distribution",
        "./scripts/architecture/check-core-dependency-rule.sh",
    )

    d = st.mutant("dep-platform-to-distribution")
    (d / "apps/generic/platform").mkdir(parents=True, exist_ok=True)
    (d / "apps/generic/platform/selftest_reverse_import.go").write_text(
        "package platform\n\nimport _ \"github.com/spdrman/rclone-manager/distribution/packaging\"\n"
    )
    _go_mod_edit(d / "apps/generic", "../../distribution")
    st.expect_check_fails(
        "a platform-layer package importing distribution",
        d,
        "platform/app \u2500X\u2500\u25ba distribution",
        "./scripts/architecture/check-core-dependency-rule.sh",
    )

    d = st.mutant("dep-core-to-provider-sdk")
    (d / "core/internal/lifecycle").mkdir(parents=True, exist_ok=True)
    (d / "core/internal/lifecycle/selftest_sdk_import.go").write_text(
        'package lifecycle\n\nimport _ "github.com/truenas/api-client-golang/truenas"\n'
    )
    harness.sh(
        [
            "go", "mod", "edit",
            "-require=github.com/truenas/api-client-golang@v0.0.0",
            "-replace=github.com/truenas/api-client-golang=./internal/selftest-fake-sdk",
        ],
        cwd=d / "core",
        env={**os.environ, "GOWORK": "off"},
    )
    (d / "core/internal/selftest-fake-sdk/truenas").mkdir(parents=True, exist_ok=True)
    (d / "core/internal/selftest-fake-sdk/go.mod").write_text(
        "module github.com/truenas/api-client-golang\n\ngo 1.27.0\n"
    )
    (d / "core/internal/selftest-fake-sdk/truenas/client.go").write_text(
        "package truenas\n\n"
        "// Client stands in for a real NAS vendor SDK, so the provider-SDK rule can\n"
        "// be shown to fire against an import that actually resolves.\n"
        "type Client struct{}\n"
    )
    st.expect_check_fails(
        "core importing a NAS vendor SDK",
        d,
        "\u2500X\u2500\u25ba NAS SDKs",
        "./scripts/architecture/check-core-dependency-rule.sh",
    )

    # A check that inspected nothing must refuse rather than report
    # success over an empty set. The two controls below are a matched
    # pair: this one proves a TRACKED module nobody classified is still a
    # hard failure, and the next proves an UNTRACKED one is not. Neither
    # is worth anything without the other, because a check that ignores
    # untracked modules and a check that has stopped looking at anything
    # at all produce the same green.
    d = st.mutant("dep-unclassified-module")
    (d / "apps/selftest-unclassified-provider").mkdir(parents=True, exist_ok=True)
    (d / "apps/selftest-unclassified-provider/go.mod").write_text(
        "module github.com/spdrman/rclone-manager/apps/selftest-unclassified-provider\n\ngo 1.27.0\n"
    )
    harness.sh(["git", "-C", str(d), "add", "-A"])
    st.expect_check_fails(
        "a TRACKED Go module the manifest classifies into no layer",
        d,
        "classified into no layer",
        "./scripts/architecture/check-core-dependency-rule.sh",
    )

    # The other half of the pair, and the literal #207 regression:
    # modules that are on disk but not in the repository cannot fail this
    # gate. Two are planted, at the two shapes that behave differently.
    # The first is the real case, a nested agent worktree under
    # .claude/worktrees/, which .gitignore covers. The second is a plain
    # untracked directory that no ignore rule mentions, so this control
    # cannot pass merely because .gitignore happened to hide the first
    # one; only sourcing the list from the index excludes it.
    d = st.mutant("dep-untracked-module")
    (d / ".claude/worktrees/selftest-planted/core").mkdir(parents=True, exist_ok=True)
    (d / ".claude/worktrees/selftest-planted/core/go.mod").write_text(
        "module github.com/spdrman/rclone-manager/core\n\ngo 1.27.0\n"
    )
    (d / "selftest-untracked-module").mkdir(parents=True, exist_ok=True)
    (d / "selftest-untracked-module/go.mod").write_text(
        "module example.invalid/selftest-untracked-module\n\ngo 1.27.0\n"
    )
    if harness.sh_ok(
        ["git", "-C", str(d), "ls-files", "--error-unmatch", "selftest-untracked-module/go.mod"]
    ):
        st.tally.bad("the planted untracked module is tracked after all, so this control proves nothing.")
    out = st.expect_check_passes(
        "untracked go.mod files, in a nested worktree and in a plain untracked directory (must NOT fire)",
        d,
        "./scripts/architecture/check-core-dependency-rule.sh",
    )

    # And assert WHY it passed, because "the check passed" is also what a
    # check that inspected nothing looks like. The same run has to have
    # named a real tracked module, and neither planted path.
    if (
        re.search(r"^==> core \(core layer\)$", out, re.MULTILINE)
        and "selftest-planted" not in out
        and "selftest-untracked-module" not in out
    ):
        st.tally.ok("why", "the same run still inspected the tracked core module, and named neither planted path")
    else:
        st.tally.bad(
            "the check passed beside the planted untracked modules, but not for the required reason.",
            out,
            '    it must still inspect tracked modules ("==> core (core layer)") and mention neither planted path.',
        )

    print(flush=True)
    print("==> layer ownership, one mutation per rule", flush=True)

    # One planted declaration per rule, in a REAL platform package and a
    # REAL distribution package alternately, so no rule is proven only in
    # one layer.
    def plant_ownership(label: str, rule: str, target: str, pkg: str, decl: str) -> None:
        d = st.mutant(f"own-{label}")
        (d / target).parent.mkdir(parents=True, exist_ok=True)
        (d / target).write_text(f"package {pkg}\n\n{decl}\n")
        st.expect_check_fails(label, d, rule, "./scripts/architecture/check-layer-ownership.sh")

    plant_ownership(
        "lifecycle-state declared in a runtime profile",
        'violates rule "lifecycle-state"',
        "apps/generic/platform/selftest_owns.go",
        "platform",
        "// LifecycleState is planted by the self-test.\ntype LifecycleState int",
    )
    plant_ownership(
        "retention-policy declared in a runtime profile",
        'violates rule "retention-policy"',
        "apps/generic/platform/selftest_owns.go",
        "platform",
        "// ApplyRetentionPlan is planted by the self-test. Note the camel case:\n"
        '// a word-boundary rule would never fire between "Apply" and "Retention".\n'
        "func ApplyRetentionPlan() {}",
    )
    plant_ownership(
        "validation-rules declared in a distribution package",
        'violates rule "validation-rules"',
        "distribution/packaging/selftest_owns.go",
        "packaging",
        "// ValidatorCatalog is planted by the self-test.\nvar ValidatorCatalog = map[string]string{}",
    )
    plant_ownership(
        "catalog-truth declared in a distribution package",
        'violates rule "catalog-truth"',
        "distribution/packaging/selftest_owns.go",
        "packaging",
        "// RebuildCatalog is planted by the self-test.\nfunc RebuildCatalog() {}",
    )
    plant_ownership(
        "backup-policy declared in a runtime profile",
        'violates rule "backup-policy"',
        "apps/generic/platform/selftest_owns.go",
        "platform",
        "// BackupPolicy is planted by the self-test.\ntype BackupPolicy struct{}",
    )

    # The declaration shapes the Go scan used to walk straight past. It
    # looked at file.Decls only, so a top-level type, func or var was the
    # whole population it could ever see, and every mutation above is one
    # of those. The three below are the ones an adapter would actually
    # drift into: a field on a metadata struct it already owns, a method
    # on an interface it already declares, and a type declared inside a
    # function body.
    plant_ownership(
        "retention-policy declared as a struct field in a distribution package",
        'violates rule "retention-policy"',
        "distribution/packaging/selftest_owns.go",
        "packaging",
        "// Config is planted by the self-test: the prohibited concept arrives as a\n"
        "// field on a struct the adapter legitimately owns, not as a declaration of\n"
        "// its own.\ntype Config struct {\n\tName           string\n\tRetentionTiers []string\n}",
    )
    plant_ownership(
        "catalog-truth declared as an interface method in a runtime profile",
        'violates rule "catalog-truth"',
        "apps/generic/platform/selftest_owns.go",
        "platform",
        "// Bridge is planted by the self-test.\ntype Bridge interface {\n\tRebuildCatalog() error\n}",
    )
    plant_ownership(
        "lifecycle-state declared inside a function body in a runtime profile",
        'violates rule "lifecycle-state"',
        "apps/generic/platform/selftest_owns.go",
        "platform",
        "// selftestLocal is planted by the self-test.\nfunc selftestLocal() {\n"
        "\ttype LifecycleState int\n\t_ = LifecycleState(0)\n}",
    )

    # The control the widening most needs: it deliberately stops short of
    # short variable declarations and function parameters, where the
    # identifier is a local convenience rather than a claim of ownership.
    # Neither may fire, or the rule becomes noise a contributor learns to
    # route around.
    d = st.mutant("own-go-local-noise")
    (d / "apps/generic/platform/selftest_owns.go").write_text(
        "package platform\n\n"
        "// selftestNoise is planted by the self-test.\n"
        "func selftestNoise(retentionPolicy int) int {\n"
        "\tvalidatorCatalog := retentionPolicy\n"
        "\treturn validatorCatalog\n}\n"
    )
    st.expect_check_passes(
        "a prohibited name as a function parameter and a short variable (must NOT fire)",
        d,
        "./scripts/architecture/check-layer-ownership.sh",
    )

    # TypeScript side: the bridges are where a runtime profile would most
    # plausibly grow a second opinion about retention.
    d = st.mutant("own-ts")
    _append(
        d / "apps/truenas/frontend/platform.ts",
        "\n// Planted by the self-test.\nexport interface RetentionPolicy { keep: number }\n",
    )
    st.expect_check_fails(
        "retention-policy declared in a provider bridge (TypeScript)",
        d,
        'violates rule "retention-policy"',
        "./scripts/architecture/check-layer-ownership.sh",
    )

    # And the control the TypeScript scanner most needs: a mention inside
    # a comment is NOT a declaration, and must not fire.
    d = st.mutant("own-ts-comment")
    _append(
        d / "apps/truenas/frontend/platform.ts",
        "\n// This bridge never defines a RetentionPolicy; core owns that.\n"
        "// export interface RetentionPolicy { keep: number }\n",
    )
    st.expect_check_passes(
        "a commented-out retention declaration in a bridge (must NOT fire)",
        d,
        "./scripts/architecture/check-layer-ownership.sh",
    )

    # The ownership check's own empty-set guard: a manifest whose
    # platform and distribution paths hold no Go or TypeScript at all
    # would otherwise make "no violations found" true and worthless.
    d = st.mutant("own-nothing-to-scan")
    _rewrite_layers_to_container(d / arch.MANIFEST)
    st.expect_check_fails(
        "an ownership run with no Go or TypeScript file to scan",
        d,
        "verified nothing",
        "./scripts/architecture/check-layer-ownership.sh",
    )

    print(flush=True)
    print("==> shared UI provider-SDK import scan", flush=True)

    # Only the fast static half is mutated here. The deletion half needs
    # an npm ci in a fresh worktree, which is minutes rather than
    # seconds, and it is the scan that carries the four platforms with no
    # directory to delete.
    d = st.mutant("ui-relative-import")
    _append(
        d / "ui/shared/src/types/platform.ts",
        '\n// Planted by the self-test.\nexport { ugosBridge } from "../../../apps/ugos/frontend/platform";\n',
    )
    st.expect_check_fails(
        "the shared UI reaching into a provider directory",
        d,
        "a relative reach into a provider directory",
        "./scripts/architecture/check-ui-shared-provider-imports.sh",
    )

    d = st.mutant("ui-bare-sdk")
    _append(
        d / "ui/shared/src/types/platform.ts",
        "\n// Planted by the self-test: a platform with no directory to delete, so\n"
        '// only the scan can catch it.\nexport { probe } from "@casaos/app-store-sdk";\n',
    )
    st.expect_check_fails(
        "the shared UI importing a bare provider SDK package",
        d,
        "a provider SDK package",
        "./scripts/architecture/check-ui-shared-provider-imports.sh",
    )

    # The control the scan most needs: ui/shared names every platform in
    # its PlatformId union and in prose, and none of that is an import.
    d = st.mutant("ui-prose-mention")
    _append(
        d / "ui/shared/src/types/platform.ts",
        "\n// Planted by the self-test: prose and a type, never an import. CasaOS,\n"
        "// ZimaOS, Dockge and Portainer are named here on purpose.\n"
        'export type SelftestPlatformId = "casaos" | "zimaos" | "dockge" | "portainer";\n',
    )
    st.expect_check_passes(
        "platform names in prose and in a type union (must NOT fire)",
        d,
        "./scripts/architecture/check-ui-shared-provider-imports.sh",
    )

    # The scan's own empty-set guard: if ui/shared/src moved, "no
    # provider import was found" would be true and meaningless.
    d = st.mutant("ui-nothing-to-scan")
    (d / "ui/shared/src").rename(d / "ui/shared/source")
    st.expect_check_fails(
        "a UI scan with no TypeScript file to read",
        d,
        "verified nothing",
        "./scripts/architecture/check-ui-shared-provider-imports.sh",
    )

    print(flush=True)
    print("==> Go files no module owns (#417)", flush=True)

    # Two Go files in this repository sit outside every module and
    # outside go.work (scripts/api/gen-bindings.go,
    # scripts/architecture/ownership.go), so the per-module `go vet` and
    # `golangci-lint` steps this gate runs cannot reach either of them.
    # Nothing had ever vetted or linted them, which is how one of the
    # pair came to be the only unformatted Go file in the tree.
    #
    # The count is part of the contract: a run that examined nothing
    # would pass this check trivially, and both cells below would then be
    # planting into a tree whose check never looks. The real tree has
    # two.
    code, out = _run(root, "./scripts/architecture/check-unowned-go.sh")
    if code == 0:
        if re.search(r"OK: [1-9][0-9]* Go file\(s\)", out):
            st.tally.ok("clean", "check-unowned-go examined at least one unowned file and passed")
        else:
            st.tally.bad(
                "check-unowned-go passed without examining anything, so the cells below plant into a "
                "check that never looks.",
                out,
            )
    else:
        st.tally.bad("check-unowned-go FAILED against the real tree, so its failures mean nothing.", out)

    d = st.mutant("unowned-vet")
    (d / "scripts/selftest-unowned").mkdir(parents=True, exist_ok=True)
    # A printf verb that does not match its argument: compiles, runs, and
    # is exactly the class `go vet` exists for.
    (d / "scripts/selftest-unowned/planted.go").write_text(
        'package main\n\nimport "fmt"\n\nfunc main() {\n'
        '\tfmt.Printf("%d\\n", "planted by scripts/architecture/selftest.sh")\n}\n'
    )
    harness.sh(["git", "-C", str(d), "add", "-A"])
    st.expect_check_fails(
        "a vet-catchable defect in a Go file no module owns",
        d,
        "wrong type string",
        "./scripts/architecture/check-unowned-go.sh",
    )

    d = st.mutant("unowned-lint")
    (d / "scripts/selftest-unowned").mkdir(parents=True, exist_ok=True)
    # An ineffectual assignment: `go vet` is silent on this one
    # (measured, not assumed), and golangci-lint's ineffassign is not.
    # Without this cell the lint half of the check could be removed and
    # every other cell would stay green.
    (d / "scripts/selftest-unowned/planted.go").write_text(
        'package main\n\nimport "fmt"\n\nfunc main() {\n\tn := 1\n\tn = 2\n\tfmt.Println(n)\n}\n'
    )
    harness.sh(["git", "-C", str(d), "add", "-A"])
    st.expect_check_fails(
        "a defect only the linter catches, in a Go file no module owns",
        d,
        "ineffectual assignment",
        "./scripts/architecture/check-unowned-go.sh",
    )

    if st.tally.failed != 0:
        print(file=sys.stderr, flush=True)
        print(
            f"FAIL: {st.tally.failed} of {st.tally.passed + st.tally.failed} architecture controls "
            "did not behave as required.",
            file=sys.stderr,
            flush=True,
        )
        return harness.EXIT_FAILED
    print(flush=True)
    print(
        f"OK: all {st.tally.passed} architecture controls behaved as required (every rule was shown to "
        "fire against a real planted violation, and shown not to fire on the real tree).",
        flush=True,
    )
    return harness.EXIT_OK


def _append(path: Path, text: str) -> None:
    with open(path, "a", encoding="utf-8") as fh:
        fh.write(text)


def _sub(path: Path, pattern: str, replacement: str) -> None:
    """One `sed -i` line substitution, anchored per line."""
    text = path.read_text(encoding="utf-8")
    path.write_text(re.sub(pattern, replacement, text, flags=re.MULTILINE), encoding="utf-8")


def _rewrite_layers_to_container(path: Path) -> None:
    """Point every platform and distribution row at `container`.

    The awk this replaces was `$1 == "platform" || $1 == "distribution"
    { print $1, $2, "container"; next } { print }` -- note it reflows the
    matched rows to single-space separation and leaves every other line
    (comments included) byte-for-byte. Reproduced, because the ownership
    check reads this file and a port that rewrote the comments too would
    be mutating more than the control claims.
    """
    out: list[str] = []
    for line in path.read_text(encoding="utf-8").splitlines():
        fields = line.split()
        if fields and fields[0] in ("platform", "distribution") and len(fields) >= 2:
            out.append(f"{fields[0]} {fields[1]} container")
        else:
            out.append(line)
    path.write_text("\n".join(out) + "\n", encoding="utf-8")


def _go_mod_edit(module_dir: Path, replace_target: str) -> None:
    env = {**os.environ, "GOWORK": "off"}
    for arg in (
        "-require=github.com/spdrman/rclone-manager/distribution@v0.0.0",
        f"-replace=github.com/spdrman/rclone-manager/distribution={replace_target}",
    ):
        harness.sh(["go", "mod", "edit", arg], cwd=module_dir, env=env)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
