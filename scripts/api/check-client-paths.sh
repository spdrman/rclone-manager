#!/usr/bin/env bash
# The /api/v1 CLIENT PATH gate (#211), at the path everything already names.
#
# The gate itself is scripts/rcmtools/api/check_client_paths.py now (EPIC I,
# I1.6 / #672). This file stays because the path is load bearing:
#
#   * scripts/ci-local.sh runs `bash scripts/api/check-client-paths.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this literal
#     path to drive the gate step it stubs;
#   * scripts/rcmtools/api/selftest.py drives THIS path (from inside each
#     mutant copy of the tree) rather than the module directly.
#
# `exec`, so the exit status is the gate's own.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/rcmtools/api/check_client_paths.py" "$@"
