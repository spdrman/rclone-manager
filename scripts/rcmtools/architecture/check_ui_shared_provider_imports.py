#!/usr/bin/env python3
"""The shared UI's provider-SDK import scan (issue #165), under EPIC I
(#672 / #697).

Ported from `scripts/architecture/check-ui-shared-provider-imports.sh`,
which stays on disk as an `exec` shim.

#165's acceptance criterion is stated over Phase 6's full platform list,
which adds Portainer, CasaOS, ZimaOS and Dockge to the six that have
directories today:

  "GIVEN the shared UI source tree, WHEN it is scanned for UGOS, TrueNAS,
   Unraid, Portainer, CasaOS, ZimaOS, Synology, OpenMediaVault or Proxmox
   SDK imports, THEN none are found."

Deleting a directory that does not exist yet proves nothing, so the four
without directories are covered by scanning instead. That puts the rule in
place BEFORE anyone reaches for the SDK, which is the only time a negative
check is worth anything.

This is the fast static half. `verify-ui-shared-without-provider-sdks.sh` is
the heavy deletion proof, and calls this first so a violation reports in
seconds rather than after an npm ci.

The scan reads module SPECIFIERS, not whole lines. ui/shared legitimately
names every platform in prose and in its PlatformId union (platform
differences are capability data, per EPIC B #81, so the names have to exist
somewhere), and a line-level grep would either fire on all of that or be
watered down until it fired on nothing.

# PORTED-CHECK HAZARD NOTE

Every assertion here is a NEGATIVE one: "no specifier names a provider".
A negative assertion is true of an empty set, so each of the hazards below
fails in the same direction -- fewer files read, fewer specifiers extracted,
a narrower match -- and every one of them ends with the same cheerful OK
line naming a file count nobody checks.

  the specifier regex still matches what grep matched
    hazard in bash:   `grep -oE '(from|import|require)[[:space:]]*\\(?
                       [[:space:]]*["'"'"'][^"'"'"']+["'"'"']'` per file,
                       then `sed -E 's/.*["'"'"']([^"'"'"']+)["'"'"'].*/\\1/'`
                       to keep the specifier. Deliberately not anchored and
                       deliberately not word-bounded: the point is to catch
                       every form that pulls a module into a TypeScript file
                       -- static import, re-export, dynamic `import(...)`,
                       `require(...)`.
    hazard in python: LIVE, and the trap is the one thing `grep` does that
                       `re` does not: grep matches LINE BY LINE, so `[^"']+`
                       can never cross a newline. `re.finditer` over the
                       whole file text CAN, which silently rewrites the
                       check: an unterminated quote anywhere would let one
                       "specifier" swallow the rest of the file (matching
                       nothing a provider pattern fires on) and, worse,
                       consume the real imports that followed it inside a
                       single non-overlapping match. Closed by splitting on
                       `"\\n"` and scanning each line, and by splitting on
                       `"\\n"` rather than `str.splitlines()`, which also
                       breaks on \\x0b, \\x0c and \\u2028 -- three characters
                       grep treats as ordinary text.
                       The specifier is captured by the group in the pattern
                       instead of re-extracted by a second substitution;
                       that is the same string, because the pattern admits
                       exactly two quote characters (the keyword prefix
                       cannot contain one), which is what made the bash's
                       greedy `.*["']` land on the opening quote.
    held by:          the mutation in this port's report: a relative reach
                       and a bare SDK import planted in ui/shared, each
                       reported with its own reason; plus selftest.sh's
                       `ui-prose-mention` control, which plants the platform
                       names in prose and in a type union and requires this
                       check NOT to fire.

  the file set is still every .ts/.tsx under ui/shared/src
    hazard in bash:   `find ui/shared/src -type f \\( -name '*.ts' -o -name
                       '*.tsx' \\) | sort`, with the CWD at the repository
                       top level, so the paths quoted in a finding are
                       repository-relative.
    hazard in python: LIVE in two ways. `-type f` tests the entry itself, so
                       a symlink is NOT a regular file to find, while
                       `os.walk` lists it among filenames and
                       `Path.is_file()` follows it -- held by skipping links
                       explicitly. And `find` without `-L` does not descend
                       symlinked directories, which is `os.walk`'s default
                       (`followlinks=False`) and is left as the default on
                       purpose rather than switched on for tidiness.
                       The paths are made relative to the resolved top level
                       rather than absolute: a finding quoting
                       `/private/var/folders/.../ui/shared/src/x.ts` from a
                       self-test mutant would be operator output that names
                       a tree the operator does not have.
    held by:          the differential test in this port's report, which
                       compares the count line: 155 file(s) from both
                       implementations, and `find | sort` byte-identical to
                       the sorted list this module builds.

  the empty-set guard still fires
    hazard in bash:   `[ "$scanned" -eq 0 ]` -> `exit 1`, after the findings
                       test. The bash's own comment says why: if
                       ui/shared/src moved or the find stopped matching, "no
                       provider import was found" would be true and
                       meaningless.
    hazard in python: STILL EXISTS, and it is the only thing standing
                       between a moved directory and a green run. `os.walk`
                       of a path that does not exist yields NOTHING and
                       raises nothing -- it is quieter than `find`, which at
                       least printed an error to stderr -- so without the
                       count test this check would print OK for zero files
                       read. Kept as an explicit count test, in the bash's
                       order (findings first, then the count).
    held by:          selftest.sh's `ui-nothing-to-scan` arm, which renames
                       ui/shared/src and greps for "verified nothing", and
                       by the vacuity mutation in this port's report.

  the platform list is HARD-CODED, and must stay that way
    hazard in bash:   `PROVIDERS=(ugos synology truenas unraid
                       openmediavault proxmox portainer casaos zimaos
                       dockge)` and a separate `SDK_PATTERN`, both literal.
                       The bash did NOT read layers.conf for this, and the
                       comment above the array says why: the list is #165's
                       acceptance criterion, "including the four with no
                       directory yet".
    hazard in python: STILL EXISTS, and nothing in the port removes it: the
                       literals are kept verbatim, which is the whole
                       measure taken. Deriving the list from
                       `arch.layer_paths` -- the obvious "don't repeat the
                       manifest" improvement -- would make the check's
                       COVERAGE a function of the manifest: a platform whose
                       directory has not been created yet has no manifest
                       row to derive from, so the four platforms this scan
                       exists for would drop out of it, and the check would
                       go green on the very import it was written to catch.
                       A manifest edit would silently shrink a negative
                       assertion, which is this programme's whole failure
                       mode. `SDK_PATTERN` stays separate from `PROVIDERS`
                       for the bash's stated reason too: a specifier like
                       `@ixsystems/...` names TrueNAS's vendor rather than
                       the platform, so it belongs to the bare-package
                       pattern and to no directory name.
    held by:          selftest.sh's `ui-bare-sdk` arm plants
                       `@casaos/app-store-sdk` -- CasaOS being one of the
                       four with no directory to delete -- so a derived list
                       would turn that control green against a planted
                       violation.

  the two rules stay distinguishable, and stay in order
    hazard in bash:   the relative-reach test runs on EVERY specifier and
                       `continue`s on a hit, so a relative path into
                       apps/<provider> is reported once and as a reach; the
                       bare-SDK test then runs only on specifiers that start
                       with neither `.` nor `/`, case-INSENSITIVELY
                       (`grep -qiE`), while the reach test is
                       case-SENSITIVE (`grep -qE`) because it is matching a
                       real directory name.
    hazard in python: LIVE. Collapsing the two into one `IGNORECASE` search
                       would make `Apps/UGOS/...` a reach that the
                       filesystem does not have, and dropping the `continue`
                       would report one import twice. `re.IGNORECASE` is
                       therefore set on the SDK pattern only, and the reason
                       string each finding carries is the string
                       selftest.sh greps for, so a swap between the two
                       reasons is caught rather than merely inaccurate.
    held by:          both ui mutations in this port's report: the reach is
                       reported as "(a relative reach into a provider
                       directory)" and the bare package as "(a provider SDK
                       package)", which are the two substrings selftest.sh
                       asserts on.

  the refusal is this script's exit status
    hazard in bash:   none; `exit 1` after each `echo ... >&2`, under
                       `set -euo pipefail`.
    hazard in python: LIVE, the domain-wide one. Both refusals return
                       `harness.EXIT_FAILED` from `body()` through
                       `harness.finish`, and the `FAIL:` lines are printed
                       directly rather than raised through `harness.die`,
                       which would prefix them with its own
                       `==> check-ui-shared-provider-imports: FAILED.`
                       banner and break the byte-for-byte fidelity
                       selftest.sh's substring greps and the differential
                       test both depend on.
    held by:          the mutations in this port's report (exit 1 observed,
                       not just the message), and every
                       `expect_check_fails` arm, which fails a check that
                       prints a refusal and exits 0.
"""

from __future__ import annotations

import os
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from rcmtools import architecture as arch
from rcmtools import harness

PROGRAM = "check-ui-shared-provider-imports"

UI_SRC = "ui/shared/src"

# The full Phase 6 platform list, including the four with no directory yet.
# Hard-coded on purpose; see the hazard note above.
PROVIDERS = (
    "ugos",
    "synology",
    "truenas",
    "unraid",
    "openmediavault",
    "proxmox",
    "portainer",
    "casaos",
    "zimaos",
    "dockge",
)

# Third-party SDK package names, matched against a bare module specifier.
# Separate from PROVIDERS because a specifier like "@ixsystems/..." names
# TrueNAS's vendor rather than the platform.
SDK_PATTERN = "ugreen|ugos|truenas|ixsystems|unraid|synology|openmediavault|proxmox|portainer|casaos|zimaos|dockge"

# Every form that can pull a module into a TypeScript file: static import,
# re-export, dynamic import, and require. The specifier is captured, and
# only the specifier is matched. `[ \t\n\r\f\v]` is POSIX `[[:space:]]`.
SPECIFIER_RE = re.compile(r"""(from|import|require)[ \t\n\r\f\v]*\(?[ \t\n\r\f\v]*["']([^"']+)["']""")

# A relative reach into a provider directory. Case-sensitive: it is a real
# directory name.
REACH_RE = re.compile(r"(^|/)apps/(" + "|".join(PROVIDERS) + r")(/|$)")

SDK_RE = re.compile(SDK_PATTERN, re.IGNORECASE)


def typescript_files(root: Path) -> list[str]:
    """Every .ts/.tsx regular file under ui/shared/src, repository-relative
    and sorted -- `find ui/shared/src -type f ... | sort`.

    Symlinks are skipped and symlinked directories are not descended, which
    is what `find` without `-L` does; see the hazard note.
    """
    base = root / UI_SRC
    found: list[str] = []
    for directory, _subdirs, names in os.walk(base):
        for name in names:
            if not name.endswith((".ts", ".tsx")):
                continue
            path = Path(directory) / name
            if path.is_symlink():
                continue
            found.append(path.relative_to(root).as_posix())
    return sorted(found)


def specifiers(path: Path) -> list[str]:
    """The module specifiers named on each line of `path`, in order.

    Line by line, because `grep` cannot match across a newline and this
    pattern's `[^"']+` would happily do so; see the hazard note.
    """
    text = path.read_text(encoding="utf-8", errors="replace")
    return [match.group(2) for line in text.split("\n") for match in SPECIFIER_RE.finditer(line)]


def body(root: Path) -> int:
    print(f"==> scanning {UI_SRC} for provider SDK imports ({len(PROVIDERS)} platforms)")

    findings: list[str] = []
    files = typescript_files(root)

    for rel in files:
        for spec in specifiers(root / rel):
            # A relative reach into a provider directory.
            if REACH_RE.search(spec):
                findings.append(f"  {rel}: imports {spec} (a relative reach into a provider directory)")
                continue
            # A bare third-party provider SDK.
            if spec.startswith((".", "/")):
                continue
            if SDK_RE.search(spec):
                findings.append(f"  {rel}: imports {spec} (a provider SDK package)")

    if findings:
        print(
            "FAIL: the shared UI imports provider-specific code. One shared UI, no provider SDKs in it "
            "(EPIC B #81):",
            file=sys.stderr,
        )
        for finding in findings:
            print(finding, file=sys.stderr)
        return harness.EXIT_FAILED

    # A scan that read no files must not report success: if ui/shared/src
    # moved or the walk stopped matching, "no provider import was found"
    # would be true and meaningless.
    if not files:
        print(
            f"FAIL: no TypeScript file was scanned under {UI_SRC}, so this check verified nothing.",
            file=sys.stderr,
        )
        return harness.EXIT_FAILED

    print(
        f"OK: no module specifier in {len(files)} file(s) under {UI_SRC} names a provider directory "
        "or a provider SDK."
    )
    return harness.EXIT_OK


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    if argv:
        print(f"usage: {sys.argv[0]}", file=sys.stderr)
        return harness.EXIT_USAGE
    root = arch.resolve_toplevel()
    return harness.finish(lambda: body(root))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
