#!/usr/bin/env bash
# The e2e gate's entry point. The gate itself is
# scripts/bdtools/e2e/run_tests_repo_gate.py, ported from the 204 lines of
# bash that used to be this file (#672, EPIC I / #662).
#
# This is a FILE and not a symlink, and scripts/ci-local.sh still calls THIS
# rather than python3 directly, and both of those are load-bearing.
# scripts/tests/ci-local-gate.test.sh fabricates a stand-in gate at this
# literal path inside a sandbox tree -- a marker-printing one for Group G's
# "which steps did the gate choose to run", a failing one for "a red suite
# refuses the commit", and an `exit 3` one for the ledger cases. A symlink
# would resolve out of the sandbox into this checkout and run the real gate
# against a tree with no core/ and no pin; a ci-local.sh that invoked Python
# directly would not execute the fabricated file at all, and six assertions
# that read as green would be watching a step they no longer reach.
#
# `exec` rather than a call, so the gate's own exit status arrives at
# ci-local.sh untouched. That matters more here than it looks: 3 is the
# gate ledger's INCOMPLETE, and run_tests_repo_gate.py reaches 3 only
# through harness.cannot_run. Anything this shim added between the two
# could invent that verdict.
set -euo pipefail
exec python3 "$(cd "$(dirname "$0")/../.." && pwd)/scripts/bdtools/e2e/run_tests_repo_gate.py" "$@"
