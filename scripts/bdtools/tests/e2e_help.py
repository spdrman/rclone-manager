#!/usr/bin/env python3
"""The two end-to-end drivers' --help is operator-visible text, and this pins
it (issue #514).

What it is guarding against. Both scripts used to render their help by
reading their own header BY LINE NUMBER:

  sed -n '2,110p' "$0"     two-machine-backup.sh
  sed -n '2,84p'  "$0"     run-machine-tier.sh

so the help an operator read was a set of coordinates rather than a piece
of text. Inserting a comment above the boundary rewrote it and deleting one
truncated it, silently, and by the time #514 was written both had already
drifted: two-machine-backup.sh ended on a bare section heading with the
section missing, and run-machine-tier.sh ended mid-sentence, on "so about
76s of". Nothing anywhere rendered either script's help, so nothing could
have said so.

FR-35 clause 4 is the rule this is an instance of: nothing may reword a
line an operator already reads. core/tests/compat enforces that for the
CLI, byte for byte, and these two surfaces had none of it. So the rendered
text is now pinned against a golden the same way, and a reword fails here
until somebody updates the golden on purpose.

Four things get asserted, because pinning the text alone would not have
caught the defect that produced #514:

  A  the rendered help is byte for byte the golden, from a foreign working
     directory, for both --help and -h.
  B  neither script addresses its help by line number any more, and both
     carry exactly one HELP-START and one HELP-END marker.
  C  a comment inserted into the header ABOVE the block leaves the rendered
     help unchanged. That is the property that did not hold before, and it
     is the only one here that is about the shape rather than the content.
  D  the controls. C is also true of a mutation that never landed and of a
     renderer that prints nothing at all, so: the same insertion applied to
     a script that renders by line number MUST change its help, a reword
     inside the block MUST be seen, and a block with its markers removed
     MUST refuse out loud rather than print an empty help and exit 0.

Ported from `scripts/tests/e2e-help.test.sh` under EPIC I (#672 / #697),
carrying the fix from `fix/663-e2e` / #662 (the no-shim branch has to ASK
the filesystem rather than assert nothing about it): that file now execs
this one.

# PORTED-CHECK HAZARD NOTE

EPIC I / I1.6 (#672) moved both drivers to scripts/bdtools/e2e/*.py behind
exec shims at their old scripts/e2e/*.sh paths. A port can silently convert
a check into one that cannot fail, so each assertion answers for itself.

A  the rendered help is byte for byte the golden
  hazard in bash:   somebody rewords operator-visible text without deciding
                    to (FR-35 clause 4).
  hazard in python: STILL EXISTS, and grew a second half. The block moved
                    into a `#` comment block in the .py file rather than
                    into the module docstring, precisely so render_help
                    keeps ONE rule (strip a leading `# `) instead of
                    guessing. A docstring would have forced six heading
                    lines in each golden to change for no operator-visible
                    reason, and a golden diff nobody can read is a golden
                    nobody checks.
  held by:          the byte comparison, plus the shim-parity check: the
                    old path must render the same help, because that is
                    the path ci-local.sh, ci.yml and operators still name.

B  no help is addressed by line number; the markers are unique
  hazard in bash:   `sed -n '2,110p' "$0"` -- help as coordinates, silently
                    rewritten by any edit above the boundary. This is #514.
  hazard in python: STILL EXISTS. The grep runs on the help-owning file
                    whatever language it is in, and a Python renderer
                    slicing __doc__ by index would be the same defect.
  held by:          the same regex, repointed at subject_file, unchanged.

C  a comment inserted above the block does not change the help
  hazard in bash:   the boundary moves and the help silently truncates.
  hazard in python: STILL EXISTS. A comment between a Python shebang and
                    the docstring is legal and leaves the docstring first,
                    so the identical one-line mutation is still the honest
                    one.
  held by:          C, whose teeth are D1.

D3 an absent block is refused out loud, not answered with an empty help
  hazard in bash:   awk prints nothing and exits 0; the gate, the operator
                    and this suite all read it as a short help.
  hazard in python: STILL EXISTS AND GREW A NEW WAY TO GO VACUOUS. D3 asks
                    only for a non-zero exit, and a ported driver imports
                    bdtools before it parses argv -- so a sandbox missing
                    the package exits non-zero on an ImportError and D3
                    PASSES having measured nothing. This is why render_help
                    reads the FILE rather than __doc__ (a docstring reader
                    cannot tell deleted markers from an absent docstring),
                    why sandbox_copy copies the package, and why check S
                    exists at all.
  held by:          S (the unmutated sandbox must render, identically),
                    plus D3's second assertion that the refusal SAYS
                    "help block is missing" rather than merely failing.

S  is new, and has no bash counterpart: the port created the hazard it
   closes. Noted rather than left as an unexplained extra check.

This driver's own hazard, not present in bash: the mutated-sandbox helpers
(`insert_above_help`, `reword_inside_help`, `strip_markers`) rewrite the
COPY at `copy` in place, exactly like the `awk ... >"$copy.new" && mv
"$copy.new" "$copy"` idiom every bash mutation used; a version that
mutated the ORIGINAL under `scripts/bdtools/e2e/` instead would corrupt
this suite's own source of truth. Held by the fact every mutation function
below takes the sandbox path returned by `sandbox_copy`, never `script`.
"""

from __future__ import annotations

import difflib
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools.tests import testkit

SCRIPTS_DIR = Path(__file__).resolve().parents[2]
REPO_ROOT = SCRIPTS_DIR.parent
GOLDEN_DIR = SCRIPTS_DIR / "tests" / "testdata"

# A subject is a NAME, not a path, because I1.6 moved the ported drivers to
# scripts/bdtools/e2e/*.py and left an exec shim at each old
# scripts/e2e/*.sh path. The help lives with the driver, not with the shim,
# so this suite follows the driver: `subject_file` says where the
# help-owning file is and `subject_interp` says what runs it.
#
# The table stays a TABLE, and `subject_interp` stays a real lookup rather
# than a constant, precisely so a subject that is still bash does not need
# this file restructured to keep being checked. three-machine-web-ui (#699,
# and the driver #687's gate fix depends on) is that subject: never ported,
# so its help-owning file and its bash entry point are the same file.
#
# This is the shape `main` restructured the bash suite into while this port
# was in flight, carried forward deliberately. Collapsing it back to "every
# subject is python3" would drop the third subject silently, which is a
# capability disappearing in a port -- exactly what I1.6 exists to prevent.
SUBJECTS = ["two-machine-backup", "run-machine-tier", "three-machine-web-ui"]

_SUBJECT_FILE = {
    "two-machine-backup": "scripts/bdtools/e2e/two_machine_backup.py",
    "run-machine-tier": "scripts/bdtools/e2e/run_machine_tier.py",
    "three-machine-web-ui": "scripts/e2e/three-machine-web-ui.sh",
}
_SUBJECT_INTERP = {
    "two-machine-backup": "python3",
    "run-machine-tier": "python3",
    "three-machine-web-ui": "bash",
}
# The old scripts/e2e path that must still be a runnable entry point, or
# nothing where there is no longer any reason for one.
#
# Not every ported driver keeps its old path, and which ones do is a fact
# about who NAMES the path rather than a matter of consistency.
# two-machine-backup.sh is still exec'd by scripts/ci-local.sh, reached by
# .github/workflows/ci.yml through two-machine-ci.sh, driven by
# scripts/tests/two-machine-exit-status.test.sh, and FABRICATED at that
# literal path by scripts/tests/ci-local-gate.test.sh. run-machine-tier.sh
# was named by nothing that runs it once its callers were repointed, so it
# was deleted rather than left as a file whose only purpose is to be found.
# three-machine-web-ui is neither: it was never ported, so `subject_file`
# already points straight at its one and only entry point, and its "shim"
# is that same file. The branch this feeds still exercises something real
# for it -- A's "renders identically through <shim>" check runs the
# identical file twice, through the same interpreter, which is weaker than
# a real shim's comparison but not vacuous: a driver that read argv from
# anything other than its own invocation (a stray cwd assumption, say)
# would still fail it.
_SUBJECT_SHIM = {
    "two-machine-backup": "scripts/e2e/two-machine-backup.sh",
    "three-machine-web-ui": "scripts/e2e/three-machine-web-ui.sh",
}

INSERTED = "# An unrelated implementation note, added later, above the help block."


def subject_file(name: str) -> str:
    return _SUBJECT_FILE[name]


def subject_interp(name: str) -> str:
    """What runs `name`'s help-owning file. A lookup, not a constant: see
    the table's own comment for why collapsing this is how the third
    subject would vanish."""
    return _SUBJECT_INTERP[name]


def subject_shim(name: str) -> str:
    return _SUBJECT_SHIM.get(name, "")


def render(interp: str, script: Path, flag: str = "--help") -> tuple[str, int]:
    """A script's rendered help, from `/` rather than from the repository
    root -- the drivers resolve their own root from `__file__`, and this
    is what proves it, the same way it caught #514 before this port."""
    proc = subprocess.run(
        [interp, str(script), flag], cwd="/", stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True
    )
    return proc.stdout, proc.returncode


def sandbox_copy(tmpdirs: list[str], subject: str) -> Path:
    """A throwaway checkout holding one driver, its root resolution still
    landing on a directory it can cd into, and -- for a PORTED subject --
    the bdtools package it imports before it looks at argv.

    Staging the package is not convenience. A ported driver does
    `sys.path.insert(...); from bdtools import harness` before it reads
    argv, so a sandbox holding only the driver file dies on an ImportError
    before its help is ever rendered. C and D2 would then fail for a
    reason that is not what they measure, and D3 -- which only asks for a
    NON-ZERO EXIT -- would PASS on that ImportError, pinning nothing at
    all. That is the vacuous-green hazard this whole issue exists to watch
    for, and check S below is what proves the sandbox renders before
    anything mutates it.

    An unported bash subject such as three-machine-web-ui (#699) is a
    complete program on its own, so the package is NOT staged for it:
    copying a package it never imports would be copying for its own sake,
    and its file does not live under scripts/bdtools/e2e, so the
    directories below would be created for nothing.
    """
    d = Path(tempfile.mkdtemp())
    tmpdirs.append(str(d))
    file = subject_file(subject)
    (d / Path(file).parent).mkdir(parents=True, exist_ok=True)
    if subject_interp(subject) == "python3":
        (d / "scripts" / "bdtools" / "e2e").mkdir(parents=True, exist_ok=True)
        shutil.copy(REPO_ROOT / "scripts/bdtools/__init__.py", d / "scripts/bdtools/__init__.py")
        shutil.copy(REPO_ROOT / "scripts/bdtools/harness.py", d / "scripts/bdtools/harness.py")
        shutil.copy(REPO_ROOT / "scripts/bdtools/e2e/__init__.py", d / "scripts/bdtools/e2e/__init__.py")
    shutil.copy(REPO_ROOT / file, d / file)
    return d / file


def diff_head(expected: str, actual: str, limit: int) -> str:
    lines = list(
        difflib.unified_diff(expected.splitlines(keepends=True), actual.splitlines(keepends=True), n=3)
    )
    return "".join(lines[:limit])


def main() -> int:
    print("==> e2e driver --help (#514)", flush=True)
    suite = testkit.Suite("e2e driver --help")
    tmpdirs: list[str] = []

    try:
        # ------------------------------------ A: the rendered text is what it was
        for subject in SUBJECTS:
            script = REPO_ROOT / subject_file(subject)
            interp = subject_interp(subject)
            shim = subject_shim(subject)
            golden = GOLDEN_DIR / f"{subject}.help.txt"

            if not script.is_file():
                suite.bad(f"A {subject}'s help-owning file is where this expects it", f"no file at {script}")
                continue
            if not golden.is_file():
                suite.bad(f"A {subject}'s help is pinned", f"no golden at {golden}")
                continue

            actual, status = render(interp, script)

            suite.check(status == 0, f"A {subject} --help exits 0", f"A {subject} --help exits 0, got {status}", actual)

            golden_lines = golden.read_text().splitlines()
            suite.check(
                len(golden_lines) >= 20,
                f"A {subject}'s golden is a real help text rather than an empty file",
                f"A {subject}'s golden is a real help text rather than an empty file",
                f"{golden} has {len(golden_lines)} lines",
            )

            expected = golden.read_text().rstrip("\n")
            actual_stripped = actual.rstrip("\n")
            if actual_stripped == expected:
                suite.ok(f"A {subject} --help is byte for byte its golden")
            else:
                suite.bad(
                    f"A {subject} --help is byte for byte its golden",
                    diff_head(expected + "\n", actual_stripped + "\n", 40)
                    + f"""--
This is operator-visible text under FR-35 clause 4. If the reword is
deliberate, update the golden and say so in the commit:

  bash scripts/e2e/{subject}.sh --help > scripts/tests/testdata/{subject}.help.txt

If it is not deliberate, the help block in {subject_file(subject)} has moved
underneath somebody, which is the whole of #514.""",
                )

            short, _short_status = render(interp, script, "-h")
            short_stripped = short.rstrip("\n")
            suite.check(
                short_stripped == actual_stripped,
                f"A {subject} -h renders the same help as --help",
                f"A {subject} -h renders the same help as --help",
                diff_head(actual_stripped + "\n", short_stripped + "\n", 20),
            )

            # The old path is what ci-local.sh, ci.yml and an operator's muscle
            # memory all still name, and I1.6 kept a real exec shim there rather
            # than moving it. The no-shim branch has to ASK the filesystem (#662)
            # rather than assert nothing about it.
            if not shim:
                # Repo-relative, as `stale="scripts/e2e/$subject.sh"` was:
                # the message is read by a human deciding whether to delete
                # a file, and a /Users/... prefix is noise that also makes
                # the line differ between checkouts. `stale_abs` is what
                # asks the filesystem.
                stale = f"scripts/e2e/{subject}.sh"
                stale_abs = REPO_ROOT / stale
                if not stale_abs.exists():
                    suite.ok(f"A {subject} needs no scripts/e2e entry point, and has none at {stale} to drift")
                else:
                    suite.bad(
                        f"A {subject} needs no scripts/e2e entry point, and has none to drift",
                        f"""{stale} exists. subject_shim names no shim for {subject},
so nothing runs that file and nothing
compares its help against the driver's: it can drift arbitrarily far from
{subject_file(subject)} and this suite would never say so. Either delete it, or give {subject} a
shim entry in subject_shim so the branch above compares the two.""",
                    )
            elif (REPO_ROOT / shim).is_file():
                through_shim, _status = render("bash", REPO_ROOT / shim)
                suite.check(
                    through_shim.rstrip("\n") == actual_stripped,
                    f"A {subject} --help renders identically through {shim}",
                    f"A {subject} --help renders identically through {shim}",
                    diff_head(actual_stripped + "\n", through_shim.rstrip("\n") + "\n", 20),
                )
            else:
                suite.bad(
                    f"A {subject} still has an entry point at {shim}",
                    "scripts/ci-local.sh execs that literal path and scripts/tests/ci-local-gate.test.sh "
                    "fabricates a stand-in at it",
                )

        # ---------------------------------- B: nothing addresses help by line number
        for subject in SUBJECTS:
            script = REPO_ROOT / subject_file(subject)
            if not script.is_file():
                continue
            source_text = script.read_text()
            source_lines = source_text.splitlines()

            by_number = []
            pattern = re.compile(r"(sed|awk|head|tail)[^|]*['\"]?[0-9]+,[0-9]+p")
            for lineno, line in enumerate(source_lines, 1):
                if pattern.search(line) and not re.match(r"^\s*#", line):
                    by_number.append(f"{lineno}:{line}")
            suite.check(
                not by_number,
                f"B {subject} renders no part of itself by line number",
                f"B {subject} renders no part of itself by line number",
                "\n".join(by_number),
            )

            for marker in ("# HELP-START", "# HELP-END"):
                count = sum(1 for line in source_lines if line == marker)
                suite.check(
                    count == 1,
                    f"B {subject} carries exactly one {marker}",
                    f"B {subject} carries exactly one {marker}, found {count}",
                )

            suite.check(
                "render_help" in source_text,
                f"B {subject} renders its help through render_help",
                f"B {subject} renders its help through render_help",
            )

        # ------------- S: the sandbox itself renders, before anything mutates it
        for subject in SUBJECTS:
            script = REPO_ROOT / subject_file(subject)
            interp = subject_interp(subject)
            if not script.is_file():
                continue

            copy = sandbox_copy(tmpdirs, subject)
            sandbox_help, sandbox_status = render(interp, copy)
            real_help, _real_status = render(interp, script)
            if sandbox_status == 0 and sandbox_help == real_help:
                suite.ok(
                    f"S {subject}: an unmutated sandbox copy renders the same help, so C/D2/D3 measure what they claim"
                )
            else:
                suite.bad(
                    f"S {subject}: an unmutated sandbox copy renders the same help, so C/D2/D3 measure what they claim",
                    f"exit {sandbox_status} from {copy}\n{sandbox_help}",
                )

        # --------------- C: an edit above the block does not rewrite the help
        for subject in SUBJECTS:
            script = REPO_ROOT / subject_file(subject)
            interp = subject_interp(subject)
            if not script.is_file():
                continue
            before, _status = render(interp, script)

            copy = sandbox_copy(tmpdirs, subject)
            insert_above_help(copy)

            after, _status = render(interp, copy)
            suite.check(
                after == before,
                f"C {subject}: a comment added above the help block does not change --help",
                f"C {subject}: a comment added above the help block does not change --help",
                diff_head(before, after, 20),
            )

        # ------------------------------------------------- D: the controls for C

        # D1. The same insertion applied to a script that renders help by line
        # number MUST change its help, or C would pass against a mutation that
        # did nothing at all.
        control_dir = Path(tempfile.mkdtemp())
        tmpdirs.append(str(control_dir))
        (control_dir / "scripts" / "e2e").mkdir(parents=True)
        control = control_dir / "scripts" / "e2e" / "by-line-number.sh"
        control.write_text(
            "#!/usr/bin/env bash\n"
            "# first help line\n"
            "# second help line\n"
            "# third help line\n"
            "set -euo pipefail\n"
            "sed -n '2,4p' \"$0\" | sed 's/^# \\{0,1\\}//'\n"
        )
        control.chmod(0o755)
        control_before, _status = render("bash", control)
        insert_above_help(control)
        control_after, _status = render("bash", control)
        suite.check(
            control_before != control_after,
            "D1 the same insertion DOES change a help rendered by line number, so C has teeth",
            "D1 the same insertion DOES change a help rendered by line number, so C has teeth",
            f"the control script's help was [{control_before}] before and after, so C is measuring nothing",
        )

        # D2. A reword INSIDE the block has to be seen.
        for subject in SUBJECTS:
            script = REPO_ROOT / subject_file(subject)
            interp = subject_interp(subject)
            if not script.is_file():
                continue
            before, _status = render(interp, script)

            copy = sandbox_copy(tmpdirs, subject)
            reword_inside_help(copy)

            after, _status = render(interp, copy)
            reworded = "REWORDED" in after
            if after != before and reworded:
                suite.ok(f"D2 {subject}: a word changed inside the block does change --help")
            else:
                suite.bad(
                    f"D2 {subject}: a word changed inside the block does change --help",
                    "the mutated copy rendered help that is neither different nor carries the reword, "
                    "so A is pinning something that cannot move",
                )

        # D3. A renderer that answers an absent block with an empty help and
        # exit 0 is #160's silent skip wearing a different hat.
        for subject in SUBJECTS:
            script = REPO_ROOT / subject_file(subject)
            interp = subject_interp(subject)
            if not script.is_file():
                continue

            copy = sandbox_copy(tmpdirs, subject)
            strip_markers(copy)

            out, status = render(interp, copy)
            suite.check(
                status != 0,
                f"D3 {subject} refuses to render a help block whose markers are gone",
                f"D3 {subject} refuses to render a help block whose markers are gone",
                f"it exited 0 and printed {len(out)} bytes",
            )
            suite.check(
                "help block is missing" in out,
                f"D3 {subject} says what is missing rather than printing nothing",
                f"D3 {subject} says what is missing rather than printing nothing",
                out,
            )
    finally:
        for d in tmpdirs:
            shutil.rmtree(d, ignore_errors=True)

    return suite.finish()


def insert_above_help(path: Path) -> None:
    lines = path.read_text().splitlines(keepends=True)
    if not lines:
        return
    out = [lines[0], INSERTED + "\n", *lines[1:]]
    path.write_text("".join(out))


def reword_inside_help(path: Path) -> None:
    lines = path.read_text().splitlines()
    out = []
    i = 0
    while i < len(lines):
        out.append(lines[i])
        if lines[i] == "# HELP-START" and i + 1 < len(lines):
            out.append(lines[i + 1] + " REWORDED")
            i += 2
            continue
        i += 1
    path.write_text("\n".join(out) + "\n")


def strip_markers(path: Path) -> None:
    lines = path.read_text().splitlines()
    kept = [line for line in lines if line not in ("# HELP-START", "# HELP-END")]
    path.write_text("\n".join(kept) + "\n")


if __name__ == "__main__":
    sys.exit(main())
