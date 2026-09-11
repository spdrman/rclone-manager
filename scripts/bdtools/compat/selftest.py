#!/usr/bin/env python3
"""Positive controls for EPIC E's FR-35 compatibility gate (issue #242).

core/tests/compat is a wall of negative assertions: "config validation did
not move", "the artifact rows are what the migrations left", "the CLI
prints what it printed", "the contract lost nothing". A negative assertion
nobody has watched fail is indistinguishable from one that cannot fail, and
this repository has now found fifteen checks that passed for the wrong
reason, three of them on the day this gate was written: a rule that could
never fire because settled rows counted as throughput, a mutation that
disabled two mechanisms at once so a budget looked safe, and a guard that
matched a flag in the comment explaining the flag rather than in the
command.

So every cell in that corpus is mutation-tested against the real tree
here: a copy of the working tree gets one deliberate violation planted in
a real file, the gate runs, and it must fail AND name the cell whose
promise the violation broke. Naming the cell, not merely failing, is what
stops a mutation that broke the build for an unrelated reason from
reading as a pass. `scripts/bdtools/conformance/selftest.py` is the same
discipline for the composed half, and this is deliberately its sibling.

Two of the violations below are the ones docs/EPIC-E-alternative-storage.md
section 4 names by hand:

  "Compatibility (FR-35): a migration variant that rewrites
   retention_tier during backfill; the golden retention suite must fail
   it."

and the source-safety family's shape, applied to the one destructive path
that exists in this tree today: prune deleting a file it could not stat
instead of refusing.

This is not fast. Each mutant builds core/ and backupd and runs a
real capture, so budget a few minutes. It is the only thing standing
between "the compatibility suite is green" and "the compatibility suite
is green because it cannot go red".

Every mutation below is anchored to a verbatim copy of product source,
tabs and all, which means a refactor over there can leave an anchor here
naming code that is no longer in the tree. That is the third verdict,
STALE ANCHOR: the control is skipped, because a tree with nothing planted
in it would pass and reading that as a pass is the exact failure this
file exists to rule out, and the run carries on so one run names every
stale control instead of dying on the first (#458).

`python3 scripts/bdtools/compat/selftest.py --check-anchors` is that
check on its own: every anchor against the real tree, building nothing,
in about a second. `scripts/bdtools/selftest/check_anchors.py` runs it
for every selftest that has anchors.

Ported from `scripts/compat/selftest.sh` under EPIC I (#672 / #662).
`scripts/compat/selftest.sh` stays as a real, runnable shim:
`scripts/tests/ci-local-gate.test.sh` FABRICATES a stand-in at that
literal path, `scripts/ci-local.sh` still names it, and
`core/cmd/backupd/usagepins_test.go` and two files under
`core/tests/compat` cite it in their own comments. This file consumes
`scripts/bdtools/selftest_swap.py`'s `AnchorTracker` rather than
reimplementing anchor-matching a second time -- and rather than forking
it, which the module's own docstring names as the exact mistake #672 is
about, one language later, inside the very PR arguing against exactly
that duplication.

# PORTED-CHECK HAZARD NOTE

a go test failure silently discarded rather than read as "caught"
  hazard in bash:   every gate ran inside `if ... ; then ... else ... fi`,
                     so bash's own exit status was the only thing being
                     asked about; a typo that dropped the `if` would have
                     made a non-zero `go test` just keep running the
                     script under `set -euo pipefail`, dying loudly rather
                     than misreporting.
  hazard in python: STILL EXISTS in the shape the rest of this campaign
                     has already found: `subprocess.run(...)` returns a
                     `CompletedProcess` and propagates nothing on its own.
                     A helper that called `subprocess.run` and never read
                     `.returncode` would report every mutant as "caught"
                     whether the gate failed or not, and nothing would
                     raise to say so.
  held by:          `compat_gate`/`unit_gate` below always run with
                     `check=False` (the default) and hand their caller an
                     explicit `(returncode, output)` pair; every one of
                     `expect_gate_passes`, `expect_unit_gate_passes`,
                     `expect_cell_fails` and `expect_unit_check_fails`
                     branches on that pair before doing anything else.
                     None of these calls goes through `bdtools.harness`,
                     whose whole point is the opposite contract (raise on
                     an unguarded non-zero); a gate that is SUPPOSED to
                     fail cannot be run through a helper built to turn
                     failure into an exception.

a mutant that failed to compile reading as a caught violation
  hazard in bash:   `grep -qF "$cell"`/`grep -qF "$needle"` against the
                     combined output is the only thing standing between a
                     real catch and a mutant that simply does not build.
                     A cell name or needle picked too loosely (present in
                     a Go compiler error, say) would let a broken mutant
                     "pass" this check.
  hazard in python: STILL EXISTS, faithfully: `expect_cell_fails` and
                     `expect_unit_check_fails` do the identical substring
                     search and no more. #672's own rule is that a port
                     narrows or strengthens nothing quietly, so every
                     cell name and needle is carried over exactly as
                     chosen by whoever wrote the mutation, unexamined for
                     whether it could also match a build failure.
  held by:          nothing further than the substring itself, exactly
                     as inherited.

the anchor actually landing in the copy the gate then runs
  hazard in bash:   `_selftest_anchor`'s match-count discipline (exactly
                     once, or refuse) lived in `scripts/lib/selftest-swap.sh`,
                     sourced by this file and by scripts/conformance/selftest.sh
                     both; a refactor of that shared library could have
                     silently changed what "planted" means for every
                     caller at once.
  hazard in python: GONE as a duplication risk: there is exactly one
                     implementation, `AnchorTracker` in
                     `scripts/bdtools/selftest_swap.py`, and this module
                     calls `tracker.swap(...)`/`tracker.mutate(...)` for
                     every plant rather than reimplementing the
                     match-count rule.
  held by:          every mutation in `body()` below going through
                     `tracker.swap` or `tracker.mutate`, never a bare
                     file write -- `plant_migration` is the one exception,
                     and it is not an anchor: it appends a brand-new
                     migration file rather than editing existing source,
                     so there is no "old" text that could silently stop
                     matching. Its own hazard is below.

a COMPAT_UPDATE re-capture's own exit status masking the control it feeds
  hazard in bash:   `(cd "$d/core" && ... go test ... COMPAT_UPDATE=1 ...
                     >/dev/null 2>&1) || true` -- the `|| true` discards
                     the re-capture's status ON PURPOSE, because the
                     re-capture is not the proof; the `expect_cell_fails`
                     call straight after it is. A `set -e` without the
                     `|| true` would have killed the whole script the
                     first time a scoped re-capture legitimately found
                     nothing to update.
  hazard in python: the SAME discard, now indistinguishable at a glance
                     from the accidental kind this whole campaign keeps
                     finding (`two-machine-exit-status`, case 2). A reader
                     who does not already know the re-capture is meant to
                     be silent cannot tell this `check=False` with no
                     `.returncode` read apart from a bug.
  held by:          `_recapture` below never reads `.returncode` and says
                     so in its own docstring; the control that follows it
                     (`expect_cell_fails`) runs its own gate with its own
                     `compat_gate` call and reads THAT status, which is
                     the actual proof. The two are never the same
                     subprocess call.

`root` resolved from this file's own location rather than from the
current working directory
  hazard in bash:   `cd "$(git rev-parse --show-toplevel)"` -- resolved
                     from wherever the invoker's shell was standing, not
                     from where this script lives on disk.
  hazard in python: a DIFFERENT resolution rule, and this exact
                     difference is what #697 found in the `release`
                     port: a guard script that tests OTHER scripts
                     against throwaway git fixtures needs the cwd rule,
                     because its whole job is to be pointed at a fixture
                     that is not the real repository, and `repo_root(
                     Path(__file__))` would silently resolve to the real
                     dev checkout instead and validate the wrong tree.
                     This file is not that shape: it is the top-level
                     entry point itself, never invoked from inside
                     another script's fixture, and `root` here means
                     "the real repository this mutation campaign copies
                     FROM" -- which is exactly what `Path(__file__)`,
                     living inside that same real repository, resolves
                     to. `scripts/bdtools/conformance/selftest.py`,
                     already integrated and reviewed, resolves `root` the
                     identical way for the identical reason.
  held by:          `main()` below calling `harness.repo_root(Path(__file__))`
                     exactly as conformance's port does, and this note,
                     so the next reader does not have to re-derive why
                     that call is safe here after finding the release
                     defect.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import harness, selftest_swap

PROGRAM = "compat selftest"


def _env() -> dict[str, str]:
    return {**os.environ, "GOWORK": "off"}


def compat_gate(tree: Path) -> tuple[int, str]:
    """Run the FR-35 suite in `tree`."""
    proc = subprocess.run(
        ["go", "test", "-count=1", "./tests/compat/"],
        cwd=tree / "core",
        env=_env(),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    return proc.returncode, proc.stdout


def unit_gate(tree: Path, pkg: str, pattern: str = ".") -> tuple[int, str]:
    """One non-corpus package, for the controls whose claim does not live
    in core/tests/compat."""
    proc = subprocess.run(
        ["go", "test", "-count=1", "-run", pattern, pkg],
        cwd=tree / "core",
        env=_env(),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    return proc.returncode, proc.stdout


def _matched_line(out: str, needle: str) -> str:
    for line in out.splitlines():
        if needle in line:
            return line.strip()[:400]
    return ""


def expect_gate_passes(
    tracker: selftest_swap.AnchorTracker, tally: selftest_swap.Tally, label: str, tree: Path
) -> None:
    """The negative control, and the two early returns are what stop it
    from lying. A control whose anchor went stale planted nothing, so
    running the gate against that tree would report a clean pass for a
    mutation that never happened; and --check-anchors is not a run at
    all, so it must not report a verdict either."""
    if tracker.stale_verdict(label):
        return
    if tracker.anchors_only(label):
        return
    code, out = compat_gate(tree)
    if code == 0:
        tally.ok(f"clean: {label}")
    else:
        tally.bad(f"{label}. The gate FAILED against an unmutated tree, so its failures mean nothing.", out)


def expect_unit_gate_passes(
    tracker: selftest_swap.AnchorTracker,
    tally: selftest_swap.Tally,
    label: str,
    tree: Path,
    pkg: str,
    pattern: str = ".",
) -> None:
    """expect_gate_passes for the controls that read their verdict out of
    a package rather than out of the corpus. Same argument: a mutation
    that turns a suite red proves nothing about the mutation if the suite
    was red already."""
    if tracker.stale_verdict(label):
        return
    if tracker.anchors_only(label):
        return
    code, out = unit_gate(tree, pkg, pattern)
    if code == 0:
        tally.ok(f"clean: {label}")
    else:
        tally.bad(f"{label}. The package FAILED against an unmutated tree, so its failures mean nothing.", out)


def expect_cell_fails(
    tracker: selftest_swap.AnchorTracker,
    tally: selftest_swap.Tally,
    label: str,
    tree: Path,
    cell: str,
    quiet: str | None = None,
) -> None:
    """The cell name is the expected substring, so a mutation that broke
    the build, or broke a different promise, does not read as this
    control passing.

    `quiet`, when given, is for the one control where a cell staying
    QUIET is half of what is being claimed. A scoped re-capture has to
    have actually re-captured the cell it named, and "the other cell is
    still red" is equally what a scoped re-capture that silently did
    nothing at all would produce, so that control names both halves
    rather than one."""
    if tracker.stale_verdict(label):
        return
    if tracker.anchors_only(label):
        return
    code, out = compat_gate(tree)
    if code == 0:
        tally.bad(f"{label}. The FR-35 gate PASSED against a planted violation.", out)
    elif cell not in out:
        tally.bad(
            f"{label}. The gate failed, but never named the cell whose promise was broken.",
            f"expected its output to mention: {cell}\n{out}",
        )
    elif quiet and quiet in out:
        tally.bad(
            f"{label}. The gate named {quiet}, and this control requires that cell to have been dealt with already.",
            out,
        )
    else:
        tally.ok(f"caught: {label} -> {cell}")


def expect_unit_check_fails(
    tracker: selftest_swap.AnchorTracker,
    tally: selftest_swap.Tally,
    label: str,
    tree: Path,
    needle: str,
    pkg: str,
    pattern: str,
) -> None:
    """The migration-variant controls above read their verdict off the
    FR-35 corpus, because that is where their claim lives. FR-29's guard
    is different in one way that matters: "the backfill records only
    durable copies" is a claim about the placements table itself, and no
    medium-free surface renders one, so the corpus is exactly the wrong
    place to ask. Its verdict comes from internal/state's own
    both-directions test instead, or -- for the usage-pin controls --
    from cmd/backupd's own guard test."""
    if tracker.stale_verdict(label):
        return
    if tracker.anchors_only(label):
        return
    code, out = unit_gate(tree, pkg, pattern)
    if code == 0:
        tally.bad(f"{label}. {pkg} PASSED against a planted violation.", out)
    elif needle not in out:
        tally.bad(
            f"{label}. The package failed, but never named the promise that was broken.",
            f"expected its output to mention: {needle}\n{out}",
        )
    else:
        tally.ok(f"caught: {label}", _matched_line(out, needle))


def plant_migration(dir_: Path, body: str, *, dry_run: bool) -> None:
    """Write `body` as the NEXT migration in `dir_`, whichever number that
    is.

    These controls used to hardcode 0007, which worked right up until
    0007 was taken: EPIC E's placements migration landed on main and
    every migration-shaped control started failing for "migrations
    0007_placements and 0007_compat_selftest both claim version 7"
    instead of for the violation it planted. The gate still went red,
    which is why `expect_cell_fails` insists on the output naming the
    cell rather than accepting any red at all, and that insistence is the
    only reason this was caught instead of read as five passes.

    Under `--check-anchors` this is a no-op: `dir_` is the real tree
    itself in that mode (`AnchorTracker`/`selftest_swap.mutant` return it
    unchanged when nothing is being planted), and writing a new migration
    file into the real tree is not a "check", it is a mutation to the
    checkout somebody is standing in.
    """
    if dry_run:
        return
    migrations = dir_ / "core" / "migrations"
    numbers = []
    for path in migrations.glob("[0-9]*.sql"):
        prefix = path.name.split("_", 1)[0].lstrip("0")
        numbers.append(int(prefix) if prefix else 0)
    next_n = max(numbers) + 1
    out = migrations / f"{next_n:04d}_compat_selftest_planted_violation.sql"
    out.write_text(body + "\n")


def _recapture(tree: Path, cell: str | None, *, dry_run: bool) -> None:
    """`COMPAT_UPDATE=<cell or 1>` against `tree`, output and status both
    discarded on purpose -- see this module's PORTED-CHECK HAZARD NOTE.
    This is not the proof; the `expect_cell_fails` call straight after it
    is, and it runs its own separate `compat_gate` with its own status
    read."""
    if dry_run:
        return
    subprocess.run(
        ["go", "test", "-count=1", "-run", "TestMediumFreeSurfacesAreUnchanged", "./tests/compat/"],
        cwd=tree / "core",
        env={**_env(), "COMPAT_UPDATE": cell if cell else "1"},
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    )


def _drop_response_property(p: Path) -> None:
    doc = json.loads(p.read_text())
    schema = doc["components"]["schemas"]["Artifact"]
    assert (
        "retention_tier" in schema["properties"]
    ), "the Artifact schema no longer has the property this control removes"
    del schema["properties"]["retention_tier"]
    p.write_text(json.dumps(doc, indent=2) + "\n")


def _change_property_type(p: Path) -> None:
    doc = json.loads(p.read_text())
    schema = doc["components"]["schemas"]["Artifact"]
    schema["properties"]["size_bytes"] = {"type": "string"}
    p.write_text(json.dumps(doc, indent=2) + "\n")


def _add_required_request_field(p: Path) -> None:
    doc = json.loads(p.read_text())
    schema = doc["components"]["schemas"]["ApplyRetentionRequest"]
    schema.setdefault("properties", {})["acknowledge_medium_disclosure"] = {"type": "boolean"}
    schema.setdefault("required", []).append("acknowledge_medium_disclosure")
    p.write_text(json.dumps(doc, indent=2) + "\n")


def _add_required_query_parameter(p: Path) -> None:
    doc = json.loads(p.read_text())
    op = doc["paths"]["/activity"]["get"]
    op.setdefault("parameters", []).append(
        {"name": "medium", "in": "query", "required": True, "schema": {"type": "string"}}
    )
    p.write_text(json.dumps(doc, indent=2) + "\n")


def _serve_cost_figure(p: Path) -> None:
    doc = json.loads(p.read_text())
    doc["components"]["schemas"]["Artifact"]["properties"]["estimated_restore_cost_usd"] = {"type": "number"}
    p.write_text(json.dumps(doc, indent=2) + "\n")


def _serve_restore_eta(p: Path) -> None:
    doc = json.loads(p.read_text())
    props = doc["components"]["schemas"]["Artifact"]["properties"]
    props["restore_eta_seconds"] = {"type": "integer"}
    props["restore_percent_complete"] = {"type": "integer"}
    p.write_text(json.dumps(doc, indent=2) + "\n")


def _delete_corpus_cell(p: Path) -> None:
    doc = json.loads(p.read_text())
    del doc["cells"]["05-prune-verdicts"]
    p.write_text(json.dumps(doc, indent=2) + "\n")


def _empty_corpus_cell(p: Path) -> None:
    doc = json.loads(p.read_text())
    doc["cells"]["06-cli-surfaces"]["lines"] = []
    p.write_text(json.dumps(doc, indent=2) + "\n")


def body(root: Path, dry_run: bool) -> int:
    tally = selftest_swap.Tally()

    with tempfile.TemporaryDirectory(prefix="backupd-compat-selftest.") as tmp_name:
        tmp = Path(tmp_name)
        tracker = selftest_swap.AnchorTracker(dry_run=dry_run, root=root, tmp=tmp)

        def mutant(name: str) -> Path:
            return selftest_swap.mutant(root, tmp, name, dry_run=dry_run)

        print("==> negative control: the FR-35 gate is clean on the real tree")
        expect_gate_passes(tracker, tally, "core/tests/compat on an unmutated tree", root)

        print()
        print("==> the spec's own planted violation for FR-35")

        d = mutant("backfill-rewrites-retention-tier")
        # docs/EPIC-E-alternative-storage.md section 4, verbatim: "a
        # migration variant that rewrites retention_tier during backfill".
        # Written as the next migration in sequence, which is exactly
        # where the placements migration will land, so this is the
        # mistake in the place it would actually be made.
        plant_migration(
            d,
            "-- PLANTED VIOLATION (scripts/bdtools/compat/selftest.py). A backfill that helps\n"
            "-- itself to a column it does not own.\n"
            "UPDATE artifacts SET retention_tier = '';",
            dry_run=dry_run,
        )
        expect_cell_fails(tracker, tally, "a backfill that rewrites retention_tier", d, "10-upgraded-artifact-rows")

        d = mutant("backfill-rewrites-discovered-at")
        # The same shape aimed at the field FR-32 protects: an artifact's
        # retention bucketing must be invariant under anything a migration
        # or a move does to it, so rewriting discovery time has to move
        # the verdicts.
        plant_migration(
            d,
            "-- PLANTED VIOLATION (scripts/bdtools/compat/selftest.py). Discovery time is\n"
            "-- journal truth (FR-32); nothing may re-derive it from anywhere else.\n"
            "--\n"
            "-- The value moves the artifact into a different calendar month on\n"
            "-- purpose. A rewrite that lands in the same bucket changes the row and\n"
            "-- not the decision, and the decision is what this control is aimed at:\n"
            "-- the first draft shifted the timestamp by one minute, the verdicts did\n"
            "-- not move, and the control failed for being too gentle rather than the\n"
            "-- gate failing for being too weak.\n"
            "UPDATE artifacts SET discovered_at = '2026-08-29T09:00:00Z' WHERE artifact_name = 'monthly-only.dump';",
            dry_run=dry_run,
        )
        expect_cell_fails(
            tracker,
            tally,
            "a backfill that rewrites the journal's discovery timestamp",
            d,
            "11-upgraded-retention-verdicts",
        )

        expect_unit_gate_passes(tracker, tally, "core/internal/state on an unmutated tree", root, "./internal/state/")

        d = mutant("backfill-takes-every-artifact-row")
        # docs/EPIC-E-alternative-storage.md section 4, verbatim: "a
        # migration variant that backfills every artifact row". The spec
        # says beside it why that is a guard rather than a tidiness rule,
        # and it is worth repeating where the mutation lives: a `.partial`
        # holding an ACTIVE placement is what lets a move delete a source
        # against an incomplete copy.
        #
        # V9 of the matrix's ledger, and it had no row there at all until
        # #522. TestTheViolationLedgerHasARowPerSpecGuard is what found
        # that, by counting the ledger against the spec's own guard table
        # rather than trusting it.
        plant_migration(
            d,
            "-- PLANTED VIOLATION (scripts/bdtools/compat/selftest.py). A backfill that takes\n"
            "-- every artifact row rather than only the durable ones, so an in-flight\n"
            "-- .partial gets an ACTIVE placement pointing at a half-written file.\n"
            "INSERT INTO placements (artifact_id, medium, location, status, created_at, updated_at)\n"
            "SELECT a.id, 'local', COALESCE(a.local_path, ''), 'ACTIVE', a.discovered_at, a.discovered_at\n"
            "  FROM artifacts a\n"
            " WHERE a.id NOT IN (SELECT artifact_id FROM placements);",
            dry_run=dry_run,
        )
        expect_unit_check_fails(
            tracker,
            tally,
            "a backfill that takes every artifact row rather than only the durable ones",
            d,
            "a .partial in flight is not a durable one",
            "./internal/state/",
            "TestMigration0007BackfillsEveryDurableArtifactAndNothingElse",
        )

        print()
        print("==> the schema an upgrade inherits")

        d = mutant("migration-drops-an-index")
        plant_migration(
            d,
            "-- PLANTED VIOLATION (scripts/bdtools/compat/selftest.py). An upgrade may add to\n"
            "-- the schema; it may not take away from it.\n"
            "DROP INDEX idx_artifacts_state;",
            dry_run=dry_run,
        )
        expect_cell_fails(
            tracker, tally, "a migration that drops an index an upgrade inherits", d, "03-migrated-schema"
        )

        d = mutant("migration-adds-a-column-to-artifacts")
        # FR-29 decided placements live in a new table "rather than new
        # columns on the artifact row". This is that decision being
        # quietly reversed.
        plant_migration(
            d,
            "-- PLANTED VIOLATION (scripts/bdtools/compat/selftest.py). FR-29 put placements in\n"
            "-- their own table on purpose.\n"
            "ALTER TABLE artifacts ADD COLUMN placement_medium TEXT NOT NULL DEFAULT 'local';",
            dry_run=dry_run,
        )
        expect_cell_fails(
            tracker, tally, "a placement column bolted onto the artifact row", d, "02-artifact-rows-after-migration"
        )

        print()
        print("==> the decisions a medium-free deployment already makes")

        d = mutant("absent-medium-stops-meaning-local")
        tracker.swap(
            d / "core/internal/config/config.go",
            'func (t RetentionTier) EffectiveMedium() string {\n'
            '\tif t.Medium == "" {\n'
            "\t\treturn MediumLocal\n"
            "\t}\n"
            "\treturn t.Medium\n"
            "}",
            "func (t RetentionTier) EffectiveMedium() string {\n\treturn t.Medium\n}",
        )
        expect_cell_fails(
            tracker, tally, "a tier with no medium key resolving to no medium at all", d, "01-config-validation"
        )

        d = mutant("last-known-good-protection-dropped")
        tracker.swap(
            d / "core/internal/retention/lastknowngood.go",
            "\tif !lkg.Protected {\n\t\treturn verdicts\n\t}",
            "\tif true {\n\t\treturn verdicts\n\t}",
        )
        expect_cell_fails(
            tracker, tally, "FR-19 protection silently not composed onto the verdicts", d, "04-retention-verdicts"
        )

        d = mutant("prune-deletes-what-it-cannot-stat")
        tracker.swap(
            d / "core/internal/retention/prune.go",
            "\tinfo, err := os.Lstat(expected)\n"
            "\tif err != nil {\n"
            '\t\treturn "", fmt.Errorf("retention: prune: refusing %s: cannot stat %q: %w", '
            "rec.Artifact, expected, err)\n"
            "\t}\n"
            "\tif info.Mode()&os.ModeSymlink != 0 {",
            "\tinfo, err := os.Lstat(expected)\n"
            "\tif err != nil {\n"
            "\t\treturn expected, nil\n"
            "\t}\n"
            "\tif info.Mode()&os.ModeSymlink != 0 {",
        )
        expect_cell_fails(
            tracker, tally, "prune promoting a file it could not stat from REFUSE to DELETE", d, "05-prune-verdicts"
        )

        print()
        print("==> what an operator sees in a terminal")

        d = mutant("cli-renders-a-placement-line-unconditionally")
        # FR-35 allows an additive CLI column only when a non-local
        # placement exists. This is that column rendered on a deployment
        # that has none, which is the single most likely way EPIC E
        # breaks this clause.
        tracker.swap(
            d / "core/cmd/backupd/artifacts.go",
            '\tif rec.RetentionTier != "" {\n\t\tfmt.Printf("retention_tier:      %s\\n", rec.RetentionTier)\n\t}',
            '\tfmt.Printf("placements:          local\\n")\n'
            '\tif rec.RetentionTier != "" {\n'
            '\t\tfmt.Printf("retention_tier:      %s\\n", rec.RetentionTier)\n'
            "\t}",
        )
        expect_cell_fails(
            tracker, tally, "an additive CLI line rendered with no non-local placement anywhere", d, "06-cli-surfaces"
        )

        d = mutant("cli-retention-line-reshaped")
        # #239 gave this line a mediumSuffix() call, so the old edit no
        # longer matched and the control could not plant its violation.
        # It now mutates the function instead, which is closer to the
        # thing being guarded: the line is allowed to say medium= when
        # there IS one, and FR-35's promise is that a deployment naming
        # no medium sees exactly what it saw before.
        tracker.swap(
            d / "core/cmd/backupd/retention.go",
            "\tdefault:\n\t\treturn \"\"\n\t}\n}",
            '\tdefault:\n\t\treturn " medium=local"\n\t}\n}',
        )
        expect_cell_fails(
            tracker,
            tally,
            "the retention preview line growing a medium column for a local-only deployment",
            d,
            "07-cli-retention-preview",
        )

        d = mutant("cli-usage-line-reworded")
        # The usage block is compared additively so a new subcommand does
        # not force a regeneration. This is the other direction: a line
        # an operator already reads, quietly reworded.
        tracker.swap(
            d / "core/cmd/backupd/main.go",
            "  reconcile                                      run FR-17 reconciliation for every backup set",
            "  reconcile                                      reconcile every backup set",
        )
        expect_cell_fails(
            tracker, tally, "a usage line reworded under an operator who already read it", d, "06b-cli-usage-block"
        )

        print()
        print("==> the commands nothing pinned in the first place")

        # The other half of clause 4, and the reason #549 was filed. The
        # cell above only fails on a line the corpus already holds, so a
        # command whose usage lines were never captured is not protected
        # by it at all: `unconfigured` and `medium preflight` shipped
        # registered, listed and pinned nowhere, and 06b stayed green the
        # whole time because additive-only forgives a line that is merely
        # new.
        #
        # The guard that closes it lives in core/cmd/backupd rather
        # than in the corpus package, because the list of registered
        # commands is a map only package main can read, and reading the
        # map beats parsing the file that declares it. It holds three
        # real lists against each other, the map, the usage block this
        # build prints and the corpus, and there is one control per way
        # they can disagree.
        #
        # The first is the one #549 is about, and the thing worth knowing
        # while reading it is that core/tests/compat is GREEN against
        # exactly this mutant: `vacuum` is registered, listed in the
        # usage block and pinned nowhere, and every cell of that corpus
        # passes. That is the hole. This is the thing that sees it.
        expect_unit_gate_passes(
            tracker,
            tally,
            "the registered-command pin guard on an unmutated tree",
            root,
            "./cmd/backupd/",
            "TestUsage_EveryRegisteredCommandIsPinned",
        )

        d = mutant("command-registered-and-listed-but-never-pinned")
        tracker.swap(
            d / "core/cmd/backupd/main.go",
            '\t"version":      cmdVersion,\n}',
            '\t"version":      cmdVersion,\n\t"vacuum":       cmdVersion,\n}',
        )
        tracker.swap(
            d / "core/cmd/backupd/main.go",
            "  version                                        report version information",
            "  vacuum                                         compact the state database\n"
            "  version                                        report version information",
        )
        expect_unit_check_fails(
            tracker,
            tally,
            "a command registered and listed, with nothing pinning a word of what it prints",
            d,
            "is a registered command whose usage entry nothing pins",
            "./cmd/backupd/",
            "TestUsage_EveryRegisteredCommandIsPinned",
        )

        d = mutant("command-registered-and-never-listed")
        # The shape `backup-set remove` shipped in (#391), which
        # usage_test.go already catches for the backup-set verbs and,
        # since this batch, for the medium ones. catalog and quarantine
        # still dispatch against string literals, so nothing holds their
        # subcommands against usage.
        tracker.swap(
            d / "core/cmd/backupd/main.go",
            '\t"version":      cmdVersion,\n}',
            '\t"version":      cmdVersion,\n\t"vacuum":       cmdVersion,\n}',
        )
        expect_unit_check_fails(
            tracker,
            tally,
            "a command that is dispatchable and undiscoverable",
            d,
            "is registered in the commands map and the usage block does not list it",
            "./cmd/backupd/",
            "TestUsage_EveryRegisteredCommandIsPinned",
        )

        d = mutant("usage-lists-a-verb-nothing-dispatches")
        # And the same gap read the other way: the only reference an
        # operator has, offering them a command that answers "unknown
        # command".
        tracker.swap(
            d / "core/cmd/backupd/main.go",
            "  version                                        report version information",
            "  vacuum                                         compact the state database\n"
            "  version                                        report version information",
        )
        expect_unit_check_fails(
            tracker,
            tally,
            "a usage entry for a verb nothing dispatches",
            d,
            "and nothing dispatches it",
            "./cmd/backupd/",
            "TestUsage_EveryRegisteredCommandIsPinned",
        )

        print()
        print("==> what the /api/v1 contract already promises")

        d = mutant("contract-drops-a-response-property")
        tracker.mutate(d / "api/v1/openapi.json", _drop_response_property)
        expect_cell_fails(tracker, tally, "a response property taken off the contract", d, "08-api-contract-promises")

        d = mutant("contract-changes-a-property-type")
        tracker.mutate(d / "api/v1/openapi.json", _change_property_type)
        expect_cell_fails(
            tracker, tally, "a response property whose type changed under a client", d, "08-api-contract-promises"
        )

        d = mutant("contract-adds-a-required-request-field")
        # The one break additive-only cannot see on its own: adding
        # something is an addition, and this addition breaks every client
        # already sending the old shape.
        tracker.mutate(d / "api/v1/openapi.json", _add_required_request_field)
        expect_cell_fails(
            tracker,
            tally,
            "a new required field on a request every existing client already sends",
            d,
            "09-api-request-requirements",
        )

        d = mutant("contract-adds-a-required-query-parameter")
        # The parameter-shaped version of the same blind spot:
        # additive-only sees a new line and waves it through, and every
        # caller that was not already sending it starts getting a 400.
        tracker.mutate(d / "api/v1/openapi.json", _add_required_query_parameter)
        expect_cell_fails(
            tracker,
            tally,
            "a new required query parameter on an operation clients already call",
            d,
            "09-api-request-requirements",
        )

        print()
        print("==> the numbers FR-34 refuses to invent")

        d = mutant("contract-serves-a-cost-figure")
        tracker.mutate(d / "api/v1/openapi.json", _serve_cost_figure)
        expect_cell_fails(
            tracker, tally, "a cost figure on a public schema", d, "no surface renders a cost figure or an invented ETA"
        )

        d = mutant("contract-serves-a-restore-eta")
        tracker.mutate(d / "api/v1/openapi.json", _serve_restore_eta)
        expect_cell_fails(
            tracker,
            tally,
            "an invented restore ETA and a percentage S3 never reports",
            d,
            "no surface renders a cost figure or an invented ETA",
        )

        print()
        print("==> the invariant a regenerated corpus cannot silence")

        d = mutant("backfill-rewrites-retention-tier-and-corpus-regenerated")
        # The same backfill as above, with the corpus obligingly
        # re-captured around it, which is what somebody in a hurry does
        # to a red golden file.
        plant_migration(
            d,
            "-- PLANTED VIOLATION (scripts/bdtools/compat/selftest.py). The same backfill as\n"
            "-- above, with the corpus obligingly re-captured around it, which is what\n"
            "-- somebody in a hurry does to a red golden file.\n"
            "UPDATE artifacts SET retention_tier = '';",
            dry_run=dry_run,
        )
        _recapture(d, None, dry_run=dry_run)
        expect_cell_fails(
            tracker,
            tally,
            "the same backfill, with the corpus regenerated to accept it",
            d,
            "differs between a fresh install and an in-place upgrade",
        )

        # And the property the scoped form of that command has to have,
        # or it is just the sweep with extra typing (#549). Two unrelated
        # things move here: a usage line, deliberately, and a config
        # decision that has nothing to do with it. The re-capture names
        # the usage cell and only the usage cell, so the config cell has
        # to still be red afterwards. A COMPAT_UPDATE=1 in the same place
        # would have absorbed both and left a commit claiming to be about
        # wording carrying a changed validation outcome, which is the
        # laundering EPIC #536 came within one careful reviewer of
        # shipping.
        d = mutant("scoped-recapture-cannot-launder-another-cell")
        tracker.swap(
            d / "core/cmd/backupd/main.go",
            "  reconcile                                      run FR-17 reconciliation for every backup set",
            "  reconcile                                      run FR-17 reconciliation across the deployment",
        )
        tracker.swap(
            d / "core/internal/config/config.go",
            'func (t RetentionTier) EffectiveMedium() string {\n'
            '\tif t.Medium == "" {\n'
            "\t\treturn MediumLocal\n"
            "\t}\n"
            "\treturn t.Medium\n"
            "}",
            'func (t RetentionTier) EffectiveMedium() string {\n\treturn t.Medium\n}',
        )
        _recapture(d, "06b-cli-usage-block", dry_run=dry_run)
        expect_cell_fails(
            tracker,
            tally,
            "an unrelated cell, still red after a re-capture scoped to the one that moved",
            d,
            "01-config-validation",
            quiet="06b-cli-usage-block",
        )

        print()
        print("==> the matrix cannot claim a suite that is not there")

        d = mutant("matrix-cites-a-suite-that-does-not-exist")
        tracker.swap(
            d / "docs/conformance/epic-e-matrix.md",
            "`core/tests/compat`, twelve cells",
            "`core/tests/there-is-no-such-suite`",
        )
        expect_cell_fails(
            tracker,
            tally,
            "a PASS row citing a suite this repository does not have",
            d,
            "name something this repository does not have",
        )

        # This slot used to hold "every blocked row promoted to PASS",
        # against a floor in core/tests/compat that asserted SOME row was
        # still BLOCKED. The floor was correct when it was written and it
        # barred the finished state, so #522 replaced it, and the control
        # had to move with it: the mutation now has nothing to promote,
        # since every row earned its PASS.
        #
        # What replaced the floor is the promise the floor was standing
        # in for. The matrix carries one row per line of the spec's two
        # exit gates, and the row nobody can make green is the row most
        # worth deleting, so that is the mutation. P2.6 is the row in
        # question: it is the only one that is not PASS, its outcome is a
        # paragraph about which half of an archive claim can be run here,
        # and it is exactly the cell somebody tidying a table would take
        # out.
        #
        # It is planted by making the row invisible to the parser rather
        # than by cutting the line, and that is the same edit as far as
        # every check is concerned, with an anchor that survives the
        # row's prose being rewritten.
        d = mutant("matrix-drops-the-row-nobody-can-make-green")
        tracker.swap(
            d / "docs/conformance/epic-e-matrix.md",
            "| P2.6 | PARTIAL (the end-to-end archive half cannot be run here) |",
            "| ~~P2.6~~ | PARTIAL (the end-to-end archive half cannot be run here) |",
        )
        expect_cell_fails(
            tracker,
            tally,
            "the one row nobody can make green, dropped from the table",
            d,
            "it has to carry one row per line",
        )

        # And the spec's own checkboxes, in both directions, because the
        # drift #522 found ran both ways: every box in both exit gates
        # sat unticked long after phase 2 landed, and nothing in the
        # repository could tell.
        #
        # The optimistic direction first: a box ticked for a row that is
        # not PASS.
        d = mutant("spec-ticks-a-box-the-matrix-does-not-support")
        tracker.swap(
            d / "docs/EPIC-E-alternative-storage.md",
            "- [ ] An artifact on an archive class shows",
            "- [x] An artifact on an archive class shows",
        )
        expect_cell_fails(
            tracker,
            tally,
            "a spec box ticked for a row the matrix calls PARTIAL",
            d,
            "which claims more than anything has been watched to prove",
        )

        # And the direction that actually happened: a row is PASS,
        # watched, and the document somebody reads to find out where the
        # EPIC stands still says no.
        d = mutant("spec-leaves-a-passing-line-unticked")
        tracker.swap(
            d / "docs/EPIC-E-alternative-storage.md",
            "- [x] FR-35 holds:",
            "- [ ] FR-35 holds:",
        )
        expect_cell_fails(
            tracker,
            tally,
            "a spec box left unticked after its row earned its PASS",
            d,
            "the spec is the document somebody reads to find out where this EPIC stands",
        )

        print()
        print("==> the gate's own corpus")

        d = mutant("corpus-cell-deleted")
        # A cell quietly dropped from the corpus is how a gate shrinks to
        # nothing one commit at a time.
        tracker.mutate(d / "core/tests/compat/testdata/medium-free-surfaces.json", _delete_corpus_cell)
        expect_cell_fails(tracker, tally, "a corpus cell deleted rather than satisfied", d, "05-prune-verdicts")

        d = mutant("corpus-cell-emptied")
        tracker.mutate(d / "core/tests/compat/testdata/medium-free-surfaces.json", _empty_corpus_cell)
        expect_cell_fails(
            tracker, tally, "a corpus cell emptied so it passes whatever the product does", d, "06-cli-surfaces"
        )

    print()
    if dry_run:
        print(f"==> {tracker.anchors_checked} anchors checked, {tracker.anchors_stale} stale")
    else:
        print(f"==> {tally.passed} passed, {tally.failed} failed, {tracker.stale_count} stale")
    tracker.stale_summary()
    if tally.failed != 0 or tracker.stale_count != 0:
        return harness.EXIT_FAILED
    return harness.EXIT_OK


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    dry_run = selftest_swap.parse_selftest_args(argv, "selftest.py")
    root = harness.repo_root(Path(__file__))
    return harness.finish(lambda: body(root, dry_run))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
