#!/usr/bin/env python3
"""Positive controls for EPIC E's composed conformance scenario (issue #242).

core/tests/conformance is a wall of assertions about a chain that mostly
works: the artifact ends up on the medium the chain named, the invariant
held at every instant, the crash cells converged, the bytes hash right. An
assertion nobody has watched fail is indistinguishable from one that cannot
fail, and this repository has now found more than fifteen checks that
passed for the wrong reason.

So every claim that suite makes is mutation-tested here against the real
tree: a copy of the working tree gets one deliberate violation planted in a
real product file, the suite runs, and it must fail AND name the check
whose promise the violation broke. Naming the check, not merely failing,
is what stops a mutation that broke the build for an unrelated reason from
reading as a pass. `scripts/bdtools/compat/selftest.py` is the same
discipline for the FR-35 half, and this is deliberately its sibling.

Two of the violations below are named by the conformance matrix itself:

  P2.1: "an engine that lets both placements be non-verified at the same
         instant. The harness has to observe it at that instant, which is
         why 'continuously' is in the gate line and why sampling would not
         do."

  P2.2 / V1: "a mutation that issues the source delete before VERIFIED is
         durably recorded."

They are the same planted edit here, because in this engine they are the
same mistake seen from two sides.

This is not fast. Each mutant rebuilds core/ and runs a suite that stands
up MinIO containers, so budget ten minutes. It is the only thing standing
between "the composed scenario is green" and "the composed scenario is
green because it cannot go red".

Every mutation below is anchored to a verbatim copy of product source,
tabs and all, which means a refactor over there can leave an anchor here
naming code that is no longer in the tree. #437 did exactly that to the
crash-matrix anchor. That is the third verdict, STALE ANCHOR: the control
is skipped, because a tree with nothing planted in it would pass and
reading that as a pass is the exact failure this file exists to rule out,
and the run carries on so one run names every stale control instead of
dying on the first (#458).

`python3 scripts/bdtools/conformance/selftest.py --check-anchors` is that
check on its own: every anchor against the real tree, building nothing, in
about a second. `scripts/bdtools/selftest/check_anchors.py` runs it for
every selftest that has anchors.

Ported from `scripts/conformance/selftest.sh` under EPIC I (#672 / #662).
`scripts/conformance/selftest.sh` stays as a real, runnable shim:
`scripts/tests/ci-local-gate.test.sh` FABRICATES a stand-in at that
literal path, and `scripts/ci-local.sh` still names it. This file consumes
`scripts/bdtools/selftest_swap.py`'s `AnchorTracker` rather than
reimplementing anchor-matching a second time; see that module's own
docstring for why a private copy here would be the exact mistake #672 is
about.

# PORTED-CHECK HAZARD NOTE

a go test failure silently discarded rather than read as "caught"
  hazard in bash:   every control ran its gate inside `if ... ; then
                     ... else ... fi`, so bash's own exit status was the
                     only thing being asked about, and a typo that dropped
                     the `if` would have made a non-zero `go test` just
                     keep running the script under `set -e`, dying loudly
                     rather than misreporting -- bash has no way to run a
                     command AND continue AND forget its status.
  hazard in python: STILL EXISTS in the shape the rest of this campaign
                     has already found: `subprocess.run(...)` returns a
                     `CompletedProcess` and propagates nothing on its own.
                     A helper that called `subprocess.run` and then never
                     read `.returncode` would report every mutant as
                     "caught" whether the suite failed or not, and nothing
                     would raise to say so -- the exact class of bug
                     `two-machine-exit-status` case 2 found.
  held by:          `conformance_gate`/`unit_gate`/`unit_gate_multi` below
                     always run with `check=False` and hand their caller
                     an explicit `(returncode, output)` pair; every one of
                     `expect_gate_passes`, `expect_unit_gate_passes`,
                     `expect_check_fails` and `expect_unit_check_fails`
                     branches on that `returncode` before doing anything
                     else. None of these calls goes through
                     `bdtools.harness.sh`, whose whole point is the
                     opposite contract (raise on an unguarded non-zero);
                     a gate that is SUPPOSED to fail cannot be run through
                     a helper that turns failure into an exception without
                     first catching that exception and losing the
                     distinction between "the suite failed" and "the
                     harness call itself broke".

a mutant that failed to compile reading as a caught violation
  hazard in bash:   `grep -qF "$needle"` against the combined output is
                     the only thing standing between a real catch and a
                     mutant that simply does not build. A needle picked
                     too loosely (present in a Go compiler error, say)
                     would let a broken mutant "pass" this check.
  hazard in python: STILL EXISTS, faithfully: `expect_check_fails` and
                     `expect_unit_check_fails` do the identical substring
                     search and no more. Narrowing it further is out of
                     scope for a port: #672's own rule is that a port
                     narrows or strengthens nothing quietly, so each
                     needle is carried over exactly as chosen by whoever
                     wrote the mutation, unexamined for whether it could
                     also match a build failure.
  held by:          nothing further than the needle text itself, exactly
                     as inherited.

the anchor actually landing in the copy the gate then runs
  hazard in bash:   `_selftest_anchor`'s match-count discipline (exactly
                     once, or refuse) lived in `scripts/lib/selftest-swap.sh`,
                     sourced by this file among others; a refactor of that
                     shared library could have silently changed what
                     "planted" means for every caller at once.
  hazard in python: GONE as a duplication risk, though not as a fact this
                     file must get right: there is exactly one
                     implementation, `AnchorTracker` in
                     `scripts/bdtools/selftest_swap.py`, and this module
                     calls `tracker.swap(...)` for every plant rather than
                     reimplementing the match-count rule. See that
                     module's own hazard note for the anchor-matching
                     mechanics themselves.
  held by:          every mutation in `body()` below going through
                     `tracker.swap`, never a bare file write.
"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import harness, selftest_swap

PROGRAM = "conformance selftest"


def _env() -> dict[str, str]:
    return {**os.environ, "GOWORK": "off"}


def conformance_gate(tree: Path, pattern: str = ".") -> tuple[int, str]:
    """Run the composed suite in `tree`, narrowed to `pattern`. `-run`
    narrows it to the checks a mutation is expected to move, per call, so
    a ten-minute self-test does not become an hour one."""
    proc = subprocess.run(
        ["go", "test", "-count=1", "-timeout", "30m", "-run", pattern, "./tests/conformance/"],
        cwd=tree / "core",
        env=_env(),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    return proc.returncode, proc.stdout


def unit_gate(tree: Path, pkg: str, pattern: str = ".") -> tuple[int, str]:
    """One non-composed package, for the halves of a claim that do not
    live in tests/conformance. -timeout 30m rather than go test's default
    10: one of the phase 1 controls runs ./tests/miniointegration/, which
    stands up a container before it asserts anything, and a control that
    died on a timeout would report "the suite failed but never named the
    promise", which is the same verdict as a broken anchor for a
    completely different reason."""
    proc = subprocess.run(
        ["go", "test", "-count=1", "-timeout", "30m", "-run", pattern, pkg],
        cwd=tree / "core",
        env=_env(),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    return proc.returncode, proc.stdout


def unit_gate_multi(tree: Path, pkgs: list[str]) -> tuple[int, str]:
    proc = subprocess.run(
        ["go", "test", "-count=1", "-timeout", "30m", *pkgs],
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
    """The negative control, and the two early returns are the part that
    matters. A control whose anchor no longer matches planted nothing, so
    running the suite against it would report a clean pass for a mutation
    that never happened; and under --check-anchors nothing is built at
    all, so there is no verdict to give."""
    if tracker.stale_verdict(label):
        return
    if tracker.anchors_only(label):
        return
    code, out = conformance_gate(tree)
    if code == 0:
        tally.ok(f"clean: {label}")
    else:
        tally.bad(f"{label}. The suite FAILED against an unmutated tree, so its failures mean nothing.", out)


def expect_unit_gate_passes(
    tracker: selftest_swap.AnchorTracker, tally: selftest_swap.Tally, label: str, tree: Path, pkgs: list[str]
) -> None:
    """expect_gate_passes for the unit-level controls, and it exists for
    exactly the same reason: a mutation that turns a suite red proves
    nothing about the mutation if the suite was red already."""
    if tracker.stale_verdict(label):
        return
    if tracker.anchors_only(label):
        return
    code, out = unit_gate_multi(tree, pkgs)
    if code == 0:
        tally.ok(f"clean: {label}")
    else:
        tally.bad(f"{label}. The suites FAILED against an unmutated tree, so their failures mean nothing.", out)


def expect_check_fails(
    tracker: selftest_swap.AnchorTracker,
    tally: selftest_swap.Tally,
    label: str,
    tree: Path,
    needle: str,
    pattern: str = ".",
) -> None:
    if tracker.stale_verdict(label):
        return
    if tracker.anchors_only(label):
        return
    code, out = conformance_gate(tree, pattern)
    if code == 0:
        tally.bad(f"{label}. The composed suite PASSED against a planted violation.", out)
    elif needle not in out:
        tally.bad(
            f"{label}. The suite failed, but never named the promise that was broken.",
            f"expected its output to mention: {needle}\n{out}",
        )
    else:
        tally.ok(f"caught: {label}", _matched_line(out, needle))


def expect_unit_check_fails(
    tracker: selftest_swap.AnchorTracker,
    tally: selftest_swap.Tally,
    label: str,
    tree: Path,
    needle: str,
    pkg: str,
    pattern: str,
) -> None:
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


def body(root: Path, dry_run: bool) -> int:
    tally = selftest_swap.Tally()

    with tempfile.TemporaryDirectory(prefix="backupd-conformance-selftest.") as tmp_name:
        tmp = Path(tmp_name)
        tracker = selftest_swap.AnchorTracker(dry_run=dry_run, root=root, tmp=tmp)

        def mutant(name: str) -> Path:
            return selftest_swap.mutant(root, tmp, name, dry_run=dry_run)

        print("==> negative control: the composed suite is clean on the real tree")
        expect_gate_passes(tracker, tally, "core/tests/conformance on an unmutated tree", root)

        print()
        print("==> EPIC E's phase 1 exit lines (P1.3, P1.4, P1.5, P1.7, P1.8, V4, V8)")

        # Seven rows of docs/conformance/epic-e-matrix.md sat BLOCKED long
        # after the code they certify had landed, and the reason was never
        # missing product code. Each of these violations was planted ONCE,
        # by hand, in the PR that landed it (#369 for the backend set, the
        # MediumStore contract and the credential canary; #383 for the
        # verification-honesty half), and this matrix's own definition of
        # PASS asks for one that lives in a self-test and runs every time
        # instead. Nobody owned that, EPIC E was closed with the rows
        # still BLOCKED, and #522 is the repair.
        #
        # They live here rather than in scripts/bdtools/compat/selftest.py
        # because that script is the FR-35 corpus and none of these is a
        # corpus cell. Six of the seven are unit gates, so they cost a
        # build each and no container at all; the seventh is the half of
        # P1.4 that only a real S3 endpoint can answer, and it stands up
        # MinIO the same way the composed cells above do.

        # The negative control for the six unit controls, and it is the
        # same argument the composed one at the top of this file makes: a
        # mutation that turns a suite red proves nothing if the suite was
        # red already.
        expect_unit_gate_passes(
            tracker,
            tally,
            "the phase 1 exit-line suites on an unmutated tree",
            root,
            ["./internal/transport/...", "./internal/placement/", "./internal/revalidate/"],
        )

        # P1.3, FR-4's "each backend is an architecture decision, not an
        # import line". rclone registers backends through package-level
        # init, so the set this binary supports is decided by blank
        # imports anywhere in the graph and is written down in exactly one
        # place. A fourth one arriving without that line is the whole
        # failure, and it is planted as a real blank import rather than by
        # editing the expectation, because editing the expectation is
        # what the check is FOR.
        d = mutant("phase1-a-fourth-rclone-backend-registered")
        tracker.swap(
            d / "core/internal/transport/rclone/adapter.go",
            '\t_ "github.com/rclone/rclone/backend/local"\n'
            '\t_ "github.com/rclone/rclone/backend/s3"\n'
            '\t_ "github.com/rclone/rclone/backend/sftp"',
            '\t_ "github.com/rclone/rclone/backend/local"\n'
            '\t_ "github.com/rclone/rclone/backend/s3"\n'
            '\t_ "github.com/rclone/rclone/backend/sftp"\n'
            "\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): a fourth\n"
            "\t// backend, registered with no line in backends.go admitting it.\n"
            '\t_ "github.com/rclone/rclone/backend/memory"',
        )
        expect_unit_check_fails(
            tracker,
            tally,
            "a fourth rclone backend registered with nothing in backends.go admitting it",
            d,
            "registered rclone backends changed",
            "./internal/transport/rclone/",
            "TestRegisteredBackendsExactSet",
        )

        # P1.4's literal falsification, at the ladder: an `attested`
        # request silently degrading to `existence`. placement.Verify runs
        # exactly the class it was asked for and returns exactly the class
        # it ran, and the endpoint this product actually ships against
        # cannot attest at all, so a fallback here would turn "we could
        # not check" into "we checked" on every s3 medium in existence.
        d = mutant("phase1-attested-request-falls-back-to-existence")
        tracker.swap(
            d / "core/internal/placement/ladder.go",
            "\tattestation, err := store.ObjectChecksum(ctx, medium, p.Location, transport.SHA256)\n"
            "\tif err != nil {\n"
            '\t\treturn Result{}, fmt.Errorf("%w: %q cannot attest a full-object %s for %q: %w",\n'
            "\t\t\tErrClassUnavailable, p.Medium, transport.SHA256, p.Location, err)\n"
            "\t}",
            "\tattestation, err := store.ObjectChecksum(ctx, medium, p.Location, transport.SHA256)\n"
            "\tif err != nil {\n"
            "\t\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): the\n"
            "\t\t// endpoint cannot attest, so run the class it can and report\n"
            "\t\t// that instead of refusing.\n"
            "\t\treturn verifyExistence(ctx, store, medium, p, now)\n"
            "\t}",
        )
        expect_unit_check_fails(
            tracker,
            tally,
            "an attested request that quietly runs an existence check instead",
            d,
            "Verify fell back and returned",
            "./internal/placement/",
            "TestVerifyNeverFallsBack",
        )

        # P1.4 at the boundary the row cites, in-tree against rclone's
        # local backend. This boundary attests SHA-256 and nothing else,
        # so an md5 ask has to be an explicit capability refusal;
        # answering it with the digest the backend happens to hold is the
        # same silent degrade one rung lower, and it is the shape FR-32
        # was written about, because the digest an S3 endpoint hands back
        # for free is an ETag's MD5.
        d = mutant("phase1-medium-checksum-answers-an-algorithm-it-cannot-speak")
        tracker.swap(
            d / "core/internal/transport/rclone/medium.go",
            "\tif alg != transport.SHA256 {\n"
            '\t\treturn transport.ChecksumAttestation{}, WrapCtx(ctx, "object_checksum", fmt.Errorf(\n'
            "\t\t\t\"%w: this boundary attests %s and nothing else, so an ETag's MD5 can never be "
            'compared to a recorded hash: %q",\n'
            "\t\t\tErrUnsupportedHash, transport.SHA256, alg))\n"
            "\t}",
            "\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): answer the ask\n"
            "\t// with whatever digest this backend does have, rather than refusing\n"
            "\t// the one it cannot serve.\n"
            "\t_ = alg",
        )
        expect_unit_check_fails(
            tracker,
            tally,
            "a checksum ask this boundary cannot serve, answered instead of refused",
            d,
            "an MD5 from an ETag is exactly what FR-32 forbids comparing to it",
            "./internal/transport/rclone/",
            "TestRcloneAdapter_LocalBackend_MediumContractSuite",
        )

        # P1.5 and V4, FR-33's own planted violation: a build that logs
        # the resolved medium config verbatim. The canary is a value that
        # exists nowhere else in this repository, so the scan that looks
        # for it in every output FR-33 names is a real scan; what it
        # needed was a build that actually leaks, planted in the product
        # rather than inside the test that reads the log.
        d = mutant("phase1-resolved-credentials-logged-verbatim")
        tracker.swap(
            d / "core/internal/transport/rclone/mediumcreds.go",
            "\tif resolved.hasSessionToken {\n"
            '\t\tcfg.Set("session_token", resolved.sessionToken.Reveal())\n'
            "\t}",
            "\tif resolved.hasSessionToken {\n"
            '\t\tcfg.Set("session_token", resolved.sessionToken.Reveal())\n'
            "\t}\n"
            "\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): FR-33's own,\n"
            "\t// a build that logs the resolved medium config verbatim.\n"
            '\tslog.Info("resolved medium config", "medium", medium.ID, "options", fmt.Sprintf("%v", cfg))',
        )
        expect_unit_check_fails(
            tracker,
            tally,
            "a build that logs the resolved medium config verbatim",
            d,
            "the canary reached an observable output",
            "./internal/transport/rclone/",
            "TestMediumCredentialCanary$",
        )

        # P1.7 and V8, FR-31's verification honesty. The pass HEADs the
        # object, which is placement.Existence and is the automatic
        # ceiling; reporting that run as content verification is a claim
        # about bytes nobody read, and it is the claim an operator six
        # months later is reading out of the journal.
        d = mutant("phase1-revalidation-claims-content-verification")
        tracker.swap(
            d / "core/internal/revalidate/checks.go",
            "\treturn true, anyPassed, automatic, detail, nil\n}",
            "\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): a HEAD,\n"
            "\t// reported as content verification.\n"
            "\treturn true, anyPassed, placement.Content, detail, nil\n}",
        )
        expect_unit_check_fails(
            tracker,
            tally,
            "an existence check reported as content verification",
            d,
            "must not be reported as anything stronger",
            "./internal/revalidate/",
            "TestRevalidationOfAMediumPlacementIsExistenceAndSaysSo",
        )

        # P1.8, the no-new-deletion-path line. It was a whole-module
        # source scan through phase 1 and it still is one; what #238
        # changed is the claim, from "no production file calls
        # DeleteObject" to "exactly one package does, and it is the move
        # engine". The violation is a SECOND place the ordering that
        # protects a backup gets decided, so it is planted in the one pass
        # whose own comment says it has no business acquiring a
        # MediumStore.
        d = mutant("phase1-a-second-production-deletion-path")
        tracker.swap(
            d / "core/internal/reconcile/reconcile.go",
            "func leftOnMedium(rec state.Record, st lifecycle.State, local localValidity) Finding {\n"
            "\treturn noAction(rec.Artifact, st, local.Reason)\n}",
            "func leftOnMedium(rec state.Record, st lifecycle.State, local localValidity) Finding {\n"
            "\treturn noAction(rec.Artifact, st, local.Reason)\n}\n"
            "\n"
            "// PLANTED VIOLATION (scripts/conformance/selftest.sh): a second production\n"
            "// deletion path, in the pass whose own comment says it has no business\n"
            "// acquiring a MediumStore.\n"
            "func plantedMediumDeletion(ctx context.Context, store transport.MediumStore, medium "
            "transport.Medium, key string) error {\n"
            "\treturn store.DeleteObject(ctx, medium, key)\n}",
        )
        expect_unit_check_fails(
            tracker,
            tally,
            "a second production deletion path outside the move engine",
            d,
            "these production files call DeleteObject",
            "./internal/transport/",
            "TestOnlyTheMoveEngineDeletesFromAMedium",
        )

        # The half of P1.4 no local backend can answer. rclone's local
        # backend hashes a file it can read, so the in-tree run exercises
        # ObjectChecksum's SUCCESS path and can say nothing at all about
        # the refusal; rclone v1.75.0's s3 backend exposes MD5 from the
        # ETag and nothing else, so the refusal branch exists only against
        # a real endpoint. This is that branch, mutated into the silent
        # downgrade FR-31 forbids, and it is inert against the local
        # backend on purpose: it is planted behind the capability test the
        # local backend passes.
        d = mutant("phase1-minio-attestation-degrades-instead-of-refusing")
        tracker.swap(
            d / "core/internal/transport/rclone/medium.go",
            "\tif !f.Hashes().Contains(hash.SHA256) {\n"
            '\t\treturn transport.ChecksumAttestation{}, WrapCtx(ctx, "object_checksum", fmt.Errorf(\n'
            '\t\t\t"%w: medium %q (type %s) cannot attest a full-object %s",\n'
            "\t\t\tErrUnsupportedHash, medium.ID, medium.Type, transport.SHA256))\n"
            "\t}",
            "\tif !f.Hashes().Contains(hash.SHA256) {\n"
            "\t\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): this\n"
            "\t\t// backend cannot attest, so hand back a nil error and let the\n"
            "\t\t// caller record a verification nobody ran.\n"
            '\t\treturn transport.ChecksumAttestation{Algorithm: transport.SHA256, Value: "planted"}, nil\n'
            "\t}",
        )
        expect_unit_check_fails(
            tracker,
            tally,
            "an endpoint that cannot attest, silently downgraded instead of refused",
            d,
            "never a silent downgrade",
            "./tests/miniointegration/",
            "TestMinioMediumContractSuite|TestMinioAttestationIsRefused",
        )

        print()
        print("==> the matrix's own BLOCKED citations")

        # core/tests/compat's citation guard used to accept any "#" in a
        # BLOCKED outcome, which is how three rows spent months citing
        # #235, #236 and #237 after all three were closed: a reference
        # nobody can act on, satisfying a check nobody could fail. It
        # resolves the issue against GitHub now, and this is the control
        # that says so. The plant makes one PASS row BLOCKED citing #235,
        # which is closed, and the guard has to say which row, which
        # issue, and that it is closed.
        #
        # The stub-resolver half of the same guard runs offline on every
        # gate (TestTheBlockedCitationGuardCanFail); this one is the
        # wiring, and it needs a working `gh`. A run without one fails
        # here rather than passing, for the reason every control in this
        # file exists.
        d = mutant("matrix-blocked-row-cites-a-closed-issue")
        tracker.swap(
            d / "docs/conformance/epic-e-matrix.md",
            "| P1.3 | PASS |",
            "| P1.3 | BLOCKED (#235) |",
        )
        expect_unit_check_fails(
            tracker,
            tally,
            "a BLOCKED row citing an issue that is closed",
            d,
            "is CLOSED",
            "./tests/compat/",
            "TestEveryBlockedRowCitesAnIssueThatIsStillOpen",
        )

        print()
        print("==> the matrix's own falsification for the phase 2 exit gate (P2.1, P2.2, V1)")

        # The source delete issued before the destination has been
        # verified. It is one edit in two places because the ordering is
        # enforced twice: the phase table has no edge, and the driver has
        # no case. Mutating only one of them produces a refused write
        # rather than the bug, which would be a mutation that "passed" for
        # the wrong reason.
        d = mutant("source-delete-before-verified")
        tracker.swap(
            d / "core/internal/placement/phases.go",
            "\t{From: Copied, To: Verifying},",
            "\t{From: Copied, To: Verifying},\n"
            "\t// PLANTED VIOLATION (scripts/conformance/selftest.sh).\n"
            "\t{From: Copied, To: SourceDeletePending},",
        )
        tracker.swap(
            d / "core/internal/placement/engine.go",
            "\t\tcase Copied:\n\t\t\tnext, err = e.startVerify(ctx, mv)",
            "\t\tcase Copied:\n"
            "\t\t\t// PLANTED VIOLATION (scripts/conformance/selftest.sh).\n"
            "\t\t\tnext, err = e.intendSourceDelete(ctx, mv)",
        )
        # The third edit is what makes this a real violation rather than a
        # refused write. intendSourceDelete names the phase it is leaving,
        # and without this the mutant stalls at the copy phase and the
        # suite goes red for "the write expected a different phase", which
        # is a mutation that broke the build rather than one that broke
        # the promise. The first run of this control did exactly that.
        tracker.swap(
            d / "core/internal/placement/engine.go",
            "\t\tMoveID: mv.ID, From: state.MoveVerified, To: state.MoveSourceDeletePending,",
            "\t\tMoveID: mv.ID, From: mv.Phase, To: state.MoveSourceDeletePending,",
        )
        expect_check_fails(
            tracker,
            tally,
            "the source delete issued before the destination is verified",
            d,
            "FR-30's standing invariant did not hold",
            "TestTheThreeTierChainEndToEnd|TestTheCrashMatrixAgainstARealS3Endpoint",
        )

        print()
        print("==> FR-27's home rule")

        # Chain order is load-bearing: the FIRST selecting tier names the
        # home, so an artifact claimed by daily and by monthly lives on
        # daily's medium. A rule that took the last one instead would
        # quietly send every warm artifact offsite.
        d = mutant("home-medium-takes-the-last-tier")
        tracker.swap(
            d / "core/internal/retention/homemedium.go",
            '\t\treturn t.EffectiveMedium(), true, nil\n\t}\n\treturn "", false, nil',
            "\t\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): keep\n"
            "\t\t// going, so the LAST selecting tier wins instead of the first.\n"
            "\t\tmedium, hasHome = t.EffectiveMedium(), true\n"
            "\t}\n"
            "\treturn medium, hasHome, nil",
        )
        expect_check_fails(
            tracker,
            tally,
            "the home rule taking the last selecting tier instead of the first",
            d,
            'want "local"',
            "TestTheThreeTierChainEndToEnd",
        )

        print()
        print("==> the bucketing invariant across a move (P2.3, V2's composed shape)")

        # FR-32: nothing a medium reports may reach a retention decision.
        # The composed shape of that is a move changing where an artifact
        # is bucketed, and the value most likely to do it is the one the
        # move itself writes, which is when the destination copy was
        # verified.
        d = mutant("bucketing-reads-when-the-copy-was-verified")
        tracker.swap(
            d / "core/internal/retention/bucketkey.go",
            "\tr := gfsDiscoveryInstant(rec)\n\tdiscovered = gfsPlacement{date: gfsCivilDateIn(r, loc), occurred: r}",
            "\tr := gfsDiscoveryInstant(rec)\n"
            "\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): let where the\n"
            "\t// bytes are decide when they were produced.\n"
            "\tfor _, pl := range rec.Placements {\n"
            "\t\tif pl.Status == state.PlacementActive && pl.VerifiedAt != nil {\n"
            "\t\t\tr = *pl.VerifiedAt\n"
            "\t\t}\n"
            "\t}\n"
            "\tdiscovered = gfsPlacement{date: gfsCivilDateIn(r, loc), occurred: r}",
        )
        expect_check_fails(
            tracker,
            tally,
            "bucketing derived from when a copy was verified",
            d,
            "changed its retention verdict",
            "TestAMoveDoesNotChangeARetentionVerdict",
        )

        print()
        print("==> the engine's refusals")

        # Medium to medium goes through a local staging copy (#429). The
        # chain's second hop is the composed run of it, so putting the old
        # outright refusal back has to turn that check red: the artifact
        # stays on the monthly medium and never reaches the annual one.
        d = mutant("medium-to-medium-refused-again")
        tracker.swap(
            d / "core/internal/placement/engine.go",
            "\t\tif err := e.canStage(rec.Artifact); err != nil {",
            '\t\tif err := fmt.Errorf("PLANTED VIOLATION (scripts/conformance/selftest.sh)"); err != nil {',
        )
        expect_check_fails(
            tracker,
            tally,
            "the medium-to-medium hop refused instead of staged",
            d,
            "want DONE (refusal:",
            "TestTheChainsSecondHopIsMediumToMedium",
        )

        # The staging copy is removed at the end of the copy phase. Leave
        # it and every hop between two mediums costs a permanent
        # artifact-sized file on the backup set's own disk, which is the
        # disk the next hop's size check is about. Nothing else about the
        # move changes, so only a check that looks for the leftover can
        # see it.
        d = mutant("staging-copy-left-behind")
        tracker.swap(
            d / "core/internal/placement/staging.go",
            "\trmErr := e.Local.Remove(ctx, staged)",
            "\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): the upload\n"
            "\t// landed; the staging copy stays.\n"
            "\tvar rmErr error",
        )
        expect_check_fails(
            tracker,
            tally,
            "a staged hop that never removes its staging copy",
            d,
            "the staging copy is still at",
            "TestTheChainsSecondHopIsMediumToMedium",
        )

        # The other end of the same ordering: the source delete is
        # durably intended, and then it actually happens. A move that
        # records the intent and leaves the file behind reads as a
        # completed move everywhere except on the disk, and every artifact
        # quietly costs two copies for ever.
        d = mutant("source-copy-never-removed")
        tracker.swap(
            d / "core/internal/placement/engine.go",
            "\tif t.localPath != \"\" {\n"
            "\t\tif e.Local == nil {\n"
            '\t\t\treturn fmt.Errorf("no local store is configured")\n'
            "\t\t}\n"
            "\t\treturn e.Local.Remove(ctx, t.localPath)\n\t}",
            "\tif t.localPath != \"\" {\n"
            "\t\tif e.Local == nil {\n"
            '\t\t\treturn fmt.Errorf("no local store is configured")\n'
            "\t\t}\n"
            "\t\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): the journal\n"
            "\t\t// says the source is gone; the file stays.\n"
            "\t\treturn nil\n\t}",
        )
        expect_check_fails(
            tracker,
            tally,
            "a completed move that never removes the source copy",
            d,
            'still has a local copy after a completed move to "annual_s3"',
            "TestTheThreeTierChainEndToEnd",
        )

        # A copy that fails leaves its reason on the move row. Without it
        # the row reads COPYING with an empty error for as long as the
        # failure lasts, and the only account of what went wrong lives in
        # a cycle report that is gone.
        d = mutant("failed-copy-records-no-reason")
        tracker.swap(
            d / "core/internal/placement/engine.go",
            "\t\tnoted, noteErr := e.step(ctx, mv, Copying, Copying, wrapped.Error())\n"
            "\t\tif noteErr != nil {\n"
            '\t\t\treturn mv, fmt.Errorf("%w (and the reason could not be recorded on the move row: %v)", '
            "wrapped, noteErr)\n"
            "\t\t}\n"
            "\t\treturn noted, wrapped",
            "\t\t// PLANTED VIOLATION (scripts/conformance/selftest.sh).\n\t\treturn mv, wrapped",
        )
        expect_check_fails(
            tracker,
            tally,
            "a failed copy that records no reason on the move row",
            d,
            "the move row carries no error at all",
            "TestAFailedCopyLeavesItsReasonOnTheMoveRow",
        )

        print()
        print("==> prune meeting an artifact the chain has moved (P2.4, V6)")

        # #239's medium-aware prune is in this tree, so what these
        # mutations break is no longer "refuse until it is built". It is
        # the shape of the delete: the decision is retention's, taken
        # without ever reading the medium (FR-32), and the evidence is
        # placement's, taken at the moment of the delete (FR-16). The
        # composed scenario is the only place either half runs against an
        # object a real move really put on a real bucket.

        # An artifact whose copy is on a medium sent down FR-20's local
        # path instead. That is wrong in the most expensive way available:
        # the local file is not there any more, so the verdict is about a
        # path nothing owns while the object survives unreferenced.
        d = mutant("prune-takes-the-local-path-for-a-medium-copy")
        tracker.swap(
            d / "core/internal/retention/prune.go",
            "\tif loc.OnMedium() {\n\t\treturn pruneEvaluateOnMedium(rec, keepVerdict, lkg, loc.Medium)\n\t}",
            "\t// PLANTED VIOLATION (scripts/conformance/selftest.sh).\n"
            "\tif false {\n"
            "\t\treturn pruneEvaluateOnMedium(rec, keepVerdict, lkg, loc.Medium)\n\t}",
        )
        expect_check_fails(
            tracker,
            tally,
            "an artifact on a medium sent down FR-20's local path",
            d,
            "is REFUSE, want DELETE",
            "TestPruneRemovesAnArtifactsOnlyCopyFromAMedium",
        )

        # A DELETE reported without anything being asked to make it. The
        # verdict reads exactly the same and the object is still there,
        # which is the failure mode a dry run cannot show and only the
        # apply can.
        d = mutant("prune-apply-never-asks-the-pruner")
        tracker.swap(
            d / "core/internal/retention/prune.go",
            "\t\t\tif err := medium.DeleteFromMedium(ctx, rec, verdicts[i].Medium); err != nil {\n"
            "\t\t\t\tverdicts[i].Action = PruneRefuse\n"
            '\t\t\t\tverdicts[i].Reason = fmt.Sprintf("nothing was removed from %q: %v", verdicts[i].Medium, err)\n'
            "\t\t\t}\n"
            "\t\t\tcontinue",
            "\t\t\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): report the\n"
            "\t\t\t// delete without making it.\n"
            "\t\t\t_ = medium\n"
            "\t\t\t_ = rec\n"
            "\t\t\tcontinue",
        )
        expect_check_fails(
            tracker,
            tally,
            "a medium delete reported but never made",
            d,
            "PruneApply asked the medium pruner for []",
            "TestPruneRemovesAnArtifactsOnlyCopyFromAMedium",
        )

        # V6's own planted violation, from the matrix: something else is
        # at the key by the time prune applies. FR-16 says compare before
        # deleting, and this is the comparison being run and then ignored.
        d = mutant("fr16-recheck-result-ignored")
        tracker.swap(
            d / "core/internal/placement/reclaim.go",
            "\tif !existence.Passed {\n\t\treturn refuse(\"%s\", existence.Detail)\n\t}",
            "\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): an object that\n"
            "\t// is there is an object worth deleting.\n"
            "\t_ = existence",
        )
        expect_check_fails(
            tracker,
            tally,
            "the FR-16 identity re-check run and its answer ignored",
            d,
            "and prune's verdict is DELETE, not REFUSE",
            "TestPruneRefusesAnObjectThatIsNoLongerTheOneTheJournalRecorded",
        )

        # The other half of the same row: FR-30 asks that the mandatory
        # dry-run NAME the medium for every proposed deletion, and that
        # surface is the CLI's, not the composed suite's. "DELETE 40
        # artifacts" reads very differently when half of them are objects
        # in a bucket, and an operator confirming a plan is confirming
        # this text.
        d = mutant("dry-run-does-not-name-the-medium")
        tracker.swap(
            d / "core/cmd/backupd/retention.go",
            "\tcase loc.Status == retention.LocationConfirmed && loc.Medium != config.MediumLocal:\n"
            '\t\treturn " medium=" + loc.Medium',
            "\tcase loc.Status == retention.LocationConfirmed && loc.Medium != config.MediumLocal:\n"
            "\t\t// PLANTED VIOLATION (scripts/conformance/selftest.sh).\n"
            '\t\treturn ""',
        )
        expect_unit_check_fails(
            tracker,
            tally,
            "a dry-run that does not say where a deletion would happen",
            d,
            "does not say where its deletion would happen",
            "./cmd/backupd/",
            "TestRun_RetentionNamesWhereADeletionWouldHappen",
        )

        print()
        print("==> the archive gate")

        # A retention tier bound to an archive-class medium is refused at
        # LOAD, before a daemon starts (#442). Take that away and the
        # config validates, the deployment runs, and every cycle plans a
        # move the engine then refuses for ever; before #437 it also paid
        # for two uploads and three deletes a cycle on a class that bills
        # a 180-day minimum for each discarded copy.
        #
        # The engine's own plan-time refusal is still there and is still
        # the second line of defence, for a journal written by an older
        # build or a bucket that grew a lifecycle rule. It is unreachable
        # from this suite now, because the config it needs no longer
        # loads, so its falsification lives in internal/placement's
        # archiveupload_test.go instead.
        d = mutant("archive-tier-accepted-at-load")
        tracker.swap(
            d / "core/internal/config/validate.go",
            "\tclass, ok := archived[t.Medium]\n\tif !ok {\n\t\treturn\n\t}",
            "\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): accept the\n"
            "\t// pairing and let the operator find out per cycle.\n"
            "\tclass, ok := archived[t.Medium]\n"
            "\tif true || !ok {\n"
            "\t\t_ = class\n"
            "\t\treturn\n\t}",
        )
        expect_check_fails(
            tracker,
            tally,
            "a retention tier on an archive class accepted at load",
            d,
            "the config loaded with the annual tier on a GLACIER medium",
            "TestAnArchiveClassTierIsRefusedAtLoad",
        )

        # An archived copy tops out at existence, because reading one
        # fails. Let it claim content and the composed scenario's whole
        # argument about why the annual tier is blocked stops being true.
        d = mutant("archived-copy-claims-content")
        tracker.swap(
            d / "core/internal/placement/gate.go",
            "\tcase archive.RequiresRestore, archive.Restoring:\n\t\treturn Existence",
            "\tcase archive.RequiresRestore, archive.Restoring:\n"
            "\t\t// PLANTED VIOLATION (scripts/conformance/selftest.sh).\n"
            "\t\treturn Content",
        )
        expect_check_fails(
            tracker,
            tally,
            "an archived copy allowed to claim content class",
            d,
            "now has a ceiling of",
            "TestNoConfigurableVerificationCanBeAchievedOnAnArchiveClass",
        )

        # The standing invariant means content class unless the operator
        # opted into attested. Admitting existence would make an archived
        # copy an acceptable SOLE copy, which is the failure the whole
        # EPIC is built to prevent.
        d = mutant("invariant-admits-existence")
        tracker.swap(
            d / "core/internal/placement/engine.go",
            "\tif len(sufficient) == 0 {\n\t\tsufficient = []Class{Content}\n\t}",
            "\tif len(sufficient) == 0 {\n"
            "\t\t// PLANTED VIOLATION (scripts/conformance/selftest.sh).\n"
            "\t\tsufficient = []Class{Content, Existence}\n\t}",
        )
        expect_check_fails(
            tracker,
            tally,
            "the standing invariant admitting existence class",
            d,
            "satisfies FR-30's standing invariant",
            "TestAnArchiveClassCopyCannotSatisfyTheStandingInvariant",
        )

        print()
        print("==> the crash matrix's convergence")

        # The destination is re-read and re-hashed before the source is
        # deleted, on the fresh path and the restart path alike. Trust the
        # upload instead and the two hostile-endpoint cells stop
        # protecting anything: the local copy goes and what survives is
        # whatever the endpoint felt like keeping.
        #
        # It takes BOTH return paths, and that is not tidiness. #437 split
        # verifyCopy in two, an ungated Verify and a gated
        # VerifyWithAccess, and a mutation that took only one of them would
        # leave the other still reading the bytes back, which is a
        # mutation that proves nothing. The anchor this used to carry was
        # the pre-split one, and it stopped matching: the swap refused to
        # plant, which is exactly what it is for.
        #
        # The three `_ =` lines are load-bearing too. Without them the
        # mutant does not compile, and a mutant that does not compile
        # fails the suite for a reason that has nothing to do with the
        # promise.
        d = mutant("destination-trusted-instead-of-verified")
        tracker.swap(
            d / "core/internal/placement/engine.go",
            "\tif !gated {\n"
            "\t\tres, err := Verify(ctx, e.Store, medium, candidate, want, e.now())\n"
            "\t\treturn res, want, err\n"
            "\t}\n"
            "\t// observe spends a restore-status call only for a class that needs\n"
            "\t// one, so a STANDARD destination costs exactly what it cost before.\n"
            "\tobs := e.observe(ctx, medium, mv.DestinationKey, medium.StorageClass)\n"
            "\tres, err := VerifyWithAccess(ctx, e.Store, medium, candidate, want, obs, e.now())\n"
            "\treturn res, want, err",
            "\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): trust the\n"
            "\t// upload rather than reading it back, on both paths.\n"
            "\t_ = gated\n"
            "\t_ = medium\n"
            "\t_ = candidate\n"
            '\treturn Result{Passed: true, Class: want, Detail: "planted"}, want, nil',
        )
        expect_check_fails(
            tracker,
            tally,
            "a destination trusted instead of verified",
            d,
            "does not hold the artifact's bytes",
            "TestTheCrashMatrixAgainstARealS3Endpoint",
        )

        print()
        print("==> the continuity claim itself")

        # The sampler in sampler_test.go is the control that gives
        # "continuously" its meaning, and a sampler that secretly looked
        # continuously would make the comparison vacuous while leaving it
        # green. So the control gets mutation-tested too: make the sampler
        # look at every event and the comparison must refuse to conclude
        # anything.
        d = mutant("sampler-secretly-watches-continuously")
        tracker.swap(
            d / "core/tests/conformance/watcher_test.go",
            "\tmv, err := j.inner.AdvanceMove(ctx, a)\n"
            '\tj.w.observe(fmt.Sprintf("after the journal wrote phase %s", a.To))\n'
            "\treturn mv, err",
            "\tmv, err := j.inner.AdvanceMove(ctx, a)\n"
            '\tj.w.observe(fmt.Sprintf("after the journal wrote phase %s", a.To))\n'
            "\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): the sampler\n"
            "\t// looks at every event too, so the comparison compares nothing.\n"
            "\tif j.w.sampler != nil {\n"
            '\t\tj.w.sampler.sample(j.w.t, "at every event")\n'
            "\t}\n"
            "\treturn mv, err",
        )
        tracker.swap(
            d / "core/tests/conformance/watcher_test.go",
            "\t// destroyed is the set of copies this run has watched being deleted\n"
            "\t// and has not seen rewritten, keyed by medium AND locator. See this\n"
            "\t// file's own comment for why the locator alone will not do.\n"
            "\tdestroyed map[string]bool",
            "\t// destroyed is the set of copies this run has watched being deleted\n"
            "\t// and has not seen rewritten, keyed by medium AND locator. See this\n"
            "\t// file's own comment for why the locator alone will not do.\n"
            "\tdestroyed map[string]bool\n"
            "\tsampler   *sampler",
        )
        tracker.swap(
            d / "core/tests/conformance/sampler_test.go",
            "\tsm := newSampler(w.journal, []model.ArtifactID{summer.id})\n"
            "\n"
            '\tsm.sample(t, "before the cycle")\n'
            '\twa.observe("before the cycle")\n'
            "\n"
            "\tplant := &earlyRelease{read: w.journal, target: summer.id}",
            "\tsm := newSampler(w.journal, []model.ArtifactID{summer.id})\n"
            "\twa.sampler = sm\n"
            "\n"
            '\tsm.sample(t, "before the cycle")\n'
            '\twa.observe("before the cycle")\n'
            "\n"
            "\tplant := &earlyRelease{read: w.journal, target: summer.id}",
        )
        expect_check_fails(
            tracker,
            tally,
            "a sampler that secretly looks continuously",
            d,
            "the sampler saw the breach",
            "TestTheWatcherCatchesABreachASamplerWouldMiss",
        )

        # And the other direction: a planted breach that plants nothing
        # must be refused rather than read as agreement between the two
        # judgements.
        d = mutant("planted-breach-plants-nothing")
        tracker.swap(
            d / "core/tests/conformance/sampler_test.go",
            "\ta.Placements = append(a.Placements, src.Update().WithStatus(state.PlacementDeletePending))\n"
            "\tj.fired++",
            "\t// PLANTED VIOLATION (scripts/conformance/selftest.sh): plant nothing.\n\t_ = src",
        )
        expect_check_fails(
            tracker,
            tally,
            "a planted breach that plants nothing",
            d,
            "the planted breach never fired",
            "TestTheWatcherCatchesABreachASamplerWouldMiss",
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
