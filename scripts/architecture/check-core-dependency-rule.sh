#!/usr/bin/env bash
# The dependency-direction check (issue #106 WP1.1, extended to three
# layers by issue #165): every Go module in the repository is held to the
# layer scripts/architecture/layers.conf declares it in, so a core-layer
# module imports neither platform nor distribution, a platform-layer
# module does not import distribution, and neither imports a NAS SDK.
#
# The check itself is scripts/bdtools/architecture/check_core_dependency_rule.py
# now (EPIC I, I1.6 / #672 / #697). This file stays because the path is
# load bearing:
#
#   * scripts/ci-local.sh runs `bash scripts/architecture/check-core-dependency-rule.sh`
#     TWICE (once in the static repository-structure section, once at the
#     head of the deletion-proof section) and .github/workflows/ci.yml
#     runs it once, as "core/app imports neither platform, distribution
#     nor a NAS SDK (static check)";
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

exec python3 "$(cd "$(dirname "$0")/../.." && pwd)/scripts/bdtools/architecture/check_core_dependency_rule.py" "$@"
