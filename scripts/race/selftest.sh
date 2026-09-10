#!/usr/bin/env bash
# The race detector's own positive controls (#417), at the path everything
# already names.
#
# The controls themselves are scripts/rcmtools/race/selftest.py now (EPIC
# I, I1.6 / #672). This file stays because the path is load bearing:
#
#   * scripts/ci-local.sh runs `bash scripts/race/selftest.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a stand-in at this
#     literal path to drive the gate step it stubs;
#   * scripts/rcmtools/selftest/check_anchors.py (once ported) and
#     scripts/selftest/check-anchors.sh both run it with --check-anchors
#     for the same reason they run every other anchored selftest that way.
#
# `exec`, so the exit status is the self-test's own.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/rcmtools/race/selftest.py" "$@"
