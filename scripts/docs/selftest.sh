#!/usr/bin/env bash
# The package-documentation gate's own positive controls (#526), at the
# path everything already names.
#
# The controls themselves are scripts/bdtools/docs/selftest.py now (EPIC
# I, I1.6 / #672). This file stays because the path is load bearing:
#
#   * scripts/ci-local.sh runs `bash scripts/docs/selftest.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a file at this
#     literal path to drive the gate step it stubs.
#   * scripts/bdtools/selftest/check_anchors.py runs it with
#     --check-anchors for the same reason it runs every other anchored
#     selftest that way.
#
# `exec`, so the exit status is the self-test's own.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/bdtools/docs/selftest.py" "$@"
