"""The three-layer repository-structure checks (issue #106 WP1.1, #165,
spec §7.1 "Dependency enforcement"), under EPIC I (#672 / #697).

Ported from `scripts/architecture/lib.sh`, which every check in that
directory sourced. The bash paths all remain as `exec` shims: they are
named by `scripts/ci-local.sh`, by `.github/workflows/ci.yml`, and --
load-bearingly -- FABRICATED at their literal paths by
`scripts/tests/ci-local-gate.test.sh`, which stubs all ten of them in its
synthetic-tree fixture. Deleting any of those paths turns that fixture
red. `scripts/architecture/ownership.go` stays exactly where it is and
exactly what it is, a standalone module-less Go program invoked with
`GOWORK=off go run`, for the same reason `scripts/api/gen-bindings.go`
did: the thing it detects is a Go *declaration*, and only a Go parser can
tell a declaration from a comment.

Two kinds of check live in this domain, and the difference matters:

  * STATIC checks (`check_layer_manifest`, `check_core_dependency_rule`,
    `check_layer_ownership`, `check_ui_shared_provider_imports`,
    `check_unowned_go`) inspect the WORKING TREE, so a contributor sees
    the failure before committing.
  * DELETION PROOFS (`verify_core_without_apps`,
    `verify_core_without_distribution`,
    `verify_ui_shared_without_provider_sdks`, `verify_ugos_removable`)
    check out HEAD into a throwaway worktree, delete the directory the
    boundary claim says should not matter, and run the real build there.
    A static import scan can be fooled by a path built at runtime or a
    fixture reaching across the boundary; an actual deletion cannot.
    They necessarily prove a property of HEAD, not of your edits, which
    is what `warn_if_dirty` exists to say out loud.

# PORTED-CHECK HAZARD NOTE

This module makes no assertion of its own; it is the vocabulary the ten
checks assert THROUGH. That makes its hazards worse than theirs, because
a hole here fails open in ten places at once and each of them still
prints its own cheerful OK line. `check_layer_manifest` has caught two
real defects in one day (#686's unclassified CONTRIBUTING.md and this
programme's own CHANGELOG.md), and both were caught by exactly the code
paths below.

  manifest_rows() drops no row bash kept
    hazard in bash:   `sed 's/#.*//' | awk 'NF >= 3 { print $1, $2, $3 }'`
                       -- strip from the first `#` to end of line
                       (INLINE comments included, not only whole-line
                       ones), split on runs of whitespace, keep any row
                       with three or more fields, and read only the first
                       three.
    hazard in python: LIVE, and it is the nastiest hazard in this port,
                       because every way of getting it wrong fails OPEN:
                         * skipping only lines that START with `#` leaves
                           an inline comment's words in the row;
                         * requiring EXACTLY three fields silently drops
                           a four-field row that bash accepted, and a
                           dropped row is an UNCHECKED PATH -- worse, a
                           dropped `distribution adapter` row makes
                           verify_core_without_distribution delete
                           nothing and pass vacuously;
                         * `str.split()` with no argument is required:
                           `split(" ")` yields empty fields for runs of
                           spaces and for tab-separated rows, which turns
                           a valid row into a malformed one.
                       Closed by doing all three the bash way, verbatim.
    held by:          selftest.py's manifest arms, and directly by
                       check_layer_manifest's own count line: the number
                       of classified files is printed, so a row that
                       stopped being read shows up as files that stopped
                       being classified.

  classify() requires a path SEPARATOR, and longest match wins
    hazard in bash:   `p == entry || index(p, entry "/") == 1`, with
                       `length(entry) > best_len` keeping the FIRST of
                       two equal-length matches.
    hazard in python: LIVE. A bare `path.startswith(entry)` classifies
                       `core-tools/x.go` as `core`, which is the exact
                       shape of a new product directory hiding inside an
                       existing classification -- the blind spot
                       layers.conf's own header says the completeness
                       guard must not have. `>` not `>=` is load bearing
                       too: flipping it changes which of two equal-length
                       entries wins, silently reclassifying files.
    held by:          selftest.py's specificity arm (a longer entry must
                       beat the directory it sits in) and its
                       separator arm.

  manifest_path_problem() is TEXTUAL, never resolved
    hazard in bash:   four `case` globs -- empty, absolute, `.`/`..`, and
                       any `..` segment.
    hazard in python: LIVE, and tempting to get wrong: a port using
                       `Path.resolve()` or `os.path.normpath` ACCEPTS
                       `apps/../apps/unraid` because the `..` cancels
                       out, and accepts a symlink that leaves the tree.
                       The bash rejected the SHAPE, on purpose, because
                       `verify_core_without_distribution` hands every
                       `distribution adapter` entry to `rm -rf`: a
                       reviewed manifest line is the only thing standing
                       between that and a delete nobody looked at.
                       Closed by keeping the four textual cases.
    held by:          selftest.py's manifest-shape arms, one per case.

  every check propagates its own exit status
    hazard in bash:   none; `set -euo pipefail` did it.
    hazard in python: LIVE, and this is the domain-wide one. Each bash
                       path is now an `exec` shim over a module, so there
                       are two places a status can be dropped: the shim
                       (fixed by `exec`) and the module (fixed by
                       `harness.finish` plus `harness.sh`'s default
                       `check=True`). A dropped status here prints the
                       real refusal text and exits 0 -- green output, no
                       check.
    held by:          selftest.py drives the SHIMS, not the modules, so
                       both halves are exercised by every control.
"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from bdtools import harness

MANIFEST = "scripts/architecture/layers.conf"

LAYERS = ("core", "platform", "distribution", "infrastructure")
DISTRIBUTION_KINDS = ("adapter", "canonical")


@dataclass(frozen=True)
class Row:
    """One manifest row: `<layer> <kind> <path>`."""

    layer: str
    kind: str
    path: str


def manifest(root: Path | str = ".") -> Path:
    """The manifest's path. `arch::manifest` printed it relative to the
    repository root and every caller joined it onto one; this joins for
    them, and `MANIFEST` is still the bare relative string for the
    messages that quote it."""
    return Path(root) / MANIFEST


def manifest_rows(root: Path | str = ".") -> list[Row]:
    """The manifest with comments and blank lines stripped.

    A verbatim port of `sed 's/#.*//' | awk 'NF >= 3 { print $1, $2, $3 }'`
    -- see this module's hazard note for why each of the three steps is
    exactly as it is, and what fails open if any of them is 'improved'.
    """
    text = manifest(root).read_text(encoding="utf-8")
    rows: list[Row] = []
    for line in text.splitlines():
        # `sed 's/#.*//'`: from the FIRST `#` to end of line, wherever it
        # is. An inline trailing comment is not part of the row.
        uncommented = line.split("#", 1)[0]
        # `awk`'s default field splitting: runs of any whitespace, no
        # empty fields. `split()` with no argument, never `split(" ")`.
        fields = uncommented.split()
        # `NF >= 3`, not `== 3`: a row with more fields is still a row,
        # and dropping it would stop checking the path it names.
        if len(fields) >= 3:
            rows.append(Row(fields[0], fields[1], fields[2]))
    return rows


def layer_paths(layer: str, kind: str = "any", root: Path | str = ".") -> list[str]:
    """The manifest paths in `layer`, optionally narrowed to one `kind`.
    A kind of `"any"` or `""` means every kind, as in the bash."""
    return [r.path for r in manifest_rows(root) if r.layer == layer and kind in ("any", "", r.kind)]


def classify(path: str, root: Path | str = ".") -> Row | None:
    """The manifest row owning a repository-relative `path`, or `None`.

    The LONGEST matching entry wins, so a specific entry beats the
    directory it sits in, and a match requires either equality or a `/`
    immediately after the entry. `None` is what `check_layer_manifest`
    turns into a completeness failure.
    """
    best: Row | None = None
    best_len = 0
    for row in manifest_rows(root):
        entry = row.path
        if path == entry or path.startswith(entry + "/"):
            # `>`, not `>=`: the FIRST of two equal-length matches wins,
            # exactly as the awk did.
            if len(entry) > best_len:
                best_len = len(entry)
                best = row
    return best


def manifest_path_problem(path: str) -> str | None:
    """Why `path` is unsafe as a manifest entry, or `None` if it is fine.

    Textual, never resolved -- see this module's hazard note. Every
    reader joins an entry onto a directory and hands the result to a real
    filesystem operation, and `verify_core_without_distribution` hands
    the `distribution adapter` ones to `rm -rf`.
    """
    if path == "":
        return "is empty"
    if path.startswith("/"):
        return "is absolute, and manifest paths are relative to the repository root"
    if path in (".", ".."):
        return f'is "{path}", which names the repository or its parent rather than something in it'
    # The bash globs `../*|*/../*|*/..` -- a ".." segment anywhere, but
    # NOT a filename that merely begins with two dots (`..gitignore`) and
    # not `a..b`. Splitting on "/" asks exactly that question.
    if ".." in path.split("/"):
        return 'contains a ".." segment, which escapes the repository'
    return None


def warn_if_dirty(root: Path | str = ".") -> None:
    """Say out loud that a deletion proof checks HEAD, not your edits.

    Every proof that builds a worktree proves a property of the last
    commit, so an uncommitted change -- including one that violates the
    very boundary being checked -- is invisible to it.
    """
    if harness.sh_out(["git", "-C", str(root), "status", "--porcelain"], check=False).strip():
        print(
            "WARNING: working tree has uncommitted changes; this check only proves the property "
            "against the last commit (HEAD), not your uncommitted edits.",
            file=sys.stderr,
            flush=True,
        )


def tracked_files(root: Path | str = ".") -> list[str]:
    """`git ls-files`, which is the question the completeness guard asks.

    Not a filesystem walk: what matters is what the REPOSITORY contains,
    not what happens to be lying in the working tree (a node_modules, a
    build output, a scratch file).
    """
    out = harness.sh_out(["git", "-C", str(root), "ls-files"])
    return [line for line in out.splitlines() if line]


@contextmanager
def worktree(root: Path | str = ".") -> Iterator[Path]:
    """A detached worktree of HEAD in a fresh temp directory.

    `--detach`, because this proves a property of the current tree rather
    than making a branch anyone will commit on. Removal tolerates the
    check's own `rm -rf` having already deleted files git expects to
    find, which is the normal case here: deleting the tree under test is
    the whole point.
    """
    warn_if_dirty(root)
    directory = Path(tempfile.mkdtemp(prefix="backupd-arch-check.", dir=os.environ.get("TMPDIR", "/tmp")))
    harness.sh(["git", "-C", str(root), "worktree", "add", "--quiet", "--detach", str(directory), "HEAD"])
    try:
        yield directory
    finally:
        if not harness.sh_ok(["git", "-C", str(root), "worktree", "remove", "--force", str(directory)]):
            subprocess.run(["rm", "-rf", str(directory)], check=False)
        harness.sh_ok(["git", "-C", str(root), "worktree", "prune"])


def resolve_toplevel() -> Path:
    """`git rev-parse --show-toplevel` of the CURRENT WORKING DIRECTORY,
    not of this file's own location -- the Python equivalent of the
    `cd "$(git rev-parse --show-toplevel)"` every script in this domain
    opened with.

    These checks prove things about whatever repository they are invoked
    from: a mutant copy of the tree built by `selftest.py`, or a real
    checkout. Resolving the target from `__file__` would silently check
    the developer's real workspace instead, print plausible refusals
    about the wrong repository, and make every selftest control vacuous.
    That is not hypothetical: it is the defect the release domain's port
    was caught on an hour before this one was written.
    """
    try:
        return Path(harness.sh_out(["git", "rev-parse", "--show-toplevel"]))
    except (harness.CommandFailed, harness.Failure):
        print(f"{harness.program()}: not inside a git work tree", file=sys.stderr)
        raise SystemExit(harness.EXIT_USAGE) from None
