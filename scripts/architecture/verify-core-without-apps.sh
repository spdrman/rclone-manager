#!/usr/bin/env bash
# EPIC-B WP1.1: "core/ builds and its full test suite passes with apps/
# deleted entirely" (docs/EPIC-B-multi-nas.md §7.1, §69 WP1.1), proved by
# actually deleting apps/ in a throwaway worktree rather than by trusting a
# static import scan to have caught every path.
#
# The check itself is scripts/bdtools/architecture/verify_core_without_apps.py
# now (EPIC I, I1.6 / #672 / #697). This file stays because the path is load
# bearing:
#
#   * scripts/ci-local.sh and .github/workflows/ci.yml run
#     `bash scripts/architecture/verify-core-without-apps.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path to drive the gate step it stubs;
#   * scripts/architecture/selftest.sh drives THIS path from inside each
#     mutant copy of the tree, so the shim is exercised by every control.
#
# `exec`, so the exit status is the check's own.
#
# NOTE: no `cd` here, deliberately. The check resolves its target tree with
# `git rev-parse --show-toplevel` of the CURRENT WORKING DIRECTORY, exactly
# as the bash original's `cd "$(git rev-parse --show-toplevel)"` did, which
# is what lets the self-test point it at a mutant copy.
set -euo pipefail

exec python3 "$(cd "$(dirname "$0")/../.." && pwd)/scripts/bdtools/architecture/verify_core_without_apps.py" "$@"
