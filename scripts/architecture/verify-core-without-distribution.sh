#!/usr/bin/env bash
# EPIC B #81's dependency rule, the literal half (issue #165): "the core and
# generic application must build and pass tests if the distribution adapter
# tree is deleted." It deletes exactly the paths
# scripts/architecture/layers.conf marks "distribution adapter" -- never a
# list of its own -- so adding an adapter without declaring it fails
# check-layer-manifest.sh rather than silently escaping this proof.
#
# The check itself is
# scripts/bdtools/architecture/verify_core_without_distribution.py now
# (EPIC I, I1.6 / #672 / #697). This file stays because the path is load
# bearing:
#
#   * scripts/ci-local.sh and .github/workflows/ci.yml run
#     `bash scripts/architecture/verify-core-without-distribution.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path to drive the gate step it stubs;
#   * scripts/architecture/selftest.sh drives THIS path from inside each
#     mutant copy of the tree -- including its delete-traversal and
#     delete-symlink-escape controls, which grep the check's own refusal
#     text -- so the shim is exercised by every control.
#
# `exec`, so the exit status is the check's own.
#
# NOTE: no `cd` here, deliberately. The check resolves its target tree with
# `git rev-parse --show-toplevel` of the CURRENT WORKING DIRECTORY, exactly
# as the bash original's `cd "$(git rev-parse --show-toplevel)"` did, which
# is what lets the self-test point it at a mutant copy.
set -euo pipefail

exec python3 "$(cd "$(dirname "$0")/../.." && pwd)/scripts/bdtools/architecture/verify_core_without_distribution.py" "$@"
