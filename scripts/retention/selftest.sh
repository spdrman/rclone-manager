#!/usr/bin/env bash
# FR-20's retention-apply mutation self-test (#602), at the path
# everything already names.
#
# The controls themselves are scripts/rcmtools/retention/selftest.py now
# (EPIC I, I1.6 / #672). This file stays because the path is load
# bearing:
#
#   * scripts/ci-local.sh runs `bash scripts/retention/selftest.sh`;
#   * scripts/tests/ci-local-gate.test.sh FABRICATES a stand-in at this
#     literal path to drive the gate step it stubs;
#   * scripts/rcmtools/selftest/check_anchors.py runs it with
#     --check-anchors for the same reason it runs every other anchored
#     selftest that way.
#
# `exec`, so the exit status is the self-test's own.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/rcmtools/retention/selftest.py" "$@"
